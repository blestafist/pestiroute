package responses

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/blestafist/pestiroute/internal/core"
)

const base = `{"model":"gpt-4.1-mini-2025-04-14"}`

func request(body string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "http://localhost/v1/responses", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestDecodeOpaqueAndCapabilities(t *testing.T) {
	body := " { \n\"model\" : \"gpt-4.1-mini-2025-04-14\", \"unknown\": {\"nested\": [1, 2]}, " +
		`"stream":true,"tools":[{"type":"function","name":"f"}],"parallel_tool_calls":true,` +
		`"reasoning":{"effort":"low"},"text":{"format":{"type":"json_schema"}},` +
		`"input":[{"role":"user","content":[{"type":"input_image","image_url":"x"},{"type":"input_audio","input_audio":{}},{"type":"input_text","text":"hello"}]}]}`
	r := request(body)
	r.Header.Set("Accept", "application/json")
	r.Header.Set("X-Trace", "abc")
	r.Header.Set("Authorization", "Bearer private")
	r.Header.Set("Cookie", "private")
	r.Header.Set("X-Api-Key", "private")
	r.Header.Set("OpenAI-Organization", "private")
	r.Header.Set("X-Client-Secret", "private")
	r.Header.Set("Idempotency-Key", "request-1")
	r.Header.Set("Connection", "X-Trace, keep-alive")
	r.Header.Set("Keep-Alive", "timeout=10")
	got, err := Decode(r, int64(len(body)), 4096)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Payload.Body) != body || got.Payload.Protocol != protocol || got.Payload.ContentType != "application/json" || got.Model != model || got.ID != "" {
		t.Fatalf("invalid envelope: %+v", got)
	}
	for _, key := range []string{"llm.streaming", "llm.tools", "llm.tools.parallel", "llm.reasoning", "llm.structured_output", "llm.vision", "llm.audio"} {
		if _, ok := got.Capabilities[core.Capability(key)]; !ok {
			t.Errorf("missing %s", key)
		}
	}
	if got.Metadata.Streaming == nil || !*got.Metadata.Streaming || len(got.Metadata.Headers["Accept"]) != 1 {
		t.Fatalf("missing safe metadata: %+v", got.Metadata)
	}
	if got.Metadata.Headers["Idempotency-Key"][0] != "request-1" {
		t.Fatalf("idempotency header lost: %+v", got.Metadata.Headers)
	}
	for _, key := range []string{"Authorization", "Cookie", "X-Api-Key", "Openai-Organization", "X-Client-Secret", "Connection", "Keep-Alive", "X-Trace"} {
		if _, ok := got.Metadata.Headers[key]; ok {
			t.Errorf("unsafe header %s", key)
		}
	}
}

func TestDecodeInvalid(t *testing.T) {
	cases := []struct {
		name, body, header, value string
		category                  core.ErrorCategory
	}{
		{"malformed", `{"model":`, "", "", core.CategoryInvalidRequest},
		{"trailing", base + " {}", "", "", core.CategoryInvalidRequest},
		{"duplicate model", base[:len(base)-1] + `,"model":"other"}`, "", "", core.CategoryInvalidRequest},
		{"escaped duplicate", base[:len(base)-1] + `,"mo\u0064el":"other"}`, "", "", core.CategoryInvalidRequest},
		{"duplicate stream", base[:len(base)-1] + `,"stream":true,"stream":false}`, "", "", core.CategoryInvalidRequest},
		{"duplicate unknown", base[:len(base)-1] + `,"future":1,"future":2}`, "", "", core.CategoryInvalidRequest},
		{"duplicate image type", base[:len(base)-1] + `,"input":[{"content":[{"type":"input_image","ty\u0070e":"input_text"}]}]}`, "", "", core.CategoryInvalidRequest},
		{"duplicate format type", base[:len(base)-1] + `,"text":{"format":{"type":"json_schema","type":"text"}}}`, "", "", core.CategoryInvalidRequest},
		{"duplicate tool type", base[:len(base)-1] + `,"tools":[{"type":"function","ty\u0070e":"future"}]}`, "", "", core.CategoryInvalidRequest},
		{"duplicate opaque nested", base[:len(base)-1] + `,"future":{"a":[{"field":1,"field":2}]}}`, "", "", core.CategoryInvalidRequest},
		{"missing model", `{}`, "", "", core.CategoryInvalidRequest},
		{"wrong model", `{"model":"other"}`, "", "", core.CategoryInvalidRequest},
		{"stream null", base[:len(base)-1] + `,"stream":null}`, "", "", core.CategoryInvalidRequest},
		{"reasoning null", base[:len(base)-1] + `,"reasoning":null}`, "", "", core.CategoryInvalidRequest},
		{"tools object", base[:len(base)-1] + `,"tools":{}}`, "", "", core.CategoryInvalidRequest},
		{"invalid tool", base[:len(base)-1] + `,"tools":[{}]}`, "", "", core.CategoryInvalidRequest},
		{"parallel type", base[:len(base)-1] + `,"parallel_tool_calls":"yes"}`, "", "", core.CategoryInvalidRequest},
		{"input shape", base[:len(base)-1] + `,"input":{}}`, "", "", core.CategoryInvalidRequest},
		{"input null", base[:len(base)-1] + `,"input":null}`, "", "", core.CategoryInvalidRequest},
		{"invalid content type", base[:len(base)-1] + `,"input":[{"content":[{"type":42}]}]}`, "", "", core.CategoryInvalidRequest},
		{"missing content type", base[:len(base)-1] + `,"input":[{"content":[{}]}]}`, "", "", core.CategoryInvalidRequest},
		{"unknown format", base[:len(base)-1] + `,"text":{"format":{"type":"future"}}}`, "", "", core.CategoryUnsupportedFeature},
		{"format null", base[:len(base)-1] + `,"text":{"format":null}}`, "", "", core.CategoryInvalidRequest},
		{"media", base, "Content-Type", "text/plain", core.CategoryUnsupportedFeature},
		{"charset", base, "Content-Type", "application/json; charset=latin1", core.CategoryUnsupportedFeature},
		{"encoding", base, "Content-Encoding", "gzip", core.CategoryUnsupportedFeature},
		{"combined encoding", base, "Content-Encoding", "identity, gzip", core.CategoryUnsupportedFeature},
		{"large header", base, "X-Large", strings.Repeat("a", 5000), core.CategoryInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := request(tc.body)
			if tc.header != "" {
				r.Header.Set(tc.header, tc.value)
			}
			got, err := Decode(r, 4096, 4096)
			if err == nil || err.Category != tc.category || err.Message == "" || len(got.Payload.Body) != 0 {
				t.Fatalf("got %+v, %+v", got, err)
			}
		})
	}
}

type countingReader struct {
	source io.Reader
	count  int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.source.Read(p)
	c.count += n
	return n, err
}
func (c *countingReader) Close() error { return nil }

func TestDecodeBodyLimit(t *testing.T) {
	for _, extra := range []string{"", " "} {
		r := request(base + extra)
		counter := &countingReader{source: strings.NewReader(base + extra + strings.Repeat("x", 10000))}
		r.Body = counter
		r.ContentLength = -1 // chunked or otherwise unknown length
		got, err := Decode(r, int64(len(base)), 4096)
		if extra == "" && err == nil && string(got.Payload.Body) == base {
			// A boundary-sized request must end at the boundary, not contain more bytes.
			t.Fatal("accepted over-limit body")
		}
		if err == nil || err.Category != core.CategoryInvalidRequest || counter.count > len(base)+1 {
			t.Fatalf("limit: bytes=%d error=%+v", counter.count, err)
		}
	}
	r := request(base)
	r.ContentLength = -1
	if got, err := Decode(r, int64(len(base)), 4096); err != nil || string(got.Payload.Body) != base {
		t.Fatalf("boundary: %+v %+v", got, err)
	}
	r = request(base + " ")
	r.ContentLength = int64(len(base) + 1)
	if _, err := Decode(r, int64(len(base)), 4096); err == nil || err.Category != core.CategoryInvalidRequest {
		t.Fatalf("known length: %+v", err)
	}
}

func TestDecodeOptionalFeatures(t *testing.T) {
	r := request(base[:len(base)-1] + `,"stream":false,"tools":[],"parallel_tool_calls":true,"text":{"format":{"type":"text"}},"future":{"type":"input_image"}}`)
	r.Header.Set("Content-Type", "application/json; charset=UTF-8")
	r.Header.Set("Content-Encoding", "identity")
	got, err := Decode(r, 4096, 4096)
	if err != nil || got.Metadata.Streaming == nil || *got.Metadata.Streaming || len(got.Capabilities) != 0 {
		t.Fatalf("optional fields: %+v %+v", got, err)
	}
	r = request(base)
	got, err = Decode(r, 4096, 4096)
	if err != nil || got.Metadata.Streaming != nil || len(got.Capabilities) != 0 {
		t.Fatalf("unspecified fields: %+v %+v", got, err)
	}
}

func TestDecodeOpaqueHistoryAndToolImage(t *testing.T) {
	body := base[:len(base)-1] + `,"input":[` +
		`{"role":"assistant","content":[{"type":"output_text","text":"prior"},{"type":"refusal","refusal":"no"},{"type":"reasoning_text","text":"replay"}]},` +
		`{"role":"user","content":[{"type":"input_file","file_id":"file-1"}]},` +
		`{"type":"function_call_output","call_id":"call-1","output":[{"type":"input_image","image_url":"data:image/png;base64,a"},{"type":"future_content","value":{"opaque":1}}]}]}`
	got, err := Decode(request(body), int64(len(body)), 4096)
	if err != nil || string(got.Payload.Body) != body {
		t.Fatalf("agent history: %+v %+v", got, err)
	}
	if len(got.Capabilities) != 1 {
		t.Fatalf("unexpected requirements: %+v", got.Capabilities)
	}
	if _, ok := got.Capabilities["llm.vision"]; !ok {
		t.Fatalf("tool output image needs vision: %+v", got.Capabilities)
	}
}
