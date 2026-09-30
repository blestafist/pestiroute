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
		func(r *ExecutionRequest) { r.Model = "other" },
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
