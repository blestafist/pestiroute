package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

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
			return core.AuthResult{Supported: true, State: "opaque-state-marker", NextAction: "continue"}
		case "continue":
			if string(req.State) == "opaque-state-marker" {
				return core.AuthResult{Supported: true, State: "opaque-next-marker", NextAction: "continue"}
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
	if session == "" || !strings.Contains(started, "expires_at=") {
		t.Fatalf("start guidance: %q", started)
	}
	advanced, err := run("auth", "continue", "--session", session)
	if err != nil || !strings.Contains(advanced, "session="+session) {
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
