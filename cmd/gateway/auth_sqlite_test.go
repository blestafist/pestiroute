package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	adapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

type principalDispatch struct {
	principal    core.TrustedPrincipal
	ok           bool
	credential   string
	metadataAuth bool
	body         string
}

type principalSpyConnector struct {
	mu       sync.Mutex
	seen     []principalDispatch
	executes int
	supports int
}

func (*principalSpyConnector) Descriptor() core.Descriptor {
	return core.Descriptor{ID: "test.connector", Kind: core.ComponentConnector, ImplementationVersion: "1.0.0",
		APIVersions: []core.APIVersion{{Major: 1}}, Protocols: []string{responsesProtocol}, ConnectorType: "api"}
}
func (*principalSpyConnector) Init(context.Context, core.ComponentConfig) error { return nil }
func (*principalSpyConnector) Health(context.Context) core.Health {
	return core.Health{State: core.HealthReady}
}
func (c *principalSpyConnector) Capabilities(context.Context, core.CapabilityScope) core.CapabilityResult {
	c.recordSupport()
	return core.CapabilityResult{Values: map[core.Capability]core.CapabilityState{
		"llm.streaming": core.Supported, "llm.tools": core.Supported, "llm.tools.parallel": core.Supported,
		"llm.reasoning": core.Supported, "llm.structured_output": core.Supported,
		"llm.vision": core.Supported, "llm.audio": core.Supported,
	}}
}
func (c *principalSpyConnector) Close(context.Context) error { return nil }
func (c *principalSpyConnector) HTTPDoer() core.HTTPDoer     { return principalSpyDoer{} }
func (c *principalSpyConnector) Models(context.Context, core.ModelQuery, core.InvocationServices) (core.ModelsResult, *core.GatewayError) {
	c.recordSupport()
	return core.ModelsResult{}, nil
}
func (c *principalSpyConnector) EstimateUsage(context.Context, core.UsageQuery, core.InvocationServices) (core.EstimateResult, *core.GatewayError) {
	c.recordSupport()
	return core.EstimateResult{}, nil
}
func (c *principalSpyConnector) Authenticate(context.Context, core.AuthRequest, core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	c.recordSupport()
	return core.AuthResult{}, nil
}
func (c *principalSpyConnector) recordSupport() {
	c.mu.Lock()
	c.supports++
	c.mu.Unlock()
}
func (c *principalSpyConnector) Execute(ctx context.Context, request core.ExecutionRequest, _ core.AttemptScope, services core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	principal, ok := core.TrustedPrincipalFromContext(ctx)
	credential, _ := services.Credentials.Get(ctx, "bearer")
	if services.Logger != nil {
		services.Logger.Info("dispatch observed", "key_id", principal.KeyID, "credential", string(credential))
	}
	c.mu.Lock()
	c.executes++
	c.seen = append(c.seen, principalDispatch{principal: principal, ok: ok, credential: string(credential), metadataAuth: request.Metadata.Headers["Authorization"] != nil, body: string(request.Payload.Body)})
	c.mu.Unlock()
	return core.ExecutionResponse{}, &core.GatewayError{Code: "test_complete", Category: core.CategoryUnavailable, Message: "Test connector completed"}
}
func (c *principalSpyConnector) snapshot() (int, int, []principalDispatch) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.executes, c.supports, append([]principalDispatch(nil), c.seen...)
}

type principalSpyDoer struct{}

func (principalSpyDoer) Do(*http.Request) (*http.Response, error) { panic("unexpected transport call") }

func TestSQLiteVirtualKeyStoreProtectedHandler(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	policies := sqlite.NewKeyPolicies(db)
	policy, err := policies.Create(ctx, sqlite.CreateKeyPolicyParams{ID: "policy-snapshot", Models: []string{"gpt-5.4-mini"}, Connectors: []string{"connector"}, RPM: 10, TPM: 1000})
	if err != nil {
		t.Fatal(err)
	}
	keys := sqlite.NewVirtualKeys(db)
	accounts := sqlite.NewAccounts(db)
	if _, err := accounts.Create(ctx, sqlite.Account{ID: "account", Connector: "connector", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	accountAuthorizer := sqliteAccountAuthorizer{accounts: accounts}
	for _, tc := range []struct {
		account, connector string
		wantErr            bool
	}{
		{"account", "connector", false},
		{"account", "other-connector", true},
		{"missing-account", "connector", true},
	} {
		err := accountAuthorizer.AuthorizeAccount(ctx, tc.account, tc.connector)
		if (err != nil) != tc.wantErr {
			t.Fatalf("account authorization (%q, %q) error = %v", tc.account, tc.connector, err)
		}
	}
	valid, err := keys.Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := keys.Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.SetEnabled(ctx, disabled.ID, false); err != nil {
		t.Fatal(err)
	}
	revoked, err := keys.Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Revoke(ctx, revoked.ID); err != nil {
		t.Fatal(err)
	}

	const providerSecret = "file-backed-provider-secret"
	t.Setenv("M3018_PROVIDER_CREDENTIAL", providerSecret)
	var logOutput bytes.Buffer
	spy := &principalSpyConnector{}
	cfg := config{
		Listen: "127.0.0.1:0", keyStore: sqliteVirtualKeyStore{keys: keys}, policyStore: sqlitePolicyStore{policies: policies},
		accounting:        sqliteAccountingStore{ledger: sqlite.NewLedger(db)},
		accountAuthorizer: accountAuthorizer, logger: slog.New(slog.NewTextHandler(&logOutput, nil)),
		Components: []topologyComponent{
			{ID: "adapter", Implementation: "pestiroute.responses.native", Kind: core.ComponentAdapter},
			{ID: "connector", Implementation: "pestiroute.responses.native", Kind: core.ComponentConnector,
				Endpoint: "http://127.0.0.1:9999/v1/responses", CredentialEnv: "M3018_PROVIDER_CREDENTIAL", MaxBodyBytes: 4096, MaxHeaderBytes: 4096,
				ConnectTimeout: "1s", TLSTimeout: "1s", HeaderTimeout: "1s", IdleTimeout: "1s"},
		},
		Routes: []topologyRoute{{Protocol: responsesProtocol, Mode: core.ModeNative, Model: "gpt-5.4-mini", Account: "account", Adapter: "adapter", Connector: "connector", Budget: core.RouteBudget{UnknownEstimate: core.UnknownEstimateReserve, ConservativeTokens: 100}, BudgetPolicy: "reserve", RouteID: "auth-sqlite-route"}},
	}
	ready, draining := &atomic.Bool{}, &atomic.Bool{}
	handler, closeComponents, err := composeHandlerWithFactory(cfg, ready, draining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		return spy
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeComponents(context.Background()) }()

	principalWant := core.TrustedPrincipal{KeyID: valid.ID, PolicyID: policy.ID, KeyRevision: valid.Revision, PolicyRevision: policy.Revision}
	post := func(headers ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.4-mini"}`))
		r.Header.Set("Content-Type", "application/json")
		for _, value := range headers {
			r.Header.Add("Authorization", value)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := post("Bearer " + valid.Secret); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("valid key status = %d, body %s", w.Code, w.Body.String())
	}
	executes, _, seen := spy.snapshot()
	if executes != 1 || len(seen) != 1 || !seen[0].ok || seen[0].principal != principalWant {
		t.Fatalf("dispatched principal = %+v; executes=%d, want %+v", seen, executes, principalWant)
	}
	if seen[0].credential != providerSecret || seen[0].metadataAuth || seen[0].body != `{"model":"gpt-5.4-mini"}` {
		t.Fatalf("services credential/metadata authorization/body = %q/%t/%q", seen[0].credential, seen[0].metadataAuth, seen[0].body)
	}
	var attemptID string
	if err := db.QueryRowContext(ctx, `SELECT a.id FROM attempts a JOIN requests q ON q.id=a.request_id WHERE q.virtual_key_id=?`, valid.ID).Scan(&attemptID); err != nil {
		t.Fatalf("read dispatched attempt: %v", err)
	}
	ledger := sqlite.NewLedger(db)
	attempt, err := ledger.GetAttempt(ctx, attemptID)
	if err != nil || attempt.State != "failed" || attempt.DispatchedAt == nil {
		t.Fatalf("durable attempt = %+v, %v", attempt, err)
	}
	reservation, err := ledger.GetReservation(ctx, attemptID)
	if err != nil || reservation.State != "conservative" || reservation.EffectiveCharge != 100 {
		t.Fatalf("durable reservation = %+v, %v", reservation, err)
	}
	logs := logOutput.String()
	if strings.Contains(logs, valid.Secret) || strings.Contains(logs, providerSecret) || !strings.Contains(logs, "[REDACTED]") {
		t.Fatalf("redacting logger leaked a secret or missed provider redaction: %s", logs)
	}

	for _, tc := range []struct {
		name, token string
	}{
		{name: "invalid", token: "prv_not-issued"},
		{name: "disabled", token: disabled.Secret},
		{name: "revoked", token: revoked.Secret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeExec, beforeSupport, beforeSeen := spy.snapshot()
			beforeLogs := logOutput.Len()
			w := post("Bearer " + tc.token)
			if w.Code != http.StatusUnauthorized || !bytes.Contains(w.Body.Bytes(), []byte(`"code":"invalid_api_key"`)) {
				t.Fatalf("%s response = %d %s", tc.name, w.Code, w.Body.String())
			}
			afterExec, afterSupport, afterSeen := spy.snapshot()
			if afterExec != beforeExec || afterSupport != beforeSupport || len(afterSeen) != len(beforeSeen) {
				t.Fatalf("rejected key dispatched/called support: executes %d->%d support %d->%d captures %d->%d", beforeExec, afterExec, beforeSupport, afterSupport, len(beforeSeen), len(afterSeen))
			}
			if logOutput.Len() != beforeLogs || strings.Contains(logOutput.String(), tc.token) {
				t.Fatalf("rejected key logged or changed dispatch logs: %s", logOutput.String()[beforeLogs:])
			}
		})
	}
	beforeExec, beforeSupport, beforeSeen := spy.snapshot()
	beforeLogs := logOutput.Len()
	if w := post("Bearer first", "Bearer second"); w.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate Authorization status = %d, body %s", w.Code, w.Body.String())
	}
	afterExec, afterSupport, afterSeen := spy.snapshot()
	if afterExec != beforeExec || afterSupport != beforeSupport || len(afterSeen) != len(beforeSeen) || logOutput.Len() != beforeLogs {
		t.Fatalf("duplicate Authorization reached dispatch/support/logging")
	}

	beforeExec, beforeSupport, beforeSeen = spy.snapshot()
	if _, err := policies.Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{
		Enabled: true, Models: []string{}, Connectors: []string{"connector"}, RPM: 10, TPM: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	deniedByPolicy, err := keys.Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision + 1})
	if err != nil {
		t.Fatal(err)
	}
	if w := post("Bearer " + deniedByPolicy.Secret); w.Code != http.StatusForbidden || !bytes.Contains(w.Body.Bytes(), []byte(`"code":"permission_denied"`)) {
		t.Fatalf("model policy denial = %d %s", w.Code, w.Body.String())
	}
	if gotExec, gotSupport, gotSeen := spy.snapshot(); gotExec != beforeExec || gotSupport != beforeSupport || len(gotSeen) != len(beforeSeen) {
		t.Fatal("model policy denial reached connector or support operation")
	}

	if _, err := accounts.SetEnabled(ctx, "account", false); err != nil {
		t.Fatal(err)
	}
	if w := post("Bearer " + valid.Secret); w.Code != http.StatusForbidden || !bytes.Contains(w.Body.Bytes(), []byte(`"code":"permission_denied"`)) {
		t.Fatalf("disabled account denial = %d %s", w.Code, w.Body.String())
	}
	if gotExec, gotSupport, gotSeen := spy.snapshot(); gotExec != beforeExec || gotSupport != beforeSupport || len(gotSeen) != len(beforeSeen) {
		t.Fatal("disabled account denial reached connector or support operation")
	}
}
