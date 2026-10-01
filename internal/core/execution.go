// Package core defines the opaque execution boundary shared by adapters, the
// runtime, and connectors. It does not interpret protocol payload bytes.
package core

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

var ErrStreamContract = errors.New("invalid execution stream lifecycle")

type Capability string

type ExecutionRequest struct {
	ID           string // Assigned by the runtime, never taken from client metadata.
	Model        string
	Capabilities map[Capability]struct{}
	Metadata     RequestMetadata
	Payload      RawPayload
}

type RawPayload struct {
	Protocol    string
	ContentType string
	Body        []byte
}

type RequestMetadata struct {
	Headers    map[string][]string
	Streaming  *bool          // Nil means the client did not specify a preference.
	Extensions map[string]any // Only explicitly declared, namespaced extensions.
}

// Opaque request and response bytes belong to their producer until handed off.
// After handoff neither producer nor consumer may mutate them; a consumer that
// needs to retain bytes beyond the next Next call must copy them. Native mode
// forwards the admitted Body unchanged, including unrecognized fields.
type ExecutionResponse struct {
	Stream Stream
}

type FrameType string

const (
	FrameHead     FrameType = "head"
	FrameBody     FrameType = "body"
	FrameComplete FrameType = "complete"
)

// Exactly one payload corresponding to Type must be present.
type StreamFrame struct {
	Type     FrameType
	Head     *HeadFrame
	Body     *BodyFrame
	Complete *CompleteFrame
}

type HeadFrame struct {
	Protocol    string
	ContentType string
	HTTPStatus  *int // Nil for non-HTTP transports.
	Headers     map[string][]string
	Error       *GatewayError // Rejection classification is available before commit.
}

type BodyFrame struct {
	Data []byte
}

type Outcome string

const (
	OutcomeSucceeded  Outcome = "succeeded"
	OutcomeFailed     Outcome = "failed"
	OutcomeCancelled  Outcome = "cancelled"
	OutcomeIncomplete Outcome = "incomplete"
)

type CompleteFrame struct {
	Outcome Outcome
	Usage   *UsageReport
	Error   *GatewayError
}

type ErrorCategory string

const (
	CategoryInvalidRequest     ErrorCategory = "invalid_request"
	CategoryUnsupportedFeature ErrorCategory = "unsupported_feature"
	CategoryUnauthenticated    ErrorCategory = "unauthenticated"
	CategoryPermissionDenied   ErrorCategory = "permission_denied"
	CategoryRateLimited        ErrorCategory = "rate_limited"
	CategoryUnavailable        ErrorCategory = "unavailable"
	CategoryTimeout            ErrorCategory = "timeout"
	CategoryCancelled          ErrorCategory = "cancelled"
	CategoryInternal           ErrorCategory = "internal"
)

type RetryDisposition string

const (
	RetrySafe    RetryDisposition = "safe"
	RetryUnsafe  RetryDisposition = "unsafe"
	RetryUnknown RetryDisposition = "unknown"
)

// Only Message is client-safe; Provider and OriginalError are diagnostic and
// must never be automatically serialized into a client error response.
type GatewayError struct {
	Code             string
	Category         ErrorCategory
	Retryable        bool             // Unspecified is false.
	RetryDisposition RetryDisposition // Empty means unknown, never safe.
	Provider         string
	OriginalError    string
	Message          string
	RetryAfter       *time.Duration
}

func (e *GatewayError) Error() string { return e.Message }

type UsageSource string

const (
	UsageProvider UsageSource = "provider"
	UsageEstimate UsageSource = "estimate"
	UsageUnknown  UsageSource = "unknown"
)

type UsageCompleteness string

const (
	UsageComplete            UsageCompleteness = "complete"
	UsagePartial             UsageCompleteness = "partial"
	UsageUnknownCompleteness UsageCompleteness = "unknown"
)

type UsageReport struct {
	InputTokens     *int64
	OutputTokens    *int64
	ReasoningTokens *int64
	CachedTokens    *int64
	Source          UsageSource
	Completeness    UsageCompleteness
}

// Stream has one consumer. Next respects ctx, returning frames in order and
// io.EOF only after Complete; premature termination reports a non-EOF error.
// Close is idempotent, interrupts a blocked Next,
// and releases producer resources even when the stream is only partly read.
// The producer must honor cancellation of the context passed to Next, and
// release a blocked Next when Close is called from another goroutine.
type Stream interface {
	Next(context.Context) (StreamFrame, error)
	Close() error
}

// checkedStream validates an untrusted producer at the shared boundary.
// The owner must Close it after consumption or on abandonment.
type checkedStream struct {
	source    Stream
	protocol  string
	mu        sync.Mutex
	phase     int // 0 before head, 1 in body, 2 completed, 3 failed/closed
	headError bool
	terminal  error // Non-EOF termination without a Complete frame.
	once      sync.Once
	closeErr  error
}

// NewCheckedStream checks the lifecycle and admitted response protocol. The
// caller retains responsibility for closing the stream after EOF or abandonment.
func NewCheckedStream(source Stream, admittedProtocol string) Stream {
	return &checkedStream{source: source, protocol: admittedProtocol}
}

func (s *checkedStream) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		if s.phase != 2 && s.terminal == nil {
			s.terminal = context.Canceled
		}
		s.phase = 3
		s.mu.Unlock()
		s.closeErr = s.source.Close()
	})
	return s.closeErr
}

func (s *checkedStream) Next(ctx context.Context) (StreamFrame, error) {
	if err := ctx.Err(); err != nil {
		s.Close()
		return StreamFrame{}, err
	}
	s.mu.Lock()
	phase := s.phase
	terminal := s.terminal
	s.mu.Unlock()
	if phase == 3 {
		if terminal != nil {
			return StreamFrame{}, terminal
		}
		return StreamFrame{}, io.EOF
	}
	f, err := s.source.Next(ctx)
	if ctx.Err() != nil {
		s.Close()
		return StreamFrame{}, ctx.Err()
	}
	s.mu.Lock()
	closed := s.phase == 3
	terminal = s.terminal
	s.mu.Unlock()
	if closed {
		if terminal != nil {
			return StreamFrame{}, terminal
		}
		return StreamFrame{}, io.EOF
	}
	if err != nil {
		if err == io.EOF && phase != 2 {
			err = ErrStreamContract
		}
		if err != io.EOF {
			s.mu.Lock()
			if s.terminal == nil {
				s.terminal = err
			}
			s.mu.Unlock()
			s.Close()
		}
		return StreamFrame{}, err
	}
	s.mu.Lock()
	if s.phase == 3 {
		terminal = s.terminal
		s.mu.Unlock()
		if terminal != nil {
			return StreamFrame{}, terminal
		}
		return StreamFrame{}, io.EOF
	}
	valid := false
	switch f.Type {
	case FrameHead:
		valid = phase == 0 && f.Head != nil && f.Body == nil && f.Complete == nil && f.Head.Protocol == s.protocol && f.Head.Protocol != ""
		if valid {
			s.phase = 1
			s.headError = f.Head.Error != nil
		}
	case FrameBody:
		valid = phase == 1 && f.Head == nil && f.Body != nil && len(f.Body.Data) > 0 && f.Complete == nil
	case FrameComplete:
		valid = phase == 1 && f.Head == nil && f.Body == nil && f.Complete != nil
		if valid {
			c := f.Complete
			valid = (c.Outcome == OutcomeSucceeded && c.Error == nil && !s.headError) ||
				(c.Outcome != OutcomeSucceeded && c.Error != nil && (c.Outcome == OutcomeFailed || c.Outcome == OutcomeCancelled || c.Outcome == OutcomeIncomplete))
		}
		if valid {
			s.phase = 2
		}
	}
	s.mu.Unlock()
	if !valid {
		s.mu.Lock()
		if s.terminal == nil {
			s.terminal = ErrStreamContract
		}
		s.mu.Unlock()
		s.Close()
		return StreamFrame{}, ErrStreamContract
	}
	return f, nil
}
