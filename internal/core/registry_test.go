package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
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
				if _, descriptor, ok := r.Lookup(id); ok && descriptor.ID != "shared-implementation" {
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

type lifecycleComponent struct {
	descriptor Descriptor
	initErr    error
	closeErr   error
	initCalls  atomic.Int32
	closeCalls atomic.Int32
	health     atomic.Value
}

func newLifecycleComponent() *lifecycleComponent {
	descriptor := validDescriptor(ComponentAdapter)
	descriptor.Operations = []string{"example.required"}
	c := &lifecycleComponent{descriptor: descriptor}
	c.health.Store(Health{State: HealthReady})
	return c
}

func (c *lifecycleComponent) Descriptor() Descriptor { return c.descriptor.Clone() }
func (c *lifecycleComponent) Init(context.Context, ComponentConfig) error {
	c.initCalls.Add(1)
	return c.initErr
}
func (c *lifecycleComponent) Health(context.Context) Health { return c.health.Load().(Health) }
func (*lifecycleComponent) Capabilities(context.Context, CapabilityScope) CapabilityResult {
	return CapabilityResult{}
}
func (c *lifecycleComponent) Close(context.Context) error { c.closeCalls.Add(1); return c.closeErr }

type blockingLifecycleComponent struct {
	*lifecycleComponent
	initStarted   chan struct{}
	initGate      chan struct{}
	healthStarted chan struct{}
	healthGate    chan struct{}
}

type gatedCloseComponent struct {
	*lifecycleComponent
	closeStarted chan struct{}
	closeGate    chan struct{}
}

func (c *gatedCloseComponent) Close(ctx context.Context) error {
	close(c.closeStarted)
	select {
	case <-c.closeGate:
		return c.lifecycleComponent.Close(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

type observedCancelContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *observedCancelContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func (c *blockingLifecycleComponent) Init(context.Context, ComponentConfig) error {
	close(c.initStarted)
	<-c.initGate
	c.initCalls.Add(1)
	return nil
}

func (c *blockingLifecycleComponent) Health(context.Context) Health {
	c.healthStarted <- struct{}{}
	<-c.healthGate
	return c.lifecycleComponent.Health(context.Background())
}

func TestRegistryInitHealthAndAdmission(t *testing.T) {
	r := registryForTest(t)
	c := newLifecycleComponent()
	if err := r.Register("lifecycle", c, ComponentAdapter); err != nil {
		t.Fatal(err)
	}
	if component, descriptor, ok := r.Lookup("lifecycle"); !ok || descriptor.ID == "" || component != nil {
		t.Fatal("pre-init descriptor lookup unavailable")
	}
	if _, _, ok := r.Admit(context.Background(), "lifecycle"); ok {
		t.Fatal("uninitialized component admitted")
	}
	if err := r.Init(context.Background(), "lifecycle", ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	if component, _, ok := r.Lookup("lifecycle"); !ok || component == nil {
		t.Fatal("initialized component lookup unavailable")
	}
	if _, _, ok := r.Admit(context.Background(), "lifecycle"); !ok {
		t.Fatal("healthy initialized component denied")
	}
	for _, state := range []HealthState{HealthUnavailable, HealthUnknown} {
		c.health.Store(Health{State: state, Diagnostic: strings.Repeat("a", 255) + "é"})
		if health := r.Health(context.Background(), "lifecycle"); health.State != state {
			t.Fatalf("Health() = %q, want %q", health.State, state)
		} else if len(health.Diagnostic) > maxHealthDiagnosticBytes {
			t.Fatalf("Health() diagnostic has %d bytes", len(health.Diagnostic))
		} else if !utf8.ValidString(health.Diagnostic) || len(health.Diagnostic) != 255 {
			t.Fatalf("Health() diagnostic is not rune-boundary truncated: %q", health.Diagnostic)
		}
		if _, _, ok := r.Admit(context.Background(), "lifecycle"); ok {
			t.Fatalf("component admitted with %q health", state)
		}
	}
	c.health.Store(Health{State: HealthReady})
	if _, _, ok := r.Admit(context.Background(), "lifecycle"); !ok {
		t.Fatal("component did not recover after ready health")
	}
}

func TestRegistryLookupDoesNotWaitForInitOrHealth(t *testing.T) {
	base := newLifecycleComponent()
	c := &blockingLifecycleComponent{
		lifecycleComponent: base,
		initStarted:        make(chan struct{}), initGate: make(chan struct{}),
		healthStarted: make(chan struct{}, 2), healthGate: make(chan struct{}),
	}
	r := registryForTest(t)
	if err := r.Register("blocking", c, ComponentAdapter); err != nil {
		t.Fatal(err)
	}
	initDone := make(chan error, 1)
	go func() { initDone <- r.Init(context.Background(), "blocking", ComponentConfig{}) }()
	<-c.initStarted
	lookupDone := make(chan struct{})
	go func() {
		defer close(lookupDone)
		if component, descriptor, ok := r.Lookup("blocking"); !ok || component != nil || descriptor.ID != base.descriptor.ID {
			t.Errorf("Lookup during Init = (%v, %q, %v)", component, descriptor.ID, ok)
		}
	}()
	waitLookup(t, lookupDone)
	close(c.initGate)
	if err := <-initDone; err != nil {
		t.Fatal(err)
	}

	healthDone := make(chan struct{}, 2)
	for range 2 {
		go func() {
			_ = r.Health(context.Background(), "blocking")
			healthDone <- struct{}{}
		}()
	}
	for range 2 {
		select {
		case <-c.healthStarted:
		case <-time.After(time.Second):
			t.Fatal("concurrent Health calls were serialized")
		}
	}
	lookupDone = make(chan struct{})
	go func() {
		defer close(lookupDone)
		if _, descriptor, ok := r.Lookup("blocking"); !ok || descriptor.ID != base.descriptor.ID {
			t.Errorf("Lookup during Health returned descriptor %q, found=%v", descriptor.ID, ok)
		}
	}()
	waitLookup(t, lookupDone)
	close(c.healthGate)
	for range 2 {
		<-healthDone
	}
}

func waitLookup(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Lookup blocked behind a component operation")
	}
}

func TestRegistryFailedInitClosesPartialResources(t *testing.T) {
	r := registryForTest(t)
	c := newLifecycleComponent()
	c.initErr = fmt.Errorf("synthetic init failure")
	if err := r.Register("failed", c, ComponentAdapter); err != nil {
		t.Fatal(err)
	}
	if err := r.Init(context.Background(), "failed", ComponentConfig{}); err == nil {
		t.Fatal("failed Init returned nil")
	}
	if c.closeCalls.Load() != 1 {
		t.Fatalf("Close calls = %d, want 1", c.closeCalls.Load())
	}
	if health := r.Health(context.Background(), "failed"); health.State != HealthUnavailable {
		t.Fatalf("failed component health = %q", health.State)
	}
	if _, _, ok := r.Admit(context.Background(), "failed"); ok {
		t.Fatal("failed component admitted")
	}
}

func TestRegistryConcurrentHealthAndAdmission(t *testing.T) {
	r := registryForTest(t)
	c := newLifecycleComponent()
	if err := r.Register("concurrent", c, ComponentAdapter); err != nil {
		t.Fatal(err)
	}
	if err := r.Init(context.Background(), "concurrent", ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				c.health.Store(Health{State: HealthUnavailable, Diagnostic: string(make([]byte, 512))})
				_ = r.Health(context.Background(), "concurrent")
				_, _, _ = r.Admit(context.Background(), "concurrent")
				c.health.Store(Health{State: HealthReady})
			}
		})
	}
	wg.Wait()
}

func TestRegistryConcurrentAlwaysReadyAdmissionNeverRejects(t *testing.T) {
	r := registryForTest(t)
	c := newLifecycleComponent()
	if err := r.Register("always-ready", c, ComponentAdapter); err != nil {
		t.Fatal(err)
	}
	if err := r.Init(context.Background(), "always-ready", ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var denied atomic.Int32
	for range 16 {
		wg.Go(func() {
			for range 1000 {
				if _, _, ok := r.Admit(context.Background(), "always-ready"); !ok {
					denied.Add(1)
				}
			}
		})
	}
	wg.Wait()
	if count := denied.Load(); count != 0 {
		t.Fatalf("Admit denied %d of 16000 always-ready observations", count)
	}
}

func TestRegistryCloseIsIdempotentAggregatesErrorsAndRejectsWork(t *testing.T) {
	r := registryForTest(t)
	first, second := newLifecycleComponent(), newLifecycleComponent()
	wantErr := fmt.Errorf("synthetic close failure")
	first.closeErr = wantErr
	for id, c := range map[InstanceID]*lifecycleComponent{"first": first, "second": second} {
		if err := r.Register(id, c, ComponentAdapter); err != nil {
			t.Fatal(err)
		}
		if err := r.Init(context.Background(), id, ComponentConfig{}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errCh <- r.Close(context.Background()) })
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if !errors.Is(err, wantErr) {
			t.Fatalf("Close error %v does not include component failure", err)
		}
	}
	if first.closeCalls.Load() != 1 || second.closeCalls.Load() != 1 {
		t.Fatalf("component Close counts = (%d, %d), want (1, 1)", first.closeCalls.Load(), second.closeCalls.Load())
	}
	if err := r.Register("late", newLifecycleComponent(), ComponentAdapter); err == nil {
		t.Fatal("Register succeeded after Close")
	}
	if err := r.Init(context.Background(), "first", ComponentConfig{}); err == nil {
		t.Fatal("Init succeeded after Close")
	}
	if _, _, ok := r.Admit(context.Background(), "second"); ok {
		t.Fatal("Admit succeeded after Close")
	}
	if health := r.Health(context.Background(), "second"); health.State != HealthUnavailable {
		t.Fatalf("Health after Close = %q, want unavailable", health.State)
	}
}

func TestRegistryCloseDoesNotRepeatFailedInitCleanup(t *testing.T) {
	r := registryForTest(t)
	c := newLifecycleComponent()
	c.initErr = fmt.Errorf("synthetic init failure")
	if err := r.Register("failed", c, ComponentAdapter); err != nil {
		t.Fatal(err)
	}
	if err := r.Init(context.Background(), "failed", ComponentConfig{}); err == nil {
		t.Fatal("failed Init returned nil")
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.closeCalls.Load() != 1 {
		t.Fatalf("Close calls after failed Init = %d, want 1", c.closeCalls.Load())
	}
}

func TestRegistryCloseSerializesWithInitAndRejectsConcurrentAdmission(t *testing.T) {
	base := newLifecycleComponent()
	c := &blockingLifecycleComponent{
		lifecycleComponent: base,
		initStarted:        make(chan struct{}), initGate: make(chan struct{}),
		healthStarted: make(chan struct{}, 2), healthGate: make(chan struct{}),
	}
	r := registryForTest(t)
	if err := r.Register("closing", c, ComponentAdapter); err != nil {
		t.Fatal(err)
	}
	initDone := make(chan error, 1)
	go func() { initDone <- r.Init(context.Background(), "closing", ComponentConfig{}) }()
	<-c.initStarted
	closeDone := make(chan error, 1)
	go func() { closeDone <- r.Close(context.Background()) }()
	deadline := time.After(time.Second)
	for !r.isClosed() {
		select {
		case <-deadline:
			t.Fatal("Close did not stop admission")
		case <-time.After(time.Millisecond):
		}
	}
	if _, _, ok := r.Admit(context.Background(), "closing"); ok {
		t.Fatal("Admit succeeded while registry was closing")
	}
	if component, descriptor, ok := r.Lookup("closing"); !ok || component != nil || descriptor.ID == "" {
		t.Fatal("Lookup during Close returned an available component or lost its descriptor")
	}
	close(c.initGate)
	if err := <-initDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if c.closeCalls.Load() != 1 {
		t.Fatalf("Close calls = %d, want 1", c.closeCalls.Load())
	}
}

func TestRegistryCloseWaiterCancellationAndCompletedResult(t *testing.T) {
	r := registryForTest(t)
	base := newLifecycleComponent()
	wantErr := fmt.Errorf("synthetic close failure")
	base.closeErr = wantErr
	c := &gatedCloseComponent{lifecycleComponent: base, closeStarted: make(chan struct{}), closeGate: make(chan struct{})}
	if err := r.Register("gated", c, ComponentAdapter); err != nil {
		t.Fatal(err)
	}
	if err := r.Init(context.Background(), "gated", ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- r.Close(context.Background()) }()
	<-c.closeStarted

	waitCtx, cancelWait := context.WithCancel(context.Background())
	observed := &observedCancelContext{Context: waitCtx, observed: make(chan struct{})}
	waiterDone := make(chan error, 1)
	go func() { waiterDone <- r.Close(observed) }()
	<-observed.observed // Confirms the waiter entered the incomplete-close select.
	cancelWait()
	if err := <-waiterDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter Close = %v, want context.Canceled", err)
	}

	close(c.closeGate)
	if err := <-firstDone; !errors.Is(err, wantErr) {
		t.Fatalf("first Close = %v, want joined component error", err)
	}
	completedCtx, cancelCompleted := context.WithCancel(context.Background())
	cancelCompleted()
	if err := r.Close(completedCtx); !errors.Is(err, wantErr) {
		t.Fatalf("completed Close with canceled context = %v, want original close error", err)
	}
}
