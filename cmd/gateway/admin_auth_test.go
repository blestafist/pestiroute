package main

import (
	"bytes"
	"context"
	"encoding/base64"
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

	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

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
	connector := &integratedAuthConnector{call: func(req core.AuthRequest) core.AuthResult {
		switch req.Action {
		case "start":
			return core.AuthResult{Supported: true, State: "opaque-state-marker", NextAction: "continue", UserAction: &core.AuthUserAction{VerificationURI: "https://auth.example.test/device", UserCode: "ABCD-EFGH", PollInterval: 5 * time.Second}}
		case "continue":
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
	run := func(args ...string) (string, error) {
		var out, stderr bytes.Buffer
		full := append([]string{"--db", dbPath, "--master-key", keyPath}, args...)
		err := runAdmin(adminEnvironment{ctx: ctx, args: full, stdout: &out, stderr: &stderr, authRegistry: registry})
		combined := out.String() + stderr.String()
		for _, secret := range []string{"opaque-state-marker", "opaque-next-marker", "original-secret", "rotated-secret-marker", "refreshed-secret-marker", "master-key-marker", strings.Repeat("9", 32)} {
			if strings.Contains(combined+errorString(err), secret) {
				t.Fatalf("secret leaked: %q", secret)
			}
		}
		return combined, err
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
	advanced, err := run("auth", "continue", "--session", session)
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
