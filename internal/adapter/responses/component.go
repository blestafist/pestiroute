package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/blestafist/pestiroute/internal/core"
)

const componentID = "pestiroute.responses.native"

type adapterConfig struct {
	MaxBodyBytes   int64 `json:"max_body_bytes"`
	MaxHeaderBytes int64 `json:"max_header_bytes"`
}

// HTTPResponse is the adapter-owned transport passed to Encode.
type HTTPResponse struct {
	Writer  http.ResponseWriter
	Request *http.Request
}

// Adapter manages the native Responses protocol boundary.
type Adapter struct {
	mu     sync.Mutex
	state  core.HealthState
	closed bool
	limits adapterConfig
	ctx    context.Context
	cancel context.CancelFunc
	active sync.WaitGroup
}

var _ core.ProtocolAdapter = (*Adapter)(nil)

func NewAdapter() *Adapter { return &Adapter{state: core.HealthUnknown} }

func (*Adapter) Descriptor() core.Descriptor {
	return core.Descriptor{
		ID: componentID, Kind: core.ComponentAdapter, ImplementationVersion: "1.0.0",
		APIVersions: []core.APIVersion{{Major: 1, Minor: 0}}, Protocols: []string{protocol},
	}.Clone()
}

func (*Adapter) Protocol() string { return protocol }

func (a *Adapter) Init(_ context.Context, config core.ComponentConfig) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.state == core.HealthReady {
		return errors.New("Responses adapter cannot be initialized in its current state")
	}
	var cfg adapterConfig
	d := json.NewDecoder(bytes.NewReader(config.Data))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		a.state = core.HealthUnavailable
		return fmt.Errorf("invalid Responses adapter configuration: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		a.state = core.HealthUnavailable
		return errors.New("invalid Responses adapter configuration: trailing data")
	}
	if cfg.MaxBodyBytes <= 0 || cfg.MaxHeaderBytes <= 0 {
		a.state = core.HealthUnavailable
		return errors.New("Responses adapter body and header limits must be positive")
	}
	a.limits = cfg
	a.ctx, a.cancel = context.WithCancel(context.Background())
	a.state = core.HealthReady
	return nil
}

func (a *Adapter) Health(context.Context) core.Health {
	a.mu.Lock()
	defer a.mu.Unlock()
	return core.Health{State: a.state}
}

func (a *Adapter) Capabilities(_ context.Context, scope core.CapabilityScope) core.CapabilityResult {
	a.mu.Lock()
	ready := a.state == core.HealthReady && !a.closed
	a.mu.Unlock()
	values := make(map[core.Capability]core.CapabilityState)
	if ready && scope.Protocol == protocol && scope.Mode == "native" && scope.Model != "" {
		for _, capability := range []core.Capability{
			"llm.streaming", "llm.tools", "llm.tools.parallel", "llm.reasoning", "llm.structured_output", "llm.vision", "llm.audio",
		} {
			values[capability] = core.Supported
		}
	}
	return core.CapabilityResult{Values: values}
}

func (a *Adapter) begin(ctx context.Context) (context.Context, func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if a.state != core.HealthReady || a.closed {
		return nil, nil, errors.New("Responses adapter is not ready")
	}
	a.active.Add(1)
	callCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.ctx, cancel)
	return callCtx, func() { stop(); cancel(); a.active.Done() }, nil
}

func (a *Adapter) Decode(ctx context.Context, request core.ClientRequest) (core.ExecutionRequest, *core.GatewayError) {
	callCtx, done, err := a.begin(ctx)
	if err != nil {
		return core.ExecutionRequest{}, adapterGatewayError(err)
	}
	defer done()
	r, ok := request.Transport.(*http.Request)
	if !ok || r == nil {
		return core.ExecutionRequest{}, invalid("Invalid request")
	}
	requestCtx, cancel := context.WithCancel(callCtx)
	stopRequest := context.AfterFunc(r.Context(), cancel)
	defer func() { stopRequest(); cancel() }()
	if r.Body != nil {
		stopBodyClose := context.AfterFunc(requestCtx, func() { _ = r.Body.Close() })
		defer stopBodyClose()
	}
	decoded, gatewayErr := Decode(r.WithContext(requestCtx), a.limits.MaxBodyBytes, a.limits.MaxHeaderBytes)
	if err := requestCtx.Err(); err != nil {
		return core.ExecutionRequest{}, adapterGatewayError(err)
	}
	return decoded, gatewayErr
}

func (a *Adapter) Encode(ctx context.Context, response core.ClientResponse, gatewayErr *core.GatewayError, execution core.ExecutionResponse) error {
	callCtx, done, err := a.begin(ctx)
	if err != nil {
		if execution.Stream != nil {
			_ = execution.Stream.Close()
		}
		return err
	}
	defer done()
	transport, ok := response.Transport.(HTTPResponse)
	if !ok || transport.Writer == nil || transport.Request == nil {
		if execution.Stream != nil {
			_ = execution.Stream.Close()
		}
		return errors.New("invalid Responses response transport")
	}
	requestCtx, cancel := context.WithCancel(callCtx)
	stopRequest := context.AfterFunc(transport.Request.Context(), cancel)
	defer func() { stopRequest(); cancel() }()
	return Encode(transport.Writer, transport.Request.WithContext(requestCtx), execution, gatewayErr)
}

func adapterGatewayError(err error) *core.GatewayError {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &core.GatewayError{Code: "request_timeout", Category: core.CategoryTimeout, Message: "Request timed out"}
	case errors.Is(err, context.Canceled):
		return &core.GatewayError{Code: "request_cancelled", Category: core.CategoryCancelled, Message: "Request cancelled"}
	default:
		return &core.GatewayError{Code: "adapter_unavailable", Category: core.CategoryUnavailable, Message: "Responses adapter is unavailable"}
	}
}

func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	a.closed = true
	a.state = core.HealthUnavailable
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()
	done := make(chan struct{})
	go func() { a.active.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
