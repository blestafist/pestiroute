//go:build linux

package external

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/ipc"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type executeGateCredentials struct {
	called    chan struct{}
	release   chan struct{}
	cancelled chan struct{}
}

type executeGateTransport struct {
	called    chan struct{}
	cancelled chan struct{}
	once      sync.Once
}

func (t *executeGateTransport) Do(req *http.Request) (*http.Response, error) {
	t.called <- struct{}{}
	<-req.Context().Done()
	t.once.Do(func() { close(t.cancelled) })
	return nil, req.Context().Err()
}

func (c executeGateCredentials) Get(ctx context.Context, name string) ([]byte, error) {
	if name != "token" {
		return nil, context.Canceled
	}
	select {
	case c.called <- struct{}{}:
	default:
	}
	select {
	case <-c.release:
		return []byte("released"), nil
	case <-ctx.Done():
		if c.cancelled != nil {
			close(c.cancelled)
		}
		return nil, ctx.Err()
	}
}

func TestM6ExecuteCancel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deadline bool
	}{
		{name: "before head cancel"},
		{name: "before head deadline", deadline: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := startProcess(t, "execute-stall", startupTimeout)
			c, err := p.ConnectComponent(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			if tc.deadline {
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			}
			defer cancel()
			finished := make(chan *core.GatewayError, 1)
			go func() {
				response, gatewayErr := c.Execute(ctx, core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1"}}, core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}, core.InvocationServices{})
				if response.Stream != nil {
					_ = response.Stream.Close()
				}
				finished <- gatewayErr
			}()
			waitForDiagnostic(t, p, "execute-stall-called")
			requestedAt := time.Now()
			if !tc.deadline {
				cancel()
			} else {
				<-ctx.Done()
				requestedAt = time.Now()
			}
			select {
			case got := <-finished:
				want := core.CategoryCancelled
				wantCode := "execute_cancelled"
				if tc.deadline {
					want = core.CategoryTimeout
					wantCode = "connector_timeout"
				}
				if got == nil || got.Category != want || got.Code != wantCode {
					t.Fatalf("Execute error=%+v want %s/%s", got, wantCode, want)
				}
				if len(c.gate) != 0 {
					t.Fatal("Execute acquisition gate was not released")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Execute did not unwind within cancellation bound")
			}
			waitForDiagnostic(t, p, "execute-stall-cancel-ack")
			if elapsed := time.Since(requestedAt); elapsed > 2*time.Second {
				t.Fatalf("child cancellation acknowledgement took %s", elapsed)
			}
		})
	}

	for _, deadline := range []bool{false, true} {
		name, want := "after head cancel", error(context.Canceled)
		if deadline {
			name, want = "after head deadline", context.DeadlineExceeded
		}
		t.Run(name, func(t *testing.T) {
			p := startProcess(t, "execute-head-stall", startupTimeout)
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
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			}
			defer cancel()
			waitForDiagnostic(t, p, "execute-posthead-authorized")
			finished := make(chan error, 1)
			started := make(chan struct{})
			go func() { close(started); _, err := response.Stream.Next(ctx); finished <- err }()
			<-started
			requestedAt := time.Now()
			if !deadline {
				cancel()
			} else {
				<-ctx.Done()
				requestedAt = time.Now()
			}
			select {
			case err := <-finished:
				if !errors.Is(err, want) {
					t.Fatalf("Next error=%v want %v", err, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Next did not unwind within cancellation bound")
			}
			waitForDiagnostic(t, p, "execute-posthead-revoked")
			if elapsed := time.Since(requestedAt); elapsed > 2*time.Second {
				t.Fatalf("child cancellation acknowledgement took %s", elapsed)
			}
			if len(c.gate) != 0 {
				t.Fatal("stream acquisition gate was not released")
			}
			if err := response.Stream.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := response.Stream.Next(context.Background()); !errors.Is(err, io.EOF) {
				t.Fatalf("Next after Close=%v, want EOF", err)
			}
		})
	}

	for _, callbackKind := range []string{"credential", "http"} {
		for _, deadline := range []bool{false, true} {
			name := callbackKind + " parent cancel"
			if deadline {
				name = callbackKind + " parent deadline"
			}
			t.Run(name, func(t *testing.T) {
				mode := "execute-callback-" + callbackKind
				p := startProcess(t, mode, startupTimeout)
				c, err := p.ConnectComponent(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
					t.Fatal(err)
				}
				var ctx context.Context
				var cancel context.CancelFunc
				if deadline {
					ctx, cancel = context.WithTimeout(context.Background(), time.Second)
				} else {
					ctx, cancel = context.WithCancel(context.Background())
				}
				defer cancel()
				services := core.InvocationServices{}
				var entered, released <-chan struct{}
				if callbackKind == "credential" {
					callback := executeGateCredentials{called: make(chan struct{}, 1), release: make(chan struct{}), cancelled: make(chan struct{})}
					services.Credentials = callback
					entered, released = callback.called, callback.cancelled
				} else {
					callback := &executeGateTransport{called: make(chan struct{}, 1), cancelled: make(chan struct{})}
					services.Transport = callback
					entered, released = callback.called, callback.cancelled
				}
				response, gatewayErr := c.Execute(ctx, core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1"}}, core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}, services)
				if gatewayErr != nil {
					t.Fatal(gatewayErr)
				}
				if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
					t.Fatalf("Head=%+v err=%v", frame, err)
				}
				select {
				case <-entered:
				case <-time.After(2 * time.Second):
					t.Fatal("reverse callback was not admitted")
				}
				nextDone := make(chan error, 1)
				nextStarted := make(chan struct{})
				go func() { close(nextStarted); _, err := response.Stream.Next(context.Background()); nextDone <- err }()
				<-nextStarted
				requestedAt := time.Now()
				if !deadline {
					cancel()
				} else {
					<-ctx.Done()
					requestedAt = time.Now()
				}
				select {
				case <-released:
				case <-time.After(2 * time.Second):
					t.Fatal("parent cancellation did not release stalled callback")
				}
				waitForDiagnostic(t, p, "execute-"+callbackKind+"-cancel-ack")
				waitForDiagnostic(t, p, "execute-late-callback-denied")
				if elapsed := time.Since(requestedAt); elapsed > 2*time.Second {
					t.Fatalf("child/callback cancellation acknowledgement took %s", elapsed)
				}
				select {
				case nextErr := <-nextDone:
					wantErr := error(context.Canceled)
					if deadline {
						wantErr = context.DeadlineExceeded
					}
					if !errors.Is(nextErr, wantErr) {
						t.Fatalf("Next error=%v, want %v", nextErr, wantErr)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("Next did not unblock after parent cancellation")
				}
				c.invocations.mu.Lock()
				active, callbacks := len(c.invocations.active), c.invocations.outstanding
				c.invocations.mu.Unlock()
				if active != 0 || callbacks != 0 || len(c.gate) != 0 {
					t.Fatalf("resources retained: invocations=%d callbacks=%d gate=%d", active, callbacks, len(c.gate))
				}
				if err := response.Stream.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := response.Stream.Next(context.Background()); !errors.Is(err, io.EOF) {
					t.Fatalf("Next after cancellation=%v, want EOF", err)
				}
			})
		}
	}

	t.Run("concurrent idempotent close", func(t *testing.T) {
		p := startProcess(t, "execute-head-stall", startupTimeout)
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
		waitForDiagnostic(t, p, "execute-posthead-authorized")
		nextDone := make(chan struct{})
		go func() { _, _ = response.Stream.Next(context.Background()); close(nextDone) }()
		started := make(chan struct{})
		go func() { _ = response.Stream.Close(); _ = response.Stream.Close(); close(started) }()
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("Close blocked")
		}
		select {
		case <-nextDone:
		case <-time.After(2 * time.Second):
			t.Fatal("Next did not unblock")
		}
		if len(c.gate) != 0 {
			t.Fatal("acquisition gate was not released")
		}
		if _, err := response.Stream.Next(context.Background()); !errors.Is(err, io.EOF) {
			t.Fatalf("Next after Close=%v, want EOF", err)
		}
	})
}

func TestM6ExecuteIncrementalBodyArrivesBeforeProducerCompletes(t *testing.T) {
	p := startProcess(t, "execute-incremental", startupTimeout)
	c, err := p.ConnectComponent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte{0x80}, 2*ipc.MaxFragmentBytes+17)
	copy(body[ipc.MaxFragmentBytes-1:], []byte("€\x00\xff"))
	gate := executeGateCredentials{called: make(chan struct{}, 1), release: make(chan struct{})}
	response, gatewayErr := c.Execute(context.Background(), core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1", Body: body}}, core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}, core.InvocationServices{Credentials: gate, Transport: authTransport{}, Logger: slog.Default()})
	if gatewayErr != nil || response.Stream == nil {
		t.Fatalf("Execute response=%+v error=%+v", response, gatewayErr)
	}
	defer response.Stream.Close()
	if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
		t.Fatalf("Head=%+v err=%v", frame, err)
	}
	first, err := response.Stream.Next(context.Background())
	if err != nil || first.Type != core.FrameBody || !bytes.Equal(first.Body.Data, body[:ipc.MaxFragmentBytes]) {
		t.Fatalf("first partial Body=%+v err=%v", first, err)
	}
	select {
	case <-gate.called:
	case <-time.After(2 * time.Second):
		t.Fatal("child did not reach its post-partial-body gate")
	}
	close(gate.release)
	output := append([]byte(nil), first.Body.Data...)
	for {
		frame, err := response.Stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch frame.Type {
		case core.FrameBody:
			output = append(output, frame.Body.Data...)
		case core.FrameComplete:
			if frame.Complete.Outcome != core.OutcomeSucceeded || frame.Complete.Usage == nil || frame.Complete.Usage.InputTokens == nil || *frame.Complete.Usage.InputTokens != 12 {
				t.Fatalf("Complete=%+v", frame.Complete)
			}
		default:
			t.Fatalf("unexpected frame: %+v", frame)
		}
	}
	if !bytes.Equal(output, body) {
		t.Fatalf("opaque body changed: got %d bytes want %d", len(output), len(body))
	}
}

func TestM6FaultExecuteIncrementalTerminalOutcomesAndMalformedOrder(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want core.Outcome
	}{
		{"execute-incremental-failed", core.OutcomeFailed},
		{"execute-incremental-cancelled", core.OutcomeCancelled},
		{"execute-incremental-incomplete", core.OutcomeIncomplete},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			p := startProcess(t, tc.mode, startupTimeout)
			c, err := p.ConnectComponent(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
				t.Fatal(err)
			}
			response, gatewayErr := c.Execute(context.Background(), core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1", Body: []byte("body")}}, core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}, core.InvocationServices{})
			if gatewayErr != nil || response.Stream == nil {
				t.Fatalf("Execute response=%+v error=%+v", response, gatewayErr)
			}
			defer response.Stream.Close()
			for _, typ := range []core.FrameType{core.FrameHead, core.FrameBody} {
				frame, err := response.Stream.Next(context.Background())
				if err != nil || frame.Type != typ {
					t.Fatalf("frame=%+v err=%v want %s", frame, err, typ)
				}
			}
			frame, err := response.Stream.Next(context.Background())
			if err != nil || frame.Type != core.FrameComplete || frame.Complete.Outcome != tc.want || frame.Complete.Usage == nil || frame.Complete.Usage.InputTokens == nil || *frame.Complete.Usage.InputTokens != 12 || frame.Complete.Error == nil {
				t.Fatalf("Complete=%+v err=%v", frame, err)
			}
		})
	}
	for _, mode := range []string{"execute-incremental-before-head", "execute-incremental-after-complete", "execute-incremental-duplicate-complete"} {
		t.Run(mode, func(t *testing.T) {
			p := startProcess(t, mode, startupTimeout)
			c, err := p.ConnectComponent(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
				t.Fatal(err)
			}
			response, gatewayErr := c.Execute(context.Background(), core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1", Body: []byte("body")}}, core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}, core.InvocationServices{Credentials: authCredentials{account: "account"}, Transport: authTransport{}, Logger: slog.Default()})
			if mode == "execute-incremental-before-head" {
				if gatewayErr == nil || response.Stream != nil {
					t.Fatalf("accepted body before Head: response=%+v error=%+v", response, gatewayErr)
				}
				return
			}
			if gatewayErr != nil || response.Stream == nil {
				t.Fatalf("Execute response=%+v error=%+v", response, gatewayErr)
			}
			defer response.Stream.Close()
			_, err = response.Stream.Next(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for {
				_, err = response.Stream.Next(context.Background())
				if err != nil {
					break
				}
			}
			if err == nil || errors.Is(err, io.EOF) {
				t.Fatalf("accepted invalid trailing frame sequence: %v", err)
			}
		})
	}
}

func TestM6ExecuteFixedRoundTripsRequestResponseAndEOF(t *testing.T) {
	input := append([]byte{0, 0xff, '\n'}, bytes.Repeat([]byte{0, 1, 0xfe, 'x'}, 40<<10)...)
	p := startProcess(t, "execute-fixed", startupTimeout)
	c, err := p.ConnectComponent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	statusCode := 207
	response, gatewayErr := c.Execute(context.Background(), core.ExecutionRequest{ID: "runtime-id", Model: "fixture", Payload: core.RawPayload{Protocol: "test.v1", ContentType: "application/octet-stream", Body: input}}, core.AttemptScope{ID: "attempt-1", AccountID: "account", Mode: core.ModeNative, MaxBodyBytes: int64(len(input))}, core.InvocationServices{})
	if gatewayErr != nil || response.Stream == nil {
		t.Fatalf("Execute response=%+v error=%+v", response, gatewayErr)
	}
	frame, err := response.Stream.Next(context.Background())
	if err != nil || frame.Type != core.FrameHead || frame.Head.HTTPStatus == nil || *frame.Head.HTTPStatus != statusCode || frame.Head.Protocol != "test.v1" || frame.Head.Headers["X-Fixture"][0] != "fixed" {
		t.Fatalf("head=%+v err=%v", frame, err)
	}
	var output []byte
	for {
		frame, err = response.Stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch frame.Type {
		case core.FrameBody:
			output = append(output, frame.Body.Data...)
		case core.FrameComplete:
			if frame.Complete.Outcome != core.OutcomeSucceeded || frame.Complete.Usage == nil || frame.Complete.Usage.InputTokens == nil || *frame.Complete.Usage.InputTokens != 12 {
				t.Fatalf("complete=%+v", frame.Complete)
			}
		default:
			t.Fatalf("unexpected frame: %+v", frame)
		}
	}
	if !bytes.Equal(output, input) {
		t.Fatalf("opaque body changed: got %d bytes want %d", len(output), len(input))
	}
	if err := response.Stream.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestM6ExecuteFixedPreHeadGatewayErrorAndProtocolFailure(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want *core.GatewayError
	}{
		{"execute-error", &core.GatewayError{Code: "rejected", Category: core.CategoryRateLimited, Retryable: true, RetryDisposition: core.RetrySafe, Provider: "fixture", OriginalError: "diagnostic", Message: "try later"}},
		{"execute-bad-epoch", nil},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			p := startProcess(t, tc.mode, startupTimeout)
			c, err := p.ConnectComponent(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
				t.Fatal(err)
			}
			response, got := c.Execute(context.Background(), core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1"}}, core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}, core.InvocationServices{})
			if response.Stream != nil {
				t.Fatal("pre-head failure returned a stream")
			}
			if tc.want == nil {
				if got == nil || got.Category != core.CategoryInvalidRequest || got.RetryDisposition != core.RetryUnsafe {
					t.Fatalf("malformed response error=%+v", got)
				}
				return
			}
			if got == nil || got.Code != tc.want.Code || got.Category != tc.want.Category || got.Retryable != tc.want.Retryable || got.RetryDisposition != tc.want.RetryDisposition || got.Provider != tc.want.Provider || got.OriginalError != tc.want.OriginalError || got.Message != tc.want.Message {
				t.Fatalf("GatewayError=%+v want %+v", got, tc.want)
			}
		})
	}
}

func TestM6FaultChildCrashBeforeAndAfterHead(t *testing.T) {
	for _, mode := range []string{"execute-crash-before-head", "execute-crash-after-head"} {
		t.Run(mode, func(t *testing.T) {
			p := startProcess(t, mode, startupTimeout)
			c, err := p.ConnectComponent(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			response, gatewayErr := c.Execute(context.Background(), core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1"}}, core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}, core.InvocationServices{})
			if mode == "execute-crash-before-head" {
				if gatewayErr == nil || gatewayErr.Category != core.CategoryUnavailable || gatewayErr.RetryDisposition == core.RetrySafe || response.Stream != nil {
					t.Fatalf("pre-Head crash response=%+v error=%+v", response, gatewayErr)
				}
			} else {
				if gatewayErr != nil || response.Stream == nil {
					t.Fatalf("post-Head crash response=%+v error=%+v", response, gatewayErr)
				}
				if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
					t.Fatalf("Head=%+v err=%v", frame, err)
				}
				_, err := response.Stream.Next(context.Background())
				if err == nil {
					t.Fatal("crash after Head fabricated successful stream termination")
				}
				if time.Since(started) > 2*time.Second {
					t.Fatal("post-Head crash did not promptly unblock Next")
				}
			}
			c.invocations.mu.Lock()
			active := len(c.invocations.active)
			c.invocations.mu.Unlock()
			frames, bytes := c.invocations.credits.usage()
			if active != 0 || frames != 0 || bytes != 0 {
				t.Fatalf("crash leaked invocation/credits: active=%d frames=%d bytes=%d", active, frames, bytes)
			}
		})
	}
}

func TestM6FaultBeforeSendCancellation(t *testing.T) {
	p := startProcess(t, "execute-fixed", startupTimeout)
	c, err := p.ConnectComponent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response, gatewayErr := c.Execute(ctx, core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1"}}, core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}, core.InvocationServices{})
	if response.Stream != nil || gatewayErr == nil || gatewayErr.Category != core.CategoryCancelled {
		t.Fatalf("pre-send cancellation response=%+v error=%+v", response, gatewayErr)
	}
	c.invocations.mu.Lock()
	active := len(c.invocations.active)
	c.invocations.mu.Unlock()
	if active != 0 {
		t.Fatalf("pre-send cancellation leaked invocation: active=%d", active)
	}
}

func TestM6FaultRemoteCancelAndDeadlineAreUnavailable(t *testing.T) {
	for _, tc := range []struct {
		mode, marker string
	}{
		{"execute-child-cancelled-before-head", "execute-child-cancelled-before-head"},
		{"execute-child-deadline-before-head", "execute-child-deadline-before-head"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			p := startProcess(t, tc.mode, startupTimeout)
			c, err := p.ConnectComponent(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
				t.Fatal(err)
			}
			response, got := c.Execute(context.Background(), core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1"}}, core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}, core.InvocationServices{})
			if response.Stream != nil || got == nil || got.Category != core.CategoryUnavailable || got.RetryDisposition != core.RetryUnknown {
				t.Fatalf("remote status response=%+v error=%+v", response, got)
			}
			if count := strings.Count(p.Diagnostics(), tc.marker); count != 1 {
				t.Fatalf("child Execute count=%d; want exactly one; diagnostics=%q", count, p.Diagnostics())
			}
		})
	}
}

func TestM6FaultRemoteCancelAndDeadlineAfterHead(t *testing.T) {
	for _, tc := range []struct {
		mode, marker string
	}{
		{"execute-child-cancelled-after-head", "execute-child-cancelled-after-head"},
		{"execute-child-deadline-after-head", "execute-child-deadline-after-head"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			p := startProcess(t, tc.mode, startupTimeout)
			c, err := p.ConnectComponent(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
				t.Fatal(err)
			}
			response, gatewayErr := c.Execute(context.Background(), core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1"}}, core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}, core.InvocationServices{})
			if gatewayErr != nil || response.Stream == nil {
				t.Fatalf("Execute response=%+v error=%+v", response, gatewayErr)
			}
			if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
				t.Fatalf("Head=%+v err=%v", frame, err)
			}
			_, err = response.Stream.Next(context.Background())
			var got *core.GatewayError
			if !errors.As(err, &got) || got.Category != core.CategoryUnavailable || got.RetryDisposition != core.RetryUnknown {
				t.Fatalf("remote post-Head status error=%T %v", err, err)
			}
			if _, err := response.Stream.Next(context.Background()); !errors.Is(err, io.EOF) {
				t.Fatalf("failed stream was not closed: %v", err)
			}
			c.invocations.mu.Lock()
			active := len(c.invocations.active)
			c.invocations.mu.Unlock()
			frames, bytes := c.invocations.credits.usage()
			if active != 0 || frames != 0 || bytes != 0 {
				t.Fatalf("post-Head status leaked invocation/credits: active=%d frames=%d bytes=%d", active, frames, bytes)
			}
			if count := strings.Count(p.Diagnostics(), tc.marker); count != 1 {
				t.Fatalf("child Execute count=%d; want exactly one; diagnostics=%q", count, p.Diagnostics())
			}
		})
	}
}

func TestM6FaultCallerDeadlineBeforeAndAfterHead(t *testing.T) {
	t.Run("before Head", func(t *testing.T) {
		p := startProcess(t, "execute-stall", startupTimeout)
		c, err := p.ConnectComponent(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		_, got := c.Execute(ctx, core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1"}}, core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}, core.InvocationServices{})
		if got == nil || got.Category != core.CategoryTimeout {
			t.Fatalf("caller deadline error=%+v", got)
		}
	})
	t.Run("after Head", func(t *testing.T) {
		p := startProcess(t, "execute-head-stall", startupTimeout)
		c, err := p.ConnectComponent(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
			t.Fatal(err)
		}
		response, got := c.Execute(context.Background(), core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1"}}, core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}, authServices())
		if got != nil || response.Stream == nil {
			t.Fatalf("Execute response=%+v error=%+v", response, got)
		}
		if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
			t.Fatalf("Head=%+v err=%v", frame, err)
		}
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		if _, err := response.Stream.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("post-Head caller deadline error=%v", err)
		}
	})
}

func TestM6FaultMalformedExecuteStreams(t *testing.T) {
	for _, mode := range []string{"execute-bad-epoch", "execute-incremental-before-head", "execute-bad-sequence", "execute-eof-after-head"} {
		t.Run(mode, func(t *testing.T) {
			p := startProcess(t, mode, startupTimeout)
			c, err := p.ConnectComponent(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
				t.Fatal(err)
			}
			response, gatewayErr := c.Execute(context.Background(), core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1"}}, core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}, core.InvocationServices{})
			if response.Stream == nil {
				if gatewayErr == nil || gatewayErr.Category != core.CategoryInvalidRequest || gatewayErr.RetryDisposition != core.RetryUnsafe {
					t.Fatalf("%s malformed pre-Head response error=%+v diagnostics=%s", mode, gatewayErr, p.Diagnostics())
				}
				return
			}
			if gatewayErr != nil {
				t.Fatal(gatewayErr)
			}
			if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
				t.Fatalf("Head=%+v err=%v", frame, err)
			}
			for {
				frame, err := response.Stream.Next(context.Background())
				if err != nil {
					var ge *core.GatewayError
					if !errors.As(err, &ge) || ge.Category != core.CategoryInvalidRequest || ge.RetryDisposition != core.RetryUnsafe {
						t.Fatalf("malformed stream error=%T %+v category=%v retry=%v code=%s", err, err, ge.Category, ge.RetryDisposition, ge.Code)
					}
					return
				}
				if frame.Type == core.FrameComplete {
					t.Fatalf("malformed stream synthesized Complete: %+v", frame.Complete)
				}
			}
		})
	}
}

func TestM6ExecuteFixedEmptyBodies(t *testing.T) {
	p := startProcess(t, "execute-fixed", startupTimeout)
	c, err := p.ConnectComponent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	response, gatewayErr := c.Execute(context.Background(), core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1"}}, core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}, core.InvocationServices{})
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	for {
		frame, err := response.Stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameBody {
			t.Fatalf("empty body produced a chunk: %+v", frame)
		}
	}
}

func TestM6FaultCallerCancellationBeforeHead(t *testing.T) {
	p := startProcess(t, "execute-stall", startupTimeout)
	c, err := p.ConnectComponent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *core.GatewayError, 1)
	go func() {
		response, gatewayErr := c.Execute(ctx, core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1"}}, core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}, core.InvocationServices{})
		if response.Stream != nil {
			_ = response.Stream.Close()
		}
		done <- gatewayErr
	}()
	waitForDiagnostic(t, p, "execute-stall-called")
	cancel()
	select {
	case gatewayErr := <-done:
		if gatewayErr == nil || gatewayErr.Category != core.CategoryCancelled {
			t.Fatalf("cancel result=%+v", gatewayErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled Execute remained blocked before Head")
	}
}

func TestM6ExecuteFixedNextCancellationClosesInvocation(t *testing.T) {
	p := startProcess(t, "execute-head-stall", startupTimeout)
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
	defer response.Stream.Close()
	if frame, err := response.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
		t.Fatalf("Head=%+v err=%v", frame, err)
	}
	waitForDiagnostic(t, p, "execute-posthead-authorized")
	c.invocations.mu.Lock()
	active := len(c.invocations.active)
	c.invocations.mu.Unlock()
	if active != 1 {
		t.Fatalf("active invocation before cancellation=%d, want 1", active)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, err := response.Stream.Next(ctx)
		finished <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked Next cancellation error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next remained blocked after its context was cancelled")
	}
	waitForDiagnostic(t, p, "execute-posthead-revoked")
	c.invocations.mu.Lock()
	active = len(c.invocations.active)
	c.invocations.mu.Unlock()
	if active != 0 {
		t.Fatalf("cancelled Execute invocation remains active: %d", active)
	}
}

func TestM6ExecuteFixedEnforcesAdmittedBodyLimit(t *testing.T) {
	t.Run("host rejects before sending", func(t *testing.T) {
		p := startProcess(t, "execute-fixed", startupTimeout)
		c, err := p.ConnectComponent(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
			t.Fatal(err)
		}
		_, got := c.Execute(context.Background(), core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1", Body: []byte("too large")}}, core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative, MaxBodyBytes: 1}, core.InvocationServices{})
		if got == nil || got.Category != core.CategoryInvalidRequest {
			t.Fatalf("over-limit request error=%+v", got)
		}
	})
	t.Run("child rejects excess streamed chunks", func(t *testing.T) {
		p := startProcess(t, "execute-fixed", startupTimeout)
		c, err := p.ConnectComponent(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
			t.Fatal(err)
		}
		stream, err := c.client.Execute(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		const invocationID = "execute-limit-test"
		start := &gen.ExecuteStart{Scope: ipc.AttemptTo(core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}), MaxBodyBytes: 1}
		start.Request, err = ipc.ExecutionRequestTo(core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := ipc.WriteRecord(ipc.Identity{Epoch: c.epoch, CallID: 1, RecordID: 0, Type: "ExecuteStart"}, start, func(f *gen.RecordFragment) error {
			f.InvocationId = invocationID
			return stream.Send(&gen.ExecuteFrame{Epoch: c.epoch, CallId: 1, InvocationId: invocationID, Payload: &gen.ExecuteFrame_Record{Record: f}})
		}); err != nil {
			t.Fatal(err)
		}
		chunk, err := ipc.BodyChunkTo([]byte("12"))
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&gen.ExecuteFrame{Epoch: c.epoch, CallId: 1, InvocationId: invocationID, Payload: &gen.ExecuteFrame_Body{Body: chunk}}); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("child over-limit result error=%v", err)
		}
	})
}

func TestM6ExecuteFixedRejectsMalformedChildRequests(t *testing.T) {
	for _, tc := range []struct {
		name        string
		badFragment bool
	}{
		{name: "mismatched invocation"},
		{name: "bad start sequence", badFragment: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := startProcess(t, "execute-fixed", startupTimeout)
			c, err := p.ConnectComponent(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Init(context.Background(), core.ComponentConfig{}); err != nil {
				t.Fatal(err)
			}
			stream, err := c.client.Execute(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			const invocationID = "execute-malformed-test"
			if tc.badFragment {
				f := &gen.RecordFragment{Epoch: c.epoch, CallId: 1, RecordId: 0, RecordType: "ExecuteStart", TotalLength: 1, Sequence: 1, Final: true, Data: []byte{0}, InvocationId: invocationID}
				if err := stream.Send(&gen.ExecuteFrame{Epoch: c.epoch, CallId: 1, InvocationId: invocationID, Payload: &gen.ExecuteFrame_Record{Record: f}}); err != nil {
					t.Fatal(err)
				}
			} else {
				start := &gen.ExecuteStart{Scope: ipc.AttemptTo(core.AttemptScope{ID: "attempt", AccountID: "account", Mode: core.ModeNative}), MaxBodyBytes: 1}
				start.Request, err = ipc.ExecutionRequestTo(core.ExecutionRequest{Payload: core.RawPayload{Protocol: "test.v1"}})
				if err != nil {
					t.Fatal(err)
				}
				if err := ipc.WriteRecord(ipc.Identity{Epoch: c.epoch, CallID: 1, RecordID: 0, Type: "ExecuteStart"}, start, func(f *gen.RecordFragment) error {
					f.InvocationId = invocationID
					return stream.Send(&gen.ExecuteFrame{Epoch: c.epoch, CallId: 1, InvocationId: invocationID, Payload: &gen.ExecuteFrame_Record{Record: f}})
				}); err != nil {
					t.Fatal(err)
				}
				chunk, err := ipc.BodyChunkTo([]byte("x"))
				if err != nil {
					t.Fatal(err)
				}
				if err := stream.Send(&gen.ExecuteFrame{Epoch: c.epoch, CallId: 1, InvocationId: "wrong-invocation", Payload: &gen.ExecuteFrame_Body{Body: chunk}}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("malformed Execute request error=%v", err)
			}
		})
	}
}
