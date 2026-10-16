package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestTranslateMessagesRequest(t *testing.T) {
	request := core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol, ContentType: "application/json", Body: []byte(`{"model":"synthetic-model","instructions":"rules","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"tools":[{"type":"function","name":"lookup","description":"query","parameters":{"type":"object"}}],"tool_choice":"required","max_output_tokens":64,"stream":true}`)}}
	out, ge := translateRequest(request, "offline-token", "synthetic-beta")
	if ge != nil {
		t.Fatal(ge)
	}
	if out.URL != messagesURL || out.Header.Get("anthropic-version") != "2023-06-01" || out.Header.Get("anthropic-beta") != "synthetic-beta" || out.Header.Get("Authorization") != "Bearer offline-token" || out.Header.Get("User-Agent") != "PestiRoute" {
		t.Fatalf("request target/headers: %#v %v", out, out.Header)
	}
	if !strings.Contains(string(out.Body), `"role":"user"`) || !strings.Contains(string(out.Body), `"input_schema":{"type":"object"}`) || !strings.Contains(string(out.Body), `"type":"any"`) {
		t.Fatalf("translated body: %s", out.Body)
	}
	for _, bad := range []string{`{"model":"m","input":"x","previous_response_id":"resp_x"}`, `{"model":"m","input":"x","background":true}`, `{"model":"m","input":"x","input":"duplicate"}`} {
		request.Payload.Body = []byte(bad)
		if _, ge := translateRequest(request, "offline-token", "beta"); ge == nil || ge.Code != "unsupported_feature" || ge.Category != core.CategoryUnsupportedFeature {
			t.Fatalf("expected safe unsupported error for %s, got %v", bad, ge)
		}
	}
	request.Payload.Body = []byte(`{"model":"m","input":"x","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":"none"}`)
	if _, ge := translateRequest(request, "offline-token", "beta"); ge == nil || ge.Code != "unsupported_feature" || ge.Category != core.CategoryUnsupportedFeature {
		t.Fatalf("tool_choice none with declared tools must be rejected losslessly, got %v", ge)
	}
}

func TestSSEFixtures(t *testing.T) {
	for _, name := range []string{"stream-text.sse", "stream-tools.sse", "stream-thinking.sse", "stream-usage.sse"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + name)
			if err != nil {
				t.Fatal(err)
			}
			checkStream(t, name, data)
		})
	}
	for _, name := range []string{"stream-malformed.sse", "stream-premature-eof.sse", "stream-invalid-utf8.sse", "stream-midstream-disconnect.sse", "stream-error.sse"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + name)
			if err != nil {
				t.Fatal(err)
			}
			frames := collect(newMessagesStream(strings.NewReader(string(data))))
			if len(frames) == 0 || frames[len(frames)-1].Type != core.FrameComplete || frames[len(frames)-1].Complete.Outcome != core.OutcomeFailed {
				t.Fatalf("expected failed terminal frame, got %#v", frames)
			}
		})
	}
}

func TestSSEStreamsBeforeTerminalAndBoundsFinalProjection(t *testing.T) {
	prefix := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"synthetic\",\"content\":[],\"usage\":{\"input_tokens\":1}}}\n\n"
	reader := &countReader{Reader: strings.NewReader(prefix)}
	s := newMessagesStream(reader)
	if f, err := s.Next(context.Background()); err != nil || f.Type != core.FrameHead || reader.reads != 1 {
		t.Fatalf("Head should follow message_start only: frame=%+v reads=%d err=%v", f, reader.reads, err)
	}
	if f, err := s.Next(context.Background()); err != nil || f.Type != core.FrameBody || !strings.Contains(string(f.Body.Data), "response.created") || reader.reads != 1 {
		t.Fatalf("response.created should be delivered before EOF/read-ahead: frame=%+v reads=%d err=%v", f, reader.reads, err)
	}

	large := strings.Repeat("x", 600_000)
	stream := prefix + "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", large) +
		fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", large)
	s = newMessagesStream(strings.NewReader(stream))
	seenDelta := false
	for range 20 {
		f, err := s.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if f.Type == core.FrameBody && strings.Contains(string(f.Body.Data), "response.output_text.delta") {
			seenDelta = true
			break
		}
	}
	if !seenDelta {
		t.Fatal("first content delta was not delivered incrementally")
	}
	for range 20 {
		f, err := s.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if f.Type == core.FrameComplete {
			if f.Complete.Outcome != core.OutcomeFailed || f.Complete.Error == nil {
				t.Fatalf("expected bounded-retention failure, got %+v", f.Complete)
			}
			return
		}
	}
	t.Fatal("oversized retained final projection was not rejected")
}

type countReader struct {
	*strings.Reader
	reads int
}

func (r *countReader) Read(p []byte) (int, error) { r.reads++; return r.Reader.Read(p) }

func checkStream(t *testing.T, name string, data []byte) {
	t.Helper()
	frames := collect(newMessagesStream(strings.NewReader(string(data))))
	if len(frames) < 3 || frames[0].Type != core.FrameHead || frames[len(frames)-1].Type != core.FrameComplete || frames[len(frames)-1].Complete.Outcome != core.OutcomeSucceeded {
		var terminal any
		if len(frames) > 0 && frames[len(frames)-1].Complete != nil {
			terminal = frames[len(frames)-1].Complete.Error
		}
		t.Fatalf("unexpected terminal: %#v; frame count %d", terminal, len(frames))
	}
	var body strings.Builder
	for _, f := range frames {
		if f.Type == core.FrameBody {
			body.Write(f.Body.Data)
		}
	}
	names, payloads := responseEvents(t, body.String())
	want := map[string][]string{
		"stream-text.sse":     {"response.created", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed"},
		"stream-tools.sse":    {"response.created", "response.output_item.added", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.output_item.done", "response.completed"},
		"stream-thinking.sse": {"response.created", "response.output_item.added", "response.reasoning_summary_part.added", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_summary_part.done", "response.output_item.done", "response.completed"},
		"stream-usage.sse":    {"response.created", "response.completed"},
	}
	if !reflect.DeepEqual(names, want[name]) {
		t.Fatalf("Responses event order = %v, want %v", names, want[name])
	}
	for i, p := range payloads {
		if p["sequence_number"] != float64(i) || p["type"] != names[i] {
			t.Fatalf("event %d payload metadata = %#v", i, p)
		}
	}
	if frames[len(frames)-2].Type != core.FrameBody || names[len(names)-1] != "response.completed" {
		t.Fatalf("response.completed body must precede control Complete: %#v", frames)
	}
	response, ok := payloads[len(payloads)-1]["response"].(map[string]any)
	if !ok || response["status"] != "completed" {
		t.Fatalf("final response metadata = %#v", payloads[len(payloads)-1])
	}
	output, ok := response["output"].([]any)
	if !ok {
		t.Fatalf("final response output missing: %#v", response)
	}
	switch name {
	case "stream-text.sse":
		item := outputItem(t, output)
		content, ok := item["content"].([]any)
		if !ok || len(content) != 1 || content[0].(map[string]any)["text"] != "Synthetic hello." || item["status"] != "completed" {
			t.Fatalf("text final output = %#v", output)
		}
	case "stream-tools.sse":
		item := outputItem(t, output)
		if item["type"] != "function_call" || item["arguments"] != `{"query":"offline"}` || item["status"] != "completed" {
			t.Fatalf("tool final output = %#v", output)
		}
	case "stream-thinking.sse":
		item := outputItem(t, output)
		summary, ok := item["summary"].([]any)
		if !ok || len(summary) != 1 || summary[0].(map[string]any)["text"] != "Synthetic reasoning." || item["status"] != "completed" {
			t.Fatalf("reasoning final output = %#v", output)
		}
	case "stream-usage.sse":
		if len(output) != 0 {
			t.Fatalf("empty response output = %#v", output)
		}
	}
	usage := frames[len(frames)-1].Complete.Usage
	if usage == nil || usage.Source != core.UsageProvider || usage.Completeness != core.UsageComplete {
		t.Fatalf("usage report: %#v", usage)
	}
	if strings.Contains(string(data), `"input_tokens":23`) && (usage.InputTokens == nil || *usage.InputTokens != 23 || usage.OutputTokens == nil || *usage.OutputTokens != 9 || usage.CachedTokens != nil) {
		t.Fatalf("cache details must not be added to input usage: %#v", usage)
	}
	wantUsage := map[string][2]float64{"stream-text.sse": {11, 4}, "stream-tools.sse": {8, 5}, "stream-thinking.sse": {7, 3}, "stream-usage.sse": {23, 9}}
	finalUsage, ok := response["usage"].(map[string]any)
	w := wantUsage[name]
	if !ok || finalUsage["input_tokens"] != w[0] || finalUsage["output_tokens"] != w[1] || finalUsage["total_tokens"] != w[0]+w[1] {
		t.Fatalf("final response usage = %#v", response["usage"])
	}
}

func outputItem(t *testing.T, output []any) map[string]any {
	t.Helper()
	if len(output) != 1 {
		t.Fatalf("expected one final output item, got %#v", output)
	}
	item, ok := output[0].(map[string]any)
	if !ok {
		t.Fatalf("final item shape = %#v", output[0])
	}
	return item
}

func responseEvents(t *testing.T, body string) ([]string, []map[string]any) {
	t.Helper()
	var names []string
	var payloads []map[string]any
	for _, chunk := range strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n") {
		lines := strings.SplitN(chunk, "\n", 2)
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("malformed mapped SSE event %q", chunk)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &payload); err != nil {
			t.Fatal(err)
		}
		name := strings.TrimPrefix(lines[0], "event: ")
		names = append(names, name)
		payloads = append(payloads, payload)
	}
	return names, payloads
}

func collect(s *messagesStream) []core.StreamFrame {
	var frames []core.StreamFrame
	for range 100 {
		f, err := s.Next(context.Background())
		if err == io.EOF {
			return frames
		}
		if err != nil {
			return frames
		}
		frames = append(frames, f)
		if f.Type == core.FrameComplete {
			return frames
		}
	}
	return frames
}
