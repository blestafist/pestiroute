package main

import (
	"context"
	"errors"
	"testing"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestCoreAdmissionErrorMappingIsNarrow(t *testing.T) {
	limit := errors.Join(sqlite.ErrLedgerConflict, sqlite.ErrAdmissionLimit)
	if got := coreAdmissionError(limit); !errors.Is(got, core.ErrAdmissionLimit) {
		t.Fatalf("limit mapping = %v", got)
	}
	conflict := errors.New("stale authorization conflict")
	wrappedConflict := errors.Join(sqlite.ErrLedgerConflict, conflict)
	if got := coreAdmissionError(wrappedConflict); got != wrappedConflict || errors.Is(got, core.ErrAdmissionLimit) {
		t.Fatalf("non-limit conflict mapping = %v", got)
	}
}

func TestAccountingStoreErrorDistinguishesStorageFailures(t *testing.T) {
	for _, err := range []error{
		sqlite.ErrLedgerConflict,
		sqlite.ErrLedgerNotFound,
		core.ErrAdmissionLimit,
		context.Canceled,
		context.DeadlineExceeded,
	} {
		if got := accountingStoreError(context.Background(), err); got != err {
			t.Errorf("accountingStoreError(%v) = %T, want unchanged business/cancellation error", err, got)
		}
	}
	infrastructure := errors.New("private database detail")
	got := accountingStoreError(context.Background(), infrastructure)
	var failure core.AccountingStorageFailure
	if !errors.As(got, &failure) || !errors.Is(got, infrastructure) || got.Error() != "accounting storage failure" {
		t.Fatalf("infrastructure classification = %#v", got)
	}
}

func TestAccountingStoreErrorNormalizesDriverInterruptFromCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := accountingStoreError(ctx, errors.New("driver interrupted operation"))
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("canceled context + driver interrupt = %T %v, want context.Canceled", got, got)
	}
	var failure core.AccountingStorageFailure
	if errors.As(got, &failure) {
		t.Fatalf("canceled driver interrupt was marked as storage failure: %#v", got)
	}
}
