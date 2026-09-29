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
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
)

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
	requestBody := []byte(" { \"model\" : \"gpt-4.1-mini-2025-04-14\", \"unknown\": {\"nested\": [ 1, {\"extra\": true} ]} } \n")
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
	requestBody := []byte(`{ "model": "gpt-4.1-mini-2025-04-14", "stream": true, "extra": {"opaque": 1} }`)
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
	initial := []byte(`{"model":"gpt-4.1-mini-2025-04-14","stream":true,"unknown":{"keep":1}}`)
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
	followup := []byte(` {"model":"gpt-4.1-mini-2025-04-14","stream":true,"previous_response_id":"response_1","input":[{"type":"function_call_output","call_id":"call_B","output":"B result","unknown":{"keep":2}},{"type":"function_call_output","call_id":"call_A","output":"A result"}],"unknown":{"keep":3}} `)
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

func TestGatewayLifecycle(t *testing.T) {
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
