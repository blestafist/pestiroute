package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
	"github.com/google/uuid"
)

const (
	toolDiagnosticBodyLimit = 1 << 20
	toolDiagnosticArgLimit  = 256
	toolDiagnosticDirectory = "/tmp/opencode/pestiroute-tool-evidence"
)

type toolCallSummary struct {
	Name           string `json:"name"`
	ArgumentsValid bool   `json:"arguments_valid"`
}

type toolDiagnosticArtifact struct {
	SchemaVersion     int                  `json:"schema_version"`
	Scene             string               `json:"scene"`
	Profile           string               `json:"profile"`
	Model             string               `json:"model"`
	ParallelRequested bool                 `json:"parallel_requested"`
	Status            *int                 `json:"status"`
	Classification    string               `json:"classification"`
	RejectionCode     string               `json:"rejection_code"`
	RejectionField    string               `json:"rejection_field"`
	EventOrder        []diagnosticEventRun `json:"event_order"`
	ToolCalls         []toolCallSummary    `json:"tool_calls"`
	ToolCallCount     int                  `json:"tool_call_count"`
	TerminalStatus    string               `json:"terminal_status"`
	StreamStatus      string               `json:"stream_status"`
	UsageObserved     bool                 `json:"usage_observed"`
	DirectRequests    int                  `json:"direct_requests"`
}

func validToolDiagnostic(a toolDiagnosticArtifact) bool {
	if a.SchemaVersion != 1 || a.Profile != "codex-responses-http-sse-lite-v1" || a.Model != "gpt-6-luna" ||
		!oneOf(a.Scene, "single-tool", "parallel-tools") || a.Status == nil || *a.Status < 100 || *a.Status > 599 ||
		!oneOf(a.Classification, "tool_emission", "no_tool_emission", "http_rejected", "transport_error") ||
		!oneOf(a.RejectionCode, "other", "invalid_request_error", "unsupported_parameter", "unsupported_value", "model_not_found", "insufficient_quota", "rate_limit_exceeded", "server_error") ||
		!oneOf(a.RejectionField, "other", "model", "input", "tools", "tool_choice", "parallel_tool_calls") ||
		!oneOf(a.TerminalStatus, "none", "completed", "failed", "incomplete", "trailing_data") ||
		!oneOf(a.StreamStatus, "not_observed", "complete", "failed", "incomplete", "read_failure", "size_limit", "malformed", "trailing_data") ||
		a.DirectRequests != 1 || len(a.ToolCalls) > 2 || a.ToolCallCount < 0 || a.ToolCallCount > 3 || a.ToolCallCount < len(a.ToolCalls) {
		return false
	}
	wantParallel := a.Scene == "parallel-tools"
	if a.ParallelRequested != wantParallel || (!wantParallel && len(a.ToolCalls) > 1) {
		return false
	}
	if a.ToolCallCount == 0 && a.Classification == "tool_emission" || a.ToolCallCount > 0 && a.Classification != "tool_emission" || len(a.ToolCalls) != min(a.ToolCallCount, 2) {
		return false
	}
	for _, c := range a.ToolCalls {
		if c.Name != "smoke_lookup" {
			return false
		}
	}
	for _, e := range a.EventOrder {
		if e.Count <= 0 || !oneOf(e.Type, "response.created", "response.in_progress", "response.output_item.added", "response.output_item.done", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.completed", "response.failed", "response.incomplete", "error", "unknown") {
			return false
		}
	}
	return true
}

func buildToolDiagnosticBody(threadID, scene string) ([]byte, error) {
	if scene != "single-tool" && scene != "parallel-tools" {
		return nil, errors.New("unsupported tool scene")
	}
	body, err := diagnosticRequestBody(diagnosticProfileLunaLiteHighCompletion, threadID)
	if err != nil {
		return nil, err
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	tools := []any{map[string]any{
		"type": "function", "name": "smoke_lookup", "description": "Return a fixed synthetic marker.",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string", "enum": []string{"alpha", "beta"}}}, "required": []string{"key"}, "additionalProperties": false}, "strict": true,
	}}
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		return nil, err
	}
	input := request["input"].([]any)
	prefix := input[0].(map[string]any)
	prefix["tools"] = tools
	threadUUID, err := uuid.Parse(threadID)
	if err != nil {
		return nil, errors.New("invalid tool diagnostic thread id")
	}
	prefixNamespace := uuid.NewSHA1(uuid.NameSpaceOID, []byte(threadUUID.String()))
	prefix["id"] = "at_" + uuid.NewSHA1(prefixNamespace, toolsJSON).String()
	message := input[1].(map[string]any)
	text := "Call smoke_lookup once with key alpha."
	parallel := false
	if scene == "parallel-tools" {
		text = "Call smoke_lookup with key alpha and also with key beta; make both independent calls."
		parallel = true
	}
	message["content"].([]any)[0].(map[string]any)["text"] = text
	request["parallel_tool_calls"] = parallel
	return json.Marshal(request)
}

func inspectToolDiagnosticSSE(body io.Reader, scene string) toolDiagnosticArtifact {
	a := toolDiagnosticArtifact{SchemaVersion: 1, Scene: scene, Profile: "codex-responses-http-sse-lite-v1", Model: "gpt-6-luna", ParallelRequested: scene == "parallel-tools", Classification: "no_tool_emission", RejectionCode: "other", RejectionField: "other", EventOrder: []diagnosticEventRun{}, ToolCalls: []toolCallSummary{}, TerminalStatus: "none", StreamStatus: "incomplete"}
	limited := &io.LimitedReader{R: body, N: toolDiagnosticBodyLimit + 1}
	r := bufio.NewReaderSize(limited, 4096)
	var eventName string
	var data []byte
	var total int
	for {
		line, err := r.ReadString('\n')
		total += len(line)
		if total > toolDiagnosticBodyLimit {
			a.StreamStatus = "size_limit"
			return a
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" && len(data) > 0 {
			var event struct {
				Type     string                                 `json:"type"`
				Item     struct{ Type, Name, Arguments string } `json:"item"`
				Delta    string                                 `json:"delta"`
				Response struct {
					Status string          `json:"status"`
					Usage  json.RawMessage `json:"usage"`
				} `json:"response"`
			}
			if json.Unmarshal(data, &event) == nil {
				name := eventName
				if name == "" {
					name = event.Type
				}
				category := diagnosticEventCategory(name)
				appendDiagnosticEvent(&a.EventOrder, category)
				if category == "response.output_item.added" && event.Item.Type == "function_call" {
					if a.ToolCallCount < 3 {
						a.ToolCallCount++
					}
					if len(a.ToolCalls) < 2 {
						a.ToolCalls = append(a.ToolCalls, toolCallSummary{Name: safeToolName(event.Item.Name)})
					}
					a.Classification = "tool_emission"
				}
				if category == "response.output_item.done" && event.Item.Type == "function_call" {
					for i := len(a.ToolCalls) - 1; i >= 0; i-- {
						if a.ToolCalls[i].Name == safeToolName(event.Item.Name) && !a.ToolCalls[i].ArgumentsValid {
							a.ToolCalls[i].ArgumentsValid = validSyntheticToolArgs(event.Item.Arguments)
							break
						}
					}
				}
				if len(event.Response.Usage) > 0 {
					a.UsageObserved = true
				}
				if category == "response.completed" && event.Response.Status == "completed" {
					a.TerminalStatus = "completed"
				}
				if category == "response.failed" || category == "error" {
					a.TerminalStatus = "failed"
				}
				if category == "response.incomplete" {
					a.TerminalStatus = "incomplete"
				}
			}
			clear(data)
			data = nil
			eventName = ""
		} else if strings.HasPrefix(line, "event: ") {
			eventName = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data = append(data, strings.TrimPrefix(line, "data: ")...)
			data = append(data, '\n')
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) == 0 {
				if a.TerminalStatus == "completed" {
					a.StreamStatus = "complete"
				} else if a.TerminalStatus == "failed" {
					a.StreamStatus = "failed"
				}
			}
			if len(a.ToolCalls) > 0 {
				a.Classification = "tool_emission"
			}
			return a
		}
	}
}

func safeToolName(value string) string {
	if value == "smoke_lookup" {
		return value
	}
	return "unknown"
}

func validSyntheticToolArgs(raw string) bool {
	if len(raw) == 0 || len(raw) > toolDiagnosticArgLimit {
		return false
	}
	object, err := scanJSONObject([]byte(raw), "key")
	if err != nil || len(object) != 1 {
		return false
	}
	var key string
	if json.Unmarshal(object["key"], &key) != nil || len(key) > 5 {
		return false
	}
	return key == "alpha" || key == "beta"
}

func writeToolDiagnosticArtifact(scene string, artifact toolDiagnosticArtifact) error {
	if err := ensureDiagnosticResultDirectory(toolDiagnosticDirectory); err != nil {
		return err
	}
	return writeToolDiagnosticArtifactInDirectory(scene, artifact, toolDiagnosticDirectory)
}

func writeToolDiagnosticArtifactInDirectory(scene string, artifact toolDiagnosticArtifact, directory string) error {
	if !validToolDiagnostic(artifact) || (scene != "single-tool" && scene != "parallel-tools") || artifact.Scene != scene || verifyDiagnosticDirectory(directory, true) != nil {
		return errors.New("invalid tool diagnostic artifact")
	}
	path := filepath.Join(directory, scene+".json")
	data, err := json.Marshal(artifact)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
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
	}
	return err
}

func TestOperatorToolDiagnostic(t *testing.T) {
	if os.Getenv("PESTIROUTE_DIAG_TOOL_PROBE") != "authorized" {
		t.Skip("operator tool probe not enabled")
	}
	if err := ensureDiagnosticResultDirectory(toolDiagnosticDirectory); err != nil {
		t.Fatal("tool diagnostic artifact destination unavailable")
	}
	for _, scene := range []string{"single-tool", "parallel-tools"} {
		if _, err := os.Lstat(filepath.Join(toolDiagnosticDirectory, scene+".json")); err == nil || !errors.Is(err, os.ErrNotExist) {
			t.Fatal("tool diagnostic artifact already exists or is unavailable")
		}
	}
	dbPath, keyPath, account := os.Getenv("PESTIROUTE_DIAG_DB"), os.Getenv("PESTIROUTE_DIAG_KEY"), os.Getenv("PESTIROUTE_DIAG_ACCOUNT")
	if dbPath == "" || keyPath == "" || account == "" {
		t.Fatal("tool diagnostic credential checkpoint failed")
	}
	key, err := crypto.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal("tool diagnostic credential checkpoint failed")
	}
	db, err := openDiagnosticDB(dbPath)
	if err != nil {
		t.Fatal("tool diagnostic credential checkpoint failed")
	}
	defer db.Close()
	repo := sqlite.NewCredentials(db)
	row, err := repo.Get(context.Background(), account, "oauth")
	if err != nil || row.ExpiresAt == nil || !row.ExpiresAt.After(time.Now()) {
		t.Fatal("tool diagnostic credential checkpoint failed")
	}
	plain, err := repo.GetDecrypted(context.Background(), account, "oauth", key)
	if err != nil {
		t.Fatal("tool diagnostic credential checkpoint failed")
	}
	defer clear(plain)
	bundle, err := decodeOAuthBundle(plain)
	if err != nil || !bundle.ExpiresAt.After(time.Now()) {
		t.Fatal("tool diagnostic credential checkpoint failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, scene := range []string{"single-tool", "parallel-tools"} {
		if err := runToolDiagnostic(ctx, scene, bundle); err != nil {
			t.Fatal("tool diagnostic stopped; inspect the private sanitized artifact and test result")
		}
	}
}

func runToolDiagnostic(ctx context.Context, scene string, bundle oauthBundle) error {
	id, err := uuid.NewRandom()
	if err != nil {
		return err
	}
	body, err := buildToolDiagnosticBody(id.String(), scene)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexResponsesEndpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bundle.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", bundle.AccountID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")
	applyDiagnosticProfileHeaders(req, diagnosticProfileLunaLiteHighCompletion, id.String())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	configureCodexTransport(transport)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	status := resp.StatusCode
	a := toolDiagnosticArtifact{SchemaVersion: 1, Scene: scene, Profile: "codex-responses-http-sse-lite-v1", Model: "gpt-6-luna", ParallelRequested: scene == "parallel-tools", Status: &status, Classification: "http_rejected", RejectionCode: "other", RejectionField: "other", EventOrder: []diagnosticEventRun{}, ToolCalls: []toolCallSummary{}, TerminalStatus: "none", StreamStatus: "not_observed", DirectRequests: 1}
	if status >= 200 && status < 300 {
		a = inspectToolDiagnosticSSE(io.LimitReader(resp.Body, toolDiagnosticBodyLimit+1), scene)
		a.Status = &status
		a.DirectRequests = 1
	} else {
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, toolDiagnosticBodyLimit+1))
		if readErr != nil || len(b) > toolDiagnosticBodyLimit {
			clear(b)
			return errors.New("bounded response read failed")
		}
		_, code, field := classifyDiagnosticError(b)
		clear(b)
		a.Classification, a.RejectionCode, a.RejectionField = "http_rejected", code, field
	}
	if err := writeToolDiagnosticArtifact(scene, a); err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return errors.New("provider rejected tool probe")
	}
	if scene == "single-tool" && len(a.ToolCalls) != 1 {
		return errors.New("single tool emission not observed")
	}
	if scene == "parallel-tools" && len(a.ToolCalls) != 2 {
		return errors.New("two calls in one response not observed")
	}
	return nil
}

func TestDiagnosticToolArtifactPrivacy(t *testing.T) {
	status := http.StatusOK
	a := toolDiagnosticArtifact{SchemaVersion: 1, Scene: "single-tool", Profile: "codex-responses-http-sse-lite-v1", Model: "gpt-6-luna", Status: &status, Classification: "tool_emission", RejectionCode: "other", RejectionField: "other", EventOrder: []diagnosticEventRun{{Type: "response.output_item.done", Count: 1}}, ToolCalls: []toolCallSummary{{Name: "smoke_lookup", ArgumentsValid: true}}, ToolCallCount: 1, TerminalStatus: "completed", StreamStatus: "complete", DirectRequests: 1}
	if !validToolDiagnostic(a) {
		t.Fatal("safe synthetic artifact rejected")
	}
	for _, raw := range []string{`{"key":"alpha"}`, `{"key":"beta"}`} {
		if !validSyntheticToolArgs(raw) {
			t.Fatal("valid synthetic arguments rejected")
		}
	}
	for _, raw := range []string{`{"key":"PRIVATE"}`, `{"key":"alpha","extra":"x"}`, `{"key":"alpha","key":"beta"}`, `not json`, strings.Repeat("x", toolDiagnosticArgLimit+1)} {
		if validSyntheticToolArgs(raw) {
			t.Fatal("invalid or overlong arguments accepted")
		}
	}
	for _, mutate := range []func(*toolDiagnosticArtifact){func(v *toolDiagnosticArtifact) { v.ToolCalls[0].Name = "PRIVATE" }, func(v *toolDiagnosticArtifact) { v.EventOrder[0].Type = "PRIVATE_EVENT" }, func(v *toolDiagnosticArtifact) { v.DirectRequests = 2 }, func(v *toolDiagnosticArtifact) { v.ParallelRequested = true }, func(v *toolDiagnosticArtifact) { v.RejectionField = "PRIVATE" }} {
		bad := a
		bad.EventOrder = append([]diagnosticEventRun(nil), a.EventOrder...)
		bad.ToolCalls = append([]toolCallSummary(nil), a.ToolCalls...)
		mutate(&bad)
		if validToolDiagnostic(bad) {
			t.Fatal("poisoned diagnostic artifact accepted")
		}
	}
}

func TestDiagnosticToolProbeShapeAndSSEPrivacy(t *testing.T) {
	thread := "00000000-0000-4000-8000-000000000001"
	for _, scene := range []string{"single-tool", "parallel-tools"} {
		body, err := buildToolDiagnosticBody(thread, scene)
		if err != nil {
			t.Fatal("tool probe request did not build")
		}
		var request map[string]any
		if json.Unmarshal(body, &request) != nil {
			t.Fatal("tool probe request was invalid JSON")
		}
		if request["model"] != "gpt-6-luna" || request["parallel_tool_calls"] != (scene == "parallel-tools") || request["tool_choice"] != "auto" || request["tools"] != nil {
			t.Fatal("tool probe changed the reviewed Lite request envelope")
		}
		input := request["input"].([]any)
		prefix := input[0].(map[string]any)
		tools := prefix["tools"].([]any)
		tool := tools[0].(map[string]any)
		prefixID, ok := prefix["id"].(string)
		if prefix["type"] != "additional_tools" || !ok || !strings.HasPrefix(prefixID, "at_") {
			t.Fatal("additional-tools prefix differed")
		}
		toolsJSON, err := json.Marshal(tools)
		if err != nil || prefixID != "at_"+uuid.NewSHA1(uuid.NewSHA1(uuid.NameSpaceOID, []byte(thread)), toolsJSON).String() {
			t.Fatal("additional-tools id was not derived from the emitted tool list")
		}
		if tool["type"] != "function" || tool["name"] != "smoke_lookup" || tool["strict"] != true || tool["description"] != "Return a fixed synthetic marker." {
			t.Fatal("synthetic function definition differed")
		}
	}
	fixture := strings.Join([]string{
		`event: response.output_item.added`, `data: {"type":"response.output_item.added","item":{"type":"function_call","name":"smoke_lookup"}}`, ``,
		`event: response.output_item.done`, `data: {"type":"response.output_item.done","item":{"type":"function_call","name":"smoke_lookup","arguments":"{\"key\":\"alpha\"}"}}`, ``,
		`event: response.completed`, `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3}}}`, ``, ``,
	}, "\n")
	got := inspectToolDiagnosticSSE(strings.NewReader(fixture), "single-tool")
	status := http.StatusOK
	got.Status = &status
	got.DirectRequests = 1
	encoded, err := json.Marshal(got)
	if err != nil || !validToolDiagnostic(got) || len(got.ToolCalls) != 1 || got.ToolCallCount != 1 || !got.ToolCalls[0].ArgumentsValid || !got.UsageObserved || !strings.Contains(string(encoded), `"terminal_status":"completed"`) || strings.Contains(string(encoded), `"key"`) {
		t.Fatalf("tool SSE observation was incomplete, invalid, or retained raw arguments: %+v", got)
	}
}

func TestDiagnosticToolArtifactWriterPrivacy(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal("private artifact test directory setup failed")
	}
	status := http.StatusOK
	a := toolDiagnosticArtifact{SchemaVersion: 1, Scene: "single-tool", Profile: "codex-responses-http-sse-lite-v1", Model: "gpt-6-luna", Status: &status, Classification: "tool_emission", RejectionCode: "other", RejectionField: "other", EventOrder: []diagnosticEventRun{{Type: "response.output_item.added", Count: 1}}, ToolCalls: []toolCallSummary{{Name: "smoke_lookup", ArgumentsValid: false}}, ToolCallCount: 1, TerminalStatus: "completed", StreamStatus: "complete", DirectRequests: 1}
	if err := writeToolDiagnosticArtifactInDirectory("single-tool", a, directory); err != nil {
		t.Fatal("sanitized fixture artifact did not write")
	}
	path := filepath.Join(directory, "single-tool.json")
	data, err := os.ReadFile(path)
	info, statErr := os.Stat(path)
	var saved toolDiagnosticArtifact
	decodeErr := json.Unmarshal(data, &saved)
	if err != nil || statErr != nil || info.Mode().Perm() != 0600 || strings.Contains(string(data), "alpha") || strings.Contains(string(data), "PRIVATE") || !json.Valid(data) || decodeErr != nil || !validToolDiagnostic(saved) {
		t.Fatal("artifact was not private and payload-free")
	}
	if err := writeToolDiagnosticArtifactInDirectory("single-tool", a, directory); err == nil {
		t.Fatal("existing artifact was overwritten")
	}
	poison := a
	poison.Scene = "parallel-tools"
	if err := writeToolDiagnosticArtifactInDirectory("parallel-tools", poison, directory); err == nil {
		t.Fatal("inconsistent scene artifact was written")
	}
}
