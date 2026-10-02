package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/blestafist/pestiroute/internal/crypto"
)

var (
	ErrNotFound         = errors.New("credential not found")
	ErrRevisionMismatch = errors.New("credential revision mismatch")
)

// Credential is an encrypted credential row. Envelope slices are copied at
// repository boundaries so callers cannot mutate a previously returned value.
type Credential struct {
	ID            string
	AccountID     string
	FormatVersion int
	KeyVersion    string
	Nonce         []byte
	Ciphertext    []byte
	ExpiresAt     *time.Time
	Revision      int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Credentials stores encrypted envelopes; plaintext is only returned by GetDecrypted.
type Credentials struct{ db *sql.DB }

func NewCredentials(db *sql.DB) *Credentials { return &Credentials{db: db} }

func (c Credential) clone() Credential {
	c.Nonce = append([]byte(nil), c.Nonce...)
	c.Ciphertext = append([]byte(nil), c.Ciphertext...)
	if c.ExpiresAt != nil {
		expires := *c.ExpiresAt
		c.ExpiresAt = &expires
	}
	return c
}

func (r *Credentials) Create(ctx context.Context, credential Credential) (Credential, error) {
	credential = credential.clone()
	now := time.Now().UTC().Truncate(time.Millisecond)
	credential.CreatedAt, credential.UpdatedAt, credential.Revision = now, now, 1
	if _, err := r.db.ExecContext(ctx, `INSERT INTO credentials
		(id, account_id, format_version, key_version, nonce, ciphertext, expires_at, revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`, credential.ID, credential.AccountID,
		credential.FormatVersion, credential.KeyVersion, credential.Nonce, credential.Ciphertext,
		unixMillis(credential.ExpiresAt), now.UnixMilli(), now.UnixMilli()); err != nil {
		return Credential{}, fmt.Errorf("create credential: %w", err)
	}
	return credential, nil
}

func (r *Credentials) Get(ctx context.Context, accountID, id string) (Credential, error) {
	var credential Credential
	var expires sql.NullInt64
	var created, updated int64
	err := r.db.QueryRowContext(ctx, `SELECT id, account_id, format_version, key_version, nonce, ciphertext,
		expires_at, revision, created_at, updated_at FROM credentials WHERE account_id = ? AND id = ?`, accountID, id).
		Scan(&credential.ID, &credential.AccountID, &credential.FormatVersion, &credential.KeyVersion,
			&credential.Nonce, &credential.Ciphertext, &expires, &credential.Revision, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	if err != nil {
		return Credential{}, fmt.Errorf("get credential: %w", err)
	}
	credential.CreatedAt, credential.UpdatedAt = fromUnixMillis(created), fromUnixMillis(updated)
	if expires.Valid {
		value := fromUnixMillis(expires.Int64)
		credential.ExpiresAt = &value
	}
	return credential.clone(), nil
}

func (r *Credentials) GetDecrypted(ctx context.Context, accountID, id string, key crypto.MasterKey) ([]byte, error) {
	credential, err := r.Get(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	plaintext, err := crypto.Open(key, crypto.Envelope{
		FormatVersion: credential.FormatVersion, KeyVersion: credential.KeyVersion,
		Nonce: credential.Nonce, Ciphertext: credential.Ciphertext,
	}, "credentials", credential.ID, credential.AccountID)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), plaintext...), nil
}

// Update replaces one envelope only when credential.Revision matches the stored revision.
func (r *Credentials) Update(ctx context.Context, credential Credential) (Credential, error) {
	updated, err := r.Replace(ctx, []Credential{credential})
	if err != nil {
		return Credential{}, err
	}
	return updated[0], nil
}

// Replace atomically replaces all supplied envelopes, using each Revision as its expected value.
func (r *Credentials) Replace(ctx context.Context, credentials []Credential) ([]Credential, error) {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire credential connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return nil, fmt.Errorf("begin credential replacement: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	result := make([]Credential, len(credentials))
	for i, input := range credentials {
		credential := input.clone()
		expected := credential.Revision
		now := time.Now().UTC().Truncate(time.Millisecond)
		res, err := conn.ExecContext(ctx, `UPDATE credentials SET format_version = ?, key_version = ?, nonce = ?,
			ciphertext = ?, expires_at = ?, revision = revision + 1,
			updated_at = CASE WHEN updated_at >= ? THEN updated_at + 1 ELSE ? END
			WHERE id = ? AND account_id = ? AND revision = ?`, credential.FormatVersion, credential.KeyVersion,
			credential.Nonce, credential.Ciphertext, unixMillis(credential.ExpiresAt), now.UnixMilli(), now.UnixMilli(),
			credential.ID, credential.AccountID, expected)
		if err != nil {
			return nil, fmt.Errorf("update credential: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("check credential update: %w", err)
		}
		if affected == 0 {
			var exists int
			if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM credentials WHERE account_id = ? AND id = ?)`, credential.AccountID, credential.ID).Scan(&exists); err != nil {
				return nil, fmt.Errorf("check credential existence: %w", err)
			}
			if exists == 0 {
				return nil, ErrNotFound
			}
			return nil, ErrRevisionMismatch
		}
		credential.Revision = expected + 1
		var createdAt, updatedAt int64
		if err := conn.QueryRowContext(ctx, `SELECT created_at, updated_at FROM credentials WHERE account_id = ? AND id = ?`, credential.AccountID, credential.ID).Scan(&createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("read updated credential timestamps: %w", err)
		}
		credential.CreatedAt, credential.UpdatedAt = fromUnixMillis(createdAt), fromUnixMillis(updatedAt)
		result[i] = credential
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, fmt.Errorf("commit credential replacement: %w", err)
	}
	committed = true
	return result, nil
}

func unixMillis(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().UnixMilli()
}

func fromUnixMillis(value int64) time.Time { return time.UnixMilli(value).UTC() }
