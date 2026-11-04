package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

// Preserve chunked delivery and Flush: buffering with a Recorder hides unread
// HTTP framing when the Python runner stops at response.completed.
type m043StreamingStatusWriter struct {
	http.ResponseWriter
	status *atomic.Int32
}

func (w m043StreamingStatusWriter) WriteHeader(code int) {
	w.status.Store(int32(code))
	w.ResponseWriter.WriteHeader(code)
}

func (w m043StreamingStatusWriter) Flush() {
	w.ResponseWriter.(http.Flusher).Flush()
}

// Run the actual Python entrypoint, stdin key handling and exact public request
// through production YAML composition. All credentials and upstream bytes are
// synthetic; only the local TLS fixture can receive dispatches.
func TestAntigravityToolRunnerProductionCompositionOffline(t *testing.T) {
	t.Setenv("PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT", "1")
	_, dbPath, keyPath := protectedFixture(t)
	signatureBytes := []byte("SYNTHETIC_SIGNATURE_CANARY")
	signature := base64.StdEncoding.EncodeToString(signatureBytes)
	var upstreamCalls, gatewayCalls, lastStatus atomic.Int32
	var secondRequestValid atomic.Bool
	var gatewayConnectionsMu sync.Mutex
	var gatewayConnections []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := upstreamCalls.Add(1)
		if call == 2 {
			var got map[string]any
			decodeErr := json.NewDecoder(r.Body).Decode(&got)
			var want map[string]any
			wantJSON := fmt.Sprintf(`{"contents":[{"parts":[{"text":"Add 17 and 25."}],"role":"user"},{"parts":[{"functionCall":{"args":{"a":17,"b":25},"id":"synthetic-call","name":"add"},"thoughtSignature":%q}],"role":"model"},{"parts":[{"functionResponse":{"id":"synthetic-call","name":"add","response":{"output":"42"}}}],"role":"user"}],"tools":[{"functionDeclarations":[{"description":"Add two bounded integers.","name":"add","parametersJsonSchema":{"additionalProperties":false,"properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"],"type":"object"}}]}]}`, signature)
			if json.Unmarshal([]byte(wantJSON), &want) == nil && decodeErr == nil {
				// Match the complete history and declaration; unrelated dynamic envelope fields vary per request.
				request, ok := got["request"].(map[string]any)
				secondRequestValid.Store(ok && reflect.DeepEqual(request["contents"], want["contents"]) && reflect.DeepEqual(request["tools"], want["tools"]))
			}
		} else {
			_, _ = io.Copy(io.Discard, r.Body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			frame, _ := json.Marshal(map[string]any{"response": map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"functionCall": map[string]any{"name": "add", "id": "synthetic-call", "args": map[string]int{"a": 17, "b": 25}}, "thoughtSignature": signature}}}}}}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
			w.(http.Flusher).Flush()
		} else {
			_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Synthetic ordinary answer\"}]}}]}}\n\n")
		}
		_, _ = io.WriteString(w, "data: {\"response\":{\"done\":true}}\n\n")
	}))
	defer upstream.Close()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal("synthetic database unavailable")
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: "antigravity-local", Connector: "antigravity", Enabled: true}); err != nil {
		t.Fatal("synthetic account setup failed")
	}
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0x31}, secure.KeySize), 0600); err != nil {
		t.Fatal("synthetic key setup failed")
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal("synthetic key unavailable")
	}
	sealed, err := secure.Seal(key, 1, "test-v1", "credentials", "oauth", "antigravity-local", []byte(`{"access_token":"SYNTHETIC_ACCESS","project_id":"antigravity-local"}`))
	if err != nil {
		t.Fatal("synthetic sealing failed")
	}
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "oauth", AccountID: "antigravity-local", FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}); err != nil {
		t.Fatal("synthetic credential setup failed")
	}
	policies := sqlite.NewKeyPolicies(db)
	policy, err := policies.GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal("synthetic policy unavailable")
	}
	policy, err = policies.Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{"gemini-2.5-flash"}, Connectors: []string{"antigravity"}, RPM: 20, TPM: 100000})
	if err != nil {
		t.Fatal("synthetic policy setup failed")
	}
	configPath := filepath.Join(t.TempDir(), "gateway.yaml")
	yaml := fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", max_request_bytes: 1048576, shutdown_timeout: 1s}
storage: {driver: sqlite, path: %q}
secrets: {master_key_file: %q}
connectors:
  - id: antigravity
    kind: connector
    implementation: pestiroute.antigravity.v1internal
    protocols: [openai.responses.v1]
    settings: {project: antigravity-local, model: gemini-2.5-flash, account_id: antigravity-local, endpoint: %s}
routes:
  - id: antigravity-tool-smoke
    protocol: openai.responses.v1
    mode: translation
    model: gemini-2.5-flash
    adapter: pestiroute.responses.native
    policy: standard
    budget: {unknown_estimate: reserve, conservative_tokens: 4096}
    targets: [{connector: antigravity, account: antigravity-local}]
policies: {standard: policy-id-a}
`, dbPath, keyPath, upstream.URL)
	if err := os.WriteFile(configPath, []byte(yaml), 0600); err != nil {
		t.Fatal("synthetic config setup failed")
	}
	protected, err := loadProtectedYAML(configPath)
	if err != nil {
		t.Fatal("synthetic config rejected")
	}
	certPath := filepath.Join(t.TempDir(), "roots.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}), 0600); err != nil {
		t.Fatal("synthetic trust setup failed")
	}
	t.Setenv("SSL_CERT_FILE", certPath)
	prepared, err := prepareProtectedConfig(ctx, config{protected: &protected, DatabasePath: dbPath, MasterKeyFile: keyPath})
	if err != nil {
		t.Fatal("synthetic preparation failed")
	}
	defer prepared.runtimeDB.Close()
	defer prepared.processLock.Close()
	var ready, draining atomic.Bool
	ready.Store(true)
	h, closeComponents, err := composeHandler(prepared, &ready, &draining, nil)
	if err != nil {
		t.Fatal("synthetic composition failed")
	}
	defer closeComponents(ctx)
	issued, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal("synthetic virtual key setup failed")
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gatewayCalls.Add(1)
		gatewayConnectionsMu.Lock()
		gatewayConnections = append(gatewayConnections, r.RemoteAddr)
		gatewayConnectionsMu.Unlock()
		h.ServeHTTP(m043StreamingStatusWriter{ResponseWriter: w, status: &lastStatus}, r)
	}))
	defer gateway.Close()
	scriptPath, err := filepath.Abs("../../scripts/antigravity_live_tool_smoke.py")
	if err != nil {
		t.Fatal("runner path unavailable")
	}
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal("runner unavailable")
	}
	diagnosticsPath := filepath.Join(filepath.Dir(scriptPath), "antigravity_smoke_response.py")
	diagnostics, err := os.ReadFile(diagnosticsPath)
	if err != nil {
		t.Fatal("safe diagnostic helper unavailable")
	}
	// Reconstruct precisely the historical public request, with store:false.
	legacy := strings.ReplaceAll(string(script), `"stream": True,`, `"stream": True, "store": False,`)
	for _, tc := range []struct {
		name, script            string
		success                 bool
		status, calls, upstream int32
	}{
		{"historical_store_false_rejected_before_dispatch", legacy, false, 400, 1, 0},
		{"exact_runner_chunked_two_turn_success", string(script), true, 200, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gatewayCalls.Store(0)
			upstreamCalls.Store(0)
			secondRequestValid.Store(false)
			lastStatus.Store(0)
			gatewayConnectionsMu.Lock()
			gatewayConnections = nil
			gatewayConnectionsMu.Unlock()
			path := filepath.Join(t.TempDir(), "runner.py")
			if err := os.WriteFile(path, []byte(tc.script), 0600); err != nil {
				t.Fatal("runner fixture setup failed")
			}
			if err := os.WriteFile(filepath.Join(filepath.Dir(path), "antigravity_smoke_response.py"), diagnostics, 0600); err != nil {
				t.Fatal("diagnostic helper fixture setup failed")
			}
			cmd := exec.CommandContext(ctx, "python3", path, strings.TrimPrefix(gateway.URL, "http://127.0.0.1:"))
			cmd.Stdin = strings.NewReader(issued.Secret + "\n")
			output, runErr := cmd.CombinedOutput()
			if (runErr == nil) != tc.success || lastStatus.Load() != tc.status || gatewayCalls.Load() != tc.calls || upstreamCalls.Load() != tc.upstream {
				t.Fatalf("runner_success=%t http_status=%d gateway_calls=%d upstream_calls=%d", runErr == nil, lastStatus.Load(), gatewayCalls.Load(), upstreamCalls.Load())
			}
			if tc.success && (!strings.Contains(string(output), "turn=1 status=200 failure=none") ||
				!strings.Contains(string(output), "turn=2 status=200 failure=none") ||
				!strings.Contains(string(output), "dispatches=2 replay_accepted=true")) {
				t.Fatal("safe per-turn success telemetry missing")
			}
			if tc.success && !secondRequestValid.Load() {
				t.Fatal("exact stateless Gemini history replay missing")
			}
			if tc.success {
				gatewayConnectionsMu.Lock()
				connections := append([]string(nil), gatewayConnections...)
				gatewayConnectionsMu.Unlock()
				if len(connections) != 2 || connections[0] == connections[1] {
					t.Fatal("two turns did not use distinct gateway connections")
				}
			}
			if strings.Contains(string(output), issued.Secret) || strings.Contains(string(output), signature) || strings.Contains(string(output), "synthetic-call") {
				t.Fatal("synthetic canary leaked")
			}
			t.Logf("runner_success=%t http_status=%d gateway_calls=%d upstream_calls=%d", runErr == nil, lastStatus.Load(), gatewayCalls.Load(), upstreamCalls.Load())
		})
	}
	var consumed int
	if err := db.QueryRowContext(ctx, `SELECT consumed FROM operator_request_budgets WHERE account_id=?`, sqlite.AntigravityOperatorBudgetID).Scan(&consumed); err != nil || consumed != 2 {
		t.Fatalf("synthetic budget consumed=%d err=%v; want two fake-upstream dispatches", consumed, err)
	}
}
