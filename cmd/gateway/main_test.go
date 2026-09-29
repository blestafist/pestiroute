package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestConfig(t *testing.T) {
	path := t.TempDir() + "/gateway.json"
	if err := os.WriteFile(path, []byte(`{"listen":"127.0.0.1:0","shutdown_timeout":"250ms"}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, duration, err := loadConfig([]string{"-config", path})
	if err != nil || c.Listen != "127.0.0.1:0" || duration != 250*time.Millisecond {
		t.Fatalf("config: %+v, %v, %v", c, duration, err)
	}
	for _, args := range [][]string{
		{"-config", path + ".missing"},
		{"-listen", "not-an-address"},
		{"-shutdown-timeout", "0s"},
	} {
		if _, _, err := loadConfig(args); err == nil {
			t.Errorf("expected error for %v", args)
		}
	}
	for _, body := range []string{`{"listen":`, `{"unexpected":1}`, `{} {}`, `{"listen":""}`} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadConfig([]string{"-config", path}); err == nil {
			t.Errorf("expected error for %q", body)
		}
	}
}

func TestInferenceConfig(t *testing.T) {
	const credential = "synthetic-secret-never-print"
	t.Setenv("PESTIROUTE_TEST_CREDENTIAL", credential)
	path := t.TempDir() + "/inference.json"
	valid := map[string]any{
		"listen": "127.0.0.1:0", "upstream_endpoint": "https://api.example.test/v1/responses",
		"upstream_credential_env": "PESTIROUTE_TEST_CREDENTIAL", "max_request_body_bytes": 1048576,
		"max_request_header_bytes": 8192, "connect_timeout": "1s", "tls_handshake_timeout": "2s",
		"response_header_timeout": "3s", "stream_idle_timeout": "4s",
	}
	check := func(fields map[string]any, args []string) (config, error) {
		t.Helper()
		data, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		c, _, err := loadConfig(append([]string{"-config", path}, args...))
		if strings.Contains(fmt.Sprintf("%+v %#v %v", c, c, err), credential) {
			t.Fatal("credential leaked in config or diagnostic")
		}
		return c, err
	}
	copyFields := func() map[string]any {
		fields := make(map[string]any, len(valid))
		for key, value := range valid {
			fields[key] = value
		}
		return fields
	}
	c, err := check(valid, nil)
	if err != nil || string(c.credential) != credential {
		t.Fatalf("valid inference configuration: %v", err)
	}
	if c, err = check(valid, []string{"-listen", "[::1]:0"}); err != nil || c.Listen != "[::1]:0" {
		t.Fatalf("flag precedence: %v", err)
	}
	fields := copyFields()
	fields["upstream_endpoint"] = "http://127.0.0.1:1234/v1/responses"
	if _, err := check(fields, nil); err != nil {
		t.Fatalf("local fake endpoint: %v", err)
	}

	for _, tc := range []struct {
		field string
		value any
		want  string
	}{
		{"upstream_endpoint", "http://example.test/v1/responses", "upstream_endpoint"},
		{"upstream_endpoint", "https://example.test/v1/other", "upstream_endpoint"},
		{"upstream_endpoint", "https://user:password@example.test/v1/responses", "upstream_endpoint"},
		{"upstream_endpoint", "https://example.test/v1/responses?x=1", "upstream_endpoint"},
		{"upstream_endpoint", "https://example.test/v1/responses#fragment", "upstream_endpoint"},
		{"upstream_endpoint", "https://example.test/v1/responses#", "upstream_endpoint"},
		{"upstream_endpoint", "http://localhost/v1/responses", "upstream_endpoint"},
		{"upstream_credential_env", "PESTIROUTE_UNSET_CREDENTIAL", "upstream_credential_env"},
		{"upstream_credential_env", "", "upstream_credential_env"},
		{"max_request_body_bytes", 0, "max_request_body_bytes"},
		{"max_request_header_bytes", -1, "max_request_header_bytes"},
		{"connect_timeout", "0s", "connect_timeout"},
		{"tls_handshake_timeout", "bad", "tls_handshake_timeout"},
		{"response_header_timeout", "-1s", "response_header_timeout"},
		{"stream_idle_timeout", "", "stream_idle_timeout"},
		{"upstream_endpoint", nil, "upstream_endpoint"},
		{"listen", "0.0.0.0:0", "loopback"},
		{"listen", "localhost:0", "loopback"},
		{"listen", "192.0.2.1:0", "loopback"},
	} {
		t.Run(fmt.Sprintf("%s=%v", tc.field, tc.value), func(t *testing.T) {
			fields := copyFields()
			fields[tc.field] = tc.value
			if _, err := check(fields, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %s error, got %v", tc.want, err)
			}
		})
	}
	for _, field := range []string{"upstream_endpoint", "upstream_credential_env", "max_request_body_bytes", "max_request_header_bytes", "connect_timeout", "tls_handshake_timeout", "response_header_timeout", "stream_idle_timeout"} {
		fields := copyFields()
		delete(fields, field)
		if _, err := check(fields, nil); err == nil || !strings.Contains(err.Error(), "partial inference configuration") {
			t.Errorf("missing %s: %v", field, err)
		}
	}
	fields = copyFields()
	fields["upstream_credential_env"] = "PESTIROUTE_EMPTY_CREDENTIAL"
	t.Setenv("PESTIROUTE_EMPTY_CREDENTIAL", "")
	if _, err := check(fields, nil); err == nil || !strings.Contains(err.Error(), "upstream_credential_env") {
		t.Errorf("empty credential: %v", err)
	}
	if _, err := check(map[string]any{"listen": "0.0.0.0:0"}, nil); err != nil {
		t.Errorf("probe-only bind: %v", err)
	}
}

func TestProbes(t *testing.T) {
	var ready atomic.Bool
	srv := &http.Server{Handler: probes(&ready)}
	// Use a real loopback listener so request-method routing is exercised.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(listener)
	defer srv.Close()
	base := "http://" + listener.Addr().String()
	check := func(path string, want int) {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: got %d, want %d", path, resp.StatusCode, want)
		}
	}
	check("/healthz", 200)
	check("/readyz", 503)
	ready.Store(true)
	check("/readyz", 200)
}

func TestGatewayLifecycle(t *testing.T) {
	binary := t.TempDir() + "/gateway"
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build gateway: %v: %s", err, out)
	}
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "-listen", "127.0.0.1:0", "-shutdown-timeout", "200ms")
			stderr, err := cmd.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(stderr)
			line, err := reader.ReadString('\n')
			if err != nil || !strings.HasPrefix(line, "gateway: listening on ") {
				t.Fatalf("startup: %q: %v", line, err)
			}
			base := "http://" + strings.TrimSpace(strings.TrimPrefix(line, "gateway: listening on "))
			for _, path := range []string{"/healthz", "/readyz"} {
				resp, err := http.Get(base + path)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatalf("%s: status %d", path, resp.StatusCode)
				}
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			rest, _ := io.ReadAll(reader)
			if err := cmd.Wait(); err != nil {
				t.Fatalf("signal exit: %v; stderr: %s", err, rest)
			}
			if _, err := http.Get(base + "/healthz"); err == nil {
				t.Fatal("listener still open after shutdown")
			}
		})
	}

	path := t.TempDir() + "/broken.json"
	if err := os.WriteFile(path, []byte(`{"listen":`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-config", path}, {"-config", path + ".missing"}, {"-listen", "invalid"}} {
		cmd := exec.Command(binary, args...)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "gateway:") || !strings.Contains(string(out), args[1]) {
			t.Errorf("invalid %v: output %q, error %v", args, out, err)
		}
	}
}
