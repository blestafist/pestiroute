package core

import (
	"context"
	"errors"
	"testing"
)

type authorizationPolicyStore struct {
	snapshot PolicySnapshot
	err      error
}

func (s *authorizationPolicyStore) Snapshot(ctx context.Context, _ TrustedPrincipal) (PolicySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return PolicySnapshot{}, err
	}
	return s.snapshot, s.err
}

type authorizationAccount struct{ err error }

func (a authorizationAccount) AuthorizeAccount(context.Context, string, string) error { return a.err }

func authContext() context.Context {
	return WithTrustedPrincipal(context.Background(), TrustedPrincipal{KeyID: "key", PolicyID: "policy", KeyRevision: 1, PolicyRevision: 3})
}

func authCandidate() CandidateAuthorizationInput {
	scope := CapabilityScope{Protocol: "p", Mode: ModeNative, Model: "model", AccountID: "account"}
	return CandidateAuthorizationInput{
		Model: "model", Connector: "connector", AccountID: "account", Account: authorizationAccount{}, Scope: scope,
		Candidate: EligibilityCandidate{
			Scope: scope, Adapter: Descriptor{Kind: ComponentAdapter, Protocols: []string{"p"}},
			Connector: Descriptor{Kind: ComponentConnector, Protocols: []string{"p"}}, InitializedAndReady: true,
			AdapterCapabilityScope: scope, ConnectorCapabilityScope: scope,
			AdapterCapabilities:   CapabilityResult{Values: map[Capability]CapabilityState{"required": Supported}},
			ConnectorCapabilities: CapabilityResult{Values: map[Capability]CapabilityState{"required": Supported}},
		}, Required: EligibilityRequirements{Request: map[Capability]struct{}{"required": {}}},
	}
}

func TestCandidateAuthorizationPolicyAndEligibility(t *testing.T) {
	store := &authorizationPolicyStore{snapshot: PolicySnapshot{ID: "policy", Revision: 3, Enabled: true, Models: []string{"model"}, Connectors: []string{"connector"}}}
	authorization, err := LoadCandidateAuthorization(authContext(), store)
	if err != nil {
		t.Fatal(err)
	}
	// The request keeps an owned policy snapshot even when a store reuses buffers.
	store.snapshot.Models[0] = "changed"
	store.snapshot.Connectors[0] = "changed"
	for _, tc := range []struct {
		name   string
		mutate func(*CandidateAuthorizationInput)
		denied bool
	}{
		{"authorized", func(*CandidateAuthorizationInput) {}, false},
		{"model denied", func(in *CandidateAuthorizationInput) { in.Model = "other" }, true},
		{"connector denied", func(in *CandidateAuthorizationInput) { in.Connector = "other" }, true},
		{"empty model list", func(*CandidateAuthorizationInput) {}, true},
		{"account denied", func(in *CandidateAuthorizationInput) {
			in.Account = authorizationAccount{err: errors.New("disabled or wrong owner")}
		}, true},
		{"unknown capability", func(in *CandidateAuthorizationInput) { in.Candidate.ConnectorCapabilities = CapabilityResult{} }, true},
		{"unsupported capability", func(in *CandidateAuthorizationInput) {
			in.Candidate.AdapterCapabilities = CapabilityResult{Values: map[Capability]CapabilityState{"required": Unsupported}}
		}, true},
		{"protocol mismatch", func(in *CandidateAuthorizationInput) { in.Candidate.Connector.Protocols = []string{"other"} }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := authCandidate()
			if tc.name == "empty model list" {
				authorization.policy.Models = nil
			} else {
				authorization.policy.Models = []string{"model"}
			}
			authorization.policy.Connectors = []string{"connector"}
			tc.mutate(&in)
			err := authorization.AuthorizeCandidate(context.Background(), in)
			if (err != nil) != tc.denied {
				t.Fatalf("AuthorizeCandidate error = %v, denied = %v", err, tc.denied)
			}
		})
	}
}

func TestLoadCandidateAuthorizationFailsClosed(t *testing.T) {
	valid := PolicySnapshot{ID: "policy", Revision: 3, Enabled: true}
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		store   PolicyStore
		wantErr error
	}{
		{"absent principal", context.Background(), &authorizationPolicyStore{snapshot: valid}, ErrPermissionDenied},
		{"absent store", authContext(), nil, ErrPermissionDenied},
		{"snapshot error", authContext(), &authorizationPolicyStore{err: errors.New("storage")}, ErrPermissionDenied},
		{"disabled", authContext(), &authorizationPolicyStore{snapshot: PolicySnapshot{ID: "policy", Revision: 3}}, ErrPermissionDenied},
		{"revision mismatch", authContext(), &authorizationPolicyStore{snapshot: PolicySnapshot{ID: "policy", Revision: 4, Enabled: true}}, ErrPermissionDenied},
		{"policy mismatch", authContext(), &authorizationPolicyStore{snapshot: PolicySnapshot{ID: "other", Revision: 3, Enabled: true}}, ErrPermissionDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadCandidateAuthorization(tc.ctx, tc.store)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("LoadCandidateAuthorization error = %v, want %v", err, tc.wantErr)
			}
		})
	}
	ctx, cancel := context.WithCancel(authContext())
	cancel()
	_, err := LoadCandidateAuthorization(ctx, &authorizationPolicyStore{snapshot: valid})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled snapshot error = %v", err)
	}
}

func TestCandidateAuthorizationUsesOneSnapshotForEachCandidate(t *testing.T) {
	authorization, err := LoadCandidateAuthorization(authContext(), &authorizationPolicyStore{snapshot: PolicySnapshot{
		ID: "policy", Revision: 3, Enabled: true, Models: []string{"model"}, Connectors: []string{"connector", "retry-connector"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	initial := authCandidate()
	if err := authorization.AuthorizeCandidate(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	retry := authCandidate()
	retry.Connector = "retry-connector"
	if err := authorization.AuthorizeCandidate(context.Background(), retry); err != nil {
		t.Fatalf("retry candidate was not evaluated against snapshot: %v", err)
	}
}

func TestDispatcherAuthorizationDenialMakesNoConnectorCalls(t *testing.T) {
	ctx := authContext()
	registry, err := NewRegistry(map[ComponentKind]APIVersion{ComponentAdapter: {Major: 1}, ComponentConnector: {Major: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	protocol := m1Protocol
	scope := CapabilityScope{Protocol: protocol, Mode: ModeNative, Model: m1Model, AccountID: "account"}
	adapter := &testAdapter{registryComponent: registryComponent{descriptor: validDescriptor(ComponentAdapter)}, caps: map[CapabilityScope]CapabilityResult{scope: {Values: map[Capability]CapabilityState{}}}}
	connector := &dispatchConnector{registryComponent: registryComponent{descriptor: validDescriptor(ComponentConnector)}, caps: map[CapabilityScope]CapabilityResult{scope: {Values: map[Capability]CapabilityState{}}}}
	if err := registry.Register("adapter", adapter, ComponentAdapter); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("connector-a", connector, ComponentConnector); err != nil {
		t.Fatal(err)
	}
	for _, id := range []InstanceID{"adapter", "connector-a"} {
		if err := registry.Init(ctx, id, ComponentConfig{}); err != nil {
			t.Fatal(err)
		}
	}
	table, err := NewRouteTable([]Route{{
		Identity: RouteIdentity{RouteLookupKey: RouteLookupKey{Protocol: protocol, Mode: ModeNative, Model: m1Model}, AccountID: "account"},
		Adapter:  "adapter", Connector: "connector-a",
	}}, registry)
	if err != nil {
		t.Fatal(err)
	}
	policies := &authorizationPolicyStore{snapshot: PolicySnapshot{ID: "policy", Revision: 3, Enabled: true, Models: []string{m1Model}, Connectors: []string{"connector-a"}}}
	for _, tc := range []struct {
		name         string
		policy       PolicySnapshot
		accountError error
	}{
		{"model denied", PolicySnapshot{ID: "policy", Revision: 3, Enabled: true, Models: []string{}, Connectors: []string{"connector-a"}}, nil},
		{"connector denied", PolicySnapshot{ID: "policy", Revision: 3, Enabled: true, Models: []string{m1Model}, Connectors: nil}, nil},
		{"account disabled or mismatched", policies.snapshot, ErrPermissionDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policies.snapshot = tc.policy
			d := Dispatcher{Routes: table, Services: &testServices{}, AccountID: "account", Policies: policies, Accounts: authorizationAccount{err: tc.accountError}}
			r := request()
			_, gatewayErr := d.Execute(ctx, r)
			if gatewayErr == nil || gatewayErr.Category != CategoryPermissionDenied || gatewayErr.Code != "permission_denied" {
				t.Fatalf("error = %#v; want permission denied", gatewayErr)
			}
			if len(connector.calls) != 0 {
				t.Fatalf("denied request reached connector %d times", len(connector.calls))
			}
		})
	}
}
