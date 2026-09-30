package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"sync"
)

const m1Protocol = "openai.responses.v1"
const m1Model = "gpt-4.1-mini-2025-04-14"

// AttemptScope is runtime-owned; client metadata is never used for selection.
type AttemptScope struct {
	ID        string
	AccountID string
	Mode      string
}

type Target interface {
	Execute(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError)
}

type CapabilityState string

const (
	Supported   CapabilityState = "supported"
	Unsupported CapabilityState = "unsupported"
	Unknown     CapabilityState = "unknown"
)

type AttemptResult struct {
	RequestID string
	Scope     AttemptScope
	Committed bool
	Outcome   Outcome
	Usage     UsageReport
	Error     *GatewayError
}

// Dispatcher is configured for one target/account and exact, scoped M1 support
// declarations. The callback records one terminal observation per admitted attempt.
type Dispatcher struct {
	Target    Target
	AccountID string
	Mode      string // Empty uses the M1 native mode.
	Adapter   map[Capability]CapabilityState
	Connector map[Capability]CapabilityState
	Finalize  func(AttemptResult)
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (d *Dispatcher) Execute(ctx context.Context, in ExecutionRequest) (ExecutionResponse, *GatewayError) {
	if err := ctx.Err(); err != nil {
		return ExecutionResponse{}, executionError(err)
	}
	if in.Payload.Protocol != m1Protocol || in.Model != m1Model || (d.Mode != "" && d.Mode != "native") || d.Target == nil || d.AccountID == "" {
		return ExecutionResponse{}, &GatewayError{Code: "unsupported_target", Category: CategoryUnsupportedFeature, Message: "Unsupported execution target"}
	}
	for capability := range in.Capabilities {
		if (capability != "llm.streaming" && capability != "llm.tools" && capability != "llm.structured_output") || d.Adapter[capability] != Supported || d.Connector[capability] != Supported {
			return ExecutionResponse{}, &GatewayError{Code: "unsupported_capability", Category: CategoryUnsupportedFeature, Message: "Unsupported required capability"}
		}
	}
	requestID, err := newID()
	if err != nil {
		return ExecutionResponse{}, executionError(err)
	}
	attemptID, err := newID()
	if err != nil {
		return ExecutionResponse{}, executionError(err)
	}
	in.ID = requestID
	scope := AttemptScope{ID: attemptID, AccountID: d.AccountID, Mode: "native"}
	result := AttemptResult{RequestID: requestID, Scope: scope, Outcome: OutcomeIncomplete, Usage: UsageReport{Source: UsageUnknown, Completeness: UsageUnknownCompleteness}}
	finish := func(r AttemptResult) {
		if d.Finalize != nil {
			d.Finalize(r)
		}
	}
	response, gatewayErr := d.Target.Execute(ctx, in, scope)
	if gatewayErr != nil {
		if response.Stream != nil {
			response.Stream.Close()
		}
		result.Outcome = OutcomeFailed
		if gatewayErr.Category == CategoryCancelled {
			result.Outcome = OutcomeCancelled
		}
		result.Error = gatewayErr
		finish(result)
		return ExecutionResponse{}, gatewayErr
	}
	if response.Stream == nil {
		gatewayErr = executionError(ErrStreamContract)
		result.Outcome = OutcomeFailed
		result.Error = gatewayErr
		finish(result)
		return ExecutionResponse{}, gatewayErr
	}
	s := &attemptStream{source: NewCheckedStream(response.Stream, in.Payload.Protocol), ctx: ctx, result: result, finish: finish}
	s.stopMu.Lock()
	s.stop = context.AfterFunc(ctx, func() { s.Close() })
	s.stopMu.Unlock()
	return ExecutionResponse{Stream: s}, nil
}

func executionError(err error) *GatewayError {
	if errors.Is(err, context.Canceled) {
		return &GatewayError{Code: "execution_cancelled", Category: CategoryCancelled, Message: "Execution cancelled"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &GatewayError{Code: "execution_timeout", Category: CategoryTimeout, Message: "Execution timed out"}
	}
	return &GatewayError{Code: "execution_failed", Category: CategoryInternal, Message: "Execution failed"}
}

type attemptStream struct {
	source Stream
	ctx    context.Context
	result AttemptResult
	finish func(AttemptResult)
	stopMu sync.Mutex
	stop   func() bool
	mu     sync.Mutex // Serializes Head handoff and terminal observation.
	done   bool
	commit bool
	closed sync.Once
}

func (s *attemptStream) finalize(outcome Outcome, usage *UsageReport, err *GatewayError, preHeadGateway bool) bool {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return false
	}
	s.done = true
	r := s.result
	r.Committed = s.commit
	if preHeadGateway && !s.commit {
		outcome = OutcomeFailed
	}
	r.Outcome = outcome
	if usage != nil {
		r.Usage = *usage
	}
	r.Error = err
	s.mu.Unlock()
	s.finish(r) // Never invoke an observer under the handoff lock.
	return true
}

func (s *attemptStream) handoff(head bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return false
	}
	if head {
		s.commit = true
	}
	return true
}

func (s *attemptStream) Close() error {
	var err error
	s.closed.Do(func() {
		s.stopMu.Lock()
		if s.stop != nil {
			s.stop()
		}
		s.stopMu.Unlock()
		err = s.source.Close()
	})
	cause := s.ctx.Err()
	if cause == nil {
		cause = context.Canceled
	}
	s.fail(cause)
	return err
}

func (s *attemptStream) fail(err error) {
	outcome := OutcomeIncomplete
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		outcome = OutcomeCancelled
	}
	var gatewayErr *GatewayError
	preHeadGateway := errors.As(err, &gatewayErr)
	if !preHeadGateway {
		gatewayErr = executionError(err)
	}
	s.finalize(outcome, nil, gatewayErr, preHeadGateway)
}

func (s *attemptStream) Next(ctx context.Context) (StreamFrame, error) {
	if err := s.ctx.Err(); err != nil {
		s.Close()
		return StreamFrame{}, err
	}
	f, err := s.source.Next(ctx)
	if err != nil {
		if err != io.EOF {
			s.fail(err)
		}
		s.Close()
		return StreamFrame{}, err
	}
	switch f.Type {
	case FrameHead:
		if !s.handoff(true) {
			return StreamFrame{}, context.Canceled
		}
	case FrameComplete:
		if !s.finalize(f.Complete.Outcome, f.Complete.Usage, f.Complete.Error, false) {
			return StreamFrame{}, context.Canceled
		}
		s.Close()
	case FrameBody:
		if !s.handoff(false) {
			return StreamFrame{}, context.Canceled
		}
	}
	return f, nil
}
