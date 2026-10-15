//go:build linux

package conformance

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/connector/external"
	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
)

const externalConformanceToken = "PESTIROUTE_CONFORMANCE_TOKEN"

// TestConformanceExternalChildProcess is the child entry point used by Process.
func TestConformanceExternalChildProcess(t *testing.T) {
	if os.Getenv("PESTIROUTE_CONNECTOR_SOCKET") == "" {
		return
	}
	if err := external.RunNativeResponsesChild(context.Background()); err != nil {
		t.Errorf("external conformance child: %v", err)
	}
}

func externalNativeFixture(t *testing.T, response fakeupstream.Response) (fixture, func() []byte) {
	t.Helper()
	// Keep conformance children out of the shared /tmp namespace: gateway
	// lifecycle tests also assert that their own socket directories are reaped.
	tmp, err := os.MkdirTemp("", "prc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	t.Cleanup(func() {
		if err := os.RemoveAll(tmp); err != nil {
			t.Errorf("remove conformance temp directory: %v", err)
		}
	})
	upstream := fakeupstream.New(response)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	process, err := external.Start(ctx, external.ProcessConfig{
		Executable: exe, Args: []string{"-test.run=^TestConformanceExternalChildProcess$"},
		WorkingDirectory: filepath.Dir(exe),
	})
	if err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	connector, err := process.ConnectComponent(ctx)
	if err != nil {
		_ = process.Shutdown(context.Background(), nil)
		upstream.Close()
		t.Fatal(err)
	}
	config, err := json.Marshal(map[string]any{
		"transport": map[string]any{"endpoint": upstream.URL + "/v1/responses"},
		"model":     model, "account_id": account,
	})
	if err != nil {
		_ = connector.Close(context.Background())
		_ = process.Shutdown(context.Background(), nil)
		upstream.Close()
		t.Fatal(err)
	}
	t.Setenv(externalConformanceToken, "synthetic-token")
	var captured []byte
	wantBody := append([]byte(nil), response.Body...)
	if len(response.Steps) != 0 {
		wantBody = nil
		for _, step := range response.Steps {
			wantBody = append(wantBody, step.Data...)
		}
	}
	fx := fixture{
		connector: connector,
		init: func(ctx context.Context) error {
			return connector.Init(ctx, core.ComponentConfig{Data: config})
		},
		failedInit: func() error {
			return connector.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"unknown":true}`)})
		},
		request: core.ExecutionRequest{ID: request, Model: model, Payload: core.RawPayload{
			Protocol: protocol, ContentType: "application/json", Body: append([]byte(nil), opaqueRequest...),
		}},
		scope:    core.AttemptScope{ID: "conformance-attempt", AccountID: account, Mode: core.ModeNative},
		wantBody: wantBody,
		services: func() core.InvocationServices {
			client := &http.Client{Transport: &http.Transport{DisableCompression: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			return core.NewEnvironmentServices(
				map[string]map[string]string{account: {"bearer": externalConformanceToken}},
				map[string]core.HTTPDoer{account: client}, nil,
			).ForAttempt(core.AttemptScope{AccountID: account, Mode: core.ModeNative})
		},
		callCount: upstream.RequestCount,
		waitCancel: func() bool {
			var req fakeupstream.Request
			select {
			case req = <-upstream.Requests:
			case <-time.After(2 * time.Second):
				return false
			}
			select {
			case <-req.Cancelled:
			case <-time.After(2 * time.Second):
				return false
			}
			return req.CancelCount() == 1
		},
		close: func() {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer closeCancel()
			if err := connector.Close(closeCtx); err != nil {
				t.Errorf("close external fixture: %v; diagnostics=%q", err, process.Diagnostics())
			}
			if err := process.Shutdown(closeCtx, nil); err != nil {
				t.Errorf("shutdown external fixture: %v; diagnostics=%q", err, process.Diagnostics())
			}
			upstream.Close()
		},
	}
	return fx, func() []byte {
		if captured == nil {
			select {
			case req := <-upstream.Requests:
				captured = append([]byte(nil), req.Body...)
			default:
			}
		}
		return append([]byte(nil), captured...)
	}
}

func externalOpaqueFixture(t *testing.T, parts [][]byte) (fixture, func() []byte) {
	steps := make([]fakeupstream.Step, len(parts))
	for i, part := range parts {
		steps[i].Data = part
	}
	return externalNativeFixture(t, fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: steps})
}

func externalGatedFixture(t *testing.T, gate <-chan struct{}, sent chan<- struct{}) fixture {
	steps := []fakeupstream.Step{{Data: incrementalFirst}, {Gate: gate, Sent: sent, Data: incrementalTerminal}}
	fx, _ := externalNativeFixture(t, fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: steps})
	streaming := true
	fx.request.Metadata.Streaming = &streaming
	return fx
}
