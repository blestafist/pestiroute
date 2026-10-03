package core

import (
	"context"
	"testing"
)

func routeRegistry(t *testing.T) (*Registry, *registryComponent, *registryComponent) {
	t.Helper()
	r, err := NewRegistry(map[ComponentKind]APIVersion{
		ComponentAdapter: {Major: 1}, ComponentConnector: {Major: 1},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &registryComponent{descriptor: validDescriptor(ComponentAdapter)}
	c := &registryComponent{descriptor: validDescriptor(ComponentConnector)}
	if err := r.Register("adapter", a, ComponentAdapter); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("connector", c, ComponentConnector); err != nil {
		t.Fatal(err)
	}
	return r, a, c
}

type routeHealthComponent struct {
	registryComponent
	health      HealthState
	healthCalls int
}

func (c *routeHealthComponent) Health(context.Context) Health {
	c.healthCalls++
	return Health{State: c.health}
}

func routeRegistryWithPairs(t *testing.T) (*Registry, *routeHealthComponent, *routeHealthComponent, *routeHealthComponent, *routeHealthComponent) {
	t.Helper()
	r, err := NewRegistry(map[ComponentKind]APIVersion{
		ComponentAdapter: {Major: 1}, ComponentConnector: {Major: 1},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	makeComponent := func(kind ComponentKind) *routeHealthComponent {
		return &routeHealthComponent{registryComponent: registryComponent{descriptor: validDescriptor(kind)}, health: HealthReady}
	}
	a1, c1, a2, c2 := makeComponent(ComponentAdapter), makeComponent(ComponentConnector), makeComponent(ComponentAdapter), makeComponent(ComponentConnector)
	for _, item := range []struct {
		id        InstanceID
		component *routeHealthComponent
		kind      ComponentKind
	}{
		{"adapter-a", a1, ComponentAdapter}, {"connector-a", c1, ComponentConnector},
		{"adapter-b", a2, ComponentAdapter}, {"connector-b", c2, ComponentConnector},
	} {
		if err := r.Register(item.id, item.component, item.kind); err != nil {
			t.Fatal(err)
		}
	}
	return r, a1, c1, a2, c2
}

func initRouteComponents(t *testing.T, r *Registry) {
	t.Helper()
	for _, id := range []InstanceID{"adapter", "connector", "adapter-a", "connector-a", "adapter-b", "connector-b"} {
		if _, _, ok := r.Lookup(id); !ok {
			continue
		}
		if err := r.Init(context.Background(), id, ComponentConfig{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRouteTableValidation(t *testing.T) {
	r, _, _ := routeRegistry(t)
	good := Route{Identity: RouteIdentity{RouteLookupKey: RouteLookupKey{Protocol: "openai.responses.v1", Mode: ModeNative, Model: "m"}, AccountID: "a"}, Adapter: "adapter", Connector: "connector"}
	if _, err := NewRouteTable([]Route{good}, r); err != nil {
		t.Fatalf("valid route rejected: %v", err)
	}
	for _, tc := range []struct {
		name  string
		route Route
	}{
		{"empty protocol", func() Route { x := good; x.Identity.Protocol = ""; return x }()},
		{"empty mode", func() Route { x := good; x.Identity.Mode = ""; return x }()},
		{"empty model", func() Route { x := good; x.Identity.Model = ""; return x }()},
		{"empty account", func() Route { x := good; x.Identity.AccountID = ""; return x }()},
		{"unsupported mode", func() Route { x := good; x.Identity.Mode = "translate"; return x }()},
		{"unknown adapter", func() Route { x := good; x.Adapter = "missing"; return x }()},
		{"wrong adapter kind", func() Route { x := good; x.Adapter = "connector"; return x }()},
		{"wrong protocol", func() Route { x := good; x.Identity.Protocol = "other"; return x }()},
		{"empty adapter instance", func() Route { x := good; x.Adapter = ""; return x }()},
		{"empty connector instance", func() Route { x := good; x.Connector = ""; return x }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRouteTable([]Route{tc.route}, r); err == nil {
				t.Fatal("invalid route accepted")
			}
		})
	}
	if _, err := NewRouteTable([]Route{good, good}, r); err == nil {
		t.Fatal("duplicate identity accepted")
	}
	wrongProtocol := &registryComponent{descriptor: validDescriptor(ComponentConnector)}
	wrongProtocol.descriptor.Protocols = []string{"other.protocol.v1"}
	if err := r.Register("connector-other-protocol", wrongProtocol, ComponentConnector); err != nil {
		t.Fatal(err)
	}
	badConnector := good
	badConnector.Connector = "missing-connector"
	if _, err := NewRouteTable([]Route{badConnector}, r); err == nil {
		t.Fatal("unknown connector accepted")
	}
	badConnector.Connector = "adapter"
	if _, err := NewRouteTable([]Route{badConnector}, r); err == nil {
		t.Fatal("adapter accepted as connector")
	}
	badConnector.Connector = "connector-other-protocol"
	if _, err := NewRouteTable([]Route{badConnector}, r); err == nil {
		t.Fatal("connector protocol mismatch accepted")
	}
}

func TestRouteSelectionExactTrustedIdentityAndFailClosed(t *testing.T) {
	r, _, _ := routeRegistry(t)
	first := Route{Identity: RouteIdentity{RouteLookupKey: RouteLookupKey{Protocol: "openai.responses.v1", Mode: ModeNative, Model: "m1"}, AccountID: "a"}, Adapter: "adapter", Connector: "connector"}
	second := first
	second.Identity.Model, second.Identity.AccountID = "m2", "b"
	table, err := NewRouteTable([]Route{first, second}, r)
	if err != nil {
		t.Fatal(err)
	}
	request := ExecutionRequest{Model: "m1", Payload: RawPayload{Protocol: "openai.responses.v1", Body: []byte(`{"model":"m1","x":1}`)}, Metadata: RequestMetadata{Extensions: map[string]any{"account": "b"}}}
	original := append([]byte(nil), request.Payload.Body...)
	if _, err := table.Select(context.Background(), request, SelectionContext{Mode: ModeNative}); err == nil {
		t.Fatal("missing trusted account selected a route")
	}
	initRouteComponents(t, r)
	selection, err := table.Select(context.Background(), request, SelectionContext{Mode: ModeNative, AccountID: "a"})
	if err != nil || selection.Route.Identity != first.Identity {
		t.Fatalf("wrong route selected: %+v, %v", selection, err)
	}
	if request.Model != "m1" || string(request.Payload.Body) != string(original) {
		t.Fatal("selection mutated request model or payload")
	}
	if _, err := table.Select(context.Background(), request, SelectionContext{Mode: ModeNative, AccountID: "b"}); err == nil {
		t.Fatal("client account metadata influenced selection")
	}
	request.Payload.Protocol = "other.protocol.v1"
	if _, err := table.Select(context.Background(), request, SelectionContext{Mode: ModeNative, AccountID: "a"}); err == nil {
		t.Fatal("payload protocol mismatch selected a route")
	}
	request.Payload.Protocol = "openai.responses.v1"
	if _, err := table.Select(context.Background(), request, SelectionContext{Mode: "translate", AccountID: "a"}); err == nil {
		t.Fatal("unsupported mode selected route")
	}
	request.Model = "missing"
	if _, err := table.Select(context.Background(), request, SelectionContext{Mode: ModeNative, AccountID: "a"}); err == nil {
		t.Fatal("missing route selected")
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	request.Model = "m1"
	if _, err := table.Select(context.Background(), request, SelectionContext{Mode: ModeNative, AccountID: "a"}); err == nil {
		t.Fatal("closed components remained selectable")
	}
}

func TestRouteSelectionRequiresInitializedComponents(t *testing.T) {
	r, _, _ := routeRegistry(t)
	route := Route{Identity: RouteIdentity{RouteLookupKey: RouteLookupKey{Protocol: "openai.responses.v1", Mode: ModeNative, Model: "m"}, AccountID: "a"}, Adapter: "adapter", Connector: "connector"}
	table, err := NewRouteTable([]Route{route}, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := table.Select(context.Background(), ExecutionRequest{Model: "m", Payload: RawPayload{Protocol: "openai.responses.v1"}}, SelectionContext{Mode: ModeNative, AccountID: "a"}); err == nil {
		t.Fatal("uninitialized components were selectable")
	}
	initRouteComponents(t, r)
	if _, err := table.Select(context.Background(), ExecutionRequest{Model: "m", Payload: RawPayload{Protocol: "openai.responses.v1"}}, SelectionContext{Mode: ModeNative, AccountID: "a"}); err != nil {
		t.Fatalf("initialized components not selected: %v", err)
	}
}

func TestRouteSelectionRejectsUnconfiguredValidModel(t *testing.T) {
	r, adapter, connector, _, _ := routeRegistryWithPairs(t)
	route := Route{Identity: RouteIdentity{RouteLookupKey: RouteLookupKey{Protocol: "openai.responses.v1", Mode: ModeNative, Model: "gpt-5.4-mini"}, AccountID: "a"}, Adapter: "adapter-a", Connector: "connector-a"}
	table, err := NewRouteTable([]Route{route}, r)
	if err != nil {
		t.Fatal(err)
	}
	initRouteComponents(t, r)
	adapter.healthCalls, connector.healthCalls = 0, 0
	request := ExecutionRequest{Model: "vendor/model:preview-2", Payload: RawPayload{Protocol: "openai.responses.v1"}}
	if _, err := table.Select(context.Background(), request, SelectionContext{Mode: ModeNative, AccountID: "a"}); err == nil || err.Error() != `no route for protocol "openai.responses.v1", mode "native", model "vendor/model:preview-2", account "a"` {
		t.Fatalf("unconfigured valid model error = %v", err)
	}
	if adapter.healthCalls != 0 || connector.healthCalls != 0 {
		t.Fatalf("unconfigured model reached target admission: adapter health calls=%d, connector health calls=%d", adapter.healthCalls, connector.healthCalls)
	}
}

func TestRouteSelectionUsesDistinctConfiguredPairs(t *testing.T) {
	r, adapterA, connectorA, adapterB, connectorB := routeRegistryWithPairs(t)
	routes := []Route{
		{Identity: RouteIdentity{RouteLookupKey: RouteLookupKey{Protocol: "openai.responses.v1", Mode: ModeNative, Model: "m1"}, AccountID: "account-a"}, Adapter: "adapter-a", Connector: "connector-a"},
		{Identity: RouteIdentity{RouteLookupKey: RouteLookupKey{Protocol: "openai.responses.v1", Mode: ModeNative, Model: "m2"}, AccountID: "account-b"}, Adapter: "adapter-b", Connector: "connector-b"},
	}
	table, err := NewRouteTable(routes, r)
	if err != nil {
		t.Fatal(err)
	}
	initRouteComponents(t, r)
	for i, tc := range []struct {
		model, account string
		adapter        Component
		connector      Component
	}{
		{"m1", "account-a", adapterA, connectorA},
		{"m2", "account-b", adapterB, connectorB},
	} {
		request := ExecutionRequest{Model: tc.model, Payload: RawPayload{Protocol: "openai.responses.v1"}}
		selection, err := table.Select(context.Background(), request, SelectionContext{Mode: ModeNative, AccountID: tc.account})
		if err != nil {
			t.Fatalf("route %d selection failed: %v", i, err)
		}
		if selection.Route.Adapter != routes[i].Adapter || selection.Route.Connector != routes[i].Connector {
			t.Fatalf("route %d returned IDs (%q, %q)", i, selection.Route.Adapter, selection.Route.Connector)
		}
		if selection.Adapter != tc.adapter || selection.Connector != tc.connector {
			t.Fatalf("route %d returned incorrect component instances", i)
		}
	}
}

func TestRouteCandidatesPreserveDeclarationOrder(t *testing.T) {
	r, _, _, _, _ := routeRegistryWithPairs(t)
	routes := []Route{
		{Identity: RouteIdentity{RouteLookupKey: RouteLookupKey{Protocol: "openai.responses.v1", Mode: ModeNative, Model: "m"}, AccountID: "account-a"}, Adapter: "adapter-a", Connector: "connector-a", CandidateGroup: "route", MaxBodyBytes: 1024, MaxHeaderBytes: 1024, Requirements: []Capability{"route.required"}},
		{Identity: RouteIdentity{RouteLookupKey: RouteLookupKey{Protocol: "openai.responses.v1", Mode: ModeNative, Model: "m"}, AccountID: "account-b"}, Adapter: "adapter-b", Connector: "connector-b", CandidateGroup: "route", MaxBodyBytes: 1024, MaxHeaderBytes: 1024, Requirements: []Capability{"route.required"}},
	}
	table, err := NewRouteTable(routes, r)
	if err != nil {
		t.Fatal(err)
	}
	request := ExecutionRequest{Model: "m", Payload: RawPayload{Protocol: "openai.responses.v1"}}
	got := table.Candidates(request, ModeNative, "account-a")
	if len(got) != 2 || got[0].Identity.AccountID != "account-a" || got[1].Identity.AccountID != "account-b" {
		t.Fatalf("candidate order = %+v", got)
	}
	got[0].Identity.AccountID = "mutated"
	got[0].Requirements[0] = "mutated"
	if candidate := table.Candidates(request, ModeNative, "account-a"); candidate[0].Identity.AccountID != "account-a" || candidate[0].Requirements[0] != "route.required" {
		t.Fatal("candidate result mutated route table")
	}
	if _, err := NewRouteTable([]Route{routes[0], routes[0]}, r); err == nil {
		t.Fatal("duplicate target accepted")
	}
}

func TestRouteSelectionRejectsUnavailableHealth(t *testing.T) {
	for _, target := range []string{"adapter", "connector"} {
		t.Run(target, func(t *testing.T) {
			r, adapter, connector, _, _ := routeRegistryWithPairs(t)
			route := Route{Identity: RouteIdentity{RouteLookupKey: RouteLookupKey{Protocol: "openai.responses.v1", Mode: ModeNative, Model: "m"}, AccountID: "a"}, Adapter: "adapter-a", Connector: "connector-a"}
			table, err := NewRouteTable([]Route{route}, r)
			if err != nil {
				t.Fatal(err)
			}
			initRouteComponents(t, r)
			for _, id := range []InstanceID{"adapter-a", "connector-a"} {
				if _, _, ready := r.Admit(context.Background(), id); !ready {
					t.Fatalf("fixture component %q did not reach ready state", id)
				}
			}
			if target == "adapter" {
				adapter.health = HealthUnavailable
			} else {
				connector.health = HealthUnavailable
			}
			if _, err := table.Select(context.Background(), ExecutionRequest{Model: "m", Payload: RawPayload{Protocol: "openai.responses.v1"}}, SelectionContext{Mode: ModeNative, AccountID: "a"}); err == nil {
				t.Fatalf("unavailable %s remained selectable", target)
			}
		})
	}
}
