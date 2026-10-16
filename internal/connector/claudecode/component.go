package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/blestafist/pestiroute/internal/core"
)

const (
	componentID = "pestiroute.claudecode.messages"
	protocol    = "openai.responses.v1"
	profile     = "claude-code-messages-v1"
)

type componentConfig struct {
	Profile string `json:"profile"`
}

type Connector struct {
	mu     sync.Mutex
	state  core.HealthState
	closed bool
}

var _ core.Connector = (*Connector)(nil)

func NewConnector() *Connector { return &Connector{state: core.HealthUnavailable} }

func (c *Connector) Descriptor() core.Descriptor {
	return core.Descriptor{
		ID: componentID, Kind: core.ComponentConnector, ImplementationVersion: "1.0.0",
		APIVersions:   []core.APIVersion{{Major: 1, Minor: 0}},
		Protocols:     []string{protocol},
		ConnectorType: "api", AuthMethods: []string{"oauth"},
	}.Clone()
}

func (c *Connector) Init(ctx context.Context, config core.ComponentConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.state == core.HealthReady {
		return errors.New("Claude Code connector cannot be initialized in its current state")
	}
	c.state = core.HealthUnavailable
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("initialize Claude Code connector: %w", err)
	}
	var cfg componentConfig
	d := json.NewDecoder(bytes.NewReader(config.Data))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return fmt.Errorf("invalid Claude Code connector configuration: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("invalid Claude Code connector configuration: trailing data")
	}
	if cfg.Profile != profile {
		return fmt.Errorf("Claude Code connector profile must be %q", profile)
	}
	c.state = core.HealthReady
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
	return core.CapabilityResult{Values: map[core.Capability]core.CapabilityState{
		"llm.streaming": core.Unknown,
		"llm.tools":     core.Unknown,
		"llm.reasoning": core.Unknown,
		"auth.oauth":    core.Unknown,
		"usage.exact":   core.Unknown,
	}}
}

func (c *Connector) Close(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.state = core.HealthUnavailable
	return nil
}

func (c *Connector) Execute(ctx context.Context, req core.ExecutionRequest, scope core.AttemptScope, _ core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	c.mu.Lock()
	ready := c.state == core.HealthReady
	c.mu.Unlock()
	if !ready {
		return core.ExecutionResponse{}, unavailableError()
	}
	if ge := contextError(ctx); ge != nil {
		return core.ExecutionResponse{}, ge
	}
	if req.Payload.Protocol != protocol {
		return core.ExecutionResponse{}, &core.GatewayError{Code: "unsupported_protocol", Category: core.CategoryUnsupportedFeature, Message: "Unsupported response protocol"}
	}
	if scope.Mode != core.ModeTranslation {
		return core.ExecutionResponse{}, &core.GatewayError{Code: "unsupported_mode", Category: core.CategoryUnsupportedFeature, Message: "Unsupported execution mode"}
	}
	return core.ExecutionResponse{}, &core.GatewayError{
		Code: "connector_unavailable", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown,
		Message: "Claude Code connector execution is unavailable",
	}
}

func (c *Connector) Authenticate(ctx context.Context, _ core.AuthRequest, _ core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	c.mu.Lock()
	ready := c.state == core.HealthReady
	c.mu.Unlock()
	if !ready {
		return core.AuthResult{}, unavailableError()
	}
	if ge := contextError(ctx); ge != nil {
		return core.AuthResult{}, ge
	}
	return core.AuthResult{Supported: false}, nil
}

func (c *Connector) Models(ctx context.Context, query core.ModelQuery, _ core.InvocationServices) (core.ModelsResult, *core.GatewayError) {
	if ge := c.validateQuery(ctx, query.Protocol, query.Mode); ge != nil {
		return core.ModelsResult{}, ge
	}
	return core.ModelsResult{Supported: false}, nil
}

func (c *Connector) EstimateUsage(ctx context.Context, query core.UsageQuery, _ core.InvocationServices) (core.EstimateResult, *core.GatewayError) {
	if ge := c.validateQuery(ctx, query.Protocol, query.Mode); ge != nil {
		return core.EstimateResult{}, ge
	}
	if query.MaxBodyBytes <= 0 || int64(len(query.Payload.Body)) > query.MaxBodyBytes {
		return core.EstimateResult{}, &core.GatewayError{Code: "invalid_request", Category: core.CategoryInvalidRequest, Message: "Usage estimate body limit is invalid or exceeded"}
	}
	return core.EstimateResult{Supported: false, Known: false}, nil
}

func (c *Connector) validateQuery(ctx context.Context, requestProtocol, mode string) *core.GatewayError {
	c.mu.Lock()
	ready := c.state == core.HealthReady
	c.mu.Unlock()
	if !ready {
		return unavailableError()
	}
	if ge := contextError(ctx); ge != nil {
		return ge
	}
	if requestProtocol != protocol {
		return &core.GatewayError{Code: "unsupported_protocol", Category: core.CategoryUnsupportedFeature, Message: "Unsupported response protocol"}
	}
	if mode != core.ModeTranslation {
		return &core.GatewayError{Code: "unsupported_mode", Category: core.CategoryUnsupportedFeature, Message: "Unsupported execution mode"}
	}
	return nil
}

func contextError(ctx context.Context) *core.GatewayError {
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return &core.GatewayError{Code: "request_timeout", Category: core.CategoryTimeout, Message: "Connector request timed out"}
		}
		return &core.GatewayError{Code: "request_cancelled", Category: core.CategoryCancelled, Message: "Connector request was cancelled"}
	}
	return nil
}

func unavailableError() *core.GatewayError {
	return &core.GatewayError{Code: "connector_unavailable", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "Claude Code connector is unavailable"}
}
