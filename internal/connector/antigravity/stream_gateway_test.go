package antigravity

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	chatadapter "github.com/blestafist/pestiroute/internal/adapter/chatcompletions"
	responsesadapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/core"
)

func TestConnectorAdapterLoopbackStreamFailuresAndIncrementality(t *testing.T) {
	responses := responsesadapter.NewAdapter()
	if err := responses.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"max_body_bytes":1048576,"max_header_bytes":65536}`)}); err != nil {
		t.Fatal(err)
	}
	chat := chatadapter.NewAdapter()
	if err := chat.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"max_body_bytes":1048576,"max_header_bytes":65536}`)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = responses.Close(context.Background()); _ = chat.Close(context.Background()) })
	for _, tc := range []struct {
		name       string
		wire       string
		wantStatus int
		wantBody   string
	}{
		{name: "empty initial frame", wantStatus: http.StatusServiceUnavailable, wantBody: `"code":"upstream_stream_error"`},
		{name: "malformed initial frame", wire: "data: {bad}\n\n", wantStatus: http.StatusServiceUnavailable, wantBody: `"code":"upstream_stream_error"`},
		{name: "postcommit failure", wire: "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}}\n\n", wantStatus: http.StatusOK, wantBody: "partial"},
	} {
		for _, adapterName := range []string{"responses", "chat"} {
			t.Run(tc.name+"/"+adapterName, func(t *testing.T) {
				upstream := &fakeDoer{response: response(200, tc.wire)}
				result, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"synthetic","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{Transport: packageTestDoer(upstream), Credentials: credentials{data: []byte(`{"access_token":"synthetic-token","project_id":"synthetic-project"}`)}})
				if ge != nil {
					t.Fatalf("execute returned error: %v", ge)
				}
				h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if adapterName == "chat" {
						_ = chat.Encode(r.Context(), core.ClientResponse{Transport: chatadapter.HTTPResponse{Writer: w, Request: r}}, nil, result)
					} else {
						_ = responses.Encode(r.Context(), core.ClientResponse{Transport: responsesadapter.HTTPResponse{Writer: w, Request: r}}, nil, result)
					}
				})
				server := httptest.NewServer(h)
				defer server.Close()
				resp, err := server.Client().Get(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil || resp.StatusCode != tc.wantStatus || !strings.Contains(string(body), tc.wantBody) {
					t.Fatalf("status=%d err=%v body=%q", resp.StatusCode, err, body)
				}
				if tc.name == "postcommit failure" && strings.Contains(string(body), `"error"`) {
					t.Fatalf("postcommit stream was rewritten: %q", body)
				}
			})
		}
	}

	var previousBody string
	for _, newline := range []string{"\n", "\r\n"} {
		t.Run("valid incremental "+strings.ReplaceAll(newline, "\r", "CR"), func(t *testing.T) {
			wire := strings.Join([]string{
				`data: {"response":{"candidates":[{"content":{"parts":[{"text":"one"}]}}]}}`,
				`data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]}}]}}`,
				`data: {"response":{"candidates":[{"content":{"parts":[{"text":" two"}]}}]}}`,
				`data: {"response":{"done":true}}`,
			}, newline+newline) + newline + newline
			upstream := &fakeDoer{response: response(200, wire)}
			result, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"synthetic","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{Transport: packageTestDoer(upstream), Credentials: credentials{data: []byte(`{"access_token":"synthetic-token","project_id":"synthetic-project"}`)}})
			if ge != nil {
				t.Fatal(ge)
			}
			stream := result.Stream.(*stream)
			stream.responseID, stream.messageID = "resp_fixture", "msg_fixture"
			rr := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if err := responses.Encode(r.Context(), core.ClientResponse{Transport: responsesadapter.HTTPResponse{Writer: rr, Request: r}}, nil, result); err != nil {
				t.Fatal(err)
			}
			body := rr.Body.String()
			if rr.Code != http.StatusOK || !strings.Contains(body, `"delta":"one"`) || !strings.Contains(body, `"delta":" two"`) || !strings.Contains(body, "response.function_call_arguments.delta") || !strings.Contains(body, "response.completed") {
				t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
			}
			if previousBody != "" && body != previousBody {
				t.Fatalf("LF/CRLF output differs\nLF: %q\nCRLF: %q", previousBody, body)
			}
			previousBody = body
		})
	}
}

type chunkReadCloser struct {
	r     *strings.Reader
	chunk int
}

func (c *chunkReadCloser) Read(p []byte) (int, error) {
	if len(p) > c.chunk {
		p = p[:c.chunk]
	}
	return c.r.Read(p)
}
func (*chunkReadCloser) Close() error { return nil }

func TestSSEStandardsFramingAcrossChunkBoundaries(t *testing.T) {
	data := `{"response":{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}}`
	for _, tc := range []struct{ name, wire string }{
		{"LF+BOM+metadata+multiline", "\xef\xbb\xbf:comment\nevent: message\nid: x\nretry: 1\nunknown: value\ncolonless\ndata: {\ndata: \"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}}\n\n"},
		{"CR", strings.ReplaceAll("data: "+data+"\n\n", "\n", "\r")},
		{"CR+BOM", "\xef\xbb\xbf" + strings.ReplaceAll("data: "+data+"\n\n", "\n", "\r")},
		{"CRLF", "data: " + data + "\r\n\r\n"},
		{"CRLF+BOM", "\xef\xbb\xbfdata: " + data + "\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for chunk := 1; chunk <= 7; chunk++ {
				s := newStream(&chunkReadCloser{r: strings.NewReader(tc.wire), chunk: chunk}, "r", "m")
				event, err := s.nextEvent(context.Background())
				if err != nil {
					t.Fatalf("chunk %d: %v", chunk, err)
				}
				if tc.name == "LF+BOM+metadata+multiline" {
					if string(event) != "{\n"+data[1:] {
						t.Fatalf("chunk %d event=%q", chunk, event)
					}
				} else if string(event) != data {
					t.Fatalf("chunk %d event=%q", chunk, event)
				}
			}
		})
	}
}

func TestSSEUnterminatedFinalDataIsNotDispatched(t *testing.T) {
	for _, wire := range []string{
		`data: {"response":{"done":true}}`,
		"data: {\"response\":{\"done\":true}}\r",
		"data: {\"response\":{\"done\":true}}\n",
	} {
		s := newStream(&chunkReadCloser{r: strings.NewReader(wire), chunk: 1}, "r", "m")
		if event, err := s.nextEvent(context.Background()); !errors.Is(err, io.ErrUnexpectedEOF) || event != nil {
			t.Fatalf("unterminated event dispatched: event=%q err=%v", event, err)
		}
	}
}

func TestSSELimitsAreIncremental(t *testing.T) {
	for name, wire := range map[string]string{
		"line":  "unknown: " + strings.Repeat("x", maxSSELine),
		"event": "data: " + strings.Repeat("x", maxEventSize) + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			s := newStream(&chunkReadCloser{r: strings.NewReader(wire), chunk: 13}, "r", "m")
			if _, err := s.nextEvent(context.Background()); !errors.Is(err, errSSEBounds) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestStreamDebugFailureClassificationIsSecretSafe(t *testing.T) {
	for _, tc := range []struct{ wire, category string }{
		{"", "precommit_eof"},
		{"data: {PRIVATE-BODY}\n\n", "precommit_invalid_json"},
		{`data: {"response":{"candidates":[{"finishReason":"MAX_TOKENS","content":{"parts":[{"text":"PRIVATE-TEXT"}]}}]}}` + "\n\n", "precommit_unsupported_finish"},
		{`data: {"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"PRIVATE-REASONING","thoughtSignature":null}]}}]}}` + "\n\n", "precommit_unsupported_reasoning"},
		{`data: {"response":{"usageMetadata":{"promptTokenCount":-1}}}` + "\n\n", "precommit_invalid_usage"},
		{`data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup"}}]}}]}}` + "\n\n", "precommit_invalid_function"},
		{`data: {"response":{"candidates":[{"content":{"parts":[]}}]}}` + "\n\n", "precommit_empty_content"},
	} {
		var logs strings.Builder
		logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		result, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"PRIVATE-PROMPT","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{Transport: packageTestDoer(&fakeDoer{response: response(200, tc.wire)}), Credentials: credentials{data: []byte(`{"access_token":"synthetic-token","project_id":"synthetic-project"}`)}, Logger: logger})
		if ge != nil {
			t.Fatal(ge)
		}
		_, err := result.Stream.Next(context.Background())
		if err == nil {
			t.Fatal("expected initial frame failure")
		}
		got := logs.String()
		if !strings.Contains(got, "phase=precommit") || !strings.Contains(got, "category="+tc.category) || strings.Contains(got, "PRIVATE-") || strings.Contains(got, "https://") {
			t.Fatalf("unsafe/missing stream diagnostic: %q", got)
		}
		_ = result.Stream.Close()
	}
}

func TestUnsupportedReasoningDiagnosticMatrix(t *testing.T) {
	const canary = "PRIVATE-REASONING-SIGNATURE-CANARY"
	thoughts := []struct {
		name, raw string
		active    bool
	}{{"true", "true", true}, {"false", "false", false}, {"missing", "", false}}
	signatures := []struct{ name, raw, kind string }{{"absent", "", "absent"}, {"null", `null`, "null"}, {"string", `"` + canary + `"`, "string"}, {"object", `{"value":"` + canary + `"}`, "other"}, {"bool", `true`, "other"}, {"empty", `""`, "string"}}
	for _, thought := range thoughts {
		for _, signature := range signatures {
			t.Run(thought.name+"/"+signature.name, func(t *testing.T) {
				fields := `"text":"PRIVATE-CANDIDATE-TEXT"`
				if thought.raw != "" {
					fields += `,"thought":` + thought.raw
				}
				if signature.raw != "" {
					fields += `,"thoughtSignature":` + signature.raw
				}
				raw := []byte(`{"response":{"candidates":[{"content":{"parts":[{` + fields + `}]}}]}}`)
				var logs strings.Builder
				s := newStream(&chunkReadCloser{r: strings.NewReader("")}, "r", "m", slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
				err := s.consume(raw)
				fails := signature.raw != ""
				if !fails {
					if err != nil {
						t.Fatalf("unexpected rejection: %v", err)
					}
					return
				}
				if !errors.Is(err, errUnsupportedReasoning) {
					t.Fatalf("error=%v", err)
				}
				s.logStreamError(err, true)
				kind := "signature_only"
				if thought.active && signature.raw != "" {
					kind = "both"
				} else if thought.active {
					kind = "thought_only"
				}
				got := logs.String()
				for _, want := range []string{"category=precommit_unsupported_reasoning", "kind=" + kind, "signature_type=" + signature.kind} {
					if !strings.Contains(got, want) {
						t.Errorf("missing %q in %q", want, got)
					}
				}
				if strings.Contains(got, "PRIVATE-") || strings.Contains(got, canary) {
					t.Fatalf("leaked private data: %q", got)
				}
			})
		}
	}
}

func TestSemanticConsumeErrorsHaveFixedCategories(t *testing.T) {
	const private = "PRIVATE-SEMANTIC-CANARY"
	tests := []struct {
		name, raw, category string
		prepare             func(*stream)
	}{
		{"missing function args", `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup"}}]}}]}}`, "precommit_invalid_function", nil},
		{"extra function field", `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{},"extra":"` + private + `"}}]}}]}}`, "precommit_invalid_function", nil},
		{"scalar function args", `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":1}}]}}]}}`, "precommit_invalid_function", nil},
		{"invalid function id", `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{},"id":" "}}]}}]}}`, "precommit_invalid_function", nil},
		{"duplicate function ids", `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{},"id":"same"}},{"functionCall":{"name":"lookup","args":{},"id":"same"}}]}}]}}`, "precommit_invalid_function", nil},
		{"no candidates", `{"response":{"candidates":[]}}`, "precommit_empty_content", nil},
		{"empty parts", `{"response":{"candidates":[{"content":{"parts":[]}}]}}`, "precommit_empty_content", nil},
		{"unsupported part", `{"response":{"candidates":[{"content":{"parts":[{}]}}]}}`, "precommit_empty_content", nil},
		{"empty text", `{"response":{"candidates":[{"content":{"parts":[{"text":""}]}}]}}`, "precommit_invalid_content", nil},
		{"terminal candidate content", `{"response":{"done":true,"candidates":[{"content":{"parts":[{"text":"x"}]}}]}}`, "precommit_invalid_content", nil},
		{"after terminal", `{"response":{"done":true}}`, "precommit_invalid_order", func(s *stream) { s.terminal = true }},
		{"after finish", `{"response":{"candidates":[{"content":{"parts":[{"text":"x"}]}}]}}`, "precommit_invalid_order", func(s *stream) { s.sawFinish = true }},
		{"oversized text", `{"response":{"candidates":[{"content":{"parts":[{"text":"` + strings.Repeat("x", maxRequestBytes+1) + `"}]}}]}}`, "precommit_bounds_exceeded", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs strings.Builder
			s := newStream(&chunkReadCloser{r: strings.NewReader(""), chunk: 1}, "r", "m", slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			if tc.prepare != nil {
				tc.prepare(s)
			}
			err := s.consume([]byte(tc.raw))
			if err == nil {
				t.Fatal("expected semantic rejection")
			}
			s.logStreamError(err, true)
			if !strings.Contains(logs.String(), "category="+tc.category) || strings.Contains(logs.String(), private) {
				t.Fatalf("category or privacy mismatch: %q", logs.String())
			}
		})
	}
}

func TestUnsignedThoughtSummaryResponsesLifecycleAndOrder(t *testing.T) {
	wire := strings.Join([]string{
		`data: {"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"a"},{"text":"x"}]}}]}}`,
		`data: {"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"b"},{"functionCall":{"name":"lookup","args":{}}}]}}]}}`,
		`data: {"response":{"candidates":[{"finishReason":"STOP","content":{"parts":[{"thought":true,"text":"c"}]}}]}}`,
	}, "\n\n") + "\n\n"
	s := newStream(&chunkReadCloser{r: strings.NewReader(wire), chunk: 7}, "resp_test", "msg_test")
	var got []string
	var bodies []string
	for {
		frame, err := s.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameBody {
			body := string(frame.Body.Data)
			bodies = append(bodies, body)
			got = append(got, strings.TrimPrefix(strings.SplitN(body, "\n", 2)[0], "event: "))
		}
	}
	want := []string{
		"response.created",
		"response.output_item.added", "response.reasoning_summary_part.added", "response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done", "response.reasoning_summary_part.done", "response.output_item.done",
		"response.output_item.added", "response.output_text.delta",
		"response.output_item.added", "response.reasoning_summary_part.added", "response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done", "response.reasoning_summary_part.done", "response.output_item.done",
		"response.output_item.added", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.output_item.done",
		"response.output_item.added", "response.reasoning_summary_part.added", "response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done", "response.reasoning_summary_part.done", "response.output_item.done",
		"response.output_text.done", "response.output_item.done", "response.completed",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event order:\n got %v\nwant %v", got, want)
	}
	for _, check := range []string{
		`"item_id":"rs_resp_test_0","output_index":0`,
		`"item_id":"rs_resp_test_2","output_index":2`,
		`"item_id":"rs_resp_test_4","output_index":4`,
		`"output_index":1`, `"output_index":3`,
		`"text":"c","type":"summary_text"`,
	} {
		if !strings.Contains(strings.Join(bodies, ""), check) {
			t.Errorf("missing %q", check)
		}
	}
}

func TestGatewayDeliversThoughtDeltaBeforeUpstreamCompletes(t *testing.T) {
	responses := responsesadapter.NewAdapter()
	if err := responses.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"max_body_bytes":1048576,"max_header_bytes":65536}`)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = responses.Close(context.Background()) })
	upstreamReader, upstreamWriter := io.Pipe()
	t.Cleanup(func() { _ = upstreamReader.Close(); _ = upstreamWriter.Close() })
	upstream := &fakeDoer{do: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: upstreamReader}, nil
	}}
	result, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"synthetic","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{Transport: packageTestDoer(upstream), Credentials: credentials{data: []byte(`{"access_token":"synthetic-token","project_id":"synthetic-project"}`)}})
	if ge != nil {
		t.Fatal(ge)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = responses.Encode(r.Context(), core.ClientResponse{Transport: responsesadapter.HTTPResponse{Writer: w, Request: r}}, nil, result)
	})
	server := httptest.NewServer(h)
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	type resultWithError struct {
		resp *http.Response
		err  error
	}
	responseReady := make(chan resultWithError, 1)
	go func() { resp, err := client.Get(server.URL); responseReady <- resultWithError{resp, err} }()
	_, _ = upstreamWriter.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"thought\":true,\"text\":\"early\"}]}}]}}\n\n"))
	resultResponse := <-responseReady
	if resultResponse.err != nil {
		t.Fatal(resultResponse.err)
	}
	resp := resultResponse.resp
	var got strings.Builder
	reader := bufio.NewReader(resp.Body)
	for {
		line, readErr := reader.ReadString('\n')
		got.WriteString(line)
		if strings.Contains(got.String(), `"delta":"early"`) {
			break
		}
		if readErr != nil {
			_ = resp.Body.Close()
			t.Fatalf("delta not delivered before upstream completion: %v; body=%q", readErr, got.String())
		}
	}
	_ = upstreamWriter.Close()
	_ = resp.Body.Close()
	if !strings.Contains(got.String(), `"delta":"early"`) {
		t.Fatalf("missing early delta: %q", got.String())
	}
}

func TestMixedThoughtSignatureFrameIsAtomic(t *testing.T) {
	s := newStream(&chunkReadCloser{r: strings.NewReader("")}, "resp_atomic", "msg_atomic")
	good := []byte(`{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"safe"}]}}]}}`)
	if err := s.consume(good); err != nil {
		t.Fatal(err)
	}
	beforeEvents, beforeOutputs, beforeRetained, beforeIndex := len(s.pending), len(s.outputs), s.retainedBytes, s.outputIndex
	bad := []byte(`{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"not-emitted"},{"text":"signed","thoughtSignature":null}]}}]}}`)
	if err := s.consume(bad); !errors.Is(err, errUnsupportedReasoning) {
		t.Fatalf("err=%v", err)
	}
	if len(s.pending) != beforeEvents || len(s.outputs) != beforeOutputs || s.retainedBytes != beforeRetained || s.outputIndex != beforeIndex || s.reasoningText.String() != "safe" {
		t.Fatalf("frame partially committed: pending=%d outputs=%d retained=%d index=%d reasoning=%q", len(s.pending), len(s.outputs), s.retainedBytes, s.outputIndex, s.reasoningText.String())
	}
	for _, event := range s.pending {
		if strings.Contains(string(event), "not-emitted") || strings.Contains(string(event), "signed") {
			t.Fatal("rejected frame leaked event text")
		}
	}
}

func TestThoughtSummarySharesTerminalRetentionCeiling(t *testing.T) {
	s := newStream(&chunkReadCloser{r: strings.NewReader("")}, "resp_bound", "msg_bound")
	text := strings.Repeat("t", maxRequestBytes/2)
	reasoning := strings.Repeat("r", maxRequestBytes/2)
	if err := s.consume([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"` + text + `"}]}}]}}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.consume([]byte(`{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"` + reasoning + `"}]}}]}}`)); err != nil {
		t.Fatal(err)
	}
	if s.retainedBytes != maxRequestBytes {
		t.Fatalf("retained=%d want %d", s.retainedBytes, maxRequestBytes)
	}
	pending, outputs, index := len(s.pending), len(s.outputs), s.outputIndex
	if err := s.consume([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"x"}]}}]}}`)); !errors.Is(err, errSSEBounds) {
		t.Fatalf("overflow error=%v", err)
	}
	if len(s.pending) != pending || len(s.outputs) != outputs || s.outputIndex != index || s.retainedBytes != maxRequestBytes {
		t.Fatal("over-limit frame partially committed")
	}
}
