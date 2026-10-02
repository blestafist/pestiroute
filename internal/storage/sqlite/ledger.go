package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrLedgerNotFound = errors.New("ledger record not found")
	ErrLedgerConflict = errors.New("ledger record conflict")
)

type RequestRecord struct {
	ID, VirtualKeyID, PolicyID, Protocol, Model, RouteID string
	KeyRevision, PolicyRevision                          int64
	AcceptedAt                                           time.Time
	State                                                string
	FinishedAt                                           *time.Time
}

type AttemptRecord struct {
	ID, RequestID, AccountID, Connector, RouteID, BudgetPolicy, EstimateMethod string
	Ordinal                                                                    int64
	EstimateTokens                                                             int64
	State                                                                      string
	Committed                                                                  bool
	ErrorCategory, ErrorReason                                                 *string
	DispatchedAt, FinishedAt                                                   *time.Time
}

type UsageRecord struct {
	AttemptID                                                string
	InputTokens, OutputTokens, ReasoningTokens, CachedTokens *int64
	Source, Completeness                                     string
	RecordedAt                                               time.Time
}

// TerminalAttempt contains only sanitized lifecycle metadata; error fields must
// be stable classifications/reasons, never raw provider diagnostics.
type TerminalAttempt struct {
	AttemptID, State string
	Committed        bool
	ErrorCategory    *string
	ErrorReason      *string
	Usage            UsageRecord
	FinishedAt       time.Time
}

type Ledger struct{ db *sql.DB }

func NewLedger(db *sql.DB) *Ledger { return &Ledger{db: db} }

func (r *Ledger) GetRequest(ctx context.Context, id string) (RequestRecord, error) {
	var v RequestRecord
	var key sql.NullString
	var accepted int64
	var finished sql.NullInt64
	err := r.db.QueryRowContext(ctx, `SELECT id, virtual_key_id, key_revision, policy_id, policy_revision, accepted_at, protocol, model, route_id, state, finished_at FROM requests WHERE id=?`, id).Scan(&v.ID, &key, &v.KeyRevision, &v.PolicyID, &v.PolicyRevision, &accepted, &v.Protocol, &v.Model, &v.RouteID, &v.State, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return RequestRecord{}, ErrLedgerNotFound
	}
	if err != nil {
		return RequestRecord{}, fmt.Errorf("get request: %w", err)
	}
	v.VirtualKeyID = key.String
	v.AcceptedAt = fromUnixMillis(accepted)
	if finished.Valid {
		t := fromUnixMillis(finished.Int64)
		v.FinishedAt = &t
	}
	return v, nil
}

func (r *Ledger) GetAttempt(ctx context.Context, id string) (AttemptRecord, error) {
	var v AttemptRecord
	var account, category, reason sql.NullString
	var committed int64
	var dispatched, finished sql.NullInt64
	err := r.db.QueryRowContext(ctx, `SELECT id, request_id, ordinal, account_id, connector, route_id, budget_policy, estimate_tokens, estimate_method, state, committed, error_category, error_reason, dispatched_at, finished_at FROM attempts WHERE id=?`, id).Scan(&v.ID, &v.RequestID, &v.Ordinal, &account, &v.Connector, &v.RouteID, &v.BudgetPolicy, &v.EstimateTokens, &v.EstimateMethod, &v.State, &committed, &category, &reason, &dispatched, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return AttemptRecord{}, ErrLedgerNotFound
	}
	if err != nil {
		return AttemptRecord{}, fmt.Errorf("get attempt: %w", err)
	}
	v.AccountID = account.String
	v.Committed = committed != 0
	if category.Valid {
		v.ErrorCategory = &category.String
	}
	if reason.Valid {
		v.ErrorReason = &reason.String
	}
	if dispatched.Valid {
		t := fromUnixMillis(dispatched.Int64)
		v.DispatchedAt = &t
	}
	if finished.Valid {
		t := fromUnixMillis(finished.Int64)
		v.FinishedAt = &t
	}
	return v, nil
}

func (r *Ledger) GetUsage(ctx context.Context, attemptID string) (UsageRecord, error) {
	var v UsageRecord
	var in, out, reasoning, cached sql.NullInt64
	var recorded int64
	err := r.db.QueryRowContext(ctx, `SELECT attempt_id,input_tokens,output_tokens,reasoning_tokens,cached_tokens,source,completeness,recorded_at FROM usage_records WHERE attempt_id=?`, attemptID).Scan(&v.AttemptID, &in, &out, &reasoning, &cached, &v.Source, &v.Completeness, &recorded)
	if errors.Is(err, sql.ErrNoRows) {
		return UsageRecord{}, ErrLedgerNotFound
	}
	if err != nil {
		return UsageRecord{}, fmt.Errorf("get usage: %w", err)
	}
	v.InputTokens = nullIntPtr(in)
	v.OutputTokens = nullIntPtr(out)
	v.ReasoningTokens = nullIntPtr(reasoning)
	v.CachedTokens = nullIntPtr(cached)
	v.RecordedAt = fromUnixMillis(recorded)
	return v, nil
}

func (r *Ledger) CreateRequest(ctx context.Context, v RequestRecord) error {
	if v.ID == "" || v.KeyRevision < 1 || v.PolicyID == "" || v.PolicyRevision < 1 || v.Protocol == "" || v.Model == "" || v.RouteID == "" || v.State != "admitted" || v.AcceptedAt.IsZero() || v.FinishedAt != nil {
		return fmt.Errorf("create request: %w: invalid admitted request", ErrLedgerConflict)
	}
	res, err := r.db.ExecContext(ctx, `INSERT INTO requests (id, virtual_key_id, key_revision, policy_id, policy_revision, accepted_at, protocol, model, route_id, state, finished_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL) ON CONFLICT(id) DO NOTHING`, v.ID, nullableString(v.VirtualKeyID), v.KeyRevision, v.PolicyID, v.PolicyRevision, millis(v.AcceptedAt), v.Protocol, v.Model, v.RouteID, v.State)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("create request result: %w", err)
	}
	if n == 1 {
		return nil
	}
	var key, state string
	var rev, polRev, accepted int64
	var pol, protocol, model, route string
	err = r.db.QueryRowContext(ctx, `SELECT COALESCE(virtual_key_id,''), key_revision, policy_id, policy_revision, accepted_at, protocol, model, route_id, state FROM requests WHERE id=?`, v.ID).Scan(&key, &rev, &pol, &polRev, &accepted, &protocol, &model, &route, &state)
	if err != nil {
		return fmt.Errorf("check duplicate request: %w", err)
	}
	if key != v.VirtualKeyID || rev != v.KeyRevision || pol != v.PolicyID || polRev != v.PolicyRevision || accepted != millis(v.AcceptedAt) || protocol != v.Protocol || model != v.Model || route != v.RouteID || state != v.State {
		return fmt.Errorf("create request %q: %w", v.ID, ErrLedgerConflict)
	}
	return nil
}

func (r *Ledger) CreateAttempt(ctx context.Context, v AttemptRecord) error {
	if v.ID == "" || v.RequestID == "" || v.Ordinal < 1 || v.Connector == "" || v.RouteID == "" || v.BudgetPolicy == "" || v.EstimateTokens < 0 || v.EstimateMethod == "" || v.State != "reserved" || v.Committed || v.ErrorCategory != nil || v.ErrorReason != nil || v.DispatchedAt != nil || v.FinishedAt != nil {
		return fmt.Errorf("create attempt: %w: invalid reserved attempt", ErrLedgerConflict)
	}
	res, err := r.db.ExecContext(ctx, `INSERT INTO attempts (id, request_id, ordinal, account_id, connector, route_id, budget_policy, estimate_tokens, estimate_method, state, committed, error_category, error_reason, dispatched_at, finished_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'reserved', 0, NULL, NULL, NULL, NULL) ON CONFLICT(id) DO NOTHING`, v.ID, v.RequestID, v.Ordinal, nullableString(v.AccountID), v.Connector, v.RouteID, v.BudgetPolicy, v.EstimateTokens, v.EstimateMethod)
	if err != nil {
		var duplicate bool
		if checkErr := r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM attempts WHERE id=? OR (request_id=? AND ordinal=?))`, v.ID, v.RequestID, v.Ordinal).Scan(&duplicate); checkErr == nil && duplicate {
			return fmt.Errorf("create attempt: %w", ErrLedgerConflict)
		}
		return fmt.Errorf("create attempt: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("create attempt result: %w", err)
	}
	if n == 1 {
		return nil
	}
	var got AttemptRecord
	err = r.db.QueryRowContext(ctx, `SELECT request_id, ordinal, COALESCE(account_id,''), connector, route_id, budget_policy, estimate_tokens, estimate_method, state, committed FROM attempts WHERE id=?`, v.ID).Scan(&got.RequestID, &got.Ordinal, &got.AccountID, &got.Connector, &got.RouteID, &got.BudgetPolicy, &got.EstimateTokens, &got.EstimateMethod, &got.State, &got.Committed)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("create attempt %q: %w", v.ID, ErrLedgerConflict)
	}
	if err != nil {
		return fmt.Errorf("check duplicate attempt: %w", err)
	}
	if got.RequestID != v.RequestID || got.Ordinal != v.Ordinal || got.AccountID != v.AccountID || got.Connector != v.Connector || got.RouteID != v.RouteID || got.BudgetPolicy != v.BudgetPolicy || got.EstimateTokens != v.EstimateTokens || got.EstimateMethod != v.EstimateMethod || got.State != "reserved" || got.Committed {
		return fmt.Errorf("create attempt %q: %w", v.ID, ErrLedgerConflict)
	}
	return nil
}

func (r *Ledger) RecordDispatchIntent(ctx context.Context, id string, at time.Time) error {
	if at.IsZero() {
		return fmt.Errorf("record dispatch intent: %w: timestamp is required", ErrLedgerConflict)
	}
	res, err := r.db.ExecContext(ctx, `UPDATE attempts SET state='intent', dispatched_at=? WHERE id=? AND state='reserved'`, millis(at), id)
	if err != nil {
		return fmt.Errorf("record dispatch intent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("record dispatch intent result: %w", err)
	}
	if n == 1 {
		return nil
	}
	var state string
	var dispatched sql.NullInt64
	err = r.db.QueryRowContext(ctx, `SELECT state, dispatched_at FROM attempts WHERE id=?`, id).Scan(&state, &dispatched)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLedgerNotFound
	}
	if err != nil {
		return fmt.Errorf("read dispatch intent: %w", err)
	}
	if state == "intent" && dispatched.Valid && dispatched.Int64 == millis(at) {
		return nil
	}
	return fmt.Errorf("record dispatch intent %q: %w", id, ErrLedgerConflict)
}

func (r *Ledger) FinalizeAttempt(ctx context.Context, terminal TerminalAttempt) error {
	if terminal.State != "succeeded" && terminal.State != "failed" && terminal.State != "cancelled" && terminal.State != "interrupted" {
		return fmt.Errorf("finalize attempt: %w: invalid terminal state", ErrLedgerConflict)
	}
	usage := terminal.Usage
	if terminal.AttemptID == "" || usage.AttemptID != terminal.AttemptID || usage.Source == "" || usage.Completeness == "" || usage.RecordedAt.IsZero() || terminal.FinishedAt.IsZero() {
		return fmt.Errorf("finalize attempt: %w: invalid usage or timestamp", ErrLedgerConflict)
	}
	// Acquire SQLite's writer reservation before inspecting state. A deferred
	// read-then-write transaction can fail with SQLITE_BUSY when identical
	// finalizers race after both have read the same nonterminal state.
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire attempt finalization connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin attempt finalization: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var current string
	err = conn.QueryRowContext(ctx, `SELECT state FROM attempts WHERE id=?`, terminal.AttemptID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLedgerNotFound
	}
	if err != nil {
		return fmt.Errorf("read attempt before finalization: %w", err)
	}
	if current != "reserved" && current != "intent" {
		var got UsageRecord
		var in, out, reasoning, cached sql.NullInt64
		var recorded, existingFinished sql.NullInt64
		var gotCommitted int64
		var gotCategory, gotReason sql.NullString
		err = conn.QueryRowContext(ctx, `SELECT u.input_tokens, u.output_tokens, u.reasoning_tokens, u.cached_tokens, u.source, u.completeness, u.recorded_at, a.finished_at, a.committed, a.error_category, a.error_reason FROM usage_records u JOIN attempts a ON a.id=u.attempt_id WHERE u.attempt_id=?`, terminal.AttemptID).Scan(&in, &out, &reasoning, &cached, &got.Source, &got.Completeness, &recorded, &existingFinished, &gotCommitted, &gotCategory, &gotReason)
		if err != nil {
			return fmt.Errorf("read finalized usage: %w", err)
		}
		got.AttemptID = usage.AttemptID
		got.InputTokens, got.OutputTokens, got.ReasoningTokens, got.CachedTokens = nullIntPtr(in), nullIntPtr(out), nullIntPtr(reasoning), nullIntPtr(cached)
		if recorded.Valid {
			got.RecordedAt = fromUnixMillis(recorded.Int64)
		}
		if current == terminal.State && usageEqual(got, usage) && existingFinished.Valid && existingFinished.Int64 == millis(terminal.FinishedAt) && (gotCommitted != 0) == terminal.Committed && nullableStringPtrEqual(gotCategory, terminal.ErrorCategory) && nullableStringPtrEqual(gotReason, terminal.ErrorReason) {
			return nil
		}
		return fmt.Errorf("finalize attempt %q: %w", usage.AttemptID, ErrLedgerConflict)
	}
	_, err = conn.ExecContext(ctx, `UPDATE attempts SET state=?, committed=?, error_category=?, error_reason=?, finished_at=? WHERE id=? AND state IN ('reserved','intent')`, terminal.State, terminal.Committed, terminal.ErrorCategory, terminal.ErrorReason, millis(terminal.FinishedAt), terminal.AttemptID)
	if err != nil {
		return fmt.Errorf("update terminal attempt: %w", err)
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO usage_records (attempt_id,input_tokens,output_tokens,reasoning_tokens,cached_tokens,source,completeness,recorded_at) VALUES (?,?,?,?,?,?,?,?)`, usage.AttemptID, usage.InputTokens, usage.OutputTokens, usage.ReasoningTokens, usage.CachedTokens, usage.Source, usage.Completeness, millis(usage.RecordedAt))
	if err != nil {
		return fmt.Errorf("insert terminal usage: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit attempt finalization: %w", err)
	}
	committed = true
	return nil
}

func usageEqual(a, b UsageRecord) bool {
	return a.AttemptID == b.AttemptID && ptrEqual(a.InputTokens, b.InputTokens) && ptrEqual(a.OutputTokens, b.OutputTokens) && ptrEqual(a.ReasoningTokens, b.ReasoningTokens) && ptrEqual(a.CachedTokens, b.CachedTokens) && a.Source == b.Source && a.Completeness == b.Completeness && millis(a.RecordedAt) == millis(b.RecordedAt)
}
func ptrEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
func nullIntPtr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}
func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullableStringPtrEqual(a sql.NullString, b *string) bool {
	if !a.Valid || b == nil {
		return !a.Valid && b == nil
	}
	return a.String == *b
}
func millis(v time.Time) int64 { return v.UTC().Truncate(time.Millisecond).UnixMilli() }
