package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	testDeviceAuthStartDocumentedResponseDefaults(t)
}

func testDeviceAuthStartDocumentedResponseDefaults(t *testing.T) {
	t.Helper()
	transport := authRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://auth.openai.com"+authStartPath {
			t.Fatalf("request URL = %s", req.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"device_auth_id":"synthetic-device-code-not-valid","user_code":"SYNTHETIC-CODE-1234","interval":"5"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	started := time.Now()
	result, ge := readyAuthConnector(t, "https://auth.openai.com").Authenticate(context.Background(), core.AuthRequest{Action: "start", AccountID: "account-a"}, core.InvocationServices{Transport: transport})
	if ge != nil || !result.Supported || result.UserAction == nil {
		t.Fatalf("result=%+v error=%+v", result, ge)
	}
	if result.UserAction.VerificationURI != "https://auth.openai.com/codex/device" || result.UserAction.UserCode != "SYNTHETIC-CODE-1234" || result.UserAction.PollInterval != 5*time.Second {
		t.Fatalf("unexpected safe action: %+v", result.UserAction)
	}
	var continuation deviceAuthContinuation
	if err := json.Unmarshal([]byte(result.State), &continuation); err != nil || continuation.ExpiresAt.Before(started.Add(14*time.Minute)) || continuation.ExpiresAt.After(started.Add(maxDeviceAuthLifetime+time.Second)) {
		t.Fatalf("default continuation expiry=%v err=%v", continuation.ExpiresAt, err)
	}
}

type authCredentials []byte

func (credential authCredentials) Get(context.Context, string) ([]byte, error) {
	return append([]byte(nil), credential...), nil
}

func TestSelectedAccountRefreshExchange(t *testing.T) {
	prior, err := encodeOAuthBundle(oauthBundle{Version: oauthBundleVersion, AccessToken: "prior-access-secret", RefreshToken: "prior-refresh-secret", AccountID: "provider-b", ExpiresAt: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		fixture, wantRefresh string
	}{
		{"refresh-success.json", "synthetic-rotated-refresh-token-not-valid"},
		{"refresh-no-rotation.json", "prior-refresh-secret"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			data := loadAuthFixture(t, tc.fixture)
			var tokenResponse map[string]json.RawMessage
			if err := json.Unmarshal(data, &tokenResponse); err != nil {
				t.Fatal("invalid synthetic refresh fixture")
			}
			tokenResponse["id_token"], _ = json.Marshal(testJWT([]byte(`{"chatgpt_account_id":"provider-b"}`)))
			data, _ = json.Marshal(tokenResponse)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, _ := io.ReadAll(r.Body)
				form, err := url.ParseQuery(string(body))
				if err != nil || r.Method != http.MethodPost || r.URL.Path != authTokenPath || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" ||
					form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "prior-refresh-secret" || form.Get("client_id") != deviceClientID {
					t.Errorf("unexpected refresh request: %s %s %q form=%v err=%v", r.Method, r.URL.Path, r.Header.Get("Content-Type"), form, err)
				}
				w.Write(data)
			}))
			defer server.Close()
			c := readyAuthConnector(t, server.URL)
			result, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "refresh", AccountID: "account-a"}, core.InvocationServices{Transport: server.Client(), Credentials: authCredentials(prior)})
			if ge != nil || !result.Supported || result.NextAction != "" || result.State != "" || result.UserAction != nil || calls != 1 {
				t.Fatalf("result=%+v error=%+v calls=%d", result, ge, calls)
			}
			got, err := decodeOAuthBundle(result.Credentials["oauth"])
			if err != nil || got.RefreshToken != tc.wantRefresh || got.AccountID != "provider-b" || got.AccessToken != "synthetic-refreshed-access-token-not-valid" {
				t.Fatalf("bundle=%+v err=%v", got, err)
			}
			if result.CredentialExpiresAt == nil || !result.CredentialExpiresAt.Equal(got.ExpiresAt) || !got.ExpiresAt.After(time.Now()) {
				t.Fatalf("expiry metadata=%v bundle expiry=%v", result.CredentialExpiresAt, got.ExpiresAt)
			}
		})
	}
}

func TestSelectedAccountRefreshDocumentedTokenShape(t *testing.T) {
	prior := mustBundle(t, oauthBundle{Version: oauthBundleVersion, AccessToken: "prior", RefreshToken: "prior-refresh", AccountID: "account-a", ExpiresAt: time.Now()})
	body := `{"access_token":"opaque-refreshed-access","id_token":"` + testJWT([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account-a"}}`)) + `"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
	defer server.Close()
	result, ge := readyAuthConnector(t, server.URL).Authenticate(context.Background(), core.AuthRequest{Action: "refresh", AccountID: "account-a"}, core.InvocationServices{Transport: server.Client(), Credentials: authCredentials(prior)})
	if ge != nil || !result.Supported {
		t.Fatalf("refresh result=%+v error=%+v", result, ge)
	}
	bundle, err := decodeOAuthBundle(result.Credentials["oauth"])
	if err != nil || bundle.AccessToken != "opaque-refreshed-access" || bundle.RefreshToken != "prior-refresh" || bundle.AccountID != "account-a" || time.Until(bundle.ExpiresAt) < 59*time.Minute || time.Until(bundle.ExpiresAt) > 61*time.Minute {
		t.Fatalf("refresh bundle=%+v err=%v", bundle, err)
	}
}

func TestSelectedAccountRefreshExchangeFailures(t *testing.T) {
	prior, err := encodeOAuthBundle(oauthBundle{Version: oauthBundleVersion, AccessToken: "private-access", RefreshToken: "private-refresh", AccountID: "account-a", ExpiresAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, body string
		status     int
	}{
		{"terminal", string(loadAuthFixture(t, "refresh-terminal-error.json")), http.StatusBadRequest},
		{"malformed", `{`, http.StatusOK},
		{"missing access", `{"refresh_token":"rotated","expires_in":30,"token_type":"Bearer"}`, http.StatusOK},
		{"zero expiry", `{"access_token":"new","expires_in":0,"token_type":"Bearer"}`, http.StatusOK},
		{"negative expiry", `{"access_token":"new","expires_in":-1,"token_type":"Bearer"}`, http.StatusOK},
		{"null expiry", `{"access_token":"new","expires_in":null,"token_type":"Bearer"}`, http.StatusOK},
		{"oversized", strings.Repeat("x", authStartLimit+1), http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			result, ge := readyAuthConnector(t, server.URL).Authenticate(context.Background(), core.AuthRequest{Action: "refresh", AccountID: "account-a"}, core.InvocationServices{Transport: server.Client(), Credentials: authCredentials(prior)})
			if ge == nil || len(result.Credentials) != 0 || calls != 1 || strings.Contains(ge.Error(), "private-") {
				t.Fatalf("result=%+v error=%+v calls=%d", result, ge, calls)
			}
		})
	}
	for _, tc := range []struct {
		name string
		cred core.CredentialAccess
		acct string
	}{
		{"missing credentials", nil, "account-a"},
		{"missing refresh", authCredentials(mustBundle(t, oauthBundle{Version: oauthBundleVersion, AccessToken: "access", AccountID: "account-a", ExpiresAt: time.Now()})), "account-a"},
		{"request account mismatch", authCredentials(prior), "account-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
			defer server.Close()
			result, ge := readyAuthConnector(t, server.URL).Authenticate(context.Background(), core.AuthRequest{Action: "refresh", AccountID: tc.acct}, core.InvocationServices{Transport: server.Client(), Credentials: tc.cred})
			if ge == nil || len(result.Credentials) != 0 || called {
				t.Fatalf("result=%+v error=%+v outbound=%v", result, ge, called)
			}
		})
	}
}

func TestSelectedAccountRefreshExchangeBoundaries(t *testing.T) {
	prior := mustBundle(t, oauthBundle{Version: oauthBundleVersion, AccessToken: "prior", RefreshToken: "prior-refresh", AccountID: "provider-b", ExpiresAt: time.Now()})
	wrongIdentity := `{"access_token":"` + testJWT([]byte(`{"chatgpt_account_id":"provider-c"}`)) + `","expires_in":3600,"token_type":"Bearer"}`
	for _, tc := range []struct{ name, body string }{{"identity mismatch", wrongIdentity}} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, tc.body) }))
			defer server.Close()
			result, ge := readyAuthConnector(t, server.URL).Authenticate(context.Background(), core.AuthRequest{Action: "refresh", AccountID: "account-a"}, core.InvocationServices{Transport: server.Client(), Credentials: authCredentials(prior)})
			if ge == nil || ge.Code != "scope_mismatch" || len(result.Credentials) != 0 || strings.Contains(ge.Error(), "account-b") {
				t.Fatalf("result=%+v error=%+v", result, ge)
			}
		})
	}
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	if _, ge := NewConnector().Authenticate(context.Background(), core.AuthRequest{Action: "refresh", AccountID: "account-a"}, core.InvocationServices{}); ge == nil || ge.Code != "connector_unavailable" {
		t.Fatalf("unready connector error=%+v", ge)
	}
	if called {
		t.Fatal("unready connector contacted upstream")
	}

	var targetCalls int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { targetCalls++; w.Write([]byte(`{}`)) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer redirect.Close()
	result, ge := readyAuthConnector(t, redirect.URL).Authenticate(context.Background(), core.AuthRequest{Action: "refresh", AccountID: "account-a"}, core.InvocationServices{Transport: &http.Client{}, Credentials: authCredentials(prior)})
	if ge == nil || len(result.Credentials) != 0 || targetCalls != 0 {
		t.Fatalf("redirect result=%+v error=%+v target calls=%d", result, ge, targetCalls)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	cancelServer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { close(started); <-release }))
	defer cancelServer.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *core.GatewayError, 1)
	cancelConnector := readyAuthConnector(t, cancelServer.URL)
	go func() {
		_, ge := cancelConnector.Authenticate(ctx, core.AuthRequest{Action: "refresh", AccountID: "account-a"}, core.InvocationServices{Transport: cancelServer.Client(), Credentials: authCredentials(prior)})
		done <- ge
	}()
	<-started
	cancel()
	if ge := <-done; ge == nil || ge.Code != "auth_cancelled" {
		t.Fatalf("cancellation error=%+v", ge)
	}
}

func mustBundle(t *testing.T, bundle oauthBundle) []byte {
	t.Helper()
	data, err := encodeOAuthBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return data
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
		{"negative omitted-shape interval", http.StatusOK, `{"device_auth_id":"synthetic-device","user_code":"SAFE-CODE","interval":-1}`},
		{"negative omitted-shape expiry", http.StatusOK, `{"device_auth_id":"synthetic-device","user_code":"SAFE-CODE","expires_in":-1}`},
		{"empty explicit URI", http.StatusOK, strings.Replace(string(loadAuthFixture(t, "start-success.json")), `"https://auth.openai.com/codex/device"`, `""`, 1)},
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

func TestDeviceAuthPollExchange(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		status  int
	}{
		{"poll-pending.json", http.StatusOK},
		{"poll-pending-403.json", http.StatusForbidden},
		{"poll-pending-404.json", http.StatusNotFound},
		{"poll-slow-down.json", http.StatusOK},
		{"poll-authorized.json", http.StatusOK},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			originalExpiry := time.Now().Add(10 * time.Minute).Truncate(time.Second)
			continuation := deviceAuthContinuation{DeviceCode: "synthetic-device-code-not-valid", UserCode: "SYNTHETIC-CODE-1234", VerificationURI: "https://auth.openai.com/codex/device", Interval: time.Second, ExpiresAt: originalExpiry}
			state, _ := json.Marshal(continuation)
			fixture := loadAuthFixture(t, tc.fixture)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != authPollPath || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("request method/path/content-type = %s %s %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != `{"device_auth_id":"synthetic-device-code-not-valid","user_code":"SYNTHETIC-CODE-1234"}` {
					t.Errorf("request body = %s", body)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write(fixture)
			}))
			defer server.Close()
			result, ge := readyAuthConnector(t, server.URL).Authenticate(context.Background(), core.AuthRequest{Action: "continue", AccountID: "account-a", State: state}, core.InvocationServices{Transport: server.Client()})
			if ge != nil || !result.Supported || result.NextAction != "continue" || len(result.Credentials) != 0 {
				t.Fatalf("result=%+v error=%+v", result, ge)
			}
			var next deviceAuthContinuation
			if err := json.Unmarshal([]byte(result.State), &next); err != nil || !next.ExpiresAt.Equal(originalExpiry) {
				t.Fatalf("continuation expiry changed: %+v, err=%v", next, err)
			}
			if tc.fixture == "poll-authorized.json" {
				if result.UserAction != nil || next.AuthorizationCode != "synthetic-authorization-code" || next.CodeVerifier != "synthetic-code-verifier" || next.DeviceCode != "" {
					t.Fatalf("authorized transition result=%+v state=%+v", result, next)
				}
				return
			}
			if result.UserAction == nil || result.UserAction.UserCode != continuation.UserCode || result.UserAction.VerificationURI != continuation.VerificationURI || next.LastPolledAt.IsZero() {
				t.Fatalf("pending projection/state result=%+v state=%+v", result, next)
			}
			wantInterval := time.Second
			if tc.fixture == "poll-slow-down.json" {
				wantInterval = 10 * time.Second
			}
			if result.UserAction.PollInterval != wantInterval || next.Interval != wantInterval {
				t.Fatalf("interval action=%s state=%s want=%s", result.UserAction.PollInterval, next.Interval, wantInterval)
			}
		})
	}
}

func TestDeviceAuthPollFailuresAndTiming(t *testing.T) {
	makeState := func(expiry, last time.Time, interval time.Duration) []byte {
		data, _ := json.Marshal(deviceAuthContinuation{DeviceCode: "private-device", UserCode: "SAFE-CODE", VerificationURI: "https://auth.openai.com/codex/device", Interval: interval, LastPolledAt: last, ExpiresAt: expiry})
		return data
	}
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(http.StatusBadGateway) }))
	defer server.Close()
	c := readyAuthConnector(t, server.URL)
	_, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", AccountID: "account-a", State: makeState(time.Now().Add(-time.Second), time.Time{}, time.Second)}, core.InvocationServices{Transport: server.Client()})
	if ge == nil || ge.Code != "auth_expired" || called {
		t.Fatalf("expired poll error=%+v transport=%v", ge, called)
	}
	if _, ge = c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", AccountID: "account-a", State: []byte("{")}, core.InvocationServices{Transport: server.Client()}); ge == nil || ge.Code != "auth_invalid_state" {
		t.Fatalf("malformed state error=%+v", ge)
	}
	_, ge = c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", AccountID: "account-a", State: makeState(time.Now().Add(time.Minute), time.Time{}, time.Second)}, core.InvocationServices{Transport: failingAuthTransport{}})
	if ge == nil || ge.Code != "auth_request_failed" || strings.Contains(ge.Message, "private") {
		t.Fatalf("transport failure error=%+v", ge)
	}
	for _, tc := range []struct {
		name      string
		account   string
		connector *Connector
	}{
		{"scope", "wrong", c}, {"unready", "account-a", NewConnector()},
	} {
		called = false
		_, ge := tc.connector.Authenticate(context.Background(), core.AuthRequest{Action: "continue", AccountID: tc.account, State: makeState(time.Now().Add(time.Minute), time.Time{}, time.Second)}, core.InvocationServices{Transport: server.Client()})
		if ge == nil || called {
			t.Errorf("%s error=%+v transport=%v", tc.name, ge, called)
		}
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"terminal status", http.StatusBadGateway, `{}`}, {"malformed", http.StatusOK, `{`}, {"unexpected", http.StatusOK, `{"error":"access_denied","secret":"hidden"}`}, {"oversized", http.StatusOK, strings.Repeat("x", authStartLimit+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer bad.Close()
			result, ge := readyAuthConnector(t, bad.URL).Authenticate(context.Background(), core.AuthRequest{Action: "continue", AccountID: "account-a", State: makeState(time.Now().Add(time.Minute), time.Time{}, time.Second)}, core.InvocationServices{Transport: bad.Client()})
			if ge == nil || result.State != "" || strings.Contains(ge.Message, "hidden") {
				t.Fatalf("result=%+v error=%+v", result, ge)
			}
		})
	}
	started := time.Now()
	waitState := makeState(time.Now().Add(time.Minute), time.Now().Add(-900*time.Millisecond), time.Second)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(loadAuthFixture(t, "poll-pending.json")) })
	_, ge = c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", AccountID: "account-a", State: waitState}, core.InvocationServices{Transport: server.Client()})
	if ge != nil || time.Since(started) < 80*time.Millisecond {
		t.Fatalf("interval wait elapsed=%s error=%+v", time.Since(started), ge)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelState := makeState(time.Now().Add(time.Minute), time.Now().Add(-900*time.Millisecond), time.Second)
	_, ge = c.Authenticate(ctx, core.AuthRequest{Action: "continue", AccountID: "account-a", State: cancelState}, core.InvocationServices{Transport: server.Client()})
	if ge == nil || ge.Code != "auth_cancelled" {
		t.Fatalf("wait cancellation error=%+v", ge)
	}
}

func TestDeviceAuthPollRejectsRedirect(t *testing.T) {
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls++ }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer redirect.Close()
	state, _ := json.Marshal(deviceAuthContinuation{DeviceCode: "private", UserCode: "SAFE", VerificationURI: "https://auth.openai.com/codex/device", Interval: time.Second, ExpiresAt: time.Now().Add(time.Minute)})
	result, ge := readyAuthConnector(t, redirect.URL).Authenticate(context.Background(), core.AuthRequest{Action: "continue", AccountID: "account-a", State: state}, core.InvocationServices{Transport: &http.Client{}})
	if ge == nil || result.State != "" || targetCalls != 0 {
		t.Fatalf("redirect result=%+v error=%+v target calls=%d", result, ge, targetCalls)
	}
}

func TestDeviceAuthPollRequestCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	state, _ := json.Marshal(deviceAuthContinuation{DeviceCode: "private", UserCode: "SAFE", VerificationURI: "https://auth.openai.com/codex/device", Interval: time.Second, ExpiresAt: time.Now().Add(time.Minute)})
	c := readyAuthConnector(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan *core.GatewayError, 1)
	go func() {
		_, ge := c.Authenticate(ctx, core.AuthRequest{Action: "continue", AccountID: "account-a", State: state}, core.InvocationServices{Transport: server.Client()})
		result <- ge
	}()
	<-started
	cancel()
	if ge := <-result; ge == nil || ge.Code != "auth_cancelled" {
		t.Fatalf("request cancellation error=%+v", ge)
	}
}

func TestDeviceAuthCodeExchange(t *testing.T) {
	const code, verifier = "private-authorization-code", "private-code-verifier"
	access := testJWT([]byte(`{"chatgpt_account_id":"provider-b"}`))
	fixture := decodeAuthFixture(t, "exchange-success.json")
	fixture["access_token"], _ = json.Marshal(access)
	fixture["id_token"], _ = json.Marshal(testJWT([]byte(`{"chatgpt_account_id":"provider-b"}`)))
	body, _ := json.Marshal(fixture)
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != authTokenPath || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("request method/path/content-type = %s %s %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != code ||
			r.Form.Get("code_verifier") != verifier || r.Form.Get("redirect_uri") != "https://auth.openai.com/deviceauth/callback" ||
			r.Form.Get("client_id") != deviceClientID {
			t.Errorf("unexpected OAuth form: %v err=%v", r.Form, err)
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	state, _ := json.Marshal(deviceAuthContinuation{AuthorizationCode: code, CodeVerifier: verifier, ExpiresAt: time.Now().Add(time.Minute)})
	result, ge := readyAuthConnector(t, server.URL).Authenticate(context.Background(), core.AuthRequest{Action: "continue", AccountID: "account-a", State: state}, core.InvocationServices{Transport: server.Client()})
	if ge != nil || !result.Supported || result.NextAction != "" || result.State != "" || result.UserAction != nil || requests != 1 {
		t.Fatalf("result=%+v error=%+v requests=%d", result, ge, requests)
	}
	bundle, err := decodeOAuthBundle(result.Credentials["oauth"])
	if err != nil || bundle.AccessToken != access || bundle.AccountID != "provider-b" || bundle.RefreshToken == "" || result.CredentialExpiresAt == nil || !bundle.ExpiresAt.Equal(*result.CredentialExpiresAt) || !bundle.ExpiresAt.After(time.Now()) {
		t.Fatalf("bundle=%+v expiry=%v err=%v", bundle, result.CredentialExpiresAt, err)
	}

	for _, tc := range []struct {
		name   string
		body   []byte
		status int
		want   string
	}{
		{"terminal", loadAuthFixture(t, "exchange-terminal-error.json"), http.StatusBadGateway, "auth_rejected"},
		{"no refresh", loadAuthFixture(t, "exchange-no-refresh.json"), http.StatusOK, "auth_invalid_response"},
		{"malformed", loadAuthFixture(t, "malformed.json"), http.StatusOK, "auth_invalid_response"},
		{"missing identity", []byte(`{"token_type":"Bearer","access_token":"` + testJWT([]byte(`{"sub":"user"}`)) + `","refresh_token":"refresh","expires_in":3600}`), http.StatusOK, "auth_invalid_response"},
		{"zero expiry", []byte(`{"token_type":"Bearer","access_token":"` + access + `","refresh_token":"refresh","expires_in":0}`), http.StatusOK, "auth_invalid_response"},
		{"negative expiry", []byte(`{"token_type":"Bearer","access_token":"` + access + `","refresh_token":"refresh","expires_in":-1}`), http.StatusOK, "auth_invalid_response"},
		{"oversized", []byte(strings.Repeat("x", authStartLimit+1)), http.StatusOK, "auth_invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status); _, _ = w.Write(tc.body) }))
			defer bad.Close()
			state, _ := json.Marshal(deviceAuthContinuation{AuthorizationCode: code, CodeVerifier: verifier, ExpiresAt: time.Now().Add(time.Minute)})
			result, ge := readyAuthConnector(t, bad.URL).Authenticate(context.Background(), core.AuthRequest{Action: "continue", AccountID: "account-a", State: state}, core.InvocationServices{Transport: bad.Client()})
			if ge == nil || ge.Code != tc.want || result.State != "" || len(result.Credentials) != 0 || strings.Contains(ge.Message, code) || strings.Contains(ge.Message, verifier) || strings.Contains(ge.Message, access) {
				t.Fatalf("result=%+v error=%+v", result, ge)
			}
		})
	}

	called := false
	noCall := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer noCall.Close()
	for _, account := range []string{"wrong", "account-a"} {
		connector := readyAuthConnector(t, noCall.URL)
		if account == "account-a" {
			connector.Close(context.Background())
		}
		_, ge := connector.Authenticate(context.Background(), core.AuthRequest{Action: "continue", AccountID: account, State: state}, core.InvocationServices{Transport: noCall.Client()})
		if ge == nil || called {
			t.Fatalf("account=%s error=%+v contacted=%v", account, ge, called)
		}
	}
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls++ }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer redirect.Close()
	_, ge = readyAuthConnector(t, redirect.URL).Authenticate(context.Background(), core.AuthRequest{Action: "continue", AccountID: "account-a", State: state}, core.InvocationServices{Transport: &http.Client{}})
	if ge == nil || targetCalls != 0 {
		t.Fatalf("redirect error=%+v target calls=%d", ge, targetCalls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer blocked.Close()
	got := make(chan *core.GatewayError, 1)
	go func() {
		_, ge := readyAuthConnector(t, blocked.URL).Authenticate(ctx, core.AuthRequest{Action: "continue", AccountID: "account-a", State: state}, core.InvocationServices{Transport: blocked.Client()})
		got <- ge
	}()
	<-started
	cancel()
	close(release)
	if ge := <-got; ge == nil || ge.Code != "auth_cancelled" {
		t.Fatalf("cancel error=%+v", ge)
	}
}

func TestDeviceAuthCodeExchangeDocumentedTokenShape(t *testing.T) {
	access := "opaque-access-token"
	idToken := testJWT([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account-a"}}`))
	response := func(tokenType, expires string) string {
		body := `{"access_token":"` + access + `","id_token":"` + idToken + `","refresh_token":"refresh"`
		if tokenType != "" {
			body += `,"token_type":` + tokenType
		}
		if expires != "" {
			body += `,"expires_in":` + expires
		}
		return body + `}`
	}
	for _, tc := range []struct {
		name, body string
		wantOK     bool
	}{
		{"omitted optional response fields", response("", ""), true},
		{"invalid explicit token type", response(`"MAC"`, "3600"), false},
		{"invalid explicit expiry", response(`"Bearer"`, `"3600"`), false},
		{"null explicit expiry", response(`"Bearer"`, `null`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tc.body) }))
			defer server.Close()
			state, _ := json.Marshal(deviceAuthContinuation{AuthorizationCode: "synthetic-code", CodeVerifier: "synthetic-verifier", ExpiresAt: time.Now().Add(time.Minute)})
			result, ge := readyAuthConnector(t, server.URL).Authenticate(context.Background(), core.AuthRequest{Action: "continue", AccountID: "account-a", State: state}, core.InvocationServices{Transport: server.Client()})
			if tc.wantOK {
				bundle, err := decodeOAuthBundle(result.Credentials["oauth"])
				if ge != nil || err != nil || bundle.AccessToken != access || bundle.AccountID != "account-a" || time.Until(bundle.ExpiresAt) < 59*time.Minute || time.Until(bundle.ExpiresAt) > 61*time.Minute {
					t.Fatalf("exchange bundle=%+v err=%v provider_error=%+v", bundle, err, ge)
				}
			} else if ge == nil || ge.Code != "auth_invalid_response" || len(result.Credentials) != 0 {
				t.Fatalf("exchange result=%+v error=%+v", result, ge)
			}
		})
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
