package antigravity

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestM048SupplementalUsageMetadataValidation(t *testing.T) {
	valid := []string{
		`{"cachedContentTokenCount":2}`,
		`{"thoughtsTokenCount":3}`,
		`{"toolUsePromptTokenCount":4}`,
		`{"promptTokensDetails":[{"modality":"TEXT","tokenCount":1}]}`,
		`{"promptTokensDetails":[{"modality":"MODALITY_UNSPECIFIED","tokenCount":0}]}`,
		`{"cacheTokensDetails":[{"modality":"IMAGE","tokenCount":2}]}`,
		`{"candidatesTokensDetails":[{"modality":"VIDEO","tokenCount":3}]}`,
		`{"toolUsePromptTokensDetails":[{"modality":"AUDIO","tokenCount":4},{"modality":"DOCUMENT","tokenCount":0}]}`,
		`{"serviceTier":"unspecified"}`,
		`{"serviceTier":"standard"}`,
		`{"serviceTier":"flex"}`,
		`{"serviceTier":"priority"}`,
		`{"promptTokenCount":5,"candidatesTokenCount":6,"totalTokenCount":12,"cachedContentTokenCount":1,"thoughtsTokenCount":2,"toolUsePromptTokenCount":3,"promptTokensDetails":[],"cacheTokensDetails":[],"candidatesTokensDetails":[],"toolUsePromptTokensDetails":[],"serviceTier":"standard"}`,
	}
	for _, raw := range valid {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseUsageMetadata(json.RawMessage(raw)); err != nil {
				t.Fatalf("parseUsageMetadata(%s) error = %v", raw, err)
			}
		})
	}

	invalid := []struct {
		raw  string
		kind usageSubkind
	}{
		{`{"cachedContentTokenCount":-1}`, usageNegativeRange},
		{`{"thoughtsTokenCount":2147483648}`, usageInvalidType},
		{`{"toolUsePromptTokenCount":1.5}`, usageInvalidType},
		{`{"promptTokensDetails":{}}`, usageInvalidType},
		{`{"promptTokensDetails":[{"modality":"UNKNOWN","tokenCount":1}]}`, usageInvalidType},
		{`{"cacheTokensDetails":[{"modality":"TEXT","tokenCount":-1}]}`, usageNegativeRange},
		{`{"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":1,"extra":0}]}`, usageInvalidType},
		{`{"toolUsePromptTokensDetails":[{"modality":"TEXT"}]}`, usageInvalidType},
		{`{"serviceTier":"FAST"}`, usageInvalidType},
		{`{"serviceTier":1}`, usageInvalidType},
		{`{"trafficType":"standard"}`, usageUnknownField},
		{`{"cached_content_token_count":1}`, usageUnknownField},
	}
	for _, tc := range invalid {
		t.Run(tc.raw, func(t *testing.T) {
			_, err := parseUsageMetadata(json.RawMessage(tc.raw))
			var classified invalidUsageError
			if !errors.As(err, &classified) || classified.subkind != tc.kind {
				t.Fatalf("error=%v classification=%#v want subkind=%s", err, classified, tc.kind)
			}
		})
	}
}

func TestM048SupplementalUsageTrailerDiagnosticsAndAccounting(t *testing.T) {
	trailer := []byte(`{"response":{"usageMetadata":{"thoughtsTokenCount":3,"serviceTier":"standard","promptTokensDetails":[{"modality":"TEXT","tokenCount":2}]}}}`)
	if !usageOnlyFrame(trailer) {
		t.Fatal("documented supplemental-only trailer was rejected")
	}
	if usageOnlyFrame([]byte(`{"response":{"usageMetadata":{"trafficType":"standard"}}}`)) {
		t.Fatal("undocumented trailer key was admitted")
	}

	usage, err := parseUsageMetadata(json.RawMessage(`{"promptTokenCount":7,"candidatesTokenCount":9,"totalTokenCount":20,"thoughtsTokenCount":4,"toolUsePromptTokenCount":2}`))
	if err != nil {
		t.Fatal(err)
	}
	report, err := parseUsage(usage)
	if err != nil || report == nil || report.InputTokens == nil || *report.InputTokens != 7 || report.OutputTokens == nil || *report.OutputTokens != 9 {
		t.Fatalf("report=%#v error=%v", report, err)
	}
	known := map[string]usageKnownField{
		"promptTokenCount":           usageKnownPromptTokenCount,
		"candidatesTokenCount":       usageKnownCandidatesTokenCount,
		"totalTokenCount":            usageKnownTotalTokenCount,
		"cachedContentTokenCount":    usageKnownCachedContentTokenCount,
		"thoughtsTokenCount":         usageKnownThoughtsTokenCount,
		"toolUsePromptTokenCount":    usageKnownToolUsePromptTokenCount,
		"promptTokensDetails":        usageKnownPromptTokensDetails,
		"cacheTokensDetails":         usageKnownCacheTokensDetails,
		"candidatesTokensDetails":    usageKnownCandidatesTokensDetails,
		"toolUsePromptTokensDetails": usageKnownToolUsePromptTokens,
		"serviceTier":                usageKnownServiceTier,
	}
	for key, want := range known {
		if got := classifyUsageKnownField(key); got != want {
			t.Errorf("classifyUsageKnownField(%q) = %q, want %q", key, got, want)
		}
	}
	if classifyUsageKnownField("trafficType") != usageKnownOther {
		t.Fatal("undocumented key received a documented-field diagnostic")
	}

	s := newStream(io.NopCloser(strings.NewReader("")), "r", "m")
	frame := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":9,"thoughtsTokenCount":4,"serviceTier":"standard"}}}`)
	if err := s.consume(frame); err != nil {
		t.Fatalf("candidate frame with supplemental usage rejected: %v", err)
	}
	if s.usage == nil || s.usage.InputTokens == nil || *s.usage.InputTokens != 7 || s.usage.OutputTokens == nil || *s.usage.OutputTokens != 9 {
		t.Fatalf("candidate frame accounted supplemental tokens: %#v", s.usage)
	}

	var logs strings.Builder
	logged := newStream(io.NopCloser(strings.NewReader("")), "r", "m", slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	_, err = parseUsageMetadata(json.RawMessage(`{"serviceTier":"PRIVATE-CANARY","thoughtsTokenCount":987654}`))
	if !errors.Is(err, errInvalidUsage) {
		t.Fatalf("invalid supplemental value error=%v", err)
	}
	logged.logStreamError(err, true)
	if strings.Contains(logs.String(), "PRIVATE-CANARY") || strings.Contains(logs.String(), "987654") {
		t.Fatalf("supplemental usage values leaked to diagnostics: %q", logs.String())
	}
}
