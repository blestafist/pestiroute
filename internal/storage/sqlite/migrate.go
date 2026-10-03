package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type migration struct {
	version    int
	statements []string
}

var schemaMigrations = []migration{{
	version: 1,
	statements: []string{`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`},
}, {
	version: 2,
	statements: []string{
		`CREATE TABLE accounts (
			id TEXT PRIMARY KEY,
			connector TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE credentials (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
			format_version INTEGER NOT NULL,
			key_version TEXT NOT NULL,
			nonce BLOB NOT NULL,
			ciphertext BLOB NOT NULL,
			expires_at INTEGER,
			revision INTEGER NOT NULL CHECK (revision >= 1),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			UNIQUE (account_id, id)
		)`,
		`CREATE INDEX idx_credentials_account_id ON credentials(account_id)`,
	},
}, {
	version: 3,
	statements: []string{
		`CREATE TABLE key_policies (
			id TEXT NOT NULL,
			revision INTEGER NOT NULL CHECK (revision >= 1),
			enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
			models TEXT NOT NULL,
			connectors TEXT NOT NULL,
			rpm INTEGER NOT NULL CHECK (rpm >= 0),
			tpm INTEGER NOT NULL CHECK (tpm >= 0),
			created_at INTEGER NOT NULL,
			PRIMARY KEY (id, revision)
		)`,
		`CREATE TABLE virtual_keys (
			id TEXT PRIMARY KEY,
			key_id TEXT NOT NULL UNIQUE,
			digest TEXT NOT NULL UNIQUE,
			policy_id TEXT NOT NULL,
			policy_revision INTEGER NOT NULL,
			revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
			enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
			revoked INTEGER NOT NULL DEFAULT 0 CHECK (revoked IN (0, 1)),
			created_at INTEGER NOT NULL,
			revoked_at INTEGER,
			FOREIGN KEY (policy_id, policy_revision) REFERENCES key_policies(id, revision) ON DELETE RESTRICT
		)`,
		`CREATE INDEX idx_virtual_keys_digest ON virtual_keys(digest)`,
		`CREATE INDEX idx_virtual_keys_policy ON virtual_keys(policy_id, policy_revision)`,
	},
}, {
	version: 4,
	statements: []string{
		`CREATE TABLE requests (
			id TEXT PRIMARY KEY,
			virtual_key_id TEXT REFERENCES virtual_keys(id) ON DELETE SET NULL,
			key_revision INTEGER NOT NULL CHECK (key_revision >= 1),
			policy_id TEXT NOT NULL,
			policy_revision INTEGER NOT NULL,
			accepted_at INTEGER NOT NULL,
			protocol TEXT NOT NULL,
			model TEXT NOT NULL,
			route_id TEXT NOT NULL,
			state TEXT NOT NULL CHECK (state IN ('admitted', 'succeeded', 'failed', 'cancelled', 'interrupted')),
			finished_at INTEGER,
			FOREIGN KEY (policy_id, policy_revision) REFERENCES key_policies(id, revision) ON DELETE RESTRICT
		)`,
		`CREATE TABLE attempts (
			id TEXT PRIMARY KEY,
			request_id TEXT NOT NULL REFERENCES requests(id) ON DELETE RESTRICT,
			ordinal INTEGER NOT NULL CHECK (ordinal >= 1),
			account_id TEXT REFERENCES accounts(id) ON DELETE SET NULL,
			connector TEXT NOT NULL,
			route_id TEXT NOT NULL,
			budget_policy TEXT NOT NULL,
			estimate_tokens INTEGER NOT NULL CHECK (estimate_tokens >= 0),
			estimate_method TEXT NOT NULL,
			state TEXT NOT NULL CHECK (state IN ('reserved', 'intent', 'succeeded', 'failed', 'cancelled', 'interrupted')),
			committed INTEGER NOT NULL DEFAULT 0 CHECK (committed IN (0, 1)),
			error_category TEXT,
			error_reason TEXT,
			dispatched_at INTEGER,
			finished_at INTEGER,
			UNIQUE (request_id, ordinal)
		)`,
		`CREATE TABLE usage_records (
			attempt_id TEXT PRIMARY KEY REFERENCES attempts(id) ON DELETE RESTRICT,
			input_tokens INTEGER CHECK (input_tokens >= 0),
			output_tokens INTEGER CHECK (output_tokens >= 0),
			reasoning_tokens INTEGER CHECK (reasoning_tokens >= 0),
			cached_tokens INTEGER CHECK (cached_tokens >= 0),
			source TEXT NOT NULL,
			completeness TEXT NOT NULL,
			recorded_at INTEGER NOT NULL
		)`,
		`CREATE TABLE reservations (
			attempt_id TEXT PRIMARY KEY REFERENCES attempts(id) ON DELETE RESTRICT,
			estimated_tokens INTEGER NOT NULL CHECK (estimated_tokens >= 0),
			actual_tokens INTEGER CHECK (actual_tokens >= 0),
			effective_charge INTEGER CHECK (effective_charge >= 0),
			state TEXT NOT NULL CHECK (state IN ('held', 'settled', 'released', 'conservative')),
			reconciled_at INTEGER
		)`,
		`CREATE INDEX idx_requests_key_accepted ON requests(virtual_key_id, accepted_at)`,
		`CREATE INDEX idx_attempts_request ON attempts(request_id)`,
		`CREATE INDEX idx_reservations_state_reconciled ON reservations(state, reconciled_at)`,
	},
}, {
	version: 5,
	statements: []string{
		`CREATE TABLE auth_sessions (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
			connector TEXT NOT NULL,
			kind TEXT NOT NULL CHECK (kind IN ('interactive','refresh')),
			expected_credential_revision INTEGER NOT NULL CHECK (expected_credential_revision >= 1),
			lifecycle TEXT NOT NULL CHECK (lifecycle IN ('active','refresh_in_progress','uncertain','consumed')),
			expires_at INTEGER,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			format_version INTEGER,
			key_version TEXT,
			nonce BLOB,
			ciphertext BLOB,
			quarantine_reason TEXT CHECK (quarantine_reason IN ('ambiguous_result','cancelled_after_call','persistence_failed','restart_in_progress')),
			current_credential_revision INTEGER CHECK (current_credential_revision >= 1),
			CHECK ((kind = 'interactive' AND expires_at IS NOT NULL AND
			        ((lifecycle = 'active' AND format_version IS NOT NULL AND key_version IS NOT NULL AND nonce IS NOT NULL AND ciphertext IS NOT NULL) OR
			         (lifecycle = 'consumed' AND format_version IS NULL AND key_version IS NULL AND nonce IS NULL AND ciphertext IS NULL))) OR
			       (kind = 'refresh' AND lifecycle IN ('refresh_in_progress','uncertain') AND format_version IS NULL AND key_version IS NULL AND nonce IS NULL AND ciphertext IS NULL AND expires_at IS NULL))
		)`,
		`CREATE UNIQUE INDEX idx_auth_sessions_account_refresh_active ON auth_sessions(account_id) WHERE lifecycle IN ('refresh_in_progress','uncertain')`,
		`CREATE INDEX idx_auth_sessions_account_id ON auth_sessions(account_id)`,
		`CREATE INDEX idx_auth_sessions_lifecycle_expires ON auth_sessions(lifecycle, expires_at)`,
	},
}}

// CurrentSchemaVersion is the newest schema version supported by this binary.
func CurrentSchemaVersion() int { return len(schemaMigrations) }

// SchemaVersion reads and validates the applied migration journal without
// creating tables or applying pending migrations.
func SchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'
	)`).Scan(&exists); err != nil {
		return 0, fmt.Errorf("read sqlite schema version: %w", err)
	}
	if !exists {
		return 0, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return 0, fmt.Errorf("read sqlite migration journal: %w", err)
	}
	defer rows.Close()
	current := 0
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return 0, fmt.Errorf("read sqlite migration version: %w", err)
		}
		if version != current+1 || version > CurrentSchemaVersion() {
			return 0, fmt.Errorf("invalid or unsupported sqlite schema version %d", version)
		}
		current = version
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read sqlite migration journal: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close sqlite migration journal: %w", err)
	}
	return current, nil
}

// Migrate applies every pending schema migration atomically.
func Migrate(ctx context.Context, db *sql.DB) error {
	return migrate(ctx, db, schemaMigrations)
}

func migrate(ctx context.Context, db *sql.DB, migrations []migration) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("migrate sqlite schema: %w", err)
	}
	if len(migrations) == 0 {
		return errors.New("sqlite migrations are empty")
	}
	for i, m := range migrations {
		if m.version != i+1 || len(m.statements) == 0 {
			return fmt.Errorf("invalid sqlite migration sequence at position %d", i+1)
		}
	}

	for {
		done, err := migrateNext(ctx, db, migrations)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// migrateNext serializes the version read and one migration under SQLite's
// reserved write lock. A fresh Conn is used because database/sql Tx starts
// deferred transactions, which permit stale version reads before write-lock
// acquisition.
func migrateNext(ctx context.Context, db *sql.DB, migrations []migration) (done bool, retErr error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire sqlite migration connection: %w", err)
	}
	transactionAttempted := false
	defer func() {
		if transactionAttempted {
			if _, err := conn.ExecContext(context.Background(), `ROLLBACK`); err != nil && !strings.Contains(err.Error(), "no transaction is active") {
				retErr = errors.Join(retErr, fmt.Errorf("rollback sqlite migration: %w", err))
			}
		}
		if err := conn.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release sqlite migration connection: %w", err))
		}
	}()
	transactionAttempted = true
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return false, fmt.Errorf("begin immediate sqlite migration: %w", err)
	}

	var exists bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'
	)`).Scan(&exists); err != nil {
		return false, fmt.Errorf("inspect sqlite migration journal: %w", err)
	}
	current := 0
	if exists {
		rows, err := conn.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
		if err != nil {
			return false, fmt.Errorf("read sqlite migration journal: %w", err)
		}
		for expected := 1; rows.Next(); expected++ {
			var version int
			if err := rows.Scan(&version); err != nil {
				_ = rows.Close()
				return false, fmt.Errorf("read sqlite migration version: %w", err)
			}
			if version > len(migrations) {
				_ = rows.Close()
				return false, fmt.Errorf("sqlite schema version %d is newer than supported version %d", version, len(migrations))
			}
			if version != expected {
				_ = rows.Close()
				return false, fmt.Errorf("invalid sqlite migration journal: expected version %d, found %d", expected, version)
			}
			current = version
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("read sqlite migration journal: %w", err)
		}
		if err := rows.Close(); err != nil {
			return false, fmt.Errorf("close sqlite migration journal: %w", err)
		}
	}
	if current == len(migrations) {
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return false, fmt.Errorf("finish sqlite migration check: %w", err)
		}
		transactionAttempted = false
		return true, nil
	}
	m := migrations[current]
	for _, statement := range m.statements {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return false, fmt.Errorf("execute sqlite migration %d: %w", m.version, err)
		}
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, m.version, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return false, fmt.Errorf("record sqlite migration %d: %w", m.version, err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return false, fmt.Errorf("commit sqlite migration %d: %w", m.version, err)
	}
	transactionAttempted = false
	return false, nil
}
