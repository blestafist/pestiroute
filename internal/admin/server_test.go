package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMiddlewareAndIsolation(t *testing.T) {
	t.Setenv("PESTIROUTE_ADMIN_TEST_TOKEN", "operator-secret")
	h, err := New(Config{Address: "127.0.0.1:9090", TokenEnv: "PESTIROUTE_ADMIN_TEST_TOKEN", AllowedHosts: []string{"proxy.example"}, MaxBodyBytes: 4, ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	request := func(path, auth, host, origin string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		r.Host = host
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		path, auth, host, origin string
		status                   int
	}{
		{"/admin/v1/status", "", "127.0.0.1:9090", "", 401},
		{"/admin/v1/status", "operator-secret", "127.0.0.1:9090", "", 401},
		{"/admin/v1/status", "Bearer inference-key", "127.0.0.1:9090", "", 401},
		{"/admin/v1/status", "Bearer  operator-secret", "127.0.0.1:9090", "", 401},
		{"/admin/v1/status", "Bearer operator-secret", "evil.example", "", 403},
		{"/admin/v1", "", "evil.example", "", 403},
		{"/admin/v1", "", "127.0.0.1:9090", "", 401},
		{"/admin/v1/status", "Bearer operator-secret", "proxy.example", "", 404},
		{"/admin/v1/status", "Bearer operator-secret", "127.0.0.1:9090", "null", 403},
		{"/admin/v1/status", "Bearer operator-secret", "127.0.0.1:9090", "", 404},
		{"/v1/responses", "Bearer operator-secret", "127.0.0.1:9090", "", 404},
	} {
		if got := request(tc.path, tc.auth, tc.host, tc.origin, nil).Code; got != tc.status {
			t.Errorf("%s: got %d want %d", tc.path, got, tc.status)
		}
	}
	unauthorized := request("/admin/v1/status", "", "127.0.0.1:9090", "", nil)
	var envelope struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(unauthorized.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != "unauthorized" || envelope.Error.RequestID == "" {
		t.Fatalf("invalid unauthorized envelope: %s (%v)", unauthorized.Body, err)
	}
	duplicate := httptest.NewRequest(http.MethodGet, "/admin/v1/status", nil)
	duplicate.Host = "127.0.0.1:9090"
	duplicate.Header.Add("Authorization", "Bearer operator-secret")
	duplicate.Header.Add("Authorization", "Bearer operator-secret")
	duplicateResult := httptest.NewRecorder()
	h.ServeHTTP(duplicateResult, duplicate)
	if duplicateResult.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate Authorization accepted: %d", duplicateResult.Code)
	}
	if got := request("/admin/v1/status", "Bearer operator-secret", "127.0.0.1:9090", "", []byte("12345")).Code; got != 413 {
		t.Fatalf("oversize body: got %d want 413", got)
	}
}

func TestExposureValidation(t *testing.T) {
	t.Setenv("PESTIROUTE_ADMIN_TEST_TOKEN", "operator-secret")
	base := Config{Address: "0.0.0.0:9090", TokenEnv: "PESTIROUTE_ADMIN_TEST_TOKEN", ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second}
	if _, err := New(base); err == nil {
		t.Fatal("public bind without TLS accepted")
	}
	for _, token := range []string{"operator secret", "operator\tsecret", "operator\nsecret", "bad\x01token", "bad\x7ftoken", "tökén", "a=b", "=abc"} {
		t.Setenv("PESTIROUTE_ADMIN_TEST_TOKEN", token)
		if _, err := New(Config{Address: "127.0.0.1:9090", TokenEnv: "PESTIROUTE_ADMIN_TEST_TOKEN", ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second}); err == nil {
			t.Errorf("invalid token %q accepted", token)
		}
	}
}

func TestBearerTokenGrammar(t *testing.T) {
	for _, token := range []string{"abc", "operator-secret", "A0-._~+/", "encoded=="} {
		if got := bearerToken("Bearer " + token); got != token {
			t.Errorf("valid token %q parsed as %q", token, got)
		}
	}
	for _, token := range []string{"", "=", "===", "bad\x01token", "bad\x7ftoken", "tökén", "a=b", "=abc", "abc=def", "bad token"} {
		if got := bearerToken("Bearer " + token); got != "" {
			t.Errorf("invalid token %q parsed as %q", token, got)
		}
	}
}

func TestTokenFileOriginAndStaticSeam(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("file-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	static := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	h, err := New(Config{Address: "127.0.0.1:9091", TokenFile: tokenPath, AllowedOrigins: []string{"https://ui.example"}, MaxBodyBytes: 64, ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second, Static: static})
	if err != nil {
		t.Fatal(err)
	}
	call := func(path, origin string, api bool) int {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Host = "127.0.0.1:9091"
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if api {
			r.Header.Set("Authorization", "Bearer file-secret")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := call("/admin/v1/status", "https://ui.example", true); got != 404 {
		t.Fatalf("configured origin status=%d", got)
	}
	if got := call("/admin/v1/status", "http://127.0.0.1:9091", true); got != 404 {
		t.Fatalf("same origin status=%d", got)
	}
	if got := call("/admin/v1/status", "https://evil.example", true); got != 403 {
		t.Fatalf("unapproved origin status=%d", got)
	}
	if got := call("/admin/app.js", "", false); got != 204 {
		t.Fatalf("static status=%d", got)
	}
	preflight := httptest.NewRequest(http.MethodOptions, "/admin/v1/status", nil)
	preflight.Host = "127.0.0.1:9091"
	preflight.Header.Set("Origin", "https://ui.example")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	preflightResult := httptest.NewRecorder()
	h.ServeHTTP(preflightResult, preflight)
	if preflightResult.Code != 204 || preflightResult.Header().Get("Access-Control-Allow-Origin") != "https://ui.example" {
		t.Fatalf("configured origin preflight: status=%d headers=%v", preflightResult.Code, preflightResult.Header())
	}
	if got := call("/admin/v1/private", "", false); got != 401 {
		t.Fatalf("API fell through to static: status=%d", got)
	}
	if err := os.Chmod(tokenPath, 0660); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Address: "127.0.0.1:9091", TokenFile: tokenPath, ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second}); err == nil {
		t.Fatal("writable token file accepted")
	}
	if err := os.Chmod(tokenPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tokenPath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Address: "127.0.0.1:9091", TokenFile: tokenPath, ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second}); err == nil {
		t.Fatal("group/world-readable token file accepted")
	}
	if err := os.Chmod(tokenPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Address: "127.0.0.1:9091", TokenFile: tokenPath, ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second}); err == nil {
		t.Fatal("empty token file accepted")
	}
	for _, token := range []string{"file secret\n", "bad\x01token\n", "bad\x7ftoken\n", "tökén\n", "a=b\n"} {
		if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Config{Address: "127.0.0.1:9091", TokenFile: tokenPath, ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second}); err == nil {
			t.Errorf("invalid token file value %q accepted", token)
		}
	}
}
