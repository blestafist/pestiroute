package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

func chatFixtureWithCapabilities(t *testing.T, f *chatProtectedFixture, capabilities map[string]string) {
	t.Helper()
	f.prepared.Components[0].Capabilities = capabilities
	h, closeComponents, err := composeHandler(f.prepared, new(atomic.Bool), new(atomic.Bool), nil)
	if err != nil {
		t.Fatal(err)
	}
	f.server.Close()
	f.server = httptest.NewServer(h)
	t.Cleanup(func() {
		f.server.Close()
		_ = closeComponents(context.Background())
	})
}

func TestChatAgentWorkflow_MultiRoundTool(t *testing.T) {
	firstRequest := `{"model":"model-a","messages":[{"role":"user","content":"weather?","client_extension":{"v":1}}],"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}]}`
	secondRequest := `{"model":"model-a","messages":[{"role":"user","content":"weather?","client_extension":{"v":1}},{"role":"assistant","tool_calls":[{"id":"call_7Qx","type":"function","function":{"name":"weather","arguments":"{ \"city\" : \"Z\", \"units\":\"c\\\"\" }"}}],"client_turn":{"opaque":true}},{"role":"tool","tool_call_id":"call_7Qx","content":"rain","tool_extension":[1,  2]}]}`
	firstResponse := ` {"id":"chatcmpl-first","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_7Qx","type":"function","function":{"name":"weather","arguments":"{ \"city\" : \"Z\", \"units\":\"c\\\"\" }"}}]}}]} `
	finalResponse := ` {"id":"chatcmpl-final","choices":[{"message":{"role":"assistant","content":"Rain.","finish_reason":"stop"}}]} `
	var calls atomic.Int32
	f := newChatProtectedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		switch calls.Add(1) {
		case 1:
			if !bytes.Equal(body, []byte(firstRequest)) {
				t.Errorf("round 1 request changed: %q", body)
			}
			_, _ = io.WriteString(w, firstResponse)
		case 2:
			if !bytes.Equal(body, []byte(secondRequest)) {
				t.Errorf("round 2 request changed: %q", body)
			}
			_, _ = io.WriteString(w, finalResponse)
		default:
			t.Errorf("unexpected backend request %d", calls.Load())
		}
	}))
	chatFixtureWithCapabilities(t, f, map[string]string{"llm.tools": string(core.Supported)})
	for _, turn := range []struct{ request, response string }{{firstRequest, firstResponse}, {secondRequest, finalResponse}} {
		resp := chatRequestTo(t, f, "/v1/chat/completions", f.key, turn.request)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(body, []byte(turn.response)) {
			t.Fatalf("tool round status=%d body=%q err=%v", resp.StatusCode, body, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("backend calls=%d, want two rounds", calls.Load())
	}
}

func TestChatAgentWorkflow_StreamingToolDeltas(t *testing.T) {
	chunks := []string{
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_\",\"type\":\"function\",\"function\":{\"name\":\"wea\",\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"7Qx\",\"function\":{\"name\":\"ther\",\"arguments\":\"Z\\\"}\"}}]}}]}\n\n",
		"data: [DONE]\n\n",
	}
	release := make(chan struct{})
	firstWritten := make(chan struct{})
	f := newChatProtectedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chunks[0])
		w.(http.Flusher).Flush()
		close(firstWritten)
		<-release
		_, _ = io.WriteString(w, chunks[1]+chunks[2])
	}))
	chatFixtureWithCapabilities(t, f, map[string]string{"llm.streaming": string(core.Supported), "llm.tools": string(core.Supported)})
	request := `{"model":"model-a","stream":true,"messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"weather"}}]}`
	resp := chatRequestTo(t, f, "/v1/chat/completions", f.key, request)
	defer resp.Body.Close()
	gotFirst := make([]byte, len(chunks[0]))
	if _, err := io.ReadFull(resp.Body, gotFirst); err != nil || !bytes.Equal(gotFirst, []byte(chunks[0])) {
		t.Fatalf("first tool delta=%q err=%v", gotFirst, err)
	}
	select {
	case <-firstWritten:
	case <-time.After(3 * time.Second):
		t.Fatal("backend did not hold stream after first tool delta")
	}
	select {
	case <-release:
		t.Fatal("backend stream released before first delta reached client")
	default:
	}
	close(release)
	rest, err := io.ReadAll(resp.Body)
	want := []byte(chunks[1] + chunks[2])
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(rest, want) {
		t.Fatalf("remaining tool deltas=%q want=%q status=%d err=%v", rest, want, resp.StatusCode, err)
	}
}

func TestChatAgentWorkflow_ParallelToolCalls(t *testing.T) {
	request := `{"model":"model-a","messages":[{"role":"user","content":"weather in A and B"}],"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}],"parallel_tool_calls":true}`
	response := ` {"id":"chatcmpl-parallel","choices":[{"message":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"weather","arguments":"{\"city\":\"A\"}"}},{"index":1,"id":"call_b","type":"function","function":{"name":"weather","arguments":"{\"city\":\"B\"}"}}]}}]} `
	var calls atomic.Int32
	f := newChatProtectedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, []byte(request)) {
			t.Errorf("parallel request changed: %q", body)
		}
		_, _ = io.WriteString(w, response)
	}))
	// Both capabilities are scoped to this configured connector; support must be explicit.
	chatFixtureWithCapabilities(t, f, map[string]string{"llm.tools": string(core.Supported), "llm.tools.parallel": string(core.Supported)})
	resp := chatRequestTo(t, f, "/v1/chat/completions", f.key, request)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(body, []byte(response)) || calls.Load() != 1 {
		t.Fatalf("parallel response status=%d calls=%d body=%q err=%v", resp.StatusCode, calls.Load(), body, err)
	}

	for _, state := range []string{"absent", string(core.Unknown)} {
		t.Run("tools supported, parallel "+state, func(t *testing.T) {
			var deniedCalls atomic.Int32
			denied := newChatProtectedFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { deniedCalls.Add(1) }))
			capabilities := map[string]string{"llm.tools": string(core.Supported)}
			if state != "absent" {
				capabilities["llm.tools.parallel"] = state
			}
			chatFixtureWithCapabilities(t, denied, capabilities)
			resp := chatRequestTo(t, denied, "/v1/chat/completions", denied.key, request)
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil || resp.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(`"code":"unsupported_capability"`)) || deniedCalls.Load() != 0 {
				t.Fatalf("unsupported parallel status=%d calls=%d body=%q err=%v", resp.StatusCode, deniedCalls.Load(), body, readErr)
			}
		})
	}

	// parallel_tool_calls alone requires only llm.tools.parallel, not llm.tools.
	parallelOnlyRequest := `{"model":"model-a","messages":[{"role":"user","content":"parallel mode"}],"parallel_tool_calls":true}`
	var parallelOnlyCalls atomic.Int32
	parallelOnly := newChatProtectedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		parallelOnlyCalls.Add(1)
		_, _ = io.WriteString(w, chatResponse)
	}))
	chatFixtureWithCapabilities(t, parallelOnly, map[string]string{"llm.tools.parallel": string(core.Supported)})
	resp = chatRequestTo(t, parallelOnly, "/v1/chat/completions", parallelOnly.key, parallelOnlyRequest)
	body, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(body, []byte(chatResponse)) || parallelOnlyCalls.Load() != 1 {
		t.Fatalf("parallel-only supported status=%d calls=%d body=%q err=%v", resp.StatusCode, parallelOnlyCalls.Load(), body, err)
	}

	for _, state := range []string{"absent", string(core.Unknown)} {
		t.Run("parallel-only "+state, func(t *testing.T) {
			var deniedCalls atomic.Int32
			denied := newChatProtectedFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { deniedCalls.Add(1) }))
			capabilities := map[string]string{}
			if state != "absent" {
				capabilities["llm.tools.parallel"] = state
			}
			chatFixtureWithCapabilities(t, denied, capabilities)
			resp := chatRequestTo(t, denied, "/v1/chat/completions", denied.key, parallelOnlyRequest)
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil || resp.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(`"code":"unsupported_capability"`)) || deniedCalls.Load() != 0 {
				t.Fatalf("unsupported parallel-only status=%d calls=%d body=%q err=%v", resp.StatusCode, deniedCalls.Load(), body, readErr)
			}
		})
	}
}

func TestChatAgentWorkflow_ToolChoice(t *testing.T) {
	choices := []string{`"auto"`, `"required"`, `{"type":"function","function":{"name":"weather"}}`}
	var calls atomic.Int32
	var expectedRequest string
	f := newChatProtectedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, []byte(expectedRequest)) {
			t.Errorf("tool choice request (including opaque fields) changed: got %s want %s", body, expectedRequest)
		}
		calls.Add(1)
		_, _ = io.WriteString(w, chatResponse)
	}))
	chatFixtureWithCapabilities(t, f, map[string]string{"llm.tools": string(core.Supported)})
	for _, choice := range choices {
		expectedRequest = `{"model":"model-a","messages":[{"role":"user","content":"weather?"},{"role":"assistant","tool_calls":[{"id":"call_x","type":"function","function":{"name":"weather","arguments":"{  \"city\" : \"Z\" }"}}]}],"tools":[{"type":"function","function":{"name":"weather"}}],"tool_choice":` + choice + `,"extension":{"keep":true}}`
		resp := chatRequestTo(t, f, "/v1/chat/completions", f.key, expectedRequest)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(body, []byte(chatResponse)) {
			t.Fatalf("tool_choice=%s status=%d body=%q err=%v", choice, resp.StatusCode, body, err)
		}
	}
	// `none` has no llm.tools requirement and still passes natively without a declaration.
	noTools := newChatProtectedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, chatResponse)
	}))
	none := `{"model":"model-a","messages":[{"role":"user","content":"hello"}],"tool_choice":"none"}`
	resp := chatRequestTo(t, noTools, "/v1/chat/completions", noTools.key, none)
	_, err := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("tool_choice none status=%d err=%v", resp.StatusCode, err)
	}

	var deniedCalls atomic.Int32
	denied := newChatProtectedFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { deniedCalls.Add(1) }))
	for _, request := range []string{
		`{"model":"model-a","messages":[{"role":"user","content":"x"}],"tool_choice":"required"}`,
		`{"model":"model-a","messages":[{"role":"user","content":"x"}],"tool_choice":{"type":"function","function":{"name":"weather"}}}`,
	} {
		resp := chatRequestTo(t, denied, "/v1/chat/completions", denied.key, request)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(`"code":"unsupported_capability"`)) || deniedCalls.Load() != 0 {
			t.Fatalf("unsupported tool choice status=%d calls=%d body=%q", resp.StatusCode, deniedCalls.Load(), body)
		}
	}
	invalid := `{"model":"model-a","messages":[{"role":"user","content":"x"}],"tool_choice":{"type":"function","function":{"name":""}}}`
	resp = chatRequestTo(t, f, "/v1/chat/completions", f.key, invalid)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || calls.Load() != int32(len(choices)+1) {
		t.Fatalf("invalid tool choice status=%d dispatched=%d", resp.StatusCode, calls.Load())
	}
}

func TestChatAgentWorkflow_ResponseFormat(t *testing.T) {
	formats := []string{
		`{"type":"text"}`,
		`{"type":"json_object","provider_extension":{"x": 1}}`,
		`{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object","properties":{"ok":{"type":"boolean"}},"additionalProperties":false}}}`,
	}
	var calls atomic.Int32
	var expectedRequest string
	f := newChatProtectedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, []byte(expectedRequest)) {
			t.Errorf("response_format request changed: got %s want %s", body, expectedRequest)
		}
		calls.Add(1)
		_, _ = io.WriteString(w, chatResponse)
	}))
	chatFixtureWithCapabilities(t, f, map[string]string{"llm.structured_output": string(core.Supported)})
	for _, format := range formats {
		expectedRequest = `{"model":"model-a","messages":[{"role":"user","content":"answer"}],"response_format":` + format + `}`
		resp := chatRequestTo(t, f, "/v1/chat/completions", f.key, expectedRequest)
		_, err := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("response_format=%s status=%d err=%v", format, resp.StatusCode, err)
		}
	}
	var deniedCalls atomic.Int32
	denied := newChatProtectedFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { deniedCalls.Add(1) }))
	for _, format := range formats[1:] {
		request := `{"model":"model-a","messages":[{"role":"user","content":"answer"}],"response_format":` + format + `}`
		resp := chatRequestTo(t, denied, "/v1/chat/completions", denied.key, request)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(`"code":"unsupported_capability"`)) || deniedCalls.Load() != 0 {
			t.Fatalf("unsupported format status=%d calls=%d body=%q", resp.StatusCode, deniedCalls.Load(), body)
		}
	}
	var textCalls atomic.Int32
	textOnly := newChatProtectedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		want := `{"model":"model-a","messages":[{"role":"user","content":"answer"}],"response_format":{"type":"text"}}`
		if !bytes.Equal(body, []byte(want)) {
			t.Errorf("text response format changed: got %s want %s", body, want)
		}
		textCalls.Add(1)
		_, _ = io.WriteString(w, chatResponse)
	}))
	textResponse := `{"model":"model-a","messages":[{"role":"user","content":"answer"}],"response_format":{"type":"text"}}`
	resp := chatRequestTo(t, textOnly, "/v1/chat/completions", textOnly.key, textResponse)
	_, err := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || textCalls.Load() != 1 {
		t.Fatalf("text response format status=%d calls=%d err=%v", resp.StatusCode, textCalls.Load(), err)
	}
	malformed := []string{
		`{"type":"json_schema","json_schema":{"name":"answer","schema":[]}}`,
		`{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"},"strict":"yes"}}`,
		`{"type":"xml_object"}`,
	}
	for _, format := range malformed {
		request := `{"model":"model-a","messages":[{"role":"user","content":"answer"}],"response_format":` + format + `}`
		resp := chatRequestTo(t, f, "/v1/chat/completions", f.key, request)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || calls.Load() != int32(len(formats)) {
			t.Fatalf("malformed response format status=%d dispatches=%d body=%q", resp.StatusCode, calls.Load(), body)
		}
	}
}
