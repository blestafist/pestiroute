package claudecode

import (
	"encoding/json"
	"testing"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestToolHistoryTranslationGroupsCallsAndResults(t *testing.T) {
	body := []byte(`{"model":"synthetic-model","input":[{"type":"message","role":"user","content":"start"},{"type":"function_call","call_id":"call_a","name":"lookup","arguments":"{\"q\":\"a\"}"},{"type":"function_call","call_id":"call_b","name":"lookup","arguments":"{\"q\":\"b\"}"},{"type":"function_call_output","call_id":"call_b","output":"result b"},{"type":"function_call_output","call_id":"call_a","output":"result a"},{"type":"message","role":"assistant","content":"done"}]}`)
	translated, ge := translateRequest(core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol, ContentType: "application/json", Body: body}}, "synthetic-access", "synthetic-beta")
	if ge != nil {
		t.Fatal(ge)
	}
	var got messagesRequest
	if err := json.Unmarshal(translated.Body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 4 || got.Messages[1].Role != "assistant" || got.Messages[2].Role != "user" {
		t.Fatalf("turns were not grouped: %+v", got.Messages)
	}
	calls := got.Messages[1].Content
	if len(calls) != 2 || calls[0].Type != "tool_use" || calls[0].ID != "call_a" || calls[0].Name != "lookup" || string(calls[0].Input) != `{"q":"a"}` || calls[1].ID != "call_b" {
		t.Fatalf("assistant calls = %+v", calls)
	}
	results := got.Messages[2].Content
	if len(results) != 2 || results[0].Type != "tool_result" || results[0].ToolUseID != "call_b" || results[0].Content == nil || *results[0].Content != "result b" || results[1].ToolUseID != "call_a" {
		t.Fatalf("tool results = %+v", results)
	}
}

func TestToolHistoryRejectsUnsupportedOrInvalidChronology(t *testing.T) {
	call := `{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"}`
	result := `{"type":"function_call_output","call_id":"c1","output":"ok"}`
	for _, tc := range []struct {
		name         string
		items        string
		wantInputErr string
	}{
		{"orphan output", `[` + result + `]`, "orphan or duplicate function output"},
		{"duplicate output", `[` + call + `,` + result + `,` + result + `]`, "orphan or duplicate function output"},
		{"dangling call", `[` + call + `]`, "unresolved function calls"},
		{"interleaved message", `[` + call + `,{"type":"message","role":"user","content":"bad"},` + result + `]`, "message while tool results are pending"},
		{"call interleaved with results", `[` + call + `,{"type":"function_call","call_id":"c2","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"},{"type":"function_call","call_id":"c3","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"c2","output":"ok"}]`, "function call interleaved with tool results"},
		{"unsupported reasoning", `[{"type":"reasoning","summary":"hidden"}]`, "unsupported input item"},
		{"unknown function field", `[{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}","custom":true}]`, "unsupported function call field"},
		{"malformed arguments", `[{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{"}]`, "invalid function call"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var translated messagesRequest
			if err := translateInput(json.RawMessage(tc.items), &translated); err == nil || err.Error() != tc.wantInputErr {
				t.Fatalf("translateInput error = %v, want %q", err, tc.wantInputErr)
			}
			body := []byte(`{"model":"synthetic-model","input":` + tc.items + `}`)
			_, ge := translateRequest(core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol, ContentType: "application/json", Body: body}}, "synthetic-access", "synthetic-beta")
			if ge == nil || ge.Code != "unsupported_feature" || ge.Category != core.CategoryUnsupportedFeature {
				t.Fatalf("translation error = %+v, want unsupported_feature", ge)
			}
		})
	}
}
