package chatcompletions

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestDecodePreservesOpaqueBodyAndDerivesCapabilities(t *testing.T) {
	body := ` { "model" : "m", "messages" : [{"role":"assistant","tool_calls":[{"id":"call-a","type":"function","function":{"name":"f","arguments":"{\"x\":1}"}}]}], "stream":true, "tools":[{"type":"function","function":{"name":"f","parameters":{"z":1}}}], "x-unknown":{"space": [1, 2]} }` + "\n"
	r := httptestRequest(body)
	got, ge := Decode(r, 4096, 4096)
	if ge != nil {
		t.Fatalf("Decode: %v", ge)
	}
	if string(got.Payload.Body) != body || got.Payload.Protocol != protocol || got.Model != "m" || got.Metadata.Streaming == nil || !*got.Metadata.Streaming {
		t.Fatalf("request changed: %#v", got)
	}
	for _, c := range []core.Capability{"llm.streaming", "llm.tools"} {
		if _, ok := got.Capabilities[c]; !ok {
			t.Errorf("missing capability %s", c)
		}
	}
}

func TestDescriptorBinding(t *testing.T) {
	d := NewAdapter().Descriptor()
	if d.ID != componentID || d.Protocols == nil || len(d.Protocols) != 1 || d.Protocols[0] != protocol || NewAdapter().Protocol() != protocol {
		t.Fatalf("unexpected adapter descriptor: %#v", d)
	}
}

func TestDecodeDoesNotReturnSuccessAfterCancellation(t *testing.T) {
	a := NewAdapter()
	if err := a.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"max_body_bytes":4096,"max_header_bytes":4096}`)}); err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	r := httptestRequest("{}").WithContext(ctx)
	body := &blockedRequestBody{started: make(chan struct{}), closed: make(chan struct{})}
	r.Body = body
	result := make(chan *core.GatewayError, 1)
	go func() { _, ge := a.Decode(ctx, core.ClientRequest{Transport: r}); result <- ge }()
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("Decode did not begin reading")
	}
	cancel()
	select {
	case ge := <-result:
		if ge == nil || ge.Code != "request_cancelled" {
			t.Fatalf("Decode result after cancellation: %#v", ge)
		}
	case <-time.After(time.Second):
		t.Fatal("Decode did not stop after cancellation")
	}
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("request body was not closed")
	}
}

type blockedRequestBody struct {
	once    sync.Once
	started chan struct{}
	closed  chan struct{}
}

func (b *blockedRequestBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *blockedRequestBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func TestDecodeRejectsInvalidAndUnsupportedRequests(t *testing.T) {
	for _, body := range []string{
		`[]`, `{"model":1,"messages":[{"role":"user","content":"x"}]}`,
		`{"model":"m","messages":[]}`, `{"model":"m","messages":[{"role":"user","content":[{"type":"file"}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"web_search_options":{}}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"unknown":{"x":1,"x":2}}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"tool_choice":null}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"function_call":null}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"parallel_tool_calls":null}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"functions":[{}]}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"f","strict":"yes"}}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x","detail":"huge"}}]}]}`,
		`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
	} {
		if _, ge := Decode(httptestRequest(body), 4096, 4096); ge == nil {
			t.Errorf("accepted %s", body)
		}
	}
	if _, ge := Decode(httptestRequest(`{"model":"m","messages":[{"role":"user","content":"x"}]}`), 4, 4096); ge == nil {
		t.Fatal("body limit not enforced")
	}
}

func TestDecodeFiltersSensitiveUpstreamHeaders(t *testing.T) {
	r := httptestRequest(`{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	for name, value := range map[string]string{"X-API-Key": "secret", "OpenAI-Organization": "org", "OpenAI-Project": "project", "Idempotency-Key": "allowed"} {
		r.Header.Set(name, value)
	}
	got, ge := Decode(r, 4096, 4096)
	if ge != nil {
		t.Fatal(ge)
	}
	for _, name := range []string{"X-API-Key", "OpenAI-Organization", "OpenAI-Project"} {
		if got.Metadata.Headers[http.CanonicalHeaderKey(name)] != nil {
			t.Errorf("forwardable metadata contains %s", name)
		}
	}
	if got.Metadata.Headers["Idempotency-Key"][0] != "allowed" {
		t.Fatal("idempotency key was not retained")
	}
}

func httptestRequest(body string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", io.NopCloser(strings.NewReader(body)))
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = int64(len(body))
	return r
}
