package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const m1Protocol = "openai.responses.v1"
const m1Model = "gpt-5.4-mini"

var (
	ErrRouteUnavailable  = errors.New("route table unavailable")
	ErrCandidateAffinity = errors.New("candidate group requires known stateless affinity")
	ErrCandidateLimits   = errors.New("candidate route limits require measured ingress headers")
)

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
	accountingDegraded atomic.Bool
	degradationLatch   *atomic.Bool
	Target             Target
	Routes             *RouteTable
	Policies           PolicyStore
	Accounts           AccountAuthorizer
	Accounting         AccountingStore
	Budget             RouteBudget
	BudgetPolicy       string
	RetryMaxAttempts   int
	RetryDeadline      time.Duration
	RouteID            string
	Services           interface {
		ForAttempt(AttemptScope) InvocationServices
	}
	AccountID           string
	Mode                string // Empty uses the M1 native mode.
	Adapter             map[Capability]CapabilityState
	Connector           map[Capability]CapabilityState
	Finalize            func(AttemptResult)
	Observations        AttemptObservationSink
	OnAccountingFailure func()
}

func (d *Dispatcher) degraded() bool {
	if d.degradationLatch != nil {
		return d.degradationLatch.Load()
	}
	return d.accountingDegraded.Load()
}

func (d *Dispatcher) markDegraded() {
	if d.OnAccountingFailure != nil {
		d.OnAccountingFailure()
	}
	d.accountingDegraded.Store(true)
	if d.degradationLatch != nil {
		d.degradationLatch.Store(true)
	}
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (d *Dispatcher) executeAttempt(ctx context.Context, in ExecutionRequest) (ExecutionResponse, *GatewayError) {
	if err := ctx.Err(); err != nil {
		gatewayErr := executionError(contextError(ctx))
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
	var authorization CandidateAuthorization
	_, principalPresent := TrustedPrincipalFromContext(ctx)
	protected := principalPresent || d.Policies != nil
	if protected && d.degraded() {
		return ExecutionResponse{}, accountingUnavailableError(nil)
	}
	legacy := d.Routes == nil
	executionMode := d.Mode
	if executionMode == "" {
		executionMode = ModeNative
	}
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
		candidates := d.Routes.Candidates(in, executionMode, d.AccountID)
		if len(candidates) == 0 {
			return ExecutionResponse{}, &GatewayError{Code: "unsupported_target", Category: CategoryUnsupportedFeature, Message: "Unsupported execution target"}
		}
		if len(candidates) > 1 && (in.Metadata.SessionBound || !in.Metadata.AffinityKnown) {
			return ExecutionResponse{}, &GatewayError{Code: "unsupported_target", Category: CategoryUnsupportedFeature, Message: "Unsupported execution target"}
		}
		selection, err := d.Routes.Select(ctx, in, SelectionContext{Mode: executionMode, AccountID: d.AccountID})
		if err != nil {
			return ExecutionResponse{}, &GatewayError{Code: "unsupported_target", Category: CategoryUnsupportedFeature, Message: "Unsupported execution target"}
		}
		if selection.Route.MaxBodyBytes > 0 && int64(len(in.Payload.Body)) > selection.Route.MaxBodyBytes ||
			selection.Route.MaxHeaderBytes > 0 && (in.Metadata.IngressHeaderBytes == 0 || in.Metadata.IngressHeaderBytes > selection.Route.MaxHeaderBytes) {
			return ExecutionResponse{}, &GatewayError{Code: "invalid_request", Category: CategoryInvalidRequest, Message: "Request too large for target limits"}
		}
		var ok bool
		connector, ok = selection.Connector.(Connector)
		if !ok || d.Services == nil {
			return ExecutionResponse{}, &GatewayError{Code: "unsupported_target", Category: CategoryUnsupportedFeature, Message: "Unsupported execution target"}
		}
		if protected {
			var err error
			authorization, err = LoadCandidateAuthorization(ctx, d.Policies)
			if err != nil {
				return ExecutionResponse{}, authorizationError(err)
			}
			if err := authorization.AuthorizeTarget(ctx, in.Model, string(selection.Route.Connector), d.AccountID, d.Accounts); err != nil {
				return ExecutionResponse{}, authorizationError(err)
			}
		}
		scope := CapabilityScope{Protocol: in.Payload.Protocol, Mode: executionMode, Model: in.Model, AccountID: d.AccountID}
		candidate := EligibilityCandidate{
			Scope: scope, Adapter: selection.Adapter.Descriptor(), Connector: selection.Connector.Descriptor(),
			InitializedAndReady: true, AdapterCapabilityScope: scope, ConnectorCapabilityScope: scope,
			AdapterCapabilities: selection.Adapter.Capabilities(ctx, scope), ConnectorCapabilities: selection.Connector.Capabilities(ctx, scope),
		}
		requirements := EligibilityRequirements{Request: make(map[Capability]struct{}, len(in.Capabilities)), Route: make(map[Capability]struct{}, len(selection.Route.Requirements))}
		for capability := range in.Capabilities {
			requirements.Request[capability] = struct{}{}
		}
		for _, capability := range selection.Route.Requirements {
			requirements.Route[capability] = struct{}{}
		}
		if protected {
			if authErr := authorization.AuthorizeEligibility(scope, candidate, requirements); authErr != nil {
				if errors.Is(authErr, ErrPermissionDenied) || ctx.Err() != nil {
					return ExecutionResponse{}, authorizationError(authErr)
				}
				return ExecutionResponse{}, &GatewayError{Code: "unsupported_capability", Category: CategoryUnsupportedFeature, Message: "Unsupported required capability"}
			}
		} else if candidate.Eligible(scope, requirements) != nil {
			return ExecutionResponse{}, &GatewayError{Code: "unsupported_capability", Category: CategoryUnsupportedFeature, Message: "Unsupported required capability"}
		}
		route, adapterID, connectorID = selection.Route.Identity, selection.Route.Adapter, selection.Route.Connector
	}
	state, statePresent := ctx.Value(dispatchStateKey{}).(dispatchState)
	requestID := state.requestID
	if !statePresent || requestID == "" {
		var err error
		requestID, err = newID()
		if err != nil {
			return ExecutionResponse{}, executionError(err)
		}
	}
	attemptID, err := newID()
	if err != nil {
		return ExecutionResponse{}, executionError(err)
	}
	in.ID = requestID
	scope := AttemptScope{ID: attemptID, AccountID: d.AccountID, Mode: executionMode}
	result := AttemptResult{RequestID: requestID, Scope: scope, Route: route, Adapter: adapterID, Connector: connectorID, Outcome: OutcomeIncomplete, Usage: UsageReport{Source: UsageUnknown, Completeness: UsageUnknownCompleteness}, StartedAt: time.Now()}
	var gatewayErr *GatewayError
	var accountingEstimate ResolvedEstimate
	if protected {
		if legacy || d.Accounting == nil || d.BudgetPolicy == "" || d.RouteID == "" {
			return ExecutionResponse{}, &GatewayError{Code: "accounting_unavailable", Category: CategoryUnavailable, Message: "Request accounting is unavailable"}
		}
		principal, _ := authorization.AccountingIdentity()
		accountingEstimate, gatewayErr = ResolveEstimate(ctx, connector, UsageQuery{
			Protocol: in.Payload.Protocol, Mode: scope.Mode, Model: in.Model,
			AccountID: scope.AccountID, Payload: in.Payload,
		}, d.Services.ForAttempt(scope), d.Budget)
		if gatewayErr != nil {
			return ExecutionResponse{}, gatewayErr
		}
		if accountingEstimate.Method == "" {
			accountingEstimate.Method = "conservative"
		}
		admission := AccountingAdmission{
			RequestID: requestID, AttemptID: attemptID, KeyID: principal.KeyID, PolicyID: principal.PolicyID,
			KeyRevision: principal.KeyRevision, PolicyRevision: principal.PolicyRevision,
			Protocol: route.Protocol, Model: route.Model, RouteID: d.RouteID,
			AccountID: d.AccountID, Connector: string(connectorID),
			EstimateTokens: accountingEstimate.Tokens, EstimateMethod: accountingEstimate.Method,
			BudgetPolicy: d.BudgetPolicy,
		}
		admit := d.Accounting.Admit
		if state.ordinal > 0 {
			admit = d.Accounting.BeginAttempt
		}
		if statePresent && state.ordinal == 0 && state.deadline != nil {
			state.deadline.start()
		}
		if err := admit(ctx, admission); err != nil {
			if errors.Is(err, ErrAdmissionLimit) {
				return ExecutionResponse{}, &GatewayError{Code: "rate_limit_exceeded", Category: CategoryRateLimited, Retryable: true, Message: "Request rate limit exceeded"}
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return ExecutionResponse{}, executionError(contextError(ctx))
			}
			if isAccountingStorageFailure(err) {
				d.markDegraded()
			}
			return ExecutionResponse{}, accountingUnavailableError(err)
		}
	}
	observe := func(r AttemptResult) {
		if d.Observations != nil {
			d.Observations.TryRecord(observationFromResult(r))
		}
	}
	persist := func(r AttemptResult) error {
		if protected {
			if err := d.Accounting.FinalizeAttempt(context.WithoutCancel(ctx), accountingTerminal(r)); err != nil {
				if isAccountingStorageFailure(err) {
					d.markDegraded()
				}
				return accountingUnavailableError(err)
			}
		}
		return nil
	}
	finish := func(r AttemptResult) {
		if d.Finalize != nil {
			d.Finalize(r)
		}
	}
	finishBeforeStream := func(r AttemptResult) *GatewayError {
		r.EndedAt = time.Now()
		observe(r)
		if err := persist(r); err != nil {
			return accountingUnavailableError(err)
		}
		finish(r)
		return nil
	}
	if statePresent && state.deadline != nil && state.deadline.isExpired() {
		gatewayErr = executionError(context.DeadlineExceeded)
		result.Outcome, result.Error = OutcomeCancelled, gatewayErr
		if persistErr := finishBeforeStream(result); persistErr != nil {
			return ExecutionResponse{}, persistErr
		}
		return ExecutionResponse{}, gatewayErr
	}
	var response ExecutionResponse
	invoke := func() {
		if legacy {
			response, gatewayErr = d.Target.Execute(ctx, in, scope)
		} else {
			response, gatewayErr = connector.Execute(ctx, in, scope, d.Services.ForAttempt(scope))
		}
	}
	execute := func() error {
		invoke()
		if gatewayErr != nil {
			return gatewayErr
		}
		return nil
	}
	if protected {
		if err := ExecuteAfterDispatchIntent(ctx, d.Accounting, attemptID, time.Now(), execute); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				gatewayErr = executionError(contextError(ctx))
			}
			if gatewayErr == nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					gatewayErr = executionError(err)
				} else {
					if isAccountingStorageFailure(err) {
						d.markDegraded()
					}
					gatewayErr = accountingUnavailableError(err)
				}
			}
			if response.Stream != nil {
				_ = response.Stream.Close()
			}
			result.Outcome, result.Error = OutcomeFailed, gatewayErr
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || gatewayErr.Category == CategoryCancelled {
				result.Outcome = OutcomeCancelled
			}
			if accountingErr := finishBeforeStream(result); accountingErr != nil {
				return ExecutionResponse{}, accountingErr
			}
			return ExecutionResponse{}, gatewayErr
		}
	} else {
		invoke()
	}
	if gatewayErr != nil {
		if err := ctx.Err(); err != nil {
			gatewayErr = executionError(contextError(ctx))
		}
		if response.Stream != nil {
			response.Stream.Close()
		}
		result.Outcome = OutcomeFailed
		if gatewayErr.Category == CategoryCancelled {
			result.Outcome = OutcomeCancelled
		}
		result.Error = gatewayErr
		if persistErr := finishBeforeStream(result); persistErr != nil {
			return ExecutionResponse{}, persistErr
		}
		return ExecutionResponse{}, gatewayErr
	}
	if response.Stream == nil {
		gatewayErr = executionError(ErrStreamContract)
		result.Outcome = OutcomeFailed
		result.Error = gatewayErr
		if persistErr := finishBeforeStream(result); persistErr != nil {
			return ExecutionResponse{}, persistErr
		}
		return ExecutionResponse{}, gatewayErr
	}
	s := &attemptStream{source: NewCheckedStream(response.Stream, in.Payload.Protocol), ctx: ctx, result: result, observe: observe, finish: finish, persist: persist,
		allowRejectedHeadRetry: d.RetryMaxAttempts > 1, deferHeadCommit: d.RetryMaxAttempts > 1}
	s.stopMu.Lock()
	s.stop = context.AfterFunc(ctx, func() { s.Close() })
	s.stopMu.Unlock()
	return ExecutionResponse{Stream: s}, nil
}

// AuthorizedCandidates returns eligible route targets in configured order. It
// performs no estimation, accounting admission, or execution; retry policy and
// attempt creation remain owned by the later retry/fallback flow.
func (d *Dispatcher) AuthorizedCandidates(ctx context.Context, in ExecutionRequest) ([]RouteSelection, error) {
	if d == nil || d.Routes == nil {
		return nil, ErrRouteUnavailable
	}
	mode := d.Mode
	if mode == "" {
		mode = ModeNative
	}
	routes := d.Routes.Candidates(in, mode, d.AccountID)
	if len(routes) > 1 && (in.Metadata.SessionBound || !in.Metadata.AffinityKnown) {
		return nil, ErrCandidateAffinity
	}
	_, principalPresent := TrustedPrincipalFromContext(ctx)
	protected := principalPresent || d.Policies != nil
	var authorization CandidateAuthorization
	if protected {
		var err error
		authorization, err = LoadCandidateAuthorization(ctx, d.Policies)
		if err != nil {
			return nil, err
		}
	}
	eligible := make([]RouteSelection, 0, len(routes))
	for _, route := range routes {
		if route.MaxBodyBytes > 0 && int64(len(in.Payload.Body)) > route.MaxBodyBytes {
			continue
		}
		if route.MaxHeaderBytes > 0 {
			if in.Metadata.IngressHeaderBytes == 0 {
				return nil, ErrCandidateLimits
			}
			if in.Metadata.IngressHeaderBytes > route.MaxHeaderBytes {
				continue
			}
		}
		selection, err := d.Routes.Select(ctx, in, SelectionContext{Mode: mode, AccountID: route.Identity.AccountID})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if _, ok := selection.Connector.(Connector); !ok || d.Services == nil {
			continue
		}
		if protected {
			if err := authorization.AuthorizeTarget(ctx, in.Model, string(route.Connector), route.Identity.AccountID, d.Accounts); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue
			}
		}
		scope := CapabilityScope{Protocol: in.Payload.Protocol, Mode: mode, Model: in.Model, AccountID: route.Identity.AccountID}
		requirements := EligibilityRequirements{Request: make(map[Capability]struct{}, len(in.Capabilities)), Route: make(map[Capability]struct{}, len(route.Requirements))}
		for capability := range in.Capabilities {
			requirements.Request[capability] = struct{}{}
		}
		for _, capability := range route.Requirements {
			requirements.Route[capability] = struct{}{}
		}
		candidate := EligibilityCandidate{
			Scope: scope, Adapter: selection.Adapter.Descriptor(), Connector: selection.Connector.Descriptor(),
			InitializedAndReady: true, AdapterCapabilityScope: scope, ConnectorCapabilityScope: scope,
			AdapterCapabilities: selection.Adapter.Capabilities(ctx, scope), ConnectorCapabilities: selection.Connector.Capabilities(ctx, scope),
		}
		if protected {
			err = authorization.AuthorizeEligibility(scope, candidate, requirements)
		} else {
			err = candidate.Eligible(scope, requirements)
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		eligible = append(eligible, selection)
	}
	return eligible, nil
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

func authorizationError(err error) *GatewayError {
	if errors.Is(err, context.Canceled) {
		return executionError(context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return executionError(context.DeadlineExceeded)
	}
	return &GatewayError{Code: "permission_denied", Category: CategoryPermissionDenied, Message: "Permission denied"}
}

func accountingUnavailableError(error) *GatewayError {
	return &GatewayError{Code: "accounting_unavailable", Category: CategoryUnavailable, Message: "Request accounting is unavailable"}
}

func isAccountingStorageFailure(err error) bool {
	var failure AccountingStorageFailure
	return errors.As(err, &failure)
}

func accountingTerminal(result AttemptResult) AccountingTerminal {
	state := string(result.Outcome)
	if result.Outcome == OutcomeIncomplete {
		state = "interrupted"
	}
	usage := result.Usage
	if !result.HasUsage {
		usage = UsageReport{Source: UsageUnknown, Completeness: UsageUnknownCompleteness}
	}
	terminal := AccountingTerminal{
		AttemptID: result.Scope.ID, Outcome: Outcome(state), Committed: result.Committed,
		Usage: usage, EndedAt: result.EndedAt,
	}
	if result.Error != nil {
		category := result.Error.Category
		reason := result.Error.Code
		terminal.Category, terminal.Reason = category, reason
	}
	return terminal
}

type attemptStream struct {
	source                 Stream
	ctx                    context.Context
	result                 AttemptResult
	observe                func(AttemptResult)
	finish                 func(AttemptResult)
	persist                func(AttemptResult) error
	stopMu                 sync.Mutex
	stop                   func() bool
	mu                     sync.Mutex // Serializes Head handoff and terminal observation.
	done                   bool
	closing                bool
	closeCause             error
	persisted              chan struct{}
	commit                 bool
	pending                *CompleteFrame
	closed                 sync.Once
	finishErr              error
	allowRejectedHeadRetry bool
	deferHeadCommit        bool
}

func (s *attemptStream) finalize(outcome Outcome, usage *UsageReport, err *GatewayError, preHeadGateway bool) bool {
	s.mu.Lock()
	if s.done {
		persisted := s.persisted
		s.mu.Unlock()
		if persisted != nil {
			<-persisted
		}
		return false
	}
	r := s.reserveLocked(outcome, usage, err, preHeadGateway)
	s.mu.Unlock()
	s.publish(r)
	return true
}

func (s *attemptStream) reserveLocked(outcome Outcome, usage *UsageReport, err *GatewayError, preHeadGateway bool) AttemptResult {
	s.done = true
	s.persisted = make(chan struct{})
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
	return r
}

func (s *attemptStream) publish(r AttemptResult) {
	if s.observe != nil {
		s.observe(r)
	}
	if s.persist != nil {
		err := s.persist(r)
		s.mu.Lock()
		s.finishErr = err
		s.mu.Unlock()
	}
	close(s.persisted)
	if s.finish != nil {
		s.finish(r)
	}
}

// finalizeEOF arbitrates orderly EOF against Close under the lifecycle lock.
func (s *attemptStream) finalizeEOF(pending *CompleteFrame) error {
	s.mu.Lock()
	if s.done {
		persisted, cause := s.persisted, s.closeCause
		s.mu.Unlock()
		if persisted != nil {
			<-persisted
		}
		if cause != nil {
			return cause
		}
		return s.finishErr
	}
	outcome, usage, gatewayErr := pending.Outcome, pending.Usage, pending.Error
	if s.closing {
		cause := s.closeCause
		if cause == nil {
			cause = context.Canceled
		}
		outcome, usage, gatewayErr = OutcomeCancelled, pending.Usage, executionError(cause)
		r := s.reserveLocked(outcome, usage, gatewayErr, false)
		s.mu.Unlock()
		s.publish(r)
		if cause != nil {
			return cause
		}
		return s.finishErr
	}
	r := s.reserveLocked(outcome, usage, gatewayErr, false)
	s.mu.Unlock()
	s.publish(r)
	return s.finishErr
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
	cause := contextError(s.ctx)
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

func (s *attemptStream) finalizationError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishErr
}

func (s *attemptStream) Next(ctx context.Context) (StreamFrame, error) {
	if err := s.ctx.Err(); err != nil {
		s.Close()
		return StreamFrame{}, contextError(s.ctx)
	}
	f, err := s.source.Next(ctx)
	if err != nil && s.ctx.Err() != nil {
		err = contextError(s.ctx)
	}
	if err != nil {
		if err == io.EOF {
			s.mu.Lock()
			pending := s.pending
			s.mu.Unlock()
			if pending == nil {
				s.fail(ErrStreamContract)
				s.Close()
				if persistErr := s.finalizationError(); persistErr != nil {
					return StreamFrame{}, persistErr
				}
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
				if persistErr := s.finalizationError(); persistErr != nil {
					return StreamFrame{}, persistErr
				}
				return StreamFrame{}, ErrStreamContract
			}
			s.fail(err)
		}
		s.Close()
		if persistErr := s.finalizationError(); persistErr != nil {
			return StreamFrame{}, persistErr
		}
		return StreamFrame{}, err
	}
	switch f.Type {
	case FrameHead:
		if f.Head.Error != nil && s.allowRejectedHeadRetry {
			s.fail(f.Head.Error)
			_ = s.Close()
			if persistErr := s.finalizationError(); persistErr != nil {
				return StreamFrame{}, persistErr
			}
			return StreamFrame{}, f.Head.Error
		}
		if !s.deferHeadCommit && !s.handoff(true) {
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
		// Complete is control metadata. Do not expose its outcome until the
		// producer has proved there is no trailing frame or stream error.
		_, terminalErr := s.source.Next(ctx)
		if terminalErr != io.EOF {
			if terminalErr == nil || (!errors.Is(terminalErr, context.Canceled) && !errors.Is(terminalErr, context.DeadlineExceeded)) {
				terminalErr = ErrStreamContract
			}
			s.fail(terminalErr)
			_ = s.Close()
			return StreamFrame{}, terminalErr
		}
		if cause := s.finalizeEOF(&complete); cause != nil {
			_ = s.Close()
			return StreamFrame{}, cause
		}
		_ = s.Close()
	case FrameBody:
		if !s.handoff(false) {
			return StreamFrame{}, context.Canceled
		}
	}
	return f, nil
}
