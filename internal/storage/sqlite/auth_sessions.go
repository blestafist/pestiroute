package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/blestafist/pestiroute/internal/crypto"
)

var (
	ErrAuthSessionUnavailable = errors.New("authentication session unavailable")
	ErrRefreshMarkerConflict  = errors.New("refresh marker conflict")
)

type AuthSession struct {
	ID, AccountID, Connector, Kind, Lifecycle string
	ExpectedCredentialRevision                int64
	ExpiresAt, CreatedAt, UpdatedAt           time.Time
	FormatVersion                             int
	KeyVersion                                string
	Nonce, Ciphertext                         []byte
	QuarantineReason                          string
	CurrentCredentialRevision                 *int64
}

type AuthSessions struct{ db *sql.DB }

func NewAuthSessions(db *sql.DB) *AuthSessions { return &AuthSessions{db: db} }

func (s AuthSession) clone() AuthSession {
	s.Nonce = append([]byte(nil), s.Nonce...)
	s.Ciphertext = append([]byte(nil), s.Ciphertext...)
	if s.CurrentCredentialRevision != nil {
		rev := *s.CurrentCredentialRevision
		s.CurrentCredentialRevision = &rev
	}
	return s
}

func (r *AuthSessions) CreateInteractiveSession(ctx context.Context, id, accountID, connector string, expectedRevision int64, expiresAt time.Time, state []byte, key crypto.MasterKey, keyVersion string, now time.Time) (AuthSession, error) {
	if id == "" || accountID == "" || connector == "" || expectedRevision < 1 || expiresAt.IsZero() || !expiresAt.After(now) {
		return AuthSession{}, fmt.Errorf("create interactive session: invalid input")
	}
	envelope, err := crypto.Seal(key, 1, keyVersion, "auth_sessions", id, accountID, state)
	if err != nil {
		return AuthSession{}, fmt.Errorf("encrypt interactive session: %w", err)
	}
	nowMS, expiryMS := millis(now), millis(expiresAt)
	res, err := r.db.ExecContext(ctx, `INSERT INTO auth_sessions
		(id,account_id,connector,kind,expected_credential_revision,lifecycle,expires_at,created_at,updated_at,format_version,key_version,nonce,ciphertext)
		SELECT ?,a.id,a.connector,'interactive',?,'active',?,?,?,?,?,?,? FROM accounts a WHERE a.id=? AND a.connector=? AND a.enabled=1 AND (NOT EXISTS (SELECT 1 FROM credentials c WHERE c.account_id=a.id) OR EXISTS (SELECT 1 FROM credentials c WHERE c.account_id=a.id AND c.revision=?))`, id, expectedRevision, expiryMS,
		nowMS, nowMS, envelope.FormatVersion, envelope.KeyVersion, envelope.Nonce, envelope.Ciphertext, accountID, connector, expectedRevision)
	if err != nil {
		return AuthSession{}, fmt.Errorf("create interactive session: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err == nil && credentialRevisionChanged(ctx, r.db, accountID, expectedRevision) {
			return AuthSession{}, ErrRevisionMismatch
		}
		return AuthSession{}, fmt.Errorf("create interactive session: account/connector mismatch: %w", errors.Join(err, ErrAuthSessionUnavailable))
	}
	return AuthSession{ID: id, AccountID: accountID, Connector: connector, Kind: "interactive", ExpectedCredentialRevision: expectedRevision,
		Lifecycle: "active", ExpiresAt: fromUnixMillis(expiryMS), CreatedAt: fromUnixMillis(nowMS), UpdatedAt: fromUnixMillis(nowMS),
		FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext}.clone(), nil
}

func (r *AuthSessions) GetInteractiveSessionDecrypted(ctx context.Context, id string, key crypto.MasterKey, now time.Time) (AuthSession, []byte, error) {
	v, err := scanAuthSession(r.db.QueryRowContext(ctx, `SELECT `+authSessionColumns+` FROM auth_sessions WHERE id=? AND kind='interactive' AND NOT EXISTS (SELECT 1 FROM auth_session_invocations i WHERE i.session_id=auth_sessions.id)`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AuthSession{}, nil, ErrAuthSessionUnavailable
	}
	if err != nil {
		return AuthSession{}, nil, fmt.Errorf("get interactive session: %w", err)
	}
	if v.Lifecycle == "active" && !v.ExpiresAt.After(now) {
		if err := r.ConsumeInteractiveSession(ctx, v.ID, v.AccountID, now); err != nil && !errors.Is(err, ErrAuthSessionUnavailable) {
			return AuthSession{}, nil, fmt.Errorf("consume expired interactive session: %w", err)
		}
		return AuthSession{}, nil, ErrAuthSessionUnavailable
	}
	if v.Lifecycle != "active" {
		return AuthSession{}, nil, ErrAuthSessionUnavailable
	}
	plain, err := crypto.Open(key, crypto.Envelope{FormatVersion: v.FormatVersion, KeyVersion: v.KeyVersion, Nonce: v.Nonce, Ciphertext: v.Ciphertext}, "auth_sessions", v.ID, v.AccountID)
	if err != nil {
		return AuthSession{}, nil, err
	}
	return v.clone(), append([]byte(nil), plain...), nil
}

// ClaimInteractiveSession persists the no-replay boundary before Authenticate.
// A process crash leaves the session unavailable until expiry or startup recovery.
func (r *AuthSessions) ClaimInteractiveSession(ctx context.Context, id, account, connector string, revision int64, nonce []byte, now time.Time) error {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire auth claim connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin auth claim: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var available bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth_sessions s JOIN accounts a ON a.id=s.account_id AND a.connector=s.connector AND a.enabled=1 WHERE s.id=? AND s.account_id=? AND s.connector=? AND s.kind='interactive' AND s.lifecycle='active' AND s.expires_at>? AND s.expected_credential_revision=? AND s.nonce=? AND EXISTS(SELECT 1 FROM credentials c WHERE c.account_id=s.account_id AND c.revision=?) AND NOT EXISTS(SELECT 1 FROM auth_session_invocations i WHERE i.session_id=s.id))`, id, account, connector, millis(now), revision, nonce, revision).Scan(&available); err != nil {
		return fmt.Errorf("check auth claim: %w", err)
	}
	if !available {
		return ErrAuthSessionUnavailable
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO auth_session_invocations(session_id,started_at) VALUES (?,?)`, id, millis(now)); err != nil {
		return fmt.Errorf("claim auth continuation: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit auth claim: %w", err)
	}
	committed = true
	return nil
}

func (r *AuthSessions) ConsumeInteractiveSession(ctx context.Context, id, accountID string, now time.Time) error {
	res, err := r.db.ExecContext(ctx, `UPDATE auth_sessions SET lifecycle='consumed',format_version=NULL,key_version=NULL,nonce=NULL,ciphertext=NULL,updated_at=? WHERE id=? AND account_id=? AND kind='interactive' AND lifecycle='active'`, millis(now), id, accountID)
	if err != nil {
		return fmt.Errorf("consume interactive session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("check consumed session: %w", err)
	}
	if n != 1 {
		return ErrAuthSessionUnavailable
	}
	return nil
}

// AdvanceInteractiveSession replaces opaque state only at the recorded revision.
// A stale session is consumed and its envelope erased in the same transaction.
func (r *AuthSessions) AdvanceInteractiveSession(ctx context.Context, id, accountID, connector string, expectedRevision int64, expiresAt time.Time, state []byte, key crypto.MasterKey, keyVersion string, now time.Time) error {
	envelope, err := crypto.Seal(key, 1, keyVersion, "auth_sessions", id, accountID, state)
	if err != nil {
		return fmt.Errorf("encrypt interactive session: %w", err)
	}
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire interactive session connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin interactive session advance: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	res, err := conn.ExecContext(ctx, `UPDATE auth_sessions SET expires_at=?,format_version=?,key_version=?,nonce=?,ciphertext=?,updated_at=?
		WHERE id=? AND account_id=? AND connector=? AND kind='interactive' AND lifecycle='active' AND expires_at>? AND expected_credential_revision=?
		AND EXISTS (SELECT 1 FROM accounts WHERE id=auth_sessions.account_id AND connector=auth_sessions.connector AND enabled=1)
		AND EXISTS (SELECT 1 FROM credentials WHERE account_id=? AND revision=?)`, millis(expiresAt), envelope.FormatVersion, envelope.KeyVersion, envelope.Nonce, envelope.Ciphertext, millis(now), id, accountID, connector, millis(now), expectedRevision, accountID, expectedRevision)
	if err != nil {
		return fmt.Errorf("advance interactive session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("check interactive session advance: %w", err)
	}
	if n != 1 {
		_, qerr := conn.ExecContext(ctx, `UPDATE auth_sessions SET lifecycle='consumed',format_version=NULL,key_version=NULL,nonce=NULL,ciphertext=NULL,updated_at=? WHERE id=? AND account_id=? AND kind='interactive' AND lifecycle='active'`, millis(now), id, accountID)
		if qerr != nil {
			return fmt.Errorf("consume stale interactive session: %w", qerr)
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return fmt.Errorf("commit stale interactive session: %w", err)
		}
		committed = true
		return ErrRevisionMismatch
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM auth_session_invocations WHERE session_id=?`, id); err != nil {
		return fmt.Errorf("resolve auth continuation claim: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit interactive session advance: %w", err)
	}
	committed = true
	return nil
}

// FinishInteractiveSession atomically CAS-replaces an optional credential and
// consumes the session. A stale revision consumes the session without writing.
func (r *AuthSessions) FinishInteractiveSession(ctx context.Context, id, accountID, connector string, expectedRevision int64, replacement *Credential, now time.Time) error {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire interactive finish connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin interactive finish: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var active int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_sessions WHERE id=? AND account_id=? AND connector=? AND kind='interactive' AND lifecycle='active' AND expires_at>? AND expected_credential_revision=? AND EXISTS (SELECT 1 FROM accounts WHERE id=auth_sessions.account_id AND connector=auth_sessions.connector AND enabled=1)`, id, accountID, connector, millis(now), expectedRevision).Scan(&active); err != nil {
		return fmt.Errorf("check interactive finish session: %w", err)
	}
	if active != 1 {
		return r.consumeStaleInteractive(ctx, conn, id, accountID, now, &committed)
	}
	if replacement != nil {
		res, err := conn.ExecContext(ctx, `UPDATE credentials SET format_version=?,key_version=?,nonce=?,ciphertext=?,expires_at=?,revision=revision+1,
			updated_at=CASE WHEN updated_at>=? THEN updated_at+1 ELSE ? END WHERE id=? AND account_id=? AND revision=? AND EXISTS (SELECT 1 FROM accounts WHERE id=credentials.account_id AND enabled=1)`, replacement.FormatVersion, replacement.KeyVersion, replacement.Nonce, replacement.Ciphertext, unixMillis(replacement.ExpiresAt), millis(now), millis(now), replacement.ID, accountID, expectedRevision)
		if err != nil {
			return fmt.Errorf("replace interactive credentials: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("check interactive credential replacement: %w", err)
		}
		if n != 1 {
			return r.consumeStaleInteractive(ctx, conn, id, accountID, now, &committed)
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM auth_sessions WHERE account_id=? AND kind='refresh' AND lifecycle='uncertain'`, accountID); err != nil {
			return fmt.Errorf("resolve reauthentication quarantine: %w", err)
		}
	} else {
		var count int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM credentials WHERE account_id=? AND revision=?`, accountID, expectedRevision).Scan(&count); err != nil {
			return fmt.Errorf("check interactive credential revision: %w", err)
		}
		if count == 0 {
			return r.consumeStaleInteractive(ctx, conn, id, accountID, now, &committed)
		}
	}
	res, err := conn.ExecContext(ctx, `UPDATE auth_sessions SET lifecycle='consumed',format_version=NULL,key_version=NULL,nonce=NULL,ciphertext=NULL,updated_at=? WHERE id=? AND account_id=? AND connector=? AND kind='interactive' AND lifecycle='active' AND expires_at>? AND expected_credential_revision=?`, millis(now), id, accountID, connector, millis(now), expectedRevision)
	if err != nil {
		return fmt.Errorf("consume completed interactive session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("check completed interactive session: %w", err)
	}
	if n != 1 {
		return r.consumeStaleInteractive(ctx, conn, id, accountID, now, &committed)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM auth_session_invocations WHERE session_id=?`, id); err != nil {
		return fmt.Errorf("resolve completed auth claim: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit interactive finish: %w", err)
	}
	committed = true
	return nil
}

// ReplaceCredentialsAndResolveUncertain commits explicit reauthentication and
// clears that account's quarantined refresh markers in the same transaction.
func (r *AuthSessions) ReplaceCredentialsAndResolveUncertain(ctx context.Context, replacement Credential, now time.Time) (Credential, error) {
	if replacement.ID == "" || replacement.AccountID == "" || replacement.Revision < 1 || replacement.FormatVersion < 1 || replacement.KeyVersion == "" || len(replacement.Nonce) == 0 || len(replacement.Ciphertext) == 0 {
		return Credential{}, fmt.Errorf("reauthenticate: invalid replacement")
	}
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return Credential{}, fmt.Errorf("acquire reauthentication connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return Credential{}, fmt.Errorf("begin reauthentication: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	res, err := conn.ExecContext(ctx, `UPDATE credentials SET format_version=?,key_version=?,nonce=?,ciphertext=?,expires_at=?,revision=revision+1,
		updated_at=CASE WHEN updated_at>=? THEN updated_at+1 ELSE ? END WHERE id=? AND account_id=? AND revision=? AND EXISTS (SELECT 1 FROM accounts WHERE id=credentials.account_id AND enabled=1)`, replacement.FormatVersion, replacement.KeyVersion, replacement.Nonce, replacement.Ciphertext, unixMillis(replacement.ExpiresAt), millis(now), millis(now), replacement.ID, replacement.AccountID, replacement.Revision)
	if err != nil {
		return Credential{}, fmt.Errorf("replace reauthentication credentials: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Credential{}, fmt.Errorf("check reauthentication credentials: %w", err)
	}
	if n != 1 {
		return Credential{}, ErrRevisionMismatch
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM auth_sessions WHERE account_id=? AND kind='refresh' AND lifecycle='uncertain'`, replacement.AccountID); err != nil {
		return Credential{}, fmt.Errorf("resolve reauthentication quarantine: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return Credential{}, fmt.Errorf("commit reauthentication: %w", err)
	}
	committed = true
	replacement.Revision++
	replacement.UpdatedAt = fromUnixMillis(millis(now))
	return replacement.clone(), nil
}

func (r *AuthSessions) consumeStaleInteractive(ctx context.Context, conn *sql.Conn, id, accountID string, now time.Time, committed *bool) error {
	if _, err := conn.ExecContext(ctx, `UPDATE auth_sessions SET lifecycle='consumed',format_version=NULL,key_version=NULL,nonce=NULL,ciphertext=NULL,updated_at=? WHERE id=? AND account_id=? AND kind='interactive' AND lifecycle='active'`, millis(now), id, accountID); err != nil {
		return fmt.Errorf("consume stale interactive session: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit stale interactive session: %w", err)
	}
	*committed = true
	return ErrRevisionMismatch
}

func (r *AuthSessions) Invalidate(ctx context.Context, accountID, connector string, now time.Time) error {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire auth invalidation connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin auth invalidation: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if _, err := conn.ExecContext(ctx, `UPDATE auth_sessions SET lifecycle='consumed',format_version=NULL,key_version=NULL,nonce=NULL,ciphertext=NULL,updated_at=? WHERE account_id=? AND connector=? AND kind='interactive' AND lifecycle='active'`, millis(now), accountID, connector); err != nil {
		return fmt.Errorf("consume invalidated auth sessions: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE auth_sessions SET lifecycle='uncertain',quarantine_reason='ambiguous_result',updated_at=? WHERE account_id=? AND connector=? AND kind='refresh' AND lifecycle='refresh_in_progress'`, millis(now), accountID, connector); err != nil {
		return fmt.Errorf("quarantine invalidated refresh: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit auth invalidation: %w", err)
	}
	committed = true
	return nil
}

func (r *AuthSessions) CreateRefreshMarker(ctx context.Context, id, accountID, connector string, expectedRevision int64, now time.Time) error {
	if id == "" || accountID == "" || connector == "" || expectedRevision < 1 {
		return fmt.Errorf("create refresh marker: invalid input")
	}
	stamp := millis(now)
	res, err := r.db.ExecContext(ctx, `INSERT INTO auth_sessions (id,account_id,connector,kind,expected_credential_revision,lifecycle,created_at,updated_at)
		SELECT ?,a.id,a.connector,'refresh',?,'refresh_in_progress',?,? FROM accounts a WHERE a.id=? AND a.connector=? AND a.enabled=1 AND (NOT EXISTS (SELECT 1 FROM credentials c WHERE c.account_id=a.id) OR EXISTS (SELECT 1 FROM credentials c WHERE c.account_id=a.id AND c.revision=?))`, id, expectedRevision, stamp, stamp, accountID, connector, expectedRevision)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: auth_sessions.account_id") {
			return errors.Join(ErrRefreshMarkerConflict, fmt.Errorf("create refresh marker: %w", err))
		}
		return fmt.Errorf("create refresh marker: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err == nil && credentialRevisionChanged(ctx, r.db, accountID, expectedRevision) {
			return ErrRevisionMismatch
		}
		return fmt.Errorf("create refresh marker: account/connector mismatch: %w", errors.Join(err, ErrAuthSessionUnavailable))
	}
	return nil
}

// HasQuarantinedRefresh reports whether an account has an uncertain exchange
// whose credential generation cannot be trusted.
func (r *AuthSessions) HasQuarantinedRefresh(ctx context.Context, accountID string) (bool, error) {
	var found bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth_sessions WHERE account_id=? AND kind='refresh' AND lifecycle='uncertain')`, accountID).Scan(&found)
	return found, err
}

func (r *AuthSessions) QuarantineRefreshMarker(ctx context.Context, id, accountID, reason string, currentRevision int64, now time.Time) error {
	if reason != "ambiguous_result" && reason != "cancelled_after_call" && reason != "persistence_failed" && reason != "restart_in_progress" {
		return fmt.Errorf("quarantine refresh marker: invalid reason")
	}
	var current any
	if currentRevision > 0 {
		current = currentRevision
	}
	res, err := r.db.ExecContext(ctx, `UPDATE auth_sessions SET lifecycle='uncertain',quarantine_reason=?,current_credential_revision=?,updated_at=?
		WHERE id=? AND account_id=? AND kind='refresh' AND lifecycle='refresh_in_progress'`, reason, current, millis(now), id, accountID)
	if err != nil {
		return fmt.Errorf("quarantine refresh marker: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		var lifecycle, storedReason string
		lookupErr := r.db.QueryRowContext(ctx, `SELECT lifecycle,quarantine_reason FROM auth_sessions WHERE id=? AND account_id=? AND kind='refresh'`, id, accountID).Scan(&lifecycle, &storedReason)
		if lookupErr == nil && lifecycle == "uncertain" && storedReason == reason {
			return nil
		}
		return fmt.Errorf("quarantine refresh marker: %w", errors.Join(err, ErrRefreshMarkerConflict))
	}
	return nil
}

// ClearRefreshMarker is only for a preflight that proves Authenticate was never invoked.
func (r *AuthSessions) ClearRefreshMarker(ctx context.Context, id, accountID string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE id=? AND account_id=? AND kind='refresh' AND lifecycle='refresh_in_progress'`, id, accountID)
	if err != nil {
		return fmt.Errorf("clear refresh marker: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("clear refresh marker: %w", errors.Join(err, ErrRefreshMarkerConflict))
	}
	return nil
}

// ResolveRefreshAndReplaceCredentials replaces one encrypted credential and clears its marker atomically.
func (r *AuthSessions) ResolveRefreshAndReplaceCredentials(ctx context.Context, markerID string, replacement Credential, now time.Time) (Credential, error) {
	if markerID == "" || replacement.ID == "" || replacement.AccountID == "" || replacement.Revision < 1 || replacement.FormatVersion < 1 || replacement.KeyVersion == "" || len(replacement.Nonce) == 0 || len(replacement.Ciphertext) == 0 {
		return Credential{}, fmt.Errorf("resolve refresh: invalid replacement")
	}
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return Credential{}, fmt.Errorf("acquire refresh resolution connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return Credential{}, fmt.Errorf("begin refresh resolution: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var accountID string
	var expected int64
	var lifecycle string
	if err := conn.QueryRowContext(ctx, `SELECT s.account_id,s.expected_credential_revision,s.lifecycle FROM auth_sessions s JOIN accounts a ON a.id=s.account_id AND a.connector=s.connector AND a.enabled=1 WHERE s.id=? AND s.kind='refresh'`, markerID).Scan(&accountID, &expected, &lifecycle); errors.Is(err, sql.ErrNoRows) {
		return Credential{}, ErrRefreshMarkerConflict
	} else if err != nil {
		return Credential{}, fmt.Errorf("read refresh marker: %w", err)
	}
	if accountID != replacement.AccountID || lifecycle != "refresh_in_progress" {
		return Credential{}, ErrRefreshMarkerConflict
	}
	res, err := conn.ExecContext(ctx, `UPDATE credentials SET format_version=?,key_version=?,nonce=?,ciphertext=?,expires_at=?,revision=revision+1,
		updated_at=CASE WHEN updated_at>=? THEN updated_at+1 ELSE ? END WHERE id=? AND account_id=? AND revision=? AND revision=?`,
		replacement.FormatVersion, replacement.KeyVersion, replacement.Nonce, replacement.Ciphertext, unixMillis(replacement.ExpiresAt), millis(now), millis(now), replacement.ID, accountID, expected, replacement.Revision)
	if err != nil {
		return Credential{}, fmt.Errorf("replace refresh credential: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Credential{}, fmt.Errorf("check refresh credential: %w", err)
	}
	if n != 1 {
		var current int64
		if err := conn.QueryRowContext(ctx, `SELECT revision FROM credentials WHERE id=? AND account_id=?`, replacement.ID, accountID).Scan(&current); err != nil {
			return Credential{}, ErrRevisionMismatch
		}
		_, qerr := conn.ExecContext(ctx, `UPDATE auth_sessions SET lifecycle='uncertain',quarantine_reason='ambiguous_result',current_credential_revision=?,updated_at=? WHERE id=? AND lifecycle='refresh_in_progress'`, current, millis(now), markerID)
		if qerr != nil {
			return Credential{}, fmt.Errorf("quarantine stale refresh: %w", qerr)
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return Credential{}, fmt.Errorf("commit stale refresh quarantine: %w", err)
		}
		committed = true
		return Credential{}, ErrRevisionMismatch
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM auth_sessions WHERE id=? AND account_id=? AND kind='refresh' AND lifecycle='refresh_in_progress'`, markerID, accountID); err != nil {
		return Credential{}, fmt.Errorf("clear resolved refresh marker: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return Credential{}, fmt.Errorf("commit refresh resolution: %w", err)
	}
	committed = true
	replacement.Revision++
	replacement.UpdatedAt = fromUnixMillis(millis(now))
	return replacement.clone(), nil
}

func (r *AuthSessions) CleanupExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, `UPDATE auth_sessions SET lifecycle='consumed',format_version=NULL,key_version=NULL,nonce=NULL,ciphertext=NULL,updated_at=? WHERE kind='interactive' AND lifecycle='active' AND expires_at<=?`, millis(now), millis(now))
	if err != nil {
		return 0, fmt.Errorf("cleanup expired auth sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count expired auth sessions: %w", err)
	}
	return n, nil
}

func (r *AuthSessions) RecoverAuthSessions(ctx context.Context, now time.Time) (int64, error) {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire auth recovery connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return 0, fmt.Errorf("begin auth recovery: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	res, err := conn.ExecContext(ctx, `UPDATE auth_sessions SET lifecycle='uncertain',quarantine_reason='restart_in_progress',updated_at=? WHERE kind='refresh' AND lifecycle='refresh_in_progress'`, millis(now))
	if err != nil {
		return 0, fmt.Errorf("recover auth sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count recovered auth sessions: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE auth_sessions SET lifecycle='consumed',format_version=NULL,key_version=NULL,nonce=NULL,ciphertext=NULL,updated_at=? WHERE kind='interactive' AND lifecycle='active' AND EXISTS(SELECT 1 FROM auth_session_invocations i WHERE i.session_id=auth_sessions.id)`, millis(now)); err != nil {
		return 0, fmt.Errorf("recover auth continuation claims: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM auth_session_invocations`); err != nil {
		return 0, fmt.Errorf("clear recovered auth claims: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return 0, fmt.Errorf("commit auth recovery: %w", err)
	}
	committed = true
	return n, nil
}

const authSessionColumns = `id,account_id,connector,kind,expected_credential_revision,lifecycle,expires_at,created_at,updated_at,format_version,key_version,nonce,ciphertext,quarantine_reason,current_credential_revision`

type rowScanner interface{ Scan(...any) error }

func scanAuthSession(row rowScanner) (AuthSession, error) {
	var v AuthSession
	var expiry, format, current sql.NullInt64
	var created, updated int64
	var keyVersion, reason sql.NullString
	err := row.Scan(&v.ID, &v.AccountID, &v.Connector, &v.Kind, &v.ExpectedCredentialRevision, &v.Lifecycle, &expiry, &created, &updated, &format, &keyVersion, &v.Nonce, &v.Ciphertext, &reason, &current)
	if err != nil {
		return AuthSession{}, err
	}
	v.CreatedAt, v.UpdatedAt = fromUnixMillis(created), fromUnixMillis(updated)
	if expiry.Valid {
		v.ExpiresAt = fromUnixMillis(expiry.Int64)
	}
	if format.Valid {
		v.FormatVersion = int(format.Int64)
	}
	if keyVersion.Valid {
		v.KeyVersion = keyVersion.String
	}
	if reason.Valid {
		v.QuarantineReason = reason.String
	}
	if current.Valid {
		v.CurrentCredentialRevision = &current.Int64
	}
	return v, nil
}

func credentialRevisionChanged(ctx context.Context, db *sql.DB, accountID string, expected int64) bool {
	var current int64
	if err := db.QueryRowContext(ctx, `SELECT revision FROM credentials WHERE account_id=? LIMIT 1`, accountID).Scan(&current); err != nil {
		return false
	}
	return current != expected
}
