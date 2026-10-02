package sqlite

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

func TestSettlementReconcilesTerminalAttempts(t *testing.T) {
	tests := []struct {
		name       string
		dispatched bool
		complete   string
		input      *int64
		output     *int64
		state      string
		actual     *int64
		charge     int64
	}{
		{name: "complete exact", dispatched: true, complete: "complete", input: tokenPtr(7), output: tokenPtr(5), state: "settled", actual: tokenPtr(12), charge: 12},
		{name: "overage", dispatched: true, complete: "complete", input: tokenPtr(70), output: tokenPtr(50), state: "settled", actual: tokenPtr(120), charge: 120},
		{name: "zero", dispatched: true, complete: "complete", input: tokenPtr(0), output: tokenPtr(0), state: "settled", actual: tokenPtr(0), charge: 0},
		{name: "partial lower bound", dispatched: true, complete: "partial", input: tokenPtr(25), state: "conservative", charge: 25},
		{name: "unknown", dispatched: true, complete: "unknown", state: "conservative", charge: 20},
		{name: "undispatched", complete: "unknown", input: tokenPtr(8), state: "released", charge: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ledger, ctx, key, policy := admissionFixture(t, 4, 1000)
			now := time.Date(2026, 10, 3, 12, 0, 0, 123000000, time.UTC)
			ledger.now = func() time.Time { return now }
			q, a, r := admissionRecords(key, policy, "request", "attempt", 20)
			if err := ledger.Admit(ctx, q, a, r); err != nil {
				t.Fatal(err)
			}
			if tc.dispatched {
				if err := ledger.RecordDispatchIntent(ctx, a.ID, now); err != nil {
					t.Fatal(err)
				}
			}
			usage := UsageRecord{AttemptID: a.ID, InputTokens: tc.input, OutputTokens: tc.output, ReasoningTokens: tokenPtr(30), CachedTokens: tokenPtr(40), Source: "provider", Completeness: tc.complete, RecordedAt: now}
			terminal := TerminalAttempt{AttemptID: a.ID, State: "cancelled", Usage: usage, FinishedAt: now.Add(time.Second)}
			if err := ledger.FinalizeAttempt(ctx, terminal); err != nil {
				t.Fatal(err)
			}
			got, err := ledger.GetReservation(ctx, a.ID)
			if err != nil || got.State != tc.state || got.EffectiveCharge != tc.charge || !ptrEqual(got.ActualTokens, tc.actual) || got.ReconciledAt == nil || !got.ReconciledAt.Equal(now) {
				t.Fatalf("reservation = %#v, %v", got, err)
			}
			if err := ledger.FinalizeAttempt(ctx, terminal); err != nil {
				t.Fatalf("duplicate finalization: %v", err)
			}
			after, err := ledger.GetReservation(ctx, a.ID)
			if err != nil || after.ReconciledAt == nil || !after.ReconciledAt.Equal(now) || after.EffectiveCharge != tc.charge {
				t.Fatalf("duplicate altered reservation: %#v, %v", after, err)
			}
			conflict := terminal
			conflict.State = "failed"
			if err := ledger.FinalizeAttempt(ctx, conflict); !errors.Is(err, ErrLedgerConflict) {
				t.Fatalf("conflicting finalization = %v", err)
			}
		})
	}
}

func TestSettlementCompetingTerminalRace(t *testing.T) {
	ledger, ctx, key, policy := admissionFixture(t, 4, 1000)
	q, a, r := admissionRecords(key, policy, "request", "attempt", 20)
	if err := ledger.Admit(ctx, q, a, r); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := ledger.RecordDispatchIntent(ctx, a.ID, now); err != nil {
		t.Fatal(err)
	}
	usage := UsageRecord{AttemptID: a.ID, InputTokens: tokenPtr(2), OutputTokens: tokenPtr(3), Source: "provider", Completeness: "complete", RecordedAt: now}
	terminals := []TerminalAttempt{
		{AttemptID: a.ID, State: "succeeded", Usage: usage, FinishedAt: now},
		{AttemptID: a.ID, State: "cancelled", Usage: usage, FinishedAt: now},
	}
	start := make(chan struct{})
	results := make(chan error, len(terminals))
	var wg sync.WaitGroup
	for _, terminal := range terminals {
		terminal := terminal
		wg.Go(func() {
			<-start
			results <- ledger.FinalizeAttempt(ctx, terminal)
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var succeeded, conflicted int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrLedgerConflict):
			conflicted++
		default:
			t.Fatalf("competing finalization: %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("successes=%d conflicts=%d; want exactly one each", succeeded, conflicted)
	}
	got, err := ledger.GetReservation(ctx, a.ID)
	if err != nil || got.State != "settled" || got.EffectiveCharge != 5 || got.ReconciledAt == nil {
		t.Fatalf("competing finalization reservation = %#v, %v", got, err)
	}
}

func TestSettlementOverflowAndRollbackRetainHeldReservation(t *testing.T) {
	ledger, ctx, key, policy := admissionFixture(t, 4, math.MaxInt64)
	q, a, r := admissionRecords(key, policy, "request", "attempt", 20)
	if err := ledger.Admit(ctx, q, a, r); err != nil {
		t.Fatal(err)
	}
	if err := ledger.RecordDispatchIntent(ctx, a.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	usage := UsageRecord{AttemptID: a.ID, InputTokens: tokenPtr(math.MaxInt64), OutputTokens: tokenPtr(1), Source: "provider", Completeness: "complete", RecordedAt: time.Now()}
	terminal := TerminalAttempt{AttemptID: a.ID, State: "succeeded", Usage: usage, FinishedAt: time.Now()}
	if err := ledger.FinalizeAttempt(ctx, terminal); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("overflow finalization = %v", err)
	}
	if _, err := ledger.db.ExecContext(ctx, `CREATE TRIGGER fail_reconcile BEFORE UPDATE ON reservations BEGIN SELECT RAISE(ABORT,'injected reconcile failure'); END`); err != nil {
		t.Fatal(err)
	}
	usage.InputTokens, usage.OutputTokens, usage.Completeness = tokenPtr(3), tokenPtr(4), "complete"
	terminal.Usage = usage
	if err := ledger.FinalizeAttempt(ctx, terminal); err == nil {
		t.Fatal("injected transaction failure succeeded")
	}
	got, err := ledger.GetAttempt(ctx, a.ID)
	if err != nil || got.State != "intent" {
		t.Fatalf("attempt after rollback = %#v, %v", got, err)
	}
	reservation, err := ledger.GetReservation(ctx, a.ID)
	if err != nil || reservation.State != "held" || reservation.ReconciledAt != nil || reservation.EffectiveCharge != 0 {
		t.Fatalf("reservation after rollback = %#v, %v", reservation, err)
	}
}

func tokenPtr(v int64) *int64 { return &v }
