package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrAccountNotFound = errors.New("account not found")

// Account contains only safe account metadata; credentials are stored separately.
type Account struct {
	ID        string
	Connector string
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

type AccountFilter struct {
	Connector string
	Enabled   *bool
}

type Accounts struct{ db *sql.DB }

func NewAccounts(db *sql.DB) *Accounts { return &Accounts{db: db} }

func (r *Accounts) Create(ctx context.Context, account Account) (Account, error) {
	if account.ID == "" || account.Connector == "" {
		return Account{}, errors.New("account ID and connector are required")
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := r.db.ExecContext(ctx, `INSERT INTO accounts (id, connector, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`, account.ID, account.Connector, account.Enabled, now.UnixMilli(), now.UnixMilli()); err != nil {
		return Account{}, fmt.Errorf("create account: %w", err)
	}
	account.CreatedAt, account.UpdatedAt = now, now
	return account, nil
}

func (r *Accounts) Get(ctx context.Context, id string) (Account, error) {
	account, err := scanAccount(r.db.QueryRowContext(ctx, `SELECT id, connector, enabled, created_at, updated_at
		FROM accounts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrAccountNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account: %w", err)
	}
	return account, nil
}

func (r *Accounts) List(ctx context.Context, filter AccountFilter) ([]Account, error) {
	query := `SELECT id, connector, enabled, created_at, updated_at FROM accounts WHERE 1 = 1`
	args := make([]any, 0, 2)
	if filter.Connector != "" {
		query += ` AND connector = ?`
		args = append(args, filter.Connector)
	}
	if filter.Enabled != nil {
		query += ` AND enabled = ?`
		args = append(args, *filter.Enabled)
	}
	query += ` ORDER BY created_at ASC, id ASC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()
	accounts := make([]Account, 0)
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	return accounts, nil
}

func (r *Accounts) SetEnabled(ctx context.Context, id string, enabled bool) (Account, error) {
	now := time.Now().UTC().Truncate(time.Millisecond).UnixMilli()
	account, err := scanAccount(r.db.QueryRowContext(ctx, `UPDATE accounts SET enabled = ?,
		updated_at = CASE WHEN updated_at >= ? THEN updated_at + 1 ELSE ? END
		WHERE id = ? RETURNING id, connector, enabled, created_at, updated_at`, enabled, now, now, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrAccountNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("set account enabled: %w", err)
	}
	return account, nil
}

func (r *Accounts) Delete(ctx context.Context, id string) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check account deletion: %w", err)
	}
	if deleted == 0 {
		return ErrAccountNotFound
	}
	return nil
}

type accountScanner interface{ Scan(...any) error }

func scanAccount(row accountScanner) (Account, error) {
	var account Account
	var enabled, created, updated int64
	if err := row.Scan(&account.ID, &account.Connector, &enabled, &created, &updated); err != nil {
		return Account{}, err
	}
	account.Enabled = enabled != 0
	account.CreatedAt, account.UpdatedAt = fromUnixMillis(created), fromUnixMillis(updated)
	return account, nil
}
