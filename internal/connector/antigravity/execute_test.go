package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	responsesAdapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/core"
)

type fakeDoer struct {
	response *http.Response
	err      error
	calls    int
	request  *http.Request
}
type ambientDoer struct{ calls int }

func (f *ambientDoer) Do(*http.Request) (*http.Response, error) {
	f.calls++
	return nil, errors.New("must not dispatch")
}

func (f *fakeDoer) antigravityOfflineFakeOnly() {}
func (f *fakeDoer) Do(r *http.Request) (*http.Response, error) {
	f.calls++
	f.request = r
	return f.response, f.err
}

type credentials struct {
	data []byte
	err  error
}
type trackingBody struct {
	io.Reader
	read   int
	closed bool
}

func (b *trackingBody) Read(p []byte) (int, error) {
	n, e := b.Reader.Read(p)
	b.read += n
	return n, e
}
func (b *trackingBody) Close() error { b.closed = true; return nil }

type cancelBody struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
	sent    bool
}

func (b *cancelBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, `data: {"response":{"candidates":[{"content":{"parts":[{"text":"x"}]}}]}}`+"\n\n"), nil
	}
	b.once.Do(func() { close(b.started) })
	<-b.closed
	return 0, errors.New("closed")
}
func (b *cancelBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func (c credentials) Get(context.Context, string) ([]byte, error) { return c.data, c.err }
func ready(t *testing.T) *Connector {
	t.Helper()
	c := NewConnector()
	if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"profile":"antigravity-v1internal","project":"synthetic-project"}`)}); err != nil {
		t.Fatal(err)
	}
	return c
}
func request(body string) core.ExecutionRequest {
	return core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol, ContentType: "application/json", Body: []byte(body)}}
}
func services(fake *fakeDoer) core.InvocationServices {
	return core.InvocationServices{Transport: fake, Credentials: credentials{data: []byte(`{"access_token":"synthetic-token"}`)}}
}
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestExecuteStreamsIncrementallyAndBuildsCandidateRequest(t *testing.T) {
	data, err := os.ReadFile("testdata/stream-text.sse")
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's final text chunk is intentionally not terminal; append the synthetic terminal fixture.
	usage, err := os.ReadFile("testdata/stream-usage.sse")
	if err != nil {
		t.Fatal(err)
	}
	frames := string(data) + "\n" + strings.TrimLeft(strings.SplitN(string(usage), "\n\n", 2)[1], "\n") + "\n"
	f := &fakeDoer{response: response(200, frames)}
	c := ready(t)
	out, ge := c.Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"Synthetic prompt","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, services(f))
	if ge != nil {
		t.Fatal(ge)
	}
	if f.calls != 1 || f.request.URL.String() != "https://antigravity.synthetic.invalid/v1internal/projects/synthetic-project:generateContent?alt=sse" || f.request.Header.Get("Authorization") != "Bearer synthetic-token" {
		t.Fatalf("bad dispatch: calls=%d request=%v", f.calls, f.request)
	}
	reqBody, err := io.ReadAll(f.request.Body)
	if err != nil {
		t.Fatal(err)
	}
	var candidate map[string]any
	if err := json.Unmarshal(reqBody, &candidate); err != nil || candidate["project"] != "synthetic-project" || candidate["requestType"] != "agent" {
		t.Fatalf("bad candidate envelope %s (%v)", reqBody, err)
	}
	s := out.Stream
	defer s.Close()
	frame, err := s.Next(context.Background())
	if err != nil || frame.Type != core.FrameHead {
		t.Fatalf("head: %#v %v", frame, err)
	}
	var body strings.Builder
	for {
		frame, err = s.Next(context.Background())
		if err != nil || frame.Type != core.FrameBody {
			t.Fatalf("expected streamed body: %#v %v", frame, err)
		}
		body.Write(frame.Body.Data)
		if strings.Contains(string(frame.Body.Data), "Synthetic ") {
			break
		}
	}
	for {
		frame, err = s.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameComplete {
			if frame.Complete.Usage == nil || *frame.Complete.Usage.InputTokens != 7 {
				t.Fatalf("missing terminal usage: %+v", frame.Complete.Usage)
			}
			break
		}
		body.Write(frame.Body.Data)
	}
	if !strings.Contains(body.String(), "stream text.") || !strings.Contains(body.String(), "response.completed") {
		t.Fatalf("translated output incomplete: %s", body.String())
	}
}

func TestResponsesEncoderAcceptsConnectorStream(t *testing.T) {
	wire := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"usable\"}]}}]}}\n\n" + "data: {\"response\":{\"done\":true}}\n\n"
	f := &fakeDoer{response: response(http.StatusOK, wire)}
	out, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, services(f))
	if ge != nil {
		t.Fatal(ge)
	}
	rw := httptest.NewRecorder()
	if err := responsesAdapter.Encode(rw, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), out, nil); err != nil {
		t.Fatalf("Encode rejected connector stream: %v", err)
	}
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), "usable") {
		t.Fatalf("encoded status=%d body=%q", rw.Code, rw.Body.String())
	}
}

func TestTerminalUsagePreservesMissingAndPartialCounts(t *testing.T) {
	for _, tc := range []struct {
		name, usage   string
		wantNil       bool
		input, output *int64
		completeness  core.UsageCompleteness
	}{
		{name: "missing", usage: "", wantNil: true},
		{name: "empty", usage: ",\"usageMetadata\":{}", wantNil: true},
		{name: "input only", usage: ",\"usageMetadata\":{\"promptTokenCount\":3}", input: new(int64(3)), completeness: core.UsagePartial},
		{name: "output zero", usage: ",\"usageMetadata\":{\"candidatesTokenCount\":0}", output: new(int64(0)), completeness: core.UsagePartial},
		{name: "both", usage: ",\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":0}", input: new(int64(3)), output: new(int64(0)), completeness: core.UsageComplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			terminal := "data: {\"response\":{\"done\":true" + tc.usage + "}}\n\n"
			wire := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"x\"}]}}]}}\n\n" + terminal
			f := &fakeDoer{response: response(http.StatusOK, wire)}
			out, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, services(f))
			if ge != nil {
				t.Fatal(ge)
			}
			defer out.Stream.Close()
			var usage *core.UsageReport
			for {
				frame, err := out.Stream.Next(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if frame.Type == core.FrameComplete {
					usage = frame.Complete.Usage
					break
				}
			}
			if tc.wantNil {
				if usage != nil {
					t.Fatalf("usage=%+v, want nil", usage)
				}
				return
			}
			if usage == nil || !sameCount(usage.InputTokens, tc.input) || !sameCount(usage.OutputTokens, tc.output) || usage.Completeness != tc.completeness {
				t.Fatalf("usage=%+v", usage)
			}
		})
	}
}

func sameCount(got, want *int64) bool {
	if got == nil || want == nil {
		return got == want
	}
	return *got == *want
}

func TestAdmissionRejectsBeforeDispatch(t *testing.T) {
	f := &fakeDoer{}
	c := ready(t)
	svc := services(f)
	for _, body := range []string{`{"model":"gemini-2.5-pro","input":"x","stream":true,"extra":1}`, `{"model":"gemini-2.5-pro","model":"gemini-2.5-pro","input":"x","stream":true}`, `{"model":"gemini-2.5-pro","input":"x","stream":false}`, `{"model":"gemini-2.5-pro","input":["x"],"stream":true}`, `{"model":"bad","input":"x","stream":true}`, `{"model":"gemini-2.5-pro","input":"x","stream":true,"tools":[]}`} {
		_, ge := c.Execute(context.Background(), request(body), core.AttemptScope{Mode: core.ModeTranslation}, svc)
		if ge == nil {
			t.Errorf("accepted %s", body)
		}
	}
	if f.calls != 0 {
		t.Fatalf("invalid requests dispatched %d times", f.calls)
	}
}

func TestCredentialAndHTTPFailures(t *testing.T) {
	c := ready(t)
	req := request(`{"model":"gemini-2.5-flash","input":"x","stream":true}`)
	for _, raw := range [][]byte{nil, []byte(`{}`), []byte(`{"access_token":"x","refresh_token":"y"}`), []byte(`{"access_token":"x"} {}`)} {
		f := &fakeDoer{response: response(200, "data: {}\n\n")}
		svc := services(f)
		svc.Credentials = credentials{data: raw}
		_, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: core.ModeTranslation}, svc)
		if ge == nil || ge.Category != core.CategoryUnauthenticated || f.calls != 0 {
			t.Errorf("credentials %s: error=%v calls=%d", raw, ge, f.calls)
		}
	}
	for status, category := range map[int]core.ErrorCategory{401: core.CategoryUnauthenticated, 403: core.CategoryUnauthenticated, 429: core.CategoryRateLimited, 500: core.CategoryUnavailable, 302: core.CategoryUnavailable} {
		limited := &trackingBody{Reader: strings.NewReader(strings.Repeat("x", 20000))}
		f := &fakeDoer{response: &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: limited}}
		_, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: core.ModeTranslation}, services(f))
		if ge == nil || ge.Category != category || f.calls != 1 {
			t.Errorf("status %d: error=%v calls=%d", status, ge, f.calls)
		}
		if limited.read > 8192 || !limited.closed {
			t.Errorf("status %d body drain=%d closed=%t", status, limited.read, limited.closed)
		}
	}
}

func TestPrematureMalformedCancellationAndFakeBoundary(t *testing.T) {
	c := ready(t)
	req := request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`)
	for _, wire := range []string{"data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"x\"}]}}]}}\n\n", "data: {not-json}\n\n"} {
		f := &fakeDoer{response: response(200, wire)}
		out, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: core.ModeTranslation}, services(f))
		if ge != nil {
			t.Fatal(ge)
		}
		s := out.Stream
		_, streamErr := s.Next(context.Background())
		for streamErr == nil {
			_, streamErr = s.Next(context.Background())
		}
		if errors.Is(streamErr, io.EOF) {
			t.Fatalf("bad stream succeeded: %q", wire)
		}
		_ = s.Close()
	}
	f := &fakeDoer{err: errors.New("ambiguous")}
	_, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: core.ModeTranslation}, services(f))
	if ge == nil || ge.RetryDisposition != core.RetryUnknown || f.calls != 1 {
		t.Fatalf("ambiguous Do error: %v, calls %d", ge, f.calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f = &fakeDoer{}
	_, ge = c.Execute(ctx, req, core.AttemptScope{Mode: core.ModeTranslation}, services(f))
	if ge == nil || f.calls != 0 {
		t.Fatalf("cancelled dispatch: %v, calls %d", ge, f.calls)
	}
	ambient := &ambientDoer{}
	svc := services(nil)
	svc.Transport = ambient
	_, ge = c.Execute(context.Background(), req, core.AttemptScope{Mode: core.ModeTranslation}, svc)
	if ge == nil || ge.RetryDisposition != core.RetryUnknown || ambient.calls != 0 {
		t.Fatalf("ambient transport was not rejected: %v, calls %d", ge, ambient.calls)
	}
}

func TestCancellationClosesActiveStream(t *testing.T) {
	b := &cancelBody{started: make(chan struct{}), closed: make(chan struct{})}
	f := &fakeDoer{response: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: b}}
	out, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, services(f))
	if ge != nil {
		t.Fatal(ge)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if frame, err := out.Stream.Next(ctx); err != nil || frame.Type != core.FrameHead {
		t.Fatalf("head: %v %v", frame, err)
	}
	_, _ = out.Stream.Next(ctx) // response.created
	_, _ = out.Stream.Next(ctx) // first text delta
	done := make(chan error, 1)
	go func() { _, err := out.Stream.Next(ctx); done <- err }()
	select {
	case <-b.started:
	case <-time.After(time.Second):
		t.Fatal("stream read did not block")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled stream read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not release stream read")
	}
	select {
	case <-b.closed:
	default:
		t.Fatal("cancellation did not close response body")
	}
}

func TestConcurrentCloseAndCancellationDuringNext(t *testing.T) {
	for range 100 {
		b := &cancelBody{started: make(chan struct{}), closed: make(chan struct{})}
		s := newStream(b)
		ctx, cancel := context.WithCancel(context.Background())
		start := make(chan struct{})
		done := make(chan struct{})
		go func() { <-start; _, _ = s.Next(ctx); close(done) }()
		go func() { <-start; cancel(); _ = s.Close() }()
		close(start)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("concurrent cancellation did not release Next")
		}
		cancel()
		_ = s.Close()
		select {
		case <-b.closed:
		default:
			t.Fatal("concurrent cancellation did not close body")
		}
	}
}
