package codex

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
	"github.com/blestafist/pestiroute/internal/testutil/codexfixtures"
	"github.com/google/uuid"
)

const roundtripArtifactName = "single-tool-roundtrip.json"

type roundtripArtifact struct {
	SchemaVersion        int                  `json:"schema_version"`
	Profile              string               `json:"profile"`
	Model                string               `json:"model"`
	DirectRequests       int                  `json:"direct_requests"`
	FirstStatus          *int                 `json:"first_status"`
	SecondStatus         *int                 `json:"second_status"`
	FirstClassification  string               `json:"first_classification"`
	SecondClassification string               `json:"second_classification"`
	FirstRejectionCode   string               `json:"first_rejection_code"`
	FirstRejectionField  string               `json:"first_rejection_field"`
	SecondRejectionCode  string               `json:"second_rejection_code"`
	SecondRejectionField string               `json:"second_rejection_field"`
	FirstEvents          []diagnosticEventRun `json:"first_events"`
	SecondEvents         []diagnosticEventRun `json:"second_events"`
	FirstToolCallCount   int                  `json:"first_tool_call_count"`
	FirstToolNameValid   bool                 `json:"first_tool_name_valid"`
	ArgumentsValid       bool                 `json:"arguments_valid"`
	LocalLookupExecuted  bool                 `json:"local_lookup_executed"`
	CallResultLinked     bool                 `json:"call_result_linked"`
	FinalAnswerVerified  bool                 `json:"final_answer_verified"`
	FirstTerminal        string               `json:"first_terminal"`
	SecondTerminal       string               `json:"second_terminal"`
	FirstStreamStatus    string               `json:"first_stream_status"`
	SecondStreamStatus   string               `json:"second_stream_status"`
	FirstUsageObserved   bool                 `json:"first_usage_observed"`
	SecondUsageObserved  bool                 `json:"second_usage_observed"`
	Transport            string               `json:"transport"`
}

func validRoundtripArtifact(a roundtripArtifact) bool {
	if a.SchemaVersion != 1 || a.Profile != "codex-responses-http-sse-lite-v1" || a.Model != "gpt-6-luna" ||
		a.DirectRequests < 0 || a.DirectRequests > 2 || !safeRoundtripStatus(a.FirstStatus) || !safeRoundtripStatus(a.SecondStatus) ||
		!oneOf(a.FirstClassification, "not_run", "tool_emission", "no_tool_emission", "http_rejected", "transport_error") ||
		!oneOf(a.SecondClassification, "not_run", "roundtrip_completed", "http_rejected", "transport_error", "invalid_response") ||
		!oneOf(a.FirstRejectionCode, "other", "invalid_request_error", "unsupported_parameter", "unsupported_value", "model_not_found", "insufficient_quota", "rate_limit_exceeded", "server_error") ||
		!oneOf(a.SecondRejectionCode, "other", "invalid_request_error", "unsupported_parameter", "unsupported_value", "model_not_found", "insufficient_quota", "rate_limit_exceeded", "server_error") ||
		!oneOf(a.FirstRejectionField, "other", "model", "input", "tools", "tool_choice", "parallel_tool_calls") ||
		!oneOf(a.SecondRejectionField, "other", "model", "input", "tools", "tool_choice", "parallel_tool_calls") ||
		!oneOf(a.FirstTerminal, "none", "completed", "failed", "incomplete", "trailing_data") ||
		!oneOf(a.SecondTerminal, "none", "completed", "failed", "incomplete", "trailing_data") ||
		!oneOf(a.FirstStreamStatus, "not_run", "complete", "failed", "incomplete", "read_failure", "size_limit", "output_limit", "malformed", "trailing_data") ||
		!oneOf(a.SecondStreamStatus, "not_run", "complete", "failed", "incomplete", "read_failure", "size_limit", "output_limit", "malformed", "trailing_data") ||
		!oneOf(a.Transport, "none", "deadline", "canceled", "dns_failure", "connect_failure", "tls_verification", "tls_protocol_error", "unexpected_eof", "connection_closed", "permission_denied", "connection_reset", "malformed_http_response", "invalid_header", "content_length", "proxy_error", "unknown") ||
		a.FirstToolCallCount < 0 || a.FirstToolCallCount > 3 || !validRoundtripEvents(a.FirstEvents) || !validRoundtripEvents(a.SecondEvents) {
		return false
	}
	if (a.FirstStatus != nil && *a.FirstStatus >= 200 && *a.FirstStatus < 300 && !oneOf(a.FirstClassification, "tool_emission", "no_tool_emission")) ||
		(a.SecondStatus != nil && *a.SecondStatus >= 200 && *a.SecondStatus < 300 && a.SecondClassification != "roundtrip_completed" && a.SecondClassification != "invalid_response") {
		return false
	}
	if a.LocalLookupExecuted && (!a.ArgumentsValid || !a.FirstToolNameValid || a.FirstToolCallCount != 1 || a.DirectRequests < 1) {
		return false
	}
	if a.CallResultLinked && (!a.LocalLookupExecuted || a.DirectRequests < 2) || a.FinalAnswerVerified && !a.CallResultLinked {
		return false
	}
	return true
}

func safeRoundtripStatus(status *int) bool { return status == nil || *status >= 100 && *status <= 599 }

func validRoundtripEvents(events []diagnosticEventRun) bool {
	for _, e := range events {
		if e.Count <= 0 || !oneOf(e.Type, "response.created", "response.in_progress", "response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done", "response.output_text.delta", "response.output_text.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.completed", "response.failed", "response.incomplete", "error", "unknown") {
			return false
		}
	}
	return true
}

type roundtripStream struct {
	Events          []diagnosticEventRun
	Terminal        string
	Status          string
	Usage           bool
	Calls           int
	NameValid       bool
	ArgsValid       bool
	Arguments       []byte   `json:"-"`
	CallID          string   `json:"-"`
	HistoryItems    [][]byte `json:"-"`
	Answer          []byte   `json:"-"`
	HistoryBytes    int
	answerDeltaSeen bool
	answerObserved  bool
}

func newRoundtripStream() roundtripStream {
	return roundtripStream{Events: []diagnosticEventRun{}, Terminal: "none", Status: "incomplete", HistoryItems: [][]byte{}, Answer: []byte{}}
}

func (s *roundtripStream) clear() {
	for _, item := range s.HistoryItems {
		clear(item)
	}
	clear(s.Answer)
	clear(s.Arguments)
	s.HistoryItems = nil
	s.Answer = nil
	s.CallID = ""
}

func observeRoundtripSSE(body io.Reader, captureHistory bool, cancel context.CancelFunc) roundtripStream {
	s := newRoundtripStream()
	r := bufio.NewReaderSize(body, 4096)
	var total int
	var eventName string
	var data []byte
	defer func() { clear(data) }()
	terminalSeen := false
	dataLines := false
	for {
		line, readErr := r.ReadString('\n')
		total += len(line)
		if total > toolDiagnosticBodyLimit {
			s.Status = "size_limit"
			cancel()
			return s
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" && dataLines {
			var event struct {
				Type  string          `json:"type"`
				Item  json.RawMessage `json:"item"`
				Delta string          `json:"delta"`
				Text  string          `json:"text"`
				Part  struct {
					Text string `json:"text"`
				} `json:"part"`
				Response struct {
					Status string          `json:"status"`
					Usage  json.RawMessage `json:"usage"`
				} `json:"response"`
			}
			if json.Unmarshal(data, &event) == nil {
				categoryName := eventName
				if categoryName == "" {
					categoryName = event.Type
				}
				category := diagnosticEventCategory(categoryName)
				appendDiagnosticEvent(&s.Events, category)
				if terminalSeen {
					s.Terminal, s.Status = "trailing_data", "trailing_data"
				} else {
					if len(event.Response.Usage) > 0 && string(event.Response.Usage) != "null" {
						s.Usage = true
					}
					if category == "response.output_item.done" && len(event.Item) > 0 && captureHistory {
						if !s.inspectHistoryItem(event.Item) {
							cancel()
							clear(data)
							return s
						}
					}
					if category == "response.output_text.delta" || category == "response.output_text.done" || category == "response.content_part.done" {
						fragment := ""
						if category == "response.output_text.delta" {
							fragment = event.Delta
						} else if !s.answerObserved && !s.answerDeltaSeen {
							if category == "response.output_text.done" {
								fragment = event.Text
							} else {
								fragment = event.Part.Text
							}
						}
						if len(s.Answer)+len(fragment) > 256 {
							s.Status = "output_limit"
							cancel()
							clear(data)
							return s
						}
						s.Answer = append(s.Answer, fragment...)
						if category == "response.output_text.delta" {
							s.answerDeltaSeen = true
						}
						if category != "response.output_text.delta" || fragment != "" {
							s.answerObserved = true
						}
					}
					if terminal, seen := roundtripTerminal(category, event.Type, event.Response.Status); seen {
						terminalSeen = true
						s.Terminal = terminal
					}
				}
			}
			clear(data)
			data, eventName, dataLines = nil, "", false
		} else if event, ok := strings.CutPrefix(line, "event: "); ok {
			eventName = event
		} else if strings.HasPrefix(line, "data: ") {
			dataLines = true
			data = append(data, strings.TrimPrefix(line, "data: ")...)
			data = append(data, '\n')
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) && len(line) == 0 && !dataLines && eventName == "" {
				if s.Status == "trailing_data" {
					return s
				}
				s.Status = "incomplete"
				if terminalSeen && s.Terminal == "completed" {
					s.Status = "complete"
				} else if terminalSeen && s.Terminal == "failed" {
					s.Status = "failed"
				}
			} else if errors.Is(readErr, io.EOF) {
				s.Status = "malformed"
			} else {
				s.Status = "read_failure"
			}
			return s
		}
	}
}

func (s *roundtripStream) inspectHistoryItem(raw json.RawMessage) bool {
	var item struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		CallID    string `json:"call_id"`
		Arguments string `json:"arguments"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return true
	}
	switch item.Type {
	case "reasoning", "message":
		if len(raw) > toolDiagnosticBodyLimit-s.HistoryBytes {
			s.Status = "size_limit"
			return false
		}
		s.HistoryItems = append(s.HistoryItems, bytes.Clone(raw))
		s.HistoryBytes += len(raw)
	case "function_call":
		if s.Calls < 3 {
			s.Calls++
		}
		s.NameValid = item.Name == "smoke_lookup"
		s.ArgsValid = validExpectedSyntheticArgs(item.Arguments)
		if s.Calls == 1 && s.ArgsValid {
			s.Arguments = append([]byte(nil), item.Arguments...)
		}
		if s.Calls == 1 && s.NameValid && s.ArgsValid && len(item.CallID) > 0 && len(item.CallID) <= 128 && safeHeaderValue(item.CallID) {
			s.CallID = item.CallID
		}
		if s.Calls == 1 && s.NameValid && s.ArgsValid && s.CallID != "" {
			if len(raw) > toolDiagnosticBodyLimit-s.HistoryBytes {
				s.Status = "size_limit"
				return false
			}
			s.HistoryItems = append(s.HistoryItems, bytes.Clone(raw))
			s.HistoryBytes += len(raw)
		}
	}
	return true
}

func validExpectedSyntheticArgs(raw string) bool {
	if len(raw) == 0 || len(raw) > toolDiagnosticArgLimit {
		return false
	}
	fields, err := scanJSONObject([]byte(raw), "key")
	if err != nil || len(fields) != 1 {
		return false
	}
	var key string
	return json.Unmarshal(fields["key"], &key) == nil && key == "alpha"
}

func roundtripTerminal(category, eventType, responseStatus string) (string, bool) {
	switch category {
	case "response.completed":
		if eventType == "response.completed" && responseStatus == "completed" {
			return "completed", true
		}
		return "incomplete", true
	case "response.failed", "error":
		return "failed", true
	case "response.incomplete":
		return "incomplete", true
	default:
		return "", false
	}
}

func buildRoundtripFollowup(initialBody []byte, items [][]byte, callID, output string) ([]byte, error) {
	if len(initialBody) > toolDiagnosticBodyLimit || callID == "" || len(callID) > 128 || !safeHeaderValue(callID) || output != "synthetic-alpha" {
		return nil, errors.New("invalid roundtrip history")
	}
	inputStart, inputEnd, err := roundtripInputBounds(initialBody)
	if err != nil {
		return nil, errors.New("invalid initial request")
	}
	initialInput := initialBody[inputStart:inputEnd]
	if !validInitialRoundtripInput(initialInput) {
		return nil, errors.New("invalid initial input")
	}
	for _, item := range items {
		if !json.Valid(item) {
			return nil, errors.New("invalid opaque history item")
		}
		var kind struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(item, &kind) != nil || !oneOf(kind.Type, "reasoning", "message", "function_call") {
			return nil, errors.New("unsupported opaque history item")
		}
	}
	callOutput, _ := json.Marshal(map[string]string{"type": "function_call_output", "call_id": callID, "output": output})
	close := len(initialInput) - 1
	for close >= 0 && (initialInput[close] == ' ' || initialInput[close] == '\n' || initialInput[close] == '\r' || initialInput[close] == '\t') {
		close--
	}
	if close < 0 || initialInput[close] != ']' {
		return nil, errors.New("invalid initial input")
	}
	added := 1 + len(callOutput)
	for _, item := range items {
		added += len(item) + 1
	}
	newSize := len(initialBody) + added
	if newSize > toolDiagnosticBodyLimit {
		return nil, errors.New("roundtrip request exceeds bound")
	}
	body := make([]byte, 0, newSize)
	body = append(body, initialBody[:inputStart]...)
	body = append(body, initialInput[:close]...)
	body = append(body, ',')
	for _, item := range items {
		body = append(body, item...)
		body = append(body, ',')
	}
	body = append(body, callOutput...)
	body = append(body, initialInput[close:]...)
	body = append(body, initialBody[inputEnd:]...)
	return body, nil
}

func roundtripInputBounds(body []byte) (start, end int, err error) {
	if !json.Valid(body) {
		return 0, 0, errors.New("invalid JSON request")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return 0, 0, errors.New("invalid JSON request")
	}
	seen := make(map[string]struct{})
	found := false
	for d.More() {
		tok, err = d.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return 0, 0, errors.New("invalid JSON request")
		}
		if _, duplicate := seen[key]; duplicate {
			return 0, 0, errors.New("invalid JSON request")
		}
		seen[key] = struct{}{}
		if strings.EqualFold(key, "input") && key != "input" {
			return 0, 0, errors.New("invalid JSON request")
		}
		valueStart := int(d.InputOffset())
		for valueStart < len(body) && isJSONWhitespace(body[valueStart]) {
			valueStart++
		}
		if valueStart >= len(body) || body[valueStart] != ':' {
			return 0, 0, errors.New("invalid JSON request")
		}
		valueStart++
		for valueStart < len(body) && isJSONWhitespace(body[valueStart]) {
			valueStart++
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return 0, 0, errors.New("invalid JSON request")
		}
		valueEnd := int(d.InputOffset())
		if key == "input" {
			start, end, found = valueStart, valueEnd, true
		}
		clear(value)
	}
	if _, err := d.Token(); err != nil {
		return 0, 0, errors.New("invalid JSON request")
	}
	if err := d.Decode(new(any)); err != io.EOF || !found {
		return 0, 0, errors.New("invalid JSON request")
	}
	return start, end, nil
}

func validInitialRoundtripInput(input []byte) bool {
	d := json.NewDecoder(bytes.NewReader(input))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('[') {
		return false
	}
	count := 0
	for d.More() {
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return false
		}
		clear(value)
		count++
	}
	if _, err := d.Token(); err != nil || count != 2 {
		return false
	}
	return d.Decode(new(any)) == io.EOF
}

func isJSONWhitespace(b byte) bool { return b == ' ' || b == '\n' || b == '\r' || b == '\t' }

func executeSyntheticSmokeLookup(name string, arguments string) (string, bool) {
	if name != "smoke_lookup" || !validExpectedSyntheticArgs(arguments) {
		return "", false
	}
	var argumentsObject struct {
		Key string `json:"key"`
	}
	if json.Unmarshal([]byte(arguments), &argumentsObject) != nil {
		return "", false
	}
	result, ok := map[string]string{"alpha": "synthetic-alpha"}[argumentsObject.Key]
	return result, ok
}

func classifyRoundtripHTTPError(body []byte) (code, field string) {
	_, code, field = classifyDiagnosticError(body)
	return code, field
}

func buildRoundtripInitialBody(threadID string) ([]byte, error) {
	body, err := buildToolDiagnosticBody(threadID, "single-tool")
	if err != nil {
		return nil, err
	}
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		return nil, errors.New("invalid initial tool request")
	}
	input := request["input"].([]any)
	prefix := input[0].(map[string]any)
	tools := prefix["tools"].([]any)
	tool := tools[0].(map[string]any)
	parameters := tool["parameters"].(map[string]any)
	properties := parameters["properties"].(map[string]any)
	key := properties["key"].(map[string]any)
	key["enum"] = []any{"alpha"}
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		return nil, err
	}
	threadUUID, err := uuid.Parse(threadID)
	if err != nil {
		return nil, errors.New("invalid roundtrip thread id")
	}
	prefixNamespace := uuid.NewSHA1(uuid.NameSpaceOID, []byte(threadUUID.String()))
	prefix["id"] = "at_" + uuid.NewSHA1(prefixNamespace, toolsJSON).String()
	message := input[1].(map[string]any)
	message["content"].([]any)[0].(map[string]any)["text"] = "Call smoke_lookup with key alpha. After receiving its result, reply with only the marker value."
	return json.Marshal(request)
}

func makeRoundtripRequest(ctx context.Context, body []byte, bundle oauthBundle, threadID string) (*http.Request, error) {
	return makeRoundtripRequestTo(ctx, codexResponsesEndpoint, body, bundle, threadID)
}

func makeRoundtripRequestTo(ctx context.Context, endpoint string, body []byte, bundle oauthBundle, threadID string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bundle.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", bundle.AccountID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")
	applyDiagnosticProfileHeaders(req, diagnosticProfileLunaLiteHighCompletion, threadID)
	return req, nil
}

func saveRoundtripArtifact(artifact roundtripArtifact) error {
	if err := ensureDiagnosticResultDirectory(toolDiagnosticDirectory); err != nil {
		return err
	}
	return writeRoundtripArtifactInDirectory(artifact, toolDiagnosticDirectory)
}

func writeRoundtripArtifactInDirectory(artifact roundtripArtifact, directory string) error {
	if !validRoundtripArtifact(artifact) || verifyDiagnosticDirectory(directory, true) != nil {
		return errors.New("invalid roundtrip artifact")
	}
	path := filepath.Join(directory, roundtripArtifactName)
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

func TestOperatorToolRoundtripDiagnostic(t *testing.T) {
	if os.Getenv("PESTIROUTE_DIAG_TOOL_ROUNDTRIP") != "authorized" {
		t.Skip("operator roundtrip diagnostic not enabled")
	}
	if err := ensureDiagnosticResultDirectory(toolDiagnosticDirectory); err != nil {
		t.Fatal("roundtrip artifact destination unavailable")
	}
	path := filepath.Join(toolDiagnosticDirectory, roundtripArtifactName)
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("roundtrip artifact already exists or is unavailable")
	}
	db, err := openDiagnosticDB(os.Getenv("PESTIROUTE_DIAG_DB"))
	if err != nil {
		t.Fatal("roundtrip credential checkpoint failed")
	}
	defer db.Close()
	key, err := crypto.LoadMasterKey(os.Getenv("PESTIROUTE_DIAG_KEY"))
	if err != nil {
		t.Fatal("roundtrip credential checkpoint failed")
	}
	account := os.Getenv("PESTIROUTE_DIAG_ACCOUNT")
	if account == "" {
		t.Fatal("roundtrip credential checkpoint failed")
	}
	if err := runToolRoundtrip(db, key, account); err != nil {
		t.Fatal("roundtrip diagnostic stopped; inspect only the sanitized artifact")
	}
}

func runToolRoundtrip(db *sql.DB, key crypto.MasterKey, account string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	row, err := sqlite.NewCredentials(db).Get(ctx, account, "oauth")
	if err != nil || row.ExpiresAt == nil || !row.ExpiresAt.After(time.Now()) {
		return errors.New("credential unavailable")
	}
	plain, err := sqlite.NewCredentials(db).GetDecrypted(ctx, account, "oauth", key)
	if err != nil {
		return errors.New("credential unavailable")
	}
	defer clear(plain)
	bundle, err := decodeOAuthBundle(plain)
	if err != nil || !bundle.ExpiresAt.After(time.Now()) {
		return errors.New("credential unavailable")
	}
	threadID := uuid.NewString()
	initialBody, err := buildRoundtripInitialBody(threadID)
	if err != nil {
		return err
	}
	defer clear(initialBody)
	artifact := roundtripArtifact{SchemaVersion: 1, Profile: "codex-responses-http-sse-lite-v1", Model: "gpt-6-luna", FirstClassification: "not_run", SecondClassification: "not_run", FirstRejectionCode: "other", FirstRejectionField: "other", SecondRejectionCode: "other", SecondRejectionField: "other", FirstEvents: []diagnosticEventRun{}, SecondEvents: []diagnosticEventRun{}, FirstTerminal: "none", SecondTerminal: "none", FirstStreamStatus: "not_run", SecondStreamStatus: "not_run", Transport: "none"}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	configureCodexTransport(transport)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	firstReq, err := makeRoundtripRequest(ctx, initialBody, bundle, threadID)
	if err != nil {
		return err
	}
	firstResp, err := client.Do(firstReq)
	artifact.DirectRequests = 1
	if err != nil {
		artifact.FirstClassification, artifact.Transport = "transport_error", classifyDiagnosticTransportError(err)
		_ = saveRoundtripArtifact(artifact)
		return err
	}
	firstStatus := firstResp.StatusCode
	artifact.FirstStatus = &firstStatus
	if firstStatus < 200 || firstStatus >= 300 {
		body, readErr := io.ReadAll(io.LimitReader(firstResp.Body, toolDiagnosticBodyLimit+1))
		_ = firstResp.Body.Close()
		if readErr != nil || len(body) > toolDiagnosticBodyLimit {
			clear(body)
			artifact.FirstClassification = "http_rejected"
			artifact.FirstStreamStatus = "read_failure"
			_ = saveRoundtripArtifact(artifact)
			return errors.New("bounded first error response failed")
		}
		artifact.FirstRejectionCode, artifact.FirstRejectionField = classifyRoundtripHTTPError(body)
		clear(body)
		artifact.FirstClassification, artifact.Transport = "http_rejected", "none"
		if err := saveRoundtripArtifact(artifact); err != nil {
			return err
		}
		return errors.New("first request rejected")
	}
	first := observeRoundtripSSE(io.LimitReader(firstResp.Body, toolDiagnosticBodyLimit+1), true, cancel)
	_ = firstResp.Body.Close()
	defer first.clear()
	artifact.FirstClassification = "no_tool_emission"
	if first.Calls > 0 {
		artifact.FirstClassification = "tool_emission"
	}
	artifact.FirstEvents, artifact.FirstToolCallCount = first.Events, first.Calls
	artifact.FirstTerminal, artifact.FirstStreamStatus, artifact.FirstUsageObserved = first.Terminal, first.Status, first.Usage
	artifact.FirstToolNameValid, artifact.ArgumentsValid = first.NameValid, first.ArgsValid
	if first.Status != "complete" || first.Terminal != "completed" || first.Calls != 1 || !first.NameValid || !first.ArgsValid || first.CallID == "" || first.Status == "size_limit" {
		_ = saveRoundtripArtifact(artifact)
		return errors.New("first function call was incomplete or invalid")
	}
	output, ok := executeSyntheticSmokeLookup("smoke_lookup", string(first.Arguments))
	if !ok {
		_ = saveRoundtripArtifact(artifact)
		return errors.New("synthetic lookup rejected fixed input")
	}
	artifact.LocalLookupExecuted = true
	followupBody, err := buildRoundtripFollowup(initialBody, first.HistoryItems, first.CallID, output)
	if err != nil {
		_ = saveRoundtripArtifact(artifact)
		return err
	}
	defer clear(followupBody)
	secondReq, err := makeRoundtripRequest(ctx, followupBody, bundle, threadID)
	if err != nil {
		_ = saveRoundtripArtifact(artifact)
		return err
	}
	artifact.DirectRequests = 2
	artifact.CallResultLinked = first.CallID != "" && output == "synthetic-alpha"
	secondResp, err := client.Do(secondReq)
	if err != nil {
		artifact.SecondClassification, artifact.Transport = "transport_error", classifyDiagnosticTransportError(err)
		_ = saveRoundtripArtifact(artifact)
		return err
	}
	secondStatus := secondResp.StatusCode
	artifact.SecondStatus = &secondStatus
	if secondStatus < 200 || secondStatus >= 300 {
		body, readErr := io.ReadAll(io.LimitReader(secondResp.Body, toolDiagnosticBodyLimit+1))
		_ = secondResp.Body.Close()
		if readErr != nil || len(body) > toolDiagnosticBodyLimit {
			clear(body)
			artifact.SecondClassification = "http_rejected"
			artifact.SecondStreamStatus = "read_failure"
			_ = saveRoundtripArtifact(artifact)
			return errors.New("bounded second error response failed")
		}
		artifact.SecondRejectionCode, artifact.SecondRejectionField = classifyRoundtripHTTPError(body)
		clear(body)
		artifact.SecondClassification = "http_rejected"
		if err := saveRoundtripArtifact(artifact); err != nil {
			return err
		}
		return errors.New("follow-up request rejected")
	}
	second := observeRoundtripSSE(io.LimitReader(secondResp.Body, toolDiagnosticBodyLimit+1), false, cancel)
	_ = secondResp.Body.Close()
	defer second.clear()
	artifact.SecondEvents, artifact.SecondTerminal, artifact.SecondStreamStatus, artifact.SecondUsageObserved = second.Events, second.Terminal, second.Status, second.Usage
	artifact.CallResultLinked = artifact.CallResultLinked && second.Status == "complete" && second.Terminal == "completed"
	artifact.FinalAnswerVerified = artifact.CallResultLinked && bytes.Equal(second.Answer, []byte("synthetic-alpha"))
	artifact.SecondClassification = "roundtrip_completed"
	if !artifact.FinalAnswerVerified {
		artifact.SecondClassification = "invalid_response"
	}
	if err := saveRoundtripArtifact(artifact); err != nil {
		return err
	}
	if !artifact.FinalAnswerVerified {
		return errors.New("expected final marker not observed")
	}
	return nil
}

func TestRoundtripToolDiagnosticOffline(t *testing.T) {
	threadID := "00000000-0000-4000-8000-000000000001"
	initial, err := buildRoundtripInitialBody(threadID)
	if err != nil {
		t.Fatal("roundtrip initial request failed to build")
	}
	var gotFixture, wantFixture map[string]any
	if json.Unmarshal(initial, &gotFixture) != nil || json.Unmarshal([]byte(codexfixtures.FunctionInitial), &wantFixture) != nil {
		t.Fatal("shared direct/gateway function fixture is invalid")
	}
	gotInput := gotFixture["input"].([]any)
	wantInput := wantFixture["input"].([]any)
	gotInput[0].(map[string]any)["id"] = wantInput[0].(map[string]any)["id"]
	if !reflect.DeepEqual(gotFixture, wantFixture) {
		t.Fatal("M5.1-044 direct request diverged from the shared gateway function fixture")
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(initial, &request) != nil {
		t.Fatal("initial request invalid")
	}
	var model string
	var parallel bool
	var reasoningConfig struct{ Effort, Context string }
	if json.Unmarshal(request["model"], &model) != nil || model != "gpt-6-luna" ||
		json.Unmarshal(request["parallel_tool_calls"], &parallel) != nil || parallel ||
		json.Unmarshal(request["reasoning"], &reasoningConfig) != nil || reasoningConfig.Effort != "high" || reasoningConfig.Context != "all_turns" ||
		len(request["tools"]) != 0 || string(request["tool_choice"]) != `"auto"` {
		t.Fatal("initial request deviated from the reviewed native Lite profile")
	}
	var input []json.RawMessage
	if json.Unmarshal(request["input"], &input) != nil || len(input) != 2 {
		t.Fatal("initial native prefix/history missing")
	}
	var prefix struct {
		Tools []struct {
			Name       string `json:"name"`
			Parameters struct {
				Properties map[string]struct {
					Enum []string `json:"enum"`
				} `json:"properties"`
			} `json:"parameters"`
		} `json:"tools"`
	}
	if json.Unmarshal(input[0], &prefix) != nil || len(prefix.Tools) != 1 || prefix.Tools[0].Name != "smoke_lookup" || len(prefix.Tools[0].Parameters.Properties["key"].Enum) != 1 || prefix.Tools[0].Parameters.Properties["key"].Enum[0] != "alpha" {
		t.Fatal("roundtrip function definition was not limited to the fixed synthetic key")
	}
	reasoning := json.RawMessage(`{"type":"reasoning","id":"ri_1","summary":[{"type":"summary_text","text":"private-summary"}],"encrypted_content":"private-ciphertext","x-opaque":{"v":1}}`)
	message := json.RawMessage(`{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"pre-call note"}],"x-opaque":true}`)
	call := json.RawMessage(`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"smoke_lookup","arguments":"{\"key\":\"alpha\"}","x-opaque":true}`)
	streamFixture := strings.Join([]string{
		`event: response.output_item.done`, `data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"ri_1","summary":[{"type":"summary_text","text":"private-summary"}],"encrypted_content":"private-ciphertext","x-opaque":{"v":1}}}`, ``,
		`event: response.output_item.done`, `data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"pre-call note"}],"x-opaque":true}}`, ``,
		`event: response.output_item.done`, `data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"smoke_lookup","arguments":"{\"key\":\"alpha\"}","x-opaque":true}}`, ``,
		`event: response.completed`, `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5}}}`, ``, ``,
	}, "\n")
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := observeRoundtripSSE(strings.NewReader(streamFixture), true, cancel)
	defer observed.clear()
	if observed.Status != "complete" || observed.Calls != 1 || !observed.NameValid || !observed.ArgsValid || observed.CallID != "call_1" || !observed.Usage || len(observed.HistoryItems) != 3 {
		t.Fatal("synthetic first response was not safely observed")
	}
	if !bytes.Equal(observed.HistoryItems[0], reasoning) || !bytes.Equal(observed.HistoryItems[1], message) || !bytes.Equal(observed.HistoryItems[2], call) {
		t.Fatal("opaque response items were normalized or reordered")
	}
	output, ok := executeSyntheticSmokeLookup("smoke_lookup", `{"key":"alpha"}`)
	if !ok {
		t.Fatal("fixed local lookup did not run")
	}
	followup, err := buildRoundtripFollowup(initial, observed.HistoryItems, observed.CallID, output)
	if err != nil {
		t.Fatal("follow-up body failed to build")
	}
	var replay map[string]json.RawMessage
	if json.Unmarshal(followup, &replay) != nil {
		t.Fatal("follow-up body invalid")
	}
	if string(replay["parallel_tool_calls"]) != "false" || string(replay["tool_choice"]) != `"auto"` || len(replay["tools"]) != 0 {
		t.Fatal("follow-up changed native Lite tool options")
	}
	var history []json.RawMessage
	if json.Unmarshal(replay["input"], &history) != nil || len(history) != 6 {
		t.Fatal("follow-up omitted chronological history/result")
	}
	if !bytes.Equal(history[0], input[0]) || !bytes.Equal(history[1], input[1]) || !bytes.Equal(history[2], reasoning) || !bytes.Equal(history[3], message) || !bytes.Equal(history[4], call) {
		t.Fatal("follow-up changed opaque history or order")
	}
	var linked struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	if json.Unmarshal(history[5], &linked) != nil || linked.Type != "function_call_output" || linked.CallID != observed.CallID || linked.Output != output {
		t.Fatal("function result was not linked to captured call id")
	}
	final := strings.Join([]string{`event: response.output_text.delta`, `data: {"type":"response.output_text.delta","delta":"synthetic-alpha"}`, ``, `event: response.output_text.done`, `data: {"type":"response.output_text.done","text":"synthetic-alpha"}`, ``, `event: response.completed`, `data: {"type":"response.completed","response":{"status":"completed"}}`, ``, ``}, "\n")
	result := observeRoundtripSSE(strings.NewReader(final), false, cancel)
	defer result.clear()
	if result.Status != "complete" || !bytes.Equal(result.Answer, []byte("synthetic-alpha")) {
		t.Fatal("final expected marker was not verified")
	}
	serialized, _ := json.Marshal(result)
	if strings.Contains(string(serialized), "synthetic-alpha") {
		t.Fatal("stream summary retained response output")
	}
}

func TestRoundtripTwoFakeResponsesEndToEnd(t *testing.T) {
	threadID := "00000000-0000-4000-8000-000000000001"
	reasoning := json.RawMessage("{ \"type\" : \"reasoning\", \"id\" : \"ri_fake\", \"summary\":[{\"type\":\"summary_text\",\"text\":\"private-summary <>&\u2028\u2029\"}], \"encrypted_content\" : \"private-ciphertext\", \"x-opaque\" : {\"v\" : 1} }")
	message := json.RawMessage(`{ "type":"message", "id":"msg_fake", "role":"assistant", "content":[{"type":"output_text","text":"pre-call note <>&"}], "x-opaque":true }`)
	call := json.RawMessage(`{"type":"function_call","id":"fc_fake","call_id":"call_fake_1","name":"smoke_lookup","arguments":"{\"key\":\"alpha\"}","x-opaque":true}`)
	initial, err := buildRoundtripInitialBody(threadID)
	if err != nil {
		t.Fatal("initial body failed")
	}
	initialStart, initialEnd, err := roundtripInputBounds(initial)
	if err != nil {
		t.Fatal("initial input bounds failed")
	}
	var initialInput []json.RawMessage
	if json.Unmarshal(initial[initialStart:initialEnd], &initialInput) != nil || len(initialInput) != 2 {
		t.Fatal("initial history failed to parse")
	}
	firstSSE := strings.Join([]string{
		`event: response.output_item.done`, `data: {"type":"response.output_item.done","item":` + string(reasoning) + `}`, ``,
		`event: response.output_item.done`, `data: {"type":"response.output_item.done","item":` + string(message) + `}`, ``,
		`event: response.output_item.done`, `data: {"type":"response.output_item.done","item":` + string(call) + `}`, ``,
		`event: response.completed`, `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":4}}}`, ``, ``,
	}, "\n")
	finalSSE := strings.Join([]string{
		`event: response.output_text.delta`, `data: {"type":"response.output_text.delta","delta":"synthetic-alpha"}`, ``,
		`event: response.output_text.done`, `data: {"type":"response.output_text.done","text":"synthetic-alpha"}`, ``,
		`event: response.completed`, `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":6}}}`, ``, ``,
	}, "\n")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" || r.Header.Get("x-openai-internal-codex-responses-lite") != "true" || r.Header.Get("thread-id") != threadID {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, toolDiagnosticBodyLimit+1))
		defer clear(body)
		if err != nil || len(body) > toolDiagnosticBodyLimit {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		var request map[string]json.RawMessage
		if json.Unmarshal(body, &request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var input []json.RawMessage
		if json.Unmarshal(request["input"], &input) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if requests == 1 {
			if len(input) != 2 || !bytes.Equal(input[0], initialInput[0]) || !bytes.Equal(input[1], initialInput[1]) || string(request["parallel_tool_calls"]) != "false" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		} else {
			if len(input) != 6 || !bytes.Equal(input[0], initialInput[0]) || !bytes.Equal(input[1], initialInput[1]) || !bytes.Equal(input[2], reasoning) || !bytes.Equal(input[3], message) || !bytes.Equal(input[4], call) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var output struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Output string `json:"output"`
			}
			if json.Unmarshal(input[5], &output) != nil || output.Type != "function_call_output" || output.CallID != "call_fake_1" || output.Output != "synthetic-alpha" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			_, _ = io.WriteString(w, firstSSE)
		} else {
			_, _ = io.WriteString(w, finalSSE)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bundle := oauthBundle{AccessToken: "synthetic", AccountID: "synthetic"}
	firstReq, err := makeRoundtripRequestTo(ctx, server.URL+"/backend-api/codex/responses", initial, bundle, threadID)
	if err != nil {
		t.Fatal("first request failed to build")
	}
	firstResp, err := server.Client().Do(firstReq)
	if err != nil {
		t.Fatal("fake first response failed")
	}
	if firstResp.StatusCode != http.StatusOK {
		firstResp.Body.Close()
		t.Fatal("fake first response rejected request")
	}
	first := observeRoundtripSSE(io.LimitReader(firstResp.Body, toolDiagnosticBodyLimit+1), true, cancel)
	firstResp.Body.Close()
	defer first.clear()
	if first.Status != "complete" || first.Calls != 1 || !first.NameValid || !first.ArgsValid || !first.Usage {
		t.Fatal("fake first response did not produce one valid call")
	}
	output, ok := executeSyntheticSmokeLookup("smoke_lookup", string(first.Arguments))
	if !ok {
		t.Fatal("fixed local lookup failed")
	}
	followup, err := buildRoundtripFollowup(initial, first.HistoryItems, first.CallID, output)
	if err != nil {
		t.Fatal("follow-up body failed")
	}
	defer clear(followup)
	secondReq, err := makeRoundtripRequestTo(ctx, server.URL+"/backend-api/codex/responses", followup, bundle, threadID)
	if err != nil {
		t.Fatal("second request failed to build")
	}
	secondResp, err := server.Client().Do(secondReq)
	if err != nil {
		t.Fatal("fake second response failed")
	}
	if secondResp.StatusCode != http.StatusOK {
		secondResp.Body.Close()
		t.Fatal("fake second response rejected linked history")
	}
	second := observeRoundtripSSE(io.LimitReader(secondResp.Body, toolDiagnosticBodyLimit+1), false, cancel)
	secondResp.Body.Close()
	defer second.clear()
	if requests != 2 || second.Status != "complete" || second.Terminal != "completed" || !second.Usage || !bytes.Equal(second.Answer, []byte("synthetic-alpha")) {
		t.Fatal("two-response fake roundtrip did not complete with the expected marker")
	}
}

func TestRoundtripToolDiagnosticBoundsAndPrivacy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tooMuch := strings.Join([]string{`event: response.output_text.delta`, `data: {"type":"response.output_text.delta","delta":"` + strings.Repeat("x", 257) + `"}`, ``, ``}, "\n")
	got := observeRoundtripSSE(strings.NewReader(tooMuch), false, cancel)
	if got.Status != "output_limit" || ctx.Err() != context.Canceled {
		t.Fatal("visible output limit did not cancel stream")
	}
	cancel()
	oversized := observeRoundtripSSE(strings.NewReader(strings.Repeat("x", toolDiagnosticBodyLimit+1)), false, cancel)
	if oversized.Status != "size_limit" {
		t.Fatal("SSE body limit not enforced")
	}
	broken := observeRoundtripSSE(roundtripFaultReader{}, false, cancel)
	if broken.Status != "read_failure" {
		t.Fatal("SSE reader fault was not classified")
	}
	for _, args := range []string{`{"key":"alpha"}`, `{"key":"beta"}`, `{"key":"alpha","extra":true}`, `{"key":"alpha","key":"alpha"}`, strings.Repeat("x", 257)} {
		_, valid := executeSyntheticSmokeLookup("smoke_lookup", args)
		if valid != (args == `{"key":"alpha"}`) {
			t.Fatal("fixed synthetic lookup accepted invalid arguments")
		}
	}
	_, valid := executeSyntheticSmokeLookup("other_tool", `{"key":"alpha"}`)
	if valid {
		t.Fatal("arbitrary tool name reached local executor")
	}
	badItem := []byte(`{"type":"reasoning","private":"provider-secret"`)
	if _, err := buildRoundtripFollowup([]byte(`{"input":[{},{}]}`), [][]byte{badItem}, "call-safe", "synthetic-alpha"); err == nil || strings.Contains(err.Error(), "provider-secret") {
		t.Fatal("malformed provider item was not generically rejected")
	}
	nearLimit := []byte(`{"input":[{},{}],"padding":"` + strings.Repeat("x", toolDiagnosticBodyLimit-128) + `"}`)
	if len(nearLimit) >= toolDiagnosticBodyLimit {
		t.Fatal("combined-size fixture exceeded source limit")
	}
	if _, err := buildRoundtripFollowup(nearLimit, [][]byte{[]byte(`{"type":"reasoning"}`)}, "call-safe", "synthetic-alpha"); err == nil {
		t.Fatal("combined follow-up request exceeded its one-megabyte cap")
	}
	oversizedSource := append(bytes.Clone(nearLimit), bytes.Repeat([]byte{' '}, toolDiagnosticBodyLimit)...)
	if _, err := buildRoundtripFollowup(oversizedSource, nil, "call-safe", "synthetic-alpha"); err == nil {
		t.Fatal("oversized source request was accepted")
	}
	status := http.StatusOK
	artifact := roundtripArtifact{SchemaVersion: 1, Profile: "codex-responses-http-sse-lite-v1", Model: "gpt-6-luna", DirectRequests: 2, FirstStatus: &status, SecondStatus: &status, FirstClassification: "tool_emission", SecondClassification: "roundtrip_completed", FirstRejectionCode: "other", FirstRejectionField: "other", SecondRejectionCode: "other", SecondRejectionField: "other", FirstEvents: []diagnosticEventRun{{Type: "response.output_item.done", Count: 2}}, SecondEvents: []diagnosticEventRun{{Type: "response.output_text.delta", Count: 1}}, FirstToolCallCount: 1, FirstToolNameValid: true, ArgumentsValid: true, LocalLookupExecuted: true, CallResultLinked: true, FinalAnswerVerified: true, FirstTerminal: "completed", SecondTerminal: "completed", FirstStreamStatus: "complete", SecondStreamStatus: "complete", Transport: "none"}
	if !validRoundtripArtifact(artifact) {
		t.Fatal("safe roundtrip summary rejected")
	}
	artifact.FinalAnswerVerified = false
	artifact.SecondClassification = "PRIVATE_PAYLOAD"
	if validRoundtripArtifact(artifact) {
		t.Fatal("poisoned roundtrip summary accepted")
	}
}

type roundtripFaultReader struct{}

func (roundtripFaultReader) Read([]byte) (int, error) { return 0, errors.New("synthetic read failure") }

func TestRoundtripRequestUsesExactLiteIdentity(t *testing.T) {
	ctx := context.Background()
	id := "00000000-0000-4000-8000-000000000001"
	body, err := buildRoundtripInitialBody(id)
	if err != nil {
		t.Fatal("initial body did not build")
	}
	req, err := makeRoundtripRequest(ctx, body, oauthBundle{AccessToken: "synthetic", AccountID: "synthetic"}, id)
	if err != nil || req.URL.Path != "/backend-api/codex/responses" || req.Header.Get("x-openai-internal-codex-responses-lite") != "true" || req.Header.Get("session-id") != id || req.Header.Get("thread-id") != id || req.Header.Get("x-client-request-id") != id || req.Header.Get("originator") != "pestiroute" || req.Header.Get("User-Agent") != "PestiRoute" {
		t.Fatal("roundtrip request did not use exact fixed Lite identity")
	}
	actual, err := io.ReadAll(req.Body)
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatal("the POST body differed from the tested request source")
	}
}

func TestRoundtripDeadlineRejectionAndArtifactPrivacy(t *testing.T) {
	id := "00000000-0000-4000-8000-000000000001"
	bundle := oauthBundle{AccessToken: "synthetic", AccountID: "synthetic"}
	deadlineCtx, deadlineCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer deadlineCancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{name: "canceled", ctx: canceledRoundtripContext(), want: "canceled"},
		{name: "deadline", ctx: deadlineCtx, want: "deadline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := buildRoundtripInitialBody(id)
			if err != nil {
				t.Fatal("request source failed")
			}
			req, err := makeRoundtripRequest(tc.ctx, body, bundle, id)
			if err != nil {
				t.Fatal("request construction failed")
			}
			_, err = http.DefaultClient.Do(req)
			if err == nil || classifyDiagnosticTransportError(err) != tc.want {
				t.Fatal("cancel/deadline was not classified safely")
			}
		})
	}
	code, field := classifyRoundtripHTTPError([]byte(`{"error":{"code":"unsupported_value","param":"tools","message":"PRIVATE_TOKEN=private-value"}}`))
	if code != "unsupported_value" || field != "tools" {
		t.Fatal("non-2xx classification lost allowlisted fields")
	}
	status := http.StatusBadRequest
	a := roundtripArtifact{SchemaVersion: 1, Profile: "codex-responses-http-sse-lite-v1", Model: "gpt-6-luna", DirectRequests: 1, FirstStatus: &status, FirstClassification: "http_rejected", SecondClassification: "not_run", FirstRejectionCode: code, FirstRejectionField: field, SecondRejectionCode: "other", SecondRejectionField: "other", FirstEvents: []diagnosticEventRun{}, SecondEvents: []diagnosticEventRun{}, FirstTerminal: "none", SecondTerminal: "none", FirstStreamStatus: "not_run", SecondStreamStatus: "not_run", Transport: "none"}
	encoded, err := json.Marshal(a)
	if err != nil || strings.Contains(string(encoded), "PRIVATE") || strings.Contains(string(encoded), "private-value") || !validRoundtripArtifact(a) {
		t.Fatal("HTTP rejection summary retained provider text")
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal("private directory setup failed")
	}
	if err := writeRoundtripArtifactInDirectory(a, directory); err != nil {
		t.Fatal("safe rejection artifact write failed")
	}
	path := filepath.Join(directory, roundtripArtifactName)
	info, err := os.Stat(path)
	data, readErr := os.ReadFile(path)
	if err != nil || readErr != nil || info.Mode().Perm() != 0600 || strings.Contains(string(data), "private-value") {
		t.Fatal("artifact permissions/privacy failed")
	}
	if err := writeRoundtripArtifactInDirectory(a, directory); err == nil {
		t.Fatal("roundtrip artifact overwrite was allowed")
	}
}

func canceledRoundtripContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
