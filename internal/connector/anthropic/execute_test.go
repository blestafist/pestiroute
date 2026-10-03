package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

const executeBody = `{"model":"gpt-4.1-mini","stream":true,"input":"hello"}`

func executeConnector(t *testing.T) *Connector {
	t.Helper()
	c := NewConnector()
	if err := c.Init(context.Background(), config("gpt-4.1-mini", "account-a")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func TestExecuteRejectsReasoningBeforeTransport(t *testing.T) {
	c := executeConnector(t)
	_, gatewayErr := c.Execute(context.Background(), core.ExecutionRequest{
		Model: "gpt-4.1-mini",
		Payload: core.RawPayload{
			Protocol: protocol,
			Body:     []byte(`{"model":"gpt-4.1-mini","stream":true,"reasoning":{"effort":"high"},"input":"hello"}`),
		},
	}, core.AttemptScope{Mode: core.ModeTranslation, AccountID: "account-a"}, core.InvocationServices{})
	if gatewayErr == nil || gatewayErr.Category != core.CategoryInvalidRequest {
		t.Fatalf("Execute() error = %+v, want local invalid_request before credentials/transport", gatewayErr)
	}
}

func TestExecuteStreamTranslateEarlyHeadAndUsage(t *testing.T) {
	first := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"
	terminal := "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	gate := make(chan struct{})
	upstreamDone := make(chan struct{})
	pr, pw := io.Pipe()
	c := executeConnector(t)
	resp, gatewayErr := c.Execute(context.Background(), core.ExecutionRequest{Model: "gpt-4.1-mini", Payload: core.RawPayload{Protocol: protocol, Body: []byte(executeBody)}}, core.AttemptScope{Mode: core.ModeTranslation, AccountID: "account-a"}, core.InvocationServices{
		Credentials: credentialStub("secret"),
		Transport: doerFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("X-Api-Key") != "secret" {
				t.Errorf("credential missing")
			}
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body["model"] != backendModel {
				t.Errorf("translated request=%v err=%v", body, err)
			}
			go func() {
				_, _ = io.WriteString(pw, first)
				<-gate
				_, _ = io.WriteString(pw, terminal)
				_ = pw.Close()
				close(upstreamDone)
			}()
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: pr}, nil
		}),
	})
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	defer resp.Stream.Close()
	frame, err := resp.Stream.Next(context.Background())
	if err != nil || frame.Type != core.FrameHead || frame.Head.Protocol != protocol || frame.Head.HTTPStatus == nil || *frame.Head.HTTPStatus != 200 || frame.Head.ContentType != "text/event-stream" {
		t.Fatalf("Head=%+v err=%v", frame, err)
	}
	seen := []string{}
	for {
		frame, err = resp.Stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type != core.FrameBody {
			t.Fatalf("before terminal gate: frame=%+v", frame)
		}
		name := strings.SplitN(strings.TrimPrefix(string(frame.Body.Data), "event: "), "\n", 2)[0]
		if strings.Contains(string(frame.Body.Data), "anthropic") {
			t.Fatalf("provider event leaked: %s", frame.Body.Data)
		}
		if strings.HasPrefix(name, "response.") {
			seen = append(seen, name)
		}
		if name == "response.output_text.delta" {
			break
		}
	}
	select {
	case <-upstreamDone:
		t.Fatal("terminal upstream completed before early delta delivery")
	default:
	}
	if strings.Join(seen, ",") != "response.created,response.in_progress,response.output_item.added,response.content_part.added,response.output_text.delta" {
		t.Fatalf("event order=%v", seen)
	}
	close(gate)
	for {
		frame, err = resp.Stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameComplete {
			if frame.Complete.Outcome != core.OutcomeSucceeded || frame.Complete.Usage == nil || *frame.Complete.Usage.InputTokens != 12 || *frame.Complete.Usage.OutputTokens != 3 {
				t.Fatalf("Complete=%+v", frame.Complete)
			}
			break
		}
	}
	<-upstreamDone
	if _, err := resp.Stream.Next(context.Background()); err != io.EOF {
		t.Fatalf("terminal EOF=%v", err)
	}
}

func TestExecuteBackpressureReadsOnlyOnDemand(t *testing.T) {
	steps := []string{
		`event: message_start` + "\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n",
		`event: content_block_start` + "\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n",
		`event: content_block_delta` + "\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"first\"}}\n\n",
		`event: content_block_delta` + "\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"second\"}}\n\n",
		`event: content_block_stop` + "\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		`event: message_delta` + "\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n",
		`event: message_stop` + "\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	body := &steppedBody{steps: steps}
	stream := newMessagesStream(context.Background(), func() {}, body)
	defer stream.Close()
	if frame, err := stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead || body.reads != 0 {
		t.Fatalf("Head=%+v reads=%d err=%v", frame, body.reads, err)
	}
	for {
		frame, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameBody && strings.Contains(string(frame.Body.Data), `"delta":"first"`) {
			break
		}
	}
	if body.reads != 3 {
		t.Fatalf("upstream read %d events at first delta, want 3", body.reads)
	}
	deltas := "first"
	for {
		frame, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameBody {
			_, payload := decodeResponseFrame(t, frame.Body.Data)
			if delta, ok := payload["delta"].(string); ok {
				deltas += delta
			}
		}
		if frame.Type == core.FrameComplete {
			if frame.Complete.Outcome != core.OutcomeSucceeded {
				t.Fatalf("completion = %+v", frame.Complete)
			}
			break
		}
	}
	if deltas != "firstsecond" || body.reads != len(steps) {
		t.Fatalf("resumed stream deltas=%q reads=%d, want ordered deltas and %d reads", deltas, body.reads, len(steps))
	}
}

type steppedBody struct {
	steps []string
	reads int
}

func (b *steppedBody) Read(p []byte) (int, error) {
	if b.reads == len(b.steps) {
		return 0, io.EOF
	}
	step := b.steps[b.reads]
	b.reads++
	return copy(p, step), nil
}

func (*steppedBody) Close() error { return nil }

func TestExecuteToolStreamLifecycle(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`, `data: {"type":"message_start","message":{"usage":{"input_tokens":7,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`, ``,
		`event: content_block_start`, `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_weather","name":"weather"}}`, ``,
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Mün"}}`, ``,
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"chen 🌍\"}"}}`, ``,
		`event: content_block_stop`, `data: {"type":"content_block_stop","index":0}`, ``,
		`event: message_delta`, `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`, ``,
		`event: message_stop`, `data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"
	stream := newMessagesStream(context.Background(), func() {}, io.NopCloser(strings.NewReader(body)))
	defer stream.Close()
	if frame, err := stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
		t.Fatalf("Head=%+v err=%v", frame, err)
	}
	types := []string{}
	deltas := ""
	var itemID string
	for {
		frame, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameComplete {
			if frame.Complete.Outcome != core.OutcomeSucceeded || frame.Complete.Usage == nil || *frame.Complete.Usage.InputTokens != 7 || *frame.Complete.Usage.OutputTokens != 4 {
				t.Fatalf("Complete=%+v", frame.Complete)
			}
			break
		}
		name, payload := decodeResponseFrame(t, frame.Body.Data)
		types = append(types, name)
		switch name {
		case "response.output_item.added":
			item := payload["item"].(map[string]any)
			if item["type"] != "function_call" || item["call_id"] != "call_weather" || item["name"] != "weather" || item["id"] == stream.(*messagesStream).emitter.responseID {
				t.Fatalf("tool item=%#v", item)
			}
			itemID = item["id"].(string)
		case "response.function_call_arguments.delta":
			if payload["item_id"] != itemID {
				t.Fatalf("delta item=%#v", payload)
			}
			deltas += payload["delta"].(string)
		case "response.function_call_arguments.done":
			if payload["arguments"] != deltas {
				t.Fatalf("done=%#v deltas=%q", payload, deltas)
			}
		case "response.output_item.done":
			item := payload["item"].(map[string]any)
			if item["arguments"] != deltas || item["id"] != itemID {
				t.Fatalf("done item=%#v", item)
			}
		case "response.completed":
			response := payload["response"].(map[string]any)
			if response["status"] != "completed" || response["output"].([]any)[0].(map[string]any)["arguments"] != deltas {
				t.Fatalf("response=%#v", response)
			}
		}
	}
	if deltas != `{"city":"München 🌍"}` {
		t.Fatalf("arguments=%q", deltas)
	}
	want := "response.created,response.in_progress,response.output_item.added,response.function_call_arguments.delta,response.function_call_arguments.delta,response.function_call_arguments.done,response.output_item.done,response.completed"
	if strings.Join(types, ",") != want {
		t.Fatalf("events=%v", types)
	}
}

func TestExecuteTwoToolStreamAndParallelControls(t *testing.T) {
	streamBody := strings.Join([]string{
		`event: message_start`, `data: {"type":"message_start","message":{"usage":{"input_tokens":7}}}`, ``,
		`event: content_block_start`, `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_one","name":"one"}}`, ``,
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"n\":1}"}}`, ``,
		`event: content_block_stop`, `data: {"type":"content_block_stop","index":0}`, ``,
		`event: content_block_start`, `data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_two","name":"two"}}`, ``,
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"n\":2}"}}`, ``,
		`event: content_block_stop`, `data: {"type":"content_block_stop","index":1}`, ``,
		`event: message_delta`, `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`, ``,
		`event: message_stop`, `data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"
	for _, parallel := range []bool{false, true} {
		t.Run(map[bool]string{false: "parallel_false", true: "parallel_true"}[parallel], func(t *testing.T) {
			requestBody := `{"model":"gpt-4.1-mini","stream":true,"input":"hello","tools":[{"type":"function","name":"one","parameters":{"type":"object"}},{"type":"function","name":"two","parameters":{"type":"object"}}],"tool_choice":"auto","parallel_tool_calls":` + strconv.FormatBool(parallel) + `}`
			c := executeConnector(t)
			resp, gatewayErr := c.Execute(context.Background(), core.ExecutionRequest{Model: "gpt-4.1-mini", Payload: core.RawPayload{Protocol: protocol, Body: []byte(requestBody)}}, core.AttemptScope{Mode: core.ModeTranslation, AccountID: "account-a"}, core.InvocationServices{
				Credentials: credentialStub("secret"),
				Transport: doerFunc(func(req *http.Request) (*http.Response, error) {
					var wire struct {
						ToolChoice struct {
							Disable *bool `json:"disable_parallel_tool_use"`
						} `json:"tool_choice"`
					}
					if err := json.NewDecoder(req.Body).Decode(&wire); err != nil {
						t.Errorf("decode translated request: %v", err)
					} else if parallel && wire.ToolChoice.Disable != nil || !parallel && (wire.ToolChoice.Disable == nil || !*wire.ToolChoice.Disable) {
						t.Errorf("parallel=%t wire choice=%+v", parallel, wire.ToolChoice)
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(streamBody))}, nil
				}),
			})
			if gatewayErr != nil {
				t.Fatal(gatewayErr)
			}
			defer resp.Stream.Close()
			if frame, err := resp.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
				t.Fatalf("Head=%+v err=%v", frame, err)
			}
			var addedIDs []string
			var output []any
			var eventNames []string
			activeIndex := -1
			activeID := ""
			activeArgs := ""
			for {
				frame, err := resp.Stream.Next(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if frame.Type == core.FrameComplete {
					break
				}
				name, payload := decodeResponseFrame(t, frame.Body.Data)
				eventNames = append(eventNames, name)
				switch name {
				case "response.output_item.added":
					item := payload["item"].(map[string]any)
					addedIDs = append(addedIDs, item["id"].(string))
					if payload["output_index"] != float64(len(addedIDs)-1) || item["call_id"] != []string{"call_one", "call_two"}[len(addedIDs)-1] {
						t.Fatalf("added item=%#v", payload)
					}
					activeIndex, activeID = len(addedIDs)-1, addedIDs[len(addedIDs)-1]
					activeArgs = ""
				case "response.function_call_arguments.delta":
					if payload["output_index"] != float64(activeIndex) || payload["item_id"] != activeID {
						t.Fatalf("call event=%s payload=%#v", name, payload)
					}
					activeArgs += payload["delta"].(string)
				case "response.function_call_arguments.done":
					if payload["output_index"] != float64(activeIndex) || payload["item_id"] != activeID || payload["arguments"] != activeArgs {
						t.Fatalf("arguments closure=%#v delta=%q", payload, activeArgs)
					}
				case "response.output_item.done":
					item := payload["item"].(map[string]any)
					if payload["output_index"] != float64(activeIndex) || item["id"] != activeID || item["arguments"] != activeArgs {
						t.Fatalf("item closure=%#v", payload)
					}
				case "response.completed":
					response := payload["response"].(map[string]any)
					output = response["output"].([]any)
				}
			}
			if len(addedIDs) != 2 || addedIDs[0] == addedIDs[1] || !strings.HasPrefix(addedIDs[0], "fc_") || !strings.HasPrefix(addedIDs[1], "fc_") || len(output) != 2 {
				t.Fatalf("IDs=%v output=%#v", addedIDs, output)
			}
			for i, want := range []struct{ call, args string }{{"call_one", `{"n":1}`}, {"call_two", `{"n":2}`}} {
				item := output[i].(map[string]any)
				if item["id"] != addedIDs[i] || item["call_id"] != want.call || item["arguments"] != want.args || item["status"] != "completed" {
					t.Fatalf("snapshot item %d=%#v", i, item)
				}
			}
			if strings.Join(eventNames, ",") != "response.created,response.in_progress,response.output_item.added,response.function_call_arguments.delta,response.function_call_arguments.done,response.output_item.done,response.output_item.added,response.function_call_arguments.delta,response.function_call_arguments.done,response.output_item.done,response.completed" {
				t.Fatalf("event order=%v", eventNames)
			}
		})
	}
}

func TestToolStreamRejectsUnknownDeltaAndOverflow(t *testing.T) {
	e, _ := newResponsesEmitter()
	_, _ = e.StartResponse()
	if _, err := e.StartTool("call_x", "tool"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ToolDelta(strings.Repeat("x", maxRetainedText+1)); err == nil {
		t.Fatal("oversized tool arguments accepted")
	}
	if _, err := e.Finish("tool_use", new(int64(1)), new(int64(1)), nil); err == nil {
		t.Fatal("overflow emitted successful terminal")
	}
	stream := &messagesStream{started: true, block: true, tool: true, emitter: func() *responsesEmitter {
		x, _ := newResponsesEmitter()
		_, _ = x.StartResponse()
		_, _ = x.StartTool("call_x", "tool")
		return x
	}()}
	if err := stream.consume(messagesSSEEvent{typeName: "content_block_delta", data: []byte(`{"index":0,"delta":{"type":"unknown_delta","value":"x"}}`)}); err == nil {
		t.Fatal("unknown delta accepted")
	}
	for _, tc := range []struct {
		data  string
		block bool
	}{
		{data: `{"index":1,"content_block":{"type":"tool_use","id":"call_x","name":"tool"}}`},
		{data: `{"index":0,"content_block":{"type":"tool_use","id":"call_x","name":"tool"}}`, block: true},
	} {
		invalid := &messagesStream{started: true, emitter: func() *responsesEmitter { x, _ := newResponsesEmitter(); _, _ = x.StartResponse(); return x }()}
		invalid.block = tc.block
		if err := invalid.consume(messagesSSEEvent{typeName: "content_block_start", data: []byte(tc.data)}); err == nil {
			t.Fatalf("malformed or duplicate block identity accepted: %s", tc.data)
		}
	}
}

func TestReasoningProviderBlocksFailClosed(t *testing.T) {
	for _, block := range []string{"thinking", "redacted_thinking"} {
		t.Run(block, func(t *testing.T) {
			e, err := newResponsesEmitter()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.StartResponse(); err != nil {
				t.Fatal(err)
			}
			stream := &messagesStream{started: true, emitter: e}
			data := `{"index":0,"content_block":{"type":"` + block + `"}}`
			if err := stream.consume(messagesSSEEvent{typeName: "content_block_start", data: []byte(data)}); err == nil {
				t.Fatal("reasoning block was accepted")
			}
			if stream.block || len(stream.pending) != 0 {
				t.Fatalf("reasoning block changed output state: block=%t pending=%d", stream.block, len(stream.pending))
			}
		})
	}
}

func TestReasoningDeltasOnTextAndToolBlocksFailClosed(t *testing.T) {
	for _, block := range []string{"text", "tool_use"} {
		for _, delta := range []struct{ kind, field, marker string }{
			{"thinking_delta", "thinking", "thinking_private_marker"},
			{"signature_delta", "signature", "signature_private_marker"},
		} {
			t.Run(block+"/"+delta.kind, func(t *testing.T) {
				blockJSON := `{"type":"text"}`
				if block == "tool_use" {
					blockJSON = `{"type":"tool_use","id":"call_x","name":"lookup"}`
				}
				body := "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
					"event: content_block_start\ndata: {\"index\":0,\"content_block\":" + blockJSON + "}\n\n" +
					"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"" + delta.kind + "\",\"" + delta.field + "\":\"" + delta.marker + "\"}}\n\n"
				stream := newMessagesStream(context.Background(), func() {}, io.NopCloser(strings.NewReader(body)))
				defer stream.Close()
				if frame, err := stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
					t.Fatalf("Head=%+v err=%v", frame, err)
				}
				var output strings.Builder
				var terminals []string
				for {
					frame, err := stream.Next(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					if frame.Type == core.FrameBody {
						output.Write(frame.Body.Data)
						for _, name := range []string{"response.completed", "response.incomplete", "response.failed"} {
							if strings.Contains(string(frame.Body.Data), name) {
								terminals = append(terminals, name)
							}
						}
					}
					if frame.Type == core.FrameComplete {
						if frame.Complete.Outcome != core.OutcomeFailed || frame.Complete.Error == nil || frame.Complete.Error.Message != "Upstream response was invalid" || len(terminals) != 1 || terminals[0] != "response.failed" {
							t.Fatalf("Complete=%+v terminals=%v", frame.Complete, terminals)
						}
						break
					}
				}
				if strings.Contains(output.String(), delta.marker) || !strings.Contains(output.String(), "response.created") || strings.Contains(output.String(), "response.completed") {
					t.Fatalf("unexpected response bytes: %s", output.String())
				}
			})
		}
	}
}

func TestToolStreamRejectsDuplicateIDsAndOutOfOrderBlocks(t *testing.T) {
	e, _ := newResponsesEmitter()
	_, _ = e.StartResponse()
	_, _ = e.StartTool("call_same", "first")
	if _, err := e.FinishTool(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.StartTool("call_same", "second"); err == nil {
		t.Fatal("duplicate call ID accepted")
	}
	stream := &messagesStream{started: true, emitter: func() *responsesEmitter { x, _ := newResponsesEmitter(); _, _ = x.StartResponse(); return x }()}
	if err := stream.consume(messagesSSEEvent{typeName: "content_block_start", data: []byte(`{"index":0,"content_block":{"type":"tool_use","id":"call_1","name":"one"}}`)}); err != nil {
		t.Fatal(err)
	}
	if err := stream.consume(messagesSSEEvent{typeName: "content_block_stop", data: []byte(`{"index":0}`)}); err != nil {
		t.Fatal(err)
	}
	if err := stream.consume(messagesSSEEvent{typeName: "content_block_start", data: []byte(`{"index":2,"content_block":{"type":"tool_use","id":"call_2","name":"two"}}`)}); err == nil {
		t.Fatal("out-of-order block index accepted")
	}

	bounded, _ := newResponsesEmitter()
	_, _ = bounded.StartResponse()
	_, _ = bounded.StartTool("call_a", "a")
	if _, err := bounded.ToolDelta(strings.Repeat("x", maxRetainedText-1)); err != nil {
		t.Fatal(err)
	}
	if _, err := bounded.FinishTool(); err != nil {
		t.Fatal(err)
	}
	_, _ = bounded.StartTool("call_b", "b")
	if _, err := bounded.ToolDelta("xx"); err == nil {
		t.Fatal("aggregate arguments over 1 MiB accepted")
	}
}

func TestTranslateFunctionHistoryRejectsBeforeDispatchAndHTTPRejection(t *testing.T) {
	calls := 0
	services := core.InvocationServices{Credentials: credentialStub("secret"), Transport: doerFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, nil })}
	for _, body := range []string{
		`{"model":"gpt-4.1-mini","stream":true,"input":"hello","tools":[{"type":"function","name":"bad name","parameters":{"type":"object"}}]}`,
		`{"model":"gpt-4.1-mini","stream":true,"input":"hello","tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"missing"}}`,
		`{"model":"gpt-4.1-mini","stream":true,"input":[{"type":"function_call_output","id":"out","call_id":"missing","output":"x"}]}`,
		`{"model":"gpt-4.1-mini","stream":true,"input":[{"type":"function_call","id":"item","call_id":"call","name":"f","arguments":"{"}]}`,
		`{"model":"gpt-4.1-mini","stream":true,"input":[{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"message","role":"user","content":"interleaved"},{"type":"function_call_output","call_id":"c","output":"x"}]}`,
		`{"model":"gpt-4.1-mini","stream":true,"input":[{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"message","role":"user","content":"not a result"}]}`,
	} {
		request := core.ExecutionRequest{Model: "gpt-4.1-mini", Payload: core.RawPayload{Protocol: protocol, Body: []byte(body)}}
		if _, err := executeConnector(t).Execute(context.Background(), request, core.AttemptScope{Mode: core.ModeTranslation, AccountID: "account-a"}, services); err == nil || err.Category != core.CategoryInvalidRequest || calls != 0 {
			t.Fatalf("local rejection=%+v dispatches=%d", err, calls)
		}
	}
	c := executeConnector(t)
	closed := false
	services.Transport = doerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"3"}}, Body: closeReader{Reader: strings.NewReader(`private provider detail`), close: func() { closed = true }}}, nil
	})
	err := func() *core.GatewayError {
		_, e := c.Execute(context.Background(), core.ExecutionRequest{Model: "gpt-4.1-mini", Payload: core.RawPayload{Protocol: protocol, Body: []byte(executeBody)}}, core.AttemptScope{Mode: core.ModeTranslation, AccountID: "account-a"}, services)
		return e
	}()
	if err == nil || err.Category != core.CategoryRateLimited || err.RetryDisposition != core.RetryUnknown || err.RetryAfter == nil || *err.RetryAfter != 3*time.Second || strings.Contains(err.Message, "private") || calls != 1 || !closed {
		t.Fatalf("rejection=%+v calls=%d closed=%t", err, calls, closed)
	}
}

func TestExecuteEOFWithoutMessageStopIsError(t *testing.T) {
	c := executeConnector(t)
	resp, gatewayErr := c.Execute(context.Background(), core.ExecutionRequest{Model: "gpt-4.1-mini", Payload: core.RawPayload{Protocol: protocol, Body: []byte(executeBody)}}, core.AttemptScope{Mode: core.ModeTranslation, AccountID: "account-a"}, core.InvocationServices{
		Credentials: credentialStub("secret"),
		Transport: doerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}}\n\n"))}, nil
		}),
	})
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	defer resp.Stream.Close()
	if _, err := resp.Stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := resp.Stream.Next(context.Background()); err != nil {
			t.Fatalf("initial lifecycle frame %d: %v", i, err)
		}
	}
	var event string
	for {
		frame, err := resp.Stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameBody && strings.Contains(string(frame.Body.Data), "response.incomplete") {
			event = "response.incomplete"
		}
		if frame.Type == core.FrameComplete {
			if frame.Complete.Outcome != core.OutcomeIncomplete || frame.Complete.Error.Code != "incomplete_response" || frame.Complete.Usage == nil || frame.Complete.Usage.Completeness != core.UsagePartial || frame.Complete.Usage.InputTokens == nil || *frame.Complete.Usage.InputTokens != 1 || event == "" {
				t.Fatalf("EOF without message_stop returned %+v, event=%q", frame, event)
			}
			break
		}
	}
}

func TestExecuteStopReasonAndStreamErrorOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, stop string
		outcome    core.Outcome
		terminal   string
	}{
		{"end_turn", "end_turn", core.OutcomeSucceeded, "response.completed"},
		{"stop_sequence", "stop_sequence", core.OutcomeSucceeded, "response.completed"},
		{"tool_use", "tool_use", core.OutcomeSucceeded, "response.completed"},
		{"max_tokens", "max_tokens", core.OutcomeIncomplete, "response.incomplete"},
		{"unknown", "future_reason", core.OutcomeFailed, "response.failed"},
		{"refusal", "refusal", core.OutcomeFailed, "response.failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := `"type":"text"`
			if tc.stop == "tool_use" {
				block = `"type":"tool_use","id":"call_x","name":"tool"`
			}
			body := "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
				"event: content_block_start\ndata: {\"index\":0,\"content_block\":{" + block + "}}\n\n" +
				"event: content_block_stop\ndata: {\"index\":0}\n\n" +
				"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"" + tc.stop + "\"},\"usage\":{\"output_tokens\":2}}\n\n" +
				"event: message_stop\ndata: {}\n\n"
			stream := newMessagesStream(context.Background(), func() {}, io.NopCloser(strings.NewReader(body)))
			defer stream.Close()
			if _, err := stream.Next(context.Background()); err != nil {
				t.Fatal(err)
			}
			var terminals []string
			var incompleteReason string
			for {
				frame, err := stream.Next(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if frame.Type == core.FrameBody {
					for _, name := range []string{"response.completed", "response.incomplete", "response.failed"} {
						if strings.Contains(string(frame.Body.Data), name) {
							terminals = append(terminals, name)
							if name == "response.incomplete" {
								_, payload := decodeResponseFrame(t, frame.Body.Data)
								incompleteReason = payload["response"].(map[string]any)["incomplete_details"].(map[string]any)["reason"].(string)
							}
						}
					}
				}
				if frame.Type == core.FrameComplete {
					if frame.Complete.Outcome != tc.outcome || len(terminals) != 1 || terminals[0] != tc.terminal {
						t.Fatalf("Complete=%+v terminals=%v", frame.Complete, terminals)
					}
					if tc.outcome != core.OutcomeSucceeded && (frame.Complete.Error == nil || strings.Contains(frame.Complete.Error.Message, tc.stop)) {
						t.Fatalf("unsafe error: %+v", frame.Complete.Error)
					}
					if tc.stop == "max_tokens" && (frame.Complete.Error.Code != "output_truncated" || incompleteReason != "max_output_tokens") {
						t.Fatalf("truncation Complete=%+v incomplete reason=%q", frame.Complete, incompleteReason)
					}
					break
				}
			}
		})
	}
	t.Run("in-stream rate limit error", func(t *testing.T) {
		body := "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"private\"}}\n\n"
		stream := newMessagesStream(context.Background(), func() {}, io.NopCloser(strings.NewReader(body)))
		defer stream.Close()
		_, _ = stream.Next(context.Background())
		var terminal string
		for {
			frame, err := stream.Next(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if frame.Type == core.FrameBody && strings.Contains(string(frame.Body.Data), "response.failed") {
				terminal = "response.failed"
			}
			if frame.Type == core.FrameComplete {
				if frame.Complete.Outcome != core.OutcomeFailed || frame.Complete.Error.Category != core.CategoryRateLimited || frame.Complete.Error.Code != "upstream_rate_limited" || strings.Contains(frame.Complete.Error.Message, "private") || terminal == "" {
					t.Fatalf("Complete=%+v terminal=%q", frame.Complete, terminal)
				}
				break
			}
		}
	})
}

func TestExecuteTerminalIsExactlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		outcome    core.Outcome
	}{
		{
			name: "repeated stop delta is a single failure",
			body: "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":1,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}}\n\n" +
				"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
				"event: content_block_stop\ndata: {\"index\":0}\n\n" +
				"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
				"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
				"event: message_stop\ndata: {}\n\n",
			outcome: core.OutcomeFailed,
		},
		{
			name: "second stop and trailing error ignored after terminal",
			body: "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":1,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}}\n\n" +
				"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
				"event: content_block_stop\ndata: {\"index\":0}\n\n" +
				"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
				"event: message_stop\ndata: {}\n\n" +
				"event: message_stop\ndata: {}\n\n" +
				"event: error\ndata: {\"error\":{\"type\":\"api_error\"}}\n\n",
			outcome: core.OutcomeSucceeded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := newMessagesStream(context.Background(), func() {}, io.NopCloser(strings.NewReader(tc.body)))
			defer stream.Close()
			if _, err := stream.Next(context.Background()); err != nil {
				t.Fatal(err)
			}
			completeCount, terminalCount := 0, 0
			var outcome core.Outcome
			var input, output *int64
			for {
				frame, err := stream.Next(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if frame.Type == core.FrameBody && (strings.Contains(string(frame.Body.Data), "response.completed") || strings.Contains(string(frame.Body.Data), "response.failed")) {
					terminalCount++
				}
				if frame.Type == core.FrameComplete {
					completeCount++
					outcome = frame.Complete.Outcome
					input, output = frame.Complete.Usage.InputTokens, frame.Complete.Usage.OutputTokens
					if outcome != tc.outcome {
						t.Fatalf("outcome=%q want %q", outcome, tc.outcome)
					}
					break
				}
			}
			if completeCount != 1 || terminalCount != 1 || input == nil || *input != 1 || output == nil || *output != 2 {
				t.Fatalf("complete=%d terminal=%d outcome=%q usage=(%v,%v)", completeCount, terminalCount, outcome, input, output)
			}
			for i := 0; i < 2; i++ {
				if _, err := stream.Next(context.Background()); err != io.EOF {
					t.Fatalf("post-terminal Next %d: %v", i, err)
				}
			}
			if err := stream.Close(); err != nil {
				t.Fatal(err)
			}
			if err := stream.Close(); err != nil {
				t.Fatal(err)
			}
			if completeCount != 1 || outcome != tc.outcome || *input != 1 || *output != 2 {
				t.Fatalf("post-terminal state changed: complete=%d outcome=%q usage=(%d,%d)", completeCount, outcome, *input, *output)
			}
		})
	}
}

func TestExecuteProviderErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, providerType, code string
		category                         core.ErrorCategory
	}{
		{"before message_start", "", "api_error", "upstream_api_error", core.CategoryUnavailable},
		{"overloaded", "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n", "overloaded_error", "upstream_overloaded", core.CategoryUnavailable},
		{"api", "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n", "api_error", "upstream_api_error", core.CategoryUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.prefix + "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"" + tc.providerType + "\",\"message\":\"private provider detail\"}}\n\n"
			stream := newMessagesStream(context.Background(), func() {}, io.NopCloser(strings.NewReader(body)))
			defer stream.Close()
			if _, err := stream.Next(context.Background()); err != nil {
				t.Fatal(err)
			}
			var terminal string
			for {
				frame, err := stream.Next(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if frame.Type == core.FrameBody && strings.Contains(string(frame.Body.Data), "response.failed") {
					terminal = string(frame.Body.Data)
				}
				if frame.Type == core.FrameComplete {
					if frame.Complete.Outcome != core.OutcomeFailed || frame.Complete.Error.Code != tc.code || frame.Complete.Error.Category != tc.category || strings.Contains(frame.Complete.Error.Message, "private") {
						t.Fatalf("Complete=%+v", frame.Complete)
					}
					if tc.prefix != "" && (terminal == "" || strings.Contains(terminal, "private provider detail")) {
						t.Fatalf("unsafe/missing terminal: %q", terminal)
					}
					if tc.prefix == "" && terminal != "" {
						t.Fatalf("terminal emitted before response start: %q", terminal)
					}
					break
				}
			}
			if _, err := stream.Next(context.Background()); err != io.EOF {
				t.Fatalf("post-failure Next: %v", err)
			}
		})
	}
}

func TestExecuteUsageCacheCumulativeAndMissing(t *testing.T) {
	for _, tc := range []struct {
		name, start, deltas   string
		input, output, cached *int64
	}{
		{name: "cache and cumulative", start: `{"input_tokens":10,"cache_read_input_tokens":2,"cache_creation_input_tokens":3}`, deltas: `{"output_tokens":7}`, input: new(int64(15)), output: new(int64(7)), cached: new(int64(2))},
		{name: "zero and missing", start: `{"input_tokens":0,"cache_read_input_tokens":0}`, deltas: `{"output_tokens":0}`, output: new(int64(0)), cached: new(int64(0))},
		{name: "all missing", start: `{}`, deltas: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &messagesStream{body: io.NopCloser(strings.NewReader("")), cancel: func() {}, phase: 1}
			for _, event := range []messagesSSEEvent{
				{typeName: "message_start", data: []byte(`{"message":{"usage":` + tc.start + `}}`)},
				{typeName: "content_block_start", data: []byte(`{"index":0,"content_block":{"type":"text"}}`)},
				{typeName: "content_block_stop", data: []byte(`{"index":0}`)},
				{typeName: "message_delta", data: []byte(`{"delta":{"stop_reason":null},"usage":{"output_tokens":4}}`)},
				{typeName: "message_delta", data: []byte(`{"delta":{"stop_reason":"end_turn"},"usage":` + tc.deltas + `}`)},
			} {
				if err := stream.consume(event); err != nil {
					t.Fatal(err)
				}
			}
			if err := stream.consume(messagesSSEEvent{typeName: "message_stop", data: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
			usage := stream.terminal.Complete.Usage
			check := func(name string, got, want *int64) {
				t.Helper()
				if got == nil != (want == nil) || got != nil && *got != *want {
					t.Fatalf("%s = %v, want %v", name, got, want)
				}
			}
			check("input", usage.InputTokens, tc.input)
			check("output", usage.OutputTokens, tc.output)
			check("cached", usage.CachedTokens, tc.cached)
			if usage.Completeness != core.UsageComplete {
				t.Fatalf("usage completeness = %q", usage.Completeness)
			}
			wantSource := core.UsageProvider
			if tc.input == nil && tc.output == nil && tc.cached == nil {
				wantSource = core.UsageUnknown
			}
			if usage.Source != wantSource {
				t.Fatalf("usage source = %q, want %q", usage.Source, wantSource)
			}
			var response map[string]any
			for _, frame := range stream.pending {
				name, payload := decodeResponseFrame(t, frame)
				if name == "response.completed" {
					response = payload["response"].(map[string]any)
				}
			}
			wireUsage := response["usage"].(map[string]any)
			if tc.input == nil {
				if _, exists := wireUsage["input_tokens"]; exists {
					t.Fatalf("unknown input serialized: %#v", wireUsage)
				}
			} else if wireUsage["input_tokens"] != float64(*tc.input) {
				t.Fatalf("response usage = %#v", wireUsage)
			}
			if tc.output != nil && wireUsage["output_tokens"] != float64(*tc.output) {
				t.Fatalf("response output usage = %#v", wireUsage)
			}
			if tc.input != nil && tc.output != nil && wireUsage["total_tokens"] != float64(*tc.input+*tc.output) {
				t.Fatalf("response total usage = %#v", wireUsage)
			}
			if tc.cached != nil && wireUsage["input_tokens_details"].(map[string]any)["cached_tokens"] != float64(*tc.cached) {
				t.Fatalf("cache details = %#v", wireUsage)
			}
		})
	}
}

func TestExecuteUsageRejectsNegativeAndOverflow(t *testing.T) {
	for _, usage := range []string{
		`{"input_tokens":-1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`,
		`{"input_tokens":9223372036854775807,"cache_read_input_tokens":1,"cache_creation_input_tokens":0}`,
	} {
		stream := &messagesStream{}
		if err := stream.consume(messagesSSEEvent{typeName: "message_start", data: []byte(`{"message":{"usage":` + usage + `}}`)}); err == nil {
			t.Fatalf("invalid usage accepted: %s", usage)
		}
	}
}

func TestExecuteCloseInterruptsBlockedRead(t *testing.T) {
	c := executeConnector(t)
	reader, writer := io.Pipe()
	started := make(chan struct{})
	resp, gatewayErr := c.Execute(context.Background(), core.ExecutionRequest{Model: "gpt-4.1-mini", Payload: core.RawPayload{Protocol: protocol, Body: []byte(executeBody)}}, core.AttemptScope{Mode: core.ModeTranslation, AccountID: "account-a"}, core.InvocationServices{
		Credentials: credentialStub("secret"),
		Transport: doerFunc(func(*http.Request) (*http.Response, error) {
			go func() { _, _ = io.WriteString(writer, "data: {\"type\":\"ping\"}"); close(started) }()
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: reader}, nil
		}),
	})
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	if _, err := resp.Stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() { _, err := resp.Stream.Next(context.Background()); readDone <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not reach blocked frame")
	}
	if err := resp.Stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("blocked Next returned without error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not interrupt blocked upstream read")
	}
	_ = writer.Close()
}

func TestExecuteTimeoutInterruptsBlockedRead(t *testing.T) {
	c := executeConnector(t)
	reader, writer := io.Pipe()
	readStarted := make(chan struct{})
	resp, gatewayErr := c.Execute(context.Background(), core.ExecutionRequest{Model: "gpt-4.1-mini", Payload: core.RawPayload{Protocol: protocol, Body: []byte(executeBody)}}, core.AttemptScope{Mode: core.ModeTranslation, AccountID: "account-a"}, core.InvocationServices{
		Credentials: credentialStub("secret"),
		Transport: doerFunc(func(*http.Request) (*http.Response, error) {
			go func() { _, _ = io.WriteString(writer, "data: {\"type\":\"ping\"}\n\n"); close(readStarted) }()
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: reader}, nil
		}),
	})
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	if _, err := resp.Stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := resp.Stream.Next(ctx); done <- err }()
	select {
	case <-readStarted:
	case <-time.After(time.Second):
		t.Fatal("upstream read did not start")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timed out Next error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("deadline did not interrupt upstream read")
	}
	if _, err := resp.Stream.Next(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next after timeout = %v, want canceled", err)
	}
	_ = writer.Close()
}

func TestExecuteCloseWinsBlockedTerminalRace(t *testing.T) {
	for range 50 {
		ctx, cancel := context.WithCancel(context.Background())
		reader := &terminalRaceReader{started: make(chan struct{}), closed: make(chan struct{})}
		emitter, err := newResponsesEmitter()
		if err != nil {
			t.Fatal(err)
		}
		stream := &messagesStream{ctx: ctx, cancel: cancel, body: reader, reader: newMessagesSSEReader(reader), phase: 1, started: true, nextIndex: 1, stop: "end_turn", emitter: emitter}
		done := make(chan error, 1)
		go func() {
			frame, err := stream.Next(context.Background())
			if err == nil && frame.Type == core.FrameComplete && frame.Complete.Outcome == core.OutcomeSucceeded {
				done <- errors.New("close raced to a late success")
			} else {
				done <- err
			}
		}()
		select {
		case <-reader.started:
		case <-time.After(time.Second):
			t.Fatal("upstream read did not start")
		}
		if err := stream.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("blocked Next returned no cancellation error")
			}
		case <-time.After(time.Second):
			t.Fatal("Close did not interrupt terminal race")
		}
	}
}

type terminalRaceReader struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (r *terminalRaceReader) Read(p []byte) (int, error) {
	select {
	case <-r.started:
	default:
		close(r.started)
	}
	<-r.closed
	return copy(p, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"), io.EOF
}

func (r *terminalRaceReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

type closeReader struct {
	io.Reader
	close func()
}

func (r closeReader) Close() error { r.close(); return nil }
