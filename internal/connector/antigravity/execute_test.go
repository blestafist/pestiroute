package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	responsesAdapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/core"
)

type fakeDoer struct {
	response *http.Response
	err      error
	calls    int
	request  *http.Request
	do       func(*http.Request) (*http.Response, error)
}
type ambientDoer struct {
	calls   int
	request *http.Request
}

func (f *ambientDoer) Do(request *http.Request) (*http.Response, error) {
	f.calls++
	f.request = request
	return nil, errors.New("must not dispatch")
}

func (f *fakeDoer) antigravityOfflineFakeOnly() {}
func (f *fakeDoer) Do(r *http.Request) (*http.Response, error) {
	f.calls++
	f.request = r
	if f.do != nil {
		return f.do(r)
	}
	return f.response, f.err
}

type credentials struct {
	data []byte
	err  error
}
type trackingBody struct {
	io.Reader
	read   int
	closed bool
}

func (b *trackingBody) Read(p []byte) (int, error) {
	n, e := b.Reader.Read(p)
	b.read += n
	return n, e
}
func (b *trackingBody) Close() error { b.closed = true; return nil }

type cancelBody struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
	sent    bool
}

func (b *cancelBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, `data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]}}]}}`+"\n\n"), nil
	}
	b.once.Do(func() { close(b.started) })
	<-b.closed
	return 0, errors.New("closed")
}
func (b *cancelBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func (c credentials) Get(context.Context, string) ([]byte, error) { return c.data, c.err }
func ready(t *testing.T) *Connector {
	t.Helper()
	c := NewConnector()
	if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"profile":"antigravity-v1internal","project":"synthetic-project","endpoint":"https://antigravity.synthetic.invalid","oauth":{"client_secret":"SYNTHETIC_CLIENT_SECRET"}}`)}); err != nil {
		t.Fatal(err)
	}
	return c
}
func request(body string) core.ExecutionRequest {
	return core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol, ContentType: "application/json", Body: []byte(body)}}
}
func services(fake *fakeDoer) core.InvocationServices {
	return core.InvocationServices{Transport: packageTestDoer(fake), Credentials: credentials{data: []byte(`{"access_token":"synthetic-token"}`)}}
}
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestExecuteStreamsIncrementallyAndBuildsCandidateRequest(t *testing.T) {
	data, err := os.ReadFile("testdata/stream-text.sse")
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's final text chunk is intentionally not terminal; append the synthetic terminal fixture.
	usage, err := os.ReadFile("testdata/stream-usage.sse")
	if err != nil {
		t.Fatal(err)
	}
	frames := string(data) + "\n" + strings.TrimLeft(strings.SplitN(string(usage), "\n\n", 2)[1], "\n") + "\n"
	f := &fakeDoer{response: response(200, frames)}
	c := ready(t)
	out, ge := c.Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"Synthetic prompt","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, services(f))
	if ge != nil {
		t.Fatal(ge)
	}
	if f.calls != 1 || f.request.URL.String() != "https://antigravity.synthetic.invalid/v1internal/projects/synthetic-project:generateContent?alt=sse" || f.request.Header.Get("Authorization") != "Bearer synthetic-token" {
		t.Fatalf("bad dispatch: calls=%d request=%v", f.calls, f.request)
	}
	reqBody, err := io.ReadAll(f.request.Body)
	if err != nil {
		t.Fatal(err)
	}
	var candidate map[string]any
	if err := json.Unmarshal(reqBody, &candidate); err != nil || candidate["project"] != "synthetic-project" || candidate["requestType"] != "agent" {
		t.Fatalf("bad candidate envelope %s (%v)", reqBody, err)
	}
	s := out.Stream
	defer s.Close()
	frame, err := s.Next(context.Background())
	if err != nil || frame.Type != core.FrameHead {
		t.Fatalf("head: %#v %v", frame, err)
	}
	var body strings.Builder
	for {
		frame, err = s.Next(context.Background())
		if err != nil || frame.Type != core.FrameBody {
			t.Fatalf("expected streamed body: %#v %v", frame, err)
		}
		body.Write(frame.Body.Data)
		if strings.Contains(string(frame.Body.Data), "Synthetic ") {
			break
		}
	}
	for {
		frame, err = s.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameComplete {
			if frame.Complete.Usage == nil || *frame.Complete.Usage.InputTokens != 7 {
				t.Fatalf("missing terminal usage: %+v", frame.Complete.Usage)
			}
			break
		}
		body.Write(frame.Body.Data)
	}
	if !strings.Contains(body.String(), "stream text.") || !strings.Contains(body.String(), "response.completed") {
		t.Fatalf("translated output incomplete: %s", body.String())
	}
}

func TestSeparateExecutionsUseDistinctResponseAndMessageIDs(t *testing.T) {
	c := ready(t)
	type streamIDs struct{ response, message string }
	readIDs := func() streamIDs {
		t.Helper()
		wire := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"x\"}]}}]}}\n\ndata: {\"response\":{\"done\":true}}\n\n"
		f := &fakeDoer{response: response(http.StatusOK, wire)}
		out, ge := c.Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, services(f))
		if ge != nil {
			t.Fatal(ge)
		}
		defer out.Stream.Close()
		var responseID, messageID, completedResponseID, completedMessageID string
		for {
			frame, err := out.Stream.Next(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if frame.Type == core.FrameBody {
				for _, line := range strings.Split(string(frame.Body.Data), "\n") {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					var event struct {
						Type     string `json:"type"`
						ItemID   string `json:"item_id"`
						Response struct {
							ID     string `json:"id"`
							Output []struct {
								ID string `json:"id"`
							} `json:"output"`
						} `json:"response"`
					}
					if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
						t.Fatal(err)
					}
					switch event.Type {
					case "response.created":
						responseID = event.Response.ID
					case "response.output_text.delta":
						messageID = event.ItemID
					case "response.completed":
						completedResponseID = event.Response.ID
						if len(event.Response.Output) > 0 {
							completedMessageID = event.Response.Output[0].ID
						}
					}
				}
			}
			if frame.Type == core.FrameComplete {
				break
			}
		}
		if responseID == "" || responseID != completedResponseID || messageID == "" || messageID != completedMessageID {
			t.Fatalf("response/message IDs changed within execution: created=%q/%q completed=%q/%q", responseID, messageID, completedResponseID, completedMessageID)
		}
		return streamIDs{response: responseID, message: messageID}
	}
	idsA, idsB := readIDs(), readIDs()
	if idsA.response == idsB.response || idsA.message == idsB.message {
		t.Fatalf("execution IDs were not independently generated: %+v %+v", idsA, idsB)
	}
}

func TestInferenceEndpointDefaultsAndOverride(t *testing.T) {
	for _, tc := range []struct{ name, config, env, want string }{
		{"production default", `{"profile":"antigravity-v1internal","project":"synthetic-project"}`, "", "https://cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse"},
		{"environment override", `{"profile":"antigravity-v1internal","project":"synthetic-project"}`, "https://api.example.invalid", "https://api.example.invalid/v1internal:streamGenerateContent?alt=sse"},
		{"environment overrides configured endpoint", `{"profile":"antigravity-v1internal","project":"synthetic-project","endpoint":"https://configured.example.invalid"}`, "https://api.example.invalid", "https://api.example.invalid/v1internal:streamGenerateContent?alt=sse"},
		{"explicit endpoint", `{"profile":"antigravity-v1internal","project":"synthetic-project","endpoint":"https://api.example.invalid"}`, "", "https://api.example.invalid/v1internal:streamGenerateContent?alt=sse"},
		{"synthetic compatibility", `{"profile":"antigravity-v1internal","project":"synthetic-project","endpoint":"https://antigravity.synthetic.invalid"}`, "", "https://antigravity.synthetic.invalid/v1internal/projects/synthetic-project:generateContent?alt=sse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PESTIROUTE_ANTIGRAVITY_ENDPOINT", tc.env)
			c := NewConnector()
			if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(tc.config)}); err != nil {
				t.Fatal(err)
			}
			f := &fakeDoer{response: response(http.StatusOK, "data: {\"response\":{\"done\":true}}\n\n")}
			out, ge := c.Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, services(f))
			if ge != nil {
				t.Fatal(ge)
			}
			_ = out.Stream.Close()
			if f.request == nil || f.request.URL.String() != tc.want {
				t.Fatalf("request URL=%v want %q", f.request, tc.want)
			}
		})
	}
}

func TestInferenceEndpointRejectsUnsafeURL(t *testing.T) {
	for _, endpoint := range []string{"http://api.example.invalid", "https://user@api.example.invalid", "https://api.example.invalid/path", "https://api.example.invalid?query=1", "https://api.example.invalid/#fragment"} {
		c := NewConnector()
		config := `{"profile":"antigravity-v1internal","project":"synthetic-project","endpoint":"` + endpoint + `"}`
		if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(config)}); err == nil {
			t.Errorf("unsafe endpoint accepted: %q", endpoint)
		}
	}
}

func TestRealInferenceTransportRequiresExplicitOptIn(t *testing.T) {
	req := request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`)
	doer := &ambientDoer{}
	t.Setenv("PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT", "")
	c := NewConnector()
	if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"profile":"antigravity-v1internal","project":"synthetic-project"}`)}); err != nil {
		t.Fatal(err)
	}
	services := services(nil)
	services.Transport = doer
	if _, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: core.ModeTranslation}, services); ge == nil || doer.calls != 0 {
		t.Fatalf("disabled real transport dispatched: error=%v calls=%d", ge, doer.calls)
	}
	t.Setenv("PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT", "1")
	if _, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: core.ModeTranslation}, services); ge == nil || doer.calls != 0 {
		t.Fatalf("arbitrary opted-in transport was not rejected: error=%v calls=%d", ge, doer.calls)
	}
	c.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusOK, "data: {\"response\":{\"done\":true}}\n\n"), nil
	})
	services.Transport = c.HTTPDoer()
	out, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: core.ModeTranslation}, services)
	if ge != nil {
		t.Fatalf("opted-in connector HTTP client rejected: %v", ge)
	}
	_ = out.Stream.Close()
	if doer.calls != 0 {
		t.Fatalf("arbitrary opted-in doer dispatched %d times", doer.calls)
	}
	out, ge = c.Execute(context.Background(), req, core.AttemptScope{Mode: core.ModeTranslation}, services)
	if ge != nil {
		t.Fatalf("opted-in connector HTTP client rejected: %v", ge)
	}
	_ = out.Stream.Close()
	redirectCalls := 0
	c.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		redirectCalls++
		resp := response(http.StatusTemporaryRedirect, "")
		resp.Header.Set("Location", "https://redirect.example.invalid/")
		return resp, nil
	})
	if _, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: core.ModeTranslation}, services); ge == nil || redirectCalls != 1 {
		t.Fatalf("redirect was followed or accepted: error=%v calls=%d", ge, redirectCalls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCapabilitiesOnlyPromoteVerifiedTranslationStreaming(t *testing.T) {
	c := ready(t)
	for _, tc := range []struct {
		protocol string
		mode     string
		want     core.CapabilityState
	}{
		{protocol, core.ModeTranslation, core.Supported},
		{protocol, core.ModeNative, core.Unknown},
		{"other.protocol", core.ModeTranslation, core.Unknown},
	} {
		result := c.Capabilities(context.Background(), core.CapabilityScope{Protocol: tc.protocol, Mode: tc.mode})
		if got := result.Values["llm.streaming"]; (tc.want == core.Supported && got != core.Supported) || (tc.want != core.Supported && got == core.Supported) {
			t.Errorf("scope %+v streaming=%v want supported=%v", tc, got, tc.want == core.Supported)
		}
		for _, cap := range []core.Capability{"auth.oauth", "usage.exact"} {
			if result.Values[cap] == core.Supported {
				t.Errorf("scope %+v %s=%v want unknown", tc, cap, result.Values[cap])
			}
		}
	}
}

func TestStreamTerminalEvidenceAndReasoningAdmission(t *testing.T) {
	stop := "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"parts\":[{\"text\":\"answer\"}]}}]}}\n\n"
	for _, tc := range []struct {
		name string
		wire string
		ok   bool
	}{
		{"candidate STOP plus clean EOF", stop, true},
		{"trailing usage after STOP", stop + "data: {\"response\":{\"usageMetadata\":{\"promptTokenCount\":1}}}\n\n", true},
		{"candidate after STOP", stop + "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"late\"}]}}]}}\n\n", false},
		{"conflicting finish after STOP", stop + "data: {\"response\":{\"candidates\":[{\"finishReason\":\"MAX_TOKENS\"}]}}\n\n", false},
		{"content after trailing usage", stop + "data: {\"response\":{\"usageMetadata\":{\"promptTokenCount\":1}}}\n\ndata: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"late\"}]}}]}}\n\n", false},
		{"synthetic done", "data: {\"response\":{\"done\":true}}\n\n", true},
		{"usage alone", "data: {\"response\":{\"usageMetadata\":{\"promptTokenCount\":1}}}\n\n", false},
		{"truncated after STOP", stop + "data: {\"response\":", false},
		{"thought", "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hidden\",\"thought\":true}]}}]}}\n\n", false},
		{"signature", "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hidden\",\"thoughtSignature\":\"sig\"}]}}]}}\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStream(io.NopCloser(strings.NewReader(tc.wire)), "resp_test", "msg_test")
			defer s.Close()
			completed := false
			body := strings.Builder{}
			for {
				frame, err := s.Next(context.Background())
				if err != nil {
					if tc.ok {
						t.Fatalf("unexpected stream error: %v", err)
					}
					break
				}
				if frame.Type == core.FrameBody {
					body.Write(frame.Body.Data)
				}
				if frame.Type == core.FrameComplete {
					completed = true
					break
				}
			}
			if completed != tc.ok {
				t.Fatalf("completed=%v want %v", completed, tc.ok)
			}
			if strings.Contains(body.String(), "hidden") || strings.Contains(body.String(), "late") {
				t.Fatalf("rejected content leaked: %s", body.String())
			}
		})
	}
}

func TestFunctionToolDeclarationMappingAndFailClosedOptions(t *testing.T) {
	const accepted = `{"model":"gemini-2.5-pro","input":"x","stream":true,"tools":[{"type":"function","name":"lookup","description":"Find a value","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}],"tool_choice":{"type":"function","name":"lookup"}}`
	f := &fakeDoer{response: response(http.StatusOK, "data: {\"response\":{\"done\":true}}\n\n")}
	out, ge := ready(t).Execute(context.Background(), request(accepted), core.AttemptScope{Mode: core.ModeTranslation}, services(f))
	if ge != nil {
		t.Fatal(ge)
	}
	_ = out.Stream.Close()
	body, err := io.ReadAll(f.request.Body)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Request struct {
			Tools []struct {
				Functions []struct {
					Name        string         `json:"name"`
					Description string         `json:"description"`
					Parameters  map[string]any `json:"parametersJsonSchema"`
				} `json:"functionDeclarations"`
			} `json:"tools"`
			ToolConfig struct {
				Calling struct {
					Mode    string   `json:"mode"`
					Allowed []string `json:"allowedFunctionNames"`
				} `json:"functionCallingConfig"`
			} `json:"toolConfig"`
		} `json:"request"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Request.Tools) != 1 || len(envelope.Request.Tools[0].Functions) != 1 || envelope.Request.Tools[0].Functions[0].Name != "lookup" || envelope.Request.Tools[0].Functions[0].Description != "Find a value" || envelope.Request.ToolConfig.Calling.Mode != "ANY" || len(envelope.Request.ToolConfig.Calling.Allowed) != 1 || envelope.Request.ToolConfig.Calling.Allowed[0] != "lookup" {
		t.Fatalf("unexpected mapped function declaration %s (%v)", body, err)
	}

	f = &fakeDoer{}
	for _, body := range []string{
		`{"model":"gemini-2.5-pro","input":"x","stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","required":1}}]}`,
		`{"model":"gemini-2.5-pro","input":"x","stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","required":null}}]}`,
		`{"model":"gemini-2.5-pro","input":"x","stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","description":null}}]}`,
		`{"model":"gemini-2.5-pro","input":"x","stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","title":null}}]}`,
		`{"model":"gemini-2.5-pro","input":"x","stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"value":{"type":"string","pattern":null}}}}]}`,
		`{"model":"gemini-2.5-pro","input":"x","stream":true,"tools":[{"type":"function","name":"lookup","description":null,"parameters":{"type":"object"}}]}`,
		`{"model":"gemini-2.5-pro","input":"x","stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"nested":{"type":"object","patternProperties":{"x":{"type":"string"}}}}}}]}`,
		`{"model":"gemini-2.5-pro","input":"x","stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"nested":{"type":"array","items":{"type":"string","maxLength":"many"}}}}}]}`,
		`{"model":"gemini-2.5-pro","input":"x","stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"nested":{"type":"string","minLength":8,"maxLength":2}}}}]}`,
		`{"model":"gemini-2.5-pro","input":"x","stream":true,"tools":[{"type":"computer","name":"lookup","parameters":{"type":"object"}}]}`,
		`{"model":"gemini-2.5-pro","input":"x","stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":"file_search"}`,
		`{"model":"gemini-2.5-pro","input":"x","stream":true,"tool_choice":"required"}`,
	} {
		_, ge := ready(t).Execute(context.Background(), request(body), core.AttemptScope{Mode: core.ModeTranslation}, services(f))
		if ge == nil {
			t.Errorf("unsupported/invalid tool request admitted: %s", body)
		}
	}
	if f.calls != 0 {
		t.Fatalf("invalid tool requests dispatched %d times", f.calls)
	}
}

func TestFunctionToolChoiceModes(t *testing.T) {
	for _, tc := range []struct {
		choice string
		mode   string
		want   bool
	}{
		{`"auto"`, "", false},
		{`"none"`, "NONE", true},
		{`"required"`, "ANY", true},
		{`{"type":"function","name":"lookup"}`, "ANY", true},
	} {
		config, err := functionToolChoice(json.RawMessage(tc.choice), map[string]bool{"lookup": true})
		if err != nil {
			t.Fatalf("choice %s: %v", tc.choice, err)
		}
		if !tc.want {
			if config != nil {
				t.Errorf("automatic choice emitted config: %+v", config)
			}
			continue
		}
		calling := config["functionCallingConfig"].(map[string]any)
		if calling["mode"] != tc.mode {
			t.Errorf("choice %s config=%+v", tc.choice, config)
		}
	}
}

func TestFunctionCallsStreamIncrementallyWithTextAndParallelIndices(t *testing.T) {
	wire, err := os.ReadFile("testdata/stream-function-calls.sse")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDoer{response: response(http.StatusOK, string(wire))}
	out, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, services(f))
	if ge != nil {
		t.Fatal(ge)
	}
	defer out.Stream.Close()
	types := []string{}
	indices := map[string][]int{}
	var completed map[string]any
	for {
		frame, err := out.Stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameComplete {
			break
		}
		if frame.Type != core.FrameBody {
			continue
		}
		for _, line := range strings.Split(string(frame.Body.Data), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatal(err)
			}
			kind := event["type"].(string)
			types = append(types, kind)
			if index, ok := event["output_index"].(float64); ok {
				indices[kind] = append(indices[kind], int(index))
			}
			if kind == "response.completed" {
				completed = event["response"].(map[string]any)
			}
		}
	}
	wantTypes := []string{"response.created", "response.output_item.added", "response.output_text.delta", "response.output_item.added", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.output_item.done", "response.output_text.delta", "response.output_item.added", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.output_item.done", "response.output_text.delta", "response.output_text.done", "response.output_item.done", "response.completed"}
	if strings.Join(types, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("event order: %v", types)
	}
	callIndices := indices["response.function_call_arguments.delta"]
	if len(callIndices) != 2 || callIndices[0] != 1 || callIndices[1] != 2 {
		t.Fatalf("call output indices: %v", indices)
	}
	output := completed["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("completed output: %#v", output)
	}
	message := output[0].(map[string]any)
	text := message["content"].([]any)[0].(map[string]any)["text"]
	if text != "before between after" || message["status"] != "completed" {
		t.Fatalf("interleaved text output: %#v", message)
	}
	itemIDs := map[string]bool{}
	for i, id := range []string{"candidate-call-a", "candidate-call-b"} {
		call := output[i+1].(map[string]any)
		if call["type"] != "function_call" || call["call_id"] != id || call["status"] != "completed" {
			t.Fatalf("call %d: %#v", i, call)
		}
		itemID := call["id"].(string)
		if itemID == "" || itemIDs[itemID] {
			t.Fatalf("duplicate or missing call item ID: %q", itemID)
		}
		itemIDs[itemID] = true
		if !strings.Contains(call["arguments"].(string), []string{"alpha", "beta"}[i]) {
			t.Fatalf("call arguments: %#v", call)
		}
	}
	if completed["usage"].(map[string]any)["input_tokens"] != float64(5) {
		t.Fatalf("usage missing: %#v", completed["usage"])
	}
}

func TestFunctionCallWithoutCandidateIDGetsScopedID(t *testing.T) {
	wire := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{}}}]}}]}}\n\ndata: {\"response\":{\"done\":true}}\n\n"
	f := &fakeDoer{response: response(http.StatusOK, wire)}
	out, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, services(f))
	if ge != nil {
		t.Fatal(ge)
	}
	defer out.Stream.Close()
	for {
		frame, err := out.Stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type != core.FrameBody || !strings.Contains(string(frame.Body.Data), `"type":"response.completed"`) {
			continue
		}
		var event struct {
			Response struct {
				Output []struct {
					CallID string `json:"call_id"`
				} `json:"output"`
			} `json:"response"`
		}
		line := strings.TrimPrefix(strings.Split(string(frame.Body.Data), "\n")[1], "data: ")
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if len(event.Response.Output) != 1 || !strings.HasPrefix(event.Response.Output[0].CallID, "call_resp_") {
			t.Fatalf("missing response-scoped call ID: %+v", event.Response.Output)
		}
		return
	}
}

func TestDoneOnlyStreamCompletesWithEmptyOutputArray(t *testing.T) {
	f := &fakeDoer{response: response(http.StatusOK, "data: {\"response\":{\"done\":true}}\n\n")}
	out, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, services(f))
	if ge != nil {
		t.Fatal(ge)
	}
	defer out.Stream.Close()
	for {
		frame, err := out.Stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type != core.FrameBody || !strings.Contains(string(frame.Body.Data), `"type":"response.completed"`) {
			if frame.Type == core.FrameComplete {
				t.Fatal("missing response.completed event")
			}
			continue
		}
		var event struct {
			Response struct {
				Output json.RawMessage `json:"output"`
			} `json:"response"`
		}
		line := strings.TrimPrefix(strings.Split(string(frame.Body.Data), "\n")[1], "data: ")
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if string(event.Response.Output) != "[]" {
			t.Fatalf("done-only response output=%s, want []", event.Response.Output)
		}
		return
	}
}

func TestFunctionCallIDsStagePerFrameWithoutPartialCommit(t *testing.T) {
	s := newStream(io.NopCloser(strings.NewReader("")), "resp_test", "msg_test")
	if err := s.consume([]byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","id":"existing","args":{}}}]}}]}}`)); err != nil {
		t.Fatal(err)
	}
	pendingBefore, idsBefore := len(s.pending), len(s.usedCallIDs)
	invalid := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","id":"new-id","args":{}}},{"functionCall":{"name":"lookup","id":"existing","args":{}}}]}}]}}`)
	if err := s.consume(invalid); err == nil {
		t.Fatal("accepted duplicate function-call ID")
	}
	if len(s.usedCallIDs) != idsBefore || s.usedCallIDs["new-id"] || len(s.pending) != pendingBefore {
		t.Fatalf("failed frame partially committed IDs/events: IDs=%v pending=%d", s.usedCallIDs, len(s.pending))
	}
	wire := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"id\":\"same\",\"args\":{}}},{\"functionCall\":{\"name\":\"lookup\",\"id\":\"same\",\"args\":{}}}]}}]}}\n\n"
	body := &trackingBody{Reader: strings.NewReader(wire)}
	stream := newStream(body, "resp_stream", "msg_stream")
	frame, err := stream.Next(context.Background())
	if err == nil || frame.Type != "" || !body.closed {
		t.Fatalf("duplicate-call frame escaped or body remained open: frame=%+v err=%v closed=%v", frame, err, body.closed)
	}
}

func TestFunctionCallRejectsInvalidAndSignatureBearingParts(t *testing.T) {
	if err := newStream(io.NopCloser(strings.NewReader("")), "resp_test", "msg_test").consume([]byte{0xff}); err == nil {
		t.Fatal("accepted invalid UTF-8 candidate bytes")
	}
	for _, part := range []string{
		`{"functionCall":{"name":"","args":{}}}`,
		`{"functionCall":{"name":"lookup","args":[]}}`,
		`{"functionCall":{"name":"lookup","args":{"a":1,"a":2}}}`,
		`{"functionCall":{"name":"lookup","args":{},"thoughtSignature":"opaque"}}`,
		`{"functionCall":{"name":"lookup","args":{}},"thought":true}`,
		`{"text":"ignored","functionCall":{"name":"lookup","args":{}}}`,
		`{"functionCall":{"name":"lookup","args":{}} ,"thoughtSignature":"opaque"}`,
	} {
		wire := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[" + part + "]}}]}}\n\n"
		f := &fakeDoer{response: response(http.StatusOK, wire)}
		out, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, services(f))
		if ge != nil {
			t.Fatal(ge)
		}
		if frame, err := out.Stream.Next(context.Background()); err == nil || frame.Type != "" {
			t.Errorf("accepted malformed/tool-signature part %s: %+v %v", part, frame, err)
		}
		_ = out.Stream.Close()
	}
	large := `{"functionCall":{"name":"lookup","args":{"value":"` + strings.Repeat("x", maxRequestBytes) + `"}}}`
	wire := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[" + large + "]}}]}}\n\n"
	f := &fakeDoer{response: response(http.StatusOK, wire)}
	out, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, services(f))
	if ge != nil {
		t.Fatal(ge)
	}
	if frame, err := out.Stream.Next(context.Background()); err == nil || frame.Type != "" {
		t.Fatalf("accepted oversized args: %+v %v", frame, err)
	}
	_ = out.Stream.Close()

	text := strings.Repeat("t", maxRequestBytes/2)
	args := strings.Repeat("a", maxRequestBytes/2)
	wire = "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"" + text + "\"}]}}]}}\n\n" +
		"data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"value\":\"" + args + "\"}}}]}}]}}\n\n"
	f = &fakeDoer{response: response(http.StatusOK, wire)}
	out, ge = ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, services(f))
	if ge != nil {
		t.Fatal(ge)
	}
	defer out.Stream.Close()
	for range 4 { // head, then created/item-added/text-delta body frames
		if frame, err := out.Stream.Next(context.Background()); err != nil || frame.Type == core.FrameComplete {
			t.Fatalf("text prefix did not stream before over-limit call: %+v %v", frame, err)
		}
	}
	if frame, err := out.Stream.Next(context.Background()); err == nil || frame.Type != "" {
		t.Fatalf("text and arguments exceeded shared retained-byte budget: %+v %v", frame, err)
	}
}

func TestResponsesEncoderAcceptsConnectorStream(t *testing.T) {
	wire := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"usable\"}]}}]}}\n\n" + "data: {\"response\":{\"done\":true}}\n\n"
	f := &fakeDoer{response: response(http.StatusOK, wire)}
	out, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, services(f))
	if ge != nil {
		t.Fatal(ge)
	}
	rw := httptest.NewRecorder()
	if err := responsesAdapter.Encode(rw, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), out, nil); err != nil {
		t.Fatalf("Encode rejected connector stream: %v", err)
	}
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), "usable") {
		t.Fatalf("encoded status=%d body=%q", rw.Code, rw.Body.String())
	}
}

func TestTerminalUsagePreservesMissingAndPartialCounts(t *testing.T) {
	for _, tc := range []struct {
		name, usage   string
		wantNil       bool
		input, output *int64
		completeness  core.UsageCompleteness
	}{
		{name: "missing", usage: "", wantNil: true},
		{name: "empty", usage: ",\"usageMetadata\":{}", wantNil: true},
		{name: "input only", usage: ",\"usageMetadata\":{\"promptTokenCount\":3}", input: new(int64(3)), completeness: core.UsagePartial},
		{name: "output zero", usage: ",\"usageMetadata\":{\"candidatesTokenCount\":0}", output: new(int64(0)), completeness: core.UsagePartial},
		{name: "both", usage: ",\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":0}", input: new(int64(3)), output: new(int64(0)), completeness: core.UsageComplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			terminal := "data: {\"response\":{\"done\":true" + tc.usage + "}}\n\n"
			wire := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"x\"}]}}]}}\n\n" + terminal
			f := &fakeDoer{response: response(http.StatusOK, wire)}
			out, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, services(f))
			if ge != nil {
				t.Fatal(ge)
			}
			defer out.Stream.Close()
			var usage *core.UsageReport
			for {
				frame, err := out.Stream.Next(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if frame.Type == core.FrameComplete {
					usage = frame.Complete.Usage
					break
				}
			}
			if tc.wantNil {
				if usage != nil {
					t.Fatalf("usage=%+v, want nil", usage)
				}
				return
			}
			if usage == nil || !sameCount(usage.InputTokens, tc.input) || !sameCount(usage.OutputTokens, tc.output) || usage.Completeness != tc.completeness {
				t.Fatalf("usage=%+v", usage)
			}
		})
	}
}

func sameCount(got, want *int64) bool {
	if got == nil || want == nil {
		return got == want
	}
	return *got == *want
}

func TestAdmissionRejectsBeforeDispatch(t *testing.T) {
	f := &fakeDoer{}
	c := ready(t)
	svc := services(f)
	for _, body := range []string{`{"model":"gemini-2.5-pro","input":"x","stream":true,"extra":1}`, `{"model":"gemini-2.5-pro","model":"gemini-2.5-pro","input":"x","stream":true}`, `{"model":"gemini-2.5-pro","input":"x","stream":false}`, `{"model":"gemini-2.5-pro","input":["x"],"stream":true}`, `{"model":"bad","input":"x","stream":true}`, `{"model":"gemini-2.5-pro","input":"x","stream":true,"tools":[{"type":"computer","name":"x"}]}`, `{"model":"gemini-2.5-pro","input":"x","stream":true,"previous_response_id":"resp_1"}`} {
		_, ge := c.Execute(context.Background(), request(body), core.AttemptScope{Mode: core.ModeTranslation}, svc)
		if ge == nil {
			t.Errorf("accepted %s", body)
		}
	}
	if f.calls != 0 {
		t.Fatalf("invalid requests dispatched %d times", f.calls)
	}
}

func TestCredentialAndHTTPFailures(t *testing.T) {
	c := ready(t)
	req := request(`{"model":"gemini-2.5-flash","input":"x","stream":true}`)
	for _, raw := range [][]byte{nil, []byte(`{}`), []byte(`{"access_token":"x","other":"y"}`), []byte(`{"access_token":"x","refresh_token":""}`), []byte(`{"access_token":"x","access_token":"y"}`), []byte(`{"access_token":"x"} {}`), []byte(`{"access_token":"x","refresh_token":4}`), []byte(`{"access_token":"x","REFRESH_TOKEN":"y"}`), []byte(`{"access_token":"x","refresh_token":"r","REFRESH_TOKEN":"y"}`), []byte(`{"ACCESS_TOKEN":"x","refresh_token":"r"}`)} {
		f := &fakeDoer{response: response(200, "data: {}\n\n")}
		svc := services(f)
		svc.Credentials = credentials{data: raw}
		_, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: core.ModeTranslation}, svc)
		if ge == nil || ge.Category != core.CategoryUnauthenticated || f.calls != 0 {
			t.Errorf("credentials %s: error=%v calls=%d", raw, ge, f.calls)
		}
	}
	f := &fakeDoer{response: response(200, "data: {\"response\":{\"done\":true}}\n\n")}
	svc := services(f)
	svc.Credentials = credentials{data: []byte(`{"access_token":"SYNTHETIC_ACCESS","refresh_token":"SYNTHETIC_REFRESH"}`)}
	out, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: core.ModeTranslation}, svc)
	if ge != nil || f.calls != 1 || f.request == nil || f.request.Header.Get("Authorization") != "Bearer SYNTHETIC_ACCESS" {
		t.Fatalf("oauth bundle dispatch: error=%v calls=%d", ge, f.calls)
	}
	_ = out.Stream.Close()
	for status, category := range map[int]core.ErrorCategory{401: core.CategoryUnauthenticated, 403: core.CategoryUnauthenticated, 429: core.CategoryRateLimited, 500: core.CategoryUnavailable, 302: core.CategoryUnavailable} {
		limited := &trackingBody{Reader: strings.NewReader(strings.Repeat("x", 20000))}
		f := &fakeDoer{response: &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: limited}}
		_, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: core.ModeTranslation}, services(f))
		if ge == nil || ge.Category != category || f.calls != 1 {
			t.Errorf("status %d: error=%v calls=%d", status, ge, f.calls)
		}
		if limited.read > 8192 || !limited.closed {
			t.Errorf("status %d body drain=%d closed=%t", status, limited.read, limited.closed)
		}
	}
}

func TestPrematureMalformedCancellationAndFakeBoundary(t *testing.T) {
	c := ready(t)
	req := request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`)
	for _, wire := range []string{"data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"x\"}]}}]}}\n\n", "data: {not-json}\n\n"} {
		f := &fakeDoer{response: response(200, wire)}
		out, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: core.ModeTranslation}, services(f))
		if ge != nil {
			t.Fatal(ge)
		}
		s := out.Stream
		_, streamErr := s.Next(context.Background())
		for streamErr == nil {
			_, streamErr = s.Next(context.Background())
		}
		if errors.Is(streamErr, io.EOF) {
			t.Fatalf("bad stream succeeded: %q", wire)
		}
		_ = s.Close()
	}
	f := &fakeDoer{err: errors.New("ambiguous")}
	_, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: core.ModeTranslation}, services(f))
	if ge == nil || ge.RetryDisposition != core.RetryUnknown || f.calls != 1 {
		t.Fatalf("ambiguous Do error: %v, calls %d", ge, f.calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f = &fakeDoer{}
	_, ge = c.Execute(ctx, req, core.AttemptScope{Mode: core.ModeTranslation}, services(f))
	if ge == nil || f.calls != 0 {
		t.Fatalf("cancelled dispatch: %v, calls %d", ge, f.calls)
	}
	ambient := &ambientDoer{}
	svc := services(nil)
	svc.Transport = ambient
	_, ge = c.Execute(context.Background(), req, core.AttemptScope{Mode: core.ModeTranslation}, svc)
	if ge == nil || ge.RetryDisposition != core.RetryUnknown || ambient.calls != 0 {
		t.Fatalf("ambient transport was not rejected: %v, calls %d", ge, ambient.calls)
	}
}

func TestCancellationClosesActiveStream(t *testing.T) {
	b := &cancelBody{started: make(chan struct{}), closed: make(chan struct{})}
	f := &fakeDoer{response: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: b}}
	out, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, services(f))
	if ge != nil {
		t.Fatal(ge)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if frame, err := out.Stream.Next(ctx); err != nil || frame.Type != core.FrameHead {
		t.Fatalf("head: %v %v", frame, err)
	}
	for range 5 { // response.created and the complete function-call item lifecycle
		if frame, err := out.Stream.Next(ctx); err != nil || frame.Type != core.FrameBody {
			t.Fatalf("function-call event: %+v %v", frame, err)
		}
	}
	done := make(chan error, 1)
	go func() { _, err := out.Stream.Next(ctx); done <- err }()
	select {
	case <-b.started:
	case <-time.After(time.Second):
		t.Fatal("stream read did not block")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled stream read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not release stream read")
	}
	select {
	case <-b.closed:
	default:
		t.Fatal("cancellation did not close response body")
	}
}

func TestConcurrentCloseAndCancellationDuringNext(t *testing.T) {
	for range 100 {
		b := &cancelBody{started: make(chan struct{}), closed: make(chan struct{})}
		s := newStream(b, "resp_test", "msg_test")
		ctx, cancel := context.WithCancel(context.Background())
		start := make(chan struct{})
		done := make(chan struct{})
		go func() { <-start; _, _ = s.Next(ctx); close(done) }()
		go func() { <-start; cancel(); _ = s.Close() }()
		close(start)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("concurrent cancellation did not release Next")
		}
		cancel()
		_ = s.Close()
		select {
		case <-b.closed:
		default:
			t.Fatal("concurrent cancellation did not close body")
		}
	}
}
