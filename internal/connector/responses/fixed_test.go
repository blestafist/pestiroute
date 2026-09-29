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

func fixedRequest() core.ExecutionRequest {
	in := testRequest()
	in.Payload.Protocol = protocol
	return in
}

func fixedFrames(t *testing.T, response fakeupstream.Response) (*core.HeadFrame, []byte, *core.CompleteFrame, int) {
	t.Helper()
	up := fakeupstream.New(response)
	defer up.Close()
	tr := testTransport(up.URL + "/v1/responses")
	defer tr.Close()
	result, failure := tr.ExecuteFixedJSON(context.Background(), fixedRequest())
	if failure != nil {
		t.Fatal(failure)
	}
	defer result.Stream.Close()
	var head *core.HeadFrame
	var body []byte
	var complete *core.CompleteFrame
	parts := 0
	for {
		frame, err := result.Stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch frame.Type {
		case core.FrameHead:
			if head != nil || parts != 0 || complete != nil {
				t.Fatal("head out of order")
			}
			head = frame.Head
		case core.FrameBody:
			if head == nil || complete != nil || len(frame.Body.Data) == 0 || len(frame.Body.Data) > chunkSize {
				t.Fatal("invalid body frame")
			}
			body = append(body, frame.Body.Data...)
			parts++
		case core.FrameComplete:
			if head == nil || complete != nil {
				t.Fatal("complete out of order")
			}
			complete = frame.Complete
		default:
			t.Fatalf("unexpected frame: %+v", frame)
		}
	}
	if head == nil || complete == nil {
		t.Fatal("missing head or complete")
	}
	return head, body, complete, parts
}

func TestExecuteFixedJSON_Success(t *testing.T) {
	data := []byte(`{"status":"completed","usage":{"input_tokens":12,"output_tokens":3},"opaque":"` + strings.Repeat("Z", 80<<10) + `"}`)
	head, body, done, parts := fixedFrames(t, fakeupstream.Response{Body: data, Header: http.Header{"Content-Type": {"application/json"}, "X-Safe": {"ok"}}})
	if !bytes.Equal(body, data) || parts < 2 || *head.HTTPStatus != 200 || http.Header(head.Headers).Get("X-Safe") != "ok" || done.Outcome != core.OutcomeSucceeded || done.Usage.Source != core.UsageProvider || *done.Usage.InputTokens != 12 || *done.Usage.OutputTokens != 3 {
		t.Fatalf("head=%+v parts=%d done=%+v equal=%v", head, parts, done, bytes.Equal(body, data))
	}
	large := []byte(`{"status":"completed","opaque":"` + strings.Repeat("Z", observationLimit) + `"}`)
	_, body, done, _ = fixedFrames(t, fakeupstream.Response{Body: large})
	if !bytes.Equal(body, large) || done.Outcome != core.OutcomeIncomplete || done.Usage.Source != core.UsageUnknown {
		t.Fatal("observation overflow changed bytes or inferred success")
	}
}

func TestExecuteFixedJSON_ProviderFailureIn200(t *testing.T) {
	for _, data := range []string{`{"status":"failed"}`, `{"status":"completed","error":{"message":"private"}}`, `{"status":"incomplete"}`} {
		_, body, done, _ := fixedFrames(t, fakeupstream.Response{Body: []byte(data)})
		want := core.OutcomeFailed
		if strings.Contains(data, "incomplete") {
			want = core.OutcomeIncomplete
		}
		if string(body) != data || done.Outcome != want || done.Error == nil || strings.Contains(done.Error.Message, "private") {
			t.Fatalf("%s: %+v", data, done)
		}
	}
}

func TestExecuteFixedJSON_AmbiguousTerminalMetadata(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"case-folded status", `{"Status":"completed"}`},
		{"case-folded error", `{"status":"completed","Error":null}`},
		{"case-folded usage", `{"status":"completed","Usage":{"input_tokens":1,"output_tokens":2}}`},
		{"duplicate status", `{"status":"failed","status":"completed"}`},
		{"mixed-case status", `{"status":"failed","Status":"completed"}`},
		{"escaped status", `{"status":"failed","sta\u0074us":"completed"}`},
		{"duplicate error", `{"status":"completed","error":{"message":"private"},"error":null}`},
		{"mixed-case error", `{"status":"completed","error":{"message":"private"},"Error":null}`},
		{"escaped error", `{"status":"completed","err\u006fr":{"message":"private"},"error":null}`},
		{"duplicate usage", `{"status":"completed","usage":{"input_tokens":1,"output_tokens":2},"Usage":null}`},
		{"escaped usage", `{"status":"completed","usage":null,"us\u0061ge":{"input_tokens":1,"output_tokens":2}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, body, done, _ := fixedFrames(t, fakeupstream.Response{Body: []byte(tc.body)})
			if string(body) != tc.body || done.Outcome != core.OutcomeIncomplete || done.Error == nil || done.Usage == nil || done.Usage.Source != core.UsageUnknown {
				t.Fatalf("body=%q completion=%+v", body, done)
			}
		})
	}
}

func TestExecuteFixedJSON_Usage(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		outcome    core.Outcome
		known      bool
	}{
		{"cached and reasoning", `{"status":"completed","usage":{"input_tokens":12,"output_tokens":3,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":2}}}`, core.OutcomeSucceeded, true},
		{"failed with usage", `{"status":"failed","usage":{"input_tokens":12,"output_tokens":3,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":2}}}`, core.OutcomeFailed, true},
		{"incomplete with usage", `{"status":"incomplete","usage":{"input_tokens":12,"output_tokens":3,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":2}}}`, core.OutcomeIncomplete, true},
		{"missing usage", `{"status":"completed"}`, core.OutcomeSucceeded, false},
		{"invalid count", `{"status":"completed","usage":{"input_tokens":"12","output_tokens":3}}`, core.OutcomeSucceeded, false},
		{"ambiguous cached", `{"status":"completed","usage":{"input_tokens":12,"output_tokens":3,"input_tokens_details":{"cached_tokens":13}}}`, core.OutcomeSucceeded, false},
		{"ambiguous reasoning", `{"status":"completed","usage":{"input_tokens":12,"output_tokens":3,"output_tokens_details":{"reasoning_tokens":4}}}`, core.OutcomeSucceeded, false},
		{"duplicate input", `{"status":"completed","usage":{"input_tokens":12,"input_tokens":8,"output_tokens":3}}`, core.OutcomeSucceeded, false},
		{"mixed-case duplicate input", `{"status":"completed","usage":{"input_tokens":12,"Input_Tokens":8,"output_tokens":3}}`, core.OutcomeSucceeded, false},
		{"escaped duplicate detail", `{"status":"completed","usage":{"input_tokens":12,"output_tokens":3,"input_tokens_details":{"cached_tokens":1,"cached_\u0074okens":4}}}`, core.OutcomeSucceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, body, done, _ := fixedFrames(t, fakeupstream.Response{Body: []byte(tc.body)})
			if string(body) != tc.body || done.Outcome != tc.outcome || done.Usage == nil {
				t.Fatalf("body=%q outcome=%s usage=%+v", body, done.Outcome, done.Usage)
			}
			if !tc.known {
				if done.Usage.Source != core.UsageUnknown || done.Usage.InputTokens != nil || done.Usage.OutputTokens != nil || done.Usage.CachedTokens != nil || done.Usage.ReasoningTokens != nil {
					t.Fatalf("inferred ambiguous usage: %+v", done.Usage)
				}
				return
			}
			if done.Usage.Source != core.UsageProvider || done.Usage.Completeness != core.UsageComplete || done.Usage.InputTokens == nil || *done.Usage.InputTokens != 12 || done.Usage.OutputTokens == nil || *done.Usage.OutputTokens != 3 || done.Usage.CachedTokens == nil || *done.Usage.CachedTokens != 4 || done.Usage.ReasoningTokens == nil || *done.Usage.ReasoningTokens != 2 {
				t.Fatalf("usage: %+v", done.Usage)
			}
		})
	}
}

func TestExecuteFixedJSON_Rejection(t *testing.T) {
	for _, status := range []int{400, 429, 500} {
		data := []byte(` {"error":"raw private"} `)
		head, body, done, _ := fixedFrames(t, fakeupstream.Response{Status: status, Body: data, Header: http.Header{"X-Request-Id": {"safe"}, "Connection": {"X-Secret"}, "X-Secret": {"hidden"}}})
		if *head.HTTPStatus != status || head.Error == nil || done.Error == nil || done.Outcome != core.OutcomeFailed || !bytes.Equal(body, data) || http.Header(head.Headers).Get("X-Request-Id") != "safe" || http.Header(head.Headers).Get("X-Secret") != "" {
			t.Fatalf("%d: head=%+v done=%+v body=%q", status, head, done, body)
		}
	}
}

func TestExecuteFixedJSON_TruncatedBody(t *testing.T) {
	_, body, done, _ := fixedFrames(t, fakeupstream.Response{Steps: []fakeupstream.Step{{Data: []byte(`{"status":"completed"`), Drop: true}}})
	if string(body) != `{"status":"completed"` || done.Outcome != core.OutcomeIncomplete || done.Error == nil {
		t.Fatalf("%q %+v", body, done)
	}
}

func TestExecuteFixedJSON_PartialReadClose(t *testing.T) {
	gate := make(chan struct{})
	up := fakeupstream.New(fakeupstream.Response{Header: http.Header{"Content-Type": {"application/json"}}, Steps: []fakeupstream.Step{{Data: []byte("{")}, {Gate: gate, Data: []byte(`"status":"completed"}`)}}})
	defer up.Close()
	tr := testTransport(up.URL + "/v1/responses")
	defer tr.Close()
	result, failure := tr.ExecuteFixedJSON(context.Background(), fixedRequest())
	if failure != nil {
		t.Fatal(failure)
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
			t.Fatalf("blocked read: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read not interrupted")
	}
	select {
	case <-capture.Cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request not cancelled")
	}
}

func TestExecuteFixedJSON_ParentCancellation(t *testing.T) {
	gate := make(chan struct{})
	up := fakeupstream.New(fakeupstream.Response{Steps: []fakeupstream.Step{{Gate: gate, Data: []byte(`{"status":"completed"}`)}}})
	defer up.Close()
	tr := testTransport(up.URL + "/v1/responses")
	defer tr.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, failure := tr.ExecuteFixedJSON(ctx, fixedRequest())
	if failure != nil {
		t.Fatal(failure)
	}
	defer result.Stream.Close()
	if frame, err := result.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
		t.Fatalf("head: %+v %v", frame, err)
	}
	capture := <-up.Requests
	cancel()
	frame, err := result.Stream.Next(context.Background())
	if err != nil || frame.Type != core.FrameComplete || frame.Complete.Outcome != core.OutcomeCancelled {
		t.Fatalf("completion: %+v %v", frame, err)
	}
	select {
	case <-capture.Cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream not cancelled")
	}
}

func TestExecuteFixedJSON_EncodedResponseRejected(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		encodings []string
	}{
		{"success single", 200, []string{"gzip"}},
		{"rejection single", 400, []string{"gzip"}},
		{"success multiline", 200, []string{"identity", "gzip"}},
		{"rejection multiline", 400, []string{"identity", "gzip"}},
		{"success comma list", 200, []string{"identity, gzip"}},
		{"rejection comma list", 400, []string{"identity, gzip"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := fakeupstream.New(fakeupstream.Response{Status: tc.status, Header: http.Header{"Content-Encoding": tc.encodings}, Body: []byte("encoded")})
			tr := testTransport(up.URL + "/v1/responses")
			result, failure := tr.ExecuteFixedJSON(context.Background(), fixedRequest())
			tr.Close()
			up.Close()
			if failure == nil || failure.Code != "unsupported_response_encoding" || result.Stream != nil {
				t.Fatalf("status=%d encodings=%q result=%+v error=%+v", tc.status, tc.encodings, result, failure)
			}
		})
	}
}
