package responses

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
)

func sseFrames(t *testing.T, response fakeupstream.Response) ([]byte, *core.CompleteFrame, int) {
	t.Helper()
	up := fakeupstream.New(response)
	defer up.Close()
	tr := testTransport(up.URL + "/v1/responses")
	defer tr.Close()
	result, err := tr.ExecuteSSE(context.Background(), fixedRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer result.Stream.Close()
	var raw []byte
	var done *core.CompleteFrame
	count := 0
	for {
		f, e := result.Stream.Next(context.Background())
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		switch f.Type {
		case core.FrameHead:
			if count != 0 {
				t.Fatal("head out of order")
			}
			count++
		case core.FrameBody:
			if count < 1 || done != nil || len(f.Body.Data) == 0 || len(f.Body.Data) > chunkSize {
				t.Fatal("body out of order")
			}
			raw = append(raw, f.Body.Data...)
			count++
		case core.FrameComplete:
			if done != nil {
				t.Fatal("duplicate complete")
			}
			done = f.Complete
		default:
			t.Fatal("unexpected frame")
		}
	}
	if done == nil {
		t.Fatal("missing completion")
	}
	return raw, done, count
}
func terminal(kind, status, usage string) string {
	return "event: " + kind + "\ndata: {\"type\":\"" + kind + "\",\"response\":{\"status\":\"" + status + "\"" + usage + "}}\n\n"
}
func TestExecuteSSE_GatedIncrementalBodies(t *testing.T) {
	gate := make(chan struct{})
	first := []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
	last := []byte(terminal("response.completed", "completed", ""))
	up := fakeupstream.New(fakeupstream.Response{Steps: []fakeupstream.Step{{Data: first}, {Gate: gate, Data: last}}})
	defer up.Close()
	tr := testTransport(up.URL + "/v1/responses")
	defer tr.Close()
	result, fail := tr.ExecuteSSE(context.Background(), fixedRequest())
	if fail != nil {
		t.Fatal(fail)
	}
	defer result.Stream.Close()
	capture := <-up.Requests
	_ = capture
	head, err := result.Stream.Next(context.Background())
	if err != nil || head.Type != core.FrameHead {
		t.Fatalf("head %v %v", head, err)
	}
	body, err := result.Stream.Next(context.Background())
	if err != nil || body.Type != core.FrameBody || !bytes.Equal(body.Body.Data, first) {
		t.Fatalf("first body %v %v", body, err)
	}
	close(gate)
	var raw []byte
	raw = append(raw, body.Body.Data...)
	var done *core.CompleteFrame
	for {
		f, e := result.Stream.Next(context.Background())
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if f.Type == core.FrameBody {
			raw = append(raw, f.Body.Data...)
		}
		if f.Type == core.FrameComplete {
			done = f.Complete
		}
	}
	if !bytes.Equal(raw, append(first, last...)) || done == nil || done.Outcome != core.OutcomeSucceeded {
		t.Fatalf("raw match=%v completion=%+v", bytes.Equal(raw, append(first, last...)), done)
	}
}
func TestExecuteSSE_TerminalClassification(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       core.Outcome
	}{
		{"completed", terminal("response.completed", "completed", ""), core.OutcomeSucceeded},
		{"failed", terminal("response.failed", "failed", ""), core.OutcomeFailed},
		{"incomplete", terminal("response.incomplete", "incomplete", ""), core.OutcomeIncomplete},
		{"error", "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"private\"}}\n\n", core.OutcomeFailed},
		{"missing", "event: response.created\ndata: {\"type\":\"response.created\"}\n\n", core.OutcomeIncomplete},
		{"malformed", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",}}\n\n", core.OutcomeIncomplete},
		{"status mismatch", terminal("response.completed", "failed", ""), core.OutcomeIncomplete},
		{"scalar overflow", terminal("response.completed", strings.Repeat("x", scalarLimit+1), ""), core.OutcomeIncomplete},
		{"duplicate status", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"failed\",\"status\":\"completed\"}}\n\n", core.OutcomeIncomplete},
		{"invalid usage", terminal("response.completed", "completed", `,"usage":{"input_tokens":"12","output_tokens":3}`), core.OutcomeIncomplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, done, _ := sseFrames(t, fakeupstream.Response{Body: []byte(tc.body), Header: http.Header{"Content-Type": {"text/event-stream"}}})
			if string(raw) != tc.body || done.Outcome != tc.want || done.Usage.Source != core.UsageUnknown {
				t.Fatalf("raw=%q done=%+v", raw, done)
			}
		})
	}
	raw, done, _ := sseFrames(t, fakeupstream.Response{Steps: []fakeupstream.Step{{Data: []byte(terminal("response.completed", "completed", "")), Drop: true}}})
	if len(raw) == 0 || done.Outcome != core.OutcomeIncomplete {
		t.Fatalf("truncated %+v", done)
	}
}
func TestExecuteSSE_LargeEventBoundedScratch(t *testing.T) {
	raw := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"text\":\"" + strings.Repeat("X", 300<<10) + "\"}],\"status\":\"completed\",\"usage\":{\"input_tokens\":12,\"output_tokens\":3,\"input_tokens_details\":{\"cached_tokens\":4},\"output_tokens_details\":{\"reasoning_tokens\":2}}}}\n\n")
	got, done, parts := sseFrames(t, fakeupstream.Response{Body: raw, Header: http.Header{"Content-Type": {"text/event-stream"}}})
	if !bytes.Equal(got, raw) || parts < 3 || done.Outcome != core.OutcomeSucceeded || done.Usage.Source != core.UsageProvider || *done.Usage.CachedTokens != 4 || *done.Usage.ReasoningTokens != 2 {
		t.Fatalf("parts=%d done=%+v raw=%v", parts, done, bytes.Equal(got, raw))
	}
	largeKey := []byte("event: response.completed\ndata: {\"" + strings.Repeat("z", 300<<10) + "\":true,\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	got, done, _ = sseFrames(t, fakeupstream.Response{Body: largeKey, Header: http.Header{"Content-Type": {"text/event-stream"}}})
	if !bytes.Equal(got, largeKey) || done.Outcome != core.OutcomeSucceeded {
		t.Fatalf("large opaque key: raw=%v done=%+v", bytes.Equal(got, largeKey), done)
	}
}

func TestExecuteSSE_NestedOutputMetadataIsOpaque(t *testing.T) {
	for _, tc := range []struct {
		name, output string
	}{
		{"array item type", `[{"type":"message"}]`},
		{"array item response", `[{"response":{"status":"failed","usage":{"input_tokens":99,"output_tokens":99}},"type":"message"}]`},
		{"unobserved object", `{"payload":{"type":"message","response":{"status":"failed"}}}`},
		{"large array item", `[{"type":"message","text":"` + strings.Repeat("x", 300<<10) + `"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`event: response.completed` + "\n" + `data: {"type":"response.completed","response":{"output":` + tc.output + `,"status":"completed","usage":{"input_tokens":4,"output_tokens":2}}}` + "\n\n")
			got, done, _ := sseFrames(t, fakeupstream.Response{Body: raw, Header: http.Header{"Content-Type": {"text/event-stream"}}})
			if !bytes.Equal(got, raw) || done.Outcome != core.OutcomeSucceeded || done.Usage.Source != core.UsageProvider || *done.Usage.InputTokens != 4 || *done.Usage.OutputTokens != 2 {
				t.Fatalf("raw=%v completion=%+v", bytes.Equal(got, raw), done)
			}
		})
	}
}

func TestExecuteSSE_PostTerminalDataIncomplete(t *testing.T) {
	first := terminal("response.completed", "completed", `,"usage":{"input_tokens":4,"output_tokens":2}`)
	for _, tc := range []struct{ name, suffix string }{
		{"second completed", terminal("response.completed", "completed", "")},
		{"later failed", terminal("response.failed", "failed", "")},
		{"later created", "event: response.created\ndata: {\"type\":\"response.created\"}\n\n"},
		{"later raw data", "data: {}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := first + tc.suffix
			got, done, _ := sseFrames(t, fakeupstream.Response{Body: []byte(raw), Header: http.Header{"Content-Type": {"text/event-stream"}}})
			if string(got) != raw || done.Outcome != core.OutcomeIncomplete || done.Error == nil || done.Usage.Source != core.UsageUnknown {
				t.Fatalf("raw=%v completion=%+v", string(got) == raw, done)
			}
		})
	}
}
func TestExecuteSSE_MultilineAndSplitUTF8(t *testing.T) {
	data := []byte("event: response.completed\r\ndata: {\"type\":\"response.completed\",\r\ndata: \"note\":\"é\",\"response\":{\"status\":\"completed\"}}\r\n\r\n")
	split := bytes.Index(data, []byte("é")) + 1
	steps := []fakeupstream.Step{{Data: data[:split]}, {Data: data[split:]}}
	got, done, _ := sseFrames(t, fakeupstream.Response{Steps: steps})
	if !bytes.Equal(got, data) || done.Outcome != core.OutcomeSucceeded {
		t.Fatalf("raw=%v done=%+v", bytes.Equal(got, data), done)
	}
}
func TestExecuteSSE_EncodedResponseRejected(t *testing.T) {
	for _, status := range []int{200, 400} {
		up := fakeupstream.New(fakeupstream.Response{Status: status, Header: http.Header{"Content-Encoding": {"identity", "gzip"}}, Body: []byte("private")})
		tr := testTransport(up.URL + "/v1/responses")
		result, err := tr.ExecuteSSE(context.Background(), fixedRequest())
		tr.Close()
		up.Close()
		if result.Stream != nil || err == nil || err.Code != "unsupported_response_encoding" {
			t.Fatalf("status=%d result=%+v err=%+v", status, result, err)
		}
	}
}
func TestExecuteSSE_EarlyCloseCancellation(t *testing.T) {
	gate := make(chan struct{})
	up := fakeupstream.New(fakeupstream.Response{Steps: []fakeupstream.Step{{Data: []byte("data: {}\n\n")}, {Gate: gate, Data: []byte("data: {}\n\n")}}})
	defer up.Close()
	tr := testTransport(up.URL + "/v1/responses")
	defer tr.Close()
	result, fail := tr.ExecuteSSE(context.Background(), fixedRequest())
	if fail != nil {
		t.Fatal(fail)
	}
	capture := <-up.Requests
	for i := 0; i < 2; i++ {
		if _, err := result.Stream.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	read := make(chan error, 1)
	go func() { _, err := result.Stream.Next(context.Background()); read <- err }()
	if err := result.Stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := result.Stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-read:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("read: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked read")
	}
	select {
	case <-capture.Cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream not cancelled")
	}
}
