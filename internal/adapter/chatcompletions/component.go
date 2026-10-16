package chatcompletions

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

const protocol = "openai.chat-completions.v1"
const componentID = "pestiroute.chat-completions.native"

type limits struct {
	MaxBodyBytes   int64 `json:"max_body_bytes"`
	MaxHeaderBytes int64 `json:"max_header_bytes"`
}

type HTTPResponse struct {
	Writer  http.ResponseWriter
	Request *http.Request
}

type Adapter struct {
	mu     sync.Mutex
	state  core.HealthState
	closed bool
	limits limits
	ctx    context.Context
	cancel context.CancelFunc
	active sync.WaitGroup
}

var _ core.ProtocolAdapter = (*Adapter)(nil)

func NewAdapter() *Adapter        { return &Adapter{state: core.HealthUnknown} }
func (*Adapter) Protocol() string { return protocol }
func (*Adapter) Descriptor() core.Descriptor {
	return core.Descriptor{ID: componentID, Kind: core.ComponentAdapter, ImplementationVersion: "1.0.0", APIVersions: []core.APIVersion{{Major: 1}}, Protocols: []string{protocol}}.Clone()
}

func (a *Adapter) Init(_ context.Context, c core.ComponentConfig) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.state == core.HealthReady {
		return errors.New("Chat Completions adapter cannot be initialized in its current state")
	}
	var cfg limits
	d := json.NewDecoder(bytes.NewReader(c.Data))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		a.state = core.HealthUnavailable
		return fmt.Errorf("invalid Chat Completions adapter configuration: %w", err)
	}
	if d.Decode(new(any)) != io.EOF || cfg.MaxBodyBytes <= 0 || cfg.MaxHeaderBytes <= 0 {
		a.state = core.HealthUnavailable
		return errors.New("Chat Completions adapter body and header limits must be positive")
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
func (a *Adapter) Capabilities(_ context.Context, s core.CapabilityScope) core.CapabilityResult {
	a.mu.Lock()
	ready := a.state == core.HealthReady && !a.closed
	a.mu.Unlock()
	values := map[core.Capability]core.CapabilityState{}
	if ready && s.Protocol == protocol && s.Mode == core.ModeNative && s.Model != "" {
		for _, c := range []core.Capability{"llm.streaming", "llm.tools", "llm.tools.parallel", "llm.reasoning", "llm.structured_output", "llm.vision", "llm.audio"} {
			values[c] = core.Supported
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
	if a.closed || a.state != core.HealthReady {
		return nil, nil, errors.New("Chat Completions adapter unavailable")
	}
	a.active.Add(1)
	c, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.ctx, cancel)
	return c, func() { stop(); cancel(); a.active.Done() }, nil
}
func (a *Adapter) Decode(ctx context.Context, rq core.ClientRequest) (core.ExecutionRequest, *core.GatewayError) {
	c, done, err := a.begin(ctx)
	if err != nil {
		return core.ExecutionRequest{}, statusError(err)
	}
	defer done()
	r, ok := rq.Transport.(*http.Request)
	if !ok || r == nil {
		return core.ExecutionRequest{}, invalid("Invalid request")
	}
	requestCtx, cancel := context.WithCancel(c)
	stop := context.AfterFunc(r.Context(), cancel)
	defer func() { stop(); cancel() }()
	if r.Body != nil {
		stopClose := context.AfterFunc(requestCtx, func() { _ = r.Body.Close() })
		defer stopClose()
	}
	decoded, gatewayErr := Decode(r.WithContext(requestCtx), a.limits.MaxBodyBytes, a.limits.MaxHeaderBytes)
	if err := requestCtx.Err(); err != nil {
		return core.ExecutionRequest{}, statusError(err)
	}
	return decoded, gatewayErr
}
func (a *Adapter) Encode(ctx context.Context, rs core.ClientResponse, e *core.GatewayError, x core.ExecutionResponse) error {
	c, done, err := a.begin(ctx)
	if err != nil {
		if x.Stream != nil {
			_ = x.Stream.Close()
		}
		return err
	}
	defer done()
	t, ok := rs.Transport.(HTTPResponse)
	if !ok || t.Writer == nil || t.Request == nil {
		if x.Stream != nil {
			_ = x.Stream.Close()
		}
		return errors.New("invalid Chat Completions response transport")
	}
	rctx, cancel := context.WithCancel(c)
	stop := context.AfterFunc(t.Request.Context(), cancel)
	defer func() { stop(); cancel() }()
	return Encode(t.Writer, t.Request.WithContext(rctx), x, e)
}
func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	a.closed = true
	a.state = core.HealthUnavailable
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()
	d := make(chan struct{})
	go func() { a.active.Wait(); close(d) }()
	select {
	case <-d:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func statusError(err error) *core.GatewayError {
	if errors.Is(err, context.DeadlineExceeded) {
		return &core.GatewayError{Code: "request_timeout", Category: core.CategoryTimeout, Message: "Request timed out"}
	}
	if errors.Is(err, context.Canceled) {
		return &core.GatewayError{Code: "request_cancelled", Category: core.CategoryCancelled, Message: "Request cancelled"}
	}
	return &core.GatewayError{Code: "adapter_unavailable", Category: core.CategoryUnavailable, Message: "Chat Completions adapter is unavailable"}
}
