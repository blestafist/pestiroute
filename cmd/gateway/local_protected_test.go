package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func localProtectedYAML(dbPath, keyPath, url, profile, auth, extra string) string {
	return fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", max_request_bytes: 1048576, shutdown_timeout: 1s}
storage: {driver: sqlite, path: %q}
secrets: {master_key_file: %q}
connectors:
  - id: local
    kind: connector
    implementation: pestiroute.responses.local
    protocols: [openai.responses.v1]
    settings:
      base_url: %q
      runtime_profile: %q
      auth_mode: %q
      max_request_body_bytes: 1048576
      max_request_header_bytes: 8192
      connect_timeout: 1s
      tls_handshake_timeout: 1s
      response_header_timeout: 1s
      stream_idle_timeout: 1s
      model: local-model
      account_id: local-account
%s
routes:
  - id: local-route
    protocol: openai.responses.v1
    mode: native
    model: local-model
    adapter: pestiroute.responses.native
    policy: standard
    budget: {unknown_estimate: reserve, conservative_tokens: 20}
    targets: [{connector: local, account: local-account}]
policies: {standard: policy-local}
`, dbPath, keyPath, url, profile, auth, extra)
}

func TestLocalProtectedSettingsRejectInvalidAndMixedProfiles(t *testing.T) {
	for name, input := range map[string]string{
		"unsupported auth":                    localProtectedYAML("db", "key", "http://127.0.0.1:11434/v1/responses", "ollama-v0.40.3", "api_key", ""),
		"unsupported profile":                 localProtectedYAML("db", "key", "http://127.0.0.1:11434/v1/responses", "ollama-latest", "none", ""),
		"non-loopback http":                   localProtectedYAML("db", "key", "http://example.test/v1/responses", "ollama-v0.40.3", "none", ""),
		"non-loopback https":                  localProtectedYAML("db", "key", "https://runtime.example/v1/responses", "ollama-v0.40.3", "none", ""),
		"mixed native fields":                 localProtectedYAML("db", "key", "http://127.0.0.1:11434/v1/responses", "ollama-v0.40.3", "none", "      credential_env: SOME_SECRET\n"),
		"unsupported capability":              localProtectedYAML("db", "key", "http://127.0.0.1:11434/v1/responses", "ollama-v0.40.3", "none", "      capabilities: {llm.tools: supported}\n"),
		"unevidenced parallel tools":          localProtectedYAML("db", "key", "http://127.0.0.1:11434/v1/responses", "ollama-v0.40.3", "none", "      capabilities: {llm.tools.parallel: supported}\n"),
		"unevidenced reasoning":               localProtectedYAML("db", "key", "http://127.0.0.1:11434/v1/responses", "ollama-v0.40.3", "none", "      capabilities: {llm.reasoning: supported}\n"),
		"unevidenced structured output":       localProtectedYAML("db", "key", "http://127.0.0.1:11434/v1/responses", "ollama-v0.40.3", "none", "      capabilities: {llm.structured_output: supported}\n"),
		"unevidenced vision":                  localProtectedYAML("db", "key", "http://127.0.0.1:11434/v1/responses", "ollama-v0.40.3", "none", "      capabilities: {llm.vision: supported}\n"),
		"unevidenced audio":                   localProtectedYAML("db", "key", "http://127.0.0.1:11434/v1/responses", "ollama-v0.40.3", "none", "      capabilities: {llm.audio: supported}\n"),
		"account mismatch":                    strings.Replace(localProtectedYAML("db", "key", "http://127.0.0.1:11434/v1/responses", "ollama-v0.40.3", "none", ""), "account_id: local-account", "account_id: other-account", 1),
		"model mismatch":                      strings.Replace(localProtectedYAML("db", "key", "http://127.0.0.1:11434/v1/responses", "ollama-v0.40.3", "none", ""), "model: local-model", "model: other-model", 1),
		"target mismatch":                     strings.Replace(localProtectedYAML("db", "key", "http://127.0.0.1:11434/v1/responses", "ollama-v0.40.3", "none", ""), "connector: local, account: local-account", "connector: local, account: other-account", 1),
		"missing bearer credential reference": localProtectedYAML("db", "key", "https://localhost/v1/responses", "vllm-v0.31.0", "bearer", ""),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadProtectedYAML(path); err == nil {
				t.Fatal("invalid local configuration accepted")
			}
		})
	}
	valid := localProtectedYAML("db", "key", "http://127.0.0.1:11434/v1/responses", "ollama-v0.40.3", "none", "      capabilities: {llm.streaming: supported}\n")
	path := filepath.Join(t.TempDir(), "valid.yaml")
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProtectedYAML(path); err != nil {
		t.Fatalf("valid local protected config: %v", err)
	}
	localhostTLS := localProtectedYAML("db", "key", "https://localhost:11434/v1/responses", "vllm-v0.31.0", "none", "")
	if err := os.WriteFile(path, []byte(localhostTLS), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProtectedYAML(path); err != nil {
		t.Fatalf("loopback HTTPS local protected config: %v", err)
	}
}

func TestProtectedLocalRouteCompositionKeepsVirtualKeyAndNativeRoute(t *testing.T) {
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "native-synthetic")
	_, dbPath, keyPath := protectedFixture(t)
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	accounts, policies := sqlite.NewAccounts(db), sqlite.NewKeyPolicies(db)
	for _, account := range []sqlite.Account{{ID: "local-account", Connector: "local", Enabled: true}, {ID: "anthropic-account", Connector: "anthropic", Enabled: true}, {ID: "codex-account", Connector: "codex", Enabled: true}} {
		if _, err := accounts.Create(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := policies.GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	policy, err = policies.Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{"model-a", "local-model", "anthropic-model", "codex-model"}, Connectors: []string{"upstream", "local", "anthropic", "codex"}, RPM: policy.RPM, TPM: policy.TPM})
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
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("none auth forwarded Authorization: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed"}`))
	}))
	defer upstream.Close()
	c := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
	local := protectedConnector{ID: "local", Kind: "connector", Implementation: "pestiroute.responses.local", Protocols: []string{responsesProtocol}, Settings: nativeSettings{
		BaseURL: upstream.URL + "/v1/responses", RuntimeProfile: "llamacpp-v0.6.0", AuthMode: "none", MaxRequestBodyBytes: 1 << 20, MaxRequestHeaderBytes: 8192,
		ConnectTimeout: "1s", TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s", StreamIdleTimeout: "1s", Model: "local-model", AccountID: "local-account",
	}}
	c.protected.Connectors = append(c.protected.Connectors, local)
	c.protected.Connectors = append(c.protected.Connectors,
		protectedConnector{ID: "anthropic", Kind: "connector", Implementation: "pestiroute.anthropic.messages", Settings: nativeSettings{Model: "anthropic-model", AccountID: "anthropic-account", CredentialID: "anthropic-key"}},
		protectedConnector{ID: "codex", Kind: "connector", Implementation: "pestiroute.codex.responses", Settings: nativeSettings{Model: "codex-model", AccountID: "codex-account", Profile: "codex-responses-http-sse-v1"}},
	)
	c.protected.Routes = append(c.protected.Routes, protectedRoute{ID: "local-route", Protocol: responsesProtocol, Mode: "native", Model: "local-model", Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(20)}, Targets: []routeTarget{{Connector: "local", Account: "local-account"}}})
	c.protected.Routes = append(c.protected.Routes,
		protectedRoute{ID: "anthropic-route", Protocol: responsesProtocol, Mode: "translation", Model: "anthropic-model", Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(4096)}, Targets: []routeTarget{{Connector: "anthropic", Account: "anthropic-account"}}},
		protectedRoute{ID: "codex-route", Protocol: responsesProtocol, Mode: "native", Model: "codex-model", Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(20)}, Targets: []routeTarget{{Connector: "codex", Account: "codex-account"}}},
	)
	c.protected.Policies["standard"] = "policy-id-a"
	prepared, err := prepareProtectedConfig(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	var ready, draining atomic.Bool
	h, closeComponents, err := composeHandler(prepared, &ready, &draining, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeComponents(context.Background()) }()
	unauthenticated := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"local-model","input":"synthetic"}`))
	unauthenticated.Header.Set("Content-Type", "application/json")
	unauthenticatedResponse := httptest.NewRecorder()
	h.ServeHTTP(unauthenticatedResponse, unauthenticated)
	if unauthenticatedResponse.Code != http.StatusUnauthorized || calls.Load() != 0 {
		t.Fatalf("unauthenticated local request status=%d calls=%d", unauthenticatedResponse.Code, calls.Load())
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"local-model","input":"synthetic"}`))
	r.Header.Set("Authorization", "Bearer "+issued.Secret)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("local response status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
	}
	attempts, err := sqlite.NewLedger(prepared.runtimeDB).QueryAttempts(ctx, sqlite.AttemptFilter{Connector: "local"})
	if err != nil || len(attempts) != 1 || attempts[0].Reservation.EstimatedTokens != 20 {
		t.Fatalf("local unknown-usage reservation = %+v, %v", attempts, err)
	}
	usage, err := sqlite.NewLedger(prepared.runtimeDB).GetUsage(ctx, attempts[0].Attempt.ID)
	if err != nil || usage.Source != "unknown" || usage.Completeness != "unknown" || usage.InputTokens != nil || usage.OutputTokens != nil || attempts[0].Reservation.State != "conservative" || attempts[0].Reservation.EffectiveCharge < 20 {
		t.Fatalf("missing local usage was inferred free/zero: usage=%+v reservation=%+v err=%v", usage, attempts[0].Reservation, err)
	}
	if c.protected.Connectors[0].Implementation != "pestiroute.responses.native" {
		t.Fatal("native connector configuration changed")
	}
}

func TestLocalProtectedAccountingPolicyAndCredentialIsolation(t *testing.T) {
	ctx := context.Background()
	_, dbPath, keyPath := protectedFixture(t)
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	accounts, policies := sqlite.NewAccounts(db), sqlite.NewKeyPolicies(db)
	if _, err := accounts.Create(ctx, sqlite.Account{ID: "local-account-a", Connector: "local-a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.Create(ctx, sqlite.Account{ID: "local-account-alt", Connector: "local-alt", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.Create(ctx, sqlite.Account{ID: "bearer-account", Connector: "local-b", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	envelope, err := secure.Seal(key, 2, "test-v1", "credentials", "bearer-credential", "bearer-account", []byte("synthetic-bearer"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "bearer-credential", AccountID: "bearer-account", FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	policy, err := policies.GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	policy, err = policies.Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{"model-a", "model-b"}, Connectors: []string{"local-a", "local-alt", "local-b"}, RPM: policy.RPM, TPM: policy.TPM})
	if err != nil {
		t.Fatal(err)
	}
	issued, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	persistKey, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewVirtualKeys(db).SetEnabled(ctx, disabled.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var callsA, callsB atomic.Int32
	response := `{"status":"completed","usage":{"input_tokens":3,"output_tokens":2}}`
	cancelStarted, cancelRelease := make(chan struct{}), make(chan struct{})
	var cancelOnce sync.Once
	persistStarted, persistRelease := make(chan struct{}), make(chan struct{})
	backend := func(target string, calls *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			want := ""
			if target == "b" {
				want = "Bearer synthetic-bearer"
			}
			if got := r.Header.Get("Authorization"); got != want {
				t.Errorf("target %s Authorization=%q, want %q", target, got, want)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read target %s request body: %v", target, err)
			}
			w.Header().Set("Content-Type", "application/json")
			if target == "a" && bytes.Contains(body, []byte("terminal-failure")) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if target == "a" && bytes.Contains(body, []byte("incomplete")) {
				_, _ = io.WriteString(w, `{"status":"incomplete","usage":{"input_tokens":7,"output_tokens":0}}`)
				return
			}
			if target == "a" && bytes.Contains(body, []byte("interrupt")) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n")
				return
			}
			if target == "a" && bytes.Contains(body, []byte("cancel")) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n")
				w.(http.Flusher).Flush()
				cancelOnce.Do(func() { close(cancelStarted) })
				<-cancelRelease
				return
			}
			if target == "a" && bytes.Contains(body, []byte("persist-failure")) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n")
				w.(http.Flusher).Flush()
				close(persistStarted)
				<-persistRelease
				_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n")
				return
			}
			_, _ = io.WriteString(w, response)
		}))
	}
	var alternateCalls atomic.Int32
	a, b, alternate := backend("a", &callsA), backend("b", &callsB), backend("alt", &alternateCalls)
	defer a.Close()
	defer b.Close()
	defer alternate.Close()
	c := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
	connector := func(id, account, model, endpoint, profile, auth, credential string) protectedConnector {
		return protectedConnector{ID: id, Kind: "connector", Implementation: "pestiroute.responses.local", Protocols: []string{responsesProtocol}, Settings: nativeSettings{
			BaseURL: endpoint + "/v1/responses", RuntimeProfile: profile, AuthMode: auth, CredentialID: credential, MaxRequestBodyBytes: 1 << 20, MaxRequestHeaderBytes: 8192,
			ConnectTimeout: "1s", TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s", StreamIdleTimeout: "1s", Model: model, AccountID: account,
		}}
	}
	c.protected.Connectors[0] = connector("local-a", "local-account-a", "model-a", a.URL, "ollama-v0.40.3", "none", "")
	c.protected.Connectors[0].Settings.Capabilities = map[string]string{"llm.streaming": "supported"}
	c.protected.Connectors = append(c.protected.Connectors, connector("local-b", "bearer-account", "model-b", b.URL, "vllm-v0.31.0", "bearer", "bearer-credential"))
	c.protected.Connectors = append(c.protected.Connectors, connector("local-alt", "local-account-alt", "model-a", alternate.URL, "ollama-v0.40.3", "none", ""))
	c.protected.Connectors[len(c.protected.Connectors)-1].Settings.Capabilities = map[string]string{"llm.streaming": "supported"}
	c.protected.Routes[0].ID, c.protected.Routes[0].Model, c.protected.Routes[0].Targets = "route-a", "model-a", []routeTarget{{Connector: "local-a", Account: "local-account-a"}, {Connector: "local-alt", Account: "local-account-alt"}}
	retryAttempts := 2
	c.protected.Routes[0].Retry = &retryConfig{MaxAttempts: &retryAttempts, Deadline: "5s"}
	c.protected.Routes = append(c.protected.Routes, protectedRoute{ID: "route-b", Protocol: responsesProtocol, Mode: "native", Model: "model-b", Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(20)}, Targets: []routeTarget{{Connector: "local-b", Account: "bearer-account"}}})
	c.protected.Policies["standard"] = "policy-id-a"
	if err := validateBudget("local route", routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(0)}); err == nil {
		t.Fatal("zero conservative reservation budget accepted")
	}
	prepared, err := prepareProtectedConfig(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	var retryRouteFound bool
	for _, route := range prepared.Routes {
		if route.RouteID == "route-a" {
			retryRouteFound = true
			if route.RetryMaxAttempts != 2 || route.RetryDeadline != 5*time.Second {
				t.Fatalf("eligible alternate route retry config: max=%d deadline=%s", route.RetryMaxAttempts, route.RetryDeadline)
			}
		}
	}
	if !retryRouteFound {
		t.Fatal("configured two-target retry route missing from prepared runtime")
	}
	accountingCalls := &accountingCallCounters{}
	prepared.accounting = &countedSQLiteAccountingStore{store: sqliteAccountingStore{ledger: sqlite.NewLedger(prepared.runtimeDB)}, calls: accountingCalls}
	var ready, draining atomic.Bool
	ready.Store(true)
	h, closeComponents, err := composeHandler(prepared, &ready, &draining, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeComponents(context.Background()) })
	gateway := httptest.NewServer(h)
	defer gateway.Close()
	makeRequest := func(requestCtx context.Context, model, secret, input string, stream bool) *http.Request {
		streamValue := "false"
		if stream {
			streamValue = "true"
		}
		req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"input":%q,"stream":%s}`, model, input, streamValue)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		return req
	}
	postInput := func(model, secret, input string, stream bool) int {
		req := makeRequest(ctx, model, secret, input, stream)
		resp, err := gateway.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		return resp.StatusCode
	}
	post := func(model, secret string) int { return postInput(model, secret, "synthetic", false) }
	if got := post("model-a", issued.Secret); got != http.StatusOK {
		t.Fatalf("unauthenticated target status=%d", got)
	}
	if got := post("model-b", issued.Secret); got != http.StatusOK {
		t.Fatalf("bearer target status=%d", got)
	}
	if got := postInput("model-a", issued.Secret, "terminal-failure", false); got < 400 {
		t.Fatalf("terminal error status=%d", got)
	}
	if got := postInput("model-a", issued.Secret, "incomplete", false); got != http.StatusOK {
		t.Fatalf("incomplete response status=%d", got)
	}
	if got := postInput("model-a", issued.Secret, "interrupt", true); got != http.StatusOK {
		t.Fatalf("interrupted stream status=%d", got)
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancelResp, err := gateway.Client().Do(makeRequest(cancelCtx, "model-a", issued.Secret, "cancel", true))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	select {
	case <-cancelStarted:
	case <-time.After(3 * time.Second):
		cancel()
		_ = cancelResp.Body.Close()
		t.Fatal("cancel stream did not reach fake local target")
	}
	if _, err := bufio.NewReader(cancelResp.Body).ReadString('\n'); err != nil {
		cancel()
		_ = cancelResp.Body.Close()
		t.Fatalf("read first cancel stream event: %v", err)
	}
	cancel()
	_ = cancelResp.Body.Close()
	close(cancelRelease)
	deadline := time.Now().Add(3 * time.Second)
	for accountingCalls.finalize.Load() != 6 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := accountingCalls.snapshot().finalize; got != 6 {
		t.Fatalf("six local executions finalized %d times, want exactly once each", got)
	}
	rows, err := sqlite.NewLedger(prepared.runtimeDB).QueryRequests(ctx, sqlite.RequestFilter{Limit: 10})
	if err != nil || len(rows) != 6 {
		t.Fatalf("local lifecycle SQLite requests=%d err=%v", len(rows), err)
	}
	var known, failedUnknown, incompleteKnown, incompleteUnknown int
	for _, row := range rows {
		if len(row.Attempts) != 1 || row.Attempts[0].Usage == nil {
			t.Fatalf("local attempt missing usage or duplicate attempt: %+v", row)
		}
		attempt, usage := row.Attempts[0].Attempt, row.Attempts[0].Usage
		switch {
		case attempt.State == "succeeded" && usage.Source == "provider" && usage.Completeness == "complete" && usage.InputTokens != nil && *usage.InputTokens == 3 && usage.OutputTokens != nil && *usage.OutputTokens == 2:
			known++
		case attempt.State == "failed" && usage.Source == "unknown" && usage.Completeness == "unknown" && usage.InputTokens == nil && usage.OutputTokens == nil:
			failedUnknown++
		case attempt.State == "interrupted" && usage.Source == "provider" && usage.Completeness == "complete" && usage.InputTokens != nil && *usage.InputTokens == 7 && usage.OutputTokens != nil && *usage.OutputTokens == 0:
			incompleteKnown++
		case (attempt.State == "cancelled" || attempt.State == "interrupted") && usage.Source == "unknown" && usage.Completeness == "unknown" && usage.InputTokens == nil && usage.OutputTokens == nil:
			incompleteUnknown++
		default:
			t.Fatalf("unexpected local lifecycle attempt=%+v usage=%+v", attempt, usage)
		}
	}
	if known != 2 || failedUnknown != 1 || incompleteKnown != 1 || incompleteUnknown != 2 {
		t.Fatalf("SQLite lifecycle counts known=%d failed-unknown=%d incomplete-known=%d unknown-interrupted/cancelled=%d", known, failedUnknown, incompleteKnown, incompleteUnknown)
	}
	partialStore := sqliteAccountingStore{ledger: sqlite.NewLedger(prepared.runtimeDB)}
	partial := core.AccountingAdmission{RequestID: "local-partial-fixture", AttemptID: "local-partial-attempt", KeyID: issued.ID, KeyRevision: issued.Revision, PolicyID: policy.ID, PolicyRevision: issued.PolicyRevision, Protocol: responsesProtocol, Model: "model-a", RouteID: "route-a", AccountID: "local-account-a", Connector: "local-a", EstimateTokens: 20, EstimateMethod: "fixture", BudgetPolicy: "reserve"}
	if err := partialStore.Admit(ctx, partial); err != nil {
		t.Fatal(err)
	}
	if err := partialStore.RecordDispatchIntent(ctx, partial.AttemptID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	partialInput := int64(4)
	if err := partialStore.FinalizeAttempt(ctx, core.AccountingTerminal{AttemptID: partial.AttemptID, Outcome: core.OutcomeIncomplete, Committed: true, Usage: core.UsageReport{InputTokens: &partialInput, Source: core.UsageProvider, Completeness: core.UsagePartial}, EndedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := partialStore.FinishRequest(ctx, partial.RequestID); err != nil {
		t.Fatal(err)
	}
	partialUsage, err := partialStore.ledger.GetUsage(ctx, partial.AttemptID)
	if err != nil || partialUsage.Source != "provider" || partialUsage.Completeness != "partial" || partialUsage.InputTokens == nil || *partialUsage.InputTokens != partialInput || partialUsage.OutputTokens != nil {
		t.Fatalf("partial usage was not durably preserved: %+v err=%v", partialUsage, err)
	}
	wantCallsA, wantCallsB := callsA.Load(), callsB.Load()
	if got := post("not-permitted", issued.Secret); got < 400 || callsA.Load() != wantCallsA || callsB.Load() != wantCallsB {
		t.Fatalf("out-of-policy status=%d upstream calls=%d/%d", got, callsA.Load(), callsB.Load())
	}
	rows, err = sqlite.NewLedger(prepared.runtimeDB).QueryRequests(ctx, sqlite.RequestFilter{Limit: 10})
	if err != nil || len(rows) != 7 {
		t.Fatalf("ledger requests including partial SQLite fixture=%d err=%v", len(rows), err)
	}
	for _, row := range rows {
		if len(row.Attempts) != 1 || row.Attempts[0].Attempt.State == "succeeded" && (row.Attempts[0].Usage == nil || row.Attempts[0].Usage.InputTokens == nil || *row.Attempts[0].Usage.InputTokens != 3 || row.Attempts[0].Usage.OutputTokens == nil || *row.Attempts[0].Usage.OutputTokens != 2 || row.Attempts[0].Reservation.State != "settled") {
			t.Fatalf("known usage did not settle once in SQLite: %+v", row)
		}
	}
	if _, err := sqlite.NewVirtualKeys(prepared.runtimeDB).Revoke(ctx, issued.ID); err != nil {
		t.Fatal(err)
	}
	if got := post("model-a", issued.Secret); got < 400 || callsA.Load() != wantCallsA {
		t.Fatalf("revoked key status=%d upstream calls=%d", got, callsA.Load())
	}
	if got := post("model-a", disabled.Secret); got < 400 || callsA.Load() != wantCallsA {
		t.Fatalf("disabled key status=%d upstream calls=%d", got, callsA.Load())
	}

	persistCtx, persistCancel := context.WithCancel(ctx)
	persistRespCh := make(chan *http.Response, 1)
	persistErrCh := make(chan error, 1)
	go func() {
		resp, err := gateway.Client().Do(makeRequest(persistCtx, "model-a", persistKey.Secret, "persist-failure", true))
		if err != nil {
			persistErrCh <- err
			return
		}
		persistRespCh <- resp
	}()
	select {
	case <-persistStarted:
	case <-time.After(3 * time.Second):
		persistCancel()
		t.Fatal("post-commit persistence request did not reach primary local target")
	}
	var persistResp *http.Response
	select {
	case persistResp = <-persistRespCh:
	case err := <-persistErrCh:
		persistCancel()
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		persistCancel()
		t.Fatal("post-commit local response was not delivered")
	}
	if _, err := bufio.NewReader(persistResp.Body).ReadString('\n'); err != nil {
		persistCancel()
		_ = persistResp.Body.Close()
		t.Fatalf("read committed local stream: %v", err)
	}
	if err := prepared.runtimeDB.Close(); err != nil {
		t.Fatal(err)
	}
	close(persistRelease)
	_, _ = io.Copy(io.Discard, persistResp.Body)
	_ = persistResp.Body.Close()
	persistCancel()
	if ready.Load() {
		t.Fatal("gateway remained ready after local SQLite settlement failure")
	}
	readyResp, err := gateway.Client().Get(gateway.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_ = readyResp.Body.Close()
	if readyResp.StatusCode != http.StatusServiceUnavailable || alternateCalls.Load() != 0 {
		t.Fatalf("storage failure readyz=%d alternate dispatches=%d", readyResp.StatusCode, alternateCalls.Load())
	}
}

func TestLocalProtectedConnectorPolicyDeniesConfiguredTarget(t *testing.T) {
	ctx := context.Background()
	_, dbPath, keyPath := protectedFixture(t)
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: "local-account-a", Connector: "local-a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	policy, err := sqlite.NewKeyPolicies(db).GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	policy, err = sqlite.NewKeyPolicies(db).Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{"model-a"}, Connectors: []string{"local-a"}, RPM: policy.RPM, TPM: policy.TPM})
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
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"completed"}`)
	}))
	defer backend.Close()
	c := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
	c.protected.Connectors[0] = protectedConnector{ID: "local-a", Kind: "connector", Implementation: "pestiroute.responses.local", Protocols: []string{responsesProtocol}, Settings: nativeSettings{
		BaseURL: backend.URL + "/v1/responses", RuntimeProfile: "ollama-v0.40.3", AuthMode: "none", MaxRequestBodyBytes: 1 << 20, MaxRequestHeaderBytes: 8192,
		ConnectTimeout: "1s", TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s", StreamIdleTimeout: "1s", Model: "model-a", AccountID: "local-account-a",
	}}
	c.protected.Routes[0].Targets = []routeTarget{{Connector: "local-a", Account: "local-account-a"}}
	prepared, err := prepareProtectedConfig(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	policies := sqlite.NewKeyPolicies(prepared.runtimeDB)
	latest, err := policies.GetLatest(ctx, policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	latest, err = policies.Update(ctx, latest.ID, latest.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: latest.Models, Connectors: []string{"unrelated-connector"}, RPM: latest.RPM, TPM: latest.TPM})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewVirtualKeys(prepared.runtimeDB).UpdatePolicy(ctx, issued.ID, latest.ID, latest.Revision, issued.Revision); err != nil {
		t.Fatal(err)
	}
	var ready, draining atomic.Bool
	ready.Store(true)
	h, closeComponents, err := composeHandler(prepared, &ready, &draining, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeComponents(context.Background()) })
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model-a","input":"synthetic"}`))
	req.Header.Set("Authorization", "Bearer "+issued.Secret)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"code":"permission_denied"`) || calls.Load() != 0 {
		t.Fatalf("connector policy denial status=%d body=%s upstream calls=%d", w.Code, w.Body.String(), calls.Load())
	}
}

func TestProtectedLocalTextJSONAndIncrementalSSE(t *testing.T) {
	for _, tc := range []struct{ profile, fixture string }{
		{"ollama-v0.40.3", "ollama-v0.40.3"},
		{"vllm-v0.31.0", "vllm-v0.31.0"},
		{"llamacpp-v0.6.0", "llama.cpp-v0.6.0"},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			ctx := context.Background()
			_, dbPath, keyPath := protectedFixture(t)
			db, err := sqlite.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			issued, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: "policy-id-a", PolicyRevision: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			fixtureDir := "../../internal/connector/responses/testdata/local-runtime/" + tc.fixture + "/"
			response, err := os.ReadFile(fixtureDir + "response.json")
			if err != nil {
				t.Fatal(err)
			}
			stream, err := os.ReadFile(fixtureDir + "stream.sse")
			if err != nil {
				t.Fatal(err)
			}
			prefixEnd := bytes.Index(stream, []byte("\n\n")) + 2
			if prefixEnd <= 1 {
				t.Fatal("fixture has no complete initial SSE event")
			}
			gate, started := make(chan struct{}), make(chan struct{})
			var mode atomic.Int32
			var startedOnce sync.Once
			var captured atomic.Value
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, readErr := io.ReadAll(r.Body)
				if readErr != nil {
					t.Errorf("read request: %v", readErr)
				}
				captured.Store(body)
				if r.Header.Get("Authorization") != "" {
					t.Errorf("no-auth connector forwarded Authorization")
				}
				if mode.Load() == 3 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":{"message":"synthetic busy"}}`)
					return
				}
				if mode.Load() == 0 {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(response)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(stream[:prefixEnd])
				w.(http.Flusher).Flush()
				startedOnce.Do(func() { close(started) })
				if mode.Load() == 2 {
					return
				}
				<-gate
				_, _ = w.Write(stream[prefixEnd:])
			}))
			defer backend.Close()

			c := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
			c.protected.Connectors[0] = protectedConnector{ID: "upstream", Kind: "connector", Implementation: "pestiroute.responses.local", Protocols: []string{responsesProtocol}, Settings: nativeSettings{
				BaseURL: backend.URL + "/v1/responses", RuntimeProfile: tc.profile, AuthMode: "none", MaxRequestBodyBytes: 1 << 20, MaxRequestHeaderBytes: 8192,
				ConnectTimeout: "1s", TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s", StreamIdleTimeout: "5s", Model: "model-a", AccountID: "account-a", Capabilities: map[string]string{"llm.streaming": "supported"},
			}}
			prepared, err := prepareProtectedConfig(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			var ready, draining atomic.Bool
			ready.Store(true)
			h, closeComponents, err := composeHandler(prepared, &ready, &draining, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = closeComponents(context.Background()) })
			gateway := httptest.NewServer(h)
			defer gateway.Close()
			requestBody := func(streaming bool) []byte {
				flag := "false"
				if streaming {
					flag = "true"
				}
				return []byte(fmt.Sprintf("{ \"model\" : \"model-a\", \"stream\" : %s, \"input\" : \"synthetic\", \"x_extension\" : { \"keep\" : true } }", flag))
			}
			request := func(streaming bool) *http.Request {
				body := requestBody(streaming)
				req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+issued.Secret)
				req.Header.Set("Content-Type", "application/json")
				return req
			}
			mode.Store(0)
			resp, err := gateway.Client().Do(request(false))
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(got, response) {
				t.Fatalf("JSON status=%d err=%v body=%q", resp.StatusCode, err, got)
			}
			if body, ok := captured.Load().([]byte); !ok || !bytes.Equal(body, requestBody(false)) {
				t.Fatalf("request body changed: %q", body)
			}
			mode.Store(3)
			rejected, err := gateway.Client().Do(request(false))
			if err != nil {
				t.Fatal(err)
			}
			rejectedBody, err := io.ReadAll(rejected.Body)
			_ = rejected.Body.Close()
			if err != nil || rejected.StatusCode != http.StatusTooManyRequests || !bytes.Equal(rejectedBody, []byte(`{"error":{"message":"synthetic busy"}}`)) {
				t.Fatalf("pre-commit rejection status=%d err=%v body=%q", rejected.StatusCode, err, rejectedBody)
			}

			mode.Store(2)
			truncated, err := gateway.Client().Do(request(true))
			if err != nil {
				t.Fatal(err)
			}
			truncatedBody, err := io.ReadAll(truncated.Body)
			_ = truncated.Body.Close()
			if err != nil || truncated.StatusCode != http.StatusOK || !bytes.HasPrefix(truncatedBody, stream[:prefixEnd]) || bytes.Contains(truncatedBody, []byte("response.completed")) {
				t.Fatalf("truncated SSE status=%d err=%v body=%q", truncated.StatusCode, err, truncatedBody)
			}
			rows, err := sqlite.NewLedger(prepared.runtimeDB).QueryRequests(ctx, sqlite.RequestFilter{Model: "model-a", Limit: 10})
			if err != nil || len(rows) != 3 || len(rows[2].Attempts) != 1 || rows[2].Attempts[0].Attempt.State != "interrupted" || rows[2].Attempts[0].Reservation.State == "settled" {
				t.Fatalf("truncated SSE settlement rows=%+v err=%v", rows, err)
			}

			mode.Store(1)
			respCh := make(chan *http.Response, 1)
			errCh := make(chan error, 1)
			go func() {
				r, err := gateway.Client().Do(request(true))
				if err != nil {
					errCh <- err
				} else {
					respCh <- r
				}
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream did not send first SSE event")
			}
			if body, ok := captured.Load().([]byte); !ok || !bytes.Equal(body, requestBody(true)) {
				t.Fatalf("streaming request body changed: %q", body)
			}
			var sseResp *http.Response
			select {
			case sseResp = <-respCh:
			case err := <-errCh:
				t.Fatal(err)
			case <-time.After(5 * time.Second):
				t.Fatal("gateway buffered before committing SSE")
			}
			reader := bufio.NewReader(sseResp.Body)
			first := make([]byte, prefixEnd)
			if _, err := io.ReadFull(reader, first); err != nil || !bytes.Equal(first, stream[:prefixEnd]) {
				t.Fatalf("early SSE bytes=%q err=%v", first, err)
			}
			close(gate)
			rest, err := io.ReadAll(reader)
			_ = sseResp.Body.Close()
			if err != nil || !bytes.Equal(append(first, rest...), stream) {
				t.Fatalf("SSE bytes differ err=%v", err)
			}
		})
	}
}

func TestProtectedLocalFunctionToolRoundTripIsClientOwned(t *testing.T) {
	for _, profile := range []string{
		"ollama-v0.40.3", "vllm-v0.31.0", "llamacpp-v0.6.0",
	} {
		t.Run(profile, func(t *testing.T) {
			ctx := context.Background()
			_, dbPath, keyPath := protectedFixture(t)
			db, err := sqlite.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			issued, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: "policy-id-a", PolicyRevision: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			callResponse := []byte(`{"id":"resp_call","status":"completed","output":[{"type":"function_call","id":"fc_item_42","call_id":"call_42","name":"lookup","arguments":"{  \"key\" : \"alpha\" }"}]}`)
			finalResponse := []byte(`{"id":"resp_done","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done"}]}]}`)
			var calls atomic.Int32
			captured := make(chan []byte, 4)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, readErr := io.ReadAll(r.Body)
				if readErr != nil {
					t.Errorf("read request: %v", readErr)
				}
				captured <- body
				w.Header().Set("Content-Type", "application/json")
				if calls.Add(1) == 1 {
					_, _ = w.Write(callResponse)
					return
				}
				_, _ = w.Write(finalResponse)
			}))
			defer backend.Close()

			c := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
			c.protected.Connectors[0] = protectedConnector{ID: "upstream", Kind: "connector", Implementation: "pestiroute.responses.local", Protocols: []string{responsesProtocol}, Settings: nativeSettings{
				BaseURL: backend.URL + "/v1/responses", RuntimeProfile: profile, AuthMode: "none", MaxRequestBodyBytes: 1 << 20, MaxRequestHeaderBytes: 8192,
				ConnectTimeout: "1s", TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s", StreamIdleTimeout: "5s", Model: "model-a", AccountID: "account-a",
			}}
			prepared, err := prepareProtectedConfig(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			var ready, draining atomic.Bool
			ready.Store(true)
			h, closeComponents, err := composeHandler(prepared, &ready, &draining, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = closeComponents(context.Background()) })
			gateway := httptest.NewServer(h)
			defer gateway.Close()
			post := func(body []byte) ([]byte, int) {
				req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+issued.Secret)
				req.Header.Set("Content-Type", "application/json")
				resp, err := gateway.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				return got, resp.StatusCode
			}
			first := []byte(`{"model":"model-a","input":"Find alpha"}`)
			got, status := post(first)
			if status != http.StatusOK || !bytes.Equal(got, callResponse) {
				t.Fatalf("call response status=%d body=%s", status, got)
			}
			if upstream := <-captured; !bytes.Equal(upstream, first) {
				t.Fatalf("first request changed: %s", upstream)
			}

			// The test client supplies the result in a separate follow-up request;
			// neither gateway nor backend receives an execution callback.
			second := []byte(`{"model":"model-a","input":[{"type":"function_call","id":"fc_item_42","call_id":"call_42","name":"lookup","arguments":"{  \"key\" : \"alpha\" }"},{"type":"function_call_output","call_id":"call_42","output":"alpha-result"}]}`)
			got, status = post(second)
			if status != http.StatusOK || !bytes.Equal(got, finalResponse) {
				t.Fatalf("follow-up response status=%d body=%s", status, got)
			}
			if upstream := <-captured; !bytes.Equal(upstream, second) {
				t.Fatalf("follow-up request changed: %s", upstream)
			}
			if calls.Load() != 2 {
				t.Fatalf("backend dispatches=%d", calls.Load())
			}

			choiceRequest := []byte(`{"model":"model-a","input":"Find alpha","tool_choice":"auto"}`)
			_, status = post(choiceRequest)
			if status != http.StatusBadRequest || calls.Load() != 2 {
				t.Fatalf("unproven tool_choice status=%d backend dispatches=%d", status, calls.Load())
			}
			toolRequest := []byte(`{"model":"model-a","input":"Find alpha","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
			_, status = post(toolRequest)
			if status != http.StatusBadRequest || calls.Load() != 2 {
				t.Fatalf("unevidenced function tool status=%d backend dispatches=%d", status, calls.Load())
			}
		})
	}
}

func TestProtectedLocalMultipleCallsAndParallelRequirementFailClosed(t *testing.T) {
	const trace = "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"item_a\",\"type\":\"function_call\",\"call_id\":\"call_a\",\"name\":\"lookup\",\"arguments\":\"\"}}\n\nevent: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"item_a\",\"delta\":\"{\\\"key\\\":\\\"\"}\n\nevent: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"id\":\"item_b\",\"type\":\"function_call\",\"call_id\":\"call_b\",\"name\":\"lookup\",\"arguments\":\"\"}}\n\nevent: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":1,\"item_id\":\"item_b\",\"delta\":\"{\\\"key\\\":\\\"beta\\\"}\"}\n\nevent: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"item_a\",\"delta\":\"alpha\\\"}\"}\n\nevent: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"output_index\":1,\"item_id\":\"item_b\",\"arguments\":\"{\\\"key\\\":\\\"beta\\\"}\"}\n\nevent: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"output_index\":0,\"item_id\":\"item_a\",\"arguments\":\"{\\\"key\\\":\\\"alpha\\\"}\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	for _, profile := range []string{"ollama-v0.40.3", "vllm-v0.31.0", "llamacpp-v0.6.0"} {
		t.Run(profile, func(t *testing.T) {
			ctx := context.Background()
			_, dbPath, keyPath := protectedFixture(t)
			db, err := sqlite.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			issued, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: "policy-id-a", PolicyRevision: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			var dispatches atomic.Int32
			captured := make(chan []byte, 2)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read upstream request: %v", err)
				}
				captured <- body
				if dispatches.Add(1) == 1 {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, trace)
					return
				}
				_, _ = io.WriteString(w, `{"id":"done","status":"completed"}`)
			}))
			defer backend.Close()

			config := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
			config.protected.Connectors[0] = protectedConnector{ID: "upstream", Kind: "connector", Implementation: "pestiroute.responses.local", Protocols: []string{responsesProtocol}, Settings: nativeSettings{
				BaseURL: backend.URL + "/v1/responses", RuntimeProfile: profile, AuthMode: "none", MaxRequestBodyBytes: 1 << 20, MaxRequestHeaderBytes: 8192,
				ConnectTimeout: "1s", TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s", StreamIdleTimeout: "5s", Model: "model-a", AccountID: "account-a",
				Capabilities: map[string]string{"llm.streaming": "supported"},
			}}
			prepared, err := prepareProtectedConfig(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			var ready, draining atomic.Bool
			ready.Store(true)
			h, closeComponents, err := composeHandler(prepared, &ready, &draining, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = closeComponents(context.Background()) })
			gateway := httptest.NewServer(h)
			defer gateway.Close()

			post := func(body string) (*http.Response, []byte) {
				req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+issued.Secret)
				req.Header.Set("Content-Type", "application/json")
				resp, err := gateway.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				return resp, got
			}
			first := `{"model":"model-a","stream":true,"parallel_tool_calls":false,"input":"find two"}`
			resp, body := post(first)
			if resp.StatusCode != http.StatusOK || string(body) != trace || string(<-captured) != first {
				var result struct {
					Error struct{ Code, Message string } `json:"error"`
				}
				_ = json.Unmarshal(body, &result)
				t.Fatalf("synthetic interleaved trace status=%d code=%s message=%s", resp.StatusCode, result.Error.Code, result.Error.Message)
			}
			callIDs := make(map[string]string)
			deltas := make(map[string]string)
			completed := make(map[string]string)
			for _, frame := range strings.Split(string(body), "\n\n") {
				if frame == "" {
					continue
				}
				data := strings.TrimPrefix(strings.SplitN(frame, "\n", 2)[1], "data: ")
				var event struct {
					Type      string `json:"type"`
					ItemID    string `json:"item_id"`
					Delta     string `json:"delta"`
					Arguments string `json:"arguments"`
					Item      struct {
						ID     string `json:"id"`
						CallID string `json:"call_id"`
					} `json:"item"`
				}
				if err := json.Unmarshal([]byte(data), &event); err != nil {
					t.Fatalf("decode synthetic SSE event %q: %v", data, err)
				}
				switch event.Type {
				case "response.output_item.added":
					callIDs[event.Item.ID] = event.Item.CallID
				case "response.function_call_arguments.delta":
					deltas[event.ItemID] += event.Delta
				case "response.function_call_arguments.done":
					completed[event.ItemID] = event.Arguments
				}
			}
			wantCalls := map[string]string{"item_a": "call_a", "item_b": "call_b"}
			wantArguments := map[string]string{"item_a": `{"key":"alpha"}`, "item_b": `{"key":"beta"}`}
			for itemID, callID := range wantCalls {
				if callIDs[itemID] != callID || deltas[itemID] != wantArguments[itemID] || completed[itemID] != deltas[itemID] {
					t.Fatalf("call trace item=%q call=%q deltas=%q complete=%q; want call=%q arguments=%q", itemID, callIDs[itemID], deltas[itemID], completed[itemID], callID, wantArguments[itemID])
				}
			}
			second := `{"model":"model-a","parallel_tool_calls":false,"input":[{"type":"function_call","id":"item_a","call_id":"call_a","name":"lookup","arguments":"{\"key\":\"alpha\"}"},{"type":"function_call_output","call_id":"call_a","output":"result-a"},{"type":"function_call","id":"item_b","call_id":"call_b","name":"lookup","arguments":"{\"key\":\"beta\"}"},{"type":"function_call_output","call_id":"call_b","output":"result-b"}]}`
			resp, body = post(second)
			upstreamHistory := string(<-captured)
			if resp.StatusCode != http.StatusOK || upstreamHistory != second || dispatches.Load() != 2 {
				t.Fatalf("client-resubmitted calls status=%d dispatched=%d body=%s", resp.StatusCode, dispatches.Load(), body)
			}
			var history struct {
				Input []struct {
					Type      string `json:"type"`
					ID        string `json:"id"`
					CallID    string `json:"call_id"`
					Arguments string `json:"arguments"`
					Output    string `json:"output"`
				} `json:"input"`
			}
			if err := json.Unmarshal([]byte(upstreamHistory), &history); err != nil {
				t.Fatalf("decode resubmitted history: %v", err)
			}
			resubmitted := make(map[string]string)
			results := make(map[string]string)
			for _, item := range history.Input {
				switch item.Type {
				case "function_call":
					if callIDs[item.ID] != item.CallID {
						t.Fatalf("resubmitted item %q linked to %q; trace linked to %q", item.ID, item.CallID, callIDs[item.ID])
					}
					resubmitted[item.ID] = item.Arguments
				case "function_call_output":
					results[item.CallID] = item.Output
				}
			}
			for itemID, arguments := range completed {
				if resubmitted[itemID] != arguments || results[callIDs[itemID]] == "" {
					t.Fatalf("resubmitted call %q arguments=%q result=%q; completed arguments=%q", itemID, resubmitted[itemID], results[callIDs[itemID]], arguments)
				}
			}
			resp, body = post(`{"model":"model-a","parallel_tool_calls":true,"input":"parallel required"}`)
			var failure struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal(body, &failure) != nil || resp.StatusCode != http.StatusBadRequest || failure.Error.Code != "unsupported_capability" || dispatches.Load() != 2 {
				t.Fatalf("parallel requirement status=%d code=%q dispatches=%d body=%s", resp.StatusCode, failure.Error.Code, dispatches.Load(), body)
			}
		})
	}
}
