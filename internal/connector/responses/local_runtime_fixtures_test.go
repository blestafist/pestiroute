package responses

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"testing"
)

//go:embed testdata/local-runtime
var localRuntimeFixtures embed.FS

type runtimeFixture struct {
	dir, version, commit string
	wantEvents           []string
	terminal             string
}

var runtimeFixtures = []runtimeFixture{
	{"ollama-v0.40.3", "Ollama v0.40.3", "eab97e9f92b9a25c2d52d2cc6c1b1c99bd9fae21", []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed"}, "response.completed"},
	{"vllm-v0.31.0", "vLLM v0.31.0", "db9527a46873454610df6dbedf79a36d6bf1a7f6", []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed"}, "response.completed"},
	{"llama.cpp-v0.6.0", "llama.cpp v0.6.0", "d81235049384534c167caea52b85a694f6103d14", []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed"}, "response.completed"},
}

func TestLocalRuntimeFixtureIntegrityAndWireSyntax(t *testing.T) {
	for _, runtime := range runtimeFixtures {
		t.Run(runtime.dir, func(t *testing.T) {
			base := "testdata/local-runtime/" + runtime.dir + "/"
			provenance, err := localRuntimeFixtures.ReadFile(base + "provenance.txt")
			if err != nil || !strings.Contains(string(provenance), runtime.version) || !strings.Contains(string(provenance), runtime.commit) || !strings.Contains(string(provenance), "Synthetic") || !strings.Contains(string(provenance), "not live compatibility evidence") {
				t.Fatalf("invalid synthetic provenance: %q, %v", provenance, err)
			}

			var requests []struct {
				Scenario string          `json:"scenario"`
				Body     json.RawMessage `json:"body"`
			}
			readJSONFixture(t, base+"requests.json", &requests)
			seen := map[string]bool{}
			for _, request := range requests {
				if request.Scenario == "" || len(request.Body) == 0 || !json.Valid(request.Body) || seen[request.Scenario] {
					t.Fatalf("invalid/duplicate request fixture: %+v", request)
				}
				seen[request.Scenario] = true
				if err := validateRequestScenario(runtime.dir, request.Scenario, request.Body); err != nil {
					t.Errorf("request scenario %q: %v", request.Scenario, err)
				}
			}
			if len(seen) != len(expectedRequestScenarios(runtime.dir)) {
				t.Errorf("request fixture count = %d, expected %d", len(seen), len(expectedRequestScenarios(runtime.dir)))
			}
			for _, expected := range expectedRequestScenarios(runtime.dir) {
				if !seen[expected] {
					t.Errorf("missing request scenario %q", expected)
				}
			}

			var response struct {
				Status string `json:"status"`
				Usage  struct {
					Input        int `json:"input_tokens"`
					Output       int `json:"output_tokens"`
					Total        int `json:"total_tokens"`
					InputDetails struct {
						Cached int `json:"cached_tokens"`
					} `json:"input_tokens_details"`
				} `json:"usage"`
			}
			readJSONFixture(t, base+"response.json", &response)
			if response.Status != "completed" || response.Usage.Input <= 0 || response.Usage.Output <= 0 || response.Usage.Total != response.Usage.Input+response.Usage.Output {
				t.Fatalf("invalid synthetic response/usage: %+v", response)
			}

			events := readSSEEvents(t, base+"stream.sse")
			if !equalStrings(events, runtime.wantEvents) || events[len(events)-1] != runtime.terminal {
				t.Fatalf("event sequence = %v, want %v", events, runtime.wantEvents)
			}
			assertRuntimeTextLifecycle(t, base+"stream.sse", runtime.wantEvents, runtime.dir)
			assertRuntimeTerminalUsage(t, base+"stream.sse", runtime.terminal, response.Usage.Input, response.Usage.Output)
		})
	}

	failed := readSSEEvents(t, "testdata/local-runtime/ollama-v0.40.3/web-search-failed.sse")
	if !equalStrings(failed, []string{"response.failed"}) {
		t.Fatalf("Ollama's source-specialized web-search failure event = %v", failed)
	}
	assertNoRuntimeAttribution(t, "testdata/local-runtime/generic-observer/provenance.txt")
	assertGenericObserverFixtures(t)
	var toolTrace struct {
		Response struct {
			Output []struct {
				CallID string `json:"call_id"`
			} `json:"output"`
		} `json:"response"`
		Resubmission struct {
			Input []struct {
				CallID string `json:"call_id"`
			} `json:"input"`
		} `json:"resubmission"`
	}
	readJSONFixture(t, "testdata/local-runtime/vllm-v0.31.0/tool-trace.json", &toolTrace)
	if len(toolTrace.Response.Output) != 1 || len(toolTrace.Resubmission.Input) != 2 || toolTrace.Response.Output[0].CallID == "" || toolTrace.Response.Output[0].CallID != toolTrace.Resubmission.Input[0].CallID || toolTrace.Response.Output[0].CallID != toolTrace.Resubmission.Input[1].CallID {
		t.Fatalf("synthetic tool call/result identifiers do not correlate: %+v", toolTrace)
	}
}

func expectedRequestScenarios(runtime string) []string {
	switch runtime {
	case "ollama-v0.40.3":
		return []string{"text-string", "text-items-resubmitted-history", "function-declaration", "include-ignored", "conversation-decoded-unused", "previous-response-no-source-handling", "invalid-item-type-source-parse-error"}
	case "vllm-v0.31.0":
		return []string{"text-string", "text-items-client-history", "prompt-cache-key-accepted-ignored", "store-true-disabled-storage", "background-disabled-storage", "function-tool-synthetic-parser-trace"}
	default:
		return []string{"text-string", "function-declaration", "text-items-client-history", "unsupported-previous-response-id", "rejected-input-file", "instruction-reasoning-mapping", "non-function-tool-skipped"}
	}
}

func validateRequestScenario(runtime, scenario string, raw json.RawMessage) error {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	if model, _ := body["model"].(string); !strings.HasPrefix(model, "synthetic-") {
		return fmt.Errorf("model must be synthetic, got %q", model)
	}
	if stream, ok := body["stream"].(bool); !ok || stream {
		return fmt.Errorf("fixture must explicitly set stream=false")
	}
	input, _ := body["input"]
	inputArray, _ := input.([]any)
	switch scenario {
	case "text-string":
		if _, ok := input.(string); !ok {
			return fmt.Errorf("input must be a string")
		}
	case "text-items-resubmitted-history", "text-items-client-history":
		if len(inputArray) == 0 {
			return fmt.Errorf("input must contain client-owned history items")
		}
		if err := validateFunctionHistory(inputArray); err != nil {
			return err
		}
	case "function-declaration", "function-tool-synthetic-parser-trace":
		if err := validateFunctionDeclaration(body["tools"]); err != nil {
			return err
		}
	case "include-ignored":
		if !hasString(body["include"], "reasoning.encrypted_content") {
			return fmt.Errorf("include must request the documented ignored field")
		}
	case "conversation-decoded-unused":
		if body["conversation"] != "synthetic-conversation" {
			return fmt.Errorf("conversation field must be present with synthetic value")
		}
	case "previous-response-no-source-handling", "unsupported-previous-response-id":
		if body["previous_response_id"] != "resp_synthetic" {
			return fmt.Errorf("previous_response_id field absent or not synthetic")
		}
	case "invalid-item-type-source-parse-error", "rejected-input-file":
		if len(inputArray) != 1 || objectString(inputArray[0], "type") != "input_file" {
			return fmt.Errorf("source-rejected input_file item missing")
		}
	case "prompt-cache-key-accepted-ignored":
		if body["prompt_cache_key"] != "synthetic-cache-hint" {
			return fmt.Errorf("prompt_cache_key must represent the ignored cache hint")
		}
	case "store-true-disabled-storage", "background-disabled-storage":
		field := "store"
		if strings.HasPrefix(scenario, "background-") {
			field = "background"
		}
		if value, ok := body[field].(bool); !ok || !value {
			return fmt.Errorf("%s must be true for disabled-storage case", field)
		}
	case "instruction-reasoning-mapping":
		reasoning, _ := body["reasoning"].(map[string]any)
		if body["instructions"] != "Synthetic system instruction" || reasoning["effort"] != "low" || body["max_output_tokens"] != float64(32) {
			return fmt.Errorf("instructions/reasoning/max_output_tokens source mappings absent")
		}
	case "non-function-tool-skipped":
		tools, _ := body["tools"].([]any)
		if len(tools) != 1 || objectString(tools[0], "type") != "web_search_preview" {
			return fmt.Errorf("non-function tool skip case missing")
		}
	default:
		return fmt.Errorf("unexpected scenario")
	}
	return nil
}

func validateFunctionDeclaration(value any) error {
	tools, _ := value.([]any)
	if len(tools) != 1 || objectString(tools[0], "type") != "function" || objectString(tools[0], "name") != "lookup" {
		return fmt.Errorf("function tool declaration missing")
	}
	return nil
}

func validateFunctionHistory(items []any) error {
	var callID string
	var foundCall, foundOutput bool
	for _, item := range items {
		switch objectString(item, "type") {
		case "function_call":
			callID = objectString(item, "call_id")
			foundCall = callID == "call_ollama_1" || callID == "call_vllm_1" || callID == "call_llama_1"
		case "function_call_output":
			foundOutput = objectString(item, "call_id") == callID && callID != ""
		}
	}
	if !foundCall || !foundOutput {
		return fmt.Errorf("function call and client-resubmitted result must share call_id")
	}
	return nil
}

func objectString(value any, field string) string {
	object, _ := value.(map[string]any)
	text, _ := object[field].(string)
	return text
}

func hasString(value any, want string) bool {
	items, _ := value.([]any)
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func readJSONFixture(t *testing.T, name string, target any) {
	t.Helper()
	data, err := localRuntimeFixtures.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Fatalf("invalid JSON fixture %s", name)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

func readSSEEvents(t *testing.T, name string) []string {
	t.Helper()
	data, err := localRuntimeFixtures.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	var event string
	var payload bytes.Buffer
	flush := func() {
		if event == "" && payload.Len() == 0 {
			return
		}
		if event == "" || !json.Valid(payload.Bytes()) {
			t.Fatalf("invalid SSE event/payload in %s: event=%q payload=%q", name, event, payload.String())
		}
		var body struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload.Bytes(), &body); err != nil || body.Type != event {
			t.Fatalf("SSE event/data mismatch in %s: %q / %q", name, event, payload.String())
		}
		events = append(events, event)
		event = ""
		payload.Reset()
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			flush()
		} else if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			if payload.Len() != 0 {
				payload.WriteByte('\n')
			}
			payload.WriteString(strings.TrimPrefix(line, "data: "))
		} else if strings.HasPrefix(line, ":") {
			continue
		} else {
			t.Fatalf("unexpected SSE line in %s: %q", name, line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	flush()
	return events
}

func assertRuntimeTextLifecycle(t *testing.T, name string, want []string, runtime string) {
	t.Helper()
	data, err := localRuntimeFixtures.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	events := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n\n")
	var itemID string
	frames := 0
	for _, rawEvent := range events {
		var eventName string
		var payload json.RawMessage
		for _, line := range strings.Split(rawEvent, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if strings.HasPrefix(line, "event: ") {
				eventName = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				payload = json.RawMessage(strings.TrimPrefix(line, "data: "))
			}
		}
		if eventName == "" { // Ignore the legal synthetic comment trailer.
			continue
		}
		frames++
		if eventName == "response.output_item.added" {
			var body struct {
				Item struct {
					ID string `json:"id"`
				} `json:"item"`
			}
			if err := json.Unmarshal(payload, &body); err != nil || body.Item.ID == "" {
				t.Fatalf("%s output_item.added missing item id: %s (%v)", runtime, payload, err)
			}
			itemID = body.Item.ID
		}
		if strings.HasPrefix(eventName, "response.output_text.") || strings.HasPrefix(eventName, "response.content_part.") {
			var body struct {
				ItemID string `json:"item_id"`
			}
			if err := json.Unmarshal(payload, &body); err != nil || body.ItemID == "" || itemID != "" && body.ItemID != itemID {
				t.Fatalf("%s %s item id mismatch: %s (%v), expected %q", runtime, eventName, payload, err, itemID)
			}
		}
		if eventName == "response.output_item.done" {
			var body struct {
				Item struct {
					ID string `json:"id"`
				} `json:"item"`
			}
			if err := json.Unmarshal(payload, &body); err != nil || body.Item.ID == "" || itemID != "" && body.Item.ID != itemID {
				t.Fatalf("%s output_item.done item id mismatch: %s (%v), expected %q", runtime, payload, err, itemID)
			}
		}
	}
	if frames != len(want) {
		t.Fatalf("%s lifecycle frame count = %d, want %d", runtime, frames, len(want))
	}
}

func assertRuntimeTerminalUsage(t *testing.T, name, terminal string, input, output int) {
	t.Helper()
	data, err := localRuntimeFixtures.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, `"type":"`+terminal+`"`) {
			continue
		}
		var body struct {
			Response struct {
				Status string `json:"status"`
				Usage  struct {
					Input        int `json:"input_tokens"`
					Output       int `json:"output_tokens"`
					Total        int `json:"total_tokens"`
					InputDetails struct {
						Cached int `json:"cached_tokens"`
					} `json:"input_tokens_details"`
				} `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &body); err != nil || body.Response.Status != "completed" || body.Response.Usage.Input != input || body.Response.Usage.Output != output || body.Response.Usage.Total != input+output {
			t.Fatalf("terminal usage in %s does not match synthetic response: %+v (%v)", name, body, err)
		}
		return
	}
	t.Fatalf("source-evidenced terminal %q missing from %s", terminal, name)
}

func assertNoRuntimeAttribution(t *testing.T, name string) {
	t.Helper()
	data, err := localRuntimeFixtures.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"Ollama", "vLLM", "llama.cpp", "eab97e9", "db9527a", "d812350"} {
		if bytes.Contains(data, []byte(version)) {
			t.Fatalf("generic fixture %s has runtime attribution %q", name, version)
		}
	}
}

func assertGenericObserverFixtures(t *testing.T) {
	t.Helper()
	name := "testdata/local-runtime/generic-observer/mechanics.json"
	var cases []struct {
		Scenario string   `json:"scenario"`
		Valid    bool     `json:"syntax_valid"`
		Chunks   []string `json:"chunks"`
	}
	readJSONFixture(t, name, &cases)
	want := map[string]bool{"failure-unattributed": true, "incomplete-unattributed": true, "premature-eof-unattributed": true, "malformed-json-unattributed": true, "split-crlf-event-unattributed": true, "split-data-event-unattributed": true}
	for _, item := range cases {
		delete(want, item.Scenario)
		joined := strings.Join(item.Chunks, "")
		if len(item.Chunks) == 0 || (item.Scenario == "split-crlf-event-unattributed" || item.Scenario == "split-data-event-unattributed") && len(item.Chunks) < 2 {
			t.Errorf("invalid chunks for %s", item.Scenario)
		}
		if item.Scenario == "malformed-json-unattributed" {
			if item.Valid || !strings.Contains(joined, `,}`) {
				t.Errorf("malformed JSON fixture not explicit: %+v", item)
			}
		} else {
			if !item.Valid {
				t.Errorf("valid generic fixture marked invalid: %s", item.Scenario)
			}
			if valid := genericSSEJSONValid(joined); !valid {
				t.Errorf("generic fixture %s has invalid SSE/JSON syntax", item.Scenario)
			}
		}
		for _, version := range []string{"Ollama", "vLLM", "llama.cpp", "eab97e9", "db9527a", "d812350"} {
			if strings.Contains(item.Scenario+joined, version) {
				t.Errorf("generic scenario %s attributes runtime %q", item.Scenario, version)
			}
		}
	}
	if len(want) != 0 {
		t.Errorf("missing generic observer scenarios: %v", want)
	}
}

func genericSSEJSONValid(data string) bool {
	var event, payload string
	count := 0
	flush := func() bool {
		if event == "" && payload == "" {
			return true
		}
		var body struct {
			Type string `json:"type"`
		}
		if event == "" || !json.Valid([]byte(payload)) || json.Unmarshal([]byte(payload), &body) != nil || body.Type != event {
			return false
		}
		count++
		event, payload = "", ""
		return true
	}
	for _, line := range strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n") {
		switch {
		case line == "":
			if !flush() {
				return false
			}
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			payload = strings.TrimPrefix(line, "data: ")
		default:
			return false
		}
	}
	return flush() && count > 0
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLocalRuntimeFixtureInventory(t *testing.T) {
	entries, err := fs.Glob(localRuntimeFixtures, "testdata/local-runtime/*/*")
	if err != nil {
		t.Fatal(err)
	}
	for _, runtime := range runtimeFixtures {
		for _, file := range []string{"provenance.txt", "requests.json", "response.json", "stream.sse"} {
			want := "testdata/local-runtime/" + runtime.dir + "/" + file
			if !contains(entries, want) {
				t.Errorf("fixture missing: %s", want)
			}
		}
	}
	if !contains(entries, "testdata/local-runtime/ollama-v0.40.3/web-search-failed.sse") || !contains(entries, "testdata/local-runtime/vllm-v0.31.0/tool-trace.json") || !contains(entries, "testdata/local-runtime/generic-observer/mechanics.json") {
		t.Fatal("required specialized, tool-trace, or generic fixture absent")
	}
	for _, entry := range entries {
		if path.Base(entry) == "" {
			t.Errorf("invalid fixture path %q", entry)
		}
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
