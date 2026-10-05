package codex

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

const (
	codexResponsesEndpoint   = "https://chatgpt.com/backend-api/codex/responses"
	codexConnectTimeout      = 2 * time.Second
	codexTLSHandshakeTimeout = 2 * time.Second
	codexHeaderTimeout       = 30 * time.Second
	codexStreamIdleTimeout   = 120 * time.Second
	codexRequestTimeout      = 30 * time.Minute
)

func newHTTPTransport() (*http.Transport, *http.Client) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	configureCodexTransport(transport)
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return transport, client
}

func configureCodexTransport(transport *http.Transport) {
	dialContext := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialCtx, cancel := context.WithTimeout(ctx, codexConnectTimeout)
		defer cancel()
		if dialContext != nil {
			return dialContext(dialCtx, network, address)
		}
		return (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext(dialCtx, network, address)
	}
	if dialTLSContext := transport.DialTLSContext; dialTLSContext != nil {
		transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			tlsCtx, cancel := context.WithTimeout(ctx, codexTLSHandshakeTimeout)
			defer cancel()
			return dialTLSContext(tlsCtx, network, address)
		}
	}
	transport.TLSHandshakeTimeout = codexTLSHandshakeTimeout
	transport.ResponseHeaderTimeout = codexHeaderTimeout
	transport.DisableCompression = true
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
}

// doCodexRequest preserves the runtime client's proxy/TLS policy while
// overriding transport behavior required by this non-replayable attempt. A
// generic Doer is rejected because its redirect, compression, and timeout
// behavior cannot be constrained at this boundary.
func doCodexRequest(doer core.HTTPDoer, req *http.Request) (*http.Response, func(), error) {
	if client, ok := doer.(*http.Client); ok {
		clientCopy := *client
		client = &clientCopy
		transport, ok := client.Transport.(*http.Transport)
		if client.Transport == nil {
			transport, ok = http.DefaultTransport.(*http.Transport)
		}
		if !ok {
			return nil, func() {}, errors.New("Codex transport cannot enforce request policy")
		}
		transport = transport.Clone()
		if transport.DialTLS != nil {
			return nil, func() {}, errors.New("Codex transport cannot enforce TLS handshake timeout")
		}
		configureCodexTransport(transport)
		client.Transport = transport
		client.Jar = nil
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := client.Do(req)
		return resp, transport.CloseIdleConnections, err
	}
	return nil, func() {}, errors.New("Codex invocation transport cannot enforce request policy")
}
