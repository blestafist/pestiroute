package core

import "testing"

func validDescriptor(kind ComponentKind) Descriptor {
	d := Descriptor{
		ID: "example", Kind: kind, ImplementationVersion: "1.2.3",
		APIVersions: []APIVersion{{Major: 1, Minor: 0}, {Major: 1, Minor: 2}},
		Protocols:   []string{"openai.responses.v1"}, Operations: []string{"example.lookup"},
	}
	if kind == ComponentConnector {
		d.ConnectorType = "api"
		d.AuthMethods = []string{"api_key"}
	}
	return d
}

func TestDescriptorValidationAndExactVersionMatching(t *testing.T) {
	tests := []struct {
		name         string
		kind         ComponentKind
		edit         func(*Descriptor)
		wantErr      bool
		checkVersion bool
	}{
		{name: "adapter", kind: ComponentAdapter, checkVersion: true},
		{name: "connector", kind: ComponentConnector},
		{name: "missing ID", kind: ComponentAdapter, edit: func(d *Descriptor) { d.ID = "" }, wantErr: true},
		{name: "missing implementation version", kind: ComponentAdapter, edit: func(d *Descriptor) { d.ImplementationVersion = "" }, wantErr: true},
		{name: "empty API versions", kind: ComponentAdapter, edit: func(d *Descriptor) { d.APIVersions = nil }, wantErr: true},
		{name: "zero major", kind: ComponentAdapter, edit: func(d *Descriptor) { d.APIVersions[0].Major = 0 }, wantErr: true},
		{name: "duplicate versions", kind: ComponentAdapter, edit: func(d *Descriptor) { d.APIVersions = []APIVersion{{1, 0}, {1, 0}} }, wantErr: true},
		{name: "unsorted versions", kind: ComponentAdapter, edit: func(d *Descriptor) { d.APIVersions = []APIVersion{{1, 2}, {1, 0}} }, wantErr: true},
		{name: "empty protocols", kind: ComponentAdapter, edit: func(d *Descriptor) { d.Protocols = nil }, wantErr: true},
		{name: "duplicate protocols", kind: ComponentAdapter, edit: func(d *Descriptor) { d.Protocols = append(d.Protocols, d.Protocols[0]) }, wantErr: true},
		{name: "empty operation", kind: ComponentAdapter, edit: func(d *Descriptor) { d.Operations = []string{""} }, wantErr: true},
		{name: "duplicate operations", kind: ComponentAdapter, edit: func(d *Descriptor) { d.Operations = append(d.Operations, d.Operations[0]) }, wantErr: true},
		{name: "connector without type", kind: ComponentConnector, edit: func(d *Descriptor) { d.ConnectorType = "" }, wantErr: true},
		{name: "empty auth method", kind: ComponentConnector, edit: func(d *Descriptor) { d.AuthMethods = []string{""} }, wantErr: true},
		{name: "duplicate auth methods", kind: ComponentConnector, edit: func(d *Descriptor) { d.AuthMethods = append(d.AuthMethods, d.AuthMethods[0]) }, wantErr: true},
		{name: "adapter auth methods", kind: ComponentAdapter, edit: func(d *Descriptor) { d.AuthMethods = []string{"api_key"} }, wantErr: true},
		{name: "wrong kind", kind: ComponentAdapter, edit: func(d *Descriptor) { d.Kind = ComponentConnector }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := validDescriptor(tt.kind)
			if tt.edit != nil {
				tt.edit(&d)
			}
			if err := d.Validate(tt.kind); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, want error %v", err, tt.wantErr)
			}
			if tt.checkVersion && (!d.SupportsAPIVersion(APIVersion{1, 0}) || d.SupportsAPIVersion(APIVersion{1, 1})) {
				t.Fatal("API matching was not exact")
			}
		})
	}
}

func TestDeclarationSnapshotsAreIsolated(t *testing.T) {
	descriptor := validDescriptor(ComponentConnector)
	snapshot := descriptor.Clone()
	snapshot.APIVersions[0].Minor = 9
	snapshot.Protocols[0] = "changed"
	snapshot.Operations[0] = "changed"
	snapshot.AuthMethods[0] = "changed"
	if descriptor.APIVersions[0].Minor != 0 || descriptor.Protocols[0] != "openai.responses.v1" ||
		descriptor.Operations[0] != "example.lookup" || descriptor.AuthMethods[0] != "api_key" {
		t.Fatal("descriptor snapshot shares mutable declarations")
	}

	result := CapabilityResult{Values: map[Capability]CapabilityState{
		"llm.streaming": Supported,
		"llm.tools":     Unsupported,
	}}
	copy := result.Clone()
	copy.Values["llm.streaming"] = Unsupported
	delete(copy.Values, "llm.tools")
	if result.State("llm.streaming") != Supported || result.State("llm.tools") != Unsupported || result.State("llm.reasoning") != Unknown {
		t.Fatal("capability snapshot leaked or missing entry was not unknown")
	}
	if (CapabilityResult{}).State("llm.streaming") != Unknown {
		t.Fatal("empty capability declaration was not unknown")
	}
	scopeA := CapabilityScope{Protocol: "openai.responses.v1", Mode: "native", Model: "model-a", AccountID: "account-a"}
	scopeB := CapabilityScope{Protocol: "openai.responses.v1", Mode: "native", Model: "model-a", AccountID: "account-b"}
	results := map[CapabilityScope]CapabilityResult{
		scopeA: {Values: map[Capability]CapabilityState{"llm.tools": Supported}},
		scopeB: {Values: map[Capability]CapabilityState{"llm.tools": Unsupported}},
	}
	if results[scopeA].State("llm.tools") != Supported || results[scopeB].State("llm.tools") != Unsupported {
		t.Fatal("capability results were not distinct for the two account scopes")
	}
}
