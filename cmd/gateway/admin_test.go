package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestAdminHelpAndInvalidCommandsDoNotOpenFiles(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
		ok   bool
	}{
		{[]string{"--help"}, "usage: gateway admin", true},
		{[]string{"migrate", "--help"}, "migrate: no additional arguments", true},
		{[]string{"--db", "/missing/db", "--master-key", "/missing/key", "status", "--help"}, "status: no additional arguments", true},
		{[]string{"unknown"}, "usage: gateway admin", false},
		{[]string{"--bad=private-secret"}, "usage: gateway admin", false},
	} {
		var out, stderr bytes.Buffer
		err := runAdmin(adminEnvironment{ctx: context.Background(), args: tc.args, stdout: &out, stderr: &stderr})
		if (err == nil) != tc.ok || !strings.Contains(out.String()+stderr.String(), tc.want) {
			t.Fatalf("args %q: err=%v stdout=%q stderr=%q", tc.args, err, out.String(), stderr.String())
		}
		if strings.Contains(out.String()+stderr.String()+errorString(err), "private-secret") {
			t.Fatalf("secret leaked for args %q", tc.args)
		}
	}
}

func TestAdminStatusDoesNotCreateMissingDatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath, keyPath := filepath.Join(dir, "absent.db"), filepath.Join(dir, "absent.key")
	for _, args := range [][]string{
		{"--help"},
		{"unknown"},
		{"--db", dbPath, "migrate"}, // Missing key.
		{"migrate"},                 // Missing both required flags.
	} {
		var out, stderr bytes.Buffer
		_ = runAdmin(adminEnvironment{ctx: context.Background(), args: args, stdout: &out, stderr: &stderr})
		if _, err := os.Lstat(dbPath); !os.IsNotExist(err) {
			t.Fatalf("args %q created/opened database: %v", args, err)
		}
		if _, err := os.Lstat(keyPath); !os.IsNotExist(err) {
			t.Fatalf("args %q created/opened key: %v", args, err)
		}
	}
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{1}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	err := runAdmin(adminEnvironment{ctx: context.Background(), args: []string{"--db", dbPath, "--master-key", keyPath, "status"}, stdout: &out, stderr: &stderr})
	if err == nil || !strings.Contains(err.Error(), "cannot open database") {
		t.Fatalf("status on missing DB: err=%v output=%q", err, out.String()+stderr.String())
	}
	if _, err := os.Lstat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("status created missing database: %v", err)
	}
	backing := filepath.Join(dir, "backing.db")
	if err := os.WriteFile(backing, nil, 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias.db")
	if err := os.Symlink(backing, alias); err != nil {
		t.Fatal(err)
	}
	var symlinkOut, symlinkErr bytes.Buffer
	err = runAdmin(adminEnvironment{ctx: context.Background(), args: []string{"--db", alias, "--master-key", keyPath, "migrate"}, stdout: &symlinkOut, stderr: &symlinkErr})
	if err == nil || !strings.Contains(err.Error(), "cannot open database") {
		t.Fatalf("database symlink accepted: %v", err)
	}
	if info, err := os.Stat(backing); err != nil || info.Size() != 0 {
		t.Fatalf("symlink target changed: info=%v err=%v", info, err)
	}
}

func TestAdminMigrationStatusAndKeyValidation(t *testing.T) {
	dir := t.TempDir()
	dbPath, keyPath := filepath.Join(dir, "state.db"), filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0x5a}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		var out, stderr bytes.Buffer
		err := runAdmin(adminEnvironment{ctx: context.Background(), args: args, stdin: strings.NewReader(""), stdout: &out, stderr: &stderr})
		return out.String() + stderr.String(), err
	}
	if output, err := run("--db", dbPath, "--master-key", keyPath, "migrate"); err != nil || !strings.Contains(output, "schema is current") {
		t.Fatalf("migrate: output=%q err=%v", output, err)
	}
	if info, err := os.Stat(dbPath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("new database mode: info=%v err=%v", info, err)
	}
	if err := os.Chmod(dbPath, 0644); err != nil {
		t.Fatal(err)
	}
	if output, err := run("--db", dbPath, "--master-key", keyPath, "status"); err != nil || !strings.Contains(output, fmt.Sprintf("schema version %d", sqlite.CurrentSchemaVersion())) {
		t.Fatalf("status: output=%q err=%v", output, err)
	}
	if info, err := os.Stat(dbPath); err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("existing database mode changed: info=%v err=%v", info, err)
	}
	if output, err := run("--db", dbPath, "--master-key", keyPath, "migrate"); err != nil || !strings.Contains(output, "schema is current") {
		t.Fatalf("migrate existing DB: output=%q err=%v", output, err)
	}
	if info, err := os.Stat(dbPath); err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("migration changed existing database mode: info=%v err=%v", info, err)
	}
	if err := os.Chmod(keyPath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := run("--db", dbPath, "--master-key", keyPath, "status"); err == nil || !strings.Contains(err.Error(), "invalid master key") {
		t.Fatalf("permissive key accepted: %v", err)
	}
}

func TestAdminMasterKeyValidationLayers(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "must-not-exist.db")
	valid := filepath.Join(dir, "valid.key")
	if err := os.WriteFile(valid, bytes.Repeat([]byte{0x4c}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	short := filepath.Join(dir, "short.key")
	if err := os.WriteFile(short, bytes.Repeat([]byte{0x5d}, 31), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.key")
	if err := os.Symlink(valid, link); err != nil {
		t.Fatal(err)
	}
	for _, keyPath := range []string{filepath.Join(dir, "missing.key"), link, short} {
		var out, stderr bytes.Buffer
		err := runAdmin(adminEnvironment{ctx: context.Background(), args: []string{"--db", dbPath, "--master-key", keyPath, "migrate"}, stdout: &out, stderr: &stderr})
		if err == nil || !strings.Contains(err.Error(), "invalid master key") {
			t.Fatalf("key %q accepted or misclassified: %v", keyPath, err)
		}
		if strings.Contains(out.String()+stderr.String()+err.Error(), keyPath) {
			t.Fatalf("key path leaked: %q", keyPath)
		}
		if _, err := os.Lstat(dbPath); !os.IsNotExist(err) {
			t.Fatalf("bad key created DB: %v", err)
		}
	}
}

func TestAdminFutureSchemaFailsClosed(t *testing.T) {
	dir := t.TempDir()
	dbPath, keyPath := filepath.Join(dir, "state.db"), filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0x6b}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := func() (string, error) {
		var out, stderr bytes.Buffer
		err := runAdmin(adminEnvironment{ctx: context.Background(), args: []string{"--db", dbPath, "--master-key", keyPath, "migrate"}, stdout: &out, stderr: &stderr})
		return out.String() + stderr.String(), err
	}()
	if err != nil || !strings.Contains(output, "schema is current") {
		t.Fatalf("initial migration: output=%q err=%v", output, err)
	}
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (?, 'future')`, sqlite.CurrentSchemaVersion()+1); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	err = runAdmin(adminEnvironment{ctx: context.Background(), args: []string{"--db", dbPath, "--master-key", keyPath, "status"}, stdout: &out, stderr: &stderr})
	if err == nil || !strings.Contains(err.Error(), "unsupported or unreadable schema") {
		t.Fatalf("future schema accepted: output=%q err=%v", out.String(), err)
	}
}

func TestAdminAccountCredentialRequireCurrentSchema(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0x6b}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		setup func(string) error
		want  string
	}{
		{"future", func(path string) error {
			if err := adminMigrateFile(path, keyPath); err != nil {
				return err
			}
			db, err := sqlite.Open(path)
			if err != nil {
				return err
			}
			defer db.Close()
			_, err = db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (?, 'future')`, sqlite.CurrentSchemaVersion()+1)
			return err
		}, "unsupported or unreadable schema"},
		{"unmigrated", func(path string) error {
			db, err := sqlite.Open(path)
			if err != nil {
				return err
			}
			return db.Close()
		}, "schema is not fully migrated"},
		{"partial", func(path string) error {
			db, err := sqlite.Open(path)
			if err != nil {
				return err
			}
			defer db.Close()
			_, err = db.Exec(`CREATE TABLE accounts (id TEXT PRIMARY KEY)`)
			return err
		}, "schema is not fully migrated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "state.db")
			if err := tc.setup(dbPath); err != nil {
				t.Fatal(err)
			}
			before := adminTableNames(t, dbPath)
			for _, op := range [][]string{
				{"account", "create", "--id", "no-write", "--connector", "responses"},
				{"credential", "create", "--account", "acct", "--id", "no-write"},
			} {
				input := &countingReader{data: []byte("must-not-be-read")}
				_, err := runAdminCaptured(dbPath, keyPath, input, op...)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("%v: expected %q gate, got %v", op, tc.want, err)
				}
				if input.reads != 0 {
					t.Fatalf("%v consumed stdin %d times", op, input.reads)
				}
			}
			if got := adminTableNames(t, dbPath); !equalStrings(got, before) {
				t.Fatalf("schema gate wrote tables: before=%v after=%v", before, got)
			}
		})
	}
}

func TestAdminAccountCredentialFailBeforeInputOnBadSetup(t *testing.T) {
	dir := t.TempDir()
	validKey := filepath.Join(dir, "master.key")
	if err := os.WriteFile(validKey, bytes.Repeat([]byte{0x41}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	badKey := filepath.Join(dir, "bad.key")
	if err := os.WriteFile(badKey, bytes.Repeat([]byte{0x42}, 32), 0644); err != nil {
		t.Fatal(err)
	}
	existingDB := filepath.Join(dir, "existing.db")
	if err := adminMigrateFile(existingDB, validKey); err != nil {
		t.Fatal(err)
	}
	missingDB := filepath.Join(dir, "missing.db")
	missingKey := filepath.Join(dir, "missing.key")
	for _, tc := range []struct {
		name            string
		dbPath, keyPath string
		prefix          []string
		want            string
		mustNotExist    string
	}{
		{"missing flags", "", "", nil, "--db and --master-key are required", missingDB},
		{"missing database", missingDB, validKey, nil, "cannot open database", missingDB},
		{"missing key", existingDB, missingKey, nil, "invalid master key", ""},
		{"bad mode key", existingDB, badKey, nil, "invalid master key", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before []int64
			if tc.dbPath != "" && tc.dbPath != missingDB {
				before = adminAccountCredentialCounts(t, tc.dbPath)
			}
			for _, op := range [][]string{{"account", "create", "--id", "no-write", "--connector", "responses"}, {"credential", "create", "--account", "acct", "--id", "no-write"}} {
				input := &countingReader{data: []byte("must-not-be-read")}
				args := append(append([]string(nil), tc.prefix...), op...)
				_, err := runAdminCaptured(tc.dbPath, tc.keyPath, input, args...)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("%v: expected %q, got %v", op, tc.want, err)
				}
				if input.reads != 0 {
					t.Fatalf("%v consumed stdin %d times", op, input.reads)
				}
			}
			if tc.mustNotExist != "" {
				if _, err := os.Lstat(tc.mustNotExist); !os.IsNotExist(err) {
					t.Fatalf("created database path: %v", err)
				}
			}
			if before != nil {
				if after := adminAccountCredentialCounts(t, tc.dbPath); !equalInt64s(after, before) {
					t.Fatalf("failed setup modified account/credential rows: before=%v after=%v", before, after)
				}
			}
		})
	}
}

func TestAdminConcurrentMigrationsSerialize(t *testing.T) {
	dir := t.TempDir()
	dbPath, keyPath := filepath.Join(dir, "state.db"), filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0x2d}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	// Initialize the WAL before the concurrent writers so this exercises the
	// migration journal lock, not competing first-open PRAGMA initialization.
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	const workers = 6
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out, stderr bytes.Buffer
			errCh <- runAdmin(adminEnvironment{ctx: context.Background(), args: []string{"--db", dbPath, "--master-key", keyPath, "migrate"}, stdout: &out, stderr: &stderr})
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent migrate: %v", err)
		}
	}
}

func TestAdminMigrationContentionIsBounded(t *testing.T) {
	dir := t.TempDir()
	dbPath, keyPath := filepath.Join(dir, "state.db"), filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0x2d}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if err := runAdmin(adminEnvironment{ctx: context.Background(), args: []string{"--db", dbPath, "--master-key", keyPath, "migrate"}, stdout: &out, stderr: &stderr}); err != nil {
		t.Fatal(err)
	}
	locker, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	conn, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), `ROLLBACK`)
	start := time.Now()
	var adminOut, adminErr bytes.Buffer
	err = runAdmin(adminEnvironment{ctx: context.Background(), args: []string{"--db", dbPath, "--master-key", keyPath, "migrate"}, stdout: &adminOut, stderr: &adminErr})
	elapsed := time.Since(start)
	if err == nil || elapsed < 400*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("contention result err=%v elapsed=%s output=%q", err, elapsed, adminOut.String()+adminErr.String())
	}
}

func TestAdminSecretInputBoundedTrimmedAndZeroable(t *testing.T) {
	input, err := readAdminSecret(strings.NewReader("private-secret\n"))
	if err != nil || string(input) != "private-secret" {
		t.Fatalf("read secret=%q err=%v", input, err)
	}
	zeroAdminSecret(input)
	if !bytes.Equal(input, make([]byte, len(input))) {
		t.Fatal("secret buffer was not cleared")
	}
	if value, err := readAdminSecret(strings.NewReader(strings.Repeat("x", maxAdminSecret+1))); err == nil || value != nil {
		t.Fatalf("oversized secret accepted: len=%d err=%v", len(value), err)
	}
}

func TestAdminAccountLifecycleAcrossInvocations(t *testing.T) {
	dir := t.TempDir()
	dbPath, keyPath := adminTestFiles(t, dir)
	run := func(args ...string) (string, error) { return runAdminTest(t, dbPath, keyPath, nil, args...) }
	created, err := run("account", "create", "--id", "acct", "--connector", "responses")
	if err != nil || !strings.Contains(created, "enabled=true") || !strings.Contains(created, "created_at=") {
		t.Fatalf("create output=%q err=%v", created, err)
	}
	createdAt := adminTimestamp(t, created, "created_at")
	initialUpdatedAt := adminTimestamp(t, created, "updated_at")
	if _, err := run("account", "create", "--id", "acct", "--connector", "responses"); err == nil || !strings.Contains(err.Error(), "cannot create account") {
		t.Fatalf("duplicate account: %v", err)
	}
	accountsDB, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := sqlite.NewAccounts(accountsDB).List(context.Background(), sqlite.AccountFilter{})
	if err != nil || len(retained) != 1 || retained[0].ID != "acct" {
		t.Fatalf("duplicate altered account rows: %+v err=%v", retained, err)
	}
	if err := accountsDB.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := run("account", "create", "--id", "", "--connector", "responses"); err == nil {
		t.Fatal("empty account ID accepted")
	}
	if _, err := run("account", "disable", "acct"); err != nil {
		t.Fatal(err)
	}
	disabled, err := run("account", "get", "acct")
	if err != nil || !strings.Contains(disabled, "enabled=false") {
		t.Fatalf("get output=%q err=%v", disabled, err)
	}
	if got := adminTimestamp(t, disabled, "created_at"); !got.Equal(createdAt) {
		t.Fatalf("created_at changed: got %s want %s", got, createdAt)
	}
	if got := adminTimestamp(t, disabled, "updated_at"); !got.After(initialUpdatedAt) {
		t.Fatalf("updated_at did not advance: got %s initial %s", got, initialUpdatedAt)
	}
	if _, err := run("account", "enable", "acct"); err != nil {
		t.Fatal(err)
	}
	filtered, err := run("account", "list", "--connector", "responses", "--enabled=true")
	if err != nil || strings.Count(filtered, "id=acct") != 1 {
		t.Fatalf("list output=%q err=%v", filtered, err)
	}
	if _, err := run("account", "list", "--enabled=1"); err == nil {
		t.Fatal("non-literal enabled filter accepted")
	}
	if _, err := run("account", "create", "--id", "bad", "--connector", ""); err == nil {
		t.Fatal("empty connector accepted")
	}
}

func TestAdminCredentialCreateUpdateAndSecretIsolation(t *testing.T) {
	dir := t.TempDir()
	dbPath, keyPath := adminTestFiles(t, dir)
	if _, err := runAdminTest(t, dbPath, keyPath, nil, "account", "create", "--id", "acct", "--connector", "responses"); err != nil {
		t.Fatal(err)
	}
	const secret1, secret2 = "credential-private-marker-one", "credential-private-marker-two"
	args := []string{"credential", "create", "--account", "acct", "--id", "primary", "--expires-at", "2030-01-02T03:04:05Z"}
	out, err := runAdminTest(t, dbPath, keyPath, strings.NewReader(secret1+"\n"), args...)
	if err != nil || !strings.Contains(out, "revision=1") {
		t.Fatalf("create output=%q err=%v", out, err)
	}
	if strings.Contains(out, secret1) {
		t.Fatalf("secret in output: %q", out)
	}
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := sqlite.NewCredentials(db).Get(context.Background(), "acct", "primary")
	if err != nil || stored.Revision != 1 {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := secure.Open(key, secure.Envelope{FormatVersion: stored.FormatVersion, KeyVersion: stored.KeyVersion, Nonce: stored.Nonce, Ciphertext: stored.Ciphertext}, "credentials", stored.ID, stored.AccountID)
	if err != nil || string(plain) != secret1 {
		t.Fatalf("decrypt=%q err=%v", plain, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	update := []string{"credential", "update", "--account", "acct", "--id", "primary", "--expected-revision", "1"}
	if out, err := runAdminTest(t, dbPath, keyPath, strings.NewReader(secret2), update...); err != nil || !strings.Contains(out, "revision=2") {
		t.Fatalf("update output=%q err=%v", out, err)
	}
	if _, err := runAdminTest(t, dbPath, keyPath, strings.NewReader("stale-secret"), update...); err == nil || !strings.Contains(err.Error(), "revision mismatch") {
		t.Fatalf("stale update: %v", err)
	}
	assertAdminCredential(t, dbPath, keyPath, "primary", 2, secret2)
	if _, err := runAdminTest(t, dbPath, keyPath, strings.NewReader("orphan-secret"), "credential", "create", "--account", "missing", "--id", "orphan"); err == nil {
		t.Fatal("missing account accepted")
	}
	assertAdminCredentialMissing(t, dbPath, "missing", "orphan")
	if _, err := runAdminTest(t, dbPath, keyPath, strings.NewReader(""), "credential", "create", "--account", "acct", "--id", "empty"); err == nil {
		t.Fatal("empty secret accepted")
	}
	assertAdminCredentialMissing(t, dbPath, "acct", "empty")
	if _, err := runAdminTest(t, dbPath, keyPath, strings.NewReader("argv-secret"), "credential", "create", "--account", "acct", "--id", "argv", "--secret", "argv-secret"); err == nil {
		t.Fatal("secret argument accepted")
	}
	assertAdminCredentialMissing(t, dbPath, "acct", "argv")
	if _, err := runAdminTest(t, dbPath, keyPath, strings.NewReader(strings.Repeat("x", maxAdminSecret+1)), "credential", "create", "--account", "acct", "--id", "oversized"); err == nil || !strings.Contains(err.Error(), "exceeds 64 KiB") {
		t.Fatalf("oversized input result: %v", err)
	}
	assertAdminCredentialMissing(t, dbPath, "acct", "oversized")
	for _, path := range []string{dbPath, dbPath + "-wal"} {
		if b, err := os.ReadFile(path); err == nil {
			for _, marker := range []string{secret1, secret2, "stale-secret", "orphan-secret", "argv-secret"} {
				if bytes.Contains(b, []byte(marker)) {
					t.Fatalf("plaintext %q found in %s", marker, path)
				}
			}
		}
	}
}

func assertAdminCredential(t *testing.T, dbPath, keyPath, id string, revision int64, want string) {
	t.Helper()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stored, err := sqlite.NewCredentials(db).Get(context.Background(), "acct", id)
	if err != nil || stored.Revision != revision {
		t.Fatalf("stored credential=%+v err=%v", stored, err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := secure.Open(key, secure.Envelope{FormatVersion: stored.FormatVersion, KeyVersion: stored.KeyVersion, Nonce: stored.Nonce, Ciphertext: stored.Ciphertext}, "credentials", stored.ID, stored.AccountID)
	if err != nil || string(plain) != want {
		t.Fatalf("stored plaintext mismatch: err=%v", err)
	}
}

func assertAdminCredentialMissing(t *testing.T, dbPath, accountID, id string) {
	t.Helper()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := sqlite.NewCredentials(db).Get(context.Background(), accountID, id); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("credential (%s,%s) exists or errored unexpectedly: %v", accountID, id, err)
	}
}

func adminTestFiles(t *testing.T, dir string) (string, string) {
	t.Helper()
	dbPath, keyPath := filepath.Join(dir, "admin.db"), filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0x39}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runAdminTest(t, dbPath, keyPath, nil, "migrate"); err != nil {
		t.Fatal(err)
	}
	return dbPath, keyPath
}

type countingReader struct {
	data  []byte
	reads int
}

func (r *countingReader) Read(p []byte) (int, error) {
	r.reads++
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func adminMigrateFile(path, key string) error {
	var out, stderr bytes.Buffer
	return runAdmin(adminEnvironment{ctx: context.Background(), args: []string{"--db", path, "--master-key", key, "migrate"}, stdout: &out, stderr: &stderr})
}

func runAdminCaptured(dbPath, keyPath string, input io.Reader, args ...string) (string, error) {
	var out, stderr bytes.Buffer
	full := make([]string, 0, len(args)+4)
	if dbPath != "" {
		full = append(full, "--db", dbPath)
	}
	if keyPath != "" {
		full = append(full, "--master-key", keyPath)
	}
	full = append(full, args...)
	err := runAdmin(adminEnvironment{ctx: context.Background(), args: full, stdin: input, stdout: &out, stderr: &stderr})
	return out.String() + stderr.String(), err
}

func adminTableNames(t *testing.T, path string) []string {
	t.Helper()
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

func adminAccountCredentialCounts(t *testing.T, path string) []int64 {
	t.Helper()
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var accounts, credentials int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM credentials`).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	return []int64{accounts, credentials}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func adminTimestamp(t *testing.T, output, field string) time.Time {
	t.Helper()
	for _, part := range strings.Fields(output) {
		name, value, ok := strings.Cut(part, "=")
		if ok && name == field {
			parsed, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				t.Fatal(err)
			}
			return parsed
		}
	}
	t.Fatalf("missing %s in %q", field, output)
	return time.Time{}
}

func runAdminTest(t *testing.T, dbPath, keyPath string, stdin *strings.Reader, args ...string) (string, error) {
	t.Helper()
	var out, stderr bytes.Buffer
	full := append([]string{"--db", dbPath, "--master-key", keyPath}, args...)
	var input io.Reader
	if stdin != nil {
		input = stdin
	}
	err := runAdmin(adminEnvironment{ctx: context.Background(), args: full, stdin: input, stdout: &out, stderr: &stderr})
	combined := out.String() + stderr.String()
	if err != nil && (strings.Contains(combined, "credential-private-marker") || strings.Contains(err.Error(), "credential-private-marker")) {
		t.Fatal("secret leaked through command output/error")
	}
	return combined, err
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
