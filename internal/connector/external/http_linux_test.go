//go:build linux

package external

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/ipc"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type httpDoerFunc func(*http.Request) (*http.Response, error)

func (f httpDoerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

type malformedResponseHost struct {
	gen.UnimplementedHostServicesServer
	registry *invocationRegistry
	body     *blockingHTTPBody
}

func (s *malformedResponseHost) HTTPDo(stream grpc.BidiStreamingServer[gen.HTTPFrame, gen.HTTPFrame]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	lease, _, _, err := s.registry.admitCallback(first.Epoch, first.InvocationId, first.CallId)
	if err != nil {
		return err
	}
	defer lease.release()
	for {
		_, err = stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	meta := &gen.HTTPResponse{StatusCode: 200, Headers: map[string]*gen.StringList{}}
	if err := ipc.WriteRecord(ipc.Identity{Epoch: first.Epoch, CallID: first.CallId, RecordID: 0, Type: "HTTPResponse"}, meta, func(f *gen.RecordFragment) error {
		return stream.Send(httpFrameRecord(first.Epoch, first.CallId, first.InvocationId, f))
	}); err != nil {
		return err
	}
	chunk := &gen.BodyChunk{Data: make([]byte, ipc.MaxFragmentBytes+1)}
	if err := stream.Send(&gen.HTTPFrame{Epoch: first.Epoch, CallId: first.CallId, InvocationId: first.InvocationId, Payload: &gen.HTTPFrame_Body{Body: chunk}}); err != nil {
		return err
	}
	<-stream.Context().Done()
	_ = s.body.Close()
	return nil
}

func TestM6HTTPRoundTripStreamsOpaqueBodiesAndHeaders(t *testing.T) {
	requestBody := bytes.Repeat([]byte{0, 0xff, 'x'}, 40<<10)
	responseBody := bytes.Repeat([]byte{0xff, 0, 'r'}, 40<<10)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PATCH" || r.URL.String() != "/path?q=1" || r.Host != "host.example" || r.Header.Get("X-Test") != "exact" {
			t.Errorf("loopback request metadata lost: %#v", r)
		}
		if r.Header.Get("Connection") != "" || r.Header.Get("X-Hop") != "" {
			t.Errorf("hop-by-hop header forwarded: %#v", r.Header)
		}
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read loopback request body: %v", err)
		}
		if !bytes.Equal(got, requestBody) {
			t.Errorf("loopback request body changed: %d bytes", len(got))
		}
		w.Header().Set("X-Reply", "preserved")
		w.WriteHeader(207)
		_, _ = w.Write(responseBody)
	}))
	defer backend.Close()
	registry := newInvocationRegistry(31)
	defer registry.close()
	id, _, err := registry.beginWithServices(context.Background(), "selected-account", nil, &http.Client{Transport: &http.Transport{Proxy: nil}})
	if err != nil {
		t.Fatal(err)
	}
	client := HTTPDoerClient{client: credentialTestServer(t, registry), epoch: 31, invocationID: id}
	req, _ := http.NewRequest("PATCH", backend.URL+"/path?q=1", bytes.NewReader(requestBody))
	req.Host = "host.example"
	req.Header.Set("X-Test", "exact")
	req.Header.Set("Connection", "X-Hop")
	req.Header.Set("X-Hop", "remove")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 207 || resp.Header.Get("X-Reply") != "preserved" {
		t.Fatalf("response metadata: %#v", resp)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, responseBody) {
		t.Fatalf("response body changed: %d bytes", len(got))
	}
}

type blockingHTTPBody struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingHTTPBody() *blockingHTTPBody         { return &blockingHTTPBody{closed: make(chan struct{})} }
func (b *blockingHTTPBody) Read([]byte) (int, error) { <-b.closed; return 0, errors.New("closed") }
func (b *blockingHTTPBody) Close() error             { b.once.Do(func() { close(b.closed) }); return nil }

type stagedHTTPBody struct {
	first     []byte
	release   chan struct{}
	closed    chan struct{}
	sent      bool
	closeOnce sync.Once
}

func newStagedHTTPBody(first string) *stagedHTTPBody {
	return &stagedHTTPBody{first: []byte(first), release: make(chan struct{}), closed: make(chan struct{})}
}
func (b *stagedHTTPBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, b.first), nil
	}
	select {
	case <-b.release:
		return 0, io.EOF
	case <-b.closed:
		return 0, io.EOF
	}
}
func (b *stagedHTTPBody) Close() error  { b.closeOnce.Do(func() { close(b.closed) }); return nil }
func (b *stagedHTTPBody) releaseLater() { close(b.release) }

func TestM6HTTPCancellationReleasesCallback(t *testing.T) {
	registry := newInvocationRegistry(32)
	defer registry.close()
	id, _, err := registry.beginWithServices(context.Background(), "acct", nil, httpDoerFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() }))
	if err != nil {
		t.Fatal(err)
	}
	client := HTTPDoerClient{client: credentialTestServer(t, registry), epoch: 32, invocationID: id}
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://example.invalid", nil)
	done := make(chan error, 1)
	go func() { _, e := client.Do(req); done <- e }()
	deadline := time.Now().Add(time.Second)
	for {
		registry.mu.Lock()
		active := registry.outstanding
		registry.mu.Unlock()
		if active > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("callback was not admitted")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP call did not cancel promptly")
	}
	deadline = time.Now().Add(time.Second)
	for {
		registry.mu.Lock()
		n := registry.outstanding
		registry.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("callback lease not released")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestM6HTTPResponseBodyCloseCancelsHostStream(t *testing.T) {
	registry := newInvocationRegistry(35)
	defer registry.close()
	body := newBlockingHTTPBody()
	id, _, err := registry.beginWithServices(context.Background(), "acct", nil, httpDoerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	client := HTTPDoerClient{client: credentialTestServer(t, registry), epoch: 35, invocationID: id}
	req, _ := http.NewRequest("GET", "http://example.invalid", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		registry.mu.Lock()
		n := registry.outstanding
		registry.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("response close did not release callback")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("response close did not close host body")
	}
}

func TestM6HTTPEarlyResponseStopsUnboundedUpload(t *testing.T) {
	registry := newInvocationRegistry(36)
	defer registry.close()
	body := newBlockingHTTPBody()
	id, _, err := registry.beginWithServices(context.Background(), "acct", nil, httpDoerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 413, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("rejected"))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	client := HTTPDoerClient{client: credentialTestServer(t, registry), epoch: 36, invocationID: id}
	req, _ := http.NewRequest("POST", "http://example.invalid", body)
	type doResult struct {
		response *http.Response
		err      error
	}
	done := make(chan doResult, 1)
	go func() { resp, e := client.Do(req); done <- doResult{resp, e} }()
	var result doResult
	select {
	case result = <-done:
	case <-time.After(time.Second):
		_ = body.Close()
		<-done
		t.Fatal("early response waited for the unbounded upload")
	}
	resp, err := result.response, result.err
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 413 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil || string(got) != "rejected" {
		t.Fatalf("early response body %q: %v", got, err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("early response did not stop request-body reader")
	}
	_ = resp.Body.Close()
}

func TestM6HTTPLoopbackRejectionStopsUnboundedUpload(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Errorf("enable full duplex: %v", err)
			return
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "rejected")
	}))
	defer backend.Close()
	registry := newInvocationRegistry(39)
	defer registry.close()
	body := newBlockingHTTPBody()
	id, _, err := registry.beginWithServices(context.Background(), "acct", nil, &http.Client{Transport: &http.Transport{Proxy: nil}})
	if err != nil {
		t.Fatal(err)
	}
	client := HTTPDoerClient{client: credentialTestServer(t, registry), epoch: 39, invocationID: id}
	req, _ := http.NewRequest("POST", backend.URL, body)
	type result struct {
		response *http.Response
		err      error
	}
	done := make(chan result, 1)
	go func() { resp, e := client.Do(req); done <- result{resp, e} }()
	var got result
	select {
	case got = <-done:
	case <-time.After(time.Second):
		_ = body.Close()
		<-done
		t.Fatal("loopback rejection waited for upload EOF")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", got.response.StatusCode)
	}
	response, err := io.ReadAll(got.response.Body)
	if err != nil || string(response) != "rejected" {
		t.Fatalf("response %q: %v", response, err)
	}
	_ = got.response.Body.Close()
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("loopback rejection did not stop request reader")
	}
}

func TestM6HTTPTruncatedRequestWithoutCompleteIsRejected(t *testing.T) {
	registry := newInvocationRegistry(40)
	defer registry.close()
	observed := make(chan error, 1)
	id, _, err := registry.beginWithServices(context.Background(), "acct", nil, httpDoerFunc(func(r *http.Request) (*http.Response, error) {
		_, e := io.ReadAll(r.Body)
		observed <- e
		return nil, e
	}))
	if err != nil {
		t.Fatal(err)
	}
	client := credentialTestServer(t, registry)
	stream, err := client.HTTPDo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", "http://example.invalid", nil)
	metadata, err := ipc.HTTPRequestTo(req)
	if err != nil {
		t.Fatal(err)
	}
	const callID = 1000
	if err = ipc.WriteRecord(ipc.Identity{Epoch: 40, CallID: callID, RecordID: 0, Type: "HTTPRequest"}, metadata, func(f *gen.RecordFragment) error { return stream.Send(httpFrameRecord(40, callID, id, f)) }); err != nil {
		t.Fatal(err)
	}
	chunk, _ := ipc.BodyChunkTo([]byte("partial"))
	if err = stream.Send(&gen.HTTPFrame{Epoch: 40, CallId: callID, InvocationId: id, Payload: &gen.HTTPFrame_Body{Body: chunk}}); err != nil {
		t.Fatal(err)
	}
	if err = stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); err == nil {
		t.Fatal("truncated HTTP request returned a response")
	}
	select {
	case err := <-observed:
		if !errors.Is(err, ipc.ErrMalformed) {
			t.Fatalf("body error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not reject truncated request promptly")
	}
}

func TestM6HTTPResponseHopByHopHeadersAreRemoved(t *testing.T) {
	registry := newInvocationRegistry(41)
	defer registry.close()
	id, _, err := registry.beginWithServices(context.Background(), "acct", nil, httpDoerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"X-Visible": {"yes"}, "Connection": {"X-Private"}, "X-Private": {"hidden"}, "Keep-Alive": {"timeout=5"}, "Proxy-Authenticate": {"Basic realm=\"x\""}, "Proxy-Authorization": {"secret"}, "Proxy-Connection": {"keep-alive"}, "TE": {"trailers"}, "Trailer": {"X-Trailer"}, "Transfer-Encoding": {"chunked"}, "Upgrade": {"websocket"}}, Body: io.NopCloser(strings.NewReader("body"))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	client := HTTPDoerClient{client: credentialTestServer(t, registry), epoch: 41, invocationID: id}
	req, _ := http.NewRequest("GET", "http://example.invalid", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Visible") != "yes" {
		t.Fatalf("visible header lost: %#v", resp.Header)
	}
	for _, name := range []string{"Connection", "X-Private", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		if resp.Header.Get(name) != "" {
			t.Errorf("hop-by-hop response header %s leaked", name)
		}
	}
}

func TestM6HTTPResponseReadErrorCancelsAndReleasesCallback(t *testing.T) {
	registry := newInvocationRegistry(42)
	defer registry.close()
	id, _, err := registry.begin(context.Background(), "acct")
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	body := newBlockingHTTPBody()
	gen.RegisterHostServicesServer(server, &malformedResponseHost{registry: registry, body: body})
	go func() { _ = server.Serve(listener) }()
	conn, err := grpc.NewClient("passthrough:///malformed-http-response", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); server.Stop(); _ = listener.Close() })
	client := NewHTTPDoerClient(gen.NewHostServicesClient(conn), 42, id)
	req, _ := http.NewRequest("GET", "http://example.invalid", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resp.Body.Read(make([]byte, 8)); !errors.Is(err, ipc.ErrMalformed) {
		t.Fatalf("read error = %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		registry.mu.Lock()
		outstanding := registry.outstanding
		registry.mu.Unlock()
		select {
		case <-body.closed:
			if outstanding == 0 {
				return
			}
		case <-time.After(time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("malformed response retained body/callback (outstanding=%d)", outstanding)
		}
	}
}

func TestM6HTTPEarlySmallChunksBeforeLaterData(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		registry := newInvocationRegistry(37)
		defer registry.close()
		body := newStagedHTTPBody("q")
		id, _, err := registry.beginWithServices(context.Background(), "acct", nil, httpDoerFunc(func(r *http.Request) (*http.Response, error) {
			one := make([]byte, 1)
			if _, e := io.ReadFull(r.Body, one); e != nil {
				return nil, e
			}
			if string(one) != "q" {
				t.Errorf("first request byte %q", one)
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody}, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		client := HTTPDoerClient{client: credentialTestServer(t, registry), epoch: 37, invocationID: id}
		req, _ := http.NewRequest("POST", "http://example.invalid", body)
		type doResult struct {
			response *http.Response
			err      error
		}
		done := make(chan doResult, 1)
		go func() { resp, e := client.Do(req); done <- doResult{resp, e} }()
		var result doResult
		select {
		case result = <-done:
		case <-time.After(time.Second):
			body.releaseLater()
			<-done
			t.Fatal("request's first small chunk waited for later data")
		}
		resp, err := result.response, result.err
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		select {
		case <-body.closed:
		case <-time.After(time.Second):
			t.Fatal("request upload did not stop after response")
		}
	})
	t.Run("response", func(t *testing.T) {
		registry := newInvocationRegistry(38)
		defer registry.close()
		body := newStagedHTTPBody("z")
		id, _, err := registry.beginWithServices(context.Background(), "acct", nil, httpDoerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		client := HTTPDoerClient{client: credentialTestServer(t, registry), epoch: 38, invocationID: id}
		req, _ := http.NewRequest("GET", "http://example.invalid", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		first := make([]byte, 8)
		type readResult struct {
			n   int
			err error
		}
		readDone := make(chan readResult, 1)
		go func() { n, e := resp.Body.Read(first); readDone <- readResult{n, e} }()
		var read readResult
		select {
		case read = <-readDone:
		case <-time.After(time.Second):
			body.releaseLater()
			<-readDone
			t.Fatal("response's first small chunk waited for later data")
		}
		if read.err != nil || string(first[:read.n]) != "z" {
			t.Fatalf("first response bytes %q: %v", first[:read.n], read.err)
		}
		body.releaseLater()
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	})
}

func TestM6HTTPRejectsOversizedHeaders(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.invalid", nil)
	req.Header.Set("X-Large", strings.Repeat("x", 17<<10))
	if _, err := ipc.HTTPRequestTo(req); err == nil {
		t.Fatal("oversized request headers accepted")
	}
	if _, err := ipc.HTTPResponseTo(&http.Response{StatusCode: 200, Header: http.Header{"X-Large": {strings.Repeat("x", 17<<10)}}}); err == nil {
		t.Fatal("oversized response headers accepted")
	}
}

func TestM6HTTPRejectsMalformedHeaderNamesAndValues(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.invalid", nil)
	for _, h := range []http.Header{{"Bad Name": {"value"}}, {"X-Test": {"ok\r\nInjected: yes"}}} {
		req.Header = h
		if _, err := ipc.HTTPRequestTo(req); err == nil {
			t.Fatalf("malformed request header accepted: %#v", h)
		}
	}
	for _, h := range []http.Header{{"Bad Name": {"value"}}, {"X-Test": {"ok\x00bad"}}} {
		if _, err := ipc.HTTPResponseTo(&http.Response{StatusCode: 200, Header: h}); err == nil {
			t.Fatalf("malformed response header accepted: %#v", h)
		}
	}
	if _, err := ipc.HTTPRequestFrom(&gen.HTTPRequest{Url: "http://example.invalid", Headers: map[string]*gen.StringList{"X-Test": {Values: []string{"bad\nvalue"}}}}, http.NoBody); err == nil {
		t.Fatal("malformed remote request header accepted")
	}
	if _, err := ipc.HTTPResponseFrom(&gen.HTTPResponse{StatusCode: 200, Headers: map[string]*gen.StringList{"Bad Name": {Values: []string{"value"}}}}, http.NoBody); err == nil {
		t.Fatal("malformed remote response header accepted")
	}
}

func TestM6HTTPRejectsUnknownStaleAndReplayedAuthority(t *testing.T) {
	registry := newInvocationRegistry(33)
	defer registry.close()
	id, _, err := registry.beginWithServices(context.Background(), "acct", nil, httpDoerFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	client := credentialTestServer(t, registry)
	req, _ := http.NewRequest("GET", "http://example.invalid", nil)
	if _, err := (&HTTPDoerClient{client: client, epoch: 33, invocationID: "unknown"}).doWithCallID(req, 991); err == nil {
		t.Fatal("unknown invocation accepted")
	}
	if _, err := (&HTTPDoerClient{client: client, epoch: 34, invocationID: id}).doWithCallID(req, 992); err == nil {
		t.Fatal("stale epoch accepted")
	}
	valid := &HTTPDoerClient{client: client, epoch: 33, invocationID: id}
	if _, err := valid.doWithCallID(req, 993); err != nil {
		t.Fatal(err)
	}
	if _, err := valid.doWithCallID(req, 993); err == nil {
		t.Fatal("replayed call ID accepted")
	}
	registry.close()
	if _, err := valid.doWithCallID(req, 994); err == nil {
		t.Fatal("closed invocation accepted")
	}
}
