package antigravity

import (
	"context"
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
	f := &fakeDoer{response: &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(fixture)))}}
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
