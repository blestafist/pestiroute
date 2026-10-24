package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

const (
	componentID = "pestiroute.antigravity.v1internal"
	profile     = "antigravity-v1internal"
	protocol    = "openai.responses.v1"
)

var projectPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}[a-z0-9]$|^[a-z]$`)

type config struct {
	Profile      string      `json:"profile"`
	Project      string      `json:"project"`
	Endpoint     string      `json:"endpoint,omitempty"`
	AllowOnboard bool        `json:"allow_onboard,omitempty"`
	OAuth        oauthConfig `json:"oauth"`
}

const defaultInferenceEndpoint = "https://cloudcode-pa.googleapis.com"

type oauthConfig struct {
	ClientID              string   `json:"client_id"`
	ClientSecret          string   `json:"client_secret,omitempty"`
	AuthorizationEndpoint string   `json:"auth_endpoint,omitempty"`
	TokenEndpoint         string   `json:"token_endpoint,omitempty"`
	RedirectURI           string   `json:"redirect_uri,omitempty"`
	Scopes                []string `json:"scopes,omitempty"`
}

const (
	defaultAuthEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	defaultTokenEndpoint = "https://oauth2.googleapis.com/token"
	// Public client identity from the pinned 9Router source; credentials remain operator-supplied.
	defaultClientID = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	defaultScopes   = "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile https://www.googleapis.com/auth/cclog https://www.googleapis.com/auth/experimentsandconfigs"
)

type Connector struct {
	mu     sync.Mutex
	state  core.HealthState
	closed bool
	cfg    config
	client *http.Client
	doer   *runtimeHTTPDoer
}

type runtimeHTTPDoer struct{ client *http.Client }

func (d *runtimeHTTPDoer) Do(request *http.Request) (*http.Response, error) {
	return d.client.Do(request)
}

var _ core.Connector = (*Connector)(nil)

func NewConnector() *Connector { return &Connector{state: core.HealthUnavailable} }

func (c *Connector) Descriptor() core.Descriptor {
	return core.Descriptor{ID: componentID, Kind: core.ComponentConnector, ImplementationVersion: "1.0.0", APIVersions: []core.APIVersion{{Major: 1}}, Protocols: []string{protocol}, Operations: []string{"execute", "authenticate"}, ConnectorType: "api", AuthMethods: []string{"oauth"}}.Clone()
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
	cfg.OAuth = withOAuthDefaults(cfg.OAuth)
	if !validOAuthConfig(cfg.OAuth) {
		return errors.New("invalid Antigravity OAuth configuration")
	}
	if endpointOverride := strings.TrimSpace(os.Getenv("PESTIROUTE_ANTIGRAVITY_ENDPOINT")); endpointOverride != "" {
		cfg.Endpoint = endpointOverride
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultInferenceEndpoint
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "" {
		return errors.New("invalid Antigravity endpoint")
	}
	c.cfg, c.state = cfg, core.HealthReady
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = 90 * time.Second
	c.client = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	c.doer = &runtimeHTTPDoer{client: c.client}
	return nil
}

// HTTPDoer exposes the connector-scoped, redirect-disabled runtime client.
func (c *Connector) HTTPDoer() core.HTTPDoer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.doer
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
	return core.CapabilityResult{Values: map[core.Capability]core.CapabilityState{"llm.streaming": core.Supported, "auth.oauth": core.Unknown, "usage.exact": core.Unknown}}
}
func (c *Connector) Close(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.state = core.HealthUnavailable
	if c.client != nil {
		c.client.CloseIdleConnections()
	}
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
func (c *Connector) Authenticate(ctx context.Context, req core.AuthRequest, services core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	if e := c.validate(ctx, protocol, core.ModeTranslation); e != nil {
		return core.AuthResult{}, e
	}
	c.mu.Lock()
	cfg := c.cfg.OAuth
	discoveryOrigin := c.cfg.Endpoint
	allowOnboard := c.cfg.AllowOnboard
	c.mu.Unlock()
	if req.Action == "start" && len(req.State) == 0 && req.CallbackInput == "" {
		if cfg.ClientID == "" {
			return core.AuthResult{Supported: false}, nil
		}
		return c.authenticateStart(ctx, cfg)
	}
	if req.Action == "continue" && len(req.State) != 0 {
		if cfg.ClientID == "" {
			return core.AuthResult{Supported: false}, nil
		}
		return c.authenticateContinue(ctx, cfg, discoveryOrigin, allowOnboard, req, services)
	}
	if req.Action != "refresh" || len(req.State) != 0 || req.CallbackInput != "" {
		return core.AuthResult{Supported: false}, nil
	}
	return c.authenticateRefresh(ctx, cfg, services)
}

func withOAuthDefaults(cfg oauthConfig) oauthConfig {
	if cfg.ClientID == "" {
		cfg.ClientID = defaultClientID
	}
	if cfg.AuthorizationEndpoint == "" {
		cfg.AuthorizationEndpoint = defaultAuthEndpoint
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = strings.Fields(defaultScopes)
	}
	return cfg
}

func validOAuthConfig(cfg oauthConfig) bool {
	endpoint := func(raw string) bool {
		u, err := url.Parse(raw)
		return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
	}
	if cfg.ClientID == "" || cfg.ClientSecret != "" && strings.TrimSpace(cfg.ClientSecret) == "" || strings.ContainsAny(cfg.ClientID+cfg.ClientSecret, "\r\n") || !endpoint(cfg.AuthorizationEndpoint) || cfg.TokenEndpoint != "" && !endpoint(cfg.TokenEndpoint) || cfg.RedirectURI != "" && !validRedirectURI(cfg.RedirectURI) || len(cfg.Scopes) == 0 {
		return false
	}
	for _, scope := range cfg.Scopes {
		if strings.TrimSpace(scope) != scope || scope == "" || strings.ContainsAny(scope, "\r\n") {
			return false
		}
	}
	return true
}

func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.IsAbs() && u.Opaque == "" && u.Host != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
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
