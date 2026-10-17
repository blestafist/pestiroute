package chatcompletions

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

type credentials struct{}

func (credentials) Get(context.Context, string) ([]byte, error) { return []byte("scoped-secret"), nil }

func TestDescriptorBinding(t *testing.T) {
	d := NewConnector().Descriptor()
	if d.ID != componentID || len(d.Protocols) != 1 || d.Protocols[0] != protocol || d.ConnectorType != "api" || len(d.AuthMethods) != 1 || d.AuthMethods[0] != authBearer {
		t.Fatalf("unexpected connector descriptor: %#v", d)
	}
}

func TestExecuteForwardsOpaqueJSONAndMapsUsage(t *testing.T) {
	reqBody := []byte(" { \"model\":\"m\",\"messages\":[{\"role\":\"user\",\"content\":\"x\"}],\"unknown\":{\"b\":2,\"a\":1} }\n")
	responseBody := `{"choices":[{"message":{"content":"ok"}}],"opaque":{"number":-1.25e+2,"key\u0061":"unicode"},"usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		got, _ := io.ReadAll(r.Body)
		if string(got) != string(reqBody) {
			t.Fatalf("request changed: %q", got)
		}
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer scoped-secret" {
			t.Fatalf("bad upstream request: %s %v", r.URL, r.Header)
		}
		for _, name := range []string{"X-API-Key", "OpenAI-Organization", "OpenAI-Project"} {
			if r.Header.Get(name) != "" {
				t.Fatalf("sensitive request header forwarded: %s", name)
			}
		}
		if r.Header.Get("Idempotency-Key") != "preserve" {
			t.Fatal("safe idempotency header was dropped")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(responseBody))}, nil
	})
	c := NewConnector()
	cfg := `{"endpoint":"https://example.test/v1/chat/completions","model":"m","account_id":"acct","max_request_header_bytes":4096}`
	if e := c.Init(context.Background(), core.ComponentConfig{Data: []byte(cfg)}); e != nil {
		t.Fatal(e)
	}
	result, ge := c.Execute(context.Background(), core.ExecutionRequest{Model: "m", Metadata: core.RequestMetadata{IngressHeaderBytes: 100, Headers: map[string][]string{"X-API-Key": {"hidden"}, "OpenAI-Organization": {"org"}, "OpenAI-Project": {"project"}, "Idempotency-Key": {"preserve"}}}, Payload: core.RawPayload{Protocol: protocol, ContentType: "application/json", Body: reqBody}}, core.AttemptScope{Mode: core.ModeNative, AccountID: "acct", MaxBodyBytes: 4096}, core.InvocationServices{Credentials: credentials{}, Transport: doer})
	if ge != nil {
		t.Fatal(ge)
	}
	defer result.Stream.Close()
	head, e := result.Stream.Next(context.Background())
	if e != nil || head.Type != core.FrameHead {
		t.Fatalf("head: %#v %v", head, e)
	}
	body, e := result.Stream.Next(context.Background())
	if e != nil || string(body.Body.Data) != responseBody {
		t.Fatalf("body: %#v %v", body, e)
	}
	done, e := result.Stream.Next(context.Background())
	if e != nil || done.Complete.Outcome != core.OutcomeSucceeded {
		t.Fatalf("complete: %#v %v", done, e)
	}
	u := done.Complete.Usage
	if u.InputTokens == nil || *u.InputTokens != 12 || u.OutputTokens == nil || *u.OutputTokens != 8 || u.CachedTokens == nil || *u.CachedTokens != 3 || u.ReasoningTokens == nil || *u.ReasoningTokens != 2 {
		t.Fatalf("usage: %#v", u)
	}
}

func TestSSEPreservesEventsAndTerminalUsage(t *testing.T) {
	first := `data: {"id":"x","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call","function":{"name":"f","arguments":"{\""}}]}}]}` + "\n\n"
	rest := `data: {"id":"x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":2},"completion_tokens_details":{"reasoning_tokens":1}}}` + "\n\n" + "data: [DONE]\n\n"
	stream := first + rest
	reader := &gatedReader{first: []byte(first), rest: []byte(rest), release: make(chan struct{})}
	resp, ge := execute(context.Background(), config{Endpoint: "https://example.test/v1/chat/completions"}, core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol, Body: []byte(`{}`)}}, "token", doerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}, nil
	}))
	if ge != nil {
		t.Fatal(ge)
	}
	defer resp.Stream.Close()
	if _, e := resp.Stream.Next(context.Background()); e != nil {
		t.Fatal(e)
	}
	frame, e := resp.Stream.Next(context.Background())
	if e != nil || string(frame.Body.Data) != first {
		t.Fatalf("first SSE event was not delivered incrementally: %#v %v", frame, e)
	}
	close(reader.release)
	var got strings.Builder
	got.Write(frame.Body.Data)
	for {
		f, e := resp.Stream.Next(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		if f.Type == core.FrameComplete {
			if f.Complete.Outcome != core.OutcomeSucceeded {
				t.Fatalf("incomplete: %#v", f.Complete)
			}
			if f.Complete.Usage.InputTokens == nil || *f.Complete.Usage.InputTokens != 10 {
				t.Fatalf("usage: %#v", f.Complete.Usage)
			}
			break
		}
		got.Write(f.Body.Data)
	}
	if got.String() != stream {
		t.Fatalf("SSE changed\nwant %q\n got %q", stream, got.String())
	}
}

type gatedReader struct {
	first, rest             []byte
	release                 chan struct{}
	firstOffset, restOffset int
	waited                  bool
}

func (r *gatedReader) Read(p []byte) (int, error) {
	if r.firstOffset < len(r.first) {
		n := copy(p, r.first[r.firstOffset:])
		r.firstOffset += n
		return n, nil
	}
	if !r.waited {
		<-r.release
		r.waited = true
	}
	if r.restOffset < len(r.rest) {
		n := copy(p, r.rest[r.restOffset:])
		r.restOffset += n
		return n, nil
	}
	return 0, io.EOF
}
func (*gatedReader) Close() error { return nil }

func TestSSEMissingDoneIsIncomplete(t *testing.T) {
	resp, ge := execute(context.Background(), config{Endpoint: "https://example.test/v1/chat/completions"}, core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol}}, "token", doerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {}\n\n"))}, nil
	}))
	if ge != nil {
		t.Fatal(ge)
	}
	defer resp.Stream.Close()
	_, _ = resp.Stream.Next(context.Background())
	_, _ = resp.Stream.Next(context.Background())
	done, e := resp.Stream.Next(context.Background())
	if e != nil || done.Complete.Outcome != core.OutcomeIncomplete {
		t.Fatalf("expected incomplete: %#v %v", done, e)
	}
}

func TestSSEMalformedAndUnterminatedEventsFail(t *testing.T) {
	for _, body := range []string{"data: not-json\n\ndata: [DONE]\n\n", "data: {\"choices\":[]}\n"} {
		resp, ge := execute(context.Background(), config{Endpoint: "https://example.test/v1/chat/completions"}, core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol}}, "token", doerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		if ge != nil {
			t.Fatal(ge)
		}
		defer resp.Stream.Close()
		_, _ = resp.Stream.Next(context.Background())
		for {
			f, e := resp.Stream.Next(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			if f.Type == core.FrameComplete {
				if f.Complete.Outcome != core.OutcomeIncomplete {
					t.Fatalf("accepted malformed stream: %#v", f.Complete)
				}
				break
			}
		}
	}
}

func TestSSEWithoutUsageKeepsUsageUnknown(t *testing.T) {
	body := "data: {\"id\":\"chunk\"}\n\ndata: [DONE]\n\n"
	resp, ge := execute(context.Background(), config{Endpoint: "https://example.test/v1/chat/completions"}, core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol}}, "token", doerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	if ge != nil {
		t.Fatal(ge)
	}
	defer resp.Stream.Close()
	_, _ = resp.Stream.Next(context.Background())
	for {
		f, e := resp.Stream.Next(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		if f.Type == core.FrameComplete {
			if f.Complete.Outcome != core.OutcomeSucceeded || f.Complete.Usage.Source != core.UsageUnknown {
				t.Fatalf("completion %#v", f.Complete)
			}
			break
		}
	}
}

func TestJSONUnknownUsageAndMalformedResponseAreNotBufferedAsSuccess(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		outcome    core.Outcome
	}{
		{name: "missing usage", body: `{"choices":[]}`, outcome: core.OutcomeSucceeded},
		{name: "malformed JSON", body: `{"choices":`, outcome: core.OutcomeIncomplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, ge := execute(context.Background(), config{Endpoint: "https://example.test/v1/chat/completions"}, core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol}}, "token", doerFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			if ge != nil {
				t.Fatal(ge)
			}
			defer resp.Stream.Close()
			_, _ = resp.Stream.Next(context.Background())
			f, e := resp.Stream.Next(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			if string(f.Body.Data) != tc.body {
				t.Fatalf("body changed: %q", f.Body.Data)
			}
			done, e := resp.Stream.Next(context.Background())
			if e != nil || done.Complete.Outcome != tc.outcome {
				t.Fatalf("completion %#v %v", done, e)
			}
			if tc.name == "missing usage" && done.Complete.Usage.Source != core.UsageUnknown {
				t.Fatalf("missing usage was not unknown: %#v", done.Complete.Usage)
			}
		})
	}
}

func TestUpstreamFailuresDoNotRetryOrInventTerminalData(t *testing.T) {
	t.Run("pre-head HTTP error", func(t *testing.T) {
		calls := 0
		resp, ge := execute(context.Background(), config{Endpoint: "https://example.test/v1/chat/completions"}, core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol}}, "token", doerFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":"busy"}`))}, nil
		}))
		if ge != nil {
			t.Fatal(ge)
		}
		defer resp.Stream.Close()
		head, e := resp.Stream.Next(context.Background())
		if e != nil || head.Head.Error == nil {
			t.Fatalf("missing classified rejection: %#v %v", head, e)
		}
		_, _ = resp.Stream.Next(context.Background())
		done, e := resp.Stream.Next(context.Background())
		if e != nil || done.Complete.Outcome != core.OutcomeFailed || calls != 1 {
			t.Fatalf("completion %#v calls=%d err=%v", done, calls, e)
		}
	})
	t.Run("post-head read failure", func(t *testing.T) {
		calls := 0
		partial := `{"choices":[]}`
		resp, ge := execute(context.Background(), config{Endpoint: "https://example.test/v1/chat/completions"}, core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol}}, "token", doerFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: &errorBody{data: []byte(partial)}}, nil
		}))
		if ge != nil {
			t.Fatal(ge)
		}
		defer resp.Stream.Close()
		_, _ = resp.Stream.Next(context.Background())
		f, e := resp.Stream.Next(context.Background())
		if e != nil || string(f.Body.Data) != partial {
			t.Fatalf("body %#v %v", f, e)
		}
		done, e := resp.Stream.Next(context.Background())
		if e != nil || done.Complete.Outcome != core.OutcomeIncomplete || calls != 1 {
			t.Fatalf("completion %#v calls=%d err=%v", done, calls, e)
		}
	})
	t.Run("post-head SSE failure", func(t *testing.T) {
		calls := 0
		partial := "data: {\"choices\":[]}\n\n"
		resp, ge := execute(context.Background(), config{Endpoint: "https://example.test/v1/chat/completions"}, core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol}}, "token", doerFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: &errorBody{data: []byte(partial)}}, nil
		}))
		if ge != nil {
			t.Fatal(ge)
		}
		defer resp.Stream.Close()
		_, _ = resp.Stream.Next(context.Background())
		f, e := resp.Stream.Next(context.Background())
		if e != nil || string(f.Body.Data) != partial {
			t.Fatalf("raw SSE changed: %#v %v", f, e)
		}
		done, e := resp.Stream.Next(context.Background())
		if e != nil || done.Complete.Outcome != core.OutcomeIncomplete || strings.Contains(string(f.Body.Data), "[DONE]") || calls != 1 {
			t.Fatalf("completion %#v calls=%d err=%v", done, calls, e)
		}
	})
}

type errorBody struct {
	data []byte
	sent bool
}

func (b *errorBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, errors.New("synthetic upstream read failure")
	}
	b.sent = true
	return copy(p, b.data), nil
}
func (*errorBody) Close() error { return nil }

type blockedBody struct {
	once    sync.Once
	closed  chan struct{}
	started chan struct{}
}

func (b *blockedBody) Read([]byte) (int, error) {
	if b.started != nil {
		select {
		case <-b.started:
		default:
			close(b.started)
		}
	}
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *blockedBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

type pacedBody struct {
	chunks  [][]byte
	release []chan struct{}
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
	index   int
}

func newPacedBody(chunks [][]byte) *pacedBody {
	b := &pacedBody{chunks: chunks, release: make([]chan struct{}, len(chunks)-1), started: make(chan struct{}), closed: make(chan struct{})}
	for i := range b.release {
		b.release[i] = make(chan struct{}, 1)
	}
	return b
}

func (b *pacedBody) Read(p []byte) (int, error) {
	if b.index == len(b.chunks) {
		return 0, io.EOF
	}
	i := b.index
	if i > 0 {
		b.started <- struct{}{}
		select {
		case <-b.release[i-1]:
		case <-b.closed:
			return 0, io.ErrClosedPipe
		}
	}
	b.index++
	return copy(p, b.chunks[i]), nil
}

func (b *pacedBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

func TestStreamIdleTimeoutResetsForActiveSSEAndJSON(t *testing.T) {
	const idleTimeout = time.Second
	for _, tc := range []struct{ name, contentType, response string }{
		{name: "SSE", contentType: "text/event-stream", response: "data: {\"id\":\"x\",\"choices\":[]}\n\ndata: [DONE]\n\n"},
		{name: "JSON", contentType: "application/json", response: `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(tc.response)
			chunks := make([][]byte, 5)
			for i := range chunks {
				chunks[i] = data[i*len(data)/len(chunks) : (i+1)*len(data)/len(chunks)]
			}
			body := newPacedBody(chunks)
			defer body.Close()
			resp, ge := execute(context.Background(), config{Endpoint: "https://example.test/v1/chat/completions", StreamIdleTimeout: idleTimeout}, core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol}}, "token", doerFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{tc.contentType}}, Body: body}, nil
			}))
			if ge != nil {
				t.Fatal(ge)
			}
			defer resp.Stream.Close()
			if frame, err := resp.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
				t.Fatalf("head=%#v err=%v", frame, err)
			}
			startedAt := time.Now()
			var got []byte
			for i, want := range chunks {
				var frame core.StreamFrame
				var err error
				if i == 0 {
					frame, err = resp.Stream.Next(context.Background())
				} else {
					next := make(chan struct {
						frame core.StreamFrame
						err   error
					}, 1)
					go func() {
						f, e := resp.Stream.Next(context.Background())
						next <- struct {
							frame core.StreamFrame
							err   error
						}{f, e}
					}()
					select {
					case <-body.started:
					case <-time.After(2 * time.Second):
						t.Fatal("upstream did not start the next chunk read")
					}
					// Each body.Read waits only 300ms (<1s), while four gaps keep
					// the stream active for >1s overall.
					timer := time.NewTimer(300 * time.Millisecond)
					<-timer.C
					body.release[i-1] <- struct{}{}
					select {
					case result := <-next:
						frame, err = result.frame, result.err
					case <-time.After(2 * time.Second):
						t.Fatal("upstream chunk read did not resume")
					}
				}
				if err != nil || frame.Type != core.FrameBody || !bytes.Equal(frame.Body.Data, want) {
					t.Fatalf("chunk %d=%#v err=%v want=%q", i, frame, err, want)
				}
				got = append(got, frame.Body.Data...)
			}
			if elapsed := time.Since(startedAt); elapsed <= idleTimeout {
				t.Fatalf("paced stream lasted %v, want longer than idle timeout %v", elapsed, idleTimeout)
			}
			complete, err := resp.Stream.Next(context.Background())
			if err != nil || complete.Type != core.FrameComplete || complete.Complete.Outcome != core.OutcomeSucceeded || !bytes.Equal(got, data) {
				t.Fatalf("paced stream complete=%#v err=%v bytes match=%t", complete, err, bytes.Equal(got, data))
			}

			fast, ge := execute(context.Background(), config{Endpoint: "https://example.test/v1/chat/completions", StreamIdleTimeout: idleTimeout}, core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol}}, "token", doerFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{tc.contentType}}, Body: io.NopCloser(strings.NewReader(tc.response))}, nil
			}))
			if ge != nil {
				t.Fatal(ge)
			}
			defer fast.Stream.Close()
			_, _ = fast.Stream.Next(context.Background())
			var fastBytes []byte
			for {
				frame, err := fast.Stream.Next(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if frame.Type == core.FrameComplete {
					if frame.Complete.Outcome != core.OutcomeSucceeded || !bytes.Equal(fastBytes, data) {
						t.Fatalf("fast completion=%#v bytes match=%t", frame.Complete, bytes.Equal(fastBytes, data))
					}
					break
				}
				fastBytes = append(fastBytes, frame.Body.Data...)
			}
		})
	}
}

func TestStreamIdleTimeoutClosesAndCancelsSSEAndJSON(t *testing.T) {
	for _, tc := range []struct{ name, contentType string }{
		{name: "SSE", contentType: "text/event-stream"},
		{name: "JSON", contentType: "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &blockedBody{closed: make(chan struct{}), started: make(chan struct{})}
			var requestCtx context.Context
			response, ge := execute(context.Background(), config{Endpoint: "https://example.test/v1/chat/completions", StreamIdleTimeout: 30 * time.Millisecond}, core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol}}, "token", doerFunc(func(r *http.Request) (*http.Response, error) {
				requestCtx = r.Context()
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{tc.contentType}}, Body: body}, nil
			}))
			if ge != nil {
				t.Fatal(ge)
			}
			_, _ = response.Stream.Next(context.Background())
			frame, err := response.Stream.Next(context.Background())
			if err != nil || frame.Type != core.FrameComplete || frame.Complete.Outcome != core.OutcomeIncomplete || frame.Complete.Error == nil || frame.Complete.Error.Code != "upstream_timeout" {
				t.Fatalf("timeout completion = %#v, %v", frame, err)
			}
			select {
			case <-body.started:
			default:
				t.Fatal("upstream read did not start")
			}
			select {
			case <-body.closed:
			default:
				t.Fatal("timeout did not close upstream body")
			}
			select {
			case <-requestCtx.Done():
			default:
				t.Fatal("timeout did not cancel upstream request")
			}
		})
	}
}

func TestSSECloseInterruptsBlockedRead(t *testing.T) {
	body := &blockedBody{closed: make(chan struct{})}
	status := 200
	s := newSSE(body, &core.HeadFrame{Protocol: protocol, HTTPStatus: &status}, func() {}, 0)
	_, _ = s.Next(context.Background())
	done := make(chan error, 1)
	go func() { _, e := s.Next(context.Background()); done <- e }()
	select {
	case <-time.After(20 * time.Millisecond):
	case e := <-done:
		t.Fatalf("read did not block: %v", e)
	}
	_ = s.Close()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("blocked read returned success after close")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt upstream read")
	}
}

func TestJSONResponseIsForwardedInBoundedIncrementalFrames(t *testing.T) {
	first := `{"choices":[],` + strings.Repeat(" ", 50<<10)
	rest := `"padding":1,"usage":{"prompt_tokens":1,"completion_tokens":2}}`
	reader := &gatedReader{first: []byte(first), rest: []byte(rest), release: make(chan struct{})}
	resp, ge := execute(context.Background(), config{Endpoint: "https://example.test/v1/chat/completions"}, core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol}}, "token", doerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: reader}, nil
	}))
	if ge != nil {
		t.Fatal(ge)
	}
	defer resp.Stream.Close()
	_, _ = resp.Stream.Next(context.Background())
	chunk, e := resp.Stream.Next(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if len(chunk.Body.Data) > jsonFrameSize {
		t.Fatalf("unbounded frame: %d", len(chunk.Body.Data))
	}
	close(reader.release)
	var got []byte
	got = append(got, chunk.Body.Data...)
	for {
		f, e := resp.Stream.Next(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		if f.Type == core.FrameComplete {
			if f.Complete.Outcome != core.OutcomeSucceeded || f.Complete.Usage.InputTokens == nil || *f.Complete.Usage.InputTokens != 1 {
				t.Fatalf("completion %#v", f.Complete)
			}
			break
		}
		got = append(got, f.Body.Data...)
	}
	if string(got) != first+rest {
		t.Fatal("JSON response bytes changed")
	}
}
