//go:build linux

package external

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/ipc"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type responsesChildFlowCaller struct{}

func (responsesChildFlowCaller) FlowControl(context.Context, *gen.FlowControlRequest, ...grpc.CallOption) (*gen.FlowControlResponse, error) {
	return nil, nil
}

func TestM6ResponsesChildComposition(t *testing.T) {
	if os.Getenv(connectorSocketEnv) != "" {
		return
	}
	flow, err := NewChildFlowControl(27, responsesChildFlowCaller{})
	if err != nil {
		t.Fatal(err)
	}
	child, err := NewResponsesChild(gen.NewHostServicesClient(nil), flow, 27)
	if err != nil {
		t.Fatal(err)
	}
	inProcess := child.connector.Descriptor()
	external := child.connector.Descriptor()
	if inProcess.ID != external.ID || inProcess.Kind != external.Kind || inProcess.ImplementationVersion != external.ImplementationVersion || len(inProcess.APIVersions) != len(external.APIVersions) {
		t.Fatalf("child descriptor diverged from native connector: %#v %#v", inProcess, external)
	}
	if got := child.connector.Health(context.Background()).State; got != core.HealthUnknown {
		t.Fatalf("uninitialized child health=%q", got)
	}
	if got := child.connector.Capabilities(context.Background(), core.CapabilityScope{}).Values; len(got) != 0 {
		t.Fatalf("uninitialized child claimed capabilities: %#v", got)
	}
}

// TestM6ResponsesChildProcess is the actual executable entry used by Process;
// the parent injects only the two binding socket paths.
func TestM6ResponsesChildProcess(t *testing.T) {
	if os.Getenv(connectorSocketEnv) == "" {
		return
	}
	if len(os.Environ()) != 2 {
		t.Fatalf("child inherited environment: %d entries", len(os.Environ()))
	}
	if err := RunNativeResponsesChild(context.Background()); err != nil {
		t.Errorf("child runner: %v", err)
	}
}

type responsesChildCredentials struct {
	mu      sync.Mutex
	gets    []string
	account string
}

func (c *responsesChildCredentials) Get(_ context.Context, name string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets = append(c.gets, name)
	if name != "bearer" || c.account != "fixture-account" {
		return nil, errors.New("credential denied")
	}
	return []byte("fixture-token"), nil
}

func responsesChildProcessConfig(t *testing.T) ProcessConfig {
	return processTestConfig(t, "TestM6ResponsesChildProcess")
}

func processTestConfig(t *testing.T, entry string) ProcessConfig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return ProcessConfig{Executable: exe, Args: []string{"-test.run=^" + entry + "$"}, WorkingDirectory: filepath.Dir(exe)}
}

func startResponsesChild(t *testing.T) (*Process, *Connector) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := startWithTimeout(ctx, responsesChildProcessConfig(t), startupTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(p.dir); err != nil || st.Mode().Perm() != 0700 {
		t.Fatalf("child IPC directory mode: %v %v", st, err)
	}
	c, err := p.ConnectComponent(ctx)
	if err != nil {
		_ = p.Shutdown(context.Background(), nil)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = c.Close(closeCtx)
		_ = p.Shutdown(context.Background(), nil)
	})
	return p, c
}

func nativeResponsesConfig(t *testing.T, endpoint string) core.ComponentConfig {
	t.Helper()
	v := map[string]any{"transport": map[string]any{"Endpoint": endpoint, "ConnectTimeout": int64(time.Second), "TLSHandshakeTimeout": int64(time.Second), "ResponseHeaderTimeout": int64(5 * time.Second), "StreamIdleTimeout": int64(5 * time.Second)}, "model": "fixture-model", "account_id": "fixture-account"}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return core.ComponentConfig{Data: data}
}

func testHTTPServices() core.InvocationServices {
	return core.InvocationServices{Credentials: &responsesChildCredentials{account: "fixture-account"}, Transport: &http.Client{Transport: &http.Transport{DisableCompression: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type collectedResponses struct {
	head     core.HeadFrame
	body     []byte
	complete core.CompleteFrame
}

func collectResponses(t *testing.T, stream core.Stream) collectedResponses {
	t.Helper()
	defer stream.Close()
	var got collectedResponses
	for {
		frame, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			t.Fatal(err)
		}
		switch frame.Type {
		case core.FrameHead:
			got.head = *frame.Head
		case core.FrameBody:
			got.body = append(got.body, frame.Body.Data...)
		case core.FrameComplete:
			got.complete = *frame.Complete
		default:
			t.Fatalf("unexpected frame %q", frame.Type)
		}
	}
}

func equalResponsesModels(a, b core.ModelsResult) bool {
	if a.Supported != b.Supported || len(a.Models) != len(b.Models) {
		return false
	}
	for i := range a.Models {
		x, y := a.Models[i], b.Models[i]
		if x.ID != y.ID || !maps.Equal(x.Capabilities, y.Capabilities) {
			return false
		}
		if x.Available == nil || y.Available == nil {
			if x.Available != nil || y.Available != nil {
				return false
			}
		} else if *x.Available != *y.Available {
			return false
		}
	}
	return true
}

func TestM6ResponsesChildOperationsAndFixedParity(t *testing.T) {
	body := []byte(`{"id":"resp-1","status":"completed","output":[{"type":"message","text":"opaque","unknown":{"bytes":"\u0000\ufffd"}}]}`)
	newUpstream := func() *fakeupstream.Server {
		return fakeupstream.New(fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"application/json"}, "X-Opaque": {"one", "two"}}, Body: body})
	}
	inUp, childUp := newUpstream(), newUpstream()
	defer inUp.Close()
	defer childUp.Close()
	inproc := responses.NewConnector()
	if err := inproc.Init(context.Background(), nativeResponsesConfig(t, inUp.URL+"/v1/responses")); err != nil {
		t.Fatal(err)
	}
	defer inproc.Close(context.Background())
	_, child := startResponsesChild(t)
	config := nativeResponsesConfig(t, childUp.URL+"/v1/responses")
	if err := child.Init(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inproc.Descriptor(), child.Descriptor()) {
		t.Fatal("descriptor differs across composition")
	}
	if got, want := child.Health(context.Background()), inproc.Health(context.Background()); got.State != want.State {
		t.Fatalf("health child=%+v native=%+v", got, want)
	}
	scope := core.CapabilityScope{Protocol: "openai.responses.v1", Mode: core.ModeNative, Model: "fixture-model", AccountID: "fixture-account"}
	if got, want := child.Capabilities(context.Background(), scope), inproc.Capabilities(context.Background(), scope); !maps.Equal(got.Values, want.Values) {
		t.Fatalf("capabilities child=%+v native=%+v", got, want)
	}
	modelQuery := core.ModelQuery{Protocol: scope.Protocol, Mode: scope.Mode, AccountID: scope.AccountID}
	childModels, childModelErr := child.Models(context.Background(), modelQuery, core.InvocationServices{})
	nativeModels, nativeModelErr := inproc.Models(context.Background(), modelQuery, core.InvocationServices{})
	if childModelErr != nil || nativeModelErr != nil || !equalResponsesModels(childModels, nativeModels) {
		t.Fatalf("models child=%+v/%v native=%+v/%v", childModels, childModelErr, nativeModels, nativeModelErr)
	}
	usageQuery := core.UsageQuery{Protocol: scope.Protocol, Mode: scope.Mode, AccountID: scope.AccountID, MaxBodyBytes: 1024, Payload: core.RawPayload{Protocol: scope.Protocol, Body: []byte{0, 255}}}
	childUsage, childUsageErr := child.EstimateUsage(context.Background(), usageQuery, core.InvocationServices{})
	nativeUsage, nativeUsageErr := inproc.EstimateUsage(context.Background(), usageQuery, core.InvocationServices{})
	if childUsageErr != nil || nativeUsageErr != nil || !reflect.DeepEqual(childUsage, nativeUsage) {
		t.Fatalf("estimate child=%+v/%v native=%+v/%v", childUsage, childUsageErr, nativeUsage, nativeUsageErr)
	}
	auth := core.AuthRequest{AccountID: scope.AccountID, Action: "start", State: []byte{0, 255}}
	childAuth, childAuthErr := child.Authenticate(context.Background(), auth, core.InvocationServices{})
	nativeAuth, nativeAuthErr := inproc.Authenticate(context.Background(), auth, core.InvocationServices{})
	if childAuthErr != nil || nativeAuthErr != nil || !reflect.DeepEqual(childAuth, nativeAuth) {
		t.Fatalf("auth child=%+v/%v native=%+v/%v", childAuth, childAuthErr, nativeAuth, nativeAuthErr)
	}
	requestBody := []byte{'{', '"', 'x', '"', ':', '[', 0, 255, ']', '}'}
	request := core.ExecutionRequest{ID: "r", Model: "fixture-model", Payload: core.RawPayload{Protocol: scope.Protocol, ContentType: "application/json", Body: requestBody}}
	inResponse, inErr := inproc.Execute(context.Background(), request, core.AttemptScope{ID: "a", AccountID: scope.AccountID, Mode: scope.Mode, MaxBodyBytes: 1024}, testHTTPServices())
	if inErr != nil {
		t.Fatal(inErr)
	}
	childCredentials := &responsesChildCredentials{account: scope.AccountID}
	services := testHTTPServices()
	services.Credentials = childCredentials
	extResponse, extErr := child.Execute(context.Background(), request, core.AttemptScope{ID: "a", AccountID: scope.AccountID, Mode: scope.Mode, MaxBodyBytes: 1024}, services)
	if extErr != nil {
		t.Fatal(extErr)
	}
	inCollected := collectResponses(t, inResponse.Stream)
	extCollected := collectResponses(t, extResponse.Stream)
	if !reflect.DeepEqual(inCollected.head, extCollected.head) || !bytes.Equal(inCollected.body, extCollected.body) || !reflect.DeepEqual(inCollected.complete, extCollected.complete) {
		t.Fatalf("native/external mismatch\n native=%+v\n external=%+v", inCollected, extCollected)
	}
	for _, up := range []*fakeupstream.Server{inUp, childUp} {
		select {
		case captured := <-up.Requests:
			if !bytes.Equal(captured.Body, requestBody) {
				t.Fatalf("upstream request bytes changed: %x", captured.Body)
			}
			if captured.Header.Get("Authorization") != "Bearer fixture-token" {
				t.Fatalf("credential not scoped/applied: %q", captured.Header.Get("Authorization"))
			}
		default:
			t.Fatal("upstream request not captured")
		}
	}
	if len(childCredentials.gets) != 1 || childCredentials.gets[0] != "bearer" {
		t.Fatalf("reverse credential lookups: %#v", childCredentials.gets)
	}
}

func TestM6ResponsesChildSSEEarlyDeliveryAndCancellation(t *testing.T) {
	firstSent := make(chan struct{})
	terminalGate := make(chan struct{})
	first := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"part\"}\n\n")
	terminal := []byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	up := fakeupstream.New(fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: []fakeupstream.Step{{Data: first, Sent: firstSent}, {Data: terminal, Gate: terminalGate}}})
	defer up.Close()
	_, child := startResponsesChild(t)
	if err := child.Init(context.Background(), nativeResponsesConfig(t, up.URL+"/v1/responses")); err != nil {
		t.Fatal(err)
	}
	streaming := true
	req := core.ExecutionRequest{Model: "fixture-model", Metadata: core.RequestMetadata{Streaming: &streaming}, Payload: core.RawPayload{Protocol: "openai.responses.v1", Body: []byte(`{"model":"fixture-model"}`)}}
	creds := &responsesChildCredentials{account: "fixture-account"}
	services := testHTTPServices()
	services.Credentials = creds
	response, ge := child.Execute(context.Background(), req, core.AttemptScope{ID: "stream", AccountID: "fixture-account", Mode: core.ModeNative, MaxBodyBytes: 1024}, services)
	if ge != nil {
		t.Fatal(ge)
	}
	defer response.Stream.Close()
	head, err := response.Stream.Next(context.Background())
	if err != nil || head.Type != core.FrameHead {
		t.Fatalf("Head: %v %+v", err, head)
	}
	select {
	case <-firstSent:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not flush first partial event")
	}
	partial, err := response.Stream.Next(context.Background())
	if err != nil || partial.Type != core.FrameBody || !bytes.Contains(partial.Body.Data, []byte("part")) {
		t.Fatalf("partial output unavailable before terminal event: %v %+v", err, partial)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = response.Stream.Next(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stream cancellation=%v", err)
	}
	var captured fakeupstream.Request
	select {
	case captured = <-up.Requests:
	case <-time.After(time.Second):
		t.Fatal("host HTTPDo did not reach upstream")
	}
	select {
	case <-captured.Cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not abort the reverse HTTPDo request")
	}
	child.invocations.mu.Lock()
	active := len(child.invocations.active)
	child.invocations.mu.Unlock()
	if active != 0 {
		t.Fatal("cancelled invocation retained host callback authority")
	}
	frames, queued := child.invocations.credits.usage()
	if frames != 0 || queued != 0 {
		t.Fatalf("cancelled invocation retained credits: frames=%d bytes=%d", frames, queued)
	}
	close(terminalGate)
}

func TestM6ResponsesChildSSEParity(t *testing.T) {
	steps := []fakeupstream.Step{{Data: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"one\"}\r\n\r\n")}, {Data: []byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"unknown\":\"preserved\"}}\r\n\r\n")}}
	makeServer := func() *fakeupstream.Server {
		return fakeupstream.New(fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Fixture": {"a", "b"}}, Steps: steps})
	}
	nativeUp, childUp := makeServer(), makeServer()
	defer nativeUp.Close()
	defer childUp.Close()
	inproc := responses.NewConnector()
	if err := inproc.Init(context.Background(), nativeResponsesConfig(t, nativeUp.URL+"/v1/responses")); err != nil {
		t.Fatal(err)
	}
	defer inproc.Close(context.Background())
	p, child := startResponsesChild(t)
	if err := child.Init(context.Background(), nativeResponsesConfig(t, childUp.URL+"/v1/responses")); err != nil {
		t.Fatal(err)
	}
	streaming := true
	request := core.ExecutionRequest{ID: "sse", Model: "fixture-model", Metadata: core.RequestMetadata{Streaming: &streaming}, Payload: core.RawPayload{Protocol: "openai.responses.v1", Body: []byte(`{"unknown":true}`)}}
	scope := core.AttemptScope{ID: "sse-attempt", AccountID: "fixture-account", Mode: core.ModeNative, MaxBodyBytes: 1024}
	nativeResponse, nativeErr := inproc.Execute(context.Background(), request, scope, testHTTPServices())
	if nativeErr != nil {
		t.Fatal(nativeErr)
	}
	services := testHTTPServices()
	services.Credentials = &responsesChildCredentials{account: "fixture-account"}
	externalResponse, externalErr := child.Execute(context.Background(), request, scope, services)
	if externalErr != nil {
		t.Fatal(externalErr)
	}
	native := collectResponses(t, nativeResponse.Stream)
	var external collectedResponses
	for {
		f, e := externalResponse.Stream.Next(context.Background())
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			t.Fatalf("external SSE Next: %v child diagnostics=%q", e, p.Diagnostics())
		}
		switch f.Type {
		case core.FrameHead:
			external.head = *f.Head
		case core.FrameBody:
			external.body = append(external.body, f.Body.Data...)
		case core.FrameComplete:
			external.complete = *f.Complete
		}
	}
	if !reflect.DeepEqual(native.head, external.head) || !bytes.Equal(native.body, external.body) || !reflect.DeepEqual(native.complete, external.complete) {
		t.Fatalf("SSE native/external mismatch\n native=%+v\n external=%+v", native, external)
	}
}

func TestM6ResponsesChildErrorMappingParity(t *testing.T) {
	makeServer := func() *fakeupstream.Server {
		return fakeupstream.New(fakeupstream.Response{Status: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"error":{"message":"sensitive backend detail"}}`)})
	}
	nativeUp, childUp := makeServer(), makeServer()
	defer nativeUp.Close()
	defer childUp.Close()
	inproc := responses.NewConnector()
	if err := inproc.Init(context.Background(), nativeResponsesConfig(t, nativeUp.URL+"/v1/responses")); err != nil {
		t.Fatal(err)
	}
	defer inproc.Close(context.Background())
	_, child := startResponsesChild(t)
	if err := child.Init(context.Background(), nativeResponsesConfig(t, childUp.URL+"/v1/responses")); err != nil {
		t.Fatal(err)
	}
	req := core.ExecutionRequest{Model: "fixture-model", Payload: core.RawPayload{Protocol: "openai.responses.v1", Body: []byte(`{}`)}}
	scope := core.AttemptScope{ID: "error", AccountID: "fixture-account", Mode: core.ModeNative, MaxBodyBytes: 10}
	native, nativeErr := inproc.Execute(context.Background(), req, scope, testHTTPServices())
	if nativeErr != nil {
		t.Fatal(nativeErr)
	}
	services := testHTTPServices()
	services.Credentials = &responsesChildCredentials{account: "fixture-account"}
	external, externalErr := child.Execute(context.Background(), req, scope, services)
	if externalErr != nil {
		t.Fatal(externalErr)
	}
	a, b := collectResponses(t, native.Stream), collectResponses(t, external.Stream)
	if !reflect.DeepEqual(a.head, b.head) || !bytes.Equal(a.body, b.body) || !reflect.DeepEqual(a.complete, b.complete) {
		t.Fatalf("error mapping differs\n native=%+v\n external=%+v", a, b)
	}
}

func TestM6ResponsesChildCancelBeforeHeadReleasesCallback(t *testing.T) {
	headerGate := make(chan struct{})
	up := fakeupstream.New(fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"status":"completed"}`), HeaderGate: headerGate})
	defer up.Close()
	_, child := startResponsesChild(t)
	if err := child.Init(context.Background(), nativeResponsesConfig(t, up.URL+"/v1/responses")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *core.GatewayError, 1)
	creds := &responsesChildCredentials{account: "fixture-account"}
	services := testHTTPServices()
	services.Credentials = creds
	go func() {
		_, ge := child.Execute(ctx, core.ExecutionRequest{Model: "fixture-model", Payload: core.RawPayload{Protocol: "openai.responses.v1", Body: []byte(`{}`)}}, core.AttemptScope{ID: "pre-head", AccountID: "fixture-account", Mode: core.ModeNative, MaxBodyBytes: 1024}, services)
		done <- ge
	}()
	var captured fakeupstream.Request
	select {
	case captured = <-up.Requests:
	case <-time.After(2 * time.Second):
		t.Fatal("reverse HTTP callback did not reach upstream")
	}
	cancel()
	select {
	case <-captured.Cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("pre-Head cancellation did not close host HTTP body")
	}
	select {
	case ge := <-done:
		if ge == nil || ge.Category != core.CategoryCancelled {
			t.Fatalf("cancel before Head error=%+v", ge)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("external call did not unwind")
	}
	child.invocations.mu.Lock()
	active := len(child.invocations.active)
	child.invocations.mu.Unlock()
	frames, queued := child.invocations.credits.usage()
	if active != 0 || frames != 0 || queued != 0 {
		t.Fatalf("callback/credit leak: active=%d frames=%d bytes=%d", active, frames, queued)
	}
}

func TestM6ResponsesChildCloseRemovesOwnedIPC(t *testing.T) {
	p, child := startResponsesChild(t)
	if err := child.Init(context.Background(), nativeResponsesConfig(t, "http://127.0.0.1:1/v1/responses")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := child.Close(ctx); err != nil {
		t.Fatalf("orderly child close: %v; diagnostics=%q", err, p.Diagnostics())
	}
	for _, path := range []string{p.connector, p.hostServices, p.dir} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned IPC path remains %s: %v", path, err)
		}
	}
}

type expiryResponsesConnector struct {
	core.Connector
	result core.AuthResult
}

func (c *expiryResponsesConnector) Authenticate(context.Context, core.AuthRequest, core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	return c.result, nil
}

func TestM6ResponsesChildAuthenticatePreservesExpiry(t *testing.T) {
	expiry := time.Date(2031, 2, 3, 4, 5, 6, 789, time.UTC)
	want := core.AuthResult{Supported: true, State: string([]byte{0, 255, 's'}), NextAction: "continue", Credentials: map[string][]byte{"bearer": {0, 255, 'c'}}, CredentialExpiresAt: &expiry}
	flow, err := NewChildFlowControl(51, responsesChildFlowCaller{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewConnectorChild(&expiryResponsesConnector{result: want}, gen.NewHostServicesClient(nil), flow, 51)
	if err != nil {
		t.Fatal(err)
	}
	defer service.stopLogger()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	gen.RegisterConnectorServer(server, service)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///responses-child-test", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := gen.NewConnectorClient(conn).Authenticate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request := ipc.AuthRequestTo(core.AuthRequest{AccountID: "account", Action: "continue", State: []byte{0, 255, 'r'}})
	if err = ipc.WriteRecord(ipc.Identity{Epoch: 51, CallID: 1, RecordID: 0, Type: "AuthRequest"}, request, func(f *gen.RecordFragment) error { f.InvocationId = "inv-1"; return stream.Send(f) }); err != nil {
		t.Fatal(err)
	}
	if err = stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	assembler := ipc.NewAssembler(ipc.Identity{Epoch: 51, CallID: 1, RecordID: 0, Type: "AuthResult"})
	for !assembler.Complete() {
		f, e := stream.Recv()
		if e != nil {
			t.Fatal(e)
		}
		if f.InvocationId != "inv-1" {
			t.Fatalf("response invocation id=%q", f.InvocationId)
		}
		if e = assembler.Add(f); e != nil {
			t.Fatal(e)
		}
	}
	if _, err = stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected extra authentication result: %v", err)
	}
	var wire gen.AuthResult
	if err = assembler.Decode(&wire); err != nil {
		t.Fatal(err)
	}
	got, err := ipc.AuthResultFrom(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("auth result changed across child bridge: got=%+v want=%+v", got, want)
	}
}

type childQueueProbeConnector struct{}

func (*childQueueProbeConnector) Descriptor() core.Descriptor {
	return core.Descriptor{ID: "fixture.log-queue", Kind: core.ComponentConnector, ImplementationVersion: "1", APIVersions: []core.APIVersion{{Major: 1}}, Protocols: []string{"openai.responses.v1"}, ConnectorType: "fixture"}
}
func (*childQueueProbeConnector) Init(context.Context, core.ComponentConfig) error { return nil }
func (*childQueueProbeConnector) Health(context.Context) core.Health {
	return core.Health{State: core.HealthReady}
}
func (*childQueueProbeConnector) Capabilities(context.Context, core.CapabilityScope) core.CapabilityResult {
	return core.CapabilityResult{Values: map[core.Capability]core.CapabilityState{}}
}
func (*childQueueProbeConnector) Close(context.Context) error { return nil }
func (*childQueueProbeConnector) Models(context.Context, core.ModelQuery, core.InvocationServices) (core.ModelsResult, *core.GatewayError) {
	return core.ModelsResult{}, nil
}
func (*childQueueProbeConnector) EstimateUsage(context.Context, core.UsageQuery, core.InvocationServices) (core.EstimateResult, *core.GatewayError) {
	return core.EstimateResult{}, nil
}
func (*childQueueProbeConnector) Authenticate(context.Context, core.AuthRequest, core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	return core.AuthResult{}, nil
}
func (*childQueueProbeConnector) Execute(_ context.Context, _ core.ExecutionRequest, _ core.AttemptScope, services core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	handler, ok := services.Logger.Handler().(*childLogHandler)
	if !ok {
		return core.ExecutionResponse{}, &core.GatewayError{Code: "fixture_logger_missing", Category: core.CategoryInternal}
	}
	maxQueued := 0
	for i := range 4096 {
		services.Logger.Info("queue-probe", "index", i)
		if n := len(handler.dispatcher.queue); n > maxQueued {
			maxQueued = n
		}
	}
	status := 200
	body := []byte(strconv.Itoa(maxQueued))
	return core.ExecutionResponse{Stream: &childQueueProbeStream{frames: []core.StreamFrame{{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: "openai.responses.v1", ContentType: "text/plain", HTTPStatus: &status}}, {Type: core.FrameBody, Body: &core.BodyFrame{Data: body}}, {Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded}}}}}, nil
}

type childQueueProbeStream struct{ frames []core.StreamFrame }

func (s *childQueueProbeStream) Next(context.Context) (core.StreamFrame, error) {
	if len(s.frames) == 0 {
		return core.StreamFrame{}, io.EOF
	}
	f := s.frames[0]
	s.frames = s.frames[1:]
	return f, nil
}
func (*childQueueProbeStream) Close() error { return nil }

func TestM6ResponsesChildLoggingProcess(t *testing.T) {
	if os.Getenv(connectorSocketEnv) == "" {
		return
	}
	if len(os.Environ()) != 2 {
		t.Fatalf("logging child inherited environment: %d entries", len(os.Environ()))
	}
	if err := RunConnectorChild(context.Background(), &childQueueProbeConnector{}); err != nil {
		t.Errorf("logging child runner: %v", err)
	}
}

type blockedSinkHandler struct {
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (h *blockedSinkHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *blockedSinkHandler) Handle(context.Context, slog.Record) error {
	h.once.Do(func() { close(h.entered) })
	<-h.release
	return nil
}
func (h *blockedSinkHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *blockedSinkHandler) WithGroup(string) slog.Handler      { return h }

func TestM6ResponsesChildStalledHostLoggingDoesNotBlockStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := startWithTimeout(ctx, processTestConfig(t, "TestM6ResponsesChildLoggingProcess"), startupTimeout)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	sink := &blockedSinkHandler{entered: make(chan struct{}), release: release}
	old := slog.Default()
	slog.SetDefault(slog.New(sink))
	child, err := p.ConnectComponent(ctx)
	slog.SetDefault(old)
	if err != nil {
		_ = p.Shutdown(context.Background(), nil)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(release)
		closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = child.Close(closeCtx)
		_ = p.Shutdown(context.Background(), nil)
	})
	if err := child.Init(ctx, core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	response, ge := child.Execute(ctx, core.ExecutionRequest{Model: "fixture", Payload: core.RawPayload{Protocol: "openai.responses.v1"}}, core.AttemptScope{ID: "log-stall", AccountID: "fixture-account", Mode: core.ModeNative}, core.InvocationServices{})
	if ge != nil {
		t.Fatal(ge)
	}
	defer response.Stream.Close()
	select {
	case <-sink.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("host logging sink was not stalled")
	}
	head, err := response.Stream.Next(ctx)
	if err != nil || head.Type != core.FrameHead {
		t.Fatalf("stream Head blocked by logging: %v %+v", err, head)
	}
	frame, err := response.Stream.Next(ctx)
	if err != nil || frame.Type != core.FrameBody {
		t.Fatalf("stream Body blocked by logging: %v %+v", err, frame)
	}
	maxQueued, err := strconv.Atoi(string(frame.Body.Data))
	if err != nil || maxQueued == 0 || maxQueued > maxLogQueue {
		t.Fatalf("child process log queue bound=%d err=%v max=%d", maxQueued, err, maxLogQueue)
	}
	frame, err = response.Stream.Next(ctx)
	if err != nil || frame.Type != core.FrameComplete {
		t.Fatalf("stream Complete blocked by logging: %v %+v", err, frame)
	}
	if _, err = response.Stream.Next(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("stream EOF=%v", err)
	}
}
