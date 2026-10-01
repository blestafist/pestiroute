package core

import (
	"context"
	"fmt"
	"sync"
	"unicode/utf8"
)

const maxHealthDiagnosticBytes = 256

type LifecycleState string

const (
	LifecycleUninitialized LifecycleState = "uninitialized"
	LifecycleReady         LifecycleState = "ready"
	LifecycleUnavailable   LifecycleState = "unavailable"
	LifecycleFailed        LifecycleState = "failed"
)

type registryEntry struct {
	lifecycle  sync.Mutex
	stateMu    sync.RWMutex
	component  Component
	descriptor Descriptor
	state      LifecycleState
	healthSeq  uint64
}

// Registry owns runtime instance identities and validates declarations before
// exposing a component for later lifecycle management. Register and Lookup may
// be called concurrently; each entry retains the descriptor snapshot validated
// at registration, rather than re-querying mutable component declarations.
type Registry struct {
	mu                 sync.RWMutex
	apiVersions        map[ComponentKind]APIVersion
	requiredOperations map[ComponentKind]map[string]struct{}
	components         map[InstanceID]*registryEntry
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
		components: make(map[InstanceID]*registryEntry),
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
	r.components[id] = &registryEntry{component: component, descriptor: descriptor, state: LifecycleUninitialized}
	return nil
}

// Lookup returns the registered component and descriptor, including before Init.
func (r *Registry) Lookup(id InstanceID) (Component, Descriptor, bool) {
	entry, ok := r.entry(id)
	if !ok {
		return nil, Descriptor{}, false
	}
	entry.stateMu.RLock()
	defer entry.stateMu.RUnlock()
	if entry.state != LifecycleReady {
		return nil, entry.descriptor.Clone(), true
	}
	return entry.component, entry.descriptor.Clone(), true
}

// Init initializes a registered component once. Failed initialization always
// attempts Close so partially acquired resources are released.
func (r *Registry) Init(ctx context.Context, id InstanceID, config ComponentConfig) error {
	entry, ok := r.entry(id)
	if !ok {
		return fmt.Errorf("unknown component instance %q", id)
	}
	entry.lifecycle.Lock()
	defer entry.lifecycle.Unlock()
	entry.stateMu.RLock()
	if entry.state != LifecycleUninitialized {
		state := entry.state
		entry.stateMu.RUnlock()
		return fmt.Errorf("component instance %q cannot initialize from %s", id, state)
	}
	entry.stateMu.RUnlock()
	if err := entry.component.Init(ctx, config); err != nil {
		entry.stateMu.Lock()
		entry.state = LifecycleFailed
		entry.healthSeq++
		entry.stateMu.Unlock()
		if closeErr := entry.component.Close(ctx); closeErr != nil {
			return fmt.Errorf("initialize component %q: %v; cleanup: %w", id, err, closeErr)
		}
		return fmt.Errorf("initialize component %q: %w", id, err)
	}
	entry.stateMu.Lock()
	entry.state = LifecycleReady
	entry.healthSeq++
	entry.stateMu.Unlock()
	return nil
}

// Health reports the latest observational state without background polling.
func (r *Registry) Health(ctx context.Context, id InstanceID) Health {
	entry, ok := r.entry(id)
	if !ok {
		return Health{State: HealthUnknown, Diagnostic: "unknown component instance"}
	}
	health, _ := r.observeHealth(ctx, entry)
	return health
}

// Admit returns the component only while it is initialized and currently ready.
func (r *Registry) Admit(ctx context.Context, id InstanceID) (Component, Descriptor, bool) {
	entry, ok := r.entry(id)
	if !ok {
		return nil, Descriptor{}, false
	}
	health, _ := r.observeHealth(ctx, entry)
	entry.stateMu.RLock()
	defer entry.stateMu.RUnlock()
	if health.State != HealthReady || entry.state != LifecycleReady {
		return nil, entry.descriptor.Clone(), false
	}
	return entry.component, entry.descriptor.Clone(), true
}

func (r *Registry) observeHealth(ctx context.Context, entry *registryEntry) (Health, uint64) {
	entry.stateMu.Lock()
	if entry.state == LifecycleUninitialized || entry.state == LifecycleFailed {
		entry.stateMu.Unlock()
		return Health{State: HealthUnavailable, Diagnostic: "component is not initialized"}, 0
	}
	entry.healthSeq++
	seq := entry.healthSeq
	entry.stateMu.Unlock()

	health := entry.component.Health(ctx)
	if len(health.Diagnostic) > maxHealthDiagnosticBytes {
		cut := health.Diagnostic[:maxHealthDiagnosticBytes]
		for !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		health.Diagnostic = cut
	}
	entry.stateMu.Lock()
	defer entry.stateMu.Unlock()
	if seq != entry.healthSeq || entry.state == LifecycleUninitialized || entry.state == LifecycleFailed {
		return health, seq
	}
	switch health.State {
	case HealthReady:
		entry.state = LifecycleReady
	case HealthUnavailable, HealthUnknown:
		entry.state = LifecycleUnavailable
	default:
		health = Health{State: HealthUnknown, Diagnostic: "component reported invalid health state"}
		entry.state = LifecycleUnavailable
	}
	return health, seq
}

func (r *Registry) entry(id InstanceID) (*registryEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.components[id]
	return entry, ok
}
