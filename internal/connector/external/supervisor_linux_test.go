//go:build linux

package external

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestM6SupervisorRestartPolicy(t *testing.T) {
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond, 2 * time.Second, 2 * time.Second}
	for i, delay := range want {
		if got := supervisorBackoff(i + 1); got != delay {
			t.Fatalf("backoff(%d) = %s, want %s", i+1, got, delay)
		}
	}
	var budget restartBudget
	start := time.Now()
	for i := range supervisorRestartLimit {
		ordinal, admitted := budget.admit(start.Add(time.Duration(i) * time.Second))
		if !admitted || ordinal != i+1 || supervisorBackoff(ordinal) != want[i] {
			t.Fatalf("restart %d unexpectedly denied", i+1)
		}
	}
	if _, admitted := budget.admit(start.Add(3 * time.Second)); admitted {
		t.Fatal("fourth restart inside rolling minute was admitted")
	}
	ordinal, admitted := budget.admit(start.Add(3*time.Second + supervisorWindow + time.Second))
	if !admitted || ordinal != 1 || supervisorBackoff(ordinal) != supervisorBaseBackoff {
		t.Fatal("restart was not admitted after the rolling window")
	}
}

func TestM6SupervisorPublicationCloseOrdering(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Supervisor{ctx: ctx, cancel: cancel}
	s.mu.Lock()
	if !s.publishLocked(nil, nil, 1) {
		t.Fatal("generation was not published")
	}
	closed := make(chan struct{})
	go func() { _ = s.Close(context.Background()); close(closed) }()
	s.mu.Unlock()
	deadline := time.After(time.Second)
	for {
		s.mu.RLock()
		isClosed := s.closed
		s.mu.RUnlock()
		if isClosed {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Close did not enter its state transition")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-closed:
		t.Fatal("Close failed to wait for the published watcher")
	default:
	}
	s.watchers.Done() // release the fake watcher registered by publishLocked
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after the watcher exited")
	}
	s.mu.Lock()
	if s.publishLocked(nil, nil, 2) {
		t.Fatal("generation published after Close")
	}
	s.mu.Unlock()
}

func TestM6ShutdownSupervisorCloseWaitsForSharedCompletion(t *testing.T) {
	s, err := StartSupervisor(context.Background(), processConfig("lifecycle-close-delay-fail"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	p, dir := s.process, s.process.dir
	s.mu.RUnlock()
	short, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if err := s.Close(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close=%v, want caller deadline", err)
	}

	long, cancelLong := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelLong()
	second := make(chan error, 1)
	go func() { second <- s.Close(long) }()
	select {
	case err := <-second:
		t.Fatalf("second Close returned before cleanup completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("resources were removed before child Close finished: %v", err)
	}
	var closeErr error
	select {
	case closeErr = <-second:
		if closeErr == nil {
			t.Fatal("supervisor reported success for child Close failure")
		}
	case <-long.Done():
		t.Fatalf("second Close did not observe completion: %v", long.Err())
	}
	if err := s.Close(context.Background()); err == nil || err.Error() != closeErr.Error() {
		t.Fatalf("repeated Close error=%v, want stable result %v", err, closeErr)
	}
	assertProcessReaped(t, p)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("supervisor returned before owned directory cleanup: %v", err)
	}
}

func TestM6ShutdownSupervisorWaitsForOrderlyDescendantExit(t *testing.T) {
	s, err := StartSupervisor(context.Background(), processConfig("supervisor-orderly-descendant"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	p := s.process
	s.mu.RUnlock()
	waitForDiagnostic(t, p, "descendant-started")
	started := time.Now()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 250*time.Millisecond || elapsed >= 2*time.Second {
		t.Fatalf("orderly descendant drain elapsed %s; expected natural exit within grace", elapsed)
	}
	if !strings.Contains(p.Diagnostics(), "descendant-orderly-exit") || strings.Contains(p.Diagnostics(), "descendant-term") {
		t.Fatalf("descendant was terminated before its orderly exit: %q", p.Diagnostics())
	}
	assertProcessReaped(t, p)
}

func TestM6ShutdownSupervisorWaitsPastSafetyDeadlineForCleanup(t *testing.T) {
	s, err := StartSupervisor(context.Background(), processConfig("supervisor-ignore-term-descendant"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.closeWaitLimit = 75 * time.Millisecond
	p, dir := s.process, s.process.dir
	s.mu.Unlock()
	waitForDiagnostic(t, p, "descendant-started")
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Close(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close=%v, want caller deadline", err)
	}

	long, cancelLong := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelLong()
	second := make(chan error, 1)
	go func() { second <- s.Close(long) }()
	select {
	case err := <-second:
		t.Fatalf("Supervisor.Close completed before Connector cleanup: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("owned directory disappeared before process-group cleanup: %v", err)
	}
	var closeErr error
	select {
	case closeErr = <-second:
		if !errors.Is(closeErr, context.DeadlineExceeded) {
			t.Fatalf("safety-limit result=%v, want stable deadline error", closeErr)
		}
	case <-long.Done():
		t.Fatalf("cleanup did not finish: %v", long.Err())
	}
	if err := s.Close(context.Background()); err == nil || err.Error() != closeErr.Error() {
		t.Fatalf("repeated Close error=%v, want stable result %v", err, closeErr)
	}
	assertProcessReaped(t, p)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("Supervisor.Close completed before owned directory cleanup: %v", err)
	}
}

func TestM6SupervisorValidInvalidRequestDoesNotRestart(t *testing.T) {
	s, err := StartSupervisor(context.Background(), processConfig("supervisor-valid-invalid-request"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	initialEpoch, initialPID := s.epoch, s.process.PID()
	s.mu.RUnlock()
	request := core.ExecutionRequest{ID: "rejected", Model: "fixture", Payload: core.RawPayload{Protocol: "test.v1", ContentType: "application/json"}}
	scope := core.AttemptScope{ID: "rejected-attempt", AccountID: "account", Mode: core.ModeNative}
	_, ge := s.Execute(context.Background(), request, scope, core.InvocationServices{})
	if ge == nil || ge.Category != core.CategoryInvalidRequest || ge.Code != "invalid_request" {
		t.Fatalf("child rejection=%+v", ge)
	}
	time.Sleep(250 * time.Millisecond)
	s.mu.RLock()
	stillReady := s.ready && s.epoch == initialEpoch && s.process.PID() == initialPID
	s.mu.RUnlock()
	if !stillReady {
		t.Fatal("valid child GatewayError incorrectly restarted the process")
	}
	models, ge := s.Models(context.Background(), core.ModelQuery{Protocol: "test.v1", Mode: core.ModeTranslation, AccountID: "account"}, core.InvocationServices{})
	if ge != nil || !models.Supported {
		t.Fatalf("unrelated new operation failed after rejection: %+v, %v", models, ge)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestM6SupervisorRestartsNewWorkOnly(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "crashed-once")
	attemptMarker := filepath.Join(t.TempDir(), "attempts")
	cfg := processConfig("supervisor-crash-once-execute")
	cfg.Args = append(cfg.Args, "--marker", marker, "--attempt-marker", attemptMarker)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	s, err := StartSupervisor(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Init(ctx, core.ComponentConfig{Data: []byte("stable-config")}); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	oldProcess, oldEpoch, oldRegistry := s.process, s.epoch, s.current.invocations
	s.mu.RUnlock()
	request := core.ExecutionRequest{ID: "request", Model: "fixture", Payload: core.RawPayload{Protocol: "test.v1", ContentType: "application/json"}}
	scope := core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}
	type executeResult struct{ err *core.GatewayError }
	result := make(chan executeResult, 1)
	go func() {
		_, ge := s.Execute(ctx, request, scope, core.InvocationServices{})
		result <- executeResult{err: ge}
	}()
	deadlineSend := time.After(time.Second)
	for {
		if _, err := os.Stat(attemptMarker); err == nil {
			break
		}
		select {
		case <-deadlineSend:
			t.Fatal("Execute was not delivered to the first child")
		case <-time.After(5 * time.Millisecond):
		}
	}
	select {
	case got := <-result:
		if got.err == nil || got.err.Category != core.CategoryUnavailable || got.err.RetryDisposition != core.RetryUnknown {
			t.Fatalf("active execution did not fail unknown/unavailable: %+v", got.err)
		}
	case <-ctx.Done():
		t.Fatal("active execution was not failed on child crash")
	}
	if _, ready := s.get(); ready {
		t.Fatal("supervisor admitted new work after process loss")
	}
	if _, ge := s.Models(ctx, core.ModelQuery{}, core.InvocationServices{}); ge == nil || ge.Category != core.CategoryUnavailable {
		t.Fatalf("request during restart was not rejected unavailable: %+v", ge)
	}
	deadline := time.After(9 * time.Second)
	for {
		s.mu.RLock()
		restarted := s.ready && s.epoch > oldEpoch
		s.mu.RUnlock()
		if restarted {
			break
		}
		select {
		case <-deadline:
			t.Fatal("replacement did not become ready")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, _, _, err := oldRegistry.admitCallback(oldEpoch, "stale", 1); err == nil {
		t.Fatal("callback from old process epoch was admitted")
	}
	if _, err := os.Stat(oldProcess.dir); !os.IsNotExist(err) {
		t.Fatalf("old process resources remain: %v", err)
	}
	if health := s.Health(ctx); health.State != core.HealthReady {
		t.Fatalf("new work not ready after restart: %+v", health)
	}
	models, ge := s.Models(ctx, core.ModelQuery{Protocol: "test.v1", Mode: core.ModeTranslation, AccountID: "account"}, core.InvocationServices{})
	if ge != nil || !models.Supported {
		t.Fatalf("new request failed after re-init: result=%+v err=%+v", models, ge)
	}
	if content, err := os.ReadFile(attemptMarker); err != nil || string(content) != "attempt\n" {
		t.Fatalf("active request replayed after restart: %q, %v", content, err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestM6SupervisorRestartBudgetAndClose(t *testing.T) {
	cfg := processConfig("supervisor-crash-loop")
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	s, err := StartSupervisor(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Init(ctx, core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for {
		s.mu.RLock()
		failed := s.failed
		s.mu.RUnlock()
		if failed {
			break
		}
		select {
		case <-deadline:
			t.Fatal("crash loop did not exhaust restart budget")
		case <-time.After(10 * time.Millisecond):
		}
	}
	s.mu.RLock()
	used := len(s.budget.restarts)
	s.mu.RUnlock()
	if used != supervisorRestartLimit {
		t.Fatalf("restart attempts=%d, want %d", used, supervisorRestartLimit)
	}
	if h := s.Health(context.Background()); h.State != core.HealthUnavailable {
		t.Fatalf("failed supervisor health=%+v", h)
	}
	if _, ge := s.Execute(context.Background(), core.ExecutionRequest{}, core.AttemptScope{}, core.InvocationServices{}); ge == nil || ge.Category != core.CategoryUnavailable || !strings.Contains(ge.Code, "unavailable") {
		t.Fatalf("failed supervisor admitted work: %+v", ge)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestM6SupervisorCloseCancelsRestart(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "crashed-once")
	cfg := processConfig("supervisor-crash-once")
	cfg.Args = append(cfg.Args, "--marker", marker)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	s, err := StartSupervisor(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Init(ctx, core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	epoch := s.epoch
	s.mu.RUnlock()
	deadline := time.After(5 * time.Second)
	for {
		if _, ready := s.get(); !ready {
			break
		}
		select {
		case <-deadline:
			t.Fatal("child did not enter recovery")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	s.mu.RLock()
	gotEpoch := s.epoch
	s.mu.RUnlock()
	if gotEpoch != epoch {
		t.Fatalf("closed supervisor restarted: epoch %d -> %d", epoch, gotEpoch)
	}
}
