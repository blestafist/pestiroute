package core

import (
	"context"
	"testing"
	"time"
)

func TestRetryDecision(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	after := time.Second
	base := RetryInput{
		Context: context.Background(), Now: now, Deadline: now.Add(3 * time.Second),
		AttemptIndex: 0, MaxAttempts: 2,
		Failure:             &GatewayError{Retryable: true, RetryDisposition: RetrySafe},
		RemainingCandidates: []int{4, 8},
	}
	tests := []struct {
		name   string
		mutate func(*RetryInput)
		denial RetryDenial
		index  int
		delay  time.Duration
	}{
		{name: "allow", index: 4},
		{name: "retry after", mutate: func(in *RetryInput) { in.Failure.RetryAfter = &after }, index: 4, delay: after},
		{name: "not retryable", mutate: func(in *RetryInput) { in.Failure.Retryable = false }, denial: RetryNotRetryable},
		{name: "unsafe", mutate: func(in *RetryInput) { in.Failure.RetryDisposition = RetryUnsafe }, denial: RetryDispositionDenied},
		{name: "unknown", mutate: func(in *RetryInput) { in.Failure.RetryDisposition = RetryUnknown }, denial: RetryDispositionDenied},
		{name: "unspecified disposition", mutate: func(in *RetryInput) { in.Failure.RetryDisposition = "" }, denial: RetryDispositionDenied},
		{name: "committed", mutate: func(in *RetryInput) { in.Committed = true }, denial: RetryCommitted},
		{name: "attempt limit", mutate: func(in *RetryInput) { in.MaxAttempts = 1 }, denial: RetryAttemptLimit},
		{name: "cancelled", mutate: func(in *RetryInput) { ctx, cancel := context.WithCancel(in.Context); cancel(); in.Context = ctx }, denial: RetryCancelled},
		{name: "no deadline", mutate: func(in *RetryInput) { in.Deadline = time.Time{} }, denial: RetryDeadline},
		{name: "deadline reached", mutate: func(in *RetryInput) { in.Now = in.Deadline }, denial: RetryDeadline},
		{name: "no candidates", mutate: func(in *RetryInput) { in.RemainingCandidates = nil }, denial: RetryCandidatesExhausted},
		{name: "negative retry after", mutate: func(in *RetryInput) { d := -time.Second; in.Failure.RetryAfter = &d }, denial: RetryAfterInvalid},
		{name: "retry after exceeds deadline", mutate: func(in *RetryInput) { d := 3*time.Second + 1; in.Failure.RetryAfter = &d }, denial: RetryAfterDeadline},
		{name: "retry after equals deadline", mutate: func(in *RetryInput) { d := 3 * time.Second; in.Failure.RetryAfter = &d }, index: 4, delay: 3 * time.Second},
		{name: "combined negatives", mutate: func(in *RetryInput) { in.Committed = true; in.Failure.Retryable = false }, denial: RetryNotRetryable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base
			in.Failure = cloneGatewayError(base.Failure)
			if tt.mutate != nil {
				tt.mutate(&in)
			}
			got := DecideRetry(in)
			if tt.denial != "" {
				if got.Allowed || got.Denial != tt.denial {
					t.Fatalf("DecideRetry() = %+v, want denial %q", got, tt.denial)
				}
				return
			}
			if !got.Allowed || got.Denial != "" || got.CandidateIndex != tt.index || got.Delay != tt.delay {
				t.Fatalf("DecideRetry() = %+v, want allowed candidate %d with delay %s", got, tt.index, tt.delay)
			}
		})
	}
}

func cloneGatewayError(in *GatewayError) *GatewayError {
	clone := *in
	if in.RetryAfter != nil {
		d := *in.RetryAfter
		clone.RetryAfter = &d
	}
	return &clone
}
