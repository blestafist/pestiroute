package core

import (
	"context"
	"errors"
	"time"
)

var ErrAdmissionLimit = errors.New("admission limit exceeded")

// AccountingStorageFailure marks errors that indicate the durable store cannot
// safely serve accounting operations. Business conflicts and cancellation must
// not be wrapped in this type.
type AccountingStorageFailure struct{ Err error }

func (AccountingStorageFailure) Error() string { return "accounting storage failure" }

func (e AccountingStorageFailure) Unwrap() error { return e.Err }

// AccountingStore is the authoritative request/attempt ledger. Implementations
// acknowledge each operation only after its durable transaction commits.
type AccountingStore interface {
	Admit(context.Context, AccountingAdmission) error
	BeginAttempt(context.Context, AccountingAdmission) error
	RecordDispatchIntent(context.Context, string, time.Time) error
	FinalizeAttempt(context.Context, AccountingTerminal) error
}

type AccountingAdmission struct {
	RequestID, AttemptID, KeyID, PolicyID string
	KeyRevision, PolicyRevision           int64
	Protocol, Model, RouteID              string
	AccountID, Connector                  string
	EstimateTokens                        int64
	EstimateMethod, BudgetPolicy          string
}

type AccountingTerminal struct {
	AttemptID string
	Outcome   Outcome
	Committed bool
	Usage     UsageReport
	Category  ErrorCategory
	Reason    string
	EndedAt   time.Time
}
