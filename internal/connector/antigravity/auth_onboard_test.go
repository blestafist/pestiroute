package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestEligibleFreeTierTruthTable(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"free-tier absent tos accepted", `{"allowedTiers":[{"id":"free-tier"}],"currentTier":null,"paidTier":null}`, true},
		{"unknown id default", `{"allowedTiers":[{"id":"standard-tier","isDefault":true}]}`, false},
		{"duplicate", `{"allowedTiers":[{"id":"free-tier"},{"id":"free-tier"}]}`, false},
		{"tos false", `{"allowedTiers":[{"id":"free-tier","hasAcceptedTos":false}]}`, false},
		{"user project", `{"allowedTiers":[{"id":"free-tier","userDefinedCloudaicompanionProject":true}]}`, false},
		{"bad boolean", `{"allowedTiers":[{"id":"free-tier","hasAcceptedTos":"true"}]}`, false},
		{"paid", `{"allowedTiers":[{"id":"free-tier"}],"paidTier":{"id":"paid"}}`, false},
		{"current tier", `{"allowedTiers":[{"id":"free-tier"}],"currentTier":{"id":"free-tier"}}`, false},
		{"privacy notice", `{"allowedTiers":[{"id":"free-tier","privacyNotice":{"showNotice":true}}]}`, false},
		{"nullable user project false", `{"allowedTiers":[{"id":"free-tier","userDefinedCloudaicompanionProject":null}]}`, true},
		{"null terms malformed", `{"allowedTiers":[{"id":"free-tier","hasAcceptedTos":null}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := eligibleFreeTier([]byte(tc.body)); got != tc.want {
				t.Fatalf("got %t want %t", got, tc.want)
			}
		})
	}
}

func TestOnboardOptInPollsOncePerOperationAndValidatesProject(t *testing.T) {
	oldInterval := operationPollInterval
	operationPollInterval = time.Nanosecond
	t.Cleanup(func() { operationPollInterval = oldInterval })
	c := NewConnector()
	if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"profile":"antigravity-v1internal","project":"synthetic-project","endpoint":"https://discovery.synthetic.invalid","allow_onboard":true,"oauth":{"client_id":"synthetic-client","client_secret":"synthetic-secret","token_endpoint":"https://oauth.synthetic.invalid/token","redirect_uri":"http://localhost:8080/callback"}}`)}); err != nil {
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
	var requests []string
	var diagnostics bytes.Buffer
	transport := &fakeDoer{do: func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Method+" "+req.URL.String())
		switch req.URL.String() {
		case "https://oauth.synthetic.invalid/token":
			return response(http.StatusOK, `{"access_token":"SYNTHETIC_ACCESS","refresh_token":"SYNTHETIC_REFRESH","expires_in":3600,"token_type":"Bearer"}`), nil
		case "https://discovery.synthetic.invalid/v1internal:loadCodeAssist":
			return response(http.StatusOK, `{"allowedTiers":[{"id":"free-tier"}],"currentTier":null,"paidTier":null}`), nil
		case "https://discovery.synthetic.invalid/v1internal:onboardUser":
			if req.Method != http.MethodPost {
				return nil, errors.New("expected POST")
			}
			var body map[string]json.RawMessage
			_ = json.NewDecoder(req.Body).Decode(&body)
			if _, exists := body["cloudaicompanionProject"]; exists {
				return nil, errors.New("project hint must be omitted")
			}
			return response(http.StatusOK, `{"name":"v1internal/operations/synthetic-op","done":false}`), nil
		case "https://discovery.synthetic.invalid/v1internal/operations/synthetic-op":
			return response(http.StatusOK, `{"done":true,"response":{"cloudaicompanionProject":{"id":"synthetic-managed-project"}}}`), nil
		default:
			return nil, errors.New("unexpected synthetic request")
		}
	}}
	callback := continuation.RedirectURI + "?" + url.Values{"code": {"SYNTHETIC_CODE"}, "state": {continuation.State}}.Encode()
	result, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: []byte(started.State), CallbackInput: callback}, core.InvocationServices{Transport: packageTestDoer(transport), Logger: slog.New(slog.NewTextHandler(&diagnostics, nil))})
	if ge != nil {
		t.Fatalf("continue failed: %v", ge)
	}
	var saved oauthCredentials
	if json.Unmarshal(result.Credentials["oauth"], &saved) != nil || saved.ProjectID != "synthetic-managed-project" {
		t.Fatalf("bad credential result: %s", result.Credentials["oauth"])
	}
	if transport.calls != 4 || len(requests) != 4 || !strings.HasPrefix(requests[2], "POST https://discovery.synthetic.invalid/v1internal:onboardUser") || !strings.HasPrefix(requests[3], "GET https://discovery.synthetic.invalid/v1internal/operations/") {
		t.Fatalf("requests=%v calls=%d", requests, transport.calls)
	}
	if !strings.Contains(diagnostics.String(), "reason=auth_onboarding_started") || !strings.Contains(diagnostics.String(), "reason=auth_onboarding_polling") {
		t.Fatalf("safe stage diagnostics missing: %q", diagnostics.String())
	}
	for _, canary := range []string{"SYNTHETIC_ACCESS", "SYNTHETIC_REFRESH", "synthetic-op", "synthetic-managed-project", "v1internal/operations"} {
		if strings.Contains(diagnostics.String(), canary) {
			t.Fatalf("diagnostics leaked %q", canary)
		}
	}
}

func TestOnboardRefusesUnsafeNamesAndCancelledContext(t *testing.T) {
	for _, name := range []string{"https://evil.invalid/v1internal/operations/x", "/v1internal/operations/x", "/operations/x", "/v1internal/operations/../x", "v1internal/operations/x?next=1", "v1internal/operations/x#f", "operations/x/y"} {
		if validOperationName(name) {
			t.Errorf("accepted unsafe operation name %q", name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	doer := &fakeDoer{}
	_, ge := onboardingRequest(ctx, doer, http.MethodPost, "https://discovery.synthetic.invalid/v1internal:onboardUser", "SYNTHETIC", nil)
	if ge == nil || doer.calls != 0 {
		t.Fatalf("cancelled dispatch: error=%v calls=%d", ge, doer.calls)
	}
}

func TestOnboardPollExhaustionCancellationAndBudgetFailure(t *testing.T) {
	oldInterval := operationPollInterval
	operationPollInterval = time.Nanosecond
	t.Cleanup(func() { operationPollInterval = oldInterval })
	load := json.RawMessage(`{"allowedTiers":[{"id":"free-tier"}]}`)
	t.Run("poll limit", func(t *testing.T) {
		calls := 0
		fake := &fakeDoer{do: func(req *http.Request) (*http.Response, error) {
			calls++
			if req.Method == http.MethodPost {
				return response(http.StatusOK, `{"name":"operations/synthetic-op","done":false}`), nil
			}
			return response(http.StatusOK, `{"name":"operations/synthetic-op","done":false}`), nil
		}}
		_, ge := onboardProject(context.Background(), "https://discovery.synthetic.invalid", packageTestDoer(fake), "SYNTHETIC_ACCESS", load, nil)
		if ge == nil || ge.Code != "auth_onboarding_failed" || calls != 13 {
			t.Fatalf("error=%+v dispatches=%d", ge, calls)
		}
	})
	t.Run("cancel after post", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		fake := &fakeDoer{do: func(*http.Request) (*http.Response, error) {
			cancel()
			return response(http.StatusOK, `{"name":"operations/synthetic-op","done":false}`), nil
		}}
		_, ge := onboardProject(ctx, "https://discovery.synthetic.invalid", packageTestDoer(fake), "SYNTHETIC_ACCESS", load, nil)
		if ge == nil || fake.calls != 1 {
			t.Fatalf("error=%+v dispatches=%d", ge, fake.calls)
		}
	})
	t.Run("budget exhausted before post", func(t *testing.T) {
		fake := &fakeDoer{err: core.ErrRequestBudgetExceeded}
		_, ge := onboardProject(context.Background(), "https://discovery.synthetic.invalid", packageTestDoer(fake), "SYNTHETIC_ACCESS", load, nil)
		if ge == nil || ge.Code != "request_budget_exceeded" || fake.calls != 1 {
			t.Fatalf("error=%+v dispatches=%d", ge, fake.calls)
		}
	})
	t.Run("ambiguous post is never retried", func(t *testing.T) {
		fake := &fakeDoer{err: errors.New("synthetic ambiguous transport result")}
		_, ge := onboardProject(context.Background(), "https://discovery.synthetic.invalid", packageTestDoer(fake), "SYNTHETIC_ACCESS", load, nil)
		if ge == nil || ge.Code != "auth_onboarding_failed" || fake.calls != 1 {
			t.Fatalf("error=%+v dispatches=%d", ge, fake.calls)
		}
	})
	t.Run("existing current tier does not onboard", func(t *testing.T) {
		fake := &fakeDoer{}
		_, ge := onboardProject(context.Background(), "https://discovery.synthetic.invalid", packageTestDoer(fake), "SYNTHETIC_ACCESS", []byte(`{"currentTier":{"id":"free-tier"},"allowedTiers":[{"id":"free-tier"}]}`), nil)
		if ge == nil || ge.Code != "auth_project_missing" || fake.calls != 0 {
			t.Fatalf("error=%+v dispatches=%d", ge, fake.calls)
		}
	})
}

func TestOnboardImmediateProjectAndTerminalErrors(t *testing.T) {
	oldInterval := operationPollInterval
	operationPollInterval = time.Nanosecond
	t.Cleanup(func() { operationPollInterval = oldInterval })
	load := json.RawMessage(`{"allowedTiers":[{"id":"free-tier"}]}`)
	for _, tc := range []struct {
		name                  string
		post, poll            string
		wantProject, wantCode string
		wantCalls             int
	}{
		{name: "immediate project", post: `{"cloudaicompanionProject":{"id":"synthetic-project"},"done":true}`, wantProject: "synthetic-project", wantCalls: 1},
		{name: "terminal error", post: `{"name":"operations/synthetic-op","error":{"code":7}}`, wantCode: "auth_onboarding_failed", wantCalls: 1},
		{name: "bad operation path", post: `{"name":"https://outside.invalid/operations/synthetic-op"}`, wantCode: "auth_onboarding_failed", wantCalls: 1},
		{name: "leading internal slash", post: `{"name":"/v1internal/operations/synthetic-op"}`, wantCode: "auth_onboarding_failed", wantCalls: 1},
		{name: "leading operations slash", post: `{"name":"/operations/synthetic-op"}`, wantCode: "auth_onboarding_failed", wantCalls: 1},
		{name: "malformed json", post: `{`, wantCode: "auth_onboarding_failed", wantCalls: 1},
		{name: "terminal response lacks project", post: `{"name":"operations/synthetic-op"}`, poll: `{"done":true,"response":{}}`, wantCode: "auth_onboarding_failed", wantCalls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			fake := &fakeDoer{do: func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Header.Get("Authorization") != "Bearer SYNTHETIC_ACCESS" {
					return nil, errors.New("missing scoped bearer")
				}
				if req.Method == http.MethodPost {
					return response(http.StatusOK, tc.post), nil
				}
				if req.URL.Host != "discovery.synthetic.invalid" || req.Method != http.MethodGet {
					return nil, errors.New("poll escaped same-origin GET")
				}
				return response(http.StatusOK, tc.poll), nil
			}}
			got, ge := onboardProject(context.Background(), "https://discovery.synthetic.invalid", packageTestDoer(fake), "SYNTHETIC_ACCESS", load, nil)
			if calls != tc.wantCalls {
				t.Fatalf("calls=%d want=%d", calls, tc.wantCalls)
			}
			if ge != nil {
				if tc.wantCode == "" || ge.Code != tc.wantCode {
					t.Fatalf("error=%+v wantCode=%q", ge, tc.wantCode)
				}
			} else if got != tc.wantProject {
				t.Fatalf("project=%q want=%q", got, tc.wantProject)
			}
		})
	}
}

func TestOnboardFixturesAreSyntheticAndValidJSON(t *testing.T) {
	files, err := filepath.Glob("testdata/onboard-*.json")
	if err != nil || len(files) != 7 {
		t.Fatalf("fixtures=%v err=%v", files, err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil || !json.Valid(data) {
			t.Fatalf("fixture %s invalid: %v", file, err)
		}
		if strings.Contains(string(data), "access_token") {
			t.Fatalf("fixture contains token: %s", file)
		}
	}
}
