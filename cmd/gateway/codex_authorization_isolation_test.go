package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
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

func TestCodexAuthorizationAndCredentialIsolation(t *testing.T) {
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
	type accountFixture struct{ account, connector, model, credential, token string }
	fixtures := []accountFixture{
		{account: "codex-account-a", connector: "codex-a", model: "model-a", credential: "codex-cred-1", token: "synthetic-a-token"},
		{account: "codex-account-b", connector: "codex-b", model: "model-b", credential: "codex-cred-2", token: "synthetic-b-token"},
	}
	accounts, credentialStore := sqlite.NewAccounts(db), sqlite.NewCredentials(db)
	for _, fixture := range fixtures {
		if _, err := accounts.Create(ctx, sqlite.Account{ID: fixture.account, Connector: fixture.connector, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		expires := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
		bundle := fmt.Sprintf(`{"version":1,"access_token":%q,"refresh_token":"refresh-%s","account_id":%q,"expires_at":%q}`, fixture.token, fixture.account, fixture.account, expires.Format(time.RFC3339Nano))
		sealed, err := secure.Seal(master, 1, "v1", "credentials", fixture.credential, fixture.account, []byte(bundle))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := credentialStore.Create(ctx, sqlite.Credential{ID: fixture.credential, AccountID: fixture.account, FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext, ExpiresAt: &expires}); err != nil {
			t.Fatal(err)
		}
	}

	policies := sqlite.NewKeyPolicies(db)
	policyA, err := policies.GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	policyA, err = policies.Update(ctx, policyA.ID, policyA.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{"model-a"}, Connectors: []string{"codex-a"}, RPM: 20, TPM: 10000})
	if err != nil {
		t.Fatal(err)
	}
	keyA, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policyA.ID, PolicyRevision: policyA.Revision})
	if err != nil {
		t.Fatal(err)
	}
	policyB, err := policies.Create(ctx, sqlite.CreateKeyPolicyParams{ID: "policy-b", Enabled: true, Models: []string{"model-b"}, Connectors: []string{"codex-b"}, RPM: 20, TPM: 10000})
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policyB.ID, PolicyRevision: policyB.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var sends atomic.Int32
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		fixture := fixtures[0]
		if r.Header.Get("ChatGPT-Account-Id") == fixtures[1].account {
			fixture = fixtures[1]
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+fixture.token {
			t.Errorf("provider received wrong scoped authorization for %s", fixture.account)
		}
		if got := r.Header.Get("ChatGPT-Account-Id"); got != fixture.account {
			t.Errorf("provider account = %q, want selected %q", got, fixture.account)
		}
		for _, name := range []string{"Cookie", "Cookie2", "Session-Id", "X-Session-Id", "X-Account-Id", "X-Api-Key", "X-Private-Hop"} {
			if r.Header.Get(name) != "" {
				t.Errorf("spoofed client header %q reached provider", name)
			}
		}
		if r.Header.Get("Authorization") == "Bearer "+keyA.Secret || r.Header.Get("Authorization") == "Bearer "+keyB.Secret {
			t.Error("virtual key reached provider")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || !strings.Contains(string(body), `"prompt_cache_key":"shared-cache-key"`) {
			t.Errorf("client cache affinity was not preserved: read error=%v", err)
		}
		if strings.Contains(string(body), "previous_response_id") {
			t.Error("cache affinity was treated as response-resource ownership")
		}
		if fixture.account == fixtures[0].account {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `{"error":{"type":"server_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	defer backend.Close()
	addr := backend.Listener.Addr().String()
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}
	defer transport.CloseIdleConnections()
	doer := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	p := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0").protected
	p.Connectors = []protectedConnector{
		{ID: fixtures[0].connector, Kind: "connector", Implementation: "pestiroute.codex.responses", Protocols: []string{responsesProtocol}, Settings: nativeSettings{Profile: "codex-responses-http-sse-v1", Model: fixtures[0].model, AccountID: fixtures[0].account}},
		{ID: fixtures[1].connector, Kind: "connector", Implementation: "pestiroute.codex.responses", Protocols: []string{responsesProtocol}, Settings: nativeSettings{Profile: "codex-responses-http-sse-v1", Model: fixtures[1].model, AccountID: fixtures[1].account}},
	}
	budget := routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(100)}
	p.Routes = []protectedRoute{
		{ID: "route-a", Protocol: responsesProtocol, Mode: "native", Model: fixtures[0].model, Adapter: "pestiroute.responses.native", Policy: "standard", Budget: budget, Targets: []routeTarget{{Connector: fixtures[0].connector, Account: fixtures[0].account}}},
		{ID: "route-b", Protocol: responsesProtocol, Mode: "native", Model: fixtures[1].model, Adapter: "pestiroute.responses.native", Policy: "account-b", Budget: budget, Targets: []routeTarget{{Connector: fixtures[1].connector, Account: fixtures[1].account}}},
	}
	p.Policies["account-b"] = policyB.ID
	prepared, err := prepareProtectedConfig(ctx, config{protected: p, DatabasePath: dbPath, MasterKeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	// Protected YAML intentionally fixes Codex's logical credential name to oauth.
	// The composition binding maps that logical name to a separate persistent ID per account.
	for i := range prepared.Components {
		for _, fixture := range fixtures {
			if prepared.Components[i].ID == core.InstanceID(fixture.connector) {
				prepared.Components[i].CredentialEnv = fixture.credential
			}
		}
	}
	var logs bytes.Buffer
	prepared.logger = slog.New(slog.NewTextHandler(&logs, nil))
	ready, draining := atomic.Bool{}, atomic.Bool{}
	ready.Store(true)
	h, closeComponents, err := composeHandlerWithFactory(prepared, &ready, &draining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		return codexCompositionConnector{Connector: codex.NewConnector(), doer: doer}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeComponents(context.Background()) })
	callBody := func(token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("ChatGPT-Account-Id", fixtures[1].account)
		r.Header.Set("Cookie", "session=private-client-session")
		r.Header.Set("Cookie2", "private-cookie2")
		r.Header.Set("Session-Id", "client-session")
		r.Header.Set("X-Session-Id", "client-session-2")
		r.Header.Set("X-Account-Id", fixtures[1].account)
		r.Header.Set("X-Api-Key", "private-client-api-key")
		r.Header.Set("Connection", "X-Private-Hop")
		r.Header.Set("X-Private-Hop", "must-not-forward")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	call := func(token, model string) *httptest.ResponseRecorder {
		return callBody(token, fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"prompt_cache_key":"shared-cache-key","input":"hello"}`, model))
	}
	responseA := call(keyA.Secret, fixtures[0].model)
	if responseA.Code == http.StatusOK {
		t.Fatalf("account A provider failure returned success: status=%d", responseA.Code)
	}
	for _, secret := range []string{fixtures[0].token, fixtures[1].token, keyA.Secret, keyB.Secret, "refresh-" + fixtures[0].account, "refresh-" + fixtures[1].account} {
		if strings.Contains(responseA.Body.String(), secret) {
			t.Fatal("account A error response leaked a credential or virtual-key secret")
		}
	}
	responseB := call(keyB.Secret, fixtures[1].model)
	if responseB.Code != http.StatusOK || !strings.Contains(responseB.Body.String(), "response.completed") {
		t.Fatalf("account B request failed after account A error: status=%d body=%s", responseB.Code, responseB.Body.String())
	}
	if sends.Load() != 2 {
		t.Fatalf("expected exactly two authorized provider sends, got %d", sends.Load())
	}
	resourceReuse := callBody(keyB.Secret, fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"prompt_cache_key":"shared-cache-key","previous_response_id":"response-owned-by-a","input":"hello"}`, fixtures[1].model))
	if resourceReuse.Code == http.StatusOK || sends.Load() != 2 {
		t.Fatalf("cache affinity enabled cross-account response-resource reuse: status=%d sends=%d", resourceReuse.Code, sends.Load())
	}
	if denied := call(keyA.Secret, fixtures[1].model); denied.Code == http.StatusOK || sends.Load() != 2 {
		t.Fatalf("out-of-policy request dispatched: status=%d sends=%d", denied.Code, sends.Load())
	}
	if _, err := sqlite.NewVirtualKeys(prepared.runtimeDB).Revoke(ctx, keyA.ID); err != nil {
		t.Fatal(err)
	}
	if denied := call(keyA.Secret, fixtures[0].model); denied.Code == http.StatusOK || sends.Load() != 2 {
		t.Fatalf("revoked key dispatched: status=%d sends=%d", denied.Code, sends.Load())
	}
	if _, err := sqlite.NewAccounts(prepared.runtimeDB).SetEnabled(ctx, fixtures[1].account, false); err != nil {
		t.Fatal(err)
	}
	if denied := call(keyB.Secret, fixtures[1].model); denied.Code == http.StatusOK || sends.Load() != 2 {
		t.Fatalf("disabled account dispatched: status=%d sends=%d", denied.Code, sends.Load())
	}
	for _, secret := range []string{fixtures[0].token, fixtures[1].token, keyA.Secret, keyB.Secret, "refresh-" + fixtures[0].account, "refresh-" + fixtures[1].account} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("credential or virtual-key secret leaked to logs")
		}
	}
}
