package core

import (
	"errors"
	"math"
)

const AdmissionWindowMillis int64 = 60_000

var ErrInvalidAdmissionValue = errors.New("invalid admission value")

// WindowCutoff returns the exclusive start of the rolling 60-second window.
func WindowCutoff(now int64) (int64, error) {
	if now < 0 {
		return 0, ErrInvalidAdmissionValue
	}
	return now - AdmissionWindowMillis, nil
}

// InAdmissionWindow excludes the cutoff and future timestamps. previousNow
// detects a wall-clock rollback between samples; pass now for the first sample.
func InAdmissionWindow(timestamp, now, previousNow int64) (bool, error) {
	if timestamp < 0 || now < 0 || previousNow < 0 || now < previousNow {
		return false, ErrInvalidAdmissionValue
	}
	cutoff, err := WindowCutoff(now)
	if err != nil {
		return false, err
	}
	return timestamp > cutoff && timestamp <= now, nil
}

// AdmitRPM returns whether one request slot can be consumed and the resulting
// accepted-request count. Attempts after the initial request do not call this
// with a slot; use AdmissionUnits to distinguish request from attempt identity.
func AdmitRPM(current, limit int64) (bool, int64, error) {
	if current < 0 {
		return false, current, ErrInvalidAdmissionValue
	}
	if limit <= 0 || current >= limit {
		return false, current, nil
	}
	if current == math.MaxInt64 {
		return false, current, ErrInvalidAdmissionValue
	}
	return true, current + 1, nil
}

// AdmitTPM checks held reservations, settled charges, and the candidate hold.
func AdmitTPM(held, settled, estimate, limit int64) (bool, error) {
	if held < 0 || settled < 0 || estimate < 0 {
		return false, ErrInvalidAdmissionValue
	}
	if limit <= 0 {
		return false, nil
	}
	used, ok := checkedTokenAdd(held, settled)
	if !ok {
		return false, nil
	}
	used, ok = checkedTokenAdd(used, estimate)
	if !ok {
		return false, nil
	}
	return used <= limit, nil
}

// AdmissionUnits assigns RPM to the client request and token budget to each
// attempt: the first attempt consumes both, later attempts only tokens.
func AdmissionUnits(initialRequest bool, estimate int64) (rpmSlots, tokenHold int64, err error) {
	if estimate < 0 {
		return 0, 0, ErrInvalidAdmissionValue
	}
	if initialRequest {
		return 1, estimate, nil
	}
	return 0, estimate, nil
}

// EffectiveTokenCharge calculates billable input+output tokens. Usage detail
// counters remain untouched and are not added to the total.
func EffectiveTokenCharge(estimate int64, usage *UsageReport, dispatched bool) (int64, error) {
	if estimate < 0 {
		return 0, ErrInvalidAdmissionValue
	}
	if usage != nil {
		for _, counter := range []*int64{usage.InputTokens, usage.OutputTokens, usage.ReasoningTokens, usage.CachedTokens} {
			if counter != nil && *counter < 0 {
				return 0, ErrInvalidAdmissionValue
			}
		}
	}
	if !dispatched {
		return 0, nil
	}
	if usage == nil {
		return estimate, nil
	}
	if usage.Completeness == UsageComplete && usage.InputTokens != nil && usage.OutputTokens != nil {
		if total, ok := checkedTokenAdd(*usage.InputTokens, *usage.OutputTokens); ok {
			return total, nil
		}
		return 0, ErrInvalidAdmissionValue
	}
	lower := int64(0)
	for _, counter := range []*int64{usage.InputTokens, usage.OutputTokens} {
		if counter != nil {
			var ok bool
			lower, ok = checkedTokenAdd(lower, *counter)
			if !ok {
				return 0, ErrInvalidAdmissionValue
			}
		}
	}
	if lower > estimate {
		return lower, nil
	}
	return estimate, nil
}

func checkedTokenAdd(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}
