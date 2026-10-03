package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestCoreAdmissionErrorMappingIsNarrow(t *testing.T) {
	limit := errors.Join(sqlite.ErrLedgerConflict, sqlite.ErrAdmissionLimit)
	if got := coreAdmissionError(limit); !errors.Is(got, core.ErrAdmissionLimit) {
		t.Fatalf("limit mapping = %v", got)
	}
	conflict := errors.New("stale authorization conflict")
	wrappedConflict := errors.Join(sqlite.ErrLedgerConflict, conflict)
	if got := coreAdmissionError(wrappedConflict); got != wrappedConflict || errors.Is(got, core.ErrAdmissionLimit) {
		t.Fatalf("non-limit conflict mapping = %v", got)
	}
}

func TestAccountingStoreErrorDistinguishesStorageFailures(t *testing.T) {
	for _, err := range []error{
		sqlite.ErrLedgerConflict,
		sqlite.ErrLedgerNotFound,
		core.ErrAdmissionLimit,
		context.Canceled,
		context.DeadlineExceeded,
	} {
		if got := accountingStoreError(context.Background(), err); got != err {
			t.Errorf("accountingStoreError(%v) = %T, want unchanged business/cancellation error", err, got)
		}
	}
	infrastructure := errors.New("private database detail")
	got := accountingStoreError(context.Background(), infrastructure)
	var failure core.AccountingStorageFailure
	if !errors.As(got, &failure) || !errors.Is(got, infrastructure) || got.Error() != "accounting storage failure" {
		t.Fatalf("infrastructure classification = %#v", got)
	}
}

func TestAccountingStoreErrorNormalizesDriverInterruptFromCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := accountingStoreError(ctx, errors.New("driver interrupted operation"))
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("canceled context + driver interrupt = %T %v, want context.Canceled", got, got)
	}
	var failure core.AccountingStorageFailure
	if errors.As(got, &failure) {
		t.Fatalf("canceled driver interrupt was marked as storage failure: %#v", got)
	}
}

func TestSQLiteAccountingStorePersistsFallbackAttemptWithoutSecondRPM(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "fallback.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	policy, err := sqlite.NewKeyPolicies(db).Create(ctx, sqlite.CreateKeyPolicyParams{
		ID: "policy", Models: []string{"model"}, Connectors: []string{"connector-a", "connector-b"}, RPM: 1, TPM: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	accounts := sqlite.NewAccounts(db)
	for _, target := range []struct{ id, connector string }{{"account-a", "connector-a"}, {"account-b", "connector-b"}} {
		if _, err := accounts.Create(ctx, sqlite.Account{ID: target.id, Connector: target.connector, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	store := sqliteAccountingStore{ledger: sqlite.NewLedger(db)}
	base := core.AccountingAdmission{
		RequestID: "request", KeyID: key.ID, KeyRevision: 1, PolicyID: policy.ID, PolicyRevision: policy.Revision,
		Protocol: "protocol", Model: "model", RouteID: "route", EstimateTokens: 5, EstimateMethod: "fixture", BudgetPolicy: "known",
	}
	first := base
	first.AttemptID, first.AccountID, first.Connector = "attempt-a", "account-a", "connector-a"
	if err := store.Admit(ctx, first); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.FinalizeAttempt(ctx, core.AccountingTerminal{
		AttemptID: first.AttemptID, Outcome: core.OutcomeFailed, Usage: core.UsageReport{Source: core.UsageUnknown, Completeness: core.UsageUnknownCompleteness},
		Category: core.CategoryUnavailable, Reason: "safe_rejection", EndedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	second := base
	second.AttemptID, second.AccountID, second.Connector = "attempt-b", "account-b", "connector-b"
	if err := store.BeginAttempt(ctx, second); err != nil {
		t.Fatalf("fallback attempt repeated request RPM admission: %v", err)
	}
	if err := store.RecordDispatchIntent(ctx, second.AttemptID, now.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	input, output := int64(4), int64(6)
	if err := store.FinalizeAttempt(ctx, core.AccountingTerminal{
		AttemptID: second.AttemptID, Outcome: core.OutcomeSucceeded, Committed: true,
		Usage: core.UsageReport{InputTokens: &input, OutputTokens: &output, Source: core.UsageProvider, Completeness: core.UsageComplete}, EndedAt: now.Add(2 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	request, err := store.ledger.GetRequest(ctx, first.RequestID)
	if err != nil || request.ID != first.RequestID {
		t.Fatalf("logical request record = %+v, %v", request, err)
	}
	attempts, err := store.ledger.QueryAttempts(ctx, sqlite.AttemptFilter{RequestID: first.RequestID, Limit: 10})
	if err != nil || len(attempts) != 2 {
		t.Fatalf("durable attempts = %d, %v", len(attempts), err)
	}
	if attempts[0].Attempt.ID == attempts[1].Attempt.ID || attempts[0].Attempt.Ordinal != 1 || attempts[1].Attempt.Ordinal != 2 {
		t.Fatalf("attempt identities/ordinals = %+v / %+v", attempts[0].Attempt, attempts[1].Attempt)
	}
	for _, attemptID := range []string{first.AttemptID, second.AttemptID} {
		reservation, err := store.ledger.GetReservation(ctx, attemptID)
		if err != nil || reservation.State == "held" {
			t.Fatalf("reservation %q was not reconciled: %+v, %v", attemptID, reservation, err)
		}
	}
	firstReservation, err := store.ledger.GetReservation(ctx, first.AttemptID)
	if err != nil || firstReservation.State != "released" || firstReservation.EffectiveCharge != 0 {
		t.Fatalf("undispatched safe failure reservation = %+v, %v", firstReservation, err)
	}
	secondReservation, err := store.ledger.GetReservation(ctx, second.AttemptID)
	if err != nil || secondReservation.State != "settled" || secondReservation.EffectiveCharge != 10 {
		t.Fatalf("exact fallback settlement = %+v, %v", secondReservation, err)
	}
	partialInput := int64(3)
	third := base
	third.AttemptID, third.AccountID, third.Connector = "attempt-c", "account-a", "connector-a"
	third.EstimateTokens = 11
	if err := store.BeginAttempt(ctx, third); !errors.Is(err, core.ErrAdmissionLimit) {
		t.Fatalf("fallback beyond remaining TPM = %v, want admission limit", err)
	}
	if _, err := store.ledger.GetReservation(ctx, third.AttemptID); !errors.Is(err, sqlite.ErrLedgerNotFound) {
		t.Fatalf("rejected fallback left reservation: %v", err)
	}
	third.AttemptID = "attempt-d"
	third.EstimateTokens = 5
	if err := store.BeginAttempt(ctx, third); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDispatchIntent(ctx, third.AttemptID, now.Add(3*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	terminal := core.AccountingTerminal{
		AttemptID: third.AttemptID, Outcome: core.OutcomeIncomplete, Committed: true,
		Usage: core.UsageReport{InputTokens: &partialInput, Source: core.UsageProvider, Completeness: core.UsagePartial}, EndedAt: now.Add(4 * time.Millisecond),
	}
	start := make(chan struct{})
	errs := make(chan error, 8)
	for i := 0; i < cap(errs); i++ {
		go func() { <-start; errs <- store.FinalizeAttempt(ctx, terminal) }()
	}
	close(start)
	for i := 0; i < cap(errs); i++ {
		if err := <-errs; err != nil {
			t.Fatalf("idempotent competing finalization: %v", err)
		}
	}
	thirdReservation, err := store.ledger.GetReservation(ctx, third.AttemptID)
	if err != nil || thirdReservation.State != "conservative" || thirdReservation.EffectiveCharge != 5 {
		t.Fatalf("partial fallback settlement = %+v, %v", thirdReservation, err)
	}
	usage, err := store.ledger.GetUsage(ctx, third.AttemptID)
	if err != nil || usage.InputTokens == nil || *usage.InputTokens != partialInput || usage.OutputTokens != nil {
		t.Fatalf("partial fallback usage record = %+v, %v", usage, err)
	}
	unknown := base
	unknown.AttemptID, unknown.AccountID, unknown.Connector = "attempt-e", "account-b", "connector-b"
	if err := store.BeginAttempt(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDispatchIntent(ctx, unknown.AttemptID, now.Add(5*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeAttempt(ctx, core.AccountingTerminal{
		AttemptID: unknown.AttemptID, Outcome: core.OutcomeIncomplete, Committed: true,
		Usage: core.UsageReport{Source: core.UsageUnknown, Completeness: core.UsageUnknownCompleteness}, EndedAt: now.Add(6 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	unknownReservation, err := store.ledger.GetReservation(ctx, unknown.AttemptID)
	if err != nil || unknownReservation.State != "conservative" || unknownReservation.EffectiveCharge != 5 {
		t.Fatalf("unknown fallback settlement = %+v, %v", unknownReservation, err)
	}
	secondRequest := base
	secondRequest.RequestID, secondRequest.AttemptID = "request-rpm-check", "attempt-rpm-check"
	secondRequest.AccountID, secondRequest.Connector = "account-a", "connector-a"
	if err := store.Admit(ctx, secondRequest); !errors.Is(err, core.ErrAdmissionLimit) {
		t.Fatalf("second client request under RPM=1 = %v, want admission limit", err)
	}
	exhausted := base
	exhausted.AttemptID, exhausted.AccountID, exhausted.Connector = "attempt-f", "account-a", "connector-a"
	exhausted.EstimateTokens = 1
	if err := store.BeginAttempt(ctx, exhausted); !errors.Is(err, core.ErrAdmissionLimit) {
		t.Fatalf("fallback after exhausted budget = %v, want admission limit", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger := sqlite.NewLedger(db)
	request, err = ledger.GetRequest(ctx, first.RequestID)
	if err != nil || request.ID != first.RequestID {
		t.Fatalf("reopened fallback request = %+v, %v", request, err)
	}
	for _, id := range []string{first.AttemptID, second.AttemptID, third.AttemptID, unknown.AttemptID} {
		reservation, err := ledger.GetReservation(ctx, id)
		if err != nil || reservation.State == "held" {
			t.Fatalf("reopened fallback reservation %q = %+v, %v", id, reservation, err)
		}
	}
	if _, err := ledger.GetRequest(ctx, secondRequest.RequestID); !errors.Is(err, sqlite.ErrLedgerNotFound) {
		t.Fatalf("RPM-rejected request persisted: %v", err)
	}
}
