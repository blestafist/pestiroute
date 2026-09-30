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
}

// secret stays in runtime state, never in configuration output or diagnostics.
type secret string

func (secret) String() string   { return "[REDACTED]" }
func (secret) GoString() string { return "[REDACTED]" }

func (c config) String() string {
	c.credential = ""
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
	if count != 0 {
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
	}
	return c, d, nil
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

type fixedTarget struct{ *connector.Transport }

func (t fixedTarget) Execute(ctx context.Context, req core.ExecutionRequest, _ core.AttemptScope) (core.ExecutionResponse, *core.GatewayError) {
	if req.Metadata.Streaming != nil && *req.Metadata.Streaming {
		return t.ExecuteSSE(ctx, req)
	}
	return t.ExecuteFixedJSON(ctx, req)
}

func handler(c config, ready *atomic.Bool) (http.Handler, func()) {
	return handlerWithFinalize(c, ready, nil)
}

func handlerWithFinalize(c config, ready *atomic.Bool, finalize func(core.AttemptResult)) (http.Handler, func()) {
	mux := probes(ready)
	if c.UpstreamEndpoint == "" {
		return mux, func() {}
	}
	connect, _ := time.ParseDuration(c.ConnectTimeout)
	tlsHandshake, _ := time.ParseDuration(c.TLSHandshakeTimeout)
	responseHeader, _ := time.ParseDuration(c.ResponseHeaderTimeout)
	transport := connector.NewTransport(connector.Config{
		Endpoint: c.UpstreamEndpoint, Credential: string(c.credential), ConnectTimeout: connect,
		TLSHandshakeTimeout: tlsHandshake, ResponseHeaderTimeout: responseHeader,
	})
	dispatch := &core.Dispatcher{Target: fixedTarget{transport}, AccountID: c.UpstreamCredentialEnv, Finalize: finalize}
	endpoint, _ := url.Parse(c.UpstreamEndpoint)
	if endpoint.Scheme == "http" && isLoopbackHost(endpoint.Hostname()) {
		// Fixture-only support; public-account streaming needs live verification.
		dispatch.Adapter = map[core.Capability]core.CapabilityState{"llm.streaming": core.Supported}
		dispatch.Connector = map[core.Capability]core.CapabilityState{"llm.streaming": core.Supported}
	}
	mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		req, gatewayErr := adapter.Decode(r, c.MaxRequestBodyBytes, c.MaxRequestHeaderBytes)
		if gatewayErr != nil {
			_ = adapter.Encode(w, r, core.ExecutionResponse{}, gatewayErr)
			return
		}
		resp, gatewayErr := dispatch.Execute(r.Context(), req)
		_ = adapter.Encode(w, r, resp, gatewayErr)
	})
	return mux, transport.Close
}

func run(ctx context.Context, args []string) error {
	c, timeout, err := loadConfig(args)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return fmt.Errorf("listen %q: %w", c.Listen, err)
	}
	var ready atomic.Bool
	h, closeTransport := handler(c, &ready)
	defer closeTransport()
	server := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() {
		ready.Store(true)
		done <- server.Serve(listener)
	}()
	fmt.Fprintf(os.Stderr, "gateway: listening on %s\n", listener.Addr())
	select {
	case err := <-done:
		ready.Store(false)
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
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
