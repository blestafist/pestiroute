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
	controlConn *grpc.ClientConn
	client      gen.ConnectorClient
	control     gen.ConnectorClient
	descriptor  core.Descriptor
	epoch       uint64
	invocations *invocationRegistry
	hostServer  *grpc.Server
	hostCancel  context.CancelFunc
	fatal       chan struct{}
	fatalOnce   sync.Once

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
	controlConn, controlClient, err := p.connectControl(ctx)
	if err != nil {
		_ = conn.Close()
		return nil, p.abort(fmt.Errorf("connector flow-control connection: %w", err))
	}
	serviceCtx, cancel := context.WithCancel(context.Background())
	registry := newInvocationRegistry(p.epoch)
	registry.secrets = p.secrets
	hostServer := p.serveHostServices(serviceCtx, registry, controlClient)
	c := &Connector{process: p, conn: conn, controlConn: controlConn, client: gen.NewConnectorClient(conn), control: controlClient, descriptor: descriptor.Clone(), epoch: p.epoch, invocations: registry, hostServer: hostServer, hostCancel: cancel, fatal: make(chan struct{}), gate: make(chan struct{}, 1)}
	probe := &gen.FlowIdentity{Epoch: p.epoch, InvocationId: "probe", Operation: "Execute", CallId: 1, Direction: "host_to_child", Sequence: 1}
	response, err := controlClient.FlowControl(ctx, &gen.FlowControlRequest{Kind: gen.FlowControlKind_FLOW_CONTROL_PROBE, Identity: probe})
	if err != nil || response == nil || response.Kind != gen.FlowControlKind_FLOW_CONTROL_PROBE || !proto.Equal(response.Identity, probe) {
		cancel()
		hostServer.Stop()
		_ = controlConn.Close()
		_ = conn.Close()
		if err != nil {
			return nil, p.abort(fmt.Errorf("connector flow-control handshake: %w", err))
		}
		return nil, p.abort(errors.New("child does not implement required flow-control protocol"))
	}
	return c, nil
}

func (c *Connector) signalFatal() {
	if c.fatal != nil {
		c.fatalOnce.Do(func() { close(c.fatal) })
	}
}

var _ core.Component = (*Connector)(nil)

func (c *Connector) Descriptor() core.Descriptor { return c.descriptor.Clone() }

func (c *Connector) stopForRecovery() {
	c.mu.Lock()
	c.unavailable = true
	c.mu.Unlock()
	c.invocations.close()
	c.hostCancel()
	c.hostServer.Stop()
	_ = c.conn.Close()
	_ = c.controlConn.Close()
}

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
	c.hostCancel()
	c.hostServer.Stop()
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
		if ctx.Err() == nil {
			c.signalFatal()
		}
		return failedHealth(err)
	}
	var wire gen.Health
	if err = readRecordCall(stream, c.epoch, 1, "Health", &wire); err != nil {
		if ctx.Err() == nil {
			c.signalFatal()
		}
		return failedHealth(err)
	}
	value, err := ipc.HealthFrom(&wire)
	if err != nil {
		c.signalFatal()
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
		c.fatalUnlessCanceled(ctx, err)
		return core.CapabilityResult{}
	}
	if err = ipc.WriteRecord(ipc.Identity{Epoch: c.epoch, CallID: 1, RecordID: 0, Type: "CapabilityScope"}, ipc.ScopeTo(scope), stream.Send); err != nil {
		c.fatalUnlessCanceled(ctx, err)
		return core.CapabilityResult{}
	}
	if err = stream.CloseSend(); err != nil {
		c.fatalUnlessCanceled(ctx, err)
		return core.CapabilityResult{}
	}
	var wire gen.CapabilityResult
	if err = readBidiRecord(stream, c.epoch, "CapabilityResult", &wire); err != nil {
		c.fatalUnlessCanceled(ctx, err)
		return core.CapabilityResult{}
	}
	result, err := ipc.CapabilityResultFrom(&wire)
	if err != nil {
		c.fatalUnlessCanceled(ctx, err)
		return core.CapabilityResult{}
	}
	return result
}

func (c *Connector) fatalUnlessCanceled(ctx context.Context, err error) {
	if err != nil && ctx.Err() == nil {
		c.signalFatal()
	}
}

// Models forwards one scoped model query and returns only a fully validated result.
func (c *Connector) Models(ctx context.Context, query core.ModelQuery, _ core.InvocationServices) (core.ModelsResult, *core.GatewayError) {
	ctx, cancel := boundedContext(ctx, queryTimeout)
	defer cancel()
	if err := c.acquire(ctx); err != nil {
		return core.ModelsResult{}, queryGatewayError(err, "models")
	}
	defer c.release()
	c.mu.Lock()
	valid := !c.closing && !c.unavailable && c.initialized && query.Protocol != "" && validMode(query.Mode)
	c.mu.Unlock()
	if !valid {
		return core.ModelsResult{}, queryGatewayError(errors.New("invalid or unavailable query"), "models")
	}
	stream, err := c.client.Models(ctx)
	if err == nil {
		err = ipc.WriteRecord(ipc.Identity{Epoch: c.epoch, CallID: 1, RecordID: 0, Type: "ModelQuery"}, ipc.ModelQueryTo(query), stream.Send)
	}
	if err == nil {
		err = stream.CloseSend()
	}
	var wire gen.ModelsResult
	if err == nil {
		err = readBidiRecord(stream, c.epoch, "ModelsResult", &wire)
	}
	if err != nil {
		c.fatalUnlessCanceled(ctx, err)
		return core.ModelsResult{}, queryGatewayError(err, "models")
	}
	result, err := ipc.ModelsResultFrom(&wire)
	if err != nil {
		c.fatalUnlessCanceled(ctx, err)
		return core.ModelsResult{}, queryGatewayError(err, "models")
	}
	return result, nil
}

// EstimateUsage sends query metadata as fragments and its opaque body as
// separately bounded chunks; Core has already admitted this body against the route limit.
func (c *Connector) EstimateUsage(ctx context.Context, query core.UsageQuery, _ core.InvocationServices) (core.EstimateResult, *core.GatewayError) {
	ctx, cancel := boundedContext(ctx, queryTimeout)
	defer cancel()
	if err := c.acquire(ctx); err != nil {
		return core.EstimateResult{}, queryGatewayError(err, "estimate")
	}
	defer c.release()
	c.mu.Lock()
	valid := !c.closing && !c.unavailable && c.initialized && query.Protocol != "" && validMode(query.Mode)
	c.mu.Unlock()
	if !valid {
		return core.EstimateResult{}, queryGatewayError(errors.New("invalid or unavailable query"), "estimate")
	}
	if query.MaxBodyBytes < 0 || int64(len(query.Payload.Body)) > query.MaxBodyBytes {
		return core.EstimateResult{}, &core.GatewayError{Code: "invalid_request", Category: core.CategoryInvalidRequest, RetryDisposition: core.RetryUnsafe, Message: "Estimate request body exceeds the admitted route limit"}
	}
	stream, err := c.client.EstimateUsage(ctx)
	if err == nil {
		id := ipc.Identity{Epoch: c.epoch, CallID: 1, RecordID: 0, Type: "UsageQuery"}
		err = ipc.WriteRecord(id, ipc.UsageQueryTo(query), func(f *gen.RecordFragment) error {
			return stream.Send(&gen.EstimateFrame{Epoch: c.epoch, CallId: 1, Payload: &gen.EstimateFrame_Record{Record: f}})
		})
	}
	if err == nil {
		err = ipc.WriteBody(query.Payload.Body, uint64(query.MaxBodyBytes), func(body []byte) error {
			chunk, e := ipc.BodyChunkTo(body)
			if e != nil {
				return e
			}
			return stream.Send(&gen.EstimateFrame{Epoch: c.epoch, CallId: 1, Payload: &gen.EstimateFrame_Body{Body: chunk}})
		})
	}
	if err == nil {
		err = stream.CloseSend()
	}
	var wire gen.EstimateResult
	if err == nil {
		err = readEstimateRecord(stream, c.epoch, &wire)
	}
	if err != nil {
		c.fatalUnlessCanceled(ctx, err)
		return core.EstimateResult{}, queryGatewayError(err, "estimate")
	}
	result, err := ipc.EstimateFrom(&wire)
	if err != nil {
		c.fatalUnlessCanceled(ctx, err)
		return core.EstimateResult{}, queryGatewayError(err, "estimate")
	}
	return result, nil
}

// Authenticate forwards the runtime-owned account/action and opaque continuation
// state; candidate credentials remain unpersisted until Core acknowledges CAS.
func (c *Connector) Authenticate(ctx context.Context, req core.AuthRequest, services core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	ctx, cancel := boundedContext(ctx, queryTimeout)
	defer cancel()
	if err := c.acquire(ctx); err != nil {
		return core.AuthResult{}, queryGatewayError(err, "authenticate")
	}
	defer c.release()
	c.mu.Lock()
	valid := !c.closing && !c.unavailable && c.initialized && req.AccountID != "" && (req.Action == "start" || req.Action == "continue" || req.Action == "refresh")
	c.mu.Unlock()
	if !valid {
		return core.AuthResult{}, &core.GatewayError{Code: "invalid_request", Category: core.CategoryInvalidRequest, RetryDisposition: core.RetryUnsafe, Message: "Invalid or unavailable authentication request"}
	}
	invocationID, invokeCtx, err := c.invocations.beginWithLogger(ctx, req.AccountID, services.Credentials, services.Transport, services.Logger)
	if err != nil {
		return core.AuthResult{}, queryGatewayError(err, "authenticate")
	}
	defer c.invocations.revoke(c.epoch, invocationID)
	stream, err := c.client.Authenticate(invokeCtx)
	if err == nil {
		err = ipc.WriteRecord(ipc.Identity{Epoch: c.epoch, CallID: 1, RecordID: 0, Type: "AuthRequest"}, ipc.AuthRequestTo(req), func(fragment *gen.RecordFragment) error {
			fragment.InvocationId = invocationID
			return stream.Send(fragment)
		})
	}
	if err == nil {
		err = stream.CloseSend()
	}
	var wire gen.AuthResult
	if err == nil {
		err = readInvocationRecord(stream, c.epoch, invocationID, "AuthResult", &wire)
	}
	if err != nil {
		c.fatalUnlessCanceled(ctx, err)
		return core.AuthResult{}, queryGatewayError(err, "authenticate")
	}
	result, err := ipc.AuthResultFrom(&wire)
	if err != nil {
		c.fatalUnlessCanceled(ctx, err)
		return core.AuthResult{}, queryGatewayError(err, "authenticate")
	}
	return result, nil
}

func validMode(mode string) bool { return mode == core.ModeNative || mode == core.ModeTranslation }

func queryGatewayError(err error, operation string) *core.GatewayError {
	if errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled {
		return &core.GatewayError{Code: operation + "_cancelled", Category: core.CategoryCancelled, RetryDisposition: core.RetryUnknown, Message: "External connector query was cancelled"}
	}
	if errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded {
		return &core.GatewayError{Code: "connector_timeout", Category: core.CategoryTimeout, RetryDisposition: core.RetryUnknown, Message: "External connector query timed out"}
	}
	return &core.GatewayError{Code: "connector_unavailable", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "External connector query failed"}
}

func readEstimateRecord(stream grpc.BidiStreamingClient[gen.EstimateFrame, gen.RecordFragment], epoch uint64, dst proto.Message) error {
	a := ipc.NewAssembler(ipc.Identity{Epoch: epoch, CallID: 1, RecordID: 0, Type: "EstimateResult"})
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

func (c *Connector) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closing = true
	c.invocations.close()
	c.hostCancel()
	c.hostServer.Stop()
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
			controlErr := c.controlConn.Close()
			err = errors.Join(lifecycleError("Close", shutdownErr), lifecycleError("connection close", connErr), lifecycleError("control connection close", controlErr))
		}
	} else {
		// Even when admission could not drain before the caller's deadline, the
		// supervisor still owns terminating/reaping the process group.
		shutdownErr := c.process.Shutdown(bounded, c.closeRPC)
		connErr := c.conn.Close()
		controlErr := c.controlConn.Close()
		err = errors.Join(lifecycleError("Close", shutdownErr), lifecycleError("connection close", connErr), lifecycleError("control connection close", controlErr))
	}
	c.mu.Lock()
	c.closeErr = err
	close(done)
	c.mu.Unlock()
}

func (c *Connector) closeRPC(ctx context.Context) error {
	id := &gen.FlowIdentity{Epoch: c.epoch, InvocationId: "process", Operation: "Execute", CallId: 1, Direction: "host_to_child", Sequence: 1}
	_, controlErr := controlCall(ctx, c.invocations.control, c.control, controlUrgent, &gen.FlowControlRequest{Kind: gen.FlowControlKind_FLOW_CONTROL_CLOSE, Identity: id})
	_, err := c.client.Close(ctx, lifecycleRequest(c.epoch))
	return errors.Join(controlErr, err)
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

func readInvocationRecord(stream grpc.BidiStreamingClient[gen.RecordFragment, gen.RecordFragment], epoch uint64, invocationID, typ string, dst proto.Message) error {
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
		if fragment == nil || fragment.InvocationId != invocationID {
			return ipc.ErrMalformed
		}
		if err := a.Add(fragment); err != nil {
			return err
		}
	}
}
