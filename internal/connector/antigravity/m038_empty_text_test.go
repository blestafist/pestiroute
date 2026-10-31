package antigravity

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestM038EmptyTextPartsAreNoOpsAcrossIncrementalStream(t *testing.T) {
	wire := strings.Join([]string{
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":""},{"thought":true,"text":""}]}}]}}`,
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":""},{"text":"before"},{"text":""},{"thought":true,"text":""},{"text":"after"}]}}]}}`,
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":""},{"functionCall":{"name":"lookup","args":{"q":"x"}}},{"text":""}]}}]}}`,
		`data: {"response":{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":""}]}}]}}`,
		`data: {"response":{"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2}}}`,
	}, "\n\n") + "\n\n"
	s := newStream(&chunkReadCloser{r: strings.NewReader(wire), chunk: 3}, "resp_m038", "msg_m038")
	var events []string
	var completion string
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
			if strings.Contains(body, "response.completed") {
				completion = body
			}
			if strings.HasPrefix(body, "event: ") {
				events = append(events, strings.SplitN(body, "\n", 2)[0][len("event: "):])
			}
		}
	}
	joinedEvents := strings.Join(events, ",")
	if strings.Count(joinedEvents, "response.output_text.delta") != 2 {
		t.Fatalf("empty text emitted an extra delta: %v", events)
	}
	if completion == "" || !strings.Contains(completion, `"input_tokens":4`) || !strings.Contains(completion, `"output_tokens":2`) {
		t.Fatalf("missing usage-preserving completion: %s", completion)
	}
	if !strings.Contains(joinedEvents, "response.function_call_arguments.delta") {
		t.Fatalf("function call between empty text parts was lost: %v", events)
	}
	if strings.Count(joinedEvents, "response.output_item.added") != 2 || strings.Contains(joinedEvents, "response.reasoning_summary_text.delta") {
		t.Fatalf("empty part created a phantom output item: %v", events)
	}
}

func TestM038EmptyOnlyTerminalFailsClosed(t *testing.T) {
	wire := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"\"}]}}]}}\n\n" +
		"data: {\"response\":{\"done\":true}}\n\n"
	s := newStream(&chunkReadCloser{r: strings.NewReader(wire), chunk: 2}, "resp_m038", "msg_m038")
	frame, err := s.Next(context.Background())
	if err != nil || frame.Type != core.FrameHead {
		t.Fatalf("initial frame = %v, %v", frame.Type, err)
	}
	for {
		frame, err = s.Next(context.Background())
		if err != nil {
			if !errors.Is(err, errEmptyContent) {
				t.Fatalf("terminal error = %v", err)
			}
			return
		}
		if frame.Type == core.FrameComplete || frame.Type == core.FrameBody && strings.Contains(string(frame.Body.Data), "response.completed") {
			t.Fatal("empty-only stream fabricated a successful completion")
		}
	}
}

func TestM038InvalidContentSubkindsAreFixed(t *testing.T) {
	tests := []struct {
		name, raw string
		subkind   invalidContentSubkind
	}{
		{"done with candidates", `{"response":{"done":true,"candidates":[{}]}}`, invalidContentDoneWithCandidates},
		{"text and function call", `{"response":{"candidates":[{"content":{"parts":[{"text":"","functionCall":{"name":"lookup","args":{}}}]}}]}}`, invalidContentTextWithFunctionCall},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs strings.Builder
			s := newStream(io.NopCloser(strings.NewReader("")), "r", "m", slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			err := s.consume([]byte(tc.raw))
			if !errors.Is(err, errInvalidContent) {
				t.Fatalf("consume error = %v", err)
			}
			s.logStreamError(err, true)
			got := logs.String()
			if !strings.Contains(got, "category=precommit_invalid_content") || !strings.Contains(got, "subkind="+string(tc.subkind)) || strings.Contains(got, tc.raw) {
				t.Fatalf("unexpected diagnostic: %q", got)
			}
		})
	}

	var logs strings.Builder
	s := newStream(io.NopCloser(strings.NewReader("")), "r", "m", slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	s.logStreamError(invalidContentError{subkind: invalidContentInvalidUTF8Text}, true)
	if !strings.Contains(logs.String(), "subkind=invalid_utf8_text") {
		t.Fatalf("invalid UTF-8 subkind missing: %q", logs.String())
	}

	logs.Reset()
	s.logStreamError(invalidContentError{subkind: invalidContentSubkind("PRIVATE-UNKNOWN-CANARY")}, true)
	if !strings.Contains(logs.String(), "category=precommit_invalid_content") || strings.Contains(logs.String(), "subkind=") || strings.Contains(logs.String(), "PRIVATE-UNKNOWN-CANARY") {
		t.Fatalf("unknown subkind was logged: %q", logs.String())
	}
}

func TestM038EmptyTextSignatureRejectsAtomically(t *testing.T) {
	for _, signature := range []string{`"sig"`, `null`, `{}`} {
		s := newStream(io.NopCloser(strings.NewReader("")), "r", "m")
		before := len(s.pending)
		raw := `{"response":{"candidates":[{"content":{"parts":[{"text":""},{"text":"x","thoughtSignature":` + signature + `}]}}]}}`
		if err := s.consume([]byte(raw)); !errors.Is(err, errUnsupportedReasoning) {
			t.Fatalf("signature %s error = %v", signature, err)
		}
		if len(s.pending) != before || s.outputs != nil || s.retainedBytes != 0 {
			t.Fatalf("signature %s partially committed frame", signature)
		}
	}
}

type m038CancelBody struct {
	data    []byte
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (b *m038CancelBody) Read(p []byte) (int, error) {
	if len(b.data) > 0 {
		n := copy(p, b.data)
		b.data = b.data[n:]
		return n, nil
	}
	b.once.Do(func() { close(b.started) })
	<-b.closed
	return 0, errors.New("closed")
}

func (b *m038CancelBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func TestM038EmptyNonterminalFrameWaitRemainsCancelable(t *testing.T) {
	body := &m038CancelBody{
		data:    []byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"\"}]}}]}}\n\n"),
		started: make(chan struct{}),
		closed:  make(chan struct{}),
	}
	s := newStream(body, "resp_m038", "msg_m038")
	frame, err := s.Next(context.Background())
	if err != nil || frame.Type != core.FrameHead {
		t.Fatalf("empty-frame head = %v, %v", frame.Type, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := s.Next(ctx)
		result <- err
	}()
	<-body.started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait error = %v", err)
	}
}
