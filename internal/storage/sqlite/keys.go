package sqlite

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

var (
	ErrVirtualKeyNotFound         = errors.New("virtual key not found")
	ErrInvalidVirtualKey          = errors.New("invalid virtual key")
	ErrVirtualKeyDisabled         = errors.New("virtual key disabled")
	ErrVirtualKeyRevoked          = errors.New("virtual key revoked")
	ErrVirtualKeyRevisionMismatch = errors.New("virtual key revision mismatch")
)

// VirtualKey contains safe metadata only. Digest is a one-way SHA-256 value;
// the issued secret is never included in this type.
type VirtualKey struct {
	ID             string
	KeyID          string
	Digest         string
	PolicyID       string
	PolicyRevision int64
	Revision       int64
	Enabled        bool
	Revoked        bool
	CreatedAt      time.Time
	RevokedAt      *time.Time
}

type IssuedVirtualKey struct {
	VirtualKey
	Secret string
}

func (k IssuedVirtualKey) String() string {
	return fmt.Sprintf("{VirtualKey:%v Secret:[REDACTED]}", k.VirtualKey)
}

func (k IssuedVirtualKey) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprintf(state, "{VirtualKey:%v Secret:[REDACTED]}", k.VirtualKey)
}

type TrustedPrincipal struct {
	KeyID, PolicyID             string
	KeyRevision, PolicyRevision int64
}

type CreateVirtualKeyParams struct {
	PolicyID       string
	PolicyRevision int64
}

type VirtualKeyFilter struct {
	PolicyID string
	Enabled  *bool
	Revoked  *bool
}

type VirtualKeys struct{ db *sql.DB }

func NewVirtualKeys(db *sql.DB) *VirtualKeys { return &VirtualKeys{db: db} }

func (r *VirtualKeys) Create(ctx context.Context, params CreateVirtualKeyParams) (IssuedVirtualKey, error) {
	id, err := randomID(24)
	if err != nil {
		return IssuedVirtualKey{}, fmt.Errorf("generate virtual key ID: %w", err)
	}
	keyID, err := randomID(18)
	if err != nil {
		return IssuedVirtualKey{}, fmt.Errorf("generate virtual key identifier: %w", err)
	}
	secret, err := randomID(32)
	if err != nil {
		return IssuedVirtualKey{}, fmt.Errorf("generate virtual key secret: %w", err)
	}
	keyID = "prk_" + keyID
	secret = "prv_" + secret
	digest := digestVirtualKey(secret)
	now := time.Now().UTC().Truncate(time.Millisecond)
	_, err = r.db.ExecContext(ctx, `INSERT INTO virtual_keys
		(id, key_id, digest, policy_id, policy_revision, revision, enabled, revoked, created_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, 1, 1, 0, ?, NULL)`, id, keyID, digest, params.PolicyID, params.PolicyRevision, now.UnixMilli())
	if err != nil {
		return IssuedVirtualKey{}, fmt.Errorf("create virtual key: %w", err)
	}
	return IssuedVirtualKey{VirtualKey: VirtualKey{
		ID: id, KeyID: keyID, Digest: digest, PolicyID: params.PolicyID,
		PolicyRevision: params.PolicyRevision, Revision: 1, Enabled: true, CreatedAt: now,
	}, Secret: secret}, nil
}

func (r *VirtualKeys) Get(ctx context.Context, id string) (VirtualKey, error) {
	return r.get(ctx, `WHERE id = ?`, id)
}

func (r *VirtualKeys) GetByKeyID(ctx context.Context, keyID string) (VirtualKey, error) {
	return r.get(ctx, `WHERE key_id = ?`, keyID)
}

func (r *VirtualKeys) get(ctx context.Context, where string, arg any) (VirtualKey, error) {
	k, err := scanVirtualKey(r.db.QueryRowContext(ctx, virtualKeySelect+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return VirtualKey{}, ErrVirtualKeyNotFound
	}
	if err != nil {
		return VirtualKey{}, fmt.Errorf("get virtual key: %w", err)
	}
	return k, nil
}

func (r *VirtualKeys) List(ctx context.Context, filter VirtualKeyFilter) ([]VirtualKey, error) {
	query := virtualKeySelect + `WHERE 1 = 1`
	args := make([]any, 0, 3)
	if filter.PolicyID != "" {
		query += ` AND policy_id = ?`
		args = append(args, filter.PolicyID)
	}
	if filter.Enabled != nil {
		query += ` AND enabled = ?`
		args = append(args, *filter.Enabled)
	}
	if filter.Revoked != nil {
		query += ` AND revoked = ?`
		args = append(args, *filter.Revoked)
	}
	query += ` ORDER BY created_at ASC, id ASC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list virtual keys: %w", err)
	}
	defer rows.Close()
	keys := make([]VirtualKey, 0)
	for rows.Next() {
		key, err := scanVirtualKey(rows)
		if err != nil {
			return nil, fmt.Errorf("scan virtual key: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list virtual keys: %w", err)
	}
	return keys, nil
}

func (r *VirtualKeys) Verify(ctx context.Context, presentedKey string) (TrustedPrincipal, error) {
	if len(presentedKey) < len("prv_")+43 || len(presentedKey) > len("prv_")+44 || presentedKey[:min(len(presentedKey), len("prv_"))] != "prv_" {
		return TrustedPrincipal{}, ErrInvalidVirtualKey
	}
	digest := digestVirtualKey(presentedKey)
	k, err := scanVirtualKey(r.db.QueryRowContext(ctx, virtualKeySelect+`WHERE digest = ?`, digest))
	if errors.Is(err, sql.ErrNoRows) {
		return TrustedPrincipal{}, ErrInvalidVirtualKey
	}
	if err != nil {
		return TrustedPrincipal{}, fmt.Errorf("verify virtual key: %w", err)
	}
	storedDigest := digestVirtualKey(presentedKey)
	if subtle.ConstantTimeCompare([]byte(k.Digest), []byte(storedDigest)) != 1 {
		return TrustedPrincipal{}, ErrInvalidVirtualKey
	}
	if k.Revoked {
		return TrustedPrincipal{}, ErrVirtualKeyRevoked
	}
	if !k.Enabled {
		return TrustedPrincipal{}, ErrVirtualKeyDisabled
	}
	return TrustedPrincipal{KeyID: k.KeyID, PolicyID: k.PolicyID, KeyRevision: k.Revision, PolicyRevision: k.PolicyRevision}, nil
}

func (r *VirtualKeys) Revoke(ctx context.Context, id string) (VirtualKey, error) {
	now := time.Now().UTC().Truncate(time.Millisecond).UnixMilli()
	k, err := scanVirtualKey(r.db.QueryRowContext(ctx, `UPDATE virtual_keys SET revoked = 1,
		revoked_at = CASE WHEN revoked = 0 THEN ? ELSE revoked_at END,
		revision = revision + CASE WHEN revoked = 0 THEN 1 ELSE 0 END
		WHERE id = ? RETURNING id, key_id, digest, policy_id, policy_revision, revision, enabled, revoked, created_at, revoked_at`, now, id))
	if errors.Is(err, sql.ErrNoRows) {
		return VirtualKey{}, ErrVirtualKeyNotFound
	}
	if err != nil {
		return VirtualKey{}, fmt.Errorf("revoke virtual key: %w", err)
	}
	return k, nil
}

func (r *VirtualKeys) SetEnabled(ctx context.Context, id string, enabled bool) (VirtualKey, error) {
	k, err := scanVirtualKey(r.db.QueryRowContext(ctx, `UPDATE virtual_keys SET enabled = ?,
		revision = revision + CASE WHEN enabled <> ? THEN 1 ELSE 0 END
		WHERE id = ? AND revoked = 0 RETURNING id, key_id, digest, policy_id, policy_revision, revision, enabled, revoked, created_at, revoked_at`, enabled, enabled, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r.mutationMiss(ctx, id, false)
	}
	if err != nil {
		return VirtualKey{}, fmt.Errorf("set virtual key enabled: %w", err)
	}
	return k, nil
}

func (r *VirtualKeys) UpdatePolicy(ctx context.Context, id, policyID string, policyRevision, expectedRevision int64) (VirtualKey, error) {
	k, err := scanVirtualKey(r.db.QueryRowContext(ctx, `UPDATE virtual_keys SET policy_id = ?, policy_revision = ?, revision = revision + 1
		WHERE id = ? AND revision = ? AND revoked = 0
		RETURNING id, key_id, digest, policy_id, policy_revision, revision, enabled, revoked, created_at, revoked_at`,
		policyID, policyRevision, id, expectedRevision))
	if errors.Is(err, sql.ErrNoRows) {
		return r.mutationMiss(ctx, id, true)
	}
	if err != nil {
		return VirtualKey{}, fmt.Errorf("update virtual key policy: %w", err)
	}
	return k, nil
}

func (r *VirtualKeys) mutationMiss(ctx context.Context, id string, revisionCheck bool) (VirtualKey, error) {
	k, err := r.Get(ctx, id)
	if err != nil {
		return VirtualKey{}, err
	}
	if k.Revoked {
		return VirtualKey{}, ErrVirtualKeyRevoked
	}
	if revisionCheck {
		return VirtualKey{}, ErrVirtualKeyRevisionMismatch
	}
	return VirtualKey{}, ErrVirtualKeyRevoked
}

const virtualKeySelect = `SELECT id, key_id, digest, policy_id, policy_revision, revision, enabled, revoked, created_at, revoked_at FROM virtual_keys `

type virtualKeyScanner interface{ Scan(...any) error }

func scanVirtualKey(row virtualKeyScanner) (VirtualKey, error) {
	var k VirtualKey
	var enabled, revoked, created int64
	var revokedAt sql.NullInt64
	if err := row.Scan(&k.ID, &k.KeyID, &k.Digest, &k.PolicyID, &k.PolicyRevision, &k.Revision,
		&enabled, &revoked, &created, &revokedAt); err != nil {
		return VirtualKey{}, err
	}
	k.Enabled, k.Revoked = enabled != 0, revoked != 0
	k.CreatedAt = fromUnixMillis(created)
	if revokedAt.Valid {
		value := fromUnixMillis(revokedAt.Int64)
		k.RevokedAt = &value
	}
	return k, nil
}

func randomID(size int) (string, error) {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func digestVirtualKey(secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(digest[:])
}
