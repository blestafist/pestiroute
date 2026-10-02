package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMigrateFreshAndIdempotent(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "migrate.db"))
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var version int
	var applied string
	if err := db.QueryRow(`SELECT version, applied_at FROM schema_migrations`).Scan(&version, &applied); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("version = %d, want 1", version)
	}
	if timestamp, err := time.Parse(time.RFC3339Nano, applied); err != nil || timestamp.Location() != time.UTC {
		t.Fatalf("applied_at = %q, err = %v; want UTC RFC3339 timestamp", applied, err)
	}
	var before int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var count, after int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if count != 1 || after != before {
		t.Fatalf("repeat migration changed journal/schema: entries=%d tables=%d (before %d)", count, after, before)
	}
}

func TestMigrateFailedStepRollsBack(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "rollback.db"))
	migrations := append(append([]migration(nil), schemaMigrations...), migration{
		version: 2,
		statements: []string{
			`CREATE TABLE should_rollback (id INTEGER PRIMARY KEY)`,
			`THIS IS NOT SQL`,
		},
	})
	if err := migrate(context.Background(), db, migrations); err == nil || !strings.Contains(err.Error(), "migration 2") {
		t.Fatalf("migrate error = %v, want migration 2 failure", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("journal entries = %d, err = %v, want 1", count, err)
	}
	var exists int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'should_rollback'`).Scan(&exists); err != nil || exists != 0 {
		t.Fatalf("failed migration table count = %d, err = %v", exists, err)
	}
}

func TestMigrateRejectsFutureSchemaWithoutModification(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "future.db"))
	for _, statement := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`,
		`INSERT INTO schema_migrations VALUES (1, 'existing')`,
		`INSERT INTO schema_migrations VALUES (99, 'future')`,
		`CREATE TABLE preserve_me (value TEXT)`,
		`INSERT INTO preserve_me VALUES ('unchanged')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	before := databaseSnapshot(t, db)
	err := Migrate(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("Migrate error = %v, want explicit future-version refusal", err)
	}
	if after := databaseSnapshot(t, db); after != before {
		t.Fatalf("future-schema refusal modified database: before %q after %q", before, after)
	}
}

func TestMigrateCancellationRollsBackAndReleasesConnection(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "cancel.db"))
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE lock_holder (value INTEGER)`); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(`INSERT INTO lock_holder VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	signalCtx := &lockSignalContext{Context: context.Background(), lockPath: make(chan struct{})}
	ctx, cancel := context.WithCancel(signalCtx)
	result := make(chan error, 1)
	go func() {
		result <- migrate(ctx, db, append(append([]migration(nil), schemaMigrations...), migration{
			version: 2, statements: []string{`CREATE TABLE cancelled_migration (id INTEGER)`},
		}))
	}()
	select {
	case <-signalCtx.lockPath:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("migration did not enter the SQLite lock acquisition path")
	}
	cancel()
	err = <-result
	if err == nil || ctx.Err() == nil {
		t.Fatalf("cancelled migration error = %v, context error = %v", err, ctx.Err())
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("database unusable after cancelled migration: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("journal entries after cancellation = %d, err = %v", count, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if err := Migrate(ctx, db); err == nil {
		t.Fatal("pre-cancelled migration succeeded")
	}
}

func TestMigrateConcurrentHandlesSerialize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	db1 := openTestDB(t, path)
	db2 := openTestDB(t, path)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, db := range []*sql.DB{db1, db2} {
		go func(db *sql.DB) {
			<-start
			results <- Migrate(context.Background(), db)
		}(db)
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db1.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("concurrent migration journal entries = %d, err = %v; want 1", count, err)
	}
}

func TestMigrateReadsFutureVersionAfterWaitingForWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future-race.db")
	db1 := openTestDB(t, path)
	db2 := openTestDB(t, path)
	if err := Migrate(context.Background(), db1); err != nil {
		t.Fatal(err)
	}
	blocker, err := db1.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(`UPDATE schema_migrations SET version = 99 WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	ctx := &lockSignalContext{Context: context.Background(), lockPath: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		result <- migrate(ctx, db2, append(append([]migration(nil), schemaMigrations...), migration{
			version: 2, statements: []string{`CREATE TABLE must_not_apply (id INTEGER)`},
		}))
	}()
	select {
	case <-ctx.lockPath:
	case <-time.After(2 * time.Second):
		_ = blocker.Rollback()
		t.Fatal("migration did not enter the SQLite lock acquisition path")
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("migration error = %v, want future-version refusal", err)
	}
	var exists int
	if err := db1.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'must_not_apply'`).Scan(&exists); err != nil || exists != 0 {
		t.Fatalf("future-version race created migration table: count=%d err=%v", exists, err)
	}
}

type lockSignalContext struct {
	context.Context
	lockPath chan struct{}
	once     sync.Once
}

func (c *lockSignalContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.lockPath) })
	return c.Context.Done()
}

func databaseSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var maxVersion, preserved int
	if err := db.QueryRow(`SELECT max(version), (SELECT count(*) FROM preserve_me WHERE value = 'unchanged') FROM schema_migrations`).Scan(&maxVersion, &preserved); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d:%d", maxVersion, preserved)
}
