package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/connector/antigravity"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

type antigravityAuthDoer struct {
	calls            int
	request          *http.Request
	tokenRequestBody string
	err              error
	onDo             func()
}

func (d *antigravityAuthDoer) Do(req *http.Request) (*http.Response, error) {
	d.calls++
	d.request = req
	if d.onDo != nil {
		d.onDo()
	}
	if d.err != nil {
		return nil, d.err
	}
	if req.URL.Path == "/v1internal:loadCodeAssist" {
		if req.Header.Get("Authorization") != "Bearer SYNTHETIC_LOGIN_ACCESS" {
			return nil, errors.New("discovery did not use the exchanged access token")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"cloudaicompanionProject":{"id":"synthetic-project"}}`)), Request: req}, nil
	}
	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		d.tokenRequestBody = string(body)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"SYNTHETIC_LOGIN_ACCESS","refresh_token":"SYNTHETIC_LOGIN_REFRESH","expires_in":3600,"token_type":"Bearer"}`)), Request: req}, nil
}

func TestAdminAuthAntigravityEncryptedBrowserLifecycleAndExecuteReload(t *testing.T) {
	ctx := context.Background()
	t.Setenv("PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT", "")
	dbPath, keyPath := adminTestFiles(t, t.TempDir())
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const accountID = "antigravity-auth-account"
	if _, err = sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: accountID, Connector: "antigravity", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := secure.Seal(key, 1, "v1", "credentials", "antigravity-oauth", accountID, []byte(`{"access_token":"SYNTHETIC_OLD_ACCESS"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "antigravity-oauth", AccountID: accountID, FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PESTIROUTE_ANTIGRAVITY_CLIENT_ID", "synthetic-gateway-client")
	t.Setenv("PESTIROUTE_ANTIGRAVITY_AUTH_ENDPOINT", "https://auth.synthetic.invalid/authorize")
	t.Setenv("PESTIROUTE_ANTIGRAVITY_TOKEN_ENDPOINT", "https://oauth.synthetic.invalid/token")
	t.Setenv("PESTIROUTE_ANTIGRAVITY_REDIRECT_URI", "http://localhost:8080/callback")
	transport := &antigravityAuthDoer{}
	services := adminAuthServicesFunc(func(core.AttemptScope) core.InvocationServices { return core.InvocationServices{Transport: transport} })
	run := func(input string, args ...string) string {
		t.Helper()
		var out, stderr bytes.Buffer
		all := append([]string{"--db", dbPath, "--master-key", keyPath}, args...)
		if err := runAdmin(adminEnvironment{ctx: ctx, args: all, stdin: strings.NewReader(input), stdout: &out, stderr: &stderr, authServices: services}); err != nil {
			t.Fatalf("run %v: %v (%s)", args, err, stderr.String())
		}
		text := out.String() + stderr.String()
		for _, secret := range []string{"SYNTHETIC_LOGIN_ACCESS", "SYNTHETIC_LOGIN_REFRESH", "synthetic-local-code"} {
			if strings.Contains(text, secret) {
				t.Fatalf("authentication output leaked secret %q", secret)
			}
		}
		return out.String()
	}
	started := run("", "auth", "start", "--account", accountID)
	_, verifyText, found := strings.Cut(started, "verification_uri=\"")
	if !found {
		t.Fatalf("verification URL missing: %q", started)
	}
	verifyText, _, _ = strings.Cut(verifyText, "\"")
	verify, err := url.Parse(verifyText)
	if err != nil {
		t.Fatal(err)
	}
	state := verify.Query().Get("state")
	session := adminSessionID(started)
	if state == "" || session == "" || transport.calls != 0 {
		t.Fatal("start did not remain offline or omit state/session")
	}
	debugStart := run("", "auth", "start", "--account", accountID)
	debugSession := adminSessionID(debugStart)
	const callbackCanary = "http://localhost:8080/callback?code=CODE_CANARY&state=STATE_CANARY&scope=SCOPE_CANARY&authuser=AUTHUSER_CANARY&prompt=PROMPT_CANARY&hd=HD_CANARY&iss=ISS_CANARY&error=ERROR_CANARY&provider=PROVIDER_CANARY"
	var debugOut, debugErr bytes.Buffer
	debugArgs := []string{"--db", dbPath, "--master-key", keyPath, "auth", "--debug", "continue", "--session", debugSession, "--callback-stdin"}
	debugCallErr := runAdmin(adminEnvironment{ctx: ctx, args: debugArgs, stdin: strings.NewReader(callbackCanary + "\n"), stdout: &debugOut, stderr: &debugErr, authServices: services})
	debugText := debugOut.String() + debugErr.String()
	wantClass := fmt.Sprintf("kind=callback_url length=%d", len(callbackCanary))
	if debugCallErr == nil || transport.calls != 0 || !strings.Contains(debugText, wantClass) || !strings.Contains(debugText, "reason=callback_error_param") {
		t.Fatalf("debug callback classification/reason missing: err=%v calls=%d class=%t reason=%t output=%q", debugCallErr, transport.calls, strings.Contains(debugText, wantClass), strings.Contains(debugText, "reason=callback_error_param"), debugText)
	}
	for _, canary := range []string{"CODE_CANARY", "STATE_CANARY", "SCOPE_CANARY", "AUTHUSER_CANARY", "PROMPT_CANARY", "HD_CANARY", "ISS_CANARY", "ERROR_CANARY", "PROVIDER_CANARY", callbackCanary} {
		if strings.Contains(debugText+debugCallErr.Error(), canary) {
			t.Fatalf("debug auth output leaked callback canary %q: %q", canary, debugText)
		}
	}
	issuerStart := run("", "auth", "start", "--account", accountID)
	issuerSession, issuerState := adminAuthSessionAndState(t, issuerStart)
	issuerCallback := "http://localhost:8080/callback?code=ISSUER_CODE_CANARY&state=" + url.QueryEscape(issuerState) + "&iss=ISSUER_VALUE_CANARY"
	var issuerOut, issuerErr bytes.Buffer
	issuerArgs := []string{"--db", dbPath, "--master-key", keyPath, "auth", "--debug", "continue", "--session", issuerSession, "--callback-stdin"}
	issuerCallErr := runAdmin(adminEnvironment{ctx: ctx, args: issuerArgs, stdin: strings.NewReader(issuerCallback + "\n"), stdout: &issuerOut, stderr: &issuerErr, authServices: services})
	issuerText := issuerOut.String() + issuerErr.String() + errorString(issuerCallErr)
	if issuerCallErr == nil || transport.calls != 0 || !strings.Contains(issuerText, "reason=callback_iss_mismatch") {
		t.Fatalf("issuer diagnostic missing: calls=%d output=%q", transport.calls, issuerText)
	}
	for _, canary := range []string{"ISSUER_CODE_CANARY", "ISSUER_VALUE_CANARY", issuerCallback} {
		if strings.Contains(issuerText, canary) {
			t.Fatalf("issuer callback leaked canary %q", canary)
		}
	}
	completed := run("synthetic-local-code\n", "auth", "continue", "--session", session, "--callback-stdin")
	if !strings.Contains(completed, "authentication complete") || transport.calls != 2 {
		t.Fatalf("continue=%q calls=%d", completed, transport.calls)
	}
	var duplicateOutput, duplicateError bytes.Buffer
	duplicateArgs := []string{"--db", dbPath, "--master-key", keyPath, "auth", "continue", "--session", session, "--callback-stdin"}
	if err := runAdmin(adminEnvironment{ctx: ctx, args: duplicateArgs, stdin: strings.NewReader("http://localhost:8080/callback?code=synthetic-local-code&state=" + url.QueryEscape(state) + "\n"), stdout: &duplicateOutput, stderr: &duplicateError, authServices: services}); err == nil || transport.calls != 2 {
		t.Fatalf("consumed continuation replay dispatched: err=%v calls=%d", err, transport.calls)
	}
	form, err := url.ParseQuery(transport.tokenRequestBody)
	if err != nil || form.Get("grant_type") != "authorization_code" || form.Get("client_id") != "synthetic-gateway-client" {
		t.Fatalf("exchange form=%v error=%v", form, err)
	}
	start2 := run("", "auth", "start", "--account", accountID)
	session2, state2 := adminAuthSessionAndState(t, start2)
	transport.err = errors.New("synthetic ambiguous exchange")
	ambiguousArgs := []string{"--db", dbPath, "--master-key", keyPath, "auth", "continue", "--session", session2, "--callback-stdin"}
	callback2 := "http://localhost:8080/callback?code=ambiguous-code&state=" + url.QueryEscape(state2) + "\n"
	var ambiguousOut, ambiguousErr bytes.Buffer
	if err := runAdmin(adminEnvironment{ctx: ctx, args: ambiguousArgs, stdin: strings.NewReader(callback2), stdout: &ambiguousOut, stderr: &ambiguousErr, authServices: services}); err == nil || transport.calls != 3 {
		t.Fatalf("ambiguous exchange result=%v calls=%d", err, transport.calls)
	}
	var replayOut, replayErr bytes.Buffer
	if err := runAdmin(adminEnvironment{ctx: ctx, args: ambiguousArgs, stdin: strings.NewReader(callback2), stdout: &replayOut, stderr: &replayErr, authServices: services}); err == nil || transport.calls != 3 {
		t.Fatalf("ambiguous continuation replay dispatched: err=%v calls=%d", err, transport.calls)
	}
	transport.err = nil
	start3 := run("", "auth", "start", "--account", accountID)
	session3, state3 := adminAuthSessionAndState(t, start3)
	cancelCtx, cancel := context.WithCancel(ctx)
	transport.onDo = cancel
	cancelArgs := []string{"--db", dbPath, "--master-key", keyPath, "auth", "continue", "--session", session3, "--callback-stdin"}
	var cancelOut, cancelErr bytes.Buffer
	if err := runAdmin(adminEnvironment{ctx: cancelCtx, args: cancelArgs, stdin: strings.NewReader("http://localhost:8080/callback?code=cancel-code&state=" + url.QueryEscape(state3) + "\n"), stdout: &cancelOut, stderr: &cancelErr, authServices: services}); err == nil || transport.calls != 4 {
		t.Fatalf("cancelled exchange result=%v calls=%d", err, transport.calls)
	}
	transport.onDo = nil
	cancel()
	verifyDB := mustOpenAdminDB(t, dbPath)
	stored, err := sqlite.NewCredentials(verifyDB).Get(ctx, accountID, "antigravity-oauth")
	if err != nil || stored.Revision != 2 || stored.ExpiresAt == nil {
		t.Fatalf("credential persistence revision=%d err=%v", stored.Revision, err)
	}
	plain, err := secure.Open(key, secure.Envelope{FormatVersion: stored.FormatVersion, KeyVersion: stored.KeyVersion, Nonce: stored.Nonce, Ciphertext: stored.Ciphertext}, "credentials", stored.ID, stored.AccountID)
	if err != nil || !strings.Contains(string(plain), "SYNTHETIC_LOGIN_ACCESS") || !strings.Contains(string(plain), "synthetic-project") || bytes.Contains(stored.Ciphertext, []byte("SYNTHETIC_LOGIN_ACCESS")) {
		t.Fatal("credential was not encrypted and reloadable")
	}
	connector := antigravity.NewConnector()
	if err := connector.Init(ctx, core.ComponentConfig{Data: []byte(`{"profile":"antigravity-v1internal","project":"synthetic-project","endpoint":"https://antigravity.synthetic.invalid"}`)}); err != nil {
		t.Fatal(err)
	}
	execTransport, err := antigravity.NewOfflineTestTransport(antigravity.OfflineTestResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: []byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}}\n\ndata: {\"response\":{\"done\":true}}\n\n")})
	if err != nil {
		t.Fatal(err)
	}
	response, ge := connector.Execute(ctx, core.ExecutionRequest{Payload: core.RawPayload{Protocol: "openai.responses.v1", ContentType: "application/json", Body: []byte(`{"model":"gemini-2.5-pro","input":"synthetic","stream":true}`)}}, core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{Transport: execTransport, Credentials: adminTestCredential{plain}})
	if ge != nil {
		t.Fatalf("execute after decrypt/reload: %v", ge)
	}
	_ = response.Stream.Close()
}

func adminAuthSessionAndState(t *testing.T, output string) (string, string) {
	t.Helper()
	_, uriText, found := strings.Cut(output, "verification_uri=\"")
	if !found {
		t.Fatalf("verification URL missing: %q", output)
	}
	uriText, _, _ = strings.Cut(uriText, "\"")
	verify, err := url.Parse(uriText)
	if err != nil {
		t.Fatal(err)
	}
	session, state := adminSessionID(output), verify.Query().Get("state")
	if session == "" || state == "" {
		t.Fatalf("session/state missing: %q", output)
	}
	return session, state
}

func TestAdminAuthAntigravityDefaultTransportRequiresOptIn(t *testing.T) {
	t.Setenv("PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT", "")
	if useDefaultAdminAuthTransport("antigravity") {
		t.Fatal("Antigravity real transport enabled by default")
	}
	t.Setenv("PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT", "1")
	if !useDefaultAdminAuthTransport("pestiroute.antigravity.v1internal") {
		t.Fatal("explicit Antigravity transport opt-in ignored")
	}
}

func TestAdminAuthAntigravityBudgetCompositionAndNonRefund(t *testing.T) {
	t.Setenv("PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT", "1")
	t.Setenv("PESTIROUTE_ANTIGRAVITY_CLIENT_SECRET", "SYNTHETIC_CLIENT_SECRET")
	dbPath, keyPath := adminTestFiles(t, t.TempDir())
	ctx := context.Background()
	const accountID = "synthetic-budget-auth-account"
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: accountID, Connector: "antigravity", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := secure.Seal(key, 1, "v1", "credentials", "synthetic-oauth", accountID, []byte(`{"access_token":"SYNTHETIC_OLD_ACCESS","refresh_token":"SYNTHETIC_OLD_REFRESH"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "synthetic-oauth", AccountID: accountID, FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	doer := &antigravityAuthDoer{}
	services := adminAuthServicesFunc(func(core.AttemptScope) core.InvocationServices { return core.InvocationServices{Transport: doer} })
	run := func(callCtx context.Context) error {
		var out, stderr bytes.Buffer
		return runAdmin(adminEnvironment{ctx: callCtx, args: []string{"--db", dbPath, "--master-key", keyPath, "auth", "refresh", "--account", accountID}, stdout: &out, stderr: &stderr, authServices: services})
	}
	if err := run(ctx); err != nil || doer.calls != 1 {
		t.Fatalf("refresh err=%v dispatches=%d", err, doer.calls)
	}
	checkConsumed := func(want int) {
		t.Helper()
		db, err := sqlite.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var consumed int
		if err := db.QueryRowContext(ctx, `SELECT consumed FROM operator_request_budgets WHERE account_id=?`, sqlite.AntigravityOperatorBudgetID).Scan(&consumed); err != nil || consumed != want {
			t.Fatalf("persisted auth budget=%d err=%v want=%d", consumed, err, want)
		}
	}
	checkConsumed(1)
	cancelCtx, cancel := context.WithCancel(ctx)
	doer.onDo = cancel
	doer.err = errors.New("synthetic timeout after dispatch")
	if err := run(cancelCtx); err == nil || doer.calls != 2 {
		t.Fatalf("cancelled refresh err=%v dispatches=%d", err, doer.calls)
	}
	cancel()
	doer.onDo, doer.err = nil, nil
	checkConsumed(2)
	db, err = sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE operator_request_budgets SET consumed=ceiling WHERE account_id=?`, sqlite.AntigravityOperatorBudgetID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx); err == nil || doer.calls != 2 {
		t.Fatalf("exhausted refresh err=%v dispatches=%d", err, doer.calls)
	}
	db, err = sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE operator_request_budgets`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx); err == nil || doer.calls != 2 {
		t.Fatalf("store failure refresh err=%v dispatches=%d", err, doer.calls)
	}
}

type adminOnboardDoer struct {
	calls  int
	mode   string
	cancel context.CancelFunc
}

func (d *adminOnboardDoer) Do(req *http.Request) (*http.Response, error) {
	d.calls++
	status, body := http.StatusOK, `{"access_token":"SYNTHETIC_ONBOARD_ACCESS","refresh_token":"SYNTHETIC_ONBOARD_REFRESH","expires_in":3600,"token_type":"Bearer"}`
	switch req.URL.Path {
	case "/v1internal:loadCodeAssist":
		body = `{"allowedTiers":[{"id":"free-tier"}],"currentTier":null,"paidTier":null}`
		switch d.mode {
		case "ineligible":
			body = `{"allowedTiers":[{"id":"free-tier"}],"currentTier":null,"paidTier":{"id":"PAID_ID_CANARY"}}`
		case "candidate-missing":
			body = `{"allowedTiers":[{"id":"UNKNOWN_ID_CANARY"}]}`
		case "ambiguous":
			body = `{"allowedTiers":[{"id":"free-tier"},{"id":"free-tier"}]}`
		case "userprojectrequired":
			body = `{"allowedTiers":[{"id":"free-tier","userDefinedCloudaicompanionProject":true}]}`
		case "terms":
			body = `{"allowedTiers":[{"id":"free-tier","hasAcceptedTos":false}]}`
		case "privacy":
			body = `{"allowedTiers":[{"id":"free-tier","privacyNotice":{"showNotice":true}}]}`
		case "providerineligible":
			body = `{"allowedTiers":[{"id":"free-tier"}],"ineligibleTiers":[{"tierId":"free-tier","reasonCode":"OTHER_CANARY"}]}`
		case "validationrequired":
			body = `{"allowedTiers":[{"id":"free-tier"}],"ineligibleTiers":[{"tierId":"free-tier","reasonCode":"VALIDATION_REQUIRED"}]}`
		case "schema-invalid":
			body = `{"allowedTiers":[{"id":"free-tier","hasAcceptedTos":"BAD_TYPE_CANARY"}]}`
		case "schema-unrelated":
			body = `{"allowedTiers":[{"id":"free-tier"},{"id":"UNKNOWN_SCHEMA_ID_CANARY","isDefault":"BAD_TYPE_CANARY"}]}`
		}
	case "/v1internal:onboardUser":
		if req.Method != http.MethodPost {
			return nil, errors.New("onboarding was not POST")
		}
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			return nil, err
		}
		if _, ok := payload["cloudaicompanionProject"]; ok {
			return nil, errors.New("project hint was not omitted")
		}
		if d.mode == "error" {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("private provider response canary")), Request: req}, nil
		}
		if d.mode == "cancel" && d.cancel != nil {
			d.cancel()
		}
		body = `{"name":"v1internal/operations/admin-synthetic-op","done":false}`
	case "/v1internal/operations/admin-synthetic-op":
		if req.Method != http.MethodGet {
			return nil, errors.New("operation was not GET")
		}
		body = `{"done":true,"response":{"cloudaicompanionProject":{"id":"synthetic-managed-project"}}}`
	default:
		if req.URL.Host == "oauth.synthetic.invalid" {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		}
		return nil, errors.New("unexpected synthetic path")
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func TestAdminAuthOnboardingOptInAndDurableBudget(t *testing.T) {
	t.Setenv("PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT", "1")
	t.Setenv("PESTIROUTE_ANTIGRAVITY_ALLOW_ONBOARD", "")
	t.Setenv("PESTIROUTE_ANTIGRAVITY_CLIENT_ID", "synthetic-onboard-client")
	t.Setenv("PESTIROUTE_ANTIGRAVITY_CLIENT_SECRET", "SYNTHETIC_CLIENT_SECRET")
	t.Setenv("PESTIROUTE_ANTIGRAVITY_TOKEN_ENDPOINT", "https://oauth.synthetic.invalid/token")
	dbPath, keyPath := adminTestFiles(t, t.TempDir())
	ctx := context.Background()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"onboard-happy", "onboard-default", "onboard-ineligible", "onboard-error", "onboard-cancel", "onboard-budget"}
	for _, mode := range []string{"candidate-missing", "ambiguous", "userprojectrequired", "terms", "privacy", "providerineligible", "validationrequired", "schema-invalid", "schema-unrelated"} {
		ids = append(ids, "diag-"+mode)
	}
	for _, id := range ids {
		if _, err = sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: id, Connector: "antigravity", Enabled: true}); err != nil {
			t.Fatal(err)
		}
		sealed, sealErr := secure.Seal(key, 1, "v1", "credentials", id+"-oauth", id, []byte(`{"access_token":"SYNTHETIC_OLD_ACCESS"}`))
		if sealErr != nil {
			t.Fatal(sealErr)
		}
		if _, err = sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: id + "-oauth", AccountID: id, FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	doer := &adminOnboardDoer{}
	services := adminAuthServicesFunc(func(core.AttemptScope) core.InvocationServices { return core.InvocationServices{Transport: doer} })
	runStart := func(account string) string {
		var out, stderr bytes.Buffer
		err := runAdmin(adminEnvironment{ctx: ctx, args: []string{"--db", dbPath, "--master-key", keyPath, "auth", "start", "--account", account}, stdout: &out, stderr: &stderr, authServices: services})
		if err != nil {
			t.Fatalf("start: %v %s", err, stderr.String())
		}
		return out.String()
	}
	runContinue := func(account, extra string, contexts ...context.Context) error {
		callCtx := ctx
		if len(contexts) != 0 {
			callCtx = contexts[0]
		}
		started := runStart(account)
		session := adminSessionID(started)
		var out, stderr bytes.Buffer
		args := []string{"--db", dbPath, "--master-key", keyPath, "auth", "continue", "--session", session, "--callback-stdin"}
		if extra != "" {
			if strings.Contains(extra, "--debug") {
				args = append(args[:5], append([]string{"--debug"}, args[5:]...)...)
			}
			for _, flag := range strings.Fields(extra) {
				if flag != "--debug" {
					args = append(args, flag)
				}
			}
		}
		err := runAdmin(adminEnvironment{ctx: callCtx, args: args, stdin: strings.NewReader("SYNTHETIC_CALLBACK_CODE\n"), stdout: &out, stderr: &stderr, authServices: services})
		text := out.String() + stderr.String() + errorString(err)
		if strings.Contains(extra, "--debug") {
			code := map[string]string{"ineligible": "paid_present", "candidate-missing": "candidate_missing", "ambiguous": "ambiguous", "userprojectrequired": "userprojectrequired", "terms": "terms", "privacy": "privacy", "providerineligible": "providerineligible", "validationrequired": "validationrequired", "schema-invalid": "schema_invalid", "schema-unrelated": "schema_invalid"}[doer.mode]
			if code == "" || !strings.Contains(text, "reason=auth_tier_"+code) {
				t.Fatalf("debug diagnostic missing fixed reason for %q: %q", doer.mode, text)
			}
		}
		for _, canary := range []string{"SYNTHETIC_ONBOARD_ACCESS", "SYNTHETIC_ONBOARD_REFRESH", "admin-synthetic-op", "synthetic-managed-project", "private provider response canary", "SYNTHETIC_CALLBACK_CODE", "PAID_ID_CANARY", "UNKNOWN_ID_CANARY", "OTHER_CANARY", "BAD_TYPE_CANARY", "UNKNOWN_SCHEMA_ID_CANARY"} {
			if strings.Contains(text, canary) {
				t.Fatalf("auth output leaked canary %q", canary)
			}
		}
		return err
	}
	if err = runContinue("onboard-happy", "--allow-onboard"); err != nil {
		t.Fatalf("opt-in onboarding: %v", err)
	}
	if doer.calls != 4 {
		t.Fatalf("successful dispatch count=%d want=4", doer.calls)
	}
	verify := mustOpenAdminDB(t, dbPath)
	var budget int
	if err = verify.QueryRowContext(ctx, `SELECT consumed FROM operator_request_budgets WHERE account_id=?`, sqlite.AntigravityOperatorBudgetID).Scan(&budget); err != nil || budget != 4 {
		t.Fatalf("budget=%d err=%v", budget, err)
	}
	stored, err := sqlite.NewCredentials(verify).Get(ctx, "onboard-happy", "onboard-happy-oauth")
	if err != nil || stored.Revision != 2 {
		t.Fatalf("credential revision=%d err=%v", stored.Revision, err)
	}
	plain, err := secure.Open(key, secure.Envelope{FormatVersion: stored.FormatVersion, KeyVersion: stored.KeyVersion, Nonce: stored.Nonce, Ciphertext: stored.Ciphertext}, "credentials", stored.ID, stored.AccountID)
	if err != nil || !strings.Contains(string(plain), "synthetic-managed-project") {
		t.Fatalf("confirmed project not persisted: %v", err)
	}
	doer.mode = "ineligible"
	before := doer.calls
	if err = runContinue("onboard-ineligible", "--allow-onboard --debug"); err == nil {
		t.Fatal("ineligible tier unexpectedly succeeded")
	}
	if doer.calls-before != 2 {
		t.Fatalf("ineligible dispatches=%d want 2", doer.calls-before)
	}
	if stored, err = sqlite.NewCredentials(verify).Get(ctx, "onboard-ineligible", "onboard-ineligible-oauth"); err != nil || stored.Revision != 1 {
		t.Fatalf("ineligible credential revision=%d err=%v", stored.Revision, err)
	}
	if err = verify.QueryRowContext(ctx, `SELECT consumed FROM operator_request_budgets WHERE account_id=?`, sqlite.AntigravityOperatorBudgetID).Scan(&budget); err != nil || budget != 6 {
		t.Fatalf("ineligible budget=%d err=%v", budget, err)
	}
	for _, mode := range []string{"candidate-missing", "ambiguous", "userprojectrequired", "terms", "privacy", "providerineligible", "validationrequired", "schema-invalid", "schema-unrelated"} {
		doer.mode = mode
		before = doer.calls
		if err = runContinue("diag-"+mode, "--allow-onboard --debug"); err == nil || doer.calls-before != 2 {
			t.Fatalf("%s err=%v requests=%d", mode, err, doer.calls-before)
		}
	}
	if err = verify.QueryRowContext(ctx, `SELECT consumed FROM operator_request_budgets WHERE account_id=?`, sqlite.AntigravityOperatorBudgetID).Scan(&budget); err != nil || budget != 24 {
		t.Fatalf("diagnostic budget=%d err=%v", budget, err)
	}
	for _, scenario := range []struct{ name, mode string }{{"onboard-error", "error"}, {"onboard-cancel", "cancel"}} {
		doer.mode = scenario.mode
		before = doer.calls
		callCtx, cancel := context.WithCancel(ctx)
		if scenario.mode == "cancel" {
			doer.cancel = cancel
		}
		err = runContinue(scenario.name, "--allow-onboard", callCtx)
		cancel()
		doer.cancel = nil
		if err == nil || doer.calls-before != 3 {
			t.Fatalf("%s err=%v calls=%d", scenario.name, err, doer.calls-before)
		}
		stored, getErr := sqlite.NewCredentials(verify).Get(ctx, scenario.name, scenario.name+"-oauth")
		if getErr != nil || stored.Revision != 1 {
			t.Fatalf("%s credential revision=%d err=%v", scenario.name, stored.Revision, getErr)
		}
	}
	if err = verify.QueryRowContext(ctx, `SELECT consumed FROM operator_request_budgets WHERE account_id=?`, sqlite.AntigravityOperatorBudgetID).Scan(&budget); err != nil || budget != 30 {
		t.Fatalf("failure/cancel budget=%d err=%v", budget, err)
	}
	doer.mode = ""
	before = doer.calls
	t.Setenv("PESTIROUTE_ANTIGRAVITY_ALLOW_ONBOARD", "1")
	if err = runContinue("onboard-default", ""); err == nil {
		t.Fatal("default mode unexpectedly onboarded")
	}
	t.Setenv("PESTIROUTE_ANTIGRAVITY_ALLOW_ONBOARD", "")
	if doer.calls-before != 2 {
		t.Fatalf("default mode requests=%d want 2", doer.calls-before)
	}
	stored, err = sqlite.NewCredentials(verify).Get(ctx, "onboard-default", "onboard-default-oauth")
	if err != nil || stored.Revision != 1 {
		t.Fatalf("default mutated credentials revision=%d err=%v", stored.Revision, err)
	}
	if _, err = verify.ExecContext(ctx, `UPDATE operator_request_budgets SET consumed=999 WHERE account_id=?`, sqlite.AntigravityOperatorBudgetID); err != nil {
		t.Fatal(err)
	}
	before = doer.calls
	if err = runContinue("onboard-budget", "--allow-onboard"); err == nil {
		t.Fatal("budget exhaustion unexpectedly succeeded")
	}
	if doer.calls-before != 1 {
		t.Fatalf("budget exhaustion dispatched %d requests after final available reservation", doer.calls-before)
	}
	if err = verify.QueryRowContext(ctx, `SELECT consumed FROM operator_request_budgets WHERE account_id=?`, sqlite.AntigravityOperatorBudgetID).Scan(&budget); err != nil || budget != 1000 {
		t.Fatalf("exhausted budget=%d err=%v", budget, err)
	}
	stored, err = sqlite.NewCredentials(verify).Get(ctx, "onboard-budget", "onboard-budget-oauth")
	if err != nil || stored.Revision != 1 {
		t.Fatalf("budget failure mutated credentials revision=%d err=%v", stored.Revision, err)
	}
	if err = verify.Close(); err != nil {
		t.Fatal(err)
	}
}

type adminTestCredential struct{ value []byte }

func (c adminTestCredential) Get(context.Context, string) ([]byte, error) {
	return append([]byte(nil), c.value...), nil
}

func TestAdminAuthScriptedCLIAndRestart(t *testing.T) {
	ctx := context.Background()
	dbPath, keyPath := adminTestFiles(t, t.TempDir())
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: "acct", Connector: "auth-connector", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	env, err := secure.Seal(key, 1, "v1", "credentials", "primary", "acct", []byte("original-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "primary", AccountID: "acct", FormatVersion: env.FormatVersion, KeyVersion: env.KeyVersion, Nonce: env.Nonce, Ciphertext: env.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	browserRedirect := false
	connector := &integratedAuthConnector{call: func(req core.AuthRequest) core.AuthResult {
		switch req.Action {
		case "start":
			action := &core.AuthUserAction{VerificationURI: "https://auth.example.test/device", UserCode: "ABCD-EFGH", PollInterval: 5 * time.Second}
			state := "opaque-state-marker"
			if browserRedirect {
				action = &core.AuthUserAction{VerificationURI: "https://auth.example.test/authorize"}
				state = "opaque-browser-state-marker"
			}
			return core.AuthResult{Supported: true, State: state, NextAction: "continue", UserAction: action}
		case "continue":
			switch string(req.State) {
			case "opaque-state-marker":
				if req.CallbackInput != "https://callback.example.test/?code=callback-secret-marker" {
					t.Error("callback input missing or changed")
				}
			case "opaque-next-marker", "opaque-browser-state-marker":
				if req.CallbackInput != "" {
					t.Error("legacy empty continuation input changed")
				}
			}
			if string(req.State) == "opaque-state-marker" {
				return core.AuthResult{Supported: true, State: "opaque-next-marker", NextAction: "continue", UserAction: &core.AuthUserAction{VerificationURI: "https://auth.example.test/device", UserCode: "IJKL-MNOP", PollInterval: 2500 * time.Millisecond}}
			}
			return core.AuthResult{Supported: true, Credentials: map[string][]byte{"bearer": []byte("rotated-secret-marker")}}
		case "refresh":
			return core.AuthResult{Supported: true, Credentials: map[string][]byte{"bearer": []byte("refreshed-secret-marker")}}
		}
		return core.AuthResult{}
	}}
	registry := newAdminAuthRegistry(t, "auth-connector", connector)
	runWithInput := func(input string, args ...string) (string, error) {
		var out, stderr bytes.Buffer
		full := append([]string{"--db", dbPath, "--master-key", keyPath}, args...)
		err := runAdmin(adminEnvironment{ctx: ctx, args: full, stdin: strings.NewReader(input), stdout: &out, stderr: &stderr, authRegistry: registry})
		combined := out.String() + stderr.String()
		for _, secret := range []string{"opaque-state-marker", "opaque-next-marker", "original-secret", "rotated-secret-marker", "refreshed-secret-marker", "master-key-marker", "callback-secret-marker", strings.Repeat("9", 32)} {
			if strings.Contains(combined+errorString(err), secret) {
				t.Fatalf("secret leaked: %q", secret)
			}
		}
		return combined, err
	}
	run := func(args ...string) (string, error) {
		return runWithInput("", args...)
	}
	started, err := run("auth", "start", "--account", "acct")
	if err != nil {
		t.Fatal(err)
	}
	var session string
	for _, field := range strings.Fields(started) {
		if strings.HasPrefix(field, "session=") {
			session = strings.TrimPrefix(field, "session=")
		}
	}
	if session == "" || !strings.Contains(started, "expires_at=") || !strings.Contains(started, `verification_uri="https://auth.example.test/device" user_code="ABCD-EFGH" interval_seconds=5`) {
		t.Fatalf("start guidance: %q", started)
	}
	advanced, err := runWithInput("https://callback.example.test/?code=callback-secret-marker\n", "auth", "continue", "--session", session, "--callback-stdin")
	if err != nil || !strings.Contains(advanced, "session="+session) || !strings.Contains(advanced, `user_code="IJKL-MNOP" interval_seconds=2.5`) {
		t.Fatalf("continue=%q err=%v", advanced, err)
	}
	completed, err := run("auth", "continue", "--session", session)
	if err != nil || !strings.Contains(completed, "authentication complete") {
		t.Fatalf("completion=%q err=%v", completed, err)
	}
	if _, err = run("auth", "continue", "--session", session); err == nil {
		t.Fatal("consumed session reused")
	}
	refreshed, err := run("auth", "refresh", "--account", "acct")
	if err != nil || !strings.Contains(refreshed, "revision=3") {
		t.Fatalf("refresh=%q err=%v", refreshed, err)
	}
	browserRedirect = true
	redirect, err := run("auth", "start", "--account", "acct")
	if err != nil || !strings.Contains(redirect, `verification_uri="https://auth.example.test/authorize"`) || strings.Contains(redirect, "user_code") || strings.Contains(redirect, "interval_seconds") {
		t.Fatalf("browser redirect guidance=%q err=%v", redirect, err)
	}

	// Distinct runAdmin invocations reopen SQLite; the continuation state is durable.
	started, err = run("auth", "start", "--account", "acct")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range strings.Fields(started) {
		if strings.HasPrefix(field, "session=") {
			session = strings.TrimPrefix(field, "session=")
		}
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatal(err)
	}
	if _, err := run("auth", "continue", "--session", session); err != nil {
		t.Fatal(err)
	}
}

func TestAdminAuthNativeUnsupportedAndRefreshPersistenceFailure(t *testing.T) {
	ctx := context.Background()
	dbPath, keyPath := adminTestFiles(t, t.TempDir())
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: "acct", Connector: "responses", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	key, _ := secure.LoadMasterKey(keyPath)
	sealed, _ := secure.Seal(key, 1, "v1", "credentials", "primary", "acct", []byte("old-secret-marker"))
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "primary", AccountID: "acct", FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// The ordinary admin entrypoint registers the built-in native connector; Authenticate contacts no network.
	out, err := runAdminAuthWithRegistry(t, dbPath, keyPath, nil, "auth", "start", "--account", "acct")
	if err == nil || !strings.Contains(err.Error(), "unsupported or unavailable") || out != "" {
		t.Fatalf("native unsupported output=%q err=%v", out, err)
	}
	if count := adminAuthSessionCount(t, dbPath); count != 0 {
		t.Fatalf("unsupported auth created %d sessions", count)
	}

	// A failed credential replacement after exchange is classified; its durable marker stays quarantined.
	connector := &integratedAuthConnector{call: func(req core.AuthRequest) core.AuthResult {
		if req.Action == "refresh" {
			return core.AuthResult{Supported: true, Credentials: map[string][]byte{"bearer": []byte("new-secret-marker")}}
		}
		return core.AuthResult{}
	}}
	registry := newAdminAuthRegistry(t, "responses", connector)
	db, err = sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_auth_credential BEFORE UPDATE ON credentials BEGIN SELECT RAISE(ABORT, 'private storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	out, err = runAdminAuthWithRegistry(t, dbPath, keyPath, registry, "auth", "refresh", "--account", "acct")
	if err == nil || !strings.Contains(err.Error(), "authentication persistence failed") || out != "" || strings.Contains(out+err.Error(), "new-secret-marker") {
		t.Fatalf("failed refresh out=%q err=%v", out, err)
	}
	db, err = sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var lifecycle, reason string
	err = db.QueryRow(`SELECT lifecycle, quarantine_reason FROM auth_sessions WHERE kind='refresh'`).Scan(&lifecycle, &reason)
	if err != nil || lifecycle != "uncertain" || reason != "persistence_failed" {
		t.Fatalf("marker lifecycle=%q reason=%q err=%v", lifecycle, reason, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

type diagnosticAdminAuthConnector struct {
	integratedAuthConnector
	failure *core.GatewayError
}

func (c *diagnosticAdminAuthConnector) Authenticate(context.Context, core.AuthRequest, core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	return core.AuthResult{}, c.failure
}

func TestAdminAuthDebugAllowsOnlySanitizedDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, code, original, want string
	}{
		{name: "status", code: "auth_rejected", original: "HTTP status 403", want: "reason=auth_rejected HTTP status 403"},
		{name: "poisoned fields", code: "private-code-marker", original: "private-body-marker", want: "reason=auth_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath, keyPath := adminTestFiles(t, t.TempDir())
			db, err := sqlite.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = sqlite.NewAccounts(db).Create(context.Background(), sqlite.Account{ID: "acct", Connector: "auth-connector", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			key, _ := secure.LoadMasterKey(keyPath)
			sealed, err := secure.Seal(key, 1, "v1", "credentials", "primary", "acct", []byte("credential-marker"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = sqlite.NewCredentials(db).Create(context.Background(), sqlite.Credential{ID: "primary", AccountID: "acct", FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}); err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			failure := &core.GatewayError{Code: tc.code, Message: "private-message-marker", Provider: "private-provider-marker", OriginalError: tc.original + " private-tail-marker"}
			if tc.name == "status" {
				failure.OriginalError = tc.original
			}
			connector := &diagnosticAdminAuthConnector{failure: failure}
			registry := newAdminAuthRegistry(t, "auth-connector", connector)
			out, err := runAdminAuthWithRegistry(t, dbPath, keyPath, registry, "auth", "--debug", "start", "--account", "acct")
			if err == nil || err.Error() != "admin: authentication unsupported or unavailable" || !strings.Contains(out, tc.want) {
				t.Fatalf("debug output=%q err=%v", out, err)
			}
			for _, secret := range []string{"private-code-marker", "private-body-marker", "private-message-marker", "private-provider-marker", "private-tail-marker", "credential-marker"} {
				if strings.Contains(out+errorString(err), secret) {
					t.Fatalf("diagnostic leaked %q: %q", secret, out)
				}
			}
			plain, err := runAdminAuthWithRegistry(t, dbPath, keyPath, registry, "auth", "start", "--account", "acct")
			if err == nil || plain != "" {
				t.Fatalf("default output=%q err=%v", plain, err)
			}
		})
	}
}

func TestAdminAuthCodexConfiguredRegistryUsesLocalRoundTripper(t *testing.T) {
	ctx := context.Background()
	dbPath, keyPath := adminTestFiles(t, t.TempDir())
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: "codex-account", Connector: "codex", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := secure.Seal(key, 1, "v1", "credentials", "oauth", "codex-account", []byte(`{"version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "oauth", AccountID: "codex-account", FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var polls, exchanges, refreshes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			_, _ = fmt.Fprint(w, `{"device_code":"private-device","user_code":"SAFE-CODE","verification_uri":"https://auth.example.test/device","interval":1,"expires_in":300}`)
		case "/api/accounts/deviceauth/token":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = fmt.Fprint(w, `{"authorization_code":"private-code","code_verifier":"private-verifier"}`)
		case "/oauth/token":
			form, _ := url.ParseQuery(readRequestBody(t, r))
			switch form.Get("grant_type") {
			case "authorization_code":
				exchanges++
				_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"id_token":%q,"expires_in":3600,"token_type":"Bearer"}`, codexTestJWT("codex-account"), "private-refresh", codexTestJWT("codex-account"))
			case "refresh_token":
				refreshes++
				_, _ = fmt.Fprintf(w, `{"access_token":%q,"id_token":%q,"expires_in":3600,"token_type":"Bearer"}`, codexTestJWT("codex-account"), codexTestJWT("codex-account"))
			default:
				t.Errorf("unexpected OAuth grant")
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	serverURL, _ := url.Parse(server.URL)
	transport := &http.Client{Transport: adminAuthRoundTripper(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL = new(url.URL)
		*clone.URL = *req.URL
		clone.URL.Scheme, clone.URL.Host = serverURL.Scheme, serverURL.Host
		return server.Client().Transport.RoundTrip(clone)
	})}
	run := func(args ...string) (string, error) {
		var out, stderr bytes.Buffer
		full := append([]string{"--db", dbPath, "--master-key", keyPath}, args...)
		services := adminAuthServicesFunc(func(core.AttemptScope) core.InvocationServices {
			return core.InvocationServices{Transport: transport}
		})
		err := runAdmin(adminEnvironment{ctx: ctx, args: full, stdout: &out, stderr: &stderr, authServices: services})
		combined := out.String() + stderr.String() + errorString(err)
		for _, secret := range []string{"private-device", "private-code", "private-verifier", "private-refresh", "access-token"} {
			if strings.Contains(combined, secret) {
				t.Fatalf("credential/state leaked: %q", secret)
			}
		}
		return out.String() + stderr.String(), err
	}
	started, err := run("auth", "start", "--account", "codex-account")
	if err != nil || !strings.Contains(started, `user_code="SAFE-CODE"`) {
		t.Fatalf("start=%q err=%v", started, err)
	}
	session := adminSessionID(started)
	if session == "" {
		t.Fatalf("missing session: %q", started)
	}
	pending, err := run("auth", "continue", "--session", session)
	if err != nil || !strings.Contains(pending, `user_code="SAFE-CODE"`) {
		t.Fatalf("pending=%q err=%v", pending, err)
	}
	if _, err := run("auth", "continue", "--session", session); err != nil {
		t.Fatal(err)
	}
	completed, err := run("auth", "continue", "--session", session)
	if err != nil || !strings.Contains(completed, "authentication complete") {
		t.Fatalf("exchange=%q err=%v", completed, err)
	}
	if _, err := run("auth", "refresh", "--account", "codex-account"); err != nil {
		t.Fatal(err)
	}
	if polls != 2 || exchanges != 1 || refreshes != 1 {
		t.Fatalf("provider calls polls=%d exchanges=%d refreshes=%d", polls, exchanges, refreshes)
	}
}

func TestAdminAuthClaudeCodeEncryptedLifecycle(t *testing.T) {
	ctx := context.Background()
	dbPath, keyPath := adminTestFiles(t, t.TempDir())
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	accountID := "claude-account"
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: accountID, Connector: claudecodeComponentIDForTest, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	initial := []byte(`{"access_token":"old-access-marker","refresh_token":"old-refresh-marker","expires_at":"2000-01-01T00:00:00Z"}`)
	sealed, err := secure.Seal(key, 1, "v1", "credentials", "claude-oauth", accountID, initial)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "claude-oauth", AccountID: accountID, FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var exchanges, refreshes atomic.Int32
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" || r.Method != http.MethodPost {
			t.Errorf("unexpected fake OAuth request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		form, err := url.ParseQuery(readRequestBody(t, r))
		if err != nil {
			t.Errorf("invalid token request form: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch form.Get("grant_type") {
		case "authorization_code":
			exchanges.Add(1)
			if form.Get("code") != "local-code" || form.Get("code_verifier") == "" || form.Get("redirect_uri") != "http://127.0.0.1:9999/callback" {
				t.Errorf("authorization exchange fields invalid")
			}
			_, _ = fmt.Fprint(w, `{"access_token":"access-after-login-marker","refresh_token":"refresh-after-login-marker","token_type":"Bearer","expires_in":3600}`)
		case "refresh_token":
			refreshes.Add(1)
			if form.Get("refresh_token") != "refresh-after-login-marker" {
				t.Errorf("refresh did not use persisted rotated token")
			}
			_, _ = fmt.Fprint(w, `{"access_token":"access-after-refresh-marker","refresh_token":"refresh-after-refresh-marker","token_type":"Bearer","expires_in":3600}`)
		default:
			t.Errorf("unexpected OAuth grant type")
			http.Error(w, "rejected", http.StatusBadRequest)
		}
	}))
	defer fixture.Close()
	fixtureURL, _ := url.Parse(fixture.URL)
	transport := &http.Client{Transport: adminAuthRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.String() != "https://token.synthetic.invalid/oauth/token" {
			return nil, fmt.Errorf("unexpected OAuth fixture target: %s %s", req.Method, req.URL)
		}
		clone := req.Clone(req.Context())
		clone.URL = new(url.URL)
		*clone.URL = *req.URL
		clone.URL.Scheme, clone.URL.Host = fixtureURL.Scheme, fixtureURL.Host
		return fixture.Client().Transport.RoundTrip(clone)
	}), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	services := adminAuthServicesFunc(func(core.AttemptScope) core.InvocationServices { return core.InvocationServices{Transport: transport} })
	run := func(input string, args ...string) (string, error) {
		var out, stderr bytes.Buffer
		full := append([]string{"--db", dbPath, "--master-key", keyPath}, args...)
		err := runAdmin(adminEnvironment{ctx: ctx, args: full, stdin: strings.NewReader(input), stdout: &out, stderr: &stderr, authServices: services})
		combined := out.String() + stderr.String() + errorString(err)
		for _, secret := range []string{"access-after-login-marker", "refresh-after-login-marker", "access-after-refresh-marker", "refresh-after-refresh-marker", "local-code", "old-access-marker", "old-refresh-marker"} {
			if strings.Contains(combined, secret) {
				t.Fatalf("Claude auth secret leaked: %q", secret)
			}
		}
		return out.String() + stderr.String(), err
	}
	started, err := run("", "auth", "start", "--account", accountID)
	if err != nil || !strings.Contains(started, "https://auth.synthetic.invalid/oauth/authorize?") || !strings.Contains(started, "session=") {
		t.Fatalf("Claude login start=%q err=%v", started, err)
	}
	verify, err := url.Parse(strings.Trim(strings.TrimPrefix(strings.Split(started, "verification_uri=")[1], "\""), "\"\n"))
	if err != nil {
		t.Fatal("invalid authorization URI")
	}
	state := verify.Query().Get("state")
	if state == "" || verify.Query().Get("code_challenge") == "" || verify.Query().Has("code_verifier") {
		t.Fatal("authorization URI missing safe PKCE challenge/state")
	}
	session := adminSessionID(started)
	completed, err := run("http://127.0.0.1:9999/callback?code=local-code&state="+url.QueryEscape(state)+"\n", "auth", "continue", "--session", session, "--callback-stdin")
	if err != nil || !strings.Contains(completed, "authentication complete") {
		t.Fatalf("Claude callback=%q err=%v", completed, err)
	}
	verifyDB := mustOpenAdminDB(t, dbPath)
	credentials := sqlite.NewCredentials(verifyDB)
	stored, err := credentials.Get(ctx, accountID, "claude-oauth")
	if err != nil || stored.Revision != 2 || stored.ExpiresAt == nil || !stored.ExpiresAt.After(time.Now()) {
		t.Fatalf("login credentials not persisted with expiry: revision=%d err=%v", stored.Revision, err)
	}
	plain, err := secure.Open(key, secure.Envelope{FormatVersion: stored.FormatVersion, KeyVersion: stored.KeyVersion, Nonce: stored.Nonce, Ciphertext: stored.Ciphertext}, "credentials", stored.ID, stored.AccountID)
	if err != nil || !strings.Contains(string(plain), "access-after-login-marker") || bytes.Contains(stored.Ciphertext, []byte("access-after-login-marker")) {
		t.Fatal("Claude credentials not stored as encrypted OAuth bundle")
	}
	if _, err := verifyDB.ExecContext(ctx, `UPDATE credentials SET expires_at=? WHERE account_id=? AND id=?`, time.Now().Add(-time.Minute).UnixMilli(), accountID, "claude-oauth"); err != nil {
		t.Fatal(err)
	}
	refreshed, err := run("", "auth", "refresh", "--account", accountID)
	if err != nil || !strings.Contains(refreshed, "authentication refreshed") || exchanges.Load() != 1 || refreshes.Load() != 1 {
		t.Fatalf("Claude refresh=%q exchange=%d refresh=%d err=%v", refreshed, exchanges.Load(), refreshes.Load(), err)
	}
	stored, err = credentials.Get(ctx, accountID, "claude-oauth")
	plain, openErr := secure.Open(key, secure.Envelope{FormatVersion: stored.FormatVersion, KeyVersion: stored.KeyVersion, Nonce: stored.Nonce, Ciphertext: stored.Ciphertext}, "credentials", stored.ID, stored.AccountID)
	if err != nil || openErr != nil || stored.Revision != 3 || !strings.Contains(string(plain), "access-after-refresh-marker") || bytes.Contains(stored.Ciphertext, []byte("access-after-refresh-marker")) {
		t.Fatal("refreshed Claude credentials not atomically encrypted and persisted")
	}
}

func TestAdminAuthClaudeCodeAccountAliases(t *testing.T) {
	dbPath, _ := adminTestFiles(t, t.TempDir())
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	accounts := sqlite.NewAccounts(db)
	for i, connectorID := range []string{"claudecode", claudecodeComponentIDForTest} {
		accountID := fmt.Sprintf("claude-alias-%d", i)
		if _, err := accounts.Create(context.Background(), sqlite.Account{ID: accountID, Connector: connectorID, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		registry, err := defaultAdminAuthRegistry(context.Background(), accounts, db, accountID, false, "", false)
		if err != nil {
			t.Fatalf("connector %q rejected: %v", connectorID, err)
		}
		component, descriptor, ok := registry.Lookup(core.InstanceID(connectorID))
		if !ok || descriptor.ID != claudecodeComponentIDForTest {
			t.Fatalf("connector %q was not initialized as Claude Code: %+v", connectorID, descriptor)
		}
		result, ge := component.(core.Connector).Authenticate(context.Background(), core.AuthRequest{AccountID: accountID, Action: "start"}, core.InvocationServices{})
		_ = registry.Close(context.Background())
		if ge != nil || !result.Supported || result.UserAction == nil || !strings.HasPrefix(result.UserAction.VerificationURI, "https://auth.synthetic.invalid/") {
			t.Fatalf("connector %q OAuth profile not ready: result=%+v error=%v", connectorID, result, ge)
		}
	}
}

func TestAdminAuthClaudeCodeDefaultTransportFailsClosed(t *testing.T) {
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	dbPath, keyPath := adminTestFiles(t, t.TempDir())
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	accountID := "claude-default-transport"
	if _, err := sqlite.NewAccounts(db).Create(context.Background(), sqlite.Account{ID: accountID, Connector: "claudecode", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := secure.Seal(key, 1, "v1", "credentials", "claude-oauth", accountID, []byte(`{"access_token":"old","refresh_token":"refresh"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(db).Create(context.Background(), sqlite.Credential{ID: "claude-oauth", AccountID: accountID, FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	run := func(action, session, callback string) string {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestAdminAuthClaudeCodeProxyProcessHelper$")
		hidden := map[string]bool{
			"PESTIROUTE_CLAUDE_PROXY_HELPER": true, "PESTIROUTE_CLAUDE_PROXY_DB": true,
			"PESTIROUTE_CLAUDE_PROXY_KEY": true, "PESTIROUTE_CLAUDE_PROXY_ACTION": true,
			"PESTIROUTE_CLAUDE_PROXY_SESSION": true, "PESTIROUTE_CLAUDE_PROXY_CALLBACK": true,
			"HTTP_PROXY": true, "http_proxy": true, "HTTPS_PROXY": true, "https_proxy": true,
			"ALL_PROXY": true, "all_proxy": true, "NO_PROXY": true, "no_proxy": true,
		}
		cmd.Env = make([]string, 0, len(os.Environ())+10)
		for _, value := range os.Environ() {
			name, _, _ := strings.Cut(value, "=")
			if !hidden[name] {
				cmd.Env = append(cmd.Env, value)
			}
		}
		cmd.Env = append(cmd.Env,
			"PESTIROUTE_CLAUDE_PROXY_HELPER=1",
			"PESTIROUTE_CLAUDE_PROXY_DB="+dbPath,
			"PESTIROUTE_CLAUDE_PROXY_KEY="+keyPath,
			"PESTIROUTE_CLAUDE_PROXY_ACTION="+action,
			"PESTIROUTE_CLAUDE_PROXY_SESSION="+session,
			"PESTIROUTE_CLAUDE_PROXY_CALLBACK="+callback,
			"HTTP_PROXY="+proxy.URL, "http_proxy="+proxy.URL,
			"HTTPS_PROXY="+proxy.URL, "https_proxy="+proxy.URL,
			"ALL_PROXY="+proxy.URL, "all_proxy="+proxy.URL,
			"NO_PROXY=", "no_proxy=",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("Claude auth helper %s failed: %v: %s", action, err, out)
		}
		return string(out)
	}
	started := run("start", "", "")
	if got := proxyCalls.Load(); got != 0 {
		t.Fatalf("Claude start dispatched %d proxy requests", got)
	}
	_, actionText, found := strings.Cut(started, "verification_uri=")
	verifyText, quoted := strings.CutPrefix(actionText, "\"")
	if !found || !quoted {
		t.Fatalf("verification URI missing: %q", started)
	}
	verifyText, _, _ = strings.Cut(verifyText, "\"")
	verify, err := url.Parse(verifyText)
	if err != nil {
		t.Fatal(err)
	}
	session := adminSessionID(started)
	callback := "http://127.0.0.1:9999/callback?code=offline-only&state=" + url.QueryEscape(verify.Query().Get("state")) + "\n"
	if output := run("continue", session, callback); strings.TrimSpace(output) != "FAIL_CLOSED" {
		t.Fatalf("Claude callback did not fail closed: %q", output)
	}
	if got := proxyCalls.Load(); got != 0 {
		t.Fatalf("default Claude callback dispatched %d proxy requests", got)
	}
	if output := run("refresh", "", ""); strings.TrimSpace(output) != "FAIL_CLOSED" {
		t.Fatalf("Claude refresh did not fail closed: %q", output)
	}
	if got := proxyCalls.Load(); got != 0 {
		t.Fatalf("default Claude refresh dispatched %d proxy requests", got)
	}
}

func TestAdminAuthClaudeCodeProxyProcessHelper(t *testing.T) {
	if os.Getenv("PESTIROUTE_CLAUDE_PROXY_HELPER") != "1" {
		return
	}
	action := os.Getenv("PESTIROUTE_CLAUDE_PROXY_ACTION")
	args := []string{"--db", os.Getenv("PESTIROUTE_CLAUDE_PROXY_DB"), "--master-key", os.Getenv("PESTIROUTE_CLAUDE_PROXY_KEY"), "auth", action}
	switch action {
	case "start", "refresh":
		args = append(args, "--account", "claude-default-transport")
	case "continue":
		args = append(args, "--session", os.Getenv("PESTIROUTE_CLAUDE_PROXY_SESSION"), "--callback-stdin")
	default:
		fmt.Fprintln(os.Stderr, "invalid helper action")
		os.Exit(2)
	}
	var stdout, stderr bytes.Buffer
	err := runAdmin(adminEnvironment{ctx: context.Background(), args: args, stdin: strings.NewReader(os.Getenv("PESTIROUTE_CLAUDE_PROXY_CALLBACK")), stdout: &stdout, stderr: &stderr})
	if action == "start" {
		if err != nil {
			fmt.Fprintln(os.Stderr, "start failed")
			os.Exit(2)
		}
		fmt.Fprint(os.Stdout, stdout.String())
		os.Exit(0)
	}
	if err == nil {
		fmt.Fprintln(os.Stderr, "expected fail-closed auth operation")
		os.Exit(3)
	}
	fmt.Fprintln(os.Stdout, "FAIL_CLOSED")
	os.Exit(0)
}

const claudecodeComponentIDForTest = "pestiroute.claudecode.messages"

func mustOpenAdminDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestAdminAuthConfiguredAccountsFailBeforeTransport(t *testing.T) {
	dbPath, keyPath := adminTestFiles(t, t.TempDir())
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []sqlite.Account{
		{ID: "disabled", Connector: "codex", Enabled: false},
		{ID: "mismatch", Connector: "other", Enabled: true},
	} {
		if _, err := sqlite.NewAccounts(db).Create(context.Background(), account); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	transport := &http.Client{Transport: adminAuthRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected provider call")
	})}
	services := adminAuthServicesFunc(func(core.AttemptScope) core.InvocationServices { return core.InvocationServices{Transport: transport} })
	for _, account := range []string{"missing", "disabled", "mismatch"} {
		var out, stderr bytes.Buffer
		err := runAdmin(adminEnvironment{ctx: context.Background(), args: []string{"--db", dbPath, "--master-key", keyPath, "auth", "start", "--account", account}, stdout: &out, stderr: &stderr, authServices: services})
		if err == nil || out.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("account %q output=%q/%q err=%v", account, out.String(), stderr.String(), err)
		}
	}
	var out, stderr bytes.Buffer
	err = runAdmin(adminEnvironment{ctx: context.Background(), args: []string{"--db", dbPath, "--master-key", keyPath, "auth", "start", "--account", "disabled", "--access-token", "literal-secret"}, stdout: &out, stderr: &stderr, authServices: services})
	if err == nil || calls.Load() != 0 {
		t.Fatalf("secret flag accepted or request sent: calls=%d err=%v", calls.Load(), err)
	}
}

type adminAuthRoundTripper func(*http.Request) (*http.Response, error)

func (f adminAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func readRequestBody(t *testing.T, r *http.Request) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func codexTestJWT(account string) string {
	return "e30." + base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"chatgpt_account_id":%q}`, account)) + ".c2ln"
}

func adminSessionID(output string) string {
	for _, field := range strings.Fields(output) {
		if strings.HasPrefix(field, "session=") {
			return strings.TrimPrefix(field, "session=")
		}
	}
	return ""
}

func TestAdminAuthRefreshCommandDoesNotReportSuccessOnBadDatabase(t *testing.T) {
	dbPath, keyPath := adminTestFiles(t, t.TempDir())
	registry := newAdminAuthRegistry(t, "connector", &integratedAuthConnector{call: func(core.AuthRequest) core.AuthResult { return core.AuthResult{Supported: true} }})
	_, err := runAdminAuthWithRegistry(t, dbPath, keyPath, registry, "auth", "refresh", "--account", "missing")
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected refresh result: %v", err)
	}
}

func TestAdminAuthProcessHelper(t *testing.T) {
	if os.Getenv("PESTIROUTE_AUTH_HELPER") != "1" {
		return
	}
	dbPath, keyPath := os.Getenv("PESTIROUTE_AUTH_DB"), os.Getenv("PESTIROUTE_AUTH_KEY")
	connector := &integratedAuthConnector{call: func(req core.AuthRequest) core.AuthResult {
		if req.Action == "start" {
			return core.AuthResult{Supported: true, State: "restart-safe-state", NextAction: "continue"}
		}
		if req.Action == "continue" && string(req.State) == "restart-safe-state" {
			return core.AuthResult{Supported: true}
		}
		return core.AuthResult{}
	}}
	registry := newAdminAuthRegistry(t, "auth-connector", connector)
	var out, stderr bytes.Buffer
	args := []string{"--db", dbPath, "--master-key", keyPath, "auth", os.Getenv("PESTIROUTE_AUTH_ACTION")}
	if os.Getenv("PESTIROUTE_AUTH_ACTION") == "continue" {
		args = append(args, "--session", os.Getenv("PESTIROUTE_AUTH_SESSION"))
	} else {
		args = append(args, "--account", "acct")
	}
	err := runAdmin(adminEnvironment{ctx: context.Background(), args: args, stdout: &out, stderr: &stderr, authRegistry: registry})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Fprint(os.Stdout, out.String())
	os.Exit(0)
}

func TestAdminAuthContinuationSurvivesProcessRestart(t *testing.T) {
	ctx := context.Background()
	dbPath, keyPath := adminTestFiles(t, t.TempDir())
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: "acct", Connector: "auth-connector", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	key, _ := secure.LoadMasterKey(keyPath)
	sealed, _ := secure.Seal(key, 1, "v1", "credentials", "primary", "acct", []byte("restart-original-secret"))
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "primary", AccountID: "acct", FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	runChild := func(action, session string) string {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestAdminAuthProcessHelper$")
		cmd.Env = append(os.Environ(), "PESTIROUTE_AUTH_HELPER=1", "PESTIROUTE_AUTH_DB="+dbPath, "PESTIROUTE_AUTH_KEY="+keyPath, "PESTIROUTE_AUTH_ACTION="+action, "PESTIROUTE_AUTH_SESSION="+session)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("child %s failed: %v", action, err)
		}
		return string(out)
	}
	started := runChild("start", "")
	var session string
	for _, field := range strings.Fields(started) {
		if strings.HasPrefix(field, "session=") {
			session = strings.TrimPrefix(field, "session=")
		}
	}
	if session == "" {
		t.Fatalf("child start returned no handle: %q", started)
	}
	completed := runChild("continue", session)
	if !strings.Contains(completed, "authentication complete") {
		t.Fatalf("child continue output: %q", completed)
	}
}

func TestAdminAuthRefreshMarkerSerializesCommandCalls(t *testing.T) {
	ctx := context.Background()
	dbPath, keyPath := adminTestFiles(t, t.TempDir())
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: "acct", Connector: "auth-connector", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	key, _ := secure.LoadMasterKey(keyPath)
	sealed, _ := secure.Seal(key, 1, "v1", "credentials", "primary", "acct", []byte("old-token"))
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "primary", AccountID: "acct", FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	connector := &integratedAuthConnector{call: func(req core.AuthRequest) core.AuthResult {
		if req.Action == "refresh" {
			once.Do(func() { close(entered) })
			<-release
			return core.AuthResult{Supported: true, Credentials: map[string][]byte{"bearer": []byte("new-token")}}
		}
		return core.AuthResult{}
	}}
	registry := newAdminAuthRegistry(t, "auth-connector", connector)
	first := make(chan error, 1)
	go func() {
		_, err := runAdminAuthWithRegistry(t, dbPath, keyPath, registry, "auth", "refresh", "--account", "acct")
		first <- err
	}()
	<-entered
	second, err := runAdminAuthWithRegistry(t, dbPath, keyPath, registry, "auth", "refresh", "--account", "acct")
	if err == nil || !strings.Contains(err.Error(), "authentication operation failed") || second != "" {
		t.Fatalf("concurrent refresh out=%q err=%v", second, err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	connector.mu.Lock()
	calls := len(connector.actions)
	connector.mu.Unlock()
	if calls != 1 {
		t.Fatalf("connector refresh calls=%d", calls)
	}
}

func newAdminAuthRegistry(t *testing.T, id string, connector core.Connector) *core.Registry {
	t.Helper()
	registry, err := core.NewRegistry(map[core.ComponentKind]core.APIVersion{core.ComponentConnector: {Major: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(core.InstanceID(id), connector, core.ComponentConnector); err != nil {
		t.Fatal(err)
	}
	if err := registry.Init(context.Background(), core.InstanceID(id), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close(context.Background()) })
	return registry
}

func runAdminAuthWithRegistry(t *testing.T, dbPath, keyPath string, registry *core.Registry, args ...string) (string, error) {
	t.Helper()
	var out, stderr bytes.Buffer
	full := append([]string{"--db", dbPath, "--master-key", keyPath}, args...)
	err := runAdmin(adminEnvironment{ctx: context.Background(), args: full, stdout: &out, stderr: &stderr, authRegistry: registry})
	return out.String() + stderr.String(), err
}

func adminAuthSessionCount(t *testing.T, path string) int {
	t.Helper()
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_sessions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
