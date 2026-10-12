package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
)

func componentConfigJSON(endpoint string) []byte {
	b, _ := json.Marshal(componentConfig{Transport: Config{
		Endpoint: endpoint, ConnectTimeout: time.Second,
	}, Model: modelID, AccountID: "account-a"})
	return b
}

func localComponentConfigJSON(endpoint, authMode string) []byte {
	b, _ := json.Marshal(componentConfig{Transport: Config{Endpoint: endpoint, ConnectTimeout: time.Second}, Model: modelID, AccountID: "account-a", AuthMode: authMode, RuntimeProfile: profileOllama})
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
	if got := c.Health(context.Background()).State; got != core.HealthUnavailable {
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

func TestLocalConnectorRequiresExplicitAuthModeAndReportsDescriptor(t *testing.T) {
	for _, data := range [][]byte{
		[]byte(`{"transport":{"endpoint":"http://127.0.0.1:1/v1/responses"},"model":"m","account_id":"a"}`),
		[]byte(`{"transport":{"endpoint":"http://127.0.0.1:1/v1/responses"},"model":"m","account_id":"a","auth_mode":""}`),
		[]byte(`{"transport":{"endpoint":"http://127.0.0.1:1/v1/responses"},"model":"m","account_id":"a","auth_mode":"api_key"}`),
		[]byte(`{"transport":{"endpoint":"http://127.0.0.1:1/v1/responses"},"model":"m","account_id":"a","auth_mode":"none","runtime_profile":"ollama-latest"}`),
		[]byte(`{"transport":{"endpoint":"http://127.0.0.1:1/v1/responses"},"model":"m","account_id":"a","auth_mode":"none","unknown":true}`),
		[]byte(`{"transport":{"endpoint":"http://127.0.0.1:1/v1/responses"},"model":"m","account_id":"a","auth_mode":"none"} {}`),
	} {
		if err := NewLocalConnector(authNone).Init(context.Background(), core.ComponentConfig{Data: data}); err == nil {
			t.Fatalf("invalid local auth config accepted: %s", data)
		}
	}
	for _, mode := range []string{authNone, authBearer} {
		c := NewLocalConnector(mode)
		if err := c.Init(context.Background(), core.ComponentConfig{Data: localComponentConfigJSON("http://127.0.0.1:1/v1/responses", mode)}); err != nil {
			t.Fatal(err)
		}
		d := c.Descriptor()
		if d.ID != localComponentID || d.ConnectorType != "local" || len(d.AuthMethods) != 1 || d.AuthMethods[0] != mode {
			t.Fatalf("local descriptor for %s: %+v", mode, d)
		}
		if err := d.Validate(core.ComponentConnector); err != nil {
			t.Fatalf("invalid local descriptor: %v", err)
		}
		_ = c.Close(context.Background())
	}
	native := NewConnector().Descriptor()
	if native.ID != componentID || native.ConnectorType != "api" || len(native.AuthMethods) != 1 || native.AuthMethods[0] != authBearer {
		t.Fatalf("native descriptor changed: %+v", native)
	}
}

func TestLocalConnectorCapabilitiesAndModelsUseExactDeclaredScope(t *testing.T) {
	const model = "llama3.2:3b"
	c := NewLocalConnector(authNone)
	data, marshalErr := json.Marshal(componentConfig{
		Transport: Config{Endpoint: "http://127.0.0.1:11434/v1/responses"},
		Model:     model, AccountID: "local-account", AuthMode: authNone, RuntimeProfile: profileOllama,
		Capabilities: map[core.Capability]core.CapabilityState{"llm.streaming": core.Supported},
	})
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{Data: data}); err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())

	scope := core.CapabilityScope{Protocol: protocol, Mode: core.ModeNative, Model: model, AccountID: "local-account"}
	got := c.Capabilities(context.Background(), scope)
	if got.State("llm.streaming") != core.Supported || got.State("llm.tools") != core.Unknown || got.State("llm.reasoning") != core.Unknown || got.State("llm.tools.parallel") != core.Unknown {
		t.Fatalf("local declarations/defaults = %#v", got.Values)
	}
	for _, mismatch := range []core.CapabilityScope{
		{Protocol: "openai.chat.v1", Mode: scope.Mode, Model: model, AccountID: scope.AccountID},
		{Protocol: protocol, Mode: "translation", Model: model, AccountID: scope.AccountID},
		{Protocol: protocol, Mode: scope.Mode, Model: "qwen2.5:7b", AccountID: scope.AccountID},
		{Protocol: protocol, Mode: scope.Mode, Model: model, AccountID: "other-account"},
	} {
		if values := c.Capabilities(context.Background(), mismatch).Values; len(values) != 0 {
			t.Errorf("mismatched capability scope returned %#v", values)
		}
	}
	models, modelErr := c.Models(context.Background(), core.ModelQuery{Protocol: protocol, Mode: scope.Mode, AccountID: scope.AccountID}, core.InvocationServices{})
	if modelErr != nil || !models.Supported || len(models.Models) != 1 {
		t.Fatalf("exact Models query supported=%v count=%d, %v", models.Supported, len(models.Models), modelErr)
	}
	if models.Models[0].ID != model {
		t.Fatalf("Models ID = %q, want %q", models.Models[0].ID, model)
	}
	for _, mismatch := range []core.ModelQuery{
		{Protocol: "openai.chat.v1", Mode: scope.Mode, AccountID: scope.AccountID},
		{Protocol: protocol, Mode: "translation", AccountID: scope.AccountID},
		{Protocol: protocol, Mode: scope.Mode, AccountID: "other-account"},
	} {
		result, modelErr := c.Models(context.Background(), mismatch, core.InvocationServices{})
		if modelErr != nil || result.Supported || len(result.Models) != 0 {
			t.Errorf("mismatched Models query = %#v, %v", result, modelErr)
		}
	}

	up := fakeupstream.New(fakeupstream.Response{Body: []byte(`{"id":"resp","status":"completed"}`)})
	defer up.Close()
	routed, err := json.Marshal(componentConfig{Transport: Config{Endpoint: up.URL + "/v1/responses"}, Model: model, AccountID: scope.AccountID, AuthMode: authNone, RuntimeProfile: profileOllama})
	if err != nil {
		t.Fatal(err)
	}
	executor := NewLocalConnector(authNone)
	if err := executor.Init(context.Background(), core.ComponentConfig{Data: routed}); err != nil {
		t.Fatal(err)
	}
	defer executor.Close(context.Background())
	request := fixedRequest()
	request.Model = model
	response, gatewayErr := executor.Execute(context.Background(), request, core.AttemptScope{Mode: scope.Mode, AccountID: scope.AccountID}, core.InvocationServices{})
	if gatewayErr != nil {
		t.Fatalf("arbitrary local model Execute: %v", gatewayErr)
	}
	response.Stream.Close()
	capture := <-up.Requests
	if string(capture.Body) != string(request.Payload.Body) {
		t.Fatalf("local model request bytes changed: %q", capture.Body)
	}
}

func TestLocalConnectorLoopbackDoesNotInheritNativeCapabilities(t *testing.T) {
	c := NewLocalConnector(authNone)
	if err := c.Init(context.Background(), core.ComponentConfig{Data: localComponentConfigJSON("http://127.0.0.1:11434/v1/responses", authNone)}); err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	scope := core.CapabilityScope{Protocol: protocol, Mode: core.ModeNative, Model: modelID, AccountID: "account-a"}
	got := c.Capabilities(context.Background(), scope)
	for _, capability := range []core.Capability{"llm.streaming", "llm.tools", "llm.reasoning", "llm.tools.parallel", "usage.exact"} {
		if got.State(capability) != core.Unknown {
			t.Errorf("undeclared local %s = %q, want unknown", capability, got.State(capability))
		}
	}
	estimate, err := c.EstimateUsage(context.Background(), core.UsageQuery{}, core.InvocationServices{})
	if err != nil || estimate.Supported || estimate.Known {
		t.Fatalf("local EstimateUsage = %#v, %v", estimate, err)
	}
}

func TestLocalConnectorPreflightRejectsUnsupportedSemanticsBeforeDispatch(t *testing.T) {
	cases := []struct {
		profile string
		body    string
	}{
		{profileOllama, `{"previous_response_id":"resp_1"}`},
		{profileOllama, `{"conversation":"thread_1"}`},
		{profileOllama, `{"background":true}`},
		{profileOllama, `{"include":["message.output_text.logprobs"]}`},
		{profileOllama, `{"tools":[{"type":"function","name":"f"}],"tool_choice":"auto"}`},
		{profileVLLM, `{"store":true}`},
		{profileVLLM, `{"background":true}`},
		{profileVLLM, `{"previous_response_id":"resp_1"}`},
		{profileLlama, `{"previous_response_id":"resp_1"}`},
		{profileLlama, `{"tools":[{"type":"custom"}]}`},
		{profileLlama, `{"tools":[{"type":"function","name":"f"}],"tool_choice":"auto"}`},
		{profileVLLM, `{"tools":[{"type":"function","name":"f","parameters":{}}]}`},
		{profileVLLM, `{"parallel_tool_calls":true}`},
		{profileVLLM, `{"reasoning":{"effort":"high"}}`},
		{profileVLLM, `{"text":{"format":{"type":"json_object"}}}`},
		{profileVLLM, `{"input":[{"type":"message","content":[{"type":"input_image","image_url":"x"}]}]}`},
	}
	var calls atomic.Int32
	services := core.InvocationServices{Transport: doerFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected dispatch")
	})}
	for _, tc := range cases {
		t.Run(tc.profile+"/"+tc.body, func(t *testing.T) {
			c := NewLocalConnector(authNone)
			data, _ := json.Marshal(componentConfig{Transport: Config{Endpoint: "http://127.0.0.1:11434/v1/responses"}, Model: modelID, AccountID: "account-a", AuthMode: authNone, RuntimeProfile: tc.profile})
			if err := c.Init(context.Background(), core.ComponentConfig{Data: data}); err != nil {
				t.Fatal(err)
			}
			defer c.Close(context.Background())
			req := fixedRequest()
			req.Model, req.Payload.Body = modelID, []byte(tc.body)
			_, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: "native", AccountID: "account-a"}, services)
			if ge == nil || ge.Code != "unsupported_semantics" || strings.Contains(ge.Message, tc.body) || calls.Load() != 0 {
				t.Fatalf("preflight error=%v dispatches=%d", ge, calls.Load())
			}
		})
	}

	c := NewLocalConnector(authNone)
	if err := c.Init(context.Background(), core.ComponentConfig{Data: localComponentConfigJSON("http://127.0.0.1:11434/v1/responses", authNone)}); err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	req := fixedRequest()
	req.Model = modelID
	req.Capabilities = map[core.Capability]struct{}{"llm.tools": {}}
	if _, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: "native", AccountID: "account-a"}, services); ge == nil || ge.Code != "unsupported_semantics" || calls.Load() != 0 {
		t.Fatalf("required capability bypassed preflight: error=%v dispatches=%d", ge, calls.Load())
	}
	req.Capabilities = nil
	for _, body := range []string{`{"tools":`, `{"unknown":"bad"} trailing`, `[]`} {
		req.Payload.Body = []byte(body)
		_, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: "native", AccountID: "account-a"}, services)
		if ge == nil || ge.Code != "invalid_request" || strings.Contains(ge.Message, body) || calls.Load() != 0 {
			t.Fatalf("invalid request error=%v dispatches=%d", ge, calls.Load())
		}
	}
}

func TestLocalConnectorPreflightPreservesUnknownNativeFields(t *testing.T) {
	body := []byte(" {\"unknown_extension\": {\"future\": true}} ")
	up := fakeupstream.New(fakeupstream.Response{Body: []byte(`{"id":"resp","status":"completed"}`)})
	defer up.Close()
	c := NewLocalConnector(authNone)
	if err := c.Init(context.Background(), core.ComponentConfig{Data: localComponentConfigJSON(up.URL+"/v1/responses", authNone)}); err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	req := fixedRequest()
	req.Model, req.Payload.Body = modelID, body
	resp, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: "native", AccountID: "account-a"}, core.InvocationServices{})
	if ge != nil {
		t.Fatal(ge)
	}
	resp.Stream.Close()
	if got := <-up.Requests; !bytes.Equal(got.Body, body) {
		t.Fatalf("native request bytes changed: %q", got.Body)
	}
}

func TestLocalConnectorPreservesSupportedToolChoiceBytes(t *testing.T) {
	body := []byte(` { "model" : "model-a", "tools" : [{"type":"function","name":"lookup","parameters":{}}], "tool_choice" : "auto", "input" : "find alpha" } `)
	up := fakeupstream.New(fakeupstream.Response{Body: []byte(`{"id":"resp","status":"completed"}`)})
	defer up.Close()
	c := NewLocalConnector(authNone)
	data, _ := json.Marshal(componentConfig{Transport: Config{Endpoint: up.URL + "/v1/responses", ConnectTimeout: time.Second}, Model: modelID, AccountID: "account-a", AuthMode: authNone, RuntimeProfile: profileVLLM})
	if err := c.Init(context.Background(), core.ComponentConfig{Data: data}); err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())

	// Test-only evidence injection exercises the supported capability path without
	// changing production Init/YAML policy or claiming model support.
	c.mu.Lock()
	c.capabilities = map[core.Capability]core.CapabilityState{"llm.tools": core.Supported}
	c.mu.Unlock()
	req := fixedRequest()
	req.Model, req.Payload.Body = modelID, body
	resp, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: "native", AccountID: "account-a"}, core.InvocationServices{})
	if ge != nil {
		t.Fatalf("supported tool request rejected: %v", ge)
	}
	defer resp.Stream.Close()
	for {
		if _, err := resp.Stream.Next(context.Background()); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if got := <-up.Requests; !bytes.Equal(got.Body, body) {
		t.Fatalf("supported tool request bytes changed: %q", got.Body)
	}
}

func TestLocalConnectorPreflightAdmitsProfileUnknownFieldsAndPlainTextFormat(t *testing.T) {
	cases := []struct {
		profile string
		body    string
	}{
		{profileOllama, `{"input":"hi","text":{"format":{"type":"text"},"verbosity":"high"},"store":true}`},
		{profileOllama, `{"input":[{"type":"input_file","file_id":"file_1"}]}`},
		{profileOllama, `{"input":"hi","tools":[{"type":"web_search"}]}`},
		{profileVLLM, `{"input":"hi","text":{"verbosity":"high"},"conversation":"thread_1","include":["future.extension"]}`},
		{profileVLLM, `{"input":[{"type":"input_file","file_id":"file_1"}]}`},
		{profileVLLM, `{"input":"hi","tools":[{"type":"custom"}]}`},
		{profileVLLM, `{"input":[{"type":"compaction"}]}`},
		{profileVLLM, `{"input":[{"type":"web_search_call"}]}`},
		{profileLlama, `{"input":"hi","text":{"format":{"type":"text"},"verbosity":"high"},"background":true,"include":["future.extension"],"store":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.profile, func(t *testing.T) {
			up := fakeupstream.New(fakeupstream.Response{Body: []byte(`{"id":"resp","status":"completed"}`)})
			defer up.Close()
			c := NewLocalConnector(authNone)
			data, _ := json.Marshal(componentConfig{Transport: Config{Endpoint: up.URL + "/v1/responses"}, Model: modelID, AccountID: "account-a", AuthMode: authNone, RuntimeProfile: tc.profile})
			if err := c.Init(context.Background(), core.ComponentConfig{Data: data}); err != nil {
				t.Fatal(err)
			}
			defer c.Close(context.Background())
			req := fixedRequest()
			req.Model, req.Payload.Body = modelID, []byte(tc.body)
			resp, ge := c.Execute(context.Background(), req, core.AttemptScope{Mode: "native", AccountID: "account-a"}, core.InvocationServices{})
			if ge != nil {
				t.Fatalf("admitted profile request rejected: %v", ge)
			}
			resp.Stream.Close()
			if got := <-up.Requests; !bytes.Equal(got.Body, req.Payload.Body) {
				t.Fatalf("request bytes changed: got %q want %q", got.Body, req.Payload.Body)
			}
		})
	}
}

func TestLocalConnectorInitRejectsMissingScopeAndInvalidDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  componentConfig
	}{
		{"empty model", componentConfig{Transport: Config{Endpoint: "http://127.0.0.1:1/v1/responses"}, AccountID: "a", AuthMode: authNone}},
		{"empty account", componentConfig{Transport: Config{Endpoint: "http://127.0.0.1:1/v1/responses"}, Model: "m", AuthMode: authNone}},
		{"empty capability", componentConfig{Transport: Config{Endpoint: "http://127.0.0.1:1/v1/responses"}, Model: "m", AccountID: "a", AuthMode: authNone, Capabilities: map[core.Capability]core.CapabilityState{"": core.Supported}}},
		{"invalid capability state", componentConfig{Transport: Config{Endpoint: "http://127.0.0.1:1/v1/responses"}, Model: "m", AccountID: "a", AuthMode: authNone, Capabilities: map[core.Capability]core.CapabilityState{"llm.tools": "enabled"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := NewLocalConnector(authNone).Init(context.Background(), core.ComponentConfig{Data: data}); err == nil {
				t.Fatal("invalid local declaration accepted")
			}
		})
	}
}

func TestConnectorInitRejectsDuplicateJSONKeysIncludingEscapedNames(t *testing.T) {
	for _, input := range []string{
		`{"transport":{"endpoint":"http://127.0.0.1:1/v1/responses"},"model":"first","model":"second","account_id":"a","auth_mode":"none"}`,
		`{"transport":{"endpoint":"http://127.0.0.1:1/v1/responses","endpoint":"http://127.0.0.1:2/v1/responses"},"model":"m","account_id":"a","auth_mode":"none"}`,
		`{"transport":{"endpoint":"http://127.0.0.1:1/v1/responses"},"model":"first","\u006dodel":"second","account_id":"a","auth_mode":"none"}`,
		`{"transport":{"endpoint":"http://127.0.0.1:1/v1/responses"},"model":"m","account_id":"a","auth_mode":"none","capabilities":{"llm.tools":"unknown","llm.\u0074ools":"unsupported"}}`,
	} {
		if err := NewLocalConnector(authNone).Init(context.Background(), core.ComponentConfig{Data: []byte(input)}); err == nil {
			t.Errorf("duplicate JSON key accepted: %s", input)
		}
	}
}

func TestLocalConnectorInitRejectsUnevidencedModelCapabilities(t *testing.T) {
	for _, capability := range []core.Capability{"llm.tools", "llm.reasoning", "llm.tools.parallel", "llm.structured_output", "llm.vision", "llm.audio"} {
		data, err := json.Marshal(componentConfig{
			Transport: Config{Endpoint: "http://127.0.0.1:11434/v1/responses"},
			Model:     "arbitrary-model", AccountID: "account-a", AuthMode: authNone,
			Capabilities: map[core.Capability]core.CapabilityState{capability: core.Supported},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := NewLocalConnector(authNone).Init(context.Background(), core.ComponentConfig{Data: data}); err == nil {
			t.Errorf("unevidenced %s support accepted", capability)
		}
	}
	data, err := json.Marshal(componentConfig{
		Transport: Config{Endpoint: "http://127.0.0.1:11434/v1/responses"},
		Model:     "arbitrary-model", AccountID: "account-a", AuthMode: authNone, RuntimeProfile: profileOllama,
		Capabilities: map[core.Capability]core.CapabilityState{"llm.streaming": core.Supported},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := NewLocalConnector(authNone).Init(context.Background(), core.ComponentConfig{Data: data}); err != nil {
		t.Fatalf("transport-level streaming declaration rejected: %v", err)
	}
}

func TestLocalConnectorAuthPolicyAndClientHeaderIsolation(t *testing.T) {
	for _, mode := range []string{authNone, authBearer} {
		t.Run(mode, func(t *testing.T) {
			up := fakeupstream.New(fakeupstream.Response{Body: []byte(`{"id":"resp","status":"completed"}`)})
			defer up.Close()
			c := NewLocalConnector(mode)
			if err := c.Init(context.Background(), core.ComponentConfig{Data: localComponentConfigJSON(up.URL+"/v1/responses", mode)}); err != nil {
				t.Fatal(err)
			}
			defer c.Close(context.Background())
			req := fixedRequest()
			req.Model = modelID
			req.Metadata.Headers = map[string][]string{"Authorization": {"Bearer client-fake"}, "Proxy-Authorization": {"Basic client-fake"}}
			var credentialCalls atomic.Int32
			services := core.InvocationServices{Credentials: credentialFunc(func(_ context.Context, name string) ([]byte, error) {
				credentialCalls.Add(1)
				if name != "bearer" {
					t.Fatalf("credential name = %q", name)
				}
				return []byte("selected-account-secret"), nil
			})}
			_, ge := c.Execute(context.Background(), req, core.AttemptScope{AccountID: "account-a", Mode: "native"}, services)
			if ge != nil {
				t.Fatal(ge)
			}
			capture := <-up.Requests
			wantAuth, wantCalls := "", int32(0)
			if mode == authBearer {
				wantAuth, wantCalls = "Bearer selected-account-secret", 1
			}
			if capture.Header.Get("Authorization") != wantAuth || capture.Header.Get("Proxy-Authorization") != "" || credentialCalls.Load() != wantCalls {
				t.Fatalf("auth=%q proxy-auth=%q credential calls=%d", capture.Header.Get("Authorization"), capture.Header.Get("Proxy-Authorization"), credentialCalls.Load())
			}
		})
	}
}

func TestLocalBearerMissingCredentialsRejectBeforeSend(t *testing.T) {
	up := fakeupstream.New(fakeupstream.Response{Body: []byte(`{}`)})
	defer up.Close()
	c := NewLocalConnector(authBearer)
	if err := c.Init(context.Background(), core.ComponentConfig{Data: localComponentConfigJSON(up.URL+"/v1/responses", authBearer)}); err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	req := fixedRequest()
	req.Model = modelID
	scope := core.AttemptScope{AccountID: "account-a", Mode: "native"}
	for name, credentials := range map[string]core.CredentialAccess{
		"nil":   nil,
		"empty": credentialFunc(func(context.Context, string) ([]byte, error) { return nil, nil }),
		"error": credentialFunc(func(context.Context, string) ([]byte, error) { return nil, errors.New("private") }),
	} {
		_, ge := c.Execute(context.Background(), req, scope, core.InvocationServices{Credentials: credentials})
		if ge == nil || ge.Category != core.CategoryUnauthenticated {
			t.Fatalf("%s credential result = %+v", name, ge)
		}
	}
	if len(up.Requests) != 0 {
		t.Fatal("missing bearer credential dispatched upstream")
	}
}

func TestConnectorConfiguredCapabilitiesPreserveLiveUnknowns(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, model                  string
		wantStreaming, wantTools, wantParallel core.CapabilityState
	}{
		{"live baseline", "https://api.openai.com/v1/responses", modelID, core.Supported, core.Unsupported, core.Unknown},
		{"unverified remote", "https://example.test/v1/responses", "custom-model", core.Unknown, core.Unsupported, core.Unknown},
		{"local fixture", "http://127.0.0.1:1/v1/responses", "custom-model", core.Supported, core.Unsupported, core.Supported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewConnector()
			data, err := json.Marshal(componentConfig{Transport: Config{Endpoint: tc.endpoint}, Model: tc.model, AccountID: "selected", Capabilities: map[core.Capability]core.CapabilityState{
				"llm.streaming": core.Supported, "llm.tools": core.Unsupported, "llm.tools.parallel": core.Supported,
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Init(context.Background(), core.ComponentConfig{Data: data}); err != nil {
				t.Fatal(err)
			}
			defer c.Close(context.Background())
			scope := core.CapabilityScope{Protocol: protocol, Mode: core.ModeNative, Model: tc.model, AccountID: "selected"}
			result := c.Capabilities(context.Background(), scope)
			if result.State("llm.streaming") != tc.wantStreaming || result.State("llm.tools") != tc.wantTools || result.State("llm.tools.parallel") != tc.wantParallel {
				t.Fatalf("configured capability scope: %+v", result.Values)
			}
			result.Values["llm.tools"] = core.Supported
			if c.Capabilities(context.Background(), scope).State("llm.tools") != core.Unsupported {
				t.Fatal("capability result exposed component state")
			}
			scope.AccountID = "other"
			if len(c.Capabilities(context.Background(), scope).Values) != 0 {
				t.Fatal("declaration leaked to another account")
			}
		})
	}
	for _, declaration := range []map[core.Capability]core.CapabilityState{{"": core.Supported}, {"llm.tools": "invalid"}} {
		data, _ := json.Marshal(componentConfig{Transport: Config{Endpoint: "http://127.0.0.1:1/v1/responses"}, Model: modelID, AccountID: "selected", Capabilities: declaration})
		if err := NewConnector().Init(context.Background(), core.ComponentConfig{Data: data}); err == nil {
			t.Fatal("invalid capability declaration accepted")
		}
	}
}

type credentialFunc func(context.Context, string) ([]byte, error)

func (f credentialFunc) Get(ctx context.Context, name string) ([]byte, error) { return f(ctx, name) }

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestConnectorExecuteUsesAttemptServicesAndChecksScopeBeforeNetwork(t *testing.T) {
	body := []byte(`{"id":"resp","status":"completed"}`)
	up := fakeupstream.New(fakeupstream.Response{Body: body})
	defer up.Close()
	c := NewConnector()
	if err := c.Init(context.Background(), core.ComponentConfig{Data: componentConfigJSON(up.URL + "/v1/responses")}); err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	var calls atomic.Int32
	var captured *http.Request
	services := core.InvocationServices{
		Credentials: credentialFunc(func(_ context.Context, name string) ([]byte, error) {
			if name != "bearer" {
				t.Fatalf("credential name = %q", name)
			}
			return []byte("attempt-secret"), nil
		}),
		Transport: doerFunc(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			captured = req
			return http.DefaultClient.Do(req)
		}),
	}
	req := fixedRequest()
	req.Model = modelID
	resp, ge := c.Execute(context.Background(), req, core.AttemptScope{AccountID: "account-a", Mode: "native"}, services)
	if ge != nil {
		t.Fatal(ge)
	}
	var got []byte
	for {
		frame, err := resp.Stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameBody {
			got = append(got, frame.Body.Data...)
		}
	}
	resp.Stream.Close()
	if calls.Load() != 1 || captured == nil || captured.Header.Get("Authorization") != "Bearer attempt-secret" || !bytes.Equal(got, body) {
		t.Fatalf("custom transport calls=%d request=%v body=%q", calls.Load(), captured, got)
	}
	for _, bad := range []struct {
		req   core.ExecutionRequest
		scope core.AttemptScope
	}{
		{req, core.AttemptScope{AccountID: "other", Mode: "native"}},
		{req, core.AttemptScope{AccountID: "account-a", Mode: "translation"}},
		{func() core.ExecutionRequest { r := req; r.Model = "other"; return r }(), core.AttemptScope{AccountID: "account-a", Mode: "native"}},
		{func() core.ExecutionRequest { r := req; r.Payload.Protocol = "other"; return r }(), core.AttemptScope{AccountID: "account-a", Mode: "native"}},
	} {
		_, err := c.Execute(context.Background(), bad.req, bad.scope, services)
		if err == nil {
			t.Fatal("mismatched protocol or scope accepted")
		}
	}
	_, err := c.Execute(context.Background(), req, core.AttemptScope{AccountID: "account-a", Mode: "native"}, core.InvocationServices{
		Credentials: credentialFunc(func(context.Context, string) ([]byte, error) { return nil, errors.New("private failure") }),
	})
	if err == nil || err.Code != "credential_unavailable" || strings.Contains(err.Code+err.Message+err.Provider+err.OriginalError, "private failure") || calls.Load() != 1 {
		t.Fatalf("credential failure leaked or attempted request: %v, calls=%d", err, calls.Load())
	}
}

func TestConnectorExecuteRejectsUnavailableAndMissingCredentialsBeforeHTTP(t *testing.T) {
	var calls atomic.Int32
	services := core.InvocationServices{
		Transport: doerFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, errors.New("unexpected HTTP request")
		}),
	}
	req := fixedRequest()
	req.Model = modelID
	scope := core.AttemptScope{AccountID: "account-a", Mode: "native"}

	checkRejected := func(name string, c *Connector, services core.InvocationServices) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			_, ge := c.Execute(context.Background(), req, scope, services)
			if ge == nil || calls.Load() != 0 {
				t.Fatalf("Execute error = %v, HTTP calls = %d; want rejection before HTTP", ge, calls.Load())
			}
		})
	}

	uninitialized := NewConnector()
	checkRejected("uninitialized", uninitialized, services)

	up := fakeupstream.New(fakeupstream.Response{Body: []byte(`{}`)})
	defer up.Close()
	closed := NewConnector()
	if err := closed.Init(context.Background(), core.ComponentConfig{Data: componentConfigJSON(up.URL + "/v1/responses")}); err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkRejected("closed", closed, services)

	ready := NewConnector()
	if err := ready.Init(context.Background(), core.ComponentConfig{Data: componentConfigJSON(up.URL + "/v1/responses")}); err != nil {
		t.Fatal(err)
	}
	defer ready.Close(context.Background())
	checkRejected("nil_credentials", ready, services)
	checkRejected("empty_credential_value", ready, core.InvocationServices{
		Transport: services.Transport,
		Credentials: credentialFunc(func(context.Context, string) ([]byte, error) {
			return []byte{}, nil
		}),
	})
}

func TestConnectorExecuteSelectsIncrementalSSEFromRequestMetadata(t *testing.T) {
	body := []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	up := fakeupstream.New(fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body})
	defer up.Close()
	c := NewConnector()
	if err := c.Init(context.Background(), core.ComponentConfig{Data: componentConfigJSON(up.URL + "/v1/responses")}); err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	streaming := true
	req := fixedRequest()
	req.Model, req.Metadata.Streaming = modelID, &streaming
	resp, ge := c.Execute(context.Background(), req, core.AttemptScope{AccountID: "account-a", Mode: "native"}, core.InvocationServices{
		Credentials: credentialFunc(func(context.Context, string) ([]byte, error) { return []byte("selected"), nil }),
	})
	if ge != nil {
		t.Fatal(ge)
	}
	defer resp.Stream.Close()
	var got []byte
	for {
		frame, err := resp.Stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameBody {
			got = append(got, frame.Body.Data...)
		}
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("stream body changed: %q", got)
	}
}
