package scripted

import (
	"context"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestConcurrentScriptsAreIsolatedAndCaptured(t *testing.T) {
	gateA := make([]chan struct{}, 3)
	gateB := make([]chan struct{}, 3)
	waitA := make([]chan struct{}, 3)
	waitB := make([]chan struct{}, 3)
	stepsA, stepsB := validSteps("a"), validSteps("b")
	for i := range stepsA {
		gateA[i], gateB[i] = make(chan struct{}), make(chan struct{})
		waitA[i], waitB[i] = make(chan struct{}, 1), make(chan struct{}, 1)
		stepsA[i].Gate, stepsB[i].Gate = gateA[i], gateB[i]
		stepsA[i].Waiting, stepsB[i].Waiting = waitA[i], waitB[i]
	}
	connector := newConnector(Script{ID: "a", Steps: stepsA}, Script{ID: "b", Steps: stepsB})
	if err := connector.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	responses := make(map[string]core.ExecutionResponse, 2)
	type execution struct {
		id       string
		response core.ExecutionResponse
		err      *core.GatewayError
	}
	start, executed := make(chan struct{}), make(chan execution, 2)
	for _, id := range []string{"a", "b"} {
		id := id
		go func() {
			<-start
			scope := core.AttemptScope{ID: "attempt-" + id, AccountID: "account-" + id, Mode: "native"}
			request := core.ExecutionRequest{ID: id, Payload: core.RawPayload{Body: []byte(id)}}
			response, gatewayErr := connector.Execute(context.Background(), request, scope, core.InvocationServices{})
			request.Payload.Body[0] = 'x'
			executed <- execution{id: id, response: response, err: gatewayErr}
		}()
	}
	close(start)
	for range 2 {
		result := <-executed
		if result.err != nil {
			t.Fatalf("Execute(%s): %v", result.id, result.err)
		}
		responses[result.id] = result.response
	}
	// Both streams have reached each frame gate before either is released. Release
	// in interleaved order so independence cannot pass via sequential completion.
	headA, headB := pull(responses["a"].Stream), pull(responses["b"].Stream)
	awaitWaiting(t, waitA[0])
	awaitWaiting(t, waitB[0])
	close(gateB[0])
	assertPull(t, headB, core.FrameHead, "")
	close(gateA[0])
	assertPull(t, headA, core.FrameHead, "")
	bodyA, bodyB := pull(responses["a"].Stream), pull(responses["b"].Stream)
	awaitWaiting(t, waitA[1])
	awaitWaiting(t, waitB[1])
	close(gateA[1])
	assertPull(t, bodyA, core.FrameBody, "a")
	close(gateB[1])
	assertPull(t, bodyB, core.FrameBody, "b")
	completeA, completeB := pull(responses["a"].Stream), pull(responses["b"].Stream)
	awaitWaiting(t, waitA[2])
	awaitWaiting(t, waitB[2])
	close(gateB[2])
	assertPull(t, completeB, core.FrameComplete, "")
	close(gateA[2])
	assertPull(t, completeA, core.FrameComplete, "")
	for _, id := range []string{"a", "b"} {
		if _, err := responses[id].Stream.Next(context.Background()); err != io.EOF {
			t.Fatalf("EOF(%s) = %v", id, err)
		}
		_ = responses[id].Stream.Close()
	}
	if got := connector.CallCount(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
	calls := connector.Calls()
	for _, call := range calls {
		if string(call.Request.Payload.Body) != call.Request.ID || call.Scope.AccountID != "account-"+call.Request.ID {
			t.Errorf("captured call = %#v", call)
		}
	}
	calls[0].Request.Payload.Body[0] = 'z'
	if reflect.DeepEqual(calls[0].Request.Payload.Body, connector.Calls()[0].Request.Payload.Body) {
		t.Fatal("Calls exposed mutable internal capture")
	}
}

func TestGateBoundsPullAndCancellationAndClose(t *testing.T) {
	gate := make(chan struct{})
	waiting := make(chan struct{}, 1)
	steps := validSteps("payload")
	steps[1].Gate, steps[1].Waiting = gate, waiting
	connector := newConnector(Script{ID: "blocked", Steps: steps})
	if err := connector.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	response, gatewayErr := connector.Execute(context.Background(), core.ExecutionRequest{ID: "blocked"}, core.AttemptScope{}, core.InvocationServices{})
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	if _, duplicateErr := connector.Execute(context.Background(), core.ExecutionRequest{ID: "blocked"}, core.AttemptScope{}, core.InvocationServices{}); duplicateErr == nil || connector.CallCount() != 1 {
		t.Fatalf("duplicate execution accepted: err=%v calls=%d", duplicateErr, connector.CallCount())
	}
	if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, err := response.Stream.Next(ctx)
		finished <- err
	}()
	awaitWaiting(t, waiting)
	cancel()
	if err := <-finished; err != context.Canceled {
		t.Fatalf("blocked Next error = %v", err)
	}
	close(gate)
	frame, err := response.Stream.Next(context.Background())
	if err != nil || frame.Type != core.FrameBody || string(frame.Body.Data) != "payload" {
		t.Fatalf("cancelled pull advanced cursor: frame=%#v err=%v", frame, err)
	}
	frame, err = response.Stream.Next(context.Background())
	if err != nil || frame.Type != core.FrameComplete {
		t.Fatalf("Complete after gated body: frame=%#v err=%v", frame, err)
	}
	if _, err = response.Stream.Next(context.Background()); err != io.EOF {
		t.Fatalf("EOF after Complete = %v", err)
	}
	if err := response.Stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := response.Stream.Close(); err != nil {
		t.Fatalf("second stream Close: %v", err)
	}
	if err := connector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := connector.Close(context.Background()); err != nil || connector.CloseCount() != 1 {
		t.Fatalf("idempotent Close: err=%v count=%d", err, connector.CloseCount())
	}
}

func TestCloseInterruptsBlockedNextAndCapabilitySnapshot(t *testing.T) {
	gate := make(chan struct{})
	waiting := make(chan struct{}, 1)
	scope := core.CapabilityScope{Protocol: "proto", Mode: "native", Model: "m", AccountID: "a"}
	declared := core.CapabilityResult{Values: map[core.Capability]core.CapabilityState{"llm.streaming": core.Supported}}
	connector := New(testDescriptor(), map[core.CapabilityScope]core.CapabilityResult{scope: declared}, Script{ID: "close", Steps: []Step{{Frame: core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: "proto"}}, Gate: gate, Waiting: waiting}}})
	descriptor := connector.Descriptor()
	if descriptor.ID != "scripted" || descriptor.Kind != core.ComponentConnector {
		t.Fatalf("descriptor = %#v", descriptor)
	}
	if err := descriptor.Validate(core.ComponentConnector); err != nil {
		t.Fatalf("Descriptor.Validate: %v", err)
	}
	if models, err := connector.Models(context.Background(), core.ModelQuery{}, core.InvocationServices{}); err != nil || models.Supported {
		t.Fatalf("Models = %#v, %v", models, err)
	}
	if estimate, err := connector.EstimateUsage(context.Background(), core.UsageQuery{}, core.InvocationServices{}); err != nil || estimate.Supported {
		t.Fatalf("EstimateUsage = %#v, %v", estimate, err)
	}
	if auth, err := connector.Authenticate(context.Background(), core.AuthRequest{}, core.InvocationServices{}); err != nil || auth.Supported {
		t.Fatalf("Authenticate = %#v, %v", auth, err)
	}
	declared.Values["llm.streaming"] = core.Unsupported
	got := connector.Capabilities(context.Background(), scope)
	if got.State("llm.streaming") != core.Supported {
		t.Fatalf("capability snapshot = %v", got.State("llm.streaming"))
	}
	if connector.Health(context.Background()).State != core.HealthUnavailable {
		t.Fatal("uninitialized connector is not unavailable")
	}
	if _, err := connector.Execute(context.Background(), core.ExecutionRequest{ID: "close"}, core.AttemptScope{}, core.InvocationServices{}); err == nil {
		t.Fatal("Execute before Init accepted")
	}
	if err := connector.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	if connector.Health(context.Background()).State != core.HealthReady {
		t.Fatal("initialized connector is not ready")
	}
	response, gatewayErr := connector.Execute(context.Background(), core.ExecutionRequest{ID: "close"}, core.AttemptScope{}, core.InvocationServices{})
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	finished := make(chan error, 1)
	go func() { _, err := response.Stream.Next(context.Background()); finished <- err }()
	awaitWaiting(t, waiting)
	if err := connector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != context.Canceled {
		t.Fatalf("Close-blocked Next = %v", err)
	}
	if connector.Health(context.Background()).State != core.HealthUnavailable || connector.CloseCount() != 1 {
		t.Fatal("closed connector lifecycle state/count incorrect")
	}
	if err := connector.Init(context.Background(), core.ComponentConfig{}); err == nil {
		t.Fatal("Init after Close accepted")
	}
}

func TestWaitingNotificationSendCanBeCancelledOrClosed(t *testing.T) {
	for _, action := range []string{"cancel", "close"} {
		t.Run(action, func(t *testing.T) {
			waiting := make(chan struct{}, 1)
			waiting <- struct{}{} // A full notification channel makes its send block.
			gate := make(chan struct{})
			connector := newConnector(Script{ID: action, Steps: []Step{{
				Frame: core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: "proto"}},
				Gate:  gate, Waiting: waiting,
			}}})
			if err := connector.Init(context.Background(), core.ComponentConfig{}); err != nil {
				t.Fatal(err)
			}
			response, gatewayErr := connector.Execute(context.Background(), core.ExecutionRequest{ID: action}, core.AttemptScope{}, core.InvocationServices{})
			if gatewayErr != nil {
				t.Fatal(gatewayErr)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started, result := make(chan struct{}), make(chan error, 1)
			go func() {
				close(started)
				_, err := response.Stream.Next(ctx)
				result <- err
			}()
			<-started
			if action == "cancel" {
				cancel()
			} else if err := response.Stream.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if err != context.Canceled {
					t.Fatalf("Next after %s = %v, want context.Canceled", action, err)
				}
			case <-time.After(time.Second):
				t.Fatalf("Next remained blocked after %s", action)
			}
			_ = response.Stream.Close()
			_ = connector.Close(context.Background())
		})
	}
}

type pullResult struct {
	frame core.StreamFrame
	err   error
	done  chan pullResult
}

func pull(stream core.Stream) *pullResult {
	result := &pullResult{}
	done := make(chan pullResult, 1)
	go func() {
		frame, err := stream.Next(context.Background())
		done <- pullResult{frame: frame, err: err}
	}()
	result.done = done
	return result
}

func awaitPull(t *testing.T, result *pullResult) pullResult {
	t.Helper()
	return <-result.done
}

func awaitWaiting(t *testing.T, waiting <-chan struct{}) {
	t.Helper()
	<-waiting
}

func assertPull(t *testing.T, pending *pullResult, wantType core.FrameType, wantBody string) {
	t.Helper()
	got := awaitPull(t, pending)
	if got.err != nil || got.frame.Type != wantType {
		t.Fatalf("frame = %#v, err=%v; want %s", got.frame, got.err, wantType)
	}
	if wantBody != "" && (got.frame.Body == nil || string(got.frame.Body.Data) != wantBody) {
		t.Fatalf("body = %#v; want %q", got.frame.Body, wantBody)
	}
}

func newConnector(scripts ...Script) *Connector { return New(testDescriptor(), nil, scripts...) }

func testDescriptor() core.Descriptor {
	return core.Descriptor{ID: "scripted", Kind: core.ComponentConnector, ImplementationVersion: "test", APIVersions: []core.APIVersion{{Major: 1}}, Protocols: []string{"proto"}, ConnectorType: "local", Operations: []string{"execute"}}
}

func validSteps(body string) []Step {
	return []Step{
		{Frame: core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: "proto"}}},
		{Frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: []byte(body)}}},
		{Frame: core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded}}},
	}
}
