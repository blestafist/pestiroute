package claudecode

import (
	"bytes"
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
	"sync"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

type fakeAuthDoer func(*http.Request) (*http.Response, error)

func (f fakeAuthDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

type fakeCredentialAccess []byte

func (f fakeCredentialAccess) Get(context.Context, string) ([]byte, error) {
	return append([]byte(nil), f...), nil
}

type blockingCredentialAccess struct{ entered chan struct{} }

func (b blockingCredentialAccess) Get(ctx context.Context, _ string) ([]byte, error) {
	close(b.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

type blockingAuthBody struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

type controlledAuthContext struct {
	context.Context
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func newControlledAuthContext() (*controlledAuthContext, func(error)) {
	c := &controlledAuthContext{Context: context.Background(), done: make(chan struct{})}
	return c, func(err error) { c.mu.Lock(); c.err = err; close(c.done); c.mu.Unlock() }
}

func (c *controlledAuthContext) Done() <-chan struct{} { return c.done }
func (c *controlledAuthContext) Err() error            { c.mu.Lock(); defer c.mu.Unlock(); return c.err }

func (b *blockingAuthBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.closed
	return 0, errors.New("body closed")
}

func (b *blockingAuthBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func authConnector(t *testing.T, entropy io.Reader) (*Connector, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	c := NewConnectorWithOptions(Options{Clock: func() time.Time { return now }, Entropy: entropy})
	data := `{"profile":"` + profile + `","oauth":{"authorization_endpoint":"https://auth.synthetic.invalid/oauth/authorize","token_endpoint":"https://token.synthetic.invalid/oauth/token","redirect_uri":"http://127.0.0.1:9999/callback","client_id":"synthetic-client","scope":"synthetic:offline"}}`
	if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(data)}); err != nil {
		t.Fatal(err)
	}
	return c, now
}

func TestAuthStartPKCEAndProtectedContinuation(t *testing.T) {
	entropy := strings.NewReader(strings.Repeat("x", 32) + strings.Repeat("y", 32))
	c, _ := authConnector(t, entropy)
	result, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "start"}, core.InvocationServices{})
	if ge != nil || !result.Supported || result.NextAction != "continue" || result.UserAction == nil || result.UserAction.UserCode != "" || result.UserAction.PollInterval != 0 {
		t.Fatalf("start = (%+v,%+v)", result, ge)
	}
	parsed, err := url.Parse(result.UserAction.VerificationURI)
	if err != nil || parsed.Scheme != "https" {
		t.Fatalf("authorization URL invalid: %v", err)
	}
	q := parsed.Query()
	verifier := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	challenge := sha256.Sum256([]byte(verifier))
	if q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(challenge[:]) || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" {
		t.Fatalf("invalid PKCE authorization params: %v", q)
	}
	var continuation oauthContinuation
	if json.Unmarshal([]byte(result.State), &continuation) != nil || strings.Contains(result.UserAction.VerificationURI, verifier) || continuation.Verifier != verifier {
		t.Fatalf("verifier presentation/state separation failed: got=%q want=%q action=%q", continuation.Verifier, verifier, result.UserAction.VerificationURI)
	}
}

func TestAuthConfigurationCannotTargetLiveHosts(t *testing.T) {
	c := NewConnector()
	data := `{"profile":"` + profile + `","oauth":{"authorization_endpoint":"https://auth.synthetic.invalid/oauth/authorize","token_endpoint":"https://api.anthropic.com/v1/oauth/token","redirect_uri":"http://127.0.0.1:9999/callback","client_id":"synthetic","scope":"synthetic"}}`
	if err := c.Init(context.Background(), core.ComponentConfig{Data: []byte(data)}); err == nil {
		t.Fatal("live OAuth endpoint accepted")
	}
}

func TestAuthCallbackComparesRawRedirectComponents(t *testing.T) {
	for _, callback := range []string{
		"HTTP://example.test/callback?code=c&state=s",
		"http://EXAMPLE.test/callback?code=c&state=s",
		"http://example.test/%63allback?code=c&state=s",
	} {
		if _, _, ok := parseCallback(callback, "http://example.test/callback"); ok {
			t.Errorf("accepted raw redirect mismatch %q", callback)
		}
	}
}

func TestAuthContinueStrictCallbackExchange(t *testing.T) {
	c, now := authConnector(t, bytes.NewReader(bytes.Repeat([]byte("ab"), 4096)))
	start, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "start"}, core.InvocationServices{})
	if ge != nil {
		t.Fatal(ge)
	}
	var continuation oauthContinuation
	if err := json.Unmarshal([]byte(start.State), &continuation); err != nil {
		t.Fatal(err)
	}
	state := continuation.ExpectedState
	doer := fakeAuthDoer(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://token.synthetic.invalid/oauth/token" {
			t.Fatalf("endpoint %s", req.URL)
		}
		b, _ := io.ReadAll(req.Body)
		form, err := url.ParseQuery(string(b))
		if err != nil {
			t.Fatal(err)
		}
		if form.Get("grant_type") != "authorization_code" || form.Get("code") != "code+value" || form.Get("redirect_uri") != "http://127.0.0.1:9999/callback" || form.Get("code_verifier") == "" {
			t.Fatalf("exchange form %v", form)
		}
		fixture, err := os.ReadFile("testdata/oauth-exchange-response.json")
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(fixture))), Header: make(http.Header)}, nil
	})
	input := "http://127.0.0.1:9999/callback?code=code%2Bvalue&state=" + state
	result, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: []byte(start.State), CallbackInput: input}, core.InvocationServices{Transport: doer})
	if ge != nil || !result.Supported || result.CredentialExpiresAt == nil || !result.CredentialExpiresAt.Equal(now.Add(time.Hour)) || len(result.Credentials["oauth"]) == 0 {
		t.Fatalf("continue=(%+v,%+v)", result, ge)
	}
}

func TestAuthRejectsInvalidCallbacksBeforeDispatch(t *testing.T) {
	bad := []string{
		"http://127.0.0.1:9999/callback?code=x",
		"http://127.0.0.1:9999/callback?code=x&state=wrong",
		"http://127.0.0.1:9999/callback?code=x&code=y&state=STATE_PLACEHOLDER",
		"http://127.0.0.1:9999/callback?code=x&state=STATE_PLACEHOLDER&error=denied",
		"http://127.0.0.1:9999/callback?code=%zz&state=STATE_PLACEHOLDER",
		"http://127.0.0.1:9999/callback?code=x&state=STATE_PLACEHOLDER#",
		"http://127.0.0.1:9999/callback/?code=x&state=STATE_PLACEHOLDER",
		"http://127.0.0.1:9999:9999/callback?code=x&state=STATE_PLACEHOLDER",
		"http://127.0.0.1:9999/callback?code=x&state=STATE_PLACEHOLDER&unexpected=1",
	}
	for i, input := range bad {
		c, _ := authConnector(t, bytes.NewReader(bytes.Repeat([]byte{byte(i + 1)}, 64)))
		start, _ := c.Authenticate(context.Background(), core.AuthRequest{Action: "start"}, core.InvocationServices{})
		var continuation oauthContinuation
		if err := json.Unmarshal([]byte(start.State), &continuation); err != nil {
			t.Fatal(err)
		}
		state := continuation.ExpectedState
		input = strings.ReplaceAll(input, "STATE_PLACEHOLDER", state)
		called := false
		doer := fakeAuthDoer(func(*http.Request) (*http.Response, error) { called = true; return nil, nil })
		_, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "continue", State: []byte(start.State), CallbackInput: input}, core.InvocationServices{Transport: doer})
		if ge == nil || called {
			t.Errorf("case %d error=%+v dispatched=%v", i, ge, called)
		}
		if ge != nil && strings.Contains(ge.Message, input) {
			t.Errorf("case %d reflected callback input", i)
		}
	}
}

func TestAuthExchangeFailureAndCancellationAreRedactedAndNeverRetried(t *testing.T) {
	for _, failAfterCancel := range []bool{false, true} {
		c, _ := authConnector(t, bytes.NewReader(bytes.Repeat([]byte("xy"), 4096)))
		start, _ := c.Authenticate(context.Background(), core.AuthRequest{Action: "start"}, core.InvocationServices{})
		var continuation oauthContinuation
		if err := json.Unmarshal([]byte(start.State), &continuation); err != nil {
			t.Fatal(err)
		}
		input := "http://127.0.0.1:9999/callback?code=private-code&state=" + continuation.ExpectedState
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		doer := fakeAuthDoer(func(*http.Request) (*http.Response, error) {
			calls++
			if failAfterCancel {
				cancel()
				return nil, context.Canceled
			}
			return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(`{"error":"private-provider-detail","access_token":"must-not-leak"}`)), Header: make(http.Header)}, nil
		})
		_, ge := c.Authenticate(ctx, core.AuthRequest{Action: "continue", State: []byte(start.State), CallbackInput: input}, core.InvocationServices{Transport: doer})
		cancel()
		wantCategory := core.CategoryUnavailable
		wantCode := "auth_provider_unavailable"
		if failAfterCancel {
			wantCategory, wantCode = core.CategoryCancelled, "auth_cancelled"
		}
		if ge == nil || calls != 1 || ge.Category != wantCategory || ge.Code != wantCode || strings.Contains(ge.Message, "private") || strings.Contains(ge.Message, "private-code") {
			t.Fatalf("cancel=%v error=%+v calls=%d", failAfterCancel, ge, calls)
		}
	}
}

func TestAuthFailureCategoriesAndNoRetry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response *http.Response
		err      error
		cancel   bool
		code     string
		category core.ErrorCategory
	}{
		{name: "transport", err: errors.New("private transport detail"), code: "auth_transport_failed", category: core.CategoryUnavailable},
		{name: "timeout", err: context.DeadlineExceeded, code: "auth_timeout", category: core.CategoryTimeout},
		{name: "cancel", cancel: true, code: "auth_cancelled", category: core.CategoryCancelled},
		{name: "unauthorized", response: &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"error":"private provider detail"}`)), Header: make(http.Header)}, code: "auth_provider_rejected", category: core.CategoryUnauthenticated},
		{name: "provider unavailable", response: &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("private")), Header: make(http.Header)}, code: "auth_provider_unavailable", category: core.CategoryUnavailable},
		{name: "malformed response", response: &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("private malformed response")), Header: make(http.Header)}, code: "auth_invalid_response", category: core.CategoryUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := authConnector(t, bytes.NewReader(bytes.Repeat([]byte("xy"), 4096)))
			start, _ := c.Authenticate(context.Background(), core.AuthRequest{Action: "start"}, core.InvocationServices{})
			var continuation oauthContinuation
			if err := json.Unmarshal([]byte(start.State), &continuation); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			doer := fakeAuthDoer(func(*http.Request) (*http.Response, error) {
				calls++
				if tc.cancel {
					cancel()
					return nil, context.Canceled
				}
				return tc.response, tc.err
			})
			callback := "http://127.0.0.1:9999/callback?code=synthetic&state=" + continuation.ExpectedState
			_, ge := c.Authenticate(ctx, core.AuthRequest{Action: "continue", State: []byte(start.State), CallbackInput: callback}, core.InvocationServices{Transport: doer})
			if ge == nil || ge.Code != tc.code || ge.Category != tc.category || calls != 1 || strings.Contains(ge.Message, "private") {
				t.Fatalf("error=%+v calls=%d", ge, calls)
			}
		})
	}
}

func TestAuthEntropyFailureIsInternalAndRedacted(t *testing.T) {
	c, _ := authConnector(t, strings.NewReader(""))
	_, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "start"}, core.InvocationServices{})
	if ge == nil || ge.Code != "auth_entropy_failed" || ge.Category != core.CategoryInternal || strings.Contains(ge.Message, "entropy") {
		t.Fatalf("entropy error=%+v", ge)
	}
}

func TestAuthCredentialReadCancellationAndDeadline(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[timeout], func(t *testing.T) {
			c, _ := authConnector(t, bytes.NewReader(bytes.Repeat([]byte("xy"), 4096)))
			ctx, interrupt := newControlledAuthContext()
			entered := make(chan struct{})
			done := make(chan *core.GatewayError, 1)
			go func() {
				_, ge := c.Authenticate(ctx, core.AuthRequest{Action: "refresh"}, core.InvocationServices{Credentials: blockingCredentialAccess{entered: entered}, Transport: fakeAuthDoer(func(*http.Request) (*http.Response, error) {
					t.Error("transport called during credential read")
					return nil, nil
				})})
				done <- ge
			}()
			<-entered
			err := error(context.Canceled)
			if timeout {
				err = context.DeadlineExceeded
			}
			interrupt(err)
			ge := <-done
			wantCode, wantCategory := "auth_cancelled", core.CategoryCancelled
			if timeout {
				wantCode, wantCategory = "auth_timeout", core.CategoryTimeout
			}
			if ge == nil || ge.Code != wantCode || ge.Category != wantCategory {
				t.Fatalf("credential read error=%+v, want %s/%s", ge, wantCode, wantCategory)
			}
		})
	}
}

func TestAuthResponseBodyCancellationAndDeadlineCloseBody(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[timeout], func(t *testing.T) {
			c, _ := authConnector(t, bytes.NewReader(bytes.Repeat([]byte("xy"), 4096)))
			start, _ := c.Authenticate(context.Background(), core.AuthRequest{Action: "start"}, core.InvocationServices{})
			var continuation oauthContinuation
			if err := json.Unmarshal([]byte(start.State), &continuation); err != nil {
				t.Fatal(err)
			}
			ctx, interrupt := newControlledAuthContext()
			body := &blockingAuthBody{started: make(chan struct{}), closed: make(chan struct{})}
			calls := 0
			doer := fakeAuthDoer(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
			})
			done := make(chan *core.GatewayError, 1)
			callback := "http://127.0.0.1:9999/callback?code=synthetic&state=" + continuation.ExpectedState
			go func() {
				_, ge := c.Authenticate(ctx, core.AuthRequest{Action: "continue", State: []byte(start.State), CallbackInput: callback}, core.InvocationServices{Transport: doer})
				done <- ge
			}()
			<-body.started
			err := error(context.Canceled)
			if timeout {
				err = context.DeadlineExceeded
			}
			interrupt(err)
			ge := <-done
			wantCode, wantCategory := "auth_cancelled", core.CategoryCancelled
			if timeout {
				wantCode, wantCategory = "auth_timeout", core.CategoryTimeout
			}
			select {
			case <-body.closed:
			default:
				t.Fatal("response body was not closed after context ended")
			}
			if ge == nil || ge.Code != wantCode || ge.Category != wantCategory || calls != 1 || strings.Contains(ge.Message, "synthetic") {
				t.Fatalf("body read error=%+v calls=%d", ge, calls)
			}
		})
	}
}

func TestAuthRefreshReturnsCompleteReplacementAndNoAmbiguousRetry(t *testing.T) {
	c, now := authConnector(t, strings.NewReader(strings.Repeat("x", 64)))
	prior := []byte(`{"access_token":"old","refresh_token":"old-refresh","expires_at":"2026-10-16T10:00:00Z"}`)
	calls := 0
	doer := fakeAuthDoer(func(req *http.Request) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(req.Body)
		form, _ := url.ParseQuery(string(body))
		if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "old-refresh" {
			t.Fatalf("refresh request %v", form)
		}
		fixture, err := os.ReadFile("testdata/oauth-refresh-response.json")
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(fixture))), Header: make(http.Header)}, nil
	})
	result, ge := c.Authenticate(context.Background(), core.AuthRequest{Action: "refresh"}, core.InvocationServices{Transport: doer, Credentials: fakeCredentialAccess(prior)})
	if ge != nil || !result.Supported || result.CredentialExpiresAt == nil || !result.CredentialExpiresAt.Equal(now.Add(time.Hour)) || calls != 1 {
		t.Fatalf("refresh=(%+v,%+v), calls=%d", result, ge, calls)
	}
	var replacement oauthCredentials
	if err := json.Unmarshal(result.Credentials["oauth"], &replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.AccessToken != "SYNTHETIC_ROTATED_ACCESS_TOKEN" || replacement.RefreshToken != "SYNTHETIC_ROTATED_REFRESH_TOKEN" {
		t.Fatalf("incomplete replacement: %+v", replacement)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, ge = c.Authenticate(cancelled, core.AuthRequest{Action: "refresh"}, core.InvocationServices{Transport: doer, Credentials: fakeCredentialAccess(prior)})
	if ge == nil || calls != 1 {
		t.Fatalf("cancelled refresh=%+v calls=%d", ge, calls)
	}
}
