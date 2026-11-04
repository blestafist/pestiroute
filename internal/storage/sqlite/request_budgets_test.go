package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestOperatorRequestBudgetAtomicRestartAndImmutableCeiling(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "budget.db")
	db1 := openTestDB(t, path)
	if err := Migrate(ctx, db1); err != nil {
		t.Fatal(err)
	}
	b1 := NewOperatorRequestBudgets(db1)
	if err := b1.VerifyCeiling(ctx, AntigravityOperatorBudgetID, 1000); err != nil {
		t.Fatal(err)
	}
	if err := b1.VerifyCeiling(ctx, AntigravityOperatorBudgetID, 999); err == nil {
		t.Fatal("changed ceiling accepted")
	}
	db2 := openTestDB(t, path)
	if err := Migrate(ctx, db2); err != nil {
		t.Fatal(err)
	}
	b2 := NewOperatorRequestBudgets(db2)
	const workers = 24
	var wg sync.WaitGroup
	var mu sync.Mutex
	var successes int
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			budget := b1
			if i%2 != 0 {
				budget = b2
			}
			if err := budget.Reserve(ctx, AntigravityOperatorBudgetID); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if successes != workers {
		t.Fatalf("successful reservations=%d, want %d", successes, workers)
	}
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}
	db1 = openTestDB(t, path)
	defer db1.Close()
	if err := Migrate(ctx, db1); err != nil {
		t.Fatal(err)
	}
	budget := NewOperatorRequestBudgets(db1)
	if err := budget.VerifyCeiling(ctx, AntigravityOperatorBudgetID, 1000); err != nil {
		t.Fatal(err)
	}
	var consumed int
	if err := db1.QueryRow(`SELECT consumed FROM operator_request_budgets WHERE account_id=?`, AntigravityOperatorBudgetID).Scan(&consumed); err != nil || consumed != workers {
		t.Fatalf("consumed=%d err=%v", consumed, err)
	}
	if _, err := db1.ExecContext(ctx, `UPDATE operator_request_budgets SET consumed=ceiling WHERE account_id=?`, AntigravityOperatorBudgetID); err != nil {
		t.Fatal(err)
	}
	if err := budget.Reserve(ctx, AntigravityOperatorBudgetID); !errors.Is(err, ErrRequestBudgetExceeded) {
		t.Fatalf("exhausted reserve err=%v", err)
	}
	if err := budget.Reserve(ctx, "not-configured"); err == nil {
		t.Fatal("missing budget accepted")
	}
}

func TestRequestBudgetUpgradeSeedsFixedBudgetWithoutSecrets(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "upgrade.db"))
	ctx := context.Background()
	if err := migrate(ctx, db, schemaMigrations[:6]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `SELECT 1 FROM operator_request_budgets`); err == nil {
		t.Fatal("budget table existed before version 7")
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var ceiling, consumed int
	if err := db.QueryRowContext(ctx, `SELECT ceiling, consumed FROM operator_request_budgets WHERE account_id=?`, AntigravityOperatorBudgetID).Scan(&ceiling, &consumed); err != nil {
		t.Fatal(err)
	}
	if ceiling != 1000 || consumed != 0 {
		t.Fatalf("ceiling=%d consumed=%d", ceiling, consumed)
	}
	var secretColumns int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('operator_request_budgets') WHERE name LIKE '%secret%' OR name LIKE '%token%'`).Scan(&secretColumns); err != nil || secretColumns != 0 {
		t.Fatalf("secret columns=%d err=%v", secretColumns, err)
	}
}

func TestRequestBudgetMigrationV7RollsBackOnConflict(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "rollback.db"))
	ctx := context.Background()
	if err := migrate(ctx, db, schemaMigrations[:6]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE operator_request_budgets(conflict INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err == nil {
		t.Fatal("conflicting migration succeeded")
	}
	version, err := SchemaVersion(ctx, db)
	if err != nil || version != 6 {
		t.Fatalf("schema version=%d err=%v; want rolled back v6", version, err)
	}
	var seeded int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version=7`).Scan(&seeded); err != nil || seeded != 0 {
		t.Fatalf("v7 journal rows=%d err=%v", seeded, err)
	}
}
