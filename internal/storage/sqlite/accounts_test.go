package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func accountFixture(t *testing.T, path string) (*sql.DB, *Accounts) {
	t.Helper()
	db := openTestDB(t, path)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, NewAccounts(db)
}

func TestAccountLifecycleRestartAndFilters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db, repo := accountFixture(t, path)
	ctx := context.Background()
	created, err := repo.Create(ctx, Account{ID: "z-account", Connector: "connector-a", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if created.CreatedAt.Location() != time.UTC || created.CreatedAt.Nanosecond()%int(time.Millisecond) != 0 || !created.CreatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("Create timestamps = %#v", created)
	}
	var createdType, updatedType string
	if err := db.QueryRow(`SELECT typeof(created_at), typeof(updated_at) FROM accounts WHERE id = ?`, created.ID).Scan(&createdType, &updatedType); err != nil {
		t.Fatal(err)
	}
	if createdType != "integer" || updatedType != "integer" {
		t.Fatalf("timestamp SQLite types = %q, %q", createdType, updatedType)
	}
	if _, err := repo.Create(ctx, Account{ID: "a-account", Connector: "connector-a", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, Account{ID: "other", Connector: "connector-b", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, Account{ID: "", Connector: "connector-a"}); err == nil {
		t.Fatal("Create accepted empty ID")
	}
	if _, err := repo.Create(ctx, Account{ID: "missing-connector"}); err == nil {
		t.Fatal("Create accepted empty connector")
	}
	if _, err := repo.Create(ctx, Account{ID: "z-account", Connector: "connector-a"}); err == nil {
		t.Fatal("Create accepted duplicate ID")
	}
	got, err := repo.Get(ctx, created.ID)
	if err != nil || got != created {
		t.Fatalf("Get() = %#v, %v; want %#v", got, err, created)
	}
	if _, err := repo.Get(ctx, "absent"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("Get missing error = %v", err)
	}
	all, err := repo.List(ctx, AccountFilter{})
	if err != nil || len(all) != 3 || !sort.SliceIsSorted(all, func(i, j int) bool {
		if all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].ID < all[j].ID
		}
		return all[i].CreatedAt.Before(all[j].CreatedAt)
	}) {
		t.Fatalf("List all = %#v, %v", all, err)
	}
	falseValue := false
	disabled, err := repo.List(ctx, AccountFilter{Connector: "connector-a", Enabled: &falseValue})
	if err != nil || len(disabled) != 1 || disabled[0].ID != "a-account" {
		t.Fatalf("List filtered = %#v, %v", disabled, err)
	}
	empty, err := repo.List(ctx, AccountFilter{Connector: "absent"})
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("List empty = %#v, %v", empty, err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo = NewAccounts(db)
	got, err = repo.Get(ctx, created.ID)
	if err != nil || got != created {
		t.Fatalf("Get after reopen = %#v, %v; want %#v", got, err, created)
	}

	disabledAccount, err := repo.SetEnabled(ctx, created.ID, false)
	if err != nil || disabledAccount.Enabled || disabledAccount.ID != created.ID || !disabledAccount.CreatedAt.Equal(created.CreatedAt) || !disabledAccount.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("disable = %#v, %v", disabledAccount, err)
	}
	enabledAccount, err := repo.SetEnabled(ctx, created.ID, true)
	if err != nil || !enabledAccount.Enabled || !enabledAccount.CreatedAt.Equal(created.CreatedAt) || !enabledAccount.UpdatedAt.After(disabledAccount.UpdatedAt) {
		t.Fatalf("enable = %#v, %v", enabledAccount, err)
	}
	if _, err := repo.SetEnabled(ctx, "absent", true); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("SetEnabled missing error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo = NewAccounts(db)
	got, err = repo.Get(ctx, created.ID)
	if err != nil || !got.Enabled || !got.CreatedAt.Equal(created.CreatedAt) || !got.UpdatedAt.Equal(enabledAccount.UpdatedAt) {
		t.Fatalf("Get toggled after reopen = %#v, %v", got, err)
	}
	_ = db.Close()
}

func TestAccountDeleteRestrictedByCredential(t *testing.T) {
	db, repo := accountFixture(t, filepath.Join(t.TempDir(), "retention.db"))
	ctx := context.Background()
	if _, err := repo.Create(ctx, Account{ID: "retained", Connector: "connector", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO credentials
		(id, account_id, format_version, key_version, nonce, ciphertext, revision, created_at, updated_at)
		VALUES ('credential', 'retained', 1, 'v1', X'01', X'02', 1, 1, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SetEnabled(ctx, "retained", false); err != nil {
		t.Fatal(err)
	}
	var credentials int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM credentials WHERE account_id = 'retained'`).Scan(&credentials); err != nil || credentials != 1 {
		t.Fatalf("credential retained after disable: count=%d, err=%v", credentials, err)
	}
	if err := repo.Delete(ctx, "retained"); err == nil {
		t.Fatal("Delete removed account referenced by credential")
	}
	got, err := repo.Get(ctx, "retained")
	if err != nil || got.Enabled {
		t.Fatalf("account not retained disabled: %#v, %v", got, err)
	}
	if strings.Contains(fmt.Sprint(got), "credential") {
		t.Fatalf("account metadata exposed credential: %v", got)
	}
	if err := repo.Delete(ctx, "absent"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("Delete missing error = %v", err)
	}
}

func createAccountHistory(t *testing.T, db *sql.DB, accountID string) (attemptID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO key_policies
		(id, revision, enabled, models, connectors, rpm, tpm, created_at)
		VALUES ('history-policy', 1, 1, '[]', '[]', 0, 0, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO requests
		(id, key_revision, policy_id, policy_revision, accepted_at, protocol, model, route_id, state)
		VALUES ('history-request', 1, 'history-policy', 1, 1, 'protocol', 'model', 'route', 'succeeded')`); err != nil {
		t.Fatal(err)
	}
	const id = "history-attempt"
	if _, err := db.ExecContext(ctx, `INSERT INTO attempts
		(id, request_id, ordinal, account_id, connector, route_id, budget_policy, estimate_tokens, estimate_method, state)
		VALUES (?, 'history-request', 1, ?, 'connector', 'route', 'reject', 0, 'test', 'succeeded')`, id, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO usage_records
		(attempt_id, input_tokens, output_tokens, source, completeness, recorded_at)
		VALUES (?, 3, 4, 'provider', 'complete', 1)`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAccountDisableRetainsAttemptReferenceAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attempt-retention.db")
	db, repo := accountFixture(t, path)
	ctx := context.Background()
	account, err := repo.Create(ctx, Account{ID: "historical", Connector: "connector", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	attemptID := createAccountHistory(t, db, account.ID)
	disabled, err := repo.SetEnabled(ctx, account.ID, false)
	if err != nil || disabled.Enabled || disabled.ID != account.ID || !disabled.CreatedAt.Equal(account.CreatedAt) {
		t.Fatalf("disable historical account = %#v, %v", disabled, err)
	}
	var accountRef sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT account_id FROM attempts WHERE id = ?`, attemptID).Scan(&accountRef); err != nil || !accountRef.Valid || accountRef.String != account.ID {
		t.Fatalf("attempt account reference after disable = %#v, %v", accountRef, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo = NewAccounts(db)
	got, err := repo.Get(ctx, account.ID)
	if err != nil || got.Enabled || !got.CreatedAt.Equal(account.CreatedAt) {
		t.Fatalf("disabled account after reopen = %#v, %v", got, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT account_id FROM attempts WHERE id = ?`, attemptID).Scan(&accountRef); err != nil || !accountRef.Valid || accountRef.String != account.ID {
		t.Fatalf("attempt account reference after reopen = %#v, %v", accountRef, err)
	}
	var usageCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM usage_records WHERE attempt_id = ?`, attemptID).Scan(&usageCount); err != nil || usageCount != 1 {
		t.Fatalf("usage after reopen: count=%d, err=%v", usageCount, err)
	}
}

func TestAccountDeleteNullifiesAttemptReferenceRetainsHistory(t *testing.T) {
	db, repo := accountFixture(t, filepath.Join(t.TempDir(), "attempt-purge.db"))
	ctx := context.Background()
	account, err := repo.Create(ctx, Account{ID: "purge", Connector: "connector", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	attemptID := createAccountHistory(t, db, account.ID)
	if err := repo.Delete(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	var accountRef sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT account_id FROM attempts WHERE id = ?`, attemptID).Scan(&accountRef); err != nil || accountRef.Valid {
		t.Fatalf("attempt account reference after purge = %#v, %v; want NULL", accountRef, err)
	}
	var attempts, usage int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM attempts WHERE id = ?`, attemptID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM usage_records WHERE attempt_id = ?`, attemptID).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || usage != 1 {
		t.Fatalf("history after purge: attempts=%d usage=%d", attempts, usage)
	}
}

func TestAccountConcurrentLifecycle(t *testing.T) {
	_, repo := accountFixture(t, filepath.Join(t.TempDir(), "concurrent.db"))
	ctx := context.Background()
	created, err := repo.Create(ctx, Account{ID: "concurrent", Connector: "connector", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	const workers = 12
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	times := make(chan time.Time, workers)
	for i := range workers {
		wg.Add(1)
		go func(enabled bool) {
			defer wg.Done()
			updated, err := repo.SetEnabled(ctx, "concurrent", enabled)
			if err != nil {
				errCh <- err
				return
			}
			if updated.ID != created.ID || updated.Connector != created.Connector || !updated.CreatedAt.Equal(created.CreatedAt) {
				errCh <- fmt.Errorf("concurrent update changed immutable account metadata: %#v", updated)
				return
			}
			if updated.Enabled != enabled {
				errCh <- fmt.Errorf("SetEnabled(%t) returned enabled=%t", enabled, updated.Enabled)
				return
			}
			times <- updated.UpdatedAt
			if _, err := repo.List(ctx, AccountFilter{}); err != nil {
				errCh <- err
			}
		}(i%2 == 0)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	close(times)
	orderedTimes := make([]time.Time, 0, workers)
	for updatedAt := range times {
		orderedTimes = append(orderedTimes, updatedAt)
	}
	sort.Slice(orderedTimes, func(i, j int) bool { return orderedTimes[i].Before(orderedTimes[j]) })
	if len(orderedTimes) != workers {
		t.Fatalf("got %d successful update timestamps; want %d", len(orderedTimes), workers)
	}
	for i, updatedAt := range orderedTimes {
		if !updatedAt.After(created.UpdatedAt) || i > 0 && !updatedAt.After(orderedTimes[i-1]) {
			t.Fatalf("update timestamps are not strictly monotonic: %#v after %#v", orderedTimes, created.UpdatedAt)
		}
	}
	final, err := repo.Get(ctx, "concurrent")
	if err != nil {
		t.Fatal(err)
	}
	if final.ID != created.ID || final.Connector != created.Connector || !final.CreatedAt.Equal(created.CreatedAt) || final.UpdatedAt.Sub(created.UpdatedAt) < workers*time.Millisecond {
		t.Fatalf("concurrent final account violates lifecycle invariants: created=%#v final=%#v", created, final)
	}
}

func TestAccountConcurrentDeleteAndSetEnabled(t *testing.T) {
	_, repo := accountFixture(t, filepath.Join(t.TempDir(), "delete-race.db"))
	ctx := context.Background()
	if _, err := repo.Create(ctx, Account{ID: "race", Connector: "connector", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		if err := repo.Delete(ctx, "race"); err != nil {
			errCh <- fmt.Errorf("Delete: %w", err)
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		account, err := repo.SetEnabled(ctx, "race", false)
		if err != nil && !errors.Is(err, ErrAccountNotFound) {
			errCh <- fmt.Errorf("SetEnabled: %w", err)
		} else if err == nil && (account.ID != "race" || account.Connector != "connector" || account.Enabled != false) {
			errCh <- fmt.Errorf("unexpected SetEnabled result: %#v", account)
		}
	}()
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if _, err := repo.Get(ctx, "race"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("account after concurrent delete = %v; want not found", err)
	}
}
