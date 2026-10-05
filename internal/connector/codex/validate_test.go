package codex

import (
	"bytes"
	"strings"
	"testing"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestValidateResponsesProfile(t *testing.T) {
	fixtures := []string{"request-positive.json", "request-tools-history.json", "request-reasoning-item.json", "request-unknown-fields.json"}
	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			body := loadResponsesFixture(t, fixture)
			before := bytes.Clone(body)
			if ge := validateResponsesProfile(responsesValidationRequest(body), core.ModeNative, "gpt-5.4-mini"); ge != nil {
				t.Fatalf("valid fixture rejected: %#v", ge)
			}
			if !bytes.Equal(body, before) {
				t.Fatal("validation mutated request bytes")
			}
		})
	}

	translation := []byte(`{"model":"gpt-5.4-mini","input":"hello"}`)
	if ge := validateResponsesProfile(responsesValidationRequest(translation), core.ModeTranslation, "gpt-5.4-mini"); ge != nil {
		t.Fatalf("translation defaults were not admitted: %#v", ge)
	}
	connector := &Connector{state: core.HealthReady, model: "gpt-5.4-mini", accountID: "account"}
	invalidRequest := responsesValidationRequest([]byte(`{"model":"gpt-5.4-mini","input":"x","stream":true,"store":true}`))
	if _, ge := connector.Execute(t.Context(), invalidRequest, core.AttemptScope{Mode: core.ModeNative, AccountID: "account"}, core.InvocationServices{}); ge == nil || ge.Category != core.CategoryUnsupportedFeature {
		t.Fatalf("Execute did not reject profile violation before execution: %#v", ge)
	}
	validRequest := responsesValidationRequest(loadResponsesFixture(t, "request-positive.json"))
	if _, ge := connector.Execute(t.Context(), validRequest, core.AttemptScope{Mode: core.ModeNative, AccountID: "account"}, core.InvocationServices{}); ge == nil || ge.Code != "unsupported_operation" {
		t.Fatalf("valid profile request was not admitted to the unimplemented execution seam: %#v", ge)
	}

	bad := []struct {
		name, body string
		mode       string
		category   core.ErrorCategory
	}{
		{"store true", `{"model":"gpt-5.4-mini","input":"x","stream":true,"store":true}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"stream false", `{"model":"gpt-5.4-mini","input":"x","stream":false,"store":false}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"native stream required", `{"model":"gpt-5.4-mini","input":"x","store":false}`, core.ModeNative, core.CategoryInvalidRequest},
		{"native store required", `{"model":"gpt-5.4-mini","input":"x","stream":true}`, core.ModeNative, core.CategoryInvalidRequest},
		{"translation store true", `{"model":"gpt-5.4-mini","input":"x","store":true}`, core.ModeTranslation, core.CategoryUnsupportedFeature},
		{"background", `{"model":"gpt-5.4-mini","input":"x","stream":true,"store":false,"background":true}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"continuation", `{"model":"gpt-5.4-mini","input":"x","stream":true,"store":false,"previous_response_id":"private"}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"conversation", `{"model":"gpt-5.4-mini","input":"x","stream":true,"store":false,"conversation":"private"}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"output cap", `{"model":"gpt-5.4-mini","input":"x","stream":true,"store":false,"max_output_tokens":4}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"tool type", `{"model":"gpt-5.4-mini","input":"x","stream":true,"store":false,"tools":[{"type":"web_search"}]}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"unverified tool choice", `{"model":"gpt-5.4-mini","input":"x","stream":true,"store":false,"tool_choice":"auto"}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"unverified parallel tools", `{"model":"gpt-5.4-mini","input":"x","stream":true,"store":false,"parallel_tool_calls":true}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"resource item", `{"model":"gpt-5.4-mini","input":[{"type":"input_image","image_url":"secret"}],"stream":true,"store":false}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"model mismatch", `{"model":"other","input":"x","stream":true,"store":false}`, core.ModeNative, core.CategoryInvalidRequest},
		{"duplicate nested", `{"model":"gpt-5.4-mini","input":[{"type":"message","type":"message"}],"stream":true,"store":false}`, core.ModeNative, core.CategoryInvalidRequest},
		{"malformed", `{"model":"private-secret`, core.ModeNative, core.CategoryInvalidRequest},
		{"case alias", `{"Model":"gpt-5.4-mini","model":"gpt-5.4-mini","input":"x","stream":true,"store":false}`, core.ModeNative, core.CategoryInvalidRequest},
		{"oversize", `{"model":"gpt-5.4-mini","input":"` + strings.Repeat("x", maxResponsesBodyBytes) + `","stream":true,"store":false}`, core.ModeNative, core.CategoryInvalidRequest},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			before := bytes.Clone(body)
			ge := validateResponsesProfile(responsesValidationRequest(body), tc.mode, "gpt-5.4-mini")
			if ge == nil || ge.Category != tc.category {
				t.Fatalf("got %#v, want category %q", ge, tc.category)
			}
			if strings.Contains(ge.Message, "private-secret") || strings.Contains(ge.Message, "secret") {
				t.Fatalf("error leaked payload: %q", ge.Message)
			}
			if !bytes.Equal(body, before) {
				t.Fatal("validation mutated request bytes")
			}
		})
	}
}

func responsesValidationRequest(body []byte) core.ExecutionRequest {
	return core.ExecutionRequest{Model: "gpt-5.4-mini", Payload: core.RawPayload{Protocol: protocol, Body: body}}
}
