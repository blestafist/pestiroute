package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/connector/codex"
	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestOperatorCredentialRefreshLive(t *testing.T) {
	if os.Getenv("PESTIROUTE_DIAG_REFRESH") != "authorized" {
		t.Skip("live refresh requires explicit PESTIROUTE_DIAG_REFRESH=authorized")
	}
	dbPath, keyPath, account := os.Getenv("PESTIROUTE_PROBE_DB"), os.Getenv("PESTIROUTE_PROBE_KEY"), os.Getenv("PESTIROUTE_PROBE_ACCOUNT")
	if dbPath == "" || keyPath == "" || account == "" {
		t.Fatal("live diagnostic configuration unavailable")
	}
	ctx := context.Background()
	readDB, err := openOperatorProbeDB(dbPath)
	if err != nil {
		t.Fatal("read-only preflight failed")
	}
	key, err := crypto.LoadMasterKey(keyPath)
	if err != nil {
		readDB.Close()
		t.Fatal("master-key validation failed")
	}
	before, err := verifyOperatorCredential(ctx, readDB, key, account)
	readDB.Close()
	if err != nil {
		t.Fatal("credential preflight failed")
	}
	const margin = time.Minute
	if before.expires.After(before.checked.Add(margin)) {
		t.Log("diagnostic status: refresh_not_due refreshed_revision_bumped=false expires_advanced=false credential_consistency=true restart_decryption=true usage_success=false")
		return
	}
	if os.Getenv("PESTIROUTE_DIAG_REVIEW") != "pass" {
		t.Fatal("credential is due but independent current-diff review gate has not passed")
	}

	info, err := os.Lstat(dbPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("database path validation failed")
	}
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal("credential store unavailable")
	}
	accounts, credentials, sessions := sqlite.NewAccounts(db), sqlite.NewCredentials(db), sqlite.NewAuthSessions(db)
	connector := codex.NewConnector()
	config, _ := json.Marshal(map[string]string{"model": "gpt-6-luna", "account_id": account, "profile": "codex-responses-http-sse-lite-v1"})
	registry, err := core.NewRegistry(map[core.ComponentKind]core.APIVersion{core.ComponentConnector: {Major: 1}}, nil)
	if err != nil {
		db.Close()
		t.Fatal("refresh runtime initialization failed")
	}
	instance := core.InstanceID("operator-codex")
	if err = registry.Register(instance, connector, core.ComponentConnector); err != nil || registry.Init(ctx, instance, core.ComponentConfig{Data: config}) != nil {
		db.Close()
		t.Fatal("refresh connector initialization failed")
	}
	defer registry.Close(context.Background())
	refs := map[string]map[string]string{account: {"oauth": "oauth"}}
	store, err := newSQLiteAuthCoordinatorStore(accounts, credentials, sessions, key, "v1", refs)
	if err != nil {
		db.Close()
		t.Fatal("refresh store initialization failed")
	}
	services := codexAuthServices(func(core.AttemptScope) core.InvocationServices {
		return core.InvocationServices{Transport: connector.HTTPDoer()}
	})
	coordinator, err := core.NewAuthCoordinator(registry, store, services, time.Minute)
	if err != nil {
		db.Close()
		t.Fatal("refresh coordinator initialization failed")
	}
	old, err := credentials.Get(ctx, account, "oauth")
	if err != nil {
		db.Close()
		t.Fatal("credential revision read failed")
	}
	updated, refreshErr := coordinator.ResolveFreshCredentials(ctx, account, margin)
	if refreshErr != nil {
		current, readErr := credentials.Get(ctx, account, "oauth")
		preserved := readErr == nil && current.Revision == old.Revision && string(current.Ciphertext) == string(old.Ciphertext)
		_ = db.Close()
		checkDB, openErr := openOperatorProbeDB(dbPath)
		var restartOK bool
		if openErr == nil {
			checkpoint, verifyErr := verifyOperatorCredential(ctx, checkDB, key, account)
			restartOK = verifyErr == nil && checkpoint.revision == old.Revision
			_ = checkDB.Close()
		}
		t.Logf("diagnostic status: refreshed_revision_bumped=false expires_advanced=false credential_consistency=%t restart_decryption=%t usage_success=false", preserved, restartOK)
		if !preserved || !restartOK {
			t.Fatal("failed refresh did not preserve stored credentials")
		}
		return
	}
	newRow, err := credentials.Get(ctx, account, "oauth")
	if err != nil {
		db.Close()
		t.Fatal("updated credential read failed")
	}
	newRevision := old.Revision + 1
	expiresAdvanced := newRow.ExpiresAt != nil && old.ExpiresAt != nil && newRow.ExpiresAt.After(*old.ExpiresAt) && updated.ExpiresAt != nil && updated.ExpiresAt.Equal(*newRow.ExpiresAt)
	revisionBumped := newRow.Revision == newRevision && updated.Revision == newRevision
	if err := db.Close(); err != nil {
		t.Fatal("credential store close failed")
	}
	checkDB, err := openOperatorProbeDB(dbPath)
	if err != nil {
		t.Fatal("read-only restart verification failed")
	}
	checkpoint, verifyErr := verifyOperatorCredential(ctx, checkDB, key, account)
	checkDB.Close()
	consistency := verifyErr == nil && checkpoint.revision == newRevision && expiresAdvanced
	t.Logf("diagnostic status: refreshed_revision_bumped=%t expires_advanced=%t credential_consistency=%t restart_decryption=%t usage_success=%t", revisionBumped, expiresAdvanced, consistency, verifyErr == nil, false)
	if !revisionBumped || !expiresAdvanced || !consistency || verifyErr != nil {
		t.Fatal("refresh persistence checkpoint failed")
	}
}
