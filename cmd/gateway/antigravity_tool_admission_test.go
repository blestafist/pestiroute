package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestAntigravityToolCapabilityAdmissionAndSignedReplay(t *testing.T) {
	t.Setenv("PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT", "1")
	_, dbPath, keyPath := protectedFixture(t)
	const accountID = "antigravity-tool-admission"
	const unsupportedAccountID = "antigravity-tool-unadmitted"
	var calls atomic.Int32
	replayed := make(chan []byte, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 1:
			_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"id\":\"call-admission-1\",\"args\":{\"q\":\"synthetic\"}},\"thoughtSignature\":\"synthetic-signed-function\"}]}}]}}\n\ndata: {\"response\":{\"done\":true}}\n\n")
		case 2:
			body, _ := io.ReadAll(r.Body)
			replayed <- body
			_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Synthetic tool result complete\"}]}}]}}\n\ndata: {\"response\":{\"done\":true}}\n\n")
		default:
			_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"id\":\"call-no-include\",\"args\":{}},\"thoughtSignature\":\"must-fail-closed\"}]}}]}}\n\ndata: {\"response\":{\"done\":true}}\n\n")
		}
	}))
	t.Cleanup(upstream.Close)

	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, account := range []struct{ id, connector string }{{accountID, "antigravity"}, {unsupportedAccountID, "antigravity-unadmitted"}} {
		if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: account.id, Connector: account.connector, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0x32}, secure.KeySize), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []struct{ id, credential string }{{accountID, "oauth"}, {unsupportedAccountID, "oauth-unadmitted"}} {
		sealed, err := secure.Seal(key, 1, "test-v1", "credentials", account.credential, account.id, []byte(`{"access_token":"SYNTHETIC_ACCESS_TOKEN","project_id":"synthetic-project"}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: account.credential, AccountID: account.id, FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := sqlite.NewKeyPolicies(db).GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	policy, err = sqlite.NewKeyPolicies(db).Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{"gemini-2.5-flash", "gpt-4o"}, Connectors: []string{"antigravity", "antigravity-unadmitted"}, RPM: 20, TPM: 100000})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
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
    settings: {project: synthetic-project, model: gemini-2.5-flash, account_id: %q, endpoint: %s}
  - id: antigravity-unadmitted
    kind: connector
    implementation: pestiroute.antigravity.v1internal
    protocols: [openai.responses.v1]
    settings: {project: synthetic-project, model: gpt-4o, account_id: %q, credential_id: oauth-unadmitted, endpoint: %s}
routes:
  - id: antigravity-flash
    protocol: openai.responses.v1
    mode: translation
    model: gemini-2.5-flash
    adapter: pestiroute.responses.native
    policy: standard
    budget: {unknown_estimate: reserve, conservative_tokens: 4096}
    targets: [{connector: antigravity, account: %q}]
  - id: antigravity-unadmitted
    protocol: openai.responses.v1
    mode: translation
    model: gpt-4o
    adapter: pestiroute.responses.native
    policy: standard
    budget: {unknown_estimate: reserve, conservative_tokens: 4096}
    targets: [{connector: antigravity-unadmitted, account: %q}]
policies: {standard: policy-id-a}
`, dbPath, keyPath, accountID, upstream.URL, unsupportedAccountID, upstream.URL, accountID, unsupportedAccountID)
	if err := os.WriteFile(configPath, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	protected, err := loadProtectedYAML(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(upstream.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(t.TempDir(), "test-roots.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", certPath)
	prepared, err := prepareProtectedConfig(ctx, config{protected: &protected, DatabasePath: dbPath, MasterKeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = prepared.runtimeDB.Close()
		_ = prepared.processLock.Close()
	})
	var ready, draining atomic.Bool
	ready.Store(true)
	composeConfig := prepared
	composeConfig.runtimeDB = nil
	composeConfig.runtimeKey = secure.MasterKey{}
	h, closeComponents, err := composeHandler(composeConfig, &ready, &draining, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeComponents(context.Background()) })
	issued, err := sqlite.NewVirtualKeys(prepared.runtimeDB).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(h)
	t.Cleanup(gateway.Close)
	post := func(body string) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+issued.Secret)
		req.Header.Set("Content-Type", "application/json")
		return gateway.Client().Do(req)
	}

	request := `{"model":"gemini-2.5-flash","stream":true,"include":["reasoning.encrypted_content"],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":"auto","input":"synthetic prompt"}`
	resp, err := post(request)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	first := readGatewayUntil(t, reader, "function_call")
	remaining, err := io.ReadAll(reader)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(remaining), "response.completed") {
		t.Fatalf("admitted request status=%d err=%v stream=%q", resp.StatusCode, err, first+string(remaining))
	}
	stream := first + string(remaining)
	reasoning, call := gatewayOutputItems(t, stream)
	history, err := json.Marshal([]any{reasoning, call, map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": "synthetic result"}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err = post(`{"model":"gemini-2.5-flash","stream":true,"input":` + string(history) + `}`)
	if err != nil {
		t.Fatal(err)
	}
	final, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(final), "Synthetic tool result complete") || !strings.Contains(string(final), "response.completed") {
		t.Fatalf("tool replay status=%d err=%v stream=%q", resp.StatusCode, err, final)
	}
	select {
	case body := <-replayed:
		if !strings.Contains(string(body), "synthetic-signed-function") || !strings.Contains(string(body), "functionResponse") || !strings.Contains(string(body), "synthetic result") {
			t.Fatalf("signed tool history not replayed: %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tool replay did not reach fake provider")
	}
	if calls.Load() != 2 {
		t.Fatalf("provider calls=%d before negative cases, want 2", calls.Load())
	}

	unsupported, err := post(strings.Replace(request, "gemini-2.5-flash", "gpt-4o", 1))
	if err != nil {
		t.Fatal(err)
	}
	unsupportedBody, err := io.ReadAll(unsupported.Body)
	_ = unsupported.Body.Close()
	if err != nil || unsupported.StatusCode != http.StatusBadRequest || !strings.Contains(string(unsupportedBody), `"code":"unsupported_capability"`) || calls.Load() != 2 {
		t.Fatalf("unsupported model status=%d err=%v calls=%d body=%q", unsupported.StatusCode, err, calls.Load(), unsupportedBody)
	}

	withoutInclude := strings.Replace(request, `,"include":["reasoning.encrypted_content"]`, "", 1)
	resp, err = post(withoutInclude)
	if err != nil {
		t.Fatal(err)
	}
	noInclude, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var failure struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeErr := json.Unmarshal(noInclude, &failure)
	if err != nil || decodeErr != nil || resp.StatusCode != http.StatusServiceUnavailable || failure.Error.Code != "upstream_stream_error" || failure.Error.Type != "unavailable" || failure.Error.Message != "Upstream stream failed" || strings.Contains(string(noInclude), `"encrypted_content"`) || calls.Load() != 3 {
		t.Fatalf("missing-include signed part was not fail-closed: status=%d read_err=%v decode_err=%v code=%q type=%q calls=%d carrier=%t", resp.StatusCode, err, decodeErr, failure.Error.Code, failure.Error.Type, calls.Load(), strings.Contains(string(noInclude), `"encrypted_content"`))
	}
}
