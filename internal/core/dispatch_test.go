package core

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type targetFunc func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError)

type headThenBlock struct {
	*scriptedStream
	headSent bool
}

// gatedFrame holds a produced frame between Next and the attempt handoff.
type gatedFrame struct {
	frame   StreamFrame
	entered chan struct{}
	release chan struct{}
	closed  atomic.Int32
}

func (s *gatedFrame) Next(context.Context) (StreamFrame, error) {
	close(s.entered)
	<-s.release
	if s.frame.Type == "" {
		return StreamFrame{}, io.EOF
	}
	return s.frame, nil
}

func (s *gatedFrame) Close() error { s.closed.Add(1); return nil }

func (s *headThenBlock) Next(ctx context.Context) (StreamFrame, error) {
	if !s.headSent {
		s.headSent = true
		return head(), nil
	}
	return s.scriptedStream.Next(ctx)
}

func (f targetFunc) Execute(ctx context.Context, r ExecutionRequest, s AttemptScope) (ExecutionResponse, *GatewayError) {
	return f(ctx, r, s)
}

func request() ExecutionRequest {
	return ExecutionRequest{ID: "client-id", Model: m1Model, Payload: RawPayload{Protocol: m1Protocol, Body: []byte("\x00opaque\xff")}, Metadata: RequestMetadata{Extensions: map[string]any{"account": "intruder", "attempt_id": "client-id"}}}
}

func TestDispatchEligibilityAndIdentity(t *testing.T) {
	var calls, finals int
	d := &Dispatcher{AccountID: "selected", Adapter: map[Capability]CapabilityState{"llm.tools": Supported}, Connector: map[Capability]CapabilityState{"llm.tools": Supported}}
	d.Finalize = func(AttemptResult) { finals++ }
	d.Target = targetFunc(func(_ context.Context, r ExecutionRequest, s AttemptScope) (ExecutionResponse, *GatewayError) {
		calls++
		if r.ID == "" || r.ID == "client-id" || s.ID == "" || s.ID == r.ID || s.AccountID != "selected" || s.Mode != "native" || string(r.Payload.Body) != "\x00opaque\xff" {
			t.Errorf("untrusted identity/scope or mutated bytes: %+v %+v", r, s)
		}
		return ExecutionResponse{Stream: &scriptedStream{frames: []StreamFrame{head(), complete()}}}, nil
	})
	for _, mutate := range []func(*ExecutionRequest){
		func(r *ExecutionRequest) { r.Payload.Protocol = "other" },
		func(r *ExecutionRequest) { r.Capabilities = map[Capability]struct{}{"llm.reasoning": {}} },
		func(r *ExecutionRequest) { r.Capabilities = map[Capability]struct{}{"llm.tools.parallel": {}} },
	} {
		r := request()
		mutate(&r)
		if _, err := d.Execute(context.Background(), r); err == nil || err.Category != CategoryUnsupportedFeature {
			t.Fatalf("expected pre-execution rejection: %v", err)
		}
	}
	for _, model := range []string{"", "other", "gpt-4.1-mini-2025-04-14"} {
		r := request()
		r.Model = model
		if _, err := d.Execute(context.Background(), r); err == nil || err.Category != CategoryInvalidRequest {
			t.Fatalf("model %q: expected invalid-request rejection: %v", model, err)
		}
	}
	d.Mode = "translation"
	if _, err := d.Execute(context.Background(), request()); err == nil || err.Category != CategoryUnsupportedFeature {
		t.Fatalf("expected native-mode rejection: %v", err)
	}
	d.Mode = "native"
	r := request()
	r.Capabilities = map[Capability]struct{}{"llm.tools": {}}
	resp, err := d.Execute(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, e := resp.Stream.Next(context.Background()); e != nil {
			t.Fatal(e)
		}
	}
	if calls != 1 || finals != 1 {
		t.Fatalf("calls %d finalizations %d", calls, finals)
	}
}

func TestDispatchTerminalPaths(t *testing.T) {
	pre := &GatewayError{Code: "reject", Category: CategoryInvalidRequest, Message: "rejected"}
	partial := &UsageReport{InputTokens: new(int64), Source: UsageProvider, Completeness: UsagePartial}
	for _, tc := range []struct {
		name      string
		frames    []StreamFrame
		err       error
		want      Outcome
		committed bool
		usage     *UsageReport
	}{
		{"success", []StreamFrame{head(), body(), {Type: FrameComplete, Complete: &CompleteFrame{Outcome: OutcomeSucceeded, Usage: partial}}}, nil, OutcomeSucceeded, true, partial},
		{"pre-head error", nil, pre, OutcomeFailed, false, nil},
		{"unexpected EOF", []StreamFrame{head(), body()}, nil, OutcomeIncomplete, true, nil},
		{"invalid ordering", []StreamFrame{body()}, nil, OutcomeIncomplete, false, nil},
		{"post-head error", []StreamFrame{head()}, errors.New("drop"), OutcomeIncomplete, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var results []AttemptResult
			p := &scriptedStream{frames: tc.frames, err: tc.err}
			d := &Dispatcher{AccountID: "selected", Finalize: func(r AttemptResult) { results = append(results, r) }, Target: targetFunc(func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError) {
				return ExecutionResponse{Stream: p}, nil
			})}
			resp, ge := d.Execute(context.Background(), request())
			if ge != nil {
				t.Fatal(ge)
			}
			for range len(tc.frames) + 1 {
				_, err := resp.Stream.Next(context.Background())
				if err != nil {
					if err == io.EOF && tc.want != OutcomeSucceeded {
						t.Fatal("premature EOF treated as success")
					}
					break
				}
			}
			resp.Stream.Close()
			if len(results) != 1 || results[0].Outcome != tc.want || results[0].Committed != tc.committed || p.closed.Load() != 1 {
				t.Fatalf("results %+v, closes %d", results, p.closed.Load())
			}
			if tc.usage != nil && (results[0].Usage.InputTokens == nil || results[0].Usage.Completeness != UsagePartial) {
				t.Fatalf("lost partial usage: %+v", results[0].Usage)
			}
			if tc.usage == nil && results[0].Usage.Source != UsageUnknown {
				t.Fatalf("unknown usage: %+v", results[0].Usage)
			}
		})
	}
}

func TestDispatchCancelAndPreExecuteFailure(t *testing.T) {
	var mu sync.Mutex
	var results []AttemptResult
	p := &scriptedStream{block: true, entered: make(chan struct{}), release: make(chan struct{})}
	d := &Dispatcher{AccountID: "selected", Finalize: func(r AttemptResult) { mu.Lock(); results = append(results, r); mu.Unlock() }}
	d.Target = targetFunc(func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError) {
		return ExecutionResponse{Stream: p}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	resp, ge := d.Execute(ctx, request())
	if ge != nil {
		t.Fatal(ge)
	}
	done := make(chan error, 1)
	go func() { _, err := resp.Stream.Next(context.Background()); done <- err }()
	<-p.entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel returned %v", err)
	}
	resp.Stream.Close()
	mu.Lock()
	if len(results) != 1 || results[0].Outcome != OutcomeCancelled || results[0].Committed || p.closed.Load() != 1 {
		t.Fatalf("cancel result %+v, closes %d", results, p.closed.Load())
	}
	mu.Unlock()
	d.Target = targetFunc(func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError) {
		return ExecutionResponse{}, &GatewayError{Code: "pre", Message: "pre"}
	})
	if _, ge := d.Execute(context.Background(), request()); ge == nil || ge.Code != "pre" {
		t.Fatalf("pre-execution failure: %v", ge)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(results) != 2 || results[1].Outcome != OutcomeFailed || results[1].Committed {
		t.Fatalf("pre-execution finalization: %+v", results)
	}
}

func TestDispatchPreHeadCancellationFinalizesCancelled(t *testing.T) {
	var results []AttemptResult
	d := &Dispatcher{AccountID: "selected", Finalize: func(r AttemptResult) { results = append(results, r) }, Target: targetFunc(func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError) {
		return ExecutionResponse{}, &GatewayError{Code: "upstream_cancelled", Category: CategoryCancelled, Message: "cancelled"}
	})}
	if _, err := d.Execute(context.Background(), request()); err == nil || err.Category != CategoryCancelled {
		t.Fatalf("pre-head cancellation: %v", err)
	}
	if len(results) != 1 || results[0].Outcome != OutcomeCancelled || results[0].Committed || results[0].Error.Category != CategoryCancelled {
		t.Fatalf("pre-head cancellation finalization: %+v", results)
	}
}

func TestDispatchNilStreamFailsBeforeHead(t *testing.T) {
	var results []AttemptResult
	d := &Dispatcher{AccountID: "selected", Finalize: func(r AttemptResult) { results = append(results, r) }, Target: targetFunc(func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError) {
		return ExecutionResponse{}, nil
	})}
	resp, ge := d.Execute(context.Background(), request())
	if ge == nil || ge.Category != CategoryInternal || resp.Stream != nil || len(results) != 1 || results[0].Outcome != OutcomeFailed || results[0].Committed || results[0].Error != ge || results[0].Usage.Source != UsageUnknown {
		t.Fatalf("nil stream result: %+v, %v, %+v", resp, ge, results)
	}
}

func TestDispatchCloseAfterHead(t *testing.T) {
	p := &scriptedStream{frames: []StreamFrame{head(), body(), complete()}}
	var results []AttemptResult
	d := &Dispatcher{AccountID: "selected", Finalize: func(r AttemptResult) { results = append(results, r) }, Target: targetFunc(func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError) {
		return ExecutionResponse{Stream: p}, nil
	})}
	resp, ge := d.Execute(context.Background(), request())
	if ge != nil {
		t.Fatal(ge)
	}
	if f, err := resp.Stream.Next(context.Background()); err != nil || f.Type != FrameHead {
		t.Fatalf("head: %+v %v", f, err)
	}
	resp.Stream.Close()
	resp.Stream.Close()
	if len(results) != 1 || !results[0].Committed || results[0].Outcome != OutcomeCancelled || results[0].Usage.Source != UsageUnknown || p.closed.Load() != 1 {
		t.Fatalf("abandoned stream: %+v closes %d", results, p.closed.Load())
	}
}

func TestDispatchCloseRaceWithFrameHandoff(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame StreamFrame
		prior bool
	}{
		{"head", head(), false},
		{"complete", complete(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &gatedFrame{frame: tc.frame, entered: make(chan struct{}), release: make(chan struct{})}
			var mu sync.Mutex
			var results []AttemptResult
			s := &attemptStream{source: p, ctx: context.Background(), result: AttemptResult{Usage: UsageReport{Source: UsageUnknown}}, finish: func(r AttemptResult) {
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}}
			if tc.prior && !s.handoff(true) {
				t.Fatal("initial Head was not handed off")
			}
			done := make(chan error, 1)
			go func() { _, err := s.Next(context.Background()); done <- err }()
			<-p.entered
			// Finalize while the produced frame is held before Head/Complete handoff.
			s.Close()
			close(p.release)
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("frame escaped finalized attempt: %v", err)
			}
			s.Close()
			mu.Lock()
			defer mu.Unlock()
			if len(results) != 1 || results[0].Committed != tc.prior || results[0].Outcome != OutcomeCancelled || p.closed.Load() != 1 {
				t.Fatalf("terminal observation %+v, closes %d", results, p.closed.Load())
			}
		})
	}
}

func TestDispatchFinalizerMayClose(t *testing.T) {
	p := &scriptedStream{frames: []StreamFrame{head()}}
	s := &attemptStream{source: p, ctx: context.Background(), finish: func(AttemptResult) {}}
	var calls int
	s.finish = func(AttemptResult) { calls++; s.Close() }
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("finalization deadlocked on reentrant Close")
	}
	if calls != 1 || p.closed.Load() != 1 {
		t.Fatalf("callbacks %d, closes %d", calls, p.closed.Load())
	}
}

func TestDispatchPostHeadCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &headThenBlock{scriptedStream: &scriptedStream{block: true, entered: make(chan struct{}), release: make(chan struct{})}}
	var mu sync.Mutex
	var results []AttemptResult
	d := &Dispatcher{AccountID: "selected", Finalize: func(r AttemptResult) { mu.Lock(); results = append(results, r); mu.Unlock() }, Target: targetFunc(func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError) {
		return ExecutionResponse{Stream: p}, nil
	})}
	response, ge := d.Execute(ctx, request())
	if ge != nil {
		t.Fatal(ge)
	}
	if f, err := response.Stream.Next(ctx); err != nil || f.Type != FrameHead {
		t.Fatalf("head: %+v, %v", f, err)
	}
	done := make(chan error, 1)
	go func() { _, err := response.Stream.Next(ctx); done <- err }()
	<-p.entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	response.Stream.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(results) != 1 || results[0].Outcome != OutcomeCancelled || !results[0].Committed || p.closed.Load() != 1 {
		t.Fatalf("post-head cancellation: %+v, closes %d", results, p.closed.Load())
	}
}

func TestDispatchConcurrentIsolationAndCancellation(t *testing.T) {
	type attempt struct {
		request ExecutionRequest
		scope   AttemptScope
		stream  *scriptedStream
	}
	var mu sync.Mutex
	var attempts []attempt
	var results []AttemptResult
	finalized := make(chan AttemptResult, 2)
	d := &Dispatcher{AccountID: "selected", Finalize: func(r AttemptResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
		finalized <- r
	}}
	d.Target = targetFunc(func(_ context.Context, r ExecutionRequest, scope AttemptScope) (ExecutionResponse, *GatewayError) {
		p := &scriptedStream{frames: []StreamFrame{head(), {Type: FrameBody, Body: &BodyFrame{Data: append([]byte(nil), r.Payload.Body...)}}, complete()}}
		if string(r.Payload.Body) == "cancelled opaque bytes" {
			p = &scriptedStream{block: true, entered: make(chan struct{}), release: make(chan struct{})}
		}
		mu.Lock()
		attempts = append(attempts, attempt{r, scope, p})
		mu.Unlock()
		return ExecutionResponse{Stream: p}, nil
	})

	cancelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inputs := []struct {
		ctx  context.Context
		body string
	}{{cancelCtx, "cancelled opaque bytes"}, {context.Background(), "successful opaque bytes"}}
	responses := make([]ExecutionResponse, len(inputs))
	for i, input := range inputs {
		r := request()
		r.Payload.Body = []byte(input.body)
		resp, err := d.Execute(input.ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		responses[i] = resp
	}
	cancelledNext := make(chan struct{})
	go func() {
		_, _ = responses[0].Stream.Next(context.Background())
		close(cancelledNext)
	}()
	<-attempts[0].stream.entered
	cancel()
	select {
	case <-cancelledNext:
	case <-time.After(time.Second):
		t.Fatal("cancelled stream did not stop")
	}
	select {
	case <-finalized:
	case <-time.After(time.Second):
		t.Fatal("cancelled attempt was not finalized before success")
	}
	var successBody string
	for range 3 {
		frame, err := responses[1].Stream.Next(context.Background())
		if err != nil {
			t.Fatalf("unrelated successful stream: %v", err)
		}
		if frame.Type == FrameBody {
			successBody = string(frame.Body.Data)
		}
	}
	if successBody != "successful opaque bytes" {
		t.Fatalf("successful payload crossed requests: %q", successBody)
	}
	select {
	case <-finalized:
	case <-time.After(time.Second):
		t.Fatal("successful attempt was not finalized")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 2 || attempts[0].request.ID == attempts[1].request.ID || attempts[0].scope.ID == attempts[1].scope.ID {
		t.Fatalf("request/attempt IDs not isolated: %+v", attempts)
	}
	if len(results) != 2 {
		t.Fatalf("finalizations=%d: %+v", len(results), results)
	}
	byID := map[string]AttemptResult{}
	for _, result := range results {
		byID[result.RequestID] = result
	}
	for _, a := range attempts {
		result, ok := byID[a.request.ID]
		if !ok || result.Scope.ID != a.scope.ID {
			t.Fatalf("finalization crossed attempts: %+v", results)
		}
		if string(a.request.Payload.Body) == "cancelled opaque bytes" {
			if result.Outcome != OutcomeCancelled || result.Usage.Source != UsageUnknown {
				t.Fatalf("cancelled request result: %+v", result)
			}
		} else if result.Outcome != OutcomeSucceeded {
			t.Fatalf("successful request corrupted: %+v", result)
		}
	}
}

func TestDispatchConcurrentTerminalSignals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame StreamFrame
	}{
		{"complete", complete()},
		{"EOF", StreamFrame{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &gatedFrame{frame: tc.frame, entered: make(chan struct{}), release: make(chan struct{})}
			ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
			defer cancel()
			var mu sync.Mutex
			var results []AttemptResult
			finalized := make(chan struct{}, 1)
			d := &Dispatcher{AccountID: "selected", Finalize: func(r AttemptResult) {
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
				finalized <- struct{}{}
			}, Target: targetFunc(func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError) {
				return ExecutionResponse{Stream: p}, nil
			})}
			resp, err := d.Execute(ctx, request())
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := resp.Stream.Next(context.Background()); done <- err }()
			<-p.entered
			<-ctx.Done()
			start := make(chan struct{})
			var signals sync.WaitGroup
			signals.Add(2)
			go func() { defer signals.Done(); <-start; _ = resp.Stream.Close() }()
			go func() { defer signals.Done(); <-start; close(p.release) }()
			close(start)
			signals.Wait()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("competing terminal signals did not settle")
			}
			_ = resp.Stream.Close()
			select {
			case <-finalized:
			case <-time.After(time.Second):
				t.Fatal("attempt was not finalized")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(results) != 1 || p.closed.Load() != 1 || results[0].Usage.Source != UsageUnknown {
				t.Fatalf("terminal signals finalized %d times, closed %d: %+v", len(results), p.closed.Load(), results)
			}
			if tc.name == "EOF" && results[0].Outcome == OutcomeSucceeded {
				t.Fatalf("EOF incorrectly finalized success: %+v", results[0])
			}
		})
	}
}
