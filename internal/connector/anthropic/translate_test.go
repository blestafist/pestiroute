package anthropic

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/blestafist/pestiroute/internal/adapter/responses"
)

func TestTranslateHistory(t *testing.T) {
	positive := fixture(t, "request-positive.json")
	conversation := []byte(`{"model":"client-model","stream":true,"instructions":"Top level 🐛","input":[{"type":"message","role":"developer","content":"Dev one"},{"type":"message","role":"developer","content":[{"type":"input_text","text":"Dev two 🐞"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"你好"},{"type":"input_text","text":"second"}]},{"type":"message","role":"assistant","status":"completed","annotations":[],"logprobs":[],"content":[{"type":"output_text","text":"こんにちは"},{"type":"output_text","text":"done"}]},{"type":"message","role":"user","content":"last"}]}`)
	cases := []struct {
		name string
		body []byte
		want messagesRequest
	}{
		{
			name: "fixture with developer prefix",
			body: positive,
			want: messagesRequest{
				Model: backendModel, Stream: true, MaxTokens: 20, Temperature: numberPointer("0"),
				System:   []systemTextBlock{{Type: "text", Text: "You are concise."}},
				Messages: []textMessage{{Role: "user", Content: []textBlock{{Type: "text", Text: "Say hello."}}}},
			},
		},
		{
			name: "alternating unicode history",
			body: conversation,
			want: messagesRequest{
				Model: backendModel, Stream: true, MaxTokens: 4096,
				System: []systemTextBlock{{Type: "text", Text: "Top level 🐛"}, {Type: "text", Text: "Dev one"}, {Type: "text", Text: "Dev two 🐞"}},
				Messages: []textMessage{
					{Role: "user", Content: []textBlock{{Type: "text", Text: "你好"}, {Type: "text", Text: "second"}}},
					{Role: "assistant", Content: []textBlock{{Type: "text", Text: "こんにちは"}, {Type: "text", Text: "done"}}},
					{Role: "user", Content: []textBlock{{Type: "text", Text: "last"}}},
				},
			},
		},
		{
			name: "string input",
			body: []byte(`{"model":"client-model","stream":true,"input":"single turn"}`),
			want: messagesRequest{Model: backendModel, Stream: true, MaxTokens: 4096, Messages: []textMessage{{Role: "user", Content: []textBlock{{Type: "text", Text: "single turn"}}}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := bytes.Clone(tc.body)
			for range 2 {
				got, gatewayErr := translateRequest(tc.body)
				if gatewayErr != nil {
					t.Fatalf("translateRequest() error = %+v", gatewayErr)
				}
				if !reflect.DeepEqual(got, tc.want) {
					gotJSON, _ := json.Marshal(got)
					wantJSON, _ := json.Marshal(tc.want)
					t.Fatalf("translation = %s, want %s", gotJSON, wantJSON)
				}
				if !bytes.Equal(tc.body, original) {
					t.Fatal("translation mutated admitted request bytes")
				}
			}
		})
	}
}

func TestTranslateFunctionToolHistory(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{
			name:  "paired calls",
			input: `[{"type":"function_call","call_id":"call-1","name":"weather","arguments":"{\"city\":\"Paris\"}","status":"completed"},{"type":"function_call_output","call_id":"call-1","output":"sunny"},{"type":"function_call","id":"fc-item-2","call_id":"call-2","name":"clock","arguments":"{}"},{"type":"function_call_output","id":"fco-item-2","call_id":"call-2","output":"noon"}]`,
			want:  `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"weather","input":{"city":"Paris"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"sunny"}]},{"role":"assistant","content":[{"type":"tool_use","id":"call-2","name":"clock","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-2","content":"noon"}]}]}`,
		},
		{
			name:  "mixed text and grouped calls",
			input: `[{"type":"message","role":"user","content":"check"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"working"}]},{"type":"function_call","id":"fc1","call_id":"c1","name":"one","arguments":"{}"},{"type":"function_call","id":"fc2","call_id":"c2","name":"two","arguments":"{}"},{"type":"function_call_output","id":"out1","call_id":"c1","output":"1"},{"type":"function_call_output","id":"out2","call_id":"c2","output":"2"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]`,
			want:  `{"messages":[{"role":"user","content":[{"type":"text","text":"check"}]},{"role":"assistant","content":[{"type":"text","text":"working"},{"type":"tool_use","id":"c1","name":"one","input":{}},{"type":"tool_use","id":"c2","name":"two","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"1"},{"type":"tool_result","tool_use_id":"c2","content":"2"}]},{"role":"assistant","content":[{"type":"text","text":"done"}]}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"client-model","stream":true,"input":` + tc.input + `}`)
			original := bytes.Clone(body)
			got, gatewayErr := translateRequest(body)
			if gatewayErr != nil {
				t.Fatalf("translateRequest() error = %+v", gatewayErr)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var actual, want any
			if json.Unmarshal(encoded, &actual) != nil || json.Unmarshal([]byte(tc.want), &want) != nil || !reflect.DeepEqual(actual.(map[string]any)["messages"], want.(map[string]any)["messages"]) {
				t.Fatalf("translated payload = %s, want messages from %s", encoded, tc.want)
			}
			if !bytes.Equal(body, original) {
				t.Fatal("translation mutated admitted request bytes")
			}
		})
	}
}

func TestTranslateFunctionToolHistoryRejectsInvalidLinks(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"orphan", `[{"type":"function_call_output","id":"out","call_id":"missing","output":"x"}]`},
		{"output id substituted for call id", `[{"type":"function_call","id":"item","call_id":"call","name":"f","arguments":"{}"},{"type":"function_call_output","id":"out","call_id":"out","output":"x"}]`},
		{"duplicate call", `[{"type":"function_call","id":"i1","call_id":"same","name":"f","arguments":"{}"},{"type":"function_call","id":"i2","call_id":"same","name":"f","arguments":"{}"}]`},
		{"duplicate result", `[{"type":"function_call","id":"i","call_id":"c","name":"f","arguments":"{}"},{"type":"function_call_output","id":"o1","call_id":"c","output":"x"},{"type":"function_call_output","id":"o2","call_id":"c","output":"y"}]`},
		{"malformed arguments", `[{"type":"function_call","id":"i","call_id":"c","name":"f","arguments":"{"}]`},
		{"non-object arguments", `[{"type":"function_call","id":"i","call_id":"c","name":"f","arguments":"[]"}]`},
		{"arguments duplicate key", `[{"type":"function_call","id":"i","call_id":"c","name":"f","arguments":"{\"x\":1,\"x\":2}"}]`},
		{"missing call id", `[{"type":"function_call","id":"i","name":"f","arguments":"{}"}]`},
		{"incomplete status", `[{"type":"function_call","call_id":"c","name":"f","arguments":"{}","status":"in_progress"}]`},
		{"late result after user text", `[{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"message","role":"user","content":"interleaved"},{"type":"function_call_output","call_id":"c","output":"x"}]`},
		{"non-result user turn with outstanding call", `[{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"message","role":"user","content":"not a result"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"client-model","stream":true,"input":` + tc.input + `}`)
			original := bytes.Clone(body)
			if _, gatewayErr := translateRequest(body); gatewayErr == nil || gatewayErr.Category != "invalid_request" || gatewayErr.Message == "" {
				t.Fatalf("translateRequest() error = %+v, want client-safe invalid_request", gatewayErr)
			}
			if !bytes.Equal(body, original) {
				t.Fatal("rejected translation mutated request bytes")
			}
		})
	}
}

func TestTranslateTools(t *testing.T) {
	body := []byte(`{"model":"client-model","stream":true,"input":"find weather","tools":[{"type":"function","name":"get_weather","description":"Look up weather","parameters":{"type":"object","properties":{"where":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false},"days":{"type":"array","items":{"type":"integer","minimum":1}}},"required":["where"],"additionalProperties":true}}]}`)
	original := bytes.Clone(body)
	got, gatewayErr := translateRequest(body)
	if gatewayErr != nil {
		t.Fatalf("translateRequest() error = %+v", gatewayErr)
	}
	if !bytes.Equal(body, original) {
		t.Fatal("translation mutated admitted request bytes")
	}
	var wire map[string]json.RawMessage
	encoded, err := json.Marshal(got)
	if err != nil || json.Unmarshal(encoded, &wire) != nil {
		t.Fatalf("marshal translated request: %v", err)
	}
	wantTools := `[ {"name":"get_weather","description":"Look up weather","input_schema":{"type":"object","properties":{"where":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false},"days":{"type":"array","items":{"type":"integer","minimum":1}}},"required":["where"],"additionalProperties":true}} ]`
	var want, actual any
	if json.Unmarshal([]byte(wantTools), &want) != nil || json.Unmarshal(wire["tools"], &actual) != nil || !reflect.DeepEqual(actual, want) {
		t.Fatalf("translated tools = %s, want %s", wire["tools"], wantTools)
	}
}

func TestTranslateToolChoiceAndParallelControls(t *testing.T) {
	tools := `"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}]`
	for _, tc := range []struct {
		name, choice, parallel, want string
	}{
		{"auto", `"tool_choice":"auto"`, "", `"tools":[{"name":"weather","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto"}`},
		{"required", `"tool_choice":"required"`, "", `"tools":[{"name":"weather","input_schema":{"type":"object"}}],"tool_choice":{"type":"any"}`},
		{"named", `"tool_choice":{"type":"function","name":"weather"}`, "", `"tools":[{"name":"weather","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"weather"}`},
		{"none", `"tool_choice":"none"`, "", `"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]`},
		{"parallel enabled", `"tool_choice":"auto"`, `"parallel_tool_calls":true`, `"tools":[{"name":"weather","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto"}`},
		{"parallel omitted", `"tool_choice":"auto"`, "", `"tools":[{"name":"weather","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto"}`},
		{"parallel disabled", `"tool_choice":"auto"`, `"parallel_tool_calls":false`, `"tools":[{"name":"weather","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`},
		{"implicit choice disables parallel", ``, `"parallel_tool_calls":false`, `"tools":[{"name":"weather","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := tools
			if tc.choice != "" {
				fields += `,` + tc.choice
			}
			if tc.parallel != "" {
				fields += `,` + tc.parallel
			}
			body := []byte(`{"model":"client-model","stream":true,"input":"hi",` + fields + `}`)
			original := bytes.Clone(body)
			got, gatewayErr := translateRequest(body)
			if gatewayErr != nil {
				t.Fatalf("translateRequest() = %+v", gatewayErr)
			}
			encoded, err := json.Marshal(got)
			if err != nil || !bytes.Contains(encoded, []byte(tc.want)) || !bytes.Equal(body, original) {
				t.Fatalf("wire=%s err=%v want fragment %s; input mutated=%t", encoded, err, tc.want, !bytes.Equal(body, original))
			}
			if tc.name == "none" && bytes.Contains(encoded, []byte(`"tools"`)) {
				t.Fatalf("none must omit tools and tool_choice: %s", encoded)
			}
		})
	}
}

func TestToolChoiceRejectsUnrepresentableForms(t *testing.T) {
	for _, tc := range []struct{ name, fields string }{
		{"without tools", `"tool_choice":"auto"`},
		{"empty tools", `"tools":[],"tool_choice":"auto"`},
		{"unknown name", `"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"missing"}`},
		{"nested Chat Completions shape", `"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}],"tool_choice":{"type":"function","function":{"name":"weather"}}`},
		{"extra choice field", `"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"weather","extra":true}`},
		{"unknown string", `"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}],"tool_choice":"specific"`},
		{"parallel string", `"parallel_tool_calls":"false"`},
		{"parallel null", `"parallel_tool_calls":null`},
		{"parallel number", `"parallel_tool_calls":0`},
		{"parallel false without tools", `"parallel_tool_calls":false`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"client-model","stream":true,"input":"hi",` + tc.fields + `}`)
			original := bytes.Clone(body)
			if _, gatewayErr := translateRequest(body); gatewayErr == nil || gatewayErr.Category != "invalid_request" || !bytes.Equal(body, original) {
				t.Fatalf("translateRequest() error=%+v, want local invalid_request without mutation", gatewayErr)
			}
		})
	}
}

func TestToolSchemaRejectsUnsupportedAndMalformedDefinitions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools string
	}{
		{"non-function", `[{"type":"web_search","name":"search"}]`},
		{"Chat Completions nested function shape", `[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]`},
		{"invalid name", `[{"type":"function","name":"bad name","parameters":{"type":"object"}}]`},
		{"blank name", `[{"type":"function","name":"","parameters":{"type":"object"}}]`},
		{"long name", `[{"type":"function","name":"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopq","parameters":{"type":"object"}}]`},
		{"duplicate name", `[{"type":"function","name":"same","parameters":{"type":"object"}},{"type":"function","name":"same","parameters":{"type":"object"}}]`},
		{"non-object parameters", `[{"type":"function","name":"f","parameters":[]}]`},
		{"strict true", `[{"type":"function","name":"f","strict":true,"parameters":{"type":"object"}}]`},
		{"strict null", `[{"type":"function","name":"f","strict":null,"parameters":{"type":"object"}}]`},
		{"missing parameters", `[{"type":"function","name":"f"}]`},
		{"malformed nested schema", `[{"type":"function","name":"f","parameters":{"type":"object","properties":{"x":{"type":"string","type":"integer"}}}}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"client-model","stream":true,"input":"hi","tools":` + tc.tools + `}`)
			original := bytes.Clone(body)
			if _, gatewayErr := translateRequest(body); gatewayErr == nil || gatewayErr.Category != "invalid_request" || gatewayErr.Message == "" {
				t.Fatalf("translateRequest() error = %+v, want client-safe invalid_request", gatewayErr)
			}
			if !bytes.Equal(body, original) {
				t.Fatal("rejected translation mutated request bytes")
			}
		})
	}
}

func TestTranslateToolsAcrossResponsesDecode(t *testing.T) {
	body := []byte(`{"model":"client-model","stream":true,"input":"find weather","tools":[{"type":"function","name":"get_weather","description":"Look up weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}}]}`)
	req := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	decoded, err := responses.Decode(req, int64(len(body)), 4096)
	if err != nil {
		t.Fatalf("Responses Decode: %v", err)
	}
	if !bytes.Equal(decoded.Payload.Body, body) {
		t.Fatal("adapter mutated admitted payload bytes")
	}
	translated, gatewayErr := translateRequest(decoded.Payload.Body)
	if gatewayErr != nil {
		t.Fatalf("translateRequest: %+v", gatewayErr)
	}
	if len(translated.Tools) != 1 || translated.Tools[0].Name != "get_weather" || translated.Tools[0].Description != "Look up weather" {
		t.Fatalf("translated tool = %+v", translated.Tools)
	}
	var got, want any
	if json.Unmarshal(translated.Tools[0].InputSchema, &got) != nil || json.Unmarshal([]byte(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`), &want) != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("translated schema = %s, want %v", translated.Tools[0].InputSchema, want)
	}
}

func TestGenerationControls(t *testing.T) {
	for _, tc := range []struct {
		name            string
		body            string
		maxTokens       int
		temperature     *json.Number
		temperatureWire string
		valid           bool
	}{
		{name: "default", body: `{"model":"client-model","stream":true,"input":"hi"}`, maxTokens: 4096, valid: true},
		{name: "explicit budget and temperature", body: `{"model":"client-model","stream":true,"input":"hi","max_output_tokens":20,"temperature":0.25}`, maxTokens: 20, temperature: numberPointer("0.25"), temperatureWire: `"temperature":0.25`, valid: true},
		{name: "integral decimal budget", body: `{"model":"client-model","stream":true,"input":"hi","max_output_tokens":100.0}`, maxTokens: 100, valid: true},
		{name: "integral exponent budget", body: `{"model":"client-model","stream":true,"input":"hi","max_output_tokens":1e2}`, maxTokens: 100, valid: true},
		{name: "temperature boundaries", body: `{"model":"client-model","stream":true,"input":"hi","temperature":1}`, maxTokens: 4096, temperature: numberPointer("1"), temperatureWire: `"temperature":1`, valid: true},
		{name: "negative zero temperature", body: `{"model":"client-model","stream":true,"input":"hi","temperature":-0}`, maxTokens: 4096, temperature: numberPointer("-0"), temperatureWire: `"temperature":-0`, valid: true},
		{name: "temperature decimal preserved", body: `{"model":"client-model","stream":true,"input":"hi","temperature":0.1000}`, maxTokens: 4096, temperature: numberPointer("0.1000"), temperatureWire: `"temperature":0.1000`, valid: true},
		{name: "tiny positive temperature preserved", body: `{"model":"client-model","stream":true,"input":"hi","temperature":1e-400}`, maxTokens: 4096, temperature: numberPointer("1e-400"), temperatureWire: `"temperature":1e-400`, valid: true},
		{name: "duplicate model", body: `{"model":"first","model":"client-model","stream":true,"input":"hi"}`},
		{name: "duplicate stream", body: `{"model":"client-model","stream":false,"stream":true,"input":"hi"}`},
		{name: "duplicate store", body: `{"model":"client-model","stream":true,"input":"hi","store":true,"store":false}`},
		{name: "duplicate budget", body: `{"model":"client-model","stream":true,"input":"hi","max_output_tokens":1,"max_output_tokens":4096}`},
		{name: "stream missing", body: `{"model":"client-model","input":"hi"}`},
		{name: "stream false", body: `{"model":"client-model","stream":false,"input":"hi"}`},
		{name: "stream null", body: `{"model":"client-model","stream":null,"input":"hi"}`},
		{name: "stream nonboolean", body: `{"model":"client-model","stream":"true","input":"hi"}`},
		{name: "model missing", body: `{"stream":true,"input":"hi"}`},
		{name: "model empty", body: `{"model":"","stream":true,"input":"hi"}`},
		{name: "model nonstring", body: `{"model":1,"stream":true,"input":"hi"}`},
		{name: "budget zero", body: `{"model":"client-model","stream":true,"input":"hi","max_output_tokens":0}`},
		{name: "budget negative", body: `{"model":"client-model","stream":true,"input":"hi","max_output_tokens":-1}`},
		{name: "budget over limit", body: `{"model":"client-model","stream":true,"input":"hi","max_output_tokens":4097}`},
		{name: "budget fraction", body: `{"model":"client-model","stream":true,"input":"hi","max_output_tokens":2.5}`},
		{name: "budget decimal fraction", body: `{"model":"client-model","stream":true,"input":"hi","max_output_tokens":100.1}`},
		{name: "budget exponent fraction", body: `{"model":"client-model","stream":true,"input":"hi","max_output_tokens":1.015e2}`},
		{name: "budget numeric overflow", body: `{"model":"client-model","stream":true,"input":"hi","max_output_tokens":999999999999999999999999999999999999999999}`},
		{name: "budget impractical exponent", body: `{"model":"client-model","stream":true,"input":"hi","max_output_tokens":1e999999999}`},
		{name: "legacy budget", body: `{"model":"client-model","stream":true,"input":"hi","max_tokens":20}`},
		{name: "temperature below range", body: `{"model":"client-model","stream":true,"input":"hi","temperature":-0.1}`},
		{name: "tiny negative temperature", body: `{"model":"client-model","stream":true,"input":"hi","temperature":-1e-400}`},
		{name: "temperature above range", body: `{"model":"client-model","stream":true,"input":"hi","temperature":1.1}`},
		{name: "temperature slightly above one", body: `{"model":"client-model","stream":true,"input":"hi","temperature":1.0000000000000000000001}`},
		{name: "temperature null", body: `{"model":"client-model","stream":true,"input":"hi","temperature":null}`},
		{name: "temperature string", body: `{"model":"client-model","stream":true,"input":"hi","temperature":"0.5"}`},
		{name: "temperature impractical exponent", body: `{"model":"client-model","stream":true,"input":"hi","temperature":1e999999999}`},
		{name: "store true", body: `{"model":"client-model","stream":true,"input":"hi","store":true}`},
		{name: "store null", body: `{"model":"client-model","stream":true,"input":"hi","store":null}`},
		{name: "store string", body: `{"model":"client-model","stream":true,"input":"hi","store":"false"}`},
		{name: "include unsupported", body: `{"model":"client-model","stream":true,"input":"hi","include":["other"]}`},
		{name: "include wrong type", body: `{"model":"client-model","stream":true,"input":"hi","include":"reasoning.encrypted_content"}`},
		{name: "unknown field", body: `{"model":"client-model","stream":true,"input":"hi","unknown":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			original := bytes.Clone(body)
			got, gatewayErr := translateRequest(body)
			if !tc.valid {
				if gatewayErr == nil || gatewayErr.Category != "invalid_request" || !bytes.Equal(body, original) {
					t.Fatalf("translateRequest() = %+v, %+v; want local invalid_request without mutation", got, gatewayErr)
				}
				return
			}
			encoded, _ := json.Marshal(got)
			if gatewayErr != nil || got.MaxTokens != tc.maxTokens || !reflect.DeepEqual(got.Temperature, tc.temperature) || !bytes.Equal(body, original) || (tc.temperatureWire != "" && !bytes.Contains(encoded, []byte(tc.temperatureWire))) {
				t.Fatalf("translateRequest() = %+v, %+v; want max_tokens=%d temperature=%v without mutation", got, gatewayErr, tc.maxTokens, tc.temperature)
			}
		})
	}
}

func numberPointer(value string) *json.Number {
	number := json.Number(value)
	return &number
}

func TestTranslateHistoryRejectsUnrepresentableRequests(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{"developer only", fixture(t, "request-developer-only.json")},
		{"late developer", fixture(t, "request-late-developer.json")},
		{"system history", fixture(t, "request-system-history.json")},
		{"malformed json", fixture(t, "malformed.json")},
		{"empty input", []byte(`{"input":[]}`)},
		{"empty content", []byte(`{"input":[{"type":"message","role":"user","content":""}]}`)},
		{"assistant string content", []byte(`{"input":[{"type":"message","role":"assistant","content":"hi"}]}`)},
		{"image part", []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"x"}]}]}`)},
		{"extra message field", []byte(`{"input":[{"type":"message","role":"user","content":"hi","tool_call_id":"x"}]}`)},
		{"invalid assistant status", []byte(`{"input":[{"type":"message","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"hi"}]}]}`)},
		{"nonempty annotations", []byte(`{"input":[{"type":"message","role":"assistant","annotations":[{}],"content":[{"type":"output_text","text":"hi"}]}]}`)},
		{"nonempty logprobs", []byte(`{"input":[{"type":"message","role":"assistant","logprobs":[{}],"content":[{"type":"output_text","text":"hi"}]}]}`)},
		{"wrong assistant part type", []byte(`{"input":[{"type":"message","role":"assistant","content":[{"type":"input_text","text":"hi"}]}]}`)},
		{"missing instructions text", []byte(`{"instructions":"","input":"hi"}`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := bytes.Clone(tc.body)
			if _, gatewayErr := translateRequest(body); gatewayErr == nil || gatewayErr.Category != "invalid_request" || gatewayErr.Message == "" {
				t.Fatalf("translateRequest() error = %+v, want client-safe invalid request", gatewayErr)
			}
			if !bytes.Equal(body, tc.body) {
				t.Fatal("rejected translation mutated admitted request bytes")
			}
		})
	}
}

func TestReasoningRequestsFailClosed(t *testing.T) {
	base := `"model":"client-model","stream":true,"input":`
	tests := map[string]string{
		"effort":                    `{` + base + `"hi","reasoning":{"effort":"high"}}`,
		"summary":                   `{` + base + `"hi","reasoning":{"summary":"auto"}}`,
		"unknown reasoning control": `{` + base + `"hi","reasoning":{"provider_mode":"adaptive"}}`,
		"reasoning item":            `{` + base + `[{"type":"reasoning","summary":[]}]}`,
		"reasoning provider state":  `{` + base + `[{"type":"reasoning","provider_state":"opaque"}]}`,
		"encrypted item":            `{` + base + `[{"type":"reasoning","encrypted_content":"opaque"}]}`,
		"reasoning text part":       `{` + base + `[{"type":"message","role":"assistant","content":[{"type":"reasoning_text","text":"hidden"}]}]}`,
		"thinking content part":     `{` + base + `[{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":"hidden","signature":"opaque"}]}]}`,
		"redacted content part":     `{` + base + `[{"type":"message","role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"}]}]}`,
		"synthetic signature state": `{` + base + `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi","signature":"opaque"}]}]}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			request := []byte(body)
			original := bytes.Clone(request)
			if _, gatewayErr := translateRequest(request); gatewayErr == nil || gatewayErr.Category != "invalid_request" {
				t.Fatalf("translateRequest() error = %+v, want local invalid_request", gatewayErr)
			}
			if !bytes.Equal(request, original) {
				t.Fatal("rejected translation mutated request bytes")
			}
		})
	}
}

func TestReasoningIncludeRemainsHarmless(t *testing.T) {
	body := []byte(`{"model":"client-model","stream":true,"include":["reasoning.encrypted_content"],"input":"hello"}`)
	original := bytes.Clone(body)
	translated, gatewayErr := translateRequest(body)
	if gatewayErr != nil {
		t.Fatalf("translateRequest() error = %+v", gatewayErr)
	}
	if !bytes.Equal(body, original) {
		t.Fatal("translation mutated admitted request bytes")
	}
	wire, err := json.Marshal(translated)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte("reasoning")) || bytes.Contains(wire, []byte("encrypted_content")) {
		t.Fatalf("harmless include produced reasoning state: %s", wire)
	}
}
