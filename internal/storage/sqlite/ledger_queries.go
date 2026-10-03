package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type RequestFilter struct {
	VirtualKeyID, Model, RouteID, State string
	AccountID                           string
	Since, Until                        *time.Time
	Limit, Offset                       int
}

type AttemptFilter struct {
	RequestID, AccountID, Connector, State string
	Since, Until                           *time.Time
	Limit, Offset                          int
}

type RequestUsage struct {
	Request  RequestRecord
	Attempts []AttemptUsage
}

type AttemptUsage struct {
	Attempt     AttemptRecord
	Usage       *UsageRecord
	Reservation ReservationRecord
}

type UsageSummary struct {
	Requests, Attempts               int64
	InputTokens, OutputTokens        *int64
	ReasoningTokens, CachedTokens    *int64
	EstimatedTokens, EffectiveCharge int64
	ActualTokens                     *int64
}

// Attempts without dispatch or finish timestamps use their parent request's
// admission time, so pending/undispatched rows have one stable event time too.
const attemptEventTime = "COALESCE(a.dispatched_at,a.finished_at,q.accepted_at)"

func queryPage(limit, offset int) (int, int, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 500 || offset < 0 {
		return 0, 0, fmt.Errorf("invalid ledger pagination")
	}
	return limit, offset, nil
}

func (r *Ledger) QueryRequests(ctx context.Context, f RequestFilter) ([]RequestUsage, error) {
	limit, offset, err := queryPage(f.Limit, f.Offset)
	if err != nil {
		return nil, err
	}
	where, args := []string{"1=1"}, []any{}
	add := func(expr string, value any) { where = append(where, expr); args = append(args, value) }
	if f.VirtualKeyID != "" {
		add("q.virtual_key_id=?", f.VirtualKeyID)
	}
	if f.Model != "" {
		add("q.model=?", f.Model)
	}
	if f.RouteID != "" {
		add("q.route_id=?", f.RouteID)
	}
	if f.State != "" {
		add("q.state=?", f.State)
	}
	if f.AccountID != "" {
		add("EXISTS (SELECT 1 FROM attempts a WHERE a.request_id=q.id AND a.account_id=?)", f.AccountID)
	}
	if f.Since != nil {
		add("q.accepted_at>=?", millis(*f.Since))
	}
	if f.Until != nil {
		add("q.accepted_at<=?", millis(*f.Until))
	}
	args = append(args, limit, offset)
	rows, err := r.db.QueryContext(ctx, `SELECT q.id,COALESCE(q.virtual_key_id,''),q.policy_id,q.protocol,q.model,q.route_id,q.key_revision,q.policy_revision,q.accepted_at,q.state,q.finished_at
		FROM requests q WHERE `+strings.Join(where, " AND ")+` ORDER BY q.accepted_at ASC,q.id ASC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("query requests: %w", err)
	}
	defer rows.Close()
	var result []RequestUsage
	for rows.Next() {
		var item RequestUsage
		var accepted int64
		var finished sql.NullInt64
		q := &item.Request
		if err := rows.Scan(&q.ID, &q.VirtualKeyID, &q.PolicyID, &q.Protocol, &q.Model, &q.RouteID, &q.KeyRevision, &q.PolicyRevision, &accepted, &q.State, &finished); err != nil {
			return nil, err
		}
		q.AcceptedAt = fromUnixMillis(accepted)
		if finished.Valid {
			t := fromUnixMillis(finished.Int64)
			q.FinishedAt = &t
		}
		// Page child attempts so a request with more than one page is never
		// returned with a silently truncated attempt breakdown.
		for offset := 0; ; offset += 500 {
			page, err := r.queryAttempts(ctx, AttemptFilter{RequestID: q.ID}, 500, offset)
			if err != nil {
				return nil, err
			}
			item.Attempts = append(item.Attempts, page...)
			if len(page) < 500 {
				break
			}
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *Ledger) QueryAttempts(ctx context.Context, f AttemptFilter) ([]AttemptUsage, error) {
	limit, offset, err := queryPage(f.Limit, f.Offset)
	if err != nil {
		return nil, err
	}
	return r.queryAttempts(ctx, f, limit, offset)
}

func (r *Ledger) queryAttempts(ctx context.Context, f AttemptFilter, page ...int) ([]AttemptUsage, error) {
	limit, offset := 500, 0
	if len(page) == 2 {
		limit, offset = page[0], page[1]
	}
	where, args := []string{"1=1"}, []any{}
	add := func(expr string, value any) { where = append(where, expr); args = append(args, value) }
	if f.RequestID != "" {
		add("a.request_id=?", f.RequestID)
	}
	if f.AccountID != "" {
		add("a.account_id=?", f.AccountID)
	}
	if f.Connector != "" {
		add("a.connector=?", f.Connector)
	}
	if f.State != "" {
		add("a.state=?", f.State)
	}
	if f.Since != nil {
		add(attemptEventTime+">=?", millis(*f.Since))
	}
	if f.Until != nil {
		add(attemptEventTime+"<=?", millis(*f.Until))
	}
	args = append(args, limit, offset)
	rows, err := r.db.QueryContext(ctx, `SELECT a.id,a.request_id,a.account_id,a.connector,a.route_id,a.budget_policy,a.estimate_method,a.ordinal,a.estimate_tokens,a.state,a.committed,a.error_category,a.error_reason,a.dispatched_at,a.finished_at,
		u.attempt_id,u.input_tokens,u.output_tokens,u.reasoning_tokens,u.cached_tokens,u.source,u.completeness,u.recorded_at,
		r.attempt_id,r.estimated_tokens,r.actual_tokens,r.effective_charge,r.state,r.reconciled_at
		FROM attempts a JOIN requests q ON q.id=a.request_id LEFT JOIN usage_records u ON u.attempt_id=a.id JOIN reservations r ON r.attempt_id=a.id WHERE `+strings.Join(where, " AND ")+` ORDER BY `+attemptEventTime+` ASC,a.id ASC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("query attempts: %w", err)
	}
	defer rows.Close()
	var result []AttemptUsage
	for rows.Next() {
		var item AttemptUsage
		a := &item.Attempt
		var account, category, reason sql.NullString
		var committed int64
		var dispatched, finished sql.NullInt64
		var uid, source, complete sql.NullString
		var in, out, reasoning, cached, recorded sql.NullInt64
		var rid sql.NullString
		var actual, reconciled sql.NullInt64
		var effective sql.NullInt64
		if err := rows.Scan(&a.ID, &a.RequestID, &account, &a.Connector, &a.RouteID, &a.BudgetPolicy, &a.EstimateMethod, &a.Ordinal, &a.EstimateTokens, &a.State, &committed, &category, &reason, &dispatched, &finished,
			&uid, &in, &out, &reasoning, &cached, &source, &complete, &recorded, &rid, &item.Reservation.EstimatedTokens, &actual, &effective, &item.Reservation.State, &reconciled); err != nil {
			return nil, err
		}
		a.AccountID = account.String
		a.Committed = committed != 0
		if category.Valid {
			a.ErrorCategory = &category.String
		}
		if reason.Valid {
			a.ErrorReason = &reason.String
		}
		if dispatched.Valid {
			t := fromUnixMillis(dispatched.Int64)
			a.DispatchedAt = &t
		}
		if finished.Valid {
			t := fromUnixMillis(finished.Int64)
			a.FinishedAt = &t
		}
		item.Reservation.AttemptID = rid.String
		item.Reservation.ActualTokens = nullIntPtr(actual)
		if effective.Valid {
			item.Reservation.EffectiveCharge = effective.Int64
		}
		if reconciled.Valid {
			t := fromUnixMillis(reconciled.Int64)
			item.Reservation.ReconciledAt = &t
		}
		if uid.Valid {
			item.Usage = &UsageRecord{AttemptID: uid.String, InputTokens: nullIntPtr(in), OutputTokens: nullIntPtr(out), ReasoningTokens: nullIntPtr(reasoning), CachedTokens: nullIntPtr(cached), Source: source.String, Completeness: complete.String, RecordedAt: fromUnixMillis(recorded.Int64)}
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *Ledger) QueryUsageSummary(ctx context.Context, keyID, accountID string, since, until *time.Time) (UsageSummary, error) {
	where, args := []string{"1=1"}, []any{}
	add := func(expr string, v any) { where = append(where, expr); args = append(args, v) }
	if keyID != "" {
		add("q.virtual_key_id=?", keyID)
	}
	if accountID != "" {
		add("a.account_id=?", accountID)
	}
	if since != nil {
		add("q.accepted_at>=?", millis(*since))
	}
	if until != nil {
		add("q.accepted_at<=?", millis(*until))
	}
	var s UsageSummary
	var in, out, reasoning, cached, actual sql.NullInt64
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT q.id),COUNT(DISTINCT a.id),
		CASE WHEN SUM(CASE WHEN u.input_tokens IS NULL THEN 1 ELSE 0 END)>0 THEN NULL ELSE SUM(u.input_tokens) END,
		CASE WHEN SUM(CASE WHEN u.output_tokens IS NULL THEN 1 ELSE 0 END)>0 THEN NULL ELSE SUM(u.output_tokens) END,
		CASE WHEN SUM(CASE WHEN u.reasoning_tokens IS NULL THEN 1 ELSE 0 END)>0 THEN NULL ELSE SUM(u.reasoning_tokens) END,
		CASE WHEN SUM(CASE WHEN u.cached_tokens IS NULL THEN 1 ELSE 0 END)>0 THEN NULL ELSE SUM(u.cached_tokens) END,
		COALESCE(SUM(r.estimated_tokens),0),
		CASE WHEN SUM(CASE WHEN r.actual_tokens IS NULL THEN 1 ELSE 0 END)>0 THEN NULL ELSE SUM(r.actual_tokens) END,
		COALESCE(SUM(r.effective_charge),0)
		FROM requests q JOIN attempts a ON a.request_id=q.id JOIN reservations r ON r.attempt_id=a.id LEFT JOIN usage_records u ON u.attempt_id=a.id WHERE `+strings.Join(where, " AND "), args...).Scan(&s.Requests, &s.Attempts, &in, &out, &reasoning, &cached, &s.EstimatedTokens, &actual, &s.EffectiveCharge)
	if err != nil {
		return UsageSummary{}, fmt.Errorf("query usage summary: %w", err)
	}
	s.InputTokens = nullIntPtr(in)
	s.OutputTokens = nullIntPtr(out)
	s.ReasoningTokens = nullIntPtr(reasoning)
	s.CachedTokens = nullIntPtr(cached)
	s.ActualTokens = nullIntPtr(actual)
	return s, nil
}
