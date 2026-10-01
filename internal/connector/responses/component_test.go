package responses

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

func componentConfigJSON(endpoint string) []byte {
	b, _ := json.Marshal(componentConfig{Transport: Config{
		Endpoint: endpoint, Credential: "synthetic", ConnectTimeout: time.Second,
	}, Model: modelID, AccountID: "account-a"})
	return b
}

func TestConnectorLifecycleSupportAndExactScope(t *testing.T) {
	c := NewConnector()
	d := c.Descriptor()
	if err := d.Validate(core.ComponentConnector); err != nil || !d.SupportsAPIVersion(core.APIVersion{Major: 1}) {
		t.Fatalf("invalid connector descriptor: %v", err)
	}
	if got := c.Health(context.Background()).State; got != core.HealthUnknown {
		t.Fatalf("initial health = %q", got)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{Data: componentConfigJSON("http://127.0.0.1:9999/v1/responses")}); err != nil {
		t.Fatal(err)
	}
	if got := c.Health(context.Background()).State; got != core.HealthReady {
		t.Fatalf("health after init = %q", got)
	}
	base := core.CapabilityScope{Protocol: protocol, Mode: "native", Model: modelID, AccountID: "account-a"}
	result := c.Capabilities(context.Background(), base)
	for _, capability := range []core.Capability{"llm.streaming", "llm.tools", "llm.reasoning"} {
		if result.State(capability) != core.Supported {
			t.Errorf("%s = %q, want supported", capability, result.State(capability))
		}
	}
	if result.State("llm.tools.parallel") != core.Unknown {
		t.Fatal("parallel tool support must remain unknown")
	}
	for _, scope := range []core.CapabilityScope{
		{Protocol: "openai.chat.v1", Mode: "native", Model: modelID, AccountID: "account-a"},
		{Protocol: protocol, Mode: "translation", Model: modelID, AccountID: "account-a"},
		{Protocol: protocol, Mode: "native", Model: "other", AccountID: "account-a"},
		{Protocol: protocol, Mode: "native", Model: modelID, AccountID: "other"},
	} {
		if got := c.Capabilities(context.Background(), scope).State("llm.tools"); got != core.Unknown {
			t.Errorf("unmatched scope capability = %q", got)
		}
	}
	models, err := c.Models(context.Background(), core.ModelQuery{Protocol: protocol, Mode: "native", AccountID: "account-a"}, core.InvocationServices{})
	if err != nil || !models.Supported || len(models.Models) != 1 || models.Models[0].ID != modelID || models.Models[0].Available != nil {
		t.Fatalf("Models = %#v, %v", models, err)
	}
	unknown, err := c.Models(context.Background(), core.ModelQuery{Protocol: protocol, Mode: "native", AccountID: "other"}, core.InvocationServices{})
	if err != nil || unknown.Supported || len(unknown.Models) != 0 {
		t.Fatalf("unmatched Models = %#v, %v", unknown, err)
	}
	estimate, err := c.EstimateUsage(context.Background(), core.UsageQuery{}, core.InvocationServices{})
	if err != nil || estimate.Supported || estimate.Known || estimate.Usage != nil {
		t.Fatalf("EstimateUsage = %#v, %v", estimate, err)
	}
	auth, err := c.Authenticate(context.Background(), core.AuthRequest{}, core.InvocationServices{})
	if err != nil || auth.Supported || auth.Credentials != nil {
		t.Fatalf("Authenticate = %#v, %v", auth, err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := c.Health(context.Background()).State; got != core.HealthUnavailable {
		t.Fatalf("health after close = %q", got)
	}
}

func TestConnectorInitRejectsInvalidConfigAndUnknownEndpointCapability(t *testing.T) {
	c := NewConnector()
	if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"transport":{"endpoint":"ftp://example.com/v1/responses","credential":"x"},"model":"gpt-5.4-mini","account_id":"account-a"}`)}); err == nil {
		t.Fatal("invalid endpoint configuration was accepted")
	}
	if got := c.Health(context.Background()).State; got != core.HealthUnknown {
		t.Fatalf("health after failed init = %q", got)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{Data: componentConfigJSON("http://example.com/v1/responses")}); err != nil {
		t.Fatal(err)
	}
	if got := c.Capabilities(context.Background(), core.CapabilityScope{Protocol: protocol, Mode: "native", Model: modelID, AccountID: "account-a"}).State("llm.tools"); got != core.Unknown {
		t.Fatalf("unverified endpoint capability = %q", got)
	}
	_ = c.Close(context.Background())
}
