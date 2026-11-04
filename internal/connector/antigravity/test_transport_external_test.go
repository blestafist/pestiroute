package antigravity_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/blestafist/pestiroute/internal/connector/antigravity"
	"github.com/blestafist/pestiroute/internal/core"
)

type overridingEmbeddedTransport struct {
	*antigravity.OfflineTestTransport
	calls int
}

func (t *overridingEmbeddedTransport) Do(*http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{StatusCode: http.StatusOK}, nil
}

func TestExternalEmbeddedTransportOverrideAndNilTransportAreRejected(t *testing.T) {
	connector := antigravity.NewConnector()
	if err := connector.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"profile":"antigravity-v1internal","project":"synthetic-project"}`)}); err != nil {
		t.Fatal(err)
	}
	defer connector.Close(context.Background())
	inner, err := antigravity.NewOfflineTestTransport(antigravity.OfflineTestResponse{StatusCode: http.StatusOK})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &overridingEmbeddedTransport{OfflineTestTransport: inner}
	request := core.ExecutionRequest{Payload: core.RawPayload{Protocol: "openai.responses.v1", ContentType: "application/json", Body: []byte(`{"model":"gemini-2.5-pro","input":"x","stream":true}`)}}
	_, gatewayErr := connector.Execute(context.Background(), request, core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{Transport: wrapped})
	if gatewayErr == nil || wrapped.calls != 0 || len(inner.Requests()) != 0 {
		t.Fatalf("embedded Do override reached: error=%v calls=%d captured=%+v", gatewayErr, wrapped.calls, inner.Requests())
	}
	var nilTransport *antigravity.OfflineTestTransport
	_, gatewayErr = connector.Execute(context.Background(), request, core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{Transport: nilTransport})
	if gatewayErr == nil {
		t.Fatal("typed nil offline transport was accepted")
	}
}
