//go:build linux

package external

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

type authCredentials struct{ account string }

func (c authCredentials) Get(_ context.Context, name string) ([]byte, error) {
	if name != "token" || c.account != "account" {
		return nil, context.Canceled
	}
	return []byte{0, 0xff, 'r'}, nil
}

type authTransport struct{}

func (authTransport) Do(*http.Request) (*http.Response, error) { return nil, context.Canceled }

func authServices() core.InvocationServices {
	return core.InvocationServices{Credentials: authCredentials{account: "account"}, Transport: authTransport{}, Logger: slog.Default()}
}

func TestM6AuthenticateRoundTripsOpaqueContinuationAndCandidates(t *testing.T) {
	tests := []struct {
		mode  string
		req   core.AuthRequest
		check func(*testing.T, core.AuthResult)
	}{
		{"auth-start", core.AuthRequest{AccountID: "account", Action: "start"}, func(t *testing.T, got core.AuthResult) {
			if !got.Supported || got.NextAction != "continue" || got.UserAction == nil || got.UserAction.VerificationURI != "https://example.test/device" || got.UserAction.UserCode != "ABC-123" || got.UserAction.PollInterval != 5*time.Second {
				t.Fatalf("start result=%+v", got)
			}
			want := append([]byte{0, 0xff, 's'}, make([]byte, 70<<10)...)
			if !bytes.Equal([]byte(got.State), want) {
				t.Fatal("opaque continuation state changed")
			}
		}},
		{"auth-continue", core.AuthRequest{AccountID: "account", Action: "continue", State: append([]byte{0, 0xff}, make([]byte, 90<<10)...), CallbackInput: "https://example.test/callback?code=synthetic"}, func(t *testing.T, got core.AuthResult) {
			want := append([]byte{0, 0xff}, make([]byte, 70<<10)...)
			if !got.Supported || got.NextAction != "" || !bytes.Equal(got.Credentials["access"], want) || got.CredentialExpiresAt == nil || !got.CredentialExpiresAt.Equal(time.Date(2030, 2, 3, 3, 5, 6, 7, time.UTC)) {
				t.Fatalf("continue candidate result did not round-trip: supported=%v next=%q keys=%v expiry=%v", got.Supported, got.NextAction, mapKeys(got.Credentials), got.CredentialExpiresAt)
			}
		}},
		{"auth-refresh", core.AuthRequest{AccountID: "account", Action: "refresh"}, func(t *testing.T, got core.AuthResult) {
			if !reflect.DeepEqual(got.Credentials, map[string][]byte{"refresh": {0, 0xff, 'r'}}) || got.CredentialExpiresAt == nil || !got.CredentialExpiresAt.Equal(time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)) {
				t.Fatalf("refresh candidate result=%+v", got)
			}
		}},
		{"auth-unsupported", core.AuthRequest{AccountID: "account", Action: "start"}, func(t *testing.T, got core.AuthResult) {
			if got.Supported || got.Credentials != nil || got.State != "" {
				t.Fatalf("unsupported result=%+v", got)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.mode, func(t *testing.T) {
			p := startProcess(t, tc.mode, startupTimeout)
			c, err := p.ConnectComponent(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
				t.Fatal(err)
			}
			got, gatewayErr := c.Authenticate(context.Background(), tc.req, authServices())
			if gatewayErr != nil {
				t.Fatalf("Authenticate error: %+v", gatewayErr)
			}
			tc.check(t, got)
			if tc.mode == "auth-refresh" {
				waitForDiagnostic(t, p, "auth-complete-callback-denied")
				if strings.Contains(p.Diagnostics(), "auth-complete-callback-granted") || strings.Contains(p.Diagnostics(), "0xff") {
					t.Fatal("post-completion callback granted or secret appeared in diagnostics")
				}
			}
			stopTestProcess(t, p)
		})
	}
}

func TestM6AuthenticateRejectsInvalidAndUnavailable(t *testing.T) {
	p := startProcess(t, "lifecycle-success", startupTimeout)
	c, err := p.ConnectComponent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []core.AuthRequest{{AccountID: "account", Action: "bogus"}, {Action: "start"}, {AccountID: "account", Action: "start", CallbackInput: "secret"}, {AccountID: "account", Action: "refresh", CallbackInput: "secret"}} {
		if _, gatewayErr := c.Authenticate(context.Background(), req, core.InvocationServices{}); gatewayErr == nil || gatewayErr.Category != core.CategoryInvalidRequest {
			t.Fatalf("invalid request was not rejected: %+v", gatewayErr)
		}
	}
	if strings.Contains(p.Diagnostics(), "lifecycle-authenticate-called") {
		t.Fatal("invalid authentication request reached child")
	}
	if _, gatewayErr := c.Authenticate(context.Background(), core.AuthRequest{AccountID: "account", Action: "start"}, core.InvocationServices{}); gatewayErr == nil {
		t.Fatal("uninitialized connector accepted Authenticate")
	}
	c.mu.Lock()
	c.initialized, c.closing = true, true
	c.mu.Unlock()
	if _, gatewayErr := c.Authenticate(context.Background(), core.AuthRequest{AccountID: "account", Action: "start"}, core.InvocationServices{}); gatewayErr == nil {
		t.Fatal("closing connector accepted Authenticate")
	}
	if strings.Contains(p.Diagnostics(), "lifecycle-authenticate-called") {
		t.Fatal("unavailable authentication request reached child")
	}
	stopTestProcess(t, p)
}

func TestM6AuthenticateTimeoutAndMalformedResultFailClosed(t *testing.T) {
	for _, mode := range []string{"auth-stall", "auth-malformed"} {
		t.Run(mode, func(t *testing.T) {
			p := startProcess(t, mode, startupTimeout)
			c, err := p.ConnectComponent(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			cancel := func() {}
			if mode == "auth-stall" {
				ctx, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
			}
			defer cancel()
			got, gatewayErr := c.Authenticate(ctx, core.AuthRequest{AccountID: "account", Action: "start"}, core.InvocationServices{})
			if gatewayErr == nil || got.Supported || got.Credentials != nil || got.State != "" {
				t.Fatalf("invalid auth result not failed closed: result=%+v err=%+v", got, gatewayErr)
			}
			stopTestProcess(t, p)
		})
	}
}

func TestM6AuthenticateScopedServicesRevokedOnCancel(t *testing.T) {
	p := startProcess(t, "auth-stall", startupTimeout)
	c, err := p.ConnectComponent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *core.GatewayError, 1)
	services := authServices()
	go func() {
		_, gatewayErr := c.Authenticate(ctx, core.AuthRequest{AccountID: "account", Action: "refresh"}, services)
		done <- gatewayErr
	}()
	waitForDiagnostic(t, p, "lifecycle-authenticate-called")
	c.invocations.mu.Lock()
	if len(c.invocations.active) == 0 || len(c.invocations.active) > maxInvocations {
		c.invocations.mu.Unlock()
		t.Fatalf("authentication invocation admission count out of range: %d", len(c.invocations.active))
	}
	for _, invocation := range c.invocations.active {
		if invocation.accountID != "account" || invocation.credentials != services.Credentials || invocation.transport != services.Transport || invocation.logger != services.Logger {
			c.invocations.mu.Unlock()
			t.Fatal("auth invocation lost account-scoped services")
		}
	}
	c.invocations.mu.Unlock()
	cancel()
	if gatewayErr := <-done; gatewayErr == nil {
		t.Fatal("cancelled authentication unexpectedly succeeded")
	}
	waitForDiagnostic(t, p, "auth-cancel-callback-denied")
	if strings.Contains(p.Diagnostics(), "auth-cancel-callback-granted") || strings.Contains(p.Diagnostics(), "0xff") {
		t.Fatal("post-cancellation callback granted or secret appeared in diagnostics")
	}
	c.invocations.mu.Lock()
	active := len(c.invocations.active)
	c.invocations.mu.Unlock()
	if active != 0 {
		t.Fatalf("cancelled authentication invocation remains active: %d", active)
	}
	stopTestProcess(t, p)
}

func mapKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
