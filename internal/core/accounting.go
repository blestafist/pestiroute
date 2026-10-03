package core

import (
	"context"
	"errors"
	"time"
)

var ErrAdmissionLimit = errors.New("admission limit exceeded")

// AccountingStore is the authoritative request/attempt ledger. Implementations
// acknowledge each operation only after its durable transaction commits.
type AccountingStore interface {
	Admit(context.Context, AccountingAdmission) error
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
