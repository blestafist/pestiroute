package core

import (
	"math"
	"testing"
)

func TestAdmissionWindowBoundariesAndRollback(t *testing.T) {
	const now = int64(100_000)
	cutoff, err := WindowCutoff(now)
	if err != nil || cutoff != now-AdmissionWindowMillis {
		t.Fatalf("WindowCutoff = %d, %v", cutoff, err)
	}
	if _, err := WindowCutoff(-1); err == nil {
		t.Fatal("WindowCutoff accepted a negative timestamp")
	}
	for _, tc := range []struct {
		stamp int64
		want  bool
	}{{cutoff, false}, {cutoff + 1, true}, {now, true}, {now + 1, false}} {
		got, err := InAdmissionWindow(tc.stamp, now, now)
		if err != nil || got != tc.want {
			t.Errorf("InAdmissionWindow(%d) = %v, %v; want %v", tc.stamp, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ stamp, now, previous int64 }{{0, -1, 0}, {-1, now, now}, {now, now - 1, now}} {
		if got, err := InAdmissionWindow(tc.stamp, tc.now, tc.previous); err == nil || got {
			t.Errorf("InAdmissionWindow(%+v) = %v, %v; want fail-closed error", tc, got, err)
		}
	}
}

func TestAdmissionLimitsAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		current, limit int64
		admit          bool
		count          int64
	}{{4, 5, true, 5}, {5, 5, false, 5}, {0, 0, false, 0}} {
		got, count, err := AdmitRPM(tc.current, tc.limit)
		if err != nil || got != tc.admit || count != tc.count {
			t.Errorf("AdmitRPM(%d,%d) = %v,%d,%v", tc.current, tc.limit, got, count, err)
		}
	}
	if _, _, err := AdmitRPM(math.MaxInt64, math.MaxInt64); err != nil {
		t.Fatalf("full RPM counter should deny without overflow: %v", err)
	}
	if _, _, err := AdmitRPM(-1, 1); err == nil {
		t.Fatal("AdmitRPM accepted a negative count")
	}
	for _, tc := range []struct {
		held, settled, estimate, limit int64
		admit                          bool
	}{{8, 2, 0, 10, true}, {8, 2, 1, 10, false}, {math.MaxInt64, 1, 0, math.MaxInt64, false}, {math.MaxInt64 - 1, 0, 2, math.MaxInt64, false}, {0, 0, 1, 0, false}} {
		got, err := AdmitTPM(tc.held, tc.settled, tc.estimate, tc.limit)
		if err != nil || got != tc.admit {
			t.Errorf("AdmitTPM(%+v) = %v,%v", tc, got, err)
		}
	}
	for _, values := range [][3]int64{{-1, 0, 0}, {0, -1, 0}, {0, 0, -1}} {
		if got, err := AdmitTPM(values[0], values[1], values[2], 10); err == nil || got {
			t.Errorf("AdmitTPM(%v) = %v, %v; want fail-closed error", values, got, err)
		}
	}
	for _, tc := range []struct {
		initial bool
		rpm     int64
	}{{true, 1}, {false, 0}} {
		rpm, hold, err := AdmissionUnits(tc.initial, 9)
		if err != nil || rpm != tc.rpm || hold != 9 {
			t.Errorf("AdmissionUnits(%v) = %d,%d,%v", tc.initial, rpm, hold, err)
		}
	}
	if _, _, err := AdmissionUnits(true, -1); err == nil {
		t.Fatal("AdmissionUnits accepted a negative estimate")
	}
}

func TestEffectiveTokenCharge(t *testing.T) {
	input, output, reasoning, cached := int64(4), int64(3), int64(20), int64(10)
	negative, maximum := int64(-1), int64(math.MaxInt64)
	one := int64(1)
	for _, tc := range []struct {
		name       string
		estimate   int64
		usage      *UsageReport
		dispatched bool
		want       int64
		wantErr    bool
	}{{"exact below estimate", 12, &UsageReport{InputTokens: &input, OutputTokens: &output, Completeness: UsageComplete}, true, 7, false},
		{"partial lower bound", 5, &UsageReport{InputTokens: &input, ReasoningTokens: &reasoning, CachedTokens: &cached}, true, 5, false},
		{"partial above estimate", 5, &UsageReport{InputTokens: &input, OutputTokens: &output}, true, 7, false},
		{"complete marker with missing output uses lower bound", 2, &UsageReport{InputTokens: &input, Completeness: UsageComplete}, true, 4, false},
		{"complete marker with missing input retains estimate", 5, &UsageReport{OutputTokens: &output, Completeness: UsageComplete}, true, 5, false},
		{"unknown retains estimate", 5, nil, true, 5, false},
		{"undispatched releases", 5, &UsageReport{InputTokens: &input}, false, 0, false},
		{"negative input rejected", 5, &UsageReport{InputTokens: &negative}, true, 0, true},
		{"negative output rejected", 5, &UsageReport{OutputTokens: &negative}, true, 0, true},
		{"negative detail rejected", 5, &UsageReport{ReasoningTokens: &negative}, true, 0, true},
		{"complete overflow rejected", 0, &UsageReport{InputTokens: &maximum, OutputTokens: &output, Completeness: UsageComplete}, true, 0, true},
		{"partial lower-bound overflow rejected", 0, &UsageReport{InputTokens: &maximum, OutputTokens: &one, Completeness: UsagePartial}, true, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EffectiveTokenCharge(tc.estimate, tc.usage, tc.dispatched)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("EffectiveTokenCharge = %d, %v; want %d, error=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
	if reasoning != 20 || cached != 10 {
		t.Fatal("detail counters were modified")
	}
}
