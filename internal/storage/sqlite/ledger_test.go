package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLedgerQuerySummaryPaginationAndLifecycle(t *testing.T) {
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
	if _, err := db.ExecContext(ctx, `INSERT INTO reservations (attempt_id,estimated_tokens,state) VALUES (?,?,'held')`, attempt.ID, attempt.EstimateTokens); err != nil {
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
	if err := repo.RecordDispatchIntent(ctx, attempt.ID, now); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("terminal attempt intent = %v", err)
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
	second := AttemptRecord{ID: "attempt-uncertain", RequestID: request.ID, Ordinal: 2, AccountID: account.ID, Connector: "connector", RouteID: "route", BudgetPolicy: "known", EstimateTokens: 20, EstimateMethod: "fixture", State: "reserved"}
	if err := repo.CreateAttempt(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO reservations (attempt_id,estimated_tokens,state) VALUES (?,?,'held')`, second.ID, second.EstimateTokens); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := repo.RecordDispatchIntent(cancelled, second.ID, now); err == nil {
		t.Fatal("cancelled intent unexpectedly succeeded")
	}
	if got, err := repo.GetAttempt(ctx, second.ID); err != nil || got.State != "reserved" || got.DispatchedAt != nil {
		t.Fatalf("cancelled intent changed attempt: %#v, %v", got, err)
	}
	if err := repo.RecordDispatchIntent(ctx, second.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordDispatchIntent(ctx, second.ID, now.Add(time.Millisecond)); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("conflicting intent timestamp = %v", err)
	}
	third := AttemptRecord{ID: "attempt-2", RequestID: request.ID, Ordinal: 3, AccountID: account.ID, Connector: "connector", RouteID: "route", BudgetPolicy: "known", EstimateTokens: 20, EstimateMethod: "fixture", State: "reserved"}
	if err := repo.CreateAttempt(ctx, third); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO reservations (attempt_id,estimated_tokens,state) VALUES (?,?,'held')`, third.ID, third.EstimateTokens); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE requests SET state='failed' WHERE id=?`, request.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordDispatchIntent(ctx, third.ID, now); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("intent for non-admitted request = %v", err)
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
	if got, err := repo.GetAttempt(ctx, second.ID); err != nil || got.State != "intent" || got.DispatchedAt == nil || !got.DispatchedAt.Equal(now) {
		t.Fatalf("reopened dispatch intent = %#v, %v", got, err)
	}
	if got, err := repo.GetAttempt(ctx, third.ID); err != nil || got.State != "reserved" || got.DispatchedAt != nil {
		t.Fatalf("reopened undispatched attempt = %#v, %v", got, err)
	}
	got, err = repo.GetUsage(ctx, attempt.ID)
	if err != nil || got.Source != "provider" || !got.RecordedAt.Equal(now) || got.InputTokens == nil || *got.InputTokens != inTokens || got.CachedTokens == nil || *got.CachedTokens != cachedTokens {
		t.Fatalf("reopened usage = %#v, %v", got, err)
	}
	category, reason := "timeout", "upstream_timeout"
	secondUsage := UsageRecord{AttemptID: third.ID, Source: "unknown", Completeness: "unknown", RecordedAt: now}
	if err := repo.FinalizeAttempt(ctx, TerminalAttempt{AttemptID: third.ID, State: "failed", ErrorCategory: &category, ErrorReason: &reason, Usage: secondUsage, FinishedAt: finished}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE requests SET state='admitted',finished_at=NULL WHERE id=?`, request.ID); err != nil {
		t.Fatal(err)
	}
	pending := AttemptRecord{ID: "attempt-pending", RequestID: request.ID, Ordinal: 4, AccountID: account.ID, Connector: "connector", RouteID: "route", BudgetPolicy: "known", EstimateTokens: 20, EstimateMethod: "fixture", State: "reserved"}
	if err := repo.CreateAttempt(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO reservations (attempt_id,estimated_tokens,state) VALUES (?,?,'held')`, pending.ID, pending.EstimateTokens); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.GetAttempt(ctx, third.ID)
	if err != nil || stored.ErrorCategory == nil || *stored.ErrorCategory != category || stored.ErrorReason == nil || *stored.ErrorReason != reason {
		t.Fatalf("terminal sanitized error metadata = %#v, %v", stored, err)
	}
	since, until := now.Add(-time.Second), now.Add(time.Second)
	requests, err := repo.QueryRequests(ctx, RequestFilter{VirtualKeyID: issued.ID, AccountID: account.ID, Model: request.Model, RouteID: request.RouteID, State: "admitted", Since: &since, Until: &until, Limit: 1})
	if err != nil || len(requests) != 1 || requests[0].Request.ID != request.ID || len(requests[0].Attempts) != 4 {
		t.Fatalf("filtered paginated requests = %#v err=%v", requests, err)
	}
	attempts, err := repo.QueryAttempts(ctx, AttemptFilter{RequestID: request.ID, AccountID: account.ID, Connector: "connector", State: "failed", Since: &finished, Until: &finished, Limit: 2})
	if err != nil || len(attempts) != 1 || attempts[0].Attempt.Ordinal != 3 || attempts[0].Attempt.ErrorReason == nil || *attempts[0].Attempt.ErrorReason != reason || attempts[0].Usage == nil || attempts[0].Usage.InputTokens != nil {
		t.Fatalf("filtered paginated attempts = %#v, %v", attempts, err)
	}
	attempts, err = repo.QueryAttempts(ctx, AttemptFilter{RequestID: request.ID, Since: &now, Until: &now, Limit: 2, Offset: 1})
	if err != nil || len(attempts) != 2 || attempts[0].Attempt.Ordinal != 4 || attempts[1].Attempt.Ordinal != 2 {
		t.Fatalf("timestamp/offset attempt page = %#v, %v", attempts, err)
	}
	repeated, err := repo.QueryAttempts(ctx, AttemptFilter{RequestID: request.ID, Since: &now, Until: &now, Limit: 2, Offset: 1})
	if err != nil || len(repeated) != 2 || repeated[0].Attempt.ID != attempts[0].Attempt.ID || repeated[1].Attempt.ID != attempts[1].Attempt.ID {
		t.Fatalf("repeated offset page = %#v, %v", repeated, err)
	}
	summary, err := repo.QueryUsageSummary(ctx, issued.ID, account.ID, &since, &until)
	if err != nil || summary.Requests != 1 || summary.Attempts != 4 || summary.InputTokens != nil || summary.OutputTokens != nil || summary.EstimatedTokens != 80 || summary.EffectiveCharge != 20 {
		t.Fatalf("usage summary = %#v, err=%v", summary, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE usage_records SET input_tokens=9223372036854775807 WHERE attempt_id=?`, attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE usage_records SET input_tokens=1 WHERE attempt_id=?`, third.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.QueryUsageSummary(ctx, issued.ID, account.ID, &since, &until); err == nil {
		t.Fatal("overflowing input token aggregate unexpectedly succeeded")
	}
	if _, err := repo.QueryRequests(ctx, RequestFilter{Limit: 501}); err == nil {
		t.Fatal("oversized request page accepted")
	}
	if _, err := NewVirtualKeys(db).Revoke(ctx, issued.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAccounts(db).SetEnabled(ctx, account.ID, false); err != nil {
		t.Fatal(err)
	}
	history, err := repo.QueryRequests(ctx, RequestFilter{VirtualKeyID: issued.ID})
	if err != nil || len(history) != 1 || history[0].Request.ID != request.ID || len(history[0].Attempts) != 4 {
		t.Fatalf("revoked-key/disabled-account history = %#v, %v", history, err)
	}
	if err := NewAccounts(db).Delete(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	history, err = repo.QueryRequests(ctx, RequestFilter{VirtualKeyID: issued.ID})
	if err != nil || len(history) != 1 || len(history[0].Attempts) != 4 || history[0].Attempts[0].Attempt.AccountID != "" {
		t.Fatalf("deleted-account history = %#v, %v", history, err)
	}
	for ordinal := int64(5); ordinal <= 504; ordinal++ {
		id := fmt.Sprintf("pending-%03d", ordinal)
		if _, err := db.ExecContext(ctx, `INSERT INTO attempts (id,request_id,ordinal,connector,route_id,budget_policy,estimate_tokens,estimate_method,state,committed) VALUES (?,?,?,?,?,?,0,?,'reserved',0)`, id, request.ID, ordinal, "connector", "route", "known", "fixture"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO reservations (attempt_id,estimated_tokens,state) VALUES (?,0,'held')`, id); err != nil {
			t.Fatal(err)
		}
	}
	history, err = repo.QueryRequests(ctx, RequestFilter{VirtualKeyID: issued.ID, Limit: 1})
	count := 0
	if len(history) == 1 {
		count = len(history[0].Attempts)
	}
	if err != nil || len(history) != 1 || count != 504 {
		t.Fatalf("paged child attempt enumeration count=%d err=%v", count, err)
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
