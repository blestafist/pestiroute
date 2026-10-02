package responses

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"

	"github.com/blestafist/pestiroute/internal/core"
)

const (
	componentID = "pestiroute.responses.native"
	modelID     = "gpt-5.4-mini"
)

// componentConfig is private non-secret connector configuration.
type componentConfig struct {
	Transport Config `json:"transport"`
	Model     string `json:"model"`
	AccountID string `json:"account_id"`
}

// Connector exposes the native Responses transport through the common component API.
type Connector struct {
	mu        sync.Mutex
	transport *Transport
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

func (c *Connector) Init(_ context.Context, config core.ComponentConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.state == core.HealthReady {
		return errors.New("Responses connector cannot be initialized in its current state")
	}
	var cfg componentConfig
	d := json.NewDecoder(strings.NewReader(string(config.Data)))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return fmt.Errorf("invalid Responses connector configuration: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("invalid Responses connector configuration: trailing data")
	}
	if cfg.Model == "" || cfg.AccountID == "" {
		return errors.New("Responses connector model and account_id are required")
	}
	u, err := url.Parse(cfg.Transport.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return errors.New("Responses connector requires a valid HTTP(S) endpoint")
	}
	c.transport = NewTransport(cfg.Transport)
	c.model, c.accountID = cfg.Model, cfg.AccountID
	c.state = core.HealthReady
	return nil
}

func (c *Connector) Health(context.Context) core.Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	return core.Health{State: c.state}
}

// HTTPDoer exposes the connector's already-configured single-target transport
// for invocation-scoped runtime services.
func (c *Connector) HTTPDoer() core.HTTPDoer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.transport == nil || c.state != core.HealthReady {
		return nil
	}
	return c.transport.client
}

func (c *Connector) Capabilities(_ context.Context, scope core.CapabilityScope) core.CapabilityResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capabilitiesLocked(scope)
}

func (c *Connector) capabilitiesLocked(scope core.CapabilityScope) core.CapabilityResult {
	values := map[core.Capability]core.CapabilityState{}
	if c.state != core.HealthReady || scope.Protocol != protocol || scope.Mode != "native" ||
		scope.Model != c.model || scope.AccountID != c.accountID || c.model != modelID || !capabilityEndpoint(c.transport.endpoint) {
		return core.CapabilityResult{Values: values}
	}
	values["llm.streaming"] = core.Supported
	values["llm.tools"] = core.Supported
	values["llm.reasoning"] = core.Supported
	// Parallel tool support remains unknown for the live baseline.
	return core.CapabilityResult{Values: values}
}

func capabilityEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	verified := u.Scheme == "https" && u.Host == "api.openai.com"
	fixture := u.Scheme == "http" && isLoopbackHost(u.Hostname())
	return (verified || fixture) && u.EscapedPath() == "/v1/responses"
}

func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Connector) Close(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.state = core.HealthUnavailable
	if c.transport != nil {
		c.transport.Close()
	}
	return nil
}

func (c *Connector) Models(_ context.Context, query core.ModelQuery, _ core.InvocationServices) (core.ModelsResult, *core.GatewayError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != core.HealthReady || query.Protocol != protocol || query.Mode != "native" || query.AccountID != c.accountID {
		return core.ModelsResult{}, nil
	}
	return core.ModelsResult{Supported: true, Models: []core.ModelInfo{{ID: c.model, Capabilities: c.capabilitiesLocked(core.CapabilityScope{
		Protocol: protocol, Mode: "native", Model: c.model, AccountID: c.accountID,
	}).Values}}}, nil
}

func (c *Connector) EstimateUsage(context.Context, core.UsageQuery, core.InvocationServices) (core.EstimateResult, *core.GatewayError) {
	return core.EstimateResult{Supported: false, Known: false}, nil
}

func (c *Connector) Authenticate(context.Context, core.AuthRequest, core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	return core.AuthResult{Supported: false}, nil
}

func (c *Connector) Execute(ctx context.Context, req core.ExecutionRequest, scope core.AttemptScope, services core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	c.mu.Lock()
	t, ready := c.transport, c.state == core.HealthReady
	c.mu.Unlock()
	if !ready || t == nil {
		return core.ExecutionResponse{}, gatewayError("connector_unavailable", core.CategoryUnavailable, "Responses connector is unavailable")
	}
	if req.Payload.Protocol != protocol {
		return core.ExecutionResponse{}, gatewayError("unsupported_protocol", core.CategoryUnsupportedFeature, "Unsupported response protocol")
	}
	if scope.Mode != "native" {
		return core.ExecutionResponse{}, gatewayError("unsupported_mode", core.CategoryUnsupportedFeature, "Unsupported execution mode")
	}
	if scope.AccountID != c.accountID || req.Model != c.model {
		return core.ExecutionResponse{}, gatewayError("scope_mismatch", core.CategoryPermissionDenied, "Execution scope does not match configured target")
	}
	if services.Credentials == nil {
		return core.ExecutionResponse{}, gatewayError("credential_unavailable", core.CategoryUnauthenticated, "Selected credential is unavailable")
	}
	credential, err := services.Credentials.Get(ctx, "bearer")
	if err != nil || len(credential) == 0 {
		return core.ExecutionResponse{}, gatewayError("credential_unavailable", core.CategoryUnauthenticated, "Selected credential is unavailable")
	}
	if req.Metadata.Streaming != nil && *req.Metadata.Streaming {
		return t.ExecuteSSE(ctx, req, string(credential), services.Transport)
	}
	return t.ExecuteFixedJSON(ctx, req, string(credential), services.Transport)
}
