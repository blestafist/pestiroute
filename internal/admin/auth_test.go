package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

type fakeAuthCoordinator struct {
	starts, continues, refreshes int
	callback                     string
	startErr                     error
}

func (f *fakeAuthCoordinator) Start(context.Context, string) (core.AuthSession, error) {
	f.starts++
	if f.startErr != nil {
		return core.AuthSession{}, f.startErr
	}
	return core.AuthSession{ID: "opaque", ExpiresAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), UserAction: &core.AuthUserAction{VerificationURI: "https://example.test/auth", UserCode: "safe-code", PollInterval: 5 * time.Second}}, nil
}
func (f *fakeAuthCoordinator) Continue(_ context.Context, id, input string) (core.AuthSession, error) {
	f.continues++
	f.callback = input
	if id != "opaque" {
		return core.AuthSession{}, core.ErrAuthUnavailable
	}
	return core.AuthSession{}, nil
}
func (f *fakeAuthCoordinator) Refresh(context.Context, string) (core.AuthCredentials, error) {
	f.refreshes++
	return core.AuthCredentials{Revision: 2, Values: map[string][]byte{"token": []byte("secret-token")}}, nil
}

func TestAuthHTTPBridgeRedactionAndBounds(t *testing.T) {
	t.Setenv("PESTIROUTE_ADMIN_TEST_TOKEN", "operator-secret")
	f := &fakeAuthCoordinator{}
	h, err := New(Config{Address: "127.0.0.1:9095", TokenEnv: "PESTIROUTE_ADMIN_TEST_TOKEN", ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second, Auth: f})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = "127.0.0.1:9095"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer operator-secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := call("POST", "/admin/v1/auth/sessions", `{"account_id":"acct"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"session_id":"opaque"`) || strings.Contains(w.Body.String(), "State") {
		t.Fatalf("start response: %d %s", w.Code, w.Body.String())
	}
	w = call("POST", "/admin/v1/auth/sessions/opaque/continue", `{"callback_input":"private-callback"}`)
	if w.Code != 200 || f.callback != "private-callback" || strings.Contains(w.Body.String(), "private-callback") {
		t.Fatalf("continue response: %d %s", w.Code, w.Body.String())
	}
	w = call("POST", "/admin/v1/auth/sessions/opaque/continue", `{"callback_input":"" ,"unknown":true}`)
	if w.Code != 400 || f.continues != 1 {
		t.Fatalf("strict decode: %d calls=%d", w.Code, f.continues)
	}
	w = call("POST", "/admin/v1/auth/sessions/opaque/continue", `{"callback_input":"`+strings.Repeat("x", 4097)+`"}`)
	if w.Code != 400 || f.continues != 1 {
		t.Fatalf("callback bound: %d calls=%d", w.Code, f.continues)
	}
	w = call("POST", "/admin/v1/auth/accounts/acct/refresh", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "secret-token") || f.refreshes != 1 {
		t.Fatalf("refresh response: %d %s", w.Code, w.Body.String())
	}
	f.startErr = core.ErrAuthUnavailable
	w = call("POST", "/admin/v1/auth/sessions", `{"account_id":"acct"}`)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"code":"unavailable"`) {
		t.Fatalf("unsupported start response: %d %s", w.Code, w.Body.String())
	}
	w = call("POST", "/admin/v1/auth/sessions/stale/continue", `{"callback_input":""}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"revision_conflict"`) {
		t.Fatalf("stale continue response: %d %s", w.Code, w.Body.String())
	}
}
