package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

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
	connector  core.Connector
	init       func(context.Context) error
	failedInit func() error
	request    core.ExecutionRequest
	scope      core.AttemptScope
	services   func() core.InvocationServices
	wantBody   []byte
	callCount  func() int64
	waitCancel func() bool
	release    func()
	close      func()
	translated bool
	codex      bool
	validate   func() error
	capture    func() []byte
	dispatcher *core.Dispatcher
}

var opaqueRequest = []byte("{ \"model\" : \"gpt-5.4-mini\", \"input\" : \"snowman ☃\", \"future_field\" : { \"keep\" : true } }\n")

func TestConformanceOpacity(t *testing.T) {
	want := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"note\":\"☃\",\"future\":true}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"tool_call_id\":\"call_synthetic-42\"}}\n\n")
	cut := bytes.Index(want, []byte("☃")) + 2 // Deliberately split inside the UTF-8 encoding.
	parts := [][]byte{want[:cut], want[cut : len(want)-1], want[len(want)-1:]}
	for _, tc := range []struct {
		name string
		new  func(*testing.T, [][]byte) (fixture, func() []byte)
	}{{"native-loopback", nativeOpaqueFixture}, {"codex", codexOpaqueFixture}, {"scripted", scriptedOpaqueFixture}} {
		t.Run(tc.name, func(t *testing.T) {
			fx, captured := tc.new(t, parts)
			t.Cleanup(fx.close)
			if err := fx.init(context.Background()); err != nil {
				t.Fatal(err)
			}
			stream, gatewayErr := fx.connector.Execute(context.Background(), fx.request, fx.scope, fx.services())
			if gatewayErr != nil {
				t.Fatal(gatewayErr)
			}
			defer stream.Stream.Close()
			body, err := readOpaqueBody(stream.Stream)
			if err != nil {
				t.Fatal(err)
			}
			if err := assertOpaque(body, want); err != nil {
				t.Fatal(err)
			}
			if err := assertOpaque(captured(), fx.request.Payload.Body); err != nil {
				t.Fatalf("request: %v", err)
			}
		})
	}
}

func TestConformanceIncremental(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scripted bool
		new      func(*testing.T, <-chan struct{}, chan<- struct{}) fixture
	}{{name: "native-loopback", new: nativeGatedFixture}, {name: "codex", new: codexGatedFixture}, {name: "scripted", scripted: true, new: scriptedGatedFixture}} {
		t.Run(tc.name, func(t *testing.T) {
			gate, waiting := make(chan struct{}), make(chan struct{}, 1)
			fx := tc.new(t, gate, waiting)
			t.Cleanup(fx.close)
			if err := fx.init(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := runIncrementalScenario(fx, fx.connector, gate, waiting, tc.scripted); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// runIncrementalScenario is the shared conformance assertion used for real
// factories and deliberate connector mutations. Deadlines only bound failure;
// success is established by the closed gate and its observable Waiting/Sent.
func runIncrementalScenario(fx fixture, connector core.Connector, gate chan struct{}, signal <-chan struct{}, scripted bool) (retErr error) {
	ctx, cancel := context.WithCancel(context.Background())
	gateClosed := false
	closeGate := func() {
		if !gateClosed {
			close(gate)
			gateClosed = true
		}
	}
	defer func() { closeGate(); cancel() }()
	result, gatewayErr := connector.Execute(ctx, fx.request, fx.scope, fx.services())
	if gatewayErr != nil {
		return gatewayErr
	}
	defer result.Stream.Close()
	boundedNext := func() (core.StreamFrame, error) {
		type outcome struct {
			frame core.StreamFrame
			err   error
		}
		resultCh := make(chan outcome, 1)
		go func() { frame, err := result.Stream.Next(ctx); resultCh <- outcome{frame, err} }()
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case got := <-resultCh:
			return got.frame, got.err
		case <-timer.C:
			closeGate()
			cancel()
			<-resultCh // Ensure a timed-out Next is unblocked and joined before returning.
			return core.StreamFrame{}, errors.New("timed out waiting for incremental stream frame (safety bound)")
		}
	}
	frame, err := boundedNext()
	if err != nil || frame.Type != core.FrameHead {
		return fmt.Errorf("Head = %+v, %v", frame, err)
	}
	var first []byte
	for len(first) < len(incrementalFirst) {
		frame, err = boundedNext()
		if err != nil || frame.Type != core.FrameBody || frame.Body == nil {
			return fmt.Errorf("initial Body = %+v, %v", frame, err)
		}
		first = append(first, frame.Body.Data...)
	}
	if !bytes.Equal(first, incrementalFirst) {
		return fmt.Errorf("initial Body bytes = %q, want %q", first, incrementalFirst)
	}
	if err := assertGateClosed(gate); err != nil {
		return err
	}
	type outcome struct {
		frame core.StreamFrame
		err   error
	}
	pending := make(chan outcome, 1)
	go func() { frame, err := result.Stream.Next(ctx); pending <- outcome{frame, err} }()
	if scripted {
		select {
		case <-signal: // Scripted Next reached the terminal step, which remains gated.
		case <-time.After(2 * time.Second):
			closeGate()
			cancel()
			<-pending
			return errors.New("timed out waiting for scripted stream gate (safety bound)")
		}
	}
	closeGate()
	if !scripted {
		select {
		case <-signal: // fakeupstream Sent confirms its terminal write was flushed.
		case <-time.After(2 * time.Second):
			cancel()
			<-pending
			return errors.New("timed out waiting for gated upstream write (safety bound)")
		}
	}
	body := append([]byte(nil), first...)
	timer := time.NewTimer(2 * time.Second)
	select {
	case got := <-pending:
		if got.err != nil {
			return got.err
		}
		if got.frame.Type == core.FrameBody {
			body = append(body, got.frame.Body.Data...)
		}
	case <-timer.C:
		cancel()
		<-pending
		return errors.New("timed out awaiting gated Body (safety bound)")
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	for {
		frame, err = boundedNext()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if frame.Type == core.FrameBody {
			body = append(body, frame.Body.Data...)
		}
	}
	if !bytes.Equal(body, append(append([]byte(nil), incrementalFirst...), incrementalTerminal...)) {
		return fmt.Errorf("gated response mutated: %q", body)
	}
	return nil
}

var (
	incrementalFirst    = []byte("event: response.created\ndata: {\"type\":\"response.created\",\"note\":\"☃\"}\n\n")
	incrementalTerminal = []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
)

func assertOpaque(got, want []byte) error {
	if !bytes.Equal(got, want) {
		return fmt.Errorf("opaque bytes differ: got %q want %q", got, want)
	}
	return nil
}

func assertGateClosed(gate <-chan struct{}) error {
	select {
	case <-gate:
		return errors.New("incremental stream released its later step before initial delivery")
	default:
		return nil
	}
}

func TestConformanceIncrementalRejectsMutatedConnectors(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(core.Stream) core.Stream
	}{{"corrupt-body", func(stream core.Stream) core.Stream { return &corruptingStream{Stream: stream} }}, {"buffer-until-eof", func(stream core.Stream) core.Stream { return &bufferingStream{Stream: stream} }}} {
		t.Run(tc.name, func(t *testing.T) {
			gate, waiting := make(chan struct{}), make(chan struct{}, 1)
			fx := scriptedGatedFixture(t, gate, waiting)
			t.Cleanup(fx.close)
			if err := fx.init(context.Background()); err != nil {
				t.Fatal(err)
			}
			mutated := streamWrappingConnector{Connector: fx.connector, wrap: tc.wrap}
			if err := runIncrementalScenario(fx, mutated, gate, waiting, true); err == nil {
				t.Fatal("mutated connector passed incremental conformance")
			}
		})
	}
	if err := assertOpaque([]byte("opaque"), []byte("opaqxe")); err == nil {
		t.Fatal("byte-corrupting payload passed opacity assertion")
	}
}

type streamWrappingConnector struct {
	core.Connector
	wrap func(core.Stream) core.Stream
}

func (c streamWrappingConnector) Execute(ctx context.Context, request core.ExecutionRequest, scope core.AttemptScope, services core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	response, gatewayErr := c.Connector.Execute(ctx, request, scope, services)
	if gatewayErr == nil && response.Stream != nil {
		response.Stream = c.wrap(response.Stream)
	}
	return response, gatewayErr
}

type corruptingStream struct{ core.Stream }

func (s *corruptingStream) Next(ctx context.Context) (core.StreamFrame, error) {
	frame, err := s.Stream.Next(ctx)
	if err == nil && frame.Type == core.FrameBody && len(frame.Body.Data) != 0 {
		frame.Body.Data = append([]byte(nil), frame.Body.Data...)
		frame.Body.Data[0] ^= 1
	}
	return frame, err
}

type bufferingStream struct {
	core.Stream
	frames []core.StreamFrame
	index  int
	loaded bool
}

func (s *bufferingStream) Next(ctx context.Context) (core.StreamFrame, error) {
	if !s.loaded {
		for {
			frame, err := s.Stream.Next(ctx)
			if err != nil {
				if err == io.EOF {
					s.loaded = true
					break
				}
				return core.StreamFrame{}, err
			}
			s.frames = append(s.frames, frame)
		}
	}
	if s.index == len(s.frames) {
		return core.StreamFrame{}, io.EOF
	}
	frame := s.frames[s.index]
	s.index++
	return frame, nil
}

func readOpaqueBody(stream core.Stream) ([]byte, error) {
	var body []byte
	for {
		frame, err := stream.Next(context.Background())
		if err == io.EOF {
			return body, nil
		}
		if err != nil {
			return nil, err
		}
		if frame.Type == core.FrameBody {
			body = append(body, frame.Body.Data...)
		}
	}
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
		{name: "codex", new: codexFixture},
		{name: "anthropic-translation", new: translationFixture},
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

func TestConformanceLifecycleAndScopedServices(t *testing.T) {
	factories := []factory{
		{name: "native-loopback", new: nativeFixture},
		{name: "codex", new: codexFixture},
		{name: "anthropic-translation", new: translationFixture},
		{name: "scripted", new: scriptedFixture},
	}
	for _, f := range factories {
		t.Run(f.name, func(t *testing.T) {
			fx := f.new(t)
			t.Cleanup(fx.close)
			descriptor := fx.connector.Descriptor()
			if err := descriptor.Validate(core.ComponentConnector); err != nil {
				t.Fatalf("descriptor before Init invalid: %v", err)
			}
			if descriptor.ID == "" || !descriptor.SupportsAPIVersion(core.APIVersion{Major: 1}) || !slices.Contains(descriptor.Operations, "execute") {
				t.Fatalf("incomplete pre-Init descriptor: %+v", descriptor)
			}
			if err := assertPreInitGate(fx.connector, fx.request, fx.scope, fx.services()); err != nil {
				t.Fatal(err)
			}
			if got := fx.callCount(); got != 0 {
				t.Fatalf("pre-Init Execute reached upstream/script: %d calls", got)
			}

			if err := fx.init(context.Background()); err != nil {
				t.Fatalf("Init: %v", err)
			}
			if fx.connector.Health(context.Background()).State != core.HealthReady {
				t.Fatal("connector not ready after successful Init")
			}
			if got := fx.connector.Descriptor(); !descriptorsEqual(descriptor, got) {
				t.Fatalf("descriptor changed across Init: before=%+v after=%+v", descriptor, got)
			}
			services := fx.services()
			assertExecuteRejected(t, fx.connector, fx.request, fx.scope, core.InvocationServices{}) // bearer credential is mandatory in both descriptors
			wrongProtocol := fx.request
			wrongProtocol.Payload.Protocol = "undeclared.protocol"
			assertExecuteRejected(t, fx.connector, wrongProtocol, fx.scope, services)
			wrongAccount := fx.scope
			wrongAccount.AccountID = "other-account"
			assertExecuteRejected(t, fx.connector, fx.request, wrongAccount, services)
			wrongMode := fx.scope
			wrongMode.Mode = core.ModeTranslation
			if fx.translated {
				wrongMode.Mode = core.ModeNative
			}
			if fx.codex {
				// Codex supports both declared modes; test rejection with an undeclared mode.
				wrongMode.Mode = "unsupported-mode"
			}
			assertExecuteRejected(t, fx.connector, fx.request, wrongMode, services)
			wrongModel := fx.request
			wrongModel.Model = "other-model"
			assertExecuteRejected(t, fx.connector, wrongModel, fx.scope, services)
			if got := fx.callCount(); got != 0 {
				t.Fatalf("rejected credential/protocol/scope call reached upstream/script: %d calls", got)
			}
			models, err := fx.connector.Models(context.Background(), core.ModelQuery{Protocol: protocol, Mode: fx.scope.Mode, AccountID: account}, services)
			if err != nil || !honestModelsResult(models) {
				t.Fatalf("valid scoped Models result=%+v error=%v", models, err)
			}
			otherModels, err := fx.connector.Models(context.Background(), core.ModelQuery{Protocol: protocol, Mode: fx.scope.Mode, AccountID: "other-account"}, services)
			if fx.codex {
				// The Connector contract's exact-scope check classifies this as scope_mismatch.
				if err == nil || err.Code != "scope_mismatch" || otherModels.Supported || len(otherModels.Models) != 0 {
					t.Fatalf("cross-account Codex Models result=%+v error=%v", otherModels, err)
				}
			} else if err != nil || otherModels.Supported || len(otherModels.Models) != 0 {
				t.Fatalf("cross-account Models result=%+v error=%v", otherModels, err)
			}
			usageQuery := core.UsageQuery{}
			if fx.translated || fx.codex {
				usageQuery = core.UsageQuery{Protocol: protocol, Mode: fx.scope.Mode, Model: fx.request.Model, AccountID: fx.scope.AccountID}
			}
			if result, err := fx.connector.EstimateUsage(context.Background(), usageQuery, services); err != nil || result.Supported != fx.translated || result.Known || result.Usage != nil {
				t.Fatalf("unsupported usage estimate fabricated result=%+v error=%v", result, err)
			}
			authRequest := core.AuthRequest{}
			if fx.codex {
				authRequest.AccountID = account
			}
			if result, err := fx.connector.Authenticate(context.Background(), authRequest, services); err != nil || result.Supported || result.State != "" || len(result.Credentials) != 0 {
				t.Fatalf("unsupported authentication fabricated result=%+v error=%v", result, err)
			}
			if err := fx.connector.Close(context.Background()); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if err := fx.connector.Close(context.Background()); err != nil {
				t.Fatalf("repeated Close: %v", err)
			}
			if scripted, ok := fx.connector.(*scripted.Connector); ok && scripted.CloseCount() != 1 {
				t.Fatalf("scripted Close count=%d, want exactly one", scripted.CloseCount())
			}
			if fx.connector.Health(context.Background()).State != core.HealthUnavailable {
				t.Fatal("connector not unavailable after Close")
			}
			if err := assertPostCloseGate(fx.connector, fx.request, fx.scope, fx.services()); err != nil {
				t.Fatal(err)
			}
			if got := fx.callCount(); got != 0 {
				t.Fatalf("pre/post-lifecycle Execute reached upstream/script: %d calls", got)
			}
		})
	}
}

func TestConformanceFailedInitIsUnavailable(t *testing.T) {
	for _, f := range []factory{{name: "native-loopback", new: nativeFixture}, {name: "codex", new: codexFixture}, {name: "anthropic-translation", new: translationFixture}, {name: "scripted", new: scriptedFixture}} {
		t.Run(f.name, func(t *testing.T) {
			fx := f.new(t)
			t.Cleanup(fx.close)
			err := fx.failedInit()
			if err == nil {
				t.Fatal("invalid/cancelled Init unexpectedly succeeded")
			}
			if fx.connector.Health(context.Background()).State != core.HealthUnavailable {
				t.Fatalf("health after failed Init = %q, want unavailable", fx.connector.Health(context.Background()).State)
			}
			rejectUnavailable(t, fx)
			if got := fx.callCount(); got != 0 {
				t.Fatalf("failed-Init Execute reached upstream/script: %d calls", got)
			}
			if err := fx.connector.Close(context.Background()); err != nil {
				t.Fatalf("Close after failed Init: %v", err)
			}
			if err := fx.connector.Close(context.Background()); err != nil {
				t.Fatalf("repeated Close after failed Init: %v", err)
			}
		})
	}
}

func assertExecuteRejected(t *testing.T, connector core.Connector, request core.ExecutionRequest, scope core.AttemptScope, services core.InvocationServices) {
	t.Helper()
	if _, gatewayErr := connector.Execute(context.Background(), request, scope, services); gatewayErr == nil || gatewayErr.Code == "" || gatewayErr.Category == "" {
		t.Fatalf("Execute did not return a classified rejection for protocol=%q model=%q account=%q mode=%q: %+v", request.Payload.Protocol, request.Model, scope.AccountID, scope.Mode, gatewayErr)
	}
}

func validModelsResult(result core.ModelsResult) bool {
	if !result.Supported || len(result.Models) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(result.Models))
	for _, model := range result.Models {
		if model.ID == "" {
			return false
		}
		if _, exists := seen[model.ID]; exists {
			return false
		}
		seen[model.ID] = struct{}{}
	}
	return true
}

func honestModelsResult(result core.ModelsResult) bool {
	if !result.Supported {
		return len(result.Models) == 0
	}
	return validModelsResult(result)
}

func TestConformanceFailedInitReleasesPartialResourceOnce(t *testing.T) {
	connector := &partialInitConnector{}
	if err := assertFailedInitCleanup(connector); err != nil {
		t.Fatal(err)
	}
	if connector.releaseCount != 1 || connector.closeCount != 1 {
		t.Fatalf("release count=%d close count=%d, want one each", connector.releaseCount, connector.closeCount)
	}
}

func TestConformanceRejectsFailedInitCleanupMutation(t *testing.T) {
	connector := &partialInitConnector{leakOnClose: true}
	if err := assertFailedInitCleanup(connector); err == nil {
		t.Fatal("leaking failed-Init fixture passed conformance assertion")
	}
}

func TestConformanceRejectsLifecycleGateMutations(t *testing.T) {
	preInitReady := &partialInitConnector{readyBeforeInit: true}
	if err := assertPreInitGate(preInitReady, core.ExecutionRequest{}, core.AttemptScope{}, core.InvocationServices{}); err == nil {
		t.Fatal("pre-Init-ready fixture passed lifecycle assertion")
	}
	afterCloseAccepts := &partialInitConnector{acceptAfterClose: true}
	if err := afterCloseAccepts.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := assertPostCloseGate(afterCloseAccepts, core.ExecutionRequest{}, core.AttemptScope{}, core.InvocationServices{}); err == nil {
		t.Fatal("post-Close-execution fixture passed lifecycle assertion")
	}
}

func assertPreInitGate(connector core.Connector, request core.ExecutionRequest, scope core.AttemptScope, services core.InvocationServices) error {
	if connector.Health(context.Background()).State == core.HealthReady {
		return errors.New("connector reports ready before Init")
	}
	_, gatewayErr := connector.Execute(context.Background(), request, scope, services)
	if gatewayErr == nil || gatewayErr.Category != core.CategoryUnavailable {
		return errors.New("Execute before Init did not return unavailable")
	}
	return nil
}

func assertPostCloseGate(connector core.Connector, request core.ExecutionRequest, scope core.AttemptScope, services core.InvocationServices) error {
	if connector.Health(context.Background()).State != core.HealthUnavailable {
		return fmt.Errorf("Health after Close = %q, want unavailable", connector.Health(context.Background()).State)
	}
	_, gatewayErr := connector.Execute(context.Background(), request, scope, services)
	if gatewayErr == nil || gatewayErr.Category != core.CategoryUnavailable {
		return errors.New("Execute after Close did not return unavailable")
	}
	return nil
}

func assertFailedInitCleanup(connector *partialInitConnector) error {
	if err := connector.Init(context.Background(), core.ComponentConfig{}); err == nil {
		return errors.New("failed-Init fixture unexpectedly initialized")
	}
	if connector.Health(context.Background()).State != core.HealthUnavailable {
		return errors.New("failed-Init fixture reported ready")
	}
	_, gatewayErr := connector.Execute(context.Background(), core.ExecutionRequest{}, core.AttemptScope{}, core.InvocationServices{})
	if gatewayErr == nil || gatewayErr.Category != core.CategoryUnavailable {
		return errors.New("failed-Init fixture admitted execution")
	}
	if err := connector.Close(context.Background()); err != nil {
		return fmt.Errorf("first Close: %w", err)
	}
	if err := connector.Close(context.Background()); err != nil {
		return fmt.Errorf("repeated Close: %w", err)
	}
	if connector.resource || connector.releaseCount != 1 || connector.closeCount != 1 {
		return fmt.Errorf("partial resource cleanup mismatch: resource=%v releases=%d closes=%d", connector.resource, connector.releaseCount, connector.closeCount)
	}
	return nil
}

// partialInitConnector is a deliberately small conformance fixture for the
// contract obligation the concrete native/scripted fixtures cannot exercise:
// both validate before allocating any resource that could need failed-Init cleanup.
type partialInitConnector struct {
	resource         bool
	leakOnClose      bool
	readyBeforeInit  bool
	acceptAfterClose bool
	releaseCount     int
	closeCount       int
	closed           bool
}

func (*partialInitConnector) Descriptor() core.Descriptor {
	return core.Descriptor{
		ID: "conformance.partial-init", Kind: core.ComponentConnector, ImplementationVersion: "test",
		APIVersions: []core.APIVersion{{Major: 1}}, Protocols: []string{protocol},
		Operations: []string{"execute"}, ConnectorType: "test",
	}
}

func (c *partialInitConnector) Init(context.Context, core.ComponentConfig) error {
	c.resource = true
	return errors.New("synthetic failure after allocation")
}

func (c *partialInitConnector) Health(context.Context) core.Health {
	if c.readyBeforeInit && !c.closed {
		return core.Health{State: core.HealthReady}
	}
	return core.Health{State: core.HealthUnavailable}
}

func (*partialInitConnector) Capabilities(context.Context, core.CapabilityScope) core.CapabilityResult {
	return core.CapabilityResult{}
}

func (c *partialInitConnector) Close(context.Context) error {
	if c.closed {
		return nil
	}
	c.closed = true
	c.closeCount++
	if c.resource && !c.leakOnClose {
		c.resource = false
		c.releaseCount++
	}
	return nil
}

func (c *partialInitConnector) Execute(context.Context, core.ExecutionRequest, core.AttemptScope, core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	if c.closed && c.acceptAfterClose {
		return core.ExecutionResponse{}, nil
	}
	return core.ExecutionResponse{}, &core.GatewayError{Code: "unavailable", Category: core.CategoryUnavailable, Message: "not initialized"}
}

func (*partialInitConnector) Models(context.Context, core.ModelQuery, core.InvocationServices) (core.ModelsResult, *core.GatewayError) {
	return core.ModelsResult{}, nil
}

func (*partialInitConnector) EstimateUsage(context.Context, core.UsageQuery, core.InvocationServices) (core.EstimateResult, *core.GatewayError) {
	return core.EstimateResult{}, nil
}

func (*partialInitConnector) Authenticate(context.Context, core.AuthRequest, core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	return core.AuthResult{}, nil
}

func TestConformanceNativeScopeCredentialsAndSupport(t *testing.T) {
	fx := nativeFixture(t)
	t.Cleanup(fx.close)
	if err := fx.init(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		request core.ExecutionRequest
		scope   core.AttemptScope
		service core.InvocationServices
		code    string
	}{
		{name: "missing-credentials", request: fx.request, scope: fx.scope, service: core.InvocationServices{}, code: "credential_unavailable"},
		{name: "account-mismatch", request: fx.request, scope: core.AttemptScope{AccountID: "other", Mode: "native"}, service: fx.services(), code: "scope_mismatch"},
		{name: "model-mismatch", request: core.ExecutionRequest{ID: request, Model: "other", Payload: fx.request.Payload}, scope: fx.scope, service: fx.services(), code: "scope_mismatch"},
		{name: "mode-mismatch", request: fx.request, scope: core.AttemptScope{AccountID: account, Mode: "translated"}, service: fx.services(), code: "unsupported_mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, gatewayErr := fx.connector.Execute(context.Background(), tc.request, tc.scope, tc.service)
			if gatewayErr == nil || gatewayErr.Code != tc.code {
				t.Fatalf("Execute error=%+v, want code %q", gatewayErr, tc.code)
			}
		})
	}
	if got := fx.callCount(); got != 0 {
		t.Fatalf("rejected scoped Execute reached upstream: %d calls", got)
	}
	services := fx.services()
	models, err := fx.connector.Models(context.Background(), core.ModelQuery{Protocol: protocol, Mode: "native", AccountID: account}, services)
	if err != nil || !models.Supported || len(models.Models) != 1 || models.Models[0].ID != model {
		t.Fatalf("valid scoped Models result=%+v error=%v", models, err)
	}
	wrongModels, err := fx.connector.Models(context.Background(), core.ModelQuery{Protocol: protocol, Mode: "native", AccountID: "other"}, services)
	if err != nil || wrongModels.Supported || len(wrongModels.Models) != 0 {
		t.Fatalf("cross-account Models result=%+v error=%v", wrongModels, err)
	}
	estimate, err := fx.connector.EstimateUsage(context.Background(), core.UsageQuery{Protocol: protocol, Mode: "native", Model: model, AccountID: account}, services)
	if err != nil || estimate.Supported || estimate.Known || estimate.Usage != nil {
		t.Fatalf("unsupported usage estimate fabricated result=%+v error=%v", estimate, err)
	}
	auth, err := fx.connector.Authenticate(context.Background(), core.AuthRequest{AccountID: account}, services)
	if err != nil || auth.Supported || auth.State != "" || len(auth.Credentials) != 0 {
		t.Fatalf("unsupported authentication fabricated result=%+v error=%v", auth, err)
	}
}

func rejectUnavailable(t *testing.T, fx fixture) {
	t.Helper()
	_, gatewayErr := fx.connector.Execute(context.Background(), fx.request, fx.scope, fx.services())
	if gatewayErr == nil || gatewayErr.Category != core.CategoryUnavailable {
		t.Fatalf("Execute error=%+v, want unavailable gateway error", gatewayErr)
	}
}

func descriptorsEqual(a, b core.Descriptor) bool {
	return a.ID == b.ID && a.Kind == b.Kind && a.ImplementationVersion == b.ImplementationVersion &&
		versionsEqual(a.APIVersions, b.APIVersions) && stringsEqual(a.Protocols, b.Protocols) &&
		stringsEqual(a.Operations, b.Operations) && a.ConnectorType == b.ConnectorType && stringsEqual(a.AuthMethods, b.AuthMethods)
}

func versionsEqual(a, b []core.APIVersion) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
	t.Setenv("PESTIROUTE_CONFORMANCE_TOKEN", "synthetic-token")
	return fixture{
		connector: connector,
		init:      func(ctx context.Context) error { return connector.Init(ctx, core.ComponentConfig{Data: config}) },
		failedInit: func() error {
			return connector.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"unknown":true}`)})
		},
		request: core.ExecutionRequest{ID: request, Model: model, Payload: core.RawPayload{
			Protocol: protocol, ContentType: "application/json", Body: []byte(`{"model":"gpt-5.4-mini","input":"test"}`),
		}},
		scope: core.AttemptScope{ID: "conformance-attempt", AccountID: account, Mode: "native"},
		services: func() core.InvocationServices {
			return core.NewEnvironmentServices(
				map[string]map[string]string{account: {"bearer": "PESTIROUTE_CONFORMANCE_TOKEN"}},
				map[string]core.HTTPDoer{account: connector.HTTPDoer()}, nil,
			).ForAttempt(core.AttemptScope{AccountID: account, Mode: "native"})
		},
		wantBody:  []byte(`{"status":"completed","opaque":{"native":true}}`),
		callCount: upstream.RequestCount,
		close: func() {
			if err := connector.Close(context.Background()); err != nil {
				t.Errorf("close connector: %v", err)
			}
			upstream.Close()
		},
	}
}

func nativeOpaqueFixture(t *testing.T, parts [][]byte) (fixture, func() []byte) {
	steps := make([]fakeupstream.Step, len(parts))
	for i, part := range parts {
		steps[i].Data = part
	}
	return nativeCustomFixture(t, fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: steps})
}

func nativeGatedFixture(t *testing.T, gate <-chan struct{}, sent chan<- struct{}) fixture {
	first := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"note\":\"☃\"}\n\n")
	terminal := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	fx, _ := nativeCustomFixture(t, fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: []fakeupstream.Step{{Data: first}, {Gate: gate, Sent: sent, Data: terminal}}})
	return fx
}

func nativeCustomFixture(t *testing.T, response fakeupstream.Response) (fixture, func() []byte) {
	t.Helper()
	upstream := fakeupstream.New(response)
	connector := responses.NewConnector()
	config, err := json.Marshal(map[string]any{"transport": map[string]any{"endpoint": upstream.URL + "/v1/responses"}, "model": model, "account_id": account})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PESTIROUTE_CONFORMANCE_TOKEN", "synthetic-token")
	var captured []byte
	fx := fixture{
		connector: connector,
		init:      func(ctx context.Context) error { return connector.Init(ctx, core.ComponentConfig{Data: config}) },
		request:   core.ExecutionRequest{ID: request, Model: model, Payload: core.RawPayload{Protocol: protocol, ContentType: "application/json", Body: append([]byte(nil), opaqueRequest...)}},
		scope:     core.AttemptScope{ID: "conformance-attempt", AccountID: account, Mode: "native"},
		services: func() core.InvocationServices {
			return core.NewEnvironmentServices(map[string]map[string]string{account: {"bearer": "PESTIROUTE_CONFORMANCE_TOKEN"}}, map[string]core.HTTPDoer{account: connector.HTTPDoer()}, nil).ForAttempt(core.AttemptScope{AccountID: account, Mode: "native"})
		},
		callCount: upstream.RequestCount,
		waitCancel: func() bool {
			var request fakeupstream.Request
			select {
			case request = <-upstream.Requests:
			case <-time.After(2 * time.Second):
				return false
			}
			for _, event := range []<-chan struct{}{request.Cancelled, request.Completed} {
				select {
				case <-event:
				case <-time.After(2 * time.Second):
					return false
				}
			}
			return request.CancelCount() == 1
		},
		close: func() {
			if err := connector.Close(context.Background()); err != nil {
				t.Errorf("close connector: %v", err)
			}
			upstream.Close()
		},
	}
	return fx, func() []byte {
		if captured == nil {
			captured = (<-upstream.Requests).Body
		}
		return captured
	}
}

func scriptedOpaqueFixture(t *testing.T, parts [][]byte) (fixture, func() []byte) {
	steps := []scripted.Step{{Frame: headFrame()}}
	for _, part := range parts {
		steps = append(steps, scripted.Step{Frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: part}}})
	}
	steps = append(steps, scripted.Step{Frame: completeFrame()})
	fx := newScriptedCustomFixture(t, steps)
	return fx, func() []byte { return fx.connector.(*scripted.Connector).Calls()[0].Request.Payload.Body }
}

func scriptedGatedFixture(t *testing.T, gate <-chan struct{}, waiting chan<- struct{}) fixture {
	first := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"note\":\"☃\"}\n\n")
	terminal := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	return newScriptedCustomFixture(t, []scripted.Step{{Frame: headFrame()}, {Frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: first}}}, {Frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: terminal}}, Gate: gate, Waiting: waiting}, {Frame: completeFrame()}})
}

func newScriptedCustomFixture(t *testing.T, steps []scripted.Step) fixture {
	return newScriptedScriptsFixture(t, []scripted.Script{{ID: request, Steps: steps}})
}

func newScriptedScriptsFixture(t *testing.T, scripts []scripted.Script) fixture {
	t.Helper()
	scope := core.CapabilityScope{Protocol: protocol, Mode: "native", Model: model, AccountID: account}
	connector := scripted.New(core.Descriptor{ID: "conformance.scripted.opaque", Kind: core.ComponentConnector, ImplementationVersion: "test", APIVersions: []core.APIVersion{{Major: 1}}, Protocols: []string{protocol}, Operations: []string{"execute"}, ConnectorType: "test", AuthMethods: []string{"bearer"}}, map[core.CapabilityScope]core.CapabilityResult{scope: {}}, scripts...)
	t.Setenv("PESTIROUTE_CONFORMANCE_TOKEN", "synthetic-token")
	return fixture{connector: connector, init: func(ctx context.Context) error { return connector.Init(ctx, core.ComponentConfig{}) }, request: core.ExecutionRequest{ID: request, Model: model, Payload: core.RawPayload{Protocol: protocol, ContentType: "application/json", Body: append([]byte(nil), opaqueRequest...)}}, scope: core.AttemptScope{ID: "conformance-attempt", AccountID: account, Mode: "native"}, services: func() core.InvocationServices {
		return core.NewEnvironmentServices(map[string]map[string]string{account: {"bearer": "PESTIROUTE_CONFORMANCE_TOKEN"}}, nil, nil).ForAttempt(core.AttemptScope{AccountID: account, Mode: "native"})
	}, callCount: func() int64 { return int64(connector.CallCount()) }, close: func() {
		if err := connector.Close(context.Background()); err != nil {
			t.Errorf("close connector: %v", err)
		}
	}}
}

func scriptedFixture(t *testing.T) fixture {
	t.Helper()
	scope := core.CapabilityScope{Protocol: protocol, Mode: "native", Model: model, AccountID: account}
	connector := scripted.New(core.Descriptor{
		ID: "conformance.scripted", Kind: core.ComponentConnector, ImplementationVersion: "test",
		APIVersions: []core.APIVersion{{Major: 1}}, Protocols: []string{protocol},
		Operations: []string{"execute"}, ConnectorType: "test", AuthMethods: []string{"bearer"},
	}, map[core.CapabilityScope]core.CapabilityResult{scope: {Values: map[core.Capability]core.CapabilityState{
		"llm.images": core.Unsupported,
	}}}, scripted.Script{ID: request, Steps: []scripted.Step{
		{Frame: core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: protocol, ContentType: "application/json"}}},
		{Frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: []byte(`{"status":"completed","opaque":{"scripted":true}}`)}}},
		{Frame: core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded}}},
	}})
	t.Setenv("PESTIROUTE_CONFORMANCE_TOKEN", "synthetic-token")
	return fixture{
		connector: connector,
		init:      func(ctx context.Context) error { return connector.Init(ctx, core.ComponentConfig{}) },
		failedInit: func() error {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return connector.Init(ctx, core.ComponentConfig{})
		},
		request:  core.ExecutionRequest{ID: request, Model: model, Payload: core.RawPayload{Protocol: protocol, ContentType: "application/json", Body: []byte(`{"model":"gpt-5.4-mini","input":"test"}`)}},
		scope:    core.AttemptScope{ID: "conformance-attempt", AccountID: account, Mode: "native"},
		wantBody: []byte(`{"status":"completed","opaque":{"scripted":true}}`),
		services: func() core.InvocationServices {
			return core.NewEnvironmentServices(
				map[string]map[string]string{account: {"bearer": "PESTIROUTE_CONFORMANCE_TOKEN"}}, nil, nil,
			).ForAttempt(core.AttemptScope{AccountID: account, Mode: "native"})
		},
		callCount: func() int64 { return int64(connector.CallCount()) },
		close: func() {
			if err := connector.Close(context.Background()); err != nil {
				t.Errorf("close connector: %v", err)
			}
		},
	}
}

func runBaseline(fx fixture) error {
	ctx := context.Background()
	if fx.init != nil {
		if err := fx.init(ctx); err != nil {
			return err
		}
	}
	var services core.InvocationServices
	if fx.services != nil {
		services = fx.services()
	}
	result, gatewayErr := fx.connector.Execute(ctx, fx.request, fx.scope, services)
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
	if fx.validate != nil {
		if err := fx.validate(); err != nil {
			return err
		}
	}
	if !head || !complete || len(body) == 0 || (!fx.translated && !bytes.Equal(body, fx.wantBody)) {
		return errors.New("missing Head, Body, successful Complete, or expected response bytes")
	}
	if fx.translated && (!bytes.Contains(body, []byte(`"type":"response.output_text.delta"`)) || !bytes.Contains(body, []byte(`"type":"response.completed"`))) {
		return errors.New("translation omitted declared output-text or completion events")
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
