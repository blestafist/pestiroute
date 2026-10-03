package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/blestafist/pestiroute/internal/core"
)

const (
	componentID = "pestiroute.anthropic.messages"
	protocol    = "openai.responses.v1"
)

type componentConfig struct {
	Model     string `json:"model"`
	AccountID string `json:"account_id"`
}

// Connector owns only non-secret configuration and local lifecycle state.
type Connector struct {
	mu        sync.Mutex
	model     string
	accountID string
	state     core.HealthState
	closed    bool
}

var _ core.Connector = (*Connector)(nil)

func NewConnector() *Connector { return &Connector{state: core.HealthUnknown} }

func (c *Connector) Descriptor() core.Descriptor {
	return core.Descriptor{
		ID: componentID, Kind: core.ComponentConnector, ImplementationVersion: "1.0.0",
		APIVersions:   []core.APIVersion{{Major: 1, Minor: 0}},
		Protocols:     []string{protocol},
		Operations:    []string{"execute", "models", "estimate_usage", "authenticate"},
		ConnectorType: "api", AuthMethods: []string{"bearer"},
	}.Clone()
}

func (c *Connector) Init(ctx context.Context, config core.ComponentConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.state == core.HealthReady {
		return errors.New("Anthropic connector cannot be initialized in its current state")
	}
	c.state = core.HealthUnavailable
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("initialize Anthropic connector: %w", err)
	}
	var cfg componentConfig
	d := json.NewDecoder(strings.NewReader(string(config.Data)))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return fmt.Errorf("invalid Anthropic connector configuration: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("invalid Anthropic connector configuration: trailing data")
	}
	if strings.TrimSpace(cfg.Model) == "" || strings.TrimSpace(cfg.AccountID) == "" {
		return errors.New("Anthropic connector model and account_id are required")
	}
	c.model, c.accountID = cfg.Model, cfg.AccountID
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
	if c.state != core.HealthReady || scope.Protocol != protocol || scope.Mode != core.ModeTranslation ||
		scope.Model != c.model || scope.AccountID != c.accountID {
		return core.CapabilityResult{}
	}
	// Provider entitlement, feature support, and inference readiness are unverified.
	return core.CapabilityResult{}
}

func (c *Connector) Models(_ context.Context, query core.ModelQuery, _ core.InvocationServices) (core.ModelsResult, *core.GatewayError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != core.HealthReady || query.Protocol != protocol || query.Mode != core.ModeTranslation || query.AccountID != c.accountID {
		return core.ModelsResult{}, nil
	}
	return core.ModelsResult{Supported: true, Models: []core.ModelInfo{{ID: c.model, Capabilities: map[core.Capability]core.CapabilityState{}}}}, nil
}

func (c *Connector) EstimateUsage(_ context.Context, query core.UsageQuery, _ core.InvocationServices) (core.EstimateResult, *core.GatewayError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != core.HealthReady {
		return core.EstimateResult{}, &core.GatewayError{Code: "connector_unavailable", Category: core.CategoryUnavailable, Message: "Anthropic connector is unavailable"}
	}
	if query.Protocol != protocol {
		return core.EstimateResult{}, &core.GatewayError{Code: "unsupported_protocol", Category: core.CategoryUnsupportedFeature, Message: "Unsupported response protocol"}
	}
	if query.Mode != core.ModeTranslation {
		return core.EstimateResult{}, &core.GatewayError{Code: "unsupported_mode", Category: core.CategoryUnsupportedFeature, Message: "Unsupported execution mode"}
	}
	if query.Model != c.model || query.AccountID != c.accountID {
		return core.EstimateResult{}, &core.GatewayError{Code: "scope_mismatch", Category: core.CategoryPermissionDenied, Message: "Usage estimate scope does not match configured target"}
	}
	return core.EstimateResult{Supported: true, Known: false, Method: "conservative"}, nil
}

func (c *Connector) Authenticate(context.Context, core.AuthRequest, core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	return core.AuthResult{Supported: false}, nil
}

func (c *Connector) Execute(_ context.Context, req core.ExecutionRequest, scope core.AttemptScope, _ core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	c.mu.Lock()
	ready, model, accountID := c.state == core.HealthReady, c.model, c.accountID
	c.mu.Unlock()
	if !ready {
		return core.ExecutionResponse{}, &core.GatewayError{Code: "connector_unavailable", Category: core.CategoryUnavailable, Message: "Anthropic connector is unavailable"}
	}
	if req.Payload.Protocol != protocol {
		return core.ExecutionResponse{}, &core.GatewayError{Code: "unsupported_protocol", Category: core.CategoryUnsupportedFeature, Message: "Unsupported response protocol"}
	}
	if scope.Mode != core.ModeTranslation {
		return core.ExecutionResponse{}, &core.GatewayError{Code: "unsupported_mode", Category: core.CategoryUnsupportedFeature, Message: "Unsupported execution mode"}
	}
	if scope.AccountID != accountID || req.Model != model {
		return core.ExecutionResponse{}, &core.GatewayError{Code: "scope_mismatch", Category: core.CategoryPermissionDenied, Message: "Execution scope does not match configured target"}
	}
	return core.ExecutionResponse{}, &core.GatewayError{Code: "connector_execution_unavailable", Category: core.CategoryUnavailable, Message: "Anthropic connector execution is not available yet"}
}

func (c *Connector) Close(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.state = core.HealthUnavailable
	return nil
}
