package antigravity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	f := &fakeDoer{response: &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Replace(string(fixture), `"expires_in":3600`, `"expires_in":3600,"token_type":"Bearer"`, 1)))}}
	c := ready(t)
	prior := []byte(`{"access_token":"SYNTHETIC_OLD_ACCESS","refresh_token":"SYNTHETIC_OLD_REFRESH"}`)
	result, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "refresh"}, core.InvocationServices{Transport: packageTestDoer(f), Credentials: credentials{data: prior}})
	if ge != nil || !result.Supported || f.calls != 1 || result.CredentialExpiresAt == nil || !result.CredentialExpiresAt.After(time.Now()) {
		t.Fatalf("refresh: result=%+v error=%v calls=%d", result, ge, f.calls)
	}
	if f.request.Method != http.MethodPost || f.request.URL.String() != "https://oauth.synthetic.invalid/token" || f.request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Fatalf("bad refresh request: %+v", f.request)
	}
	body, _ := io.ReadAll(f.request.Body)
	form, err := url.ParseQuery(string(body))
	if err != nil || form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "SYNTHETIC_OLD_REFRESH" || len(form) != 2 {
		t.Fatalf("bad refresh form %q (%v)", body, err)
	}
	var bundle oauthCredentials
	if err := json.Unmarshal(result.Credentials["oauth"], &bundle); err != nil || bundle.AccessToken != "SYNTHETIC_ROTATED_ACCESS_TOKEN" || bundle.RefreshToken != "SYNTHETIC_ROTATED_REFRESH_TOKEN" {
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
	fake := &fakeDoer{response: response(200, `{"access_token":"SYNTHETIC_ACCESS","refresh_token":"SYNTHETIC_REFRESH","expires_in":3600,"token_type":"Bearer"}`)}
	for _, callback := range []string{"not-a-url", "http://localhost:8080/callback?code=x&state=" + url.QueryEscape(continuation.State) + "&extra=y", "http://localhost:8080/callback?code=x&code=y&state=" + url.QueryEscape(continuation.State)} {
		bad, err := c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: []byte(result.State), CallbackInput: callback}, core.InvocationServices{Transport: packageTestDoer(fake)})
		if err == nil || bad.Supported || fake.calls != 0 {
			t.Fatalf("malformed callback dispatched: %q result=%+v error=%v calls=%d", callback, bad, err, fake.calls)
		}
	}
	bad, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: []byte(result.State), CallbackInput: "http://localhost:8080/callback?code=local&state=wrong"}, core.InvocationServices{Transport: packageTestDoer(fake)})
	if ge == nil || bad.Supported || fake.calls != 0 {
		t.Fatalf("state mismatch dispatched: %+v %v calls=%d", bad, ge, fake.calls)
	}
	completed, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: []byte(result.State), CallbackInput: "http://localhost:8080/callback?code=local&state=" + url.QueryEscape(continuation.State)}, core.InvocationServices{Transport: packageTestDoer(fake)})
	if ge != nil || !completed.Supported || completed.CredentialExpiresAt == nil || !completed.CredentialExpiresAt.After(time.Now()) || fake.calls != 1 {
		t.Fatalf("continue: %+v %v calls=%d", completed, ge, fake.calls)
	}
	form, err := url.ParseQuery(readFakeRequest(t, fake))
	if err != nil || form.Get("grant_type") != "authorization_code" || form.Get("client_id") != "synthetic-client" || form.Get("code_verifier") != continuation.Verifier || form.Get("code") != "local" {
		t.Fatalf("invalid exchange: %v %v", form, err)
	}
	for _, tc := range []struct {
		status int
		want   core.ErrorCategory
	}{{http.StatusForbidden, core.CategoryUnauthenticated}, {http.StatusTooManyRequests, core.CategoryRateLimited}, {http.StatusBadGateway, core.CategoryUnavailable}} {
		badTransport := &fakeDoer{response: response(tc.status, `{"error":"synthetic"}`)}
		_, got := c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: []byte(result.State), CallbackInput: "http://localhost:8080/callback?code=local&state=" + url.QueryEscape(continuation.State)}, core.InvocationServices{Transport: packageTestDoer(badTransport)})
		if got == nil || got.Category != tc.want || badTransport.calls != 1 {
			t.Errorf("status %d: error=%+v calls=%d", tc.status, got, badTransport.calls)
		}
	}
}

func readFakeRequest(t *testing.T, fake *fakeDoer) string {
	t.Helper()
	body, err := io.ReadAll(fake.request.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
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
	f := &fakeDoer{response: &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"SYNTHETIC_NEW_ACCESS","expires_in":60}`))}}
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
