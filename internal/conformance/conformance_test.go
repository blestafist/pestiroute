package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
	"github.com/blestafist/pestiroute/internal/testutil/scripted"
)

const (
	protocol = "openai.responses.v1"
	model    = "gpt-5.4-mini"
	account  = "conformance-account"
	request  = "conformance-request"
)

type fixture struct {
	connector core.Connector
	request   core.ExecutionRequest
	scope     core.AttemptScope
	services  core.InvocationServices
	wantBody  []byte
	close     func()
}

type factory struct {
	name string
	new  func(*testing.T) fixture
}

type scenario struct {
	name       string
	mandatory  bool
	capability core.Capability
	run        func(fixture) error
}

func TestConformance(t *testing.T) {
	factories := []factory{
		{name: "native-loopback", new: nativeFixture},
		{name: "scripted", new: scriptedFixture},
	}
	scenarios := []scenario{
		{name: "valid-success", mandatory: true, run: runBaseline},
		// Optional feature slots deliberately have no assertion until a later
		// conformance task supplies one. A declaration alone cannot pass it.
		{name: "parallel-tools", capability: "llm.tools.parallel"},
	}
	for _, f := range factories {
		for _, scenario := range scenarios {
			t.Run(f.name+"/"+scenario.name, func(t *testing.T) {
				fx := f.new(t)
				t.Cleanup(fx.close)
				if !scenario.mandatory {
					state := fx.connector.Capabilities(context.Background(), capabilityScope()).State(scenario.capability)
					if shouldSkip(scenario.mandatory, state) {
						t.Skipf("optional scenario %s: capability %s is %s", scenario.name, scenario.capability, state)
					}
					if state != core.Supported {
						t.Fatalf("optional scenario %s has invalid capability state %q", scenario.name, state)
					}
					if scenario.run == nil {
						t.Fatalf("optional scenario %s is declared supported but has no assertion", scenario.name)
					}
				}
				if scenario.run == nil {
					t.Fatal("mandatory scenario has no assertion")
				}
				if err := scenario.run(fx); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func nativeFixture(t *testing.T) fixture {
	t.Helper()
	upstream := fakeupstream.New(fakeupstream.Response{
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   []byte(`{"status":"completed","opaque":{"native":true}}`),
	})
	connector := responses.NewConnector()
	config, err := json.Marshal(map[string]any{
		"transport": map[string]any{"endpoint": upstream.URL + "/v1/responses"},
		"model":     model, "account_id": account,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := connector.Init(context.Background(), core.ComponentConfig{Data: config}); err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	t.Setenv("PESTIROUTE_CONFORMANCE_TOKEN", "synthetic-token")
	services := core.NewEnvironmentServices(
		map[string]map[string]string{account: {"bearer": "PESTIROUTE_CONFORMANCE_TOKEN"}},
		map[string]core.HTTPDoer{account: connector.HTTPDoer()}, nil,
	).ForAttempt(core.AttemptScope{AccountID: account, Mode: "native"})
	return fixture{
		connector: connector,
		request: core.ExecutionRequest{ID: request, Model: model, Payload: core.RawPayload{
			Protocol: protocol, ContentType: "application/json", Body: []byte(`{"model":"gpt-5.4-mini","input":"test"}`),
		}},
		scope:    core.AttemptScope{ID: "conformance-attempt", AccountID: account, Mode: "native"},
		services: services,
		wantBody: []byte(`{"status":"completed","opaque":{"native":true}}`),
		close: func() {
			if err := connector.Close(context.Background()); err != nil {
				t.Errorf("close connector: %v", err)
			}
			upstream.Close()
		},
	}
}

func scriptedFixture(t *testing.T) fixture {
	t.Helper()
	scope := core.CapabilityScope{Protocol: protocol, Mode: "native", Model: model, AccountID: account}
	connector := scripted.New(core.Descriptor{
		ID: "conformance.scripted", Kind: core.ComponentConnector, ImplementationVersion: "test",
		APIVersions: []core.APIVersion{{Major: 1}}, Protocols: []string{protocol},
		Operations: []string{"execute"}, ConnectorType: "test",
	}, map[core.CapabilityScope]core.CapabilityResult{scope: {Values: map[core.Capability]core.CapabilityState{
		"llm.images": core.Unsupported,
	}}}, scripted.Script{ID: request, Steps: []scripted.Step{
		{Frame: core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: protocol, ContentType: "application/json"}}},
		{Frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: []byte(`{"status":"completed","opaque":{"scripted":true}}`)}}},
		{Frame: core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded}}},
	}})
	if err := connector.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	return fixture{
		connector: connector,
		request:   core.ExecutionRequest{ID: request, Model: model, Payload: core.RawPayload{Protocol: protocol, ContentType: "application/json", Body: []byte(`{"model":"gpt-5.4-mini","input":"test"}`)}},
		scope:     core.AttemptScope{ID: "conformance-attempt", AccountID: account, Mode: "native"},
		wantBody:  []byte(`{"status":"completed","opaque":{"scripted":true}}`),
		close: func() {
			if err := connector.Close(context.Background()); err != nil {
				t.Errorf("close connector: %v", err)
			}
		},
	}
}

func runBaseline(fx fixture) error {
	ctx := context.Background()
	result, gatewayErr := fx.connector.Execute(ctx, fx.request, fx.scope, fx.services)
	if gatewayErr != nil {
		return gatewayErr
	}
	if result.Stream == nil {
		return errors.New("Execute returned no stream")
	}
	defer result.Stream.Close()
	var head, complete bool
	var body []byte
	for {
		frame, err := result.Stream.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch frame.Type {
		case core.FrameHead:
			if head || frame.Head == nil || frame.Head.Protocol != protocol {
				return errors.New("missing or duplicate Head")
			}
			head = true
		case core.FrameBody:
			if !head || complete || frame.Body == nil {
				return errors.New("Body outside response lifecycle")
			}
			body = append(body, frame.Body.Data...)
		case core.FrameComplete:
			if !head || complete || frame.Complete == nil || frame.Complete.Outcome != core.OutcomeSucceeded {
				return errors.New("missing or unsuccessful Complete")
			}
			complete = true
		default:
			return errors.New("unknown frame type")
		}
	}
	if !head || !complete || len(body) == 0 || !bytes.Equal(body, fx.wantBody) {
		return errors.New("missing Head, Body, successful Complete, or expected response bytes")
	}
	return nil
}

func capabilityScope() core.CapabilityScope {
	return core.CapabilityScope{Protocol: protocol, Mode: "native", Model: model, AccountID: account}
}

// TestConformanceRunnerRejectsBrokenFixturesAndCleansUp proves common baseline
// assertions reject invalid frame sequences and release fixtures on failure.
func TestConformanceRunnerRejectsBrokenFixturesAndCleansUp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames []core.StreamFrame
	}{
		{name: "missing-head", frames: []core.StreamFrame{bodyFrame(), completeFrame()}},
		{name: "missing-body", frames: []core.StreamFrame{headFrame(), completeFrame()}},
		{name: "missing-complete", frames: []core.StreamFrame{headFrame(), bodyFrame()}},
		{name: "wrong-order", frames: []core.StreamFrame{bodyFrame(), headFrame(), completeFrame()}},
		{name: "failed-completion", frames: []core.StreamFrame{headFrame(), bodyFrame(), {Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeFailed, Error: &core.GatewayError{Code: "failed"}}}}},
	} {
		fixtureCleaned := false
		t.Run(tc.name, func(t *testing.T) {
			streamClosed := false
			fx := fixture{
				connector: fixtureConnector{stream: &fixtureStream{frames: tc.frames, closed: &streamClosed}},
				request:   core.ExecutionRequest{ID: request},
				wantBody:  []byte("expected"),
				close:     func() { fixtureCleaned = true },
			}
			t.Cleanup(fx.close)
			if err := runBaseline(fx); err == nil {
				t.Fatal("broken stream unexpectedly passed the baseline")
			} else {
				t.Logf("broken stream rejected: %v", err)
			}
			if !streamClosed {
				t.Fatal("failed baseline did not close its stream")
			}
		})
		if !fixtureCleaned {
			t.Errorf("fixture cleanup did not run after baseline assertion failure in %s", tc.name)
		}
	}
}

func TestConformanceMandatoryAndOptionalSupport(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mandatory bool
		state     core.CapabilityState
		wantSkip  bool
	}{
		{name: "mandatory-unsupported-runs", mandatory: true, state: core.Unsupported},
		{name: "mandatory-unknown-runs", mandatory: true, state: core.Unknown},
		{name: "optional-unsupported-skips", state: core.Unsupported, wantSkip: true},
		{name: "optional-unknown-skips", state: core.Unknown, wantSkip: true},
		{name: "optional-supported-runs", state: core.Supported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSkip(tc.mandatory, tc.state); got != tc.wantSkip {
				t.Fatalf("shouldSkip(%v, %q) = %v, want %v", tc.mandatory, tc.state, got, tc.wantSkip)
			}
		})
	}
}

func shouldSkip(mandatory bool, state core.CapabilityState) bool {
	return !mandatory && (state == core.Unsupported || state == core.Unknown)
}

func headFrame() core.StreamFrame {
	return core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: protocol}}
}

func bodyFrame() core.StreamFrame {
	return core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: []byte("expected")}}
}

func completeFrame() core.StreamFrame {
	return core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded}}
}

type fixtureConnector struct{ stream core.Stream }

func (fixtureConnector) Descriptor() core.Descriptor                      { return core.Descriptor{} }
func (fixtureConnector) Init(context.Context, core.ComponentConfig) error { return nil }
func (fixtureConnector) Health(context.Context) core.Health {
	return core.Health{State: core.HealthReady}
}
func (fixtureConnector) Capabilities(context.Context, core.CapabilityScope) core.CapabilityResult {
	return core.CapabilityResult{}
}
func (fixtureConnector) Close(context.Context) error { return nil }
func (c fixtureConnector) Execute(context.Context, core.ExecutionRequest, core.AttemptScope, core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	return core.ExecutionResponse{Stream: c.stream}, nil
}
func (fixtureConnector) Models(context.Context, core.ModelQuery, core.InvocationServices) (core.ModelsResult, *core.GatewayError) {
	return core.ModelsResult{}, nil
}
func (fixtureConnector) EstimateUsage(context.Context, core.UsageQuery, core.InvocationServices) (core.EstimateResult, *core.GatewayError) {
	return core.EstimateResult{}, nil
}
func (fixtureConnector) Authenticate(context.Context, core.AuthRequest, core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	return core.AuthResult{}, nil
}

type fixtureStream struct {
	frames []core.StreamFrame
	next   int
	closed *bool
}

func (s *fixtureStream) Next(ctx context.Context) (core.StreamFrame, error) {
	if err := ctx.Err(); err != nil {
		return core.StreamFrame{}, err
	}
	if s.next == len(s.frames) {
		return core.StreamFrame{}, io.EOF
	}
	frame := s.frames[s.next]
	s.next++
	return frame, nil
}
func (s *fixtureStream) Close() error {
	*s.closed = true
	return nil
}

type brokenConnector struct{}

func (brokenConnector) Execute(context.Context, core.ExecutionRequest, core.AttemptScope, core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	return core.ExecutionResponse{}, nil
}
func (brokenConnector) Descriptor() core.Descriptor                      { return core.Descriptor{} }
func (brokenConnector) Init(context.Context, core.ComponentConfig) error { return nil }
func (brokenConnector) Health(context.Context) core.Health {
	return core.Health{State: core.HealthReady}
}
func (brokenConnector) Capabilities(context.Context, core.CapabilityScope) core.CapabilityResult {
	return core.CapabilityResult{}
}
func (brokenConnector) Close(context.Context) error { return nil }
func (brokenConnector) Models(context.Context, core.ModelQuery, core.InvocationServices) (core.ModelsResult, *core.GatewayError) {
	return core.ModelsResult{}, nil
}
func (brokenConnector) EstimateUsage(context.Context, core.UsageQuery, core.InvocationServices) (core.EstimateResult, *core.GatewayError) {
	return core.EstimateResult{}, nil
}
func (brokenConnector) Authenticate(context.Context, core.AuthRequest, core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	return core.AuthResult{}, nil
}

var _ core.Connector = brokenConnector{}
