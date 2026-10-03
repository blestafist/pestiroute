package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestServicesReadScopedCredentialsFromSQLite(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sqlite.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	accounts, credentials := sqlite.NewAccounts(db), sqlite.NewCredentials(db)
	if _, err := accounts.Create(context.Background(), sqlite.Account{ID: "account-a", Connector: "connector", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.Create(context.Background(), sqlite.Account{ID: "account-b", Connector: "connector", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyPath, []byte("01234567890123456789012345678901"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	create := func(account, id, value string, expires *time.Time) {
		t.Helper()
		envelope, err := secure.Seal(key, 1, "v1", "credentials", id, account, []byte(value))
		if err != nil {
			t.Fatal(err)
		}
		_, err = credentials.Create(context.Background(), sqlite.Credential{ID: id, AccountID: account, FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext, ExpiresAt: expires})
		if err != nil {
			t.Fatal(err)
		}
	}
	create("account-a", "secret-a", "persisted-a", nil)
	create("account-b", "secret-b", "persisted-b", nil)
	expired := time.Now().Add(-time.Second)
	create("account-a", "expired", "expired-value", &expired)
	provider := core.NewPersistentServices(sqliteAccountReader{accounts}, sqliteCredentialReader{credentials, key}, map[string]map[string]string{
		"account-a": {"bearer": "secret-a", "old": "expired"}, "account-b": {"bearer": "secret-b"},
	}, nil, nil)
	servicesA := provider.ForAttempt(core.AttemptScope{AccountID: "account-a"})
	value, err := servicesA.Credentials.Get(context.Background(), "bearer")
	if err != nil || string(value) != "persisted-a" {
		t.Fatalf("account A credential = %q, %v", value, err)
	}
	if _, err := servicesA.Credentials.Get(context.Background(), "secret-b"); !errors.Is(err, core.ErrCredentialUnavailable) {
		t.Fatalf("cross-account reference = %v", err)
	}
	if _, err := servicesA.Credentials.Get(context.Background(), "old"); !errors.Is(err, core.ErrCredentialUnavailable) {
		t.Fatalf("expired reference = %v", err)
	}
	if _, err := accounts.SetEnabled(context.Background(), "account-a", false); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ForAttempt(core.AttemptScope{AccountID: "account-a"}).Credentials.Get(context.Background(), "bearer"); !errors.Is(err, core.ErrAccountUnavailable) {
		t.Fatalf("disabled account = %v", err)
	}
	if _, err := accounts.SetEnabled(context.Background(), "account-a", true); err != nil {
		t.Fatal(err)
	}
	wrongKeyPath := filepath.Join(dir, "wrong.key")
	if err := os.WriteFile(wrongKeyPath, []byte("abcdefghijklmnopqrstuvwxyzABCDEF"), 0600); err != nil {
		t.Fatal(err)
	}
	wrongKey, err := secure.LoadMasterKey(wrongKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	wrongKeyServices := core.NewPersistentServices(sqliteAccountReader{accounts}, sqliteCredentialReader{credentials, wrongKey}, map[string]map[string]string{"account-a": {"bearer": "secret-a"}}, nil, nil)
	if _, err := wrongKeyServices.ForAttempt(core.AttemptScope{AccountID: "account-a"}).Credentials.Get(context.Background(), "bearer"); !errors.Is(err, core.ErrCredentialUnavailable) {
		t.Fatalf("wrong master key error = %v", err)
	}
	corrupt, err := credentials.Get(context.Background(), "account-a", "secret-a")
	if err != nil {
		t.Fatal(err)
	}
	corrupt.Ciphertext[0] ^= 0xff
	if _, err := credentials.Update(context.Background(), corrupt); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ForAttempt(core.AttemptScope{AccountID: "account-a"}).Credentials.Get(context.Background(), "bearer"); !errors.Is(err, core.ErrCredentialUnavailable) {
		t.Fatalf("corrupted envelope error = %v", err)
	}
	replacement, err := secure.Seal(key, 1, "v1", "credentials", "secret-a", "account-a", []byte("persisted-next"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := credentials.Get(context.Background(), "account-a", "secret-a")
	if err != nil {
		t.Fatal(err)
	}
	row.FormatVersion, row.KeyVersion, row.Nonce, row.Ciphertext = replacement.FormatVersion, replacement.KeyVersion, replacement.Nonce, replacement.Ciphertext
	if _, err := credentials.Update(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	next, err := provider.ForAttempt(core.AttemptScope{AccountID: "account-a"}).Credentials.Get(context.Background(), "bearer")
	if err != nil || string(next) != "persisted-next" {
		t.Fatalf("replacement credential = %q, %v", next, err)
	}
	if string(value) != "persisted-a" {
		t.Fatalf("active invocation snapshot mutated: %q", value)
	}
}

func TestServicesPersistentConfigUsesStoredCredentialReference(t *testing.T) {
	t.Setenv("STORED_TOKEN_ID", "")
	configuration := map[string]any{
		"listen": "127.0.0.1:8080", "database_path": "/tmp/runtime.db", "master_key_file": "/tmp/master.key",
		"components": []topologyComponent{
			{ID: "adapter", Implementation: "pestiroute.responses.native", Kind: core.ComponentAdapter},
			{ID: "connector", Implementation: "pestiroute.responses.native", Kind: core.ComponentConnector, Endpoint: "http://127.0.0.1:9000/v1/responses", CredentialEnv: "STORED_TOKEN_ID", MaxBodyBytes: 4096, MaxHeaderBytes: 4096, ConnectTimeout: "1s", TLSTimeout: "1s", HeaderTimeout: "1s", IdleTimeout: "1s"},
		},
		"routes": []topologyRoute{{Protocol: responsesProtocol, Mode: core.ModeNative, Model: "model", Account: "account", Adapter: "adapter", Connector: "connector"}},
	}
	path := filepath.Join(t.TempDir(), "gateway.json")
	if err := os.WriteFile(path, mustJSON(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := loadConfig([]string{"-config", path})
	if err != nil {
		t.Fatalf("persistent configuration: %v", err)
	}
	if loaded.DatabasePath != "/tmp/runtime.db" || loaded.Components[1].CredentialEnv != "STORED_TOKEN_ID" {
		t.Fatalf("persistent references lost: %+v", loaded)
	}
}

func TestPersistentCredentialRefsRejectCollisionsIndependentOfRouteOrder(t *testing.T) {
	components := []topologyComponent{
		{ID: "connector-a", Kind: core.ComponentConnector, CredentialEnv: "record-a"},
		{ID: "connector-b", Kind: core.ComponentConnector, CredentialEnv: "record-b"},
	}
	routes := []topologyRoute{
		{Account: "account", Connector: "connector-a"},
		{Account: "account", Connector: "connector-b"},
	}
	for _, order := range [][]topologyRoute{routes, {routes[1], routes[0]}} {
		_, err := configuredCredentialRefs(config{DatabasePath: "runtime.db", Components: components, Routes: order})
		if err == nil || err.Error() != "persistent credential references conflict for one account" {
			t.Fatalf("conflicting route order accepted: %v", err)
		}
	}
	components[1].CredentialEnv = components[0].CredentialEnv
	for _, order := range [][]topologyRoute{routes, {routes[1], routes[0]}} {
		if _, err := configuredCredentialRefs(config{DatabasePath: "runtime.db", Components: components, Routes: order}); err != nil {
			t.Fatalf("matching credential references rejected: %v", err)
		}
	}
	if _, err := configuredCredentialRefs(config{Components: []topologyComponent{
		{ID: "connector-a", Kind: core.ComponentConnector, CredentialEnv: "record-a"},
		{ID: "connector-b", Kind: core.ComponentConnector, CredentialEnv: "record-b"},
	}, Routes: routes}); err != nil {
		t.Fatalf("non-persistent topology changed: %v", err)
	}
}

func TestPersistentCredentialConflictFailsBeforeDatabaseOrComponentSetup(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint("reverse=", reverse), func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "must-not-open.db")
			routes := []topologyRoute{{Account: "account", Connector: "connector-a"}, {Account: "account", Connector: "connector-b"}}
			if reverse {
				routes[0], routes[1] = routes[1], routes[0]
			}
			configured := config{DatabasePath: dbPath, MasterKeyFile: filepath.Join(dir, "missing.key"), Components: []topologyComponent{
				{ID: "connector-a", Kind: core.ComponentConnector, CredentialEnv: "record-a"},
				{ID: "connector-b", Kind: core.ComponentConnector, CredentialEnv: "record-b"},
			}, Routes: routes}
			constructed := false
			_, _, err := composeHandlerWithFactory(configured, &atomic.Bool{}, &atomic.Bool{}, nil, func(topologyComponent) core.Component {
				constructed = true
				return nil
			})
			if err == nil || err.Error() != "persistent credential references conflict for one account" {
				t.Fatalf("error = %v", err)
			}
			if constructed {
				t.Fatal("component setup ran before reference validation")
			}
			if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("database side effect: %v", err)
			}
		})
	}
}
