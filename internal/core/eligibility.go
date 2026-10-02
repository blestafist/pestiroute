package core

import (
	"fmt"
	"slices"
)

// EligibilityRequirements separates client-facing capabilities, which both
// components must support, from capabilities required by only one component.
type EligibilityRequirements struct {
	Request   map[Capability]struct{}
	Route     map[Capability]struct{}
	Adapter   map[Capability]struct{}
	Connector map[Capability]struct{}
}

// EligibilityCandidate is a snapshot assembled from admitted component
// metadata and scoped capability reports. It contains no executable behavior.
type EligibilityCandidate struct {
	Scope                    CapabilityScope
	Adapter                  Descriptor
	Connector                Descriptor
	InitializedAndReady      bool
	AdapterCapabilityScope   CapabilityScope
	ConnectorCapabilityScope CapabilityScope
	AdapterCapabilities      CapabilityResult
	ConnectorCapabilities    CapabilityResult
}

// Eligible checks one exact protocol/mode/model/account candidate. The caller
// must obtain component capabilities using the same scope supplied here.
func (c EligibilityCandidate) Eligible(scope CapabilityScope, requirements EligibilityRequirements) error {
	if !c.InitializedAndReady {
		return fmt.Errorf("candidate is not initialized and ready")
	}
	if c.Scope != scope {
		return fmt.Errorf("candidate scope does not match request")
	}
	if c.AdapterCapabilityScope != scope || c.ConnectorCapabilityScope != scope {
		return fmt.Errorf("capability declaration scope does not match request")
	}
	if c.Adapter.Kind != ComponentAdapter || !declaresProtocol(c.Adapter, scope.Protocol) {
		return fmt.Errorf("adapter does not declare protocol %q", scope.Protocol)
	}
	if c.Connector.Kind != ComponentConnector || !declaresProtocol(c.Connector, scope.Protocol) {
		return fmt.Errorf("connector does not declare protocol %q", scope.Protocol)
	}
	for capability := range requirements.Request {
		if err := requireBoth(capability, c); err != nil {
			return err
		}
	}
	for capability := range requirements.Route {
		if err := requireBoth(capability, c); err != nil {
			return err
		}
	}
	for capability := range requirements.Adapter {
		if c.AdapterCapabilities.State(capability) != Supported {
			return fmt.Errorf("adapter does not support required capability %q", capability)
		}
	}
	for capability := range requirements.Connector {
		if c.ConnectorCapabilities.State(capability) != Supported {
			return fmt.Errorf("connector does not support required capability %q", capability)
		}
	}
	return nil
}

func requireBoth(capability Capability, c EligibilityCandidate) error {
	if c.AdapterCapabilities.State(capability) != Supported {
		return fmt.Errorf("adapter does not support required capability %q", capability)
	}
	if c.ConnectorCapabilities.State(capability) != Supported {
		return fmt.Errorf("connector does not support required capability %q", capability)
	}
	return nil
}

func declaresProtocol(descriptor Descriptor, protocol string) bool {
	return slices.Contains(descriptor.Protocols, protocol)
}
