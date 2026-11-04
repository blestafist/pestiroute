package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	adminhttp "github.com/blestafist/pestiroute/internal/admin"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

type httpAuthConnector struct {
	core.Connector
	mu      sync.Mutex
	actions []string
}

func (*httpAuthConnector) Descriptor() core.Descriptor {
	return core.Descriptor{ID: "http-auth", Kind: core.ComponentConnector, ImplementationVersion: "test", APIVersions: []core.APIVersion{{Major: 1}}, Protocols: []string{responsesProtocol}, ConnectorType: "test"}
}
func (*httpAuthConnector) Init(context.Context, core.ComponentConfig) error { return nil }
func (*httpAuthConnector) Health(context.Context) core.Health {
	return core.Health{State: core.HealthReady}
}
func (*httpAuthConnector) Close(context.Context) error { return nil }
func (*httpAuthConnector) HTTPDoer() core.HTTPDoer     { return principalSpyDoer{} }
func (c *httpAuthConnector) Authenticate(_ context.Context, r core.AuthRequest, _ core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.actions = append(c.actions, r.Action)
	switch r.Action {
	case "start":
		return core.AuthResult{Supported: true, NextAction: "continue", State: "opaque-state", UserAction: &core.AuthUserAction{VerificationURI: "https://auth.example.test/", UserCode: "A1B2", PollInterval: time.Second * 3}}, nil
	case "continue":
		if r.CallbackInput != "callback-secret" || string(r.State) != "opaque-state" {
			return core.AuthResult{}, nil
		}
		return core.AuthResult{Supported: true, Credentials: map[string][]byte{"bearer": []byte("continued-token")}}, nil
	case "refresh":
		return core.AuthResult{Supported: true, Credentials: map[string][]byte{"bearer": []byte("refreshed-token")}}, nil
	default:
		return core.AuthResult{}, nil
	}
}

func TestAdminAuthHTTPUsesComposedCoreCoordinator(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "auth-http.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "master.key")
	if err = os.WriteFile(keyPath, []byte("01234567890123456789012345678901"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	accounts := sqlite.NewAccounts(db)
	if _, err = accounts.Create(ctx, sqlite.Account{ID: "account", Connector: "connector", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	envelope, err := secure.Seal(key, 1, "v1", "credentials", "credential", "account", []byte("initial-token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "credential", AccountID: "account", FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	connector := &httpAuthConnector{}
	coordinator := new(*core.AuthCoordinator)
	cfg := config{DatabasePath: "configured", runtimeDB: db, runtimeKey: key, authCoordinatorSink: coordinator,
		Components: []topologyComponent{{ID: "adapter", Implementation: responsesImplementation, Kind: core.ComponentAdapter}, {ID: "connector", Implementation: responsesImplementation, Kind: core.ComponentConnector, CredentialEnv: "credential", Endpoint: "http://127.0.0.1:1", MaxBodyBytes: 4096, MaxHeaderBytes: 4096, ConnectTimeout: "1s", TLSTimeout: "1s", HeaderTimeout: "1s", IdleTimeout: "1s"}},
		Routes:     []topologyRoute{{Protocol: responsesProtocol, Mode: core.ModeNative, Model: "test-model", Account: "account", Adapter: "adapter", Connector: "connector", Budget: core.RouteBudget{UnknownEstimate: core.UnknownEstimateReserve, ConservativeTokens: 10}, BudgetPolicy: "reserve", RouteID: "auth-route"}}}
	ready, draining := new(atomic.Bool), new(atomic.Bool)
	_, closeRuntime, err := composeHandlerWithFactory(cfg, ready, draining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		return connector
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeRuntime(context.Background()) }()
	if *coordinator == nil {
		t.Fatal("composition did not provide the auth coordinator")
	}
	t.Setenv("PESTIROUTE_ADMIN_TEST_TOKEN", "operator-secret")
	adminHandler, err := adminhttp.New(adminhttp.Config{Address: "127.0.0.1:9096", TokenEnv: "PESTIROUTE_ADMIN_TEST_TOKEN", ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second, DB: db, MasterKey: key, Auth: *coordinator})
	if err != nil {
		t.Fatal(err)
	}
	request := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Host = "127.0.0.1:9096"
		r.Header.Set("Authorization", "Bearer operator-secret")
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		adminHandler.ServeHTTP(w, r)
		return w
	}
	if w := request("/admin/v1/auth/sessions", `{"account_id":"absent"}`); w.Code != http.StatusNotFound {
		t.Fatalf("missing account start status %d: %s", w.Code, w.Body.String())
	}
	if _, err = accounts.SetEnabled(ctx, "account", false); err != nil {
		t.Fatal(err)
	}
	if w := request("/admin/v1/auth/sessions", `{"account_id":"account"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("disabled account start status %d: %s", w.Code, w.Body.String())
	}
	if _, err = accounts.SetEnabled(ctx, "account", true); err != nil {
		t.Fatal(err)
	}
	readRequest := httptest.NewRequest(http.MethodGet, "/admin/v1/status", nil)
	readRequest.Host = "127.0.0.1:9096"
	readRequest.Header.Set("Authorization", "Bearer operator-secret")
	readResponse := httptest.NewRecorder()
	adminHandler.ServeHTTP(readResponse, readRequest)
	if w := readResponse; w.Code != http.StatusOK || len(connector.actions) != 0 {
		t.Fatalf("read endpoint mutated auth state: status %d, actions %v", w.Code, connector.actions)
	}
	w := request("/admin/v1/auth/sessions", `{"account_id":"account"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("start status %d: %s", w.Code, w.Body.String())
	}
	var result struct {
		SessionID  string         `json:"session_id"`
		UserAction map[string]any `json:"user_action"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.SessionID == "" || result.UserAction["user_code"] != "A1B2" {
		t.Fatalf("start projection %#v: %v", result, err)
	}
	w = request("/admin/v1/auth/sessions/"+result.SessionID+"/continue", `{"callback_input":"callback-secret"}`)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "callback-secret") {
		t.Fatalf("continue status %d: %s", w.Code, w.Body.String())
	}
	w = request("/admin/v1/auth/sessions/"+result.SessionID+"/continue", `{"callback_input":"callback-secret"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("replay status %d: %s", w.Code, w.Body.String())
	}
	w = request("/admin/v1/auth/accounts/account/refresh", "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "refreshed-token") {
		t.Fatalf("refresh status %d: %s", w.Code, w.Body.String())
	}
	stored, err := sqlite.NewCredentials(db).Get(ctx, "account", "credential")
	if err != nil || stored.Revision != 3 {
		t.Fatalf("credential revision=%d err=%v", stored.Revision, err)
	}
	plain, err := secure.Open(key, secure.Envelope{FormatVersion: stored.FormatVersion, KeyVersion: stored.KeyVersion, Nonce: stored.Nonce, Ciphertext: stored.Ciphertext}, "credentials", stored.ID, stored.AccountID)
	if err != nil || !bytes.Equal(plain, []byte("refreshed-token")) {
		t.Fatalf("stored refreshed credential mismatch: %v", err)
	}
	clear(plain)
	connector.mu.Lock()
	actions := strings.Join(connector.actions, ",")
	connector.mu.Unlock()
	if actions != "start,continue,refresh" {
		t.Fatalf("connector action calls = %q", actions)
	}
	sessions := sqlite.NewAuthSessions(db)
	marker := core.AuthSession{ID: "quarantined-refresh", AccountID: "account", ConnectorID: "connector", Revision: stored.Revision}
	if err = sessions.CreateRefreshMarker(ctx, marker.ID, marker.AccountID, string(marker.ConnectorID), marker.Revision, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = sessions.QuarantineRefreshMarker(ctx, marker.ID, marker.AccountID, "ambiguous_result", marker.Revision, time.Now()); err != nil {
		t.Fatal(err)
	}
	if w := request("/admin/v1/auth/accounts/account/refresh", ""); w.Code != http.StatusConflict {
		t.Fatalf("quarantined refresh status %d: %s", w.Code, w.Body.String())
	}
	if w := request("/admin/v1/auth/accounts/missing/refresh", ""); w.Code != http.StatusNotFound {
		t.Fatalf("missing account refresh status %d: %s", w.Code, w.Body.String())
	}
	if _, err = accounts.SetEnabled(ctx, "account", false); err != nil {
		t.Fatal(err)
	}
	if w := request("/admin/v1/auth/accounts/account/refresh", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("disabled account refresh status %d: %s", w.Code, w.Body.String())
	}
}
