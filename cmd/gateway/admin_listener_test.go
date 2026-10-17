package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func freeAdminTCPAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func startAdminGateway(t *testing.T, shutdownTimeout string) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	t.Setenv("PESTIROUTE_ADMIN_TEST_TOKEN", "operator-secret")
	configPath := filepath.Join(t.TempDir(), "gateway.json")
	cfg := map[string]any{"listen": freeAdminTCPAddress(t), "shutdown_timeout": shutdownTimeout, "admin": map[string]any{
		"listen": freeAdminTCPAddress(t), "token_env": "PESTIROUTE_ADMIN_TEST_TOKEN", "read_timeout": "5s", "write_timeout": "5s", "idle_timeout": "5s",
	}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := loadConfig([]string{"-config", configPath})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config", configPath}) }()
	deadline := time.Now().Add(5 * time.Second)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + loaded.Admin.Listen + "/admin/v1/status")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				return loaded.Admin.Listen, cancel, done
			}
		}
		select {
		case err := <-done:
			cancel()
			t.Fatalf("gateway exited before ready: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	t.Fatal("admin listener did not become ready")
	return "", nil, nil
}

func beginAdminInflightRequest(t *testing.T, address string) (net.Conn, *bufio.Reader, *http.Request) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, "http://"+address+"/admin/v1/status", nil)
	_, err = fmt.Fprintf(conn, "POST /admin/v1/status HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer operator-secret\r\nExpect: 100-continue\r\nTransfer-Encoding: chunked\r\n\r\n", address)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		conn.Close()
		t.Fatalf("wait for admin handler to read body: %v", err)
	}
	if response.StatusCode != http.StatusContinue {
		response.Body.Close()
		conn.Close()
		t.Fatalf("interim response = %d, want 100 Continue", response.StatusCode)
	}
	if _, err := fmt.Fprint(conn, "1\r\nx\r\n"); err != nil {
		conn.Close()
		t.Fatalf("send chunked body: %v", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn, reader, request
}

func assertAdminListenerClosed(t *testing.T, address string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("admin listener socket leaked")
	}
}

func TestAdminListenerStopsDuringInferenceDrain(t *testing.T) {
	inferenceListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	adminListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		inferenceListener.Close()
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	inference := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})}
	admin := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })}
	serveDone := make(chan struct{}, 2)
	go func() { _ = inference.Serve(inferenceListener); serveDone <- struct{}{} }()
	go func() { _ = admin.Serve(adminListener); serveDone <- struct{}{} }()

	response := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + inferenceListener.Addr().String())
		if err == nil {
			resp.Body.Close()
		}
		response <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("controlled inference request did not start")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- shutdownHTTPServers([]*http.Server{inference, admin}, time.Second) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", adminListener.Addr().String(), 50*time.Millisecond)
		if err != nil {
			break
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("admin listener kept accepting while inference drained")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown finished before in-flight inference completed: %v", err)
	default:
	}
	close(release)
	if err := <-response; err != nil {
		t.Fatalf("in-flight inference response: %v", err)
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("concurrent shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("servers did not finish draining")
	}
	assertAdminListenerClosed(t, adminListener.Addr().String())
	for range 2 {
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Fatal("server listener did not close")
		}
	}
}

func TestAdminShutdownDrainsInflightRequest(t *testing.T) {
	address, cancel, done := startAdminGateway(t, "2s")
	conn, reader, request := beginAdminInflightRequest(t, address)
	defer conn.Close()
	cancel()
	select {
	case err := <-done:
		t.Fatalf("shutdown did not wait for in-flight request: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := fmt.Fprint(conn, "0\r\n\r\n"); err != nil {
		t.Fatalf("finish in-flight body: %v", err)
	}
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		t.Fatalf("in-flight response: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("in-flight response status = %d, want 404", response.StatusCode)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("graceful shutdown did not finish")
	}
	assertAdminListenerClosed(t, address)
}

func TestAdminShutdownForcesCloseAfterTimeout(t *testing.T) {
	address, cancel, done := startAdminGateway(t, "100ms")
	conn, _, _ := beginAdminInflightRequest(t, address)
	defer conn.Close()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("forced shutdown error = %v, want deadline exceeded", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown timeout did not force-close active request")
	}
	assertAdminListenerClosed(t, address)
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := bufio.NewReader(conn).ReadByte(); err == nil {
		t.Fatal("active request connection survived forced close")
	}
}

func TestAdminProtectedYAMLConfig(t *testing.T) {
	text := fmt.Sprintf(`version: 1
server: {listen: %q, max_request_bytes: 65536, shutdown_timeout: 2s}
storage: {driver: sqlite, path: /tmp/admin-test.db}
secrets: {master_key_file: /tmp/admin-test.key}
connectors:
  - id: anthropic
    kind: connector
    implementation: pestiroute.anthropic.messages
    protocols: [openai.responses.v1]
    settings: {model: claude, account_id: account, credential_id: credential}
routes:
  - id: route
    protocol: openai.responses.v1
    mode: translation
    model: claude
    adapter: pestiroute.responses.native
    policy: default
    budget: {unknown_estimate: reject}
    targets: [{connector: anthropic, account: account}]
policies: {default: policy-id}
admin:
  listen: %q
  token_env: PESTIROUTE_ADMIN_TEST_TOKEN
  read_timeout: 1s
  write_timeout: 1s
  idle_timeout: 1s
`, freeAdminTCPAddress(t), freeAdminTCPAddress(t))
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadProtectedYAML(path)
	if err != nil {
		t.Fatalf("valid admin YAML: %v", err)
	}
	if cfg.Admin == nil || cfg.Admin.TokenEnv != "PESTIROUTE_ADMIN_TEST_TOKEN" {
		t.Fatalf("admin config not loaded: %+v", cfg.Admin)
	}
}

func TestAdminListenerRunIsolationAndShutdown(t *testing.T) {
	t.Setenv("PESTIROUTE_ADMIN_TEST_TOKEN", "operator-secret")
	configPath := filepath.Join(t.TempDir(), "gateway.json")
	cfg := map[string]any{"listen": freeAdminTCPAddress(t), "shutdown_timeout": "2s", "admin": map[string]any{
		"listen": freeAdminTCPAddress(t), "token_env": "PESTIROUTE_ADMIN_TEST_TOKEN", "read_timeout": "1s", "write_timeout": "1s", "idle_timeout": "1s",
	}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := loadConfig([]string{"-config", configPath})
	if err != nil {
		t.Fatal(err)
	}
	inference := loaded.Listen
	management := loaded.Admin.Listen
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config", configPath}) }()
	client := &http.Client{Timeout: 150 * time.Millisecond}
	request := func(url, token string) (int, error) {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		return resp.StatusCode, nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if code, err := request("http://"+inference+"/healthz", ""); err == nil && code == 200 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, err := request("http://"+inference+"/admin/v1/status", "operator-secret"); err != nil || code != 404 {
		t.Fatalf("inference admin isolation: status=%d err=%v", code, err)
	}
	if code, err := request("http://"+management+"/admin/v1/status", ""); err != nil || code != 401 {
		t.Fatalf("admin auth: status=%d err=%v", code, err)
	}
	if code, err := request("http://"+management+"/admin/v1/status", "operator-secret"); err != nil || code != 200 {
		t.Fatalf("admin API isolation: status=%d err=%v", code, err)
	}
	if code, err := request("http://"+management+"/v1/responses", "operator-secret"); err != nil || code != 404 {
		t.Fatalf("admin inference isolation: status=%d err=%v", code, err)
	}
	client.CloseIdleConnections()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gateway listeners did not shut down")
	}
	if _, err := net.DialTimeout("tcp", inference, 100*time.Millisecond); err == nil {
		t.Fatal("inference socket leaked")
	}
	if _, err := net.DialTimeout("tcp", management, 100*time.Millisecond); err == nil {
		t.Fatal("admin socket leaked")
	}
}

func TestAdminRouteSnapshotRedactsLegacyCredentialEnvironment(t *testing.T) {
	route := topologyRoute{Protocol: responsesProtocol, Mode: core.ModeNative, Model: "model", Account: "UPSTREAM_TOKEN_ENV", Adapter: "adapter", Connector: "connector"}
	legacy := adminRouteSnapshot([]topologyRoute{route}, true)
	if len(legacy) != 1 || legacy[0].AccountID != "" {
		t.Fatalf("legacy route leaked environment reference: %+v", legacy)
	}
	route.Account = "account-id"
	explicit := adminRouteSnapshot([]topologyRoute{route}, false)
	if len(explicit) != 1 || explicit[0].AccountID != "account-id" {
		t.Fatalf("explicit route account identity omitted: %+v", explicit)
	}
}

func TestAdminStatusRedactsLoadedLegacyJSON(t *testing.T) {
	t.Setenv("PESTIROUTE_ADMIN_TEST_TOKEN", "operator-secret")
	t.Setenv("PESTIROUTE_LEGACY_SECRET_ENV", "opaque-upstream-secret")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "legacy.json")
	config := map[string]any{"listen": freeAdminTCPAddress(t), "shutdown_timeout": "1s", "upstream_endpoint": "http://127.0.0.1:9/v1/responses", "upstream_credential_env": "PESTIROUTE_LEGACY_SECRET_ENV", "max_request_body_bytes": 4096, "max_request_header_bytes": 4096, "connect_timeout": "1s", "tls_handshake_timeout": "1s", "response_header_timeout": "1s", "stream_idle_timeout": "2s", "admin": map[string]any{"listen": freeAdminTCPAddress(t), "token_env": "PESTIROUTE_ADMIN_TEST_TOKEN", "read_timeout": "2s", "write_timeout": "2s", "idle_timeout": "2s"}}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := loadConfig([]string{"-config", configPath})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config", configPath}) }()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := http.NewRequest(http.MethodGet, "http://"+loaded.Admin.Listen+"/admin/v1/status", nil)
		r.Header.Set("Authorization", "Bearer operator-secret")
		resp, requestErr := client.Do(r)
		if requestErr == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				cancel()
				if closeErr := <-done; closeErr != nil {
					t.Fatal(closeErr)
				}
				if bytes.Contains(body, []byte("PESTIROUTE_LEGACY_SECRET_ENV")) || bytes.Contains(body, []byte("opaque-upstream-secret")) || bytes.Contains(body, []byte("127.0.0.1:9")) {
					t.Fatalf("legacy status leaked secret/config: %s", body)
				}
				if !bytes.Contains(body, []byte(`"account_id":""`)) {
					t.Fatalf("legacy status missing redacted account field: %s", body)
				}
				return
			}
		}
		select {
		case err := <-done:
			cancel()
			t.Fatalf("legacy gateway exited before admin ready: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("legacy admin status did not become ready")
}

func TestAdminReadsM2PersistentJSONDatabase(t *testing.T) {
	t.Setenv("PESTIROUTE_ADMIN_TEST_TOKEN", "operator-secret")
	t.Setenv("PESTIROUTE_UPSTREAM_TEST", "not-used")
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "runtime.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	keyPath := filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyPath, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "gateway.json")
	cfg := map[string]any{"listen": freeAdminTCPAddress(t), "shutdown_timeout": "1s", "database_path": dbPath, "master_key_file": keyPath,
		"components": []map[string]any{{"id": "adapter", "kind": "adapter", "implementation": responsesImplementation}, {"id": "connector", "kind": "connector", "implementation": responsesImplementation, "endpoint": "http://127.0.0.1:1/v1/responses", "credential_env": "PESTIROUTE_UPSTREAM_TEST", "max_request_body_bytes": 4096, "max_request_header_bytes": 4096, "connect_timeout": "1s", "tls_handshake_timeout": "1s", "response_header_timeout": "1s", "stream_idle_timeout": "2s"}},
		"routes":     []map[string]any{{"protocol": responsesProtocol, "mode": "native", "model": "model", "account": "account-id", "adapter": "adapter", "connector": "connector"}},
		"admin":      map[string]any{"listen": freeAdminTCPAddress(t), "token_env": "PESTIROUTE_ADMIN_TEST_TOKEN", "read_timeout": "2s", "write_timeout": "2s", "idle_timeout": "2s"}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := loadConfig([]string{"-config", configPath})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config", configPath}) }()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		request := func(path string) *http.Response {
			r, _ := http.NewRequest(http.MethodGet, "http://"+loaded.Admin.Listen+path, nil)
			r.Header.Set("Authorization", "Bearer operator-secret")
			resp, e := client.Do(r)
			if e != nil {
				return nil
			}
			return resp
		}
		status := request("/admin/v1/status")
		if status != nil && status.StatusCode == 200 {
			statusBody, _ := io.ReadAll(status.Body)
			status.Body.Close()
			if !bytes.Contains(statusBody, []byte(`"account_id":"account-id"`)) {
				cancel()
				<-done
				t.Fatalf("explicit account identity missing: %s", statusBody)
			}
			for _, value := range [][]byte{[]byte(`"endpoint"`), []byte("PESTIROUTE_UPSTREAM_TEST")} {
				if bytes.Contains(statusBody, value) {
					cancel()
					<-done
					t.Fatalf("status leaked topology detail %q: %s", value, statusBody)
				}
			}
			usage := request("/admin/v1/usage/requests")
			if usage != nil {
				body, _ := io.ReadAll(usage.Body)
				usage.Body.Close()
				cancel()
				if usage.StatusCode != 200 || !bytes.Contains(body, []byte(`"total":0`)) {
					t.Fatalf("persistent admin usage status=%d body=%s", usage.StatusCode, body)
				}
				if closeErr := <-done; closeErr != nil {
					t.Fatal(closeErr)
				}
				return
			}
		}
		if status != nil {
			status.Body.Close()
		}
		select {
		case err := <-done:
			cancel()
			t.Fatalf("gateway exited before admin ready: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("persistent admin listener did not become ready")
}

func TestAdminTLSStartupFailureClosesListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	files := t.TempDir()
	cert := filepath.Join(files, "cert.pem")
	key := filepath.Join(files, "key.pem")
	if err := os.WriteFile(cert, []byte("invalid certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("invalid key"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := serveAdminTLS(&http.Server{}, listener, cert, key); err == nil {
		t.Fatal("ServeTLS accepted invalid certificate")
	}
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listener not closed after ServeTLS startup failure: %v", err)
	}
}
