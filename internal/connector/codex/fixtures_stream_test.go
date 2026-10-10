package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
)

const responsesFixtureDir = "testdata/responses"

func loadResponsesFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(responsesFixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parseResponsesHashes(manifest []byte) (map[string]string, error) {
	lines := strings.Split(string(manifest), "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "| File | SHA-256 |" {
			start = i
			break
		}
	}
	if start < 0 || start+1 >= len(lines) || strings.TrimSpace(lines[start+1]) != "| --- | --- |" {
		return nil, fmt.Errorf("missing or malformed SHA-256 table header")
	}
	hashes := make(map[string]string)
	for i, line := range lines[start+2:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 4 {
			return nil, fmt.Errorf("malformed SHA-256 row at line %d", start+i+3)
		}
		nameCell, hashCell := strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
		name, sum := strings.Trim(nameCell, "`"), strings.Trim(hashCell, "`")
		if nameCell != "`"+name+"`" || hashCell != "`"+sum+"`" || strings.ContainsAny(name, `/\\`) || !(strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".sse")) {
			return nil, fmt.Errorf("invalid fixture name/hash cell at line %d", start+i+3)
		}
		decoded, err := hex.DecodeString(sum)
		if err != nil || len(decoded) != sha256.Size {
			return nil, fmt.Errorf("invalid SHA-256 for %s", name)
		}
		if _, duplicate := hashes[name]; duplicate {
			return nil, fmt.Errorf("duplicate SHA-256 row for %s", name)
		}
		hashes[name] = sum
	}
	if len(hashes) == 0 {
		return nil, fmt.Errorf("SHA-256 table contains no fixture rows")
	}
	return hashes, nil
}

func responsesHashMatches(data []byte, want string) bool {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == want
}

type responsesEvent struct {
	name string
	data string
}

func parseResponsesEvents(raw []byte) ([]responsesEvent, error) {
	return parseResponsesEventReader(bytes.NewReader(raw))
}

func parseResponsesEventReader(r io.Reader) ([]responsesEvent, error) {
	var events []responsesEvent
	event := responsesEvent{}
	var data []string
	finish := func() error {
		if event.name == "" && len(data) == 0 { // comment/keepalive frame
			return nil
		}
		if event.name == "" || len(data) == 0 {
			return fmt.Errorf("malformed SSE event %q", event.name)
		}
		event.data = strings.Join(data, "\n")
		var payload map[string]json.RawMessage
		if err := json.Unmarshal([]byte(event.data), &payload); err != nil {
			return fmt.Errorf("decode %s payload: %w", event.name, err)
		}
		var typ string
		if err := json.Unmarshal(payload["type"], &typ); err != nil || typ != event.name {
			return fmt.Errorf("event %q payload type %q mismatch", event.name, typ)
		}
		events = append(events, event)
		event = responsesEvent{}
		data = nil
		return nil
	}
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			if finishErr := finish(); finishErr != nil {
				return nil, finishErr
			}
		} else if strings.HasPrefix(line, "event:") {
			event.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			data = append(data, strings.TrimPrefix(value, " "))
		}
		if err != nil {
			if err != io.EOF {
				return nil, err
			}
			if len(line) > 0 || event.name != "" || len(data) > 0 {
				if finishErr := finish(); finishErr != nil {
					return nil, finishErr
				}
			}
			return events, nil
		}
	}
}

type fixtureChunkReader struct {
	chunks [][]byte
}

func (r *fixtureChunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks = r.chunks[1:]
	return n, nil
}

func TestResponsesFixtureIntegrityAndMalformedInput(t *testing.T) {
	manifest := loadResponsesFixture(t, "README.md")
	if !bytes.Contains(manifest, []byte("synthetic")) || !bytes.Contains(manifest, []byte("No fixture contains usable credentials")) || !bytes.Contains(manifest, []byte("Intended stream outcomes")) {
		t.Fatal("manifest must state synthetic provenance, secret exclusion, and stream outcomes")
	}
	hashes, err := parseResponsesHashes(manifest)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(responsesFixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	disk := make(map[string]bool)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".sse") {
			disk[entry.Name()] = true
		}
	}
	for name := range disk {
		if _, ok := hashes[name]; !ok {
			t.Errorf("fixture %s missing from manifest", name)
		}
	}
	for name, want := range hashes {
		data, err := os.ReadFile(filepath.Join(responsesFixtureDir, name))
		if err != nil || !disk[name] || !responsesHashMatches(data, want) {
			t.Errorf("fixture %s missing or SHA-256 mismatch (read error: %v)", name, err)
		}
	}
	mutated := append([]byte(nil), loadResponsesFixture(t, "request-positive.json")...)
	mutated[0] ^= 1
	if responsesHashMatches(mutated, hashes["request-positive.json"]) {
		t.Fatal("mutated fixture unexpectedly matched manifest hash")
	}
	for _, malformed := range []string{
		"| File | SHA-256 |\n| --- | --- |\n| `../escape.sse` | `" + strings.Repeat("a", 64) + "` |\n",
		"| File | SHA-256 |\n| --- | --- |\n| `duplicate.sse` | `" + strings.Repeat("a", 64) + "` |\n| `duplicate.sse` | `" + strings.Repeat("a", 64) + "` |\n",
		"| File | SHA-256 |\n| --- | --- |\n| `bad.sse` | `xyz` |\n",
	} {
		if _, err := parseResponsesHashes([]byte(malformed)); err == nil {
			t.Errorf("malformed manifest accepted: %q", malformed)
		}
	}

	var positive map[string]json.RawMessage
	if err := json.Unmarshal(loadResponsesFixture(t, "request-positive.json"), &positive); err != nil {
		t.Fatal(err)
	}
	var model string
	var stream, store bool
	if json.Unmarshal(positive["model"], &model) != nil || model == "" || json.Unmarshal(positive["stream"], &stream) != nil || !stream || json.Unmarshal(positive["store"], &store) != nil || store {
		t.Fatalf("positive request fields invalid: %s", positive)
	}
	if !bytes.Contains(positive["instructions"], []byte("Answer")) || !bytes.Contains(positive["include"], []byte("reasoning.encrypted_content")) {
		t.Fatal("positive request is missing instructions or encrypted reasoning include")
	}
	var positiveInput []struct {
		Type  string `json:"type"`
		Role  string `json:"role"`
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal(positive["input"], &positiveInput); err != nil || len(positiveInput) < 2 || positiveInput[0].Type != "message" || positiveInput[0].Role != "assistant" || positiveInput[0].Phase != "commentary" {
		t.Fatalf("request commentary phase mismatch: %+v err=%v", positiveInput, err)
	}
	for name, want := range map[string]bool{"request-store-true.json": true, "request-stream-false.json": false} {
		var request struct {
			Store  *bool `json:"store"`
			Stream *bool `json:"stream"`
		}
		if err := json.Unmarshal(loadResponsesFixture(t, name), &request); err != nil || request.Store == nil || request.Stream == nil || *request.Store != want || *request.Stream != (name != "request-stream-false.json") {
			t.Fatalf("negative request %s mismatch: %+v err=%v", name, request, err)
		}
	}
	var unknown struct {
		Optional struct {
			Opaque []json.RawMessage `json:"opaque"`
		} `json:"optional_extension"`
	}
	var nestedExtension struct {
		X string `json:"x"`
	}
	if err := json.Unmarshal(loadResponsesFixture(t, "request-unknown-fields.json"), &unknown); err != nil || len(unknown.Optional.Opaque) != 2 || json.Unmarshal(unknown.Optional.Opaque[1], &nestedExtension) != nil || nestedExtension.X != "preserve-me" {
		t.Fatalf("unknown extension not present: %v", err)
	}
	// Negative fixtures assert explicit field shapes only; no production rejection path is exercised here.
	for _, name := range []string{"request-tools-history.json", "request-reasoning-item.json"} {
		var value any
		if err := json.Unmarshal(loadResponsesFixture(t, name), &value); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
	}
	var toolsRequest struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Input []struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			CallID string `json:"call_id"`
		} `json:"input"`
	}
	if err := json.Unmarshal(loadResponsesFixture(t, "request-tools-history.json"), &toolsRequest); err != nil || len(toolsRequest.Tools) != 1 || toolsRequest.Tools[0].Name != "lookup" || len(toolsRequest.Input) != 6 || toolsRequest.Input[1].ID != "fc_alpha" || toolsRequest.Input[1].CallID != "call_alpha" || toolsRequest.Input[2].Type != "function_call_output" || toolsRequest.Input[2].CallID != "call_alpha" {
		t.Fatal("tool history does not distinguish item IDs, call IDs, and results")
	}
	var reasoningRequest struct {
		Input []struct {
			Type      string `json:"type"`
			Encrypted string `json:"encrypted_content"`
			Summary   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"input"`
	}
	if err := json.Unmarshal(loadResponsesFixture(t, "request-reasoning-item.json"), &reasoningRequest); err != nil || len(reasoningRequest.Input) == 0 || reasoningRequest.Input[0].Type != "reasoning" || reasoningRequest.Input[0].Encrypted != "synthetic-ciphertext-not-a-credential" || reasoningRequest.Input[0].Summary[0].Text != "Synthetic summary fragment." {
		t.Fatal("opaque reasoning item is missing ciphertext or summary fragment")
	}

	wantEvents := map[string][]string{
		"stream-normal-text.sse": {"response.created", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.content_part.done", "response.output_item.done", "response.completed"},
		"stream-tools.sse":       {"response.output_item.added", "response.function_call_arguments.delta", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.output_item.done", "response.completed"},
		"stream-incomplete.sse":  {"response.incomplete"}, "stream-failed.sse": {"response.failed"}, "stream-error.sse": {"error"},
	}
	for name, want := range wantEvents {
		events, err := parseResponsesEvents(loadResponsesFixture(t, name))
		if err != nil || len(events) != len(want) {
			t.Fatalf("%s events=%v err=%v", name, events, err)
		}
		for i := range want {
			if events[i].name != want[i] {
				t.Fatalf("%s event[%d]=%s want %s", name, i, events[i].name, want[i])
			}
		}
	}
	normal, _ := parseResponsesEvents(loadResponsesFixture(t, "stream-normal-text.sse"))
	var commentary struct {
		Item struct {
			Role  string `json:"role"`
			Phase string `json:"phase"`
		} `json:"item"`
	}
	if json.Unmarshal([]byte(normal[1].data), &commentary) != nil || commentary.Item.Role != "assistant" || commentary.Item.Phase != "commentary" || commentary.Item.Phase != positiveInput[0].Phase {
		t.Fatalf("SSE commentary message phase mismatch: %+v", commentary)
	}
	tools, _ := parseResponsesEvents(loadResponsesFixture(t, "stream-tools.sse"))
	var toolAdded, toolDone struct {
		Item struct {
			ID     string `json:"id"`
			CallID string `json:"call_id"`
		} `json:"item"`
	}
	if json.Unmarshal([]byte(tools[0].data), &toolAdded) != nil || json.Unmarshal([]byte(tools[4].data), &toolDone) != nil || toolAdded.Item.ID != "fc_item_1" || toolAdded.Item.CallID != "call_1" || toolDone.Item.ID != toolAdded.Item.ID || toolDone.Item.CallID != toolAdded.Item.CallID {
		t.Fatal("tool item and call identifiers are not represented")
	}
	interleaved, err := parseResponsesEvents(loadResponsesFixture(t, "stream-interleaved.sse"))
	var firstDelta, secondDelta struct {
		ItemID      string `json:"item_id"`
		OutputIndex int    `json:"output_index"`
	}
	if len(interleaved) > 1 {
		_ = json.Unmarshal([]byte(interleaved[0].data), &firstDelta)
		_ = json.Unmarshal([]byte(interleaved[1].data), &secondDelta)
	}
	if err != nil || len(interleaved) != 5 || firstDelta.ItemID != "msg_a" || secondDelta.ItemID != "msg_b" || firstDelta.OutputIndex != 0 || secondDelta.OutputIndex != 1 {
		t.Fatalf("interleaved stream mismatch: %v err=%v", interleaved, err)
	}
	var extension struct {
		Future struct {
			Preserve bool `json:"preserve"`
		} `json:"future_extension"`
	}
	if json.Unmarshal([]byte(interleaved[0].data), &extension) != nil || !extension.Future.Preserve {
		t.Fatal("unknown event extension is not represented")
	}
	crlfRaw := loadResponsesFixture(t, "stream-crlf-multiline.sse")
	if !bytes.Contains(crlfRaw, []byte("\r\n")) || !bytes.Contains(crlfRaw, []byte("data: \"item_id\"")) {
		t.Fatal("CRLF/multiline fixture lost its framing")
	}
	crlf, err := parseResponsesEvents(crlfRaw)
	if err != nil || len(crlf) != 2 || !strings.Contains(crlf[0].data, "café") {
		t.Fatalf("CRLF/multiline parse mismatch: %v err=%v", crlf, err)
	}
	// One-byte reads split every adjacent pair, including CR|LF and field-name|colon/data-prefix boundaries.
	crlfChunks := make([][]byte, len(crlfRaw))
	for i := range crlfRaw {
		crlfChunks[i] = crlfRaw[i : i+1]
	}
	chunked, err := parseResponsesEventReader(&fixtureChunkReader{chunks: crlfChunks})
	if err != nil || len(chunked) != len(crlf) || chunked[0] != crlf[0] || chunked[1] != crlf[1] {
		t.Fatalf("one-byte CRLF/multiline chunks changed parsed events: %v err=%v", chunked, err)
	}
	var reasoningEvent struct {
		Item struct {
			Type      string `json:"type"`
			Encrypted string `json:"encrypted_content"`
			Summary   []struct {
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"item"`
	}
	reasoningEvents, err := parseResponsesEvents(loadResponsesFixture(t, "stream-reasoning.sse"))
	if err != nil || len(reasoningEvents) < 2 || json.Unmarshal([]byte(reasoningEvents[1].data), &reasoningEvent) != nil || reasoningEvent.Item.Type != "reasoning" || reasoningEvent.Item.Encrypted != "synthetic-ciphertext-not-secret" || reasoningEvent.Item.Summary[0].Text != "Synthetic reasoning summary." {
		t.Fatalf("reasoning stream opaque fields mismatch: %+v err=%v", reasoningEvent, err)
	}
	usage, err := parseResponsesEvents(loadResponsesFixture(t, "stream-normal-text.sse"))
	if err != nil || !strings.Contains(usage[len(usage)-1].data, `"total_tokens":17`) || !strings.Contains(usage[len(usage)-1].data, `"cached_tokens":3`) || !strings.Contains(usage[len(usage)-1].data, `"reasoning_tokens":2`) {
		t.Fatalf("inclusive/subset usage missing: %v err=%v", usage, err)
	}
	missingUsage, err := parseResponsesEvents(loadResponsesFixture(t, "stream-usage-zero-missing.sse"))
	var response struct {
		Response map[string]json.RawMessage `json:"response"`
	}
	if err != nil || json.Unmarshal([]byte(missingUsage[0].data), &response) != nil || len(response.Response) != 2 || response.Response["usage"] != nil {
		t.Fatalf("zero/missing usage mismatch: %v err=%v", missingUsage, err)
	}
	boundary := loadResponsesFixture(t, "stream-boundary-keepalive.sse")
	if len(boundary) < 1024*1024-1024 || len(boundary) >= 1024*1024+1024 {
		t.Fatalf("boundary fixture size = %d, not near the 1 MiB ceiling", len(boundary))
	}
	if events, err := parseResponsesEvents(boundary); err != nil || len(events) != 3 || events[0].name != "rate_limits" || events[1].name != "response.output_text.delta" {
		t.Fatalf("boundary event framing mismatch: %v err=%v", events, err)
	}
	premature := loadResponsesFixture(t, "stream-premature-eof.sse")
	if bytes.HasSuffix(premature, []byte("\n\n")) || !bytes.Contains(premature, []byte(`"delta":"truncated`)) {
		t.Fatal("premature EOF fixture unexpectedly has a terminated final delta")
	}
}

func TestResponsesFixtureFakeUpstreamStreaming(t *testing.T) {
	stream := loadResponsesFixture(t, "stream-normal-text.sse")
	deltaEnd := bytes.Index(stream, []byte("Hello\""))
	if deltaEnd < 0 {
		t.Fatal("normal stream fixture has no text delta")
	}
	deltaEnd += len("Hello\"")
	frameEnd := bytes.Index(stream[deltaEnd:], []byte("\n\n"))
	if frameEnd < 0 {
		t.Fatal("text delta fixture event is unterminated")
	}
	deltaEnd += frameEnd + 2
	prefix, remainder := stream[:deltaEnd], stream[deltaEnd:]
	firstGate, finishGate := make(chan struct{}), make(chan struct{})
	waiting, sent := make(chan struct{}, 1), make(chan struct{})
	s := fakeupstream.New(fakeupstream.Response{Steps: []fakeupstream.Step{{Gate: firstGate, Sent: sent, Data: prefix}, {Gate: finishGate, Waiting: waiting, Data: remainder}}})
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/v1/responses", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	captured := receiveResponsesSignal(t, s.Requests)
	close(firstGate)
	receiveResponsesSignal(t, sent)
	got := make([]byte, len(prefix))
	if _, err := io.ReadFull(resp.Body, got); err != nil || !bytes.Equal(got, prefix) {
		t.Fatalf("early prefix mismatch: %v", err)
	}
	receiveResponsesSignal(t, waiting) // second event cannot be written until explicitly released
	cancel()
	receiveResponsesSignal(t, captured.Cancelled)
	if s.RequestCount() != 1 {
		t.Fatalf("unexpected retry: %d", s.RequestCount())
	}

	// Split the two-byte UTF-8 rune between independently flushed upstream steps.
	unicode := loadResponsesFixture(t, "stream-crlf-multiline.sse")
	mark := bytes.Index(unicode, []byte("é"))
	if mark < 1 {
		t.Fatal("Unicode fixture has no multibyte rune")
	}
	s3 := fakeupstream.New(fakeupstream.Response{Steps: []fakeupstream.Step{{Data: unicode[:mark+1]}, {Data: unicode[mark+1:]}}})
	defer s3.Close()
	r3, err := http.Get(s3.URL + "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	unicodeGot, err := io.ReadAll(r3.Body)
	_ = r3.Body.Close()
	if err != nil || !bytes.Equal(unicodeGot, unicode) || s3.RequestCount() != 1 {
		t.Fatalf("split Unicode bytes changed: err=%v requests=%d", err, s3.RequestCount())
	}

	// A separate dropped response proves abrupt EOF after a valid prefix, without replay.
	partial := loadResponsesFixture(t, "stream-premature-eof.sse")
	lastFrame := bytes.LastIndex(partial, []byte("\n\n")) + 2
	s2 := fakeupstream.New(fakeupstream.Response{Steps: []fakeupstream.Step{{Data: partial[:lastFrame]}, {Data: partial[lastFrame:], Drop: true}}})
	defer s2.Close()
	r2, err := http.Get(s2.URL + "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(r2.Body)
	_ = r2.Body.Close()
	if !bytes.Contains(body, []byte("response.created")) || readErr == nil || s2.RequestCount() != 1 {
		t.Fatalf("premature drop not observed: bytes=%d err=%v requests=%d", len(body), readErr, s2.RequestCount())
	}
}

func receiveResponsesSignal[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for fake upstream")
		var zero T
		return zero
	}
}
