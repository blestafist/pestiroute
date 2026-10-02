package conformance

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
	"github.com/blestafist/pestiroute/internal/testutil/scripted"
)

func TestConformancePreHeadCancellation(t *testing.T) {
	for _, tc := range cancellationFixtures() {
		t.Run(tc.name, func(t *testing.T) {
			fx := tc.new(t, nil, nil)
			t.Cleanup(fx.close)
			if err := fx.init(context.Background()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			d, attempts, finalized := cancellationDispatcher(t, fx)
			response, gatewayErr := d.Execute(ctx, fx.request)
			if gatewayErr != nil || response.Stream == nil {
				t.Fatalf("Execute response=%+v error=%+v", response, gatewayErr)
			}
			cancel()
			if frame, err := response.Stream.Next(context.Background()); !errors.Is(err, context.Canceled) || frame.Type != "" {
				t.Fatalf("cancelled pre-Head Next = %+v, %v", frame, err)
			}
			_ = response.Stream.Close()
			awaitFinalized(t, finalized)
			if !tc.scripted && (fx.waitCancel == nil || !fx.waitCancel()) {
				t.Fatal("native upstream did not observe local request cancellation")
			}
			if fx.release != nil {
				fx.release()
			}
			assertOneAttempt(t, attempts, false, core.OutcomeCancelled, core.CategoryCancelled)
			if fx.callCount() != 1 {
				t.Fatalf("execution count = %d, want one", fx.callCount())
			}
			assertHealthyFollowup(t, d, fx, attempts, finalized, 1, 1)
		})
	}
}

func TestConformanceBlockedNextCancellationAndPartialClose(t *testing.T) {
	for _, tc := range cancellationFixtures() {
		for _, abandon := range []bool{false, true} {
			name := "blocked-next-cancel"
			if abandon {
				name = "partial-close"
			}
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				gate := make(chan struct{})
				waiting := make(chan struct{}, 1)
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
				for i := range 2 { // Head and first Body, leaving the producer gated.
					frame, err := response.Stream.Next(ctx)
					if err != nil || (i == 0 && frame.Type != core.FrameHead) || (i == 1 && frame.Type != core.FrameBody) {
						t.Fatalf("frame %d = %+v, %v", i, frame, err)
					}
				}
				finished := make(chan error, 1)
				go func() {
					_, err := response.Stream.Next(ctx)
					finished <- err
				}()
				if tc.scripted {
					select {
					case <-waiting:
					case <-time.After(time.Second):
						t.Fatal("scripted producer did not reach gate")
					}
				}
				if abandon {
					if err := response.Stream.Close(); err != nil {
						t.Fatalf("partial Close: %v", err)
					}
				} else {
					cancel()
				}
				select {
				case err := <-finished:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("blocked Next error = %v, want cancellation/Close", err)
					}
				case <-time.After(2 * time.Second):
					cancel()
					_ = response.Stream.Close()
					t.Fatal("cancellation/Close did not release blocked Next")
				}
				if abandon {
					cancel()
				}
				if err := response.Stream.Close(); err != nil {
					t.Fatalf("repeated Close: %v", err)
				}
				awaitFinalized(t, finalized)
				if !tc.scripted && (fx.waitCancel == nil || !fx.waitCancel()) {
					t.Fatal("native upstream did not observe local request cancellation")
				}
				if tc.scripted && fx.connector.(*scripted.Connector).ActiveStreamCount() != 0 {
					t.Fatal("scripted stream remains active after Close")
				}
				if tc.scripted && fx.connector.(*scripted.Connector).StreamCloseCount() != 1 {
					t.Fatalf("producer Close count=%d, want exactly one", fx.connector.(*scripted.Connector).StreamCloseCount())
				}
				close(gate)
				assertOneAttempt(t, attempts, true, core.OutcomeCancelled, core.CategoryCancelled)
				if fx.callCount() != 1 {
					t.Fatalf("execution count = %d, want one (no replay)", fx.callCount())
				}
				assertHealthyFollowup(t, d, fx, attempts, finalized, 1, 1)
			})
		}
	}
}

func TestConformancePartialUsageSurvivesAbandonedClose(t *testing.T) {
	input := int64(9)
	usage := &core.UsageReport{InputTokens: &input, Source: core.UsageProvider, Completeness: core.UsagePartial}
	gate, waiting := make(chan struct{}), make(chan struct{}, 1)
	fx := newScriptedCustomFixture(t, []scripted.Step{
		{Frame: headFrame()},
		{Frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: incrementalFirst}}},
		{Frame: core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded, Usage: usage}}},
		{Err: io.EOF, Gate: gate, Waiting: waiting},
	})
	defer fx.close()
	if err := fx.init(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, attempts, finalized := cancellationDispatcher(t, fx)
	response, gatewayErr := d.Execute(context.Background(), fx.request)
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	for i := range 3 {
		if _, err := response.Stream.Next(context.Background()); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	blocked := make(chan error, 1)
	go func() { _, err := response.Stream.Next(context.Background()); blocked <- err }()
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("producer did not reach the gated terminal EOF")
	}
	if err := response.Stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-blocked:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked Next error = %v, want cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not release blocked Next")
	}
	awaitFinalized(t, finalized)
	assertOneAttempt(t, attempts, true, core.OutcomeCancelled, core.CategoryCancelled)
	got := (*attempts)[0]
	if !got.HasUsage || got.Usage.InputTokens == nil || *got.Usage.InputTokens != input || got.Usage.Source != core.UsageProvider || got.Usage.Completeness != core.UsagePartial {
		t.Fatalf("partial usage was not retained: %+v", got)
	}
	if fx.connector.(*scripted.Connector).ActiveStreamCount() != 0 || fx.callCount() != 1 {
		t.Fatalf("cleanup/replay: active=%d executions=%d", fx.connector.(*scripted.Connector).ActiveStreamCount(), fx.callCount())
	}
	if fx.connector.(*scripted.Connector).StreamCloseCount() != 1 {
		t.Fatalf("producer Close count=%d, want one", fx.connector.(*scripted.Connector).StreamCloseCount())
	}
}

func TestConformanceNativeObservedUsageSurvivesClose(t *testing.T) {
	fx, _ := nativeCustomFixture(t, fakeupstream.Response{
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   []byte(`{"status":"completed","usage":{"input_tokens":9,"output_tokens":2}}`),
	})
	defer fx.close()
	if err := fx.init(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, attempts, finalized := cancellationDispatcher(t, fx)
	response, gatewayErr := d.Execute(context.Background(), fx.request)
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	for i := range 3 { // Observe Head, body, then usage-bearing Complete before EOF.
		frame, err := response.Stream.Next(context.Background())
		if err != nil || (i == 2 && frame.Type != core.FrameComplete) {
			t.Fatalf("frame %d = %+v, %v", i, frame, err)
		}
	}
	if err := response.Stream.Close(); err != nil {
		t.Fatal(err)
	}
	awaitFinalized(t, finalized)
	assertOneAttempt(t, attempts, true, core.OutcomeCancelled, core.CategoryCancelled)
	got := (*attempts)[0]
	if !got.HasUsage || got.Usage.InputTokens == nil || *got.Usage.InputTokens != 9 || got.Usage.OutputTokens == nil || *got.Usage.OutputTokens != 2 || got.Usage.Source != core.UsageProvider {
		t.Fatalf("native observed usage was not retained: %+v", got)
	}
}

func cancellationDispatcher(t *testing.T, fx fixture) (*core.Dispatcher, *[]core.AttemptResult, <-chan struct{}) {
	t.Helper()
	var attempts []core.AttemptResult
	finalized := make(chan struct{}, 1)
	d := &core.Dispatcher{AccountID: account, Finalize: func(result core.AttemptResult) {
		attempts = append(attempts, result)
		finalized <- struct{}{}
	}}
	executions := 0
	d.Target = conformanceTarget(func(ctx context.Context, in core.ExecutionRequest, scope core.AttemptScope) (core.ExecutionResponse, *core.GatewayError) {
		if _, ok := fx.connector.(*scripted.Connector); ok {
			executions++
			if executions == 1 {
				in.ID = request
			} else {
				in.ID = "conformance-followup"
			}
		}
		return fx.connector.Execute(ctx, in, scope, fx.services())
	})
	return d, &attempts, finalized
}

func assertHealthyFollowup(t *testing.T, d *core.Dispatcher, fx fixture, attempts *[]core.AttemptResult, finalized <-chan struct{}, priorAttempts int, priorCalls int64) {
	t.Helper()
	followup, gatewayErr := d.Execute(context.Background(), fx.request)
	if gatewayErr != nil {
		t.Fatalf("follow-up Execute on same connector/dispatcher: %v", gatewayErr)
	}
	if _, body, err := readFrames(followup.Stream); err != nil || len(body) == 0 {
		t.Fatalf("follow-up stream body=%q error=%v", body, err)
	}
	awaitFinalized(t, finalized)
	if len(*attempts) != priorAttempts+1 || (*attempts)[priorAttempts].Outcome != core.OutcomeSucceeded || fx.callCount() != priorCalls+1 {
		t.Fatalf("follow-up was not successful exactly once: attempts=%+v calls=%d", *attempts, fx.callCount())
	}
	if connector, ok := fx.connector.(*scripted.Connector); ok && (connector.ActiveStreamCount() != 0 || connector.StreamCloseCount() != int(priorCalls+1)) {
		t.Fatalf("follow-up left scripted capacity/cleanup wrong: active=%d closes=%d", connector.ActiveStreamCount(), connector.StreamCloseCount())
	}
}

func TestConformanceAlreadyCancelledExecute(t *testing.T) {
	for _, tc := range cancellationFixtures() {
		t.Run(tc.name, func(t *testing.T) {
			fx := tc.new(t, nil, nil)
			defer fx.close()
			if err := fx.init(context.Background()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			d, attempts, finalized := cancellationDispatcher(t, fx)
			response, gatewayErr := d.Execute(ctx, fx.request)
			if gatewayErr == nil || gatewayErr.Category != core.CategoryCancelled || response.Stream != nil || fx.callCount() != 0 {
				t.Fatalf("pre-cancelled Execute response=%+v error=%+v calls=%d", response, gatewayErr, fx.callCount())
			}
			awaitFinalized(t, finalized)
			assertOneAttempt(t, attempts, false, core.OutcomeCancelled, core.CategoryCancelled)
			if fx.release != nil {
				fx.release()
			}
			assertHealthyFollowup(t, d, fx, attempts, finalized, 1, 0)
		})
	}
}

func awaitFinalized(t *testing.T, finalized <-chan struct{}) {
	t.Helper()
	select {
	case <-finalized:
	case <-time.After(2 * time.Second):
		t.Fatal("attempt was not finalized within deadline")
	}
}

type cancellationFixture struct {
	name     string
	scripted bool
	new      func(*testing.T, <-chan struct{}, chan<- struct{}) fixture
}

func cancellationFixtures() []cancellationFixture {
	return []cancellationFixture{
		{name: "native-loopback", new: func(t *testing.T, gate <-chan struct{}, sent chan<- struct{}) fixture {
			var firstGate <-chan struct{}
			var releaseGate chan struct{}
			if gate == nil {
				releaseGate = make(chan struct{})
				firstGate = releaseGate
			}
			steps := []fakeupstream.Step{{Gate: firstGate, Data: incrementalFirst}}
			if gate != nil {
				steps = append(steps, fakeupstream.Step{Gate: gate, Sent: sent, Data: incrementalTerminal})
			} else {
				steps = append(steps, fakeupstream.Step{Data: incrementalTerminal})
			}
			fx, _ := nativeCustomFixture(t, fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: steps})
			streaming := true
			fx.request.Metadata.Streaming = &streaming
			if releaseGate != nil {
				fx.release = func() { close(releaseGate) }
			}
			return fx
		}},
		{name: "scripted", scripted: true, new: func(t *testing.T, gate <-chan struct{}, waiting chan<- struct{}) fixture {
			steps := []scripted.Step{{Frame: headFrame()}, {Frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: incrementalFirst}}}}
			if gate != nil {
				steps = append(steps, scripted.Step{Frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: incrementalTerminal}}, Gate: gate, Waiting: waiting})
			} else {
				steps = append(steps, scripted.Step{Frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: incrementalTerminal}}})
			}
			steps = append(steps, scripted.Step{Frame: completeFrame()})
			scripts := []scripted.Script{{ID: request, Steps: steps}}
			scripts = append(scripts, scripted.Script{ID: "conformance-followup", Steps: []scripted.Step{
				{Frame: headFrame()},
				{Frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: []byte("follow-up-ok")}}},
				{Frame: completeFrame()},
			}})
			return newScriptedScriptsFixture(t, scripts)
		}},
	}
}
