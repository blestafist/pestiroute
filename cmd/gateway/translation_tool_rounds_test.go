package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	adapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	anthropic "github.com/blestafist/pestiroute/internal/connector/anthropic"
	responses "github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestTranslationClientOwnedToolRounds(t *testing.T) {
	_, dbPath, keyPath := protectedFixture(t)
	ctx := context.Background()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	account := sqlite.Account{ID: "anthropic-account", Connector: "anthropic", Enabled: true}
	if _, err := sqlite.NewAccounts(db).Create(ctx, account); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := secure.Seal(key, 2, "test-v1", "credentials", "anthro-key", account.ID, []byte("synthetic-anthropic-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "anthro-key", AccountID: account.ID, FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	policy, err := sqlite.NewKeyPolicies(db).GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	policy, err = sqlite.NewKeyPolicies(db).Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{"model-a", "client-model"}, Connectors: []string{"upstream", "anthropic"}, RPM: 20, TPM: 100000})
	if err != nil {
		t.Fatal(err)
	}
	issued, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	releases := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	var requestsMu sync.Mutex
	var requests []map[string]any
	var upstreamCount atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn := int(upstreamCount.Add(1)) - 1
		if turn > len(releases) {
			t.Errorf("unexpected upstream request %d", turn+1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "synthetic-anthropic-key" || r.Header.Get("Anthropic-Version") != "2023-06-01" {
			t.Errorf("unexpected upstream request path/headers: %s %v", r.URL.Path, r.Header)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode upstream request: %v", err)
			return
		}
		requestsMu.Lock()
		requests = append(requests, payload)
		requestsMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n")
		if turn == len(releases) {
			_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"private\",\"signature\":\"private-signature\"}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-never-delivered\",\"name\":\"weather\"}}\n\n")
			return
		}
		if turn == 2 {
			_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"All done.\"}}\n\n")
		} else {
			calls := []struct{ id, name, args string }{{"call-weather-1", "weather", `{"city":"Paris"}`}}
			if turn == 1 {
				calls = []struct{ id, name, args string }{{"call-weather-2", "weather", `{"city":"Paris"}`}, {"call-clock-2", "clock", `{"zone":"UTC"}`}}
			}
			for i, call := range calls {
				fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"tool_use\",\"id\":%q,\"name\":%q}}\n\n", i, call.id, call.name)
				fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":%q}}\n\n", i, call.args)
				fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i)
			}
		}
		w.(http.Flusher).Flush()
		<-releases[turn]
		stop := `"tool_use"`
		if turn == 2 {
			stop = `"end_turn"`
			_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		}
		fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%s},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", stop)
	}))
	defer backend.Close()
	backendURL, _ := url.Parse(backend.URL)
	backendClient := backend.Client()
	proxyDoer := anthropicDoerFunc(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		copy.URL = backendURL.ResolveReference(&url.URL{Path: "/v1/messages"})
		copy.Host = copy.URL.Host
		return backendClient.Do(copy)
	})
	p := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0").protected
	p.Connectors[0].Settings.BaseURL = backend.URL + "/v1"
	p.Connectors = append(p.Connectors, protectedConnector{ID: "anthropic", Kind: "connector", Implementation: "pestiroute.anthropic.messages", Settings: nativeSettings{Model: "client-model", AccountID: account.ID, CredentialID: "anthro-key"}})
	p.Routes = append(p.Routes, protectedRoute{ID: "translation-route", Protocol: responsesProtocol, Mode: "translation", Model: "client-model", Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(4096)}, Targets: []routeTarget{{Connector: "anthropic", Account: account.ID}}})
	prepared, err := prepareProtectedConfig(ctx, config{protected: p, DatabasePath: dbPath, MasterKeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	ready, draining := atomic.Bool{}, atomic.Bool{}
	ready.Store(true)
	h, closeComponents, err := composeHandlerWithFactory(prepared, &ready, &draining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		if item.Implementation == "pestiroute.anthropic.messages" {
			return anthropicConnectorWithDoer{Connector: anthropic.NewConnector(), doer: proxyDoer}
		}
		return responses.NewConnector()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeComponents(context.Background()) })
	gateway := httptest.NewServer(h)
	defer gateway.Close()
	client := gateway.Client()

	// This history belongs to the test client; the gateway receives only each POST body.
	history := []map[string]any{{"type": "message", "role": "user", "content": "Help me check the weather and time."}}
	allCalls := []map[string]string{}
	for turn := range len(releases) {
		input, err := json.Marshal(history)
		if err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"model":"client-model","stream":true,"input":%s,"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}},{"type":"function","name":"clock","parameters":{"type":"object"}}]}`, input)
		req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+issued.Secret)
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
			data, _ := io.ReadAll(response.Body)
			t.Fatalf("turn %d status=%d content-type=%q body=%s", turn+1, response.StatusCode, response.Header.Get("Content-Type"), data)
		}
		events, calls, text := readToolRound(t, response.Body, releases[turn])
		_ = response.Body.Close()
		if len(events) == 0 || events[len(events)-1]["type"] != "response.completed" {
			t.Fatalf("turn %d did not complete: %#v", turn+1, events)
		}
		completed, _ := events[len(events)-1]["response"].(map[string]any)
		if completed["status"] != "completed" {
			t.Fatalf("turn %d completion snapshot status=%v", turn+1, completed["status"])
		}
		if turn < 2 {
			if len(calls) != turn+1 {
				t.Fatalf("turn %d got %d calls, want %d", turn+1, len(calls), turn+1)
			}
			items, _ := completed["output"].([]any)
			if len(items) != len(calls) {
				t.Fatalf("turn %d completed output items=%v, want %d calls", turn+1, completed["output"], len(calls))
			}
			for _, call := range calls {
				tool := map[string]any{"type": "function_call", "call_id": call["call_id"], "name": call["name"], "arguments": call["arguments"]}
				history = append(history, tool)
				allCalls = append(allCalls, call)
			}
			for _, call := range calls {
				history = append(history, map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": "client result for " + call["name"]})
			}
		} else if text != "All done." {
			t.Fatalf("final text=%q, want All done.", text)
		} else {
			items, _ := completed["output"].([]any)
			if len(items) != 1 || items[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "All done." {
				t.Fatalf("final completion snapshot=%v", completed["output"])
			}
		}
		if turn == 0 {
			// Reasoning attempts in client-owned tool history are rejected before dispatch.
			for name, invalidInput := range map[string]string{
				"item":             strings.TrimSuffix(string(input), "]") + `,{"type":"reasoning","id":"rs_private","encrypted_content":"private"}]`,
				"thinking content": strings.TrimSuffix(string(input), "]") + `,{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"private-signature"}]}]`,
				"redacted content": strings.TrimSuffix(string(input), "]") + `,{"type":"message","role":"assistant","content":[{"type":"redacted_thinking","data":"private-signature"}]}]`,
			} {
				invalidBody := fmt.Sprintf(`{"model":"client-model","stream":true,"input":%s,"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}},{"type":"function","name":"clock","parameters":{"type":"object"}}]}`, invalidInput)
				req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(invalidBody))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+issued.Secret)
				req.Header.Set("Content-Type", "application/json")
				bad, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(bad.Body)
				_ = bad.Body.Close()
				if bad.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), `"code":"invalid_request"`) || upstreamCount.Load() != int32(turn+1) {
					t.Fatalf("%s reasoning rejection status=%d upstream=%d body=%s", name, bad.StatusCode, upstreamCount.Load(), body)
				}
			}
		}
	}
	// The generic Adapter capability gate sees top-level reasoning before the
	// Connector can apply its profile-specific invalid_request policy.
	controlInput, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	for name, suffix := range map[string]string{
		"reasoning":         `,"reasoning":{"effort":"high"}`,
		"parallel tools":    `,"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}],"parallel_tool_calls":true`,
		"structured output": `,"text":{"format":{"type":"json_schema","name":"result","schema":{"type":"object"}}}`,
	} {
		controlBody := fmt.Sprintf(`{"model":"client-model","stream":true,"input":%s%s}`, controlInput, suffix)
		req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(controlBody))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+issued.Secret)
		req.Header.Set("Content-Type", "application/json")
		controlResponse, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		controlError, _ := io.ReadAll(controlResponse.Body)
		_ = controlResponse.Body.Close()
		if controlResponse.StatusCode != http.StatusBadRequest || !strings.Contains(string(controlError), `"code":"unsupported_capability"`) || upstreamCount.Load() != 3 {
			t.Fatalf("%s capability must be blocked before dispatch: status=%d upstream=%d body=%s", name, controlResponse.StatusCode, upstreamCount.Load(), controlError)
		}
	}
	// Provider thinking before tool_use must fail in-band without exposing a call.
	input, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"model":"client-model","stream":true,"input":%s,"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}]}`, input)
	req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+issued.Secret)
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("provider reasoning status=%d body=%s", response.StatusCode, data)
	}
	var failure strings.Builder
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			failure.WriteString(strings.TrimPrefix(scanner.Text(), "data: "))
		}
	}
	_ = response.Body.Close()
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(failure.String(), `"type":"response.failed"`) || strings.Contains(failure.String(), "response.completed") || strings.Contains(failure.String(), "response.output_item.added") || strings.Contains(failure.String(), "call-never-delivered") {
		t.Fatalf("provider reasoning did not fail closed: %s", failure.String())
	}
	if got := upstreamCount.Load(); got != 4 {
		t.Fatalf("upstream requests=%d, want three rounds plus one rejected provider stream (4)", got)
	}
	if len(allCalls) != 3 || allCalls[0]["call_id"] != "call-weather-1" || allCalls[1]["call_id"] != "call-weather-2" || allCalls[2]["call_id"] != "call-clock-2" {
		t.Fatalf("client calls=%v", allCalls)
	}
	requestsMu.Lock()
	defer requestsMu.Unlock()
	if len(requests) != 4 {
		t.Fatalf("captured upstream requests=%d, want 4", len(requests))
	}
	wantMessages := [][]any{
		{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Help me check the weather and time."}}}},
		{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Help me check the weather and time."}}}, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call-weather-1", "name": "weather", "input": map[string]any{"city": "Paris"}}}}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call-weather-1", "content": "client result for weather"}}}},
		{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Help me check the weather and time."}}}, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call-weather-1", "name": "weather", "input": map[string]any{"city": "Paris"}}}}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call-weather-1", "content": "client result for weather"}}}, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call-weather-2", "name": "weather", "input": map[string]any{"city": "Paris"}}, map[string]any{"type": "tool_use", "id": "call-clock-2", "name": "clock", "input": map[string]any{"zone": "UTC"}}}}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call-weather-2", "content": "client result for weather"}, map[string]any{"type": "tool_result", "tool_use_id": "call-clock-2", "content": "client result for clock"}}}},
	}
	for i := range wantMessages {
		if !reflect.DeepEqual(requests[i]["messages"], wantMessages[i]) {
			t.Errorf("turn %d translated messages=%#v, want %#v", i+1, requests[i]["messages"], wantMessages[i])
		}
	}
}

func readToolRound(t *testing.T, body io.Reader, release chan struct{}) ([]map[string]any, []map[string]string, string) {
	t.Helper()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var events []map[string]any
	var calls []map[string]string
	var text strings.Builder
	released := false
	var eventData string
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		eventData = strings.TrimPrefix(line, "data: ")
		var event map[string]any
		if err := json.Unmarshal([]byte(eventData), &event); err != nil {
			t.Fatalf("decode Responses event: %v (%s)", err, eventData)
		}
		events = append(events, event)
		switch event["type"] {
		case "response.function_call_arguments.delta":
			if !released {
				close(release)
				released = true
			}
		case "response.output_text.delta":
			if !released {
				close(release)
				released = true
			}
			if delta, ok := event["delta"].(string); ok {
				text.WriteString(delta)
			}
		case "response.output_item.done":
			item, _ := event["item"].(map[string]any)
			if item["type"] == "function_call" {
				call := map[string]string{"call_id": item["call_id"].(string), "name": item["name"].(string), "arguments": item["arguments"].(string), "output_index": fmt.Sprint(event["output_index"])}
				if call["output_index"] != fmt.Sprint(len(calls)) {
					t.Fatalf("function call output index=%s, want %d", call["output_index"], len(calls))
				}
				calls = append(calls, call)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read Responses stream: %v", err)
	}
	if !released {
		close(release)
		t.Fatal("stream ended without an incremental delta")
	}
	return events, calls, text.String()
}
