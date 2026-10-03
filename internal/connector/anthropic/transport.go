package anthropic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

const messagesEndpoint = "https://api.anthropic.com/v1/messages"

type Transport struct {
	client    *http.Client
	transport *http.Transport
}

func NewTransport() *Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.DisableCompression = true
	tr.ForceAttemptHTTP2 = false
	return &Transport{transport: tr, client: &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (t *Transport) Request(ctx context.Context, in core.ExecutionRequest, credential string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, messagesEndpoint, io.NopCloser(bytes.NewReader(in.Payload.Body)))
	if err != nil {
		return nil, err
	}
	req.GetBody = nil
	req.ContentLength = int64(len(in.Payload.Body))
	blocked := map[string]bool{
		"connection": true, "keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true,
		"proxy-connection": true, "te": true, "trailer": true, "trailers": true, "transfer-encoding": true,
		"upgrade": true, "authorization": true, "x-api-key": true, "anthropic-version": true,
		"anthropic-beta": true, "cookie": true, "cookie2": true, "host": true,
		"content-length": true, "content-type": true, "accept": true, "accept-encoding": true,
	}
	for name, values := range in.Metadata.Headers {
		if strings.EqualFold(name, "Connection") {
			for _, value := range values {
				for field := range strings.SplitSeq(value, ",") {
					blocked[strings.ToLower(strings.TrimSpace(field))] = true
				}
			}
		}
	}
	for name, values := range in.Metadata.Headers {
		lower := strings.ToLower(name)
		if !blocked[lower] && (lower == "user-agent" || lower == "traceparent" || lower == "tracestate") {
			for _, value := range values {
				req.Header.Add(name, value)
			}
		}
	}
	req.Header.Set("X-Api-Key", credential)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	return req, nil
}

func (t *Transport) Do(ctx context.Context, in core.ExecutionRequest, services core.InvocationServices) (*http.Response, error) {
	if services.Credentials == nil {
		return nil, errCredentialUnavailable
	}
	credential, err := services.Credentials.Get(ctx, "bearer")
	if err != nil || len(credential) == 0 {
		credential, err = services.Credentials.Get(ctx, "api_key")
	}
	if err != nil || len(credential) == 0 {
		return nil, errCredentialUnavailable
	}
	req, err := t.Request(ctx, in, string(credential))
	if err != nil {
		return nil, err
	}
	doer := services.Transport
	if doer == nil {
		doer = t.client
	}
	resp, err := doer.Do(req)
	if err != nil {
		// Doer errors may include the request, so never forward their detail.
		return nil, errUpstreamRequest
	}
	return resp, nil
}

func (t *Transport) Close() { t.transport.CloseIdleConnections() }

var (
	errCredentialUnavailable = errors.New("selected credential is unavailable")
	errUpstreamRequest       = errors.New("upstream request failed")
)
