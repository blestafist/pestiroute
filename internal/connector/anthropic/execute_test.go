package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

func TestExecuteStreamTranslateEarlyHeadAndUsage(t *testing.T) {
	first := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"
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

func TestExecuteRejectsBeforeDispatchAndHTTPRejection(t *testing.T) {
	calls := 0
	services := core.InvocationServices{Credentials: credentialStub("secret"), Transport: doerFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, nil })}
	request := core.ExecutionRequest{Model: "gpt-4.1-mini", Payload: core.RawPayload{Protocol: protocol, Body: []byte(`{"model":"gpt-4.1-mini","stream":true,"input":"hello","tools":[]}`)}}
	if _, err := executeConnector(t).Execute(context.Background(), request, core.AttemptScope{Mode: core.ModeTranslation, AccountID: "account-a"}, services); err == nil || err.Category != core.CategoryInvalidRequest || calls != 0 {
		t.Fatalf("local rejection=%+v dispatches=%d", err, calls)
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
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n"))}, nil
		}),
	})
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	defer resp.Stream.Close()
	if _, err := resp.Stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := resp.Stream.Next(context.Background()); err != nil {
			t.Fatalf("initial lifecycle frame %d: %v", i, err)
		}
	}
	if _, err := resp.Stream.Next(context.Background()); err == nil || err == io.EOF {
		t.Fatalf("EOF without message_stop returned %v", err)
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

type closeReader struct {
	io.Reader
	close func()
}

func (r closeReader) Close() error { r.close(); return nil }
