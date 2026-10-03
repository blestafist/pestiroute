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
