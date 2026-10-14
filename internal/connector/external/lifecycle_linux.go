//go:build linux

package external

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/ipc"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	initTimeout  = 30 * time.Second
	queryTimeout = 2 * time.Second
	closeTimeout = 30 * time.Second
)

// Connector is the lifecycle-only host proxy for one verified child process.
type Connector struct {
	process     *Process
	conn        *grpc.ClientConn
	client      gen.ConnectorClient
	descriptor  core.Descriptor
	epoch       uint64
	invocations *invocationRegistry

	gate               chan struct{}
	mu                 sync.Mutex
	initialized        bool
	unavailable        bool
	closing            bool
	closeStarted       bool
	closeDone          chan struct{}
	closeErr           error
	closeErrorObserved bool
}

// ConnectComponent verifies the peer, protocol, API, and descriptor before
// exposing the lifecycle proxy.
func (p *Process) ConnectComponent(ctx context.Context) (*Connector, error) {
	conn, descriptor, err := p.ConnectConnector(ctx)
	if err != nil {
		return nil, err
	}
	return &Connector{process: p, conn: conn, client: gen.NewConnectorClient(conn), descriptor: descriptor.Clone(), epoch: p.epoch, invocations: newInvocationRegistry(p.epoch), gate: make(chan struct{}, 1)}, nil
}

var _ core.Component = (*Connector)(nil)

func (c *Connector) Descriptor() core.Descriptor { return c.descriptor.Clone() }

func (c *Connector) Init(ctx context.Context, config core.ComponentConfig) error {
	ctx, cancel := boundedContext(ctx, initTimeout)
	defer cancel()
	if err := c.acquire(ctx); err != nil {
		return lifecycleError("Init", err)
	}
	defer c.release()
	c.mu.Lock()
	if c.closing || c.unavailable {
		c.mu.Unlock()
		return errors.New("external connector unavailable")
	}
	if c.initialized {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	stream, err := c.client.Init(ctx)
	if err == nil {
		err = ipc.WriteRecord(ipc.Identity{Epoch: c.epoch, CallID: 1, RecordID: 0, Type: "ComponentConfig"}, ipc.ConfigTo(config), stream.Send)
	}
	if err == nil {
		_, err = stream.CloseAndRecv()
	}
	if err == nil {
		c.mu.Lock()
		c.initialized = true
		c.mu.Unlock()
		return nil
	}
	c.mu.Lock()
	c.unavailable = true
	c.mu.Unlock()
	c.invocations.close()
	primary := lifecycleError("Init", err)
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), closeTimeout)
	defer shutdownCancel()
	shutdownErr := c.process.Shutdown(shutdownCtx, c.closeRPC)
	_ = c.conn.Close()
	return errors.Join(primary, lifecycleError("partial-init cleanup", shutdownErr))
}

func (c *Connector) Health(ctx context.Context) core.Health {
	ctx, cancel := boundedContext(ctx, queryTimeout)
	defer cancel()
	if err := c.acquire(ctx); err != nil {
		return failedHealth(err)
	}
	defer c.release()
	c.mu.Lock()
	if c.closing || c.unavailable {
		c.mu.Unlock()
		return core.Health{State: core.HealthUnavailable, Diagnostic: "external connector unavailable"}
	}
	if !c.initialized {
		c.mu.Unlock()
		return core.Health{State: core.HealthUnknown, Diagnostic: "external connector is not initialized"}
	}
	c.mu.Unlock()
	stream, err := c.client.Health(ctx, lifecycleRequest(c.epoch))
	if err != nil {
		return failedHealth(err)
	}
	var wire gen.Health
	if err = readRecordCall(stream, c.epoch, 1, "Health", &wire); err != nil {
		return failedHealth(err)
	}
	value, err := ipc.HealthFrom(&wire)
	if err != nil {
		return core.Health{State: core.HealthUnavailable, Diagnostic: "external connector returned malformed health"}
	}
	return safeHealth(value)
}

func (c *Connector) Capabilities(ctx context.Context, scope core.CapabilityScope) core.CapabilityResult {
	ctx, cancel := boundedContext(ctx, queryTimeout)
	defer cancel()
	if err := c.acquire(ctx); err != nil {
		return core.CapabilityResult{}
	}
	defer c.release()
	c.mu.Lock()
	if c.closing || c.unavailable || !c.initialized || scope.Protocol == "" ||
		(scope.Mode != core.ModeNative && scope.Mode != core.ModeTranslation) {
		c.mu.Unlock()
		return core.CapabilityResult{}
	}
	c.mu.Unlock()
	stream, err := c.client.Capabilities(ctx)
	if err != nil {
		return core.CapabilityResult{}
	}
	if err = ipc.WriteRecord(ipc.Identity{Epoch: c.epoch, CallID: 1, RecordID: 0, Type: "CapabilityScope"}, ipc.ScopeTo(scope), stream.Send); err != nil {
		return core.CapabilityResult{}
	}
	if err = stream.CloseSend(); err != nil {
		return core.CapabilityResult{}
	}
	var wire gen.CapabilityResult
	if err = readBidiRecord(stream, c.epoch, "CapabilityResult", &wire); err != nil {
		return core.CapabilityResult{}
	}
	result, err := ipc.CapabilityResultFrom(&wire)
	if err != nil {
		return core.CapabilityResult{}
	}
	return result
}

func (c *Connector) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closing = true
	c.invocations.close()
	if !c.closeStarted {
		c.closeStarted = true
		c.closeDone = make(chan struct{})
		done := c.closeDone
		go c.closeOwned(ctx, done)
	}
	done := c.closeDone
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-done:
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.closeErrorObserved {
			return nil
		}
		c.closeErrorObserved = true
		return c.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Connector) closeOwned(ctx context.Context, done chan struct{}) {
	bounded, cancel := boundedContext(ctx, closeTimeout)
	defer cancel()
	acquired := c.acquire(bounded) == nil
	var err error
	if acquired {
		defer c.release()
		c.mu.Lock()
		alreadyUnavailable := c.unavailable
		c.mu.Unlock()
		if !alreadyUnavailable {
			shutdownErr := c.process.Shutdown(bounded, c.closeRPC)
			connErr := c.conn.Close()
			err = errors.Join(lifecycleError("Close", shutdownErr), lifecycleError("connection close", connErr))
		}
	} else {
		// Even when admission could not drain before the caller's deadline, the
		// supervisor still owns terminating/reaping the process group.
		shutdownErr := c.process.Shutdown(bounded, c.closeRPC)
		connErr := c.conn.Close()
		err = errors.Join(lifecycleError("Close", shutdownErr), lifecycleError("connection close", connErr))
	}
	c.mu.Lock()
	c.closeErr = err
	close(done)
	c.mu.Unlock()
}

func (c *Connector) closeRPC(ctx context.Context) error {
	_, err := c.client.Close(ctx, lifecycleRequest(c.epoch))
	return err
}

func (c *Connector) acquire(ctx context.Context) error {
	select {
	case c.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Connector) release() { <-c.gate }

func lifecycleRequest(epoch uint64) *gen.RecordFragment {
	return &gen.RecordFragment{Epoch: epoch, CallId: 1}
}

func boundedContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, timeout)
}

func lifecycleError(operation string, err error) error {
	if err == nil {
		return nil
	}
	code := status.Code(err)
	if code == codes.Unknown {
		code = codes.Internal
	}
	return fmt.Errorf("external connector %s failed (%s)", operation, code)
}

func failedHealth(err error) core.Health {
	if status.Code(err) == codes.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) {
		return core.Health{State: core.HealthUnavailable, Diagnostic: "external connector health timed out"}
	}
	return core.Health{State: core.HealthUnavailable, Diagnostic: "external connector health failed"}
}

const maxHealthDiagnosticBytes = 128

// Child diagnostics are untrusted and may contain secrets. Expose only a
// fixed state label; these constants are valid UTF-8 and stay below the bound.
func safeHealth(value core.Health) core.Health {
	var diagnostic string
	switch value.State {
	case core.HealthReady:
		diagnostic = "external connector reports ready"
	case core.HealthUnavailable:
		diagnostic = "external connector reports unavailable"
	default:
		diagnostic = "external connector health is unknown"
	}
	if len(diagnostic) > maxHealthDiagnosticBytes {
		diagnostic = "connector health status unavailable"
	}
	value.Diagnostic = diagnostic
	return value
}

func readBidiRecord(stream grpc.BidiStreamingClient[gen.RecordFragment, gen.RecordFragment], epoch uint64, typ string, dst proto.Message) error {
	a := ipc.NewAssembler(ipc.Identity{Epoch: epoch, CallID: 1, RecordID: 0, Type: typ})
	for {
		if a.Complete() {
			if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
				if err == nil {
					return ipc.ErrMalformed
				}
				return err
			}
			return a.Decode(dst)
		}
		fragment, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return a.Decode(dst)
		}
		if err != nil {
			return err
		}
		if err := a.Add(fragment); err != nil {
			return err
		}
	}
}
