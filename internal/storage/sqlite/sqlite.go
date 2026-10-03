// Package sqlite provides the shared SQLite connection setup used by storage.
package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const busyTimeout = 500 * time.Millisecond

// Open opens a file-backed SQLite database with the required durability and
// connection-local pragmas configured for every pooled connection.
func Open(path string) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("sqlite database path is empty")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve sqlite database path: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: absPath}).String()
	query := url.Values{}
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(FULL)")
	query.Add("_pragma", "foreign_keys(ON)")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	dsn += "?" + query.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize sqlite database: %w", err)
	}
	return db, nil
}
