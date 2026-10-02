package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
)

func configuredTopology(t *testing.T, upstreams [2]*fakeupstream.Server, bodyLimit, headerLimit int64, capabilities [2]map[core.Capability]core.CapabilityState, finalized *atomic.Int32) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	t.Setenv("PESTIROUTE_REVIEW_A", "synthetic-a")
	t.Setenv("PESTIROUTE_REVIEW_B", "synthetic-b")
	c := config{Listen: "127.0.0.1:0", Components: []topologyComponent{
		{ID: "adapter", Implementation: "pestiroute.responses.native", Kind: core.ComponentAdapter},
	}, Routes: []topologyRoute{
		{Protocol: responsesProtocol, Mode: core.ModeNative, Model: "gpt-5.4-mini", Account: "a", Adapter: "adapter", Connector: "a", Capabilities: capabilities[0]},
		{Protocol: responsesProtocol, Mode: core.ModeNative, Model: "custom-model", Account: "b", Adapter: "adapter", Connector: "b", Capabilities: capabilities[1]},
	}}
	for i, id := range []core.InstanceID{"a", "b"} {
		body, header := int64(4096), int64(4096)
		if i == 0 {
			body, header = bodyLimit, headerLimit
		}
		c.Components = append(c.Components, topologyComponent{
			ID: id, Implementation: "pestiroute.responses.native", Kind: core.ComponentConnector,
			Endpoint: upstreams[i].URL + "/v1/responses", CredentialEnv: []string{"PESTIROUTE_REVIEW_A", "PESTIROUTE_REVIEW_B"}[i],
			MaxBodyBytes: body, MaxHeaderBytes: header, ConnectTimeout: "1s", TLSTimeout: "1s", HeaderTimeout: "2s", IdleTimeout: "3s",
		})
	}
	path := t.TempDir() + "/topology.json"
	if err := os.WriteFile(path, mustJSON(map[string]any{"listen": c.Listen, "components": c.Components, "routes": c.Routes}), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := loadConfig([]string{"-config", path})
	if err != nil {
		t.Fatal(err)
	}
	var ready, draining atomic.Bool
	completed := make(chan struct{}, 2)
	handler, closeComponents, err := composeHandler(loaded, &ready, &draining, func(core.AttemptResult) { finalized.Add(1); completed <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(func() { server.Close(); _ = closeComponents(context.Background()) })
	return server, completed
}

func awaitConfiguredAttempt(t *testing.T, completed <-chan struct{}) {
	t.Helper()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("accepted attempt was not finalized")
	}
}

func TestConfiguredRouteCapabilityDeclarations(t *testing.T) {
	for _, state := range []core.CapabilityState{core.Unsupported, core.Unknown} {
		t.Run(string(state), func(t *testing.T) {
			upstreams := [2]*fakeupstream.Server{
				fakeupstream.New(fakeupstream.Response{Body: []byte(`{"status":"completed"}`)}),
				fakeupstream.New(fakeupstream.Response{Body: []byte(`{"status":"completed"}`)}),
			}
			defer upstreams[0].Close()
			defer upstreams[1].Close()
			var finalized atomic.Int32
			server, completed := configuredTopology(t, upstreams, 4096, 4096, [2]map[core.Capability]core.CapabilityState{{"llm.tools": state}, {"llm.tools": core.Supported}}, &finalized)
			for i, model := range []string{"gpt-5.4-mini", "custom-model"} {
				body := `{"model":"` + model + `","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`
				resp, err := server.Client().Post(server.URL+"/v1/responses", "application/json", strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				data, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					t.Fatal(readErr)
				}
				if i == 0 {
					var result struct {
						Error struct {
							Code string `json:"code"`
						} `json:"error"`
					}
					if err := json.Unmarshal(data, &result); err != nil || resp.StatusCode != 400 || result.Error.Code != "unsupported_capability" || upstreams[0].RequestCount() != 0 || finalized.Load() != 0 {
						t.Fatalf("restricted route: status=%d body=%s upstream=%d attempts=%d", resp.StatusCode, data, upstreams[0].RequestCount(), finalized.Load())
					}
				} else {
					awaitConfiguredAttempt(t, completed)
					if resp.StatusCode != 200 || upstreams[1].RequestCount() != 1 || finalized.Load() != 1 {
						t.Fatalf("independent fixture route: status=%d body=%s upstream=%d attempts=%d", resp.StatusCode, data, upstreams[1].RequestCount(), finalized.Load())
					}
					capture := <-upstreams[1].Requests
					if !bytes.Equal(capture.Body, []byte(body)) || capture.Header.Get("Authorization") != "Bearer synthetic-b" {
						t.Fatal("fixture route changed bytes or selected the wrong credential")
					}
				}
			}
		})
	}
}

func TestConfiguredFixtureRouteStreamingDeclaration(t *testing.T) {
	want := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	upstreams := [2]*fakeupstream.Server{
		fakeupstream.New(fakeupstream.Response{Body: []byte(`{}`)}),
		fakeupstream.New(fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: want}),
	}
	defer upstreams[0].Close()
	defer upstreams[1].Close()
	var finalized atomic.Int32
	server, completed := configuredTopology(t, upstreams, 4096, 4096, [2]map[core.Capability]core.CapabilityState{nil, {"llm.streaming": core.Supported}}, &finalized)
	body := `{"model":"custom-model","stream":true,"future":{"keep":true}}`
	resp, err := server.Client().Post(server.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	awaitConfiguredAttempt(t, completed)
	if readErr != nil || resp.StatusCode != 200 || !bytes.Equal(data, want) || upstreams[0].RequestCount() != 0 || upstreams[1].RequestCount() != 1 || finalized.Load() != 1 {
		t.Fatalf("configured streaming: status=%d body=%s read=%v attempts=%d", resp.StatusCode, data, readErr, finalized.Load())
	}
	if capture := <-upstreams[1].Requests; !bytes.Equal(capture.Body, []byte(body)) {
		t.Fatal("streaming request was rewritten")
	}
}

func TestConfiguredRouteRequestLimits(t *testing.T) {
	for _, scenario := range []string{"body", "chunked-body", "headers"} {
		t.Run(scenario, func(t *testing.T) {
			upstreams := [2]*fakeupstream.Server{
				fakeupstream.New(fakeupstream.Response{Body: []byte(`{"status":"completed"}`)}),
				fakeupstream.New(fakeupstream.Response{Body: []byte(`{"status":"completed"}`)}),
			}
			defer upstreams[0].Close()
			defer upstreams[1].Close()
			var finalized atomic.Int32
			bodyLimit, headerLimit := int64(128), int64(4096)
			if scenario == "headers" {
				bodyLimit, headerLimit = 4096, 256
			}
			server, completed := configuredTopology(t, upstreams, bodyLimit, headerLimit, [2]map[core.Capability]core.CapabilityState{}, &finalized)
			for i, model := range []string{"gpt-5.4-mini", "custom-model"} {
				body := `{"model":"` + model + `","input":"` + strings.Repeat("x", 300) + `"}`
				if scenario == "headers" {
					body = `{"model":"` + model + `"}`
				}
				req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				if scenario == "chunked-body" {
					req.ContentLength = -1
				}
				if scenario == "headers" {
					req.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 1000))
				}
				resp, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				data, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					t.Fatal(readErr)
				}
				if i == 1 {
					awaitConfiguredAttempt(t, completed)
				}
				if i == 0 {
					if resp.StatusCode != 400 || !bytes.Contains(data, []byte("too large")) || upstreams[0].RequestCount() != 0 || finalized.Load() != 0 {
						t.Fatalf("restricted route: status=%d body=%s upstream=%d attempts=%d", resp.StatusCode, data, upstreams[0].RequestCount(), finalized.Load())
					}
				} else if resp.StatusCode != 200 || upstreams[1].RequestCount() != 1 || finalized.Load() != 1 {
					t.Fatalf("larger route: status=%d body=%s upstream=%d attempts=%d", resp.StatusCode, data, upstreams[1].RequestCount(), finalized.Load())
				}
			}
		})
	}
}
