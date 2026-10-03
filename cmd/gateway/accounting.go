package main

import (
	"context"
	"errors"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

type sqliteAccountingStore struct{ ledger *sqlite.Ledger }

func (s sqliteAccountingStore) Admit(ctx context.Context, in core.AccountingAdmission) error {
	request := sqlite.RequestRecord{
		ID: in.RequestID, VirtualKeyID: in.KeyID, KeyRevision: in.KeyRevision,
		PolicyID: in.PolicyID, PolicyRevision: in.PolicyRevision, Protocol: in.Protocol,
		Model: in.Model, RouteID: in.RouteID, State: "admitted",
	}
	attempt := sqlite.AttemptRecord{
		ID: in.AttemptID, RequestID: in.RequestID, Ordinal: 1, AccountID: in.AccountID,
		Connector: in.Connector, RouteID: in.RouteID, BudgetPolicy: in.BudgetPolicy,
		EstimateTokens: in.EstimateTokens, EstimateMethod: in.EstimateMethod, State: "reserved",
	}
	err := s.ledger.Admit(ctx, request, attempt, sqlite.ReservationRecord{AttemptID: in.AttemptID, EstimatedTokens: in.EstimateTokens})
	return accountingStoreError(ctx, coreAdmissionError(err))
}

func (s sqliteAccountingStore) BeginAttempt(ctx context.Context, in core.AccountingAdmission) error {
	attempt := sqlite.AttemptRecord{
		ID: in.AttemptID, RequestID: in.RequestID, AccountID: in.AccountID,
		Connector: in.Connector, RouteID: in.RouteID, BudgetPolicy: in.BudgetPolicy,
		EstimateTokens: in.EstimateTokens, EstimateMethod: in.EstimateMethod, State: "reserved",
	}
	err := s.ledger.BeginAttempt(ctx, attempt, sqlite.ReservationRecord{AttemptID: in.AttemptID, EstimatedTokens: in.EstimateTokens})
	return accountingStoreError(ctx, coreAdmissionError(err))
}

func coreAdmissionError(err error) error {
	if errors.Is(err, sqlite.ErrAdmissionLimit) {
		return core.ErrAdmissionLimit
	}
	return err
}

func accountingStoreError(ctx context.Context, err error) error {
	if err == nil || errors.Is(err, sqlite.ErrLedgerConflict) || errors.Is(err, sqlite.ErrLedgerNotFound) ||
		errors.Is(err, core.ErrAdmissionLimit) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return core.AccountingStorageFailure{Err: err}
}

func (s sqliteAccountingStore) RecordDispatchIntent(ctx context.Context, attemptID string, at time.Time) error {
	return accountingStoreError(ctx, s.ledger.RecordDispatchIntent(ctx, attemptID, at))
}

func (s sqliteAccountingStore) FinalizeAttempt(ctx context.Context, in core.AccountingTerminal) error {
	state := string(in.Outcome)
	if state == "incomplete" {
		state = "interrupted"
	}
	usage := sqlite.UsageRecord{
		AttemptID: in.AttemptID, InputTokens: in.Usage.InputTokens, OutputTokens: in.Usage.OutputTokens,
		ReasoningTokens: in.Usage.ReasoningTokens, CachedTokens: in.Usage.CachedTokens,
		Source: string(in.Usage.Source), Completeness: string(in.Usage.Completeness), RecordedAt: in.EndedAt,
	}
	terminal := sqlite.TerminalAttempt{
		AttemptID: in.AttemptID, State: state, Committed: in.Committed, Usage: usage, FinishedAt: in.EndedAt,
	}
	if in.Category != "" {
		category := string(in.Category)
		terminal.ErrorCategory = &category
	}
	if in.Reason != "" {
		terminal.ErrorReason = &in.Reason
	}
	return accountingStoreError(ctx, s.ledger.FinalizeAttempt(ctx, terminal))
}
