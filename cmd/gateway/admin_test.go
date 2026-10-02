package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
