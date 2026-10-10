package main

import (
	"bufio"
	"context"
	"crypto/tls"
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
	"github.com/blestafist/pestiroute/internal/connector/codex"
	"github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func codexCancellationGateway(t *testing.T, upstream http.Handler) (*httptest.Server, string, *atomic.Int32, *atomic.Bool, func(context.Context) error) {
	t.Helper()
	_, dbPath, keyPath := protectedFixture(t)
	ctx := context.Background()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const accountID, connectorID, model = "codex-account", "codex", "codex-model"
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: accountID, Connector: connectorID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	bundle := fmt.Sprintf(`{"version":1,"access_token":%q,"refresh_token":"synthetic-refresh","account_id":%q,"expires_at":%q}`, codexTestJWT(accountID), accountID, expires.Format(time.RFC3339Nano))
	sealed, err := secure.Seal(key, 1, "v1", "credentials", "oauth", accountID, []byte(bundle))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "oauth", AccountID: accountID, FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext, ExpiresAt: &expires}); err != nil {
		t.Fatal(err)
	}
	policy, err := sqlite.NewKeyPolicies(db).GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	policy, err = sqlite.NewKeyPolicies(db).Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{model, "model-a"}, Connectors: []string{connectorID, "upstream"}, RPM: 20, TPM: 10000})
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
	var sends atomic.Int32
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		upstream.ServeHTTP(w, r)
	}))
	t.Cleanup(backend.Close)
	addr := backend.Listener.Addr().String()
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	doer := &http.Client{Transport: transport}
	p := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0").protected
	p.Connectors = append(p.Connectors, protectedConnector{ID: connectorID, Kind: "connector", Implementation: "pestiroute.codex.responses", Protocols: []string{responsesProtocol}, Settings: nativeSettings{Profile: "codex-responses-http-sse-v1", Model: model, AccountID: accountID}})
	p.Routes = append(p.Routes, protectedRoute{ID: "codex-route", Protocol: responsesProtocol, Mode: "native", Model: model, Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(100)}, Targets: []routeTarget{{Connector: connectorID, Account: accountID}}})
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
		if item.Implementation == "pestiroute.codex.responses" {
			return codexCompositionConnector{Connector: codex.NewConnector(), doer: doer}
		}
		return responses.NewConnector()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeComponents(context.Background()) })
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	return server, issued.Secret, &sends, &draining, closeComponents
}

func codexWireRequest(t *testing.T, address, secret string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(address, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"codex-model","stream":true,"store":false,"input":"hello"}`
	if _, err := fmt.Fprintf(conn, "POST /v1/responses HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", secret, len(body), body); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	return conn
}

func TestCodexCancellationClientDisconnectClosesUpstream(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	var upstreamCalls atomic.Int32
	server, secret, _, _, _ := codexCancellationGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upstreamCalls.Add(1) > 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, codexStreamPrefix)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	conn := codexWireRequest(t, server.URL, secret)
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
	if _, err := io.ReadFull(response.Body, make([]byte, len(codexStreamPrefix))); err != nil {
		t.Fatal(err)
	}
	<-started
	_ = conn.Close()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("client disconnect did not cancel the upstream request")
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("cancelled request upstream calls=%d, want one (no post-commit fallback)", got)
	}
	// Closing the local request only proves transport cancellation; remote
	// provider compute cancellation is unobservable and remains unknown.
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"codex-model","stream":true,"store":false,"input":"independent"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	response, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "event: response.completed") || upstreamCalls.Load() != 2 {
		t.Fatalf("subsequent request status=%d calls=%d err=%v", response.StatusCode, upstreamCalls.Load(), err)
	}
}

func TestCodexCancellationStalledReaderBoundsAndResumesInOrder(t *testing.T) {
	const events = 768
	started, finished := make(chan struct{}), make(chan struct{})
	var progress atomic.Int32
	server, secret, _, _, _ := codexCancellationGateway(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < events; i++ {
			if i == 0 {
				close(started)
			}
			text := fmt.Sprintf("%04d", i) + strings.Repeat("x", 64<<10)
			_, _ = fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", text)
			w.(http.Flusher).Flush()
			progress.Add(1)
		}
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		close(finished)
	}))
	conn := codexWireRequest(t, server.URL, secret)
	defer conn.Close()
	if err := conn.(*net.TCPConn).SetReadBuffer(32 << 10); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
	<-started
	select {
	case <-finished:
		t.Fatal("upstream completed all events while the downstream socket was stalled")
	case <-time.After(300 * time.Millisecond):
	}
	if got := progress.Load(); got >= events {
		t.Fatalf("upstream advanced through all %d events while downstream was stalled", got)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	<-finished
	if got, want := strings.Count(string(body), "event: response.output_text.delta"), events; got != want {
		t.Fatalf("resumed event count=%d want=%d", got, want)
	}
	stream := string(body)
	position := 0
	for i := 0; i < events; i++ {
		marker := fmt.Sprintf(`"delta":"%04d`, i)
		next := strings.Index(stream[position:], marker)
		if next < 0 {
			t.Fatalf("resumed stream lost/reordered event %d", i)
		}
		position += next + len(marker)
	}
	if !strings.Contains(stream, "event: response.completed") {
		t.Fatal("resumed stream lost terminal event")
	}
}

func TestCodexCancellationDrainAndForcedExpiry(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "drain", true: "expiry"}[force], func(t *testing.T) {
			started, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			server, secret, _, draining, closeComponents := codexCancellationGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, codexStreamPrefix)
				w.(http.Flusher).Flush()
				close(started)
				select {
				case <-release:
					_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":1}}}\n\n")
				case <-r.Context().Done():
					close(canceled)
				}
			}))
			responseDone := make(chan error, 1)
			go func() {
				req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"codex-model","stream":true,"store":false,"input":"hello"}`))
				req.Header.Set("Authorization", "Bearer "+secret)
				req.Header.Set("Content-Type", "application/json")
				resp, err := server.Client().Do(req)
				if err == nil {
					_, err = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				responseDone <- err
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("stream did not reach upstream")
			}
			if force {
				draining.Store(true)
				ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
				err := server.Config.Shutdown(ctx)
				cancel()
				if err != context.DeadlineExceeded {
					t.Fatalf("shutdown error=%v, want deadline exceeded", err)
				}
				if err := server.Config.Close(); err != nil {
					t.Fatal(err)
				}
				if err := closeComponents(context.Background()); err != nil {
					t.Fatal(err)
				}
				select {
				case <-canceled:
				case <-time.After(2 * time.Second):
					t.Fatal("expired drain did not cancel upstream work")
				}
			} else {
				draining.Store(true)
				shutdownDone := make(chan error, 1)
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					shutdownDone <- server.Config.Shutdown(ctx)
				}()
				select {
				case err := <-shutdownDone:
					t.Fatalf("shutdown stopped draining before release: %v", err)
				default:
				}
				close(release)
				select {
				case err := <-shutdownDone:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("gateway did not drain active Codex stream")
				}
				if err := closeComponents(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-responseDone:
				if err != nil && !force {
					t.Fatalf("drained response: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("client remained active after drain/cancel")
			}
		})
	}
}
