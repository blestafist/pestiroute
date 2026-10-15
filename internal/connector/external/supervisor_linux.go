//go:build linux

package external

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

const (
	supervisorRestartLimit = 3
	supervisorWindow       = time.Minute
	supervisorBaseBackoff  = 100 * time.Millisecond
	supervisorMaxBackoff   = 2 * time.Second
)

// restartBudget tracks restart instants using time.Time's monotonic component.
type restartBudget struct {
	restarts []time.Time
}

func (b *restartBudget) admit(now time.Time) (int, bool) {
	cutoff := now.Add(-supervisorWindow)
	first := 0
	for first < len(b.restarts) && !b.restarts[first].After(cutoff) {
		first++
	}
	b.restarts = append(b.restarts[:0], b.restarts[first:]...)
	if len(b.restarts) == supervisorRestartLimit {
		return 0, false
	}
	b.restarts = append(b.restarts, now)
	return len(b.restarts), true
}

func supervisorBackoff(restart int) time.Duration {
	delay := supervisorBaseBackoff
	for i := 1; i < restart && delay < supervisorMaxBackoff; i++ {
		delay *= 2
	}
	if delay > supervisorMaxBackoff {
		return supervisorMaxBackoff
	}
	return delay
}

var errSupervisorUnavailable = &core.GatewayError{Code: "connector_unavailable", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "External connector is restarting or unavailable"}

// Supervisor owns a stable Connector facade while replacing failed child
// generations. A generation is published only after handshake, Descriptor and
// Init have all succeeded.
type Supervisor struct {
	mu             sync.RWMutex
	cfg            ProcessConfig
	component      core.ComponentConfig
	descriptor     core.Descriptor
	current        *Connector
	process        *Process
	ctx            context.Context
	cancel         context.CancelFunc
	epoch          uint64
	budget         restartBudget
	ready          bool
	failed         bool
	closed         bool
	started        bool
	closeDone      chan struct{}
	closeErr       error
	closeWaitLimit time.Duration // test hook; zero uses the production drain plus termination bound.
	watchers       sync.WaitGroup
}

func StartSupervisor(ctx context.Context, cfg ProcessConfig) (*Supervisor, error) {
	epoch, err := newEpoch()
	if err != nil {
		return nil, err
	}
	if epoch == 0 {
		epoch = 1
	}
	p, c, err := startGeneration(ctx, cfg, epoch)
	if err != nil {
		return nil, err
	}
	loopCtx, cancel := context.WithCancel(context.Background())
	s := &Supervisor{cfg: cfg, descriptor: c.Descriptor(), current: c, process: p, ctx: loopCtx, cancel: cancel, epoch: epoch}
	return s, nil
}

func startGeneration(ctx context.Context, cfg ProcessConfig, epoch uint64) (*Process, *Connector, error) {
	p, err := startWithEpoch(ctx, cfg, epoch)
	if err != nil {
		return nil, nil, err
	}
	c, err := p.ConnectComponent(ctx)
	if err != nil {
		return nil, nil, err
	}
	return p, c, nil
}

func (s *Supervisor) Descriptor() core.Descriptor { return s.descriptor.Clone() }

func (s *Supervisor) Init(ctx context.Context, cfg core.ComponentConfig) error {
	s.mu.Lock()
	if s.closed || s.failed || s.started {
		s.mu.Unlock()
		return errors.New("external connector supervisor cannot initialize")
	}
	s.component = core.ComponentConfig{Data: append([]byte(nil), cfg.Data...)}
	c, p := s.current, s.process
	s.mu.Unlock()
	if err := c.Init(ctx, core.ComponentConfig{Data: append([]byte(nil), cfg.Data...)}); err != nil {
		s.mu.Lock()
		s.failed = true
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	if s.closed || s.started {
		s.mu.Unlock()
		return errors.New("external connector supervisor cannot publish initialized generation")
	}
	s.started = true
	if !s.publishLocked(p, c, s.epoch) {
		s.mu.Unlock()
		return errors.New("external connector supervisor is closed")
	}
	s.mu.Unlock()
	go s.watch(p, c)
	return nil
}

// publishLocked registers the watcher in the same critical section that makes
// the generation visible, so Close cannot Wait before its Add.
func (s *Supervisor) publishLocked(p *Process, c *Connector, epoch uint64) bool {
	if s.closed || s.ctx.Err() != nil {
		return false
	}
	s.watchers.Add(1)
	s.process, s.current, s.epoch, s.ready = p, c, epoch, true
	return true
}

func (s *Supervisor) watch(p *Process, c *Connector) {
	defer s.watchers.Done()
	select {
	case <-s.ctx.Done():
		return
	case <-p.waitDone:
	case <-c.fatal:
	}
	s.mu.Lock()
	if s.process == p {
		s.ready = false
	}
	s.mu.Unlock()
	c.stopForRecovery()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	cleanupErr := p.Shutdown(cleanupCtx, nil)
	cleanupCancel()
	if cleanupErr != nil {
		s.fail()
		return
	}
	for {
		s.mu.Lock()
		restart, admitted := s.budget.admit(time.Now())
		if !admitted {
			s.ready, s.failed = false, true
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		if !waitSupervisor(s.ctx, supervisorBackoff(restart)) {
			return
		}
		s.mu.Lock()
		if s.epoch == ^uint64(0) {
			s.mu.Unlock()
			s.fail()
			return
		}
		nextEpoch := s.epoch + 1
		s.epoch = nextEpoch
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(s.ctx, startupTimeout+initTimeout)
		nextProcess, next, err := startGeneration(ctx, s.cfg, nextEpoch)
		if err == nil && !reflect.DeepEqual(s.descriptor, next.Descriptor()) {
			err = errors.New("replacement connector Descriptor changed")
		}
		if err == nil {
			err = next.Init(ctx, core.ComponentConfig{Data: append([]byte(nil), s.component.Data...)})
		}
		cancel()
		if err != nil {
			if next != nil {
				next.stopForRecovery()
			}
			if nextProcess != nil {
				cleanupCtx, cleanupCancel = context.WithTimeout(context.Background(), 5*time.Second)
				_ = nextProcess.Shutdown(cleanupCtx, nil)
				cleanupCancel()
			}
			continue
		}
		s.mu.Lock()
		if s.closed || s.ctx.Err() != nil {
			s.mu.Unlock()
			_ = next.Close(context.Background())
			return
		}
		select {
		case <-nextProcess.waitDone:
			s.mu.Unlock()
			cleanupCtx, cleanupCancel = context.WithTimeout(context.Background(), 5*time.Second)
			_ = nextProcess.Shutdown(cleanupCtx, nil)
			cleanupCancel()
			continue
		case <-next.fatal:
			s.mu.Unlock()
			cleanupCtx, cleanupCancel = context.WithTimeout(context.Background(), 5*time.Second)
			_ = nextProcess.Shutdown(cleanupCtx, nil)
			cleanupCancel()
			continue
		default:
		}
		if !s.publishLocked(nextProcess, next, nextEpoch) {
			s.mu.Unlock()
			_ = next.Close(context.Background())
			return
		}
		s.mu.Unlock()
		go s.watch(nextProcess, next)
		return
	}
}

func waitSupervisor(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Supervisor) fail() {
	s.mu.Lock()
	s.ready, s.failed = false, true
	s.mu.Unlock()
}

func (s *Supervisor) get() (*Connector, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.ready || s.failed || s.closed || s.current == nil || s.process == nil {
		return s.current, false
	}
	// Failure becomes observable before the watcher goroutine necessarily runs.
	// Never admit into that gap or infer readiness from the stale cached flag.
	select {
	case <-s.process.waitDone:
		return s.current, false
	case <-s.current.fatal:
		return s.current, false
	default:
		return s.current, true
	}
}

func (s *Supervisor) Health(ctx context.Context) core.Health {
	c, ok := s.get()
	if !ok {
		return core.Health{State: core.HealthUnavailable, Diagnostic: "external connector supervisor unavailable"}
	}
	return c.Health(ctx)
}

func (s *Supervisor) Capabilities(ctx context.Context, scope core.CapabilityScope) core.CapabilityResult {
	if c, ok := s.get(); ok {
		return c.Capabilities(ctx, scope)
	}
	return core.CapabilityResult{}
}

func (s *Supervisor) Models(ctx context.Context, q core.ModelQuery, svc core.InvocationServices) (core.ModelsResult, *core.GatewayError) {
	if c, ok := s.get(); ok {
		r, e := c.Models(ctx, q, svc)
		if e != nil && e.Category == core.CategoryUnavailable {
			c.signalFatal()
		}
		return r, e
	}
	return core.ModelsResult{}, errSupervisorUnavailable
}

func (s *Supervisor) EstimateUsage(ctx context.Context, q core.UsageQuery, svc core.InvocationServices) (core.EstimateResult, *core.GatewayError) {
	if c, ok := s.get(); ok {
		r, e := c.EstimateUsage(ctx, q, svc)
		if e != nil && e.Category == core.CategoryUnavailable {
			c.signalFatal()
		}
		return r, e
	}
	return core.EstimateResult{}, errSupervisorUnavailable
}

func (s *Supervisor) Authenticate(ctx context.Context, q core.AuthRequest, svc core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	if c, ok := s.get(); ok {
		r, e := c.Authenticate(ctx, q, svc)
		if e != nil && e.Category == core.CategoryUnavailable {
			c.signalFatal()
		}
		return r, e
	}
	return core.AuthResult{}, errSupervisorUnavailable
}

func (s *Supervisor) Execute(ctx context.Context, req core.ExecutionRequest, scope core.AttemptScope, svc core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	if c, ok := s.get(); ok {
		r, e := c.Execute(ctx, req, scope, svc)
		if r.Stream != nil {
			r.Stream = &supervisedStream{stream: r.Stream, connector: c}
		}
		return r, e
	}
	return core.ExecutionResponse{}, errSupervisorUnavailable
}

type supervisedStream struct {
	stream    core.Stream
	connector *Connector
}

func (s *supervisedStream) Next(ctx context.Context) (core.StreamFrame, error) {
	frame, err := s.stream.Next(ctx)
	var gatewayErr *core.GatewayError
	if errors.As(err, &gatewayErr) && gatewayErr.Category == core.CategoryUnavailable {
		s.connector.signalFatal()
	}
	return frame, err
}

func (s *supervisedStream) Close() error { return s.stream.Close() }

func (s *Supervisor) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closeDone == nil {
		s.closeDone = make(chan struct{})
		s.closed, s.ready = true, false
		s.cancel()
		c, p, done := s.current, s.process, s.closeDone
		go s.closeOwned(c, p, done)
	}
	done := s.closeDone
	s.mu.Unlock()
	select {
	case <-done:
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Supervisor) closeOwned(c *Connector, p *Process, done chan struct{}) {
	limit := s.closeWaitLimit
	if limit <= 0 {
		limit = closeTimeout + 5*time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	var closeErr error
	if c != nil && p != nil {
		_ = c.Close(ctx)
		c.mu.Lock()
		connectorDone := c.closeDone
		c.mu.Unlock()
		if connectorDone != nil {
			<-connectorDone
		}
		c.mu.Lock()
		closeErr = c.closeErr
		c.mu.Unlock()
	}
	s.watchers.Wait()
	if err := ctx.Err(); err != nil {
		closeErr = errors.Join(closeErr, ctx.Err())
	}
	s.mu.Lock()
	s.closeErr = closeErr
	close(done)
	s.mu.Unlock()
}

var _ core.Connector = (*Supervisor)(nil)
