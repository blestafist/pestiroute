package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

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
	if c.protected.Connectors[0].Implementation != "pestiroute.responses.native" {
		t.Fatal("native connector configuration changed")
	}
}
