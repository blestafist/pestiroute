package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newHTTP2OfferServer(handler http.Handler) *httptest.Server {
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	return server
}

func http2OfferTestTLSConfig(server *httptest.Server) *tls.Config {
	client := server.Client()
	transport := client.Transport.(*http.Transport)
	config := transport.TLSClientConfig.Clone()
	config.NextProtos = []string{"h2", "http/1.1"}
	return config
}

func TestCodexTransportForcesHTTP1AndKeepsStreaming(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	finished := make(chan struct{})
	requestProto := make(chan int, 1)
	server := newHTTP2OfferServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestProto <- r.ProtoMajor
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: first\n\n")
		flusher.Flush()
		<-release
		_, _ = io.WriteString(w, "data: second\n\n")
		flusher.Flush()
		close(finished)
	}))
	defer server.Close()

	originalTLS := http2OfferTestTLSConfig(server)
	originalTLS.Certificates = append(originalTLS.Certificates, server.TLS.Certificates...)
	var verifierCalls atomic.Int32
	originalTLS.VerifyConnection = func(tls.ConnectionState) error {
		verifierCalls.Add(1)
		return nil
	}
	originalProtocols := append([]string(nil), originalTLS.NextProtos...)
	source := http.DefaultTransport.(*http.Transport).Clone()
	source.TLSClientConfig = originalTLS
	transport := source.Clone()
	configureCodexTransport(transport)
	defer transport.CloseIdleConnections()

	if transport.TLSClientConfig == originalTLS || len(transport.TLSClientConfig.NextProtos) != 1 || transport.TLSClientConfig.NextProtos[0] != "http/1.1" {
		t.Fatal("configured Codex transport did not isolate an HTTP/1.1-only TLS policy")
	}
	if len(originalTLS.NextProtos) != len(originalProtocols) || originalTLS.NextProtos[0] != "h2" || originalTLS.NextProtos[1] != "http/1.1" {
		t.Fatal("configuring the clone mutated the caller TLS ALPN list")
	}
	if transport.TLSClientConfig.RootCAs != originalTLS.RootCAs || len(transport.TLSClientConfig.Certificates) != len(originalTLS.Certificates) ||
		!bytes.Equal(transport.TLSClientConfig.Certificates[0].Certificate[0], originalTLS.Certificates[0].Certificate[0]) ||
		transport.TLSClientConfig.VerifyConnection == nil {
		t.Fatal("configured TLS clone did not preserve trust, client certificates, and verifier")
	}
	if transport.ForceAttemptHTTP2 || len(transport.TLSNextProto) != 0 {
		t.Fatal("Codex transport unexpectedly enabled HTTP/2 dispatch")
	}

	phases := &diagnosticRequestPhases{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal("request construction failed")
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), phases.trace()))
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		t.Fatal("configured transport failed to parse HTTP/1.1 response")
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 1 {
		t.Fatalf("response protocol major=%d, want 1", resp.ProtoMajor)
	}
	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first != "data: first\n" {
		t.Fatal("first flushed response event was not delivered")
	}
	if blank, err := reader.ReadString('\n'); err != nil || blank != "\n" {
		t.Fatal("first SSE event delimiter was not delivered")
	}
	select {
	case <-finished:
		t.Fatal("server completed before the client received the first flushed chunk")
	default:
	}
	select {
	case proto := <-requestProto:
		if proto != 1 {
			t.Fatalf("server observed HTTP major version %d, want 1", proto)
		}
	default:
		t.Fatal("server did not observe the request")
	}
	releaseOnce.Do(func() { close(release) })
	second, err := reader.ReadString('\n')
	if err != nil || second != "data: second\n" {
		t.Fatal("resumed stream chunk was not delivered")
	}
	if diagnosticProtocolName(phases.tlsProtocol.Load()) != "http1" || verifierCalls.Load() == 0 {
		t.Fatalf("TLS diagnostic=%s verifier_called=%t", diagnosticProtocolName(phases.tlsProtocol.Load()), verifierCalls.Load() != 0)
	}
}

type genericTransportProbe struct {
	client *http.Client
	calls  *atomic.Int32
}

func (p genericTransportProbe) Do(req *http.Request) (*http.Response, error) {
	p.calls.Add(1)
	return p.client.Do(req)
}

func TestCodexRejectsGenericHTTPDoerBeforeNetwork(t *testing.T) {
	var networkCalls atomic.Int32
	client := &http.Client{Transport: authRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		networkCalls.Add(1)
		return nil, errors.New("synthetic network must not run")
	})}
	var wrapperCalls atomic.Int32
	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/backend-api/codex/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, closeFn, err := doCodexRequest(genericTransportProbe{client: client, calls: &wrapperCalls}, req)
	if resp != nil || err == nil {
		t.Fatal("generic Doer was not rejected")
	}
	closeFn()
	if wrapperCalls.Load() != 0 || networkCalls.Load() != 0 {
		t.Fatalf("generic wrapper reached network: do=%d network=%d", wrapperCalls.Load(), networkCalls.Load())
	}
}

func TestCodexTransportReproducesInheritedHTTP2ALPNFailure(t *testing.T) {
	server := newHTTP2OfferServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Recreate the pre-fix Codex transport settings with caller-inherited h2 ALPN.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = http2OfferTestTLSConfig(server)
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	defer transport.CloseIdleConnections()
	phases := &diagnosticRequestPhases{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal("request construction failed")
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), phases.trace()))
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || resp != nil {
		t.Fatalf("inherited h2 ALPN did not reproduce an unparsable response: error=%t class=%s response=%t", err != nil, classifyDiagnosticTransportError(err), resp != nil)
	}
	if diagnosticProtocolName(phases.tlsProtocol.Load()) != "http2" {
		t.Fatalf("reproduction negotiated %s instead of HTTP/2", diagnosticProtocolName(phases.tlsProtocol.Load()))
	}
}

func TestCodexTransportEnforcesCustomDialerALPN(t *testing.T) {
	var requests atomic.Int32
	server := newHTTP2OfferServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	customTLS := http2OfferTestTLSConfig(server)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = customTLS.Clone()
	var negotiated atomic.Uint32
	transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&tls.Dialer{NetDialer: &net.Dialer{}, Config: customTLS}).DialContext(ctx, network, address)
		if err == nil {
			if stateConn, ok := conn.(interface{ ConnectionState() tls.ConnectionState }); ok {
				negotiated.Store(diagnosticProtocolCode(diagnosticProtocolLabel(stateConn.ConnectionState().NegotiatedProtocol)))
			}
		}
		return conn, err
	}
	configureCodexTransport(transport)
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal("request construction failed")
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || requests.Load() != 0 || diagnosticProtocolName(negotiated.Load()) != "http2" {
		t.Fatalf("custom TLS enforcement failed: request_error=%t server_requests=%d tls_protocol=%s", err != nil, requests.Load(), diagnosticProtocolName(negotiated.Load()))
	}

	customTLS.NextProtos = []string{"http/1.1"}
	transport = http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = customTLS.Clone()
	transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&tls.Dialer{NetDialer: &net.Dialer{}, Config: customTLS}).DialContext(ctx, network, address)
	}
	configureCodexTransport(transport)
	defer transport.CloseIdleConnections()
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal("request construction failed")
	}
	resp, err = (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		t.Fatal("custom TLS dialer HTTP/1.1 request failed")
	}
	_ = resp.Body.Close()
	if resp.ProtoMajor != 1 || requests.Load() != 1 {
		t.Fatal("custom TLS dialer did not preserve the HTTP/1.1 policy")
	}
}
