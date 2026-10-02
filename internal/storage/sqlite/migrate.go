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
}}

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
