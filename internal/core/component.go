package core

import (
	"context"
	"fmt"
	"maps"
)

type ComponentKind string

const (
	ComponentAdapter   ComponentKind = "adapter"
	ComponentConnector ComponentKind = "connector"
)

type APIVersion struct {
	Major uint16
	Minor uint16
}

// Descriptor declares implementation compatibility and supported protocols.
// Instance identity is assigned separately by the runtime.
type Descriptor struct {
	ID                    string
	Kind                  ComponentKind
	ImplementationVersion string
	APIVersions           []APIVersion
	Protocols             []string
	Operations            []string
	ConnectorType         string
	AuthMethods           []string
}

// Clone isolates mutable descriptor declarations across component boundaries.
func (d Descriptor) Clone() Descriptor {
	d.APIVersions = append([]APIVersion(nil), d.APIVersions...)
	d.Protocols = append([]string(nil), d.Protocols...)
	d.Operations = append([]string(nil), d.Operations...)
	d.AuthMethods = append([]string(nil), d.AuthMethods...)
	return d
}

// Validate checks a descriptor before component initialization.
func (d Descriptor) Validate(expectedKind ComponentKind) error {
	if d.ID == "" || d.ImplementationVersion == "" {
		return fmt.Errorf("component ID and implementation version are required")
	}
	if d.Kind != expectedKind || (d.Kind != ComponentAdapter && d.Kind != ComponentConnector) {
		return fmt.Errorf("component kind %q does not match expected kind %q", d.Kind, expectedKind)
	}
	if len(d.APIVersions) == 0 {
		return fmt.Errorf("at least one API version is required")
	}
	for i, version := range d.APIVersions {
		if version.Major == 0 {
			return fmt.Errorf("API version major must be nonzero")
		}
		if i > 0 && (version.Major < d.APIVersions[i-1].Major ||
			(version.Major == d.APIVersions[i-1].Major && version.Minor <= d.APIVersions[i-1].Minor)) {
			return fmt.Errorf("API versions must be sorted and unique")
		}
	}
	if len(d.Protocols) == 0 {
		return fmt.Errorf("at least one protocol is required")
	}
	if err := uniqueNonEmpty("protocol", d.Protocols); err != nil {
		return err
	}
	if err := uniqueNonEmpty("operation", d.Operations); err != nil {
		return err
	}
	if d.Kind == ComponentConnector {
		if d.ConnectorType == "" {
			return fmt.Errorf("connector type is required")
		}
		return uniqueNonEmpty("authentication method", d.AuthMethods)
	}
	if d.ConnectorType != "" || len(d.AuthMethods) != 0 {
		return fmt.Errorf("connector fields are only valid for connectors")
	}
	return nil
}

func uniqueNonEmpty(kind string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			return fmt.Errorf("%s must not be empty", kind)
		}
		if _, ok := seen[value]; ok {
			return fmt.Errorf("duplicate %s %q", kind, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

// SupportsAPIVersion reports exact declared compatibility; no minor range is inferred.
func (d Descriptor) SupportsAPIVersion(version APIVersion) bool {
	for _, declared := range d.APIVersions {
		if declared == version {
			return true
		}
	}
	return false
}

type InstanceID string

type ComponentConfig struct{ Data []byte }

type HealthState string

const (
	HealthReady       HealthState = "ready"
	HealthUnavailable HealthState = "unavailable"
	HealthUnknown     HealthState = "unknown"
)

type Health struct {
	State      HealthState
	Diagnostic string
}

type CapabilityScope struct {
	Protocol  string
	Mode      string
	Model     string
	AccountID string
}

type CapabilityResult struct {
	Values map[Capability]CapabilityState
}

// Clone prevents callers from mutating a shared capability declaration.
func (r CapabilityResult) Clone() CapabilityResult {
	if r.Values == nil {
		return CapabilityResult{}
	}
	values := make(map[Capability]CapabilityState, len(r.Values))
	maps.Copy(values, r.Values)
	return CapabilityResult{Values: values}
}

// State treats an omitted declaration as unknown.
func (r CapabilityResult) State(capability Capability) CapabilityState {
	if state, ok := r.Values[capability]; ok {
		return state
	}
	return Unknown
}

type Component interface {
	// Implementations return fresh declaration snapshots (use Descriptor.Clone
	// and CapabilityResult.Clone when retaining mutable declarations).
	Descriptor() Descriptor
	Init(context.Context, ComponentConfig) error
	Health(context.Context) Health
	Capabilities(context.Context, CapabilityScope) CapabilityResult
	Close(context.Context) error
}
