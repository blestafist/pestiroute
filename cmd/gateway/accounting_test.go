package main

import (
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
