package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	anthropic "github.com/blestafist/pestiroute/internal/connector/anthropic"
	"github.com/blestafist/pestiroute/internal/connector/codex"
	responses "github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

type codexCompositionConnector struct {
	*codex.Connector
	doer core.HTTPDoer
}

func (c codexCompositionConnector) HTTPDoer() core.HTTPDoer { return c.doer }

func TestCodexCompositionYAMLSettingsAndRouteBinding(t *testing.T) {
	const valid = `version: 1
server: {listen: "127.0.0.1:8080", max_request_bytes: 1048576, shutdown_timeout: 5s}
storage: {driver: sqlite, path: ./data/gateway.db}
secrets: {master_key_file: ./secrets/master.key}
connectors:
  - id: codex
    kind: connector
    implementation: pestiroute.codex.responses
    protocols: [openai.responses.v1]
    settings: {profile: codex-responses-http-sse-v1, model: exact-model, account_id: account-a}
routes:
  - id: codex-route
    protocol: openai.responses.v1
    mode: native
    model: exact-model
    adapter: pestiroute.responses.native
    policy: standard
    budget: {unknown_estimate: reserve, conservative_tokens: 4096}
    targets: [{connector: codex, account: account-a}]
policies: {standard: policy-id-a}
`
	path := t.TempDir() + "/gateway.yaml"
	load := func(body string) error {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := loadProtectedYAML(path)
		return err
	}
	if err := load(valid); err != nil {
		t.Fatalf("valid Codex config rejected: %v", err)
	}
	for _, tc := range []struct{ name, from, to string }{
		{"wrong profile", "codex-responses-http-sse-v1", "other-profile"},
		{"missing model", "model: exact-model", "model: ''"},
		{"wrong route model", "model: exact-model", "model: other-model"},
		{"wrong route account", "account: account-a", "account: account-b"},
		{"unknown policy", "policy: standard", "policy: absent"},
		{"zero budget", "conservative_tokens: 4096", "conservative_tokens: 0"},
		{"int64 overflow", "conservative_tokens: 4096", "conservative_tokens: 9223372036854775808"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(valid, tc.from, tc.to, 1)
			if err := load(body); err == nil {
				t.Fatalf("invalid Codex config accepted: %s", tc.name)
			}
		})
	}
	lite := strings.Replace(valid, "codex-responses-http-sse-v1", "codex-responses-http-sse-lite-v1", 1)
	lite = strings.Replace(lite, "exact-model", "gpt-6-luna", 2)
	if err := load(lite); err != nil {
		t.Fatalf("valid opt-in Lite config rejected: %v", err)
	}
	for _, bad := range []string{
		strings.Replace(lite, "gpt-6-luna", "other-model", 1),
		strings.Replace(lite, "mode: native", "mode: translation", 1),
	} {
		if err := load(bad); err == nil {
			t.Fatalf("invalid Lite config accepted: %s", bad)
		}
	}
}

func TestCodexCompositionProtectedAdmissionAndDispatch(t *testing.T) {
	_, dbPath, keyPath := protectedFixture(t)
	ctx := context.Background()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	accountID, connectorID, model := "codex-account", "codex", "codex-model"
	if _, err = sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: accountID, Connector: connectorID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	bundle := fmt.Sprintf(`{"version":1,"access_token":"stale-access","refresh_token":"synthetic-refresh","account_id":%q,"expires_at":%q}`, accountID, expires.Format(time.RFC3339Nano))
	sealed, err := secure.Seal(key, 1, "v1", "credentials", "oauth", accountID, []byte(bundle))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "oauth", AccountID: accountID, FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext, ExpiresAt: &expires}); err != nil {
		t.Fatal(err)
	}
	const anthropicAccount, anthropicCredential = "anthropic-account", "anthropic-key"
	if _, err = sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: anthropicAccount, Connector: "anthropic", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	anthropicSecret, err := secure.Seal(key, 2, "test-v1", "credentials", anthropicCredential, anthropicAccount, []byte("synthetic-anthropic-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: anthropicCredential, AccountID: anthropicAccount, FormatVersion: anthropicSecret.FormatVersion, KeyVersion: anthropicSecret.KeyVersion, Nonce: anthropicSecret.Nonce, Ciphertext: anthropicSecret.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	var nativeCalls, anthropicCalls atomic.Int32
	nativeBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic" {
			t.Errorf("native route credential mismatch")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"completed"}`)
	}))
	defer nativeBackend.Close()
	anthropicBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicCalls.Add(1)
		if r.Header.Get("X-Api-Key") != "synthetic-anthropic-key" {
			t.Errorf("Anthropic route credential mismatch")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"anthropic-local\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer anthropicBackend.Close()
	policy, err := sqlite.NewKeyPolicies(db).GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	policy, err = sqlite.NewKeyPolicies(db).Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{model, "model-a", "client-model"}, Connectors: []string{connectorID, "upstream", "anthropic"}, RPM: 20, TPM: 10000})
	if err != nil {
		t.Fatal(err)
	}
	issued, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	var upstreamCalls, refreshCalls atomic.Int32
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			refreshCalls.Add(1)
			if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "synthetic-refresh" {
				t.Errorf("refresh did not use the selected account credential")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"rotated-refresh","id_token":%q,"expires_in":3600,"token_type":"Bearer"}`, codexTestJWT(accountID), codexTestJWT(accountID))
			return
		}
		upstreamCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+codexTestJWT(accountID) || r.Header.Get("ChatGPT-Account-Id") != accountID {
			t.Errorf("Codex auth scope mismatch: authorization=%q account=%q", r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-Id"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n")
	}))
	defer backend.Close()
	addr := backend.Listener.Addr().String()
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}
	defer transport.CloseIdleConnections()
	doer := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	p := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0").protected
	p.Connectors[0].Settings.BaseURL = nativeBackend.URL + "/v1"
	p.Connectors = append(p.Connectors,
		protectedConnector{ID: connectorID, Kind: "connector", Implementation: "pestiroute.codex.responses", Protocols: []string{responsesProtocol}, Settings: nativeSettings{Profile: "codex-responses-http-sse-v1", Model: model, AccountID: accountID}},
		protectedConnector{ID: "anthropic", Kind: "connector", Implementation: "pestiroute.anthropic.messages", Protocols: []string{responsesProtocol}, Settings: nativeSettings{Model: "client-model", AccountID: anthropicAccount, CredentialID: anthropicCredential}},
	)
	p.Routes = append(p.Routes,
		protectedRoute{ID: "codex-route", Protocol: responsesProtocol, Mode: "native", Model: model, Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(100)}, Targets: []routeTarget{{Connector: connectorID, Account: accountID}}},
		protectedRoute{ID: "anthropic-route", Protocol: responsesProtocol, Mode: "translation", Model: "client-model", Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(4096)}, Targets: []routeTarget{{Connector: "anthropic", Account: anthropicAccount}}},
	)
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
		switch item.Implementation {
		case "pestiroute.codex.responses":
			return codexCompositionConnector{Connector: codex.NewConnector(), doer: doer}
		case "pestiroute.anthropic.messages":
			upstreamURL, _ := url.Parse(anthropicBackend.URL)
			proxy := anthropicDoerFunc(func(r *http.Request) (*http.Response, error) {
				copy := r.Clone(r.Context())
				copy.URL = upstreamURL.ResolveReference(&url.URL{Path: "/v1/messages"})
				copy.Host = copy.URL.Host
				return anthropicBackend.Client().Do(copy)
			})
			return anthropicConnectorWithDoer{Connector: anthropic.NewConnector(), doer: proxy}
		default:
			return responses.NewConnector()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeComponents(context.Background()) })
	request := func(token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	response := request(issued.Secret, fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"input":"hello"}`, model))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "response.completed") || upstreamCalls.Load() != 1 || refreshCalls.Load() != 1 {
		t.Fatalf("Codex composed response status=%d inference=%d refresh=%d body=%s", response.Code, upstreamCalls.Load(), refreshCalls.Load(), response.Body.String())
	}
	nativeResponse := request(issued.Secret, `{"model":"model-a","stream":false,"input":"opaque"}`)
	if nativeResponse.Code != http.StatusOK || !strings.Contains(nativeResponse.Body.String(), "completed") {
		t.Fatalf("native route status=%d body=%s", nativeResponse.Code, nativeResponse.Body.String())
	}
	anthropicResponse := request(issued.Secret, `{"model":"client-model","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"max_output_tokens":32}`)
	if anthropicResponse.Code != http.StatusOK || !strings.Contains(anthropicResponse.Body.String(), "anthropic-local") {
		t.Fatalf("Anthropic route status=%d body=%s", anthropicResponse.Code, anthropicResponse.Body.String())
	}
	if nativeCalls.Load() != 1 || anthropicCalls.Load() != 1 || upstreamCalls.Load() != 1 || refreshCalls.Load() != 1 {
		t.Fatalf("coexisting route calls native=%d Anthropic=%d Codex=%d refresh=%d", nativeCalls.Load(), anthropicCalls.Load(), upstreamCalls.Load(), refreshCalls.Load())
	}
	wrongScope := request(issued.Secret, `{"model":"wrong-model","stream":true,"store":false,"input":"hello"}`)
	if wrongScope.Code == http.StatusOK || nativeCalls.Load() != 1 || anthropicCalls.Load() != 1 || upstreamCalls.Load() != 1 || refreshCalls.Load() != 1 {
		t.Fatalf("wrong-model scope changed provider calls: status=%d native=%d Anthropic=%d Codex=%d refresh=%d", wrongScope.Code, nativeCalls.Load(), anthropicCalls.Load(), upstreamCalls.Load(), refreshCalls.Load())
	}
	if _, err := sqlite.NewVirtualKeys(prepared.runtimeDB).Revoke(ctx, issued.ID); err != nil {
		t.Fatal(err)
	}
	if got := request(issued.Secret, fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"input":"hello"}`, model)); got.Code == http.StatusOK || upstreamCalls.Load() != 1 || refreshCalls.Load() != 1 {
		t.Fatalf("revoked key dispatched: status=%d calls=%d", got.Code, upstreamCalls.Load())
	}
	key2, err := sqlite.NewVirtualKeys(prepared.runtimeDB).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewAccounts(prepared.runtimeDB).SetEnabled(ctx, accountID, false); err != nil {
		t.Fatal(err)
	}
	if got := request(key2.Secret, fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"input":"hello"}`, model)); got.Code == http.StatusOK || upstreamCalls.Load() != 1 || refreshCalls.Load() != 1 {
		t.Fatalf("disabled account dispatched: status=%d calls=%d", got.Code, upstreamCalls.Load())
	}
	if upstreamCalls.Load() != 1 || refreshCalls.Load() != 1 {
		t.Fatalf("rejected Codex credentials reached upstream: inference=%d refresh=%d", upstreamCalls.Load(), refreshCalls.Load())
	}
	stored, err := sqlite.NewCredentials(prepared.runtimeDB).Get(ctx, accountID, "oauth")
	if err != nil || stored.Revision != 2 || stored.ExpiresAt == nil || !stored.ExpiresAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("fresh Codex credentials were not persisted before dispatch: revision=%d err=%v", stored.Revision, err)
	}
	plain, err := secure.Open(key, secure.Envelope{FormatVersion: stored.FormatVersion, KeyVersion: stored.KeyVersion, Nonce: stored.Nonce, Ciphertext: stored.Ciphertext}, "credentials", stored.ID, stored.AccountID)
	if err != nil || !strings.Contains(string(plain), "rotated-refresh") || !strings.Contains(string(plain), codexTestJWT(accountID)) {
		t.Fatal("inference did not retain the persisted refreshed credential bundle")
	}
	requests, err := sqlite.NewLedger(prepared.runtimeDB).QueryRequests(ctx, sqlite.RequestFilter{Model: model, Limit: 10})
	if err != nil || len(requests) != 1 || len(requests[0].Attempts) != 1 || requests[0].Attempts[0].Usage == nil || requests[0].Attempts[0].Usage.InputTokens == nil || *requests[0].Attempts[0].Usage.InputTokens != 3 || requests[0].Attempts[0].Usage.OutputTokens == nil || *requests[0].Attempts[0].Usage.OutputTokens != 2 {
		t.Fatalf("Codex usage accounting was not settled once")
	}
	anthropicRequests, err := sqlite.NewLedger(prepared.runtimeDB).QueryRequests(ctx, sqlite.RequestFilter{Model: "client-model", Limit: 10})
	if err != nil || len(anthropicRequests) != 1 || len(anthropicRequests[0].Attempts) != 1 || anthropicRequests[0].Attempts[0].Attempt.State != "succeeded" {
		t.Fatalf("Anthropic successful attempt was not accounted: records=%+v err=%v", anthropicRequests, err)
	}
	nativeRequests, err := sqlite.NewLedger(prepared.runtimeDB).QueryRequests(ctx, sqlite.RequestFilter{Model: "model-a", Limit: 10})
	if err != nil || len(nativeRequests) != 1 || len(nativeRequests[0].Attempts) != 1 {
		t.Fatalf("native request accounting missing after successful local response")
	}
}
