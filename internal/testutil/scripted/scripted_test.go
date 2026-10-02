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
			request := core.ExecutionRequest{ID: id, Payload: core.RawPayload{Protocol: "proto", Body: []byte(id)}}
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
	request := core.ExecutionRequest{ID: "blocked", Payload: core.RawPayload{Protocol: "proto"}}
	response, gatewayErr := connector.Execute(context.Background(), request, core.AttemptScope{}, core.InvocationServices{})
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	if _, duplicateErr := connector.Execute(context.Background(), request, core.AttemptScope{}, core.InvocationServices{}); duplicateErr == nil || connector.CallCount() != 1 {
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
	capabilityScope := core.CapabilityScope{Protocol: "proto", Mode: "native", Model: "m", AccountID: "a"}
	declared := core.CapabilityResult{Values: map[core.Capability]core.CapabilityState{"llm.streaming": core.Supported}}
	connector := New(testDescriptor(), map[core.CapabilityScope]core.CapabilityResult{capabilityScope: declared}, Script{ID: "close", Steps: []Step{{Frame: core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: "proto"}}, Gate: gate, Waiting: waiting}}})
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
	got := connector.Capabilities(context.Background(), capabilityScope)
	if got.State("llm.streaming") != core.Supported {
		t.Fatalf("capability snapshot = %v", got.State("llm.streaming"))
	}
	if connector.Health(context.Background()).State != core.HealthUnavailable {
		t.Fatal("uninitialized connector is not unavailable")
	}
	request := core.ExecutionRequest{ID: "close", Model: "m", Payload: core.RawPayload{Protocol: "proto"}}
	scope := core.AttemptScope{AccountID: "a", Mode: "native"}
	if _, err := connector.Execute(context.Background(), request, scope, core.InvocationServices{}); err == nil {
		t.Fatal("Execute before Init accepted")
	}
	if err := connector.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	if connector.Health(context.Background()).State != core.HealthReady {
		t.Fatal("initialized connector is not ready")
	}
	response, gatewayErr := connector.Execute(context.Background(), request, scope, core.InvocationServices{})
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
			response, gatewayErr := connector.Execute(context.Background(), core.ExecutionRequest{ID: action, Payload: core.RawPayload{Protocol: "proto"}}, core.AttemptScope{}, core.InvocationServices{})
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

func TestFailureAndMalformedScriptsRemainRaw(t *testing.T) {
	preHead := &core.GatewayError{Code: "rejected", Category: core.CategoryUnavailable, Retryable: true, RetryDisposition: core.RetrySafe, Message: "synthetic rejection"}
	disconnect := &core.GatewayError{Code: "disconnect", Category: core.CategoryUnavailable, Retryable: true, RetryDisposition: core.RetryUnsafe, Message: "synthetic disconnect"}
	connector := newConnector(
		Script{ID: "pre", ExecuteError: preHead},
		Script{ID: "unknown-retry", ExecuteError: &core.GatewayError{Code: "uncertain", Category: core.CategoryUnavailable, Message: "synthetic uncertainty"}},
		Script{ID: "reject", Steps: []Step{{Frame: core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: "proto", Error: preHead}}}, {Frame: core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeFailed, Error: preHead}}}}},
		Script{ID: "ambiguous", Steps: []Step{{Frame: core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: "proto"}}}, {Err: disconnect}}},
		Script{ID: "unsafe-rejection", Steps: []Step{{Frame: core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: "proto", Error: disconnect}}}, {Frame: core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeFailed, Error: disconnect}}}}},
		Script{ID: "invalid", Steps: []Step{{Frame: core.StreamFrame{Type: "unknown"}}, {Frame: core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: "wrong"}}}, {Frame: core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded}}}, {Frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: []byte("trailing")}}}}},
	)
	if err := connector.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	response, got := connector.Execute(context.Background(), core.ExecutionRequest{ID: "pre", Payload: core.RawPayload{Protocol: "proto"}}, core.AttemptScope{}, core.InvocationServices{})
	if response.Stream != nil || got == nil || got.RetryDisposition != core.RetrySafe {
		t.Fatalf("pre-Head error normalized: response=%#v error=%#v", response, got)
	}
	response, got = connector.Execute(context.Background(), core.ExecutionRequest{ID: "unknown-retry", Payload: core.RawPayload{Protocol: "proto"}}, core.AttemptScope{}, core.InvocationServices{})
	if response.Stream != nil || got == nil || got.RetryDisposition != "" || got.Retryable {
		t.Fatalf("unknown retry disposition changed: response=%#v error=%#v", response, got)
	}
	response = executeScript(t, connector, "reject")
	for _, want := range []core.FrameType{core.FrameHead, core.FrameComplete} {
		frame, err := response.Stream.Next(context.Background())
		if err != nil || frame.Type != want {
			t.Fatalf("rejection frame = %#v, %v; want %s", frame, err, want)
		}
	}
	response.Stream.Close()
	response = executeScript(t, connector, "unsafe-rejection")
	if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Head == nil || frame.Head.Error.RetryDisposition != core.RetryUnsafe {
		t.Fatalf("unsafe rejection Head = %#v, %v", frame, err)
	}
	response.Stream.Close()
	response = executeScript(t, connector, "ambiguous")
	if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
		t.Fatalf("ambiguous Head = %#v, %v", frame, err)
	}
	if _, err := response.Stream.Next(context.Background()); err != disconnect {
		t.Fatalf("post-Head disconnect = %v, want exact error", err)
	}
	response.Stream.Close()
	response = executeScript(t, connector, "invalid")
	for _, want := range []core.FrameType{"unknown", core.FrameHead, core.FrameComplete, core.FrameBody} {
		frame, err := response.Stream.Next(context.Background())
		if err != nil || frame.Type != want {
			t.Fatalf("raw frame = %#v, %v; want %s", frame, err, want)
		}
	}
	if _, err := response.Stream.Next(context.Background()); err != io.EOF {
		t.Fatalf("after raw trailing frame = %v", err)
	}
	response.Stream.Close()
}

func TestMalformedSequencesAreReturnedExactly(t *testing.T) {
	rejected := &core.GatewayError{Code: "rejected", Category: core.CategoryInvalidRequest, Message: "synthetic rejection"}
	frame := func(typ core.FrameType, head *core.HeadFrame, body *core.BodyFrame, complete *core.CompleteFrame) core.StreamFrame {
		return core.StreamFrame{Type: typ, Head: head, Body: body, Complete: complete}
	}
	head := frame(core.FrameHead, &core.HeadFrame{Protocol: "proto"}, nil, nil)
	body := frame(core.FrameBody, nil, &core.BodyFrame{Data: []byte{0, 0xff}}, nil)
	complete := func(outcome core.Outcome, err *core.GatewayError) core.StreamFrame {
		return frame(core.FrameComplete, nil, nil, &core.CompleteFrame{Outcome: outcome, Error: err})
	}
	cases := []struct {
		name  string
		steps []Step
		want  []core.StreamFrame
	}{
		{name: "duplicate Head", steps: []Step{{Frame: head}, {Frame: head}}, want: []core.StreamFrame{head, head}},
		{name: "duplicate Complete", steps: []Step{{Frame: head}, {Frame: complete(core.OutcomeSucceeded, nil)}, {Frame: complete(core.OutcomeSucceeded, nil)}}, want: []core.StreamFrame{head, complete(core.OutcomeSucceeded, nil), complete(core.OutcomeSucceeded, nil)}},
		{name: "Body first", steps: []Step{{Frame: body}}, want: []core.StreamFrame{body}},
		{name: "Head without Complete", steps: []Step{{Frame: head}}, want: []core.StreamFrame{head}},
		{name: "Succeeded with Error", steps: []Step{{Frame: head}, {Frame: complete(core.OutcomeSucceeded, rejected)}}, want: []core.StreamFrame{head, complete(core.OutcomeSucceeded, rejected)}},
		{name: "Failed without Error", steps: []Step{{Frame: head}, {Frame: complete(core.OutcomeFailed, nil)}}, want: []core.StreamFrame{head, complete(core.OutcomeFailed, nil)}},
		{name: "rejected body", steps: []Step{{Frame: frame(core.FrameHead, &core.HeadFrame{Protocol: "proto", Error: rejected}, nil, nil)}, {Frame: body}, {Frame: complete(core.OutcomeFailed, rejected)}}, want: []core.StreamFrame{frame(core.FrameHead, &core.HeadFrame{Protocol: "proto", Error: rejected}, nil, nil), body, complete(core.OutcomeFailed, rejected)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			connector := newConnector(Script{ID: "raw", Steps: tc.steps})
			if err := connector.Init(context.Background(), core.ComponentConfig{}); err != nil {
				t.Fatal(err)
			}
			response := executeScript(t, connector, "raw")
			for i, want := range tc.want {
				got, err := response.Stream.Next(context.Background())
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("frame %d = %#v, %v; want exact %#v", i, got, err, want)
				}
			}
			if got, err := response.Stream.Next(context.Background()); got != (core.StreamFrame{}) || err != io.EOF {
				t.Fatalf("after raw sequence = %#v, %v; want zero frame, EOF", got, err)
			}
			response.Stream.Close()
		})
	}
}

func TestCloseTerminatesGatedStepError(t *testing.T) {
	gate, waiting := make(chan struct{}), make(chan struct{}, 1)
	injected := &core.GatewayError{Code: "disconnect", Category: core.CategoryUnavailable, Message: "synthetic disconnect"}
	connector := newConnector(Script{ID: "trailing", Steps: []Step{
		{Frame: core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: "proto"}}},
		{Frame: core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded}}},
		{Err: injected, Gate: gate, Waiting: waiting},
	}})
	if err := connector.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	response := executeScript(t, connector, "trailing")
	for range 2 {
		if _, err := response.Stream.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { _, err := response.Stream.Next(context.Background()); done <- err }()
	awaitWaiting(t, waiting)
	if err := response.Stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != context.Canceled {
		t.Fatalf("gated error Next after Close = %v", err)
	}
}

func executeScript(t *testing.T, connector *Connector, id string) core.ExecutionResponse {
	t.Helper()
	response, err := connector.Execute(context.Background(), core.ExecutionRequest{ID: id, Payload: core.RawPayload{Protocol: "proto"}}, core.AttemptScope{}, core.InvocationServices{})
	if err != nil {
		t.Fatal(err)
	}
	return response
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
