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
	if err := db.QueryRow(`SELECT version, applied_at FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version, &applied); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("version = %d, want 2", version)
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
	if count != 2 || after != before {
		t.Fatalf("repeat migration changed journal/schema: entries=%d tables=%d (before %d)", count, after, before)
	}
}

func TestMigrateFailedStepRollsBack(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "rollback.db"))
	migrations := []migration{schemaMigrations[0], {
		version: 2,
		statements: []string{
			`CREATE TABLE should_rollback (id INTEGER PRIMARY KEY)`,
			`THIS IS NOT SQL`,
		},
	}}
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

func TestSchemaAccountsAndCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db := openTestDB(t, path)
	if err := migrate(context.Background(), db, schemaMigrations[:1]); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openTestDB(t, path)
	var version int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != 2 {
		t.Fatalf("schema version = %d, err = %v; want 2", version, err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("idempotent Migrate: %v", err)
	}

	account := `INSERT INTO accounts(id, connector, enabled, created_at, updated_at) VALUES ('a', 'connector', 1, 1000, 1000)`
	if _, err := db.Exec(account); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(account); err == nil {
		t.Fatal("duplicate account ID accepted")
	}
	credential := `INSERT INTO credentials(id, account_id, format_version, key_version, nonce, ciphertext, expires_at, revision, created_at, updated_at) VALUES (?, ?, 1, 'key-v1', x'01', x'02', ?, ?, 1000, 1000)`
	if _, err := db.Exec(credential, "c", "missing", 2000, 1); err == nil {
		t.Fatal("credential with nonexistent account accepted")
	}
	if _, err := db.Exec(credential, "c", "a", 2000, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(credential, "c", "a", 2000, 1); err == nil {
		t.Fatal("duplicate credential ID accepted")
	}
	for _, revision := range []int{0, -1} {
		if _, err := db.Exec(credential, "bad-revision", "a", 2000, revision); err == nil {
			t.Errorf("credential revision %d accepted", revision)
		}
	}
	if _, err := db.Exec(`DELETE FROM accounts WHERE id = 'a'`); err == nil {
		t.Fatal("account with credential deleted despite RESTRICT")
	}
	if _, err := db.Exec(`UPDATE accounts SET enabled = 0 WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}
	var retained int
	if err := db.QueryRow(`SELECT count(*) FROM credentials WHERE account_id = 'a'`).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("credentials after disabling account = %d, err = %v", retained, err)
	}
	var violations int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("foreign key violations = %d, err = %v", violations, err)
	}

	for _, table := range []string{"accounts", "credentials"} {
		rows, err := db.Query(`SELECT name, type FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var column, typ string
			if err := rows.Scan(&column, &typ); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			if strings.Contains(strings.ToLower(column), "plaintext") || strings.Contains(strings.ToLower(column), "master_key") || column == "secret" {
				t.Errorf("unexpected plaintext/master-key column %s.%q", table, column)
			}
			if (column == "created_at" || column == "updated_at" || column == "expires_at") && typ != "INTEGER" {
				t.Errorf("%s.%s type = %q, want INTEGER Unix milliseconds", table, column, typ)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var compositeUnique bool
	var accountIndex bool
	indexes, err := db.Query(`PRAGMA index_list(credentials)`)
	if err != nil {
		t.Fatal(err)
	}
	for indexes.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := indexes.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			_ = indexes.Close()
			t.Fatal(err)
		}
		if name == "idx_credentials_account_id" {
			accountIndex = true
		}
		if unique == 1 {
			var columns []string
			info, err := db.Query(`PRAGMA index_info(` + quoteIdentifier(name) + `)`)
			if err != nil {
				_ = indexes.Close()
				t.Fatal(err)
			}
			for info.Next() {
				var indexSeq, cid int
				var column string
				if err := info.Scan(&indexSeq, &cid, &column); err != nil {
					_ = info.Close()
					_ = indexes.Close()
					t.Fatal(err)
				}
				columns = append(columns, column)
			}
			_ = info.Close()
			if strings.Join(columns, ",") == "account_id,id" {
				compositeUnique = true
			}
		}
	}
	if err := indexes.Close(); err != nil {
		t.Fatal(err)
	}
	if !compositeUnique {
		t.Fatal("UNIQUE(account_id, id) index missing")
	}
	if !accountIndex {
		t.Fatal("idx_credentials_account_id missing")
	}
}

func quoteIdentifier(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

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
			version: 3, statements: []string{`CREATE TABLE cancelled_migration (id INTEGER)`},
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
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil || count != 2 {
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
	if err := db1.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("concurrent migration journal entries = %d, err = %v; want 2", count, err)
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
	if _, err := blocker.Exec(`UPDATE schema_migrations SET version = version + 99`); err != nil {
		t.Fatal(err)
	}
	ctx := &lockSignalContext{Context: context.Background(), lockPath: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		result <- migrate(ctx, db2, append(append([]migration(nil), schemaMigrations...), migration{
			version: 3, statements: []string{`CREATE TABLE must_not_apply (id INTEGER)`},
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
