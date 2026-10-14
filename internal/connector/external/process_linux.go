//go:build linux

package external

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	connectorSocketEnv    = "PESTIROUTE_CONNECTOR_SOCKET"
	hostServicesSocketEnv = "PESTIROUTE_HOST_SERVICES_SOCKET"
	maxDiagnostics        = 64 << 10
	startupTimeout        = 30 * time.Second
)

// ProcessConfig contains only the executable launch tuple. Environment values
// are deliberately unavailable: the child receives only its two socket paths.
type ProcessConfig struct {
	Executable       string
	Args             []string
	WorkingDirectory string
}

// Process owns one same-user child process group and its private socket paths.
type Process struct {
	cmd             *exec.Cmd
	dir             string
	connector       string
	hostServices    string
	listener        *net.UnixListener
	listenerInfo    os.FileInfo
	connectorInfo   os.FileInfo
	diagnostics     *limitedWriter
	startupDeadline time.Time
	epoch           uint64
	waitDone        chan struct{}
	waitErr         error
	closeOnce       sync.Once
	closeErr        error
}

type limitedWriter struct {
	mu sync.Mutex
	b  []byte
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	if len(w.b) < maxDiagnostics {
		w.b = append(w.b, p[:min(len(p), maxDiagnostics-len(w.b))]...)
	}
	return n, nil
}

func (w *limitedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(append([]byte(nil), w.b...))
}

// Start launches an absolute executable directly, with no inherited
// environment, in a fresh process group. Startup waits for the child's socket
// path to appear; protocol handshake and lifecycle calls use the Connector proxy.
func Start(ctx context.Context, cfg ProcessConfig) (*Process, error) {
	return startWithTimeout(ctx, cfg, startupTimeout)
}

// startWithTimeout keeps short deadlines available to deterministic package
// tests; production callers always use the binding's fixed startupTimeout.
func startWithTimeout(ctx context.Context, cfg ProcessConfig, timeout time.Duration) (*Process, error) {
	if !filepath.IsAbs(cfg.Executable) || !filepath.IsAbs(cfg.WorkingDirectory) {
		return nil, errors.New("executable and working directory must be absolute")
	}
	if timeout <= 0 {
		return nil, errors.New("startup timeout must be positive")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	wd, err := os.Stat(cfg.WorkingDirectory)
	if err != nil || !wd.IsDir() {
		return nil, errors.New("working directory must be an existing directory")
	}
	st, err := os.Stat(cfg.Executable)
	if err != nil || !st.Mode().IsRegular() || st.Mode()&0111 == 0 {
		return nil, errors.New("executable must be an executable regular file")
	}
	dir, err := os.MkdirTemp("", "pestiroute-child-")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	p := &Process{dir: dir, connector: filepath.Join(dir, "connector.sock"), hostServices: filepath.Join(dir, "host-services.sock"), waitDone: make(chan struct{}), diagnostics: &limitedWriter{}}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: p.hostServices, Net: "unix"})
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	p.listener = listener
	p.listener.SetUnlinkOnClose(false)
	p.listenerInfo, err = os.Lstat(p.hostServices)
	if err != nil {
		listener.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	if err := os.Chmod(p.hostServices, 0600); err != nil {
		p.cleanupSocket()
		os.Remove(dir)
		return nil, err
	}
	cmd := exec.Command(cfg.Executable, cfg.Args...)
	cmd.Dir = cfg.WorkingDirectory
	cmd.Env = []string{connectorSocketEnv + "=" + p.connector, hostServicesSocketEnv + "=" + p.hostServices}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = p.diagnostics, p.diagnostics
	p.cmd = cmd
	p.startupDeadline = time.Now().Add(timeout)
	if err := cmd.Start(); err != nil {
		p.cleanupSocket()
		os.Remove(dir)
		return nil, err
	}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.waitDone)
	}()
	deadline := time.NewTimer(time.Until(p.startupDeadline))
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if info, statErr := os.Lstat(p.connector); statErr == nil {
			if time.Now().After(p.startupDeadline) {
				return nil, p.abort(errors.New("child startup deadline exceeded"))
			}
			if info.Mode()&os.ModeSocket == 0 || !sameOwner(info, os.Getuid()) {
				return nil, p.abort(errors.New("child connector endpoint is not an owned Unix socket"))
			}
			p.connectorInfo = info
			return p, nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, p.abort(fmt.Errorf("inspect child connector endpoint: %w", statErr))
		}
		select {
		case <-ctx.Done():
			return nil, p.abort(ctx.Err())
		case <-deadline.C:
			return nil, p.abort(errors.New("child startup deadline exceeded"))
		case <-p.waitDone:
			return nil, p.abort(fmt.Errorf("child exited before startup: %w", p.waitErr))
		case <-tick.C:
		}
	}
}

func (p *Process) ConnectorSocket() string    { return p.connector }
func (p *Process) HostServicesSocket() string { return p.hostServices }
func (p *Process) PID() int                   { return p.cmd.Process.Pid }
func (p *Process) Diagnostics() string        { return p.diagnostics.String() }

// StartupDeadline is shared with the subsequent connect/handshake layer.
func (p *Process) StartupDeadline() time.Time { return p.startupDeadline }

// AcceptHostPeer accepts the child connection and verifies Linux peer identity.
func (p *Process) AcceptHostPeer(ctx context.Context) (*net.UnixConn, error) {
	if err := p.SocketOwned(p.hostServices); err != nil {
		return nil, err
	}
	watchDone := make(chan struct{})
	watchStopped := make(chan struct{})
	go func() {
		defer close(watchStopped)
		select {
		case <-ctx.Done():
			_ = p.listener.SetDeadline(time.Now())
		case <-watchDone:
		}
	}()
	defer func() {
		close(watchDone)
		<-watchStopped
		_ = p.listener.SetDeadline(time.Time{})
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = p.listener.SetDeadline(deadline)
	}
	conn, err := p.listener.AcceptUnix()
	if err != nil {
		return nil, err
	}
	if err := VerifyPeer(conn, p.PID()); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// DialConnector connects to the child-owned endpoint and checks its peer PID.
func (p *Process) DialConnector(ctx context.Context) (*net.UnixConn, error) {
	if err := p.SocketOwned(p.connector); err != nil {
		return nil, err
	}
	d := net.Dialer{}
	c, err := d.DialContext(ctx, "unix", p.connector)
	if err != nil {
		return nil, err
	}
	conn, ok := c.(*net.UnixConn)
	if !ok {
		c.Close()
		return nil, errors.New("connector endpoint is not Unix")
	}
	if err := VerifyPeer(conn, p.PID()); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// VerifyPeer rejects Unix peers other than the expected process ID.
func VerifyPeer(conn *net.UnixConn, pid int) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var cred *syscall.Ucred
	var controlErr error
	err = raw.Control(func(fd uintptr) {
		cred, controlErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil {
		return err
	}
	if controlErr != nil {
		return controlErr
	}
	if cred.Pid != int32(pid) || int(cred.Uid) != os.Getuid() {
		return fmt.Errorf("%w: Unix peer pid=%d uid=%d", ErrHandshakePeer, cred.Pid, cred.Uid)
	}
	return nil
}

// Shutdown drains through closeChild; a failed/expired drain starts TERM now.
// A successful drain gets 2 seconds before TERM, then KILL at 2 seconds after
// TERM. Direct-child Wait is performed once by Start's waiter.
func (p *Process) Shutdown(ctx context.Context, closeChild func(context.Context) error) error {
	p.closeOnce.Do(func() { p.closeErr = p.shutdown(ctx, closeChild) })
	return p.closeErr
}

func (p *Process) shutdown(ctx context.Context, closeChild func(context.Context) error) error {
	if closeChild == nil {
		closeChild = func(context.Context) error { return nil }
	}
	drainCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	drainErr := closeChild(drainCtx)
	cancel()
	termNow := drainErr != nil || ctx.Err() != nil
	if !termNow {
		if wait(p.waitDone, 2*time.Second) && !p.groupExists() {
			return errors.Join(drainErr, p.cleanup())
		}
	}
	reaped, groupGone := false, false
	if p.groupExists() {
		if err := syscall.Kill(-p.PID(), syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			drainErr = errors.Join(drainErr, err)
		}
		termAt := time.Now()
		if p.waitGroupGoneUntil(termAt.Add(2*time.Second)) != nil {
			_ = syscall.Kill(-p.PID(), syscall.SIGKILL)
		}
		deadline := termAt.Add(5 * time.Second)
		reaped = waitUntil(p.waitDone, deadline)
		if !reaped {
			drainErr = errors.Join(drainErr, errors.New("child was not reaped within 5 seconds"))
		}
		if err := p.waitGroupGoneUntil(deadline); err != nil {
			drainErr = errors.Join(drainErr, err)
		} else {
			groupGone = true
		}
	} else {
		reaped = wait(p.waitDone, 5*time.Second)
		groupGone = !p.groupExists()
		if !reaped {
			drainErr = errors.Join(drainErr, errors.New("child was not reaped within 5 seconds"))
		}
	}
	if reaped && groupGone {
		return errors.Join(drainErr, p.cleanup())
	}
	return drainErr
}

func (p *Process) abort(cause error) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownErr := p.Shutdown(shutdownCtx, func(context.Context) error { return cause })
	if errors.Is(shutdownErr, cause) {
		return shutdownErr
	}
	return errors.Join(cause, shutdownErr)
}

func (p *Process) waitGroupGoneUntil(deadline time.Time) error {
	for time.Now().Before(deadline) {
		if !p.groupExists() {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("child process group remains after termination deadline")
}

func waitUntil(done <-chan struct{}, deadline time.Time) bool {
	d := time.Until(deadline)
	if d <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	return wait(done, d)
}

func (p *Process) groupExists() bool {
	err := syscall.Kill(-p.PID(), 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}

func wait(done <-chan struct{}, timeout time.Duration) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

func (p *Process) cleanup() error {
	if err := p.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	p.cleanupSocket()
	if info, err := os.Lstat(p.connector); err == nil && p.connectorInfo != nil && os.SameFile(info, p.connectorInfo) && info.Mode()&os.ModeSocket != 0 {
		if err := os.Remove(p.connector); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(p.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (p *Process) cleanupSocket() {
	info, err := os.Lstat(p.hostServices)
	if err == nil && os.SameFile(info, p.listenerInfo) && info.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(p.hostServices)
	}
}

func sameOwner(info os.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid
}

// SocketOwned verifies that a path is a same-user socket beneath this process's
// private directory; it never follows symlinks.
func (p *Process) SocketOwned(path string) error {
	clean := filepath.Clean(path)
	if filepath.Dir(clean) != p.dir || strings.Contains(filepath.Base(clean), string(filepath.Separator)) {
		return errors.New("socket path is outside private directory")
	}
	var expected os.FileInfo
	switch clean {
	case p.hostServices:
		expected = p.listenerInfo
	case p.connector:
		expected = p.connectorInfo
	default:
		return errors.New("socket path is not a supervisor-owned endpoint")
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || !sameOwner(info, os.Getuid()) {
		return errors.New("path is not a same-user Unix socket")
	}
	if expected == nil || !os.SameFile(info, expected) {
		return errors.New("Unix socket path no longer identifies the owned endpoint")
	}
	return nil
}
