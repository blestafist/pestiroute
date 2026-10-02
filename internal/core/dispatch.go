package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"time"
)

const m1Protocol = "openai.responses.v1"
const m1Model = "gpt-5.4-mini"

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
	Route     RouteIdentity
	Adapter   InstanceID
	Connector InstanceID
	StartedAt time.Time
	EndedAt   time.Time
	Committed bool
	Outcome   Outcome
	Usage     UsageReport
	HasUsage  bool
	Error     *GatewayError
}

// Dispatcher uses the route registry when configured and retains the fixed
// target/account fields for the legacy M1 path. Finalize records each attempt.
type Dispatcher struct {
	Target   Target
	Routes   *RouteTable
	Services interface {
		ForAttempt(AttemptScope) InvocationServices
	}
	AccountID    string
	Mode         string // Empty uses the M1 native mode.
	Adapter      map[Capability]CapabilityState
	Connector    map[Capability]CapabilityState
	Finalize     func(AttemptResult)
	Observations AttemptObservationSink
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
		gatewayErr := executionError(err)
		requestID, requestErr := newID()
		attemptID, attemptErr := newID()
		if requestErr == nil && attemptErr == nil {
			mode := d.Mode
			if mode == "" {
				mode = "native"
			}
			result := AttemptResult{
				RequestID: requestID,
				Scope:     AttemptScope{ID: attemptID, AccountID: d.AccountID, Mode: mode},
				StartedAt: time.Now(), EndedAt: time.Now(), Outcome: OutcomeCancelled,
				Usage: UsageReport{Source: UsageUnknown, Completeness: UsageUnknownCompleteness},
				Error: gatewayErr,
			}
			if d.Observations != nil {
				d.Observations.TryRecord(observationFromResult(result))
			}
			if d.Finalize != nil {
				d.Finalize(result)
			}
		}
		return ExecutionResponse{}, gatewayErr
	}
	var connector Connector
	var route RouteIdentity
	var adapterID, connectorID InstanceID
	legacy := d.Routes == nil
	if legacy {
		if in.Model != m1Model {
			return ExecutionResponse{}, &GatewayError{Code: "invalid_request", Category: CategoryInvalidRequest, Message: "Invalid request"}
		}
		if in.Payload.Protocol != m1Protocol || (d.Mode != "" && d.Mode != ModeNative) || d.Target == nil || d.AccountID == "" {
			return ExecutionResponse{}, &GatewayError{Code: "unsupported_target", Category: CategoryUnsupportedFeature, Message: "Unsupported execution target"}
		}
		for capability := range in.Capabilities {
			if (capability != "llm.streaming" && capability != "llm.tools" && capability != "llm.reasoning" && capability != "llm.structured_output") || d.Adapter[capability] != Supported || d.Connector[capability] != Supported {
				return ExecutionResponse{}, &GatewayError{Code: "unsupported_capability", Category: CategoryUnsupportedFeature, Message: "Unsupported required capability"}
			}
		}
	} else {
		mode := d.Mode
		if mode == "" {
			mode = ModeNative
		}
		selection, err := d.Routes.Select(ctx, in, SelectionContext{Mode: mode, AccountID: d.AccountID})
		if err != nil {
			return ExecutionResponse{}, &GatewayError{Code: "unsupported_target", Category: CategoryUnsupportedFeature, Message: "Unsupported execution target"}
		}
		var ok bool
		connector, ok = selection.Connector.(Connector)
		if !ok || d.Services == nil {
			return ExecutionResponse{}, &GatewayError{Code: "unsupported_target", Category: CategoryUnsupportedFeature, Message: "Unsupported execution target"}
		}
		route, adapterID, connectorID = selection.Route.Identity, selection.Route.Adapter, selection.Route.Connector
		scope := CapabilityScope{Protocol: in.Payload.Protocol, Mode: mode, Model: in.Model, AccountID: d.AccountID}
		candidate := EligibilityCandidate{
			Scope: scope, Adapter: selection.Adapter.Descriptor(), Connector: selection.Connector.Descriptor(),
			InitializedAndReady: true, AdapterCapabilityScope: scope, ConnectorCapabilityScope: scope,
			AdapterCapabilities: selection.Adapter.Capabilities(ctx, scope), ConnectorCapabilities: selection.Connector.Capabilities(ctx, scope),
		}
		requirements := EligibilityRequirements{Request: make(map[Capability]struct{}, len(in.Capabilities))}
		for capability := range in.Capabilities {
			requirements.Request[capability] = struct{}{}
		}
		if candidate.Eligible(scope, requirements) != nil {
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
	result := AttemptResult{RequestID: requestID, Scope: scope, Route: route, Adapter: adapterID, Connector: connectorID, Outcome: OutcomeIncomplete, Usage: UsageReport{Source: UsageUnknown, Completeness: UsageUnknownCompleteness}, StartedAt: time.Now()}
	observe := func(r AttemptResult) {
		if d.Observations != nil {
			d.Observations.TryRecord(observationFromResult(r))
		}
	}
	finish := func(r AttemptResult) {
		if d.Finalize != nil {
			d.Finalize(r)
		}
	}
	finishBeforeStream := func(r AttemptResult) {
		r.EndedAt = time.Now()
		observe(r)
		finish(r)
	}
	var response ExecutionResponse
	var gatewayErr *GatewayError
	if legacy {
		response, gatewayErr = d.Target.Execute(ctx, in, scope)
	} else {
		response, gatewayErr = connector.Execute(ctx, in, scope, d.Services.ForAttempt(scope))
	}
	if gatewayErr != nil {
		if response.Stream != nil {
			response.Stream.Close()
		}
		result.Outcome = OutcomeFailed
		if gatewayErr.Category == CategoryCancelled {
			result.Outcome = OutcomeCancelled
		}
		result.Error = gatewayErr
		finishBeforeStream(result)
		return ExecutionResponse{}, gatewayErr
	}
	if response.Stream == nil {
		gatewayErr = executionError(ErrStreamContract)
		result.Outcome = OutcomeFailed
		result.Error = gatewayErr
		finishBeforeStream(result)
		return ExecutionResponse{}, gatewayErr
	}
	s := &attemptStream{source: NewCheckedStream(response.Stream, in.Payload.Protocol), ctx: ctx, result: result, observe: observe, finish: finish}
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
	source     Stream
	ctx        context.Context
	result     AttemptResult
	observe    func(AttemptResult)
	finish     func(AttemptResult)
	stopMu     sync.Mutex
	stop       func() bool
	mu         sync.Mutex // Serializes Head handoff and terminal observation.
	done       bool
	closing    bool
	closeCause error
	published  chan struct{}
	commit     bool
	pending    *CompleteFrame
	closed     sync.Once
}

func (s *attemptStream) finalize(outcome Outcome, usage *UsageReport, err *GatewayError, preHeadGateway bool) bool {
	s.mu.Lock()
	if s.done {
		published := s.published
		s.mu.Unlock()
		if published != nil {
			<-published
		}
		return false
	}
	r, published := s.reserveLocked(outcome, usage, err, preHeadGateway)
	s.mu.Unlock()
	s.publish(r, published)
	return true
}

func (s *attemptStream) reserveLocked(outcome Outcome, usage *UsageReport, err *GatewayError, preHeadGateway bool) (AttemptResult, chan struct{}) {
	s.done = true
	s.published = make(chan struct{})
	r := s.result
	r.Committed = s.commit
	if preHeadGateway && !s.commit {
		outcome = OutcomeFailed
	}
	r.Outcome = outcome
	if usage != nil {
		r.Usage = *usage
		r.HasUsage = true
	}
	r.Error = err
	r.EndedAt = time.Now()
	return r, s.published
}

func (s *attemptStream) publish(r AttemptResult, published chan struct{}) {
	if s.observe != nil {
		s.observe(r)
	}
	close(published)
	if s.finish != nil {
		s.finish(r)
	}
}

// finalizeEOF arbitrates orderly EOF against Close under the lifecycle lock.
func (s *attemptStream) finalizeEOF(pending *CompleteFrame) error {
	s.mu.Lock()
	if s.done {
		published, cause := s.published, s.closeCause
		s.mu.Unlock()
		if published != nil {
			<-published
		}
		return cause
	}
	outcome, usage, gatewayErr := pending.Outcome, pending.Usage, pending.Error
	if s.closing {
		cause := s.closeCause
		if cause == nil {
			cause = context.Canceled
		}
		outcome, usage, gatewayErr = OutcomeCancelled, pending.Usage, executionError(cause)
		r, published := s.reserveLocked(outcome, usage, gatewayErr, false)
		s.mu.Unlock()
		s.publish(r, published)
		return cause
	}
	r, published := s.reserveLocked(outcome, usage, gatewayErr, false)
	s.mu.Unlock()
	s.publish(r, published)
	return nil
}

func (s *attemptStream) handoff(head bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done || s.closing {
		return false
	}
	if head {
		s.commit = true
	}
	return true
}

func (s *attemptStream) Close() error {
	var err error
	cause := s.ctx.Err()
	if cause == nil {
		cause = context.Canceled
	}
	s.mu.Lock()
	if !s.done {
		s.closing = true
		s.closeCause = cause
	}
	s.mu.Unlock()
	s.closed.Do(func() {
		s.stopMu.Lock()
		if s.stop != nil {
			s.stop()
		}
		s.stopMu.Unlock()
		err = s.source.Close()
	})
	s.fail(cause)
	return err
}

func (s *attemptStream) fail(err error) {
	outcome := OutcomeIncomplete
	if errors.Is(err, ErrStreamContract) {
		outcome = OutcomeFailed
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		outcome = OutcomeCancelled
	}
	s.mu.Lock()
	pending := s.pending
	closing, closeCause := s.closing, s.closeCause
	s.mu.Unlock()
	if closing {
		if closeCause == nil {
			closeCause = context.Canceled
		}
		err = closeCause
		outcome = OutcomeCancelled
	}
	var gatewayErr *GatewayError
	preHeadGateway := !closing && pending == nil && errors.As(err, &gatewayErr)
	if !preHeadGateway {
		if pending != nil && outcome != OutcomeCancelled {
			outcome = OutcomeFailed
			err = ErrStreamContract
		}
		gatewayErr = executionError(err)
	}
	var usage *UsageReport
	if pending != nil {
		usage = pending.Usage
	}
	s.finalize(outcome, usage, gatewayErr, preHeadGateway)
}

func (s *attemptStream) Next(ctx context.Context) (StreamFrame, error) {
	if err := s.ctx.Err(); err != nil {
		s.Close()
		return StreamFrame{}, err
	}
	f, err := s.source.Next(ctx)
	if err != nil {
		if err == io.EOF {
			s.mu.Lock()
			pending := s.pending
			s.mu.Unlock()
			if pending == nil {
				s.fail(ErrStreamContract)
				s.Close()
				return StreamFrame{}, ErrStreamContract
			}
			if cause := s.finalizeEOF(pending); cause != nil {
				s.Close()
				return StreamFrame{}, cause
			}
		} else {
			s.mu.Lock()
			pending := s.pending
			s.mu.Unlock()
			if pending != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				s.fail(ErrStreamContract)
				_ = s.Close()
				return StreamFrame{}, ErrStreamContract
			}
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
		s.mu.Lock()
		if s.done || s.closing {
			cause := s.closeCause
			s.mu.Unlock()
			if cause != nil {
				return StreamFrame{}, cause
			}
			return StreamFrame{}, context.Canceled
		}
		complete := *f.Complete
		s.pending = &complete
		s.mu.Unlock()
	case FrameBody:
		if !s.handoff(false) {
			return StreamFrame{}, context.Canceled
		}
	}
	return f, nil
}
