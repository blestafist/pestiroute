package antigravity

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestM039TotalUsageValidationAndAccounting(t *testing.T) {
	for _, raw := range []string{
		`{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"totalTokenCount":99}}}`,
		`{"response":{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"answer"}]}}],"usageMetadata":{"totalTokenCount":99}}}`,
	} {
		s := newStream(io.NopCloser(strings.NewReader("")), "r", "m")
		if err := s.consume([]byte(raw)); err != nil {
			t.Fatalf("valid total usage rejected: %v", err)
		}
	}

	for _, tc := range []struct{ name, usage string }{
		{"negative", `"totalTokenCount":-1`},
		{"fractional", `"totalTokenCount":1.5`},
		{"null", `"totalTokenCount":null`},
		{"string", `"totalTokenCount":"2"`},
		{"unknown", `"totalTokenCount":2,"trafficType":"standard"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStream(io.NopCloser(strings.NewReader("")), "r", "m")
			raw := `{"response":{"usageMetadata":{` + tc.usage + `}}}`
			if err := s.consume([]byte(raw)); !errors.Is(err, errInvalidUsage) {
				t.Fatalf("consume error=%v, want invalid usage", err)
			}
		})
	}

	for _, raw := range []string{
		`{"response":{"usageMetadata":{"totalTokenCount":5}}}`,
		`{"response":{"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":99}}}`,
	} {
		if !usageOnlyFrame([]byte(raw)) {
			t.Fatalf("valid usage-only frame rejected: %s", raw)
		}
	}
	if usageOnlyFrame([]byte(`{"response":{"usageMetadata":{"totalTokenCount":5,"other":1}}}`)) {
		t.Fatal("usage-only frame accepted unlisted key")
	}

	// Candidate usage followed by a late usage trailer must settle only prompt/output;
	// totalTokenCount can differ because it is metadata, not another accounting bucket.
	wire := strings.Join([]string{
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"totalTokenCount":99}}}`,
		`data: {"response":{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"!"}]}}]}}`,
		`data: {"response":{"usageMetadata":{"totalTokenCount":100}}}`,
	}, "\n\n") + "\n\n"
	s := newStream(io.NopCloser(strings.NewReader(wire)), "r", "m")
	completions := 0
	for {
		frame, err := s.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameComplete {
			completions++
			usage := frame.Complete.Usage
			if usage == nil || usage.InputTokens == nil || *usage.InputTokens != 10 || usage.OutputTokens == nil || *usage.OutputTokens != 4 {
				t.Fatalf("unexpected settled usage: %#v", usage)
			}
		}
	}
	if completions != 1 {
		t.Fatalf("completion/accounting frames=%d, want exactly one", completions)
	}
}

func TestM039PostcommitCauseIsFixedAndSecretSafe(t *testing.T) {
	for _, tc := range []struct {
		err   error
		cause string
	}{
		{io.EOF, "eof"},
		{errSSEBounds, "bounds_exceeded"},
		{errMalformedCandidateFrame, "invalid_json"},
		{errUnsupportedFinish, "unsupported_finish"},
		{errUnsupportedReasoning, "unsupported_reasoning"},
		{errInvalidUsage, "invalid_usage"},
		{errInvalidFunction, "invalid_function"},
		{errEmptyContent, "empty_content"},
		{invalidContentError{subkind: invalidContentInvalidUTF8Text}, "invalid_content"},
		{errInvalidOrder, "invalid_order"},
		{errors.New("PRIVATE-RAW-ERROR-CANARY"), "framing"},
	} {
		var logs strings.Builder
		s := newStream(io.NopCloser(strings.NewReader("")), "r", "m", slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		if got := postcommitCause(tc.err); got != tc.cause {
			t.Fatalf("cause=%q, want %q", got, tc.cause)
		}
		s.logStreamError(tc.err, false)
		if !strings.Contains(logs.String(), "phase=postcommit") || !strings.Contains(logs.String(), "category=postcommit_error") || !strings.Contains(logs.String(), "cause="+tc.cause) || strings.Contains(logs.String(), "PRIVATE-") {
			t.Fatalf("unsafe/missing diagnostic: %q", logs.String())
		}
	}
}
