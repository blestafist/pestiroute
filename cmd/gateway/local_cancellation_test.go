package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func localCancellationGateway(t *testing.T, upstream http.Handler, setupConn ...func(net.Conn)) (*httptest.Server, string, *atomic.Bool, func(context.Context) error) {
	t.Helper()
	_, dbPath, keyPath := protectedFixture(t)
	ctx := context.Background()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const connectorID, alternateID, accountID, alternateAccount, model = "local", "local-alt", "local-account", "local-alt-account", "local-model"
	for _, account := range []sqlite.Account{{ID: accountID, Connector: connectorID, Enabled: true}, {ID: alternateAccount, Connector: alternateID, Enabled: true}} {
		if _, err := sqlite.NewAccounts(db).Create(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := sqlite.NewKeyPolicies(db).GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	policy, err = sqlite.NewKeyPolicies(db).Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{"model-a", model}, Connectors: []string{"upstream", connectorID, alternateID}, RPM: 20, TPM: 10000})
	if err != nil {
		t.Fatal(err)
	}
	issued, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(upstream)
	t.Cleanup(backend.Close)
	p := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0").protected
	p.Connectors[0] = protectedConnector{ID: connectorID, Kind: "connector", Implementation: "pestiroute.responses.local", Protocols: []string{responsesProtocol}, Settings: nativeSettings{
		BaseURL: backend.URL + "/v1/responses", RuntimeProfile: "llamacpp-v0.6.0", AuthMode: "none", MaxRequestBodyBytes: 1 << 20, MaxRequestHeaderBytes: 8192,
		ConnectTimeout: "1s", TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s", StreamIdleTimeout: "30s", Model: model, AccountID: accountID,
	}}
	p.Connectors[0].Settings.Capabilities = map[string]string{"llm.streaming": "supported"}
	alt := p.Connectors[0]
	alt.ID = alternateID
	alt.Settings.AccountID = alternateAccount
	p.Connectors = append(p.Connectors, alt)
	targets := []routeTarget{{Connector: connectorID, Account: accountID}, {Connector: alternateID, Account: alternateAccount}}
	maxAttempts := 2
	// Leave enough time for the 48-MiB backpressure fixture under the race
	// detector. Drain expiry is exercised with its own shutdown deadline.
	p.Routes[0] = protectedRoute{ID: "local-route", Protocol: responsesProtocol, Mode: "native", Model: model, Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(20)}, Targets: targets, Retry: &retryConfig{MaxAttempts: &maxAttempts, Deadline: "30s"}}
	prepared, err := prepareProtectedConfig(ctx, config{protected: p, DatabasePath: dbPath, MasterKeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	ready, draining := atomic.Bool{}, atomic.Bool{}
	ready.Store(true)
	h, closeComponents, err := composeHandlerWithFactory(prepared, &ready, &draining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		if item.Implementation == "pestiroute.responses.local" {
			return responses.NewLocalConnector(item.AuthMode)
		}
		return responses.NewConnector()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeComponents(context.Background()) })
	server := httptest.NewUnstartedServer(h)
	if len(setupConn) > 0 {
		server.Config.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
			setupConn[0](conn)
			return ctx
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	return server, issued.Secret, &draining, closeComponents
}

func localWireRequest(t *testing.T, address, secret string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(address, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"local-model","stream":true,"store":false,"input":"hello"}`
	if _, err := fmt.Fprintf(conn, "POST /v1/responses HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", secret, len(body), body); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	return conn
}

func TestLocalProtectedCancelDisconnectStopsUpstreamWithoutRetry(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server, secret, _, _ := localCancellationGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	conn := localWireRequest(t, server.URL, secret)
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
	<-started
	_ = conn.Close()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("client disconnect did not cancel upstream HTTP request")
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d, want one; committed/unknown delivery must not retry", calls.Load())
	}
	// Upstream request cancellation proves transport cleanup only, not remote inference cancellation.
}

func TestLocalProtectedBackpressureBoundsAndPreservesOrder(t *testing.T) {
	const events = 768
	const maxStalledEvents = 64 // 4 MiB of 64-KiB deltas is the generous per-stream socket/transport bound.
	started, finished := make(chan struct{}), make(chan struct{})
	var progress atomic.Int32
	server, secret, _, _ := localCancellationGateway(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < events; i++ {
			text := fmt.Sprintf("%04d", i) + strings.Repeat("x", 64<<10)
			_, _ = fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", text)
			w.(http.Flusher).Flush()
			progress.Add(1)
			if i == 0 {
				close(started)
			}
		}
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		close(finished)
	}), func(conn net.Conn) {
		if err := conn.(*net.TCPConn).SetWriteBuffer(32 << 10); err != nil {
			t.Errorf("set gateway TCP write buffer: %v", err)
		}
	})
	conn := localWireRequest(t, server.URL, secret)
	defer conn.Close()
	if err := conn.(*net.TCPConn).SetReadBuffer(32 << 10); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
	<-started
	select {
	case <-finished:
		t.Fatal("upstream completed all events while client socket was stalled")
	case <-time.After(300 * time.Millisecond):
	}
	if got := progress.Load(); got > maxStalledEvents {
		t.Fatalf("stalled stream advanced %d 64-KiB deltas (%d MiB), exceeding the %d-event (4-MiB) per-stream bound", got, got*64/1024, maxStalledEvents)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not finish after client resumed reading")
	}
	stream := string(body)
	position := 0
	for i := 0; i < events; i++ {
		marker := fmt.Sprintf(`"delta":"%04d`, i)
		next := strings.Index(stream[position:], marker)
		if next < 0 {
			t.Fatalf("stream lost or reordered event %d", i)
		}
		position += next + len(marker)
	}
	if !strings.Contains(stream, "event: response.completed") {
		t.Fatal("resumed stream lost terminal event")
	}
}

func TestProtectedLocalDrainCompletesAndExpiryCancels(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(map[bool]string{false: "drain", true: "expiry"}[expire], func(t *testing.T) {
			started, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			shutdownStarted := make(chan struct{})
			server, secret, draining, closeComponents := localCancellationGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
				w.(http.Flusher).Flush()
				close(started)
				select {
				case <-release:
					_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
				case <-r.Context().Done():
					close(canceled)
				}
			}))
			server.Config.RegisterOnShutdown(func() { close(shutdownStarted) })
			type responseResult struct {
				status int
				body   []byte
				err    error
			}
			done := make(chan responseResult, 1)
			go func() {
				req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"local-model","stream":true,"store":false,"input":"hello"}`))
				req.Header.Set("Authorization", "Bearer "+secret)
				req.Header.Set("Content-Type", "application/json")
				resp, err := server.Client().Do(req)
				result := responseResult{err: err}
				if err == nil {
					result.status = resp.StatusCode
					result.body, result.err = io.ReadAll(resp.Body)
					_ = resp.Body.Close()
				}
				done <- result
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("local request did not reach upstream")
			}
			draining.Store(true)
			if expire {
				ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
				err := server.Config.Shutdown(ctx)
				cancel()
				if err != context.DeadlineExceeded {
					t.Fatalf("shutdown error=%v, want deadline exceeded", err)
				}
				_ = server.Config.Close()
				if err := closeComponents(context.Background()); err != nil {
					t.Fatal(err)
				}
				select {
				case <-canceled:
				case <-time.After(2 * time.Second):
					t.Fatal("expired drain did not cancel local upstream attempt")
				}
			} else {
				shutdown := make(chan error, 1)
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					shutdown <- server.Config.Shutdown(ctx)
				}()
				select {
				case <-shutdownStarted:
				case <-time.After(2 * time.Second):
					t.Fatal("gateway shutdown did not enter its shutdown lifecycle")
				}
				select {
				case err := <-shutdown:
					t.Fatalf("shutdown stopped draining before release: %v", err)
				default:
				}
				close(release)
				if err := <-shutdown; err != nil {
					t.Fatal(err)
				}
				if err := closeComponents(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case result := <-done:
				if !expire && (result.err != nil || result.status != http.StatusOK || !strings.Contains(string(result.body), "event: response.completed")) {
					t.Fatalf("drained local response status=%d err=%v body=%q; want terminal response.completed", result.status, result.err, result.body)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("local request remained active after shutdown")
			}
		})
	}
}
