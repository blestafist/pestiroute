package conformance

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/connector/codex"
	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
)

const codexAccount = account

var codexSuccessResponse = []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")

func codexFixture(t *testing.T) fixture {
	return codexFixtureWithResponse(t, fakeupstream.Response{
		Header: http.Header{"Content-Type": {"text/event-stream"}},
		Body:   codexSuccessResponse,
	})
}

func codexFixtureWithResponse(t *testing.T, response fakeupstream.Response) fixture {
	t.Helper()
	upstream := fakeupstream.New(response)
	connector := codex.NewConnector()
	config, err := json.Marshal(map[string]string{"model": model, "account_id": codexAccount, "profile": "codex-responses-http-sse-v1"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(target))
	transport := &http.Transport{
		// The test-only reverse proxy uses an ephemeral certificate for localhost;
		// production Connector transport policy is still exercised above this dialer.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: "localhost", MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, proxy.Listener.Addr().String())
		},
	}
	client := &http.Client{Transport: transport}
	t.Setenv("PESTIROUTE_CODEX_CONFORMANCE_BUNDLE", `{"version":1,"access_token":"synthetic-token","account_id":"conformance-account","expires_at":"2099-01-01T00:00:00Z"}`)
	services := core.NewEnvironmentServices(
		map[string]map[string]string{codexAccount: {"oauth": "PESTIROUTE_CODEX_CONFORMANCE_BUNDLE"}},
		map[string]core.HTTPDoer{codexAccount: client}, nil,
	)
	requestBody := []byte(`{"model":"gpt-5.4-mini","input":"synthetic conformance request","stream":true,"store":false}`)
	var captured []byte
	return fixture{
		connector: connector,
		codex:     true,
		init: func(ctx context.Context) error {
			return connector.Init(ctx, core.ComponentConfig{Data: config})
		},
		failedInit: func() error {
			return connector.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"unknown":true}`)})
		},
		request: core.ExecutionRequest{ID: request, Model: model, Payload: core.RawPayload{
			Protocol: protocol, ContentType: "application/json", Body: requestBody,
		}, Metadata: core.RequestMetadata{Streaming: new(true)}},
		wantBody: codexSuccessResponse,
		scope:    core.AttemptScope{ID: "codex-conformance-attempt", AccountID: codexAccount, Mode: core.ModeNative},
		services: func() core.InvocationServices {
			return services.ForAttempt(core.AttemptScope{AccountID: codexAccount, Mode: core.ModeNative})
		},
		callCount: func() int64 { return upstream.RequestCount() },
		waitCancel: func() bool {
			var req fakeupstream.Request
			select {
			case req = <-upstream.Requests:
			case <-time.After(2 * time.Second):
				return false
			}
			for _, event := range []<-chan struct{}{req.Cancelled, req.Completed} {
				select {
				case <-event:
				case <-time.After(2 * time.Second):
					return false
				}
			}
			return req.CancelCount() == 1
		},
		capture: func() []byte {
			if captured == nil {
				select {
				case req := <-upstream.Requests:
					captured = append([]byte(nil), req.Body...)
				default:
				}
			}
			return append([]byte(nil), captured...)
		},
		close: func() {
			if err := connector.Close(context.Background()); err != nil {
				t.Errorf("close Codex connector: %v", err)
			}
			upstream.Close()
			proxy.Close()
		},
	}
}

func codexOpaqueFixture(t *testing.T, parts [][]byte) (fixture, func() []byte) {
	steps := make([]fakeupstream.Step, len(parts))
	for i, part := range parts {
		steps[i].Data = part
	}
	fx := codexFixtureWithResponse(t, fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: steps})
	fx.request.Payload.Body = []byte("{ \"model\" : \"gpt-5.4-mini\", \"input\" : \"snowman ☃\", \"stream\" : true, \"store\" : false, \"future_field\" : { \"keep\" : true } }\n")
	return fx, func() []byte {
		return fx.capture()
	}
}

func codexGatedFixture(t *testing.T, gate <-chan struct{}, sent chan<- struct{}) fixture {
	first := incrementalFirst
	terminal := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	return codexFixtureWithResponse(t, fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: []fakeupstream.Step{{Data: first}, {Gate: gate, Sent: sent, Data: terminal}}})
}

func TestCodexConformanceTerminalAndUsage(t *testing.T) {
	fx := codexFixture(t)
	t.Cleanup(fx.close)
	if err := fx.init(context.Background()); err != nil {
		t.Fatal(err)
	}
	response, ge := fx.connector.Execute(context.Background(), fx.request, fx.scope, fx.services())
	if ge != nil {
		t.Fatal(ge)
	}
	defer response.Stream.Close()
	var head, body, complete bool
	for {
		frame, err := response.Stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch frame.Type {
		case core.FrameHead:
			head = true
		case core.FrameBody:
			body = true
		case core.FrameComplete:
			if !head || !body || complete || frame.Complete.Outcome != core.OutcomeSucceeded || frame.Complete.Usage == nil || frame.Complete.Usage.InputTokens == nil || *frame.Complete.Usage.InputTokens != 2 || frame.Complete.Usage.OutputTokens == nil || *frame.Complete.Usage.OutputTokens != 1 {
				t.Fatalf("unexpected Codex terminal/usage frame: %+v", frame.Complete)
			}
			complete = true
		default:
			t.Fatalf("unexpected frame type %q", frame.Type)
		}
	}
	if !complete {
		t.Fatal("Codex stream omitted Complete")
	}
}
