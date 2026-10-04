package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/blestafist/pestiroute/internal/core"
)

func config(model, account string) core.ComponentConfig {
	b, _ := json.Marshal(componentConfig{Model: model, AccountID: account})
	return core.ComponentConfig{Data: b}
}

func TestHTTPDoerAccessorExposesConfiguredNoRedirectClient(t *testing.T) {
	c := NewConnector()
	if c.HTTPDoer() != nil {
		t.Fatal("uninitialized connector exposed an HTTP client")
	}
	if err := c.Init(context.Background(), config("client-model", "account-a")); err != nil {
		t.Fatal(err)
	}
	got, ok := c.HTTPDoer().(*http.Client)
	if !ok || got != c.transport.client {
		t.Fatalf("HTTPDoer() = %T, want connector's configured *http.Client", c.HTTPDoer())
	}
	if err := got.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatalf("configured redirect policy = %v, want ErrUseLastResponse", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.HTTPDoer() != nil {
		t.Fatal("closed connector exposed an HTTP client")
	}
}

func TestConnectorLifecycleScopeAndSupport(t *testing.T) {
	c := NewConnector()
	d := c.Descriptor()
	if err := d.Validate(core.ComponentConnector); err != nil || !d.SupportsAPIVersion(core.APIVersion{Major: 1, Minor: 0}) {
		t.Fatalf("descriptor invalid: %v", err)
	}
	if d.Protocols[0] != protocol || d.ConnectorType != "api" || len(d.AuthMethods) != 1 || d.AuthMethods[0] != "bearer" {
		t.Fatalf("descriptor declarations: %+v", d)
	}
	d.Protocols[0] = "mutated"
	if c.Descriptor().Protocols[0] != protocol {
		t.Fatal("descriptor returned shared mutable declarations")
	}
	if got := c.Health(context.Background()).State; got != core.HealthUnknown {
		t.Fatalf("initial health = %q", got)
	}
	if err := c.Init(context.Background(), config("gpt-4.1-mini", "account-a")); err != nil {
		t.Fatal(err)
	}
	if got := c.Health(context.Background()).State; got != core.HealthReady {
		t.Fatalf("initialized health = %q", got)
	}
	scope := core.CapabilityScope{Protocol: protocol, Mode: core.ModeTranslation, Model: "gpt-4.1-mini", AccountID: "account-a"}
	if got := c.Capabilities(context.Background(), scope).State("llm.streaming"); got != core.Supported {
		t.Fatalf("implemented streaming capability = %q", got)
	}
	if got := c.Capabilities(context.Background(), scope).State("llm.tools"); got != core.Supported {
		t.Fatalf("implemented tools capability = %q", got)
	}
	for _, capability := range []core.Capability{"llm.tools.parallel", "llm.reasoning", "llm.structured_output", "llm.vision", "llm.audio", "auth.oauth", "usage.exact"} {
		if got := c.Capabilities(context.Background(), scope).State(capability); got != core.Unknown {
			t.Errorf("unproven capability %q = %q, want unknown", capability, got)
		}
	}
	for _, bad := range []core.CapabilityScope{
		{Protocol: "other", Mode: scope.Mode, Model: scope.Model, AccountID: scope.AccountID},
		{Protocol: protocol, Mode: core.ModeNative, Model: scope.Model, AccountID: scope.AccountID},
		{Protocol: protocol, Mode: scope.Mode, Model: "other", AccountID: scope.AccountID},
		{Protocol: protocol, Mode: scope.Mode, Model: scope.Model, AccountID: "other"},
	} {
		if got := c.Capabilities(context.Background(), bad).Values; len(got) != 0 {
			t.Errorf("unmatched scope returned capabilities: %v", got)
		}
	}
	models, err := c.Models(context.Background(), core.ModelQuery{Protocol: protocol, Mode: core.ModeTranslation, AccountID: "account-a"}, core.InvocationServices{})
	if err != nil || !models.Supported || len(models.Models) != 1 || models.Models[0].ID != "gpt-4.1-mini" {
		t.Fatalf("scoped models = %+v, %v", models, err)
	}
	if models.Models[0].Capabilities["llm.streaming"] != core.Supported {
		t.Fatalf("models omitted verified streaming capability: %+v", models.Models[0])
	}
	if models.Models[0].Capabilities["llm.tools"] != core.Supported {
		t.Fatalf("models omitted verified tools capability: %+v", models.Models[0])
	}
	for _, capability := range []core.Capability{"llm.tools.parallel", "llm.reasoning", "llm.structured_output", "llm.vision", "llm.audio", "auth.oauth", "usage.exact"} {
		if got := (core.CapabilityResult{Values: models.Models[0].Capabilities}).State(capability); got != core.Unknown {
			t.Errorf("model advertised unproven capability %q = %q, want unknown", capability, got)
		}
	}
	for _, query := range []core.ModelQuery{
		{Protocol: "other", Mode: core.ModeTranslation, AccountID: "account-a"},
		{Protocol: protocol, Mode: core.ModeNative, AccountID: "account-a"},
		{Protocol: protocol, Mode: core.ModeTranslation, AccountID: "other"},
	} {
		got, err := c.Models(context.Background(), query, core.InvocationServices{})
		if err != nil || got.Supported || len(got.Models) != 0 {
			t.Errorf("unmatched model query = %+v, %v", got, err)
		}
	}
	auth, err := c.Authenticate(context.Background(), core.AuthRequest{}, core.InvocationServices{})
	if err != nil || auth.Supported || auth.Credentials != nil {
		t.Fatalf("interactive auth result = %+v, %v", auth, err)
	}
	estimate, err := c.EstimateUsage(context.Background(), core.UsageQuery{Protocol: protocol, Mode: core.ModeTranslation, Model: "gpt-4.1-mini", AccountID: "account-a"}, core.InvocationServices{})
	if err != nil || !estimate.Supported || estimate.Known || estimate.Usage != nil || estimate.Method != "conservative" {
		t.Fatalf("usage estimate = %+v, %v", estimate, err)
	}
	_, executionErr := c.Execute(context.Background(), core.ExecutionRequest{Model: "gpt-4.1-mini", Payload: core.RawPayload{Protocol: protocol}}, core.AttemptScope{Mode: core.ModeTranslation, AccountID: "account-a"}, core.InvocationServices{})
	if executionErr == nil || executionErr.Code != "invalid_request" {
		t.Fatalf("invalid Execute payload error = %+v", executionErr)
	}
	for _, tc := range []struct {
		name, protocol, mode, model, account, want string
	}{
		{"protocol", "other", core.ModeTranslation, "gpt-4.1-mini", "account-a", "unsupported_protocol"},
		{"mode", protocol, core.ModeNative, "gpt-4.1-mini", "account-a", "unsupported_mode"},
		{"model", protocol, core.ModeTranslation, "other", "account-a", "scope_mismatch"},
		{"account", protocol, core.ModeTranslation, "gpt-4.1-mini", "other", "scope_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got := c.Execute(context.Background(), core.ExecutionRequest{Model: tc.model, Payload: core.RawPayload{Protocol: tc.protocol}}, core.AttemptScope{Mode: tc.mode, AccountID: tc.account}, core.InvocationServices{})
			if got == nil || got.Code != tc.want {
				t.Fatalf("Execute mismatch error = %+v, want %q", got, tc.want)
			}
		})
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.Health(context.Background()).State; got != core.HealthUnavailable {
		t.Fatalf("closed health = %q", got)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("repeated Close: %v", err)
	}
	if err := c.Init(context.Background(), config("model", "account")); err == nil {
		t.Fatal("Init after Close succeeded")
	}
}

func TestEstimateUsageScopeReadinessAndPayloadImmutability(t *testing.T) {
	c := NewConnector()
	payload := []byte(`{"model":"client-model","stream":true,"input":"unchanged","max_output_tokens":17}`)
	query := core.UsageQuery{Protocol: protocol, Mode: core.ModeTranslation, Model: "gpt-4.1-mini", AccountID: "account-a", Payload: core.RawPayload{Protocol: protocol, Body: payload}}
	if _, got := c.EstimateUsage(context.Background(), query, core.InvocationServices{}); got == nil || got.Code != "connector_unavailable" {
		t.Fatalf("unready estimate error = %+v", got)
	}
	if err := c.Init(context.Background(), config("gpt-4.1-mini", "account-a")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, protocol, mode, model, account, want string
	}{
		{"protocol", "other", core.ModeTranslation, "gpt-4.1-mini", "account-a", "unsupported_protocol"},
		{"mode", protocol, core.ModeNative, "gpt-4.1-mini", "account-a", "unsupported_mode"},
		{"model", protocol, core.ModeTranslation, "other", "account-a", "scope_mismatch"},
		{"account", protocol, core.ModeTranslation, "gpt-4.1-mini", "other", "scope_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := query
			bad.Protocol, bad.Mode, bad.Model, bad.AccountID = tc.protocol, tc.mode, tc.model, tc.account
			if _, got := c.EstimateUsage(context.Background(), bad, core.InvocationServices{}); got == nil || got.Code != tc.want {
				t.Fatalf("estimate error = %+v, want %q", got, tc.want)
			}
		})
	}
	got, gatewayErr := c.EstimateUsage(context.Background(), query, core.InvocationServices{})
	if gatewayErr != nil || !got.Supported || got.Known || got.Usage != nil || got.Method != "conservative" {
		t.Fatalf("estimate = %+v, %v", got, gatewayErr)
	}
	if string(query.Payload.Body) != `{"model":"client-model","stream":true,"input":"unchanged","max_output_tokens":17}` {
		t.Fatalf("payload changed during estimate: %s", query.Payload.Body)
	}
}

func TestConnectorFailedInitAndConcurrentClose(t *testing.T) {
	c := NewConnector()
	for _, data := range [][]byte{
		[]byte(`{"model":"m","account_id":"a","api_key":"secret"}`),
		[]byte(`{"model":"m","account_id":"a"} {}`),
		[]byte(`{"model":" ","account_id":"a"}`),
	} {
		if err := c.Init(context.Background(), core.ComponentConfig{Data: data}); err == nil {
			t.Fatalf("invalid config accepted: %s", data)
		}
		if got := c.Health(context.Background()).State; got != core.HealthUnavailable {
			t.Fatalf("health after failed init = %q", got)
		}
		if _, err := c.Execute(context.Background(), core.ExecutionRequest{}, core.AttemptScope{}, core.InvocationServices{}); err == nil || err.Code != "connector_unavailable" {
			t.Fatalf("failed init allowed execution: %+v", err)
		}
	}
	if err := c.Init(context.Background(), config("m", "a")); err != nil {
		t.Fatalf("valid retry after failed Init: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Close(context.Background()); err != nil {
				t.Errorf("concurrent Close: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := c.Health(context.Background()).State; got != core.HealthUnavailable {
		t.Fatalf("health after concurrent Close = %q", got)
	}
}
