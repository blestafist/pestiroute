package chatcompletions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

const protocol = "openai.chat-completions.v1"
const componentID = "pestiroute.chat-completions.native"
const authBearer = "bearer"

type config struct {
	Endpoint              string                                   `json:"endpoint"`
	Model                 string                                   `json:"model"`
	AccountID             string                                   `json:"account_id"`
	MaxRequestHeaderBytes int64                                    `json:"max_request_header_bytes"`
	StreamIdleTimeout     time.Duration                            `json:"stream_idle_timeout"`
	Capabilities          map[core.Capability]core.CapabilityState `json:"capabilities,omitempty"`
}
type Connector struct {
	mu     sync.Mutex
	state  core.HealthState
	closed bool
	cfg    config
}

var _ core.Connector = (*Connector)(nil)

func NewConnector() *Connector { return &Connector{state: core.HealthUnknown} }
func (*Connector) Descriptor() core.Descriptor {
	return core.Descriptor{ID: componentID, Kind: core.ComponentConnector, ImplementationVersion: "1.0.0", APIVersions: []core.APIVersion{{Major: 1}}, Protocols: []string{protocol}, Operations: []string{"execute", "models", "estimate_usage", "authenticate"}, ConnectorType: "api", AuthMethods: []string{authBearer}}.Clone()
}
func (c *Connector) Init(_ context.Context, raw core.ComponentConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.state == core.HealthReady {
		return errors.New("Chat Completions connector cannot be initialized")
	}
	c.state = core.HealthUnavailable
	if !unique(raw.Data) {
		return errors.New("invalid connector configuration")
	}
	var x config
	d := json.NewDecoder(bytes.NewReader(raw.Data))
	d.DisallowUnknownFields()
	if d.Decode(&x) != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("invalid connector configuration")
	}
	u, e := url.Parse(x.Endpoint)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !strings.HasSuffix(u.EscapedPath(), "/v1/chat/completions") || (u.Scheme == "http" && !numericLoopback(u.Hostname())) || x.Model == "" || x.AccountID == "" || x.MaxRequestHeaderBytes <= 0 || x.StreamIdleTimeout < 0 {
		return errors.New("invalid Chat Completions endpoint or scope")
	}
	for cap, state := range x.Capabilities {
		if cap == "" || (state != core.Supported && state != core.Unsupported && state != core.Unknown) {
			return errors.New("invalid capability declaration")
		}
	}
	c.cfg = x
	c.state = core.HealthReady
	return nil
}
func numericLoopback(host string) bool { ip := net.ParseIP(host); return ip != nil && ip.IsLoopback() }
func (c *Connector) Health(context.Context) core.Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	return core.Health{State: c.state}
}
func (c *Connector) Capabilities(_ context.Context, s core.CapabilityScope) core.CapabilityResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := map[core.Capability]core.CapabilityState{}
	if c.state == core.HealthReady && s.Protocol == protocol && s.Mode == core.ModeNative && s.Model == c.cfg.Model && s.AccountID == c.cfg.AccountID {
		for k, x := range c.cfg.Capabilities {
			v[k] = x
		}
	}
	return core.CapabilityResult{Values: v}
}
func (c *Connector) Models(_ context.Context, q core.ModelQuery, _ core.InvocationServices) (core.ModelsResult, *core.GatewayError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != core.HealthReady || q.Protocol != protocol || q.Mode != core.ModeNative || q.AccountID != c.cfg.AccountID {
		return core.ModelsResult{}, nil
	}
	return core.ModelsResult{Supported: true, Models: []core.ModelInfo{{ID: c.cfg.Model, Capabilities: cloneCaps(c.cfg.Capabilities)}}}, nil
}
func (*Connector) EstimateUsage(context.Context, core.UsageQuery, core.InvocationServices) (core.EstimateResult, *core.GatewayError) {
	return core.EstimateResult{Supported: false}, nil
}
func (*Connector) Authenticate(context.Context, core.AuthRequest, core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	return core.AuthResult{Supported: false}, nil
}
func (c *Connector) Close(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.state = core.HealthUnavailable
	return nil
}
func (c *Connector) Execute(ctx context.Context, in core.ExecutionRequest, scope core.AttemptScope, services core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	c.mu.Lock()
	cfg, ready := c.cfg, c.state == core.HealthReady
	c.mu.Unlock()
	if !ready {
		return core.ExecutionResponse{}, ge("connector_unavailable", core.CategoryUnavailable, "Chat Completions connector unavailable")
	}
	if in.Payload.Protocol != protocol {
		return core.ExecutionResponse{}, ge("unsupported_protocol", core.CategoryUnsupportedFeature, "Unsupported protocol")
	}
	if scope.Mode != core.ModeNative || scope.AccountID != cfg.AccountID || in.Model != cfg.Model {
		return core.ExecutionResponse{}, ge("scope_mismatch", core.CategoryPermissionDenied, "Execution scope mismatch")
	}
	if int64(len(in.Payload.Body)) > scope.MaxBodyBytes {
		return core.ExecutionResponse{}, ge("request_too_large", core.CategoryInvalidRequest, "Request body exceeds limit")
	}
	if in.Metadata.IngressHeaderBytes <= 0 || in.Metadata.IngressHeaderBytes > cfg.MaxRequestHeaderBytes {
		return core.ExecutionResponse{}, ge("request_headers_too_large", core.CategoryInvalidRequest, "Request headers exceed configured limit")
	}
	if services.Credentials == nil {
		return core.ExecutionResponse{}, ge("credential_unavailable", core.CategoryUnauthenticated, "Selected credential unavailable")
	}
	secret, e := services.Credentials.Get(ctx, "bearer")
	if e != nil || len(secret) == 0 {
		return core.ExecutionResponse{}, ge("credential_unavailable", core.CategoryUnauthenticated, "Selected credential unavailable")
	}
	return execute(ctx, cfg, in, string(secret), services.Transport)
}
func ge(code string, cat core.ErrorCategory, msg string) *core.GatewayError {
	return &core.GatewayError{Code: code, Category: cat, Message: msg, RetryDisposition: core.RetryUnknown}
}
func unique(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	var walk func() bool
	walk = func() bool {
		t, e := d.Token()
		if e != nil {
			return false
		}
		x, ok := t.(json.Delim)
		if !ok {
			return true
		}
		if x == '{' {
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return false
				}
				s, ok := k.(string)
				if !ok || seen[s] || !walk() {
					return false
				}
				seen[s] = true
			}
			end, e := d.Token()
			return e == nil && end == json.Delim('}')
		}
		if x == '[' {
			for d.More() {
				if !walk() {
					return false
				}
			}
			end, e := d.Token()
			return e == nil && end == json.Delim(']')
		}
		return false
	}
	if !walk() {
		return false
	}
	_, e := d.Token()
	return e == io.EOF
}
func cloneCaps(in map[core.Capability]core.CapabilityState) map[core.Capability]core.CapabilityState {
	out := map[core.Capability]core.CapabilityState{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
