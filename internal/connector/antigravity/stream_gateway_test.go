package antigravity

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chatadapter "github.com/blestafist/pestiroute/internal/adapter/chatcompletions"
	responsesadapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/core"
)

func TestConnectorAdapterLoopbackStreamFailuresAndIncrementality(t *testing.T) {
	responses := responsesadapter.NewAdapter()
	if err := responses.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"max_body_bytes":1048576,"max_header_bytes":65536}`)}); err != nil {
		t.Fatal(err)
	}
	chat := chatadapter.NewAdapter()
	if err := chat.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"max_body_bytes":1048576,"max_header_bytes":65536}`)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = responses.Close(context.Background()); _ = chat.Close(context.Background()) })
	for _, tc := range []struct {
		name       string
		wire       string
		wantStatus int
		wantBody   string
	}{
		{name: "empty initial frame", wantStatus: http.StatusServiceUnavailable, wantBody: `"code":"upstream_stream_error"`},
		{name: "malformed initial frame", wire: "data: {bad}\n\n", wantStatus: http.StatusServiceUnavailable, wantBody: `"code":"upstream_stream_error"`},
		{name: "postcommit failure", wire: "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}}\n\n", wantStatus: http.StatusOK, wantBody: "partial"},
	} {
		for _, adapterName := range []string{"responses", "chat"} {
			t.Run(tc.name+"/"+adapterName, func(t *testing.T) {
				upstream := &fakeDoer{response: response(200, tc.wire)}
				result, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"synthetic","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{Transport: packageTestDoer(upstream), Credentials: credentials{data: []byte(`{"access_token":"synthetic-token","project_id":"synthetic-project"}`)}})
				if ge != nil {
					t.Fatalf("execute returned error: %v", ge)
				}
				h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if adapterName == "chat" {
						_ = chat.Encode(r.Context(), core.ClientResponse{Transport: chatadapter.HTTPResponse{Writer: w, Request: r}}, nil, result)
					} else {
						_ = responses.Encode(r.Context(), core.ClientResponse{Transport: responsesadapter.HTTPResponse{Writer: w, Request: r}}, nil, result)
					}
				})
				server := httptest.NewServer(h)
				defer server.Close()
				resp, err := server.Client().Get(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil || resp.StatusCode != tc.wantStatus || !strings.Contains(string(body), tc.wantBody) {
					t.Fatalf("status=%d err=%v body=%q", resp.StatusCode, err, body)
				}
				if tc.name == "postcommit failure" && strings.Contains(string(body), `"error"`) {
					t.Fatalf("postcommit stream was rewritten: %q", body)
				}
			})
		}
	}

	var previousBody string
	for _, newline := range []string{"\n", "\r\n"} {
		t.Run("valid incremental "+strings.ReplaceAll(newline, "\r", "CR"), func(t *testing.T) {
			wire := strings.Join([]string{
				`data: {"response":{"candidates":[{"content":{"parts":[{"text":"one"}]}}]}}`,
				`data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]}}]}}`,
				`data: {"response":{"candidates":[{"content":{"parts":[{"text":" two"}]}}]}}`,
				`data: {"response":{"done":true}}`,
			}, newline+newline) + newline + newline
			upstream := &fakeDoer{response: response(200, wire)}
			result, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"synthetic","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{Transport: packageTestDoer(upstream), Credentials: credentials{data: []byte(`{"access_token":"synthetic-token","project_id":"synthetic-project"}`)}})
			if ge != nil {
				t.Fatal(ge)
			}
			stream := result.Stream.(*stream)
			stream.responseID, stream.messageID = "resp_fixture", "msg_fixture"
			rr := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if err := responses.Encode(r.Context(), core.ClientResponse{Transport: responsesadapter.HTTPResponse{Writer: rr, Request: r}}, nil, result); err != nil {
				t.Fatal(err)
			}
			body := rr.Body.String()
			if rr.Code != http.StatusOK || !strings.Contains(body, `"delta":"one"`) || !strings.Contains(body, `"delta":" two"`) || !strings.Contains(body, "response.function_call_arguments.delta") || !strings.Contains(body, "response.completed") {
				t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
			}
			if previousBody != "" && body != previousBody {
				t.Fatalf("LF/CRLF output differs\nLF: %q\nCRLF: %q", previousBody, body)
			}
			previousBody = body
		})
	}
}

func TestStreamDebugFailureClassificationIsSecretSafe(t *testing.T) {
	for _, tc := range []struct{ wire, category string }{
		{"", "precommit_eof"},
		{"data: {PRIVATE-BODY}\n\n", "precommit_invalid_json"},
	} {
		var logs strings.Builder
		logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		result, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"PRIVATE-PROMPT","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{Transport: packageTestDoer(&fakeDoer{response: response(200, tc.wire)}), Credentials: credentials{data: []byte(`{"access_token":"synthetic-token","project_id":"synthetic-project"}`)}, Logger: logger})
		if ge != nil {
			t.Fatal(ge)
		}
		_, err := result.Stream.Next(context.Background())
		if err == nil {
			t.Fatal("expected initial frame failure")
		}
		got := logs.String()
		if !strings.Contains(got, "phase=precommit") || !strings.Contains(got, "category="+tc.category) || strings.Contains(got, "PRIVATE-") || strings.Contains(got, "https://") {
			t.Fatalf("unsafe/missing stream diagnostic: %q", got)
		}
		_ = result.Stream.Close()
	}
}
