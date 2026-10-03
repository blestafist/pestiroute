package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

type intentRecorderFunc func(context.Context, string, time.Time) error

func (f intentRecorderFunc) RecordDispatchIntent(ctx context.Context, id string, at time.Time) error {
	return f(ctx, id, at)
}

func TestGatedExecuteAfterDispatchIntent(t *testing.T) {
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	writeErr := errors.New("intent write failed")
	for _, tc := range []struct {
		name              string
		record            intentRecorderFunc
		cancel            bool
		cancelAfterRecord bool
		wantErr           error
		wantRuns          int
	}{
		{name: "write failure", record: func(context.Context, string, time.Time) error { return writeErr }, wantErr: writeErr},
		{name: "write timeout", record: func(context.Context, string, time.Time) error { return context.DeadlineExceeded }, wantErr: context.DeadlineExceeded},
		{name: "cancelled before write", record: func(context.Context, string, time.Time) error {
			t.Fatal("recorder called after cancellation")
			return nil
		}, cancel: true, wantErr: context.Canceled},
		{name: "cancelled before acknowledgement", record: func(_ context.Context, _ string, _ time.Time) error { return nil }, cancelAfterRecord: true, wantErr: context.Canceled},
		{name: "acknowledged", record: func(context.Context, string, time.Time) error { return nil }, wantRuns: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			} else if tc.cancelAfterRecord {
				tc.record = func(_ context.Context, _ string, _ time.Time) error { cancel(); return nil }
			}
			runs := 0
			err := ExecuteAfterDispatchIntent(ctx, tc.record, "attempt", at, func() error { runs++; return nil })
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if runs != tc.wantRuns {
				t.Fatalf("execute calls = %d, want %d", runs, tc.wantRuns)
			}
		})
	}
}
