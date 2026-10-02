package core_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/testutil/scripted"
)

const routedFailureProtocol = "openai.responses.v1"

func TestRoutedFailures(t *testing.T) {
	ctx := context.Background()
	registry, err := core.NewRegistry(map[core.ComponentKind]core.APIVersion{
		core.ComponentAdapter: {Major: 1}, core.ComponentConnector: {Major: 1},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := registry.Close(context.Background()); err != nil {
			t.Errorf("close registry: %v", err)
		}
	})

	models := []string{"model-a", "model-b", "model-c"}
	accounts := []string{"account-a", "account-b", "account-a"}
	capability := core.Capability("vendor.required")
	capScopes := make(map[core.CapabilityScope]core.CapabilityResult, len(models))
	for i, model := range models {
		scope := core.CapabilityScope{Protocol: routedFailureProtocol, Mode: core.ModeNative, Model: model, AccountID: accounts[i]}
		capScopes[scope] = core.CapabilityResult{Values: map[core.Capability]core.CapabilityState{}}
		if model == "model-a" {
			capScopes[scope] = core.CapabilityResult{Values: map[core.Capability]core.CapabilityState{capability: core.Supported}}
		}
	}
	adapterDescriptor := routedDescriptor(core.ComponentAdapter)
	adapterDescriptor.ID = "routed.adapter"
	adapter := &routedAdapter{descriptor: adapterDescriptor, caps: capScopes}
	if err := registry.Register("adapter", adapter, core.ComponentAdapter); err != nil {
		t.Fatal(err)
	}

	preErr := &core.GatewayError{Code: "synthetic_prehead", Category: core.CategoryUnavailable, Retryable: true, RetryDisposition: core.RetrySafe, Message: "Unavailable"}
	status := 200
	connectors := []*scripted.Connector{
		scripted.New(routedScriptDescriptor("routed.connector.a"), capScopes, scripted.Script{ExecuteError: preErr}),
		scripted.New(routedScriptDescriptor("routed.connector.b"), capScopes,
			scripted.Script{Steps: []scripted.Step{
				{Frame: core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: routedFailureProtocol, HTTPStatus: &status}}},
				{Frame: core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded}}},
			}}),
		scripted.New(routedScriptDescriptor("routed.connector.c"), capScopes,
			scripted.Script{Steps: []scripted.Step{
				{Frame: core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: routedFailureProtocol, HTTPStatus: &status}}},
				{Err: errors.New("synthetic stream interruption")},
			}}),
	}
	routes := make([]core.Route, len(models))
	for i, model := range models {
		id := core.InstanceID("connector-" + string(rune('a'+i)))
		if err := registry.Register(id, connectors[i], core.ComponentConnector); err != nil {
			t.Fatal(err)
		}
		if err := registry.Init(ctx, id, core.ComponentConfig{}); err != nil {
			t.Fatal(err)
		}
		routes[i] = core.Route{
			Identity: core.RouteIdentity{RouteLookupKey: core.RouteLookupKey{Protocol: routedFailureProtocol, Mode: core.ModeNative, Model: model}, AccountID: accounts[i]},
			Adapter:  "adapter", Connector: id,
		}
	}
	unavailableScope := core.CapabilityScope{Protocol: routedFailureProtocol, Mode: core.ModeNative, Model: "unavailable-model", AccountID: accounts[0]}
	capScopes[unavailableScope] = core.CapabilityResult{}
	unavailableConnector := scripted.New(routedScriptDescriptor("routed.connector.unavailable"), capScopes, scripted.Script{Steps: []scripted.Step{{
		Frame: core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: routedFailureProtocol}},
	}}})
	if err := registry.Register("connector-unavailable", unavailableConnector, core.ComponentConnector); err != nil {
		t.Fatal(err)
	}
	if err := registry.Init(ctx, "adapter", core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	table, err := core.NewRouteTable(routes, registry)
	if err != nil {
		t.Fatal(err)
	}
	observations, err := core.NewInMemoryAttemptObservations(8)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &core.Dispatcher{Routes: table, Services: routedServices{}, Observations: observations}
	newRequest := func(model, account string) core.ExecutionRequest {
		return core.ExecutionRequest{ID: "client-controlled-id", Model: model, Payload: core.RawPayload{
			Protocol: routedFailureProtocol, ContentType: "application/json", Body: []byte{'{', 0, 0xff, '}'},
		}, Metadata: core.RequestMetadata{Extensions: map[string]any{"account": "attacker-account", "attempt_id": "forged"}}}
	}
	consume := func(response core.ExecutionResponse) error {
		for {
			_, err := response.Stream.Next(ctx)
			if err != nil {
				return err
			}
		}
	}

	// The first route fails before a stream exists. Retry metadata is preserved,
	// but the one-attempt dispatcher must not try another route.
	dispatcher.AccountID = accounts[0]
	if _, got := dispatcher.Execute(ctx, newRequest(models[0], accounts[0])); got == nil || got.Code != preErr.Code || !got.Retryable || got.RetryDisposition != core.RetrySafe {
		t.Fatalf("pre-Head failure metadata = %+v", got)
	}
	preObservation := observations.Snapshot()[0]
	if preObservation.Outcome != core.OutcomeFailed || preObservation.Committed || !preObservation.Retryable || preObservation.RetryDisposition != core.RetrySafe || preObservation.Connector != routes[0].Connector {
		t.Fatalf("pre-Head attempt observation = %+v", preObservation)
	}

	// A missing exact route is rejected before connector execution.
	if _, got := dispatcher.Execute(ctx, newRequest("unconfigured-model", accounts[0])); got == nil || got.Code != "unsupported_target" || got.Category != core.CategoryUnsupportedFeature {
		t.Fatalf("unroutable model error = %+v", got)
	}
	unavailableRoutes, err := core.NewRouteTable(append(append([]core.Route(nil), routes...), core.Route{
		Identity: core.RouteIdentity{RouteLookupKey: core.RouteLookupKey{Protocol: routedFailureProtocol, Mode: core.ModeNative, Model: "unavailable-model"}, AccountID: accounts[0]},
		Adapter:  "adapter", Connector: "connector-unavailable",
	}), registry)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.Routes = unavailableRoutes
	if _, got := dispatcher.Execute(ctx, newRequest("unavailable-model", accounts[0])); got == nil || got.Code != "unsupported_target" {
		t.Fatalf("unavailable component error = %+v", got)
	}
	dispatcher.Routes = table

	// Unknown mandatory capability is rejected before Execute.
	dispatcher.AccountID = accounts[1]
	unsupported := newRequest(models[1], accounts[1])
	unsupported.Capabilities = map[core.Capability]struct{}{capability: {}}
	if _, got := dispatcher.Execute(ctx, unsupported); got == nil || got.Code != "unsupported_capability" || got.Category != core.CategoryUnsupportedFeature {
		t.Fatalf("unsupported capability error = %+v", got)
	}

	// Mode/policy mismatch and a missing component are likewise pre-execution.
	dispatcher.Mode = "translation"
	if _, got := dispatcher.Execute(ctx, newRequest(models[1], accounts[1])); got == nil || got.Code != "unsupported_target" {
		t.Fatalf("mode mismatch error = %+v", got)
	}
	dispatcher.Mode = ""
	if _, err := core.NewRouteTable(append(append([]core.Route(nil), routes...), core.Route{
		Identity: core.RouteIdentity{RouteLookupKey: core.RouteLookupKey{Protocol: routedFailureProtocol, Mode: core.ModeNative, Model: "missing-component"}, AccountID: accounts[0]},
		Adapter:  "adapter", Connector: "not-registered",
	}), registry); err == nil {
		t.Fatal("route with missing connector was accepted")
	}
	if connectors[0].CallCount() != 1 || connectors[1].CallCount() != 0 || connectors[2].CallCount() != 0 || unavailableConnector.CallCount() != 0 {
		t.Fatalf("pre-execute/fallback connector counts = %d, %d, %d, unavailable %d", connectors[0].CallCount(), connectors[1].CallCount(), connectors[2].CallCount(), unavailableConnector.CallCount())
	}

	// A stream interruption after Head is committed and never replayed. The v1
	// contract classifies an unexpected post-Head transport error as incomplete.
	dispatcher.AccountID = accounts[2]
	response, got := dispatcher.Execute(ctx, newRequest(models[2], accounts[2]))
	if got != nil {
		t.Fatal(got)
	}
	if _, err := response.Stream.Next(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := response.Stream.Next(ctx); err == nil || err == io.EOF {
		t.Fatalf("post-Head stream interruption = %v", err)
	}
	postObservation := observations.Snapshot()[1]
	if postObservation.Outcome != core.OutcomeIncomplete || !postObservation.Committed || postObservation.Connector != routes[2].Connector {
		t.Fatalf("post-Head attempt observation = %+v", postObservation)
	}
	if connectors[2].CallCount() != 1 || connectors[1].CallCount() != 0 {
		t.Fatalf("post-Head failure was replayed: calls = %d, %d", connectors[2].CallCount(), connectors[1].CallCount())
	}

	// Independent route succeeds immediately after the failed route, with exact
	// opaque request bytes and runtime-owned identities confined to that call.
	dispatcher.AccountID = accounts[1]
	successRequest := newRequest(models[1], accounts[1])
	successRequest.Capabilities = nil
	success, got := dispatcher.Execute(ctx, successRequest)
	if got != nil {
		t.Fatal(got)
	}
	if err := consume(success); err != io.EOF {
		t.Fatalf("healthy route terminal read = %v", err)
	}
	call := connectors[1].Calls()[0]
	if call.Request.ID == "" || call.Request.ID == successRequest.ID || call.Scope.ID == "" || call.Scope.ID == call.Request.ID || call.Scope.AccountID != accounts[1] || call.Scope.Mode != core.ModeNative ||
		call.Request.Model != models[1] || string(call.Request.Payload.Body) != string(successRequest.Payload.Body) || call.Request.Payload.Protocol != successRequest.Payload.Protocol {
		t.Fatalf("selected call identity/payload changed: %+v", call)
	}
	if len(call.Request.Metadata.Extensions) != 2 || call.Request.Metadata.Extensions["attempt_id"] != "forged" {
		t.Fatalf("client metadata was rewritten: %+v", call.Request.Metadata.Extensions)
	}
	all := observations.Snapshot()
	if len(all) != 3 || all[0].Connector != routes[0].Connector || all[1].Connector != routes[2].Connector || all[2].Connector != routes[1].Connector || all[2].Outcome != core.OutcomeSucceeded || !all[2].Committed || all[2].AccountID != accounts[1] {
		t.Fatalf("routed observations crossed attempts/routes: %+v", all)
	}
	if connectors[0].CallCount() != 1 || connectors[1].CallCount() != 1 || connectors[2].CallCount() != 1 {
		t.Fatalf("unexpected route/fallback calls: %d, %d, %d", connectors[0].CallCount(), connectors[1].CallCount(), connectors[2].CallCount())
	}
}

func routedScriptDescriptor(id string) core.Descriptor {
	descriptor := routedDescriptor(core.ComponentConnector)
	descriptor.ID = id
	descriptor.Operations = []string{"execute"}
	return descriptor
}

type routedAdapter struct {
	descriptor core.Descriptor
	caps       map[core.CapabilityScope]core.CapabilityResult
}

func (a *routedAdapter) Descriptor() core.Descriptor                    { return a.descriptor.Clone() }
func (*routedAdapter) Init(context.Context, core.ComponentConfig) error { return nil }
func (*routedAdapter) Health(context.Context) core.Health {
	return core.Health{State: core.HealthReady}
}
func (a *routedAdapter) Capabilities(_ context.Context, scope core.CapabilityScope) core.CapabilityResult {
	return a.caps[scope].Clone()
}
func (*routedAdapter) Close(context.Context) error { return nil }

type routedServices struct{}

func (routedServices) ForAttempt(core.AttemptScope) core.InvocationServices {
	return core.InvocationServices{}
}

func routedDescriptor(kind core.ComponentKind) core.Descriptor {
	d := core.Descriptor{ID: "routed", Kind: kind, ImplementationVersion: "test", APIVersions: []core.APIVersion{{Major: 1}}, Protocols: []string{routedFailureProtocol}, Operations: []string{"execute"}}
	if kind == core.ComponentConnector {
		d.ConnectorType = "test"
	}
	return d
}
