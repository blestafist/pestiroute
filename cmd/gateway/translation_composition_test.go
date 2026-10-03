package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	anthropic "github.com/blestafist/pestiroute/internal/connector/anthropic"
	responses "github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

type anthropicDoerFunc func(*http.Request) (*http.Response, error)

func (f anthropicDoerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

type anthropicConnectorWithDoer struct {
	*anthropic.Connector
	doer core.HTTPDoer
}

func (c anthropicConnectorWithDoer) HTTPDoer() core.HTTPDoer { return c.doer }

func TestProtectedConfiguredNativeAndTranslationCredentialIsolation(t *testing.T) {
	_, dbPath, keyPath := protectedFixture(t)
	ctx := context.Background()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	account := sqlite.Account{ID: "anthropic-account", Connector: "anthropic", Enabled: true}
	if _, err := sqlite.NewAccounts(db).Create(ctx, account); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	seal := func(id, account string, value []byte) {
		t.Helper()
		envelope, err := secure.Seal(key, 2, "test-v1", "credentials", id, account, value)
		if err != nil {
			t.Fatal(err)
		}
		_, err = sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: id, AccountID: account, FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext})
		if err != nil {
			t.Fatal(err)
		}
	}
	seal("anthro-key", account.ID, []byte("stored-anthropic-key"))
	policies := sqlite.NewKeyPolicies(db)
	policy, err := policies.GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	policy, err = policies.Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{
		Enabled: true, Models: []string{"model-a", "client-model"}, Connectors: []string{"upstream", "anthropic"}, RPM: 20, TPM: 100000,
	})
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

	var nativeAuthorization string
	var nativeBody []byte
	nativeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeAuthorization = r.Header.Get("Authorization")
		nativeBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"native","status":"completed"}`)
	}))
	defer nativeUpstream.Close()
	p := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0").protected
	p.Connectors[0].Settings.BaseURL = nativeUpstream.URL + "/v1"
	p.Connectors = append(p.Connectors, protectedConnector{ID: "anthropic", Kind: "connector", Implementation: "pestiroute.anthropic.messages", Settings: nativeSettings{
		Model: "client-model", AccountID: account.ID, CredentialID: "anthro-key",
	}})
	p.Routes = append(p.Routes, protectedRoute{ID: "translation-route", Protocol: responsesProtocol, Mode: "translation", Model: "client-model", Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(4096)}, Targets: []routeTarget{{Connector: "anthropic", Account: account.ID}}})
	missingRefConfig := *p
	missingRefConfig.Connectors = append([]protectedConnector(nil), p.Connectors...)
	missingRefConfig.Connectors[1].Settings.CredentialID = "  "
	if _, err := prepareProtectedConfig(ctx, config{protected: &missingRefConfig, DatabasePath: dbPath, MasterKeyFile: keyPath}); err == nil || !strings.Contains(err.Error(), "credential_id are required") {
		t.Fatalf("protected startup accepted missing credential_id: %v", err)
	}
	prepared, err := prepareProtectedConfig(ctx, config{protected: p, DatabasePath: dbPath, MasterKeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	var translatedKey string
	var translatedBody string
	var translatedCalls atomic.Int32
	translatedDoer := anthropicDoerFunc(func(r *http.Request) (*http.Response, error) {
		translatedCalls.Add(1)
		translatedKey = r.Header.Get("X-Api-Key")
		body, _ := io.ReadAll(r.Body)
		translatedBody = string(body)
		const events = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(events))}, nil
	})
	ready, draining := atomic.Bool{}, atomic.Bool{}
	ready.Store(true)
	h, closeComponents, err := composeHandlerWithFactory(prepared, &ready, &draining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		if item.Implementation == "pestiroute.anthropic.messages" {
			return anthropicConnectorWithDoer{Connector: anthropic.NewConnector(), doer: translatedDoer}
		}
		return responses.NewConnector()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeComponents(context.Background()) })

	call := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+issued.Secret)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	var wg sync.WaitGroup
	var nativeResponse, translationResponse *httptest.ResponseRecorder
	wg.Add(2)
	go func() {
		defer wg.Done()
		nativeResponse = call(`{"model":"model-a","stream":false,"input":"opaque","future_field":{"x":1}}`)
	}()
	go func() {
		defer wg.Done()
		translationResponse = call(`{"model":"client-model","stream":true,"input":"hello"}`)
	}()
	wg.Wait()
	if nativeResponse.Code != http.StatusOK || !strings.Contains(nativeResponse.Body.String(), `"id":"native"`) {
		t.Fatalf("native response=%d %s", nativeResponse.Code, nativeResponse.Body.String())
	}
	if translationResponse.Code != http.StatusOK || !strings.Contains(translationResponse.Body.String(), "hi") {
		t.Fatalf("translation response=%d calls=%d %s", translationResponse.Code, translatedCalls.Load(), translationResponse.Body.String())
	}
	if nativeAuthorization != "Bearer synthetic" || string(nativeBody) != `{"model":"model-a","stream":false,"input":"opaque","future_field":{"x":1}}` {
		t.Fatalf("native credential/body mismatch: auth=%q body=%s", nativeAuthorization, nativeBody)
	}
	if translatedKey != "stored-anthropic-key" || !strings.Contains(translatedBody, `"model":"client-model"`) || translatedCalls.Load() != 1 {
		t.Fatalf("translation credential/body mismatch: key=%q body=%s calls=%d", translatedKey, translatedBody, translatedCalls.Load())
	}
	if strings.Contains(translationResponse.Body.String(), "stored-anthropic-key") || strings.Contains(nativeResponse.Body.String(), "stored-anthropic-key") {
		t.Fatal("provider credential leaked to client")
	}
	accounts := sqlite.NewAccounts(prepared.runtimeDB)
	if _, err := accounts.SetEnabled(ctx, account.ID, false); err != nil {
		t.Fatal(err)
	}
	deniedDisabled := call(`{"model":"client-model","stream":true,"input":"hello"}`)
	if deniedDisabled.Code == http.StatusOK || translatedCalls.Load() != 1 || strings.Contains(deniedDisabled.Body.String(), "stored-anthropic-key") {
		t.Fatalf("disabled account was not denied safely: status=%d calls=%d body=%s", deniedDisabled.Code, translatedCalls.Load(), deniedDisabled.Body.String())
	}
	if _, err := accounts.SetEnabled(ctx, account.ID, true); err != nil {
		t.Fatal(err)
	}
	credentials := sqlite.NewCredentials(prepared.runtimeDB)
	stored, err := credentials.Get(ctx, account.ID, "anthro-key")
	if err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-time.Minute)
	stored.ExpiresAt = &expired
	if _, err := credentials.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	expiredResponse := call(`{"model":"client-model","stream":true,"input":"hello"}`)
	if expiredResponse.Code == http.StatusOK || translatedCalls.Load() != 1 || strings.Contains(expiredResponse.Body.String(), "stored-anthropic-key") {
		t.Fatalf("expired selected credential was not denied safely: status=%d calls=%d body=%s", expiredResponse.Code, translatedCalls.Load(), expiredResponse.Body.String())
	}
	if _, err := prepared.runtimeDB.ExecContext(ctx, `DELETE FROM credentials WHERE account_id=? AND id=?`, account.ID, "anthro-key"); err != nil {
		t.Fatal(err)
	}
	missingResponse := call(`{"model":"client-model","stream":true,"input":"hello"}`)
	if missingResponse.Code == http.StatusOK || translatedCalls.Load() != 1 || strings.Contains(missingResponse.Body.String(), "stored-anthropic-key") {
		t.Fatalf("missing selected credential was not denied safely: status=%d calls=%d body=%s", missingResponse.Code, translatedCalls.Load(), missingResponse.Body.String())
	}
	misbound, err := secure.Seal(key, 2, "test-v1", "credentials", "anthro-key", "account-a", []byte("misbound-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.Create(ctx, sqlite.Credential{ID: "anthro-key", AccountID: "account-a", FormatVersion: misbound.FormatVersion, KeyVersion: misbound.KeyVersion, Nonce: misbound.Nonce, Ciphertext: misbound.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	misboundResponse := call(`{"model":"client-model","stream":true,"input":"hello"}`)
	if misboundResponse.Code == http.StatusOK || translatedCalls.Load() != 1 || strings.Contains(misboundResponse.Body.String(), "misbound-key") {
		t.Fatalf("misbound selected credential was not denied safely: status=%d calls=%d body=%s", misboundResponse.Code, translatedCalls.Load(), misboundResponse.Body.String())
	}
}
