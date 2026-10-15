package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/testutil/scripted"
)

// IPC comparison workload shared by the in-process control and both transport
// spikes. Keep the fixture bounded: 16 MiB output, one invocation, one callback.
const (
	ipcFrames        = 256
	ipcFrameBytes    = 64 << 10
	ipcTotalBytes    = ipcFrames * ipcFrameBytes
	ipcHeaderBytes   = 1024
	ipcCallbackSize  = 64 << 10
	ipcMaxHeaders    = 16 << 10
	ipcEnvelopeBytes = 4 << 10
	ipcWireFrameMax  = ipcFrameBytes + ipcMaxHeaders + ipcEnvelopeBytes
)

func TestM6IPCScenarioControl(t *testing.T) {
	if ipcWireFrameMax != 84<<10 {
		t.Fatalf("encoded-wire frame budget = %d bytes, want 84 KiB", ipcWireFrameMax)
	}
	fx := newIPCScenario(t)
	t.Cleanup(func() { _ = fx.Close(context.Background()) })
	if err := fx.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	services := core.InvocationServices{Credentials: fx.credentials, Transport: fx.transport}
	response, ge := fx.Execute(ctx, fx.request, fx.scope, services)
	if ge != nil {
		t.Fatal(ge)
	}
	defer response.Stream.Close()
	if calls := fx.Calls(); len(calls) != 1 || !bytes.Equal(calls[0].Request.Payload.Body, fx.request.Payload.Body) {
		t.Fatal("in-process control mutated unknown/binary request payload bytes")
	}
	if frame, err := response.Stream.Next(ctx); err != nil || frame.Type != core.FrameHead {
		t.Fatalf("Head = %+v, %v", frame, err)
	}
	first, err := response.Stream.Next(ctx)
	if err != nil || first.Type != core.FrameBody || !bytes.Equal(first.Body.Data, fx.chunk) {
		t.Fatalf("first chunk = %d bytes, %v", len(first.Body.Data), err)
	}
	firstPending := make(chan ipcRead, 1)
	go func() { frame, err := response.Stream.Next(ctx); firstPending <- ipcRead{frame, err} }()
	select {
	case <-fx.gateWaiting:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("producer did not reach output stall")
	}
	if _, err := services.Credentials.Get(ctx, "bearer"); err != nil {
		t.Fatalf("stalled credential callback: %v", err)
	}
	// While output is stalled, execute the reverse HTTP service with a split,
	// binary request body and consume a split response body.
	if err := fx.runCallback(ctx, services.Transport); err != nil {
		t.Fatal(err)
	}
	close(fx.gate)
	var body []byte
	body = append(body, first.Body.Data...)
	resumed := <-firstPending
	if resumed.err != nil || resumed.frame.Type != core.FrameBody {
		t.Fatalf("first post-stall frame = %+v, %v", resumed.frame, resumed.err)
	}
	body = append(body, resumed.frame.Body.Data...)
	for {
		frame, err := response.Stream.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameBody {
			body = append(body, frame.Body.Data...)
		}
	}
	if len(body) != ipcTotalBytes || !bytes.Equal(body, fx.want) {
		t.Fatalf("body bytes=%d, want exact %d-byte stream", len(body), ipcTotalBytes)
	}
	if fx.credentials.calls.Load() != 2 || fx.transport.calls.Load() != 1 {
		t.Fatalf("callbacks: credential=%d HTTP=%d, want Execute + stalled credential and HTTP callbacks", fx.credentials.calls.Load(), fx.transport.calls.Load())
	}
}

func TestM6IPCScenarioCancellationReleasesCallback(t *testing.T) {
	fx := newIPCScenario(t)
	t.Cleanup(func() { _ = fx.Close(context.Background()) })
	if err := fx.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	doer := &ipcTransport{cancelStarted: make(chan struct{})}
	services := core.InvocationServices{Credentials: fx.credentials, Transport: doer}
	response, ge := fx.Execute(ctx, fx.request, fx.scope, services)
	if ge != nil {
		t.Fatal(ge)
	}
	if frame, err := response.Stream.Next(ctx); err != nil || frame.Type != core.FrameHead {
		t.Fatalf("Head = %+v, %v", frame, err)
	}
	if frame, err := response.Stream.Next(ctx); err != nil || frame.Type != core.FrameBody {
		t.Fatalf("initial Body = %+v, %v", frame, err)
	}
	nextDone := make(chan error, 1)
	go func() { _, err := response.Stream.Next(ctx); nextDone <- err }()
	select {
	case <-fx.gateWaiting:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not reach cancellation gate")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://scenario.invalid/callback", strings.NewReader("cancel"))
	req.Header.Set("X-Scenario", strings.Repeat("h", ipcHeaderBytes))
	req.Header.Set("X-Cancel-Probe", "true")
	callbackDone := make(chan error, 1)
	go func() { _, err := doer.Do(req); callbackDone <- err }()
	select {
	case <-doer.cancelStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP callback did not start")
	}
	cancel()
	for name, done := range map[string]<-chan error{"stream Next": nextDone, "HTTP callback": callbackDone} {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("%s returned %v, want context cancellation", name, err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s retained work after cancellation", name)
		}
	}
	if err := response.Stream.Close(); err != nil {
		t.Fatal(err)
	}
	if got := fx.ActiveStreamCount(); got != 0 {
		t.Fatalf("active stream count after cancellation/Close = %d, want zero", got)
	}
}

type ipcFixture struct {
	*scripted.Connector
	request     core.ExecutionRequest
	scope       core.AttemptScope
	gate        chan struct{}
	chunk       []byte
	want        []byte
	gateWaiting chan struct{}
	credentials *ipcCredentials
	transport   *ipcTransport
}

type ipcRead struct {
	frame core.StreamFrame
	err   error
}

func newIPCScenario(t *testing.T) *ipcFixture {
	t.Helper()
	chunk := []byte("binary:")
	chunk = append(chunk, 0, 0xff)
	chunk = append(chunk, []byte(strings.Repeat("x", ipcFrameBytes-len(chunk)))...)
	want := make([]byte, 0, ipcTotalBytes)
	steps := []scripted.Step{{Frame: headFrame()}, {Frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: chunk}}}}
	want = append(want, chunk...)
	gate := make(chan struct{})
	waiting := make(chan struct{}, 1)
	for i := 1; i < ipcFrames; i++ {
		var notify chan<- struct{}
		if i == 1 {
			notify = waiting
		}
		steps = append(steps, scripted.Step{Frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: chunk}}, Gate: gate, Waiting: notify})
		want = append(want, chunk...)
	}
	steps = append(steps, scripted.Step{Frame: completeFrame()})
	base := newScriptedCustomFixture(t, steps).connector.(*scripted.Connector)
	f := &ipcFixture{Connector: base, request: core.ExecutionRequest{ID: request, Model: model, Payload: core.RawPayload{Protocol: protocol, ContentType: "application/octet-stream", Body: append([]byte("unknown-field\x00\xff"), opaqueRequest...)}}, scope: core.AttemptScope{ID: "m6-ipc-attempt", AccountID: account, Mode: "native"}, gate: gate, chunk: chunk, want: want, credentials: &ipcCredentials{}, transport: &ipcTransport{}, gateWaiting: waiting}
	return f
}

type ipcCredentials struct{ calls atomic.Int32 }

func (c *ipcCredentials) Get(ctx context.Context, name string) ([]byte, error) {
	if name != "bearer" {
		return nil, fmt.Errorf("unexpected credential name %q", name)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.calls.Add(1)
	return []byte("synthetic-only"), nil
}

type ipcTransport struct {
	calls         atomic.Int32
	cancelStarted chan struct{}
}

func (d *ipcTransport) Do(req *http.Request) (*http.Response, error) {
	if req.Header.Get("X-Cancel-Probe") == "true" {
		if d.cancelStarted != nil {
			close(d.cancelStarted)
		}
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	if req.Header.Get("X-Scenario") == "" || req.Header.Get("X-Scenario") != strings.Repeat("h", ipcHeaderBytes) {
		return nil, fmt.Errorf("scenario header payload mismatch")
	}
	requestBody, err := readInChunks(req.Body)
	if err != nil {
		return nil, err
	}
	if len(requestBody) != ipcCallbackSize || requestBody[0] != 0 || requestBody[1] != 0xff {
		return nil, fmt.Errorf("callback request body mismatch: %d bytes", len(requestBody))
	}
	d.calls.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(requestBody)), Request: req}, nil
}

func (f *ipcFixture) runCallback(ctx context.Context, doer core.HTTPDoer) error {
	data := append([]byte{0, 0xff}, bytes.Repeat([]byte("c"), ipcCallbackSize-2)...)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://scenario.invalid/callback", io.NopCloser(bytes.NewReader(data)))
	req.Header.Set("X-Scenario", strings.Repeat("h", ipcHeaderBytes))
	resp, err := doer.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	got, err := readInChunks(resp.Body)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, data) {
		return fmt.Errorf("callback response body changed")
	}
	return nil
}

func readInChunks(r io.Reader) ([]byte, error) {
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

var _ core.CredentialAccess = (*ipcCredentials)(nil)
var _ core.HTTPDoer = (*ipcTransport)(nil)
