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
	if version != CurrentSchemaVersion() {
		t.Fatalf("version = %d, want %d", version, CurrentSchemaVersion())
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
	if count != CurrentSchemaVersion() || after != before {
		t.Fatalf("repeat migration changed journal/schema: entries=%d tables=%d (before %d)", count, after, before)
	}
}

func TestMigrateFailedStepRollsBack(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "rollback.db"))
	if err := migrate(context.Background(), db, schemaMigrations[:3]); err != nil {
		t.Fatal(err)
	}
	migrations := append(append([]migration(nil), schemaMigrations[:3]...), migration{
		version: 4,
		statements: []string{
			`CREATE TABLE should_rollback (id INTEGER PRIMARY KEY)`,
			`THIS IS NOT SQL`,
		},
	})
	if err := migrate(context.Background(), db, migrations); err == nil || !strings.Contains(err.Error(), "migration 4") {
		t.Fatalf("migrate error = %v, want migration 4 failure", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("journal entries = %d, err = %v, want 3", count, err)
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
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != CurrentSchemaVersion() {
		t.Fatalf("schema version = %d, err = %v; want %d", version, err, CurrentSchemaVersion())
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

func TestSchemaPoliciesAndVirtualKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.db")
	db := openTestDB(t, path)
	if err := migrate(context.Background(), db, schemaMigrations[:2]); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openTestDB(t, path)
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("idempotent Migrate: %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != CurrentSchemaVersion() {
		t.Fatalf("schema version = %d, err = %v; want %d", version, err, CurrentSchemaVersion())
	}

	policy := `INSERT INTO key_policies(id, revision, models, connectors, rpm, tpm, created_at) VALUES (?, ?, '[]', '[]', 0, 0, 1000)`
	if _, err := db.Exec(policy, "p", 1); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]any{{"p", 1}, {"bad", 0}, {"bad", -1}} {
		if _, err := db.Exec(policy, args...); err == nil {
			t.Errorf("invalid/duplicate policy %v accepted", args)
		}
	}
	for _, field := range []string{"rpm", "tpm", "enabled"} {
		value := -1
		if field == "enabled" {
			value = 2
		}
		query := `INSERT INTO key_policies(id, revision, enabled, models, connectors, rpm, tpm, created_at) VALUES ('bad', 1, 1, '[]', '[]', 0, 0, 1000)`
		switch field {
		case "rpm":
			query = `INSERT INTO key_policies(id, revision, models, connectors, rpm, tpm, created_at) VALUES ('bad', 1, '[]', '[]', -1, 0, 1000)`
		case "tpm":
			query = `INSERT INTO key_policies(id, revision, models, connectors, rpm, tpm, created_at) VALUES ('bad', 1, '[]', '[]', 0, -1, 1000)`
		case "enabled":
			query = fmt.Sprintf(`INSERT INTO key_policies(id, revision, enabled, models, connectors, rpm, tpm, created_at) VALUES ('bad', 1, %d, '[]', '[]', 0, 0, 1000)`, value)
		}
		if _, err := db.Exec(query); err == nil {
			t.Errorf("invalid policy %s accepted", field)
		}
	}

	key := `INSERT INTO virtual_keys(id, key_id, digest, policy_id, policy_revision, created_at) VALUES (?, ?, ?, ?, ?, 1000)`
	if _, err := db.Exec(key, "v", "public", "digest", "p", 1); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]any{{"v", "other", "other-digest", "p", 1}, {"other", "public", "other-digest", "p", 1}, {"other", "other", "digest", "p", 1}, {"orphan", "orphan", "orphan", "missing", 1}} {
		if _, err := db.Exec(key, args...); err == nil {
			t.Errorf("duplicate/invalid virtual key %v accepted", args)
		}
	}
	for _, query := range []string{
		`UPDATE virtual_keys SET revision = 0 WHERE id = 'v'`,
		`UPDATE virtual_keys SET enabled = 2 WHERE id = 'v'`,
		`UPDATE virtual_keys SET revoked = 2 WHERE id = 'v'`,
	} {
		if _, err := db.Exec(query); err == nil {
			t.Errorf("invalid virtual-key update accepted: %s", query)
		}
	}
	if _, err := db.Exec(`DELETE FROM key_policies WHERE id = 'p' AND revision = 1`); err == nil {
		t.Fatal("referenced policy deleted despite ON DELETE RESTRICT")
	}
	if _, err := db.Exec(`UPDATE key_policies SET enabled = 0 WHERE id = 'p' AND revision = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE virtual_keys SET enabled = 0, revoked = 1, revoked_at = 2000 WHERE id = 'v'`); err != nil {
		t.Fatal(err)
	}
	var policies, keys int
	if err := db.QueryRow(`SELECT count(*) FROM key_policies WHERE id = 'p'`).Scan(&policies); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM virtual_keys WHERE id = 'v'`).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if policies != 1 || keys != 1 {
		t.Fatalf("disabled/revoked history retained: policies=%d keys=%d", policies, keys)
	}
	var violations int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("foreign key violations = %d, err = %v", violations, err)
	}

	for table, expected := range map[string]map[string]bool{
		"key_policies": {"created_at": true},
		"virtual_keys": {"created_at": true, "revoked_at": true},
	} {
		rows, err := db.Query(`SELECT name, type FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for rows.Next() {
			var name, typ string
			if err := rows.Scan(&name, &typ); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			seen[name] = true
			if strings.Contains(strings.ToLower(name), "secret") || strings.Contains(strings.ToLower(name), "plaintext") || name == "key" || name == "raw_key" {
				t.Errorf("unexpected secret column %s.%s", table, name)
			}
			if expected[name] && typ != "INTEGER" {
				t.Errorf("%s.%s type = %q, want INTEGER Unix milliseconds", table, name, typ)
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		for name := range expected {
			if !seen[name] {
				t.Errorf("%s.%s missing", table, name)
			}
		}
	}
	for _, index := range []string{"idx_virtual_keys_digest", "idx_virtual_keys_policy"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_index_list('virtual_keys') WHERE name = ?`, index).Scan(&count); err != nil || count != 1 {
			t.Errorf("index %s present=%d err=%v", index, count, err)
		}
	}
}

func TestSchemaRequestsAttemptsUsageAndReservations(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "ledger.db"))
	if err := migrate(context.Background(), db, schemaMigrations[:3]); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("apply migration 4 to version 3 database: %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != CurrentSchemaVersion() {
		t.Fatalf("schema version = %d, err=%v; want %d", version, err, CurrentSchemaVersion())
	}
	for _, statement := range []string{
		`INSERT INTO accounts(id, connector, created_at, updated_at) VALUES ('a', 'c', 1000, 1000)`,
		`INSERT INTO key_policies(id, revision, models, connectors, rpm, tpm, created_at) VALUES ('p', 1, '[]', '[]', 10, 100, 1000)`,
		`INSERT INTO virtual_keys(id, key_id, digest, policy_id, policy_revision, created_at) VALUES ('v', 'public', 'digest', 'p', 1, 1000)`,
		`INSERT INTO requests(id, virtual_key_id, key_revision, policy_id, policy_revision, accepted_at, protocol, model, route_id, state) VALUES ('r', 'v', 1, 'p', 1, 1234567890123, 'responses', 'm', 'route', 'admitted')`,
		`INSERT INTO attempts(id, request_id, ordinal, account_id, connector, route_id, budget_policy, estimate_tokens, estimate_method, state) VALUES ('at', 'r', 1, 'a', 'c', 'route', 'fixed', 0, 'known', 'reserved')`,
		`INSERT INTO usage_records(attempt_id, input_tokens, output_tokens, reasoning_tokens, cached_tokens, source, completeness, recorded_at) VALUES ('at', NULL, 0, NULL, 0, 'unknown', 'partial', 1234567890123)`,
		`INSERT INTO reservations(attempt_id, estimated_tokens, actual_tokens, effective_charge, state, reconciled_at) VALUES ('at', 0, NULL, NULL, 'held', NULL)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for ordinal, id := range []string{"negative-usage", "negative-actual", "negative-charge", "invalid-reservation"} {
		if _, err := db.Exec(`INSERT INTO attempts(id, request_id, ordinal, connector, route_id, budget_policy, estimate_tokens, estimate_method, state) VALUES (?, 'r', ?, 'c', 'route', 'fixed', 0, 'known', 'reserved')`, id, ordinal+2); err != nil {
			t.Fatal(err)
		}
	}
	var input, output, reasoning, cached, estimate, actual, charge sql.NullInt64
	if err := db.QueryRow(`SELECT input_tokens, output_tokens, reasoning_tokens, cached_tokens FROM usage_records WHERE attempt_id='at'`).Scan(&input, &output, &reasoning, &cached); err != nil {
		t.Fatal(err)
	}
	if input.Valid || !output.Valid || output.Int64 != 0 || reasoning.Valid || !cached.Valid || cached.Int64 != 0 {
		t.Fatalf("nullable usage = input %v output %v reasoning %v cached %v; want NULL, zero, NULL, zero", input, output, reasoning, cached)
	}
	if err := db.QueryRow(`SELECT estimated_tokens, actual_tokens, effective_charge FROM reservations WHERE attempt_id='at'`).Scan(&estimate, &actual, &charge); err != nil {
		t.Fatal(err)
	}
	if !estimate.Valid || estimate.Int64 != 0 || actual.Valid || charge.Valid {
		t.Fatalf("reservation counters = %v %v %v; want zero, NULL, NULL", estimate, actual, charge)
	}

	for _, test := range []struct {
		name, statement, category string
	}{
		{"duplicate request primary key", `INSERT INTO requests(id, virtual_key_id, key_revision, policy_id, policy_revision, accepted_at, protocol, model, route_id, state) VALUES ('r', NULL, 1, 'p', 1, 1, 'responses', 'm', 'route', 'admitted')`, "UNIQUE constraint failed"},
		{"nonpositive key revision", `INSERT INTO requests(id, key_revision, policy_id, policy_revision, accepted_at, protocol, model, route_id, state) VALUES ('bad-revision', 0, 'p', 1, 1, 'responses', 'm', 'route', 'admitted')`, "CHECK constraint failed"},
		{"invalid request state", `INSERT INTO requests(id, key_revision, policy_id, policy_revision, accepted_at, protocol, model, route_id, state) VALUES ('bad-state', 1, 'p', 1, 1, 'responses', 'm', 'route', 'unknown')`, "CHECK constraint failed"},
		{"missing composite policy revision", `INSERT INTO requests(id, key_revision, policy_id, policy_revision, accepted_at, protocol, model, route_id, state) VALUES ('orphan-policy', 1, 'p', 2, 1, 'responses', 'm', 'route', 'admitted')`, "FOREIGN KEY constraint failed"},
		{"nonpositive attempt ordinal", `INSERT INTO attempts(id, request_id, ordinal, connector, route_id, budget_policy, estimate_tokens, estimate_method, state) VALUES ('bad-ordinal', 'r', 0, 'c', 'route', 'b', 0, 'm', 'reserved')`, "CHECK constraint failed"},
		{"negative estimate", `INSERT INTO attempts(id, request_id, ordinal, connector, route_id, budget_policy, estimate_tokens, estimate_method, state) VALUES ('bad-estimate', 'r', 7, 'c', 'route', 'b', -1, 'm', 'reserved')`, "CHECK constraint failed"},
		{"invalid attempt state", `INSERT INTO attempts(id, request_id, ordinal, connector, route_id, budget_policy, estimate_tokens, estimate_method, state) VALUES ('invalid-attempt-state', 'r', 6, 'c', 'route', 'b', 0, 'm', 'unknown')`, "CHECK constraint failed"},
		{"invalid committed flag", `INSERT INTO attempts(id, request_id, ordinal, connector, route_id, budget_policy, estimate_tokens, estimate_method, state, committed) VALUES ('bad-commit', 'r', 7, 'c', 'route', 'b', 0, 'm', 'reserved', 2)`, "CHECK constraint failed"},
		{"duplicate attempt ordinal", `INSERT INTO attempts(id, request_id, ordinal, connector, route_id, budget_policy, estimate_tokens, estimate_method, state) VALUES ('duplicate-ordinal', 'r', 1, 'c', 'route', 'b', 0, 'm', 'reserved')`, "UNIQUE constraint failed"},
		{"duplicate attempt primary key", `INSERT INTO attempts(id, request_id, ordinal, connector, route_id, budget_policy, estimate_tokens, estimate_method, state) VALUES ('at', 'r', 7, 'c', 'route', 'b', 0, 'm', 'reserved')`, "UNIQUE constraint failed"},
		{"duplicate usage primary key", `INSERT INTO usage_records(attempt_id, input_tokens, output_tokens, reasoning_tokens, cached_tokens, source, completeness, recorded_at) VALUES ('at', 0, 0, 0, 0, 'x', 'x', 1)`, "UNIQUE constraint failed"},
		{"negative usage count", `INSERT INTO usage_records(attempt_id, input_tokens, output_tokens, reasoning_tokens, cached_tokens, source, completeness, recorded_at) VALUES ('negative-usage', -1, 0, 0, 0, 'x', 'x', 1)`, "CHECK constraint failed"},
		{"invalid reservation state", `INSERT INTO reservations(attempt_id, estimated_tokens, state) VALUES ('invalid-reservation', 0, 'invalid')`, "CHECK constraint failed"},
		{"negative actual usage", `INSERT INTO reservations(attempt_id, estimated_tokens, actual_tokens, state) VALUES ('negative-actual', 0, -1, 'held')`, "CHECK constraint failed"},
		{"negative effective charge", `INSERT INTO reservations(attempt_id, estimated_tokens, effective_charge, state) VALUES ('negative-charge', 0, -1, 'held')`, "CHECK constraint failed"},
		{"orphan reservation attempt", `INSERT INTO reservations(attempt_id, estimated_tokens, state) VALUES ('missing', 0, 'held')`, "FOREIGN KEY constraint failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := db.Exec(test.statement); err == nil || !strings.Contains(err.Error(), test.category) {
				t.Fatalf("error = %v; want %q", err, test.category)
			}
		})
	}
	if _, err := db.Exec(`DELETE FROM accounts WHERE id='a'`); err != nil {
		t.Fatalf("delete account should preserve attempt history: %v", err)
	}
	var account sql.NullString
	if err := db.QueryRow(`SELECT account_id FROM attempts WHERE id='at'`).Scan(&account); err != nil || account.Valid {
		t.Fatalf("attempt account after deletion = %v, err=%v; want NULL", account, err)
	}
	if _, err := db.Exec(`DELETE FROM virtual_keys WHERE id='v'`); err != nil {
		t.Fatalf("delete virtual key should retain request history: %v", err)
	}
	var key sql.NullString
	if err := db.QueryRow(`SELECT virtual_key_id FROM requests WHERE id='r'`).Scan(&key); err != nil || key.Valid {
		t.Fatalf("request key after deletion = %v, err=%v; want NULL", key, err)
	}
	if _, err := db.Exec(`DELETE FROM attempts WHERE id='at'`); err == nil {
		t.Fatal("attempt with usage/reservation history deleted despite RESTRICT")
	}
	if _, err := db.Exec(`DELETE FROM key_policies WHERE id='p' AND revision=1`); err == nil {
		t.Fatal("policy referenced by request deleted despite RESTRICT")
	}
	for _, table := range []string{"requests", "attempts", "usage_records", "reservations"} {
		rows, err := db.Query(`SELECT name, type FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name, typ string
			if err := rows.Scan(&name, &typ); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			if strings.Contains(name, "payload") || strings.Contains(name, "body") || name == "provider_error" || name == "error_body" {
				t.Errorf("forbidden diagnostic/payload column %s.%s", table, name)
			}
			if strings.HasSuffix(name, "_at") && typ != "INTEGER" {
				t.Errorf("%s.%s type=%q, want INTEGER milliseconds", table, name, typ)
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, index := range []string{"idx_requests_key_accepted", "idx_attempts_request", "idx_reservations_state_reconciled"} {
		var found int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&found); err != nil || found != 1 {
			t.Errorf("index %s found=%d err=%v", index, found, err)
		}
	}
	var violations int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("foreign key violations=%d err=%v", violations, err)
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
			version: CurrentSchemaVersion() + 1, statements: []string{`CREATE TABLE cancelled_migration (id INTEGER)`},
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
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil || count != CurrentSchemaVersion() {
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
	if err := db1.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil || count != CurrentSchemaVersion() {
		t.Fatalf("concurrent migration journal entries = %d, err = %v; want %d", count, err, CurrentSchemaVersion())
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
			version: CurrentSchemaVersion() + 1, statements: []string{`CREATE TABLE must_not_apply (id INTEGER)`},
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
