package core

import (
	"fmt"
	"sync"
)

// Registry owns runtime instance identities and validates declarations before
// exposing a component for later lifecycle management. Register and Lookup may
// be called concurrently; each entry retains the descriptor snapshot validated
// at registration, rather than re-querying mutable component declarations.
type Registry struct {
	mu                 sync.RWMutex
	apiVersions        map[ComponentKind]APIVersion
	requiredOperations map[ComponentKind]map[string]struct{}
	components         map[InstanceID]Component
	descriptors        map[InstanceID]Descriptor
}

// NewRegistry copies its compatibility policy so callers cannot change it
// after components have been registered.
func NewRegistry(apiVersions map[ComponentKind]APIVersion, requiredOperations map[ComponentKind][]string) (*Registry, error) {
	versions := make(map[ComponentKind]APIVersion, len(apiVersions))
	for kind, version := range apiVersions {
		if kind != ComponentAdapter && kind != ComponentConnector {
			return nil, fmt.Errorf("unsupported component kind %q", kind)
		}
		if version.Major == 0 {
			return nil, fmt.Errorf("API version major must be nonzero for %q", kind)
		}
		versions[kind] = version
	}
	operations := make(map[ComponentKind]map[string]struct{}, len(requiredOperations))
	for kind, required := range requiredOperations {
		if kind != ComponentAdapter && kind != ComponentConnector {
			return nil, fmt.Errorf("unsupported component kind %q", kind)
		}
		set := make(map[string]struct{}, len(required))
		if err := uniqueNonEmpty("required operation", required); err != nil {
			return nil, err
		}
		for _, operation := range required {
			set[operation] = struct{}{}
		}
		operations[kind] = set
	}
	return &Registry{
		apiVersions: versions, requiredOperations: operations,
		components: make(map[InstanceID]Component), descriptors: make(map[InstanceID]Descriptor),
	}, nil
}

// Register validates an instance before storing it. Reusing an implementation
// descriptor ID is allowed; runtime instance IDs must be unique.
func (r *Registry) Register(id InstanceID, component Component, expectedKind ComponentKind) error {
	if id == "" {
		return fmt.Errorf("component instance ID is required")
	}
	if component == nil {
		return fmt.Errorf("component instance %q is nil", id)
	}
	version, ok := r.apiVersions[expectedKind]
	if !ok {
		return fmt.Errorf("no runtime API version configured for kind %q", expectedKind)
	}
	descriptor := component.Descriptor().Clone()
	if err := descriptor.Validate(expectedKind); err != nil {
		return fmt.Errorf("invalid component descriptor: %w", err)
	}
	if !descriptor.SupportsAPIVersion(version) {
		return fmt.Errorf("component %q does not support API %d.%d", descriptor.ID, version.Major, version.Minor)
	}
	declared := make(map[string]struct{}, len(descriptor.Operations))
	for _, operation := range descriptor.Operations {
		declared[operation] = struct{}{}
	}
	for operation := range r.requiredOperations[expectedKind] {
		if _, ok := declared[operation]; !ok {
			return fmt.Errorf("component %q does not declare required operation %q", descriptor.ID, operation)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.components[id]; exists {
		return fmt.Errorf("duplicate component instance ID %q", id)
	}
	r.components[id] = component
	r.descriptors[id] = descriptor
	return nil
}

// Lookup returns the component interface and an isolated descriptor snapshot.
func (r *Registry) Lookup(id InstanceID) (Component, Descriptor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	component, ok := r.components[id]
	if !ok {
		return nil, Descriptor{}, false
	}
	return component, r.descriptors[id].Clone(), true
}
