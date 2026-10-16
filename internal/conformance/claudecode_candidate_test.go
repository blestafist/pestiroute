package conformance

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/connector/claudecode"
	"github.com/blestafist/pestiroute/internal/core"
)

type claudeAuthDoer func(*http.Request) (*http.Response, error)

func (f claudeAuthDoer) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestClaudeCodeConnectorLifecycleAndFailClosedExecution(t *testing.T) {
	c := claudecode.NewConnector()
	ctx := context.Background()
	gotDescriptor := c.Descriptor()
	wantDescriptor := core.Descriptor{
		ID: "pestiroute.claudecode.messages", Kind: core.ComponentConnector, ImplementationVersion: "1.0.0",
		APIVersions: []core.APIVersion{{Major: 1, Minor: 0}}, Protocols: []string{"openai.responses.v1"},
		Operations: []string{"authenticate"}, ConnectorType: "api", AuthMethods: []string{"oauth"},
	}
	if !reflect.DeepEqual(gotDescriptor, wantDescriptor) {
		t.Fatalf("descriptor = %+v, want exact declaration %+v", gotDescriptor, wantDescriptor)
	}
	if err := gotDescriptor.Validate(core.ComponentConnector); err != nil {
		t.Fatalf("descriptor contract validation: %v", err)
	}
	if err := c.Init(ctx, core.ComponentConfig{Data: []byte(`{"profile":"claude-code-messages-v1"}`)}); err != nil {
		t.Fatal(err)
	}
	if got := c.Health(ctx).State; got != core.HealthReady {
		t.Fatalf("health after initialization = %v", got)
	}
	scope := core.CapabilityScope{Protocol: "openai.responses.v1", Mode: core.ModeTranslation}
	capabilities := c.Capabilities(ctx, scope).Values
	for _, name := range []core.Capability{"llm.streaming", "llm.tools", "llm.reasoning", "auth.oauth", "usage.exact"} {
		if got := capabilities[name]; got != core.Unknown {
			t.Errorf("capability %q = %q, want unknown", name, got)
		}
	}
	response, ge := c.Execute(ctx, core.ExecutionRequest{Payload: core.RawPayload{Protocol: "openai.responses.v1"}}, core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{})
	if response.Stream != nil || ge == nil || ge.Code != "connector_unavailable" || ge.RetryDisposition != core.RetryUnknown {
		t.Fatalf("uncomposed execution response = (%+v, %v), want fail-closed unavailable", response, ge)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := c.Health(ctx).State; got == core.HealthReady {
		t.Fatal("connector remained ready after close")
	}
}

func TestClaudeCodePublicAuthenticationLifecycleOffline(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	entropy := append(bytes.Repeat([]byte{0x13}, 32), bytes.Repeat([]byte{0x42}, 32)...)
	c := claudecode.NewConnectorWithOptions(claudecode.Options{Clock: func() time.Time { return now }, Entropy: bytes.NewReader(entropy)})
	config := `{"profile":"claude-code-messages-v1","oauth":{"authorization_endpoint":"https://auth.synthetic.invalid/oauth/authorize","token_endpoint":"https://token.synthetic.invalid/oauth/token","redirect_uri":"http://127.0.0.1:9999/callback","client_id":"synthetic-client","scope":"synthetic:offline"}}`
	if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(config)}); err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	start, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "start"}, core.InvocationServices{})
	if ge != nil || !start.Supported || start.NextAction != "continue" || start.UserAction == nil || len(start.Credentials) != 0 {
		t.Fatalf("start = (%+v, %v)", start, ge)
	}
	authURL, err := url.Parse(start.UserAction.VerificationURI)
	if err != nil || authURL.Scheme != "https" || authURL.Query().Get("code_challenge") == "" || authURL.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("unsafe/invalid authorization action: %v (%v)", start.UserAction, err)
	}
	state := authURL.Query().Get("state")
	verifier := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x13}, 32))
	if state == "" || strings.Contains(start.UserAction.VerificationURI, verifier) || len(start.Credentials) != 0 {
		t.Fatalf("authorization action missing state or disclosed verifier: %+v", start.UserAction)
	}
	callback := "http://127.0.0.1:9999/callback?code=synthetic-code&state=" + url.QueryEscape(state)
	continueCalls := 0
	doer := claudeAuthDoer(func(req *http.Request) (*http.Response, error) {
		continueCalls++
		if req.URL.Host != "token.synthetic.invalid" {
			t.Fatalf("unexpected exchange host: %s", req.URL.Host)
		}
		data, err := os.ReadFile("../connector/claudecode/testdata/oauth-exchange-response.json")
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, nil
	})
	continued, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: []byte(start.State), CallbackInput: callback}, core.InvocationServices{Transport: doer})
	if ge != nil || !continued.Supported || continued.NextAction != "" || continued.CredentialExpiresAt == nil || !continued.CredentialExpiresAt.Equal(now.Add(time.Hour)) || len(continued.Credentials["oauth"]) == 0 || continueCalls != 1 {
		t.Fatalf("continue = (%+v, %v), calls=%d", continued, ge, continueCalls)
	}
	if len(continued.State) != 0 || bytes.Contains(continued.Credentials["oauth"], []byte("verifier")) {
		t.Fatal("completed authentication retained continuation state or leaked verifier")
	}
	if !bytes.Contains(continued.Credentials["oauth"], []byte("SYNTHETIC_ACCESS")) {
		t.Fatal("completed authentication did not return the synthetic credential bundle")
	}
	prior := []byte(`{"access_token":"old","refresh_token":"old-refresh","expires_at":"2026-10-16T10:00:00Z"}`)
	refreshCalls := 0
	refreshDoer := claudeAuthDoer(func(req *http.Request) (*http.Response, error) {
		refreshCalls++
		form, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		values, err := url.ParseQuery(string(form))
		if err != nil || values.Get("grant_type") != "refresh_token" || values.Get("refresh_token") != "old-refresh" {
			t.Fatalf("refresh request did not use scoped token: %s (%v)", form, err)
		}
		data, err := os.ReadFile("../connector/claudecode/testdata/oauth-refresh-response.json")
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, nil
	})
	refreshed, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "refresh"}, core.InvocationServices{Transport: refreshDoer, Credentials: claudeCredentialAccess(prior)})
	if ge != nil || !refreshed.Supported || refreshed.NextAction != "" || refreshed.CredentialExpiresAt == nil || !refreshed.CredentialExpiresAt.Equal(now.Add(time.Hour)) || len(refreshed.Credentials["oauth"]) == 0 || refreshCalls != 1 {
		t.Fatalf("refresh = (%+v, %v), calls=%d", refreshed, ge, refreshCalls)
	}
}

type claudeCredentialAccess []byte

func (c claudeCredentialAccess) Get(context.Context, string) ([]byte, error) {
	return append([]byte(nil), c...), nil
}
