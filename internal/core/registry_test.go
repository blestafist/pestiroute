package core

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

type registryComponent struct {
	descriptor Descriptor
	initCalls  int
}

func (c *registryComponent) Descriptor() Descriptor { return c.descriptor.Clone() }
func (c *registryComponent) Init(context.Context, ComponentConfig) error {
	c.initCalls++
	return nil
}
func (*registryComponent) Health(context.Context) Health { return Health{State: HealthReady} }
func (*registryComponent) Capabilities(context.Context, CapabilityScope) CapabilityResult {
	return CapabilityResult{}
}
func (*registryComponent) Close(context.Context) error { return nil }

func registryForTest(t *testing.T) *Registry {
	t.Helper()
	r, err := NewRegistry(
		map[ComponentKind]APIVersion{ComponentAdapter: {Major: 1, Minor: 0}},
		map[ComponentKind][]string{ComponentAdapter: {"example.required"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func registryStub(id string) *registryComponent {
	d := validDescriptor(ComponentAdapter)
	d.ID = id
	d.Operations = []string{"example.required", "example.optional"}
	return &registryComponent{descriptor: d}
}

func TestRegistryValidatesIdentityCompatibilityAndRequiredOperations(t *testing.T) {
	r := registryForTest(t)
	first, second := registryStub("same-implementation"), registryStub("same-implementation")
	if err := r.Register("adapter-a", first, ComponentAdapter); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("adapter-b", second, ComponentAdapter); err != nil {
		t.Fatalf("same implementation in distinct instances rejected: %v", err)
	}
	if first.initCalls != 0 || second.initCalls != 0 {
		t.Fatal("registration initialized a component")
	}
	if err := r.Register("adapter-a", registryStub("other"), ComponentAdapter); err == nil {
		t.Fatal("duplicate instance ID accepted")
	}
	if _, _, ok := r.Lookup("adapter-a"); !ok {
		t.Fatal("registered instance not found")
	}
	if _, _, ok := r.Lookup("missing"); ok {
		t.Fatal("unknown instance found")
	}
}

func TestRegistryRejectsIncompatibleAPIAndUnknownRequiredOperation(t *testing.T) {
	r := registryForTest(t)
	incompatible := registryStub("incompatible")
	incompatible.descriptor.APIVersions = []APIVersion{{Major: 2, Minor: 0}}
	if err := r.Register("bad-api", incompatible, ComponentAdapter); err == nil {
		t.Fatal("incompatible major API accepted")
	}
	sameMajorWrongMinor := registryStub("wrong-minor")
	sameMajorWrongMinor.descriptor.APIVersions = []APIVersion{{Major: 1, Minor: 2}}
	if err := r.Register("bad-minor", sameMajorWrongMinor, ComponentAdapter); err == nil {
		t.Fatal("same-major but unsupported minor API accepted")
	}
	missing := registryStub("missing-operation")
	missing.descriptor.Operations = []string{"example.optional"}
	if err := r.Register("bad-operation", missing, ComponentAdapter); err == nil {
		t.Fatal("missing required operation accepted")
	}
	if incompatible.initCalls != 0 || missing.initCalls != 0 {
		t.Fatal("rejected component was initialized")
	}
}

func TestRegistryRejectsInvalidRegistrationInputs(t *testing.T) {
	r := registryForTest(t)
	if err := r.Register("", registryStub("empty-instance"), ComponentAdapter); err == nil {
		t.Fatal("empty instance ID accepted")
	}
	if err := r.Register("nil", nil, ComponentAdapter); err == nil {
		t.Fatal("nil component accepted")
	}
	if err := r.Register("unconfigured", registryStub("unconfigured"), ComponentConnector); err == nil {
		t.Fatal("unconfigured kind accepted")
	}
	configured, err := NewRegistry(map[ComponentKind]APIVersion{
		ComponentAdapter: {Major: 1}, ComponentConnector: {Major: 1},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := configured.Register("wrong-kind", registryStub("wrong-kind"), ComponentConnector); err == nil {
		t.Fatal("adapter registered as connector accepted")
	}
	invalid := registryStub("invalid-descriptor")
	invalid.descriptor.ID = ""
	if err := r.Register("invalid", invalid, ComponentAdapter); err == nil {
		t.Fatal("invalid descriptor accepted")
	}
}

func TestRegistryRejectsInvalidCompatibilityPolicy(t *testing.T) {
	tests := []struct {
		name       string
		versions   map[ComponentKind]APIVersion
		operations map[ComponentKind][]string
	}{
		{name: "zero major", versions: map[ComponentKind]APIVersion{ComponentAdapter: {Major: 0}}},
		{name: "bad kind", versions: map[ComponentKind]APIVersion{"unknown": {Major: 1}}},
		{name: "duplicate required operation", versions: map[ComponentKind]APIVersion{ComponentAdapter: {Major: 1}}, operations: map[ComponentKind][]string{ComponentAdapter: {"op", "op"}}},
		{name: "empty required operation", versions: map[ComponentKind]APIVersion{ComponentAdapter: {Major: 1}}, operations: map[ComponentKind][]string{ComponentAdapter: {""}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewRegistry(tt.versions, tt.operations); err == nil {
				t.Fatal("invalid compatibility policy accepted")
			}
		})
	}
}

func TestRegistryLookupUsesRegisteredDescriptorSnapshot(t *testing.T) {
	r := registryForTest(t)
	component := registryStub("implementation")
	if err := r.Register("adapter", component, ComponentAdapter); err != nil {
		t.Fatal(err)
	}
	component.descriptor.Protocols[0] = "mutated-after-registration"
	component.descriptor.Operations[0] = "mutated-after-registration"
	_, descriptor, ok := r.Lookup("adapter")
	if !ok {
		t.Fatal("registered instance not found")
	}
	descriptor.Protocols[0] = "mutated"
	descriptor.Operations[0] = "mutated"
	_, again, _ := r.Lookup("adapter")
	if again.Protocols[0] != "openai.responses.v1" || again.Operations[0] != "example.required" {
		t.Fatal("lookup did not retain the validated registration snapshot")
	}
}

func TestRegistryConcurrentRegisterAndLookup(t *testing.T) {
	r := registryForTest(t)
	const count = 32
	var wg sync.WaitGroup
	for i := range count {
		id := InstanceID(fmt.Sprintf("adapter-%d", i))
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := r.Register(id, registryStub("shared-implementation"), ComponentAdapter); err != nil {
				t.Errorf("Register(%q): %v", id, err)
			}
		}()
		go func() {
			defer wg.Done()
			for range 20 {
				if component, descriptor, ok := r.Lookup(id); ok && (component == nil || descriptor.ID != "shared-implementation") {
					t.Errorf("Lookup(%q) returned inconsistent entry", id)
					return
				}
			}
		}()
	}
	wg.Wait()
	for i := range count {
		id := InstanceID(fmt.Sprintf("adapter-%d", i))
		if _, _, ok := r.Lookup(id); !ok {
			t.Errorf("concurrent registration missing %q", id)
		}
	}
}
