package claudecode

import (
	"context"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestComponentLifecycleTransitions(t *testing.T) {
	c := NewConnector()
	if got := c.Health(context.Background()).State; got != core.HealthUnavailable {
		t.Fatalf("initial health = %q, want unavailable", got)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"profile":"` + profile + `"}`)}); err != nil {
		t.Fatal(err)
	}
	if got := c.Health(context.Background()).State; got != core.HealthReady {
		t.Fatalf("initialized health = %q, want ready", got)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.Health(context.Background()).State; got != core.HealthUnavailable {
		t.Fatalf("closed health = %q, want unavailable", got)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"profile":"` + profile + `"}`)}); err == nil {
		t.Fatal("Init after Close succeeded")
	}
}

func TestComponentLifecycleStrictProfileConfiguration(t *testing.T) {
	for _, data := range []string{
		`{}`,
		`{"profile":"wrong"}`,
		`{"profile":"` + profile + `","unknown":true}`,
		`{"profile":"` + profile + `"} {}`,
		`{"profile":"` + profile + `"}{}`, // trailing JSON value
	} {
		c := NewConnector()
		if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(data)}); err == nil {
			t.Errorf("Init accepted invalid config %s", data)
		}
		if got := c.Health(context.Background()).State; got != core.HealthUnavailable {
			t.Errorf("failed initialization health = %q, want unavailable", got)
		}
	}
}

func TestComponentLifecycleDescriptorAndCapabilities(t *testing.T) {
	c := NewConnector()
	d := c.Descriptor()
	if err := d.Validate(core.ComponentConnector); err != nil {
		t.Fatal(err)
	}
	if d.ID != componentID || d.Protocols[0] != protocol || d.ConnectorType != "api" || d.AuthMethods[0] != "oauth" || len(d.Operations) != 0 {
		t.Fatalf("unexpected descriptor: %+v", d)
	}
	capabilities := []core.Capability{"llm.streaming", "llm.tools", "llm.reasoning", "auth.oauth", "usage.exact"}
	if got := c.Capabilities(context.Background(), core.CapabilityScope{Protocol: protocol, Mode: core.ModeTranslation}); len(got.Values) != 0 {
		t.Fatalf("unready capabilities = %v, want empty", got.Values)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"profile":"` + profile + `"}`)}); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []core.CapabilityScope{{Protocol: protocol, Mode: core.ModeTranslation}, {Protocol: "other", Mode: core.ModeTranslation}, {Protocol: protocol, Mode: core.ModeNative}} {
		got := c.Capabilities(context.Background(), scope)
		if scope.Protocol != protocol || scope.Mode != core.ModeTranslation {
			if len(got.Values) != 0 {
				t.Errorf("capabilities for mismatched scope %+v = %v, want empty", scope, got.Values)
			}
			continue
		}
		if len(got.Values) != len(capabilities) {
			t.Errorf("capabilities for %+v = %v", scope, got.Values)
		}
		for _, capability := range capabilities {
			if state := got.State(capability); state != core.Unknown {
				t.Errorf("%s for %+v = %q, want unknown", capability, scope, state)
			}
		}
	}
	_ = c.Close(context.Background())
}

func TestComponentLifecycleContractStubs(t *testing.T) {
	c := NewConnector()
	response, gatewayErr := c.Execute(context.Background(), core.ExecutionRequest{}, core.AttemptScope{}, core.InvocationServices{})
	if gatewayErr == nil || gatewayErr.Code != "connector_unavailable" || gatewayErr.Category != core.CategoryUnavailable ||
		gatewayErr.RetryDisposition != core.RetryUnknown || gatewayErr.Message == "" || response.Stream != nil {
		t.Fatalf("Execute = (%+v, %+v), want unavailable error and no stream", response, gatewayErr)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"profile":"` + profile + `"}`)}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	auth, gatewayErr := c.Authenticate(ctx, core.AuthRequest{}, core.InvocationServices{})
	if gatewayErr != nil || auth.Supported {
		t.Fatalf("Authenticate = (%+v, %+v), want unsupported without error", auth, gatewayErr)
	}
	baseQuery := core.ModelQuery{Protocol: protocol, Mode: core.ModeTranslation}
	models, gatewayErr := c.Models(ctx, baseQuery, core.InvocationServices{})
	if gatewayErr != nil || models.Supported || len(models.Models) != 0 {
		t.Fatalf("Models = (%+v, %+v), want unsupported without fabricated models", models, gatewayErr)
	}
	usageQuery := core.UsageQuery{Protocol: protocol, Mode: core.ModeTranslation, MaxBodyBytes: 2, Payload: core.RawPayload{Body: []byte("ok")}}
	usage, gatewayErr := c.EstimateUsage(ctx, usageQuery, core.InvocationServices{})
	if gatewayErr != nil || usage.Supported || usage.Known || usage.Usage != nil {
		t.Fatalf("EstimateUsage = (%+v, %+v), want unsupported unknown estimate", usage, gatewayErr)
	}
	usageQuery.Payload.Body = []byte("too large")
	if _, ge := c.EstimateUsage(ctx, usageQuery, core.InvocationServices{}); ge == nil || ge.Code != "invalid_request" {
		t.Fatalf("oversized estimate error = %+v, want invalid_request", ge)
	}
	usageQuery.Payload.Body = nil
	usageQuery.MaxBodyBytes = -1
	if _, ge := c.EstimateUsage(ctx, usageQuery, core.InvocationServices{}); ge == nil || ge.Code != "invalid_request" {
		t.Fatalf("negative body limit error = %+v, want invalid_request", ge)
	}
	usageQuery.MaxBodyBytes = 0
	if _, ge := c.EstimateUsage(ctx, usageQuery, core.InvocationServices{}); ge == nil || ge.Code != "invalid_request" {
		t.Fatalf("zero body limit error = %+v, want invalid_request", ge)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, ge := c.Models(cancelled, baseQuery, core.InvocationServices{}); ge == nil || ge.Category != core.CategoryCancelled {
		t.Fatalf("cancelled Models error = %+v", ge)
	}
	if _, ge := c.EstimateUsage(cancelled, usageQuery, core.InvocationServices{}); ge == nil || ge.Category != core.CategoryCancelled {
		t.Fatalf("cancelled EstimateUsage error = %+v", ge)
	}
	for _, tc := range []struct {
		name     string
		protocol string
		mode     string
	}{{"protocol", "wrong", core.ModeTranslation}, {"mode", protocol, core.ModeNative}} {
		t.Run(tc.name, func(t *testing.T) {
			query := baseQuery
			query.Protocol, query.Mode = tc.protocol, tc.mode
			if _, ge := c.Models(ctx, query, core.InvocationServices{}); ge == nil {
				t.Fatal("Models accepted mismatched query")
			}
			usageQuery.Protocol, usageQuery.Mode = tc.protocol, tc.mode
			if _, ge := c.EstimateUsage(ctx, usageQuery, core.InvocationServices{}); ge == nil {
				t.Fatal("EstimateUsage accepted mismatched query")
			}
		})
	}
	if _, ge := c.Execute(ctx, core.ExecutionRequest{Payload: core.RawPayload{Protocol: protocol}}, core.AttemptScope{Mode: core.ModeNative}, core.InvocationServices{}); ge == nil || ge.Code != "unsupported_mode" {
		t.Fatalf("native Execute error = %+v, want unsupported mode", ge)
	}
	if _, ge := c.Execute(ctx, core.ExecutionRequest{Payload: core.RawPayload{Protocol: "wrong"}}, core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{}); ge == nil || ge.Code != "unsupported_protocol" {
		t.Fatalf("wrong-protocol Execute error = %+v, want unsupported protocol", ge)
	}
	cancelledExecute, cancelExecute := context.WithCancel(ctx)
	cancelExecute()
	if _, ge := c.Execute(cancelledExecute, core.ExecutionRequest{}, core.AttemptScope{}, core.InvocationServices{}); ge == nil || ge.Category != core.CategoryCancelled {
		t.Fatalf("cancelled Execute error = %+v, want cancelled", ge)
	}
	cancelledAuth, cancelAuth := context.WithCancel(ctx)
	cancelAuth()
	if _, ge := c.Authenticate(cancelledAuth, core.AuthRequest{}, core.InvocationServices{}); ge == nil || ge.Category != core.CategoryCancelled {
		t.Fatalf("cancelled Authenticate error = %+v, want cancelled", ge)
	}
	deadline, cancelDeadline := context.WithDeadline(ctx, time.Unix(1, 0))
	defer cancelDeadline()
	if _, ge := c.Execute(deadline, core.ExecutionRequest{}, core.AttemptScope{}, core.InvocationServices{}); ge == nil || ge.Category != core.CategoryTimeout {
		t.Fatalf("deadline Execute error = %+v, want timeout", ge)
	}
	if _, ge := c.Authenticate(deadline, core.AuthRequest{}, core.InvocationServices{}); ge == nil || ge.Category != core.CategoryTimeout {
		t.Fatalf("deadline Authenticate error = %+v, want timeout", ge)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if auth, ge := c.Authenticate(ctx, core.AuthRequest{}, core.InvocationServices{}); ge == nil || auth.Supported {
		t.Fatalf("closed Authenticate = (%+v, %+v), want unavailable", auth, ge)
	}
	if _, ge := c.Models(ctx, baseQuery, core.InvocationServices{}); ge == nil {
		t.Fatal("closed Models succeeded")
	}
	if _, ge := c.EstimateUsage(ctx, usageQuery, core.InvocationServices{}); ge == nil {
		t.Fatal("closed EstimateUsage succeeded")
	}
	if _, ge := c.Execute(ctx, core.ExecutionRequest{}, core.AttemptScope{}, core.InvocationServices{}); ge == nil || ge.Code != "connector_unavailable" {
		t.Fatalf("closed Execute error = %+v, want unavailable", ge)
	}
}
