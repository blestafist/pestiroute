package antigravity

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestExecuteDebugDiagnosticsAreFixedAndSecretSafe(t *testing.T) {
	const canary = "PRIVATE-CANARY-DO-NOT-LOG"
	for _, tc := range []struct {
		status   int
		body     string
		branch   string
		upstream string
	}{{401, `{"error":{"status":"UNAUTHENTICATED","message":"` + canary + `"}}`, "http_error", "UNAUTHENTICATED"},
		{403, `{"error":{"status":"PERMISSION_DENIED"}}`, "http_error", "PERMISSION_DENIED"},
		{404, `{"error":{"status":"NOT_FOUND"}}`, "http_error", "other"},
		{429, `{"error":{"status":"RESOURCE_EXHAUSTED"}}`, "http_error", "RESOURCE_EXHAUSTED"},
		{500, strings.Repeat(canary, 900), "http_error", "none"},
		{503, `{"error":{"status":"INVALID_ARGUMENT"}}`, "http_error", "INVALID_ARGUMENT"}} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			var log strings.Builder
			logger := slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))
			f := &fakeDoer{response: &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}}
			c := ready(t)
			_, _ = c.Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"synthetic","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{Transport: packageTestDoer(f), Credentials: credentials{data: []byte(`{"access_token":"synthetic-token","project_id":"synthetic-project"}`)}, Logger: logger})
			got := log.String()
			for _, want := range []string{`status=` + strconv.Itoa(tc.status), `branch=` + tc.branch, `upstream_status=` + tc.upstream, `transport_error_class=none`} {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in diagnostic %q", want, got)
				}
			}
			if strings.Contains(got, canary) || strings.Contains(got, "synthetic-token") || strings.Contains(got, "synthetic-project") || strings.Contains(got, "https://") {
				t.Fatalf("secret in diagnostic: %q", got)
			}
		})
	}
	var log strings.Builder
	logger := slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := &fakeDoer{err: errors.New(canary)}
	_, _ = ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"synthetic","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{Transport: packageTestDoer(f), Credentials: credentials{data: []byte(`{"access_token":"synthetic-token","project_id":"synthetic-project"}`)}, Logger: logger})
	if got := log.String(); !strings.Contains(got, `branch=transport_error`) || !strings.Contains(got, `transport_error_class=other`) || strings.Contains(got, canary) {
		t.Fatalf("unsafe transport diagnostic: %q", got)
	}
	log.Reset()

	quiet := &fakeDoer{response: response(200, "data: {}\n\n")}
	_, _ = ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"synthetic","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{Transport: packageTestDoer(quiet), Credentials: credentials{data: []byte(`{"access_token":"synthetic-token","project_id":"synthetic-project"}`)}})
	if log.Len() != 0 {
		t.Fatalf("success without diagnostics should be quiet: %q", log.String())
	}
}

func TestTransportErrorClassesAreBounded(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "timeout"}, {io.EOF, "eof"}, {errors.New("private transport detail"), "other"},
	} {
		if got := transportErrorClass(tc.err); got != tc.want {
			t.Errorf("transportErrorClass(%T)=%q, want %q", tc.err, got, tc.want)
		}
	}
}
