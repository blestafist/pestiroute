package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

type capturedBodyReader struct {
	data []byte
	step int
	fail bool
	refs [][]byte
}

func (r *capturedBodyReader) Read(p []byte) (int, error) {
	r.refs = append(r.refs, p)
	if len(r.data) == 0 {
		if r.fail {
			return 0, errors.New("read failed")
		}
		return 0, io.EOF
	}
	n := min(len(p), len(r.data), r.step)
	copy(p[:n], r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func (r *capturedBodyReader) assertCleared(t *testing.T) {
	t.Helper()
	if len(r.refs) == 0 {
		t.Fatal("reader was not given a buffer")
	}
	for i, ref := range r.refs {
		for _, b := range ref {
			if b != 0 {
				t.Fatalf("reader buffer %d retained plaintext", i)
			}
		}
	}
}

func TestAdminBodyBuffersClearedOnAllDispatchPaths(t *testing.T) {
	for _, tc := range []struct {
		name                string
		limit               int64
		payload             string
		fail, panicDispatch bool
		wantStatus          int
	}{
		{name: "success", limit: 20000, payload: strings.Repeat("secret-chunk-", 1000), wantStatus: 204},
		{name: "read error", limit: 20000, payload: strings.Repeat("secret-chunk-", 400), fail: true, wantStatus: 400},
		{name: "oversize", limit: 32, payload: strings.Repeat("oversized-secret-", 10), wantStatus: 413},
		{name: "panic", limit: 20000, payload: "panic-secret", panicDispatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &capturedBodyReader{data: []byte(tc.payload), step: 1024, fail: tc.fail}
			r := httptest.NewRequest(http.MethodPost, "/admin/v1/status", nil)
			r.Body = io.NopCloser(reader)
			w := httptest.NewRecorder()
			api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.panicDispatch {
					panic("synthetic dispatch panic")
				}
				w.WriteHeader(204)
			})
			func() {
				defer func() { _ = recover() }()
				dispatchBody(w, r, api, tc.limit)
			}()
			reader.assertCleared(t)
			if tc.wantStatus != 0 && w.Code != tc.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
		})
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
	if got := call("/admin/v1/status", "https://ui.example", true); got != 200 {
		t.Fatalf("configured origin status=%d", got)
	}
	if got := call("/admin/v1/status", "http://127.0.0.1:9091", true); got != 200 {
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

func TestStaticMountWithoutHandlerReturnsNotFound(t *testing.T) {
	t.Setenv("PESTIROUTE_ADMIN_TEST_TOKEN", "static-test-token")
	h, err := New(Config{Address: "127.0.0.1:9091", TokenEnv: "PESTIROUTE_ADMIN_TEST_TOKEN", ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	r.Host = "127.0.0.1:9091"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unconfigured static mount status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestStatusSnapshotAndUsageQueryValidation(t *testing.T) {
	t.Setenv("PESTIROUTE_ADMIN_TEST_TOKEN", "secret")
	h, err := New(Config{Address: "127.0.0.1:9092", TokenEnv: "PESTIROUTE_ADMIN_TEST_TOKEN", ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second,
		Version: "test-version", Readiness: "unknown", Components: []Component{{ID: "connector", Kind: "connector", Implementation: "impl", Health: "unknown", Capabilities: map[string]string{}}},
		Routes: []Route{{Protocol: "p", Mode: "native", Model: "m", AdapterID: "a", ConnectorID: "c", AccountID: "acct"}}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/v1/status", nil)
	r.Host = "127.0.0.1:9092"
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"readiness":"unknown"`) || !strings.Contains(w.Body.String(), `"adapter_id":"a"`) || !strings.Contains(w.Body.String(), `"capabilities":{}`) || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	for _, target := range []string{"/admin/v1/usage/requests?limit=501", "/admin/v1/usage/attempts?since=bad", "/admin/v1/usage/summary?offset=0", "/admin/v1/usage/requests?unexpected=", "/admin/v1/usage/requests?limit=", "/admin/v1/usage/requests?since=", "/admin/v1/usage/requests?key_id=", "/admin/v1/usage/requests?key_id=%zz", "/admin/v1/usage/requests?key_id=a;b"} {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r.Host = "127.0.0.1:9092"
		r.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"invalid_request"`) {
			t.Fatalf("%s status=%d body=%s", target, w.Code, w.Body.String())
		}
	}
}
