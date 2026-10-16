package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

type offlineFakeDoer func(*http.Request) (*http.Response, error)

func (offlineFakeDoer) claudeCodeOfflineFakeOnly()                   {}
func (f offlineFakeDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

type plainFakeDoer func(*http.Request) (*http.Response, error)

func (f plainFakeDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

type fixtureCredentials string

func (f fixtureCredentials) Get(context.Context, string) ([]byte, error) { return []byte(f), nil }

type failingCredentials struct{}

func (failingCredentials) Get(context.Context, string) ([]byte, error) {
	return nil, io.ErrUnexpectedEOF
}

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return nil }

type failingStreamBody struct {
	remaining []byte
	closed    bool
}

func (b *failingStreamBody) Read(p []byte) (int, error) {
	if len(b.remaining) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, b.remaining)
	b.remaining = b.remaining[n:]
	return n, nil
}

func (b *failingStreamBody) Close() error { b.closed = true; return nil }

func TestPublicExecuteSyntheticMessagesStream(t *testing.T) {
	var body *trackedBody
	doer := offlineFakeDoer(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.anthropic.invalid/v1/messages?beta=true" || r.Header.Get("anthropic-beta") != "synthetic-beta" || r.Header.Get("Authorization") != "Bearer synthetic-access" {
			t.Fatalf("unexpected offline request: %s %v", r.URL, r.Header)
		}
		body = &trackedBody{Reader: strings.NewReader(streamStart + "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\ndata: {\"type\":\"message_stop\"}\n\n")}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}}, Body: body}, nil
	})
	c := initializedClaudeConnector(t)
	response, ge := c.Execute(context.Background(), sampleExecuteRequest(), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{Transport: doer, Credentials: fixtureCredentials(`{"access_token":"synthetic-access"}`)})
	if ge != nil {
		t.Fatal(ge)
	}
	seenHead, seenBody := false, false
	for range 16 {
		frame, err := response.Stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		switch frame.Type {
		case core.FrameHead:
			seenHead = true
		case core.FrameBody:
			seenBody = true
		case core.FrameComplete:
			if !seenHead || !seenBody {
				t.Fatalf("incomplete observed lifecycle: head=%v body=%v", seenHead, seenBody)
			}
			goto complete
		default:
			t.Fatalf("unexpected frame: %+v", frame)
		}
	}
	t.Fatal("stream did not complete within 16 frames")
complete:
	if _, err := response.Stream.Next(context.Background()); err != io.EOF {
		t.Fatalf("stream end = %v, want EOF", err)
	}
	if !body.closed {
		t.Fatal("successful response body was not closed")
	}
}

func TestExecuteToolHistoryDispatchesSyntheticMessages(t *testing.T) {
	request := core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol, ContentType: "application/json", Body: []byte(`{"model":"synthetic-model","input":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"one\"}"},{"type":"function_call","call_id":"call_2","name":"lookup","arguments":"{\"q\":\"two\"}"},{"type":"function_call_output","call_id":"call_1","output":"one"},{"type":"function_call_output","call_id":"call_2","output":"two"}]}`)}}
	calls := 0
	c := initializedClaudeConnector(t)
	response, ge := c.Execute(context.Background(), request, core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{
		Transport: offlineFakeDoer(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Host != "api.anthropic.invalid" || r.Header.Get("anthropic-beta") != "synthetic-beta" {
				t.Fatalf("unexpected synthetic target: %s %v", r.URL, r.Header)
			}
			var got messagesRequest
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if len(got.Messages) != 2 || len(got.Messages[0].Content) != 2 || got.Messages[0].Content[1].ID != "call_2" || len(got.Messages[1].Content) != 2 || got.Messages[1].Content[0].ToolUseID != "call_1" {
				t.Fatalf("dispatched tool history = %+v", got.Messages)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(streamStart + "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\ndata: {\"type\":\"message_stop\"}\n\n"))}, nil
		}),
		Credentials: fixtureCredentials(`{"access_token":"synthetic-access"}`),
	})
	if ge != nil {
		t.Fatal(ge)
	}
	defer response.Stream.Close()
	for range 8 {
		frame, err := response.Stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameComplete {
			if calls != 1 || frame.Complete.Outcome != core.OutcomeSucceeded {
				t.Fatalf("dispatches=%d completion=%+v", calls, frame.Complete)
			}
			return
		}
	}
	t.Fatal("synthetic tool history stream did not complete")
}

func TestExecuteRejectsInvalidToolHistoryBeforeDispatch(t *testing.T) {
	calls := 0
	c := initializedClaudeConnector(t)
	call := `{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"}`
	result := `{"type":"function_call_output","call_id":"c1","output":"ok"}`
	for _, input := range []string{
		`[` + result + `]`,
		`[` + call + `]`,
		`[` + call + `,{"type":"message","role":"user","content":"interleaved"}]`,
		`[` + call + `,{"type":"function_call","call_id":"c2","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"},{"type":"function_call","call_id":"c3","name":"lookup","arguments":"{}"}]`,
	} {
		request := sampleExecuteRequest()
		request.Payload.Body = []byte(`{"model":"synthetic-model","input":` + input + `}`)
		_, ge := c.Execute(context.Background(), request, core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{
			Transport:   offlineFakeDoer(func(*http.Request) (*http.Response, error) { calls++; return nil, nil }),
			Credentials: fixtureCredentials(`{"access_token":"synthetic-access"}`),
		})
		if ge == nil || ge.Code != "unsupported_feature" || ge.Category != core.CategoryUnsupportedFeature || calls != 0 {
			t.Fatalf("Execute error=%+v dispatches=%d, want pre-dispatch unsupported_feature", ge, calls)
		}
	}
}

func TestClaudePublicExecuteDeliversIncrementallyWithBackpressure(t *testing.T) {
	reader := &eventReader{events: []string{
		streamStart,
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"early\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}}
	calls := 0
	c := initializedClaudeConnector(t)
	response, ge := c.Execute(context.Background(), sampleExecuteRequest(), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{
		Transport: offlineFakeDoer(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}, nil
		}), Credentials: fixtureCredentials(`{"access_token":"synthetic-access"}`),
	})
	if ge != nil {
		t.Fatal(ge)
	}
	defer response.Stream.Close()
	if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead || reader.reads != 1 {
		t.Fatalf("Head=(%+v,%v) reads=%d", frame, err, reader.reads)
	}
	if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameBody || reader.reads != 1 {
		t.Fatalf("initial Body=(%+v,%v) reads=%d", frame, err, reader.reads)
	}
	// The next upstream event yields two downstream frames; the queued frame must
	// be delivered before another upstream read (bounded backpressure).
	if _, err := response.Stream.Next(context.Background()); err != nil || reader.reads != 2 {
		t.Fatalf("content start read count=%d err=%v", reader.reads, err)
	}
	if _, err := response.Stream.Next(context.Background()); err != nil || reader.reads != 2 {
		t.Fatalf("queued Body caused read-ahead: reads=%d err=%v", reader.reads, err)
	}
	for range 16 {
		frame, err := response.Stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameBody && strings.Contains(string(frame.Body.Data), `"delta":"early"`) {
			if reader.reads != 3 || calls != 1 {
				t.Fatalf("early output waited for upstream finish: reads=%d calls=%d", reader.reads, calls)
			}
			return
		}
	}
	t.Fatal("incremental response frame not observed within 16 frames")
}

func TestClaudePublicExecuteCancellationAndRepeatedCloseNeverRedispatch(t *testing.T) {
	c := initializedClaudeConnector(t)
	body := newBlockingReadCloser(streamStart)
	calls := 0
	response, ge := c.Execute(context.Background(), sampleExecuteRequest(), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{
		Transport: offlineFakeDoer(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
		}), Credentials: fixtureCredentials(`{"access_token":"synthetic-access"}`),
	})
	if ge != nil {
		t.Fatal(ge)
	}
	if _, err := response.Stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := response.Stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	blocked := make(chan error, 1)
	go func() { _, err := response.Stream.Next(ctx); blocked <- err }()
	select {
	case <-body.entered:
	case <-time.After(time.Second):
		cancel()
		_ = response.Stream.Close()
		t.Fatal("stream did not enter blocked body read")
	}
	cancel()
	select {
	case err := <-blocked:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked Next=%v, want cancellation", err)
		}
	case <-time.After(time.Second):
		_ = response.Stream.Close()
		t.Fatal("cancellation did not unblock body read")
	}
	for range 3 {
		if err := response.Stream.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 || body.closes.Load() != 1 {
		t.Fatalf("transport calls=%d body closes=%d, want 1/1", calls, body.closes.Load())
	}
	if _, err := response.Stream.Next(context.Background()); err != io.EOF {
		t.Fatalf("Next after cancel/Close=%v, want EOF", err)
	}
}

func TestClaudePublicExecuteRejectsOverLimitEventWithOneFailureCompletion(t *testing.T) {
	oversized := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + strings.Repeat("x", maxSSEBytes) + "\"}}\n\n"
	body := &trackedBody{Reader: strings.NewReader(streamStart + oversized)}
	c := initializedClaudeConnector(t)
	calls := 0
	response, ge := c.Execute(context.Background(), sampleExecuteRequest(), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{
		Transport: offlineFakeDoer(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
		}), Credentials: fixtureCredentials(`{"access_token":"synthetic-access"}`),
	})
	if ge != nil {
		t.Fatal(ge)
	}
	defer response.Stream.Close()
	if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
		t.Fatalf("first frame=(%+v,%v), want Head", frame, err)
	}
	completes := 0
	for range 16 {
		frame, err := response.Stream.Next(context.Background())
		if err == io.EOF {
			if completes != 1 || calls != 1 || !body.closed {
				t.Fatalf("completions=%d calls=%d body closed=%v", completes, calls, body.closed)
			}
			if _, err := response.Stream.Next(context.Background()); err != io.EOF {
				t.Fatalf("post-terminal Next=%v, want EOF", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameComplete {
			completes++
			if frame.Complete.Outcome != core.OutcomeFailed || frame.Complete.Error == nil || frame.Complete.Error.Code != "upstream_read_error" || frame.Complete.Usage == nil || frame.Complete.Usage.Source != core.UsageProvider || frame.Complete.Usage.Completeness != core.UsagePartial {
				t.Fatalf("oversize completion=%+v", frame.Complete)
			}
		}
	}
	t.Fatalf("stream did not reach EOF within 16 frames (completions=%d)", completes)
}

func TestClaudePublicExecuteMalformedEventCompletesOnceAndCloses(t *testing.T) {
	// A delta before content_block_start is a malformed event sequence.
	body := &trackedBody{Reader: strings.NewReader(streamStart + "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"bad\"}}\n\n")}
	calls := 0
	c := initializedClaudeConnector(t)
	response, ge := c.Execute(context.Background(), sampleExecuteRequest(), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{
		Transport: offlineFakeDoer(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
		}), Credentials: fixtureCredentials(`{"access_token":"synthetic-access"}`),
	})
	if ge != nil {
		t.Fatal(ge)
	}
	defer response.Stream.Close()
	frame, err := response.Stream.Next(context.Background())
	if err != nil || frame.Type != core.FrameHead {
		t.Fatalf("first frame=(%+v,%v), want Head", frame, err)
	}
	completes := 0
	sawEOF := false
	for range 16 {
		frame, err = response.Stream.Next(context.Background())
		if err == io.EOF {
			sawEOF = true
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type != core.FrameComplete {
			continue
		}
		completes++
		usage := frame.Complete.Usage
		if frame.Complete.Outcome != core.OutcomeFailed || frame.Complete.Error == nil || frame.Complete.Error.Code != "invalid_response" || usage == nil || usage.Source != core.UsageProvider || usage.Completeness != core.UsagePartial || !sameCount(usage.InputTokens, int64Ptr(5)) || usage.OutputTokens != nil {
			t.Fatalf("malformed-event completion=%+v", frame.Complete)
		}
	}
	if !sawEOF || completes != 1 || calls != 1 || !body.closed {
		t.Fatalf("EOF=%v completions=%d calls=%d body closed=%v, want true/1/1/true", sawEOF, completes, calls, body.closed)
	}
	if _, err := response.Stream.Next(context.Background()); err != io.EOF {
		t.Fatalf("post-terminal Next=%v, want EOF", err)
	}
}

func TestPublicExecuteRejectsUntrustedTransportAndBadCredentialsBeforeDispatch(t *testing.T) {
	called := false
	unsafe := core.HTTPDoer(plainFakeDoer(func(*http.Request) (*http.Response, error) { called = true; return nil, nil }))
	c := initializedClaudeConnector(t)
	for _, tc := range []struct {
		name     string
		services core.InvocationServices
		code     string
	}{
		{"unmarked transport", core.InvocationServices{Transport: unsafe, Credentials: fixtureCredentials(`{"access_token":"x"}`)}, "connector_unavailable"},
		{"nil credentials", core.InvocationServices{Transport: offlineFakeDoer(func(*http.Request) (*http.Response, error) { called = true; return nil, nil })}, "credentials_unavailable"},
		{"credential read error", core.InvocationServices{Transport: offlineFakeDoer(func(*http.Request) (*http.Response, error) { called = true; return nil, nil }), Credentials: failingCredentials{}}, "credentials_unavailable"},
		{"malformed credential JSON", core.InvocationServices{Transport: offlineFakeDoer(func(*http.Request) (*http.Response, error) { called = true; return nil, nil }), Credentials: fixtureCredentials(`{"access_token":`)}, "credentials_invalid"},
		{"trailing credential JSON", core.InvocationServices{Transport: offlineFakeDoer(func(*http.Request) (*http.Response, error) { called = true; return nil, nil }), Credentials: fixtureCredentials(`{"access_token":"x"}{}`)}, "credentials_invalid"},
		{"CRLF access token", core.InvocationServices{Transport: offlineFakeDoer(func(*http.Request) (*http.Response, error) { called = true; return nil, nil }), Credentials: fixtureCredentials("{\"access_token\":\"x\\r\\ny\"}")}, "credentials_invalid"},
		{"unknown credential field", core.InvocationServices{Transport: offlineFakeDoer(func(*http.Request) (*http.Response, error) { called = true; return nil, nil }), Credentials: fixtureCredentials(`{"access_token":"x","other":"y"}`)}, "credentials_invalid"},
		{"empty access token", core.InvocationServices{Transport: offlineFakeDoer(func(*http.Request) (*http.Response, error) { called = true; return nil, nil }), Credentials: fixtureCredentials(`{"access_token":""}`)}, "credentials_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ge := c.Execute(context.Background(), sampleExecuteRequest(), core.AttemptScope{Mode: core.ModeTranslation}, tc.services); ge == nil || ge.Code != tc.code || ge.Category != core.CategoryUnauthenticated && tc.code != "connector_unavailable" || ge.Retryable || ge.RetryDisposition != core.RetryUnsafe && tc.code != "connector_unavailable" {
				t.Fatalf("Execute error = %+v, want %s, unauthenticated, non-retryable/unsafe", ge, tc.code)
			}
		})
	}
	if called {
		t.Fatal("rejected invocation dispatched transport")
	}
}

func TestPublicExecuteNilResponseAndBodyAreUnknownUnavailable(t *testing.T) {
	c := initializedClaudeConnector(t)
	for _, tc := range []struct {
		name     string
		response *http.Response
	}{
		{"nil response", nil},
		{"nil body", &http.Response{StatusCode: http.StatusOK}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ge := c.Execute(context.Background(), sampleExecuteRequest(), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{
				Transport:   offlineFakeDoer(func(*http.Request) (*http.Response, error) { return tc.response, nil }),
				Credentials: fixtureCredentials(`{"access_token":"x"}`),
			})
			if ge == nil || ge.Code != "upstream_unavailable" || ge.Category != core.CategoryUnavailable || ge.Retryable || ge.RetryDisposition != core.RetryUnknown {
				t.Fatalf("Execute error = %+v, want unavailable/non-retryable/unknown", ge)
			}
		})
	}
}

func TestPublicExecuteCancellationAfterDoClosesResponse(t *testing.T) {
	c := initializedClaudeConnector(t)
	ctx, cancel := context.WithCancel(context.Background())
	body := &trackedBody{Reader: strings.NewReader(streamStart)}
	_, ge := c.Execute(ctx, sampleExecuteRequest(), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{
		Transport: offlineFakeDoer(func(*http.Request) (*http.Response, error) {
			cancel()
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
		}), Credentials: fixtureCredentials(`{"access_token":"x"}`),
	})
	if ge == nil || ge.Code != "request_cancelled" || ge.Category != core.CategoryCancelled || ge.Retryable || !unknownDisposition(ge.RetryDisposition) || !body.closed {
		t.Fatalf("Execute = (%+v, %+v), want canceled/unknown and closed response", nil, ge)
	}
}

func TestPublicExecuteCanceledNextClosesStream(t *testing.T) {
	c := initializedClaudeConnector(t)
	body := &trackedBody{Reader: strings.NewReader(streamStart + "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\ndata: {\"type\":\"message_stop\"}\n\n")}
	response, ge := c.Execute(context.Background(), sampleExecuteRequest(), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{
		Transport: offlineFakeDoer(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
		}), Credentials: fixtureCredentials(`{"access_token":"x"}`),
	})
	if ge != nil {
		t.Fatal(ge)
	}
	if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
		t.Fatalf("head = (%+v, %v)", frame, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := response.Stream.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Next error = %v", err)
	}
	if !body.closed {
		t.Fatal("canceled Next did not close the upstream body")
	}
	if frame, err := response.Stream.Next(context.Background()); err != io.EOF || frame.Type != "" {
		t.Fatalf("Next after cancellation = (%+v, %v), want EOF without a success completion", frame, err)
	}
}

func TestPublicExecuteReadFailureCompletesOnceAndCloses(t *testing.T) {
	c := initializedClaudeConnector(t)
	body := &failingStreamBody{remaining: []byte(streamStart)}
	response, ge := c.Execute(context.Background(), sampleExecuteRequest(), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{
		Transport: offlineFakeDoer(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
		}), Credentials: fixtureCredentials(`{"access_token":"x"}`),
	})
	if ge != nil {
		t.Fatal(ge)
	}
	if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
		t.Fatalf("head = (%+v, %v)", frame, err)
	}
	for {
		frame, err := response.Stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameComplete {
			if frame.Complete.Outcome != core.OutcomeFailed || frame.Complete.Error == nil || frame.Complete.Error.Code != "upstream_read_error" || frame.Complete.Error.Retryable || !unknownDisposition(frame.Complete.Error.RetryDisposition) || !body.closed {
				t.Fatalf("failure completion = %+v, closed=%v", frame.Complete, body.closed)
			}
			break
		}
	}
	if _, err := response.Stream.Next(context.Background()); err != io.EOF {
		t.Fatalf("after one failure completion, Next = %v, want EOF", err)
	}
}

func unknownDisposition(disposition core.RetryDisposition) bool {
	return disposition == "" || disposition == core.RetryUnknown
}

func TestPublicExecuteConsumerCloseReleasesBodyBeforeEOF(t *testing.T) {
	c := initializedClaudeConnector(t)
	body := &trackedBody{Reader: strings.NewReader(streamStart + "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\ndata: {\"type\":\"message_stop\"}\n\n")}
	response, ge := c.Execute(context.Background(), sampleExecuteRequest(), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{
		Transport: offlineFakeDoer(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
		}), Credentials: fixtureCredentials(`{"access_token":"x"}`),
	})
	if ge != nil {
		t.Fatal(ge)
	}
	if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
		t.Fatalf("head = (%+v, %v)", frame, err)
	}
	if err := response.Stream.Close(); err != nil || !body.closed {
		t.Fatalf("Close = %v, body closed=%v", err, body.closed)
	}
	if frame, err := response.Stream.Next(context.Background()); err != io.EOF || frame.Type != "" {
		t.Fatalf("Next after consumer close = (%+v, %v), want EOF without Complete", frame, err)
	}
}

func TestPublicExecuteBoundsAndClosesErrorResponses(t *testing.T) {
	c := initializedClaudeConnector(t)
	var body *trackedBody
	_, ge := c.Execute(context.Background(), sampleExecuteRequest(), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{
		Transport: offlineFakeDoer(func(*http.Request) (*http.Response, error) {
			body = &trackedBody{Reader: strings.NewReader(strings.Repeat("x", authBodyLimit+1024))}
			return &http.Response{StatusCode: http.StatusTooManyRequests, Body: body}, nil
		}), Credentials: fixtureCredentials(`{"access_token":"x"}`),
	})
	if ge == nil || ge.Code != "upstream_rate_limited" || !ge.Retryable || ge.RetryDisposition != core.RetryUnknown {
		t.Fatalf("429 error = %+v", ge)
	}
	if !body.closed || body.Reader.(*strings.Reader).Len() != 1024 { // LimitReader must not drain beyond the bound.
		t.Fatal("error body was not closed or bounded")
	}
}

func TestPublicExecuteStatusClassificationAndTransportFailures(t *testing.T) {
	c := initializedClaudeConnector(t)
	for _, tc := range []struct {
		status      int
		code        string
		category    core.ErrorCategory
		retryable   bool
		disposition core.RetryDisposition
	}{
		{401, "upstream_unauthorized", core.CategoryUnauthenticated, false, core.RetryUnsafe},
		{403, "upstream_unauthorized", core.CategoryUnauthenticated, false, core.RetryUnsafe},
		{429, "upstream_rate_limited", core.CategoryRateLimited, true, core.RetryUnknown},
		{500, "upstream_unavailable", core.CategoryUnavailable, true, core.RetryUnknown},
		{302, "upstream_unavailable", core.CategoryUnavailable, false, core.RetryUnknown},
		{400, "upstream_unavailable", core.CategoryUnavailable, false, core.RetryUnknown},
	} {
		var body *trackedBody
		_, ge := c.Execute(context.Background(), sampleExecuteRequest(), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{
			Transport: offlineFakeDoer(func(*http.Request) (*http.Response, error) {
				body = &trackedBody{Reader: strings.NewReader("synthetic error")}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: body}, nil
			}), Credentials: fixtureCredentials(`{"access_token":"x"}`),
		})
		if ge == nil || ge.Code != tc.code || ge.Category != tc.category || ge.Retryable != tc.retryable || ge.RetryDisposition != tc.disposition || !body.closed {
			t.Errorf("status %d error/body = (%+v, %+v), want %s/%s retry=%v/%s and closed", tc.status, ge, body, tc.code, tc.category, tc.retryable, tc.disposition)
		}
	}
	_, ge := c.Execute(context.Background(), sampleExecuteRequest(), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{
		Transport:   offlineFakeDoer(func(*http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF }),
		Credentials: fixtureCredentials(`{"access_token":"x"}`),
	})
	if ge == nil || ge.Code != "upstream_unavailable" || ge.RetryDisposition != core.RetryUnknown || ge.Retryable {
		t.Fatalf("network error = %+v", ge)
	}
}

func TestPublicExecuteRejectsUnsupportedRequestsAndResponseHeaders(t *testing.T) {
	c := initializedClaudeConnector(t)
	calls := 0
	marked := offlineFakeDoer(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, nil
	})
	services := core.InvocationServices{Transport: marked, Credentials: fixtureCredentials(`{"access_token":"x"}`)}
	for _, tc := range []struct {
		name    string
		request core.ExecutionRequest
		scope   core.AttemptScope
		cancel  bool
	}{
		{"wrong protocol", sampleExecuteRequest(), core.AttemptScope{Mode: core.ModeTranslation}, false},
		{"wrong mode", sampleExecuteRequest(), core.AttemptScope{Mode: core.ModeNative}, false},
		{"unsupported payload", core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol, ContentType: "application/json", Body: []byte(`{"model":"x","input":"y","unknown":true}`)}}, core.AttemptScope{Mode: core.ModeTranslation}, false},
		{"canceled", sampleExecuteRequest(), core.AttemptScope{Mode: core.ModeTranslation}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := tc.request
			if tc.name == "wrong protocol" {
				request.Payload.Protocol = "other"
			}
			ctx := context.Background()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, ge := c.Execute(ctx, request, tc.scope, services); ge == nil {
				t.Fatal("Execute unexpectedly accepted invalid request")
			}
		})
	}
	if calls != 0 {
		t.Fatalf("invalid requests dispatched %d times", calls)
	}
	var body *trackedBody
	_, ge := c.Execute(context.Background(), sampleExecuteRequest(), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{
		Transport: offlineFakeDoer(func(*http.Request) (*http.Response, error) {
			body = &trackedBody{Reader: strings.NewReader("not sse")}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body}, nil
		}), Credentials: fixtureCredentials(`{"access_token":"x"}`),
	})
	if ge == nil || ge.Code != "invalid_response" || !body.closed {
		t.Fatalf("invalid response = (%+v, %+v), want closed invalid response", ge, body)
	}
}

func initializedClaudeConnector(t *testing.T) *Connector {
	t.Helper()
	c := NewConnector()
	if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"profile":"` + profile + `"}`)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func sampleExecuteRequest() core.ExecutionRequest {
	return core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol, ContentType: "application/json", Body: []byte(`{"model":"synthetic-model","input":"hello"}`)}}
}
