//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProtectedStartupExternalSIGTERMReapsUncooperativeChild(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	dir := t.TempDir()
	child := filepath.Join(dir, "uncooperative-child")
	buildChild := exec.Command("go", "build", "-o", child, "./cmd/gateway/testdata/uncooperative-child")
	buildChild.Dir = root
	if output, err := buildChild.CombinedOutput(); err != nil {
		t.Fatalf("build child: %v\n%s", err, output)
	}
	gateway := filepath.Join(dir, "gateway")
	buildGateway := exec.Command("go", "build", "-o", gateway, "./cmd/gateway")
	buildGateway.Dir = root
	if output, err := buildGateway.CombinedOutput(); err != nil {
		t.Fatalf("build gateway: %v\n%s", err, output)
	}
	pidFile := filepath.Join(dir, "wrapper.pid")
	_, dbPath, keyPath := protectedFixture(t)
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := probe.Addr().String()
	_ = probe.Close()
	configPath := filepath.Join(dir, "gateway.yaml")
	config := fmt.Sprintf(`version: 1
server: {listen: %q, max_request_bytes: 1048576, shutdown_timeout: 1s}
storage: {driver: sqlite, path: %q}
secrets: {master_key_file: %q}
connectors:
  - id: upstream
    kind: connector
    implementation: pestiroute.responses.native
    protocols: [openai.responses.v1]
    settings:
      base_url: https://backend.example/v1
      upstream_protocol: openai.responses.v1
      mode: native
      credential_env: PROTECTED_TEST_CREDENTIAL
      max_request_body_bytes: 1048576
      max_request_header_bytes: 16384
      connect_timeout: 2s
      tls_handshake_timeout: 2s
      response_header_timeout: 5s
      stream_idle_timeout: 30s
    process:
      executable: %q
      args: [%q]
      working_directory: %q
      environment_allowlist: []
routes:
  - id: route-a
    protocol: openai.responses.v1
    mode: native
    model: model-a
    adapter: pestiroute.responses.native
    policy: standard
    budget: {unknown_estimate: reject}
    targets: [{connector: upstream, account: account-a}]
policies: {standard: policy-id-a}
`, listen, dbPath, keyPath, child, pidFile, dir)
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	before := childProcessDirectories(t)
	cmd := exec.Command(gateway, "-config-format", "yaml", "-config", configPath)
	var gatewayOutput bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &gatewayOutput
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(pidFile); err == nil {
			resp, err := client.Get("http://" + listen + "/readyz")
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			t.Fatalf("gateway did not become ready with external child: %s", gatewayOutput.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pgid, err := strconv.Atoi(string(pidBytes))
	if err != nil {
		t.Fatalf("invalid child process-group id: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err := <-wait:
		if err != nil {
			t.Fatalf("gateway shutdown: %v: %s", err, gatewayOutput.String())
		}
	case <-time.After(45 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("gateway did not finish bounded child cleanup")
	}
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("external process group %d remains after gateway exit: %v", pgid, err)
	}
	deadline = time.Now().Add(time.Second)
	for hasNewChildProcessDirectories(before, childProcessDirectories(t)) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := childProcessDirectories(t); hasNewChildProcessDirectories(before, after) {
		t.Fatalf("child-owned socket directories remain after gateway exit: before=%v after=%v", before, after)
	}
}

func childProcessDirectories(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "pestiroute-child-") {
			paths = append(paths, entry.Name())
		}
	}
	return paths
}

func hasNewChildProcessDirectories(before, after []string) bool {
	old := make(map[string]struct{}, len(before))
	for _, path := range before {
		old[path] = struct{}{}
	}
	for _, path := range after {
		if _, exists := old[path]; !exists {
			return true
		}
	}
	return false
}
