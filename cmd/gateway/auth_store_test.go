package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

type integratedAuthConnector struct {
	core.Connector
	mu      sync.Mutex
	actions []string
	call    func(core.AuthRequest) core.AuthResult
}

type authServicesFactoryFunc func(core.AttemptScope) core.InvocationServices

func (f authServicesFactoryFunc) ForAttempt(s core.AttemptScope) core.InvocationServices { return f(s) }

func (*integratedAuthConnector) Descriptor() core.Descriptor {
	return core.Descriptor{ID: "auth-test", Kind: core.ComponentConnector, ImplementationVersion: "1", APIVersions: []core.APIVersion{{Major: 1}}, Protocols: []string{"test"}, ConnectorType: "scripted"}
}
func (*integratedAuthConnector) Init(context.Context, core.ComponentConfig) error { return nil }
func (*integratedAuthConnector) Health(context.Context) core.Health {
	return core.Health{State: core.HealthReady}
}
func (*integratedAuthConnector) Close(context.Context) error { return nil }
func (c *integratedAuthConnector) Authenticate(_ context.Context, request core.AuthRequest, _ core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.actions = append(c.actions, request.Action)
	return c.call(request), nil
}

func TestSQLiteAuthCoordinatorStoreEndToEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.db")
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	accounts, credentials, sessions := sqlite.NewAccounts(db), sqlite.NewCredentials(db), sqlite.NewAuthSessions(db)
	if _, err = accounts.Create(ctx, sqlite.Account{ID: "account", Connector: "connector", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "master.key")
	if err = os.WriteFile(keyPath, []byte("01234567890123456789012345678901"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	seal := func(value string) sqlite.Credential {
		t.Helper()
		e, err := secure.Seal(key, 1, "key-v1", "credentials", "credential", "account", []byte(value))
		if err != nil {
			t.Fatal(err)
		}
		return sqlite.Credential{ID: "credential", AccountID: "account", FormatVersion: e.FormatVersion, KeyVersion: e.KeyVersion, Nonce: e.Nonce, Ciphertext: e.Ciphertext}
	}
	if _, err = credentials.Create(ctx, seal("old-token")); err != nil {
		t.Fatal(err)
	}
	store, err := newSQLiteAuthCoordinatorStore(accounts, credentials, sessions, key, "key-v1", map[string]map[string]string{"account": {"bearer": "credential"}})
	if err != nil {
		t.Fatal(err)
	}
	connector := &integratedAuthConnector{call: func(r core.AuthRequest) core.AuthResult {
		if r.Action == "start" {
			return core.AuthResult{Supported: true, State: "private-opaque-state", NextAction: "continue"}
		}
		if string(r.State) == "private-opaque-state" {
			return core.AuthResult{Supported: true, State: "private-next-state", NextAction: "continue"}
		}
		return core.AuthResult{Supported: true, Credentials: map[string][]byte{"bearer": []byte("interactive-next-token")}}
	}}
	registry, err := core.NewRegistry(map[core.ComponentKind]core.APIVersion{core.ComponentConnector: {Major: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Register("connector", connector, core.ComponentConnector); err != nil {
		t.Fatal(err)
	}
	if err = registry.Init(ctx, "connector", core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	coordinator, err := core.NewAuthCoordinator(registry, store, authServicesFactoryFunc(func(core.AttemptScope) core.InvocationServices { return core.InvocationServices{} }), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	session, err := coordinator.Start(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext []byte
	if err = db.QueryRow(`SELECT ciphertext FROM auth_sessions WHERE id=?`, session.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if len(ciphertext) == 0 || strings.Contains(string(ciphertext), "private-opaque-state") {
		t.Fatal("interactive state was not protected at rest")
	}
	advanced, err := coordinator.Continue(ctx, session.ID)
	if err != nil || advanced.ExpiresAt != session.ExpiresAt {
		t.Fatalf("continue advance %#v err=%v", advanced, err)
	}
	if err = db.QueryRow(`SELECT ciphertext FROM auth_sessions WHERE id=?`, session.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), "private-next-state") {
		t.Fatal("advanced opaque state persisted in plaintext")
	}
	if _, err = coordinator.Continue(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.GetAuthSession(ctx, session.ID, time.Now()); !errors.Is(err, core.ErrAuthUnavailable) {
		t.Fatalf("completed session remains usable: %v", err)
	}
	got, err := credentials.GetDecrypted(ctx, "account", "credential", key)
	if err != nil || string(got) != "interactive-next-token" {
		t.Fatalf("credential=%q err=%v", got, err)
	}
	refreshed, err := coordinator.Refresh(ctx, "account")
	if err != nil || refreshed.Revision != 3 || string(refreshed.Values["bearer"]) != "interactive-next-token" {
		t.Fatalf("SQLite refresh result=%#v err=%v", refreshed, err)
	}
	var rowCount int
	if err = db.QueryRow(`SELECT COUNT(*) FROM auth_sessions WHERE account_id='account' AND kind='refresh'`).Scan(&rowCount); err != nil || rowCount != 0 {
		t.Fatalf("resolved refresh markers=%d err=%v", rowCount, err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM auth_sessions WHERE id=? AND lifecycle='consumed' AND ciphertext IS NULL`, session.ID).Scan(&rowCount); err != nil || rowCount != 1 {
		t.Fatalf("consumed session row count=%d err=%v", rowCount, err)
	}
	staleSession, err := coordinator.Start(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	row, err := credentials.Get(ctx, "account", "credential")
	if err != nil {
		t.Fatal(err)
	}
	external, err := secure.Seal(key, 1, "key-v1", "credentials", row.ID, row.AccountID, []byte("external-update"))
	if err != nil {
		t.Fatal(err)
	}
	row.FormatVersion, row.KeyVersion, row.Nonce, row.Ciphertext = external.FormatVersion, external.KeyVersion, external.Nonce, external.Ciphertext
	if _, err = credentials.Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	if _, err = coordinator.Continue(ctx, staleSession.ID); !errors.Is(err, core.ErrAuthRevisionMismatch) {
		t.Fatalf("stale session error=%v", err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM auth_sessions WHERE id=? AND lifecycle='consumed' AND ciphertext IS NULL`, staleSession.ID).Scan(&rowCount); err != nil || rowCount != 1 {
		t.Fatalf("stale session cleanup count=%d err=%v", rowCount, err)
	}
}

func TestSQLiteAuthCoordinatorRefreshCASConflictQuarantineAndInvalidation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	accounts, credentials, sessions := sqlite.NewAccounts(db), sqlite.NewCredentials(db), sqlite.NewAuthSessions(db)
	if _, err = accounts.Create(ctx, sqlite.Account{ID: "account", Connector: "connector", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "master.key")
	if err = os.WriteFile(keyPath, []byte("01234567890123456789012345678901"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	e, err := secure.Seal(key, 1, "key-v1", "credentials", "credential", "account", []byte("old"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = credentials.Create(ctx, sqlite.Credential{ID: "credential", AccountID: "account", FormatVersion: e.FormatVersion, KeyVersion: e.KeyVersion, Nonce: e.Nonce, Ciphertext: e.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	store, err := newSQLiteAuthCoordinatorStore(accounts, credentials, sessions, key, "key-v1", map[string]map[string]string{"account": {"bearer": "credential"}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	marker := core.AuthSession{ID: "marker-one", AccountID: "account", ConnectorID: "connector", Revision: 1}
	if err = store.CreateRefreshMarker(ctx, marker.ID, marker); err != nil {
		t.Fatal(err)
	}
	second := marker
	second.ID = "marker-two"
	if err = store.CreateRefreshMarker(ctx, second.ID, second); !errors.Is(err, core.ErrRefreshMarkerConflict) {
		t.Fatalf("duplicate marker error=%v", err)
	}
	updated, err := store.ResolveRefresh(ctx, marker.ID, marker, core.AuthCredentials{Revision: 1, Values: map[string][]byte{"bearer": []byte("rotated")}})
	if err != nil || updated.Revision != 2 {
		t.Fatalf("explicit resolution %#v %v", updated, err)
	}
	old, err := credentials.GetDecrypted(ctx, "account", "credential", key)
	if err != nil || string(old) != "rotated" {
		t.Fatalf("resolved credential=%q err=%v", old, err)
	}
	reauthMarker := core.AuthSession{ID: "reauth-marker", AccountID: "account", ConnectorID: "connector", Revision: 2}
	if err = store.CreateRefreshMarker(ctx, reauthMarker.ID, reauthMarker); err != nil {
		t.Fatal(err)
	}
	if err = store.QuarantineRefreshMarker(ctx, reauthMarker.ID, reauthMarker, "ambiguous_result", 2); err != nil {
		t.Fatal(err)
	}
	updated, err = store.ReplaceAuthCredentials(ctx, "account", 2, core.AuthCredentials{Revision: 2, Values: map[string][]byte{"bearer": []byte("reauthenticated")}})
	if err != nil || updated.Revision != 3 {
		t.Fatalf("explicit reauthentication %#v err=%v", updated, err)
	}
	if err = sessions.CreateRefreshMarker(ctx, "stale", "account", "connector", 3, now); err != nil {
		t.Fatal(err)
	}
	external, err := secure.Seal(key, 1, "key-v1", "credentials", "credential", "account", []byte("external-wins"))
	if err != nil {
		t.Fatal(err)
	}
	current, err := credentials.Get(ctx, "account", "credential")
	if err != nil {
		t.Fatal(err)
	}
	current.FormatVersion, current.KeyVersion, current.Nonce, current.Ciphertext = external.FormatVersion, external.KeyVersion, external.Nonce, external.Ciphertext
	if _, err = credentials.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	bad, err := secure.Seal(key, 1, "key-v1", "credentials", "credential", "account", []byte("must-not-commit"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = sessions.ResolveRefreshAndReplaceCredentials(ctx, "stale", sqlite.Credential{ID: "credential", AccountID: "account", Revision: 3, FormatVersion: bad.FormatVersion, KeyVersion: bad.KeyVersion, Nonce: bad.Nonce, Ciphertext: bad.Ciphertext}, now)
	if !errors.Is(err, sqlite.ErrRevisionMismatch) {
		t.Fatalf("stale CAS error=%v", err)
	}
	old, err = credentials.GetDecrypted(ctx, "account", "credential", key)
	if err != nil || string(old) != "external-wins" {
		t.Fatalf("stale CAS mutated credential=%q err=%v", old, err)
	}
	var staleLifecycle, staleReason string
	var staleCurrent int64
	if err = db.QueryRow(`SELECT lifecycle,quarantine_reason,current_credential_revision FROM auth_sessions WHERE id='stale'`).Scan(&staleLifecycle, &staleReason, &staleCurrent); err != nil {
		t.Fatal(err)
	}
	if staleLifecycle != "uncertain" || staleReason != "ambiguous_result" || staleCurrent != 4 {
		t.Fatalf("stale marker %q/%q revision=%d", staleLifecycle, staleReason, staleCurrent)
	}
	if _, err = accounts.Create(ctx, sqlite.Account{ID: "invalidate-account", Connector: "connector", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = sessions.CreateInteractiveSession(ctx, "active", "invalidate-account", "connector", 2, now.Add(time.Hour), []byte("state"), key, "key-v1", now); err != nil {
		t.Fatal(err)
	}
	if err = sessions.CreateRefreshMarker(ctx, "in-progress", "invalidate-account", "connector", 2, now); err != nil {
		t.Fatal(err)
	}
	if err = store.InvalidateAuth(ctx, "invalidate-account", "connector", now); err != nil {
		t.Fatal(err)
	}
	var active, uncertain int
	if err = db.QueryRow(`SELECT COUNT(*) FROM auth_sessions WHERE id='active' AND lifecycle='consumed' AND ciphertext IS NULL`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM auth_sessions WHERE id='in-progress' AND lifecycle='uncertain'`).Scan(&uncertain); err != nil {
		t.Fatal(err)
	}
	if active != 1 || uncertain != 1 {
		t.Fatalf("invalidation states active=%d uncertain=%d", active, uncertain)
	}
}

func TestSQLiteAuthCoordinatorPersistenceFailureKeepsOldCredential(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	accounts, credentials, sessions := sqlite.NewAccounts(db), sqlite.NewCredentials(db), sqlite.NewAuthSessions(db)
	if _, err = accounts.Create(ctx, sqlite.Account{ID: "account", Connector: "connector", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "master.key")
	if err = os.WriteFile(keyPath, []byte("01234567890123456789012345678901"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := secure.Seal(key, 1, "key-v1", "credentials", "credential", "account", []byte("old-sensitive-token"))
	if _, err = credentials.Create(ctx, sqlite.Credential{ID: "credential", AccountID: "account", FormatVersion: e.FormatVersion, KeyVersion: e.KeyVersion, Nonce: e.Nonce, Ciphertext: e.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	store, err := newSQLiteAuthCoordinatorStore(accounts, credentials, sessions, key, "key-v1", map[string]map[string]string{"account": {"bearer": "credential"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER fail_auth_credential BEFORE UPDATE ON credentials BEGIN SELECT RAISE(ABORT,'synthetic write failure'); END`); err != nil {
		t.Fatal(err)
	}
	connector := &integratedAuthConnector{call: func(core.AuthRequest) core.AuthResult {
		return core.AuthResult{Supported: true, Credentials: map[string][]byte{"bearer": []byte("new-sensitive-token")}}
	}}
	registry, err := core.NewRegistry(map[core.ComponentKind]core.APIVersion{core.ComponentConnector: {Major: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Register("connector", connector, core.ComponentConnector); err != nil {
		t.Fatal(err)
	}
	if err = registry.Init(ctx, "connector", core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	coordinator, err := core.NewAuthCoordinator(registry, store, authServicesFactoryFunc(func(core.AttemptScope) core.InvocationServices { return core.InvocationServices{} }), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = coordinator.Refresh(ctx, "account"); !errors.Is(err, core.ErrAuthPersistence) || strings.Contains(err.Error(), "new-sensitive-token") {
		t.Fatalf("storage failure error leaked candidate: %v", err)
	}
	plain, err := credentials.GetDecrypted(ctx, "account", "credential", key)
	if err != nil || string(plain) != "old-sensitive-token" {
		t.Fatalf("old credential changed: %q err=%v", plain, err)
	}
	var lifecycle, reason string
	if err = db.QueryRow(`SELECT lifecycle,quarantine_reason FROM auth_sessions WHERE account_id='account' AND kind='refresh'`).Scan(&lifecycle, &reason); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "uncertain" || reason != "persistence_failed" {
		t.Fatalf("marker state %q/%q", lifecycle, reason)
	}
}
