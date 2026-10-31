package antigravity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/core"
)

// Exercise the exact runner request/parser against real translation and encoding,
// using only repository-owned synthetic upstream bytes and loopback HTTP.
func TestLiveRunnerOfflineConnectorAdapterHTTP(t *testing.T) {
	scripts, err := filepath.Abs("../../../scripts")
	if err != nil {
		t.Fatal(err)
	}
	runner, err := os.ReadFile(filepath.Join(scripts, "antigravity-live-smoke.sh"))
	if err != nil {
		t.Fatal(err)
	}
	_, script, ok := strings.Cut(string(runner), "cat >\"$WORK/request.py\" <<'PY'\n")
	if !ok {
		t.Fatal("runner request program missing")
	}
	script, _, ok = strings.Cut(script, "\nPY\n")
	if !ok {
		t.Fatal("runner request program terminator missing")
	}
	text, err := os.ReadFile("testdata/stream-text.sse")
	if err != nil {
		t.Fatal(err)
	}
	terminal := "data: {\"response\":{\"done\":true}}\n\n"
	valid := string(text) + "\n\n" + terminal
	for _, tc := range []struct {
		name, wire string
		success    bool
	}{
		{"LF", valid, true},
		{"CRLF", strings.ReplaceAll(valid, "\n", "\r\n"), true},
		{"empty_upstream", "", false},
		{"malformed_first_JSON", "data: not-json\n\n", false},
		{"first_frame_missing_blank_line", strings.TrimSuffix(terminal, "\n"), false},
		{"completion_without_ordinary_text", "event: message\n" + terminal, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connector := ready(t)
			encoded := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				out, ge := connector.Execute(context.Background(), request(`{"model":"gemini-2.5-flash","input":"synthetic","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, services(&fakeDoer{response: response(200, tc.wire)}))
				encoded <- responses.Encode(w, r, out, ge)
			}))
			defer server.Close()
			port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
			cmd := exec.Command("python3", "-c", script, port, "gemini-2.5-flash")
			cmd.Env = append(os.Environ(), "PYTHONPATH="+scripts)
			cmd.Stdin = strings.NewReader("SYNTHETIC_FIXTURE_KEY\n")
			output, runErr := cmd.CombinedOutput()
			encodeErr := <-encoded
			if tc.success {
				if runErr != nil || encodeErr != nil || !strings.Contains(string(output), "nonempty=true terminal=true completed=true") {
					t.Fatalf("valid fixture not recognized: runner=%v encoder=%v summary=%s", runErr, encodeErr, output)
				}
			} else if tc.name == "completion_without_ordinary_text" {
				if runErr == nil || encodeErr == nil || !strings.Contains(string(output), "nonempty=false terminal=false completed=false") {
					t.Fatalf("completion-only fixture incorrectly accepted: runner=%v encoder=%v summary=%s", runErr, encodeErr, output)
				}
			} else if runErr == nil || encodeErr != nil || !strings.Contains(string(output), "status=503 sse_events=0 nonempty=false terminal=false completed=false") || !strings.Contains(string(output), "reason=missing_or_unlisted_code") {
				t.Fatalf("early failure did not return structured 503 with zero SSE: runner=%v encoder=%v summary=%s", runErr, encodeErr, output)
			}
			if strings.Contains(string(output), "bytes=") {
				t.Fatal("runner exposed a response length")
			}
			t.Logf("runner_success=%t encoder_error=%t summary=%s", runErr == nil, encodeErr != nil, output)
		})
	}
}
