//go:build linux

package external

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestM6ShutdownDrainsAdmittedStreamAndStopsAdmission(t *testing.T) {
	p := startProcess(t, "execute-fixed", startupTimeout)
	c, err := p.ConnectComponent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	response, gatewayErr := c.Execute(context.Background(), core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1"}}, core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}, authServices())
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
		t.Fatalf("Head=%+v err=%v", frame, err)
	}
	dir, socket := p.dir, p.hostServices
	closed := make(chan error, 1)
	go func() { closed <- c.Close(context.Background()) }()
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		closing := c.closing
		c.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close did not stop admission")
		}
		time.Sleep(time.Millisecond)
	}
	if got, unavailable := c.Execute(context.Background(), core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1"}}, core.AttemptScope{ID: "attempt-2", AccountID: "account", Mode: core.ModeNative}, authServices()); unavailable == nil || unavailable.Category != core.CategoryUnavailable || got.Stream != nil {
		t.Fatalf("new invocation after cutoff: response=%+v error=%+v", got, unavailable)
	}
	select {
	case err := <-closed:
		t.Fatalf("Close did not drain active stream: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	for {
		frame, err := response.Stream.Next(context.Background())
		if err != nil {
			t.Fatalf("active stream did not drain successfully: %v", err)
		}
		if frame.Type == core.FrameComplete {
			break
		}
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not finish after active stream ended")
	}
	if strings.Count(p.Diagnostics(), "lifecycle-close-called") != 1 {
		t.Fatalf("orderly child Close not called exactly once: %q", p.Diagnostics())
	}
	for _, path := range []string{socket, dir} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned path %s remains: %v", path, err)
		}
	}
	if _, err := response.Stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("Next after drained Close=%v, want EOF", err)
	}
}

func TestM6ShutdownCancelsExpiredDrain(t *testing.T) {
	p := startProcess(t, "execute-head-stall", startupTimeout)
	c, err := p.ConnectComponent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	callback := executeGateCredentials{called: make(chan struct{}, 1), release: make(chan struct{}), cancelled: make(chan struct{})}
	response, gatewayErr := c.Execute(context.Background(), core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1"}}, core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}, core.InvocationServices{Credentials: callback})
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
		t.Fatalf("Head=%+v err=%v", frame, err)
	}
	nextDone := make(chan error, 1)
	go func() { _, err := response.Stream.Next(context.Background()); nextDone <- err }()
	select {
	case <-callback.called:
	case <-time.After(2 * time.Second):
		t.Fatal("child callback was not admitted before drain expiry")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error=%v, want drain deadline", err)
	}
	select {
	case <-c.closeDone:
	case <-time.After(6 * time.Second):
		t.Fatal("expired-drain cleanup did not finish")
	}
	assertProcessReaped(t, p)
	select {
	case <-callback.cancelled:
	case <-time.After(time.Second):
		t.Fatal("expired drain did not cancel the active callback")
	}
	select {
	case <-nextDone:
	case <-time.After(2 * time.Second):
		t.Fatal("active stream did not unwind after drain expiry")
	}
	c.invocations.mu.Lock()
	active, callbacks := len(c.invocations.active), c.invocations.outstanding
	c.invocations.mu.Unlock()
	if active != 0 || callbacks != 0 {
		t.Fatalf("expired drain retained invocations=%d callbacks=%d", active, callbacks)
	}
}
