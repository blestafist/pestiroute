package codex

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestConnectorLifecycleScopeAndSupport(t *testing.T) {
	ctx := context.Background()
	c := NewConnector()
	descriptor := c.Descriptor()
	if err := descriptor.Validate(core.ComponentConnector); err != nil {
		t.Fatalf("descriptor invalid: %v", err)
	}
	if descriptor.ID != componentID || descriptor.ConnectorType != "agent-protocol" ||
		descriptor.AuthMethods[0] != "bearer" || len(descriptor.Protocols) != 1 || descriptor.Protocols[0] != protocol ||
		!reflect.DeepEqual(descriptor.APIVersions, []core.APIVersion{{Major: 1, Minor: 0}}) ||
		!reflect.DeepEqual(descriptor.Operations, []string{"execute", "models", "estimate_usage", "authenticate"}) ||
		!descriptor.SupportsAPIVersion(core.APIVersion{Major: 1, Minor: 0}) {
		t.Fatalf("unexpected descriptor: %+v", descriptor)
	}
	if got := c.Health(ctx).State; got != core.HealthUnknown {
		t.Fatalf("initial health = %q", got)
	}
	if _, ge := c.Execute(ctx, core.ExecutionRequest{}, core.AttemptScope{}, core.InvocationServices{}); ge == nil || ge.Code != "connector_unavailable" {
		t.Fatalf("uninitialized Execute error = %v", ge)
	}

	config := func(model, account, selectedProfile string) []byte {
		data, err := json.Marshal(componentConfig{Model: model, AccountID: account, Profile: selectedProfile})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for _, data := range [][]byte{
		config("", "account-a", profile), config("model", "", profile),
		config("model", "account-a", "other-profile"),
		[]byte(`{"model":"model","account_id":"account-a","profile":"` + profile + `","secret":"x"}`),
		[]byte(`{"model":"model","account_id":"account-a","profile":"` + profile + `"} {}`),
	} {
		failed := NewConnector()
		if err := failed.Init(ctx, core.ComponentConfig{Data: data}); err == nil {
			t.Fatalf("invalid configuration accepted: %s", data)
		}
		if got := failed.Health(ctx).State; got != core.HealthUnavailable {
			t.Fatalf("health after failed init = %q", got)
		}
		if err := failed.Close(ctx); err != nil {
			t.Fatalf("Close after failed Init: %v", err)
		}
		if err := failed.Close(ctx); err != nil {
			t.Fatalf("repeated Close after failed Init: %v", err)
		}
	}
	if err := c.Init(ctx, core.ComponentConfig{Data: config("gpt-5.4-mini", "account-a", profile)}); err != nil {
		t.Fatal(err)
	}
	if got := c.Health(ctx).State; got != core.HealthReady {
		t.Fatalf("health after Init = %q", got)
	}
	for _, mode := range []string{core.ModeNative, core.ModeTranslation} {
		result := c.Capabilities(ctx, core.CapabilityScope{Protocol: protocol, Mode: mode, Model: "gpt-5.4-mini", AccountID: "account-a"})
		if len(result.Values) != 0 || result.State("llm.streaming") != core.Unknown {
			t.Fatalf("unproven capabilities advertised for %s: %+v", mode, result.Values)
		}
	}
	for _, scope := range []core.CapabilityScope{
		{Protocol: "other", Mode: core.ModeNative, Model: "gpt-5.4-mini", AccountID: "account-a"},
		{Protocol: protocol, Mode: "other", Model: "gpt-5.4-mini", AccountID: "account-a"},
		{Protocol: protocol, Mode: core.ModeNative, Model: "other", AccountID: "account-a"},
		{Protocol: protocol, Mode: core.ModeNative, Model: "gpt-5.4-mini", AccountID: "other"},
	} {
		if got := c.Capabilities(ctx, scope).State("llm.streaming"); got != core.Unknown {
			t.Errorf("mismatched scope capability = %q", got)
		}
	}
	if _, ge := c.Execute(ctx, core.ExecutionRequest{Model: "gpt-5.4-mini", Payload: core.RawPayload{Protocol: protocol}}, core.AttemptScope{Mode: core.ModeNative, AccountID: "other"}, core.InvocationServices{}); ge == nil || ge.Code != "scope_mismatch" {
		t.Fatalf("mismatched Execute error = %v", ge)
	}
	if models, ge := c.Models(ctx, core.ModelQuery{Protocol: protocol, Mode: core.ModeNative, AccountID: "account-a"}, core.InvocationServices{}); ge != nil || models.Supported || len(models.Models) != 0 {
		t.Fatalf("Models = %+v, %v", models, ge)
	}
	if estimate, ge := c.EstimateUsage(ctx, core.UsageQuery{Protocol: protocol, Mode: core.ModeNative, Model: "gpt-5.4-mini", AccountID: "account-a"}, core.InvocationServices{}); ge != nil || estimate.Supported || estimate.Known {
		t.Fatalf("EstimateUsage = %+v, %v", estimate, ge)
	}
	if auth, ge := c.Authenticate(ctx, core.AuthRequest{AccountID: "account-a"}, core.InvocationServices{}); ge != nil || auth.Supported {
		t.Fatalf("Authenticate = %+v, %v", auth, ge)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatalf("repeated Close: %v", err)
	}
	if got := c.Health(ctx).State; got != core.HealthUnavailable {
		t.Fatalf("health after Close = %q", got)
	}
	if _, ge := c.Execute(ctx, core.ExecutionRequest{}, core.AttemptScope{}, core.InvocationServices{}); ge == nil || ge.Code != "connector_unavailable" {
		t.Fatalf("closed Execute error = %v", ge)
	}
}
