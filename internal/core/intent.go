package core

import (
	"context"
	"errors"
	"time"
)

// DispatchIntentRecorder persists the attempt's dispatch boundary before execution.
type DispatchIntentRecorder interface {
	RecordDispatchIntent(context.Context, string, time.Time) error
}

// ExecuteAfterDispatchIntent keeps upstream execution behind an acknowledged intent write.
func ExecuteAfterDispatchIntent(ctx context.Context, recorder DispatchIntentRecorder, attemptID string, at time.Time, execute func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if recorder == nil || attemptID == "" || at.IsZero() || execute == nil {
		return errors.New("invalid dispatch intent gate")
	}
	if err := recorder.RecordDispatchIntent(ctx, attemptID, at); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return execute()
}
