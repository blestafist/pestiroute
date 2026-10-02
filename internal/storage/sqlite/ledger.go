package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
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

// ReservationRecord is the token hold created with an admitted attempt.
type ReservationRecord struct {
	AttemptID       string
	EstimatedTokens int64
	ActualTokens    *int64
	EffectiveCharge int64
	State           string
	ReconciledAt    *time.Time
}

type Ledger struct {
	db  *sql.DB
	now func() time.Time
}

func NewLedger(db *sql.DB) *Ledger { return &Ledger{db: db, now: time.Now} }

// Admit atomically checks the current key/policy and rolling limits before
// creating one request, its first attempt, and its held reservation.
func (r *Ledger) Admit(ctx context.Context, request RequestRecord, attempt AttemptRecord, reservation ReservationRecord) error {
	if request.ID == "" || request.VirtualKeyID == "" || request.KeyRevision < 1 || request.PolicyID == "" || request.PolicyRevision < 1 || request.Protocol == "" || request.Model == "" || request.RouteID == "" || request.State != "admitted" || request.FinishedAt != nil || attempt.Ordinal != 1 || attempt.RequestID != request.ID || attempt.RouteID != request.RouteID || attempt.AccountID == "" || attempt.EstimateTokens <= 0 || reservation.AttemptID != attempt.ID || reservation.EstimatedTokens != attempt.EstimateTokens {
		return fmt.Errorf("admit request: %w: invalid admission records", ErrLedgerConflict)
	}
	if err := validateReservedAttempt(attempt); err != nil {
		return err
	}
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire admission connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin admission: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	// A repeated identity is checked before current authorization: replaying a
	// committed admission is a no-op, while changing any identity field conflicts.
	var state string
	err = conn.QueryRowContext(ctx, `SELECT state FROM requests WHERE id=?`, request.ID).Scan(&state)
	if err == nil {
		if state != "admitted" {
			return fmt.Errorf("admit duplicate request: %w", ErrLedgerConflict)
		}
		if err := checkDuplicateAdmission(ctx, conn, request, attempt, reservation); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return fmt.Errorf("commit duplicate admission: %w", err)
		}
		committed = true
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check duplicate request: %w", err)
	}

	var keyRevision, keyPolicyRevision, policyEnabled int64
	var enabled, revoked int64
	var keyPolicy string
	err = conn.QueryRowContext(ctx, `SELECT k.revision,k.enabled,k.revoked,k.policy_id,k.policy_revision,p.enabled
		FROM virtual_keys k JOIN key_policies p ON p.id=k.policy_id AND p.revision=k.policy_revision
		WHERE k.id=?`, request.VirtualKeyID).Scan(&keyRevision, &enabled, &revoked, &keyPolicy, &keyPolicyRevision, &policyEnabled)
	if err != nil {
		return fmt.Errorf("read admission authorization: %w", err)
	}
	if keyRevision != request.KeyRevision || keyPolicy != request.PolicyID || keyPolicyRevision != request.PolicyRevision || enabled == 0 || revoked != 0 || policyEnabled == 0 {
		return fmt.Errorf("admit request authorization changed: %w", ErrLedgerConflict)
	}
	policy, err := scanKeyPolicy(conn.QueryRowContext(ctx, keyPolicySelect+`WHERE id=? AND revision=?`, request.PolicyID, request.PolicyRevision))
	if err != nil {
		return fmt.Errorf("read admission policy snapshot: %w", err)
	}
	if !slices.Contains(policy.Models, request.Model) || !slices.Contains(policy.Connectors, attempt.Connector) {
		return fmt.Errorf("admit target outside policy: %w", ErrLedgerConflict)
	}
	if err := checkEnabledAccount(ctx, conn, attempt.AccountID, attempt.Connector); err != nil {
		return err
	}
	now, cutoff, err := r.windowNow()
	if err != nil {
		return err
	}
	if err := checkNoFutureAdmissionTimes(ctx, conn, request.VirtualKeyID, now); err != nil {
		return err
	}
	var rpmUsed, held, settled int64
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE virtual_key_id=? AND accepted_at>? AND accepted_at<=?`, request.VirtualKeyID, cutoff, now).Scan(&rpmUsed); err != nil {
		return fmt.Errorf("count admission RPM: %w", err)
	}
	if policy.RPM <= 0 || rpmUsed >= policy.RPM {
		return fmt.Errorf("admit request RPM exhausted: %w", ErrLedgerConflict)
	}
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(estimated_tokens),0) FROM reservations r JOIN attempts a ON a.id=r.attempt_id JOIN requests q ON q.id=a.request_id WHERE q.virtual_key_id=? AND r.state='held'`, request.VirtualKeyID).Scan(&held); err != nil {
		return fmt.Errorf("sum held reservations: %w", err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(effective_charge),0) FROM reservations r JOIN attempts a ON a.id=r.attempt_id JOIN requests q ON q.id=a.request_id WHERE q.virtual_key_id=? AND r.state IN ('settled','conservative') AND r.reconciled_at>? AND r.reconciled_at<=?`, request.VirtualKeyID, cutoff, now).Scan(&settled); err != nil {
		return fmt.Errorf("sum settled reservations: %w", err)
	}
	if !withinTPM(held, settled, reservation.EstimatedTokens, policy.TPM) {
		return fmt.Errorf("admit request TPM exhausted: %w", ErrLedgerConflict)
	}
	request.AcceptedAt = fromUnixMillis(now)
	if err := insertAdmittedRecords(ctx, conn, request, attempt, reservation); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit admission: %w", err)
	}
	committed = true
	return nil
}

// BeginAttempt reserves additional tokens for an already admitted request;
// ordinal assignment and TPM checking are serialized with all other admissions.
func (r *Ledger) BeginAttempt(ctx context.Context, attempt AttemptRecord, reservation ReservationRecord) error {
	if attempt.ID == "" || attempt.RequestID == "" || attempt.EstimateTokens <= 0 || reservation.AttemptID != attempt.ID || reservation.EstimatedTokens != attempt.EstimateTokens {
		return fmt.Errorf("begin attempt: %w: invalid reservation", ErrLedgerConflict)
	}
	if err := validateReservedAttempt(attempt); err != nil {
		return err
	}
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire attempt admission connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin attempt admission: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var existingRequest, state string
	var existingOrdinal, estimate int64
	err = conn.QueryRowContext(ctx, `SELECT request_id,ordinal,estimate_tokens,state FROM attempts WHERE id=?`, attempt.ID).Scan(&existingRequest, &existingOrdinal, &estimate, &state)
	if err == nil {
		if existingRequest != attempt.RequestID || estimate != attempt.EstimateTokens || state != "reserved" || attempt.Ordinal != 0 && attempt.Ordinal != existingOrdinal {
			return fmt.Errorf("begin duplicate attempt: %w", ErrLedgerConflict)
		}
		if err := checkDuplicateAttempt(ctx, conn, attempt, reservation, existingOrdinal); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return fmt.Errorf("commit duplicate attempt: %w", err)
		}
		committed = true
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check duplicate attempt: %w", err)
	}
	var keyID string
	var policyID string
	var model string
	var policyRevision, tpm int64
	if err := conn.QueryRowContext(ctx, `SELECT virtual_key_id,policy_id,policy_revision,state,model FROM requests WHERE id=?`, attempt.RequestID).Scan(&keyID, &policyID, &policyRevision, &state, &model); errors.Is(err, sql.ErrNoRows) {
		return ErrLedgerNotFound
	} else if err != nil {
		return fmt.Errorf("read retry request: %w", err)
	}
	if state != "admitted" || keyID == "" {
		return fmt.Errorf("begin attempt for closed request: %w", ErrLedgerConflict)
	}
	policy, err := scanKeyPolicy(conn.QueryRowContext(ctx, keyPolicySelect+`WHERE id=? AND revision=?`, policyID, policyRevision))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("begin attempt for disabled policy: %w", ErrLedgerConflict)
	}
	if err != nil {
		return fmt.Errorf("read retry policy: %w", err)
	}
	if !policy.Enabled || !slices.Contains(policy.Models, model) || !slices.Contains(policy.Connectors, attempt.Connector) {
		return fmt.Errorf("begin attempt outside admitted policy: %w", ErrLedgerConflict)
	}
	tpm = policy.TPM
	if err := checkEnabledAccount(ctx, conn, attempt.AccountID, attempt.Connector); err != nil {
		return err
	}
	if attempt.Ordinal != 0 {
		return fmt.Errorf("attempt ordinal is repository-assigned: %w", ErrLedgerConflict)
	}
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(ordinal),0)+1 FROM attempts WHERE request_id=?`, attempt.RequestID).Scan(&attempt.Ordinal); err != nil {
		return fmt.Errorf("assign attempt ordinal: %w", err)
	}
	var held, settled int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(estimated_tokens),0) FROM reservations r JOIN attempts a ON a.id=r.attempt_id JOIN requests q ON q.id=a.request_id WHERE q.virtual_key_id=? AND r.state='held'`, keyID).Scan(&held); err != nil {
		return fmt.Errorf("sum held reservations: %w", err)
	}
	now, cutoff, err := r.windowNow()
	if err != nil {
		return err
	}
	if err := checkNoFutureAdmissionTimes(ctx, conn, keyID, now); err != nil {
		return err
	}
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(effective_charge),0) FROM reservations r JOIN attempts a ON a.id=r.attempt_id JOIN requests q ON q.id=a.request_id WHERE q.virtual_key_id=? AND r.state IN ('settled','conservative') AND r.reconciled_at>? AND r.reconciled_at<=?`, keyID, cutoff, now).Scan(&settled); err != nil {
		return fmt.Errorf("sum settled reservations: %w", err)
	}
	if !withinTPM(held, settled, reservation.EstimatedTokens, tpm) {
		return fmt.Errorf("begin attempt TPM exhausted: %w", ErrLedgerConflict)
	}
	if err := insertAttemptReservation(ctx, conn, attempt, reservation); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit attempt admission: %w", err)
	}
	committed = true
	return nil
}

func validateReservedAttempt(v AttemptRecord) error {
	if v.ID == "" || v.RequestID == "" || v.Ordinal < 0 || v.AccountID == "" || v.Connector == "" || v.RouteID == "" || v.BudgetPolicy == "" || v.EstimateTokens <= 0 || v.EstimateMethod == "" || v.State != "reserved" || v.Committed || v.ErrorCategory != nil || v.ErrorReason != nil || v.DispatchedAt != nil || v.FinishedAt != nil {
		return fmt.Errorf("reserved attempt: %w: invalid attempt", ErrLedgerConflict)
	}
	return nil
}

func withinTPM(held, settled, estimate, limit int64) bool {
	if held < 0 || settled < 0 || estimate <= 0 || limit <= 0 || held > int64(^uint64(0)>>1)-settled {
		return false
	}
	used := held + settled
	return used <= limit && estimate <= limit-used
}

func (r *Ledger) windowNow() (now, cutoff int64, err error) {
	now = millis(r.now())
	if now < 60_000 {
		return 0, 0, fmt.Errorf("admission clock outside valid range: %w", ErrLedgerConflict)
	}
	return now, now - 60_000, nil
}

func checkEnabledAccount(ctx context.Context, conn *sql.Conn, accountID, connector string) error {
	var enabled int64
	err := conn.QueryRowContext(ctx, `SELECT enabled FROM accounts WHERE id=? AND connector=?`, accountID, connector).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) || err == nil && enabled == 0 {
		return fmt.Errorf("admission account unavailable or connector mismatch: %w", ErrLedgerConflict)
	}
	if err != nil {
		return fmt.Errorf("read admission account: %w", err)
	}
	return nil
}

func checkNoFutureAdmissionTimes(ctx context.Context, conn *sql.Conn, keyID string, now int64) error {
	var futureAccepted, futureReconciled int64
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM requests WHERE virtual_key_id=? AND accepted_at>?)`, keyID, now).Scan(&futureAccepted); err != nil {
		return fmt.Errorf("check future accepted timestamps: %w", err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM reservations r JOIN attempts a ON a.id=r.attempt_id JOIN requests q ON q.id=a.request_id WHERE q.virtual_key_id=? AND r.reconciled_at>?)`, keyID, now).Scan(&futureReconciled); err != nil {
		return fmt.Errorf("check future reconciliation timestamps: %w", err)
	}
	if futureAccepted != 0 || futureReconciled != 0 {
		return fmt.Errorf("future persisted admission timestamp: %w", ErrLedgerConflict)
	}
	return nil
}

func insertAdmittedRecords(ctx context.Context, conn *sql.Conn, q RequestRecord, a AttemptRecord, v ReservationRecord) error {
	if _, err := conn.ExecContext(ctx, `INSERT INTO requests (id,virtual_key_id,key_revision,policy_id,policy_revision,accepted_at,protocol,model,route_id,state,finished_at) VALUES (?,?,?,?,?,?,?,?,?,'admitted',NULL)`, q.ID, q.VirtualKeyID, q.KeyRevision, q.PolicyID, q.PolicyRevision, millis(q.AcceptedAt), q.Protocol, q.Model, q.RouteID); err != nil {
		return fmt.Errorf("insert admitted request: %w", err)
	}
	if err := insertAttemptReservation(ctx, conn, a, v); err != nil {
		return err
	}
	return nil
}

func insertAttemptReservation(ctx context.Context, conn *sql.Conn, a AttemptRecord, v ReservationRecord) error {
	if _, err := conn.ExecContext(ctx, `INSERT INTO attempts (id,request_id,ordinal,account_id,connector,route_id,budget_policy,estimate_tokens,estimate_method,state,committed) VALUES (?,?,?,?,?,?,?,?,?,'reserved',0)`, a.ID, a.RequestID, a.Ordinal, nullableString(a.AccountID), a.Connector, a.RouteID, a.BudgetPolicy, a.EstimateTokens, a.EstimateMethod); err != nil {
		return fmt.Errorf("insert reserved attempt: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO reservations (attempt_id,estimated_tokens,state) VALUES (?,?,'held')`, v.AttemptID, v.EstimatedTokens); err != nil {
		return fmt.Errorf("insert attempt reservation: %w", err)
	}
	return nil
}

func checkDuplicateAdmission(ctx context.Context, conn *sql.Conn, q RequestRecord, a AttemptRecord, v ReservationRecord) error {
	var request RequestRecord
	var key sql.NullString
	var at int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(virtual_key_id,''),key_revision,policy_id,policy_revision,protocol,model,route_id,state,accepted_at FROM requests WHERE id=?`, q.ID).Scan(&key, &request.KeyRevision, &request.PolicyID, &request.PolicyRevision, &request.Protocol, &request.Model, &request.RouteID, &request.State, &at); err != nil {
		return fmt.Errorf("read duplicate admission: %w", err)
	}
	if key.String != q.VirtualKeyID || request.KeyRevision != q.KeyRevision || request.PolicyID != q.PolicyID || request.PolicyRevision != q.PolicyRevision || request.Protocol != q.Protocol || request.Model != q.Model || request.RouteID != q.RouteID || request.State != "admitted" {
		return fmt.Errorf("duplicate admission identity: %w", ErrLedgerConflict)
	}
	return checkDuplicateAttempt(ctx, conn, a, v, 1)
}

func checkDuplicateAttempt(ctx context.Context, conn *sql.Conn, a AttemptRecord, v ReservationRecord, ordinal int64) error {
	var requestID, account, connector, route, budget, method, state string
	var gotOrdinal, estimate, held int64
	err := conn.QueryRowContext(ctx, `SELECT a.request_id,a.ordinal,COALESCE(a.account_id,''),a.connector,a.route_id,a.budget_policy,a.estimate_tokens,a.estimate_method,a.state,r.estimated_tokens FROM attempts a JOIN reservations r ON r.attempt_id=a.id WHERE a.id=?`, a.ID).Scan(&requestID, &gotOrdinal, &account, &connector, &route, &budget, &estimate, &method, &state, &held)
	if err != nil {
		return fmt.Errorf("read duplicate attempt: %w", err)
	}
	if requestID != a.RequestID || gotOrdinal != ordinal || account != a.AccountID || connector != a.Connector || route != a.RouteID || budget != a.BudgetPolicy || estimate != a.EstimateTokens || method != a.EstimateMethod || held != v.EstimatedTokens || state != "reserved" {
		return fmt.Errorf("duplicate attempt identity: %w", ErrLedgerConflict)
	}
	return nil
}

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
	if id == "" || at.IsZero() {
		return fmt.Errorf("record dispatch intent: %w: attempt ID and timestamp are required", ErrLedgerConflict)
	}
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire dispatch intent connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin dispatch intent: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var requestState, state string
	var dispatched sql.NullInt64
	err = conn.QueryRowContext(ctx, `SELECT q.state,a.state,a.dispatched_at FROM attempts a JOIN requests q ON q.id=a.request_id WHERE a.id=?`, id).Scan(&requestState, &state, &dispatched)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLedgerNotFound
	}
	if err != nil {
		return fmt.Errorf("read dispatch intent: %w", err)
	}
	if requestState != "admitted" {
		return fmt.Errorf("record dispatch intent for closed request %q: %w", id, ErrLedgerConflict)
	}
	if state == "intent" && dispatched.Valid && dispatched.Int64 == millis(at) {
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return fmt.Errorf("commit duplicate dispatch intent: %w", err)
		}
		committed = true
		return nil
	}
	if state != "reserved" || dispatched.Valid {
		return fmt.Errorf("record dispatch intent %q: %w", id, ErrLedgerConflict)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE attempts SET state='intent', dispatched_at=? WHERE id=? AND state='reserved'`, millis(at), id); err != nil {
		return fmt.Errorf("update dispatch intent: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit dispatch intent: %w", err)
	}
	committed = true
	return nil
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
	var dispatchedAt sql.NullInt64
	err = conn.QueryRowContext(ctx, `SELECT state,dispatched_at FROM attempts WHERE id=?`, terminal.AttemptID).Scan(&current, &dispatchedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLedgerNotFound
	}
	if err != nil {
		return fmt.Errorf("read attempt before finalization: %w", err)
	}
	var estimate int64
	if err := conn.QueryRowContext(ctx, `SELECT estimated_tokens FROM reservations WHERE attempt_id=?`, terminal.AttemptID).Scan(&estimate); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reservation for attempt %q: %w", terminal.AttemptID, ErrLedgerNotFound)
	} else if err != nil {
		return fmt.Errorf("read attempt reservation: %w", err)
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
		reservation, err := readReservation(ctx, conn, terminal.AttemptID)
		if err != nil {
			return err
		}
		expected, err := settlement(estimate, usage, dispatchedAt.Valid)
		if err != nil {
			return err
		}
		if current == terminal.State && usageEqual(got, usage) && existingFinished.Valid && existingFinished.Int64 == millis(terminal.FinishedAt) && (gotCommitted != 0) == terminal.Committed && nullableStringPtrEqual(gotCategory, terminal.ErrorCategory) && nullableStringPtrEqual(gotReason, terminal.ErrorReason) && reservation.State == expected.state && reservation.EffectiveCharge == expected.charge && ptrEqual(reservation.ActualTokens, expected.actual) && reservation.ReconciledAt != nil {
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
	result, err := settlement(estimate, usage, current == "intent")
	if err != nil {
		return err
	}
	// Sample only after BEGIN IMMEDIATE has acquired the writer lock.
	reconciled := millis(r.now())
	if _, err := conn.ExecContext(ctx, `UPDATE reservations SET state=?,actual_tokens=?,effective_charge=?,reconciled_at=? WHERE attempt_id=? AND state='held'`, result.state, result.actual, result.charge, reconciled, terminal.AttemptID); err != nil {
		return fmt.Errorf("reconcile attempt reservation: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit attempt finalization: %w", err)
	}
	committed = true
	return nil
}

type settlementResult struct {
	state  string
	actual *int64
	charge int64
}

func settlement(estimate int64, usage UsageRecord, dispatched bool) (settlementResult, error) {
	if estimate < 0 {
		return settlementResult{}, fmt.Errorf("settle reservation: %w: negative estimate", ErrLedgerConflict)
	}
	for _, token := range []*int64{usage.InputTokens, usage.OutputTokens, usage.ReasoningTokens, usage.CachedTokens} {
		if token != nil && *token < 0 {
			return settlementResult{}, fmt.Errorf("settle reservation: %w: negative usage", ErrLedgerConflict)
		}
	}
	if !dispatched {
		return settlementResult{state: "released"}, nil
	}
	if usage.Completeness == "complete" && usage.InputTokens != nil && usage.OutputTokens != nil {
		total, ok := checkedTokenSum(*usage.InputTokens, *usage.OutputTokens)
		if !ok {
			return settlementResult{}, fmt.Errorf("settle reservation: %w: token overflow", ErrLedgerConflict)
		}
		actual := total
		return settlementResult{state: "settled", actual: &actual, charge: total}, nil
	}
	lower := int64(0)
	for _, token := range []*int64{usage.InputTokens, usage.OutputTokens} {
		if token != nil {
			var ok bool
			lower, ok = checkedTokenSum(lower, *token)
			if !ok {
				return settlementResult{}, fmt.Errorf("settle reservation: %w: token overflow", ErrLedgerConflict)
			}
		}
	}
	if lower < estimate {
		lower = estimate
	}
	return settlementResult{state: "conservative", charge: lower}, nil
}

func checkedTokenSum(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}

func readReservation(ctx context.Context, conn *sql.Conn, id string) (ReservationRecord, error) {
	var reservation ReservationRecord
	var actual, reconciled sql.NullInt64
	err := conn.QueryRowContext(ctx, `SELECT attempt_id,estimated_tokens,actual_tokens,COALESCE(effective_charge,0),state,reconciled_at FROM reservations WHERE attempt_id=?`, id).Scan(&reservation.AttemptID, &reservation.EstimatedTokens, &actual, &reservation.EffectiveCharge, &reservation.State, &reconciled)
	if err != nil {
		return ReservationRecord{}, fmt.Errorf("read reservation: %w", err)
	}
	reservation.ActualTokens = nullIntPtr(actual)
	if reconciled.Valid {
		t := fromUnixMillis(reconciled.Int64)
		reservation.ReconciledAt = &t
	}
	return reservation, nil
}

func (r *Ledger) GetReservation(ctx context.Context, attemptID string) (ReservationRecord, error) {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return ReservationRecord{}, fmt.Errorf("acquire reservation connection: %w", err)
	}
	defer conn.Close()
	reservation, err := readReservation(ctx, conn, attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return ReservationRecord{}, ErrLedgerNotFound
	}
	return reservation, err
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
