package core

import (
	"context"
	"errors"
	"fmt"
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

type trailingStream struct {
	frames  []StreamFrame
	err     error
	gate    <-chan struct{}
	waiting chan<- struct{}
	closed  chan struct{}
	index   int
	once    sync.Once
}

type slowCloseAfterComplete struct {
	frames       []StreamFrame
	index        int
	closeStarted chan struct{}
	allowClose   chan struct{}
	closeOnce    sync.Once
}

func (s *slowCloseAfterComplete) Next(context.Context) (StreamFrame, error) {
	if s.index < len(s.frames) {
		frame := s.frames[s.index]
		s.index++
		return frame, nil
	}
	<-s.closeStarted
	return StreamFrame{}, io.EOF
}

func (s *slowCloseAfterComplete) Close() error {
	s.closeOnce.Do(func() { close(s.closeStarted) })
	<-s.allowClose
	return nil
}

func (s *trailingStream) Next(ctx context.Context) (StreamFrame, error) {
	if s.index < len(s.frames) {
		frame := s.frames[s.index]
		s.index++
		return frame, nil
	}
	if s.waiting != nil {
		s.waiting <- struct{}{}
		s.waiting = nil
	}
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-s.closed:
			return StreamFrame{}, context.Canceled
		case <-ctx.Done():
			return StreamFrame{}, ctx.Err()
		}
	}
	if s.err != nil {
		return StreamFrame{}, s.err
	}
	return StreamFrame{}, io.EOF
}

func (s *trailingStream) Close() error { s.once.Do(func() { close(s.closed) }); return nil }

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

type testAdapter struct {
	registryComponent
	caps map[CapabilityScope]CapabilityResult
}

func (a *testAdapter) Capabilities(_ context.Context, scope CapabilityScope) CapabilityResult {
	return a.caps[scope].Clone()
}

type testServices struct{ scopes []AttemptScope }

type attemptCredential struct{ account string }

func (a attemptCredential) Get(context.Context, string) ([]byte, error) {
	return []byte(a.account + "-secret"), nil
}

func (s *testServices) ForAttempt(scope AttemptScope) InvocationServices {
	s.scopes = append(s.scopes, scope)
	return InvocationServices{Credentials: attemptCredential{account: scope.AccountID}}
}

type dispatchConnector struct {
	registryComponent
	caps  map[CapabilityScope]CapabilityResult
	calls []ExecutionRequest
	creds []string
}

func (c *dispatchConnector) Capabilities(_ context.Context, scope CapabilityScope) CapabilityResult {
	return c.caps[scope].Clone()
}

func (c *dispatchConnector) Execute(ctx context.Context, in ExecutionRequest, _ AttemptScope, services InvocationServices) (ExecutionResponse, *GatewayError) {
	c.calls = append(c.calls, in)
	credential, err := services.Credentials.Get(ctx, "token")
	if err != nil {
		return ExecutionResponse{}, &GatewayError{Code: "credential_missing", Category: CategoryInternal, Message: "Credential missing"}
	}
	c.creds = append(c.creds, string(credential))
	return ExecutionResponse{Stream: &scriptedStream{frames: []StreamFrame{head(), complete()}}}, nil
}

func (*dispatchConnector) Models(context.Context, ModelQuery, InvocationServices) (ModelsResult, *GatewayError) {
	return ModelsResult{}, nil
}
func (*dispatchConnector) EstimateUsage(context.Context, UsageQuery, InvocationServices) (EstimateResult, *GatewayError) {
	return EstimateResult{}, nil
}
func (*dispatchConnector) Authenticate(context.Context, AuthRequest, InvocationServices) (AuthResult, *GatewayError) {
	return AuthResult{}, nil
}

func TestDispatchRouteRegistryEligibilityAndScopedServices(t *testing.T) {
	ctx := context.Background()
	protocol := "openai.responses.v1"
	capability := Capability("vendor.required")
	registry, err := NewRegistry(map[ComponentKind]APIVersion{ComponentAdapter: {Major: 1}, ComponentConnector: {Major: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	scopeA := CapabilityScope{Protocol: protocol, Mode: ModeNative, Model: "model-a", AccountID: "account-a"}
	scopeB := CapabilityScope{Protocol: protocol, Mode: ModeNative, Model: "model-b", AccountID: "account-a"}
	caps := func(scope CapabilityScope, supported bool) map[CapabilityScope]CapabilityResult {
		state := Unsupported
		if supported {
			state = Supported
		}
		return map[CapabilityScope]CapabilityResult{scope: {Values: map[Capability]CapabilityState{capability: state}}}
	}
	adapter := &testAdapter{registryComponent: registryComponent{descriptor: validDescriptor(ComponentAdapter)}, caps: caps(scopeA, true)}
	adapter.caps[scopeB] = caps(scopeB, true)[scopeB]
	var connectors [2]*dispatchConnector
	for i, scope := range []CapabilityScope{scopeA, scopeB} {
		descriptor := validDescriptor(ComponentConnector)
		descriptor.ID = "scripted-" + scope.Model
		connectors[i] = &dispatchConnector{registryComponent: registryComponent{descriptor: descriptor}, caps: caps(scope, true)}
	}
	if err := registry.Register("adapter", adapter, ComponentAdapter); err != nil {
		t.Fatal(err)
	}
	for i, connector := range connectors {
		if err := registry.Register(InstanceID("connector-"+string(rune('a'+i))), connector, ComponentConnector); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []InstanceID{"adapter", "connector-a", "connector-b"} {
		if err := registry.Init(ctx, id, ComponentConfig{}); err != nil {
			t.Fatal(err)
		}
	}
	routes, err := NewRouteTable([]Route{
		{Identity: RouteIdentity{RouteLookupKey: RouteLookupKey{Protocol: protocol, Mode: ModeNative, Model: "model-a"}, AccountID: "account-a"}, Adapter: "adapter", Connector: "connector-a"},
		{Identity: RouteIdentity{RouteLookupKey: RouteLookupKey{Protocol: protocol, Mode: ModeNative, Model: "model-b"}, AccountID: "account-a"}, Adapter: "adapter", Connector: "connector-b"},
	}, registry)
	if err != nil {
		t.Fatal(err)
	}
	services := &testServices{}
	observations, err := NewInMemoryAttemptObservations(4)
	if err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{Routes: routes, AccountID: "account-a", Services: services, Observations: observations}
	for i, model := range []string{"model-a", "model-b"} {
		r := request()
		r.Model = model
		r.Capabilities = map[Capability]struct{}{capability: {}}
		r.Metadata.Extensions["account"] = "attacker-account"
		response, gatewayErr := d.Execute(ctx, r)
		if gatewayErr != nil {
			t.Fatalf("%s: %v", model, gatewayErr)
		}
		for range 2 {
			if _, err := response.Stream.Next(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := response.Stream.Next(ctx); err != io.EOF {
			t.Fatalf("terminal read: %v", err)
		}
		if len(connectors[i].calls) != 1 || connectors[i].calls[0].ID == "client-id" {
			t.Fatalf("request identity was not runtime-owned: %+v", connectors[i].calls)
		}
		if len(connectors[i].creds) != 1 || connectors[i].creds[0] != "account-a-secret" {
			t.Fatalf("untrusted metadata selected credentials: %v", connectors[i].creds)
		}
	}
	if len(connectors[0].calls) != 1 || len(connectors[1].calls) != 1 || len(services.scopes) != 2 || services.scopes[0].ID == "" || services.scopes[0].AccountID != "account-a" || services.scopes[0].Mode != ModeNative {
		t.Fatal("both routed implementations must execute once with scoped services")
	}
	gotObservations := observations.Snapshot()
	if len(gotObservations) != 2 || gotObservations[0].Route.Model != "model-a" || gotObservations[0].Adapter != "adapter" || gotObservations[0].Connector != "connector-a" || gotObservations[0].AccountID != "account-a" || gotObservations[1].Route.Model != "model-b" || gotObservations[1].Connector != "connector-b" {
		t.Fatalf("route/instance identities missing from observations: %+v", gotObservations)
	}
	r := request()
	r.Model = "model-b"
	r.Capabilities = map[Capability]struct{}{capability: {}, "vendor.unknown": {}}
	if _, err := d.Execute(ctx, r); err == nil {
		t.Fatal("ineligible capability executed")
	}
	if len(connectors[1].calls) != 1 || len(services.scopes) != 2 {
		t.Fatal("ineligible attempt reached Execute or services")
	}
	if err := registry.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchEligibilityAndIdentity(t *testing.T) {
	var calls, finals int
	d := &Dispatcher{AccountID: "selected", Adapter: map[Capability]CapabilityState{"llm.tools": Supported, "llm.reasoning": Supported}, Connector: map[Capability]CapabilityState{"llm.tools": Supported, "llm.reasoning": Supported}}
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
	if _, err := resp.Stream.Next(context.Background()); err != io.EOF {
		t.Fatalf("terminal read: %v", err)
	}
	if calls != 1 || finals != 1 {
		t.Fatalf("calls %d finalizations %d", calls, finals)
	}
	r = request()
	r.Capabilities = map[Capability]struct{}{"llm.reasoning": {}}
	resp, err = d.Execute(context.Background(), r)
	if err != nil {
		t.Fatalf("supported reasoning rejected: %v", err)
	}
	for range 2 {
		if _, e := resp.Stream.Next(context.Background()); e != nil {
			t.Fatal(e)
		}
	}
	if _, err := resp.Stream.Next(context.Background()); err != io.EOF {
		t.Fatalf("terminal read: %v", err)
	}
	d.Connector["llm.reasoning"] = Unknown
	if _, err = d.Execute(context.Background(), r); err == nil || err.Code != "unsupported_capability" {
		t.Fatalf("reasoning without connector support: %v", err)
	}
	if calls != 2 || finals != 2 {
		t.Fatalf("rejected request reached target/finalized: calls=%d finals=%d", calls, finals)
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

func TestAttemptStreamWaitsForTerminalEOF(t *testing.T) {
	usageTokens := int64(19)
	usage := &UsageReport{InputTokens: &usageTokens, Source: UsageProvider, Completeness: UsagePartial}
	for _, tc := range []struct {
		name     string
		trailing []StreamFrame
		err      error
		outcome  Outcome
	}{
		{name: "clean EOF", outcome: OutcomeSucceeded},
		{name: "body after complete", trailing: []StreamFrame{body()}, outcome: OutcomeFailed},
		{name: "duplicate complete", trailing: []StreamFrame{complete()}, outcome: OutcomeFailed},
		{name: "unknown frame", trailing: []StreamFrame{{Type: FrameType("unknown")}}, outcome: OutcomeFailed},
		{name: "stream error", err: errors.New("upstream detail"), outcome: OutcomeFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &trailingStream{frames: []StreamFrame{head(), {Type: FrameComplete, Complete: &CompleteFrame{Outcome: OutcomeSucceeded, Usage: usage}}}, err: tc.err, closed: make(chan struct{})}
			p.frames = append(p.frames, tc.trailing...)
			var results []AttemptResult
			d := &Dispatcher{AccountID: "selected", Finalize: func(r AttemptResult) { results = append(results, r) }, Target: targetFunc(func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError) {
				return ExecutionResponse{Stream: p}, nil
			})}
			response, gatewayErr := d.Execute(context.Background(), request())
			if gatewayErr != nil {
				t.Fatal(gatewayErr)
			}
			if _, err := response.Stream.Next(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := response.Stream.Next(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(results) != 0 {
				t.Fatalf("published before terminal EOF: %+v", results)
			}
			if _, err := response.Stream.Next(context.Background()); tc.outcome == OutcomeSucceeded && err != io.EOF {
				t.Fatalf("terminal read: %v", err)
			} else if tc.outcome != OutcomeSucceeded && err == nil {
				t.Fatal("trailing stream was not rejected")
			}
			if len(results) != 1 || results[0].Outcome != tc.outcome || tc.outcome != OutcomeSucceeded && (results[0].Error == nil || results[0].Error.Code != "execution_failed") || !results[0].HasUsage || results[0].Usage.InputTokens == nil || *results[0].Usage.InputTokens != usageTokens {
				t.Fatalf("terminal result: %+v", results)
			}
			select {
			case <-p.closed:
			default:
				t.Fatal("source was not closed")
			}
			if _, err := response.Stream.Next(context.Background()); err == nil {
				t.Fatal("stream remained open after terminal result")
			}
			if len(results) != 1 {
				t.Fatalf("duplicate publication: %+v", results)
			}
		})
	}
}

func TestAttemptStreamCancellationWhileAwaitingEOF(t *testing.T) {
	entered, gate := make(chan struct{}, 1), make(chan struct{})
	inputTokens := int64(3)
	p := &trailingStream{frames: []StreamFrame{head(), {Type: FrameComplete, Complete: &CompleteFrame{Outcome: OutcomeSucceeded, Usage: &UsageReport{InputTokens: &inputTokens, Source: UsageProvider, Completeness: UsagePartial}}}}, gate: gate, waiting: entered, closed: make(chan struct{})}
	var results []AttemptResult
	finalized := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	d := &Dispatcher{AccountID: "selected", Finalize: func(r AttemptResult) { results = append(results, r); finalized <- struct{}{} }, Target: targetFunc(func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError) {
		return ExecutionResponse{Stream: p}, nil
	})}
	response, gatewayErr := d.Execute(ctx, request())
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	for range 2 {
		if _, err := response.Stream.Next(ctx); err != nil {
			t.Fatal(err)
		}
	}
	finished := make(chan error, 1)
	go func() { _, err := response.Stream.Next(ctx); finished <- err }()
	<-entered
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled terminal read: %v", err)
	}
	select {
	case <-finalized:
	case <-time.After(time.Second):
		t.Fatal("cancelled attempt was not finalized")
	}
	if len(results) != 1 {
		t.Fatalf("terminal publication count: %d", len(results))
	}
	if results[0].Outcome != OutcomeCancelled {
		t.Fatalf("cancelled pending outcome: %q", results[0].Outcome)
	}
	if !results[0].HasUsage || results[0].Usage.InputTokens == nil || *results[0].Usage.InputTokens != inputTokens {
		t.Fatalf("cancelled pending usage: has=%v report=%+v", results[0].HasUsage, results[0].Usage)
	}
	select {
	case <-p.closed:
	default:
		t.Fatal("cancelled source was not closed")
	}
}

func TestAttemptStreamDeadlineWhileAwaitingEOF(t *testing.T) {
	entered, gate := make(chan struct{}, 1), make(chan struct{})
	p := &trailingStream{frames: []StreamFrame{head(), complete()}, gate: gate, waiting: entered, closed: make(chan struct{})}
	var results []AttemptResult
	finalized := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	d := &Dispatcher{AccountID: "selected", Finalize: func(r AttemptResult) { results = append(results, r); finalized <- struct{}{} }, Target: targetFunc(func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError) {
		return ExecutionResponse{Stream: p}, nil
	})}
	response, gatewayErr := d.Execute(ctx, request())
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	for range 2 {
		if _, err := response.Stream.Next(ctx); err != nil {
			t.Fatal(err)
		}
	}
	finished := make(chan error, 1)
	go func() { _, err := response.Stream.Next(ctx); finished <- err }()
	<-entered
	if err := <-finished; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline terminal read: %v", err)
	}
	select {
	case <-finalized:
	case <-time.After(time.Second):
		t.Fatal("deadline attempt was not finalized")
	}
	if len(results) != 1 || results[0].Outcome != OutcomeCancelled || results[0].Error == nil || results[0].Error.Code != "execution_timeout" {
		t.Fatalf("deadline outcome: %+v", results)
	}
}

func TestAttemptStreamExplicitCloseWinsPendingEOF(t *testing.T) {
	inputTokens := int64(11)
	p := &slowCloseAfterComplete{
		frames:       []StreamFrame{head(), {Type: FrameComplete, Complete: &CompleteFrame{Outcome: OutcomeSucceeded, Usage: &UsageReport{InputTokens: &inputTokens, Source: UsageProvider, Completeness: UsagePartial}}}},
		closeStarted: make(chan struct{}), allowClose: make(chan struct{}),
	}
	var results []AttemptResult
	finalized := make(chan struct{}, 1)
	d := &Dispatcher{AccountID: "selected", Finalize: func(r AttemptResult) { results = append(results, r); finalized <- struct{}{} }, Target: targetFunc(func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError) {
		return ExecutionResponse{Stream: p}, nil
	})}
	response, gatewayErr := d.Execute(context.Background(), request())
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	for range 2 {
		if _, err := response.Stream.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	closeDone := make(chan struct{})
	go func() { _ = response.Stream.Close(); close(closeDone) }()
	<-p.closeStarted // Close has marked the attempt terminal but is held in source.Close.
	nextDone := make(chan error, 1)
	go func() { _, err := response.Stream.Next(context.Background()); nextDone <- err }()
	select {
	case <-finalized:
	case <-time.After(time.Second):
		t.Fatal("EOF did not publish cancellation while source.Close was blocked")
	}
	if len(results) != 1 || results[0].Outcome != OutcomeCancelled || !results[0].HasUsage || results[0].Usage.InputTokens == nil || *results[0].Usage.InputTokens != inputTokens {
		t.Fatalf("explicit Close result: %+v", results)
	}
	select {
	case <-closeDone:
		t.Fatal("source.Close was not held by the gate")
	default:
	}
	close(p.allowClose)
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after releasing source.Close")
	}
	if err := <-nextDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Next after explicit Close: %v", err)
	}
	_ = response.Stream.Close()
	if len(results) != 1 {
		t.Fatalf("duplicate terminal publication: %+v", results)
	}
}

func TestAttemptStreamIncompleteTerminals(t *testing.T) {
	inputTokens := int64(5)
	for _, tc := range []struct {
		name    string
		frames  []StreamFrame
		outcome Outcome
	}{
		{name: "failed Head-to-Complete", frames: []StreamFrame{head(), {Type: FrameComplete, Complete: &CompleteFrame{Outcome: OutcomeFailed, Error: &GatewayError{Code: "upstream_failed", Category: CategoryUnavailable}, Usage: &UsageReport{InputTokens: &inputTokens, Source: UsageProvider, Completeness: UsagePartial}}}}, outcome: OutcomeFailed},
		{name: "premature EOF", frames: []StreamFrame{head()}, outcome: OutcomeIncomplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &trailingStream{frames: tc.frames, closed: make(chan struct{})}
			var results []AttemptResult
			d := &Dispatcher{AccountID: "selected", Finalize: func(r AttemptResult) { results = append(results, r) }, Target: targetFunc(func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError) {
				return ExecutionResponse{Stream: p}, nil
			})}
			response, gatewayErr := d.Execute(context.Background(), request())
			if gatewayErr != nil {
				t.Fatal(gatewayErr)
			}
			for range len(tc.frames) {
				if _, err := response.Stream.Next(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := response.Stream.Next(context.Background()); tc.outcome == OutcomeFailed && err != io.EOF || tc.outcome == OutcomeIncomplete && err == nil {
				t.Fatalf("unexpected terminal result: %v", err)
			}
			if len(results) != 1 || results[0].Outcome != tc.outcome {
				t.Fatalf("terminal results: %+v", results)
			}
			if tc.outcome == OutcomeFailed && (!results[0].HasUsage || results[0].Usage.InputTokens == nil || *results[0].Usage.InputTokens != inputTokens) {
				t.Fatalf("failed completion lost usage: %+v", results[0])
			}
			select {
			case <-p.closed:
			default:
				t.Fatal("source was not closed")
			}
			_ = response.Stream.Close()
			if len(results) != 1 {
				t.Fatalf("duplicate terminal publication: %+v", results)
			}
		})
	}
}

func TestDispatchCancelAndPreExecuteFailure(t *testing.T) {
	var mu sync.Mutex
	var results []AttemptResult
	finalized := make(chan struct{}, 2)
	p := &scriptedStream{block: true, entered: make(chan struct{}), release: make(chan struct{})}
	d := &Dispatcher{AccountID: "selected", Finalize: func(r AttemptResult) { mu.Lock(); results = append(results, r); mu.Unlock(); finalized <- struct{}{} }}
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
	<-finalized // Next/Close may race the context.AfterFunc finalizer callback.
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
	<-finalized
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
	if _, err := responses[1].Stream.Next(context.Background()); err != io.EOF {
		t.Fatalf("terminal read: %v", err)
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

func TestAttemptObservation(t *testing.T) {
	observations, err := NewInMemoryAttemptObservations(3)
	if err != nil {
		t.Fatal(err)
	}
	var count atomic.Int32
	d := &Dispatcher{AccountID: "selected-account", Observations: observations, Target: targetFunc(func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError) {
		count.Add(1)
		input := int64(7)
		zero, reasoning, cached := int64(0), int64(2), int64(1)
		usage := &UsageReport{InputTokens: &input, OutputTokens: &zero, ReasoningTokens: &reasoning, CachedTokens: &cached, Source: UsageProvider, Completeness: UsagePartial}
		return ExecutionResponse{Stream: &scriptedStream{frames: []StreamFrame{
			head(), {Type: FrameComplete, Complete: &CompleteFrame{Outcome: OutcomeSucceeded, Usage: usage}},
		}}}, nil
	})}
	// Validation rejects before attempt identity allocation and observation.
	if _, gatewayErr := d.Execute(context.Background(), ExecutionRequest{}); gatewayErr == nil {
		t.Fatal("invalid request was accepted")
	}
	if len(observations.Snapshot()) != 0 || count.Load() != 0 {
		t.Fatal("pre-Execute rejection was observed or executed")
	}
	response, gatewayErr := d.Execute(context.Background(), request())
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	for range 2 {
		if _, err := response.Stream.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := response.Stream.Next(context.Background()); err != io.EOF {
		t.Fatalf("terminal read: %v", err)
	}
	first := observations.Snapshot()
	if len(first) != 1 {
		t.Fatalf("observations: %+v", first)
	}
	observation := first[0]
	if observation.RequestID == "" || observation.AttemptID == "" || observation.RequestID == observation.AttemptID || observation.AccountID != "selected-account" || observation.Mode != ModeNative || !observation.Committed || observation.Outcome != OutcomeSucceeded || observation.Duration < 0 || observation.EndedAt.Before(observation.StartedAt) {
		t.Fatalf("invalid terminal observation: %+v", observation)
	}
	if observation.Usage == nil || observation.Usage.InputTokens == nil || *observation.Usage.InputTokens != 7 || observation.Usage.OutputTokens == nil || *observation.Usage.OutputTokens != 0 || observation.Usage.ReasoningTokens == nil || *observation.Usage.ReasoningTokens != 2 || observation.Usage.CachedTokens == nil || *observation.Usage.CachedTokens != 1 || observation.Usage.Completeness != UsagePartial {
		t.Fatalf("lost nullable partial usage: %+v", observation.Usage)
	}
	*observation.Usage.InputTokens = 99
	*observation.Usage.OutputTokens = 99
	*first[0].Usage.InputTokens = 101
	stored := observations.Snapshot()[0].Usage
	if stored.InputTokens == nil || *stored.InputTokens != 7 || stored.OutputTokens == nil || *stored.OutputTokens != 0 || stored.ReasoningTokens == nil || *stored.ReasoningTokens != 2 || stored.CachedTokens == nil || *stored.CachedTokens != 1 {
		t.Fatalf("snapshot exposed mutable state or merged usage categories: %+v", stored)
	}
	zero := int64(0)
	copyStore, _ := NewInMemoryAttemptObservations(1)
	copyStore.TryRecord(AttemptObservation{Usage: &UsageReport{OutputTokens: &zero}})
	zero = 5
	copySnapshot := copyStore.Snapshot()[0]
	if copySnapshot.Usage.OutputTokens == nil || *copySnapshot.Usage.OutputTokens != 0 || copySnapshot.Usage.InputTokens != nil {
		t.Fatalf("zero and unknown usage were conflated: %+v", copySnapshot.Usage)
	}
	*copySnapshot.Usage.OutputTokens = 6
	if got := copyStore.Snapshot()[0].Usage.OutputTokens; got == nil || *got != 0 {
		t.Fatalf("snapshot mutation changed retained zero usage: %v", got)
	}
	for i := range 4 {
		observations.TryRecord(AttemptObservation{RequestID: string(rune('a' + i))})
	}
	bounded := observations.Snapshot()
	if len(bounded) != 3 || bounded[0].RequestID != "b" || bounded[2].RequestID != "d" {
		t.Fatalf("collector did not retain bounded newest observations: %+v", bounded)
	}

	var failed []AttemptObservation
	failedStore, _ := NewInMemoryAttemptObservations(2)
	d.Observations = failedStore
	d.Target = targetFunc(func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError) {
		return ExecutionResponse{}, &GatewayError{Code: "opaque-secret-code", Category: CategoryUnavailable, Retryable: true, RetryDisposition: RetryUnsafe, Provider: "private-provider", OriginalError: "private diagnostic", Message: "private message"}
	})
	if _, gatewayErr := d.Execute(context.Background(), request()); gatewayErr == nil {
		t.Fatal("expected pre-Head connector failure")
	}
	failed = failedStore.Snapshot()
	if len(failed) != 1 || failed[0].Outcome != OutcomeFailed || failed[0].Committed || failed[0].ErrorCategory != CategoryUnavailable || !failed[0].Retryable || failed[0].RetryDisposition != RetryUnsafe || failed[0].Usage != nil {
		t.Fatalf("invalid sanitized connector failure observation: %+v", failed)
	}
	// A cancelled attempt also finalizes once; repeated Close cannot duplicate it.
	cancelStore, _ := NewInMemoryAttemptObservations(2)
	d.Observations = cancelStore
	d.Target = targetFunc(func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError) {
		return ExecutionResponse{Stream: &scriptedStream{frames: []StreamFrame{head()}}}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	response, gatewayErr = d.Execute(ctx, request())
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	if _, err := response.Stream.Next(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = response.Stream.Close()
	_ = response.Stream.Close()
	cancelled := cancelStore.Snapshot()
	if len(cancelled) != 1 || cancelled[0].Outcome != OutcomeCancelled || !cancelled[0].Committed {
		t.Fatalf("cancellation did not publish exactly one committed outcome: %+v", cancelled)
	}

	// Complete errors and stream interruptions retain only classified metadata.
	for name, stream := range map[string]*scriptedStream{
		"incomplete":     {frames: []StreamFrame{head()}, err: errors.New("private transport detail")},
		"complete-error": {frames: []StreamFrame{head(), {Type: FrameComplete, Complete: &CompleteFrame{Outcome: OutcomeFailed, Error: &GatewayError{Category: CategoryRateLimited, Retryable: true, RetryDisposition: RetryUnknown, Provider: "private-provider", OriginalError: "private diagnostic", Message: "private message"}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			store, err := NewInMemoryAttemptObservations(1)
			if err != nil {
				t.Fatal(err)
			}
			d.Observations = store
			d.Target = targetFunc(func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError) {
				return ExecutionResponse{Stream: stream}, nil
			})
			response, gatewayErr := d.Execute(context.Background(), request())
			if gatewayErr != nil {
				t.Fatal(gatewayErr)
			}
			for range len(stream.frames) + 1 {
				if _, err := response.Stream.Next(context.Background()); err != nil {
					break
				}
			}
			got := store.Snapshot()
			if len(got) != 1 {
				t.Fatalf("expected terminal observation: %+v", got)
			}
			if name == "incomplete" && (got[0].Outcome != OutcomeIncomplete || got[0].ErrorCategory != CategoryInternal) {
				t.Fatalf("incomplete stream was not classified: %+v", got[0])
			}
			if name == "complete-error" && (got[0].Outcome != OutcomeFailed || got[0].ErrorCategory != CategoryRateLimited || !got[0].Retryable || got[0].RetryDisposition != RetryUnknown) {
				t.Fatalf("Complete error was not classified: %+v", got[0])
			}
		})
	}

	// Concurrent ring writes and snapshots stay bounded and race-free.
	concurrent, _ := NewInMemoryAttemptObservations(16)
	var workers sync.WaitGroup
	for i := range 8 {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for j := range 100 {
				concurrent.TryRecord(AttemptObservation{RequestID: fmt.Sprintf("%d-%d", worker, j)})
				_ = concurrent.Snapshot()
			}
		}(i)
	}
	workers.Wait()
	if got := concurrent.Snapshot(); len(got) != 16 {
		t.Fatalf("concurrent collector exceeded/lost capacity: %d", len(got))
	}
	assertAttemptObservationPublicationWaitsForWinner(t)
}

type gatedObservationSink struct {
	store   AttemptObservationSink
	entered chan struct{}
	release chan struct{}
}

func (s *gatedObservationSink) TryRecord(observation AttemptObservation) bool {
	close(s.entered)
	<-s.release
	return s.store.TryRecord(observation)
}

func assertAttemptObservationPublicationWaitsForWinner(t *testing.T) {
	t.Helper()
	store, err := NewInMemoryAttemptObservations(1)
	if err != nil {
		t.Fatal(err)
	}
	sink := &gatedObservationSink{store: store, entered: make(chan struct{}), release: make(chan struct{})}
	d := &Dispatcher{AccountID: "selected", Observations: sink, Target: targetFunc(func(context.Context, ExecutionRequest, AttemptScope) (ExecutionResponse, *GatewayError) {
		return ExecutionResponse{Stream: &scriptedStream{frames: []StreamFrame{head(), complete()}}}, nil
	})}
	response, gatewayErr := d.Execute(context.Background(), request())
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Type != FrameHead {
		t.Fatalf("head handoff: %+v, %v", frame, err)
	}
	result := make(chan error, 1)
	go func() {
		if _, err := response.Stream.Next(context.Background()); err != nil {
			result <- err
			return
		}
		_, err := response.Stream.Next(context.Background())
		result <- err
	}()
	<-sink.entered
	closed := make(chan struct{})
	go func() { _ = response.Stream.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("losing Close returned before terminal observation publication")
	case <-time.After(time.Millisecond):
	}
	if got := store.Snapshot(); len(got) != 0 {
		t.Fatalf("observation published before gate release: %+v", got)
	}
	close(sink.release)
	if err := <-result; err != io.EOF {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("losing Close did not finish after observation publication")
	}
	if got := store.Snapshot(); len(got) != 1 || got[0].Outcome != OutcomeSucceeded {
		t.Fatalf("winner's terminal observation missing or duplicated: %+v", got)
	}
}
