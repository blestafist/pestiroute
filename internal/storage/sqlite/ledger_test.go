package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLedgerLifecycleIdempotencyNullableUsageAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 12, 30, 0, 123456000, time.UTC).Truncate(time.Millisecond)
	policy, err := NewKeyPolicies(db).Create(ctx, CreateKeyPolicyParams{ID: "policy"})
	if err != nil {
		t.Fatal(err)
	}
	issued, err := NewVirtualKeys(db).Create(ctx, CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	account, err := NewAccounts(db).Create(ctx, Account{ID: "account", Connector: "connector", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewLedger(db)
	request := RequestRecord{ID: "request", VirtualKeyID: issued.ID, KeyRevision: 1, PolicyID: policy.ID, PolicyRevision: policy.Revision, AcceptedAt: now, Protocol: "responses", Model: "model", RouteID: "route", State: "admitted"}
	if err := repo.CreateRequest(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateRequest(ctx, request); err != nil {
		t.Fatalf("identical begin: %v", err)
	}
	conflict := request
	conflict.Model = "other"
	if err := repo.CreateRequest(ctx, conflict); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("conflicting begin = %v", err)
	}
	attempt := AttemptRecord{ID: "attempt", RequestID: request.ID, Ordinal: 1, AccountID: account.ID, Connector: "connector", RouteID: "route", BudgetPolicy: "known", EstimateTokens: 20, EstimateMethod: "fixture", State: "reserved"}
	if err := repo.CreateAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateAttempt(ctx, attempt); err != nil {
		t.Fatalf("identical attempt: %v", err)
	}
	duplicateOrdinal := attempt
	duplicateOrdinal.ID = "attempt-duplicate-ordinal"
	if err := repo.CreateAttempt(ctx, duplicateOrdinal); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("duplicate request ordinal = %v", err)
	}
	if err := repo.RecordDispatchIntent(ctx, attempt.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordDispatchIntent(ctx, attempt.ID, now); err != nil {
		t.Fatalf("identical intent: %v", err)
	}
	inTokens, cachedTokens := int64(17), int64(4)
	usage := UsageRecord{AttemptID: attempt.ID, InputTokens: &inTokens, CachedTokens: &cachedTokens, Source: "provider", Completeness: "complete", RecordedAt: now}
	finished := now.Add(time.Second)
	terminal := TerminalAttempt{AttemptID: attempt.ID, State: "succeeded", Committed: true, Usage: usage, FinishedAt: finished}
	const concurrentFinalizers = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, concurrentFinalizers)
	for range concurrentFinalizers {
		wg.Go(func() {
			<-start
			errCh <- repo.FinalizeAttempt(ctx, terminal)
		})
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Errorf("concurrent identical finalize: %v", err)
		}
	}
	if err := repo.FinalizeAttempt(ctx, terminal); err != nil {
		t.Fatalf("identical finalize: %v", err)
	}
	conflicting := terminal
	conflicting.State = "failed"
	if err := repo.FinalizeAttempt(ctx, conflicting); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("conflicting finalize = %v", err)
	}
	var usageCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_records WHERE attempt_id=?`, attempt.ID).Scan(&usageCount); err != nil || usageCount != 1 {
		t.Fatalf("usage rows after concurrent identical finalization = %d, %v", usageCount, err)
	}
	if got, err := repo.GetRequest(ctx, request.ID); err != nil || !got.AcceptedAt.Equal(now) || got.VirtualKeyID != issued.ID {
		t.Fatalf("request = %#v, %v", got, err)
	}
	if got, err := repo.GetAttempt(ctx, attempt.ID); err != nil || got.State != "succeeded" || !got.Committed || got.AccountID != account.ID || got.DispatchedAt == nil {
		t.Fatalf("attempt = %#v, %v", got, err)
	}
	got, err := repo.GetUsage(ctx, attempt.ID)
	if err != nil || got.InputTokens == nil || *got.InputTokens != inTokens || got.OutputTokens != nil || got.ReasoningTokens != nil || got.CachedTokens == nil || *got.CachedTokens != cachedTokens || !got.RecordedAt.Equal(now) {
		t.Fatalf("usage with nullable counters = %#v, %v", got, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo = NewLedger(db)
	if got, err := repo.GetRequest(ctx, request.ID); err != nil || got.Model != request.Model || !got.AcceptedAt.Equal(now) {
		t.Fatalf("reopened request = %#v, %v", got, err)
	}
	if got, err := repo.GetAttempt(ctx, attempt.ID); err != nil || got.State != "succeeded" || !got.Committed || got.AccountID != account.ID || got.FinishedAt == nil || !got.FinishedAt.Equal(finished) {
		t.Fatalf("reopened attempt = %#v, %v", got, err)
	}
	got, err = repo.GetUsage(ctx, attempt.ID)
	if err != nil || got.Source != "provider" || !got.RecordedAt.Equal(now) || got.InputTokens == nil || *got.InputTokens != inTokens || got.CachedTokens == nil || *got.CachedTokens != cachedTokens {
		t.Fatalf("reopened usage = %#v, %v", got, err)
	}
	category, reason := "timeout", "upstream_timeout"
	second := AttemptRecord{ID: "attempt-2", RequestID: request.ID, Ordinal: 2, AccountID: account.ID, Connector: "connector", RouteID: "route", BudgetPolicy: "known", EstimateTokens: 20, EstimateMethod: "fixture", State: "reserved"}
	if err := repo.CreateAttempt(ctx, second); err != nil {
		t.Fatal(err)
	}
	secondUsage := UsageRecord{AttemptID: second.ID, Source: "unknown", Completeness: "unknown", RecordedAt: now}
	if err := repo.FinalizeAttempt(ctx, TerminalAttempt{AttemptID: second.ID, State: "failed", ErrorCategory: &category, ErrorReason: &reason, Usage: secondUsage, FinishedAt: finished}); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.GetAttempt(ctx, second.ID)
	if err != nil || stored.ErrorCategory == nil || *stored.ErrorCategory != category || stored.ErrorReason == nil || *stored.ErrorReason != reason {
		t.Fatalf("terminal sanitized error metadata = %#v, %v", stored, err)
	}
}

func TestLedgerForeignKeysAndMissingRecords(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	repo := NewLedger(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	err = repo.CreateRequest(ctx, RequestRecord{ID: "orphan", KeyRevision: 1, PolicyID: "missing", PolicyRevision: 1, AcceptedAt: now, Protocol: "p", Model: "m", RouteID: "r", State: "admitted"})
	if err == nil {
		t.Fatal("expected policy foreign-key failure")
	}
	if _, err := repo.GetRequest(ctx, "missing"); !errors.Is(err, ErrLedgerNotFound) {
		t.Fatalf("missing request = %v", err)
	}
	if err := repo.RecordDispatchIntent(ctx, "missing", now); !errors.Is(err, ErrLedgerNotFound) {
		t.Fatalf("missing attempt = %v", err)
	}
}
