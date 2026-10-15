//go:build linux

package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/blestafist/pestiroute/internal/connector/external"
)

func TestExternalProcessConfig(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "child")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	valid := protectedProcess{Executable: executable, Args: []string{"$LITERAL", "$(not-a-shell)"}, WorkingDirectory: dir, EnvironmentAllowlist: []string{}}
	if err := validateProtectedProcess("connector.process", valid); err != nil {
		t.Fatalf("valid launch tuple rejected: %v", err)
	}
	for name, process := range map[string]protectedProcess{
		"relative executable":        {Executable: "child", WorkingDirectory: dir},
		"missing executable":         {Executable: filepath.Join(dir, "missing"), WorkingDirectory: dir},
		"relative working directory": {Executable: executable, WorkingDirectory: "."},
		"missing working directory":  {Executable: executable, WorkingDirectory: filepath.Join(dir, "missing")},
		"environment allowlist":      {Executable: executable, WorkingDirectory: dir, EnvironmentAllowlist: []string{"PATH"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateProtectedProcess("connector.process", process); err == nil {
				t.Fatal("accepted invalid process configuration")
			}
		})
	}
	if got := (external.ProcessConfig{Executable: executable, Args: valid.Args, WorkingDirectory: dir}).Args; got[0] != "$LITERAL" {
		t.Fatalf("arguments were not retained literally: %#v", got)
	}

	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	path := filepath.Join(dir, "protected.yaml")
	data := fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:8080", max_request_bytes: 1048576, shutdown_timeout: 5s}
storage: {driver: sqlite, path: ./gateway.db}
secrets: {master_key_file: ./master.key}
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
      args: ["$LITERAL", "$(not-a-shell)"]
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
`, executable, dir)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProtectedYAML(path); err != nil {
		t.Fatalf("strict YAML rejected valid process tuple: %v", err)
	}
	unknown := strings.Replace(data, "      working_directory:", "      unknown_field: true\n      working_directory:", 1)
	if err := os.WriteFile(path, []byte(unknown), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProtectedYAML(path); err == nil {
		t.Fatal("accepted unknown process configuration field")
	}
}

func TestProtectedStartupExternal(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	child := filepath.Join(t.TempDir(), "responses-connector-child")
	build := exec.Command("go", "build", "-o", child, "./cmd/responses-connector-child")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build child: %v\n%s", err, output)
	}
	_, dbPath, keyPath := protectedFixture(t)
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	c := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
	c.protected.Connectors[0].Process = &protectedProcess{Executable: child, WorkingDirectory: t.TempDir()}
	prepared, err := prepareProtectedConfig(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	var ready, draining atomic.Bool
	handler, closeComponents, err := composeHandler(prepared, &ready, &draining, nil)
	if err != nil {
		t.Fatal(err)
	}
	ready.Store(true)
	probe := httptest.NewRecorder()
	handler.ServeHTTP(probe, httptest.NewRequest("GET", "/readyz", nil))
	if probe.Code != 200 {
		t.Fatalf("gateway not ready after child handshake: %d", probe.Code)
	}
	if err := closeComponents(context.Background()); err != nil {
		t.Fatalf("close external gateway: %v", err)
	}
}

func TestM6ProtectedStartup(t *testing.T) {
	_, dbPath, keyPath := protectedFixture(t)
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	c := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
	c.protected.Connectors[0].Process = &protectedProcess{Executable: "/bin/false", WorkingDirectory: t.TempDir()}
	prepared, err := prepareProtectedConfig(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	var ready, draining atomic.Bool
	if _, _, err := composeHandler(prepared, &ready, &draining, nil); err == nil {
		t.Fatal("child handshake failure did not abort gateway composition")
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database disappeared after startup failure: %v", err)
	}
	second, err := prepareProtectedConfig(context.Background(), c)
	if err != nil {
		t.Fatalf("startup failure leaked the database lock: %v", err)
	}
	_ = second.runtimeDB.Close()
	_ = second.processLock.Close()
}
