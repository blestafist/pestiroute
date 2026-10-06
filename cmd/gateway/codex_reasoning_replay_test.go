package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
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

func TestCodexEncryptedReasoningAndHistoryReplay(t *testing.T) {
	_, dbPath, keyPath := protectedFixture(t)
	ctx := context.Background()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const accountID, connectorID, model, liteModel = "reasoning-account", "codex-reasoning", "reasoning-model", "gpt-6-luna"
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
	policy, err = sqlite.NewKeyPolicies(db).Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{model, liteModel, "model-a"}, Connectors: []string{connectorID, "upstream"}, RPM: 20, TPM: 10000})
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

	requests := make(chan []byte, 2)
	var sends atomic.Int32
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+codexTestJWT(accountID) || r.Header.Get("ChatGPT-Account-Id") != accountID {
			t.Errorf("Codex credential scope mismatch")
		}
		if r.Header.Get("x-openai-internal-codex-responses-lite") != "true" {
			t.Errorf("trusted Lite header missing")
		}
		sends.Add(1)
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_synthetic\",\"encrypted_content\":\"synthetic-ciphertext\"}}\n\nevent: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_synthetic\",\"encrypted_content\":\"synthetic-ciphertext\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":8,\"output_tokens\":2,\"output_tokens_details\":{\"reasoning_tokens\":1}}}}\n\n")
	}))
	defer backend.Close()
	addr := backend.Listener.Addr().String()
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}
	defer transport.CloseIdleConnections()
	doer := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	p := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0").protected
	p.Connectors = append(p.Connectors, protectedConnector{ID: connectorID, Kind: "connector", Implementation: "pestiroute.codex.responses", Protocols: []string{responsesProtocol}, Settings: nativeSettings{Profile: "codex-responses-http-sse-v1", Model: model, AccountID: accountID}})
	p.Routes = append(p.Routes, protectedRoute{ID: "codex-reasoning", Protocol: responsesProtocol, Mode: "native", Model: model, Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(100)}, Targets: []routeTarget{{Connector: connectorID, Account: accountID}}})
	prepared, err := prepareProtectedConfig(ctx, config{protected: p, DatabasePath: dbPath, MasterKeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	newHandler := func() (http.Handler, func()) {
		ready, draining := atomic.Bool{}, atomic.Bool{}
		ready.Store(true)
		h, closeComponents, err := composeHandlerWithFactory(prepared, &ready, &draining, nil, func(item topologyComponent) core.Component {
			if item.Kind == core.ComponentAdapter {
				return adapter.NewAdapter()
			}
			if item.Implementation == "pestiroute.codex.responses" {
				c := codexCompositionConnector{Connector: codex.NewConnector(), doer: doer}
				return c
			}
			return responses.NewConnector()
		})
		if err != nil {
			t.Fatal(err)
		}
		return h, func() { _ = closeComponents(context.Background()) }
	}

	const modelPrefix = `"model":"reasoning-model","stream":true,"store":false,`
	reasoning := `"reasoning":{"effort":"medium"},"instructions":"Initial instruction.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Begin."}]},{"type":"message","role":"developer","phase":"commentary","content":[{"type":"input_text","text":"Update one."}],"extension":{"v":1}},{"type":"reasoning","id":"rs_opaque","summary":[{"type":"summary_text","text":"Fragment one."},{"type":"summary_text","text":"Fragment two."}],"encrypted_content":"ciphertext/opaque+==","vendor_extension":{"keep":[1,2]}},{"type":"message","role":"developer","phase":"final_approval","content":[{"type":"input_text","text":"Update two."}],"future_field":"preserve"}]`
	post := func(gateway *httptest.Server, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+issued.Secret)
		req.Header.Set("ChatGPT-Account-Id", "client-selected-other-account")
		req.Header.Set("Content-Type", "application/json")
		resp, err := gateway.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	productionHandler, closeProduction := newHandler()
	production := httptest.NewServer(productionHandler)
	defer production.Close()
	resp := post(production, "{"+modelPrefix+reasoning+"}")
	productionBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !bytes.Contains(productionBody, []byte(`"unsupported_capability"`)) || sends.Load() != 0 {
		closeProduction()
		t.Fatalf("production reasoning request status=%d sends=%d body=%s; want Unknown capability rejection before send", resp.StatusCode, sends.Load(), productionBody)
	}
	closeProduction()
	p.Connectors[len(p.Connectors)-1].Settings.Profile = "codex-responses-http-sse-lite-v1"
	p.Connectors[len(p.Connectors)-1].Settings.Model = liteModel
	p.Routes[len(p.Routes)-1].Model = liteModel
	prepared, err = prepareProtectedConfig(ctx, config{protected: p, DatabasePath: dbPath, MasterKeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}

	h, closeTest := newHandler()
	t.Cleanup(closeTest)
	gateway := httptest.NewServer(h)
	defer gateway.Close()
	litePrefix := `"model":"gpt-6-luna","stream":true,"store":false,"reasoning":{"effort":"high","context":"all_turns"},`
	liteInput := `"instructions":"Initial instruction.","input":[{"type":"additional_tools","tools":[]},{"type":"message","role":"user","content":[{"type":"input_text","text":"Begin."}]},{"type":"message","role":"developer","phase":"commentary","content":[{"type":"input_text","text":"Update one."}],"extension":{"v":1}},{"type":"reasoning","id":"rs_opaque","summary":[{"type":"summary_text","text":"Fragment one."},{"type":"summary_text","text":"Fragment two."}],"encrypted_content":"ciphertext/opaque+==","vendor_extension":{"keep":[1,2]}},{"type":"message","role":"developer","phase":"final_approval","content":[{"type":"input_text","text":"Update two."}],"future_field":"preserve"}`
	first := "{" + litePrefix + liteInput + "]}"
	second := "{" + litePrefix + liteInput + `,{"type":"message","role":"user","content":[{"type":"input_text","text":"Continue."}]}]}`
	for i, body := range []string{first, second} {
		response := post(gateway, body)
		responseBody, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("round %d status=%d body=%s", i+1, response.StatusCode, responseBody)
		}
		if !bytes.Contains(responseBody, []byte(`"encrypted_content":"synthetic-ciphertext"`)) || !bytes.Contains(responseBody, []byte(`"reasoning_tokens":1`)) {
			t.Fatalf("round %d reasoning item or separate usage metadata was not preserved: %s", i+1, responseBody)
		}
		previous := -1
		for _, event := range []string{"response.created", "response.output_item.added", "response.output_item.done", "response.completed"} {
			position := strings.Index(string(responseBody), "event: "+event)
			if position <= previous {
				t.Fatalf("round %d missing or reordered event %s", i+1, event)
			}
			previous = position
		}
		got := <-requests
		if !bytes.Equal(got, []byte(body)) {
			t.Fatalf("round %d native request bytes changed: got %s want %s", i+1, got, body)
		}
		var want, upstream struct {
			Instructions string            `json:"instructions"`
			Input        []json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal([]byte(body), &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(got, &upstream); err != nil {
			t.Fatalf("decode upstream round %d: %v", i+1, err)
		}
		if upstream.Instructions != want.Instructions || len(upstream.Input) != len(want.Input) {
			t.Fatalf("round %d history shape changed: instructions=%q items=%d want=%q/%d", i+1, upstream.Instructions, len(upstream.Input), want.Instructions, len(want.Input))
		}
		for j := range want.Input {
			if !bytes.Equal(upstream.Input[j], want.Input[j]) {
				t.Fatalf("round %d history item %d changed/reordered: got %s want %s", i+1, j, upstream.Input[j], want.Input[j])
			}
		}
	}
	if sends.Load() != 2 {
		t.Fatalf("upstream sends=%d, want 2 replay rounds", sends.Load())
	}
	for name, payload := range map[string]string{
		"previous response":     `"previous_response_id":"resp_private"`,
		"conversation resource": `"conversation":"conv_private"`,
		"resource item":         `"input":[{"type":"input_image","image_url":"resource_private"}]`,
		"server compaction":     `"context_management":[{"type":"compaction","compact_threshold":100}]`,
		"compaction trigger":    `"compaction_trigger":{"type":"threshold"}`,
	} {
		body := "{" + modelPrefix + payload
		if name != "resource item" {
			body += `,"input":"continue"`
		}
		body += "}"
		response := post(gateway, body)
		data, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusBadRequest || !bytes.Contains(data, []byte(`"unsupported_feature"`)) || sends.Load() != 2 {
			t.Errorf("%s was not rejected before send: status=%d sends=%d body=%s", name, response.StatusCode, sends.Load(), data)
		}
	}
}
