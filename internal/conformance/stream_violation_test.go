package conformance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestConformanceStreamViolationMatrix(t *testing.T) {
	usageTokens := int64(23)
	usage := &core.UsageReport{InputTokens: &usageTokens, Source: core.UsageProvider, Completeness: core.UsagePartial}
	errCause := &core.GatewayError{Code: "rejected", Category: core.CategoryUnavailable}
	body := core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: []byte("prefix")}}
	badBody := core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: []byte("BAD")}}
	goodHead, goodComplete := headFrame(), completeFrame()
	wrongHead := headFrame()
	wrongHead.Head.Protocol = "other"
	undeclaredHead := headFrame()
	undeclaredHead.Head.Protocol = "undeclared.protocol"
	errorHead := headFrame()
	errorHead.Head.Error = errCause
	badSuccess := completeFrame()
	badSuccess.Complete.Error = errCause
	badFailed := core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeFailed}}
	badOutcome := core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: "future", Error: errCause}}
	withUsage := goodComplete
	withUsage.Complete.Usage = usage

	cases := []struct {
		name       string
		frames     []core.StreamFrame
		readErr    error
		wantPrefix []byte
		wantUsage  bool
	}{
		{name: "duplicate-head", frames: []core.StreamFrame{goodHead, body, goodHead}, wantPrefix: []byte("prefix")},
		{name: "undeclared-protocol", frames: []core.StreamFrame{undeclaredHead}},
		{name: "empty-protocol", frames: []core.StreamFrame{{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: ""}}}},
		{name: "mismatched-protocol", frames: []core.StreamFrame{wrongHead}},
		{name: "head-nil-payload", frames: []core.StreamFrame{{Type: core.FrameHead}}},
		{name: "head-extra-body", frames: []core.StreamFrame{{Type: core.FrameHead, Head: goodHead.Head, Body: body.Body}}},
		{name: "head-extra-complete", frames: []core.StreamFrame{{Type: core.FrameHead, Head: goodHead.Head, Complete: goodComplete.Complete}}},
		{name: "body-nil-payload", frames: []core.StreamFrame{goodHead, {Type: core.FrameBody}}},
		{name: "body-extra-head", frames: []core.StreamFrame{goodHead, {Type: core.FrameBody, Body: body.Body, Head: goodHead.Head}}},
		{name: "body-extra-complete", frames: []core.StreamFrame{goodHead, {Type: core.FrameBody, Body: body.Body, Complete: goodComplete.Complete}}},
		{name: "complete-nil-payload", frames: []core.StreamFrame{goodHead, {Type: core.FrameComplete}}},
		{name: "complete-extra-head", frames: []core.StreamFrame{goodHead, {Type: core.FrameComplete, Complete: goodComplete.Complete, Head: goodHead.Head}}},
		{name: "complete-extra-body", frames: []core.StreamFrame{goodHead, {Type: core.FrameComplete, Complete: goodComplete.Complete, Body: body.Body}}},
		{name: "unknown-discriminant", frames: []core.StreamFrame{{Type: "unknown"}}},
		{name: "nil-body-data", frames: []core.StreamFrame{goodHead, {Type: core.FrameBody, Body: &core.BodyFrame{}}}},
		{name: "empty-body-data", frames: []core.StreamFrame{goodHead, {Type: core.FrameBody, Body: &core.BodyFrame{Data: []byte{}}}}},
		{name: "body-before-head", frames: []core.StreamFrame{body}},
		{name: "body-after-complete", frames: []core.StreamFrame{goodHead, body, withUsage, badBody}, wantPrefix: []byte("prefix"), wantUsage: true},
		{name: "duplicate-complete", frames: []core.StreamFrame{goodHead, body, withUsage, goodComplete}, wantPrefix: []byte("prefix"), wantUsage: true},
		{name: "succeeded-with-error", frames: []core.StreamFrame{goodHead, badSuccess}},
		{name: "failed-without-error", frames: []core.StreamFrame{goodHead, badFailed}},
		{name: "head-error-then-success", frames: []core.StreamFrame{errorHead, goodComplete}},
		{name: "unknown-outcome", frames: []core.StreamFrame{goodHead, badOutcome}},
		{name: "trailing-head", frames: []core.StreamFrame{goodHead, body, withUsage, goodHead}, wantPrefix: []byte("prefix"), wantUsage: true},
		{name: "trailing-complete", frames: []core.StreamFrame{goodHead, body, withUsage, goodComplete}, wantPrefix: []byte("prefix"), wantUsage: true},
		{name: "trailing-body", frames: []core.StreamFrame{goodHead, body, withUsage, badBody}, wantPrefix: []byte("prefix"), wantUsage: true},
		{name: "unexpected-read-after-complete", frames: []core.StreamFrame{goodHead, body, withUsage}, readErr: errors.New("synthetic read failure"), wantPrefix: []byte("prefix"), wantUsage: true},
		{name: "premature-eof", frames: []core.StreamFrame{goodHead, body}, wantPrefix: []byte("prefix")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			var streams []*violationStream
			sink, err := core.NewInMemoryAttemptObservations(2)
			if err != nil {
				t.Fatal(err)
			}
			d := &core.Dispatcher{AccountID: account, Observations: sink, Target: violationTarget(func(_ context.Context, _ core.ExecutionRequest, _ core.AttemptScope) (core.ExecutionResponse, *core.GatewayError) {
				if calls.Add(1) > 1 {
					s := &violationStream{frames: []core.StreamFrame{goodHead, body, goodComplete}}
					streams = append(streams, s)
					return core.ExecutionResponse{Stream: s}, nil
				}
				s := &violationStream{frames: tc.frames, readErr: tc.readErr}
				streams = append(streams, s)
				return core.ExecutionResponse{Stream: s}, nil
			})}
			resp, gatewayErr := d.Execute(context.Background(), core.ExecutionRequest{Model: model, Payload: core.RawPayload{Protocol: protocol}})
			if gatewayErr != nil {
				t.Fatalf("Execute: %v", gatewayErr)
			}
			var prefix []byte
			var terminal error
			for {
				frame, nextErr := resp.Stream.Next(context.Background())
				if nextErr != nil {
					terminal = nextErr
					break
				}
				if frame.Type == core.FrameBody {
					prefix = append(prefix, frame.Body.Data...)
				}
			}
			if terminal == io.EOF {
				t.Fatal("malformed stream completed successfully")
			}
			if !errors.Is(terminal, core.ErrStreamContract) {
				t.Fatalf("terminal error=%v, want ErrStreamContract", terminal)
			}
			_ = resp.Stream.Close()
			observations := sink.Snapshot()
			if len(observations) != 1 || observations[0].Outcome != core.OutcomeFailed || observations[0].ErrorCategory != core.CategoryInternal {
				t.Fatalf("violation observation: %+v", observations)
			}
			if tc.wantUsage {
				if observations[0].Usage == nil || observations[0].Usage.InputTokens == nil || *observations[0].Usage.InputTokens != usageTokens {
					t.Fatalf("trailing failure lost Complete usage: %+v", observations[0].Usage)
				}
			}
			if !bytes.Equal(prefix, tc.wantPrefix) {
				t.Fatalf("delivered prefix=%q, want exactly %q", prefix, tc.wantPrefix)
			}
			if streams[0].closes.Load() != 1 {
				t.Fatalf("producer Close count=%d", streams[0].closes.Load())
			}
			followup, gatewayErr := d.Execute(context.Background(), core.ExecutionRequest{Model: model, Payload: core.RawPayload{Protocol: protocol}})
			if gatewayErr != nil {
				t.Fatalf("healthy follow-up Execute: %v", gatewayErr)
			}
			for {
				_, nextErr := followup.Stream.Next(context.Background())
				if nextErr != nil {
					if nextErr != io.EOF {
						t.Fatalf("healthy follow-up: %v", nextErr)
					}
					break
				}
			}
			observations = sink.Snapshot()
			if len(observations) != 2 || observations[1].Outcome != core.OutcomeSucceeded {
				t.Fatalf("follow-up observations: %+v", observations)
			}
		})
	}
}

func TestConformanceNativeAndScriptedStreamsSatisfyLifecycle(t *testing.T) {
	for _, f := range []factory{{name: "native-loopback", new: nativeFixture}, {name: "scripted", new: scriptedFixture}} {
		t.Run(f.name, func(t *testing.T) {
			fx := f.new(t)
			t.Cleanup(fx.close)
			if err := fx.init(context.Background()); err != nil {
				t.Fatal(err)
			}
			response, ge := fx.connector.Execute(context.Background(), fx.request, fx.scope, fx.services())
			if ge != nil {
				t.Fatal(ge)
			}
			stream := core.NewCheckedStream(response.Stream, protocol)
			defer stream.Close()
			var seenHead, seenComplete, seenBody bool
			for {
				frame, err := stream.Next(context.Background())
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				switch frame.Type {
				case core.FrameHead:
					seenHead = true
				case core.FrameBody:
					seenBody = true
				case core.FrameComplete:
					seenComplete = true
				}
			}
			if !seenHead || !seenBody || !seenComplete {
				t.Fatalf("valid lifecycle incomplete: head=%v body=%v complete=%v", seenHead, seenBody, seenComplete)
			}
		})
	}
}

type violationTarget func(context.Context, core.ExecutionRequest, core.AttemptScope) (core.ExecutionResponse, *core.GatewayError)

func (f violationTarget) Execute(ctx context.Context, in core.ExecutionRequest, scope core.AttemptScope) (core.ExecutionResponse, *core.GatewayError) {
	return f(ctx, in, scope)
}

type violationStream struct {
	frames  []core.StreamFrame
	next    int
	readErr error
	closes  atomic.Int32
}

func (s *violationStream) Next(context.Context) (core.StreamFrame, error) {
	if s.next < len(s.frames) {
		frame := s.frames[s.next]
		s.next++
		return frame, nil
	}
	if s.readErr != nil {
		err := s.readErr
		s.readErr = nil
		return core.StreamFrame{}, err
	}
	return core.StreamFrame{}, io.EOF
}
func (s *violationStream) Close() error { s.closes.Add(1); return nil }
