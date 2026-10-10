package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
	"github.com/blestafist/pestiroute/internal/testutil/scripted"
)

const backpressureFrames = 1024

type backpressureFixture struct {
	fixture
	total       int
	maxProgress int
	progress    func() int
	runner      core.Connector
	gate        chan struct{}
	waiting     chan struct{}
}

func TestConformanceBackpressure(t *testing.T) {
	for _, name := range []string{"native-loopback", "anthropic-translation", "scripted"} {
		t.Run(name, func(t *testing.T) {
			bf := newBackpressureFixture(t, name)
			t.Cleanup(bf.close)
			if err := bf.init(context.Background()); err != nil {
				t.Fatal(err)
			}
			if bf.translated {
				if err := assertTranslationBackpressure(bf, nil); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err := assertBoundedPause(bf, bf.runner); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConformanceBackpressureRejectsReadAheadMutation(t *testing.T) {
	for _, name := range []string{"native-loopback", "anthropic-translation", "scripted"} {
		t.Run(name, func(t *testing.T) {
			for _, mutationName := range []string{"whole-response", "bounded-64"} {
				if name == "native-loopback" && mutationName == "bounded-64" {
					continue // Native OS socket readahead has no fixed step-count contract.
				}
				t.Run(mutationName, func(t *testing.T) {
					bf := newBackpressureFixture(t, name)
					t.Cleanup(bf.close)
					if err := bf.init(context.Background()); err != nil {
						t.Fatal(err)
					}
					mutation := streamWrappingConnector{Connector: bf.runner, wrap: func(stream core.Stream) core.Stream {
						if mutationName == "bounded-64" {
							return &readAheadStream{Stream: stream, limit: 64}
						}
						return &bufferingStream{Stream: stream}
					}}
					var err error
					if bf.translated {
						err = assertTranslationBackpressure(bf, mutation)
					} else {
						err = assertBoundedPause(bf, mutation)
					}
					if err == nil {
						t.Fatalf("%s mutation passed the shared stalled-reader assertion", mutationName)
					}
				})
			}
		})
	}
}

// assertBoundedPause measures actual fixture production while the reader stops
// after Head and the first Body. Native writes may fill OS socket buffers, so
// the invariant is that a response far larger than those buffers is not fully
// produced before the first Body is delivered, not an exact chunk count.
func assertBoundedPause(bf backpressureFixture, connector core.Connector) error {
	response, gatewayErr := connector.Execute(context.Background(), bf.request, bf.scope, bf.services())
	if gatewayErr != nil {
		return gatewayErr
	}
	defer response.Stream.Close()
	var body []byte
	for i, want := range []core.FrameType{core.FrameHead, core.FrameBody} {
		frame, err := response.Stream.Next(context.Background())
		if err != nil || frame.Type != want {
			return fmt.Errorf("initial frame %d = %q, %v; want %q", i, frame.Type, err, want)
		}
		if frame.Type == core.FrameBody {
			body = append(body, frame.Body.Data...)
		}
	}
	if produced := bf.progress(); produced > bf.maxProgress {
		return fmt.Errorf("producer progressed %d steps while reader was paused; bound is %d", produced, bf.maxProgress)
	}
	frames, rest, err := readFrames(response.Stream)
	if err != nil {
		return err
	}
	body = append(body, rest...)
	if countFrames(frames, core.FrameComplete) != 1 || !bytes.Equal(body, bf.wantBody) {
		diff := 0
		for diff < len(body) && diff < len(bf.wantBody) && body[diff] == bf.wantBody[diff] {
			diff++
		}
		return fmt.Errorf("resumed stream mismatch: complete=%d body bytes=%d want=%d diff=%d got=%q want=%q", countFrames(frames, core.FrameComplete), len(body), len(bf.wantBody), diff, body[max(0, diff-8):min(len(body), diff+24)], bf.wantBody[max(0, diff-8):min(len(bf.wantBody), diff+24)])
	}
	return nil
}

func newBackpressureFixture(t *testing.T, name string) backpressureFixture {
	t.Helper()
	if name == "anthropic-translation" {
		return newTranslationBackpressureFixture(t)
	}
	if name == "codex" {
		const chunkSize = 32 << 10
		chunk := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"" + strings.Repeat("x", chunkSize-85) + "\"}\n\n")
		steps := []fakeupstream.Step{{Data: incrementalFirst}}
		sent := make([]chan struct{}, 0, backpressureFrames)
		want := make([]byte, 0, backpressureFrames*chunkSize)
		want = append(want, incrementalFirst...)
		for range backpressureFrames - 1 {
			delivered := make(chan struct{})
			sent = append(sent, delivered)
			steps = append(steps, fakeupstream.Step{Data: chunk, Sent: delivered})
			want = append(want, chunk...)
		}
		terminal := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		terminalSent := make(chan struct{})
		sent = append(sent, terminalSent)
		steps = append(steps, fakeupstream.Step{Data: terminal, Sent: terminalSent})
		want = append(want, terminal...)
		fx := codexFixtureWithResponse(t, fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: steps})
		fx.wantBody = want
		sentCount := 0
		return backpressureFixture{fixture: fx, total: backpressureFrames, maxProgress: backpressureFrames - 1, runner: fx.connector, progress: func() int {
			for sentCount < len(sent) {
				select {
				case <-sent[sentCount]:
					sentCount++
				default:
					return sentCount
				}
			}
			return sentCount
		}}
	}
	if name == "scripted" {
		steps := []scripted.Step{{Frame: headFrame()}}
		for range backpressureFrames {
			steps = append(steps, scripted.Step{Frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: incrementalFirst}}})
		}
		steps = append(steps, scripted.Step{Frame: completeFrame()})
		fx := newScriptedCustomFixture(t, steps)
		fx.wantBody = make([]byte, 0, backpressureFrames*len(incrementalFirst))
		for range backpressureFrames {
			fx.wantBody = append(fx.wantBody, incrementalFirst...)
		}
		var nextCount atomic.Int64
		counter := countStepsConnector{Connector: fx.connector, count: &nextCount}
		return backpressureFixture{
			fixture: fx, total: backpressureFrames, maxProgress: 2, runner: counter,
			progress: func() int { return int(nextCount.Load()) },
		}
	}

	// 1023 x 32 KiB = about 32 MiB, intentionally much larger than typical socket
	// buffering. No exact number of writes is assumed: only whole-response
	// accumulation before the first Body is forbidden.
	const chunkSize = 32 << 10
	chunk := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"" + strings.Repeat("x", chunkSize-85) + "\"}\n\n")
	steps := make([]fakeupstream.Step, 0, backpressureFrames+1)
	steps = append(steps, fakeupstream.Step{Data: incrementalFirst})
	sent := make([]chan struct{}, 0, backpressureFrames)
	for range backpressureFrames - 1 {
		delivered := make(chan struct{})
		sent = append(sent, delivered)
		steps = append(steps, fakeupstream.Step{Data: chunk, Sent: delivered})
	}
	terminal := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	terminalSent := make(chan struct{})
	sent = append(sent, terminalSent)
	steps = append(steps, fakeupstream.Step{Data: terminal, Sent: terminalSent})
	fx, _ := nativeCustomFixture(t, fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: steps})
	streaming := true
	fx.request.Metadata.Streaming = &streaming
	sentCount := 0
	want := make([]byte, 0, (backpressureFrames-1)*len(chunk)+len(incrementalFirst)+len(terminal))
	want = append(want, incrementalFirst...)
	for range backpressureFrames - 1 {
		want = append(want, chunk...)
	}
	want = append(want, terminal...)
	fx.wantBody = want
	return backpressureFixture{fixture: fx, total: backpressureFrames, maxProgress: backpressureFrames - 1, runner: fx.connector, progress: func() int {
		for sentCount < len(sent) {
			select {
			case <-sent[sentCount]:
				sentCount++
			default:
				return sentCount
			}
		}
		return sentCount
	}}
}

func newTranslationBackpressureFixture(t *testing.T) backpressureFixture {
	t.Helper()
	gate, waiting := make(chan struct{}), make(chan struct{}, 1)
	steps := []fakeupstream.Step{
		{Data: []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}}\n\n")},
		{Gate: gate, Waiting: waiting, Data: []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")},
	}
	steps = append(steps,
		fakeupstream.Step{Data: []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"released\"}}\n\n")},
		fakeupstream.Step{Data: []byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")},
		fakeupstream.Step{Data: []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n")},
		fakeupstream.Step{Data: []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")},
	)
	fx := translationFixtureWithResponse(t, fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: steps})
	fx.release = func() { close(gate) }
	return backpressureFixture{
		fixture: fx, runner: fx.connector, gate: gate, waiting: waiting,
	}
}

func assertTranslationBackpressure(bf backpressureFixture, connector core.Connector) error {
	var (
		response   core.ExecutionResponse
		gatewayErr *core.GatewayError
		attempts   []core.AttemptResult
		finalized  = make(chan struct{}, 1)
	)
	if connector == nil {
		d := bf.dispatcher
		d.Finalize = func(result core.AttemptResult) {
			attempts = append(attempts, result)
			finalized <- struct{}{}
		}
		response, gatewayErr = d.Execute(context.Background(), bf.request)
	} else {
		response, gatewayErr = connector.Execute(context.Background(), bf.request, bf.scope, bf.services())
	}
	if gatewayErr != nil {
		return gatewayErr
	}
	defer response.Stream.Close()
	defer func() {
		select {
		case <-bf.gate:
		default:
			close(bf.gate)
		}
	}()
	frame, err := boundedNext(response.Stream, 2*time.Second)
	if err != nil || frame.Type != core.FrameHead {
		return fmt.Errorf("translation Head = %+v, %v", frame, err)
	}
	frame, err = boundedNext(response.Stream, 2*time.Second)
	if err != nil || frame.Type != core.FrameBody || !bytes.Contains(frame.Body.Data, []byte(`"type":"response.created"`)) {
		return fmt.Errorf("first translated Body = %+v, %v", frame, err)
	}
	select {
	case <-bf.waiting:
	case <-time.After(2 * time.Second):
		return fmt.Errorf("upstream did not pause at the gated event")
	}
	type nextResult struct {
		frame core.StreamFrame
		err   error
	}
	var pending chan nextResult
	var seenFrames []core.StreamFrame
	var seenBody []byte
	for {
		next := make(chan nextResult, 1)
		go func() {
			frame, err := response.Stream.Next(context.Background())
			next <- nextResult{frame: frame, err: err}
		}()
		select {
		case got := <-next:
			if got.err != nil {
				return fmt.Errorf("translation stream ended before gated upstream resumed: %w", got.err)
			}
			seenFrames = append(seenFrames, got.frame)
			if got.frame.Type == core.FrameBody {
				seenBody = append(seenBody, got.frame.Body.Data...)
			}
		case <-time.After(50 * time.Millisecond):
			pending = next
			goto paused
		}
	}

paused:
	select {
	case <-bf.gate:
	default:
		close(bf.gate)
	}
	var resumed core.StreamFrame
	select {
	case got := <-pending:
		if got.err != nil {
			return fmt.Errorf("translation Next after gate: %w", got.err)
		}
		resumed = got.frame
	case <-time.After(2 * time.Second):
		return fmt.Errorf("translation Next stayed blocked after gate release")
	}
	frames, body, err := readFrames(response.Stream)
	frames = append(append(seenFrames, resumed), frames...)
	if resumed.Type == core.FrameBody {
		body = append(append(seenBody, resumed.Body.Data...), body...)
	} else {
		body = append(seenBody, body...)
	}
	if err != nil || countFrames(frames, core.FrameComplete) != 1 || !bytes.Contains(body, []byte(`"type":"response.completed"`)) {
		return fmt.Errorf("translation did not resume after gate: frames=%+v err=%v", frames, err)
	}
	if connector == nil {
		select {
		case <-finalized:
		default:
			return fmt.Errorf("dispatcher did not finalize translated backpressure attempt")
		}
		if len(attempts) != 1 || attempts[0].Outcome != core.OutcomeSucceeded || !attempts[0].Committed || !attempts[0].HasUsage || attempts[0].Usage.Source != core.UsageProvider || attempts[0].Usage.Completeness != core.UsageComplete || attempts[0].Usage.InputTokens == nil || *attempts[0].Usage.InputTokens != 1 || attempts[0].Usage.OutputTokens == nil || *attempts[0].Usage.OutputTokens != 1 {
			return fmt.Errorf("translated backpressure attempt did not finalize once as committed success: %+v", attempts)
		}
	}
	return nil
}

func boundedNext(stream core.Stream, timeout time.Duration) (core.StreamFrame, error) {
	type result struct {
		frame core.StreamFrame
		err   error
	}
	done := make(chan result, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { frame, err := stream.Next(ctx); done <- result{frame: frame, err: err} }()
	select {
	case result := <-done:
		return result.frame, result.err
	case <-time.After(timeout):
		cancel()
		<-done
		return core.StreamFrame{}, fmt.Errorf("stream Next exceeded %s", timeout)
	}
}

type readAheadStream struct {
	core.Stream
	frames      []core.StreamFrame
	limit       int
	initialized bool
}

func (s *readAheadStream) Next(ctx context.Context) (core.StreamFrame, error) {
	if !s.initialized {
		s.initialized = true
		for range s.limit {
			frame, err := s.Stream.Next(ctx)
			if err != nil {
				return core.StreamFrame{}, err
			}
			s.frames = append(s.frames, frame)
		}
	}
	if len(s.frames) == 0 {
		return s.Stream.Next(ctx)
	}
	frame := s.frames[0]
	s.frames = s.frames[1:]
	return frame, nil
}

type countStepsConnector struct {
	core.Connector
	count *atomic.Int64
}

func (c countStepsConnector) Execute(ctx context.Context, request core.ExecutionRequest, scope core.AttemptScope, services core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	response, gatewayErr := c.Connector.Execute(ctx, request, scope, services)
	if gatewayErr == nil && response.Stream != nil {
		response.Stream = countStepsStream{Stream: response.Stream, count: c.count}
	}
	return response, gatewayErr
}

type countStepsStream struct {
	core.Stream
	count *atomic.Int64
}

func (s countStepsStream) Next(ctx context.Context) (core.StreamFrame, error) {
	frame, err := s.Stream.Next(ctx)
	if err == nil {
		s.count.Add(1)
	}
	return frame, err
}

func TestConformanceBackpressureCancellationWhileStalledNext(t *testing.T) {
	for _, tc := range cancellationFixtures() {
		t.Run(tc.name, func(t *testing.T) {
			gate, waiting := make(chan struct{}), make(chan struct{}, 1)
			fx := tc.new(t, gate, waiting)
			t.Cleanup(fx.close)
			if err := fx.init(context.Background()); err != nil {
				t.Fatal(err)
			}
			d, attempts, finalized := cancellationDispatcher(t, fx)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			response, gatewayErr := d.Execute(ctx, fx.request)
			if gatewayErr != nil {
				t.Fatal(gatewayErr)
			}
			var next chan error
			if tc.translated {
				if frame, err := response.Stream.Next(ctx); err != nil || frame.Type != core.FrameHead {
					t.Fatalf("Head = %+v, %v", frame, err)
				}
				waitSignal(t, waiting, "translation upstream gate")
				next = nextUntilBlocked(ctx, response.Stream)
			} else {
				for i := range 2 {
					if _, err := response.Stream.Next(ctx); err != nil {
						t.Fatalf("initial frame %d: %v", i, err)
					}
				}
				next = make(chan error, 1)
				go func() { _, err := response.Stream.Next(ctx); next <- err }()
				waitSignal(t, waiting, "producer waiting at future-frame gate")
			}
			cancel()
			select {
			case err := <-next:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("stalled Next returned %v, want context cancellation", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("stalled Next did not unblock on cancellation")
			}
			if err := response.Stream.Close(); err != nil {
				t.Fatal(err)
			}
			if !tc.translated {
				awaitFinalized(t, finalized)
				assertOneAttempt(t, attempts, true, core.OutcomeCancelled, core.CategoryCancelled)
			}
			if !tc.scripted && (fx.waitCancel == nil || !fx.waitCancel()) {
				t.Fatal("native upstream did not observe local cancellation")
			}
			if tc.scripted && fx.connector.(*scripted.Connector).ActiveStreamCount() != 0 {
				t.Fatal("scripted producer remained active after cancellation")
			}
			close(gate)
		})
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s (safety bound)", description)
	}
}
