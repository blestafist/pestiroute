package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

var (
	ErrKeyPolicyNotFound         = errors.New("key policy not found")
	ErrKeyPolicyRevisionMismatch = errors.New("key policy revision mismatch")
	ErrInvalidKeyPolicy          = errors.New("invalid key policy")
)

type KeyPolicy struct {
	ID         string
	Revision   int64
	Enabled    bool
	Models     []string
	Connectors []string
	RPM        int64
	TPM        int64
	CreatedAt  time.Time
}

type PolicySnapshot struct {
	ID         string
	Revision   int64
	Enabled    bool
	Models     []string
	Connectors []string
	RPM        int64
	TPM        int64
}

type CreateKeyPolicyParams struct {
	ID         string
	Enabled    bool
	Models     []string
	Connectors []string
	RPM        int64
	TPM        int64
}

type UpdateKeyPolicyParams struct {
	Enabled    bool
	Models     []string
	Connectors []string
	RPM        int64
	TPM        int64
}

type KeyPolicyFilter struct{ Enabled *bool }

type KeyPolicies struct{ db *sql.DB }

func NewKeyPolicies(db *sql.DB) *KeyPolicies { return &KeyPolicies{db: db} }

func (p KeyPolicy) clone() KeyPolicy {
	p.Models = cloneStrings(p.Models)
	p.Connectors = cloneStrings(p.Connectors)
	return p
}

func (p *KeyPolicies) Create(ctx context.Context, params CreateKeyPolicyParams) (KeyPolicy, error) {
	params.Models, params.Connectors = cloneStrings(params.Models), cloneStrings(params.Connectors)
	if params.ID == "" {
		id, err := randomID(24)
		if err != nil {
			return KeyPolicy{}, fmt.Errorf("generate key policy ID: %w", err)
		}
		params.ID = id
	}
	if err := validateKeyPolicy(params.ID, params.Models, params.Connectors, params.RPM, params.TPM); err != nil {
		return KeyPolicy{}, err
	}
	// The schema's enabled default is true; false in creation params is treated as unspecified.
	params.Enabled = true
	now := time.Now().UTC().Truncate(time.Millisecond)
	models, _ := json.Marshal(params.Models)
	connectors, _ := json.Marshal(params.Connectors)
	if _, err := p.db.ExecContext(ctx, `INSERT INTO key_policies
		(id, revision, enabled, models, connectors, rpm, tpm, created_at)
		VALUES (?, 1, ?, ?, ?, ?, ?, ?)`, params.ID, params.Enabled, string(models), string(connectors), params.RPM, params.TPM, now.UnixMilli()); err != nil {
		return KeyPolicy{}, fmt.Errorf("create key policy: %w", err)
	}
	return KeyPolicy{ID: params.ID, Revision: 1, Enabled: params.Enabled, Models: params.Models,
		Connectors: params.Connectors, RPM: params.RPM, TPM: params.TPM, CreatedAt: now}.clone(), nil
}

func (p *KeyPolicies) Get(ctx context.Context, id string, revision int64) (KeyPolicy, error) {
	return p.get(ctx, `WHERE id = ? AND revision = ?`, id, revision)
}

func (p *KeyPolicies) GetLatest(ctx context.Context, id string) (KeyPolicy, error) {
	return p.get(ctx, `WHERE id = ? ORDER BY revision DESC LIMIT 1`, id)
}

func (p *KeyPolicies) get(ctx context.Context, suffix string, args ...any) (KeyPolicy, error) {
	policy, err := scanKeyPolicy(p.db.QueryRowContext(ctx, keyPolicySelect+suffix, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return KeyPolicy{}, ErrKeyPolicyNotFound
	}
	if err != nil {
		return KeyPolicy{}, fmt.Errorf("get key policy: %w", err)
	}
	return policy.clone(), nil
}

// Snapshot implements the runtime PolicyStore shape without wiring storage into Core.
func (p *KeyPolicies) Snapshot(ctx context.Context, principal TrustedPrincipal) (PolicySnapshot, error) {
	policy, err := p.Get(ctx, principal.PolicyID, principal.PolicyRevision)
	if err != nil {
		return PolicySnapshot{}, err
	}
	return PolicySnapshot{ID: policy.ID, Revision: policy.Revision, Enabled: policy.Enabled,
		Models: cloneStrings(policy.Models), Connectors: cloneStrings(policy.Connectors), RPM: policy.RPM, TPM: policy.TPM}, nil
}

func (p *KeyPolicies) Update(ctx context.Context, id string, expectedRevision int64, params UpdateKeyPolicyParams) (KeyPolicy, error) {
	params.Models, params.Connectors = cloneStrings(params.Models), cloneStrings(params.Connectors)
	if err := validateKeyPolicy(id, params.Models, params.Connectors, params.RPM, params.TPM); err != nil {
		return KeyPolicy{}, err
	}
	if expectedRevision < 1 || expectedRevision == math.MaxInt64 {
		return KeyPolicy{}, ErrKeyPolicyRevisionMismatch
	}
	conn, err := p.db.Conn(ctx)
	if err != nil {
		return KeyPolicy{}, fmt.Errorf("acquire key policy connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return KeyPolicy{}, fmt.Errorf("begin key policy update: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var latest int64
	err = conn.QueryRowContext(ctx, `SELECT revision FROM key_policies WHERE id = ? ORDER BY revision DESC LIMIT 1`, id).Scan(&latest)
	if errors.Is(err, sql.ErrNoRows) {
		return KeyPolicy{}, ErrKeyPolicyNotFound
	}
	if err != nil {
		return KeyPolicy{}, fmt.Errorf("read key policy revision: %w", err)
	}
	if latest != expectedRevision {
		return KeyPolicy{}, ErrKeyPolicyRevisionMismatch
	}
	models, _ := json.Marshal(params.Models)
	connectors, _ := json.Marshal(params.Connectors)
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := conn.ExecContext(ctx, `INSERT INTO key_policies
		(id, revision, enabled, models, connectors, rpm, tpm, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, expectedRevision+1, params.Enabled, string(models), string(connectors), params.RPM, params.TPM, now.UnixMilli()); err != nil {
		return KeyPolicy{}, fmt.Errorf("insert key policy revision: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return KeyPolicy{}, fmt.Errorf("commit key policy update: %w", err)
	}
	committed = true
	return KeyPolicy{ID: id, Revision: expectedRevision + 1, Enabled: params.Enabled, Models: params.Models,
		Connectors: params.Connectors, RPM: params.RPM, TPM: params.TPM, CreatedAt: now}.clone(), nil
}

func (p *KeyPolicies) List(ctx context.Context, filter KeyPolicyFilter) ([]KeyPolicy, error) {
	query := keyPolicySelect + `WHERE revision = (SELECT MAX(latest.revision) FROM key_policies latest WHERE latest.id = key_policies.id)`
	args := []any{}
	if filter.Enabled != nil {
		query += ` AND enabled = ?`
		args = append(args, *filter.Enabled)
	}
	query += ` ORDER BY id ASC`
	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list key policies: %w", err)
	}
	defer rows.Close()
	policies := make([]KeyPolicy, 0)
	for rows.Next() {
		policy, err := scanKeyPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("scan key policy: %w", err)
		}
		policies = append(policies, policy.clone())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list key policies: %w", err)
	}
	return policies, nil
}

const keyPolicySelect = `SELECT id, revision, enabled, models, connectors, rpm, tpm, created_at FROM key_policies `

type keyPolicyScanner interface{ Scan(...any) error }

func scanKeyPolicy(row keyPolicyScanner) (KeyPolicy, error) {
	var policy KeyPolicy
	var enabled int64
	var modelsJSON, connectorsJSON string
	var created int64
	if err := row.Scan(&policy.ID, &policy.Revision, &enabled, &modelsJSON, &connectorsJSON, &policy.RPM, &policy.TPM, &created); err != nil {
		return KeyPolicy{}, err
	}
	if err := json.Unmarshal([]byte(modelsJSON), &policy.Models); err != nil || !strings.HasPrefix(strings.TrimSpace(modelsJSON), "[") {
		return KeyPolicy{}, fmt.Errorf("%w: malformed models JSON", ErrInvalidKeyPolicy)
	}
	if err := json.Unmarshal([]byte(connectorsJSON), &policy.Connectors); err != nil || !strings.HasPrefix(strings.TrimSpace(connectorsJSON), "[") {
		return KeyPolicy{}, fmt.Errorf("%w: malformed connectors JSON", ErrInvalidKeyPolicy)
	}
	if err := validateKeyPolicy(policy.ID, policy.Models, policy.Connectors, policy.RPM, policy.TPM); err != nil {
		return KeyPolicy{}, err
	}
	policy.Enabled, policy.CreatedAt = enabled != 0, fromUnixMillis(created)
	return policy, nil
}

func validateKeyPolicy(id string, models, connectors []string, rpm, tpm int64) error {
	if strings.TrimSpace(id) == "" || rpm < 0 || tpm < 0 {
		return ErrInvalidKeyPolicy
	}
	for _, list := range [][]string{models, connectors} {
		for _, value := range list {
			if strings.TrimSpace(value) == "" {
				return ErrInvalidKeyPolicy
			}
		}
	}
	return nil
}

func cloneStrings(values []string) []string { return append([]string{}, values...) }
