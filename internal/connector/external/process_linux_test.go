//go:build linux

package external

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestM6ProcessChild(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "--mode") {
		return
	}
	connector := os.Getenv(connectorSocketEnv)
	host := os.Getenv(hostServicesSocketEnv)
	if connector == "" || host == "" || os.Getenv("PATH") != "" {
		os.Exit(21)
	}
	if len(os.Environ()) != 2 {
		os.Exit(22)
	}
	args := os.Args
	mode := "serve"
	for i, arg := range args {
		if arg == "--mode" && i+1 < len(args) {
			mode = args[i+1]
		}
	}
	if strings.HasPrefix(mode, "handshake-") || strings.HasPrefix(mode, "lifecycle-") || strings.HasPrefix(mode, "auth-") || strings.HasPrefix(mode, "execute-") || strings.HasPrefix(mode, "supervisor-") {
		if mode == "supervisor-ignore-term-descendant" {
			cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; while :; do /bin/sleep 1; done")
			if err := cmd.Start(); err != nil {
				os.Exit(36)
			}
			_, _ = fmt.Fprintln(os.Stderr, "descendant-started")
			caught := make(chan os.Signal, 1)
			signalNotify(caught)
			go func() {
				for range caught {
				}
			}()
		}
		if mode == "supervisor-orderly-descendant" {
			cmd := exec.Command("/bin/sh", "-c", "trap 'echo descendant-term >&2; exit' TERM; /bin/sleep 0.35; echo descendant-orderly-exit >&2")
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				os.Exit(35)
			}
			_, _ = fmt.Fprintln(os.Stderr, "descendant-started")
		}
		if mode == "handshake-wrong-peer" {
			cmd := exec.Command(os.Args[0], "-test.run=^TestM6ProcessChild$", "--", "--mode", "handshake-valid")
			cmd.Env = os.Environ()
			if err := cmd.Start(); err != nil {
				os.Exit(33)
			}
			_ = cmd.Wait()
			os.Exit(0)
		}
		marker := ""
		attemptMarker := ""
		for i, arg := range args {
			if arg == "--marker" && i+1 < len(args) {
				marker = args[i+1]
			}
			if arg == "--attempt-marker" && i+1 < len(args) {
				attemptMarker = args[i+1]
			}
		}
		serveHandshakeChild(connector, mode, marker, attemptMarker)
		os.Exit(0)
	}
	if mode == "hang" {
		select {}
	}
	if mode == "foreign" {
		if err := os.WriteFile(connector, []byte("foreign"), 0600); err != nil {
			os.Exit(23)
		}
		for i, arg := range args {
			if arg == "--marker" && i+1 < len(args) {
				if err := os.WriteFile(args[i+1], []byte(connector), 0600); err != nil {
					os.Exit(26)
				}
			}
		}
		select {}
	}
	if mode == "diagnostic" {
		fmt.Fprint(os.Stderr, strings.Repeat("D", 70<<10))
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: connector, Net: "unix"})
	if err != nil {
		os.Exit(24)
	}
	defer listener.Close()
	conn, err := net.Dial("unix", host)
	if err != nil {
		os.Exit(25)
	}
	conn.Close()
	if mode == "ignore-term" {
		c := make(chan os.Signal, 1)
		signalNotify(c)
		for range c {
		}
	}
	if mode == "tree-ignore-term" {
		cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; while :; do /bin/sleep 1; done")
		if err := cmd.Start(); err != nil {
			os.Exit(34)
		}
		_, _ = fmt.Fprintln(os.Stderr, "descendant-started")
		c := make(chan os.Signal, 1)
		signalNotify(c)
		for range c {
		}
	}
	select {}
}

func signalNotify(c chan<- os.Signal) { signal.Notify(c, syscall.SIGTERM) }

func processConfig(mode string) ProcessConfig {
	exe, _ := os.Executable()
	return ProcessConfig{Executable: exe, Args: []string{"-test.run=^TestM6ProcessChild$", "--", "--mode", mode}, WorkingDirectory: filepath.Dir(exe)}
}

func startProcess(t *testing.T, mode string, timeout time.Duration) *Process {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := startWithTimeout(ctx, processConfig(mode), timeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background(), func(context.Context) error { return nil }) })
	return p
}

func TestM6ProcessLaunch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := Start(ctx, processConfig("serve"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background(), nil) })
	if remaining := time.Until(p.StartupDeadline()); remaining <= 29*time.Second || remaining > startupTimeout {
		t.Fatalf("production startup deadline is not the fixed binding deadline: %s", remaining)
	}
	exe, _ := os.Executable()
	wantArgs := []string{exe, "-test.run=^TestM6ProcessChild$", "--", "--mode", "serve"}
	if strings.Join(p.cmd.Args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("argv = %#v, want %#v", p.cmd.Args, wantArgs)
	}
	if p.cmd.Dir != filepath.Dir(exe) || len(p.cmd.Env) != 2 || !strings.HasPrefix(p.cmd.Env[0], connectorSocketEnv+"=") || !strings.HasPrefix(p.cmd.Env[1], hostServicesSocketEnv+"=") {
		t.Fatalf("unexpected launch directory/environment: %q %#v", p.cmd.Dir, p.cmd.Env)
	}
	if !filepath.IsAbs(p.ConnectorSocket()) || !filepath.IsAbs(p.HostServicesSocket()) {
		t.Fatal("socket paths are not absolute")
	}
	if filepath.Dir(p.connector) != p.dir || filepath.Dir(p.hostServices) != p.dir {
		t.Fatal("sockets not paired in private directory")
	}
	if st, err := os.Stat(p.dir); err != nil || st.Mode().Perm() != 0700 {
		t.Fatalf("private directory mode: %v %v", st, err)
	}
	pgid, pgidErr := syscall.Getpgid(p.PID())
	if p.cmd.SysProcAttr == nil || !p.cmd.SysProcAttr.Setpgid || pgidErr != nil || pgid != p.PID() {
		t.Fatal("child is not process-group leader")
	}
	peerCtx, cancelPeer := context.WithTimeout(context.Background(), time.Second)
	defer cancelPeer()
	peer, err := p.AcceptHostPeer(peerCtx)
	if err != nil {
		t.Fatal(err)
	}
	peer.Close()
	peer, err = p.DialConnector(peerCtx)
	if err != nil {
		t.Fatal(err)
	}
	peer.Close()
	if err := p.SocketOwned(p.connector); err != nil {
		t.Fatal(err)
	}
}

func TestM6ProcessRejectsForeignSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	cfg := processConfig("foreign")
	marker := filepath.Join(t.TempDir(), "foreign-path")
	cfg.Args = append(cfg.Args, "--marker", marker)
	p, err := startWithTimeout(ctx, cfg, 150*time.Millisecond)
	if err == nil || p != nil {
		t.Fatal("foreign endpoint accepted")
	}
	pathBytes, readErr := os.ReadFile(marker)
	if readErr != nil {
		t.Fatalf("foreign marker missing: %v", readErr)
	}
	path := string(pathBytes)
	if contents, readErr := os.ReadFile(path); readErr != nil || string(contents) != "foreign" {
		t.Fatalf("foreign path was removed/changed: %q %v", contents, readErr)
	}
	// The foreign file is intentionally not owned by the supervisor.
	_ = os.Remove(path)
	_ = os.Remove(filepath.Dir(path))
}

func TestM6ProcessStartupDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	p, err := startWithTimeout(ctx, processConfig("hang"), 100*time.Millisecond)
	if err == nil || p != nil {
		t.Fatal("hung child accepted")
	}
	if time.Since(started) > 4*time.Second {
		t.Fatal("startup cleanup exceeded bound")
	}
	cancelCtx, cancelStartup := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancelStartup()
	}()
	if p, err := startWithTimeout(cancelCtx, processConfig("hang"), 3*time.Second); err == nil || p != nil {
		t.Fatal("cancelled startup was accepted")
	}
}

func TestM6ProcessBoundsDiagnostics(t *testing.T) {
	p := startProcess(t, "diagnostic", 3*time.Second)
	if got := len(p.Diagnostics()); got != maxDiagnostics {
		t.Fatalf("retained diagnostics = %d", got)
	}
}

func TestM6ShutdownKillsProcessGroup(t *testing.T) {
	p := startProcess(t, "ignore-term", 3*time.Second)
	started := time.Now()
	err := p.Shutdown(context.Background(), func(context.Context) error { return errors.New("close timeout") })
	if err == nil || !strings.Contains(err.Error(), "close timeout") {
		t.Fatalf("expected close error after bounded process cleanup, got %v", err)
	}
	if elapsed := time.Since(started); elapsed < 1900*time.Millisecond || elapsed > 6*time.Second {
		t.Fatalf("TERM/KILL cleanup took %s", elapsed)
	}
	if err := syscall.Kill(-p.PID(), 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("process group remains: %v", err)
	}
}

func TestM6ShutdownKillsDescendantProcessGroup(t *testing.T) {
	p := startProcess(t, "tree-ignore-term", 3*time.Second)
	waitForDiagnostic(t, p, "descendant-started")
	started := time.Now()
	if err := p.Shutdown(context.Background(), func(context.Context) error { return errors.New("close timeout") }); err == nil {
		t.Fatal("expected shutdown to report close timeout")
	}
	if elapsed := time.Since(started); elapsed < 1900*time.Millisecond || elapsed > 6*time.Second {
		t.Fatalf("process-tree TERM/KILL cleanup took %s", elapsed)
	}
	if err := syscall.Kill(-p.PID(), 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("descendant process group remains: %v", err)
	}
}

func TestM6ProcessCleansOwnedSocket(t *testing.T) {
	p := startProcess(t, "serve", 3*time.Second)
	dir, socket := p.dir, p.hostServices
	if err := p.Shutdown(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{socket, dir} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned path %s remains: %v", path, err)
		}
	}
}

func TestM6ProcessRejectsInvalidLaunch(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []ProcessConfig{{Executable: "relative", WorkingDirectory: "/tmp"}, {Executable: "/bin/true", WorkingDirectory: "/missing"}} {
		if _, err := Start(ctx, cfg); err == nil {
			t.Fatal("invalid launch accepted")
		}
	}
}
