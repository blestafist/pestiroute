package responses

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

func adapterConfigJSON() core.ComponentConfig {
	b, _ := json.Marshal(adapterConfig{MaxBodyBytes: 4096, MaxHeaderBytes: 2048})
	return core.ComponentConfig{Data: b}
}

func TestAdapterDescriptorLifecycleAndCapabilities(t *testing.T) {
	a := NewAdapter()
	d := a.Descriptor()
	if err := d.Validate(core.ComponentAdapter); err != nil || a.Protocol() != d.Protocols[0] || d.ConnectorType != "" || len(d.AuthMethods) != 0 {
		t.Fatalf("descriptor/protocol: %+v protocol %q err %v", d, a.Protocol(), err)
	}
	if _, err := a.Decode(context.Background(), core.ClientRequest{Transport: request(base)}); err == nil || err.Category != core.CategoryUnavailable {
		t.Fatalf("Decode before Init category: %+v", err)
	}
	if err := a.Encode(context.Background(), clientResponse(), nil, core.ExecutionResponse{}); err == nil {
		t.Fatal("Encode accepted before Init")
	}
	scope := core.CapabilityScope{Protocol: protocol, Mode: "native", Model: model, AccountID: "account-a"}
	if got := a.Capabilities(context.Background(), scope); len(got.Values) != 0 {
		t.Fatalf("uninitialized capabilities: %+v", got)
	}
	if err := a.Init(context.Background(), adapterConfigJSON()); err != nil {
		t.Fatal(err)
	}
	got := a.Capabilities(context.Background(), scope)
	for _, capability := range []core.Capability{"llm.streaming", "llm.tools", "llm.tools.parallel", "llm.reasoning", "llm.structured_output", "llm.vision", "llm.audio"} {
		if got.State(capability) != core.Supported {
			t.Errorf("%s: %s", capability, got.State(capability))
		}
	}
	if a.Capabilities(context.Background(), core.CapabilityScope{Protocol: protocol, Mode: "translation", Model: model}).State("llm.tools") != core.Unknown {
		t.Fatalf("overbroad declarations: %+v", got)
	}
	if _, err := a.Decode(context.Background(), core.ClientRequest{Transport: request(base)}); err != nil {
		t.Fatalf("Decode after Init: %v", err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Decode(context.Background(), core.ClientRequest{Transport: request(base)}); err == nil || err.Category != core.CategoryUnavailable {
		t.Fatalf("Decode after Close category: %+v", err)
	}
	if err := a.Encode(context.Background(), clientResponse(), nil, core.ExecutionResponse{}); err == nil {
		t.Fatal("Encode accepted after Close")
	}
	if a.Health(context.Background()).State != core.HealthUnavailable || len(a.Capabilities(context.Background(), scope).Values) != 0 {
		t.Fatal("closed adapter remained available")
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close is not idempotent: %v", err)
	}
}

func TestAdapterDecodePreCancelledContextIsCancelled(t *testing.T) {
	a := NewAdapter()
	if err := a.Init(context.Background(), adapterConfigJSON()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Decode(ctx, core.ClientRequest{Transport: request(base)}); err == nil || err.Category != core.CategoryCancelled {
		t.Fatalf("pre-cancelled Decode category: %+v", err)
	}
}

func TestAdapterCloseCancelsAndWaitsForEncode(t *testing.T) {
	a := NewAdapter()
	if err := a.Init(context.Background(), adapterConfigJSON()); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	s := &encodeStream{next: func(ctx context.Context) (core.StreamFrame, error) {
		close(started)
		<-ctx.Done()
		return core.StreamFrame{}, ctx.Err()
	}}
	encoded := make(chan error, 1)
	go func() {
		encoded <- a.Encode(context.Background(), core.ClientResponse{Transport: HTTPResponse{
			Writer: discardWriter{}, Request: httptestRequest(),
		}}, nil, core.ExecutionResponse{Stream: s})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Encode did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Close(ctx); err != nil {
		t.Fatalf("Close did not cancel active Encode: %v", err)
	}
	if err := <-encoded; err == nil || !strings.Contains(err.Error(), "canceled") || !s.closed {
		t.Fatalf("Encode cancellation: %v closed %t", err, s.closed)
	}
}

func TestAdapterCloseInterruptsBlockedDecode(t *testing.T) {
	a := NewAdapter()
	if err := a.Init(context.Background(), adapterConfigJSON()); err != nil {
		t.Fatal(err)
	}
	body := &blockingBody{started: make(chan struct{}), closed: make(chan struct{})}
	r := request(base)
	r.Body = body
	decoded := make(chan *core.GatewayError, 1)
	go func() {
		_, err := a.Decode(context.Background(), core.ClientRequest{Transport: r})
		decoded <- err
	}()
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("Decode did not read body")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Close(ctx); err != nil {
		t.Fatalf("Close did not interrupt Decode: %v", err)
	}
	if err := <-decoded; err == nil || err.Category != core.CategoryCancelled {
		t.Fatalf("Decode cancellation: %+v", err)
	}
}

func TestAdapterEncodeFollowsMethodAndRequestCancellation(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		name := "method context"
		if cancelRequest {
			name = "request context"
		}
		t.Run(name, func(t *testing.T) {
			a := NewAdapter()
			if err := a.Init(context.Background(), adapterConfigJSON()); err != nil {
				t.Fatal(err)
			}
			methodCtx, cancelMethod := context.WithCancel(context.Background())
			defer cancelMethod()
			requestCtx, cancelReq := context.WithCancel(context.Background())
			defer cancelReq()
			r := httptestRequest().WithContext(requestCtx)
			started := make(chan struct{})
			s := &encodeStream{next: func(ctx context.Context) (core.StreamFrame, error) {
				close(started)
				<-ctx.Done()
				return core.StreamFrame{}, ctx.Err()
			}}
			encoded := make(chan error, 1)
			go func() {
				encoded <- a.Encode(methodCtx, core.ClientResponse{Transport: HTTPResponse{Writer: httptest.NewRecorder(), Request: r}}, nil, core.ExecutionResponse{Stream: s})
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("Encode did not start")
			}
			if cancelRequest {
				cancelReq()
			} else {
				cancelMethod()
			}
			select {
			case err := <-encoded:
				if !errors.Is(err, context.Canceled) || !s.closed {
					t.Fatalf("Encode cancellation: %v closed %t", err, s.closed)
				}
			case <-time.After(time.Second):
				t.Fatal("Encode did not stop after cancellation")
			}
			if err := a.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type blockingBody struct {
	started     chan struct{}
	closed      chan struct{}
	startedOnce sync.Once
	closeOnce   sync.Once
}

func (b *blockingBody) Read([]byte) (int, error) {
	b.startedOnce.Do(func() { close(b.started) })
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *blockingBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

type discardWriter struct{}

func (discardWriter) Header() http.Header       { return make(http.Header) }
func (discardWriter) Write([]byte) (int, error) { return 0, nil }
func (discardWriter) WriteHeader(int)           {}
func httptestRequest() *http.Request            { return httptest.NewRequest(http.MethodPost, "/", nil) }
func clientResponse() core.ClientResponse {
	return core.ClientResponse{Transport: HTTPResponse{Writer: httptest.NewRecorder(), Request: httptestRequest()}}
}
