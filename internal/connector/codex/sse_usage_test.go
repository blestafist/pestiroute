package codex

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestUsageExtraction(t *testing.T) {
	for _, tc := range []struct {
		name         string
		json         string
		found        bool
		invalid      bool
		input        *int64
		output       *int64
		cached       *int64
		reasoning    *int64
		completeness core.UsageCompleteness
	}{
		{"inclusive and subsets", `{"response":{"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":40},"output_tokens_details":{"reasoning_tokens":5}}}}`, true, false, ptr64(100), ptr64(20), ptr64(40), ptr64(5), core.UsagePartial},
		{"explicit zeros", `{"response":{"usage":{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`, true, false, ptr64(0), ptr64(0), ptr64(0), ptr64(0), core.UsagePartial},
		{"missing totals", `{"response":{"usage":{"input_tokens":3}}}`, true, false, ptr64(3), nil, nil, nil, core.UsageUnknownCompleteness},
		{"null usage", `{"response":{"usage":null}}`, true, false, nil, nil, nil, nil, core.UsageUnknownCompleteness},
		{"empty usage", `{"response":{"usage":{}}}`, true, false, nil, nil, nil, nil, core.UsageUnknownCompleteness},
		{"absent usage", `{"type":"response.output_text.delta"}`, false, false, nil, nil, nil, nil, ""},
		{"negative", `{"response":{"usage":{"input_tokens":-1,"output_tokens":0}}}`, true, true, nil, nil, nil, nil, core.UsageUnknownCompleteness},
		{"overflow", `{"response":{"usage":{"input_tokens":9223372036854775808,"output_tokens":0}}}`, true, true, nil, nil, nil, nil, core.UsageUnknownCompleteness},
		{"cached exceeds input", `{"response":{"usage":{"input_tokens":1,"output_tokens":0,"input_tokens_details":{"cached_tokens":2}}}}`, true, true, nil, nil, nil, nil, core.UsageUnknownCompleteness},
		{"reasoning exceeds output", `{"response":{"usage":{"input_tokens":0,"output_tokens":1,"output_tokens_details":{"reasoning_tokens":2}}}}`, true, true, nil, nil, nil, nil, core.UsageUnknownCompleteness},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, found, invalid := observeUsage([]byte(tc.json))
			if found != tc.found || invalid != tc.invalid {
				t.Fatalf("found=%v invalid=%v; want %v %v", found, invalid, tc.found, tc.invalid)
			}
			if !found {
				return
			}
			assertUsageCounter(t, "input", got.InputTokens, tc.input)
			assertUsageCounter(t, "output", got.OutputTokens, tc.output)
			assertUsageCounter(t, "cached", got.CachedTokens, tc.cached)
			assertUsageCounter(t, "reasoning", got.ReasoningTokens, tc.reasoning)
			if got.Completeness != tc.completeness {
				t.Fatalf("completeness=%q want %q", got.Completeness, tc.completeness)
			}
		})
	}
}

func TestUsageExtractionPreservesStreamAndTerminalIsOnce(t *testing.T) {
	input := append(loadResponsesFixture(t, "stream-normal-text.sse"), '\n') // Terminate the fixture's final SSE event.
	stream := newTestExecuteStream(io.NopCloser(bytes.NewReader(input)), http.StatusOK)
	output, complete := drainExecuteStream(t, stream)
	if !bytes.Equal(output, input) {
		t.Fatal("observer changed caller-visible SSE bytes")
	}
	if complete.Outcome != core.OutcomeSucceeded || complete.Usage == nil || complete.Usage.Source != core.UsageProvider || complete.Usage.Completeness != core.UsageComplete {
		t.Fatalf("completion=%+v", complete)
	}
	assertUsageCounter(t, "input", complete.Usage.InputTokens, ptr64(12))
	assertUsageCounter(t, "output", complete.Usage.OutputTokens, ptr64(5))
	assertUsageCounter(t, "cached", complete.Usage.CachedTokens, ptr64(3))
	assertUsageCounter(t, "reasoning", complete.Usage.ReasoningTokens, ptr64(2))
	if frame, err := stream.Next(context.Background()); err != io.EOF {
		t.Fatalf("second terminal frame=%+v err=%v", frame, err)
	}
}

func TestUsageExtractionInvalidTerminalCountersBecomeUnknown(t *testing.T) {
	for _, usage := range []string{
		`{"input_tokens":-1,"output_tokens":0}`,
		`{"input_tokens":1,"output_tokens":0,"input_tokens_details":{"cached_tokens":2}}`,
		`{"input_tokens":0,"output_tokens":1,"output_tokens_details":{"reasoning_tokens":2}}`,
	} {
		t.Run(usage, func(t *testing.T) {
			input := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":" + usage + "}}\n\n")
			stream := newTestExecuteStream(io.NopCloser(bytes.NewReader(input)), http.StatusOK)
			output, complete := drainExecuteStream(t, stream)
			if !bytes.Equal(output, input) || complete.Usage == nil || complete.Usage.Source != core.UsageUnknown || complete.Usage.Completeness != core.UsageUnknownCompleteness {
				t.Fatalf("invalid terminal usage not rejected: outputPreserved=%v complete=%+v", bytes.Equal(output, input), complete)
			}
		})
	}
}

func TestUsageExtractionFailedAndIncompleteTerminalArePartial(t *testing.T) {
	for _, tc := range []struct {
		event   string
		outcome core.Outcome
	}{
		{"response.failed", core.OutcomeFailed},
		{"response.incomplete", core.OutcomeIncomplete},
	} {
		t.Run(tc.event, func(t *testing.T) {
			input := []byte("event: " + tc.event + "\ndata: {\"type\":\"" + tc.event + "\",\"response\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":2}}}\n\n")
			stream := newTestExecuteStream(io.NopCloser(bytes.NewReader(input)), http.StatusOK)
			_, complete := drainExecuteStream(t, stream)
			if complete.Outcome != tc.outcome || complete.Usage == nil || complete.Usage.Source != core.UsageProvider || complete.Usage.Completeness != core.UsagePartial {
				t.Fatalf("terminal usage=%+v outcome=%s; want %s with partial usage", complete.Usage, complete.Outcome, tc.outcome)
			}
			assertUsageCounter(t, "input", complete.Usage.InputTokens, ptr64(8))
			assertUsageCounter(t, "output", complete.Usage.OutputTokens, ptr64(2))
		})
	}
}

func TestUsageExtractionRetainsPartialOnInterruption(t *testing.T) {
	input := []byte("event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":2}}}\n\n")
	stream := newTestExecuteStream(io.NopCloser(bytes.NewReader(input)), http.StatusOK)
	output, complete := drainExecuteStream(t, stream)
	if !bytes.Equal(output, input) || complete.Outcome != core.OutcomeIncomplete || complete.Usage.Source != core.UsageProvider || complete.Usage.Completeness != core.UsagePartial {
		t.Fatalf("output preserved=%v completion=%+v", bytes.Equal(output, input), complete)
	}
	assertUsageCounter(t, "input", complete.Usage.InputTokens, ptr64(8))
	assertUsageCounter(t, "output", complete.Usage.OutputTokens, ptr64(2))
}

func TestUsageExtractionDoesNotPromoteIntermediateCounts(t *testing.T) {
	input := []byte("event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":2}}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	stream := newTestExecuteStream(io.NopCloser(bytes.NewReader(input)), http.StatusOK)
	_, complete := drainExecuteStream(t, stream)
	if complete.Outcome != core.OutcomeSucceeded || complete.Usage.Completeness != core.UsagePartial {
		t.Fatalf("intermediate usage promoted to terminal complete: %+v", complete)
	}
}

func ptr64(v int64) *int64 { return &v }

func assertUsageCounter(t *testing.T, name string, got, want *int64) {
	t.Helper()
	if got == nil || want == nil {
		if got != nil || want != nil {
			t.Fatalf("%s=%v want %v", name, got, want)
		}
		return
	}
	if *got != *want {
		t.Fatalf("%s=%d want %d", name, *got, *want)
	}
}
