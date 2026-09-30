package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
)

// TestNativeBaseline compares equivalent warmed serial requests and separately
// samples direct/proxy active-stream memory; figures are observations, not budgets.
func TestNativeBaseline(t *testing.T) {
	const credential = "synthetic-baseline-credential"
	requestBody := []byte(`{"model":"gpt-5.4-mini","unknown":{"keep":true}}`)
	jsonUpstream := fakeupstream.New(fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"status":"completed"}`)})
	defer jsonUpstream.Close()
	settings := config{Listen: "127.0.0.1:0", UpstreamEndpoint: jsonUpstream.URL + "/v1/responses", UpstreamCredentialEnv: "BASELINE",
		MaxRequestBodyBytes: 4096, MaxRequestHeaderBytes: 4096, ConnectTimeout: "1s", TLSHandshakeTimeout: "1s",
		ResponseHeaderTimeout: "2s", StreamIdleTimeout: "5s", credential: secret(credential)}
	var ready atomic.Bool
	h, closeTransport := handler(settings, &ready)
	gateway := httptest.NewServer(h)
	defer func() { gateway.Close(); closeTransport() }()
	transport := &http.Transport{}
	client := &http.Client{Transport: transport}
	defer transport.CloseIdleConnections()
	type sample struct{ ttfb, total time.Duration }
	measure := func(target string, proxy bool) sample {
		req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(requestBody))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if !proxy {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
		started := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var one [1]byte
		if _, err := io.ReadFull(resp.Body, one[:]); err != nil {
			t.Fatal(err)
		}
		ttfb := time.Since(started)
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return sample{ttfb, time.Since(started)}
	}
	directURL, gatewayURL := jsonUpstream.URL+"/v1/responses", gateway.URL+"/v1/responses"
	for i := 0; i < 5; i++ {
		measure(directURL, false)
		<-jsonUpstream.Requests
		measure(gatewayURL, true)
		<-jsonUpstream.Requests
	}
	const repetitions = 25
	direct, proxied := make([]sample, 0, repetitions), make([]sample, 0, repetitions)
	for i := 0; i < repetitions; i++ {
		for _, path := range []bool{false, true} {
			if path {
				proxied = append(proxied, measure(gatewayURL, true))
			} else {
				direct = append(direct, measure(directURL, false))
			}
			<-jsonUpstream.Requests
		}
	}
	logStats := func(name string, values []sample, selectValue func(sample) time.Duration) {
		series := make([]time.Duration, len(values))
		for i, value := range values {
			series[i] = selectValue(value)
		}
		sort.Slice(series, func(i, j int) bool { return series[i] < series[j] })
		p95 := series[(len(series)*95+99)/100-1]
		t.Logf("%s n=%d min=%s median=%s p95=%s max=%s", name, len(series), series[0], series[len(series)/2], p95, series[len(series)-1])
	}
	logStats("direct TTFB", direct, func(s sample) time.Duration { return s.ttfb })
	logStats("gateway TTFB", proxied, func(s sample) time.Duration { return s.ttfb })
	logStats("direct complete", direct, func(s sample) time.Duration { return s.total })
	logStats("gateway complete", proxied, func(s sample) time.Duration { return s.total })

	stableBaseline := func() int {
		client.CloseIdleConnections()
		runtime.GC()
		count, same, deadline := runtime.NumGoroutine(), 0, time.Now().Add(2*time.Second)
		for same < 10 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
			next := runtime.NumGoroutine()
			if next == count {
				same++
			} else {
				count, same = next, 0
			}
		}
		if same < 10 {
			t.Fatalf("idle goroutines did not stabilize: last=%d", count)
		}
		return count
	}
	streamBody := []byte(`{"model":"gpt-5.4-mini","stream":true}`)
	first := []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
	for _, streams := range []int{4, 8} {
		for _, proxiedPath := range []bool{false, true} {
			stableGoroutines := stableBaseline()
			gate := make(chan struct{})
			upstream := fakeupstream.New(fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: []fakeupstream.Step{{Data: first}, {Gate: gate, Data: []byte("event: response.completed\ndata: {}\n\n")}}})
			var streamGateway *httptest.Server
			streamClose := func() {}
			streamTarget := upstream.URL + "/v1/responses"
			if proxiedPath {
				streamSettings := settings
				streamSettings.UpstreamEndpoint = streamTarget
				var streamReady atomic.Bool
				streamHandler, closeStreamTransport := handler(streamSettings, &streamReady)
				streamClose = closeStreamTransport
				streamGateway = httptest.NewServer(streamHandler)
				streamTarget = streamGateway.URL + "/v1/responses"
			}
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			idleGoroutines := runtime.NumGoroutine()
			cancels := make([]context.CancelFunc, streams)
			done := make(chan struct{}, streams)
			activeReady := make(chan struct{}, streams)
			streamErrors := make(chan error, streams)
			for i := range cancels {
				ctx, cancel := context.WithCancel(context.Background())
				cancels[i] = cancel
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, streamTarget, bytes.NewReader(streamBody))
				req.Header.Set("Content-Type", "application/json")
				if !proxiedPath {
					req.Header.Set("Authorization", "Bearer "+credential)
				}
				go func() {
					resp, err := client.Do(req)
					if err == nil {
						var b [1]byte
						_, err = io.ReadFull(resp.Body, b[:])
						if err == nil {
							activeReady <- struct{}{}
							_, _ = io.Copy(io.Discard, resp.Body)
						} else {
							streamErrors <- err
						}
						_ = resp.Body.Close()
					} else {
						streamErrors <- err
					}
					done <- struct{}{}
				}()
			}
			captures := make([]fakeupstream.Request, streams)
			for i := range captures {
				select {
				case captures[i] = <-upstream.Requests:
				case <-time.After(5 * time.Second):
					t.Fatal("timed out waiting for active stream capture")
				}
			}
			for i := 0; i < streams; i++ {
				select {
				case <-activeReady:
				case err := <-streamErrors:
					t.Fatalf("active stream failed before snapshot: %v", err)
				case <-time.After(5 * time.Second):
					t.Fatal("timed out waiting for active client streams")
				}
			}
			var active runtime.MemStats
			runtime.ReadMemStats(&active)
			t.Logf("active %s streams n=%d whole_process_heap_delta=%dB per_stream_estimate=%dB goroutines=%d idle=%d", map[bool]string{false: "direct", true: "gateway"}[proxiedPath], streams, int64(active.HeapAlloc)-int64(before.HeapAlloc), (int64(active.HeapAlloc)-int64(before.HeapAlloc))/int64(streams), runtime.NumGoroutine(), idleGoroutines)
			for _, cancel := range cancels {
				cancel()
			}
			for _, capture := range captures {
				select {
				case <-capture.Cancelled:
				case <-time.After(5 * time.Second):
					t.Fatal("upstream did not observe active stream cancellation")
				}
			}
			for range cancels {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("stream client goroutine did not exit")
				}
			}
			if streamGateway != nil {
				streamGateway.Close()
			}
			streamClose()
			client.CloseIdleConnections()
			upstream.Close()
			deadline := time.Now().Add(2 * time.Second)
			for runtime.NumGoroutine() > stableGoroutines && time.Now().Before(deadline) {
				runtime.GC()
				time.Sleep(time.Millisecond)
			}
			if got := runtime.NumGoroutine(); got > stableGoroutines {
				t.Fatalf("%s stream cleanup leaked goroutines: got %d, stable pre-fixture baseline %d", map[bool]string{false: "direct", true: "gateway"}[proxiedPath], got, stableGoroutines)
			}
			runtime.GC()
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			t.Logf("after %s cancellation/fixture teardown n=%d whole_process_heap_delta=%dB client_goroutines=0 upstream_cancel_observations=%d process_goroutines=%d stable_pre_fixture=%d", map[bool]string{false: "direct", true: "gateway"}[proxiedPath], streams, int64(after.HeapAlloc)-int64(before.HeapAlloc), len(captures), runtime.NumGoroutine(), stableGoroutines)
		}
	}
}

func TestFixedResponsesComposition(t *testing.T) {
	const credential = "synthetic-selected-credential"
	const clientCredential = "synthetic-client-credential"
	t.Setenv("PESTIROUTE_TEST_UPSTREAM", credential)
	responseBody := []byte(" {\n \"status\" : \"completed\", \"unknown\": {\"nested\": [1, 2]} } \n")
	upstream := fakeupstream.New(fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: responseBody})
	defer upstream.Close()
	configPath := t.TempDir() + "/gateway.json"
	settings := map[string]any{
		"listen": "127.0.0.1:0", "upstream_endpoint": upstream.URL + "/v1/responses",
		"upstream_credential_env": "PESTIROUTE_TEST_UPSTREAM", "max_request_body_bytes": 1024,
		"max_request_header_bytes": 4096, "connect_timeout": "1s", "tls_handshake_timeout": "1s",
		"response_header_timeout": "1s", "stream_idle_timeout": "1s",
	}
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	c, _, err := loadConfig([]string{"-config", configPath})
	if err != nil {
		t.Fatal(err)
	}
	var ready atomic.Bool
	h, closeTransport := handler(c, &ready)
	defer closeTransport()
	server := httptest.NewServer(h)
	defer server.Close()
	ready.Store(true)
	client := server.Client()
	requestBody := []byte(" { \"model\" : \"gpt-5.4-mini\", \"unknown\": {\"nested\": [ 1, {\"extra\": true} ]} } \n")
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+clientCredential)
	req.Header.Set("X-Client-Token", clientCredential)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !bytes.Equal(got, responseBody) {
		t.Fatalf("fixed response: status %d, exact bytes %t, read %v", resp.StatusCode, bytes.Equal(got, responseBody), err)
	}
	select {
	case captured := <-upstream.Requests:
		if captured.Method != http.MethodPost || captured.Path != "/v1/responses" || !bytes.Equal(captured.Body, requestBody) || captured.Header.Get("Authorization") != "Bearer "+credential || captured.Header.Get("X-Client-Token") != "" || bytes.Contains(captured.Body, []byte(clientCredential)) {
			t.Fatalf("upstream request mismatch: method %s path %s exact bytes %t selected credential %t client token absent %t", captured.Method, captured.Path, bytes.Equal(captured.Body, requestBody), captured.Header.Get("Authorization") == "Bearer "+credential, captured.Header.Get("X-Client-Token") == "")
		}
	default:
		t.Fatal("missing upstream request")
	}
	for _, tc := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"malformed", "POST", "/v1/responses", `{"model":`, 400},
		{"oversized", "POST", "/v1/responses", strings.Repeat("x", 1025), 400},
		{"wrong method", "GET", "/v1/responses", "", 405},
		{"wrong path", "POST", "/v1/other", "{}", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, server.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status %d, want %d", resp.StatusCode, tc.want)
			}
			select {
			case <-upstream.Requests:
				t.Fatal("rejected request reached upstream")
			default:
			}
		})
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := client.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: status %d", path, resp.StatusCode)
		}
	}
}

func TestResponsesRejectPreCommit(t *testing.T) {
	for _, tc := range []struct {
		status   int
		category core.ErrorCategory
		retry    bool
	}{
		{http.StatusUnauthorized, core.CategoryUnauthenticated, false},
		{http.StatusForbidden, core.CategoryPermissionDenied, false},
		{http.StatusTooManyRequests, core.CategoryRateLimited, true},
		{http.StatusServiceUnavailable, core.CategoryUnavailable, true},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			const upstreamBody = ` {"error":{"message":"private provider detail"}} `
			headers := http.Header{"Content-Type": {"application/json"}}
			if tc.retry {
				headers.Set("Retry-After", "23")
			}
			upstream := fakeupstream.New(fakeupstream.Response{Status: tc.status, Header: headers, Body: []byte(upstreamBody)})
			defer upstream.Close()
			finals := make(chan core.AttemptResult, 2)
			var finalized atomic.Int32
			server, closeTransport := cancellationServer(t, upstream, finals, &finalized)
			defer closeTransport()
			defer server.Close()
			resp, err := server.Client().Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4-mini"}`))
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil || resp.StatusCode != tc.status || !bytes.Equal(body, []byte(upstreamBody)) || (resp.Header.Get("Retry-After") == "23") != tc.retry {
				t.Fatalf("native rejection: status=%d retry-after=%q body=%q read=%v", resp.StatusCode, resp.Header.Get("Retry-After"), body, readErr)
			}
			receiveRequest(t, upstream)
			if upstream.RequestCount() != 1 {
				t.Fatalf("upstream request count=%d, want exactly one", upstream.RequestCount())
			}
			select {
			case result := <-finals:
				if !result.Committed || result.Outcome != core.OutcomeFailed || result.Error == nil || result.Error.Category != tc.category || result.Error.Retryable != tc.retry || len(finals) != 0 {
					t.Fatalf("finalized rejection: %+v", result)
				}
				if tc.retry && (result.Error.RetryAfter == nil || *result.Error.RetryAfter != 23*time.Second || result.Error.RetryDisposition != core.RetryUnknown) {
					t.Fatalf("retry metadata missing before Head handoff: %+v", result.Error)
				}
			case <-time.After(time.Second):
				t.Fatal("rejected attempt was not finalized")
			}
			assertCounts(t, upstream, &finalized, 1)
		})
	}
}

func TestResponsesTransportErrorPreCommit(t *testing.T) {
	upstream := fakeupstream.New(fakeupstream.Response{Drop: true})
	defer upstream.Close()
	finals := make(chan core.AttemptResult, 2)
	var finalized atomic.Int32
	server, closeTransport := cancellationServer(t, upstream, finals, &finalized)
	defer closeTransport()
	defer server.Close()
	resp, err := server.Client().Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4-mini"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	var gatewayBody struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	decodeErr := json.Unmarshal(body, &gatewayBody)
	if readErr != nil || decodeErr != nil || resp.StatusCode != http.StatusServiceUnavailable || gatewayBody.Error.Code != "upstream_unavailable" || gatewayBody.Error.Type != string(core.CategoryUnavailable) || bytes.Contains(body, []byte("private")) {
		t.Fatalf("transport error response: status=%d body=%q read=%v", resp.StatusCode, body, readErr)
	}
	receiveRequest(t, upstream)
	select {
	case result := <-finals:
		if result.Committed || result.Outcome != core.OutcomeFailed || result.Error == nil || result.Error.Category != core.CategoryUnavailable || result.Error.Retryable || result.Error.RetryDisposition != core.RetryUnknown || len(finals) != 0 {
			t.Fatalf("pre-Head transport failure metadata: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("transport-failed attempt was not finalized")
	}
	assertCounts(t, upstream, &finalized, 1)
}

func TestResponsesConnectionFailurePreCommit(t *testing.T) {
	upstream := fakeupstream.New(fakeupstream.Response{})
	endpoint := upstream.URL + "/v1/responses"
	upstream.Close()
	finals := make(chan core.AttemptResult, 2)
	var finalized atomic.Int32
	var ready atomic.Bool
	h, closeTransport := handlerWithFinalize(config{UpstreamEndpoint: endpoint, UpstreamCredentialEnv: "TEST_UPSTREAM", credential: "synthetic", MaxRequestBodyBytes: 1024, MaxRequestHeaderBytes: 4096, ConnectTimeout: "1s", TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s"}, &ready, func(result core.AttemptResult) {
		finalized.Add(1)
		finals <- result
	})
	defer closeTransport()
	server := httptest.NewServer(h)
	defer server.Close()
	resp, err := server.Client().Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4-mini"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusServiceUnavailable || !json.Valid(body) {
		t.Fatalf("connection failure response: status=%d body=%q read=%v", resp.StatusCode, body, readErr)
	}
	select {
	case result := <-finals:
		if result.Committed || result.Outcome != core.OutcomeFailed || result.Error == nil || result.Error.Category != core.CategoryUnavailable || result.Error.Retryable || result.Error.RetryDisposition != core.RetryUnknown || len(finals) != 0 {
			t.Fatalf("connection failure metadata: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("connection-failed attempt was not finalized")
	}
	if finalized.Load() != 1 {
		t.Fatalf("connection failure attempt finalization count=%d, want exactly one", finalized.Load())
	}
}

func TestResponsesIncrementalFlush(t *testing.T) {
	const first = "event: response.created\ndata: {\"type\":\"response.created\"}\n\n"
	const second = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	gate := make(chan struct{})
	sent := make(chan struct{})
	upstream := fakeupstream.New(fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Upstream": {"one"}}, Steps: []fakeupstream.Step{{Data: []byte(first), Sent: sent}, {Gate: gate, Data: []byte(second)}}})
	defer upstream.Close()
	var ready atomic.Bool
	h, closeTransport := handler(config{UpstreamEndpoint: upstream.URL + "/v1/responses", UpstreamCredentialEnv: "TEST_UPSTREAM", credential: "synthetic", MaxRequestBodyBytes: 1024, MaxRequestHeaderBytes: 4096, ConnectTimeout: "1s", TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s"}, &ready)
	defer closeTransport()
	server := httptest.NewServer(h)
	defer server.Close()
	defer func() {
		if gate != nil {
			close(gate) // Release the fake upstream before server cleanup on failure.
		}
	}()
	client := server.Client()
	client.Timeout = 3 * time.Second
	requestBody := []byte(`{ "model": "gpt-5.4-mini", "stream": true, "extra": {"opaque": 1} }`)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" || len(resp.Header.Values("X-Upstream")) != 1 || resp.Header.Get("X-Upstream") != "one" {
		t.Fatalf("stream headers: %d %v", resp.StatusCode, resp.Header)
	}
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("upstream first event not sent")
	}
	gotFirst := make([]byte, len(first))
	if _, err := io.ReadFull(resp.Body, gotFirst); err != nil || string(gotFirst) != first {
		t.Fatalf("first event before second gate: %q, %v", gotFirst, err)
	}
	select {
	case captured := <-upstream.Requests:
		if !bytes.Equal(captured.Body, requestBody) || captured.Header.Get("Authorization") != "Bearer synthetic" {
			t.Fatalf("upstream request: exact bytes %t, selected credential %t", bytes.Equal(captured.Body, requestBody), captured.Header.Get("Authorization") == "Bearer synthetic")
		}
	default:
		t.Fatal("upstream request not captured")
	}
	closeGate := gate
	gate = nil
	close(closeGate)
	rest, err := io.ReadAll(resp.Body)
	if err != nil || string(rest) != second {
		t.Fatalf("second event and EOF: %q, %v", rest, err)
	}
}

func TestResponsesCancelBeforeHead(t *testing.T) {
	gate := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(gate)
		}
	}()
	upstream := fakeupstream.New(fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"status":"completed"}`), HeaderGate: gate})
	defer upstream.Close()
	finals := make(chan core.AttemptResult, 4)
	var finalized atomic.Int32
	server, closeTransport := cancellationServer(t, upstream, finals, &finalized)
	defer closeTransport()
	defer server.Close()
	conn := rawGatewayRequest(t, server, `{"model":"gpt-5.4-mini"}`)
	captured := receiveRequest(t, upstream)
	conn.Close()
	waitCancelled(t, captured)
	assertFinalization(t, finals, core.OutcomeCancelled, false)
	if upstream.RequestCount() != 1 {
		t.Fatalf("upstream invocation count before follow-up = %d, want 1", upstream.RequestCount())
	}
	close(gate)
	released = true
	response := sendGatewayRequest(t, server, `{"model":"gpt-5.4-mini"}`)
	if response != `{"status":"completed"}` {
		t.Fatalf("follow-up response: %q", response)
	}
	assertFinalization(t, finals, core.OutcomeSucceeded, true)
	assertCounts(t, upstream, &finalized, 2)
}

func TestResponsesCancelAfterHead(t *testing.T) {
	const first = "event: response.created\ndata: {\"type\":\"response.created\"}\n\n"
	gate := make(chan struct{})
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()
	upstream := fakeupstream.New(fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: []fakeupstream.Step{{Data: []byte(first)}, {Gate: gate, Data: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")}}})
	defer upstream.Close()
	finals := make(chan core.AttemptResult, 4)
	var finalized atomic.Int32
	server, closeTransport := cancellationServer(t, upstream, finals, &finalized)
	defer closeTransport()
	defer server.Close()
	conn := rawGatewayRequest(t, server, `{"model":"gpt-5.4-mini","stream":true}`)
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(first))
	if _, err := io.ReadFull(resp.Body, got); err != nil || string(got) != first {
		t.Fatalf("first event: %q, %v", got, err)
	}
	captured := receiveRequest(t, upstream)
	conn.Close()
	waitCancelled(t, captured)
	assertFinalization(t, finals, core.OutcomeCancelled, true)
	if upstream.RequestCount() != 1 {
		t.Fatalf("upstream invocation count before follow-up = %d, want 1", upstream.RequestCount())
	}
	close(gate)
	follow := sendGatewayRequest(t, server, `{"model":"gpt-5.4-mini","stream":true}`)
	if follow != `event: response.created`+"\ndata: {\"type\":\"response.created\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n" {
		t.Fatalf("follow-up response: %q", follow)
	}
	assertFinalization(t, finals, core.OutcomeSucceeded, true)
	assertCounts(t, upstream, &finalized, 2)
}

func TestResponsesSlowConsumerBackpressure(t *testing.T) {
	want, steps, progress := slowConsumerFixture()
	upstream := fakeupstream.New(fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: steps})
	defer upstream.Close()
	var ready atomic.Bool
	h, closeTransport := handler(config{UpstreamEndpoint: upstream.URL + "/v1/responses", UpstreamCredentialEnv: "TEST_UPSTREAM", credential: "synthetic", MaxRequestBodyBytes: 1024, MaxRequestHeaderBytes: 4096, ConnectTimeout: "1s", TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s", StreamIdleTimeout: "100ms"}, &ready)
	defer closeTransport()
	server := httptest.NewServer(h)
	defer server.Close()

	conn := slowGatewayRequest(t, server)
	defer conn.Close()
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream response: %d %v", resp.StatusCode, resp.Header)
	}
	receiveRequest(t, upstream)
	select {
	case <-progress[0]:
	case <-time.After(time.Second):
		t.Fatal("upstream did not complete its initial write")
	}
	consumed := len(steps[0].Data)
	prefix := make([]byte, consumed)
	if _, err := io.ReadFull(resp.Body, prefix); err != nil || !bytes.Equal(prefix, want[:consumed]) {
		t.Fatalf("initial downstream progress exact bytes %t, read error %v", bytes.Equal(prefix, want[:consumed]), err)
	}
	completed := waitForSlowConsumerStall(t, progress)

	got, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(append(prefix, got...), want) {
		t.Fatalf("resumed stream exact bytes %t, got %d want %d bytes: %v", bytes.Equal(append(prefix, got...), want), len(got)+len(prefix), len(want), err)
	}
	if !waitForAllSlowConsumerWrites(progress, 3*time.Second) {
		t.Fatalf("upstream completed %d of %d writes after downstream resumed", completed, len(progress))
	}
}

func TestResponsesBackpressureCancellation(t *testing.T) {
	_, steps, progress := slowConsumerFixture()
	upstream := fakeupstream.New(fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: steps})
	defer upstream.Close()
	finals := make(chan core.AttemptResult, 1)
	var finalized atomic.Int32
	server, closeTransport := cancellationServer(t, upstream, finals, &finalized)
	defer closeTransport()
	defer server.Close()

	conn := slowGatewayRequest(t, server)
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}
	captured := receiveRequest(t, upstream)
	select {
	case <-progress[0]:
	case <-time.After(time.Second):
		conn.Close()
		t.Fatal("upstream did not complete its initial write")
	}
	prefix := make([]byte, len(steps[0].Data))
	if _, err := io.ReadFull(resp.Body, prefix); err != nil || !bytes.Equal(prefix, steps[0].Data) {
		conn.Close()
		t.Fatalf("initial downstream progress exact bytes %t, read error %v", bytes.Equal(prefix, steps[0].Data), err)
	}
	waitForSlowConsumerStall(t, progress)
	if err := conn.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	waitCancelled(t, captured)
	assertFinalization(t, finals, core.OutcomeCancelled, true)
	assertCounts(t, upstream, &finalized, 1)
}

func TestResponsesTimeoutHeaderIdleHealthyAndDeadline(t *testing.T) {
	t.Run("response header timeout is 504", func(t *testing.T) {
		upstream := fakeupstream.New(fakeupstream.Response{HeaderGate: make(chan struct{})})
		defer upstream.Close()
		finals := make(chan core.AttemptResult, 2)
		var finalized atomic.Int32
		server, closeTransport := timeoutServer(t, upstream, 60*time.Millisecond, 2*time.Second, finals, &finalized)
		defer closeTransport()
		defer server.Close()
		resp, err := server.Client().Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4-mini"}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusGatewayTimeout {
			t.Fatalf("status %d", resp.StatusCode)
		}
		receiveRequest(t, upstream)
		assertTimeoutFinal(t, finals, &finalized, false)
		if upstream.RequestCount() != 1 {
			t.Fatalf("upstream attempts=%d", upstream.RequestCount())
		}
	})
	t.Run("idle timeout closes committed stream once", func(t *testing.T) {
		upstream := fakeupstream.New(fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: []fakeupstream.Step{{Data: []byte("event: response.created\ndata: {}\n\n")}, {Gate: make(chan struct{})}}})
		defer upstream.Close()
		finals := make(chan core.AttemptResult, 2)
		var finalized atomic.Int32
		server, closeTransport := timeoutServer(t, upstream, time.Second, 70*time.Millisecond, finals, &finalized)
		defer closeTransport()
		defer server.Close()
		resp, err := server.Client().Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4-mini","stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("response.created")) {
			t.Fatalf("stream status=%d body=%q read=%v", resp.StatusCode, body, readErr)
		}
		req := receiveRequest(t, upstream)
		select {
		case <-req.Cancelled:
		case <-time.After(time.Second):
			t.Fatal("idle timeout did not cancel upstream")
		}
		assertTimeoutFinal(t, finals, &finalized, true)
	})
	t.Run("long stream with timely events survives", func(t *testing.T) {
		firstSent, secondSent := make(chan struct{}), make(chan struct{})
		secondGate, thirdGate := make(chan struct{}), make(chan struct{})
		go func() {
			<-firstSent
			time.Sleep(120 * time.Millisecond)
			close(secondGate)
			<-secondSent
			time.Sleep(120 * time.Millisecond)
			close(thirdGate)
		}()
		upstream := fakeupstream.New(fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: []fakeupstream.Step{
			{Data: []byte("event: response.created\ndata: {}\n\n"), Sent: firstSent},
			{Gate: secondGate, Data: []byte("event: response.in_progress\ndata: {}\n\n"), Sent: secondSent},
			{Gate: thirdGate, Data: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")},
		}})
		defer upstream.Close()
		finals := make(chan core.AttemptResult, 1)
		var finalized atomic.Int32
		server, closeTransport := timeoutServer(t, upstream, 80*time.Millisecond, 200*time.Millisecond, finals, &finalized)
		defer closeTransport()
		defer server.Close()
		resp, err := server.Client().Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4-mini","stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("response.completed")) {
			t.Fatalf("healthy stream status=%d read=%v body=%q", resp.StatusCode, readErr, body)
		}
		assertFinalization(t, finals, core.OutcomeSucceeded, true)
		assertCounts(t, upstream, &finalized, 1)
	})
}

func TestResponsesClientDeadlineCancelsUpstreamBeforeAndAfterHead(t *testing.T) {
	for _, afterHead := range []bool{false, true} {
		name := "before Head"
		if afterHead {
			name = "after Head"
		}
		t.Run(name, func(t *testing.T) {
			response := fakeupstream.Response{HeaderGate: make(chan struct{})}
			if afterHead {
				response = fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: []fakeupstream.Step{{Gate: make(chan struct{}), Data: []byte("event: response.created\ndata: {}\n\n")}}}
			}
			upstream := fakeupstream.New(response)
			defer upstream.Close()
			finals := make(chan core.AttemptResult, 1)
			var finalized atomic.Int32
			server, closeTransport := timeoutServer(t, upstream, time.Second, time.Second, finals, &finalized)
			defer closeTransport()
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.4-mini","stream":true}`))
			req.Header.Set("Content-Type", "application/json")
			resp, err := server.Client().Do(req)
			if afterHead {
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			} else if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				t.Fatalf("deadline returned a response before Head: status=%d", resp.StatusCode)
			}
			u := receiveRequest(t, upstream)
			select {
			case <-u.Cancelled:
			case <-time.After(time.Second):
				t.Fatal("client deadline did not cancel upstream")
			}
			select {
			case r := <-finals:
				if r.Error == nil || r.Error.Category != core.CategoryCancelled {
					t.Fatalf("deadline finalization: %+v", r)
				}
				if r.Committed != afterHead {
					t.Fatalf("committed=%t", r.Committed)
				}
			case <-time.After(time.Second):
				t.Fatal("deadline did not finalize")
			}
			if finalized.Load() != 1 {
				t.Fatalf("finalized %d times", finalized.Load())
			}
		})
	}
}

func timeoutServer(t *testing.T, upstream *fakeupstream.Server, header, idle time.Duration, finals chan core.AttemptResult, finalized *atomic.Int32) (*httptest.Server, func()) {
	t.Helper()
	var ready atomic.Bool
	h, closeTransport := handlerWithFinalize(config{UpstreamEndpoint: upstream.URL + "/v1/responses", UpstreamCredentialEnv: "TEST_UPSTREAM", credential: "synthetic", MaxRequestBodyBytes: 1024, MaxRequestHeaderBytes: 4096, ConnectTimeout: "1s", TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: header.String(), StreamIdleTimeout: idle.String()}, &ready, func(r core.AttemptResult) { finalized.Add(1); finals <- r })
	s := httptest.NewServer(h)
	ready.Store(true)
	return s, closeTransport
}

func assertTimeoutFinal(t *testing.T, finals <-chan core.AttemptResult, finalized *atomic.Int32, committed bool) {
	t.Helper()
	select {
	case r := <-finals:
		if r.Committed != committed || r.Error == nil || r.Error.Category != core.CategoryTimeout || committed && r.Outcome != core.OutcomeIncomplete {
			t.Fatalf("timeout finalization: committed=%t outcome=%s error=%+v", r.Committed, r.Outcome, r.Error)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout did not finalize")
	}
	if finalized.Load() != 1 {
		t.Fatalf("finalized %d times", finalized.Load())
	}
}

func slowConsumerFixture() ([]byte, []fakeupstream.Step, []<-chan struct{}) {
	const firstChunk = 4 << 10
	const chunkSize = 64 << 10
	const terminal = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	want := append(append([]byte(":"), bytes.Repeat([]byte{'x'}, 12<<20)...), []byte("\n\n"+terminal)...)
	var steps []fakeupstream.Step
	var progress []<-chan struct{}
	for start := 0; start < len(want); {
		end := start + chunkSize
		if start == 0 {
			end = firstChunk
		} else if end > len(want) {
			end = len(want)
		}
		sent := make(chan struct{})
		steps = append(steps, fakeupstream.Step{Data: want[start:end], Sent: sent})
		progress = append(progress, sent)
		start = end
	}
	return want, steps, progress
}

func waitForSlowConsumerStall(t *testing.T, progress []<-chan struct{}) int {
	t.Helper()
	started := time.Now()
	lastProgress := started
	completed := completedSlowConsumerWrites(progress)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			current := completedSlowConsumerWrites(progress)
			if current == len(progress) {
				t.Fatal("upstream completed the entire long response while downstream was stalled")
			}
			if current != completed {
				completed = current
				lastProgress = time.Now()
			} else if time.Since(lastProgress) >= 250*time.Millisecond {
				if completed < 2 {
					t.Fatalf("upstream completed only %d writes before stall; want initial and follow-up progress", completed)
				}
				return completed
			}
			if time.Since(started) > 5*time.Second {
				t.Fatalf("upstream writes did not reach a bounded stall; completed %d/%d", completed, len(progress))
			}
		}
	}
}

func completedSlowConsumerWrites(progress []<-chan struct{}) int {
	completed := 0
	for _, sent := range progress {
		select {
		case <-sent:
			completed++
		default:
		}
	}
	return completed
}

func waitForAllSlowConsumerWrites(progress []<-chan struct{}, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for completedSlowConsumerWrites(progress) < len(progress) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

func slowGatewayRequest(t *testing.T, server *httptest.Server) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).SetReadBuffer(64 << 10); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	body := `{"model":"gpt-5.4-mini","stream":true}`
	if _, err := fmt.Fprintf(conn, "POST /v1/responses HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", server.Listener.Addr(), len(body), body); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	return conn
}

func TestResponsesWriteFailureCancelsUpstream(t *testing.T) {
	const first = "event: response.created\ndata: {\"type\":\"response.created\"}\n\n"
	gate := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(gate)
		}
	}()
	upstream := fakeupstream.New(fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: []fakeupstream.Step{{Data: []byte(first)}, {Gate: gate, Data: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")}}})
	defer upstream.Close()
	finals := make(chan core.AttemptResult, 2)
	var finalized atomic.Int32
	server, closeTransport := cancellationServer(t, upstream, finals, &finalized)
	defer server.Close()
	defer closeTransport()
	body := `{"model":"gpt-5.4-mini","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := &gatewayFailWriter{header: make(http.Header)}
	server.Config.Handler.ServeHTTP(w, req)
	captured := receiveRequest(t, upstream)
	waitCancelled(t, captured)
	assertFinalization(t, finals, core.OutcomeCancelled, true)
	if upstream.RequestCount() != 1 {
		t.Fatalf("write-failed invocation count = %d, want 1", upstream.RequestCount())
	}
	close(gate)
	released = true
	follow := sendGatewayRequest(t, server, `{"model":"gpt-5.4-mini","stream":true}`)
	if follow == "" || upstream.RequestCount() != 2 {
		t.Fatalf("follow-up failed: body %q, invocation count %d", follow, upstream.RequestCount())
	}
	assertFinalization(t, finals, core.OutcomeSucceeded, true)
	assertCounts(t, upstream, &finalized, 2)
}

func TestResponsesPostCommitLateFailures(t *testing.T) {
	const created = "event: response.created\ndata: {\"type\":\"response.created\"}\n\n"
	const failed = "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n"
	for _, tc := range []struct {
		name     string
		response fakeupstream.Response
		prefix   string
		want     core.Outcome
	}{
		{name: "Head then drop", response: fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: []fakeupstream.Step{{Drop: true}}}, want: core.OutcomeIncomplete},
		{name: "Body then drop", response: fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: []fakeupstream.Step{{Data: []byte(created)}, {Data: []byte("partial"), Drop: true}}}, prefix: created + "partial", want: core.OutcomeIncomplete},
		{name: "EOF without terminal", response: fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: []fakeupstream.Step{{Data: []byte(created)}}}, prefix: created, want: core.OutcomeIncomplete},
		{name: "native failed terminal", response: fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: []fakeupstream.Step{{Data: []byte(failed)}}}, prefix: failed, want: core.OutcomeFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gate chan struct{}
			if tc.name == "Head then drop" {
				gate = make(chan struct{})
				tc.response.Steps[0].Gate = gate
				defer func() {
					if gate == nil {
						return
					}
					select {
					case <-gate:
					default:
						close(gate)
					}
				}()
			}
			upstream := fakeupstream.New(tc.response)
			defer upstream.Close()
			finals := make(chan core.AttemptResult, 2)
			var finalized atomic.Int32
			server, closeTransport := cancellationServer(t, upstream, finals, &finalized)
			defer closeTransport()
			defer server.Close()

			conn := rawGatewayRequest(t, server, `{"model":"gpt-5.4-mini","stream":true}`)
			defer conn.Close()
			resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("committed response replaced: status=%d headers=%v", resp.StatusCode, resp.Header)
			}
			receiveRequest(t, upstream)
			if upstream.RequestCount() != 1 {
				t.Fatalf("upstream invocation count=%d, want 1", upstream.RequestCount())
			}
			if gate != nil {
				close(gate)
				gate = nil
			}
			got, readErr := io.ReadAll(resp.Body)
			if string(got) != tc.prefix || readErr != nil {
				t.Fatalf("response bytes=%q read error=%v, want bytes=%q and clean termination", got, readErr, tc.prefix)
			}
			assertFinalization(t, finals, tc.want, true)
			assertCounts(t, upstream, &finalized, 1)
		})
	}
}

type gatewayFailWriter struct {
	header http.Header
	status int
	writes int
}

func (w *gatewayFailWriter) Header() http.Header    { return w.header }
func (w *gatewayFailWriter) WriteHeader(status int) { w.status = status }
func (w *gatewayFailWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, io.ErrClosedPipe
}

func cancellationServer(t *testing.T, upstream *fakeupstream.Server, finalize chan<- core.AttemptResult, count *atomic.Int32) (*httptest.Server, func()) {
	t.Helper()
	var ready atomic.Bool
	h, closeTransport := handlerWithFinalize(config{UpstreamEndpoint: upstream.URL + "/v1/responses", UpstreamCredentialEnv: "TEST_UPSTREAM", credential: "synthetic", MaxRequestBodyBytes: 1024, MaxRequestHeaderBytes: 4096, ConnectTimeout: "1s", TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s"}, &ready, func(result core.AttemptResult) {
		count.Add(1)
		finalize <- result
	})
	return httptest.NewServer(h), closeTransport
}

func rawGatewayRequest(t *testing.T, server *httptest.Server, body string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "POST /v1/responses HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", server.Listener.Addr(), len(body), body); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	return conn
}

func assertFinalization(t *testing.T, finals <-chan core.AttemptResult, outcome core.Outcome, committed bool) {
	t.Helper()
	select {
	case result := <-finals:
		if result.Outcome != outcome || result.Committed != committed || (outcome != core.OutcomeSucceeded && result.Error == nil) || len(finals) != 0 {
			category := core.ErrorCategory("")
			if result.Error != nil {
				category = result.Error.Category
			}
			t.Fatalf("attempt finalization: outcome=%s committed=%t error=%s queued=%d", result.Outcome, result.Committed, category, len(finals))
		}
	case <-time.After(time.Second):
		t.Fatal("attempt was not finalized")
	}
}

func assertCounts(t *testing.T, upstream *fakeupstream.Server, finalized *atomic.Int32, want int32) {
	t.Helper()
	if got := upstream.RequestCount(); got != int64(want) {
		t.Fatalf("upstream invocation count = %d, want %d", got, want)
	}
	if got := finalized.Load(); got != want {
		t.Fatalf("attempt finalization count = %d, want %d", got, want)
	}
}

func receiveRequest(t *testing.T, upstream *fakeupstream.Server) fakeupstream.Request {
	t.Helper()
	select {
	case request := <-upstream.Requests:
		return request
	case <-time.After(time.Second):
		t.Fatal("upstream request not observed")
		return fakeupstream.Request{}
	}
}

func waitCancelled(t *testing.T, request fakeupstream.Request) {
	t.Helper()
	select {
	case <-request.Cancelled:
	case <-time.After(time.Second):
		t.Fatal("upstream did not observe cancellation")
	}
}

func sendGatewayRequest(t *testing.T, server *httptest.Server, body string) string {
	t.Helper()
	resp, err := server.Client().Post(server.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("follow-up status %d: %v", resp.StatusCode, err)
	}
	return string(data)
}

func TestResponsesSplitToolEvents(t *testing.T) {
	// Synthetic Responses-native trace: distinct call IDs and output indices are
	// deliberately interleaved; chunk cuts are transport artifacts, not events.
	const first = "event: response.created\r\ndata: {\"type\":\"response.created\",\"unknown\":\"café\"}\r\n\r\n"
	events := []string{
		`event: response.output_item.added` + "\n" + `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_A","id":"item_A","name":"lookup"}}` + "\n\n",
		`event: response.output_item.added` + "\n" + `data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_B","id":"item_B","name":"lookup"}}` + "\n\n",
		`event: response.function_call_arguments.delta` + "\n" + `data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"item_A","delta":"{\"key\":\""}` + "\n\n",
		`event: response.function_call_arguments.delta` + "\n" + `data: {"type":"response.function_call_arguments.delta","output_index":1,"item_id":"item_B","delta":"{\"key\":\"B\"}"}` + "\n\n",
		`event: response.function_call_arguments.delta` + "\n" + `data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"item_A","delta":"A\"}"}` + "\n\n",
		`event: vendor.unknown` + "\n" + `data: {"type":"vendor.unknown","opaque":{"x":1}}` + "\n\n",
		`event: response.output_item.done` + "\n" + `data: {"type":"response.output_item.done","output_index":1,"item":{"id":"item_B","call_id":"call_B","arguments":"{\"key\":\"B\"}"}}` + "\n\n",
		`event: response.output_item.done` + "\n" + `data: {"type":"response.output_item.done","output_index":0,"item":{"id":"item_A","call_id":"call_A","arguments":"{\"key\":\"A\"}"}}` + "\n\n",
		`event: response.completed` + "\n" + `data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n",
	}
	want := []byte(first + strings.Join(events, ""))
	// Cut inside UTF-8, JSON strings, CRLF and SSE blank-line separators.
	cut := bytes.Index([]byte(first), []byte("é"))
	if cut < 0 {
		t.Fatal("missing UTF-8 fixture")
	}
	gate := make(chan struct{})
	defer func() {
		if gate != nil {
			close(gate)
		}
	}()
	// Hold the second half of A's argument delta, not just response.created:
	// the client must receive the first half before this tool event completes.
	deltaCut := bytes.Index([]byte(events[2]), []byte(`\"key`))
	if deltaCut < 0 {
		t.Fatal("missing argument JSON split point")
	}
	deltaCut += 2
	early := []byte(first + events[0] + events[1] + events[2][:deltaCut])
	steps := []fakeupstream.Step{
		{Data: []byte(first)[:cut+1]}, {Data: []byte(first)[cut+1 : len(first)-3]},
		{Data: []byte(first)[len(first)-3:]},
	}
	for _, event := range events[:2] {
		mid := bytes.Index([]byte(event), []byte(`"output_index"`))
		steps = append(steps, fakeupstream.Step{Data: []byte(event[:mid])}, fakeupstream.Step{Data: []byte(event[mid:])})
	}
	steps = append(steps, fakeupstream.Step{Data: []byte(events[2][:deltaCut])}, fakeupstream.Step{Gate: gate, Data: []byte(events[2][deltaCut:])})
	for _, event := range events[3:] {
		mid := bytes.Index([]byte(event), []byte(`"output_index"`))
		if mid < 0 {
			mid = len(event) / 2
		}
		steps = append(steps, fakeupstream.Step{Data: []byte(event[:mid])}, fakeupstream.Step{Data: []byte(event[mid:])})
	}
	upstream := fakeupstream.New(fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: steps})
	defer upstream.Close()
	var ready atomic.Bool
	h, closeTransport := handler(config{UpstreamEndpoint: upstream.URL + "/v1/responses", UpstreamCredentialEnv: "TEST_UPSTREAM", credential: "synthetic", MaxRequestBodyBytes: 4096, MaxRequestHeaderBytes: 4096, ConnectTimeout: "1s", TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s"}, &ready)
	defer closeTransport()
	server := httptest.NewServer(h)
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	send := func(body []byte) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("stream response: %d %v", resp.StatusCode, resp.Header)
		}
		return resp
	}
	initial := []byte(`{"model":"gpt-5.4-mini","stream":true,"unknown":{"keep":1}}`)
	resp := send(initial)
	gotEarly := make([]byte, len(early))
	if _, err := io.ReadFull(resp.Body, gotEarly); err != nil || !bytes.Equal(gotEarly, early) {
		t.Fatalf("early partial tool delta: exact bytes %t, read %v", bytes.Equal(gotEarly, early), err)
	}
	close(gate)
	gate = nil
	rest, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !bytes.Equal(append(gotEarly, rest...), want) {
		t.Fatalf("ordered native SSE: exact bytes %t, read %v", bytes.Equal(append(gotEarly, rest...), want), err)
	}
	var order []string
	callIDs := map[string]string{}
	arguments := map[string]string{}
	for _, event := range events {
		var data struct {
			Type        string `json:"type"`
			OutputIndex int    `json:"output_index"`
			ItemID      string `json:"item_id"`
			Delta       string `json:"delta"`
			Item        struct {
				ID        string `json:"id"`
				CallID    string `json:"call_id"`
				Arguments string `json:"arguments"`
			} `json:"item"`
		}
		line := strings.SplitN(event, "\n", 3)
		if len(line) != 3 || json.Unmarshal([]byte(strings.TrimPrefix(line[1], "data: ")), &data) != nil {
			t.Fatalf("invalid synthetic event: %q", event)
		}
		order = append(order, fmt.Sprintf("%s:%d", data.Type, data.OutputIndex))
		switch data.Type {
		case "response.output_item.added":
			callIDs[data.Item.ID] = data.Item.CallID
		case "response.function_call_arguments.delta":
			arguments[data.ItemID] += data.Delta
		case "response.output_item.done":
			if callIDs[data.Item.ID] != data.Item.CallID || arguments[data.Item.ID] != data.Item.Arguments {
				t.Fatalf("tool item relationship lost: %+v", data.Item)
			}
		}
	}
	wantOrder := []string{"response.output_item.added:0", "response.output_item.added:1", "response.function_call_arguments.delta:0", "response.function_call_arguments.delta:1", "response.function_call_arguments.delta:0", "vendor.unknown:0", "response.output_item.done:1", "response.output_item.done:0", "response.completed:0"}
	if strings.Join(order, ",") != strings.Join(wantOrder, ",") || callIDs["item_A"] != "call_A" || callIDs["item_B"] != "call_B" || arguments["item_A"] != `{"key":"A"}` || arguments["item_B"] != `{"key":"B"}` {
		t.Fatalf("event order or call relationships: %v, %v, %v", order, callIDs, arguments)
	}
	followup := []byte(` {"model":"gpt-5.4-mini","stream":true,"previous_response_id":"response_1","input":[{"type":"function_call_output","call_id":"call_B","output":"B result","unknown":{"keep":2}},{"type":"function_call_output","call_id":"call_A","output":"A result"}],"unknown":{"keep":3}} `)
	var round struct {
		Input []struct {
			CallID string `json:"call_id"`
		} `json:"input"`
	}
	if err := json.Unmarshal(followup, &round); err != nil || len(round.Input) != 2 || round.Input[0].CallID != callIDs["item_B"] || round.Input[1].CallID != callIDs["item_A"] {
		t.Fatalf("follow-up call ID relationship: %+v, %v", round, err)
	}
	select {
	case captured := <-upstream.Requests:
		if !bytes.Equal(captured.Body, initial) || captured.Header.Get("Authorization") != "Bearer synthetic" {
			t.Fatal("initial request bytes or credential changed")
		}
	default:
		t.Fatal("missing initial upstream request")
	}
	second := send(followup)
	_, err = io.Copy(io.Discard, second.Body)
	second.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case captured := <-upstream.Requests:
		if !bytes.Equal(captured.Body, followup) || captured.Header.Get("Authorization") != "Bearer synthetic" {
			t.Fatalf("follow-up request: exact bytes %t, credential %t", bytes.Equal(captured.Body, followup), captured.Header.Get("Authorization") == "Bearer synthetic")
		}
	default:
		t.Fatal("missing follow-up upstream request")
	}
}

func TestConfig(t *testing.T) {
	path := t.TempDir() + "/gateway.json"
	if err := os.WriteFile(path, []byte(`{"listen":"127.0.0.1:0","shutdown_timeout":"250ms"}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, duration, err := loadConfig([]string{"-config", path})
	if err != nil || c.Listen != "127.0.0.1:0" || duration != 250*time.Millisecond {
		t.Fatalf("config: %+v, %v, %v", c, duration, err)
	}
	for _, args := range [][]string{
		{"-config", path + ".missing"},
		{"-listen", "not-an-address"},
		{"-shutdown-timeout", "0s"},
	} {
		if _, _, err := loadConfig(args); err == nil {
			t.Errorf("expected error for %v", args)
		}
	}
	for _, body := range []string{`{"listen":`, `{"unexpected":1}`, `{} {}`, `{"listen":""}`} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadConfig([]string{"-config", path}); err == nil {
			t.Errorf("expected error for %q", body)
		}
	}
}

func TestInferenceConfig(t *testing.T) {
	const credential = "synthetic-secret-never-print"
	t.Setenv("PESTIROUTE_TEST_CREDENTIAL", credential)
	path := t.TempDir() + "/inference.json"
	valid := map[string]any{
		"listen": "127.0.0.1:0", "upstream_endpoint": "https://api.example.test/v1/responses",
		"upstream_credential_env": "PESTIROUTE_TEST_CREDENTIAL", "max_request_body_bytes": 1048576,
		"max_request_header_bytes": 8192, "connect_timeout": "1s", "tls_handshake_timeout": "2s",
		"response_header_timeout": "3s", "stream_idle_timeout": "4s",
	}
	check := func(fields map[string]any, args []string) (config, error) {
		t.Helper()
		data, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		c, _, err := loadConfig(append([]string{"-config", path}, args...))
		if strings.Contains(fmt.Sprintf("%+v %#v %v", c, c, err), credential) {
			t.Fatal("credential leaked in config or diagnostic")
		}
		return c, err
	}
	copyFields := func() map[string]any {
		fields := make(map[string]any, len(valid))
		for key, value := range valid {
			fields[key] = value
		}
		return fields
	}
	c, err := check(valid, nil)
	if err != nil || string(c.credential) != credential {
		t.Fatalf("valid inference configuration: %v", err)
	}
	if c, err = check(valid, []string{"-listen", "[::1]:0"}); err != nil || c.Listen != "[::1]:0" {
		t.Fatalf("flag precedence: %v", err)
	}
	fields := copyFields()
	fields["upstream_endpoint"] = "http://127.0.0.1:1234/v1/responses"
	if _, err := check(fields, nil); err != nil {
		t.Fatalf("local fake endpoint: %v", err)
	}

	for _, tc := range []struct {
		field string
		value any
		want  string
	}{
		{"upstream_endpoint", "http://example.test/v1/responses", "upstream_endpoint"},
		{"upstream_endpoint", "https://example.test/v1/other", "upstream_endpoint"},
		{"upstream_endpoint", "https://user:password@example.test/v1/responses", "upstream_endpoint"},
		{"upstream_endpoint", "https://example.test/v1/responses?x=1", "upstream_endpoint"},
		{"upstream_endpoint", "https://example.test/v1/responses#fragment", "upstream_endpoint"},
		{"upstream_endpoint", "https://example.test/v1/responses#", "upstream_endpoint"},
		{"upstream_endpoint", "http://localhost/v1/responses", "upstream_endpoint"},
		{"upstream_credential_env", "PESTIROUTE_UNSET_CREDENTIAL", "upstream_credential_env"},
		{"upstream_credential_env", "", "upstream_credential_env"},
		{"max_request_body_bytes", 0, "max_request_body_bytes"},
		{"max_request_header_bytes", -1, "max_request_header_bytes"},
		{"connect_timeout", "0s", "connect_timeout"},
		{"tls_handshake_timeout", "bad", "tls_handshake_timeout"},
		{"response_header_timeout", "-1s", "response_header_timeout"},
		{"stream_idle_timeout", "", "stream_idle_timeout"},
		{"upstream_endpoint", nil, "upstream_endpoint"},
		{"listen", "0.0.0.0:0", "loopback"},
		{"listen", "localhost:0", "loopback"},
		{"listen", "192.0.2.1:0", "loopback"},
	} {
		t.Run(fmt.Sprintf("%s=%v", tc.field, tc.value), func(t *testing.T) {
			fields := copyFields()
			fields[tc.field] = tc.value
			if _, err := check(fields, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %s error, got %v", tc.want, err)
			}
		})
	}
	for _, field := range []string{"upstream_endpoint", "upstream_credential_env", "max_request_body_bytes", "max_request_header_bytes", "connect_timeout", "tls_handshake_timeout", "response_header_timeout", "stream_idle_timeout"} {
		fields := copyFields()
		delete(fields, field)
		if _, err := check(fields, nil); err == nil || !strings.Contains(err.Error(), "partial inference configuration") {
			t.Errorf("missing %s: %v", field, err)
		}
	}
	fields = copyFields()
	fields["upstream_credential_env"] = "PESTIROUTE_EMPTY_CREDENTIAL"
	t.Setenv("PESTIROUTE_EMPTY_CREDENTIAL", "")
	if _, err := check(fields, nil); err == nil || !strings.Contains(err.Error(), "upstream_credential_env") {
		t.Errorf("empty credential: %v", err)
	}
	if _, err := check(map[string]any{"listen": "0.0.0.0:0"}, nil); err != nil {
		t.Errorf("probe-only bind: %v", err)
	}
}

func TestProbes(t *testing.T) {
	var ready atomic.Bool
	srv := &http.Server{Handler: probes(&ready)}
	// Use a real loopback listener so request-method routing is exercised.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(listener)
	defer srv.Close()
	base := "http://" + listener.Addr().String()
	check := func(path string, want int) {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: got %d, want %d", path, resp.StatusCode, want)
		}
	}
	check("/healthz", 200)
	check("/readyz", 503)
	ready.Store(true)
	check("/readyz", 200)
}

func TestLifecycleGateway(t *testing.T) {
	binary := t.TempDir() + "/gateway"
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build gateway: %v: %s", err, out)
	}
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "-listen", "127.0.0.1:0", "-shutdown-timeout", "200ms")
			stderr, err := cmd.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(stderr)
			line, err := reader.ReadString('\n')
			if err != nil || !strings.HasPrefix(line, "gateway: listening on ") {
				t.Fatalf("startup: %q: %v", line, err)
			}
			base := "http://" + strings.TrimSpace(strings.TrimPrefix(line, "gateway: listening on "))
			for _, path := range []string{"/healthz", "/readyz"} {
				resp, err := http.Get(base + path)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatalf("%s: status %d", path, resp.StatusCode)
				}
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			rest, _ := io.ReadAll(reader)
			if err := cmd.Wait(); err != nil {
				t.Fatalf("signal exit: %v; stderr: %s", err, rest)
			}
			if _, err := http.Get(base + "/healthz"); err == nil {
				t.Fatal("listener still open after shutdown")
			}
		})
	}

	path := t.TempDir() + "/broken.json"
	if err := os.WriteFile(path, []byte(`{"listen":`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-config", path}, {"-config", path + ".missing"}, {"-listen", "invalid"}} {
		cmd := exec.Command(binary, args...)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "gateway:") || !strings.Contains(string(out), args[1]) {
			t.Errorf("invalid %v: output %q, error %v", args, out, err)
		}
	}
}

func TestGatewayShutdownDrainsAndCancels(t *testing.T) {
	binary := t.TempDir() + "/gateway"
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build gateway: %v: %s", err, out)
	}
	for _, expire := range []bool{false, true} {
		name := "drains active work"
		if expire {
			name = "deadline cancels active work"
		}
		t.Run(name, func(t *testing.T) {
			gate := make(chan struct{})
			upstream := fakeupstream.New(fakeupstream.Response{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"status":"completed"}`), HeaderGate: gate})
			defer upstream.Close()
			configPath := t.TempDir() + "/gateway.json"
			settings := map[string]any{
				"listen": "127.0.0.1:0", "shutdown_timeout": "250ms", "upstream_endpoint": upstream.URL + "/v1/responses",
				"upstream_credential_env": "PESTIROUTE_SHUTDOWN_CREDENTIAL", "max_request_body_bytes": 1024,
				"max_request_header_bytes": 4096, "connect_timeout": "1s", "tls_handshake_timeout": "1s",
				"response_header_timeout": "2s", "stream_idle_timeout": "2s",
			}
			data, err := json.Marshal(settings)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(binary, "-config", configPath)
			cmd.Env = append(os.Environ(), "PESTIROUTE_SHUTDOWN_CREDENTIAL=synthetic")
			stderr, err := cmd.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			finished := false
			defer func() {
				if !finished {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()
			reader := bufio.NewReader(stderr)
			line, err := reader.ReadString('\n')
			if err != nil || !strings.HasPrefix(line, "gateway: listening on ") {
				t.Fatalf("startup: %q: %v", line, err)
			}
			base := "http://" + strings.TrimSpace(strings.TrimPrefix(line, "gateway: listening on "))
			response := make(chan struct {
				status int
				body   string
				err    error
			}, 1)
			go func() {
				resp, err := http.Post(base+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4-mini"}`))
				if err != nil {
					response <- struct {
						status int
						body   string
						err    error
					}{err: err}
					return
				}
				body, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				response <- struct {
					status int
					body   string
					err    error
				}{status: resp.StatusCode, body: string(body), err: readErr}
			}()
			captured := receiveRequest(t, upstream)
			shutdownStarted := time.Now()
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			for {
				resp, err := http.Get(base + "/readyz")
				if err != nil {
					break // Shutdown may close the listener before the next probe reaches /readyz.
				}
				status := resp.StatusCode
				resp.Body.Close()
				if status == http.StatusServiceUnavailable {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("readiness did not fall after SIGTERM")
				}
				time.Sleep(time.Millisecond)
			}
			cutoff, cutoffErr := http.Post(base+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4-mini"}`))
			if cutoffErr == nil {
				io.Copy(io.Discard, cutoff.Body)
				cutoff.Body.Close()
			}
			if cutoffErr == nil && cutoff.StatusCode != http.StatusServiceUnavailable || upstream.RequestCount() != 1 {
				status := 0
				if cutoff != nil {
					status = cutoff.StatusCode
				}
				t.Fatalf("shutdown admission: status=%d error=%v upstream requests=%d", status, cutoffErr, upstream.RequestCount())
			}
			if expire {
				select {
				case <-captured.Cancelled:
				case <-time.After(time.Second):
					t.Fatal("upstream work was not cancelled at drain deadline")
				}
			} else {
				close(gate)
				got := <-response
				if got.err != nil || got.status != http.StatusOK || got.body != `{"status":"completed"}` {
					t.Fatalf("drained response: status=%d body=%q err=%v", got.status, got.body, got.err)
				}
			}
			waited := make(chan error, 1)
			go func() { waited <- cmd.Wait() }()
			var waitErr error
			select {
			case waitErr = <-waited:
			case <-time.After(2 * time.Second):
				_ = cmd.Process.Kill()
				<-waited
				finished = true
				t.Fatal("gateway exceeded bounded shutdown margin")
			}
			finished = true
			if elapsed := time.Since(shutdownStarted); elapsed > 1500*time.Millisecond {
				t.Fatalf("shutdown took %s, exceeding 1.5s margin", elapsed)
			}
			if expire && waitErr == nil || !expire && waitErr != nil {
				t.Fatalf("gateway exit: %v (expire=%t)", waitErr, expire)
			}
			_, _ = io.ReadAll(reader)
		})
	}
}

func TestLifecycleAdmissionGuards(t *testing.T) {
	upstream := fakeupstream.New(fakeupstream.Response{Status: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"status":"completed"}`)})
	defer upstream.Close()
	c := config{
		UpstreamEndpoint: upstream.URL + "/v1/responses", UpstreamCredentialEnv: "TEST_UPSTREAM", credential: "synthetic",
		MaxRequestBodyBytes: 1024, MaxRequestHeaderBytes: 4096, ConnectTimeout: "1s", TLSHandshakeTimeout: "1s",
		ResponseHeaderTimeout: "1s", StreamIdleTimeout: "1s",
	}

	t.Run("unready probe", func(t *testing.T) {
		var ready, draining atomic.Bool
		h, closeTransport := handlerWithLifecycle(c, &ready, &draining, nil)
		defer closeTransport()
		server := httptest.NewServer(h)
		defer server.Close()
		resp, err := server.Client().Get(server.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("unready /readyz status=%d, want 503", resp.StatusCode)
		}
	})

	t.Run("draining rejects inference before dispatch", func(t *testing.T) {
		var ready, draining atomic.Bool
		ready.Store(true)
		draining.Store(true)
		h, closeTransport := handlerWithLifecycle(c, &ready, &draining, nil)
		defer closeTransport()
		server := httptest.NewServer(h)
		defer server.Close()
		resp, err := server.Client().Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4-mini"}`))
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusServiceUnavailable || string(body) != "gateway is shutting down\n" {
			t.Fatalf("draining response: status=%d body=%q read=%v", resp.StatusCode, body, readErr)
		}
		if got := upstream.RequestCount(); got != 0 {
			t.Fatalf("draining request dispatched upstream %d times", got)
		}
	})
}

func TestResponsesHeaderIsolationAndIdentityEncoding(t *testing.T) {
	for _, encoding := range []string{"", "gzip", "br", "*"} {
		for _, responseEncoding := range []string{"", "identity"} {
			t.Run("accept-encoding-"+encoding+"-response-"+responseEncoding, func(t *testing.T) {
				const credential = "selected-only"
				t.Setenv("TEST_UPSTREAM", credential)
				body := []byte(" {\"status\":\"completed\",\"opaque\":true} \n")
				responseHeaders := http.Header{
					"Content-Type": {"application/json"}, "Content-Length": {fmt.Sprint(len(body))},
					"Connection": {"X-Private"}, "X-Private": {"secret"},
					"Keep-Alive": {"timeout=5"}, "Set-Cookie": {"upstream=secret"}, "Authorization": {"Bearer private"}, "X-Public": {"visible"},
				}
				if responseEncoding != "" {
					responseHeaders.Set("Content-Encoding", responseEncoding)
				}
				upstream := fakeupstream.New(fakeupstream.Response{Status: 200, Header: responseHeaders, Body: body})
				defer upstream.Close()
				var ready atomic.Bool
				ready.Store(true)
				h, closeTransport := handler(config{UpstreamEndpoint: upstream.URL + "/v1/responses", UpstreamCredentialEnv: "TEST_UPSTREAM", credential: credential,
					MaxRequestBodyBytes: 1024, MaxRequestHeaderBytes: 4096, ConnectTimeout: "1s", TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s", StreamIdleTimeout: "1s"}, &ready)
				defer closeTransport()
				server := httptest.NewServer(h)
				defer server.Close()
				req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.4-mini"}`))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer client-secret")
				req.Header.Set("Cookie", "client=secret")
				req.Header.Set("Proxy-Authorization", "Basic client-secret")
				req.Header.Set("Connection", "X-Connection-Private, X-Proxy-Connection-Private")
				req.Header.Set("X-Connection-Private", "client-secret")
				req.Header.Set("Proxy-Connection", "keep-alive")
				req.Header.Set("X-Proxy-Connection-Private", "client-secret")
				req.Header.Set("X-Client-Safe", "safe")
				if encoding != "" {
					req.Header.Set("Accept-Encoding", encoding)
				}
				clientTransport := http.DefaultTransport.(*http.Transport).Clone()
				clientTransport.DisableCompression = true
				defer clientTransport.CloseIdleConnections()
				resp, err := (&http.Client{Transport: clientTransport}).Do(req)
				if err != nil {
					t.Fatal(err)
				}
				got, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if readErr != nil || resp.StatusCode != 200 || !bytes.Equal(got, body) || resp.ContentLength != int64(len(body)) {
					t.Fatalf("response status=%d bytes=%t length=%d err=%v", resp.StatusCode, bytes.Equal(got, body), resp.ContentLength, readErr)
				}
				for _, name := range []string{"Connection", "Keep-Alive", "X-Private", "Set-Cookie", "Authorization"} {
					if resp.Header.Get(name) != "" {
						t.Errorf("leaked response header %s: %q", name, resp.Header.Get(name))
					}
				}
				if resp.Header.Get("X-Public") != "visible" {
					t.Errorf("safe response header lost: %v", resp.Header)
				}
				captured := receiveRequest(t, upstream)
				if captured.Header.Get("Authorization") != "Bearer "+credential || captured.Header.Get("Accept-Encoding") != "identity" || captured.Header.Get("X-Client-Safe") != "safe" {
					t.Fatalf("selected/safe request headers missing: %v", captured.Header)
				}
				for _, name := range []string{"Cookie", "Proxy-Authorization", "Proxy-Connection", "Connection", "X-Connection-Private", "X-Proxy-Connection-Private"} {
					if captured.Header.Get(name) != "" {
						t.Errorf("leaked request header %s: %q", name, captured.Header.Get(name))
					}
				}
			})
		}
	}
}

func TestResponsesRequestEncodingRejectedBeforeExecute(t *testing.T) {
	upstream := fakeupstream.New(fakeupstream.Response{Status: 200, Body: []byte(`{"status":"completed"}`)})
	defer upstream.Close()
	var ready atomic.Bool
	ready.Store(true)
	var finalized atomic.Int32
	h, closeTransport := handlerWithFinalize(config{UpstreamEndpoint: upstream.URL + "/v1/responses", UpstreamCredentialEnv: "TEST_UPSTREAM", credential: "synthetic",
		MaxRequestBodyBytes: 1024, MaxRequestHeaderBytes: 4096, ConnectTimeout: "1s", TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s", StreamIdleTimeout: "1s"}, &ready, func(core.AttemptResult) { finalized.Add(1) })
	defer closeTransport()
	server := httptest.NewServer(h)
	defer server.Close()
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.4-mini"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	var gatewayBody struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	decodeErr := json.Unmarshal(body, &gatewayBody)
	if readErr != nil || decodeErr != nil || resp.StatusCode != http.StatusBadRequest || gatewayBody.Error.Type != string(core.CategoryUnsupportedFeature) || upstream.RequestCount() != 0 || finalized.Load() != 0 {
		t.Fatalf("encoded request status=%d type=%q upstream=%d finalized=%d read=%v decode=%v", resp.StatusCode, gatewayBody.Error.Type, upstream.RequestCount(), finalized.Load(), readErr, decodeErr)
	}
}

func TestResponsesUpstreamEncodingRejectedPreHead(t *testing.T) {
	for _, encoding := range []string{"gzip", "identity, gzip", "deflate"} {
		for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
			t.Run(fmt.Sprintf("%s-%d", strings.ReplaceAll(encoding, ", ", "-"), status), func(t *testing.T) {
				gate := make(chan struct{})
				upstream := fakeupstream.New(fakeupstream.Response{Status: status, Header: http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {encoding}}, Steps: []fakeupstream.Step{{Data: []byte("encoded-private-body")}, {Gate: gate, Data: []byte("must-not-be-read")}}})
				defer upstream.Close()
				defer close(gate)
				finals := make(chan core.AttemptResult, 2)
				var finalized atomic.Int32
				server, closeTransport := cancellationServer(t, upstream, finals, &finalized)
				defer closeTransport()
				defer server.Close()
				resp, err := server.Client().Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4-mini"}`))
				if err != nil {
					t.Fatal(err)
				}
				body, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				wantBody := fmt.Sprintf(`{"error":{"message":"Unsupported upstream response encoding (HTTP %d)","type":"unavailable","code":"unsupported_response_encoding"}}`, status)
				if readErr != nil || resp.StatusCode != http.StatusServiceUnavailable || string(body) != wantBody {
					t.Fatalf("encoded upstream response status=%d body=%q read=%v", resp.StatusCode, body, readErr)
				}
				captured := receiveRequest(t, upstream)
				waitCancelled(t, captured)
				select {
				case result := <-finals:
					if result.Committed || result.Outcome != core.OutcomeFailed || result.Error == nil || result.Error.Category != core.CategoryUnavailable || result.Error.Retryable || result.Error.RetryDisposition != core.RetryUnknown || len(finals) != 0 {
						t.Fatalf("encoded response finalization: %+v", result)
					}
				case <-time.After(time.Second):
					t.Fatal("encoded response attempt was not finalized")
				}
				assertCounts(t, upstream, &finalized, 1)
			})
		}
	}
}

func TestLifecycleCloseTransportReleasesIdleConnection(t *testing.T) {
	closed := make(chan struct{}, 1)
	idle := make(chan struct{}, 1)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"completed"}`))
	}))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateIdle:
			select {
			case idle <- struct{}{}:
			default:
			}
		case http.StateClosed:
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	upstream.Start()
	defer upstream.Close()

	var ready, draining atomic.Bool
	ready.Store(true)
	h, closeTransport := handlerWithLifecycle(config{
		UpstreamEndpoint: upstream.URL + "/v1/responses", UpstreamCredentialEnv: "TEST_UPSTREAM", credential: "synthetic",
		MaxRequestBodyBytes: 1024, MaxRequestHeaderBytes: 4096, ConnectTimeout: "1s", TLSHandshakeTimeout: "1s",
		ResponseHeaderTimeout: "1s", StreamIdleTimeout: "1s",
	}, &ready, &draining, nil)
	server := httptest.NewServer(h)
	defer server.Close()
	resp, err := server.Client().Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4-mini"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("inference status=%d", resp.StatusCode)
	}
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("upstream connection did not become idle")
	}
	closeTransport()
	closeTransport()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not release the pooled upstream connection")
	}
}

func TestGatewayConcurrentRequestIsolation(t *testing.T) {
	type captured struct {
		marker    string
		body      []byte
		cancelled <-chan struct{}
	}
	requests := make(chan captured, 2)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		c := captured{marker: r.Header.Get("X-Request-Marker"), body: body, cancelled: r.Context().Done()}
		requests <- c
		if c.marker == "cancel-me" {
			<-r.Context().Done()
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed","marker":"keep-me"}`))
	}))
	defer upstream.Close()
	finals := make(chan core.AttemptResult, 3)
	var finalized atomic.Int32
	var ready atomic.Bool
	var draining atomic.Bool
	ready.Store(true)
	h, closeTransport := handlerWithLifecycle(config{
		UpstreamEndpoint: upstream.URL + "/v1/responses", UpstreamCredentialEnv: "TEST_UPSTREAM", credential: "synthetic",
		MaxRequestBodyBytes: 1024, MaxRequestHeaderBytes: 4096, ConnectTimeout: "1s", TLSHandshakeTimeout: "1s",
		ResponseHeaderTimeout: "1s", StreamIdleTimeout: "2s",
	}, &ready, &draining, func(r core.AttemptResult) { finalized.Add(1); finals <- r })
	defer closeTransport()
	server := httptest.NewServer(h)
	defer server.Close()
	type response struct {
		status int
		body   []byte
		err    error
	}
	contexts := make([]context.CancelFunc, 2)
	responses := make([]<-chan response, 2)
	for i, marker := range []string{"cancel-me", "keep-me"} {
		ctx, cancel := context.WithCancel(context.Background())
		contexts[i] = cancel
		ch := make(chan response, 1)
		responses[i] = ch
		go func(marker string) {
			body := fmt.Sprintf(`{"model":"gpt-5.4-mini","marker":%q}`, marker)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(body))
			if err != nil {
				ch <- response{err: err}
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Request-Marker", marker)
			resp, err := server.Client().Do(req)
			if err != nil {
				ch <- response{err: err}
				return
			}
			got, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			ch <- response{status: resp.StatusCode, body: got, err: err}
		}(marker)
	}
	defer func() { contexts[0](); contexts[1]() }()
	seen := map[string]captured{}
	for range 2 {
		select {
		case req := <-requests:
			seen[req.marker] = req
		case <-time.After(time.Second):
			a, b := <-responses[0], <-responses[1]
			t.Fatalf("both requests did not reach upstream (received %v; client results status=%d body=%s err=%v, status=%d body=%s err=%v)", len(seen), a.status, a.body, a.err, b.status, b.body, b.err)
		}
	}
	draining.Store(true)
	rejected, err := server.Client().Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4-mini","marker":"during-drain"}`))
	if err != nil {
		t.Fatal(err)
	}
	rejectedBody, readErr := io.ReadAll(rejected.Body)
	rejected.Body.Close()
	if readErr != nil || rejected.StatusCode != http.StatusServiceUnavailable || string(rejectedBody) != "gateway is shutting down\n" {
		t.Fatalf("concurrent drain admission: status=%d body=%q err=%v", rejected.StatusCode, rejectedBody, readErr)
	}
	for marker, want := range map[string]string{"cancel-me": `"marker":"cancel-me"`, "keep-me": `"marker":"keep-me"`} {
		if got := seen[marker]; !bytes.Contains(got.body, []byte(want)) || got.marker != marker {
			t.Fatalf("request crossed markers: header=%q body=%s", got.marker, got.body)
		}
	}
	contexts[0]()
	select {
	case <-seen["cancel-me"].cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancelled request did not cancel upstream")
	}
	select {
	case got := <-responses[0]:
		if got.err == nil {
			t.Fatalf("cancelled client unexpectedly received a response: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled client did not return")
	}
	close(release)
	select {
	case got := <-responses[1]:
		if got.err != nil || got.status != http.StatusOK || string(got.body) != `{"status":"completed","marker":"keep-me"}` {
			t.Fatalf("concurrent successful response: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated request did not complete")
	}
	draining.Store(false)
	followup, err := server.Client().Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4-mini","marker":"after"}`))
	if err != nil {
		t.Fatal(err)
	}
	followupBody, readErr := io.ReadAll(followup.Body)
	followup.Body.Close()
	if readErr != nil || followup.StatusCode != http.StatusOK || string(followupBody) != `{"status":"completed","marker":"keep-me"}` {
		t.Fatalf("post-cancellation follow-up: status=%d body=%q err=%v", followup.StatusCode, followupBody, readErr)
	}
	select {
	case req := <-requests:
		if req.marker != "" || !bytes.Contains(req.body, []byte(`"marker":"after"`)) {
			t.Fatalf("follow-up request state leaked: header=%q body=%s", req.marker, req.body)
		}
	case <-time.After(time.Second):
		t.Fatal("follow-up request did not reach upstream")
	}
	results := make(map[string]core.AttemptResult)
	for range 3 {
		select {
		case result := <-finals:
			results[result.RequestID] = result
		case <-time.After(time.Second):
			t.Fatal("attempt was not finalized")
		}
	}
	if len(results) != 3 || finalized.Load() != 3 {
		t.Fatalf("finalized attempts: count=%d results=%+v", finalized.Load(), results)
	}
	var cancelled, succeeded int
	for _, result := range results {
		if result.Scope.ID == "" || result.RequestID == "" || result.Usage.Source != core.UsageUnknown {
			t.Fatalf("attempt identity/usage: %+v", result)
		}
		switch result.Outcome {
		case core.OutcomeCancelled, core.OutcomeIncomplete:
			if result.Outcome != core.OutcomeCancelled {
				t.Fatalf("client cancellation was not classified as cancelled: %+v", result)
			}
			cancelled++
		case core.OutcomeSucceeded:
			succeeded++
		default:
			t.Fatalf("unexpected terminal result: %+v", result)
		}
	}
	if cancelled != 1 || succeeded != 2 {
		t.Fatalf("cancellation corrupted concurrent completion: %+v", results)
	}
}
