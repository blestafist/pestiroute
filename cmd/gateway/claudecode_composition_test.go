package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/connector/claudecode"
	responses "github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

type claudeFixtureDoer func(*http.Request) (*http.Response, error)

func (f claudeFixtureDoer) Do(req *http.Request) (*http.Response, error) { return f(req) }

type claudeTestConnector struct {
	*claudecode.Connector
	doer     core.HTTPDoer
	model    string
	account  string
	eligible bool
}

func (c *claudeTestConnector) HTTPDoer() core.HTTPDoer { return c.doer }

func (c *claudeTestConnector) Capabilities(ctx context.Context, scope core.CapabilityScope) core.CapabilityResult {
	result := c.Connector.Capabilities(ctx, scope)
	if c.eligible && scope.Protocol == responsesProtocol && scope.Mode == core.ModeTranslation && scope.Model == c.model && scope.AccountID == c.account {
		result.Values["llm.streaming"] = core.Supported
	}
	return result
}

func TestClaudeCodeDefaultCompositionInitializesButHasNoRuntimeTransport(t *testing.T) {
	const model, accountID, connectorID = "claude-default-model", "claude-default-account", "claude-default"
	ready, draining := atomic.Bool{}, atomic.Bool{}
	ready.Store(true)
	_, _, err := composeHandler(config{
		Components: []topologyComponent{
			{ID: "responses-adapter", Kind: core.ComponentAdapter, Implementation: "pestiroute.responses.native"},
			{ID: connectorID, Kind: core.ComponentConnector, Implementation: "pestiroute.claudecode.messages", MaxBodyBytes: 1 << 20, MaxHeaderBytes: 8192},
		},
		Routes: []topologyRoute{{Protocol: responsesProtocol, Mode: core.ModeTranslation, Model: model, Account: accountID, Adapter: "responses-adapter", Connector: core.InstanceID(connectorID), Budget: core.RouteBudget{UnknownEstimate: "reserve", ConservativeTokens: 100}, RouteID: "claude-default-route"}},
	}, &ready, &draining, nil)
	if err == nil || !strings.Contains(err.Error(), "connector \"claude-default\" has no scoped transport") {
		t.Fatalf("default composition transport error = %v, want closed/no scoped transport", err)
	}
}

func TestClaudeCodeCompositionProtectedAdmissionAndOfflineDispatch(t *testing.T) {
	ctx := context.Background()
	_, dbPath, keyPath := protectedFixture(t)
	const accountID, connectorID, defaultCredentialID, credentialID, model = "claude-account", "pestiroute.claudecode.messages", "oauth", "custom-claude-credential", "claude-fixture-model"
	const unwrappedAccount, unwrappedID, unwrappedModel = "claude-unwrapped-account", "claude-unwrapped", "claude-unwrapped-model"
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: accountID, Connector: connectorID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: unwrappedAccount, Connector: unwrappedID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := secure.Seal(key, 1, "v1", "credentials", defaultCredentialID, accountID, []byte(`{"access_token":"synthetic-old-access","refresh_token":"synthetic-old-refresh","expires_at":"2000-01-01T00:00:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: defaultCredentialID, AccountID: accountID, FormatVersion: initial.FormatVersion, KeyVersion: initial.KeyVersion, Nonce: initial.Nonce, Ciphertext: initial.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	const accessToken = "synthetic-claude-access"
	authFixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
			t.Errorf("unexpected synthetic OAuth request %s %s", r.Method, r.URL)
		}
		_, _ = fmt.Fprint(w, `{"access_token":"synthetic-claude-access","refresh_token":"synthetic-refresh","token_type":"Bearer","expires_in":3600}`)
	}))
	defer authFixture.Close()
	authFixtureURL, _ := url.Parse(authFixture.URL)
	authTransport := adminAuthRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://token.synthetic.invalid/oauth/token" {
			return nil, fmt.Errorf("unexpected synthetic OAuth target %s", req.URL)
		}
		copy := req.Clone(req.Context())
		target := *authFixtureURL
		target.Path, target.RawQuery = req.URL.Path, req.URL.RawQuery
		copy.URL, copy.Host = &target, target.Host
		return authFixture.Client().Transport.RoundTrip(copy)
	})
	services := adminAuthServicesFunc(func(core.AttemptScope) core.InvocationServices {
		return core.InvocationServices{Transport: &http.Client{Transport: authTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	})
	runAuth := func(input string, args ...string) (string, error) {
		var stdout, stderr bytes.Buffer
		full := append([]string{"--db", dbPath, "--master-key", keyPath}, args...)
		err := runAdmin(adminEnvironment{ctx: ctx, args: full, stdin: strings.NewReader(input), stdout: &stdout, stderr: &stderr, authServices: services})
		return stdout.String() + stderr.String(), err
	}
	started, err := runAuth("", "auth", "start", "--account", accountID)
	if err != nil {
		t.Fatalf("start synthetic Claude OAuth: %v", err)
	}
	verificationURI := strings.Trim(strings.TrimPrefix(strings.Split(started, "verification_uri=")[1], `"`), "\"\n")
	verify, err := url.Parse(verificationURI)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := runAuth("http://127.0.0.1:9999/callback?code=local-code&state="+url.QueryEscape(verify.Query().Get("state"))+"\n", "auth", "continue", "--session", adminSessionID(started), "--callback-stdin")
	if err != nil || !strings.Contains(completed, "authentication complete") {
		t.Fatalf("complete synthetic Claude OAuth: %q err=%v", completed, err)
	}
	verifyDB := mustOpenAdminDB(t, dbPath)
	storedCredential, err := sqlite.NewCredentials(verifyDB).Get(ctx, accountID, defaultCredentialID)
	if err != nil || storedCredential.ExpiresAt == nil || !storedCredential.ExpiresAt.After(time.Now()) {
		t.Fatalf("OAuth flow did not persist fresh credentials: err=%v", err)
	}
	plain, err := secure.Open(key, secure.Envelope{FormatVersion: storedCredential.FormatVersion, KeyVersion: storedCredential.KeyVersion, Nonce: storedCredential.Nonce, Ciphertext: storedCredential.Ciphertext}, "credentials", storedCredential.ID, storedCredential.AccountID)
	if err != nil || !strings.Contains(string(plain), accessToken) || strings.Contains(string(storedCredential.Ciphertext), accessToken) {
		t.Fatal("OAuth callback credential was not encrypted in SQLite")
	}
	customEnvelope, err := secure.Seal(key, storedCredential.FormatVersion, storedCredential.KeyVersion, "credentials", credentialID, accountID, plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(verifyDB).Create(ctx, sqlite.Credential{ID: credentialID, AccountID: accountID, FormatVersion: customEnvelope.FormatVersion, KeyVersion: customEnvelope.KeyVersion, Nonce: customEnvelope.Nonce, Ciphertext: customEnvelope.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	storedCredential, err = sqlite.NewCredentials(verifyDB).Get(ctx, accountID, credentialID)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyDB.Close(); err != nil {
		t.Fatal(err)
	}

	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" || r.URL.Query().Get("beta") != "true" || r.Header.Get("Authorization") != "Bearer "+accessToken || r.Header.Get("anthropic-beta") != "synthetic-beta" {
			t.Errorf("unexpected offline request %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		requestBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read synthetic request: %v", err)
			return
		}
		if bytes.Contains(requestBody, []byte("malformed")) {
			_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_delta\"}\n\n")
			return
		}
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture-message\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":2}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"offline reply\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer fixture.Close()
	fixtureURL, _ := url.Parse(fixture.URL)
	fixtureClient := fixture.Client()
	var calls atomic.Int32
	delegate := claudeFixtureDoer(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://api.anthropic.invalid/v1/messages?beta=true" {
			return nil, fmt.Errorf("fixture refused unexpected target %s", req.URL)
		}
		calls.Add(1)
		copy := req.Clone(req.Context())
		target := *fixtureURL
		target.Path, target.RawQuery = req.URL.Path, req.URL.RawQuery
		copy.URL, copy.Host = &target, target.Host
		return fixtureClient.Do(copy)
	})
	transport := claudecode.NewOfflineFakeTransport(delegate)

	config := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
	p := config.protected
	p.Connectors = []protectedConnector{
		{ID: connectorID, Kind: "connector", Implementation: "pestiroute.claudecode.messages", Protocols: []string{responsesProtocol}, Settings: nativeSettings{Model: model, AccountID: accountID, CredentialID: credentialID}},
		{ID: unwrappedID, Kind: "connector", Implementation: "pestiroute.claudecode.messages", Protocols: []string{responsesProtocol}, Settings: nativeSettings{Model: unwrappedModel, AccountID: unwrappedAccount}},
	}
	badConfig := config
	badProtected := *p
	badProtected.Connectors = append([]protectedConnector(nil), p.Connectors...)
	badProtected.Connectors[0].Settings.CredentialID = "  "
	badConfig.protected = &badProtected
	if _, err := prepareProtectedConfig(ctx, badConfig); err == nil || !strings.Contains(err.Error(), "Claude Code credential_id cannot be whitespace") {
		t.Fatalf("whitespace-only credential_id error = %v", err)
	}
	p.Routes = []protectedRoute{
		{ID: "claude-route", Protocol: responsesProtocol, Mode: "translation", Model: model, Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(100)}, Targets: []routeTarget{{Connector: connectorID, Account: accountID}}},
		{ID: "claude-unwrapped-route", Protocol: responsesProtocol, Mode: "translation", Model: unwrappedModel, Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(100)}, Targets: []routeTarget{{Connector: unwrappedID, Account: unwrappedAccount}}},
	}
	db, err = sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := sqlite.NewKeyPolicies(db).Update(ctx, "policy-id-a", 1, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{model, unwrappedModel}, Connectors: []string{connectorID, unwrappedID}, RPM: 10, TPM: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareProtectedConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	for _, component := range prepared.Components {
		if component.ID == core.InstanceID(connectorID) && component.CredentialEnv != credentialID {
			t.Fatalf("configured credential_id mapped to %q, want %q", component.CredentialEnv, credentialID)
		}
		if component.ID == core.InstanceID(unwrappedID) && component.CredentialEnv != "oauth" {
			t.Fatalf("omitted credential_id default=%q, want oauth", component.CredentialEnv)
		}
	}
	credentialRefs, err := configuredCredentialRefs(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if credentialRefs[accountID]["oauth"] != credentialID || credentialRefs[unwrappedAccount]["oauth"] != defaultCredentialID {
		t.Fatalf("credential reference projection custom=%v default=%v", credentialRefs[accountID], credentialRefs[unwrappedAccount])
	}
	keys := sqlite.NewVirtualKeys(prepared.runtimeDB)
	issued, err := keys.Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	wrongPolicy, err := sqlite.NewKeyPolicies(prepared.runtimeDB).Create(ctx, sqlite.CreateKeyPolicyParams{ID: "wrong-connector", Enabled: true, Models: []string{model}, Connectors: []string{"other"}, RPM: 10, TPM: 10000})
	if err != nil {
		t.Fatal(err)
	}
	wrongConnector, err := keys.Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: wrongPolicy.ID, PolicyRevision: wrongPolicy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	disabledPolicies := sqlite.NewKeyPolicies(prepared.runtimeDB)
	disabledPolicy, err := disabledPolicies.Create(ctx, sqlite.CreateKeyPolicyParams{ID: "disabled-claude", Models: []string{model}, Connectors: []string{connectorID}, RPM: 10, TPM: 10000})
	if err != nil {
		t.Fatal(err)
	}
	disabledPolicy, err = disabledPolicies.Update(ctx, disabledPolicy.ID, disabledPolicy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: false, Models: disabledPolicy.Models, Connectors: disabledPolicy.Connectors, RPM: disabledPolicy.RPM, TPM: disabledPolicy.TPM})
	if err != nil {
		t.Fatal(err)
	}
	disabledKey, err := keys.Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: disabledPolicy.ID, PolicyRevision: disabledPolicy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	ratePolicy, err := sqlite.NewKeyPolicies(prepared.runtimeDB).Create(ctx, sqlite.CreateKeyPolicyParams{ID: "rate-limited-claude", Enabled: true, Models: []string{model}, Connectors: []string{connectorID}, RPM: 1, TPM: 10000})
	if err != nil {
		t.Fatal(err)
	}
	rateKey, err := keys.Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: ratePolicy.ID, PolicyRevision: ratePolicy.Revision})
	if err != nil {
		t.Fatal(err)
	}

	ready, draining := atomic.Bool{}, atomic.Bool{}
	ready.Store(true)
	var composed *claudeTestConnector
	var unwrapped *claudeTestConnector
	h, closeComponents, err := composeHandlerWithFactory(prepared, &ready, &draining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		if item.ID == core.InstanceID(unwrappedID) {
			unwrapped = &claudeTestConnector{Connector: claudecode.NewConnector(), doer: &http.Client{}, model: unwrappedModel, account: unwrappedAccount, eligible: true}
			return unwrapped
		}
		if item.ID != core.InstanceID(connectorID) {
			return responses.NewConnector()
		}
		composed = &claudeTestConnector{Connector: claudecode.NewConnector(), doer: transport, model: model, account: accountID}
		return composed
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeComponents != nil {
			_ = closeComponents(context.Background())
		}
		if prepared.runtimeDB != nil {
			_ = prepared.runtimeDB.Close()
		}
	})
	gateway := httptest.NewServer(h)
	defer gateway.Close()
	type gatewayResult struct {
		Code int
		Body string
	}
	request := func(token, body string) gatewayResult {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := gateway.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return gatewayResult{Code: resp.StatusCode, Body: string(responseBody)}
	}
	body := `{"model":"` + model + `","stream":true,"input":"hello","max_output_tokens":32}`

	// The actual Connector stays Unknown; without fixture-only eligibility, Core
	// rejects streaming before credentials or transport dispatch.
	actual := composed.Connector.Capabilities(ctx, core.CapabilityScope{Protocol: responsesProtocol, Mode: core.ModeTranslation, Model: model, AccountID: accountID})
	if actual.Values["llm.streaming"] != core.Unknown {
		t.Fatalf("public Claude capability=%v, want Unknown", actual.Values["llm.streaming"])
	}
	if got := request(issued.Secret, body); got.Code == http.StatusOK || !strings.Contains(got.Body, "unsupported_capability") || calls.Load() != 0 {
		t.Fatalf("unwrapped Claude eligibility status=%d calls=%d body=%s", got.Code, calls.Load(), got.Body)
	}
	if _, ge := composed.Connector.Execute(ctx, core.ExecutionRequest{Payload: core.RawPayload{Protocol: responsesProtocol}}, core.AttemptScope{Mode: core.ModeNative, AccountID: accountID}, core.InvocationServices{}); ge == nil || ge.Code != "unsupported_mode" || calls.Load() != 0 {
		t.Fatalf("native-mode execution error=%v calls=%d", ge, calls.Load())
	}
	if _, ge := composed.Connector.Execute(ctx, core.ExecutionRequest{Payload: core.RawPayload{Protocol: responsesProtocol}}, core.AttemptScope{Mode: core.ModeTranslation, AccountID: accountID}, core.InvocationServices{Transport: &http.Client{}}); ge == nil || ge.Code != "connector_unavailable" || calls.Load() != 0 {
		t.Fatalf("unwrapped runtime transport error=%v calls=%d", ge, calls.Load())
	}
	composed.eligible = true
	if got := composed.Capabilities(ctx, core.CapabilityScope{Protocol: responsesProtocol, Mode: core.ModeTranslation, Model: model, AccountID: accountID}).Values["llm.streaming"]; got != core.Supported {
		t.Fatalf("fixture-scoped streaming eligibility=%v, want Supported", got)
	}
	if got := composed.Capabilities(ctx, core.CapabilityScope{Protocol: responsesProtocol, Mode: core.ModeTranslation, Model: "other-model", AccountID: accountID}).Values["llm.streaming"]; got != core.Unknown {
		t.Fatalf("fixture wrapper leaked streaming eligibility outside its model: %v", got)
	}
	if got := composed.Capabilities(ctx, core.CapabilityScope{Protocol: responsesProtocol, Mode: core.ModeTranslation, Model: model, AccountID: "other-account"}).Values["llm.streaming"]; got != core.Unknown {
		t.Fatalf("fixture wrapper leaked streaming eligibility outside its account: %v", got)
	}
	if got := request("invalid-key", body); got.Code == http.StatusOK || calls.Load() != 0 {
		t.Fatalf("unauthorized key dispatched: status=%d calls=%d", got.Code, calls.Load())
	}
	if got := request(wrongConnector.Secret, body); got.Code == http.StatusOK || calls.Load() != 0 {
		t.Fatalf("connector mismatch dispatched: status=%d calls=%d", got.Code, calls.Load())
	}
	if got := request(disabledKey.Secret, body); got.Code == http.StatusOK || calls.Load() != 0 {
		t.Fatalf("disabled key policy dispatched: status=%d calls=%d", got.Code, calls.Load())
	}
	if got := request(issued.Secret, `{"model":"wrong-model","stream":true,"input":"hello"}`); got.Code == http.StatusOK || calls.Load() != 0 {
		t.Fatalf("model mismatch dispatched: status=%d calls=%d", got.Code, calls.Load())
	}
	result := request(rateKey.Secret, body)
	if result.Code != http.StatusOK || !strings.Contains(result.Body, "offline reply") || !strings.Contains(result.Body, "response.completed") || calls.Load() != 1 {
		t.Fatalf("fixture roundtrip status=%d calls=%d body=%s", result.Code, calls.Load(), result.Body)
	}
	if got := request(rateKey.Secret, body); got.Code == http.StatusOK || calls.Load() != 1 {
		t.Fatalf("RPM limit did not reject before dispatch: status=%d calls=%d", got.Code, calls.Load())
	}
	malformed := request(issued.Secret, strings.Replace(body, "hello", "malformed", 1))
	if malformed.Code != http.StatusBadGateway || calls.Load() != 2 {
		t.Fatalf("malformed initial upstream event returned status=%d calls=%d body=%s; want 502 before any empty 200", malformed.Code, calls.Load(), malformed.Body)
	}
	if unwrapped.Connector.Capabilities(ctx, core.CapabilityScope{Protocol: responsesProtocol, Mode: core.ModeTranslation, Model: unwrappedModel, AccountID: unwrappedAccount}).Values["llm.streaming"] != core.Unknown {
		t.Fatal("unwrapped connector public capability was promoted")
	}
	if got := request(issued.Secret, `{"model":"`+unwrappedModel+`","stream":true,"input":"hello"}`); got.Code == http.StatusOK || !strings.Contains(got.Body, "connector_unavailable") || calls.Load() != 2 {
		t.Fatalf("gateway accepted standard runtime transport: status=%d calls=%d body=%s", got.Code, calls.Load(), got.Body)
	}
	stored, err := sqlite.NewCredentials(prepared.runtimeDB).Get(ctx, accountID, credentialID)
	if err != nil || len(stored.Ciphertext) == 0 || strings.Contains(string(stored.Ciphertext), accessToken) {
		t.Fatalf("credential was not encrypted at rest: err=%v", err)
	}
	// Persistent credential read failures and expiry must fail before transport.
	if _, err := prepared.runtimeDB.ExecContext(ctx, `DELETE FROM credentials WHERE account_id=? AND id=?`, accountID, credentialID); err != nil {
		t.Fatal(err)
	}
	if got := request(issued.Secret, body); got.Code == http.StatusOK || calls.Load() != 2 {
		t.Fatalf("missing credential dispatched: status=%d calls=%d", got.Code, calls.Load())
	}
	if _, err := sqlite.NewCredentials(prepared.runtimeDB).Create(ctx, storedCredential); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-time.Minute)
	stored, err = sqlite.NewCredentials(prepared.runtimeDB).Get(ctx, accountID, credentialID)
	if err != nil {
		t.Fatal(err)
	}
	stored.ExpiresAt = &expired
	if _, err := sqlite.NewCredentials(prepared.runtimeDB).Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if got := request(issued.Secret, body); got.Code == http.StatusOK || calls.Load() != 2 {
		t.Fatalf("expired credential dispatched: status=%d calls=%d", got.Code, calls.Load())
	}
	// Recompose the same protected route with the omitted setting to prove the
	// default oauth record still resolves and executes, not just projects.
	gateway.Close()
	if err := closeComponents(ctx); err != nil {
		t.Fatal(err)
	}
	closeComponents = nil
	if err := prepared.runtimeDB.Close(); err != nil {
		t.Fatal(err)
	}
	prepared.runtimeDB = nil
	defaultConfig := config
	defaultProtected := *p
	defaultProtected.Connectors = append([]protectedConnector(nil), p.Connectors...)
	defaultProtected.Connectors[0].Settings.CredentialID = ""
	defaultConfig.protected = &defaultProtected
	defaultPrepared, err := prepareProtectedConfig(ctx, defaultConfig)
	if err != nil {
		t.Fatal(err)
	}
	var defaultComposed *claudeTestConnector
	defaultHandler, closeDefaultComponents, err := composeHandlerWithFactory(defaultPrepared, &ready, &draining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		if item.ID == core.InstanceID(unwrappedID) {
			return &claudeTestConnector{Connector: claudecode.NewConnector(), doer: &http.Client{}, model: unwrappedModel, account: unwrappedAccount, eligible: true}
		}
		if item.ID != core.InstanceID(connectorID) {
			return responses.NewConnector()
		}
		defaultComposed = &claudeTestConnector{Connector: claudecode.NewConnector(), doer: transport, model: model, account: accountID}
		return defaultComposed
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = closeDefaultComponents(context.Background())
		_ = defaultPrepared.runtimeDB.Close()
	}()
	defaultComposed.eligible = true
	for _, component := range defaultPrepared.Components {
		if component.ID == core.InstanceID(connectorID) && component.CredentialEnv != defaultCredentialID {
			t.Fatalf("empty credential_id mapped to %q, want oauth", component.CredentialEnv)
		}
	}
	gateway = httptest.NewServer(defaultHandler)
	defer gateway.Close()
	defaultResult := request(issued.Secret, body)
	if defaultResult.Code != http.StatusOK || !strings.Contains(defaultResult.Body, "offline reply") || !strings.Contains(defaultResult.Body, "response.completed") || calls.Load() != 3 {
		t.Fatalf("default oauth fixture roundtrip status=%d calls=%d body=%s", defaultResult.Code, calls.Load(), defaultResult.Body)
	}
	requests, err := sqlite.NewLedger(defaultPrepared.runtimeDB).QueryRequests(ctx, sqlite.RequestFilter{Model: model, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var success, failedDispatched int
	var failureReasons []string
	for _, request := range requests {
		for _, attempt := range request.Attempts {
			if attempt.Attempt.State == "succeeded" {
				success++
				usage := attempt.Usage
				if usage == nil || usage.InputTokens == nil || *usage.InputTokens != 2 || usage.OutputTokens == nil || *usage.OutputTokens != 3 {
					t.Fatalf("terminal usage not settled: %+v", usage)
				}
			}
			if attempt.Attempt.State == "failed" && attempt.Attempt.ErrorReason != nil && *attempt.Attempt.ErrorReason == "upstream_read_error" {
				failedDispatched++
			}
			if attempt.Attempt.State == "failed" && attempt.Attempt.ErrorReason != nil {
				failureReasons = append(failureReasons, *attempt.Attempt.ErrorReason)
			}
		}
	}
	if success != 2 {
		t.Fatalf("successful attempt settlements=%d, want custom and default oauth attempts", success)
	}
	if failedDispatched != 1 {
		t.Fatalf("malformed dispatched attempts settled=%d, want one; reasons=%v", failedDispatched, failureReasons)
	}
}
