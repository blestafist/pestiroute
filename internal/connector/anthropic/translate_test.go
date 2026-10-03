package anthropic

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestTranslateHistory(t *testing.T) {
	positive := fixture(t, "request-positive.json")
	conversation := []byte(`{"instructions":"Top level 🐛","input":[{"type":"message","role":"developer","content":"Dev one"},{"type":"message","role":"developer","content":[{"type":"input_text","text":"Dev two 🐞"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"你好"},{"type":"input_text","text":"second"}]},{"type":"message","role":"assistant","status":"completed","annotations":[],"logprobs":[],"content":[{"type":"output_text","text":"こんにちは"},{"type":"output_text","text":"done"}]},{"type":"message","role":"user","content":"last"}]}`)
	cases := []struct {
		name string
		body []byte
		want messagesRequest
	}{
		{
			name: "fixture with developer prefix",
			body: positive,
			want: messagesRequest{
				System:   []systemTextBlock{{Type: "text", Text: "You are concise."}},
				Messages: []textMessage{{Role: "user", Content: []textBlock{{Type: "text", Text: "Say hello."}}}},
			},
		},
		{
			name: "alternating unicode history",
			body: conversation,
			want: messagesRequest{
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
			body: []byte(`{"input":"single turn"}`),
			want: messagesRequest{Messages: []textMessage{{Role: "user", Content: []textBlock{{Type: "text", Text: "single turn"}}}}},
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
