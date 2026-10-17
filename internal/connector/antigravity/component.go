package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sync"

	"github.com/blestafist/pestiroute/internal/core"
)

const (
	componentID = "pestiroute.antigravity.v1internal"
	profile     = "antigravity-v1internal"
	protocol    = "openai.responses.v1"
)

var projectPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}[a-z0-9]$|^[a-z]$`)

type config struct {
	Profile string `json:"profile"`
	Project string `json:"project"`
}

type Connector struct {
	mu     sync.Mutex
	state  core.HealthState
	closed bool
	cfg    config
}

var _ core.Connector = (*Connector)(nil)

func NewConnector() *Connector { return &Connector{state: core.HealthUnavailable} }

func (c *Connector) Descriptor() core.Descriptor {
	return core.Descriptor{ID: componentID, Kind: core.ComponentConnector, ImplementationVersion: "1.0.0", APIVersions: []core.APIVersion{{Major: 1}}, Protocols: []string{protocol}, Operations: []string{"execute"}, ConnectorType: "api", AuthMethods: []string{"oauth"}}.Clone()
}

func (c *Connector) Init(ctx context.Context, in core.ComponentConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = core.HealthUnavailable
	if c.closed {
		return errors.New("Antigravity connector is closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var cfg config
	if !uniqueJSON(in.Data) {
		return errors.New("invalid Antigravity configuration")
	}
	d := json.NewDecoder(bytes.NewReader(in.Data))
	d.DisallowUnknownFields()
	if d.Decode(&cfg) != nil || d.Decode(new(any)) != io.EOF || cfg.Profile != profile || !projectPattern.MatchString(cfg.Project) {
		return errors.New("invalid Antigravity profile or project")
	}
	c.cfg, c.state = cfg, core.HealthReady
	return nil
}

func (c *Connector) Health(context.Context) core.Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	return core.Health{State: c.state}
}
func (c *Connector) Capabilities(_ context.Context, scope core.CapabilityScope) core.CapabilityResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != core.HealthReady || scope.Protocol != protocol || scope.Mode != core.ModeTranslation {
		return core.CapabilityResult{}
	}
	return core.CapabilityResult{Values: map[core.Capability]core.CapabilityState{"llm.streaming": core.Unknown, "auth.oauth": core.Unknown, "usage.exact": core.Unknown}}
}
func (c *Connector) Close(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.state = core.HealthUnavailable
	return nil
}
func (c *Connector) Models(ctx context.Context, q core.ModelQuery, _ core.InvocationServices) (core.ModelsResult, *core.GatewayError) {
	if e := c.validate(ctx, q.Protocol, q.Mode); e != nil {
		return core.ModelsResult{}, e
	}
	return core.ModelsResult{Supported: false}, nil
}
func (c *Connector) EstimateUsage(ctx context.Context, q core.UsageQuery, _ core.InvocationServices) (core.EstimateResult, *core.GatewayError) {
	if e := c.validate(ctx, q.Protocol, q.Mode); e != nil {
		return core.EstimateResult{}, e
	}
	if q.MaxBodyBytes <= 0 || int64(len(q.Payload.Body)) > q.MaxBodyBytes {
		return core.EstimateResult{}, invalid("invalid_request")
	}
	return core.EstimateResult{Supported: false, Known: false}, nil
}
func (c *Connector) Authenticate(ctx context.Context, _ core.AuthRequest, _ core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	if err := ctx.Err(); err != nil {
		return core.AuthResult{}, contextError(err)
	}
	return core.AuthResult{Supported: false}, nil
}
func (c *Connector) validate(ctx context.Context, p, m string) *core.GatewayError {
	c.mu.Lock()
	ready := c.state == core.HealthReady
	c.mu.Unlock()
	if !ready {
		return unavailable()
	}
	if err := ctx.Err(); err != nil {
		return contextError(err)
	}
	if p != protocol || m != core.ModeTranslation {
		return &core.GatewayError{Code: "unsupported_protocol_or_mode", Category: core.CategoryUnsupportedFeature, Message: "Unsupported execution protocol or mode"}
	}
	return nil
}
func invalid(code string) *core.GatewayError {
	return &core.GatewayError{Code: code, Category: core.CategoryInvalidRequest, RetryDisposition: core.RetryUnsafe, Message: "Antigravity request is invalid"}
}
func unavailable() *core.GatewayError {
	return &core.GatewayError{Code: "connector_unavailable", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "Antigravity connector is unavailable"}
}
func contextError(err error) *core.GatewayError {
	if errors.Is(err, context.DeadlineExceeded) {
		return &core.GatewayError{Code: "request_timeout", Category: core.CategoryTimeout, Message: "Connector request timed out"}
	}
	return &core.GatewayError{Code: "request_cancelled", Category: core.CategoryCancelled, Message: "Connector request was cancelled"}
}
