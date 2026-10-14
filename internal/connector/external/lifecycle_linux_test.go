//go:build linux

package external

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestM6LifecycleDescriptorBeforeInit(t *testing.T) {
	p := startProcess(t, "lifecycle-success", startupTimeout)
	c, err := p.ConnectComponent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d := c.Descriptor()
	if d.ID != "fixture" || d.Kind != core.ComponentConnector || !d.SupportsAPIVersion(core.APIVersion{Major: 1}) {
		t.Fatalf("descriptor before Init: %+v", d)
	}
	d.Protocols[0] = "mutated"
	if c.Descriptor().Protocols[0] != "test.v1" {
		t.Fatal("Descriptor exposed mutable cached state")
	}
	if err := c.Init(context.Background(), core.ComponentConfig{Data: bytes.Repeat([]byte{0, 0xff, 'x'}, 90<<10)}); err != nil {
		t.Fatal(err)
	}
	if health := c.Health(context.Background()); health.State != core.HealthReady {
		t.Fatalf("health=%+v", health)
	}
	stopTestProcess(t, p)
}

func TestM6LifecycleInitFailureCloses(t *testing.T) {
	p := startProcess(t, "lifecycle-init-fail", startupTimeout)
	c, err := p.ConnectComponent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = c.Init(context.Background(), core.ComponentConfig{Data: []byte("private config")})
	if err == nil || strings.Contains(err.Error(), "private child error") {
		t.Fatalf("Init error is absent or leaked child diagnostic: %v", err)
	}
	assertProcessReaped(t, p)
	if !strings.Contains(p.Diagnostics(), "lifecycle-close-called") {
		t.Fatalf("partial initialization did not call child Close; diagnostics=%q", p.Diagnostics())
	}
	if health := c.Health(context.Background()); health.State != core.HealthUnavailable {
		t.Fatalf("failed Init health=%+v", health)
	}
}

func TestM6LifecycleInitCallerDeadline(t *testing.T) {
	p := startProcess(t, "lifecycle-init-stall", startupTimeout)
	c, err := p.ConnectComponent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := c.Init(ctx, core.ComponentConfig{Data: []byte("config")}); err == nil || time.Since(started) > 6*time.Second {
		t.Fatalf("Init caller deadline not enforced: err=%v elapsed=%v", err, time.Since(started))
	}
	assertProcessReaped(t, p)
}

func TestM6LifecycleHealthStates(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want core.HealthState
	}{{"lifecycle-success", core.HealthReady}, {"lifecycle-health-unavailable", core.HealthUnavailable}, {"lifecycle-health-unknown", core.HealthUnknown}} {
		t.Run(tc.mode, func(t *testing.T) {
			p := startProcess(t, tc.mode, startupTimeout)
			c, err := p.ConnectComponent(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
				t.Fatal(err)
			}
			health := c.Health(context.Background())
			want := safeHealth(core.Health{State: tc.want})
			if health != want {
				t.Fatalf("health=%+v", health)
			}
			stopTestProcess(t, p)
		})
	}
}

func TestM6LifecycleHealthDiagnosticSanitization(t *testing.T) {
	for _, mode := range []string{"lifecycle-health-malicious", "lifecycle-health-oversized"} {
		t.Run(mode, func(t *testing.T) {
			p := startProcess(t, mode, startupTimeout)
			c, err := p.ConnectComponent(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
				t.Fatal(err)
			}
			health := c.Health(context.Background())
			if health.State != core.HealthReady || !utf8.ValidString(health.Diagnostic) || len(health.Diagnostic) > maxHealthDiagnosticBytes || strings.Contains(health.Diagnostic, "private-health-secret") {
				t.Fatalf("unsafe child health diagnostic escaped: state=%s bytes=%d diagnostic=%q", health.State, len(health.Diagnostic), health.Diagnostic)
			}
			stopTestProcess(t, p)
		})
	}
}

func TestM6LifecycleHealthTimeout(t *testing.T) {
	p := startProcess(t, "lifecycle-health-stall", startupTimeout)
	c, err := p.ConnectComponent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	health := c.Health(ctx)
	if health.State != core.HealthUnavailable || time.Since(started) > time.Second {
		t.Fatalf("health timeout not bounded: health=%+v elapsed=%v", health, time.Since(started))
	}
	stopTestProcess(t, p)
}

func TestM6LifecycleCapabilities(t *testing.T) {
	p := startProcess(t, "lifecycle-success", startupTimeout)
	c, err := p.ConnectComponent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	result := c.Capabilities(context.Background(), core.CapabilityScope{Protocol: "test.v1", Mode: "native", Model: "fixture-model", AccountID: "acct"})
	if result.State("llm.streaming") != core.Supported || result.State("llm.tools") != core.Unknown || result.State("llm.audio") != core.Unknown {
		t.Fatalf("capabilities=%+v", result.Values)
	}
	invalid := c.Capabilities(context.Background(), core.CapabilityScope{Protocol: "", Mode: "native"})
	if len(invalid.Values) != 0 {
		t.Fatalf("invalid scope was sent: %+v", invalid)
	}
	invalid = c.Capabilities(context.Background(), core.CapabilityScope{Protocol: "test.v1", Mode: "mystery"})
	if len(invalid.Values) != 0 {
		t.Fatalf("unknown mode was sent: %+v", invalid)
	}
	if strings.Count(p.Diagnostics(), "lifecycle-capabilities-called") != 1 {
		t.Fatalf("invalid scope reached child: %q", p.Diagnostics())
	}
	stopTestProcess(t, p)
}

func TestM6LifecycleCloseCallersHonorContexts(t *testing.T) {
	p := startProcess(t, "lifecycle-close-delay", startupTimeout)
	c, err := p.ConnectComponent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ownerDone := make(chan error, 1)
	go func() { ownerDone <- c.Close(context.Background()) }()
	waitForDiagnostic(t, p, "lifecycle-close-called")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := c.Close(cancelled); !errors.Is(err, context.Canceled) || time.Since(started) > 100*time.Millisecond {
		t.Fatalf("cancelled Close did not return promptly: err=%v elapsed=%v", err, time.Since(started))
	}
	deadline, deadlineCancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer deadlineCancel()
	started = time.Now()
	if err := c.Close(deadline); !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 200*time.Millisecond {
		t.Fatalf("deadline Close did not return promptly: err=%v elapsed=%v", err, time.Since(started))
	}
	select {
	case err := <-ownerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("cleanup owner did not finish")
	}
	if strings.Count(p.Diagnostics(), "lifecycle-close-called") != 1 {
		t.Fatalf("Close cleanup was not exactly once: %q", p.Diagnostics())
	}
	assertProcessReaped(t, p)
}

func waitForDiagnostic(t *testing.T, p *Process, marker string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(p.Diagnostics(), marker) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("child did not emit %q before deadline", marker)
}

func TestM6LifecycleCapabilitiesFragments(t *testing.T) {
	p := startProcess(t, "lifecycle-capabilities-large", startupTimeout)
	c, err := p.ConnectComponent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	result := c.Capabilities(context.Background(), core.CapabilityScope{Protocol: "test.v1", Mode: "native", Model: strings.Repeat("m", 90<<10)})
	if len(result.Values) != 5000 || result.State("test.capability.4999") != core.Supported {
		t.Fatalf("fragmented capability result lost data: count=%d", len(result.Values))
	}
	stopTestProcess(t, p)
}

func TestM6LifecycleCloseIdempotent(t *testing.T) {
	p := startProcess(t, "lifecycle-success", startupTimeout)
	c, err := p.ConnectComponent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatalf("repeated Close: %v", err)
	}
	assertProcessReaped(t, p)
	if _, err := os.Stat(p.dir); !os.IsNotExist(err) {
		t.Fatalf("socket directory remains: %v", err)
	}
}

func TestM6LifecycleCloseSuppressesRepeatedFailure(t *testing.T) {
	p := startProcess(t, "lifecycle-close-fail", startupTimeout)
	c, err := p.ConnectComponent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = c.Close(ctx)
	if err == nil || strings.Contains(err.Error(), "private child error") {
		t.Fatalf("Close error is absent or leaked child diagnostic: %v", err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatalf("repeated Close did not suppress prior failure: %v", err)
	}
	assertProcessReaped(t, p)
}
