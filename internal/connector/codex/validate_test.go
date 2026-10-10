package codex

import (
	"bytes"
	"fmt"
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
	toolChoice := []byte(`{"model":"gpt-5.4-mini","input":"x","stream":true,"store":false,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"lookup"}}`)
	if ge := validateResponsesProfile(responsesValidationRequest(toolChoice), core.ModeNative, "gpt-5.4-mini"); ge != nil {
		t.Fatalf("valid named function choice rejected: %#v", ge)
	}
	for _, parallel := range []string{"false"} {
		body := []byte(`{"model":"gpt-5.4-mini","input":"x","stream":true,"store":false,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"parallel_tool_calls":` + parallel + `}`)
		if ge := validateResponsesProfile(responsesValidationRequest(body), core.ModeNative, "gpt-5.4-mini"); ge != nil {
			t.Errorf("explicit parallel_tool_calls=%s rejected: %#v", parallel, ge)
		}
	}
	liteFunction := `{"model":"gpt-6-luna","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}],"stream":true,"store":false,"tool_choice":{"type":"function","name":"lookup"}}`
	liteRequest := responsesValidationRequest([]byte(liteFunction))
	liteRequest.Model = liteModel
	if ge := validateResponsesProfile(liteRequest, core.ModeNative, liteModel); ge != nil {
		t.Fatalf("direct Lite function tool rejected: %#v", ge)
	}
	liteNamespace := `{"model":"gpt-6-luna","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","description":"","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]}, {"type":"function_call_output","call_id":"c1","output":"done"}],"stream":true,"store":false,"tool_choice":{"type":"function","name":"lookup"}}`
	liteNamespaceRequest := responsesValidationRequest([]byte(liteNamespace))
	liteNamespaceRequest.Model = liteModel
	if ge := validateResponsesProfile(liteNamespaceRequest, core.ModeNative, liteModel); ge != nil {
		t.Fatalf("Lite functions namespace rejected: %s/%s", ge.Code, ge.Message)
	}
	opaqueNested := []byte(`{"model":"gpt-5.4-mini","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"kind":{"const":{"type":"input_image"}}}}}],"input":[{"type":"function_call","arguments":"{\"type\":\"input_audio\"}","vendor_extension":{"nested":{"type":"input_file"}}},{"type":"message","content":[{"type":"input_text","text":"ok"}],"vendor_extension":{"type":"input_image"}},{"type":"future_item","role":"developer","content":[{"type":"input_image"}],"extension":{"content":[{"type":"input_file"}]}}],"stream":true,"store":false}`)
	opaqueBefore := bytes.Clone(opaqueNested)
	if ge := validateResponsesProfile(responsesValidationRequest(opaqueNested), core.ModeNative, "gpt-5.4-mini"); ge != nil {
		t.Fatalf("opaque nested extension falsely rejected: %s/%s", ge.Code, ge.Message)
	}
	if !bytes.Equal(opaqueNested, opaqueBefore) {
		t.Fatal("validation mutated opaque nested values")
	}
	connector := &Connector{state: core.HealthReady, model: "gpt-5.4-mini", accountID: "account"}
	invalidRequest := responsesValidationRequest([]byte(`{"model":"gpt-5.4-mini","input":"x","stream":true,"store":true}`))
	if _, ge := connector.Execute(t.Context(), invalidRequest, core.AttemptScope{Mode: core.ModeNative, AccountID: "account"}, core.InvocationServices{}); ge == nil || ge.Category != core.CategoryUnsupportedFeature {
		t.Fatalf("Execute did not reject profile violation before execution: %#v", ge)
	}
	validRequest := responsesValidationRequest(loadResponsesFixture(t, "request-positive.json"))
	if _, ge := connector.Execute(t.Context(), validRequest, core.AttemptScope{Mode: core.ModeNative, AccountID: "account"}, core.InvocationServices{}); ge == nil || ge.Code != "credential_unavailable" {
		t.Fatalf("valid profile request did not fail closed without credentials: %#v", ge)
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
		{"server compaction context", `{"model":"gpt-5.4-mini","input":"x","stream":true,"store":false,"context_management":[{"type":"compaction"}]}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"server compaction trigger", `{"model":"gpt-5.4-mini","input":"x","stream":true,"store":false,"compaction_trigger":{"type":"threshold"}}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"output cap", `{"model":"gpt-5.4-mini","input":"x","stream":true,"store":false,"max_output_tokens":4}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"tool type", `{"model":"gpt-5.4-mini","input":"x","stream":true,"store":false,"tools":[{"type":"web_search"}]}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"unknown tool choice", `{"model":"gpt-5.4-mini","input":"x","stream":true,"store":false,"tool_choice":"random"}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"unknown named tool choice", `{"model":"gpt-5.4-mini","input":"x","stream":true,"store":false,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"missing"}}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"parallel tools", `{"model":"gpt-5.4-mini","input":"x","stream":true,"store":false,"parallel_tool_calls":true}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"malformed additional tools role", `{"model":"gpt-6-luna","input":[{"type":"additional_tools","role":"user","tools":[]}],"stream":true,"store":false}`, core.ModeNative, core.CategoryInvalidRequest},
		{"unsupported namespace", `{"model":"gpt-6-luna","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"other","tools":[]}]}],"stream":true,"store":false}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"custom namespace tool", `{"model":"gpt-6-luna","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"shell"}]}]}],"stream":true,"store":false}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"invalid function definition", `{"model":"gpt-6-luna","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","parameters":{"type":"object"}}]}],"stream":true,"store":false}`, core.ModeNative, core.CategoryInvalidRequest},
		{"array function parameters", `{"model":"gpt-6-luna","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"lookup","parameters":[]}]}],"stream":true,"store":false}`, core.ModeNative, core.CategoryInvalidRequest},
		{"scalar function parameters", `{"model":"gpt-6-luna","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"lookup","parameters":"schema"}]}],"stream":true,"store":false}`, core.ModeNative, core.CategoryInvalidRequest},
		{"invalid parallel tools", `{"model":"gpt-5.4-mini","input":"x","stream":true,"store":false,"parallel_tool_calls":"true"}`, core.ModeNative, core.CategoryInvalidRequest},
		{"resource item", `{"model":"gpt-5.4-mini","input":[{"type":"input_image","image_url":"secret"}],"stream":true,"store":false}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"resource object", `{"model":"gpt-5.4-mini","input":{"type":"input_file","file_id":"secret"},"stream":true,"store":false}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"resource message part", `{"model":"gpt-5.4-mini","input":[{"type":"message","content":[{"type":"input_image","image_url":"secret"}]}],"stream":true,"store":false}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"shorthand user audio part", `{"model":"gpt-5.4-mini","input":[{"role":"user","content":[{"type":"input_audio","audio":"secret"}]}],"stream":true,"store":false}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"function output resource array", `{"model":"gpt-5.4-mini","input":[{"type":"function_call_output","call_id":"call","output":[{"type":"input_image","image_url":"secret"}]}],"stream":true,"store":false}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"audio item", `{"model":"gpt-5.4-mini","input":[{"type":"input_audio","audio":"secret"}],"stream":true,"store":false}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"video item", `{"model":"gpt-5.4-mini","input":[{"type":"input_video","video":"secret"}],"stream":true,"store":false}`, core.ModeNative, core.CategoryUnsupportedFeature},
		{"file item", `{"model":"gpt-5.4-mini","input":[{"type":"input_file","file_id":"secret"}],"stream":true,"store":false}`, core.ModeNative, core.CategoryUnsupportedFeature},
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
			requestModel, configuredModel := "gpt-5.4-mini", "gpt-5.4-mini"
			if strings.Contains(tc.body, liteModel) {
				requestModel, configuredModel = liteModel, liteModel
			}
			request := responsesValidationRequest(body)
			request.Model = requestModel
			ge := validateResponsesProfile(request, tc.mode, configuredModel)
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

func TestValidateLiteCustomToolNamespaceAndHistory(t *testing.T) {
	const custom = `{"type":"custom","name":"synthetic_echo","description":"Echo a marker","format":{"type":"grammar","syntax":"lark","definition":"start: \"marker\""}}`
	const function = `{"type":"function","name":"lookup","description":"Lookup","parameters":{"type":"object"}}`
	const prefix = `{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","description":"","tools":[` + function + `,` + custom + `]}]}`
	body := []byte(`{"model":"gpt-6-luna","stream":true,"store":false,"parallel_tool_calls":false,"tool_choice":"auto","input":[` + prefix + `,{"type":"reasoning","id":"r","encrypted_content":"opaque<>&é","vendor":{"keep":true}},{"type":"function_call","call_id":"f1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"f1","output":"done"},{"type":"custom_tool_call","call_id":"c1","name":"synthetic_echo","input":"marker","extension":{"keep":"<>&é"}},{"type":"custom_tool_call_output","call_id":"c1","output":"echoed"}]}`)
	before := bytes.Clone(body)
	req := responsesValidationRequest(body)
	req.Model = liteModel
	if ge := validateResponsesProfileForProfile(req, core.ModeNative, liteModel, liteProfile); ge != nil {
		t.Fatalf("scoped Lite function/custom namespace and history rejected: %s/%s", ge.Code, ge.Message)
	}
	if !bytes.Equal(body, before) {
		t.Fatal("custom request validation changed native bytes")
	}

	tests := []struct {
		name, profile, mode, model, tools, choice string
	}{
		{"standard profile", profile, core.ModeNative, liteModel, custom, `"auto"`},
		{"translation mode", liteProfile, core.ModeTranslation, liteModel, custom, `"auto"`},
		{"other model", liteProfile, core.ModeNative, "gpt-5.4-mini", custom, `"auto"`},
		{"grammar text type", liteProfile, core.ModeNative, liteModel, strings.Replace(custom, `"grammar"`, `"text"`, 1), `"auto"`},
		{"unproven regex syntax", liteProfile, core.ModeNative, liteModel, strings.Replace(custom, `"lark"`, `"regex"`, 1), `"auto"`},
		{"function parameters on custom", liteProfile, core.ModeNative, liteModel, strings.Replace(custom, `,"format"`, `,"parameters":{"type":"object"},"format"`, 1), `"auto"`},
		{"forced custom choice", liteProfile, core.ModeNative, liteModel, custom, `{"type":"custom","name":"synthetic_echo"}`},
		{"forced function choice with custom", liteProfile, core.ModeNative, liteModel, function + `,` + custom, `{"type":"function","name":"lookup"}`},
		{"custom without explicit auto", liteProfile, core.ModeNative, liteModel, custom, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := `[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[` + tc.tools + `]}]}]`
			request := []byte(`{"model":` + fmt.Sprintf("%q", tc.model) + `,"stream":true,"store":false,"parallel_tool_calls":false,`)
			if tc.choice != "" {
				request = append(request, []byte(`"tool_choice":`+tc.choice+`,`)...)
			}
			request = append(request, []byte(`"input":`+input+`}`)...)
			before := bytes.Clone(request)
			validation := responsesValidationRequest(request)
			validation.Model = tc.model
			ge := validateResponsesProfileForProfile(validation, tc.mode, tc.model, tc.profile)
			if ge == nil || ge.Category != core.CategoryUnsupportedFeature {
				t.Fatalf("unsupported custom scope/shape accepted: %#v", ge)
			}
			if !bytes.Equal(request, before) {
				t.Fatal("validation mutated custom request")
			}
		})
	}

	badFormats := []string{
		`{"type":"custom","name":"x","description":null,"format":{"type":"grammar","syntax":"lark","definition":"d"}}`,
		`{"type":"custom","name":"x","description":"x","format":{"type":"grammar","syntax":"lark","definition":""}}`,
		`{"type":"custom","name":"x","description":"x","format":{"type":"grammar","syntax":"lark"}}`,
		`{"type":"custom","name":"x","description":"x","format":{"type":"grammar","syntax":"lark","definition":"d"},"defer_loading":"true"}`,
	}
	for _, invalidCustom := range badFormats {
		request := []byte(`{"model":"gpt-6-luna","stream":true,"store":false,"parallel_tool_calls":false,"tool_choice":"auto","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[` + invalidCustom + `]}]}]}`)
		validation := responsesValidationRequest(request)
		validation.Model = liteModel
		if ge := validateResponsesProfileForProfile(validation, core.ModeNative, liteModel, liteProfile); ge == nil {
			t.Fatalf("malformed custom format accepted: %s", invalidCustom)
		}
	}
	topLevelCustom := []byte(`{"model":"gpt-6-luna","stream":true,"store":false,"tool_choice":"auto","tools":[{"type":"namespace","name":"functions","tools":[` + custom + `]}],"input":"Use tools."}`)
	topLevelRequest := responsesValidationRequest(topLevelCustom)
	topLevelRequest.Model = liteModel
	if ge := validateResponsesProfileForProfile(topLevelRequest, core.ModeNative, liteModel, liteProfile); ge == nil || ge.Category != core.CategoryUnsupportedFeature {
		t.Fatalf("top-level custom namespace accepted outside the official additional_tools prefix: %#v", ge)
	}
	opaqueDefinition := strings.Replace(custom, `start: \"marker\"`, `not parsed by the gateway`, 1)
	opaqueRequest := []byte(`{"model":"gpt-6-luna","stream":true,"store":false,"tool_choice":"auto","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[` + opaqueDefinition + `]}]}]}`)
	opaqueValidation := responsesValidationRequest(opaqueRequest)
	opaqueValidation.Model = liteModel
	if ge := validateResponsesProfileForProfile(opaqueValidation, core.ModeNative, liteModel, liteProfile); ge != nil {
		t.Fatalf("opaque nonempty official grammar definition was parsed/rejected: %#v", ge)
	}
}

func TestCustomToolAdmissionFollowsSelectedProfile(t *testing.T) {
	body := []byte(`{"model":"gpt-6-luna","stream":true,"store":false,"parallel_tool_calls":false,"tool_choice":"auto","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"synthetic_echo","description":"Echo","format":{"type":"grammar","syntax":"lark","definition":"start: \"marker\""}}]}]}]}`)
	for _, tc := range []struct {
		name, selectedProfile string
		want                  string
	}{
		{"standard profile", profile, "unsupported_feature"},
		{"Lite profile", liteProfile, "credential_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Connector{state: core.HealthReady, model: liteModel, accountID: "account", profile: tc.selectedProfile}
			req := responsesValidationRequest(body)
			req.Model = liteModel
			_, ge := c.Execute(t.Context(), req, core.AttemptScope{Mode: core.ModeNative, AccountID: "account"}, core.InvocationServices{})
			if ge == nil || ge.Code != tc.want {
				t.Fatalf("profile %q custom request code=%v, want %s", tc.selectedProfile, ge, tc.want)
			}
		})
	}
}

func TestUnsupportedResourcePreflightMessageForms(t *testing.T) {
	reject := func(t *testing.T, body []byte) {
		t.Helper()
		if ge := validateResponsesProfile(responsesValidationRequest(body), core.ModeNative, "gpt-5.4-mini"); ge == nil || ge.Category != core.CategoryUnsupportedFeature {
			t.Fatalf("resource content not rejected: %#v", ge)
		}
	}
	roles := []struct{ name, field string }{
		{"user", `"role":"user",`},
		{"assistant", `"role":"assistant",`},
		{"developer", `"role":"developer",`},
		{"system", `"role":"system",`},
		{"role absent", ""},
	}
	forms := []struct{ name, field string }{
		{"typed", `"type":"message",`},
		{"shorthand", ""},
	}
	resources := []string{"input_image", "input_audio", "input_file", "input_video"}
	for _, form := range forms {
		for _, role := range roles {
			for _, resource := range resources {
				t.Run(form.name+"/"+role.name+"/"+resource, func(t *testing.T) {
					item := fmt.Sprintf(`{%s%s"content":[{"type":%q}]}`, form.field, role.field, resource)
					body := fmt.Appendf(nil, `{"model":"gpt-5.4-mini","input":[%s],"stream":true,"store":false}`, item)
					reject(t, body)
				})
			}
		}
	}
	for _, resource := range resources {
		t.Run("function_call_output/"+resource, func(t *testing.T) {
			input := fmt.Sprintf(`[{"type":"function_call_output","output":[{"type":%q}]}]`, resource)
			body := fmt.Appendf(nil, `{"model":"gpt-5.4-mini","input":%s,"stream":true,"store":false}`, input)
			reject(t, body)
		})
	}
}

func responsesValidationRequest(body []byte) core.ExecutionRequest {
	return core.ExecutionRequest{Model: "gpt-5.4-mini", Payload: core.RawPayload{Protocol: protocol, Body: body}}
}
