package codex

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestTerminalOutcomeClassificationPreservesBytes(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		outcome    core.Outcome
		wantError  bool
	}{
		{"completed", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", core.OutcomeSucceeded, false},
		{"failed", "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{}}\n\n", core.OutcomeFailed, true},
		{"error", "event: error\ndata: {\"type\":\"error\"}\n\n", core.OutcomeFailed, true},
		{"incomplete", "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{}}\n\n", core.OutcomeIncomplete, true},
		{"eof without terminal", "data: {\"type\":\"response.output_text.delta\"}\n\n", core.OutcomeIncomplete, true},
		{"truncated", "event: response.completed\ndata: {\"type\":\"response.completed\"", core.OutcomeIncomplete, true},
		{"trailing data", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\nevent: response.output_text.delta\ndata: trailing\n\n", core.OutcomeIncomplete, true},
		{"malformed terminal", "event: response.completed\ndata: {not-json}\n\n", core.OutcomeIncomplete, true},
		{"missing terminal type", "event: response.failed\ndata: {\"response\":{}}\n\n", core.OutcomeIncomplete, true},
		{"conflicting terminal", "event: response.completed\ndata: {\"type\":\"response.failed\"}\n\n", core.OutcomeIncomplete, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(tc.data)
			stream := newTestExecuteStream(io.NopCloser(bytes.NewReader(input)), http.StatusOK)
			stream.idleTimeout = time.Second
			output, complete := drainExecuteStream(t, stream)
			if !bytes.Equal(output, input) {
				t.Fatal("SSE bytes were changed")
			}
			if complete.Outcome != tc.outcome || (complete.Error != nil) != tc.wantError {
				t.Fatalf("completion=%+v", complete)
			}
			if complete.Error != nil && (complete.Error.Message == "" || complete.Error.OriginalError != "") {
				t.Fatalf("unsanitized terminal error: %+v", complete.Error)
			}
			if _, err := stream.Next(context.Background()); err != io.EOF {
				t.Fatalf("after completion err=%v", err)
			}
		})
	}
}

func TestHTTPRejectionClassification(t *testing.T) {
	for _, tc := range []struct {
		status   int
		category core.ErrorCategory
	}{
		{400, core.CategoryInvalidRequest}, {422, core.CategoryInvalidRequest},
		{401, core.CategoryUnauthenticated}, {403, core.CategoryPermissionDenied},
		{429, core.CategoryRateLimited}, {500, core.CategoryUnavailable}, {503, core.CategoryUnavailable},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			c, services, sends := executeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("provider body"))
			})
			response, ge := c.Execute(t.Context(), executeRequest(loadResponsesFixture(t, "request-positive.json")), core.AttemptScope{Mode: core.ModeNative, AccountID: "account-a"}, services)
			if ge != nil {
				t.Fatal(ge)
			}
			defer response.Stream.Close()
			head, err := response.Stream.Next(t.Context())
			if err != nil || head.Type != core.FrameHead || head.Head.Error == nil || head.Head.Error.Category != tc.category || head.Head.Error.Retryable || head.Head.Error.RetryDisposition != "" || head.Head.Error.OriginalError != "" {
				t.Fatalf("rejection Head=%+v err=%v", head, err)
			}
			body, err := response.Stream.Next(t.Context())
			if err != nil || body.Type != core.FrameBody || string(body.Body.Data) != "provider body" {
				t.Fatalf("rejection Body=%+v err=%v", body, err)
			}
			complete, err := response.Stream.Next(t.Context())
			if err != nil || complete.Type != core.FrameComplete || complete.Complete.Outcome != core.OutcomeFailed {
				t.Fatalf("rejection completion=%+v err=%v", complete, err)
			}
			if _, err := response.Stream.Next(t.Context()); err != io.EOF || sends.Load() != 1 {
				t.Fatalf("post-rejection err=%v sends=%d", err, sends.Load())
			}
		})
	}
}
