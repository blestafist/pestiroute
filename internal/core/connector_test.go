package core

import (
	"context"
	"net/http"
	"testing"
)

type minimalConnector struct{}

func (minimalConnector) Descriptor() Descriptor                      { return Descriptor{} }
func (minimalConnector) Init(context.Context, ComponentConfig) error { return nil }
func (minimalConnector) Health(context.Context) Health               { return Health{} }
func (minimalConnector) Capabilities(context.Context, CapabilityScope) CapabilityResult {
	return CapabilityResult{}
}
func (minimalConnector) Close(context.Context) error { return nil }
func (minimalConnector) Execute(context.Context, ExecutionRequest, AttemptScope, InvocationServices) (ExecutionResponse, *GatewayError) {
	return ExecutionResponse{}, nil
}
func (minimalConnector) Models(context.Context, ModelQuery, InvocationServices) (ModelsResult, *GatewayError) {
	available, unavailable := true, false
	return ModelsResult{Supported: true, Models: []ModelInfo{
		{ID: "available", Available: &available},
		{ID: "unavailable", Available: &unavailable},
		{ID: "unknown", Available: nil},
	}}, nil
}
func (minimalConnector) EstimateUsage(_ context.Context, query UsageQuery, _ InvocationServices) (EstimateResult, *GatewayError) {
	if query.Model == "known-zero" {
		zero := int64(0)
		return EstimateResult{Supported: true, Known: true, Usage: &UsageReport{InputTokens: &zero, OutputTokens: &zero}}, nil
	}
	return EstimateResult{Supported: true}, nil
}
func (minimalConnector) Authenticate(context.Context, AuthRequest, InvocationServices) (AuthResult, *GatewayError) {
	return AuthResult{}, nil
}

var _ Connector = minimalConnector{}
var _ HTTPDoer = http.DefaultClient

func TestConnectorSupportOperationsRepresentUnknownAndUnsupported(t *testing.T) {
	connector := minimalConnector{}
	services := InvocationServices{}

	unknown, err := connector.EstimateUsage(context.Background(), UsageQuery{}, services)
	if err != nil || !unknown.Supported || unknown.Known || unknown.Usage != nil {
		t.Fatalf("unknown estimate = %#v, %v", unknown, err)
	}

	known, err := connector.EstimateUsage(context.Background(), UsageQuery{Model: "known-zero"}, services)
	if err != nil || !known.Supported || !known.Known || known.Usage == nil || known.Usage.InputTokens == nil || known.Usage.OutputTokens == nil || *known.Usage.InputTokens != 0 || *known.Usage.OutputTokens != 0 {
		t.Fatalf("known zero estimate = %#v", known)
	}

	models, err := connector.Models(context.Background(), ModelQuery{}, services)
	if err != nil || !models.Supported || len(models.Models) != 3 {
		t.Fatalf("models = %#v, %v", models, err)
	}
	for i, want := range []struct {
		id        string
		available *bool
	}{{"available", boolPtr(true)}, {"unavailable", boolPtr(false)}, {"unknown", nil}} {
		got := models.Models[i]
		if got.ID != want.id || (got.Available == nil) != (want.available == nil) ||
			(got.Available != nil && *got.Available != *want.available) {
			t.Fatalf("model[%d] = %#v, want ID %q availability %v", i, got, want.id, want.available)
		}
	}

	auth, err := connector.Authenticate(context.Background(), AuthRequest{}, services)
	if err != nil || auth.Supported || auth.State != "" || len(auth.Credentials) != 0 {
		t.Fatalf("unsupported auth = %#v, %v", auth, err)
	}
}

func boolPtr(value bool) *bool { return &value }
