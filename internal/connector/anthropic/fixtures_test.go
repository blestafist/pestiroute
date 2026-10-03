package anthropic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
)

var fixtureHashes = map[string]string{
	"http-error.json":             "15461cdf7d18362d2c31fcb7863999548c08bd0682898f2604e5dcb75d229b0f",
	"malformed.json":              "e921bf2934d2d0c149fbdba4e6292d4c4abdb400b46370482bac6e10044b476a",
	"request-developer-only.json": "3633802e9fb1c535dcf017dc20919687e8d505d17235a14bf9b5d27959784690",
	"request-late-developer.json": "12da7d7eaaa8cdb81d599b55fb5c07f92a6bb86270f036cd622f00e0abad6ba4",
	"request-positive.json":       "b95f7ba46ac4cbea9edd2d77d609a2017edd6a147beebe4c8c695f894f4a6ca0",
	"request-system-history.json": "b196feb3741a61e07929b358f4491beb1f6316c0d9e94dcf0c99d617579088d2",
	"stream-error.sse":            "35837c5537d85715b2a00b4d021bd58bbc77c2eb616aedc216d607bc74286f26",
	"stream-max-tokens.sse":       "4576b3ba65bff19fa109d28601d5684a28f4079d133d3d045ac74b85f4c9bb3a",
	"stream-normal.sse":           "89a8bf56a7f10b795c33ad01840083554c8bc4c50aa6b24412c873d550ec5284",
	"stream-premature-eof.sse":    "34a13b99657c51e9fb0a36bb9da840432cde47f8368125782abbf82b89a3f1bb",
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFixtureIntegrityAndMalformedInput(t *testing.T) {
	for name, want := range fixtureHashes {
		t.Run(name, func(t *testing.T) {
			sum := sha256.Sum256(fixture(t, name))
			if got := hex.EncodeToString(sum[:]); got != want {
				t.Fatalf("SHA-256 = %s, want %s", got, want)
			}
		})
	}
	var v any
	if err := json.Unmarshal(fixture(t, "malformed.json"), &v); err == nil {
		t.Fatal("malformed fixture unexpectedly decoded")
	}
	positive := decodeRequestFixture(t, "request-positive.json")
	if positive.Model != "gpt-4.1-mini" || !positive.Stream || positive.Store == nil || *positive.Store || positive.Include == nil || len(positive.Include) != 1 || positive.Include[0] != "reasoning.encrypted_content" || positive.MaxOutputTokens != 20 || positive.Temperature == nil || *positive.Temperature != 0 {
		t.Fatalf("positive request options mismatch: %+v", positive)
	}
	if len(positive.Input) != 2 || positive.Input[0].Type != "message" || positive.Input[0].Role != "developer" || string(positive.Input[0].Content) != `"You are concise."` || positive.Input[1].Type != "message" || positive.Input[1].Role != "user" || string(positive.Input[1].Content) != `[{"type":"input_text","text":"Say hello."}]` {
		t.Fatalf("positive typed history mismatch: %+v", positive.Input)
	}
	negative := []struct {
		file  string
		roles []string
	}{
		{"request-developer-only.json", []string{"developer"}},
		{"request-system-history.json", []string{"system", "user"}},
		{"request-late-developer.json", []string{"user", "developer"}},
	}
	for _, tc := range negative {
		t.Run(tc.file, func(t *testing.T) {
			request := decodeRequestFixture(t, tc.file)
			if !request.Stream || len(request.Input) != len(tc.roles) {
				t.Fatalf("request shape: %+v", request)
			}
			for i, role := range tc.roles {
				if request.Input[i].Type != "message" || request.Input[i].Role != role || len(request.Input[i].Content) == 0 || string(request.Input[i].Content) == `""` {
					t.Fatalf("input[%d] = %+v, want non-empty message role %q", i, request.Input[i], role)
				}
			}
			if tc.file == "request-developer-only.json" && containsRole(request.Input, "user") {
				t.Fatal("developer-only fixture unexpectedly contains conversational input")
			}
		})
	}
	assertFixtureEvents(t, fixture(t, "stream-normal.sse"), []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"})
	maxEvents := parseFixtureEvents(t, fixture(t, "stream-max-tokens.sse"))
	if !strings.Contains(eventData(t, maxEvents, "message_delta"), `"stop_reason":"max_tokens"`) || !hasEvent(maxEvents, "message_stop") {
		t.Fatalf("max-token terminal events missing: %+v", maxEvents)
	}
	errorEvents := parseFixtureEvents(t, fixture(t, "stream-error.sse"))
	if !hasEvent(errorEvents, "error") || !strings.Contains(eventData(t, errorEvents, "error"), `"type":"api_error"`) {
		t.Fatalf("SSE error fixture malformed: %+v", errorEvents)
	}
	prematureRaw := fixture(t, "stream-premature-eof.sse")
	if bytes.HasSuffix(prematureRaw, []byte("\n\n")) {
		t.Fatal("premature EOF fixture unexpectedly has a blank-line event terminator")
	}
	lastComplete := bytes.LastIndex(prematureRaw, []byte("\n\n"))
	if lastComplete < 0 {
		t.Fatal("premature EOF fixture has no preceding complete events")
	}
	premature := parseFixtureEvents(t, prematureRaw[:lastComplete+2])
	if hasEvent(premature, "content_block_delta") || hasEvent(premature, "message_delta") || hasEvent(premature, "message_stop") {
		t.Fatalf("premature EOF fixture's unterminated delta was treated as complete: %+v", premature)
	}
	var httpError struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(fixture(t, "http-error.json"), &httpError); err != nil || httpError.Type != "error" || httpError.Error.Type != "overloaded_error" {
		t.Fatalf("HTTP error fixture = %+v, err=%v", httpError, err)
	}
}

func TestFixtureFakeUpstreamIncrementalDelivery(t *testing.T) {
	stream := fixture(t, "stream-normal.sse")
	deltaStart := strings.Index(string(stream), "event: content_block_delta\n")
	if deltaStart < 0 {
		t.Fatal("normal fixture has no text delta")
	}
	deltaEnd := strings.Index(string(stream[deltaStart:]), "\n\n")
	if deltaEnd < 0 {
		t.Fatal("normal fixture has unterminated delta event")
	}
	deltaEnd += deltaStart + len("\n\n")
	prefix, remainder := stream[:deltaEnd], stream[deltaEnd:]
	firstGate, finishGate := make(chan struct{}), make(chan struct{})
	finishedWaiting := make(chan struct{}, 1)
	sent := make(chan struct{})
	s := fakeupstream.New(fakeupstream.Response{Steps: []fakeupstream.Step{
		{Gate: firstGate, Sent: sent, Data: prefix},
		{Gate: finishGate, Waiting: finishedWaiting, Data: remainder},
	}})
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	captured := receiveFixture(t, s.Requests)
	if s.RequestCount() != 1 || captured.Method != http.MethodPost || captured.Header.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatalf("upstream request count/request = %d, %+v", s.RequestCount(), captured)
	}
	close(firstGate)
	receiveFixture(t, sent)
	got := make([]byte, len(prefix))
	if _, err := io.ReadFull(resp.Body, got); err != nil || !bytes.Equal(got, prefix) {
		t.Fatalf("early fixture prefix differs: %v", err)
	}
	receiveFixture(t, finishedWaiting) // Upstream cannot complete while early bytes are read.
	close(finishGate)
	gotRemainder, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(gotRemainder, remainder) {
		t.Fatalf("fixture remainder differs: %v", err)
	}
	if s.RequestCount() != 1 {
		t.Fatalf("unexpected retry: %d upstream requests", s.RequestCount())
	}
}

func TestFixtureFakeUpstreamCancellation(t *testing.T) {
	gate := make(chan struct{})
	s := fakeupstream.New(fakeupstream.Response{Steps: []fakeupstream.Step{{Gate: gate, Data: fixture(t, "stream-normal.sse")}}})
	defer s.Close()
	defer close(gate)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	captured := receiveFixture(t, s.Requests)
	cancel()
	receiveFixture(t, captured.Cancelled)
	if s.RequestCount() != 1 {
		t.Fatalf("unexpected retry after cancellation: %d", s.RequestCount())
	}
}

func TestFixtureFakeUpstreamFailureScripts(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp fakeupstream.Response
		want int
	}{
		{"http-rejection", fakeupstream.Response{Status: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": {"application/json"}}, Body: fixture(t, "http-error.json")}, http.StatusServiceUnavailable},
		{"max-token-truncation", fakeupstream.Response{Steps: []fakeupstream.Step{{Data: fixture(t, "stream-max-tokens.sse")}}}, http.StatusOK},
		{"sse-error", fakeupstream.Response{Steps: []fakeupstream.Step{{Data: fixture(t, "stream-error.sse")}}}, http.StatusOK},
		{"premature-eof", fakeupstream.Response{Steps: []fakeupstream.Step{{Data: fixture(t, "stream-premature-eof.sse"), Drop: true}}}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fakeupstream.New(tc.resp)
			defer s.Close()
			resp, err := http.Get(s.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, readErr := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.want || s.RequestCount() != 1 {
				t.Fatalf("status/count = %d/%d", resp.StatusCode, s.RequestCount())
			}
			if tc.name == "premature-eof" && readErr == nil {
				t.Fatal("connection drop was not observed")
			}
			if tc.name != "premature-eof" && readErr != nil {
				t.Fatal(readErr)
			}
			wantBody := fixture(t, map[string]string{
				"http-rejection":       "http-error.json",
				"max-token-truncation": "stream-max-tokens.sse",
				"sse-error":            "stream-error.sse",
				"premature-eof":        "stream-premature-eof.sse",
			}[tc.name])
			if !bytes.Equal(body, wantBody) {
				t.Fatalf("failure fixture bytes differ: got %d bytes, want %d", len(body), len(wantBody))
			}
			if tc.name == "http-rejection" {
				var parsed struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(body, &parsed); err != nil || parsed.Type != "error" {
					t.Fatalf("HTTP error response type=%q err=%v", parsed.Type, err)
				}
			}
			if tc.name == "sse-error" && !hasEvent(parseFixtureEvents(t, body), "error") {
				t.Fatal("SSE failure did not deliver error event")
			}
			if tc.name == "max-token-truncation" && !strings.Contains(eventData(t, parseFixtureEvents(t, body), "message_delta"), `"stop_reason":"max_tokens"`) {
				t.Fatal("truncation script did not deliver max_tokens stop")
			}
			if tc.name == "premature-eof" {
				events := parseFixtureEvents(t, body)
				if hasEvent(events, "message_stop") {
					t.Fatal("premature EOF script delivered message_stop")
				}
			}
		})
	}
}

type fixtureRequest struct {
	Model           string   `json:"model"`
	Stream          bool     `json:"stream"`
	Store           *bool    `json:"store"`
	Include         []string `json:"include"`
	MaxOutputTokens int      `json:"max_output_tokens"`
	Temperature     *float64 `json:"temperature"`
	Input           []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"input"`
}

func decodeRequestFixture(t *testing.T, name string) fixtureRequest {
	t.Helper()
	var request fixtureRequest
	if err := json.Unmarshal(fixture(t, name), &request); err != nil {
		t.Fatal(err)
	}
	return request
}

func containsRole(input []struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}, role string) bool {
	for _, item := range input {
		if item.Role == role {
			return true
		}
	}
	return false
}

type fixtureEvent struct {
	name string
	data string
}

func parseFixtureEvents(t *testing.T, raw []byte) []fixtureEvent {
	t.Helper()
	var events []fixtureEvent
	for _, frame := range strings.Split(string(raw), "\n\n") {
		if frame == "" {
			continue
		}
		var event fixtureEvent
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "event: ") {
				event.name = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				event.data += strings.TrimPrefix(line, "data: ")
			}
		}
		if event.name == "" && event.data == "" {
			continue // comment-only SSE frames have no event payload
		}
		if event.name == "" || event.data == "" {
			t.Fatalf("malformed SSE fixture frame %q", frame)
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(event.data), &value); err != nil || value["type"] != event.name {
			t.Fatalf("SSE event %q payload mismatch: %s (err %v)", event.name, event.data, err)
		}
		events = append(events, event)
	}
	return events
}

func assertFixtureEvents(t *testing.T, raw []byte, want []string) {
	t.Helper()
	events := parseFixtureEvents(t, raw)
	if len(events) != len(want) {
		t.Fatalf("SSE events = %+v, want %v", events, want)
	}
	for i, name := range want {
		if events[i].name != name {
			t.Fatalf("SSE event[%d] = %q, want %q", i, events[i].name, name)
		}
	}
}

func hasEvent(events []fixtureEvent, want string) bool {
	for _, event := range events {
		if event.name == want {
			return true
		}
	}
	return false
}

func eventData(t *testing.T, events []fixtureEvent, name string) string {
	t.Helper()
	for _, event := range events {
		if event.name == name {
			return event.data
		}
	}
	t.Fatalf("SSE event %q missing from %+v", name, events)
	return ""
}

func receiveFixture[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for fake upstream")
		var zero T
		return zero
	}
}
