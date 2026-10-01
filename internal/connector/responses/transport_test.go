package responses

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
)

func testTransport(endpoint string) *Transport {
	return NewTransport(Config{Endpoint: endpoint, ConnectTimeout: time.Second, TLSHandshakeTimeout: time.Second, ResponseHeaderTimeout: time.Second})
}

func testRequest() core.ExecutionRequest {
	return core.ExecutionRequest{Payload: core.RawPayload{Body: []byte(" {\"unknown\": [1, 2]} ")}}
}

func TestTransportRequestConstruction(t *testing.T) {
	up := fakeupstream.New(fakeupstream.Response{Body: []byte("ok")})
	defer up.Close()
	endpoint := up.URL + "/v1/responses"
	tr := testTransport(endpoint)
	defer tr.Close()
	in := testRequest()
	in.Metadata.Headers = map[string][]string{
		"Authorization": {"Bearer client"}, "Connection": {"X-Remove, Keep-Alive"},
		"X-Remove": {"private"}, "Keep-Alive": {"timeout=5"},
		"Proxy-Authenticate": {"bad"}, "Proxy-Authorization": {"bad"},
		"TE": {"trailers"}, "Trailers": {"x-trailer"}, "Transfer-Encoding": {"chunked"},
		"Upgrade": {"websocket"}, "Idempotency-Key": {"retain"},
		"Content-Type": {"text/plain"}, "Accept-Encoding": {"gzip"}, "X-Allowed": {"yes"},
	}
	req, err := tr.Request(context.Background(), in, "selected")
	if err != nil || req.GetBody != nil || req.Body == nil || req.URL.String() != endpoint || req.Method != http.MethodPost {
		t.Fatalf("request: %+v, %v", req, err)
	}
	req.Body.Close()
	resp, err := tr.Do(context.Background(), in, "selected", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	capture := <-up.Requests
	if capture.Method != http.MethodPost || capture.Path != "/v1/responses" || !bytes.Equal(capture.Body, in.Payload.Body) {
		t.Fatalf("request capture: %+v", capture)
	}
	for key, want := range map[string]string{"Authorization": "Bearer selected", "Content-Type": "application/json", "Accept-Encoding": "identity", "Idempotency-Key": "retain", "X-Allowed": "yes"} {
		if got := capture.Header.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"Connection", "X-Remove", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailers", "Transfer-Encoding", "Upgrade"} {
		if got := capture.Header.Get(key); got != "" {
			t.Errorf("leaked %s = %q", key, got)
		}
	}
	in.Metadata.Headers["Connection"] = []string{"Idempotency-Key"}
	req, err = tr.Request(context.Background(), in, "selected")
	if err != nil {
		t.Fatal(err)
	}
	defer req.Body.Close()
	if req.Header.Get("Idempotency-Key") != "" {
		t.Fatal("Connection-nominated idempotency header leaked")
	}
}

func TestTransportNoRedirect(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.WriteHeader(http.StatusOK) }))
	defer destination.Close()
	up := fakeupstream.New(fakeupstream.Response{Status: http.StatusTemporaryRedirect, Header: http.Header{"Location": {destination.URL}}})
	defer up.Close()
	tr := testTransport(up.URL + "/v1/responses")
	defer tr.Close()
	resp, err := tr.Do(context.Background(), testRequest(), "selected", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || redirected.Load() != 0 {
		t.Fatalf("redirect followed: status=%d destination requests=%d", resp.StatusCode, redirected.Load())
	}
}

func TestTransportNoHiddenReplay(t *testing.T) {
	var count atomic.Int32
	addresses := make(chan string, 3)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		addresses <- r.RemoteAddr
		if count.Add(1) == 2 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer up.Close()
	tr := testTransport(up.URL + "/v1/responses")
	defer tr.Close()
	in := testRequest()
	in.Metadata.Headers = map[string][]string{"Idempotency-Key": {"key"}, "X-Idempotency-Key": {"key"}}
	first, err := tr.Do(context.Background(), in, "selected", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, first.Body)
	first.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	second, err := tr.Do(context.Background(), in, "selected", nil)
	if second != nil {
		second.Body.Close()
	}
	if err == nil || count.Load() != 2 {
		t.Fatalf("dropped request replayed: response=%v error=%v count=%d", second, err, count.Load())
	}
	if a, b := <-addresses, <-addresses; a != b {
		t.Fatalf("fixture did not use reused connection: %q != %q", a, b)
	}
}

func TestTransportNoDecompression(t *testing.T) {
	var encoded bytes.Buffer
	zw := gzip.NewWriter(&encoded)
	_, _ = zw.Write([]byte("native bytes"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	up := fakeupstream.New(fakeupstream.Response{Header: http.Header{"Content-Encoding": {"gzip"}}, Body: encoded.Bytes()})
	defer up.Close()
	tr := testTransport(up.URL + "/v1/responses")
	defer tr.Close()
	resp, err := tr.Do(context.Background(), testRequest(), "selected", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(body, encoded.Bytes()) || resp.Uncompressed || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("response decompressed: %v, %v", resp, err)
	}
}

func TestTransportCancellation(t *testing.T) {
	gate := make(chan struct{})
	up := fakeupstream.New(fakeupstream.Response{Steps: []fakeupstream.Step{{Gate: gate, Data: []byte("pending")}}})
	defer up.Close()
	tr := testTransport(up.URL + "/v1/responses")
	defer tr.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := tr.Do(ctx, testRequest(), "selected", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	capture := <-up.Requests
	result := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, resp.Body); result <- err }()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("read after cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read did not stop on cancellation")
	}
	select {
	case <-capture.Cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not observe cancellation")
	}
}
