package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func admissionFixture(t *testing.T, rpm, tpm int64) (*Ledger, context.Context, string, string) {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "admission.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	policy, err := NewKeyPolicies(db).Create(ctx, CreateKeyPolicyParams{ID: "policy", Models: []string{"model"}, Connectors: []string{"connector"}, RPM: rpm, TPM: tpm})
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewVirtualKeys(db).Create(ctx, CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewAccounts(db).Create(ctx, Account{ID: "account", Connector: "connector", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return NewLedger(db), ctx, key.ID, policy.ID
}

func admissionRecords(key, policy, requestID, attemptID string, estimate int64) (RequestRecord, AttemptRecord, ReservationRecord) {
	return RequestRecord{ID: requestID, VirtualKeyID: key, KeyRevision: 1, PolicyID: policy, PolicyRevision: 1, Protocol: "responses", Model: "model", RouteID: "route", State: "admitted"},
		AttemptRecord{ID: attemptID, RequestID: requestID, Ordinal: 1, AccountID: "account", Connector: "connector", RouteID: "route", BudgetPolicy: "known", EstimateTokens: estimate, EstimateMethod: "fixture", State: "reserved"},
		ReservationRecord{AttemptID: attemptID, EstimatedTokens: estimate}
}

func TestAdmissionSerializesRPMAndTPM(t *testing.T) {
	ledger, ctx, key, policy := admissionFixture(t, 2, 8)
	const contenders = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := range contenders {
		wg.Go(func() {
			<-start
			q, a, r := admissionRecords(key, policy, fmt.Sprintf("request-%d", i), fmt.Sprintf("attempt-%d", i), 4)
			err := ledger.Admit(ctx, q, a, r)
			if err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			} else if !errors.Is(err, ErrLedgerConflict) {
				t.Errorf("Admit: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()
	if accepted != 2 {
		t.Fatalf("accepted %d requests; want 2", accepted)
	}
}

func TestAdmissionTPMRollbackAndIdempotentRetry(t *testing.T) {
	ledger, ctx, key, policy := admissionFixture(t, 4, 10)
	q, a, r := admissionRecords(key, policy, "request", "attempt", 6)
	if err := ledger.Admit(ctx, q, a, r); err != nil {
		t.Fatal(err)
	}
	conflicting := q
	conflicting.Model = "different"
	if err := ledger.Admit(ctx, conflicting, a, r); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("conflicting duplicate Admit = %v", err)
	}
	if err := ledger.Admit(ctx, q, a, r); err != nil {
		t.Fatalf("duplicate Admit: %v", err)
	}
	if got, err := ledger.GetRequest(ctx, q.ID); err != nil || got.AcceptedAt.IsZero() {
		t.Fatalf("accepted request = %#v, %v", got, err)
	}
	q2, a2, r2 := admissionRecords(key, policy, "rejected", "rejected-attempt", 5)
	if err := ledger.Admit(ctx, q2, a2, r2); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("TPM rejection = %v", err)
	}
	var requests, attempts, reservations int
	for _, tc := range []struct {
		query string
		dest  *int
	}{{`SELECT COUNT(*) FROM requests`, &requests}, {`SELECT COUNT(*) FROM attempts`, &attempts}, {`SELECT COUNT(*) FROM reservations`, &reservations}} {
		if err := ledger.db.QueryRowContext(ctx, tc.query).Scan(tc.dest); err != nil {
			t.Fatal(err)
		}
	}
	if requests != 1 || attempts != 1 || reservations != 1 {
		t.Fatalf("partial TPM rejection: requests=%d attempts=%d reservations=%d", requests, attempts, reservations)
	}
	second := AttemptRecord{ID: "retry", RequestID: q.ID, AccountID: "account", Connector: "connector", RouteID: "route", BudgetPolicy: "known", EstimateTokens: 4, EstimateMethod: "fixture", State: "reserved"}
	retryReservation := ReservationRecord{AttemptID: second.ID, EstimatedTokens: 4}
	if err := ledger.BeginAttempt(ctx, second, retryReservation); err != nil {
		t.Fatal(err)
	}
	if err := ledger.BeginAttempt(ctx, second, retryReservation); err != nil {
		t.Fatalf("duplicate BeginAttempt: %v", err)
	}
	conflictingRetry := second
	conflictingRetry.EstimateTokens++
	if err := ledger.BeginAttempt(ctx, conflictingRetry, ReservationRecord{AttemptID: second.ID, EstimatedTokens: conflictingRetry.EstimateTokens}); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("conflicting duplicate BeginAttempt = %v", err)
	}
	got, err := ledger.GetAttempt(ctx, second.ID)
	if err != nil || got.Ordinal != 2 {
		t.Fatalf("retry attempt = %#v, %v", got, err)
	}
	var rpm int
	if err := ledger.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE virtual_key_id=?`, key).Scan(&rpm); err != nil || rpm != 1 {
		t.Fatalf("request RPM rows = %d, %v", rpm, err)
	}
}

func TestAdmissionRejectsNonPositiveReservation(t *testing.T) {
	ledger, ctx, key, policy := admissionFixture(t, 2, 20)
	q, a, r := admissionRecords(key, policy, "request", "attempt", 0)
	if err := ledger.Admit(ctx, q, a, r); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("zero reservation = %v", err)
	}
}

func TestAdmissionForeignKeyFailureRollsBack(t *testing.T) {
	ledger, ctx, key, policy := admissionFixture(t, 2, 20)
	q, a, r := admissionRecords(key, policy, "request", "attempt", 5)
	a.AccountID = "missing-account"
	if err := ledger.Admit(ctx, q, a, r); err == nil {
		t.Fatal("expected account foreign key failure")
	}
	for _, table := range []string{"requests", "attempts", "reservations"} {
		var count int
		if err := ledger.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s after rollback = %d, %v", table, count, err)
		}
	}
}

func TestAdmissionRechecksTargetRouteAndAccount(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*RequestRecord, *AttemptRecord)
	}{
		{"model", func(q *RequestRecord, _ *AttemptRecord) { q.Model = "not-allowed" }},
		{"connector", func(_ *RequestRecord, a *AttemptRecord) { a.Connector = "not-allowed" }},
		{"route", func(_ *RequestRecord, a *AttemptRecord) { a.RouteID = "different-route" }},
		{"account-mismatch", func(_ *RequestRecord, a *AttemptRecord) { a.AccountID = "different-account" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger, ctx, key, policy := admissionFixture(t, 2, 20)
			q, a, r := admissionRecords(key, policy, "request", "attempt", 5)
			tc.mutate(&q, &a)
			if err := ledger.Admit(ctx, q, a, r); !errors.Is(err, ErrLedgerConflict) {
				t.Fatalf("invalid target admitted: %v", err)
			}
			var requests, attempts int
			if err := ledger.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests`).Scan(&requests); err != nil {
				t.Fatal(err)
			}
			if err := ledger.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts`).Scan(&attempts); err != nil {
				t.Fatal(err)
			}
			if requests != 0 || attempts != 0 {
				t.Fatalf("invalid target wrote rows: requests=%d attempts=%d", requests, attempts)
			}
		})
	}
	t.Run("disabled account", func(t *testing.T) {
		ledger, ctx, key, policy := admissionFixture(t, 2, 20)
		if _, err := NewAccounts(ledger.db).SetEnabled(ctx, "account", false); err != nil {
			t.Fatal(err)
		}
		q, a, r := admissionRecords(key, policy, "request", "attempt", 5)
		if err := ledger.Admit(ctx, q, a, r); !errors.Is(err, ErrLedgerConflict) {
			t.Fatalf("disabled account admitted: %v", err)
		}
	})
	t.Run("account connector mismatch", func(t *testing.T) {
		ledger, ctx, key, policy := admissionFixture(t, 2, 20)
		if _, err := NewAccounts(ledger.db).Create(ctx, Account{ID: "other-account", Connector: "different", Enabled: true}); err != nil {
			t.Fatal(err)
		}
		q, a, r := admissionRecords(key, policy, "request", "attempt", 5)
		a.AccountID = "other-account"
		if err := ledger.Admit(ctx, q, a, r); !errors.Is(err, ErrLedgerConflict) {
			t.Fatalf("mismatched account connector admitted: %v", err)
		}
	})
	t.Run("disabled before retry", func(t *testing.T) {
		ledger, ctx, key, policy := admissionFixture(t, 2, 20)
		q, a, r := admissionRecords(key, policy, "request", "attempt", 5)
		if err := ledger.Admit(ctx, q, a, r); err != nil {
			t.Fatal(err)
		}
		if _, err := NewAccounts(ledger.db).SetEnabled(ctx, "account", false); err != nil {
			t.Fatal(err)
		}
		retry := AttemptRecord{ID: "retry", RequestID: q.ID, AccountID: "account", Connector: "connector", RouteID: "route", BudgetPolicy: "known", EstimateTokens: 2, EstimateMethod: "fixture", State: "reserved"}
		if err := ledger.BeginAttempt(ctx, retry, ReservationRecord{AttemptID: retry.ID, EstimatedTokens: 2}); !errors.Is(err, ErrLedgerConflict) {
			t.Fatalf("disabled account retry admitted: %v", err)
		}
	})
}

func TestAdmissionAccountDisableSerializesWithAdmission(t *testing.T) {
	ledger, ctx, key, policy := admissionFixture(t, 2, 20)
	q, a, r := admissionRecords(key, policy, "request", "attempt", 5)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var admitErr, disableErr error
	wg.Go(func() { <-start; admitErr = ledger.Admit(ctx, q, a, r) })
	wg.Go(func() { <-start; _, disableErr = NewAccounts(ledger.db).SetEnabled(ctx, "account", false) })
	close(start)
	wg.Wait()
	if disableErr != nil {
		t.Fatal(disableErr)
	}
	if admitErr != nil && !errors.Is(admitErr, ErrLedgerConflict) {
		t.Fatalf("concurrent admission: %v", admitErr)
	}
	if admitErr == nil {
		if _, err := ledger.GetRequest(ctx, q.ID); err != nil {
			t.Fatalf("successful serialized admission lost request: %v", err)
		}
	}
	account, err := NewAccounts(ledger.db).Get(ctx, "account")
	if err != nil || account.Enabled {
		t.Fatalf("concurrent disable result = %#v, %v", account, err)
	}
}

func TestAdmissionTimestampWindowsRejectFutureAndRespectCutoff(t *testing.T) {
	const fixedNow = int64(1_700_000_000_000)
	t.Run("exact cutoff", func(t *testing.T) {
		ledger, ctx, key, policy := admissionFixture(t, 2, 20)
		ledger.now = func() time.Time { return time.UnixMilli(fixedNow).UTC() }
		for _, tc := range []struct {
			id string
			at int64
		}{{"at-cutoff", fixedNow - 60_000}, {"after-cutoff", fixedNow - 59_999}} {
			q, _, _ := admissionRecords(key, policy, tc.id, tc.id+"-attempt", 1)
			q.AcceptedAt, q.State = time.UnixMilli(tc.at).UTC(), "admitted"
			if err := ledger.CreateRequest(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		q, a, r := admissionRecords(key, policy, "new", "new-attempt", 1)
		if err := ledger.Admit(ctx, q, a, r); err != nil {
			t.Fatalf("cutoff should be excluded: %v", err)
		}
		q, a, r = admissionRecords(key, policy, "last-slot-used", "last-slot-attempt", 1)
		if err := ledger.Admit(ctx, q, a, r); !errors.Is(err, ErrLedgerConflict) {
			t.Fatalf("window count missed cutoff+1 row: %v", err)
		}
	})
	t.Run("future accepted", func(t *testing.T) {
		ledger, ctx, key, policy := admissionFixture(t, 3, 20)
		ledger.now = func() time.Time { return time.UnixMilli(fixedNow).UTC() }
		q, _, _ := admissionRecords(key, policy, "future", "future-attempt", 1)
		q.AcceptedAt, q.State = time.UnixMilli(fixedNow+1).UTC(), "admitted"
		if err := ledger.CreateRequest(ctx, q); err != nil {
			t.Fatal(err)
		}
		q, a, r := admissionRecords(key, policy, "new", "new-attempt", 1)
		if err := ledger.Admit(ctx, q, a, r); !errors.Is(err, ErrLedgerConflict) {
			t.Fatalf("future accepted_at bypassed window: %v", err)
		}
	})
	t.Run("future reconciled", func(t *testing.T) {
		ledger, ctx, key, policy := admissionFixture(t, 3, 20)
		ledger.now = func() time.Time { return time.UnixMilli(fixedNow).UTC() }
		q, a, r := admissionRecords(key, policy, "existing", "existing-attempt", 1)
		if err := ledger.Admit(ctx, q, a, r); err != nil {
			t.Fatal(err)
		}
		if _, err := ledger.db.ExecContext(ctx, `UPDATE reservations SET state='settled',effective_charge=1,reconciled_at=? WHERE attempt_id=?`, fixedNow+1, a.ID); err != nil {
			t.Fatal(err)
		}
		q, a, r = admissionRecords(key, policy, "new", "new-attempt", 1)
		if err := ledger.Admit(ctx, q, a, r); !errors.Is(err, ErrLedgerConflict) {
			t.Fatalf("future reconciled_at bypassed window: %v", err)
		}
	})
}
