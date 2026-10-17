package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

// This operator-only diagnostic exercises explicit administrative refresh, not
// the natural freshness-triggered path in TestOperatorCredentialRefreshLive.
func TestOperatorCredentialExplicitRefreshLive(t *testing.T) {
	if os.Getenv("PESTIROUTE_DIAG_EXPLICIT_REFRESH") != "authorized" {
		t.Skip("explicit live refresh requires operator authorization")
	}
	if os.Getenv("PESTIROUTE_DIAG_REVIEW") != "pass" {
		t.Fatal("independent review required")
	}
	dbPath, keyPath, account := os.Getenv("PESTIROUTE_PROBE_DB"), os.Getenv("PESTIROUTE_PROBE_KEY"), os.Getenv("PESTIROUTE_PROBE_ACCOUNT")
	if dbPath == "" || keyPath == "" || account == "" {
		t.Fatal("operator configuration unavailable")
	}
	key, err := crypto.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal("master-key validation failed")
	}
	ctx := context.Background()
	readBundle := func() (operatorCredentialCheckpoint, operatorOAuthBundle) {
		t.Helper()
		db, err := openOperatorProbeDB(dbPath)
		if err != nil {
			t.Fatal("read-only store unavailable")
		}
		defer db.Close()
		checkpoint, err := verifyOperatorCredential(ctx, db, key, account)
		if err != nil {
			t.Fatal("credential checkpoint failed")
		}
		plain, err := sqlite.NewCredentials(db).GetDecrypted(ctx, account, "oauth", key)
		if err != nil {
			t.Fatal("credential decryption failed")
		}
		defer clear(plain)
		var bundle operatorOAuthBundle
		if json.Unmarshal(plain, &bundle) != nil {
			t.Fatal("credential decoding failed")
		}
		return checkpoint, bundle
	}
	before, old := readBundle()
	// Discard CLI output: only allowlisted observations below reach test logs.
	err = runAdmin(adminEnvironment{ctx: ctx, stdout: io.Discard, stderr: io.Discard, args: []string{
		"--db", dbPath, "--master-key", keyPath, "auth", "refresh", "--account", account,
	}})
	if err != nil {
		t.Fatal("single explicit refresh failed; stop without retry or restore")
	}
	after, current := readBundle()
	revisionOK := after.revision == before.revision+1
	expiryAdvanced := after.expires.After(before.expires)
	expiryOK := after.expires.After(after.checked.Add(time.Minute))
	identityOK := old.AccountID == current.AccountID
	accessChanged := old.AccessToken != current.AccessToken
	refreshChanged := old.RefreshToken != current.RefreshToken
	t.Logf("explicit refresh: revision_before=%d revision_after=%d revision_plus_one=%t expiry_valid=%t expiry_advanced=%t same_account=%t access_changed=%t refresh_rotated=%t restart_decryption=true expires_at_utc=%s natural_due_at_utc=%s", before.revision, after.revision, revisionOK, expiryOK, expiryAdvanced, identityOK, accessChanged, refreshChanged, after.expires.UTC().Format(time.RFC3339Nano), after.expires.UTC().Add(-time.Minute).Format(time.RFC3339Nano))
	if !revisionOK || !expiryOK || !identityOK {
		t.Fatal("explicit refresh persistence verification failed; stop without retry or restore")
	}
}
