package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
)

func TestLocalRuntimeFixturesThroughCompatibleTransport(t *testing.T) {
	for _, runtime := range runtimeFixtures {
		t.Run(runtime.dir, func(t *testing.T) {
			base := "testdata/local-runtime/" + runtime.dir + "/"
			jsonBody, err := localRuntimeFixtures.ReadFile(base + "response.json")
			if err != nil {
				t.Fatal(err)
			}
			var expected struct {
				Usage struct {
					Input     int64 `json:"input_tokens"`
					Output    int64 `json:"output_tokens"`
					InputInfo struct {
						Cached *int64 `json:"cached_tokens"`
					} `json:"input_tokens_details"`
					OutputInfo struct {
						Reasoning *int64 `json:"reasoning_tokens"`
					} `json:"output_tokens_details"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(jsonBody, &expected); err != nil {
				t.Fatal(err)
			}
			sseBody, err := localRuntimeFixtures.ReadFile(base + "stream.sse")
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name, contentType string
				body              []byte
				streaming         bool
			}{
				{"fixed-json", "application/json", jsonBody, false},
				{"incremental-sse", "text/event-stream", sseBody, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					up := fakeupstream.New(fakeupstream.Response{Header: http.Header{"Content-Type": {tc.contentType}}, Body: tc.body})
					defer up.Close()
					tr := newTransport(Config{Endpoint: up.URL + "/v1/responses", ConnectTimeout: time.Second}, authNone)
					defer tr.Close()
					request := fixedRequest()
					request.Metadata.Streaming = &tc.streaming
					var result core.ExecutionResponse
					var failure *core.GatewayError
					if tc.streaming {
						result, failure = tr.ExecuteSSE(context.Background(), request, "", nil)
					} else {
						result, failure = tr.ExecuteFixedJSON(context.Background(), request, "", nil)
					}
					if failure != nil {
						t.Fatal(failure)
					}
					defer result.Stream.Close()
					var got []byte
					var complete *core.CompleteFrame
					for {
						frame, err := result.Stream.Next(context.Background())
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Fatal(err)
						}
						switch frame.Type {
						case core.FrameBody:
							got = append(got, frame.Body.Data...)
						case core.FrameComplete:
							if complete != nil {
								t.Fatal("duplicate Complete")
							}
							complete = frame.Complete
						}
					}
					if !bytes.Equal(got, tc.body) || complete == nil || complete.Outcome != core.OutcomeSucceeded {
						t.Fatalf("wire bytes preserved=%t completion=%+v", bytes.Equal(got, tc.body), complete)
					}
					if complete.Usage == nil || complete.Usage.Source != core.UsageProvider || complete.Usage.InputTokens == nil || *complete.Usage.InputTokens != expected.Usage.Input || complete.Usage.OutputTokens == nil || *complete.Usage.OutputTokens != expected.Usage.Output {
						t.Fatalf("provider usage not observed: %+v", complete.Usage)
					}
					if !sameOptionalInt(complete.Usage.CachedTokens, expected.Usage.InputInfo.Cached) || !sameOptionalInt(complete.Usage.ReasoningTokens, expected.Usage.OutputInfo.Reasoning) {
						t.Fatalf("inclusive usage details not preserved as subsets: %+v", complete.Usage)
					}
					capture := <-up.Requests
					if capture.Path != "/v1/responses" || !bytes.Equal(capture.Body, request.Payload.Body) || capture.Header.Get("Authorization") != "" {
						t.Fatalf("local compatible request changed or gained auth: %+v", capture)
					}
				})
			}
		})
	}
}

func sameOptionalInt(got, want *int64) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return *got == *want
}

func TestGenericObserverFixturesSettleTerminalTruthfully(t *testing.T) {
	var cases []struct {
		Scenario string   `json:"scenario"`
		Chunks   []string `json:"chunks"`
	}
	readJSONFixture(t, "testdata/local-runtime/generic-observer/mechanics.json", &cases)
	for _, tc := range cases {
		t.Run(tc.Scenario, func(t *testing.T) {
			steps := make([]fakeupstream.Step, len(tc.Chunks))
			for i, chunk := range tc.Chunks {
				steps[i].Data = []byte(chunk)
			}
			_, complete, _ := sseFrames(t, fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: steps})
			want := core.OutcomeSucceeded
			switch tc.Scenario {
			case "failure-unattributed":
				want = core.OutcomeFailed
			case "incomplete-unattributed", "premature-eof-unattributed", "malformed-json-unattributed":
				want = core.OutcomeIncomplete
			}
			if complete.Outcome != want {
				t.Fatalf("outcome=%s, want %s: %+v", complete.Outcome, want, complete)
			}
		})
	}
}

func TestObserverPreservesZeroAndAbsentUsage(t *testing.T) {
	for _, tc := range []struct {
		name, fixed, stream string
		known               bool
	}{
		{"zero", `{"status":"completed","usage":{"input_tokens":0,"output_tokens":0}}`, terminal("response.completed", "completed", `,"usage":{"input_tokens":0,"output_tokens":0}`), true},
		{"absent", `{"status":"completed"}`, terminal("response.completed", "completed", ""), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, fixed, _ := fixedFrames(t, fakeupstream.Response{Body: []byte(tc.fixed)})
			_, stream, _ := sseFrames(t, fakeupstream.Response{Body: []byte(tc.stream)})
			for name, got := range map[string]*core.CompleteFrame{"fixed": fixed, "stream": stream} {
				if got.Usage == nil {
					t.Fatalf("%s usage missing", name)
				}
				if tc.known {
					if got.Usage.Source != core.UsageProvider || got.Usage.InputTokens == nil || *got.Usage.InputTokens != 0 || got.Usage.OutputTokens == nil || *got.Usage.OutputTokens != 0 {
						t.Errorf("%s zero usage collapsed: %+v", name, got.Usage)
					}
				} else if got.Usage.Source != core.UsageUnknown || got.Usage.InputTokens != nil || got.Usage.OutputTokens != nil {
					t.Errorf("%s absent usage fabricated: %+v", name, got.Usage)
				}
			}
		})
	}
}
