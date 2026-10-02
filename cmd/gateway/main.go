package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	adapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	connector "github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
)

type config struct {
	Listen                string `json:"listen"`
	ShutdownTimeout       string `json:"shutdown_timeout"`
	UpstreamEndpoint      string `json:"upstream_endpoint"`
	UpstreamCredentialEnv string `json:"upstream_credential_env"`
	MaxRequestBodyBytes   int64  `json:"max_request_body_bytes"`
	MaxRequestHeaderBytes int64  `json:"max_request_header_bytes"`
	ConnectTimeout        string `json:"connect_timeout"`
	TLSHandshakeTimeout   string `json:"tls_handshake_timeout"`
	ResponseHeaderTimeout string `json:"response_header_timeout"`
	StreamIdleTimeout     string `json:"stream_idle_timeout"`
	credential            secret
	Components            []topologyComponent `json:"components,omitempty"`
	Routes                []topologyRoute     `json:"routes,omitempty"`
	Credentials           map[string]secret   `json:"-"`
}

type topologyComponent struct {
	ID             core.InstanceID    `json:"id"`
	Implementation string             `json:"implementation"`
	Kind           core.ComponentKind `json:"kind"`
	Endpoint       string             `json:"endpoint,omitempty"`
	CredentialEnv  string             `json:"credential_env,omitempty"`
	MaxBodyBytes   int64              `json:"max_request_body_bytes,omitempty"`
	MaxHeaderBytes int64              `json:"max_request_header_bytes,omitempty"`
	ConnectTimeout string             `json:"connect_timeout,omitempty"`
	TLSTimeout     string             `json:"tls_handshake_timeout,omitempty"`
	HeaderTimeout  string             `json:"response_header_timeout,omitempty"`
	IdleTimeout    string             `json:"stream_idle_timeout,omitempty"`
}

type topologyRoute struct {
	Protocol     string                                   `json:"protocol"`
	Mode         string                                   `json:"mode"`
	Model        string                                   `json:"model"`
	Account      string                                   `json:"account"`
	Adapter      core.InstanceID                          `json:"adapter"`
	Connector    core.InstanceID                          `json:"connector"`
	Capabilities map[core.Capability]core.CapabilityState `json:"capabilities,omitempty"`
}

// secret stays in runtime state, never in configuration output or diagnostics.
type secret string

const responsesProtocol = "openai.responses.v1"

func (secret) String() string   { return "[REDACTED]" }
func (secret) GoString() string { return "[REDACTED]" }

func (c config) String() string {
	c.credential = ""
	c.Credentials = nil
	type view config
	return fmt.Sprintf("%+v", view(c))
}

func (c config) GoString() string { return c.String() }

func loadConfig(args []string) (config, time.Duration, error) {
	flags := flag.NewFlagSet("gateway", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "", "JSON configuration file")
	listen := flags.String("listen", "", "listener address")
	timeout := flags.String("shutdown-timeout", "", "graceful shutdown duration")
	if err := flags.Parse(args); err != nil {
		return config{}, 0, err
	}
	if flags.NArg() != 0 {
		return config{}, 0, fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	c := config{Listen: "127.0.0.1:8080", ShutdownTimeout: "5s"}
	var supplied map[string]json.RawMessage
	if *path != "" {
		file, err := os.Open(*path)
		if err != nil {
			return config{}, 0, fmt.Errorf("config %q: %w", *path, err)
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		if err != nil {
			return config{}, 0, fmt.Errorf("config %q: %w", *path, err)
		}
		if len(data) > 1<<20 {
			return config{}, 0, fmt.Errorf("config %q: exceeds 1 MiB", *path)
		}
		if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
			return config{}, 0, fmt.Errorf("config %q: expected JSON object", *path)
		}
		if err := rejectNullValues(data); err != nil {
			return config{}, 0, fmt.Errorf("config %q: %w", *path, err)
		}
		if err := validateJSONFieldSpellings(data); err != nil {
			return config{}, 0, fmt.Errorf("config %q: %w", *path, err)
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&c); err != nil {
			return config{}, 0, fmt.Errorf("config %q: %w", *path, err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return config{}, 0, fmt.Errorf("config %q: expected one JSON object (trailing data or oversized file): %v", *path, err)
		}
		if err := json.Unmarshal(data, &supplied); err != nil {
			return config{}, 0, fmt.Errorf("config %q: invalid JSON: %w", *path, err)
		}
	}
	if *listen != "" {
		c.Listen = *listen
	}
	if *timeout != "" {
		c.ShutdownTimeout = *timeout
	}
	if strings.TrimSpace(c.Listen) == "" {
		return config{}, 0, errors.New("listen must be a non-empty host:port address")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return config{}, 0, fmt.Errorf("invalid listen address %q: %w", c.Listen, err)
	}
	d, err := time.ParseDuration(c.ShutdownTimeout)
	if err != nil || d <= 0 {
		return config{}, 0, fmt.Errorf("invalid shutdown_timeout %q: must be a positive duration", c.ShutdownTimeout)
	}
	fields := []string{"upstream_endpoint", "upstream_credential_env", "max_request_body_bytes", "max_request_header_bytes", "connect_timeout", "tls_handshake_timeout", "response_header_timeout", "stream_idle_timeout"}
	count := 0
	for _, field := range fields {
		if _, ok := supplied[field]; ok {
			count++
		}
	}
	m2 := false
	for _, field := range []string{"components", "routes"} {
		if _, ok := supplied[field]; ok {
			m2 = true
		}
	}
	if count != 0 && m2 {
		return config{}, 0, errors.New("legacy upstream fields cannot be mixed with M2 components or routes")
	}
	if m2 {
		if _, components := supplied["components"]; !components {
			return config{}, 0, errors.New("M2 topology requires components and routes")
		}
		if _, routes := supplied["routes"]; !routes {
			return config{}, 0, errors.New("M2 topology requires components and routes")
		}
		if err := validateTopology(&c); err != nil {
			return config{}, 0, err
		}
	} else if count != 0 {
		if count != len(fields) {
			return config{}, 0, errors.New("partial inference configuration: all upstream, limit and timeout fields are required")
		}
		for _, field := range fields {
			if bytes.Equal(bytes.TrimSpace(supplied[field]), []byte("null")) {
				return config{}, 0, fmt.Errorf("%s must not be null", field)
			}
		}
		if host != "127.0.0.1" && host != "::1" {
			return config{}, 0, errors.New("inference listen must bind numeric loopback (127.0.0.1 or ::1)")
		}
		endpoint, err := url.Parse(c.UpstreamEndpoint)
		if err != nil || endpoint == nil || endpoint.Host == "" || endpoint.Opaque != "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || strings.Contains(c.UpstreamEndpoint, "#") || !strings.HasSuffix(endpoint.EscapedPath(), "/v1/responses") || (endpoint.Scheme != "https" && (endpoint.Scheme != "http" || !isLoopbackHost(endpoint.Hostname()))) {
			return config{}, 0, errors.New("invalid upstream_endpoint: require absolute HTTPS /v1/responses URL without userinfo, query or fragment (HTTP only on loopback)")
		}
		if c.MaxRequestBodyBytes <= 0 || c.MaxRequestHeaderBytes <= 0 {
			return config{}, 0, errors.New("max_request_body_bytes and max_request_header_bytes must be positive")
		}
		for _, entry := range []struct{ name, value string }{{"connect_timeout", c.ConnectTimeout}, {"tls_handshake_timeout", c.TLSHandshakeTimeout}, {"response_header_timeout", c.ResponseHeaderTimeout}, {"stream_idle_timeout", c.StreamIdleTimeout}} {
			value, err := time.ParseDuration(entry.value)
			if err != nil || value <= 0 {
				return config{}, 0, fmt.Errorf("%s must be a positive Go duration", entry.name)
			}
		}
		if c.UpstreamCredentialEnv == "" || strings.Contains(c.UpstreamCredentialEnv, "=") {
			return config{}, 0, errors.New("upstream_credential_env must name a non-empty environment variable")
		}
		value, ok := os.LookupEnv(c.UpstreamCredentialEnv)
		if !ok || value == "" {
			return config{}, 0, errors.New("upstream_credential_env is unset or empty")
		}
		c.credential = secret(value)
		c.Components = legacyComponents(c)
		c.Routes = []topologyRoute{{Protocol: responsesProtocol, Mode: core.ModeNative, Model: "gpt-5.4-mini", Account: c.UpstreamCredentialEnv, Adapter: "responses-adapter", Connector: "responses-connector"}}
		c.Credentials = map[string]secret{c.UpstreamCredentialEnv: c.credential}
	}
	return c, d, nil
}

func validateJSONFieldSpellings(data []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil // The strict decoder below preserves the established syntax diagnostic.
	}
	if err := checkJSONKeys(object, map[string]bool{
		"listen": true, "shutdown_timeout": true, "upstream_endpoint": true,
		"upstream_credential_env": true, "max_request_body_bytes": true,
		"max_request_header_bytes": true, "connect_timeout": true,
		"tls_handshake_timeout": true, "response_header_timeout": true,
		"stream_idle_timeout": true, "components": true, "routes": true,
	}); err != nil {
		return err
	}
	for _, group := range []struct {
		field string
		keys  map[string]bool
	}{
		{"components", map[string]bool{
			"id": true, "implementation": true, "kind": true, "endpoint": true,
			"credential_env": true, "max_request_body_bytes": true,
			"max_request_header_bytes": true, "connect_timeout": true,
			"tls_handshake_timeout": true, "response_header_timeout": true,
			"stream_idle_timeout": true,
		}},
		{"routes", map[string]bool{
			"protocol": true, "mode": true, "model": true, "account": true,
			"adapter": true, "connector": true, "capabilities": true,
		}},
	} {
		raw, ok := object[group.field]
		if !ok {
			continue
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			continue // Type/syntax errors are reported by the strict struct decoder.
		}
		for i, item := range items {
			var entry map[string]json.RawMessage
			if err := json.Unmarshal(item, &entry); err != nil || entry == nil {
				continue
			}
			if err := checkJSONKeys(entry, group.keys); err != nil {
				return fmt.Errorf("%s[%d]: %w", group.field, i, err)
			}
		}
	}
	return nil
}

func checkJSONKeys(object map[string]json.RawMessage, allowed map[string]bool) error {
	for key := range object {
		if !allowed[key] {
			return fmt.Errorf("unknown configuration field %q", key)
		}
	}
	return nil
}

func rejectNullValues(data []byte) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil // The strict decoder below returns the established JSON diagnostic.
	}
	var visit func(any, string) string
	visit = func(v any, path string) string {
		switch x := v.(type) {
		case nil:
			return path
		case []any:
			for i, item := range x {
				if got := visit(item, fmt.Sprintf("%s[%d]", path, i)); got != "" {
					return got
				}
			}
		case map[string]any:
			for key, item := range x {
				next := key
				if path != "" {
					next = path + "." + key
				}
				if got := visit(item, next); got != "" {
					return got
				}
			}
		}
		return ""
	}
	if field := visit(value, ""); field != "" {
		return fmt.Errorf("%s must not be null", field)
	}
	return nil
}

func legacyComponents(c config) []topologyComponent {
	return []topologyComponent{
		{ID: "responses-adapter", Implementation: "pestiroute.responses.native", Kind: core.ComponentAdapter},
		{ID: "responses-connector", Implementation: "pestiroute.responses.native", Kind: core.ComponentConnector,
			Endpoint: c.UpstreamEndpoint, CredentialEnv: c.UpstreamCredentialEnv, MaxBodyBytes: c.MaxRequestBodyBytes,
			MaxHeaderBytes: c.MaxRequestHeaderBytes, ConnectTimeout: c.ConnectTimeout, TLSTimeout: c.TLSHandshakeTimeout,
			HeaderTimeout: c.ResponseHeaderTimeout, IdleTimeout: c.StreamIdleTimeout},
	}
}

func validateTopology(c *config) error {
	if len(c.Components) == 0 || len(c.Routes) == 0 {
		return errors.New("M2 components and routes must be non-empty")
	}
	if host, _, _ := net.SplitHostPort(c.Listen); host != "127.0.0.1" && host != "::1" {
		return errors.New("inference listen must bind numeric loopback (127.0.0.1 or ::1)")
	}
	components := make(map[core.InstanceID]core.ComponentKind, len(c.Components))
	c.Credentials = make(map[string]secret)
	for i := range c.Components {
		component := &c.Components[i]
		if component.ID == "" || component.Implementation != "pestiroute.responses.native" || (component.Kind != core.ComponentAdapter && component.Kind != core.ComponentConnector) {
			return fmt.Errorf("component %d requires a valid id, implementation, and kind", i)
		}
		if _, exists := components[component.ID]; exists {
			return fmt.Errorf("duplicate component instance ID %q", component.ID)
		}
		components[component.ID] = component.Kind
		if component.Kind == core.ComponentAdapter {
			if component.Endpoint != "" || component.CredentialEnv != "" || component.MaxBodyBytes != 0 || component.MaxHeaderBytes != 0 || component.ConnectTimeout != "" || component.TLSTimeout != "" || component.HeaderTimeout != "" || component.IdleTimeout != "" {
				return fmt.Errorf("adapter component %q has connector configuration", component.ID)
			}
			continue
		}
		if component.Endpoint == "" || component.CredentialEnv == "" || component.MaxBodyBytes <= 0 || component.MaxHeaderBytes <= 0 {
			return fmt.Errorf("connector component %q requires endpoint, credential_env, and positive limits", component.ID)
		}
		endpoint, err := url.Parse(component.Endpoint)
		if err != nil || endpoint == nil || endpoint.Host == "" || endpoint.Opaque != "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || strings.Contains(component.Endpoint, "#") || !strings.HasSuffix(endpoint.EscapedPath(), "/v1/responses") || (endpoint.Scheme != "https" && (endpoint.Scheme != "http" || !isLoopbackHost(endpoint.Hostname()))) {
			return fmt.Errorf("component %q has invalid endpoint", component.ID)
		}
		for _, entry := range []struct{ name, value string }{{"connect_timeout", component.ConnectTimeout}, {"tls_handshake_timeout", component.TLSTimeout}, {"response_header_timeout", component.HeaderTimeout}, {"stream_idle_timeout", component.IdleTimeout}} {
			d, err := time.ParseDuration(entry.value)
			if err != nil || d <= 0 {
				return fmt.Errorf("component %q %s must be positive", component.ID, entry.name)
			}
		}
		if strings.TrimSpace(component.CredentialEnv) == "" || strings.Contains(component.CredentialEnv, "=") {
			return fmt.Errorf("component %q credential_env must name a non-empty environment variable", component.ID)
		}
		value, ok := os.LookupEnv(component.CredentialEnv)
		if !ok || value == "" {
			return fmt.Errorf("component %q credential_env is unset or empty", component.ID)
		}
		c.Credentials[component.CredentialEnv] = secret(value)
	}
	seen := make(map[core.RouteIdentity]struct{}, len(c.Routes))
	for _, route := range c.Routes {
		if route.Protocol != responsesProtocol || route.Mode != core.ModeNative || route.Model == "" || route.Account == "" || route.Adapter == "" || route.Connector == "" {
			return errors.New("route requires supported protocol, native mode, model, account, adapter, and connector")
		}
		identity := core.RouteIdentity{RouteLookupKey: core.RouteLookupKey{Protocol: route.Protocol, Mode: route.Mode, Model: route.Model}, AccountID: route.Account}
		if _, exists := seen[identity]; exists {
			return fmt.Errorf("duplicate route identity %+v", identity)
		}
		seen[identity] = struct{}{}
		if components[route.Adapter] != core.ComponentAdapter {
			return fmt.Errorf("route adapter instance %q is missing or has the wrong kind", route.Adapter)
		}
		if components[route.Connector] != core.ComponentConnector {
			return fmt.Errorf("route connector instance %q is missing or has the wrong kind", route.Connector)
		}
		for capability, state := range route.Capabilities {
			if capability == "" || (state != core.Supported && state != core.Unsupported && state != core.Unknown) {
				return fmt.Errorf("route has invalid capability declaration")
			}
		}
	}
	return nil
}

func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func probes(ready *atomic.Bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	return mux
}

type responsesInit struct {
	Transport connector.Config `json:"transport"`
	Model     string           `json:"model"`
	AccountID string           `json:"account_id"`
}

func handler(c config, ready *atomic.Bool) (http.Handler, func()) {
	return handlerWithFinalize(c, ready, nil)
}

func handlerWithFinalize(c config, ready *atomic.Bool, finalize func(core.AttemptResult)) (http.Handler, func()) {
	var draining atomic.Bool
	return handlerWithLifecycle(c, ready, &draining, finalize)
}

func handlerWithLifecycle(c config, ready, draining *atomic.Bool, finalize func(core.AttemptResult)) (http.Handler, func()) {
	mux := probes(ready)
	h, closeComponents, err := composeHandler(c, ready, draining, finalize)
	if err != nil {
		return mux, func() {}
	}
	return h, closeComponents
}

func composeHandler(c config, ready, draining *atomic.Bool, finalize func(core.AttemptResult)) (http.Handler, func(), error) {
	return composeHandlerWithFactory(c, ready, draining, finalize, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		return connector.NewConnector()
	})
}

func composeHandlerWithFactory(c config, ready, draining *atomic.Bool, finalize func(core.AttemptResult), construct func(topologyComponent) core.Component) (http.Handler, func(), error) {
	mux := probes(ready)
	legacy := len(c.Components) == 0 && c.UpstreamEndpoint != ""
	if legacy {
		c.Components = legacyComponents(c)
		c.Routes = []topologyRoute{{Protocol: responsesProtocol, Mode: core.ModeNative, Model: "gpt-5.4-mini", Account: c.UpstreamCredentialEnv, Adapter: "responses-adapter", Connector: "responses-connector"}}
	}
	if len(c.Components) == 0 {
		return mux, func() {}, nil
	}
	versions := map[core.ComponentKind]core.APIVersion{
		core.ComponentAdapter: {Major: 1}, core.ComponentConnector: {Major: 1},
	}
	registry, err := core.NewRegistry(versions, nil)
	if err != nil {
		return nil, nil, err
	}
	closeComponents := func() { _ = registry.Close(context.Background()) }
	componentConfigs := make(map[core.InstanceID]core.ComponentConfig, len(c.Components))
	connectorRoutes := make(map[core.InstanceID]topologyRoute)
	accountConnectors := make(map[string]core.InstanceID)
	for _, route := range c.Routes {
		if previous, ok := accountConnectors[route.Account]; ok && previous != route.Connector {
			closeComponents()
			return nil, nil, fmt.Errorf("account %q maps to multiple connectors", route.Account)
		}
		accountConnectors[route.Account] = route.Connector
		if prev, ok := connectorRoutes[route.Connector]; ok && (prev.Account != route.Account || prev.Model != route.Model) {
			closeComponents()
			return nil, nil, fmt.Errorf("connector %q cannot serve multiple account/model scopes", route.Connector)
		}
		connectorRoutes[route.Connector] = route
	}
	maxBody, maxHeader := c.MaxRequestBodyBytes, c.MaxRequestHeaderBytes
	for _, item := range c.Components {
		if item.Kind == core.ComponentConnector {
			if item.MaxBodyBytes > maxBody {
				maxBody = item.MaxBodyBytes
			}
			if item.MaxHeaderBytes > maxHeader {
				maxHeader = item.MaxHeaderBytes
			}
		}
	}
	for _, item := range c.Components {
		instance := construct(item)
		var cfg any
		if item.Kind == core.ComponentAdapter {
			cfg = adapterConfig{MaxBodyBytes: maxBody, MaxHeaderBytes: maxHeader}
		} else {
			connect, _ := time.ParseDuration(item.ConnectTimeout)
			tlsHandshake, _ := time.ParseDuration(item.TLSTimeout)
			responseHeader, _ := time.ParseDuration(item.HeaderTimeout)
			streamIdle, _ := time.ParseDuration(item.IdleTimeout)
			route := connectorRoutes[item.ID]
			componentConfigs[item.ID] = core.ComponentConfig{Data: mustJSON(responsesInit{Transport: connector.Config{
				Endpoint: item.Endpoint, ConnectTimeout: connect, TLSHandshakeTimeout: tlsHandshake,
				ResponseHeaderTimeout: responseHeader, StreamIdleTimeout: streamIdle,
			}, Model: route.Model, AccountID: route.Account})}
		}
		if err := registry.Register(item.ID, instance, item.Kind); err != nil {
			closeComponents()
			return nil, nil, err
		}
		if item.Kind == core.ComponentAdapter {
			componentConfigs[item.ID] = core.ComponentConfig{Data: mustJSON(cfg)}
		}
	}
	routes := make([]core.Route, 0, len(c.Routes))
	credentials := make(map[string]map[string]string)
	models := make(map[string]topologyRoute)
	for _, item := range c.Routes {
		if previous, ok := models[item.Model]; ok && previous.Account != item.Account {
			closeComponents()
			return nil, nil, fmt.Errorf("model %q has ambiguous account routes", item.Model)
		}
		models[item.Model] = item
		routes = append(routes, core.Route{Identity: core.RouteIdentity{RouteLookupKey: core.RouteLookupKey{Protocol: item.Protocol, Mode: item.Mode, Model: item.Model}, AccountID: item.Account}, Adapter: item.Adapter, Connector: item.Connector})
		credentials[item.Account] = map[string]string{"bearer": credentialEnv(c, item.Connector)}
	}
	table, err := core.NewRouteTable(routes, registry)
	if err != nil {
		closeComponents()
		return nil, nil, err
	}
	for _, item := range c.Components {
		if err := registry.Init(context.Background(), item.ID, componentConfigs[item.ID]); err != nil {
			closeComponents()
			return nil, nil, fmt.Errorf("initialize component %q: %w", item.ID, err)
		}
	}
	transports := make(map[string]core.HTTPDoer, len(credentials))
	for _, route := range c.Routes {
		component, _, ok := registry.Admit(context.Background(), route.Connector)
		if !ok {
			closeComponents()
			return nil, nil, fmt.Errorf("connector %q is unavailable", route.Connector)
		}
		managed, ok := component.(interface{ HTTPDoer() core.HTTPDoer })
		if !ok {
			closeComponents()
			return nil, nil, fmt.Errorf("connector %q has no scoped transport", route.Connector)
		}
		transports[route.Account] = managed.HTTPDoer()
	}
	var services interface {
		ForAttempt(core.AttemptScope) core.InvocationServices
	} = core.NewEnvironmentServices(credentials, transports, nil)
	if legacy {
		services = fixedLegacyServices{account: c.UpstreamCredentialEnv, credential: []byte(c.credential), transport: transports[c.UpstreamCredentialEnv]}
	}
	dispatchers := make(map[string]*core.Dispatcher, len(models))
	protocolAdapters := make(map[string]core.ProtocolAdapter, len(models))
	for _, route := range c.Routes {
		component, _, ok := registry.Admit(context.Background(), route.Adapter)
		if !ok {
			closeComponents()
			return nil, nil, fmt.Errorf("adapter %q is unavailable", route.Adapter)
		}
		protocolAdapters[route.Model] = component.(core.ProtocolAdapter)
		dispatchers[route.Model] = &core.Dispatcher{Routes: table, Services: services, AccountID: route.Account, Finalize: finalize}
	}
	mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if draining.Load() {
			http.Error(w, "gateway is shutting down", http.StatusServiceUnavailable)
			return
		}
		protocolAdapter := firstAdapter(protocolAdapters)
		req, gatewayErr := protocolAdapter.Decode(r.Context(), core.ClientRequest{Transport: r})
		if gatewayErr != nil {
			_ = protocolAdapter.Encode(r.Context(), core.ClientResponse{Transport: adapter.HTTPResponse{Writer: w, Request: r}}, gatewayErr, core.ExecutionResponse{})
			return
		}
		dispatch := dispatchers[req.Model]
		if dispatch == nil {
			gatewayErr = &core.GatewayError{Code: "unsupported_target", Category: core.CategoryUnsupportedFeature, Message: "Unsupported execution target"}
			_ = protocolAdapter.Encode(r.Context(), core.ClientResponse{Transport: adapter.HTTPResponse{Writer: w, Request: r}}, gatewayErr, core.ExecutionResponse{})
			return
		}
		protocolAdapter = protocolAdapters[req.Model]
		resp, gatewayErr := dispatch.Execute(r.Context(), req)
		_ = protocolAdapter.Encode(r.Context(), core.ClientResponse{Transport: adapter.HTTPResponse{Writer: w, Request: r}}, gatewayErr, resp)
	})
	return mux, closeComponents, nil
}

type fixedLegacyServices struct {
	account    string
	credential []byte
	transport  core.HTTPDoer
}

func (s fixedLegacyServices) ForAttempt(scope core.AttemptScope) core.InvocationServices {
	var access core.CredentialAccess = fixedLegacyCredential(s.credential)
	if scope.AccountID != s.account {
		access = unavailableCredential{}
	}
	return core.InvocationServices{Credentials: access, Transport: s.transport}
}

type fixedLegacyCredential []byte

func (c fixedLegacyCredential) Get(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if name != "bearer" || len(c) == 0 {
		return nil, core.ErrCredentialUnavailable
	}
	return append([]byte(nil), c...), nil
}

type unavailableCredential struct{}

func (unavailableCredential) Get(context.Context, string) ([]byte, error) {
	return nil, core.ErrCredentialUnavailable
}

func firstAdapter(adapters map[string]core.ProtocolAdapter) core.ProtocolAdapter {
	for _, value := range adapters {
		return value
	}
	return nil
}

type adapterConfig struct {
	MaxBodyBytes   int64 `json:"max_body_bytes"`
	MaxHeaderBytes int64 `json:"max_header_bytes"`
}

func mustJSON(value any) []byte { data, _ := json.Marshal(value); return data }

func credentialEnv(c config, id core.InstanceID) string {
	for _, item := range c.Components {
		if item.ID == id {
			return item.CredentialEnv
		}
	}
	return ""
}

func run(ctx context.Context, args []string) error {
	c, timeout, err := loadConfig(args)
	if err != nil {
		return err
	}
	var ready atomic.Bool
	var draining atomic.Bool
	h, closeTransport, err := composeHandler(c, &ready, &draining, nil)
	if err != nil {
		return fmt.Errorf("initialize gateway: %w", err)
	}
	defer closeTransport()
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return fmt.Errorf("listen %q: %w", c.Listen, err)
	}
	server := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() {
		ready.Store(true)
		done <- server.Serve(listener)
	}()
	fmt.Fprintf(os.Stderr, "gateway: listening on %s\n", listener.Addr())
	select {
	case err := <-done:
		draining.Store(true)
		ready.Store(false)
		if err != nil {
			server.Close()
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		draining.Store(true)
		ready.Store(false)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			server.Close()
			<-done
			return fmt.Errorf("shutdown: %w", err)
		}
		<-done
		return nil
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}
