package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenConfiguresEveryConnectionAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := openTestDB(t, path)
	for _, statement := range []string{
		`CREATE TABLE parent (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE child (parent_id INTEGER REFERENCES parent(id))`,
		`INSERT INTO parent VALUES (7)`,
		`INSERT INTO child VALUES (7)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}

	const connections = 6
	conns := make([]*sql.Conn, 0, connections)
	for range connections {
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
	}
	for i, conn := range conns {
		for pragma, want := range map[string]string{
			"journal_mode": "wal",
			"synchronous":  "2",
			"foreign_keys": "1",
			"busy_timeout": "500",
		} {
			var got string
			if err := conn.QueryRowContext(context.Background(), "PRAGMA "+pragma).Scan(&got); err != nil {
				t.Fatalf("connection %d PRAGMA %s: %v", i, pragma, err)
			}
			if !strings.EqualFold(got, want) {
				t.Errorf("connection %d PRAGMA %s = %q, want %q", i, pragma, got, want)
			}
		}
	}
	for _, conn := range conns {
		_ = conn.Close()
	}
	if _, err := db.Exec(`INSERT INTO child VALUES (99)`); err == nil {
		t.Fatal("foreign key violation accepted")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openTestDB(t, path)
	var count int
	if err := reopened.QueryRow(`SELECT count(*) FROM child`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("reopened row count = %d, err = %v", count, err)
	}
}

func TestWriteContentionHasBoundedWaitAndCancellation(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "contention.db"))
	if _, err := db.Exec(`CREATE TABLE item (id INTEGER PRIMARY KEY, value INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO item VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(`UPDATE item SET value = 1 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	err = db.QueryRow(`UPDATE item SET value = 2 WHERE id = 1 RETURNING value`).Scan(new(int))
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("contending write unexpectedly succeeded")
	}
	if elapsed < 400*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("busy wait = %s, want approximately configured 500ms", elapsed)
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}

	blocker, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(`UPDATE item SET value = 3 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	started = time.Now()
	err = db.QueryRowContext(ctx, `UPDATE item SET value = 4 WHERE id = 1 RETURNING value`).Scan(new(int))
	elapsed = time.Since(started)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("contended query err=%v, context err=%v", err, ctx.Err())
	}
	if elapsed > 2*time.Second {
		t.Fatalf("cancelled contention exceeded bounded busy wait: %s", elapsed)
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE item SET value = 5 WHERE id = 1`); err != nil {
		t.Fatalf("write after cancellation/rollback: %v", err)
	}
}

func TestNoTransactionSpansSimulatedNetworkWait(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "network.db"))
	if _, err := db.Exec(`CREATE TABLE request (id INTEGER PRIMARY KEY, state TEXT)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO request VALUES (1, 'intent')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	gate := make(chan struct{})
	started := make(chan error, 1)
	go func() {
		<-gate // Simulated network wait occurs only after the transaction commits.
		_, err := db.Exec(`UPDATE request SET state = 'done' WHERE id = 1`)
		started <- err
	}()
	if _, err := db.Exec(`INSERT INTO request VALUES (2, 'other-writer')`); err != nil {
		t.Fatalf("writer blocked during simulated network wait: %v", err)
	}
	close(gate)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
}
