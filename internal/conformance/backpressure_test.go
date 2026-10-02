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
}

func TestConformanceBackpressure(t *testing.T) {
	for _, name := range []string{"native-loopback", "scripted"} {
		t.Run(name, func(t *testing.T) {
			bf := newBackpressureFixture(t, name)
			t.Cleanup(bf.close)
			if err := bf.init(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := assertBoundedPause(bf, bf.runner); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConformanceBackpressureRejectsReadAheadMutation(t *testing.T) {
	for _, name := range []string{"native-loopback", "scripted"} {
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
					if err := assertBoundedPause(bf, mutation); err == nil {
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
			for i := range 2 {
				if _, err := response.Stream.Next(ctx); err != nil {
					t.Fatalf("initial frame %d: %v", i, err)
				}
			}
			next := make(chan error, 1)
			go func() { _, err := response.Stream.Next(ctx); next <- err }()
			waitSignal(t, waiting, "producer waiting at future-frame gate")
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
			awaitFinalized(t, finalized)
			assertOneAttempt(t, attempts, true, core.OutcomeCancelled, core.CategoryCancelled)
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
