package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRecoverCrashMatrixIdempotencyAndRetryGap(t *testing.T) {
	ledger, ctx, key, policy := admissionFixture(t, 20, 1000)
	db := ledger.db
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	ledger.now = func() time.Time { return now }
	admit := func(requestID, attemptID string) {
		q, a, hold := admissionRecords(key, policy, requestID, attemptID, 40)
		if err := ledger.Admit(ctx, q, a, hold); err != nil {
			t.Fatal(err)
		}
	}
	admit("unsent-request", "unsent-attempt")
	admit("intent-request", "intent-attempt")
	if err := ledger.RecordDispatchIntent(ctx, "intent-attempt", now); err != nil {
		t.Fatal(err)
	}
	admit("retry-gap-request", "failed-attempt")
	usage := UsageRecord{AttemptID: "failed-attempt", Source: "connector", Completeness: "complete", RecordedAt: now}
	if err := ledger.FinalizeAttempt(ctx, TerminalAttempt{AttemptID: "failed-attempt", State: "failed", Usage: usage, FinishedAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE attempts SET error_reason='upstream_timeout' WHERE id='failed-attempt'`); err != nil {
		t.Fatal(err)
	}
	first, err := ledger.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.RequestsAdmitted != 0 || first.RequestsFailed != 2 || first.RequestsInterrupted != 1 || first.RequestsSucceeded != 0 || first.AttemptsFailed != 2 || first.AttemptsInterrupted != 1 || first.AttemptsReserved != 0 || first.AttemptsIntent != 0 {
		t.Fatalf("recovery summary = %+v", first)
	}
	unsent, err := ledger.GetAttempt(ctx, "unsent-attempt")
	if err != nil || unsent.State != "failed" || unsent.ErrorReason == nil || *unsent.ErrorReason != "not_dispatched" {
		t.Fatalf("unsent attempt = %+v, %v", unsent, err)
	}
	request, err := ledger.GetRequest(ctx, "unsent-request")
	if err != nil || request.State != "failed" {
		t.Fatalf("unsent request = %+v, %v", request, err)
	}
	reservation, err := ledger.GetReservation(ctx, "unsent-attempt")
	if err != nil || reservation.State != "released" || reservation.EffectiveCharge != 0 || reservation.ReconciledAt == nil {
		t.Fatalf("released reservation = %+v, %v", reservation, err)
	}
	if got, err := ledger.GetUsage(ctx, "unsent-attempt"); !errors.Is(err, ErrLedgerNotFound) || got.AttemptID != "" {
		t.Fatalf("undispatched usage = %+v, %v", got, err)
	}
	interrupted, err := ledger.GetAttempt(ctx, "intent-attempt")
	if err != nil || interrupted.State != "interrupted" || interrupted.ErrorReason == nil || *interrupted.ErrorReason != "interrupted" {
		t.Fatalf("intent attempt = %+v, %v", interrupted, err)
	}
	request, err = ledger.GetRequest(ctx, "intent-request")
	if err != nil || request.State != "interrupted" {
		t.Fatalf("intent request = %+v, %v", request, err)
	}
	reservation, err = ledger.GetReservation(ctx, "intent-attempt")
	if err != nil || reservation.State != "conservative" || reservation.EffectiveCharge != 40 || reservation.ReconciledAt == nil {
		t.Fatalf("conservative reservation = %+v, %v", reservation, err)
	}
	unknown, err := ledger.GetUsage(ctx, "intent-attempt")
	if err != nil || unknown.Source != "unknown" || unknown.Completeness != "unknown" || unknown.InputTokens != nil || unknown.OutputTokens != nil || unknown.ReasoningTokens != nil || unknown.CachedTokens != nil {
		t.Fatalf("unknown usage = %+v, %v", unknown, err)
	}
	request, err = ledger.GetRequest(ctx, "retry-gap-request")
	if err != nil || request.State != "failed" {
		t.Fatalf("retry-gap request = %+v, %v", request, err)
	}
	stored, err := ledger.GetAttempt(ctx, "failed-attempt")
	if err != nil || stored.State != "failed" || stored.ErrorReason == nil || *stored.ErrorReason != "upstream_timeout" {
		t.Fatalf("last attempt outcome changed: %+v, %v", stored, err)
	}
	var requests, attempts, reservations, usages int64
	for _, q := range []struct {
		query string
		dst   *int64
	}{{`SELECT count(*) FROM requests`, &requests}, {`SELECT count(*) FROM attempts`, &attempts}, {`SELECT count(*) FROM reservations`, &reservations}, {`SELECT count(*) FROM usage_records`, &usages}} {
		if err := db.QueryRowContext(ctx, q.query).Scan(q.dst); err != nil {
			t.Fatal(err)
		}
	}
	second, err := ledger.Recover(ctx)
	if err != nil || second != first {
		t.Fatalf("second recovery summary = %+v, %v; first %+v", second, err, first)
	}
	var requests2, attempts2, reservations2, usages2 int64
	for _, q := range []struct {
		query string
		dst   *int64
	}{{`SELECT count(*) FROM requests`, &requests2}, {`SELECT count(*) FROM attempts`, &attempts2}, {`SELECT count(*) FROM reservations`, &reservations2}, {`SELECT count(*) FROM usage_records`, &usages2}} {
		if err := db.QueryRowContext(ctx, q.query).Scan(q.dst); err != nil {
			t.Fatal(err)
		}
	}
	if requests2 != requests || attempts2 != attempts || reservations2 != reservations || usages2 != usages {
		t.Fatalf("recovery changed row counts: %d/%d/%d/%d => %d/%d/%d/%d", requests, attempts, reservations, usages, requests2, attempts2, reservations2, usages2)
	}
}

func TestProcessLockExcludesGatewayButAllowsSQLiteWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	first, err := AcquireProcessLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := AcquireProcessLock(path); err == nil {
		_ = second.Close()
		t.Fatal("second gateway lock unexpectedly acquired")
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE cli_probe (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("CLI transaction blocked by process lock: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := AcquireProcessLock(path)
	if err != nil {
		t.Fatalf("lock not released on Close: %v", err)
	}
	defer third.Close()
}
