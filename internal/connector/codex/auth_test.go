package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

func readyAuthConnector(t *testing.T, authURL string) *Connector {
	t.Helper()
	c := NewConnector()
	config, err := json.Marshal(componentConfig{Model: "model", AccountID: "account-a", Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{Data: config}); err != nil {
		t.Fatal(err)
	}
	c.authURL = authURL
	return c
}

func TestDeviceAuthStartExchange(t *testing.T) {
	for _, fixture := range []string{"start-success.json", "start-string-interval.json"} {
		t.Run(fixture, func(t *testing.T) {
			data := loadAuthFixture(t, fixture)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != authStartPath || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("request method/path/content-type = %s %s %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != `{"client_id":"`+deviceClientID+`"}` {
					t.Errorf("request body = %s", body)
				}
				w.Write(data)
			}))
			defer server.Close()
			c := readyAuthConnector(t, server.URL)
			result, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "start", AccountID: "account-a"}, core.InvocationServices{Transport: server.Client()})
			if ge != nil {
				t.Fatalf("Authenticate error: %+v", ge)
			}
			if !result.Supported || result.NextAction != "continue" || result.State == "" || len(result.Credentials) != 0 || result.UserAction == nil {
				t.Fatalf("unexpected result: %+v", result)
			}
			if result.UserAction.VerificationURI != "https://auth.openai.com/codex/device" || result.UserAction.UserCode != "SYNTHETIC-CODE-1234" || result.UserAction.PollInterval != 5*time.Second {
				t.Fatalf("unexpected safe action: %+v", result.UserAction)
			}
			var continuation deviceAuthContinuation
			if err := json.Unmarshal([]byte(result.State), &continuation); err != nil || continuation.DeviceCode != "synthetic-device-code-not-valid" || continuation.ExpiresAt.IsZero() {
				t.Fatalf("opaque continuation missing provider state/expiry: %+v, %v", continuation, err)
			}
			for _, secret := range []string{"synthetic-device-code-not-valid", "device_auth_id", "device_code"} {
				if strings.Contains(jsonValue(t, result.UserAction), secret) {
					t.Fatalf("safe action leaked %q", secret)
				}
			}
		})
	}
}

func TestDeviceAuthStartFailuresAndScope(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"status", http.StatusBadGateway, `{}`},
		{"malformed", http.StatusOK, `{`},
		{"invalid interval", http.StatusOK, string(loadAuthFixture(t, "start-invalid-interval.json"))},
		{"oversized", http.StatusOK, strings.Repeat("x", authStartLimit+1)},
		{"empty user code", http.StatusOK, strings.Replace(string(loadAuthFixture(t, "start-success.json")), `"SYNTHETIC-CODE-1234"`, `""`, 1)},
		{"user code space", http.StatusOK, strings.Replace(string(loadAuthFixture(t, "start-success.json")), `"SYNTHETIC-CODE-1234"`, `"BAD CODE"`, 1)},
		{"user code control", http.StatusOK, strings.Replace(string(loadAuthFixture(t, "start-success.json")), `"SYNTHETIC-CODE-1234"`, `"BAD\nCODE"`, 1)},
		{"long user code", http.StatusOK, strings.Replace(string(loadAuthFixture(t, "start-success.json")), `"SYNTHETIC-CODE-1234"`, `"`+strings.Repeat("A", 257)+`"`, 1)},
		{"URI userinfo", http.StatusOK, strings.Replace(string(loadAuthFixture(t, "start-success.json")), `"https://auth.openai.com/codex/device"`, `"https://user@auth.openai.com/codex/device"`, 1)},
		{"URI whitespace", http.StatusOK, strings.Replace(string(loadAuthFixture(t, "start-success.json")), `"https://auth.openai.com/codex/device"`, `"https://auth.openai.com/a b"`, 1)},
		{"URI control", http.StatusOK, strings.Replace(string(loadAuthFixture(t, "start-success.json")), `"https://auth.openai.com/codex/device"`, `"https://auth.openai.com/a\n"`, 1)},
		{"URI too long", http.StatusOK, strings.Replace(string(loadAuthFixture(t, "start-success.json")), `"https://auth.openai.com/codex/device"`, `"https://auth.openai.com/`+strings.Repeat("a", 2049)+`"`, 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			c := readyAuthConnector(t, server.URL)
			result, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "start", AccountID: "account-a"}, core.InvocationServices{Transport: server.Client()})
			if ge == nil || result.State != "" || strings.Contains(ge.Message, "synthetic-device") {
				t.Fatalf("result=%+v error=%+v", result, ge)
			}
		})
	}
	if _, ge := readyAuthConnector(t, "https://auth.openai.com").Authenticate(context.Background(), core.AuthRequest{Action: "start", AccountID: "account-a"}, core.InvocationServices{Transport: failingAuthTransport{}}); ge == nil || ge.Code != "auth_request_failed" {
		t.Fatalf("transport failure error = %+v", ge)
	}
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	transport := server.Client()
	c := readyAuthConnector(t, server.URL)
	if _, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "start", AccountID: "wrong"}, core.InvocationServices{Transport: transport}); ge == nil || ge.Code != "scope_mismatch" || called {
		t.Fatalf("scope mismatch error=%+v outbound=%v", ge, called)
	}
	unready := NewConnector()
	if _, ge := unready.Authenticate(context.Background(), core.AuthRequest{Action: "start", AccountID: "account-a"}, core.InvocationServices{Transport: transport}); ge == nil || ge.Code != "connector_unavailable" || called {
		t.Fatalf("unready error=%+v outbound=%v", ge, called)
	}
}

func TestDeviceAuthStartRejectsRedirectBeforeFollow(t *testing.T) {
	var targetCalls int
	fixture := string(loadAuthFixture(t, "start-success.json"))
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalls++
		io.WriteString(w, fixture)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer redirect.Close()
	result, ge := readyAuthConnector(t, redirect.URL).Authenticate(context.Background(), core.AuthRequest{Action: "start", AccountID: "account-a"}, core.InvocationServices{Transport: &http.Client{}})
	if ge == nil || result.State != "" || targetCalls != 0 {
		t.Fatalf("redirect result=%+v error=%+v target calls=%d", result, ge, targetCalls)
	}
}

func TestDeviceAuthStartBaseURLAndIntervalValidation(t *testing.T) {
	for _, baseURL := range []string{"http://localhost:1234", "http://user@127.0.0.1:1234", "http://127.0.0.1.evil:1234", "https://user@auth.openai.com"} {
		c := readyAuthConnector(t, baseURL)
		_, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "start", AccountID: "account-a"}, core.InvocationServices{Transport: failingAuthTransport{}})
		if ge == nil || ge.Code != "auth_transport_unavailable" {
			t.Fatalf("base URL %q error = %+v", baseURL, ge)
		}
	}
	for _, raw := range []string{`0x1p2`, `"0x1p2"`, `0`, `"-1"`} {
		if _, err := parseDevicePollInterval(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted interval %s", raw)
		}
	}
	for _, raw := range []string{`5`, `"5"`, `0.5`} {
		got, err := parseDevicePollInterval(json.RawMessage(raw))
		if err != nil || got < time.Second {
			t.Errorf("interval %s = %s, %v", raw, got, err)
		}
	}
}

func TestDeviceAuthStartHasBoundedDeadline(t *testing.T) {
	deadlineSeen := false
	transport := authRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		defer req.Body.Close()
		deadline, ok := req.Context().Deadline()
		remaining := time.Until(deadline)
		deadlineSeen = ok && remaining > 0 && remaining <= authStartTimeout
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: req}, nil
	})
	_, ge := readyAuthConnector(t, "https://auth.openai.com").Authenticate(context.Background(), core.AuthRequest{Action: "start", AccountID: "account-a"}, core.InvocationServices{Transport: transport})
	if ge == nil || !deadlineSeen {
		t.Fatalf("bounded start deadline not enforced: error=%+v seen=%v", ge, deadlineSeen)
	}
}

type authRoundTripperFunc func(*http.Request) (*http.Response, error)

func (fn authRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func (fn authRoundTripperFunc) Do(req *http.Request) (*http.Response, error) {
	return fn.RoundTrip(req)
}

type failingAuthTransport struct{}

func (failingAuthTransport) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("private transport detail")
}

func TestDeviceAuthStartCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		server.Close()
	}()
	c := readyAuthConnector(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan *core.GatewayError, 1)
	go func() {
		_, ge := c.Authenticate(ctx, core.AuthRequest{Action: "start", AccountID: "account-a"}, core.InvocationServices{Transport: server.Client()})
		result <- ge
	}()
	<-started
	cancel()
	if ge := <-result; ge == nil || ge.Code != "auth_cancelled" {
		t.Fatalf("cancellation error = %+v", ge)
	}
}

func jsonValue(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
