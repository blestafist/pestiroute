package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

const (
	componentID = "pestiroute.codex.responses"
	protocol    = "openai.responses.v1"
	profile     = "codex-responses-http-sse-v1"
	liteProfile = "codex-responses-http-sse-lite-v1"
	liteModel   = "gpt-6-luna"
)

type componentConfig struct {
	Model     string `json:"model"`
	AccountID string `json:"account_id"`
	Profile   string `json:"profile"`
}

// Connector owns only non-secret target configuration and lifecycle state.
type Connector struct {
	mu                sync.Mutex
	model             string
	accountID         string
	profile           string
	authURL           string
	endpoint          string
	transport         *http.Transport
	client            *http.Client
	streamIdleTimeout time.Duration
	state             core.HealthState
	closed            bool
}

var _ core.Connector = (*Connector)(nil)

func NewConnector() *Connector {
	transport, client := newHTTPTransport()
	return &Connector{state: core.HealthUnknown, endpoint: codexResponsesEndpoint, transport: transport, client: client, streamIdleTimeout: codexStreamIdleTimeout}
}

func (c *Connector) Descriptor() core.Descriptor {
	return core.Descriptor{
		ID: componentID, Kind: core.ComponentConnector, ImplementationVersion: "1.0.0",
		APIVersions: []core.APIVersion{{Major: 1, Minor: 0}},
		Protocols:   []string{protocol}, Operations: []string{"execute", "models", "estimate_usage", "authenticate"},
		ConnectorType: "agent-protocol", AuthMethods: []string{"bearer"},
	}.Clone()
}

func (c *Connector) Init(_ context.Context, config core.ComponentConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.state == core.HealthReady {
		return errors.New("Codex connector cannot be initialized in its current state")
	}
	c.state = core.HealthUnavailable
	var cfg componentConfig
	d := json.NewDecoder(strings.NewReader(string(config.Data)))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return fmt.Errorf("invalid Codex connector configuration: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("invalid Codex connector configuration: trailing data")
	}
	if strings.TrimSpace(cfg.Model) == "" || strings.TrimSpace(cfg.AccountID) == "" {
		return errors.New("Codex connector model and account_id are required")
	}
	if cfg.Profile != profile && cfg.Profile != liteProfile {
		return fmt.Errorf("Codex connector profile must be %q or %q", profile, liteProfile)
	}
	if cfg.Profile == liteProfile && cfg.Model != liteModel {
		return fmt.Errorf("Codex Lite profile requires model %q", liteModel)
	}
	c.model, c.accountID, c.profile = cfg.Model, cfg.AccountID, cfg.Profile
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
	if !c.matches(scope.Protocol, scope.Mode, scope.Model, scope.AccountID) {
		return core.CapabilityResult{}
	}
	values := map[core.Capability]core.CapabilityState{
		"llm.streaming": core.Supported,
	}
	if c.profile == liteProfile && scope.Mode == core.ModeNative && scope.Model == liteModel {
		values["llm.reasoning"] = core.Supported
		values["llm.tools"] = core.Supported
	}
	return core.CapabilityResult{Values: values}
}

func (c *Connector) matches(requestProtocol, mode, model, accountID string) bool {
	return c.state == core.HealthReady && requestProtocol == protocol &&
		(mode == core.ModeNative || (mode == core.ModeTranslation && c.profile != liteProfile)) &&
		model == c.model && accountID == c.accountID
}

func (c *Connector) Close(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.state = core.HealthUnavailable
	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}
	return nil
}

// HTTPDoer exposes the connector's policy-configured client to invocation-scoped services.
func (c *Connector) HTTPDoer() core.HTTPDoer { return c.client }

func (c *Connector) Execute(ctx context.Context, req core.ExecutionRequest, scope core.AttemptScope, services core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	c.mu.Lock()
	if c.state != core.HealthReady {
		c.mu.Unlock()
		return core.ExecutionResponse{}, connectorError("connector_unavailable", core.CategoryUnavailable, "Codex connector is unavailable")
	}
	if req.Payload.Protocol != protocol {
		c.mu.Unlock()
		return core.ExecutionResponse{}, connectorError("unsupported_protocol", core.CategoryUnsupportedFeature, "Unsupported response protocol")
	}
	if scope.Mode != core.ModeNative && scope.Mode != core.ModeTranslation {
		c.mu.Unlock()
		return core.ExecutionResponse{}, connectorError("unsupported_mode", core.CategoryUnsupportedFeature, "Unsupported execution mode")
	}
	if req.Model != c.model || scope.AccountID != c.accountID {
		c.mu.Unlock()
		return core.ExecutionResponse{}, connectorError("scope_mismatch", core.CategoryPermissionDenied, "Execution scope does not match configured target")
	}
	model, accountID, selectedProfile, endpoint, client, idleTimeout := c.model, c.accountID, c.profile, c.endpoint, c.client, c.streamIdleTimeout
	c.mu.Unlock()
	if selectedProfile == liteProfile && scope.Mode != core.ModeNative {
		return core.ExecutionResponse{}, connectorError("unsupported_mode", core.CategoryUnsupportedFeature, "Codex Lite profile requires native mode")
	}
	if ge := validateResponsesProfile(req, scope.Mode, model); ge != nil {
		return core.ExecutionResponse{}, ge
	}
	body, ge := adaptRequest(req, scope.Mode)
	if ge != nil {
		return core.ExecutionResponse{}, ge
	}
	requestCtx, cancel := context.WithTimeout(ctx, codexRequestTimeout)
	headers, err := buildRequestHeaders(requestCtx, req.Metadata.Headers, services, accountID)
	if err != nil {
		cancel()
		if errors.Is(err, errRequestHeaderLimit) {
			return core.ExecutionResponse{}, connectorError("invalid_request", core.CategoryInvalidRequest, "Request headers exceed the profile limit")
		}
		return core.ExecutionResponse{}, connectorError("credential_unavailable", core.CategoryUnauthenticated, "Selected account credentials are unavailable")
	}
	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		cancel()
		return core.ExecutionResponse{}, connectorError("invalid_request", core.CategoryInvalidRequest, "Codex request could not be constructed")
	}
	httpReq.GetBody = nil
	httpReq.ContentLength = int64(len(body))
	httpReq.Header = headers
	if selectedProfile == liteProfile {
		if applyLiteIdentity(httpReq.Header) != nil {
			cancel()
			return core.ExecutionResponse{}, connectorError("credential_unavailable", core.CategoryUnauthenticated, "Selected account credentials are unavailable")
		}
		if !codexHeadersWithinLimit(httpReq.Header) {
			cancel()
			return core.ExecutionResponse{}, connectorError("invalid_request", core.CategoryInvalidRequest, "Request headers exceed the profile limit")
		}
	}
	doer := services.Transport
	if doer == nil {
		doer = client
	}
	resp, closeTransport, err := doCodexRequest(doer, httpReq)
	if err != nil {
		closeTransport()
		cancel()
		return core.ExecutionResponse{}, codexTransportError(err)
	}
	if resp == nil || resp.Body == nil {
		closeTransport()
		cancel()
		return core.ExecutionResponse{}, connectorError("upstream_unavailable", core.CategoryUnavailable, "Upstream returned an invalid response")
	}
	for _, value := range resp.Header.Values("Content-Encoding") {
		for _, encoding := range strings.Split(value, ",") {
			if !strings.EqualFold(strings.TrimSpace(encoding), "identity") {
				_ = resp.Body.Close()
				closeTransport()
				cancel()
				return core.ExecutionResponse{}, connectorError("unsupported_response_encoding", core.CategoryUnavailable, "Upstream response encoding is unsupported")
			}
		}
	}
	stream := &executeStream{body: resp.Body, cancel: cancel, head: codexResponseHead(resp), idleTimeout: idleTimeout, closeTransport: closeTransport}
	stream.stopRequest = context.AfterFunc(requestCtx, stream.abort)
	return core.ExecutionResponse{Stream: stream}, nil
}

func (c *Connector) Models(_ context.Context, query core.ModelQuery, _ core.InvocationServices) (core.ModelsResult, *core.GatewayError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != core.HealthReady {
		return core.ModelsResult{}, connectorError("connector_unavailable", core.CategoryUnavailable, "Codex connector is unavailable")
	}
	if query.Protocol != protocol {
		return core.ModelsResult{}, connectorError("unsupported_protocol", core.CategoryUnsupportedFeature, "Unsupported response protocol")
	}
	if query.Mode != core.ModeNative && query.Mode != core.ModeTranslation {
		return core.ModelsResult{}, connectorError("unsupported_mode", core.CategoryUnsupportedFeature, "Unsupported execution mode")
	}
	if query.AccountID != c.accountID {
		return core.ModelsResult{}, connectorError("scope_mismatch", core.CategoryPermissionDenied, "Model query scope does not match configured target")
	}
	return core.ModelsResult{}, nil
}

func (c *Connector) EstimateUsage(ctx context.Context, query core.UsageQuery, _ core.InvocationServices) (core.EstimateResult, *core.GatewayError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != core.HealthReady {
		return core.EstimateResult{}, connectorError("connector_unavailable", core.CategoryUnavailable, "Codex connector is unavailable")
	}
	if query.Protocol != protocol {
		return core.EstimateResult{}, connectorError("unsupported_protocol", core.CategoryUnsupportedFeature, "Unsupported response protocol")
	}
	if query.Mode != core.ModeNative && query.Mode != core.ModeTranslation {
		return core.EstimateResult{}, connectorError("unsupported_mode", core.CategoryUnsupportedFeature, "Unsupported execution mode")
	}
	if query.Model != c.model || query.AccountID != c.accountID {
		return core.EstimateResult{}, connectorError("scope_mismatch", core.CategoryPermissionDenied, "Usage estimate scope does not match configured target")
	}
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return core.EstimateResult{}, connectorError("estimate_timeout", core.CategoryTimeout, "Usage estimation timed out")
		}
		return core.EstimateResult{}, connectorError("estimate_cancelled", core.CategoryCancelled, "Usage estimation cancelled")
	}
	return core.EstimateResult{Supported: false, Known: false}, nil
}

func (c *Connector) Authenticate(ctx context.Context, request core.AuthRequest, services core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	c.mu.Lock()
	if c.state != core.HealthReady {
		c.mu.Unlock()
		return core.AuthResult{}, connectorError("connector_unavailable", core.CategoryUnavailable, "Codex connector is unavailable")
	}
	if request.AccountID != c.accountID {
		c.mu.Unlock()
		return core.AuthResult{}, connectorError("scope_mismatch", core.CategoryPermissionDenied, "Authentication scope does not match configured target")
	}
	c.mu.Unlock()
	switch request.Action {
	case "start":
		return c.authenticateStart(ctx, services)
	case "continue":
		return c.authenticateContinue(ctx, string(request.State), services)
	case "refresh":
		return c.authenticateRefresh(ctx, services)
	default:
		return core.AuthResult{Supported: false}, nil
	}
}

func connectorError(code string, category core.ErrorCategory, message string) *core.GatewayError {
	return &core.GatewayError{Code: code, Category: category, Message: message}
}

func authRejectedError(status int) *core.GatewayError {
	err := connectorError("auth_rejected", core.CategoryUnavailable, "Authentication provider rejected the request")
	if status >= 100 && status <= 599 {
		err.OriginalError = fmt.Sprintf("HTTP status %d", status)
	}
	return err
}
