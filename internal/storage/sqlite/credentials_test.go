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

func credentialFixture(t *testing.T, path string) (*sql.DB, crypto.MasterKey) {
	t.Helper()
	db := openTestDB(t, path)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO accounts(id, connector, created_at, updated_at) VALUES ('account', 'test', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0x5a}, crypto.KeySize), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := crypto.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, key
}

func sealCredential(t *testing.T, key crypto.MasterKey, id, account string, plaintext []byte) Credential {
	t.Helper()
	envelope, err := crypto.Seal(key, 1, "key-v1", "credentials", id, account, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	return Credential{ID: id, AccountID: account, FormatVersion: envelope.FormatVersion,
		KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext}
}

func TestCredentialRoundTripRestartAndImmutability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.db")
	db, key := credentialFixture(t, path)
	repo := NewCredentials(db)
	secret := []byte("synthetic-secret-token-42")
	created, err := repo.Create(context.Background(), sealCredential(t, key, "credential", "account", secret))
	if err != nil || created.Revision != 1 {
		t.Fatalf("Create() = revision %d, err %v", created.Revision, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo = NewCredentials(db)
	got, err := repo.Get(context.Background(), "account", "credential")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := repo.GetDecrypted(context.Background(), "account", "credential", key)
	if err != nil || !bytes.Equal(plain, secret) {
		t.Fatalf("GetDecrypted() = %q, %v", plain, err)
	}
	got.Nonce[0] ^= 0xff
	got.Ciphertext[0] ^= 0xff
	plain[0] ^= 0xff
	again, err := repo.GetDecrypted(context.Background(), "account", "credential", key)
	if err != nil || !bytes.Equal(again, secret) {
		t.Fatalf("mutating returned slices changed persisted credential: %q, %v", again, err)
	}
	wrongKeyPath := filepath.Join(t.TempDir(), "wrong.key")
	if err := os.WriteFile(wrongKeyPath, bytes.Repeat([]byte{0x6b}, crypto.KeySize), 0600); err != nil {
		t.Fatal(err)
	}
	wrongKey, err := crypto.LoadMasterKey(wrongKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if leaked, err := repo.GetDecrypted(context.Background(), "account", "credential", wrongKey); err == nil || leaked != nil || strings.Contains(err.Error(), string(secret)) {
		t.Fatalf("wrong key returned plaintext or unsafe error: %q, %v", leaked, err)
	}

	// Keep the WAL populated, then ensure SQLite's physical files contain no plaintext marker.
	if _, err := db.Exec(`PRAGMA wal_checkpoint(PASSIVE)`); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{path, path + "-wal"} {
		contents, err := os.ReadFile(file)
		if err == nil && bytes.Contains(contents, secret) {
			t.Fatalf("plaintext found in %s", filepath.Base(file))
		}
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

func TestCredentialRevisionCASAndAtomicReplace(t *testing.T) {
	db, key := credentialFixture(t, filepath.Join(t.TempDir(), "credentials.db"))
	repo := NewCredentials(db)
	first, err := repo.Create(context.Background(), sealCredential(t, key, "one", "account", []byte("one-old")))
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.Create(context.Background(), sealCredential(t, key, "two", "account", []byte("two-old")))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	firstReplacement := sealCredential(t, key, first.ID, first.AccountID, []byte("one-new"))
	firstReplacement.Revision = first.Revision
	firstUpdated, err := repo.Update(context.Background(), firstReplacement)
	if err != nil || firstUpdated.Revision != 2 || !firstUpdated.UpdatedAt.After(first.CreatedAt) {
		t.Fatalf("Update() = revision %d updated %v err %v", firstUpdated.Revision, firstUpdated.UpdatedAt, err)
	}
	stale := sealCredential(t, key, first.ID, first.AccountID, []byte("stale"))
	stale.Revision = 1
	if _, err := repo.Update(context.Background(), stale); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("stale Update() error = %v, want ErrRevisionMismatch", err)
	}
	current, _ := repo.Get(context.Background(), "account", "one")
	secondReplacement := sealCredential(t, key, second.ID, second.AccountID, []byte("two-new"))
	secondReplacement.Revision = second.Revision
	staleFirst := sealCredential(t, key, first.ID, first.AccountID, []byte("one-bad"))
	staleFirst.Revision = 1
	if _, err := repo.Replace(context.Background(), []Credential{secondReplacement, staleFirst}); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("Replace() error = %v, want ErrRevisionMismatch", err)
	}
	storedFirst, _ := repo.Get(context.Background(), "account", "one")
	storedSecond, _ := repo.Get(context.Background(), "account", "two")
	if !bytes.Equal(storedFirst.Ciphertext, current.Ciphertext) || !bytes.Equal(storedSecond.Ciphertext, second.Ciphertext) || storedFirst.Revision != 2 || storedSecond.Revision != 1 {
		t.Fatal("failed multi-credential replacement partially committed")
	}
	invalidSecond := secondReplacement.clone()
	invalidSecond.Revision = second.Revision
	invalidSecond.Nonce = nil
	validFirst := sealCredential(t, key, first.ID, first.AccountID, []byte("one-still-atomic"))
	validFirst.Revision = 2
	if _, err := repo.Replace(context.Background(), []Credential{validFirst, invalidSecond}); err == nil {
		t.Fatal("Replace() accepted NULL nonce")
	}
	storedFirst, _ = repo.Get(context.Background(), "account", "one")
	storedSecond, _ = repo.Get(context.Background(), "account", "two")
	if !bytes.Equal(storedFirst.Ciphertext, current.Ciphertext) || !bytes.Equal(storedSecond.Ciphertext, second.Ciphertext) {
		t.Fatal("constraint failure partially committed multi-credential replacement")
	}
	secondReplacement.Revision = second.Revision
	firstReplacement = sealCredential(t, key, first.ID, first.AccountID, []byte("one-final"))
	firstReplacement.Revision = 2
	updated, err := repo.Replace(context.Background(), []Credential{firstReplacement, secondReplacement})
	if err != nil || len(updated) != 2 || updated[0].Revision != 3 || updated[1].Revision != 2 {
		t.Fatalf("Replace() = %#v, %v", updated, err)
	}
}

func TestCredentialConcurrentCASAndForeignKey(t *testing.T) {
	db, key := credentialFixture(t, filepath.Join(t.TempDir(), "credentials.db"))
	repo := NewCredentials(db)
	base, err := repo.Create(context.Background(), sealCredential(t, key, "race", "account", []byte("initial")))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, payload := range []string{"race-a", "race-b"} {
		update := sealCredential(t, key, base.ID, base.AccountID, []byte(payload))
		update.Revision = base.Revision
		wg.Add(1)
		go func(update Credential) {
			defer wg.Done()
			_, err := repo.Update(context.Background(), update)
			results <- err
		}(update)
	}
	wg.Wait()
	close(results)
	succeeded, stale := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrRevisionMismatch) {
			stale++
		} else {
			t.Fatalf("concurrent Update() error = %v", err)
		}
	}
	if succeeded != 1 || stale != 1 {
		t.Fatalf("CAS outcomes success=%d stale=%d, want one each", succeeded, stale)
	}
	missing := sealCredential(t, key, "orphan", "missing-account", []byte("secret"))
	if _, err := repo.Create(context.Background(), missing); err == nil {
		t.Fatal("Create accepted nonexistent account")
	}
}

func TestCredentialAADAndCiphertextTampering(t *testing.T) {
	db, key := credentialFixture(t, filepath.Join(t.TempDir(), "credentials.db"))
	repo := NewCredentials(db)
	if _, err := repo.Create(context.Background(), sealCredential(t, key, "aad", "account", []byte("private"))); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.Get(context.Background(), "account", "aad")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		purpose, id, account string
		version              int
	}{{"other", "aad", "account", 1}, {"credentials", "other", "account", 1}, {"credentials", "aad", "other", 1}, {"credentials", "aad", "account", 2}} {
		if plaintext, err := crypto.Open(key, crypto.Envelope{FormatVersion: tc.version, KeyVersion: stored.KeyVersion,
			Nonce: stored.Nonce, Ciphertext: stored.Ciphertext}, tc.purpose, tc.id, tc.account); err == nil || plaintext != nil {
			t.Errorf("altered AAD tuple returned %q, %v", plaintext, err)
		}
	}
	for _, tc := range []struct{ account, id string }{{"other", "aad"}, {"account", "other"}} {
		if _, err := repo.GetDecrypted(context.Background(), tc.account, tc.id, key); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetDecrypted(%q,%q) error = %v", tc.account, tc.id, err)
		}
	}
	if _, err := db.Exec(`UPDATE credentials SET nonce = x'00' WHERE id = 'aad'`); err != nil {
		t.Fatal(err)
	}
	if plaintext, err := repo.GetDecrypted(context.Background(), "account", "aad", key); err == nil || plaintext != nil {
		t.Fatalf("altered nonce returned %q, %v", plaintext, err)
	}
	if _, err := db.Exec(`UPDATE credentials SET nonce = ?, ciphertext = x'00' WHERE id = 'aad'`, stored.Nonce); err != nil {
		t.Fatal(err)
	}
	if plaintext, err := repo.GetDecrypted(context.Background(), "account", "aad", key); err == nil || plaintext != nil {
		t.Fatalf("altered ciphertext returned %q, %v", plaintext, err)
	}
}

func TestCredentialTimestampsAreMillisecondUTC(t *testing.T) {
	db, key := credentialFixture(t, filepath.Join(t.TempDir(), "credentials.db"))
	repo := NewCredentials(db)
	created, err := repo.Create(context.Background(), sealCredential(t, key, "time", "account", []byte("x")))
	if err != nil || created.CreatedAt.Location() != time.UTC || created.CreatedAt.Nanosecond()%int(time.Millisecond) != 0 {
		t.Fatalf("timestamp = %v, error = %v", created.CreatedAt, err)
	}
}
