package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/crypto"
)

func authSessionFixture(t *testing.T, path string) (*sql.DB, crypto.MasterKey, *AuthSessions, *Credentials) {
	t.Helper()
	db, key := credentialFixture(t, path)
	if _, err := db.Exec(`INSERT INTO accounts(id,connector,created_at,updated_at) VALUES ('account-2','test',1,1)`); err != nil {
		t.Fatal(err)
	}
	return db, key, NewAuthSessions(db), NewCredentials(db)
}

func TestAuthSessionInteractiveEncryptionExpiryAndConsume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.db")
	db, key, repo, _ := authSessionFixture(t, path)
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000).UTC()
	expiry := now.Add(time.Minute)
	state := []byte("synthetic-pkce-verifier-private")
	created, err := repo.CreateInteractiveSession(ctx, "session", "account", "test", 1, expiry, state, key, "key-v1", now)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(created.Ciphertext, state) || len(created.Ciphertext) == 0 {
		t.Fatal("session state was not encrypted")
	}
	if _, _, err := repo.GetInteractiveSessionDecrypted(ctx, "session", key, expiry); !errors.Is(err, ErrAuthSessionUnavailable) {
		t.Fatalf("expiry boundary error=%v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo = NewAuthSessions(db)
	got, plain, err := repo.GetInteractiveSessionDecrypted(ctx, "session", key, now)
	if err != nil || got.AccountID != "account" || !bytes.Equal(plain, state) {
		t.Fatalf("decrypted state %q, session ID %q account %q, err %v", plain, got.ID, got.AccountID, err)
	}
	if _, err := db.Exec(`UPDATE auth_sessions SET account_id='account-2' WHERE id='session'`); err != nil {
		t.Fatal(err)
	}
	if plain, err := crypto.Open(key, crypto.Envelope{FormatVersion: created.FormatVersion, KeyVersion: created.KeyVersion, Nonce: created.Nonce, Ciphertext: created.Ciphertext}, "auth_sessions", "session", "account-2"); err == nil || plain != nil {
		t.Fatal("cross-account AAD accepted")
	}
	if _, err := db.Exec(`UPDATE auth_sessions SET account_id='account' WHERE id='session'`); err != nil {
		t.Fatal(err)
	}
	if err := repo.ConsumeInteractiveSession(ctx, "session", "account", now); err != nil {
		t.Fatal(err)
	}
	if _, plain, err := repo.GetInteractiveSessionDecrypted(ctx, "session", key, now); !errors.Is(err, ErrAuthSessionUnavailable) || plain != nil {
		t.Fatalf("consumed state returned %q, %v", plain, err)
	}
	var nonce, ciphertext any
	if err := db.QueryRow(`SELECT nonce,ciphertext FROM auth_sessions WHERE id='session'`).Scan(&nonce, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if nonce != nil || ciphertext != nil {
		t.Fatal("consumed session retained envelope")
	}
	for _, name := range []string{path, path + "-wal"} {
		raw, e := os.ReadFile(name)
		if e == nil && bytes.Contains(raw, state) {
			t.Fatalf("plaintext state in %s", filepath.Base(name))
		} else if e != nil && !os.IsNotExist(e) {
			t.Fatal(e)
		}
	}
}

func TestAuthSessionSchemaAndRefreshMarkerUniqueness(t *testing.T) {
	db, _, repo, _ := authSessionFixture(t, filepath.Join(t.TempDir(), "auth.db"))
	ctx := context.Background()
	now := time.Now().UTC()
	if version, err := SchemaVersion(ctx, db); err != nil || version != 5 {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	if _, err := db.Exec(`INSERT INTO auth_sessions(id,account_id,connector,kind,expected_credential_revision,lifecycle,created_at,updated_at) VALUES ('bad','account','test','interactive',1,'active',1,1)`); err == nil {
		t.Fatal("accepted interactive row without envelope/expiry")
	}
	if err := repo.CreateRefreshMarker(ctx, "refresh-1", "account", "test", 1, now); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []string{"refresh-2", "refresh-3"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			errs <- repo.CreateRefreshMarker(ctx, id, "account-2", "test", 1, now)
		}(id)
	}
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		} else if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
			t.Fatalf("concurrent marker creation error=%v", err)
		}
	}
	if success != 1 {
		t.Fatalf("concurrent markers succeeded=%d, want exactly one", success)
	}
	if err := repo.QuarantineRefreshMarker(ctx, "refresh-1", "account", "ambiguous_result", 1, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateRefreshMarker(ctx, "refresh-2", "account", "test", 1, now); err == nil {
		t.Fatal("created second quarantined refresh marker")
	}
}

func TestAuthSessionAtomicCredentialResolutionAndStaleCAS(t *testing.T) {
	db, key, sessions, credentials := authSessionFixture(t, filepath.Join(t.TempDir(), "auth.db"))
	ctx := context.Background()
	now := time.Now().UTC()
	base, err := credentials.Create(ctx, sealCredential(t, key, "credential", "account", []byte("old-token")))
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.CreateRefreshMarker(ctx, "refresh", "account", "test", base.Revision, now); err != nil {
		t.Fatal(err)
	}
	replacement := sealCredential(t, key, base.ID, base.AccountID, []byte("new-token"))
	replacement.Revision = base.Revision
	updated, err := sessions.ResolveRefreshAndReplaceCredentials(ctx, "refresh", replacement, now)
	if err != nil || updated.Revision != 2 {
		t.Fatalf("resolve %#v %v", updated, err)
	}
	if _, err := credentials.GetDecrypted(ctx, "account", base.ID, key); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_sessions WHERE id='refresh'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("resolved marker count=%d err=%v", count, err)
	}
	if err := sessions.CreateRefreshMarker(ctx, "stale", "account", "test", 1, now); err != nil {
		t.Fatal(err)
	}
	stale := sealCredential(t, key, base.ID, base.AccountID, []byte("must-not-write"))
	stale.Revision = 1
	if _, err := sessions.ResolveRefreshAndReplaceCredentials(ctx, "stale", stale, now); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("stale resolution error=%v", err)
	}
	got, err := credentials.Get(ctx, "account", base.ID)
	if err != nil || got.Revision != 2 {
		t.Fatalf("credential after stale CAS %#v %v", got, err)
	}
	var lifecycle, reason string
	var current int64
	if err := db.QueryRow(`SELECT lifecycle,quarantine_reason,current_credential_revision FROM auth_sessions WHERE id='stale'`).Scan(&lifecycle, &reason, &current); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "uncertain" || reason != "ambiguous_result" || current != 2 {
		t.Fatalf("stale marker state %q %q %d", lifecycle, reason, current)
	}
}

func TestAuthSessionRecoveryAndExpiryCleanup(t *testing.T) {
	db, key, repo, _ := authSessionFixture(t, filepath.Join(t.TempDir(), "auth.db"))
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := repo.CreateInteractiveSession(ctx, "expired", "account", "test", 1, now.Add(-time.Second), []byte("expired-secret"), key, "key-v1", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateRefreshMarker(ctx, "refresh", "account", "test", 1, now); err != nil {
		t.Fatal(err)
	}
	if n, err := repo.CleanupExpired(ctx, now); err != nil || n != 1 {
		t.Fatalf("cleanup %d %v", n, err)
	}
	if n, err := repo.RecoverAuthSessions(ctx, now); err != nil || n != 1 {
		t.Fatalf("recovery %d %v", n, err)
	}
	if n, err := repo.CleanupExpired(ctx, now.Add(365*24*time.Hour)); err != nil || n != 0 {
		t.Fatalf("cleanup quarantined %d %v", n, err)
	}
	var lifecycle, reason string
	var ciphertext any
	if err := db.QueryRow(`SELECT lifecycle,quarantine_reason FROM auth_sessions WHERE id='refresh'`).Scan(&lifecycle, &reason); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "uncertain" || reason != "restart_in_progress" {
		t.Fatalf("recovered state=%q reason=%q", lifecycle, reason)
	}
	if err := db.QueryRow(`SELECT ciphertext FROM auth_sessions WHERE id='expired'`).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if ciphertext != nil {
		t.Fatal("expired interactive ciphertext retained")
	}
}

func TestAuthSessionErrorsDoNotExposeState(t *testing.T) {
	db, key, repo, _ := authSessionFixture(t, filepath.Join(t.TempDir(), "auth.db"))
	now := time.Now().UTC()
	secret := []byte("never-log-this-auth-state")
	if _, err := repo.CreateInteractiveSession(context.Background(), "tamper", "account", "test", 1, now.Add(time.Hour), secret, key, "key-v1", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE auth_sessions SET ciphertext=x'00' WHERE id='tamper'`); err != nil {
		t.Fatal(err)
	}
	_, plain, err := repo.GetInteractiveSessionDecrypted(context.Background(), "tamper", key, now)
	if err == nil || plain != nil || strings.Contains(err.Error(), string(secret)) {
		t.Fatalf("tamper returned %q / %v", plain, err)
	}
}
