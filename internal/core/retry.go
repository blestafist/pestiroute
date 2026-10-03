package core

import (
	"context"
	"time"
)

type RetryDenial string

const (
	RetryAllowed             RetryDenial = ""
	RetryNotRetryable        RetryDenial = "not_retryable"
	RetryDispositionDenied   RetryDenial = "retry_disposition_denied"
	RetryCommitted           RetryDenial = "committed"
	RetryAttemptLimit        RetryDenial = "attempt_limit"
	RetryCancelled           RetryDenial = "cancelled"
	RetryDeadline            RetryDenial = "deadline"
	RetryCandidatesExhausted RetryDenial = "candidates_exhausted"
	RetryAfterInvalid        RetryDenial = "invalid_retry_after"
	RetryAfterDeadline       RetryDenial = "retry_after_deadline"
)

// RetryInput is the immutable state needed to decide whether the next
// pre-authorized candidate may be attempted. AttemptIndex and candidate indexes
// are zero-based; callers must perform all current admission checks themselves.
type RetryInput struct {
	Context             context.Context
	Now                 time.Time
	Deadline            time.Time
	AttemptIndex        int
	MaxAttempts         int
	Committed           bool
	Failure             *GatewayError
	RemainingCandidates []int
}

type RetryDecision struct {
	Allowed        bool
	Denial         RetryDenial
	CandidateIndex int
	Delay          time.Duration
}

// DecideRetry performs no waiting, scheduling, candidate discovery, or
// execution. A caller that applies Delay must wait using its cancellable context.
func DecideRetry(in RetryInput) RetryDecision {
	deny := func(reason RetryDenial) RetryDecision { return RetryDecision{Denial: reason} }
	if in.Failure == nil || !in.Failure.Retryable {
		return deny(RetryNotRetryable)
	}
	if in.Failure.RetryDisposition != RetrySafe {
		return deny(RetryDispositionDenied)
	}
	if in.Committed {
		return deny(RetryCommitted)
	}
	if in.MaxAttempts <= 0 || in.AttemptIndex < 0 || in.AttemptIndex+1 >= in.MaxAttempts {
		return deny(RetryAttemptLimit)
	}
	if in.Context == nil || in.Context.Err() != nil {
		return deny(RetryCancelled)
	}
	if in.Deadline.IsZero() || in.Now.IsZero() || !in.Now.Before(in.Deadline) {
		return deny(RetryDeadline)
	}
	if len(in.RemainingCandidates) == 0 {
		return deny(RetryCandidatesExhausted)
	}
	remaining := in.Deadline.Sub(in.Now)
	var delay time.Duration
	if in.Failure.RetryAfter != nil {
		delay = *in.Failure.RetryAfter
		if delay < 0 {
			return deny(RetryAfterInvalid)
		}
		if delay > remaining {
			return deny(RetryAfterDeadline)
		}
	}
	if in.Context.Err() != nil {
		return deny(RetryCancelled)
	}
	return RetryDecision{Allowed: true, CandidateIndex: in.RemainingCandidates[0], Delay: delay}
}
