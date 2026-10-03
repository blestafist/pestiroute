package core

import (
	"context"
	"errors"
	"sync"
	"time"
)

type dispatchStateKey struct{}

type dispatchState struct {
	requestID string
	ordinal   int
	deadline  *retryDeadline
}

func (d *Dispatcher) Execute(ctx context.Context, in ExecutionRequest) (response ExecutionResponse, gatewayErr *GatewayError) {
	_, principalPresent := TrustedPrincipalFromContext(ctx)
	if (principalPresent || d.Policies != nil) && d.degraded() {
		return ExecutionResponse{}, accountingUnavailableError(nil)
	}
	requestID, err := newID()
	if err != nil {
		return ExecutionResponse{}, executionError(err)
	}
	if (principalPresent || d.Policies != nil) && d.Accounting != nil {
		finish := func() error {
			err := d.Accounting.FinishRequest(context.WithoutCancel(ctx), requestID)
			if err != nil {
				if isAccountingStorageFailure(err) {
					d.markDegraded()
				}
				return accountingUnavailableError(err)
			}
			return nil
		}
		defer func() {
			if gatewayErr != nil {
				if err := finish(); err != nil {
					gatewayErr = accountingUnavailableError(err)
				}
			} else if response.Stream != nil {
				response.Stream = &requestAccountingStream{source: response.Stream, finish: finish}
			}
		}()
	}
	maxAttempts := d.RetryMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	mode := d.Mode
	if mode == "" {
		mode = ModeNative
	}
	multiTarget := d.Routes != nil && len(d.Routes.Candidates(in, mode, d.AccountID)) > 1
	if d.Routes == nil || (maxAttempts == 1 && d.RetryDeadline <= 0 && !multiTarget) {
		return d.executeAttempt(context.WithValue(ctx, dispatchStateKey{}, dispatchState{requestID: requestID}), in)
	}
	if d.RetryDeadline <= 0 && maxAttempts > 1 {
		return ExecutionResponse{}, &GatewayError{Code: "invalid_retry_policy", Category: CategoryInternal, Message: "Retry policy is unavailable"}
	}
	retryCtx, cancelRequest := context.WithCancelCause(ctx)
	deadline := newRetryDeadline(retryCtx, d.RetryDeadline, cancelRequest)
	state := dispatchState{requestID: requestID, deadline: deadline}
	retryCtx = context.WithValue(retryCtx, dispatchStateKey{}, state)
	keepDeadline := false
	defer func() {
		if !keepDeadline {
			deadline.cleanup()
		}
	}()
	if maxAttempts == 1 && !multiTarget {
		response, gatewayErr := d.executeAttempt(retryCtx, in)
		if gatewayErr != nil {
			return ExecutionResponse{}, gatewayErr
		}
		keepDeadline = true
		return ExecutionResponse{Stream: &cleanupStream{source: response.Stream, cleanup: deadline.cleanup}}, nil
	}
	candidates, err := d.AuthorizedCandidates(retryCtx, in)
	if err != nil || len(candidates) == 0 {
		if deadline.isExpired() || retryCtx.Err() != nil {
			return ExecutionResponse{}, deadline.error(ctx)
		}
		if errors.Is(err, ErrCandidateLimits) {
			return ExecutionResponse{}, &GatewayError{Code: "invalid_request", Category: CategoryInvalidRequest, Message: "Request too large for target limits"}
		}
		return ExecutionResponse{}, &GatewayError{Code: "unsupported_target", Category: CategoryUnsupportedFeature, Message: "Unsupported execution target"}
	}
	primary := -1
	for i := range candidates {
		if candidates[i].Route.Identity.AccountID == d.AccountID {
			primary = i
			break
		}
	}
	if primary < 0 {
		primary = 0
	}
	current := primary
	lastTried := -1
	var lastErr *GatewayError
	var selected RouteSelection
	for ordinal := 0; ordinal < maxAttempts; ordinal++ {
		if ordinal > 0 {
			fresh, authErr := d.AuthorizedCandidates(retryCtx, in)
			if authErr != nil {
				if retryCtx.Err() != nil {
					return ExecutionResponse{}, deadline.error(ctx)
				}
				return ExecutionResponse{}, authorizationError(authErr)
			}
			current = -1
			for i := range fresh {
				position := routePosition(candidates, fresh[i].Route.Identity.AccountID)
				if position > lastTried {
					current, selected = position, fresh[i]
					break
				}
			}
			if current < 0 || current >= len(candidates) {
				break
			}
		} else {
			selected = candidates[current]
		}
		if deadline.isExpired() || retryCtx.Err() != nil {
			return ExecutionResponse{}, deadline.error(ctx)
		}
		candidate := selected
		lastTried = current
		degradationLatch := d.degradationLatch
		if degradationLatch == nil {
			degradationLatch = &d.accountingDegraded
		}
		attemptDispatcher := Dispatcher{
			Target: d.Target, Routes: d.Routes, Policies: d.Policies, Accounts: d.Accounts, Accounting: d.Accounting,
			Budget: d.Budget, BudgetPolicy: d.BudgetPolicy, RetryMaxAttempts: d.RetryMaxAttempts, RetryDeadline: d.RetryDeadline,
			RouteID: d.RouteID, Services: d.Services, AccountID: candidate.Route.Identity.AccountID,
			Mode: d.Mode, Adapter: d.Adapter, Connector: d.Connector, Finalize: d.Finalize, Observations: d.Observations,
			degradationLatch: degradationLatch, OnAccountingFailure: d.OnAccountingFailure,
		}
		attemptBase, cancelAttempt := context.WithCancel(retryCtx)
		attemptCtx := context.WithValue(attemptBase, dispatchStateKey{}, dispatchState{requestID: requestID, ordinal: ordinal, deadline: deadline})
		response, gatewayErr := attemptDispatcher.executeAttempt(attemptCtx, in)
		if gatewayErr == nil {
			if maxAttempts == 1 {
				keepDeadline = true
				return ExecutionResponse{Stream: &cleanupStream{source: response.Stream, cleanup: func() { cancelAttempt(); deadline.cleanup() }}}, nil
			}
			frame, readErr := response.Stream.Next(attemptCtx)
			if readErr == nil && frame.Type == FrameHead && frame.Head != nil && frame.Head.Error == nil {
				attemptStream, ok := response.Stream.(*attemptStream)
				if !ok {
					_ = response.Stream.Close()
					cancelAttempt()
					return ExecutionResponse{}, executionError(ErrStreamContract)
				}
				keepDeadline = true
				return ExecutionResponse{Stream: &prefetchedStream{first: frame, source: response.Stream, ctx: attemptCtx,
					cleanup: func() { cancelAttempt(); deadline.cleanup() }, commitHead: func() bool { return attemptStream.handoff(true) }}}, nil
			}
			_ = response.Stream.Close()
			if readErr == nil && frame.Type == FrameHead && frame.Head != nil {
				gatewayErr = frame.Head.Error
			} else if readErr != nil {
				if !errors.As(readErr, &gatewayErr) {
					gatewayErr = executionError(readErr)
				}
			} else {
				gatewayErr = executionError(ErrStreamContract)
			}
		}
		cancelAttempt()
		if deadline.isExpired() {
			return ExecutionResponse{}, executionError(context.DeadlineExceeded)
		}
		lastErr = gatewayErr
		remaining := make([]int, 0, len(candidates)-current-1)
		for i := current + 1; i < len(candidates); i++ {
			remaining = append(remaining, i)
		}
		decision := DecideRetry(RetryInput{Context: retryCtx, Now: time.Now(), Deadline: deadline.getDeadline(), AttemptIndex: ordinal,
			MaxAttempts: maxAttempts, Failure: gatewayErr, RemainingCandidates: remaining})
		if !decision.Allowed {
			break
		}
		if decision.Delay > 0 {
			timer := time.NewTimer(decision.Delay)
			select {
			case <-retryCtx.Done():
				timer.Stop()
				return ExecutionResponse{}, deadline.error(ctx)
			case <-timer.C:
			}
		}
		current = decision.CandidateIndex
	}
	if retryCtx.Err() != nil {
		return ExecutionResponse{}, deadline.error(ctx)
	}
	return ExecutionResponse{}, lastErr
}

type retryDeadline struct {
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelCauseFunc
	duration time.Duration
	timer    *time.Timer
	at       time.Time
	expired  bool
}

func newRetryDeadline(ctx context.Context, duration time.Duration, cancel context.CancelCauseFunc) *retryDeadline {
	return &retryDeadline{ctx: ctx, cancel: cancel, duration: duration}
}

// start is called immediately before the first durable request admission and
// never again for fallback attempts.
func (d *retryDeadline) start() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.duration <= 0 || d.timer != nil || d.ctx.Err() != nil {
		return
	}
	d.at = time.Now().Add(d.duration)
	d.timer = time.AfterFunc(d.duration, d.expire)
}

func (d *retryDeadline) expire() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.expired {
		d.expired = true
		d.cancel(context.DeadlineExceeded)
	}
}

func (d *retryDeadline) getDeadline() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.at
}

func (d *retryDeadline) isExpired() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.expired && !d.at.IsZero() && !time.Now().Before(d.at) {
		d.expired = true
		d.cancel(context.DeadlineExceeded)
	}
	return d.expired
}

func (d *retryDeadline) error(parent context.Context) *GatewayError {
	if d.isExpired() {
		return executionError(context.DeadlineExceeded)
	}
	if err := contextError(parent); err != nil {
		return executionError(err)
	}
	if err := contextError(d.ctx); err != nil {
		return executionError(err)
	}
	return executionError(context.DeadlineExceeded)
}

func (d *retryDeadline) cleanup() {
	d.mu.Lock()
	if d.timer != nil {
		d.timer.Stop()
	}
	d.mu.Unlock()
	d.cancel(context.Canceled)
}

func routePosition(candidates []RouteSelection, account string) int {
	for i := range candidates {
		if candidates[i].Route.Identity.AccountID == account {
			return i
		}
	}
	return len(candidates)
}

type prefetchedStream struct {
	first      StreamFrame
	source     Stream
	ctx        context.Context
	used       bool
	cleanup    func()
	commitHead func() bool
	once       sync.Once
}

func (s *prefetchedStream) Next(ctx context.Context) (StreamFrame, error) {
	if err := ctx.Err(); err != nil {
		_ = s.Close()
		return StreamFrame{}, contextError(ctx)
	}
	if err := s.ctx.Err(); err != nil {
		_ = s.Close()
		return StreamFrame{}, contextError(s.ctx)
	}
	if !s.used {
		if s.commitHead != nil && !s.commitHead() {
			_ = s.Close()
			return StreamFrame{}, context.Canceled
		}
		s.used = true
		return s.first, nil
	}
	frame, err := s.source.Next(ctx)
	if err != nil {
		s.once.Do(s.cleanup)
	}
	return frame, err
}

func (s *prefetchedStream) Close() error {
	s.once.Do(s.cleanup)
	return s.source.Close()
}

type cleanupStream struct {
	source  Stream
	cleanup func()
	once    sync.Once
}

func (s *cleanupStream) Next(ctx context.Context) (StreamFrame, error) {
	frame, err := s.source.Next(ctx)
	if err != nil {
		s.once.Do(s.cleanup)
	}
	return frame, err
}

func (s *cleanupStream) Close() error {
	err := s.source.Close()
	s.once.Do(s.cleanup)
	return err
}

func contextError(ctx context.Context) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return ctx.Err()
}

// requestAccountingStream terminalizes the request only after the attempt has
// settled, before exposing Complete. Safe failed attempts remain open for fallback.
type requestAccountingStream struct {
	source Stream
	finish func() error
	once   sync.Once
	err    error
}

func (s *requestAccountingStream) settle() error {
	s.once.Do(func() { s.err = s.finish() })
	return s.err
}
func (s *requestAccountingStream) Next(ctx context.Context) (StreamFrame, error) {
	frame, err := s.source.Next(ctx)
	if err != nil || frame.Type == FrameComplete {
		if finishErr := s.settle(); finishErr != nil {
			return StreamFrame{}, finishErr
		}
	}
	return frame, err
}
func (s *requestAccountingStream) Close() error {
	err := s.source.Close()
	return errors.Join(err, s.settle())
}
