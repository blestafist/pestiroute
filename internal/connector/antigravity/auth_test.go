package antigravity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestAuthenticateRefreshRotatesAndExecuteUsesOnlyAccessToken(t *testing.T) {
	fixture, err := os.ReadFile("testdata/oauth-refresh-rotated.json")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDoer{response: &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(fixture)))}}
	c := ready(t)
	c.cfg.OAuth.ClientSecret = "SYNTHETIC_CLIENT_SECRET"
	prior := []byte(`{"access_token":"SYNTHETIC_OLD_ACCESS","refresh_token":"SYNTHETIC_OLD_REFRESH","project_id":"synthetic-project"}`)
	result, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "refresh"}, core.InvocationServices{Transport: packageTestDoer(f), Credentials: credentials{data: prior}})
	if ge != nil || !result.Supported || f.calls != 1 || result.CredentialExpiresAt == nil || !result.CredentialExpiresAt.After(time.Now()) {
		t.Fatalf("refresh: result=%+v error=%v calls=%d", result, ge, f.calls)
	}
	if f.request.Method != http.MethodPost || f.request.URL.String() != defaultTokenEndpoint || f.request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Fatalf("bad refresh request: %+v", f.request)
	}
	body, _ := io.ReadAll(f.request.Body)
	form, err := url.ParseQuery(string(body))
	if err != nil || form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "SYNTHETIC_OLD_REFRESH" || form.Get("client_id") != defaultClientID || form.Get("client_secret") == "" || len(form) != 4 {
		t.Fatalf("bad refresh form shape (parse error: %v)", err)
	}
	var bundle oauthCredentials
	if err := json.Unmarshal(result.Credentials["oauth"], &bundle); err != nil || bundle.AccessToken != "SYNTHETIC_ROTATED_ACCESS_TOKEN" || bundle.RefreshToken != "SYNTHETIC_ROTATED_REFRESH_TOKEN" || bundle.ProjectID != "synthetic-project" {
		t.Fatalf("bad replacement bundle %s (%v)", result.Credentials["oauth"], err)
	}
	// Execute uses a distinct fake transport and receives the exact replacement generation.
	exec := &fakeDoer{response: response(200, "data: {\"response\":{\"done\":true}}\n\n")}
	services := core.InvocationServices{Transport: packageTestDoer(exec), Credentials: credentials{data: result.Credentials["oauth"]}}
	out, ge := c.Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"Synthetic","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, services)
	if ge != nil || exec.calls != 1 || exec.request.Header.Get("Authorization") != "Bearer SYNTHETIC_ROTATED_ACCESS_TOKEN" {
		t.Fatalf("execute: error=%v calls=%d", ge, exec.calls)
	}
	for name, values := range exec.request.Header {
		if strings.Contains(strings.ToLower(name), "refresh") || strings.Contains(strings.Join(values, " "), "SYNTHETIC_ROTATED_REFRESH_TOKEN") {
			t.Fatalf("refresh credential leaked in header %q", name)
		}
	}
	requestBody, err := io.ReadAll(exec.request.Body)
	if err != nil || strings.Contains(string(requestBody), "SYNTHETIC_ROTATED_REFRESH_TOKEN") {
		t.Fatalf("refresh credential leaked in request body: %q (%v)", requestBody, err)
	}
	_ = out.Stream.Close()
}

func TestAuthenticateBrowserPKCEStartContinue(t *testing.T) {
	c := NewConnector()
	config := []byte(`{"profile":"antigravity-v1internal","project":"synthetic-project","oauth":{"client_id":"synthetic-client","auth_endpoint":"https://auth.synthetic.invalid/authorize","token_endpoint":"https://oauth.synthetic.invalid/token","redirect_uri":"http://localhost:8080/callback","scopes":["scope.one","scope.two"]}}`)
	if err := c.Init(context.Background(), core.ComponentConfig{Data: config}); err != nil {
		t.Fatal(err)
	}
	result, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "start"}, core.InvocationServices{})
	if ge != nil || !result.Supported || result.NextAction != "continue" || result.UserAction == nil {
		t.Fatalf("start: %+v %v", result, ge)
	}
	authorize, err := url.Parse(result.UserAction.VerificationURI)
	if err != nil {
		t.Fatal(err)
	}
	query := authorize.Query()
	if authorize.Host != "auth.synthetic.invalid" || query.Get("client_id") != "synthetic-client" || query.Get("code_challenge_method") != "S256" || query.Get("scope") != "scope.one scope.two" || query.Get("access_type") != "offline" || query.Get("code_verifier") != "" {
		t.Fatalf("invalid authorization URL: %s", authorize)
	}
	var continuation authContinuation
	if err := json.Unmarshal([]byte(result.State), &continuation); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(continuation.Verifier))
	if query.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(want[:]) || len(continuation.State) < 40 {
		t.Fatal("PKCE challenge or state is invalid")
	}
	var tokenRequestBody string
	fake := &fakeDoer{do: func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "oauth.synthetic.invalid" {
			body, _ := io.ReadAll(req.Body)
			tokenRequestBody = string(body)
			return response(200, `{"access_token":"SYNTHETIC_ACCESS","refresh_token":"SYNTHETIC_REFRESH","expires_in":3600,"token_type":"Bearer","scope":"synthetic scope","id_token":"SYNTHETIC_UNUSED_ID_TOKEN"}`), nil
		}
		if req.URL.String() != defaultInferenceEndpoint+"/v1internal:loadCodeAssist" || req.Header.Get("Authorization") != "Bearer SYNTHETIC_ACCESS" {
			return nil, errors.New("unexpected synthetic discovery request")
		}
		return response(200, `{"cloudaicompanionProject":{"id":"synthetic-project"}}`), nil
	}}
	stateQuery := url.QueryEscape(continuation.State)
	for _, callback := range []string{
		"http://localhost:8080/callback?code=x&state=" + stateQuery + "&extra=y",
		"http://localhost:8080/callback?code=x&code=y&state=" + stateQuery,
		"http://LOCALHOST:8080/callback?code=x&state=" + stateQuery,
		"http://localhost:80/callback?code=x&state=" + stateQuery,
		"http://localhost:8080/%63allback?code=x&state=" + stateQuery,
		" http://localhost:8080/callback?code=x&state=" + stateQuery,
		"https:/localhost/callback?code=x&state=" + stateQuery,
		"http://localhost:8080/callback?code=x&state=" + stateQuery + "#fragment",
		"http://user@localhost:8080/callback?code=x&state=" + stateQuery,
		"synthetic\ncode", "synthetic\tcode", "synthetic code", "synthetic:code",
		"synthetic?code", "synthetic#code", "synthetic&code", strings.Repeat("x", 513),
	} {
		bad, err := c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: []byte(result.State), CallbackInput: callback}, core.InvocationServices{Transport: packageTestDoer(fake)})
		if err == nil || bad.Supported || fake.calls != 0 {
			t.Fatalf("malformed callback dispatched: %q result=%+v error=%v calls=%d", callback, bad, err, fake.calls)
		}
	}
	bad, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: []byte(result.State), CallbackInput: "http://localhost:8080/callback?code=local&state=wrong"}, core.InvocationServices{Transport: packageTestDoer(fake)})
	if ge == nil || bad.Supported || fake.calls != 0 {
		t.Fatalf("state mismatch dispatched: %+v %v calls=%d", bad, ge, fake.calls)
	}
	completed, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: []byte(result.State), CallbackInput: "http://localhost:8080/callback?code=local-code&state=" + url.QueryEscape(continuation.State)}, core.InvocationServices{Transport: packageTestDoer(fake)})
	if ge != nil || !completed.Supported || completed.CredentialExpiresAt == nil || !completed.CredentialExpiresAt.After(time.Now()) || fake.calls != 2 {
		t.Fatalf("continue: %+v %v calls=%d", completed, ge, fake.calls)
	}
	var creds oauthCredentials
	if json.Unmarshal(completed.Credentials["oauth"], &creds) != nil || creds.ProjectID != "synthetic-project" {
		t.Fatalf("project discovery was not persisted in credential metadata: %+v", creds)
	}
	if strings.Contains(string(completed.Credentials["oauth"]), "SYNTHETIC_UNUSED_ID_TOKEN") || strings.Contains(string(completed.Credentials["oauth"]), "synthetic scope") {
		t.Fatalf("unused OAuth response fields persisted: %s", completed.Credentials["oauth"])
	}
	form, err := url.ParseQuery(tokenRequestBody)
	if err != nil || form.Get("grant_type") != "authorization_code" || form.Get("client_id") != "synthetic-client" || form.Get("client_secret") != "" || form.Get("code_verifier") != continuation.Verifier || form.Get("code") != "local-code" || form.Get("redirect_uri") != "http://localhost:8080/callback" {
		t.Fatalf("invalid exchange shape (parse error: %v)", err)
	}
	_, ge = c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: []byte(result.State), CallbackInput: " 4/0Aa+_-%=x "}, core.InvocationServices{Transport: packageTestDoer(fake)})
	if ge != nil || fake.calls != 4 {
		t.Fatalf("pasted code continuation: error=%v calls=%d", ge, fake.calls)
	}
	form, err = url.ParseQuery(tokenRequestBody)
	if err != nil || form.Get("code") != "4/0Aa+_-%=x" || form.Get("redirect_uri") != "http://localhost:8080/callback" {
		t.Fatalf("pasted code exchange shape mismatch (parse error: %v)", err)
	}
	for _, tc := range []struct {
		status   int
		category core.ErrorCategory
		code     string
	}{{http.StatusUnauthorized, core.CategoryUnauthenticated, "auth_rejected"}, {http.StatusForbidden, core.CategoryUnauthenticated, "auth_rejected"}, {http.StatusTooManyRequests, core.CategoryRateLimited, "auth_rate_limited"}, {http.StatusBadGateway, core.CategoryUnavailable, "auth_request_failed"}} {
		badTransport := &fakeDoer{response: response(tc.status, `{"error":"SYNTHETIC_PRIVATE_PROVIDER_BODY"}`)}
		_, got := c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: []byte(result.State), CallbackInput: "http://localhost:8080/callback?code=local&state=" + url.QueryEscape(continuation.State)}, core.InvocationServices{Transport: packageTestDoer(badTransport)})
		wantStatus := fmt.Sprintf("HTTP status %03d", tc.status)
		if got == nil || got.Code != tc.code || got.Category != tc.category || got.OriginalError != wantStatus || badTransport.calls != 1 {
			t.Errorf("status %d: error=%+v calls=%d", tc.status, got, badTransport.calls)
		} else if strings.Contains(got.Code+got.Message+got.OriginalError, "SYNTHETIC_PRIVATE_PROVIDER_BODY") {
			t.Errorf("status %d leaked provider body: %+v", tc.status, got)
		}
	}
}

func TestDiscoverProjectFailureDiagnosticsAreSanitized(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response *http.Response
		wantCode string
		wantOrig string
	}{
		{name: "missing project", response: response(http.StatusOK, `{}`), wantCode: "auth_project_missing"},
		{name: "http error", response: response(http.StatusServiceUnavailable, `SYNTHETIC_PRIVATE_DISCOVERY_BODY`), wantCode: "auth_request_failed", wantOrig: "HTTP status 503"},
		{name: "nil response", wantCode: "auth_transport_unavailable"},
		{name: "nil body", response: &http.Response{StatusCode: http.StatusOK}, wantCode: "auth_transport_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &fakeDoer{response: tc.response}
			_, got := discoverProject(context.Background(), "https://discovery.synthetic.invalid", transport, "SYNTHETIC_ACCESS")
			if got == nil || got.Code != tc.wantCode || got.OriginalError != tc.wantOrig || transport.calls != 1 {
				t.Fatalf("diagnostic=%+v calls=%d", got, transport.calls)
			}
			if strings.Contains(got.Code+got.Message+got.OriginalError, "SYNTHETIC_PRIVATE_DISCOVERY_BODY") {
				t.Fatalf("provider body leaked: %+v", got)
			}
		})
	}
}

func TestAuthenticateSourceDerivedDefaults(t *testing.T) {
	if defaultClientID != "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com" || defaultAuthEndpoint != "https://accounts.google.com/o/oauth2/v2/auth" || defaultTokenEndpoint != "https://oauth2.googleapis.com/token" {
		t.Fatal("OAuth defaults do not match the pinned 9Router observation")
	}
	if defaultScopes != "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile https://www.googleapis.com/auth/cclog https://www.googleapis.com/auth/experimentsandconfigs" {
		t.Fatal("OAuth scopes do not match the pinned 9Router observation")
	}
	if got := withOAuthDefaults(oauthConfig{}); got.ClientID != defaultClientID || got.ClientSecret != "" {
		t.Fatal("OAuth defaults must retain public client identity without injecting a secret")
	}
	c := NewConnector()
	if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"profile":"antigravity-v1internal","project":"synthetic-project"}`)}); err != nil {
		t.Fatal(err)
	}
	result, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "start"}, core.InvocationServices{})
	if ge != nil || !result.Supported || result.UserAction == nil {
		t.Fatalf("start: %+v %v", result, ge)
	}
	var continuation authContinuation
	if err := json.Unmarshal([]byte(result.State), &continuation); err != nil {
		t.Fatal(err)
	}
	redirect, err := url.Parse(continuation.RedirectURI)
	if err != nil || redirect.Scheme != "http" || redirect.Hostname() != "localhost" || redirect.Port() == "" || redirect.Path != "/callback" {
		t.Fatalf("default start did not record an ephemeral loopback redirect: %q (%v)", continuation.RedirectURI, err)
	}
	authorize, err := url.Parse(result.UserAction.VerificationURI)
	if err != nil {
		t.Fatal(err)
	}
	if authorize.Host != "accounts.google.com" || authorize.Path != "/o/oauth2/v2/auth" || authorize.Query().Get("client_id") != defaultClientID || authorize.Query().Get("redirect_uri") != continuation.RedirectURI || authorize.Query().Get("scope") != defaultScopes || authorize.Query().Get("access_type") != "offline" || authorize.Query().Get("prompt") != "consent" || authorize.Query().Get("code_challenge_method") != "S256" || authorize.Query().Get("code_challenge") == "" {
		t.Fatalf("unexpected default OAuth request: %s", authorize)
	}
}

func TestAuthenticateDefaultsShareOAuthEndpointsAndBoundDiscoveryOrigin(t *testing.T) {
	c := NewConnector()
	if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"profile":"antigravity-v1internal","project":"configured-project","endpoint":"https://discovery.synthetic.invalid","oauth":{"client_secret":"SYNTHETIC_CLIENT_SECRET"}}`)}); err != nil {
		t.Fatal(err)
	}
	started, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "start"}, core.InvocationServices{})
	if ge != nil || started.UserAction == nil {
		t.Fatalf("start action: %+v err=%v", started, ge)
	}
	var continuation authContinuation
	if err := json.Unmarshal([]byte(started.State), &continuation); err != nil {
		t.Fatal(err)
	}
	// The exact redirect is session-bound; later component configuration cannot
	// change callback validation or the token-exchange redirect_uri.
	c.mu.Lock()
	c.cfg.OAuth.RedirectURI = "http://localhost:4444/changed"
	c.mu.Unlock()
	var exchangeForm url.Values
	fake := &fakeDoer{do: func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case defaultTokenEndpoint:
			body, _ := io.ReadAll(req.Body)
			exchangeForm, _ = url.ParseQuery(string(body))
			if exchangeForm.Get("grant_type") == "refresh_token" {
				return response(http.StatusOK, `{"access_token":"SYNTHETIC_REFRESHED_ACCESS","refresh_token":"SYNTHETIC_REFRESHED_REFRESH","expires_in":3600,"token_type":"Bearer"}`), nil
			}
			return response(http.StatusOK, `{"access_token":"SYNTHETIC_LOGIN_ACCESS","refresh_token":"SYNTHETIC_LOGIN_REFRESH","expires_in":3600,"token_type":"Bearer"}`), nil
		case "https://discovery.synthetic.invalid/v1internal:loadCodeAssist":
			if req.Method != http.MethodPost || req.Header.Get("Authorization") != "Bearer SYNTHETIC_LOGIN_ACCESS" {
				return nil, errors.New("unexpected discovery request")
			}
			return response(http.StatusOK, `{"cloudaicompanionProject":"discovered-project"}`), nil
		default:
			return nil, errors.New("unexpected synthetic URL: " + req.URL.String())
		}
	}}
	callback := continuation.RedirectURI + "?" + url.Values{"code": {"SYNTHETIC_CODE"}, "state": {continuation.State}}.Encode()
	wrongCallback := "http://localhost:4444/changed?" + url.Values{"code": {"SYNTHETIC_CODE"}, "state": {continuation.State}}.Encode()
	bad, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: []byte(started.State), CallbackInput: wrongCallback}, core.InvocationServices{Transport: packageTestDoer(fake)})
	if ge == nil || bad.Supported || fake.calls != 0 {
		t.Fatalf("callback matching changed config dispatched against wrong session redirect: result=%+v err=%v calls=%d", bad, ge, fake.calls)
	}
	tampered := continuation
	tampered.RedirectURI += "?unexpected=value"
	tamperedState, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	bad, ge = c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: tamperedState, CallbackInput: callback}, core.InvocationServices{Transport: packageTestDoer(fake)})
	if ge == nil || bad.Supported || fake.calls != 0 {
		t.Fatalf("malformed stored redirect dispatched: result=%+v err=%v calls=%d", bad, ge, fake.calls)
	}
	continued, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: []byte(started.State), CallbackInput: callback}, core.InvocationServices{Transport: packageTestDoer(fake)})
	if ge != nil || !continued.Supported || fake.calls != 2 || exchangeForm.Get("redirect_uri") != continuation.RedirectURI || exchangeForm.Get("client_id") != defaultClientID || exchangeForm.Get("client_secret") == "" || exchangeForm.Get("code_verifier") == "" || exchangeForm.Get("code") != "SYNTHETIC_CODE" {
		t.Fatalf("continue defaults: result supported=%t err=%v calls=%d exchange shape invalid", continued.Supported, ge, fake.calls)
	}
	bundle, valid := parseOAuthCredentials(continued.Credentials["oauth"], true)
	if !valid || bundle.ProjectID != "discovered-project" {
		t.Fatalf("discovery metadata: %+v valid=%t", bundle, valid)
	}
	_, ge = c.Authenticate(context.Background(), core.AuthRequest{Action: "refresh"}, core.InvocationServices{Transport: packageTestDoer(fake), Credentials: credentials{data: continued.Credentials["oauth"]}})
	if ge != nil || fake.calls != 3 || fake.request.URL.String() != defaultTokenEndpoint || exchangeForm.Get("grant_type") != "refresh_token" || exchangeForm.Get("client_id") != defaultClientID || exchangeForm.Get("client_secret") == "" {
		t.Fatalf("refresh defaults: error=%v calls=%d request=%v client authentication missing", ge, fake.calls, fake.request.URL)
	}
	boundaryStart, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "start"}, core.InvocationServices{})
	if ge != nil {
		t.Fatal(ge)
	}
	_, ge = c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: []byte(boundaryStart.State), CallbackInput: strings.Repeat("x", 512)}, core.InvocationServices{Transport: packageTestDoer(fake)})
	if ge != nil || fake.calls != 5 || exchangeForm.Get("code") != strings.Repeat("x", 512) {
		t.Fatalf("512-byte code boundary: error=%v calls=%d code length=%d", ge, fake.calls, len(exchangeForm.Get("code")))
	}
}

func TestDefaultClientAuthenticationFailsClosedWithoutConfiguredSecret(t *testing.T) {
	c := NewConnector()
	if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"profile":"antigravity-v1internal","project":"synthetic-project"}`)}); err != nil {
		t.Fatal(err)
	}
	started, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "start"}, core.InvocationServices{})
	if ge != nil || !started.Supported {
		t.Fatalf("start should remain local: result=%+v error=%v", started, ge)
	}
	fake := &fakeDoer{response: response(http.StatusOK, `{}`)}
	_, ge = c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: []byte(started.State), CallbackInput: "SYNTHETIC_CODE"}, core.InvocationServices{Transport: packageTestDoer(fake)})
	if ge == nil || ge.Category != core.CategoryUnauthenticated || fake.calls != 0 {
		t.Fatalf("exchange without configured client secret was not rejected before transport: error=%v calls=%d", ge, fake.calls)
	}
	_, ge = c.Authenticate(context.Background(), core.AuthRequest{Action: "refresh"}, core.InvocationServices{Transport: packageTestDoer(fake), Credentials: credentials{data: []byte(`{"access_token":"SYNTHETIC_ACCESS","refresh_token":"SYNTHETIC_REFRESH","project_id":"synthetic-project"}`)}})
	if ge == nil || ge.Category != core.CategoryUnauthenticated || fake.calls != 0 {
		t.Fatalf("refresh without configured client secret was not rejected before transport: error=%v calls=%d", ge, fake.calls)
	}
}

func TestWhitespaceClientSecretIsInvalidConfiguration(t *testing.T) {
	c := NewConnector()
	err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"profile":"antigravity-v1internal","project":"synthetic-project","oauth":{"client_secret":"   "}}`)})
	if err == nil {
		t.Fatal("whitespace-only client secret was accepted")
	}
}

func TestAuthenticateDiscoveryFailsClosed(t *testing.T) {
	c := NewConnector()
	config := []byte(`{"profile":"antigravity-v1internal","project":"synthetic-project","oauth":{"client_id":"synthetic-client","auth_endpoint":"https://auth.synthetic.invalid/authorize","token_endpoint":"https://oauth.synthetic.invalid/token","redirect_uri":"http://localhost:8080/callback","scopes":["synthetic"]}}`)
	if err := c.Init(context.Background(), core.ComponentConfig{Data: config}); err != nil {
		t.Fatal(err)
	}
	started, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "start"}, core.InvocationServices{})
	if ge != nil {
		t.Fatal(ge)
	}
	for _, tc := range []struct {
		name string
		body string
		err  error
	}{
		{name: "missing project", body: `{}`},
		{name: "discovery network failure", err: errors.New("synthetic network error")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeDoer{do: func(req *http.Request) (*http.Response, error) {
				if req.URL.Host == "oauth.synthetic.invalid" {
					return response(http.StatusOK, `{"access_token":"SYNTHETIC_ACCESS","expires_in":3600,"token_type":"Bearer"}`), nil
				}
				if tc.err != nil {
					return nil, tc.err
				}
				return response(http.StatusOK, tc.body), nil
			}}
			result, err := c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: []byte(started.State), CallbackInput: "SYNTHETIC_CODE"}, core.InvocationServices{Transport: packageTestDoer(fake)})
			if err == nil || result.Supported || len(result.Credentials) != 0 || fake.calls != 2 {
				t.Fatalf("discovery failure leaked candidate: result=%+v error=%v calls=%d", result, err, fake.calls)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &fakeDoer{do: func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "oauth.synthetic.invalid" {
			return response(http.StatusOK, `{"access_token":"SYNTHETIC_ACCESS","expires_in":3600,"token_type":"Bearer"}`), nil
		}
		cancel()
		return response(http.StatusOK, `{"cloudaicompanionProject":"synthetic-project"}`), nil
	}}
	result, err := c.Authenticate(ctx, core.AuthRequest{Action: "continue", State: []byte(started.State), CallbackInput: "SYNTHETIC_CODE"}, core.InvocationServices{Transport: packageTestDoer(fake)})
	if err == nil || err.Category != core.CategoryCancelled || result.Supported || len(result.Credentials) != 0 || fake.calls != 2 {
		t.Fatalf("discovery cancellation leaked candidate: result=%+v error=%v calls=%d", result, err, fake.calls)
	}
}

type cancelDuringReadBody struct {
	cancel context.CancelFunc
	data   []byte
	read   bool
	closed bool
}

func (b *cancelDuringReadBody) Read(p []byte) (int, error) {
	if b.read {
		return 0, io.EOF
	}
	b.read = true
	b.cancel()
	return copy(p, b.data), nil
}
func (b *cancelDuringReadBody) Close() error { b.closed = true; return nil }

func TestAuthenticateBrowserAndRefreshCancellationDuringReadRejectCandidates(t *testing.T) {
	c := NewConnector()
	if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"profile":"antigravity-v1internal","project":"synthetic-project","oauth":{"client_id":"synthetic-client","auth_endpoint":"https://auth.synthetic.invalid/authorize","token_endpoint":"https://oauth.synthetic.invalid/token","redirect_uri":"http://localhost:8080/callback"}}`)}); err != nil {
		t.Fatal(err)
	}
	started, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "start"}, core.InvocationServices{})
	if ge != nil {
		t.Fatal(ge)
	}
	var continuation authContinuation
	if err := json.Unmarshal([]byte(started.State), &continuation); err != nil {
		t.Fatal(err)
	}
	callback := "http://localhost:8080/callback?code=local&state=" + url.QueryEscape(continuation.State)
	for _, action := range []string{"continue", "refresh"} {
		t.Run(action, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			body := &cancelDuringReadBody{cancel: cancel, data: []byte(`{"access_token":"SYNTHETIC_CANCELLED_ACCESS","refresh_token":"SYNTHETIC_CANCELLED_REFRESH","expires_in":3600,"token_type":"Bearer"}`)}
			fake := &fakeDoer{response: &http.Response{StatusCode: http.StatusOK, Body: body}}
			req := core.AuthRequest{Action: action}
			if action == "continue" {
				req.State, req.CallbackInput = []byte(started.State), callback
			}
			services := core.InvocationServices{Transport: packageTestDoer(fake), Credentials: credentials{data: []byte(`{"access_token":"SYNTHETIC_OLD_ACCESS","refresh_token":"SYNTHETIC_OLD_REFRESH"}`)}}
			result, err := c.Authenticate(ctx, req, services)
			if err == nil || err.Category != core.CategoryCancelled || result.Supported || len(result.Credentials) != 0 || !body.closed || fake.calls != 1 {
				t.Fatalf("result=%+v error=%v closed=%t calls=%d", result, err, body.closed, fake.calls)
			}
		})
	}
}

func TestAuthenticateRejectsNewlineRefreshTokenCandidates(t *testing.T) {
	c := NewConnector()
	config := []byte(`{"profile":"antigravity-v1internal","project":"synthetic-project","oauth":{"client_id":"synthetic-client","auth_endpoint":"https://auth.synthetic.invalid/authorize","token_endpoint":"https://oauth.synthetic.invalid/token","redirect_uri":"http://localhost:8080/callback"}}`)
	if err := c.Init(context.Background(), core.ComponentConfig{Data: config}); err != nil {
		t.Fatal(err)
	}
	started, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "start"}, core.InvocationServices{})
	if ge != nil {
		t.Fatal(ge)
	}
	var continuation authContinuation
	if err := json.Unmarshal([]byte(started.State), &continuation); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"continue", "refresh"} {
		t.Run(action, func(t *testing.T) {
			fake := &fakeDoer{response: response(http.StatusOK, `{"access_token":"SYNTHETIC_ACCESS","refresh_token":"SYNTHETIC_BAD\nTOKEN","expires_in":3600,"token_type":"Bearer"}`)}
			req := core.AuthRequest{Action: action}
			if action == "continue" {
				req.State = []byte(started.State)
				req.CallbackInput = "http://localhost:8080/callback?code=local&state=" + url.QueryEscape(continuation.State)
			}
			result, err := c.Authenticate(context.Background(), req, core.InvocationServices{Transport: packageTestDoer(fake), Credentials: credentials{data: []byte(`{"access_token":"SYNTHETIC_OLD_ACCESS","refresh_token":"SYNTHETIC_OLD_REFRESH"}`)}})
			if err == nil || result.Supported || len(result.Credentials) != 0 || fake.calls != 1 {
				t.Fatalf("candidate escaped: result=%+v error=%v calls=%d", result, err, fake.calls)
			}
		})
	}
}

func TestAuthenticateRefreshKeepsRefreshTokenAndFailsClosed(t *testing.T) {
	c := ready(t)
	prior := []byte(`{"access_token":"SYNTHETIC_ACCESS","refresh_token":"SYNTHETIC_PRIOR_REFRESH"}`)
	f := &fakeDoer{response: &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"SYNTHETIC_NEW_ACCESS","expires_in":60,"token_type":"Bearer"}`))}}
	result, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "refresh"}, core.InvocationServices{Transport: packageTestDoer(f), Credentials: credentials{data: prior}})
	var bundle oauthCredentials
	if ge != nil || json.Unmarshal(result.Credentials["oauth"], &bundle) != nil || bundle.RefreshToken != "SYNTHETIC_PRIOR_REFRESH" {
		t.Fatalf("omitted rotation not preserved: result=%+v error=%v", result, ge)
	}
	for _, tc := range []struct {
		name string
		body string
		code int
		err  error
	}{
		{"malformed", `{`, 200, nil},
		{"unknown", `{"access_token":"x","expires_in":60,"extra":1}`, 200, nil},
		{"duplicate", `{"access_token":"x","access_token":"y","expires_in":60}`, 200, nil},
		{"missing expiry", `{"access_token":"x"}`, 200, nil},
		{"missing token type", `{"access_token":"x","expires_in":60}`, 200, nil},
		{"empty access token", `{"access_token":"","expires_in":60,"token_type":"Bearer"}`, 200, nil},
		{"invalid scope type", `{"access_token":"x","expires_in":60,"token_type":"Bearer","scope":4}`, 200, nil},
		{"invalid id token type", `{"access_token":"x","expires_in":60,"token_type":"Bearer","id_token":false}`, 200, nil},
		{"empty rotated token", `{"access_token":"x","refresh_token":"","expires_in":60}`, 200, nil},
		{"non-success", `{}`, 403, nil},
		{"ambiguous", ``, 0, errors.New("ambiguous")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeDoer{err: tc.err}
			if tc.err == nil {
				fake.response = &http.Response{StatusCode: tc.code, Body: io.NopCloser(strings.NewReader(tc.body))}
			}
			got, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "refresh"}, core.InvocationServices{Transport: packageTestDoer(fake), Credentials: credentials{data: prior}})
			if ge == nil || got.Supported || got.Credentials != nil || fake.calls != 1 {
				t.Fatalf("result=%+v error=%v calls=%d", got, ge, fake.calls)
			}
		})
	}
	for _, raw := range []string{`{}`, `{"access_token":"a"}`, `{"access_token":"a","refresh_token":""}`, `{"access_token":"a","refresh_token":"r","extra":1}`, `{"access_token":"a","refresh_token":"r","refresh_token":"s"}`, `{"access_token":"a","REFRESH_TOKEN":"r"}`, `{"access_token":"a","refresh_token":"r","REFRESH_TOKEN":"alias"}`, `{"ACCESS_TOKEN":"a","refresh_token":"r"}`} {
		fake := &fakeDoer{}
		_, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "refresh"}, core.InvocationServices{Transport: packageTestDoer(fake), Credentials: credentials{data: []byte(raw)}})
		if ge == nil || fake.calls != 0 {
			t.Errorf("invalid credential %s: error=%v calls=%d", raw, ge, fake.calls)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &fakeDoer{}
	_, ge = c.Authenticate(ctx, core.AuthRequest{Action: "refresh"}, core.InvocationServices{Transport: packageTestDoer(fake), Credentials: credentials{data: prior}})
	if ge == nil || ge.Category != core.CategoryCancelled || fake.calls != 0 {
		t.Fatalf("cancelled refresh: error=%v calls=%d", ge, fake.calls)
	}
}
