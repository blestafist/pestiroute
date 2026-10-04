package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/blestafist/pestiroute/internal/core"
)

func reviewEvent(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

func reviewBlock(index int, kind, delta string) string {
	start := `{"type":"text","text":""}`
	deltaType, field := "text_delta", "text"
	if kind == "tool_use" {
		start = fmt.Sprintf(`{"type":"tool_use","id":"call_%d","name":"weather","input":{}}`, index)
		deltaType, field = "input_json_delta", "partial_json"
	}
	encoded, _ := json.Marshal(delta)
	return reviewEvent("content_block_start", fmt.Sprintf(`{"index":%d,"content_block":%s}`, index, start)) +
		reviewEvent("content_block_delta", fmt.Sprintf(`{"index":%d,"delta":{"type":%q,%q:%s}}`, index, deltaType, field, encoded)) +
		reviewEvent("content_block_stop", fmt.Sprintf(`{"index":%d}`, index))
}

func reviewFrames(t *testing.T, body string) ([]map[string]any, *core.CompleteFrame) {
	t.Helper()
	stream := newMessagesStream(context.Background(), func() {}, io.NopCloser(strings.NewReader(body)))
	defer stream.Close()
	var events []map[string]any
	for {
		frame, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameBody {
			_, payload := decodeResponseFrame(t, frame.Body.Data)
			if payload["sequence_number"] != float64(len(events)) {
				t.Fatalf("SSE sequence: %#v", payload)
			}
			if name := payload["type"]; name == "response.content_part.added" || name == "response.content_part.done" || name == "response.output_text.delta" || name == "response.output_text.done" {
				if payload["content_index"] != float64(0) {
					t.Fatalf("missing text content index: %#v", payload)
				}
			}
			if payload["type"] == "response.failed" {
				response := payload["response"].(map[string]any)
				errorValue, ok := response["error"].(map[string]any)
				if !ok || errorValue["code"] != "server_error" && errorValue["code"] != "rate_limit_exceeded" || errorValue["message"] == "" || payload["error"] != nil {
					t.Fatalf("Responses failure shape: %#v", payload)
				}
			}
			events = append(events, payload)
		}
		if frame.Type == core.FrameComplete {
			if _, err := stream.Next(context.Background()); err != io.EOF {
				t.Fatalf("terminal EOF: %v", err)
			}
			return events, frame.Complete
		}
	}
}

const reviewStart = "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":7,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}}\n\n"

func reviewStop(reason string) string {
	return reviewEvent("message_delta", fmt.Sprintf(`{"delta":{"stop_reason":%q},"usage":{"output_tokens":4}}`, reason)) + reviewEvent("message_stop", `{}`)
}

func TestM4ReviewMixedOutputLifecycle(t *testing.T) {
	for _, kinds := range [][]string{{"text", "tool_use"}, {"tool_use", "text", "tool_use"}, {"text", "text"}} {
		t.Run(strings.Join(kinds, "/"), func(t *testing.T) {
			body, reason := reviewStart, "end_turn"
			for i, kind := range kinds {
				delta := fmt.Sprintf("text-%d 🌍", i)
				if kind == "tool_use" {
					delta, reason = `{"city":"Warsaw"}`, "tool_use"
				}
				body += reviewBlock(i, kind, delta)
			}
			events, complete := reviewFrames(t, body+reviewStop(reason))
			if complete.Outcome != core.OutcomeSucceeded {
				t.Fatalf("mixed output failed: %+v", complete)
			}
			ids := map[string]bool{}
			added, done, deltas := 0, 0, 0
			for _, event := range events {
				switch event["type"] {
				case "response.output_item.added":
					item := event["item"].(map[string]any)
					id := item["id"].(string)
					if ids[id] || event["output_index"] != float64(added) {
						t.Fatalf("item identity/index: %#v", event)
					}
					ids[id], added = true, added+1
				case "response.output_text.delta", "response.function_call_arguments.delta":
					if !ids[event["item_id"].(string)] || event["output_index"] != float64(added-1) {
						t.Fatalf("delta linkage: %#v", event)
					}
					deltas++
				case "response.output_item.done":
					done++
				case "response.completed":
					output := event["response"].(map[string]any)["output"].([]any)
					if len(output) != len(kinds) {
						t.Fatalf("snapshot: %#v", output)
					}
					for i, item := range output {
						v := item.(map[string]any)
						if !ids[v["id"].(string)] || v["status"] != "completed" || (kinds[i] == "text") != (v["type"] == "message") {
							t.Fatalf("ordered snapshot: %#v", v)
						}
					}
				}
			}
			if added != len(kinds) || done != added || deltas != added {
				t.Fatalf("lifecycle counts: added=%d done=%d deltas=%d", added, done, deltas)
			}
		})
	}
}

func TestM4ReviewToolArgumentsAndTruncation(t *testing.T) {
	for _, tc := range []struct {
		arguments, stop string
		outcome         core.Outcome
	}{
		{`{`, "tool_use", core.OutcomeFailed},
		{`[]`, "tool_use", core.OutcomeFailed},
		{`{"x":1,"x":2}`, "tool_use", core.OutcomeFailed},
		{`{`, "max_tokens", core.OutcomeIncomplete},
		{`{}`, "tool_use", core.OutcomeSucceeded},
	} {
		t.Run(tc.arguments+tc.stop, func(t *testing.T) {
			events, complete := reviewFrames(t, reviewStart+reviewBlock(0, "tool_use", tc.arguments)+reviewStop(tc.stop))
			if complete.Outcome != tc.outcome {
				t.Fatalf("outcome=%s want %s", complete.Outcome, tc.outcome)
			}
			terminals := 0
			for _, event := range events {
				if event["type"] == "response.completed" || event["type"] == "response.failed" || event["type"] == "response.incomplete" {
					terminals++
				}
				if tc.outcome == core.OutcomeFailed && event["type"] == "response.output_item.done" {
					t.Fatal("malformed tool was exposed as a completed item")
				}
			}
			if terminals != 1 {
				t.Fatalf("terminal count=%d", terminals)
			}
		})
	}
}

func TestM4ReviewMissingToolResultRejected(t *testing.T) {
	for _, history := range []string{
		`[{"type":"function_call","call_id":"c1","name":"weather","arguments":"{}"}]`,
		`[{"type":"function_call","call_id":"c1","name":"weather","arguments":"{}"},{"type":"function_call","call_id":"c2","name":"weather","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"sunny"}]`,
		`[{"type":"function_call","call_id":"c1","name":"weather","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":null}]`,
	} {
		if _, err := translateRequest([]byte(`{"model":"client","stream":true,"input":` + history + `}`)); err == nil {
			t.Fatal("history with outstanding tool calls accepted")
		}
	}
}

func TestM4ReviewStreamRejectsSemanticLoss(t *testing.T) {
	textStop := reviewEvent("content_block_stop", `{"index":0}`)
	for _, bad := range []string{
		reviewEvent("content_block_start", `{"content_block":{"type":"text"}}`),
		reviewEvent("content_block_start", `{"index":1,"index":0,"content_block":{"type":"text"}}`),
		reviewEvent("content_block_start", `{"index":0,"content_block":{"type":"text"}}`) + reviewEvent("content_block_delta", `{"index":0,"delta":{"type":"text_delta"}}`),
		reviewEvent("content_block_start", `{"index":0,"content_block":{"type":"text"}}`) + reviewEvent("content_block_delta", `{"delta":{"type":"text_delta","text":"x"}}`),
	} {
		_, complete := reviewFrames(t, reviewStart+bad+textStop+reviewStop("end_turn"))
		if complete.Outcome != core.OutcomeFailed {
			t.Fatalf("malformed stream accepted: %s", bad)
		}
	}
	badTool := reviewEvent("content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"c","name":"weather","input":{"city":"Warsaw"}}}`)
	_, complete := reviewFrames(t, reviewStart+badTool+textStop+reviewStop("tool_use"))
	if complete.Outcome != core.OutcomeFailed {
		t.Fatal("nonempty initial tool input was silently discarded")
	}
}

func TestM4ReviewInitialTextAndPartialUsage(t *testing.T) {
	body := reviewStart + reviewEvent("content_block_start", `{"index":0,"content_block":{"type":"text","text":"initial"}}`) + reviewEvent("content_block_stop", `{"index":0}`) + reviewStop("end_turn")
	events, complete := reviewFrames(t, body)
	if complete.Outcome != core.OutcomeSucceeded {
		t.Fatal(complete)
	}
	if output := events[len(events)-1]["response"].(map[string]any)["output"].([]any); output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "initial" {
		t.Fatalf("initial text lost: %#v", output)
	}
	start := reviewEvent("message_start", `{"message":{"content":[],"usage":{"output_tokens":1}}}`)
	_, complete = reviewFrames(t, start)
	if complete.Outcome != core.OutcomeIncomplete || complete.Usage.OutputTokens == nil || *complete.Usage.OutputTokens != 1 {
		t.Fatalf("initial provider usage lost on EOF: %+v", complete)
	}
}
