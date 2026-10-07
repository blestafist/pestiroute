package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
	"github.com/blestafist/pestiroute/internal/testutil/codexfixtures"
	"github.com/google/uuid"
)

const customToolRequestFixture = codexfixtures.CustomInitial

const customToolEvidenceDirectory = "/tmp/opencode/pestiroute-tool-evidence"
const customToolArtifactName = "custom-tool-roundtrip.json"

type customToolArtifact struct {
	SchemaVersion        int    `json:"schema_version"`
	Profile              string `json:"profile"`
	Model                string `json:"model"`
	FirstHTTPStatus      int    `json:"first_http_status"`
	SecondHTTPStatus     int    `json:"second_http_status"`
	FirstRejectionCode   string `json:"first_rejection_code"`
	FirstRejectionField  string `json:"first_rejection_field"`
	SecondRejectionCode  string `json:"second_rejection_code"`
	SecondRejectionField string `json:"second_rejection_field"`
	CustomCallCount      int    `json:"custom_call_count"`
	ValidatedInput       bool   `json:"validated_input"`
	LocalResult          bool   `json:"local_result"`
	CallResultLinked     bool   `json:"call_result_linked"`
	FinalAnswerVerified  bool   `json:"final_answer_verified"`
	FirstTerminal        string `json:"first_terminal"`
	SecondTerminal       string `json:"second_terminal"`
	UsageObserved        bool   `json:"usage_observed"`
	DirectRequests       int    `json:"direct_requests"`
	Classification       string `json:"classification"`
}

type customProbeHistory struct {
	items  [][]byte
	callID string
	input  string
	answer string
}

type customProbeResponse struct {
	calls          int
	status         int
	terminal       string
	usage          bool
	history        customProbeHistory
	rejectionCode  string
	rejectionField string
}

type customFragmentReader struct{ io.Reader }

func (r customFragmentReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.Reader.Read(p)
}

func (h *customProbeHistory) clear() {
	for _, item := range h.items {
		clear(item)
	}
	h.items = nil
	h.callID, h.input, h.answer = "", "", ""
}

func validCustomProbeInput(input string) bool {
	return len(input) > 0 && len(input) <= toolDiagnosticArgLimit && input == "pestiRoute marker"
}

func buildCustomToolRequest(threadID string) ([]byte, error) {
	threadUUID, err := uuid.Parse(threadID)
	if err != nil {
		return nil, errors.New("invalid custom probe thread id")
	}
	var request map[string]any
	if json.Unmarshal([]byte(customToolRequestFixture), &request) != nil {
		return nil, errors.New("invalid custom probe fixture")
	}
	input := request["input"].([]any)
	prefix := input[0].(map[string]any)
	toolsJSON, err := json.Marshal(prefix["tools"])
	if err != nil {
		return nil, errors.New("custom tools serialization failed")
	}
	namespace := uuid.NewSHA1(uuid.NameSpaceOID, []byte(threadUUID.String()))
	prefix["id"] = "at_" + uuid.NewSHA1(namespace, toolsJSON).String()
	return json.Marshal(request)
}

// runCustomToolProbe is an opt-in test helper with a hard two-request ceiling.
// It accepts an injected client so offline tests cannot touch credentials or the network.
func runCustomToolProbe(ctx context.Context, client *http.Client, endpoint string, initial []byte, bundle *oauthBundle, threadID string) (customToolArtifact, error) {
	a := customToolArtifact{SchemaVersion: 1, Profile: "codex-responses-http-sse-lite-v1", Model: "gpt-6-luna", Classification: "not_run"}
	if client == nil || endpoint == "" || len(initial) == 0 || len(initial) > toolDiagnosticBodyLimit {
		return a, errors.New("invalid custom tool probe setup")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	probeClient := *client
	probeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	first, err := customProbeRequest(ctx, &probeClient, endpoint, initial, bundle, threadID)
	a.DirectRequests = 1
	a.FirstHTTPStatus, a.FirstTerminal, a.UsageObserved = first.status, first.terminal, first.usage
	a.FirstRejectionCode, a.FirstRejectionField = first.rejectionCode, first.rejectionField
	defer first.history.clear()
	if err != nil {
		a.Classification = "first_request_failed"
		return a, err
	}
	a.CustomCallCount = first.calls
	if first.calls != 1 || first.terminal != "completed" || first.history.callID == "" || !validCustomProbeInput(first.history.input) {
		a.Classification = "no_valid_custom_call"
		return a, errors.New("custom tool call was not valid")
	}
	a.ValidatedInput = true
	result := map[string]string{"pestiRoute marker": "synthetic-echo-ok"}[first.history.input]
	if result == "" || len(result) > toolDiagnosticArgLimit {
		return a, errors.New("synthetic local result failed")
	}
	a.LocalResult = true
	followup, err := buildCustomProbeFollowup(initial, first.history.items, first.history.callID, result)
	if err != nil {
		return a, err
	}
	defer clear(followup)
	second, err := customProbeRequest(ctx, &probeClient, endpoint, followup, bundle, threadID)
	a.DirectRequests, a.SecondHTTPStatus, a.SecondTerminal = 2, second.status, second.terminal
	a.SecondRejectionCode, a.SecondRejectionField = second.rejectionCode, second.rejectionField
	a.UsageObserved = a.UsageObserved || second.usage
	defer second.history.clear()
	if err != nil {
		a.Classification = "followup_failed"
		return a, err
	}
	a.CallResultLinked = true
	a.FinalAnswerVerified = second.terminal == "completed" && second.history.answer == "synthetic-echo-ok"
	if !a.FinalAnswerVerified {
		return a, errors.New("custom tool final answer was not verified")
	}
	a.Classification = "roundtrip_completed"
	return a, nil
}

func customProbeRequest(ctx context.Context, client *http.Client, endpoint string, body []byte, bundle *oauthBundle, threadID string) (customProbeResponse, error) {
	result := customProbeResponse{history: customProbeHistory{items: [][]byte{}}, rejectionCode: "other", rejectionField: "other"}
	if len(body) > toolDiagnosticBodyLimit {
		return result, errors.New("custom tool request exceeds bound")
	}
	var req *http.Request
	var err error
	if bundle != nil {
		req, err = makeRoundtripRequestTo(ctx, endpoint, body, *bundle, threadID)
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	}
	if err != nil {
		return result, err
	}
	if bundle == nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Accept-Encoding", "identity")
	}
	req.Close = true
	resp, err := client.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	result.status = resp.StatusCode
	if resp.ProtoMajor != 1 || resp.ProtoMinor != 1 {
		return result, errors.New("custom tool probe requires HTTP/1.1")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, toolDiagnosticBodyLimit+1))
		if readErr != nil || len(body) > toolDiagnosticBodyLimit {
			clear(body)
			return result, errors.New("bounded rejection read failed")
		}
		_, result.rejectionCode, result.rejectionField = classifyDiagnosticError(body)
		clear(body)
		return result, errors.New("custom tool probe rejected")
	}
	result.calls, result.terminal, result.usage, result.history, err = observeCustomProbeSSE(io.LimitReader(resp.Body, toolDiagnosticBodyLimit+1))
	return result, err
}

func observeCustomProbeSSE(body io.Reader) (int, string, bool, customProbeHistory, error) {
	h := customProbeHistory{items: [][]byte{}}
	r := bufio.NewReaderSize(body, 4096)
	var total, itemBytes, calls int
	var eventName string
	var data []byte
	var terminal string
	var usage bool
	var deltas strings.Builder
	defer clear(data)
	for {
		line, err := r.ReadString('\n')
		total += len(line)
		if total > toolDiagnosticBodyLimit {
			h.clear()
			return calls, terminal, usage, h, errors.New("custom tool response exceeds bound")
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" && len(data) > 0 {
			var event struct {
				Type     string          `json:"type"`
				Item     json.RawMessage `json:"item"`
				Delta    string          `json:"delta"`
				Response struct {
					Status string          `json:"status"`
					Usage  json.RawMessage `json:"usage"`
				} `json:"response"`
			}
			if json.Unmarshal(data, &event) != nil {
				h.clear()
				return calls, terminal, usage, h, errors.New("malformed custom tool event")
			}
			if terminal != "" {
				h.clear()
				return calls, terminal, usage, h, errors.New("trailing custom tool event")
			}
			kind := eventName
			if kind == "" {
				kind = event.Type
			}
			if len(event.Response.Usage) > 0 && string(event.Response.Usage) != "null" {
				usage = true
			}
			switch kind {
			case "response.custom_tool_call_input.delta":
				if deltas.Len()+len(event.Delta) > toolDiagnosticArgLimit {
					h.clear()
					return calls, terminal, usage, h, errors.New("custom input exceeds bound")
				}
				deltas.WriteString(event.Delta)
			case "response.output_item.done":
				if len(event.Item) > 0 {
					var item struct {
						Type   string `json:"type"`
						Name   string `json:"name"`
						CallID string `json:"call_id"`
						Input  string `json:"input"`
					}
					if json.Unmarshal(event.Item, &item) == nil {
						if item.Type == "custom_tool_call" {
							calls++
							if calls == 1 && item.Name == "synthetic_echo" && item.CallID != "" && len(item.CallID) <= 128 && safeHeaderValue(item.CallID) && validCustomProbeInput(item.Input) && deltas.String() == item.Input {
								h.callID, h.input = item.CallID, item.Input
							}
						}
					}
					if len(event.Item) > toolDiagnosticBodyLimit-itemBytes {
						h.clear()
						return calls, terminal, usage, h, errors.New("custom history exceeds bound")
					}
					h.items = append(h.items, bytes.Clone(event.Item))
					itemBytes += len(event.Item)
				}
			case "response.output_text.delta":
				if len(h.answer)+len(event.Delta) > toolDiagnosticArgLimit {
					h.clear()
					return calls, terminal, usage, h, errors.New("custom final output exceeds bound")
				}
				h.answer += event.Delta
			case "response.completed":
				terminal = "incomplete"
				if event.Response.Status == "completed" {
					terminal = "completed"
				}
			case "response.failed", "error":
				terminal = "failed"
			case "response.incomplete":
				terminal = "incomplete"
			}
			clear(data)
			data, eventName = nil, ""
		}
		if event, ok := strings.CutPrefix(line, "event: "); ok {
			eventName = event
		} else if value, ok := strings.CutPrefix(line, "data: "); ok {
			data = append(data, value...)
			data = append(data, '\n')
		}
		if err != nil {
			if !errors.Is(err, io.EOF) || len(line) > 0 || len(data) > 0 || terminal != "completed" {
				h.clear()
				return calls, terminal, usage, h, errors.New("custom response stream incomplete")
			}
			return calls, terminal, usage, h, nil
		}
	}
}

func buildCustomProbeFollowup(initial []byte, items [][]byte, callID, output string) ([]byte, error) {
	if len(initial) == 0 || len(initial) > toolDiagnosticBodyLimit || len(items) == 0 || callID == "" || len(callID) > 128 || !safeHeaderValue(callID) || output != "synthetic-echo-ok" || len(output) > toolDiagnosticArgLimit {
		return nil, errors.New("invalid custom follow-up")
	}
	for _, raw := range items {
		if !json.Valid(raw) || len(raw) > toolDiagnosticBodyLimit {
			return nil, errors.New("invalid custom history item")
		}
	}
	start, end, err := roundtripInputBounds(initial)
	if err != nil || !validInitialRoundtripInput(initial[start:end]) {
		return nil, errors.New("invalid custom initial request")
	}
	outputItem, _ := json.Marshal(struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}{"custom_tool_call_output", callID, output})
	input := initial[start:end]
	close := len(input) - 1
	for close >= 0 && isJSONWhitespace(input[close]) {
		close--
	}
	if close < 0 || input[close] != ']' {
		return nil, errors.New("invalid custom input array")
	}
	extra := len(outputItem) + 1
	for _, item := range items {
		extra += len(item) + 1
	}
	if len(initial)+extra > toolDiagnosticBodyLimit {
		return nil, errors.New("custom follow-up exceeds bound")
	}
	body := make([]byte, 0, len(initial)+extra)
	body = append(body, initial[:start]...)
	body = append(body, input[:close]...)
	for _, item := range items {
		body = append(body, ',')
		body = append(body, item...)
	}
	body = append(body, ',')
	body = append(body, outputItem...)
	body = append(body, input[close:]...)
	body = append(body, initial[end:]...)
	return body, nil
}

func writeCustomToolArtifact(a customToolArtifact, directory string) error {
	if a.SchemaVersion != 1 || a.Profile != "codex-responses-http-sse-lite-v1" || a.Model != "gpt-6-luna" || a.DirectRequests < 1 || a.DirectRequests > 2 || a.CustomCallCount < 0 || a.CustomCallCount > 3 || a.FirstHTTPStatus < 0 || a.FirstHTTPStatus > 599 || a.SecondHTTPStatus < 0 || a.SecondHTTPStatus > 599 || !oneOf(a.FirstRejectionCode, "other", "invalid_request_error", "unsupported_parameter", "unsupported_value", "model_not_found", "insufficient_quota", "rate_limit_exceeded", "server_error") || !oneOf(a.SecondRejectionCode, "other", "invalid_request_error", "unsupported_parameter", "unsupported_value", "model_not_found", "insufficient_quota", "rate_limit_exceeded", "server_error") || !oneOf(a.FirstRejectionField, "other", "model", "input", "tools", "tool_choice", "parallel_tool_calls") || !oneOf(a.SecondRejectionField, "other", "model", "input", "tools", "tool_choice", "parallel_tool_calls") || !oneOf(a.FirstTerminal, "none", "completed", "failed", "incomplete") || !oneOf(a.SecondTerminal, "none", "completed", "failed", "incomplete") || !oneOf(a.Classification, "not_run", "first_request_failed", "no_valid_custom_call", "followup_failed", "roundtrip_completed") {
		return errors.New("invalid custom tool artifact")
	}
	if directory == "" {
		if err := ensureDiagnosticResultDirectory(customToolEvidenceDirectory); err != nil {
			return errors.New("unsafe custom tool artifact directory")
		}
		directory = customToolEvidenceDirectory
	}
	if verifyDiagnosticDirectory(directory, true) != nil {
		return errors.New("unsafe custom tool artifact directory")
	}
	data, err := json.Marshal(a)
	if err != nil {
		return errors.New("custom tool artifact encoding failed")
	}
	path := filepath.Join(directory, customToolArtifactName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("custom tool artifact destination unavailable")
	}
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return errors.New("custom tool artifact write failed")
	}
	return nil
}

func TestOperatorCustomToolRoundtrip(t *testing.T) {
	if os.Getenv("PESTIROUTE_DIAG_CUSTOM_TOOL_PROBE") != "authorized" {
		t.Skip("operator custom-tool probe not enabled")
	}
	dbPath, keyPath, account := os.Getenv("PESTIROUTE_DIAG_DB"), os.Getenv("PESTIROUTE_DIAG_KEY"), os.Getenv("PESTIROUTE_DIAG_ACCOUNT")
	if dbPath == "" || keyPath == "" || account == "" {
		t.Fatal("custom tool credential checkpoint failed")
	}
	if err := ensureDiagnosticResultDirectory(customToolEvidenceDirectory); err != nil {
		t.Fatal("custom tool evidence destination unavailable")
	}
	artifactPath := filepath.Join(customToolEvidenceDirectory, customToolArtifactName)
	if _, err := os.Lstat(artifactPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("custom tool artifact already exists or destination is unavailable")
	}
	key, err := crypto.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal("custom tool credential checkpoint failed")
	}
	db, err := openDiagnosticDB(dbPath)
	if err != nil {
		t.Fatal("custom tool database checkpoint failed")
	}
	defer db.Close()
	repo := sqlite.NewCredentials(db)
	row, err := repo.Get(context.Background(), account, "oauth")
	if err != nil || row.ExpiresAt == nil || !row.ExpiresAt.After(time.Now()) {
		t.Fatal("custom tool credential checkpoint failed")
	}
	plain, err := repo.GetDecrypted(context.Background(), account, "oauth", key)
	if err != nil {
		t.Fatal("custom tool credential checkpoint failed")
	}
	defer clear(plain)
	bundle, err := decodeOAuthBundle(plain)
	if err != nil || !bundle.ExpiresAt.After(time.Now()) || bundle.ExpiresAt.UnixMilli() != row.ExpiresAt.UnixMilli() {
		t.Fatal("custom tool credential checkpoint failed")
	}
	threadID := uuid.NewString()
	initial, err := buildCustomToolRequest(threadID)
	if err != nil {
		t.Fatal("custom tool request construction failed")
	}
	defer clear(initial)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	configureCodexTransport(transport)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	a, probeErr := runCustomToolProbe(context.Background(), client, codexResponsesEndpoint, initial, &bundle, threadID)
	if err := writeCustomToolArtifact(a, ""); err != nil {
		t.Fatal("custom tool artifact write failed")
	}
	if probeErr != nil {
		t.Fatal("custom tool probe stopped; inspect only the sanitized artifact")
	}
}

func TestCustomToolProbeFakeTwoRequestRoundtrip(t *testing.T) {
	var requests [][]byte
	threadID := "00000000-0000-4000-8000-000000000001"
	bundle := &oauthBundle{AccessToken: "SYNTHETIC_TOKEN", AccountID: "synthetic-account", ExpiresAt: time.Now().Add(time.Hour)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/" || r.Proto != "HTTP/1.1" || r.Header.Get("Authorization") != "Bearer SYNTHETIC_TOKEN" || r.Header.Get("ChatGPT-Account-Id") != "synthetic-account" || r.Header.Get("originator") != "pestiroute" || r.Header.Get("User-Agent") != "PestiRoute" || r.Header.Get("session-id") != threadID || r.Header.Get("thread-id") != threadID || r.Header.Get("x-client-request-id") != threadID || r.Header.Get("x-openai-internal-codex-responses-lite") != "true" || r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("request did not use the reviewed scoped Lite header assembly")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, toolDiagnosticBodyLimit+1))
		if err != nil || len(body) > toolDiagnosticBodyLimit {
			t.Error("request body exceeded bound")
			return
		}
		requests = append(requests, body)
		w.Header().Set("Content-Type", "text/event-stream")
		if len(requests) == 1 {
			fmt.Fprint(w, "event: response.custom_tool_call_input.delta\ndata: {\"type\":\"response.custom_tool_call_input.delta\",\"delta\":\"pestiRoute \"}\n\nevent: response.custom_tool_call_input.delta\ndata: {\"type\":\"response.custom_tool_call_input.delta\",\"delta\":\"marker\"}\n\n")
			fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"r\",\"encrypted_content\":\"cipher<>&é\",\"extra\":1}}\n\n")
			fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\": { \"type\":\"custom_tool_call\", \"call_id\":\"c-1\", \"name\":\"synthetic_echo\", \"input\":\"pestiRoute marker\", \"extra\":\"<>&é\" }}\n\n")
			fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1}}}\n\n")
			return
		}
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"synthetic-echo-ok\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()
	ctx := context.Background()
	client := server.Client()
	a, err := runCustomToolProbe(ctx, client, server.URL, []byte(customToolRequestFixture), bundle, threadID)
	if err != nil || a.DirectRequests != 2 || !a.ValidatedInput || !a.LocalResult || !a.CallResultLinked || !a.FinalAnswerVerified || a.FirstTerminal != "completed" || a.SecondTerminal != "completed" || !a.UsageObserved {
		t.Fatalf("custom probe did not complete: %+v, %v", a, err)
	}
	if len(requests) != 2 || !bytes.Equal(requests[0], []byte(customToolRequestFixture)) || !bytes.Contains(requests[0], []byte(`"type":"custom"`)) || !bytes.Contains(requests[0], []byte(`"syntax":"lark"`)) || !bytes.Contains(requests[0], []byte(`"definition":"start: \"pestiRoute marker\""`)) {
		t.Fatal("first POST did not contain the CLI custom grammar wire shape")
	}
	if !bytes.Contains(requests[1], []byte(`{"type":"reasoning","id":"r","encrypted_content":"cipher<>&é","extra":1}`)) || !bytes.Contains(requests[1], []byte(`{ "type":"custom_tool_call", "call_id":"c-1", "name":"synthetic_echo", "input":"pestiRoute marker", "extra":"<>&é" }`)) || !bytes.Contains(requests[1], []byte(`"type":"custom_tool_call_output","call_id":"c-1","output":"synthetic-echo-ok"`)) {
		t.Fatal("follow-up did not replay opaque custom history and linked output")
	}
	if strings.Contains(fmt.Sprintf("%+v", a), "cipher") || strings.Contains(fmt.Sprintf("%+v", a), "pestiRoute") || strings.Contains(fmt.Sprintf("%+v", a), "c-1") {
		t.Fatal("sanitized artifact retained private probe data")
	}
	directory := t.TempDir()
	if os.Chmod(directory, 0700) != nil || writeCustomToolArtifact(a, directory) != nil {
		t.Fatal("sanitized private custom artifact did not write")
	}
	path := filepath.Join(directory, customToolArtifactName)
	data, err := os.ReadFile(path)
	info, statErr := os.Stat(path)
	if err != nil || statErr != nil || info.Mode().Perm() != 0600 || bytes.Contains(data, []byte("cipher")) || bytes.Contains(data, []byte("pestiRoute")) || bytes.Contains(data, []byte("c-1")) || !json.Valid(data) {
		t.Fatal("custom artifact leaked private data or permissions")
	}
	if writeCustomToolArtifact(a, directory) == nil {
		t.Fatal("custom artifact overwrote existing file")
	}
}

func TestCustomToolRequestFixtureSchemaAndBounds(t *testing.T) {
	var request struct {
		Input []struct {
			Type  string            `json:"type"`
			Tools []json.RawMessage `json:"tools"`
		} `json:"input"`
	}
	if json.Unmarshal([]byte(customToolRequestFixture), &request) != nil || len(request.Input) != 2 || len(request.Input[0].Tools) != 1 {
		t.Fatal("captured custom POST fixture malformed")
	}
	var namespace struct {
		Type  string            `json:"type"`
		Name  string            `json:"name"`
		Tools []json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(request.Input[0].Tools[0], &namespace) != nil || namespace.Type != "namespace" || namespace.Name != "functions" || len(namespace.Tools) != 1 {
		t.Fatal("custom tool namespace differs from Codex CLI source")
	}
	var custom struct {
		Type   string `json:"type"`
		Name   string `json:"name"`
		Format struct {
			Type       string `json:"type"`
			Syntax     string `json:"syntax"`
			Definition string `json:"definition"`
		} `json:"format"`
		Parameters json.RawMessage `json:"parameters"`
	}
	if json.Unmarshal(namespace.Tools[0], &custom) != nil || custom.Type != "custom" || custom.Name != "synthetic_echo" || custom.Format.Type != "grammar" || custom.Format.Syntax != "lark" || custom.Format.Definition == "" || len(custom.Parameters) != 0 {
		t.Fatal("custom format schema has function-only parameters or missing grammar")
	}
	for _, value := range []string{"", strings.Repeat("x", toolDiagnosticArgLimit+1), "pestiRoute marker "} {
		if validCustomProbeInput(value) {
			t.Fatal("invalid/oversized custom input accepted")
		}
	}
	if _, err := buildCustomProbeFollowup([]byte(customToolRequestFixture), nil, "call", "synthetic-echo-ok"); err == nil {
		t.Fatal("missing opaque history accepted")
	}
}

func TestCustomToolProbeFragmentedAndPoisonedInputs(t *testing.T) {
	stream := "event: response.custom_tool_call_input.delta\ndata: {\"type\":\"response.custom_tool_call_input.delta\",\"delta\":\"pestiRoute marker\"}\n\nevent: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"custom_tool_call\",\"call_id\":\"call-safe\",\"name\":\"synthetic_echo\",\"input\":\"pestiRoute marker\",\"unknown\":{\"x\":\"<>&é\"}}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	calls, terminal, _, history, err := observeCustomProbeSSE(customFragmentReader{strings.NewReader(stream)})
	if err != nil || calls != 1 || terminal != "completed" || history.callID != "call-safe" || !bytes.Contains(history.items[0], []byte(`"unknown":{"x":"<>&é"}`)) {
		t.Fatal("fragmented custom tool input or opaque extension was not preserved")
	}
	history.clear()
	for _, bad := range []string{
		"event: response.custom_tool_call_input.delta\ndata: {\"delta\":\"" + strings.Repeat("x", toolDiagnosticArgLimit+1) + "\"}\n\n",
		"event: response.output_item.done\ndata: {\"item\":{\"type\":\"custom_tool_call\",\"call_id\":\"bad\\ncall\",\"name\":\"synthetic_echo\",\"input\":\"pestiRoute marker\"}}\n\n",
	} {
		_, _, _, h, err := observeCustomProbeSSE(strings.NewReader(bad))
		h.clear()
		if err == nil {
			t.Fatal("malformed, oversized or unsafe custom input was accepted")
		}
	}
}

func TestCustomToolProbeStopsAfterUpstreamRejection(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, `{"error":{"code":"unsupported_value","param":"tools"}}`, http.StatusBadRequest)
	}))
	defer server.Close()
	a, err := runCustomToolProbe(context.Background(), server.Client(), server.URL, []byte(customToolRequestFixture), nil, "")
	if err == nil || requests != 1 || a.DirectRequests != 1 || a.FirstHTTPStatus != http.StatusBadRequest || a.SecondHTTPStatus != 0 || a.FirstRejectionCode != "unsupported_value" || a.FirstRejectionField != "tools" {
		t.Fatal("upstream rejection caused a blind follow-up request")
	}
}
