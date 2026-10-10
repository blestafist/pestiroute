package main

import (
	"context"
	"crypto/tls"
	"database/sql"
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
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

type codexFallbackFixture struct {
	server  *httptest.Server
	doer    *http.Client
	key     string
	db      *sql.DB
	sends   atomic.Int32
	mode    atomic.Int32
	release chan struct{}
}

type codexSafeRejectionConnector struct{ *codex.Connector }

func (codexSafeRejectionConnector) Execute(context.Context, core.ExecutionRequest, core.AttemptScope, core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	return core.ExecutionResponse{}, &core.GatewayError{Code: "proven_no_send", Category: core.CategoryUnavailable, Retryable: true, RetryDisposition: core.RetrySafe}
}

func newCodexFallbackFixture(t *testing.T, targets int) *codexFallbackFixture {
	return newCodexFallbackFixtureMode(t, targets, false)
}

func newCodexFallbackFixtureMode(t *testing.T, targets int, safeFirst bool) *codexFallbackFixture {
	t.Helper()
	fixture := &codexFallbackFixture{release: make(chan struct{})}
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.sends.Add(1)
		if r.Header.Get("Authorization") == "" || r.Header.Get("ChatGPT-Account-Id") == "" {
			t.Error("Codex request missing scoped credentials")
		}
		switch fixture.mode.Load() {
		case 1:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
		case 2:
			w.WriteHeader(http.StatusForbidden)
		case 3:
			w.WriteHeader(http.StatusTooManyRequests)
		case 4:
			w.WriteHeader(http.StatusInternalServerError)
		case 5:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n")
		case 6:
			<-fixture.release
			w.WriteHeader(http.StatusInternalServerError)
		case 7:
			hijacker, ok := w.(http.Hijacker)
			if ok {
				conn, _, err := hijacker.Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		}
	}))
	t.Cleanup(backend.Close)
	addr := backend.Listener.Addr().String()
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	fixture.doer = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	_, dbPath, keyPath := protectedFixture(t)
	ctx := context.Background()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	master, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	const model = "codex-fallback-model"
	connectors, accounts := make([]protectedConnector, 0, targets), make([]routeTarget, 0, targets)
	for i := range targets {
		account, connector, credential := fmt.Sprintf("codex-account-%d", i), fmt.Sprintf("codex-%d", i), fmt.Sprintf("codex-credential-%d", i)
		if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: account, Connector: connector, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		expires := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
		bundle := fmt.Sprintf(`{"version":1,"access_token":"synthetic-%d","refresh_token":"refresh-%d","account_id":%q,"expires_at":%q}`, i, i, account, expires.Format(time.RFC3339Nano))
		sealed, err := secure.Seal(master, 1, "v1", "credentials", credential, account, []byte(bundle))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: credential, AccountID: account, FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext, ExpiresAt: &expires}); err != nil {
			t.Fatal(err)
		}
		connectors = append(connectors, protectedConnector{ID: connector, Kind: "connector", Implementation: "pestiroute.codex.responses", Protocols: []string{responsesProtocol}, Settings: nativeSettings{Profile: "codex-responses-http-sse-v1", Model: model, AccountID: account}})
		accounts = append(accounts, routeTarget{Connector: connector, Account: account})
	}
	policies := sqlite.NewKeyPolicies(db)
	policy, err := policies.GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	allowed := make([]string, targets)
	for i := range allowed {
		allowed[i] = fmt.Sprintf("codex-%d", i)
	}
	policy, err = policies.Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{model}, Connectors: allowed, RPM: 20, TPM: 10000})
	if err != nil {
		t.Fatal(err)
	}
	key, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0").protected
	p.Connectors = connectors
	p.Routes = []protectedRoute{{ID: "codex-fallback", Protocol: responsesProtocol, Mode: "native", Model: model, Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(100)}, Targets: accounts}}
	if targets > 1 {
		attempts := targets
		p.Routes[0].Retry = &retryConfig{MaxAttempts: &attempts, Deadline: "5s"}
	}
	prepared, err := prepareProtectedConfig(ctx, config{protected: p, DatabasePath: dbPath, MasterKeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	for i := range prepared.Components {
		for j := range connectors {
			if prepared.Components[i].ID == core.InstanceID(connectors[j].ID) {
				prepared.Components[i].CredentialEnv = fmt.Sprintf("codex-credential-%d", j)
			}
		}
	}
	var ready, draining atomic.Bool
	ready.Store(true)
	h, closeComponents, err := composeHandlerWithFactory(prepared, &ready, &draining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		if safeFirst && item.ID == core.InstanceID("codex-0") {
			return codexSafeRejectionConnector{Connector: codex.NewConnector()}
		}
		return codexCompositionConnector{Connector: codex.NewConnector(), doer: fixture.doer}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeComponents(context.Background()) })
	fixture.server = httptest.NewServer(h)
	t.Cleanup(fixture.server.Close)
	t.Cleanup(func() { _ = prepared.runtimeDB.Close() })
	fixture.key = key.Secret
	fixture.db = prepared.runtimeDB
	return fixture
}

func (f *codexFallbackFixture) assertOneAttempt(t *testing.T) {
	t.Helper()
	rows, err := sqlite.NewLedger(f.db).QueryRequests(context.Background(), sqlite.RequestFilter{Model: "codex-fallback-model", Limit: 2})
	if err != nil || len(rows) != 1 || len(rows[0].Attempts) != 1 {
		t.Fatalf("Codex request records=%+v err=%v, want one request/attempt", rows, err)
	}
}

func (f *codexFallbackFixture) post(ctx context.Context, body string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.server.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.key)
	req.Header.Set("Content-Type", "application/json")
	return f.server.Client().Do(req)
}

func TestCodexFallbackRejectionsDoNotCycleCandidates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   int32
		status int
	}{
		{"401", 1, http.StatusUnauthorized}, {"403", 2, http.StatusForbidden}, {"429", 3, http.StatusTooManyRequests}, {"5xx", 4, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCodexFallbackFixture(t, 2)
			f.mode.Store(tc.mode)
			resp, err := f.post(context.Background(), `{"model":"codex-fallback-model","stream":true,"store":false,"input":"hello"}`)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.status || f.sends.Load() != 1 {
				t.Fatalf("status=%d sends=%d, want %d/1", resp.StatusCode, f.sends.Load(), tc.status)
			}
			f.assertOneAttempt(t)
		})
	}
	t.Run("committed failed SSE", func(t *testing.T) {
		f := newCodexFallbackFixture(t, 2)
		f.mode.Store(5)
		resp, err := f.post(context.Background(), `{"model":"codex-fallback-model","stream":true,"store":false,"input":"hello"}`)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || f.sends.Load() != 1 {
			t.Fatalf("status=%d sends=%d", resp.StatusCode, f.sends.Load())
		}
		f.assertOneAttempt(t)
	})
	t.Run("ambiguous transport failure", func(t *testing.T) {
		f := newCodexFallbackFixture(t, 2)
		f.mode.Store(7)
		resp, err := f.post(context.Background(), `{"model":"codex-fallback-model","stream":true,"store":false,"input":"hello"}`)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || f.sends.Load() != 1 {
			t.Fatalf("status=%d sends=%d", resp.StatusCode, f.sends.Load())
		}
		f.assertOneAttempt(t)
	})
	t.Run("client cancellation does not replay", func(t *testing.T) {
		f := newCodexFallbackFixture(t, 2)
		f.mode.Store(6)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			resp, err := f.post(ctx, `{"model":"codex-fallback-model","stream":true,"store":false,"input":"hello"}`)
			if resp != nil {
				_ = resp.Body.Close()
			}
			done <- err
		}()
		deadline := time.After(3 * time.Second)
		for f.sends.Load() == 0 {
			select {
			case <-deadline:
				t.Fatal("request did not dispatch")
			default:
				time.Sleep(time.Millisecond)
			}
		}
		cancel()
		close(f.release)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("cancelled request did not return")
		}
		time.Sleep(20 * time.Millisecond)
		if f.sends.Load() != 1 {
			t.Fatalf("upstream sends=%d, want one", f.sends.Load())
		}
		f.assertOneAttempt(t)
	})
	t.Run("local connector validation does not send", func(t *testing.T) {
		f := newCodexFallbackFixture(t, 2)
		resp, err := f.post(context.Background(), `{"model":"codex-fallback-model","stream":true,"store":true,"input":"hello"}`)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || f.sends.Load() != 0 {
			t.Fatalf("status=%d sends=%d", resp.StatusCode, f.sends.Load())
		}
	})
}

func TestCodexFallbackAdvancesOnlyAfterExplicitSafeNoSend(t *testing.T) {
	f := newCodexFallbackFixtureMode(t, 2, true)
	// The first configured target is a deterministic pre-send rejection; the second
	// uses the production Codex Connector and reaches the local SSE backend.
	resp, err := f.post(context.Background(), `{"model":"codex-fallback-model","stream":true,"store":false,"input":"hello"}`)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || f.sends.Load() != 1 {
		t.Fatalf("status=%d sends=%d, want successful second-target dispatch", resp.StatusCode, f.sends.Load())
	}
	rows, err := sqlite.NewLedger(f.db).QueryRequests(context.Background(), sqlite.RequestFilter{Model: "codex-fallback-model", Limit: 2})
	if err != nil || len(rows) != 1 || len(rows[0].Attempts) != 2 {
		t.Fatalf("Codex fallback records=%+v err=%v, want one request/two attempts", rows, err)
	}
	first, second := rows[0].Attempts[0], rows[0].Attempts[1]
	if first.Attempt.Ordinal != 1 || second.Attempt.Ordinal != 2 || first.Attempt.ID == second.Attempt.ID || first.Attempt.AccountID == second.Attempt.AccountID || first.Reservation.EstimatedTokens <= 0 || second.Reservation.EstimatedTokens <= 0 {
		t.Fatalf("fallback attempt sequence/reservations=%+v, want two distinct ordered account reservations", rows[0].Attempts)
	}
}

func TestCodexAffinityEncryptedReplayRequiresSingleton(t *testing.T) {
	const body = `{"model":"codex-fallback-model","stream":true,"store":false,"include":["reasoning.encrypted_content"],"input":[{"type":"reasoning","id":"rs_private","summary":[{"type":"summary_text","text":"prior"}],"encrypted_content":"opaque-ciphertext"},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`
	t.Run("multi-candidate rejected before send", func(t *testing.T) {
		f := newCodexFallbackFixture(t, 2)
		resp, err := f.post(context.Background(), body)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || f.sends.Load() != 0 || strings.Contains(string(data), "opaque-ciphertext") {
			t.Fatalf("status=%d sends=%d body=%s", resp.StatusCode, f.sends.Load(), data)
		}
		rows, err := sqlite.NewLedger(f.db).QueryRequests(context.Background(), sqlite.RequestFilter{Model: "codex-fallback-model", Limit: 2})
		if err != nil || len(rows) != 0 {
			t.Fatalf("multi-candidate affinity rejection admitted request: rows=%d err=%v", len(rows), err)
		}
	})
	t.Run("singleton executes once without cycling", func(t *testing.T) {
		f := newCodexFallbackFixture(t, 1)
		resp, err := f.post(context.Background(), body)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || f.sends.Load() != 1 {
			t.Fatalf("status=%d sends=%d", resp.StatusCode, f.sends.Load())
		}
		f.assertOneAttempt(t)
	})
}
