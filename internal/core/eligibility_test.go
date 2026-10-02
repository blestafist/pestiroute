package core

import (
	"testing"
)

const eligibilityProtocol = "example.native.v1"

func eligibleCandidate() EligibilityCandidate {
	return EligibilityCandidate{
		Scope:               CapabilityScope{Protocol: eligibilityProtocol, Mode: "native", Model: "model-x", AccountID: "account-a"},
		Adapter:             Descriptor{Kind: ComponentAdapter, Protocols: []string{eligibilityProtocol}},
		Connector:           Descriptor{Kind: ComponentConnector, Protocols: []string{eligibilityProtocol}},
		InitializedAndReady: true,
		AdapterCapabilityScope: CapabilityScope{
			Protocol: eligibilityProtocol, Mode: "native", Model: "model-x", AccountID: "account-a",
		},
		ConnectorCapabilityScope: CapabilityScope{
			Protocol: eligibilityProtocol, Mode: "native", Model: "model-x", AccountID: "account-a",
		},
		AdapterCapabilities: CapabilityResult{Values: map[Capability]CapabilityState{"vendor.feature.experimental": Supported}},
		ConnectorCapabilities: CapabilityResult{Values: map[Capability]CapabilityState{
			"vendor.feature.experimental": Supported,
			"llm.tools":                   Supported,
		}},
	}
}

func TestEligibilityRequirementComponentThreeStateMatrix(t *testing.T) {
	scope := eligibleCandidate().Scope
	for _, requirement := range []struct {
		name string
		set  func(*EligibilityRequirements, Capability)
	}{
		{"request", func(r *EligibilityRequirements, c Capability) { r.Request = map[Capability]struct{}{c: {}} }},
		{"route", func(r *EligibilityRequirements, c Capability) { r.Route = map[Capability]struct{}{c: {}} }},
	} {
		for _, component := range []string{"adapter", "connector"} {
			for _, state := range []CapabilityState{Supported, Unsupported, Unknown} {
				t.Run(requirement.name+"/"+component+"/"+string(state), func(t *testing.T) {
					candidate := eligibleCandidate()
					capability := Capability("vendor." + requirement.name + ".required")
					candidate.AdapterCapabilities.Values[capability] = Supported
					candidate.ConnectorCapabilities.Values[capability] = Supported
					if component == "adapter" {
						candidate.AdapterCapabilities.Values[capability] = state
					} else {
						candidate.ConnectorCapabilities.Values[capability] = state
					}
					requirements := EligibilityRequirements{}
					requirement.set(&requirements, capability)
					eligible := candidate.Eligible(scope, requirements) == nil
					if want := state == Supported; eligible != want {
						t.Fatalf("eligible=%v, want %v", eligible, want)
					}
				})
			}
		}
	}
}

func TestEligibilityExactScopeAndComponentOnlyRequirements(t *testing.T) {
	base := eligibleCandidate()
	scope := base.Scope
	for _, tc := range []struct {
		name   string
		mutate func(*EligibilityCandidate, *CapabilityScope, *EligibilityRequirements)
	}{
		{"protocol", func(_ *EligibilityCandidate, s *CapabilityScope, _ *EligibilityRequirements) { s.Protocol = "other" }},
		{"mode", func(_ *EligibilityCandidate, s *CapabilityScope, _ *EligibilityRequirements) { s.Mode = "translation" }},
		{"model", func(_ *EligibilityCandidate, s *CapabilityScope, _ *EligibilityRequirements) { s.Model = "other" }},
		{"account", func(_ *EligibilityCandidate, s *CapabilityScope, _ *EligibilityRequirements) {
			s.AccountID = "account-b"
		}},
		{"uninitialized", func(c *EligibilityCandidate, _ *CapabilityScope, _ *EligibilityRequirements) {
			c.InitializedAndReady = false
		}},
		{"wrong adapter protocol", func(c *EligibilityCandidate, _ *CapabilityScope, _ *EligibilityRequirements) {
			c.Adapter.Protocols = []string{"other"}
		}},
		{"wrong connector protocol", func(c *EligibilityCandidate, _ *CapabilityScope, _ *EligibilityRequirements) {
			c.Connector.Protocols = []string{"other"}
		}},
		{"borrowed adapter capability scope", func(c *EligibilityCandidate, _ *CapabilityScope, _ *EligibilityRequirements) {
			c.AdapterCapabilityScope.AccountID = "account-b"
		}},
		{"borrowed connector capability scope", func(c *EligibilityCandidate, _ *CapabilityScope, _ *EligibilityRequirements) {
			c.ConnectorCapabilityScope.Model = "other"
		}},
		{"adapter-only capability", func(_ *EligibilityCandidate, _ *CapabilityScope, r *EligibilityRequirements) {
			r.Adapter = map[Capability]struct{}{"vendor.adapter.local": {}}
		}},
		{"connector-only capability", func(_ *EligibilityCandidate, _ *CapabilityScope, r *EligibilityRequirements) {
			r.Connector = map[Capability]struct{}{"vendor.connector.local": {}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate, requested := base, scope
			requirements := EligibilityRequirements{}
			tc.mutate(&candidate, &requested, &requirements)
			if err := candidate.Eligible(requested, requirements); err == nil {
				t.Fatal("ineligible candidate accepted")
			}
		})
	}
	if err := base.Eligible(scope, EligibilityRequirements{Adapter: map[Capability]struct{}{"vendor.feature.experimental": {}}}); err != nil {
		t.Fatalf("generic adapter capability rejected: %v", err)
	}
	if err := base.Eligible(scope, EligibilityRequirements{Connector: map[Capability]struct{}{"vendor.feature.experimental": {}}}); err != nil {
		t.Fatalf("generic connector capability rejected: %v", err)
	}
	parallel := EligibilityRequirements{Request: map[Capability]struct{}{"llm.tools.parallel": {}}}
	if err := base.Eligible(scope, parallel); err == nil {
		t.Fatal("parent tools support satisfied missing parallel declaration")
	}
	for _, state := range []CapabilityState{Unsupported, Unknown} {
		base.AdapterCapabilities.Values["llm.tools"] = Supported
		base.ConnectorCapabilities.Values["llm.tools"] = Supported
		base.AdapterCapabilities.Values["llm.tools.parallel"] = state
		base.ConnectorCapabilities.Values["llm.tools.parallel"] = state
		if err := base.Eligible(scope, parallel); err == nil {
			t.Fatalf("parent tools support satisfied parallel tools in %s state", state)
		}
	}
	base.AdapterCapabilities.Values["llm.tools.parallel"] = Supported
	base.ConnectorCapabilities.Values["llm.tools.parallel"] = Supported
	if err := base.Eligible(scope, parallel); err != nil {
		t.Fatalf("explicit child capability rejected: %v", err)
	}

	combined := EligibilityRequirements{
		Request: map[Capability]struct{}{"llm.tools": {}},
		Route:   map[Capability]struct{}{"vendor.route.required": {}},
	}
	base.AdapterCapabilities.Values["vendor.route.required"] = Supported
	base.ConnectorCapabilities.Values["vendor.route.required"] = Supported
	if err := base.Eligible(scope, combined); err != nil {
		t.Fatalf("supported request/route union rejected: %v", err)
	}
	base.ConnectorCapabilities.Values["vendor.route.required"] = Unknown
	if err := base.Eligible(scope, combined); err == nil {
		t.Fatal("unsupported route requirement was dropped from request/route union")
	}
}
