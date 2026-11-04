package antigravity

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestM047UsageRejectionClassificationAndPriority(t *testing.T) {
	tests := []struct {
		name, raw string
		want      usageSubkind
		field     usageKnownField
	}{
		{"invalid_json", `{`, usageInvalidJSON, ""},
		{"non_object", `[]`, usageNonObject, ""},
		{"unknown", `{"mysterySecretKey":1}`, usageUnknownField, usageKnownOther},
		{"known_field", `{"thoughts_token_count":1}`, usageUnknownField, usageKnownThoughtsTokenCount},
		{"invalid_type", `{"promptTokenCount":1.5}`, usageInvalidType, ""},
		{"negative", `{"promptTokenCount":-8}`, usageNegativeRange, ""},
		{"type_precedes_range", `{"promptTokenCount":-8,"candidatesTokenCount":"bad"}`, usageInvalidType, ""},
		{"unknown_precedes_type_and_range", `{"cache_token_count":1,"thoughts_token_count":2,"promptTokenCount":"PRIVATE-CANARY"}`, usageUnknownField, usageKnownThoughtsTokenCount},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseUsageMetadata([]byte(tc.raw))
			if !errors.Is(err, errInvalidUsage) {
				t.Fatalf("error=%v, want errors.Is(err, errInvalidUsage)", err)
			}
			var classified invalidUsageError
			if !errors.As(err, &classified) || classified.subkind != tc.want || classified.knownField != tc.field {
				t.Fatalf("classification=%#v, want subkind=%q field=%q", classified, tc.want, tc.field)
			}
		})
	}
	negative := int64(-1)
	if _, err := parseUsage(&usageMetadata{Prompt: &negative}); !errors.Is(err, errInvalidUsage) {
		t.Fatalf("parseUsage error=%v, want errors.Is(err, errInvalidUsage)", err)
	}
}

func TestM047UsageDiagnosticsAreSecretSafe(t *testing.T) {
	for _, precommit := range []bool{true, false} {
		var logs strings.Builder
		s := newStream(io.NopCloser(strings.NewReader("")), "r", "m", slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		_, err := parseUsageMetadata([]byte(`{"PRIVATE-UNVETTED-FIELD-CANARY":907,"promptTokenCount":-12345}`))
		if !errors.Is(err, errInvalidUsage) {
			t.Fatalf("error=%v, want invalid usage", err)
		}
		s.logStreamError(err, precommit)
		output := logs.String()
		if !strings.Contains(output, "subkind=unknown_field") || !strings.Contains(output, "known_field=other") || strings.Contains(output, "PRIVATE-") || strings.Contains(output, "907") || strings.Contains(output, "12345") {
			t.Fatalf("unsafe or incomplete diagnostic: %q", output)
		}
	}
}
