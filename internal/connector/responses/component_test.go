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
