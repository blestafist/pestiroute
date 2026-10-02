// Package responses provides the native Responses upstream HTTP transport.
package responses

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

// Config contains only the runtime-selected endpoint and validated startup
// timeouts. It must not be populated from client metadata.
type Config struct {
	Endpoint              string
	ConnectTimeout        time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	StreamIdleTimeout     time.Duration
}

type Transport struct {
	endpoint          string
	streamIdleTimeout time.Duration
	client            *http.Client
	transport         *http.Transport
}

// NewTransport creates a reusable one-target transport from validated startup
// settings. The caller must close every response body, including redirects.
func NewTransport(c Config) *Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = (&net.Dialer{Timeout: c.ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = c.TLSHandshakeTimeout
	tr.ResponseHeaderTimeout = c.ResponseHeaderTimeout
	tr.DisableCompression = true
	// HTTP/1's non-replayable body guarantees no resend after a reused
	// connection fails; avoid the HTTP/2 no-cached-connection retry path.
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	return &Transport{endpoint: c.Endpoint, streamIdleTimeout: c.StreamIdleTimeout, transport: tr,
		client: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// Request constructs a non-replayable POST with opaque native body bytes.
func (t *Transport) Request(ctx context.Context, in core.ExecutionRequest, credential string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, io.NopCloser(bytes.NewReader(in.Payload.Body)))
	if err != nil {
		return nil, err
	}
	req.GetBody = nil
	req.ContentLength = int64(len(in.Payload.Body))
	blocked := map[string]bool{"connection": true, "keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true, "proxy-connection": true, "te": true, "trailer": true, "trailers": true, "transfer-encoding": true, "upgrade": true, "authorization": true, "host": true, "content-length": true, "content-type": true, "accept-encoding": true}
	for name, values := range in.Metadata.Headers {
		if strings.EqualFold(name, "Connection") {
			for _, value := range values {
				for _, field := range strings.Split(value, ",") {
					blocked[strings.ToLower(strings.TrimSpace(field))] = true
				}
			}
		}
	}
	for name, values := range in.Metadata.Headers {
		if !blocked[strings.ToLower(name)] {
			for _, value := range values {
				req.Header.Add(name, value)
			}
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Authorization", "Bearer "+credential)
	return req, nil
}

// Do performs one invocation; the caller owns the returned response body.
func (t *Transport) Do(ctx context.Context, in core.ExecutionRequest, credential string, doer core.HTTPDoer) (*http.Response, error) {
	req, err := t.Request(ctx, in, credential)
	if err != nil {
		return nil, err
	}
	if doer != nil {
		return doer.Do(req)
	}
	return t.client.Do(req)
}

func (t *Transport) Close() { t.transport.CloseIdleConnections() }
