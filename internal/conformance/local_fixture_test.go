package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
)

type localProfile struct {
	name, profile, runtimeProfile string
}

var localProfiles = []localProfile{
	{name: "ollama", profile: "ollama-responses-http-sse-v1", runtimeProfile: "ollama-v0.40.3"},
	{name: "vllm", profile: "vllm-responses-http-sse-v1", runtimeProfile: "vllm-v0.31.0"},
	{name: "llamacpp", profile: "llamacpp-responses-http-sse-v1", runtimeProfile: "llama.cpp-v0.6.0"},
}

func TestLocalProfilesSharedConformance(t *testing.T) {
	for _, profile := range localProfiles {
		t.Run(profile.profile, func(t *testing.T) {
			t.Run("lifecycle", func(t *testing.T) {
				fx, _ := newLocalFixture(t, profile, fakeupstream.Response{Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"status":"completed","synthetic":true}`)})
				t.Cleanup(fx.close)
				if err := runBaseline(fx); err != nil {
					t.Fatal(err)
				}
			})
			t.Run("opacity", func(t *testing.T) {
				want := []byte("event: response.completed\ndata: { \"type\" : \"response.completed\", \"unknown\":\"☃\" }\n\n")
				fx, captured := newLocalOpaqueFixture(t, profile, want)
				t.Cleanup(fx.close)
				if err := fx.init(context.Background()); err != nil {
					t.Fatal(err)
				}
				response, ge := fx.connector.Execute(context.Background(), fx.request, fx.scope, fx.services())
				if ge != nil {
					t.Fatal(ge)
				}
				defer response.Stream.Close()
				body, err := readOpaqueBody(response.Stream)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(body, want) || !bytes.Equal(captured(), fx.request.Payload.Body) {
					t.Fatalf("opaque response/request changed: response=%q request=%q", body, captured())
				}
			})
			t.Run("incremental", func(t *testing.T) {
				gate, waiting := make(chan struct{}), make(chan struct{}, 1)
				fx := newLocalGatedFixture(t, profile, gate, waiting)
				t.Cleanup(fx.close)
				if err := fx.init(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := runIncrementalScenario(fx, fx.connector, gate, waiting, false); err != nil {
					t.Fatal(err)
				}
			})
			t.Run("cancellation", func(t *testing.T) {
				gate := make(chan struct{})
				fx := newLocalGatedFixture(t, profile, gate, nil)
				t.Cleanup(fx.close)
				if err := fx.init(context.Background()); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				response, ge := fx.connector.Execute(ctx, fx.request, fx.scope, fx.services())
				if ge != nil {
					cancel()
					t.Fatal(ge)
				}
				if frame, err := response.Stream.Next(ctx); err != nil || frame.Type != core.FrameHead {
					cancel()
					t.Fatalf("Head = %+v, %v", frame, err)
				}
				if frame, err := response.Stream.Next(ctx); err != nil || frame.Type != core.FrameBody {
					cancel()
					t.Fatalf("Body = %+v, %v", frame, err)
				}
				cancel()
				_ = response.Stream.Close()
				if !fx.waitCancel() {
					t.Fatal("synthetic upstream did not observe cancellation")
				}
			})
		})
	}
}

func newLocalOpaqueFixture(t *testing.T, profile localProfile, response []byte) (fixture, func() []byte) {
	parts := [][]byte{response[:len(response)/2], response[len(response)/2:]}
	steps := []fakeupstream.Step{{Data: parts[0]}, {Data: parts[1]}}
	fx, captured := newLocalFixture(t, profile, fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: steps})
	return fx, captured
}

func newLocalGatedFixture(t *testing.T, profile localProfile, gate <-chan struct{}, waiting chan<- struct{}) fixture {
	return newLocalFixtureResponse(t, profile, fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: []fakeupstream.Step{{Data: incrementalFirst}, {Gate: gate, Waiting: waiting, Data: incrementalTerminal}}})
}

func newLocalFixture(t *testing.T, profile localProfile, response fakeupstream.Response) (fixture, func() []byte) {
	fx := newLocalFixtureResponse(t, profile, response)
	return fx, fx.capture
}

func newLocalFixtureResponse(t *testing.T, profile localProfile, response fakeupstream.Response) fixture {
	t.Helper()
	upstream := fakeupstream.New(response)
	const accountID = "local-conformance-account"
	modelID := "synthetic-" + profile.name
	connector := responses.NewLocalConnector("none")
	config, err := json.Marshal(map[string]any{
		"transport": map[string]any{"endpoint": upstream.URL + "/v1/responses"},
		"model":     modelID, "account_id": accountID, "auth_mode": "none", "runtime_profile": profile.runtimeProfile,
	})
	if err != nil {
		t.Fatal(err)
	}
	var captured []byte
	fx := fixture{
		connector: connector,
		init:      func(ctx context.Context) error { return connector.Init(ctx, core.ComponentConfig{Data: config}) },
		request:   core.ExecutionRequest{ID: request, Model: modelID, Payload: core.RawPayload{Protocol: protocol, ContentType: "application/json", Body: []byte("{ \"model\" : \"" + modelID + "\", \"input\":\"synthetic ☃\", \"future\":true }\n")}},
		scope:     core.AttemptScope{ID: "local-conformance-attempt", AccountID: accountID, Mode: "native"},
		services: func() core.InvocationServices {
			return core.NewEnvironmentServices(nil, map[string]core.HTTPDoer{accountID: connector.HTTPDoer()}, nil).ForAttempt(core.AttemptScope{AccountID: accountID, Mode: "native"})
		},
		callCount: upstream.RequestCount,
		waitCancel: func() bool {
			select {
			case req := <-upstream.Requests:
				select {
				case <-req.Cancelled:
				case <-time.After(2 * time.Second):
					return false
				}
				return req.CancelCount() == 1
			case <-time.After(2 * time.Second):
				return false
			}
		},
		close: func() {
			if err := connector.Close(context.Background()); err != nil {
				t.Errorf("close local connector: %v", err)
			}
			upstream.Close()
		},
	}
	fx.wantBody = append([]byte(nil), response.Body...)
	if len(response.Steps) != 0 {
		fx.wantBody = nil
		for _, step := range response.Steps {
			fx.wantBody = append(fx.wantBody, step.Data...)
		}
	}
	fx.capture = func() []byte {
		if captured == nil {
			select {
			case req := <-upstream.Requests:
				captured = append([]byte(nil), req.Body...)
			default:
			}
		}
		return append([]byte(nil), captured...)
	}
	return fx
}

func TestLocalProfilesBackpressure(t *testing.T) {
	for _, profile := range localProfiles {
		t.Run(profile.profile, func(t *testing.T) {
			bf := newLocalBackpressureFixture(t, profile)
			t.Cleanup(bf.close)
			if err := bf.init(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := assertBoundedPause(bf, bf.runner); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func newLocalBackpressureFixture(t *testing.T, profile localProfile) backpressureFixture {
	const chunkSize = 32 << 10
	chunk := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"" + strings.Repeat("x", chunkSize-85) + "\"}\n\n")
	steps := []fakeupstream.Step{{Data: incrementalFirst}}
	sent := make([]chan struct{}, 0, backpressureFrames)
	want := append([]byte(nil), incrementalFirst...)
	for range backpressureFrames - 1 {
		delivered := make(chan struct{})
		sent = append(sent, delivered)
		steps = append(steps, fakeupstream.Step{Data: chunk, Sent: delivered})
		want = append(want, chunk...)
	}
	terminal := incrementalTerminal
	terminalSent := make(chan struct{})
	sent = append(sent, terminalSent)
	steps = append(steps, fakeupstream.Step{Data: terminal, Sent: terminalSent})
	want = append(want, terminal...)
	fx := newLocalFixtureResponse(t, profile, fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: steps})
	fx.wantBody = want
	sentCount := 0
	return backpressureFixture{fixture: fx, total: backpressureFrames, maxProgress: backpressureFrames - 1, runner: fx.connector, progress: func() int {
		for sentCount < len(sent) {
			select {
			case <-sent[sentCount]:
				sentCount++
			default:
				return sentCount
			}
		}
		return sentCount
	}}
}
