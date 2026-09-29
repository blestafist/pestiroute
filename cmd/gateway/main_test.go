package main

import (
	"bufio"
	"context"
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
