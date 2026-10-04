package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/connector/anthropic"
	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
)

const translatedModel = "gpt-4.1-mini"

func translationFixture(t *testing.T) fixture {
	t.Helper()
	response := strings.Join([]string{
		`event: message_start`, `data: {"type":"message_start","message":{"id":"msg_test","model":"claude-test","usage":{"input_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`, ``,
		`event: content_block_start`, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`, ``,
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"translated fixture"}}`, ``,
		`event: content_block_stop`, `data: {"type":"content_block_stop","index":0}`, ``,
		`event: message_delta`, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`, ``,
		`event: message_stop`, `data: {"type":"message_stop"}`, ``,
	}, "\n") + "\n"
	return translationFixtureWithResponse(t, fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: []byte(response)})
}

func translationFixtureWithResponse(t *testing.T, response fakeupstream.Response) fixture {
	t.Helper()
	upstream := fakeupstream.New(response)
	connector := anthropic.NewConnector()
	config, err := json.Marshal(map[string]string{"model": translatedModel, "account_id": account})
	if err != nil {
		t.Fatal(err)
	}
	adapterConfig, err := json.Marshal(map[string]int{"max_body_bytes": 1 << 20, "max_header_bytes": 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := core.NewRegistry(map[core.ComponentKind]core.APIVersion{
		core.ComponentAdapter: {Major: 1}, core.ComponentConnector: {Major: 1},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("conformance.responses", responses.NewAdapter(), core.ComponentAdapter); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("conformance.anthropic", connector, core.ComponentConnector); err != nil {
		t.Fatal(err)
	}
	routes, err := core.NewRouteTable([]core.Route{{
		Identity: core.RouteIdentity{RouteLookupKey: core.RouteLookupKey{Protocol: protocol, Mode: core.ModeTranslation, Model: translatedModel}, AccountID: account},
		Adapter:  "conformance.responses", Connector: "conformance.anthropic",
	}}, registry)
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PESTIROUTE_CONFORMANCE_TOKEN", "synthetic-token")
	services := core.NewEnvironmentServices(
		map[string]map[string]string{account: {"bearer": "PESTIROUTE_CONFORMANCE_TOKEN"}},
		map[string]core.HTTPDoer{account: fixtureDoer{target: target}}, nil,
	)
	input := []byte(`{"model":"gpt-4.1-mini","stream":true,"input":"fixture input"}`)
	inputSnapshot := append([]byte(nil), input...)
	return fixture{
		connector: connector, translated: true,
		dispatcher: &core.Dispatcher{Routes: routes, AccountID: account, Mode: core.ModeTranslation, Services: services},
		init: func(ctx context.Context) error {
			if err := registry.Init(ctx, "conformance.responses", core.ComponentConfig{Data: adapterConfig}); err != nil {
				return err
			}
			return registry.Init(ctx, "conformance.anthropic", core.ComponentConfig{Data: config})
		},
		failedInit: func() error {
			return connector.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"unknown":true}`)})
		},
		request: core.ExecutionRequest{ID: request, Model: translatedModel, Payload: core.RawPayload{Protocol: protocol, ContentType: "application/json", Body: input}, Metadata: core.RequestMetadata{Streaming: new(true)}},
		scope:   core.AttemptScope{ID: "translation-attempt", AccountID: account, Mode: core.ModeTranslation},
		services: func() core.InvocationServices {
			return services.ForAttempt(core.AttemptScope{AccountID: account, Mode: core.ModeTranslation})
		},
		callCount: upstream.RequestCount,
		waitCancel: func() bool {
			var request fakeupstream.Request
			select {
			case request = <-upstream.Requests:
			case <-time.After(2 * time.Second):
				return false
			}
			for _, event := range []<-chan struct{}{request.Cancelled, request.Completed} {
				select {
				case <-event:
				case <-time.After(2 * time.Second):
					return false
				}
			}
			return request.CancelCount() == 1
		},
		validate: func() error {
			if !bytes.Equal(input, inputSnapshot) {
				return fmt.Errorf("translator mutated admitted request bytes")
			}
			var got fakeupstream.Request
			select {
			case got = <-upstream.Requests:
			default:
				return fmt.Errorf("translated upstream request was not captured")
			}
			var payload struct {
				Model     string `json:"model"`
				MaxTokens int    `json:"max_tokens"`
				Messages  []struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(got.Body, &payload); err != nil || payload.Model != "claude-opus-5-5" || payload.MaxTokens <= 0 || len(payload.Messages) != 1 || len(payload.Messages[0].Content) != 1 || payload.Messages[0].Content[0].Text != "fixture input" {
				return fmt.Errorf("unexpected declared translated request %s (decode error %v)", got.Body, err)
			}
			return nil
		},
		close: func() {
			if err := registry.Close(context.Background()); err != nil {
				t.Errorf("close translation registry: %v", err)
			}
			upstream.Close()
		},
	}
}

func translationCancellationFixture(t *testing.T, gate <-chan struct{}, waiting chan<- struct{}) fixture {
	start := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":2,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}}\n\n")
	block := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
	delta := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"cancel-test\"}}\n\n")
	steps := []fakeupstream.Step{{Data: start}, {Data: block}, {Data: delta}}
	var release chan struct{}
	if gate == nil {
		release = make(chan struct{})
		steps[0].Gate = release
	} else {
		steps = append(steps, fakeupstream.Step{Gate: gate, Waiting: waiting, Data: delta})
	}
	steps = append(steps,
		fakeupstream.Step{Data: []byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")},
		fakeupstream.Step{Data: []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n")},
		fakeupstream.Step{Data: []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")},
	)
	fx := translationFixtureWithResponse(t, fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: steps})
	if release != nil {
		fx.release = func() { close(release) }
	}
	return fx
}

type fixtureDoer struct{ target *url.URL }

func (d fixtureDoer) Do(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL = d.target
	clone.Host = d.target.Host
	return http.DefaultClient.Do(clone)
}
