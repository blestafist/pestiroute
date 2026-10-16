package conformance

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	chatadapter "github.com/blestafist/pestiroute/internal/adapter/chatcompletions"
	chatconnector "github.com/blestafist/pestiroute/internal/connector/chatcompletions"
	"github.com/blestafist/pestiroute/internal/core"
)

type chatDoer func(*http.Request) (*http.Response, error)

func (f chatDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

type chatCredential struct{}

func (chatCredential) Get(context.Context, string) ([]byte, error) { return []byte("synthetic"), nil }

func chatComponents(t *testing.T) (*chatadapter.Adapter, *chatconnector.Connector) {
	t.Helper()
	a := chatadapter.NewAdapter()
	if e := a.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"max_body_bytes":4096,"max_header_bytes":4096}`)}); e != nil {
		t.Fatal(e)
	}
	c := chatconnector.NewConnector()
	if e := c.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"endpoint":"https://fake.invalid/v1/chat/completions","model":"m","account_id":"acct","max_request_header_bytes":4096}`)}); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()); _ = c.Close(context.Background()) })
	return a, c
}

func TestChatJSONClientToFakeBackend(t *testing.T) {
	a, c := chatComponents(t)
	raw := ` { "model" : "m", "messages" : [{"role":"user","content":"hello"}], "opaque":{"nested":[1, 2]} }` + "\n"
	upstream := `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-API-Key", "must-not-forward")
	r.Header.Set("OpenAI-Organization", "must-not-forward")
	r.Header.Set("OpenAI-Project", "must-not-forward")
	decoded, ge := a.Decode(r.Context(), core.ClientRequest{Transport: r})
	if ge != nil {
		t.Fatal(ge)
	}
	resp, ge := c.Execute(r.Context(), decoded, core.AttemptScope{Mode: core.ModeNative, AccountID: "acct", MaxBodyBytes: 4096}, core.InvocationServices{Credentials: chatCredential{}, Transport: chatDoer(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("X-API-Key") != "" || req.Header.Get("OpenAI-Organization") != "" || req.Header.Get("OpenAI-Project") != "" {
			t.Fatal("sensitive client headers forwarded")
		}
		body, _ := io.ReadAll(req.Body)
		if string(body) != raw {
			t.Fatalf("upstream body changed: %q", body)
		}
		if req.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path %s", req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(upstream))}, nil
	})})
	if ge != nil {
		t.Fatal(ge)
	}
	w := httptest.NewRecorder()
	if e := a.Encode(r.Context(), core.ClientResponse{Transport: chatadapter.HTTPResponse{Writer: w, Request: r}}, nil, resp); e != nil {
		t.Fatal(e)
	}
	if w.Code != http.StatusOK || w.Body.String() != upstream {
		t.Fatalf("client response status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestChatSSEClientToFakeBackend(t *testing.T) {
	a, c := chatComponents(t)
	raw := `{"model":"m","messages":[{"role":"user","content":"hello"}],"stream":true}`
	upstream := "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-a\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}},{\"index\":1,\"id\":\"call-b\",\"function\":{\"name\":\"g\",\"arguments\":\"[]\"}}]}}]}\n\ndata: {\"id\":\"2\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]}}]}\n\ndata: [DONE]\n\n"
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	decoded, ge := a.Decode(r.Context(), core.ClientRequest{Transport: r})
	if ge != nil {
		t.Fatal(ge)
	}
	resp, ge := c.Execute(r.Context(), decoded, core.AttemptScope{Mode: core.ModeNative, AccountID: "acct", MaxBodyBytes: 4096}, core.InvocationServices{Credentials: chatCredential{}, Transport: chatDoer(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(upstream))}, nil
	})})
	if ge != nil {
		t.Fatal(ge)
	}
	w := httptest.NewRecorder()
	if e := a.Encode(r.Context(), core.ClientResponse{Transport: chatadapter.HTTPResponse{Writer: w, Request: r}}, nil, resp); e != nil {
		t.Fatal(e)
	}
	if w.Code != http.StatusOK || w.Body.String() != upstream {
		t.Fatalf("client response status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestChatParallelToolHistoryRoundTrip(t *testing.T) {
	a, c := chatComponents(t)
	first := `{"model":"m","messages":[{"role":"user","content":"run tools"}],"tools":[{"type":"function","function":{"name":"a"}},{"type":"function","function":{"name":"b"}}],"parallel_tool_calls":true}`
	r1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(first))
	r1.Header.Set("Content-Type", "application/json")
	one, ge := a.Decode(r1.Context(), core.ClientRequest{Transport: r1})
	if ge != nil {
		t.Fatal(ge)
	}
	if _, ok := one.Capabilities["llm.tools.parallel"]; !ok {
		t.Fatal("parallel capability missing")
	}
	second := ` {"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call-a","type":"function","function":{"name":"a","arguments":"{}"}},{"id":"call-b","type":"function","function":{"name":"b","arguments":"[]"}}]},{"role":"tool","tool_call_id":"call-a","content":"A"},{"role":"tool","tool_call_id":"call-b","content":"B"}],"opaque":{"round":2} }`
	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(second))
	r2.Header.Set("Content-Type", "application/json")
	two, ge := a.Decode(r2.Context(), core.ClientRequest{Transport: r2})
	if ge != nil {
		t.Fatal(ge)
	}
	if _, ok := two.Capabilities["llm.tools"]; !ok {
		t.Fatal("tool-history capability missing")
	}
	resp, ge := c.Execute(r2.Context(), two, core.AttemptScope{Mode: core.ModeNative, AccountID: "acct", MaxBodyBytes: 4096}, core.InvocationServices{Credentials: chatCredential{}, Transport: chatDoer(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		if string(body) != second {
			t.Fatalf("round-two payload changed: %q", body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[]}`))}, nil
	})})
	if ge != nil {
		t.Fatal(ge)
	}
	_ = resp.Stream.Close()
}

type chatBlockedBody struct {
	once    sync.Once
	started chan struct{}
	closed  chan struct{}
}

func (b *chatBlockedBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *chatBlockedBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

type chatWriter struct {
	header http.Header
	wrote  chan struct{}
}

func (w *chatWriter) Header() http.Header { return w.header }
func (w *chatWriter) WriteHeader(int) {
	select {
	case w.wrote <- struct{}{}:
	default:
	}
}
func (*chatWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestChatDisconnectCancelsUpstreamAndClosesBody(t *testing.T) {
	a, c := chatComponents(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	raw := `{"model":"m","messages":[{"role":"user","content":"hello"}],"stream":true}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(raw)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	decoded, ge := a.Decode(ctx, core.ClientRequest{Transport: r})
	if ge != nil {
		t.Fatal(ge)
	}
	body := &chatBlockedBody{started: make(chan struct{}), closed: make(chan struct{})}
	var upstreamCtx context.Context
	resp, ge := c.Execute(ctx, decoded, core.AttemptScope{Mode: core.ModeNative, AccountID: "acct", MaxBodyBytes: 4096}, core.InvocationServices{Credentials: chatCredential{}, Transport: chatDoer(func(req *http.Request) (*http.Response, error) {
		upstreamCtx = req.Context()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
	})})
	if ge != nil {
		t.Fatal(ge)
	}
	w := &chatWriter{header: make(http.Header), wrote: make(chan struct{}, 1)}
	encoded := make(chan error, 1)
	go func() {
		encoded <- a.Encode(ctx, core.ClientResponse{Transport: chatadapter.HTTPResponse{Writer: w, Request: r}}, nil, resp)
	}()
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("stream read did not start")
	}
	cancel()
	select {
	case err := <-encoded:
		if err == nil {
			t.Fatal("Encode returned success after disconnect")
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect did not stop Encode")
	}
	select {
	case <-upstreamCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("upstream context was not cancelled")
	}
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("upstream body was not closed")
	}
}
