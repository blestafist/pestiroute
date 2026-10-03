package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesEmitterLifecycle(t *testing.T) {
	emitter, err := newResponsesEmitter()
	if err != nil {
		t.Fatal(err)
	}
	other, err := newResponsesEmitter()
	if err != nil {
		t.Fatal(err)
	}
	if emitter.responseID == other.responseID || emitter.itemID == other.itemID || emitter.responseID == emitter.itemID {
		t.Fatal("emitter IDs must be unique across executions and ID kinds")
	}
	if !strings.HasPrefix(emitter.responseID, "resp_") || !strings.HasPrefix(emitter.itemID, "msg_") {
		t.Fatalf("unexpected IDs: %q and %q", emitter.responseID, emitter.itemID)
	}
	initial, err := emitter.Start()
	if err != nil || len(initial) != 4 {
		t.Fatalf("Start() = %d frames, %v", len(initial), err)
	}
	if _, err := emitter.Start(); err == nil {
		t.Fatal("duplicate Start succeeded")
	}
	types := []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added"}
	for i, frame := range initial {
		name, payload := decodeResponseFrame(t, frame)
		if name != types[i] {
			t.Fatalf("initial event %d = %q, want %q", i, name, types[i])
		}
		if i < 2 {
			response := payload["response"].(map[string]any)
			if response["id"] != emitter.responseID || response["status"] != "in_progress" {
				t.Fatalf("initial response = %#v", response)
			}
		} else if payload["output_index"] != float64(0) {
			t.Fatalf("initial output index = %#v", payload)
		} else if i == 2 && payload["item"].(map[string]any)["id"] != emitter.itemID {
			t.Fatalf("added item identity = %#v", payload)
		} else if i == 3 && payload["item_id"] != emitter.itemID {
			t.Fatalf("added content item identity = %#v", payload)
		} else if i == 3 {
			part := payload["part"].(map[string]any)
			if len(part["annotations"].([]any)) != 0 || len(part["logprobs"].([]any)) != 0 || len(part) != 4 {
				t.Fatalf("added content part = %#v", part)
			}
		}
	}
	if _, err := emitter.Delta("hello "); err != nil {
		t.Fatal(err)
	}
	delta, err := emitter.Delta("世界")
	if err != nil {
		t.Fatal(err)
	}
	deltaName, deltaPayload := decodeResponseFrame(t, delta)
	if deltaName != "response.output_text.delta" || deltaPayload["output_index"] != float64(0) || deltaPayload["item_id"] != emitter.itemID || deltaPayload["delta"] != "世界" {
		t.Fatalf("delta = %q %#v", deltaName, deltaPayload)
	}
	// Mutate the handed-off frame before later snapshot generation.
	delta[0] ^= 0xff
	terminal, err := emitter.Finish("end_turn", new(int64(12)), new(int64(3)), nil)
	if err != nil || len(terminal) != 4 {
		t.Fatalf("Finish() = %d frames, %v", len(terminal), err)
	}
	wantTypes := []string{"response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed"}
	for i, frame := range terminal {
		name, payload := decodeResponseFrame(t, frame)
		if name != wantTypes[i] {
			t.Fatalf("terminal event %d = %q, want %q", i, name, wantTypes[i])
		}
		if i == 0 && payload["text"] != "hello 世界" {
			t.Fatalf("done text = %#v", payload["text"])
		}
		if i == 2 {
			item := payload["item"].(map[string]any)
			if item["id"] != emitter.itemID || item["role"] != "assistant" || item["status"] != "completed" || len(item) != 5 {
				t.Fatalf("done item = %#v", item)
			}
			parts := item["content"].([]any)
			part := parts[0].(map[string]any)
			if part["text"] != "hello 世界" || len(part["annotations"].([]any)) != 0 || len(part["logprobs"].([]any)) != 0 || len(part) != 4 {
				t.Fatalf("done content part = %#v", part)
			}
		}
		if i == 3 {
			response := payload["response"].(map[string]any)
			usage := response["usage"].(map[string]any)
			if response["status"] != "completed" || usage["input_tokens"] != float64(12) || usage["output_tokens"] != float64(3) || usage["total_tokens"] != float64(15) || len(usage) != 3 {
				t.Fatalf("completed response = %#v", response)
			}
		}
	}
	if _, err := emitter.Delta("late"); err == nil {
		t.Fatal("Delta after termination succeeded")
	}
	if _, err := emitter.Finish("end_turn", new(int64(12)), new(int64(3)), nil); err == nil {
		t.Fatal("duplicate terminal succeeded")
	}
	// Mutating the handed-off delta did not change retained terminal reconstruction.
	_, done := decodeResponseFrame(t, terminal[0])
	if done["text"] != "hello 世界" {
		t.Fatalf("mutating handed-off delta changed terminal snapshot: %#v", done)
	}
}

func TestResponsesEmitterTruncationAndBounds(t *testing.T) {
	emitter, _ := newResponsesEmitter()
	if _, err := emitter.Finish("end_turn", new(int64(0)), new(int64(0)), nil); err == nil {
		t.Fatal("Finish before Start succeeded")
	}
	if _, err := emitter.Delta("before start"); err == nil {
		t.Fatal("out-of-order delta succeeded")
	}
	_, _ = emitter.Start()
	if _, err := emitter.Delta(strings.Repeat("x", maxRetainedText)); err != nil {
		t.Fatalf("1 MiB delta rejected: %v", err)
	}
	terminal, err := emitter.Finish("max_tokens", new(int64(2)), new(int64(4)), nil)
	if err != nil {
		t.Fatal(err)
	}
	name, payload := decodeResponseFrame(t, terminal[len(terminal)-1])
	response := payload["response"].(map[string]any)
	usage := response["usage"].(map[string]any)
	details := response["incomplete_details"].(map[string]any)
	output := response["output"].([]any)
	item := output[0].(map[string]any)
	part := item["content"].([]any)[0].(map[string]any)
	if name != "response.incomplete" || response["status"] != "incomplete" || details["reason"] != "max_output_tokens" || len(details) != 1 || len(usage) != 3 || usage["input_tokens"] != float64(2) || usage["output_tokens"] != float64(4) || usage["total_tokens"] != float64(6) || part["text"] != strings.Repeat("x", maxRetainedText) || len(part["annotations"].([]any)) != 0 || len(part["logprobs"].([]any)) != 0 {
		t.Fatalf("truncation response = %q %#v", name, response)
	}
	oversized, _ := newResponsesEmitter()
	_, _ = oversized.Start()
	if _, err := oversized.Delta(strings.Repeat("x", maxRetainedText+1)); err == nil {
		t.Fatal("text over 1 MiB accepted")
	}
	if _, err := oversized.Delta("later"); err == nil {
		t.Fatal("event emitted after retained-state exhaustion")
	}
	if _, err := oversized.Finish("end_turn", new(int64(1)), new(int64(1)), nil); err == nil {
		t.Fatal("terminal emitted after retained-state exhaustion")
	}
	accumulated, _ := newResponsesEmitter()
	_, _ = accumulated.Start()
	if _, err := accumulated.Delta(strings.Repeat("x", maxRetainedText)); err != nil {
		t.Fatal(err)
	}
	if _, err := accumulated.Delta("y"); err == nil {
		t.Fatal("cumulative text over 1 MiB accepted")
	}
	if _, err := accumulated.Finish("end_turn", new(int64(1)), new(int64(1)), nil); err == nil {
		t.Fatal("terminal emitted after cumulative overflow")
	}
	for _, tc := range []struct {
		name       string
		stop       string
		input, out int
	}{
		{name: "unsupported stop", stop: "tool_use", input: 1, out: 2},
		{name: "negative input usage", stop: "end_turn", input: -1, out: 2},
		{name: "negative output usage", stop: "end_turn", input: 1, out: -2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid, _ := newResponsesEmitter()
			_, _ = invalid.Start()
			if _, err := invalid.Finish(tc.stop, new(int64(tc.input)), new(int64(tc.out)), nil); err == nil {
				t.Fatal("invalid terminal arguments succeeded")
			}
			if _, err := invalid.Delta("still open"); err != nil {
				t.Fatalf("rejected Finish should not close lifecycle: %v", err)
			}
		})
	}
	invalidUTF8, _ := newResponsesEmitter()
	_, _ = invalidUTF8.Start()
	if _, err := invalidUTF8.Delta(string([]byte{0xff})); err == nil {
		t.Fatal("invalid UTF-8 delta accepted")
	}
	if _, err := invalidUTF8.Finish("end_turn", new(int64(1)), new(int64(1)), nil); err == nil {
		t.Fatal("terminal emitted after invalid UTF-8 delta")
	}
}

func TestResponsesEmitterFailureAndIncompleteAreTerminal(t *testing.T) {
	input, output := int64(5), int64(2)
	failed, _ := newResponsesEmitter()
	if _, err := failed.StartResponse(); err != nil {
		t.Fatal(err)
	}
	frame, err := failed.Failed("provider_failure", "Upstream reported a failure", &input, &output, nil)
	if err != nil {
		t.Fatal(err)
	}
	name, payload := decodeResponseFrame(t, frame)
	response := payload["response"].(map[string]any)
	usage := response["usage"].(map[string]any)
	if name != "response.failed" || response["status"] != "failed" || usage["input_tokens"] != float64(5) || usage["output_tokens"] != float64(2) || usage["total_tokens"] != float64(7) {
		t.Fatalf("failed terminal=%q payload=%#v", name, payload)
	}
	if _, err := failed.Failed("again", "again", nil, nil, nil); err == nil {
		t.Fatal("duplicate failed terminal accepted")
	}

	incomplete, _ := newResponsesEmitter()
	if _, err := incomplete.StartResponse(); err != nil {
		t.Fatal(err)
	}
	frame, err = incomplete.Incomplete(&input, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	name, payload = decodeResponseFrame(t, frame)
	response = payload["response"].(map[string]any)
	if name != "response.incomplete" || response["status"] != "incomplete" || response["incomplete_details"].(map[string]any)["reason"] != "incomplete_response" {
		t.Fatalf("incomplete terminal=%q payload=%#v", name, payload)
	}
	if _, err := incomplete.Incomplete(nil, nil, nil); err == nil {
		t.Fatal("duplicate incomplete terminal accepted")
	}
}

func decodeResponseFrame(t *testing.T, frame []byte) (string, map[string]any) {
	t.Helper()
	if !bytes.HasPrefix(frame, []byte("event: ")) || !bytes.HasSuffix(frame, []byte("\n\n")) {
		t.Fatalf("invalid SSE frame %q", frame)
	}
	fields := bytes.SplitN(frame[:len(frame)-2], []byte("\ndata: "), 2)
	if len(fields) != 2 {
		t.Fatalf("invalid SSE fields %q", frame)
	}
	name := strings.TrimPrefix(string(fields[0]), "event: ")
	var payload map[string]any
	if err := json.Unmarshal(fields[1], &payload); err != nil {
		t.Fatalf("invalid event JSON: %v", err)
	}
	var typed struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(fields[1], &typed); err != nil || typed.Type != name {
		t.Fatalf("event type in JSON = %q, SSE event = %q, error=%v", typed.Type, name, err)
	}
	return name, payload
}
