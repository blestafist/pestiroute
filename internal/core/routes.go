package core

import (
	"context"
	"fmt"
	"slices"
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
	Identity       RouteIdentity
	Adapter        InstanceID
	Connector      InstanceID
	CandidateGroup string
	MaxBodyBytes   int64
	MaxHeaderBytes int64
	Requirements   []Capability
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
	routes   map[RouteLookupKey][]Route
}

func NewRouteTable(routes []Route, registry *Registry) (*RouteTable, error) {
	if registry == nil {
		return nil, fmt.Errorf("route registry is required")
	}
	table := &RouteTable{registry: registry, routes: make(map[RouteLookupKey][]Route, len(routes))}
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
		if route.CandidateGroup != "" && (route.MaxBodyBytes <= 0 || route.MaxHeaderBytes <= 0) {
			return nil, fmt.Errorf("candidate target body and header limits must be positive")
		}
		seenRequirements := make(map[Capability]struct{}, len(route.Requirements))
		for _, capability := range route.Requirements {
			if capability == "" {
				return nil, fmt.Errorf("route requirements must be non-empty")
			}
			if _, exists := seenRequirements[capability]; exists {
				return nil, fmt.Errorf("duplicate route capability requirement %q", capability)
			}
			seenRequirements[capability] = struct{}{}
		}
		key := id.RouteLookupKey
		for _, previous := range table.routes[key] {
			if previous.CandidateGroup != route.CandidateGroup {
				return nil, fmt.Errorf("route target group conflicts for %+v", key)
			}
			if !slices.Equal(previous.Requirements, route.Requirements) {
				return nil, fmt.Errorf("route target requirements conflict for %+v", key)
			}
			if previous.Identity.AccountID == id.AccountID {
				return nil, fmt.Errorf("duplicate or conflicting route target for %+v", key)
			}
		}
		_, adapter, ok := registry.Lookup(route.Adapter)
		if !ok || adapter.Kind != ComponentAdapter || !declaresProtocol(adapter, id.Protocol) {
			return nil, fmt.Errorf("route adapter instance %q is missing, mismatched, or does not declare protocol %q", route.Adapter, id.Protocol)
		}
		_, connector, ok := registry.Lookup(route.Connector)
		if !ok || connector.Kind != ComponentConnector || !declaresProtocol(connector, id.Protocol) {
			return nil, fmt.Errorf("route connector instance %q is missing, mismatched, or does not declare protocol %q", route.Connector, id.Protocol)
		}
		table.routes[key] = append(table.routes[key], route)
		table.routes[key][len(table.routes[key])-1].Requirements = slices.Clone(route.Requirements)
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
	key := RouteLookupKey{Protocol: request.Payload.Protocol, Mode: selection.Mode, Model: request.Model}
	for _, route := range t.routes[key] {
		if route.Identity.AccountID == selection.AccountID {
			return t.selectRoute(ctx, route)
		}
	}
	return RouteSelection{}, fmt.Errorf("no route for protocol %q, mode %q, model %q, account %q", key.Protocol, key.Mode, key.Model, selection.AccountID)
}

// Candidates returns configured targets for the exact decoded route, in declaration order.
// The returned set is immutable topology; callers must still authorize each target.
func (t *RouteTable) Candidates(request ExecutionRequest, mode, accountID string) []Route {
	if t == nil || mode != ModeNative {
		return nil
	}
	routes := t.routes[RouteLookupKey{Protocol: request.Payload.Protocol, Mode: mode, Model: request.Model}]
	if len(routes) == 0 {
		return nil
	}
	if routes[0].CandidateGroup == "" {
		for _, route := range routes {
			if route.Identity.AccountID == accountID {
				return []Route{cloneRoute(route)}
			}
		}
		return nil
	}
	result := make([]Route, len(routes))
	for i, route := range routes {
		result[i] = cloneRoute(route)
	}
	return result
}

func cloneRoute(route Route) Route {
	route.Requirements = slices.Clone(route.Requirements)
	return route
}

func (t *RouteTable) selectRoute(ctx context.Context, route Route) (RouteSelection, error) {
	adapter, adapterDescriptor, adapterReady := t.registry.Admit(ctx, route.Adapter)
	connector, connectorDescriptor, connectorReady := t.registry.Admit(ctx, route.Connector)
	if !adapterReady || adapterDescriptor.Kind != ComponentAdapter {
		return RouteSelection{}, fmt.Errorf("route adapter instance %q is unavailable or ineligible", route.Adapter)
	}
	if !connectorReady || connectorDescriptor.Kind != ComponentConnector {
		return RouteSelection{}, fmt.Errorf("route connector instance %q is unavailable or ineligible", route.Connector)
	}
	return RouteSelection{Route: cloneRoute(route), Adapter: adapter, Connector: connector}, nil
}
