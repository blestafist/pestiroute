package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/connector/external"
	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

const chatRequest = ` {"model":"model-a","messages":[{"role":"user","content":"ping"}],"future":{"keep": [1,  2]}} `
const chatResponse = ` {"id":"chatcmpl-fixture","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}} `

func TestChatProtectedStartup_Validation(t *testing.T) {
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	_, dbPath, keyPath := protectedFixture(t)
	yaml := func(baseURL, protocol, settings, process string) string {
		return `version: 1
server: {listen: "127.0.0.1:0", max_request_bytes: 1048576, shutdown_timeout: 1s}
storage: {driver: sqlite, path: "` + dbPath + `"}
secrets: {master_key_file: "` + keyPath + `"}
connectors:
  - id: upstream
    kind: connector
    implementation: pestiroute.chat-completions.native
    protocols: [` + protocol + `]
    settings:
      base_url: ` + baseURL + `
      upstream_protocol: ` + protocol + `
      mode: native
      credential_env: PROTECTED_TEST_CREDENTIAL
      max_request_body_bytes: 1048576
      max_request_header_bytes: 8192
      connect_timeout: 1s
      tls_handshake_timeout: 1s
      response_header_timeout: 1s
      stream_idle_timeout: 1s
` + settings + process + `routes:
  - id: chat-route
    protocol: openai.chat-completions.v1
    mode: native
    model: model-a
    adapter: pestiroute.chat-completions.native
    policy: standard
    budget: {unknown_estimate: reserve, conservative_tokens: 10}
    targets: [{connector: upstream, account: account-a}]
policies: {standard: policy-id-a}
`
	}
	valid := yaml("https://api.example/v1/chat/completions", chatProtocol, "", "")
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	write := func(data string) error { return os.WriteFile(path, []byte(data), 0600) }
	if err := write(valid); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProtectedYAML(path); err != nil {
		t.Fatalf("valid Chat YAML rejected: %v", err)
	}
	for name, data := range map[string]string{
		"wrong endpoint path":     strings.Replace(valid, "https://api.example/v1/chat/completions", "https://api.example/v1/responses", 1),
		"wrong upstream protocol": strings.Replace(valid, "upstream_protocol: openai.chat-completions.v1", "upstream_protocol: openai.responses.v1", 1),
		"cross protocol route":    strings.Replace(valid, "protocol: openai.chat-completions.v1\n    mode", "protocol: openai.responses.v1\n    mode", 1),
		"unknown setting":         strings.Replace(valid, "      mode: native", "      model: forbidden\n      mode: native", 1),
		"process configuration":   strings.Replace(valid, "    protocols:", "    process: {executable: /bin/true, args: [], working_directory: /tmp, environment_allowlist: []}\n    protocols:", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := write(data); err != nil {
				t.Fatal(err)
			}
			if _, err := loadProtectedYAML(path); err == nil {
				t.Fatal("invalid Chat configuration accepted")
			}
		})
	}
	if err := write(strings.Replace(valid, "https://api.example", "http://127.0.0.1:9000", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProtectedYAML(path); err != nil {
		t.Fatalf("numeric loopback HTTP endpoint rejected: %v", err)
	}
}

func TestChatTopologyRejectsProcessConnector(t *testing.T) {
	t.Setenv("CHAT_TOPOLOGY_TOKEN", "synthetic")
	c := config{
		Listen: "127.0.0.1:0",
		Components: []topologyComponent{
			{ID: "chat-adapter", Implementation: chatImplementation, Kind: core.ComponentAdapter},
			{ID: "chat-upstream", Implementation: chatImplementation, Kind: core.ComponentConnector,
				Endpoint: "http://127.0.0.1:9000/v1/chat/completions", CredentialEnv: "CHAT_TOPOLOGY_TOKEN",
				MaxBodyBytes: 1 << 20, MaxHeaderBytes: 8192, ConnectTimeout: "1s", TLSTimeout: "1s", HeaderTimeout: "1s", IdleTimeout: "1s",
				Process: &external.ProcessConfig{}},
		},
		Routes: []topologyRoute{{Protocol: chatProtocol, Mode: core.ModeNative, Model: "model-a", Account: "account-a", Adapter: "chat-adapter", Connector: "chat-upstream"}},
	}
	if err := validateTopology(&c); err == nil || !strings.Contains(err.Error(), "process configuration is not supported") {
		t.Fatalf("Chat process config validation error = %v, want fail-closed rejection", err)
	}
}

type chatProtectedFixture struct {
	prepared config
	server   *httptest.Server
	key      string
	keyID    string
	backend  *httptest.Server
}

func newChatProtectedFixture(t *testing.T, backend http.Handler, limits ...int64) *chatProtectedFixture {
	t.Helper()
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	_, dbPath, keyPath := protectedFixture(t)
	upstream := httptest.NewServer(backend)
	t.Cleanup(upstream.Close)
	c := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
	c.protected.Connectors[0].Implementation = chatImplementation
	c.protected.Connectors[0].Protocols = []string{chatProtocol}
	c.protected.Connectors[0].Settings.BaseURL = upstream.URL + "/v1/chat/completions"
	c.protected.Connectors[0].Settings.UpstreamProtocol = chatProtocol
	c.protected.Routes[0].Protocol = chatProtocol
	c.protected.Routes[0].Adapter = chatImplementation
	prepared, err := prepareProtectedConfig(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	policyStore := sqlite.NewKeyPolicies(prepared.runtimeDB)
	policy, err := policyStore.GetLatest(context.Background(), "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(limits) == 2 {
		policy, err = policyStore.Update(context.Background(), policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: policy.Models, Connectors: policy.Connectors, RPM: limits[0], TPM: limits[1]})
		if err != nil {
			t.Fatal(err)
		}
	}
	issued, err := sqlite.NewVirtualKeys(prepared.runtimeDB).Create(context.Background(), sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	var ready, draining atomic.Bool
	h, closeComponents, err := composeHandler(prepared, &ready, &draining, nil)
	if err != nil {
		_ = prepared.runtimeDB.Close()
		_ = prepared.processLock.Close()
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	t.Cleanup(func() {
		server.Close()
		_ = closeComponents(context.Background())
		_ = prepared.runtimeDB.Close()
		_ = prepared.processLock.Close()
	})
	return &chatProtectedFixture{prepared: prepared, server: server, key: issued.Secret, keyID: issued.ID, backend: upstream}
}

func chatRequestTo(t *testing.T, f *chatProtectedFixture, path, key, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestChatProtectedGateway_JSON(t *testing.T) {
	var request []byte
	responseBody := []byte(chatResponse)
	f := newChatProtectedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		request, err = io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Fixture", "preserved")
		_, _ = w.Write(responseBody)
	}))
	resp := chatRequestTo(t, f, "/v1/chat/completions", f.key, chatRequest)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(got, []byte(chatResponse)) || !bytes.Equal(request, []byte(chatRequest)) || resp.Header.Get("X-Fixture") != "preserved" {
		t.Fatalf("Chat JSON status=%d body=%q request=%q err=%v", resp.StatusCode, got, request, err)
	}
	requests, err := sqlite.NewLedger(f.prepared.runtimeDB).QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 10})
	if err != nil || len(requests) != 1 || len(requests[0].Attempts) != 1 {
		t.Fatalf("accounted requests=%+v err=%v", requests, err)
	}
	a := requests[0].Attempts[0]
	if a.Attempt.State != "succeeded" || a.Usage == nil || a.Usage.InputTokens == nil || *a.Usage.InputTokens != 3 || a.Usage.OutputTokens == nil || *a.Usage.OutputTokens != 2 || a.Reservation.State != "settled" {
		t.Fatalf("usage not settled once: %+v", a)
	}
	responseBody = []byte(`{"id":"chatcmpl-unknown","choices":[]}`)
	resp = chatRequestTo(t, f, "/v1/chat/completions", f.key, chatRequest)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	requests, err = sqlite.NewLedger(f.prepared.runtimeDB).QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 10})
	if err != nil || len(requests) != 2 || len(requests[1].Attempts) != 1 || requests[1].Attempts[0].Usage == nil || requests[1].Attempts[0].Usage.Source != "unknown" || requests[1].Attempts[0].Usage.InputTokens != nil {
		t.Fatalf("missing usage was not unknown: requests=%+v err=%v", requests, err)
	}
}

func TestChatProtectedGateway_SSE(t *testing.T) {
	first := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	tail := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"
	release, waiting := make(chan struct{}), make(chan struct{}, 1)
	f := newChatProtectedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		waiting <- struct{}{}
		<-release
		_, _ = io.WriteString(w, tail)
	}))
	f.prepared.Components[0].Capabilities = map[string]string{"llm.streaming": "supported"}
	// Recompose with the fixture-only capability at this exact Connector scope.
	var ready, draining atomic.Bool
	h, closeComponents, err := composeHandler(f.prepared, &ready, &draining, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeComponents(context.Background()) })
	old := f.server
	old.Close()
	f.server = httptest.NewServer(h)
	t.Cleanup(f.server.Close)
	resp := chatRequestTo(t, f, "/v1/chat/completions", f.key, strings.Replace(chatRequest, `"messages"`, `"stream":true,"stream_options":{"include_usage":true},"messages"`, 1))
	defer resp.Body.Close()
	gotFirst := make([]byte, len(first))
	if _, err := io.ReadFull(resp.Body, gotFirst); err != nil || string(gotFirst) != first {
		t.Fatalf("first SSE event=%q err=%v", gotFirst, err)
	}
	select {
	case <-waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("fake upstream did not hold the second SSE event")
	}
	select {
	case <-release:
		t.Fatal("upstream was released before client received first SSE event")
	default:
	}
	close(release)
	gotTail, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" || !bytes.Equal(gotTail, []byte(tail)) {
		t.Fatalf("Chat SSE status=%d content-type=%q tail=%q err=%v", resp.StatusCode, resp.Header.Get("Content-Type"), gotTail, err)
	}
	requests, err := sqlite.NewLedger(f.prepared.runtimeDB).QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 10})
	if err != nil || len(requests) != 1 || len(requests[0].Attempts) != 1 {
		t.Fatalf("SSE accounting requests=%+v err=%v", requests, err)
	}
	usage := requests[0].Attempts[0].Usage
	if usage == nil || usage.InputTokens == nil || *usage.InputTokens != 3 || usage.OutputTokens == nil || *usage.OutputTokens != 2 || requests[0].Attempts[0].Reservation.State != "settled" {
		t.Fatalf("usage-only SSE chunk not settled: %+v", requests[0].Attempts[0])
	}
}

func TestChatProtectedGateway_AuthAndDenials(t *testing.T) {
	var calls atomic.Int32
	f := newChatProtectedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatResponse)
	}))
	for name, key := range map[string]string{"missing": "", "invalid": "not-a-key"} {
		t.Run(name, func(t *testing.T) {
			resp := chatRequestTo(t, f, "/v1/chat/completions", key, chatRequest)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status=%d", resp.StatusCode)
			}
		})
	}
	resp := chatRequestTo(t, f, "/v1/chat/completions", f.key, `{"model":"other","messages":[{"role":"user","content":"x"}]}`)
	_ = resp.Body.Close()
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("unauthorized model status=%d", resp.StatusCode)
	}
	if calls.Load() != 0 {
		t.Fatalf("denied requests reached upstream %d times", calls.Load())
	}
	if _, err := sqlite.NewVirtualKeys(f.prepared.runtimeDB).Revoke(context.Background(), f.keyID); err != nil {
		t.Fatal(err)
	}
	resp = chatRequestTo(t, f, "/v1/chat/completions", f.key, chatRequest)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || calls.Load() != 0 {
		t.Fatalf("revoked key status=%d upstream_calls=%d", resp.StatusCode, calls.Load())
	}
	for name, limits := range map[string][2]int64{"rpm": {0, 1000}, "tpm": {10, 5}} {
		t.Run(name, func(t *testing.T) {
			var limitedCalls atomic.Int32
			limited := newChatProtectedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				limitedCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, chatResponse)
			}), limits[0], limits[1])
			resp := chatRequestTo(t, limited, "/v1/chat/completions", limited.key, chatRequest)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusTooManyRequests || limitedCalls.Load() != 0 {
				t.Fatalf("limit status=%d upstream_calls=%d", resp.StatusCode, limitedCalls.Load())
			}
		})
	}
}

func TestChatProtectedGateway_ProtocolIsolation(t *testing.T) {
	var calls atomic.Int32
	f := newChatProtectedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatResponse)
	}))
	for path, body := range map[string]string{
		"/v1/responses":        chatRequest,
		"/v1/chat/completions": `{"model":"model-a","input":"not Chat"}`,
	} {
		resp := chatRequestTo(t, f, path, f.key, body)
		_ = resp.Body.Close()
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Fatalf("protocol mismatch path=%s status=%d", path, resp.StatusCode)
		}
	}
	resp := chatRequestTo(t, f, "/v1/chat/completions", f.key, `{"model":"unconfigured","messages":[{"role":"user","content":"x"}]}`)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(`"code":"unsupported_target"`)) || calls.Load() != 0 {
		t.Fatalf("unconfigured target status=%d body=%q upstream_calls=%d err=%v", resp.StatusCode, body, calls.Load(), err)
	}
}

func TestChatProtectedGateway_Cancellation(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	f := newChatProtectedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	f.prepared.Components[0].Capabilities = map[string]string{"llm.streaming": "supported"}
	var ready, draining atomic.Bool
	h, closeComponents, err := composeHandler(f.prepared, &ready, &draining, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeComponents(context.Background())
	f.server.Close()
	f.server = httptest.NewServer(h)
	defer f.server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.server.URL+"/v1/chat/completions", strings.NewReader(strings.Replace(chatRequest, `"messages"`, `"stream":true,"messages"`, 1)))
	req.Header.Set("Authorization", "Bearer "+f.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("fake upstream was not reached")
	}
	cancel()
	_ = resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("client cancellation did not cancel upstream")
	}
	var requests []sqlite.RequestUsage
	var queryErr error
	deadline := time.Now().Add(3 * time.Second)
	for {
		requests, queryErr = sqlite.NewLedger(f.prepared.runtimeDB).QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 10})
		if queryErr == nil && len(requests) == 1 && len(requests[0].Attempts) == 1 && requests[0].Attempts[0].Attempt.State == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled attempt=%+v err=%v", requests, queryErr)
		}
		time.Sleep(time.Millisecond)
	}
	_ = closeComponents(context.Background())
}
