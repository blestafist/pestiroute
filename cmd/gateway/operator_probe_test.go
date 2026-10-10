package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

type operatorOAuthBundle struct {
	Version      int       `json:"version"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	AccountID    string    `json:"account_id"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// openOperatorProbeDB never initializes the store: mode=ro makes SQLite reject
// writes, and query_only reinforces that invariant on the connection.
func openOperatorProbeDB(path string) (*sql.DB, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("invalid database path")
	}
	u := &url.URL{Scheme: "file", Path: absPath}
	query := u.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "query_only(ON)")
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, errors.New("database open failed")
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, errors.New("database unavailable")
	}
	return db, nil
}

func verifyOperatorCredential(ctx context.Context, db *sql.DB, key crypto.MasterKey, accountID string) error {
	repo := sqlite.NewCredentials(db)
	row, err := repo.Get(ctx, accountID, "oauth")
	if err != nil {
		return errors.New("selected credential unavailable")
	}
	if row.ExpiresAt == nil || !row.ExpiresAt.After(time.Now()) {
		return errors.New("credential expiry is not in the future")
	}
	plain, err := repo.GetDecrypted(ctx, accountID, "oauth", key)
	if err != nil {
		return errors.New("credential decryption failed")
	}
	defer clear(plain)
	var bundle operatorOAuthBundle
	if err := json.Unmarshal(plain, &bundle); err != nil || bundle.Version != 1 || bundle.AccessToken == "" || bundle.AccountID == "" || bundle.ExpiresAt.IsZero() || !bundle.ExpiresAt.After(time.Now()) || bundle.ExpiresAt.UnixMilli() != row.ExpiresAt.UnixMilli() {
		return errors.New("credential bundle validation failed")
	}
	return nil
}

func TestOperatorCredentialProbe(t *testing.T) {
	dbPath, keyPath, accountID := os.Getenv("PESTIROUTE_PROBE_DB"), os.Getenv("PESTIROUTE_PROBE_KEY"), os.Getenv("PESTIROUTE_PROBE_ACCOUNT")
	if dbPath == "" || keyPath == "" || accountID == "" {
		t.Skip("operator credential probe requires PESTIROUTE_PROBE_DB, PESTIROUTE_PROBE_KEY, and PESTIROUTE_PROBE_ACCOUNT")
	}
	key, err := crypto.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal("operator probe key validation failed")
	}
	db, err := openOperatorProbeDB(dbPath)
	if err != nil {
		t.Fatal("operator probe database validation failed")
	}
	defer db.Close()
	if err := verifyOperatorCredential(context.Background(), db, key, accountID); err != nil {
		t.Fatal("operator probe credential checkpoint failed")
	}
}

func TestOperatorCredentialProbeSynthetic(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "probe.db")
	keyPath := filepath.Join(dir, "master.key")
	keyBytes := []byte("01234567890123456789012345678901")
	if err := os.WriteFile(keyPath, keyBytes, 0600); err != nil {
		t.Fatal("synthetic key setup failed")
	}
	key, err := crypto.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal("synthetic key load failed")
	}
	writeDB, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal("synthetic database setup failed")
	}
	if err := sqlite.Migrate(ctx, writeDB); err != nil {
		t.Fatal("synthetic schema setup failed")
	}
	if _, err := writeDB.Exec(`INSERT INTO accounts(id, connector, created_at, updated_at) VALUES ('logical-account', 'synthetic', 1, 1)`); err != nil {
		t.Fatal("synthetic account setup failed")
	}
	expires := time.Now().UTC().Add(time.Hour)
	good, err := json.Marshal(operatorOAuthBundle{Version: 1, AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", AccountID: "synthetic-identity", ExpiresAt: expires})
	if err != nil {
		t.Fatal("synthetic bundle setup failed")
	}
	wrongKeyData, err := crypto.Seal(key, 1, "key-v1", "credentials", "oauth", "logical-account", good)
	if err != nil {
		t.Fatal("synthetic envelope setup failed")
	}
	row := sqlite.Credential{ID: "oauth", AccountID: "logical-account", FormatVersion: wrongKeyData.FormatVersion, KeyVersion: wrongKeyData.KeyVersion, Nonce: wrongKeyData.Nonce, Ciphertext: wrongKeyData.Ciphertext, ExpiresAt: &expires}
	if _, err := sqlite.NewCredentials(writeDB).Create(ctx, row); err != nil {
		t.Fatal("synthetic credential setup failed")
	}
	if err := writeDB.Close(); err != nil {
		t.Fatal("synthetic database close failed")
	}

	check := func(wantValid bool) {
		t.Helper()
		db, err := openOperatorProbeDB(dbPath)
		if err != nil {
			t.Fatal("synthetic read-only open failed")
		}
		defer db.Close()
		err = verifyOperatorCredential(ctx, db, key, "logical-account")
		if (err == nil) != wantValid {
			t.Fatal("synthetic credential checkpoint outcome unexpected")
		}
		if _, err := db.Exec(`UPDATE credentials SET revision = revision + 1`); err == nil {
			t.Fatal("read-only probe database accepted a write")
		}
	}
	check(true)

	wrongBytes := []byte("abcdefghijklmnopqrstuvwxyz123456")
	wrongPath := filepath.Join(dir, "wrong.key")
	if err := os.WriteFile(wrongPath, wrongBytes, 0600); err != nil {
		t.Fatal("wrong-key fixture setup failed")
	}
	wrongKey, err := crypto.LoadMasterKey(wrongPath)
	if err != nil {
		t.Fatal("wrong-key fixture load failed")
	}
	db, err := openOperatorProbeDB(dbPath)
	if err != nil {
		t.Fatal("synthetic read-only open failed")
	}
	if err := verifyOperatorCredential(ctx, db, wrongKey, "logical-account"); err == nil {
		t.Fatal("wrong key passed credential probe")
	}
	db.Close()

	updateFixture := func(payload []byte, expiry time.Time) {
		t.Helper()
		envelope, err := crypto.Seal(key, 1, "key-v1", "credentials", "oauth", "logical-account", payload)
		if err != nil {
			t.Fatal("synthetic envelope setup failed")
		}
		if err := writeFixtureUpdate(dbPath, envelope, expiry); err != nil {
			t.Fatal("synthetic fixture update failed")
		}
	}
	updateFixture([]byte("not-json"), expires)
	check(false)
	mismatch, err := json.Marshal(operatorOAuthBundle{Version: 1, AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", AccountID: "synthetic-identity", ExpiresAt: expires.Add(2 * time.Millisecond)})
	if err != nil {
		t.Fatal("synthetic mismatch bundle setup failed")
	}
	updateFixture(mismatch, expires)
	check(false)
	updateFixture(good, time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond))
	check(false)
}

func writeFixtureUpdate(path string, envelope crypto.Envelope, expiry time.Time) error {
	db, err := sqlite.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`UPDATE credentials SET format_version=?, key_version=?, nonce=?, ciphertext=?, expires_at=? WHERE id='oauth'`, envelope.FormatVersion, envelope.KeyVersion, envelope.Nonce, envelope.Ciphertext, expiry.UnixMilli())
	return err
}
