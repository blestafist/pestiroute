package anthropic

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	responses "github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/core"
)

type countedBody struct {
	reader *strings.Reader
	read   int
	closed bool
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}
func (b *countedBody) Close() error { b.closed = true; return nil }

func TestClassifyHTTPRejection(t *testing.T) {
	tests := []struct {
		status   int
		category core.ErrorCategory
		code     string
		retry    bool
	}{
		{400, core.CategoryInvalidRequest, "upstream_rejection", false},
		{401, core.CategoryUnauthenticated, "upstream_rejection", false},
		{403, core.CategoryPermissionDenied, "upstream_rejection", false},
		{404, core.CategoryInvalidRequest, "upstream_rejection", false},
		{409, core.CategoryInvalidRequest, "upstream_rejection", false},
		{413, core.CategoryInvalidRequest, "upstream_rejection", false},
		{429, core.CategoryRateLimited, "upstream_rejection", true},
		{500, core.CategoryUnavailable, "upstream_rejection", true},
		{503, core.CategoryUnavailable, "upstream_rejection", true},
		{302, core.CategoryUnavailable, "upstream_redirect", false},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			body := &countedBody{reader: strings.NewReader(strings.Repeat("provider-secret", maxHTTPErrorBody))}
			calls := 0
			transport := NewTransport()
			defer transport.Close()
			transport.client.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: tt.status, Header: http.Header{"Retry-After": {"17"}}, Body: body, Request: req}, nil
			})
			resp, err := transport.Do(context.Background(), core.ExecutionRequest{}, core.InvocationServices{Credentials: credentialStub("selected-key")})
			if err != nil || resp == nil {
				t.Fatalf("Do response=%v error=%v", resp, err)
			}
			got := classifyHTTPRejection(resp, time.Unix(0, 0))
			if calls != 1 || body.read > maxHTTPErrorBody || !body.closed {
				t.Fatalf("calls=%d bytes=%d closed=%t", calls, body.read, body.closed)
			}
			if got.Category != tt.category || got.Code != tt.code || got.Retryable != tt.retry || got.RetryDisposition != core.RetryUnknown {
				t.Fatalf("classification: %+v", got)
			}
			if strings.Contains(got.Message, "provider-secret") || got.OriginalError != "" {
				t.Fatalf("provider detail leaked: %+v", got)
			}
			if tt.retry {
				if got.RetryAfter == nil || *got.RetryAfter != 17*time.Second {
					t.Fatalf("RetryAfter=%v", got.RetryAfter)
				}
			} else if got.RetryAfter != nil {
				t.Fatalf("unexpected RetryAfter=%v", got.RetryAfter)
			}
		})
	}
}

func TestClassifyHTTPUnexpectedStatusFailsClosed(t *testing.T) {
	for _, status := range []int{0, 100, 204, 299, 600} {
		got := classifyHTTPRejection(&http.Response{StatusCode: status}, time.Unix(0, 0))
		if got.Code != "upstream_invalid_status" || got.Category != core.CategoryUnavailable || got.Retryable || got.RetryDisposition != core.RetryUnknown {
			t.Errorf("status %d classified as %+v", status, got)
		}
	}
}

func TestClassifyHTTPReadErrorClosesBodyAndDoesNotRetrySafely(t *testing.T) {
	readErr := errors.New("provider-secret read failure")
	body := &failingBody{err: readErr}
	got := classifyHTTPRejection(&http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"17"}}, Body: body}, time.Unix(0, 0))
	if !body.closed || got.Category != core.CategoryRateLimited || !got.Retryable || got.RetryDisposition != core.RetryUnknown || got.OriginalError != "" || strings.Contains(got.Message, readErr.Error()) {
		t.Fatalf("read error escaped or body not closed: closed=%t error=%+v", body.closed, got)
	}
}

type failingBody struct {
	err    error
	closed bool
}

func (b *failingBody) Read([]byte) (int, error) { return 0, b.err }
func (b *failingBody) Close() error             { b.closed = true; return nil }

type blockedBody struct {
	ctx    context.Context
	closed chan struct{}
	once   sync.Once
}

func (b *blockedBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b *blockedBody) Close() error             { b.once.Do(func() { close(b.closed) }); return nil }

func TestClassifyHTTPBlockedBodyCancellationClosesAfterOneCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var body *blockedBody
	calls := 0
	transport := NewTransport()
	defer transport.Close()
	transport.client.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body = &blockedBody{ctx: req.Context(), closed: make(chan struct{})}
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: body, Request: req}, nil
	})
	resp, err := transport.Do(ctx, core.ExecutionRequest{}, core.InvocationServices{Credentials: credentialStub("selected-key")})
	if err != nil || resp == nil || calls != 1 {
		t.Fatalf("response=%v calls=%d error=%v", resp, calls, err)
	}
	done := make(chan *core.GatewayError, 1)
	go func() { done <- classifyHTTPRejection(resp, time.Unix(0, 0)) }()
	cancel()
	select {
	case got := <-done:
		if got.RetryDisposition != core.RetryUnknown {
			t.Fatalf("cancellation changed disposition: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("bounded rejection read did not observe request cancellation")
	}
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("response body was not closed after cancellation")
	}
	if calls != 1 {
		t.Fatalf("transport was called %d times, want exactly once", calls)
	}
}

func TestClassifyHTTPMalformedOversizedAndRetryAfterBounds(t *testing.T) {
	body := &countedBody{reader: strings.NewReader("not-json" + strings.Repeat("x", maxHTTPErrorBody*2))}
	got := classifyHTTPRejection(&http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"999999999"}}, Body: body}, time.Unix(0, 0))
	if body.read != maxHTTPErrorBody || !body.closed || got.RetryAfter != nil || got.RetryDisposition != core.RetryUnknown {
		t.Fatalf("bounded malformed rejection: read=%d closed=%t error=%+v", body.read, body.closed, got)
	}

	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		value string
		want  time.Duration
	}{
		{"0", 0},
		{"17", 17 * time.Second},
		{now.Add(time.Hour).Format(http.TimeFormat), time.Hour},
	}
	for _, tt := range tests {
		delay := parseHTTPRetryAfter(tt.value, now)
		if delay == nil || *delay != tt.want {
			t.Errorf("Retry-After %q = %v, want %s", tt.value, delay, tt.want)
		}
	}
	for _, value := range []string{"", "+17", "-1", "1.5", "999999999", now.Add(-time.Second).Format(http.TimeFormat), now.Add(24*time.Hour + time.Second).Format(http.TimeFormat)} {
		if delay := parseHTTPRetryAfter(value, now); delay != nil {
			t.Errorf("accepted invalid/out-of-range Retry-After %q: %v", value, *delay)
		}
	}
	boundary := parseHTTPRetryAfter("86400", now)
	if boundary == nil || *boundary != 24*time.Hour {
		t.Fatalf("24-hour boundary Retry-After=%v", boundary)
	}
}

func TestClassifyHTTPErrorFormatsResponsesClientError(t *testing.T) {
	delay := 17 * time.Second
	err := classifyHTTPRejection(&http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"17"}}}, time.Unix(0, 0))
	err.RetryAfter = &delay
	w := httptest.NewRecorder()
	if encodeErr := responses.Encode(w, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), core.ExecutionResponse{}, err); encodeErr != nil {
		t.Fatal(encodeErr)
	}
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "17" || !strings.Contains(w.Header().Get("Content-Type"), "application/json") || !strings.Contains(w.Body.String(), `"type":"rate_limited"`) || strings.Contains(w.Body.String(), "provider") {
		t.Fatalf("Responses error status=%d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
	}
}
