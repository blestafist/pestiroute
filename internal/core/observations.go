package core

import (
	"fmt"
	"sync"
	"time"
)

// AttemptObservation contains classified metadata only; it never retains request
// payloads, credentials, provider diagnostics, or client-facing error text.
type AttemptObservation struct {
	RequestID        string
	AttemptID        string
	Route            RouteIdentity
	Adapter          InstanceID
	Connector        InstanceID
	AccountID        string
	Mode             string
	StartedAt        time.Time
	EndedAt          time.Time
	Duration         time.Duration
	Committed        bool
	Outcome          Outcome
	ErrorCategory    ErrorCategory
	Retryable        bool
	RetryDisposition RetryDisposition
	Usage            *UsageReport
}

// AttemptObservationSink implementations must return promptly. TryRecord must
// not retain mutable aliases supplied by the caller.
type AttemptObservationSink interface {
	TryRecord(AttemptObservation) bool
}

// InMemoryAttemptObservations retains only the newest capacity observations.
type InMemoryAttemptObservations struct {
	mu      sync.RWMutex
	entries []AttemptObservation
	next    int
	count   int
}

func NewInMemoryAttemptObservations(capacity int) (*InMemoryAttemptObservations, error) {
	if capacity < 1 {
		return nil, fmt.Errorf("attempt observation capacity must be positive")
	}
	return &InMemoryAttemptObservations{entries: make([]AttemptObservation, capacity)}, nil
}

func (s *InMemoryAttemptObservations) TryRecord(observation AttemptObservation) bool {
	s.mu.Lock()
	s.entries[s.next] = cloneAttemptObservation(observation)
	s.next = (s.next + 1) % len(s.entries)
	if s.count < len(s.entries) {
		s.count++
	}
	s.mu.Unlock()
	return true
}

// Snapshot returns an oldest-to-newest copy independent of future writes.
func (s *InMemoryAttemptObservations) Snapshot() []AttemptObservation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]AttemptObservation, s.count)
	start := (s.next - s.count + len(s.entries)) % len(s.entries)
	for i := range result {
		result[i] = cloneAttemptObservation(s.entries[(start+i)%len(s.entries)])
	}
	return result
}

func observationFromResult(result AttemptResult) AttemptObservation {
	observation := AttemptObservation{
		RequestID: result.RequestID, AttemptID: result.Scope.ID, Route: result.Route,
		Adapter: result.Adapter, Connector: result.Connector, AccountID: result.Scope.AccountID,
		Mode: result.Scope.Mode, StartedAt: result.StartedAt, EndedAt: result.EndedAt,
		Duration: result.EndedAt.Sub(result.StartedAt), Committed: result.Committed,
		Outcome: result.Outcome,
	}
	if result.Error != nil {
		observation.ErrorCategory = result.Error.Category
		observation.Retryable = result.Error.Retryable
		observation.RetryDisposition = result.Error.RetryDisposition
	}
	if result.HasUsage {
		usage := result.Usage
		observation.Usage = &usage
	}
	return cloneAttemptObservation(observation)
}

func cloneAttemptObservation(observation AttemptObservation) AttemptObservation {
	if observation.Usage != nil {
		usage := *observation.Usage
		usage.InputTokens = cloneInt64(usage.InputTokens)
		usage.OutputTokens = cloneInt64(usage.OutputTokens)
		usage.ReasoningTokens = cloneInt64(usage.ReasoningTokens)
		usage.CachedTokens = cloneInt64(usage.CachedTokens)
		observation.Usage = &usage
	}
	return observation
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
