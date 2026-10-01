package core

import (
	"context"
	"fmt"
)

const ModeNative = "native"

type RouteLookupKey struct {
	Protocol string
	Mode     string
	Model    string
}

type RouteIdentity struct {
	RouteLookupKey
	AccountID string
}

type SelectionContext struct {
	Mode      string
	AccountID string
}

type Route struct {
	Identity  RouteIdentity
	Adapter   InstanceID
	Connector InstanceID
}

type RouteSelection struct {
	Route     Route
	Adapter   Component
	Connector Component
}

// RouteTable contains explicit native targets. Its registry owns lifecycle and
// availability; the table only owns immutable route declarations.
type RouteTable struct {
	registry *Registry
	routes   map[RouteIdentity]Route
}

func NewRouteTable(routes []Route, registry *Registry) (*RouteTable, error) {
	if registry == nil {
		return nil, fmt.Errorf("route registry is required")
	}
	table := &RouteTable{registry: registry, routes: make(map[RouteIdentity]Route, len(routes))}
	for _, route := range routes {
		id := route.Identity
		if id.Protocol == "" || id.Mode == "" || id.Model == "" || id.AccountID == "" {
			return nil, fmt.Errorf("route protocol, mode, model, and account are required")
		}
		if id.Mode != ModeNative {
			return nil, fmt.Errorf("route mode %q is unsupported", id.Mode)
		}
		if route.Adapter == "" || route.Connector == "" {
			return nil, fmt.Errorf("route adapter and connector instances are required")
		}
		if _, exists := table.routes[id]; exists {
			return nil, fmt.Errorf("duplicate route identity %+v", id)
		}
		_, adapter, ok := registry.Lookup(route.Adapter)
		if !ok || adapter.Kind != ComponentAdapter || !declaresProtocol(adapter, id.Protocol) {
			return nil, fmt.Errorf("route adapter instance %q is missing, mismatched, or does not declare protocol %q", route.Adapter, id.Protocol)
		}
		_, connector, ok := registry.Lookup(route.Connector)
		if !ok || connector.Kind != ComponentConnector || !declaresProtocol(connector, id.Protocol) {
			return nil, fmt.Errorf("route connector instance %q is missing, mismatched, or does not declare protocol %q", route.Connector, id.Protocol)
		}
		table.routes[id] = route
	}
	return table, nil
}

// Select resolves only the exact decoded protocol/model and trusted runtime
// mode/account. Client metadata is deliberately not consulted.
func (t *RouteTable) Select(ctx context.Context, request ExecutionRequest, selection SelectionContext) (RouteSelection, error) {
	if t == nil || t.registry == nil {
		return RouteSelection{}, fmt.Errorf("route table is unavailable")
	}
	if selection.Mode != ModeNative || selection.AccountID == "" {
		return RouteSelection{}, fmt.Errorf("trusted native mode and selected account are required")
	}
	identity := RouteIdentity{RouteLookupKey: RouteLookupKey{
		Protocol: request.Payload.Protocol, Mode: selection.Mode, Model: request.Model,
	}, AccountID: selection.AccountID}
	route, ok := t.routes[identity]
	if !ok {
		return RouteSelection{}, fmt.Errorf("no route for protocol %q, mode %q, model %q, account %q", identity.Protocol, identity.Mode, identity.Model, identity.AccountID)
	}
	adapter, adapterDescriptor, adapterReady := t.registry.Admit(ctx, route.Adapter)
	connector, connectorDescriptor, connectorReady := t.registry.Admit(ctx, route.Connector)
	if !adapterReady || adapterDescriptor.Kind != ComponentAdapter {
		return RouteSelection{}, fmt.Errorf("route adapter instance %q is unavailable or ineligible", route.Adapter)
	}
	if !connectorReady || connectorDescriptor.Kind != ComponentConnector {
		return RouteSelection{}, fmt.Errorf("route connector instance %q is unavailable or ineligible", route.Connector)
	}
	return RouteSelection{Route: route, Adapter: adapter, Connector: connector}, nil
}
