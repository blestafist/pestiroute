package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var ErrRequestBudgetExceeded = errors.New("operator request budget exceeded")

const AntigravityOperatorBudgetID = "m10-antigravity-authorized-account"

type OperatorRequestBudgets struct{ db *sql.DB }

func NewOperatorRequestBudgets(db *sql.DB) *OperatorRequestBudgets {
	return &OperatorRequestBudgets{db: db}
}

// VerifyCeiling checks the migration-provisioned budget without creating or resetting it.
func (b *OperatorRequestBudgets) VerifyCeiling(ctx context.Context, accountID string, ceiling int64) error {
	if b == nil || b.db == nil || strings.TrimSpace(accountID) == "" || ceiling < 0 {
		return errors.New("invalid request budget configuration")
	}
	var stored int64
	if err := b.db.QueryRowContext(ctx, `SELECT ceiling FROM operator_request_budgets WHERE account_id=?`, accountID).Scan(&stored); err != nil {
		return err
	}
	if stored != ceiling {
		return errors.New("request budget ceiling mismatch")
	}
	return nil
}

// Reserve commits one dispatch slot under SQLite's cross-process write lock.
func (b *OperatorRequestBudgets) Reserve(ctx context.Context, accountID string) (retErr error) {
	if b == nil || b.db == nil || strings.TrimSpace(accountID) == "" {
		return errors.New("request budget store unavailable")
	}
	conn, err := b.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, conn.Close()) }()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, rollbackErr := conn.ExecContext(context.Background(), `ROLLBACK`)
			if rollbackErr != nil && !strings.Contains(rollbackErr.Error(), "no transaction is active") {
				retErr = errors.Join(retErr, rollbackErr)
			}
		}
	}()
	result, err := conn.ExecContext(ctx, `UPDATE operator_request_budgets SET consumed=consumed+1, updated_at=? WHERE account_id=? AND consumed<ceiling`, time.Now().UnixMilli(), accountID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		var exists int
		if err := conn.QueryRowContext(ctx, `SELECT 1 FROM operator_request_budgets WHERE account_id=?`, accountID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return errors.New("request budget is not configured")
		} else if err != nil {
			return err
		}
		return ErrRequestBudgetExceeded
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}
