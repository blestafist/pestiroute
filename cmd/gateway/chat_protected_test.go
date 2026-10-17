package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/connector/external"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
	"gopkg.in/yaml.v3"
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
	for name, declarations := range map[string]string{
		"valid explicit capabilities": "      capabilities: {llm.streaming: supported, llm.tools: unknown}\n",
		"empty capability name":       "      capabilities: {\"\": supported}\n",
		"invalid capability state":    "      capabilities: {llm.streaming: maybe}\n",
	} {
		t.Run(name, func(t *testing.T) {
			data := strings.Replace(valid, "      mode: native\n", "      mode: native\n"+declarations, 1)
			if err := write(data); err != nil {
				t.Fatal(err)
			}
			loaded, err := loadProtectedYAML(path)
			if name == "valid explicit capabilities" {
				if err != nil || loaded.Connectors[0].Settings.Capabilities["llm.streaming"] != "supported" || loaded.Connectors[0].Settings.Capabilities["llm.tools"] != "unknown" {
					t.Fatalf("explicit capabilities not loaded: settings=%+v err=%v", loaded.Connectors[0].Settings, err)
				}
			} else if err == nil {
				t.Fatal("invalid capability declaration accepted")
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

func newChatProtectedFixtureWithYAMLCapabilities(t *testing.T, backend http.Handler, capabilities map[string]string, idleTimeout ...string) *chatProtectedFixture {
	t.Helper()
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	_, dbPath, keyPath := protectedFixture(t)
	upstream := httptest.NewServer(backend)
	t.Cleanup(upstream.Close)
	c := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
	capabilityYAML := ""
	if len(capabilities) != 0 {
		encoded, err := yaml.Marshal(capabilities)
		if err != nil {
			t.Fatal(err)
		}
		capabilityYAML = "      capabilities:\n"
		for line := range strings.SplitSeq(string(encoded), "\n") {
			if line != "" {
				capabilityYAML += "        " + line + "\n"
			}
		}
	}
	idle := "1s"
	if len(idleTimeout) != 0 {
		idle = idleTimeout[0]
	}
	data := fmt.Appendf(nil, `version: 1
server: {listen: "127.0.0.1:0", max_request_bytes: 1048576, shutdown_timeout: 5s}
storage: {driver: sqlite, path: %q}
secrets: {master_key_file: %q}
connectors:
  - id: upstream
    kind: connector
    implementation: %s
    protocols: [%s]
    settings:
      base_url: %s/v1/chat/completions
      upstream_protocol: %s
      mode: native
      credential_env: PROTECTED_TEST_CREDENTIAL
      max_request_body_bytes: 1048576
      max_request_header_bytes: 8192
      connect_timeout: 1s
      tls_handshake_timeout: 1s
      response_header_timeout: 1s
      stream_idle_timeout: %s
%sroutes:
  - id: chat-route
    protocol: %s
    mode: native
    model: model-a
    adapter: %s
    policy: standard
    budget: {unknown_estimate: reserve, conservative_tokens: 10}
    targets: [{connector: upstream, account: account-a}]
policies: {standard: policy-id-a}
`, dbPath, keyPath, chatImplementation, chatProtocol, upstream.URL, chatProtocol, idle, capabilityYAML, chatProtocol, chatImplementation)
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadProtectedYAML(path)
	if err != nil {
		t.Fatalf("load protected Chat YAML: %v", err)
	}
	c.protected = &loaded
	prepared, err := prepareProtectedConfig(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := sqlite.NewKeyPolicies(prepared.runtimeDB).GetLatest(context.Background(), "policy-id-a")
	if err != nil {
		t.Fatal(err)
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

func waitChatAccounting(t *testing.T, f *chatProtectedFixture, count int) []sqlite.RequestUsage {
	t.Helper()
	// The client can receive the complete body before the server consumes the
	// terminal frame and persists accounting. Wait for persistence, not HTTP EOF.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ledger := sqlite.NewLedger(f.prepared.runtimeDB)
	for {
		requests, err := ledger.QueryRequests(ctx, sqlite.RequestFilter{Limit: 10})
		if err != nil {
			t.Fatalf("query Chat accounting: %v", err)
		}
		finished := len(requests) == count
		for _, request := range requests {
			if request.Request.FinishedAt == nil {
				finished = false
			}
		}
		if finished {
			return requests
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Chat accounting did not finish: requests=%+v err=%v", requests, ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
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
	requests := waitChatAccounting(t, f, 1)
	if len(requests) != 1 || len(requests[0].Attempts) != 1 {
		t.Fatalf("accounted requests=%+v", requests)
	}
	a := requests[0].Attempts[0]
	if a.Attempt.State != "succeeded" || a.Usage == nil || a.Usage.InputTokens == nil || *a.Usage.InputTokens != 3 || a.Usage.OutputTokens == nil || *a.Usage.OutputTokens != 2 || a.Reservation.State != "settled" {
		t.Fatalf("usage not settled once: %+v", a)
	}
	responseBody = []byte(`{"id":"chatcmpl-unknown","choices":[]}`)
	resp = chatRequestTo(t, f, "/v1/chat/completions", f.key, chatRequest)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	requests = waitChatAccounting(t, f, 2)
	if len(requests) != 2 || len(requests[1].Attempts) != 1 || requests[1].Attempts[0].Usage == nil || requests[1].Attempts[0].Usage.Source != "unknown" || requests[1].Attempts[0].Usage.InputTokens != nil {
		t.Fatalf("missing usage was not unknown: requests=%+v", requests)
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
	requests := waitChatAccounting(t, f, 1)
	if len(requests) != 1 || len(requests[0].Attempts) != 1 {
		t.Fatalf("SSE accounting requests=%+v", requests)
	}
	usage := requests[0].Attempts[0].Usage
	if usage == nil || usage.InputTokens == nil || *usage.InputTokens != 3 || usage.OutputTokens == nil || *usage.OutputTokens != 2 || requests[0].Attempts[0].Reservation.State != "settled" {
		t.Fatalf("usage-only SSE chunk not settled: %+v", requests[0].Attempts[0])
	}
}

func TestChatProtectedGateway_YAMLCapabilities(t *testing.T) {
	var calls atomic.Int32
	f := newChatProtectedFixtureWithYAMLCapabilities(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatResponse)
	}), map[string]string{
		"llm.streaming": "supported", "llm.tools": "supported",
		"llm.tools.parallel": "supported", "llm.structured_output": "supported",
	})
	if got := f.prepared.Components[0].Capabilities; got["llm.streaming"] != "supported" || got["llm.tools"] != "supported" || got["llm.tools.parallel"] != "supported" || got["llm.structured_output"] != "supported" {
		t.Fatalf("protected startup did not propagate YAML capabilities: %#v", got)
	}
	for name, body := range map[string]string{
		"streaming":         `{"model":"model-a","stream":true,"messages":[{"role":"user","content":"ping"}]}`,
		"tools":             `{"model":"model-a","tools":[{"type":"function","function":{"name":"lookup"}}],"messages":[{"role":"user","content":"ping"}]}`,
		"parallel tools":    `{"model":"model-a","parallel_tool_calls":true,"tools":[{"type":"function","function":{"name":"lookup"}}],"messages":[{"role":"user","content":"ping"}]}`,
		"structured output": `{"model":"model-a","response_format":{"type":"json_object"},"messages":[{"role":"user","content":"ping"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			resp := chatRequestTo(t, f, "/v1/chat/completions", f.key, body)
			_, err := io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("configured %s request status=%d err=%v", name, resp.StatusCode, err)
			}
		})
	}
	if calls.Load() != 4 {
		t.Fatalf("upstream calls=%d, want 4", calls.Load())
	}
}

func TestChatProtectedGateway_YAMLCapabilitiesFailClosed(t *testing.T) {
	requests := map[string]string{
		"llm.streaming":         `{"model":"model-a","stream":true,"messages":[{"role":"user","content":"ping"}]}`,
		"llm.tools":             `{"model":"model-a","tools":[{"type":"function","function":{"name":"lookup"}}],"messages":[{"role":"user","content":"ping"}]}`,
		"llm.tools.parallel":    `{"model":"model-a","parallel_tool_calls":true,"messages":[{"role":"user","content":"ping"}]}`,
		"llm.structured_output": `{"model":"model-a","response_format":{"type":"json_object"},"messages":[{"role":"user","content":"ping"}]}`,
	}
	for state, declaration := range map[string]string{
		"undeclared":  "",
		"unsupported": "unsupported",
	} {
		t.Run(state, func(t *testing.T) {
			var calls atomic.Int32
			capabilities := map[string]string{}
			if state == "unsupported" {
				for capability := range requests {
					capabilities[capability] = declaration
				}
			}
			f := newChatProtectedFixtureWithYAMLCapabilities(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}), capabilities)
			for capability, requestBody := range requests {
				resp := chatRequestTo(t, f, "/v1/chat/completions", f.key, requestBody)
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil || resp.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(`"code":"unsupported_capability"`)) || calls.Load() != 0 {
					t.Fatalf("%s %s capability was not denied before dispatch: status=%d body=%s calls=%d err=%v", state, capability, resp.StatusCode, body, calls.Load(), err)
				}
			}
		})
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

func TestChatProtectedGateway_MixedProtocolRoutesDispatchSeparately(t *testing.T) {
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	t.Setenv("CHAT_TEST_CREDENTIAL", "synthetic")
	_, dbPath, keyPath := protectedFixture(t)
	var responsesCalls, chatCalls atomic.Int32
	responsesBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		responsesCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp-fixture","output":[]}`)
	}))
	defer responsesBackend.Close()
	chatBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatResponse)
	}))
	defer chatBackend.Close()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	masterKey, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewAccounts(db).Create(context.Background(), sqlite.Account{ID: "chat-account", Connector: "chat-upstream", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	envelope, err := secure.Seal(masterKey, 2, "test-v1", "credentials", "CHAT_TEST_CREDENTIAL", "chat-account", []byte("synthetic"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(db).Create(context.Background(), sqlite.Credential{ID: "CHAT_TEST_CREDENTIAL", AccountID: "chat-account", FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	policies := sqlite.NewKeyPolicies(db)
	policy, err := policies.GetLatest(context.Background(), "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policies.Update(context.Background(), policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: policy.Models, Connectors: []string{"upstream", "chat-upstream"}, RPM: policy.RPM, TPM: policy.TPM}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	c := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
	responseConnector := c.protected.Connectors[0]
	responseConnector.Settings.BaseURL = responsesBackend.URL + "/v1/responses"
	c.protected.Connectors[0] = responseConnector
	chatConnector := protectedConnector{ID: "chat-upstream", Kind: "connector", Implementation: chatImplementation, Protocols: []string{chatProtocol}, Settings: nativeSettings{
		BaseURL: chatBackend.URL + "/v1/chat/completions", UpstreamProtocol: chatProtocol, Mode: "native", CredentialEnv: "CHAT_TEST_CREDENTIAL",
		MaxRequestBodyBytes: 1 << 20, MaxRequestHeaderBytes: 8192, ConnectTimeout: "1s", TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s", StreamIdleTimeout: "1s",
	}}
	c.protected.Connectors = append(c.protected.Connectors, chatConnector)
	c.protected.Routes = append(c.protected.Routes, protectedRoute{
		ID: "chat-route", Protocol: chatProtocol, Mode: "native", Model: "model-a", Adapter: chatImplementation, Policy: "standard",
		Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(10)}, Targets: []routeTarget{{Connector: "chat-upstream", Account: "chat-account"}},
	})
	prepared, err := prepareProtectedConfig(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	policyStore := sqlite.NewKeyPolicies(prepared.runtimeDB)
	policy, err = policyStore.GetLatest(context.Background(), "policy-id-a")
	if err != nil {
		t.Fatal(err)
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
	defer func() {
		server.Close()
		_ = closeComponents(context.Background())
		_ = prepared.runtimeDB.Close()
		_ = prepared.processLock.Close()
	}()
	f := &chatProtectedFixture{prepared: prepared, server: server, key: issued.Secret, keyID: issued.ID}
	for _, tc := range []struct{ path, payload string }{
		// The Responses route selects its protocol by path; foreign-looking body
		// fields stay opaque and are not schema-rejected by that adapter.
		{path: "/v1/responses", payload: chatRequest},
		{path: "/v1/chat/completions", payload: chatRequest},
	} {
		resp := chatRequestTo(t, f, tc.path, f.key, tc.payload)
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("path=%s status=%d body=%q calls=(%d,%d) err=%v", tc.path, resp.StatusCode, body, responsesCalls.Load(), chatCalls.Load(), readErr)
		}
	}
	if responsesCalls.Load() != 1 || chatCalls.Load() != 1 {
		t.Fatalf("cross-routed backend calls: Responses=%d Chat=%d", responsesCalls.Load(), chatCalls.Load())
	}
}

func TestChatProtectedGateway_YAMLStreamIdleTimeoutSettlesIncomplete(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	var upstreamCalls atomic.Int32
	var cancelOnce sync.Once
	f := newChatProtectedFixtureWithYAMLCapabilities(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("fake upstream does not support flush")
			return
		}
		flusher.Flush()
		close(started)
		<-r.Context().Done()
		cancelOnce.Do(func() { close(cancelled) })
	}), nil, "150ms")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.server.URL+"/v1/chat/completions", strings.NewReader(chatRequest))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway response status=%d body=%q err=%v", resp.StatusCode, body, readErr)
	}
	for name, signal := range map[string]<-chan struct{}{"backend start": started, "upstream cancellation": cancelled} {
		select {
		case <-signal:
		case <-time.After(3 * time.Second):
			t.Fatalf("timeout did not produce %s", name)
		}
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls=%d, want exactly one", upstreamCalls.Load())
	}
	var rows []sqlite.RequestUsage
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, err = sqlite.NewLedger(f.prepared.runtimeDB).QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 10})
		if err == nil && len(rows) == 1 && len(rows[0].Attempts) == 1 && rows[0].Attempts[0].Attempt.State != "running" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || len(rows) != 1 || rows[0].Request.State != "interrupted" || len(rows[0].Attempts) != 1 {
		t.Fatalf("request was not finalized incomplete: rows=%+v err=%v", rows, err)
	}
	attempt := rows[0].Attempts[0]
	if attempt.Attempt.State != "interrupted" || attempt.Attempt.ErrorCategory == nil || *attempt.Attempt.ErrorCategory != "timeout" || attempt.Attempt.ErrorReason == nil || *attempt.Attempt.ErrorReason != "upstream_timeout" || !attempt.Attempt.Committed || attempt.Reservation.State != "conservative" || attempt.Usage == nil || attempt.Usage.Source != "unknown" || attempt.Usage.Completeness != "unknown" || attempt.Usage.InputTokens != nil || attempt.Usage.OutputTokens != nil {
		t.Fatalf("timeout settlement=%+v", attempt)
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
