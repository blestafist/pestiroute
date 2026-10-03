package core

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryDeadlineHeadRace(t *testing.T) {
	for range 100 {
		ctx, cancel := context.WithCancelCause(context.Background())
		deadline := newRetryDeadline(ctx, time.Hour, cancel)
		deadline.start()
		var finalizations atomic.Int32
		stream := &attemptStream{
			source: NewCheckedStream(&scriptedStream{frames: []StreamFrame{{Type: FrameHead, Head: &HeadFrame{Protocol: m1Protocol}}}}, m1Protocol),
			ctx:    ctx,
			result: AttemptResult{RequestID: "request", Scope: AttemptScope{ID: "attempt", AccountID: "account", Mode: ModeNative}, Outcome: OutcomeIncomplete},
			finish: func(AttemptResult) { finalizations.Add(1) },
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; deadline.expire() }()
		var frame StreamFrame
		var nextErr error
		go func() { defer wg.Done(); <-start; frame, nextErr = stream.Next(ctx) }()
		close(start)
		wg.Wait()
		_ = stream.Close()
		headObserved := nextErr == nil && frame.Type == FrameHead
		timeoutObserved := nextErr == context.DeadlineExceeded
		if (!headObserved && !timeoutObserved) || !deadline.isExpired() || context.Cause(ctx) != context.DeadlineExceeded || finalizations.Load() != 1 {
			t.Fatalf("deadline/Head race: frame=%+v next=%v cause=%v finalized=%d", frame, nextErr, context.Cause(ctx), finalizations.Load())
		}
		deadline.cleanup()
	}
}
