package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

type m4CompatToolCall struct {
	ID, Name string
	Input    json.RawMessage
}

type m4CompatUsage struct {
	Input, Output *int64
}

type m4CompatRound struct {
	Status      int
	Terminal    string
	Text        string
	Calls       []m4CompatToolCall
	Content     []any
	OutputItems []map[string]any
	Usage       m4CompatUsage
}

type m4CompatMessageBlock struct {
	typeName string
	text     string
	call     *m4CompatToolCall
}

type m4CompatRequestRecord struct {
	Run, Attempt, Timeout, Tokens, Status int
	Stream                                bool
	Path, Purpose                         string
}

type m4CompatToolBatch struct {
	Requests []m4CompatRequestRecord
	Links    []string
	Usage    string
	Failure  *m4CompatPartialFailure
}

type m4CompatPartialFailure struct {
	SchemaVersion      int    `json:"schema_version"`
	EndpointProfile    string `json:"endpoint_profile"`
	ModelOverride      bool   `json:"test_only_model_override"`
	Leg                string `json:"leg"`
	Turn               int    `json:"turn"`
	TestDurationMS     int64  `json:"test_duration_ms"`
	HTTPStatus         *int   `json:"http_status,omitempty"`
	HTTPStatusScope    string `json:"http_status_scope"`
	ProviderDispatches int    `json:"provider_dispatches"`
	Terminal           string `json:"terminal"`
	ToolClass          string `json:"tool_class"`
	Category           string `json:"category"`
	Retries            int    `json:"retries"`
}

type m4CompatProviderDoer struct {
	endpoint, key string
	client        *http.Client
	budget        *m4CompatCallBudget
	cancelled     chan struct{}
}

func (d *m4CompatProviderDoer) Do(req *http.Request) (*http.Response, error) {
	data, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		return nil, errors.New("translated_request_read")
	}
	var source struct {
		Model  string `json:"model"`
		Max    int    `json:"max_tokens"`
		Stream bool   `json:"stream"`
	}
	if json.Unmarshal(data, &source) != nil || source.Model != "claude-opus-5-5" || source.Max != m4CompatMax || !source.Stream {
		return nil, errors.New("translated_request_bounds")
	}
	data, err = m4CompatOverrideBackendModel(data)
	if err != nil {
		return nil, errors.New("compatible_model_override")
	}
	endpoint, err := url.Parse(d.endpoint)
	if err != nil {
		return nil, errors.New("compatible_endpoint_parse")
	}
	copy := req.Clone(req.Context())
	copy.URL = endpoint
	copy.Host = endpoint.Host
	copy.Body = io.NopCloser(bytes.NewReader(data))
	copy.ContentLength = int64(len(data))
	copy.GetBody = nil
	copy.Header = req.Header.Clone()
	copy.Header.Set("x-api-key", d.key)
	copy.Header.Set("anthropic-version", "2023-06-01")
	if !d.budget.take() {
		return nil, errors.New("provider_call_cap")
	}
	resp, err := d.client.Do(copy)
	if err != nil {
		return nil, err
	}
	if d.cancelled != nil {
		go func() {
			<-req.Context().Done()
			select {
			case d.cancelled <- struct{}{}:
			default:
			}
		}()
	}
	return resp, nil
}

func m4CompatPostMessages(client *http.Client, endpoint, key string, budget *m4CompatCallBudget, body []byte) (*http.Response, error) {
	if !budget.take() {
		return nil, errors.New("provider_call_cap")
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("direct_request_build")
	}
	req.GetBody = nil
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream")
	return client.Do(req)
}

func m4CompatReadMessagesRound(body io.Reader, status int) (m4CompatRound, error) {
	round := m4CompatRound{Status: status}
	blocks := map[int]*m4CompatMessageBlock{}
	partials := map[int]string{}
	sawMessageStop := false
	parseEvent := func(data string) (bool, error) {
		var event struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Usage struct {
					Input *int64 `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				Output *int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return false, errors.New("messages_event_json")
		}
		switch event.Type {
		case "message_start":
			round.Usage.Input = event.Message.Usage.Input
		case "content_block_start":
			switch event.ContentBlock.Type {
			case "text":
				blocks[event.Index] = &m4CompatMessageBlock{typeName: "text"}
			case "tool_use":
				blocks[event.Index] = &m4CompatMessageBlock{typeName: "tool_use", call: &m4CompatToolCall{ID: event.ContentBlock.ID, Name: event.ContentBlock.Name, Input: event.ContentBlock.Input}}
			default:
				return false, errors.New("unsupported_messages_content_block")
			}
		case "content_block_delta":
			if block := blocks[event.Index]; block == nil {
				return false, errors.New("messages_delta_without_block")
			} else if block.typeName == "tool_use" && event.Delta.Type == "input_json_delta" {
				partials[event.Index] += event.Delta.PartialJSON
			} else if block.typeName == "text" && event.Delta.Type == "text_delta" {
				block.text += event.Delta.Text
				round.Text += event.Delta.Text
			} else {
				return false, errors.New("unexpected_messages_delta")
			}
		case "message_delta":
			round.Terminal = event.Delta.StopReason
			round.Usage.Output = event.Usage.Output
		case "message_stop":
			if round.Terminal == "" {
				round.Terminal = "message_stop"
			}
			sawMessageStop = true
			return true, nil
		}
		return false, nil
	}
	if err := m4CompatReadSSE(body, parseEvent); err != nil {
		if eventErr, ok := errors.AsType[*m4CompatSSEEventError](err); ok {
			return round, eventErr.err
		}
		return round, errors.New("messages_stream_read")
	}
	indices := make([]int, 0, len(blocks))
	for index := range blocks {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		block := blocks[index]
		if block.typeName == "text" {
			round.Content = append(round.Content, map[string]any{"type": "text", "text": block.text})
			continue
		}
		call := block.call
		if partials[index] != "" {
			call.Input = json.RawMessage(partials[index])
		}
		var input any
		if len(call.Input) == 0 || json.Unmarshal(call.Input, &input) != nil || call.ID == "" || call.Name == "" {
			return round, errors.New("messages_tool_arguments")
		}
		round.Calls = append(round.Calls, *call)
		round.Content = append(round.Content, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": input})
	}
	if !sawMessageStop {
		return round, errors.New("messages_terminal_missing")
	}
	return round, nil
}

type m4CompatSSEEventError struct{ err error }

func (e *m4CompatSSEEventError) Error() string { return e.err.Error() }

// m4CompatReadSSE keeps only the current event, so parsing remains incremental
// and a terminal event can stop the read without consuming trailing bytes.
func m4CompatReadSSE(body io.Reader, onData func(string) (bool, error)) error {
	const maxSSELine = 2 << 20
	const maxSSEEvent = 2 << 20
	reader := bufio.NewReaderSize(body, 4096)
	var line, data []byte
	hasData := false
	dispatch := func() (bool, error) {
		if !hasData {
			return false, nil
		}
		stop, err := onData(string(data))
		data = data[:0]
		hasData = false
		if err != nil {
			return false, &m4CompatSSEEventError{err: err}
		}
		return stop, nil
	}
	for {
		fragment, readErr := reader.ReadSlice('\n')
		if len(line)+len(fragment) > maxSSELine {
			return errors.New("messages_sse_line_limit")
		}
		line = append(line, fragment...)
		if readErr == bufio.ErrBufferFull {
			continue
		}
		if readErr != nil && readErr != io.EOF && (len(line) == 0 || line[len(line)-1] != '\n') {
			return readErr
		}
		if len(line) > 0 {
			if line[len(line)-1] == '\n' {
				line = line[:len(line)-1]
			}
			line = bytes.TrimSuffix(line, []byte{'\r'})
			if len(line) == 0 {
				stop, err := dispatch()
				if err != nil || stop {
					return err
				}
			} else if line[0] != ':' {
				field, value, found := bytes.Cut(line, []byte{':'})
				if found && bytes.Equal(field, []byte("data")) {
					if len(value) > 0 && value[0] == ' ' {
						value = value[1:]
					}
					additional := len(value)
					if hasData {
						additional++
					}
					if len(data)+additional > maxSSEEvent {
						return errors.New("messages_sse_event_limit")
					}
					if hasData {
						data = append(data, '\n')
					}
					data = append(data, value...)
					hasData = true
				}
			}
			line = line[:0]
		}
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		if readErr == io.EOF {
			stop, err := dispatch()
			if err != nil || stop {
				return err
			}
			return nil
		}
	}
}

func m4CompatReadResponsesRound(body io.Reader, status int) (m4CompatRound, error) {
	round := m4CompatRound{Status: status}
	var input, output *int64
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			return round, errors.New("responses_event_json")
		}
		switch event["type"] {
		case "response.function_call_arguments.delta":
			// The authoritative complete call is captured from output_item.done below.
		case "response.output_item.done":
			item, _ := event["item"].(map[string]any)
			if item != nil {
				round.OutputItems = append(round.OutputItems, item)
			}
			if item["type"] == "function_call" {
				id, _ := item["call_id"].(string)
				name, _ := item["name"].(string)
				args, _ := item["arguments"].(string)
				if id == "" || name == "" || !json.Valid([]byte(args)) {
					return round, errors.New("responses_tool_call_invalid")
				}
				round.Calls = append(round.Calls, m4CompatToolCall{ID: id, Name: name, Input: json.RawMessage(args)})
			}
		case "response.output_text.delta":
			round.Text += fmt.Sprint(event["delta"])
		case "response.completed":
			round.Terminal = "response.completed"
			response, _ := event["response"].(map[string]any)
			usage, _ := response["usage"].(map[string]any)
			input = m4CompatNumber(usage["input_tokens"])
			output = m4CompatNumber(usage["output_tokens"])
		}
	}
	if err := scanner.Err(); err != nil {
		return round, errors.New("responses_stream_read")
	}
	round.Usage = m4CompatUsage{Input: input, Output: output}
	if round.Terminal == "" {
		return round, errors.New("responses_terminal_missing")
	}
	return round, nil
}

func m4CompatNumber(value any) *int64 {
	n, ok := value.(float64)
	if !ok || n < 0 || n != float64(int64(n)) {
		return nil
	}
	v := int64(n)
	return &v
}

func m4CompatUsageAvailability(rounds []m4CompatRound) string {
	known, missing := false, false
	for _, round := range rounds {
		if round.Usage.Input != nil && round.Usage.Output != nil {
			known = true
		} else {
			missing = true
		}
	}
	if known && missing {
		return "reported_or_unknown"
	}
	if known {
		return "reported"
	}
	return "unknown"
}

func m4CompatTools() []map[string]any {
	return []map[string]any{
		{"name": "weather", "description": "Get fixture weather", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]string{"type": "string"}}, "required": []string{"city"}, "additionalProperties": false}},
		{"name": "clock", "description": "Get fixture UTC time", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"zone": map[string]string{"type": "string"}}, "required": []string{"zone"}, "additionalProperties": false}},
	}
}

func m4CompatChoiceMatches(raw json.RawMessage, turn int) bool {
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &choice) != nil {
		return false
	}
	if turn == 3 || turn == 6 {
		return choice.Type == "auto" && choice.Name == ""
	}
	expectedTurn := turn
	if turn > 3 {
		expectedTurn -= 3
	}
	return choice.Type == "tool" && choice.Name == m4CompatExpectedTool(expectedTurn)
}

type m4CompatHistoryMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type m4CompatHistoryBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	ToolUseID string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input"`
}

func m4CompatMockHistoryValid(messages []json.RawMessage, expectedCalls int) bool {
	var calls []m4CompatToolCall
	var results []string
	for _, raw := range messages {
		var message m4CompatHistoryMessage
		if json.Unmarshal(raw, &message) != nil || message.Role == "" {
			return false
		}
		if len(message.Content) == 0 || message.Content[0] != '[' {
			continue
		}
		var blocks []m4CompatHistoryBlock
		if json.Unmarshal(message.Content, &blocks) != nil {
			return false
		}
		for _, block := range blocks {
			switch {
			case message.Role == "assistant" && block.Type == "tool_use":
				calls = append(calls, m4CompatToolCall{ID: block.ID, Name: block.Name, Input: block.Input})
			case message.Role == "user" && block.Type == "tool_result":
				results = append(results, block.ToolUseID)
			}
		}
	}
	if len(calls) != expectedCalls || len(results) != expectedCalls {
		return false
	}
	for i, call := range calls {
		if call.ID == "" || call.ID != results[i] || m4CompatValidateToolCall(call, m4CompatExpectedTool(i+1)) != nil {
			return false
		}
	}
	return true
}

func m4CompatMockProviderRequestValid(turn, max int, stream bool, tools []struct {
	Name string `json:"name"`
}, choice json.RawMessage, messages []json.RawMessage) bool {
	if max != m4CompatMax || !stream || len(tools) != 2 || tools[0].Name != "weather" || tools[1].Name != "clock" || !m4CompatChoiceMatches(choice, turn) {
		return false
	}
	expectedCalls := 0
	switch turn {
	case 2, 5:
		expectedCalls = 1
	case 3, 6:
		expectedCalls = 2
	case 1, 4:
	default:
		return false
	}
	return m4CompatMockHistoryValid(messages, expectedCalls)
}

func m4CompatToolResult(call m4CompatToolCall) (string, error) {
	switch call.Name {
	case "weather":
		return `{"temperature_c":18,"conditions":"clear"}`, nil
	case "clock":
		return `{"time":"12:00Z"}`, nil
	default:
		return "", errors.New("unexpected_tool_name")
	}
}

func m4CompatToolClass(name string) string {
	switch name {
	case "weather":
		return "weather"
	case "clock":
		return "clock"
	default:
		return "other"
	}
}

func m4CompatTerminalClass(terminal string) string {
	switch terminal {
	case "tool_use", "end_turn", "max_tokens", "stop_sequence", "completed", "missing", "other":
		return terminal
	case "response.completed":
		return "completed"
	case "":
		return "missing"
	default:
		return "other"
	}
}

func m4CompatExpectedTool(turn int) string {
	switch turn {
	case 1:
		return "weather"
	case 2:
		return "clock"
	default:
		return ""
	}
}

func m4CompatValidateToolCall(call m4CompatToolCall, expected string) error {
	if call.Name != expected {
		return errors.New("unexpected_tool_name")
	}
	want := ""
	switch expected {
	case "weather":
		want = `{"city":"Paris"}`
	case "clock":
		want = `{"zone":"UTC"}`
	default:
		return errors.New("unexpected_tool_name")
	}
	if !bytes.Equal(bytes.TrimSpace(call.Input), []byte(want)) {
		return errors.New("unexpected_tool_arguments")
	}
	return nil
}

func m4CompatFailureCategory(err error) string {
	if err == nil {
		return "unknown"
	}
	switch err.Error() {
	case "unexpected_tool_name":
		return "unexpected_tool_name"
	case "unexpected_tool_arguments":
		return "unexpected_tool_arguments"
	case "direct_tool_round_missing":
		return "tool_round_missing"
	case "gateway_tool_round_missing":
		return "tool_round_missing"
	case "direct_final_turn_missing":
		return "final_turn_missing"
	case "gateway_final_turn_missing":
		return "final_turn_missing"
	case "messages_terminal_missing":
		return "terminal_missing"
	case "messages_event_json":
		return "event_invalid"
	case "messages_stream_read":
		return "stream_read"
	case "unsupported_messages_content_block", "messages_delta_without_block", "unexpected_messages_delta", "messages_tool_arguments":
		return "message_shape_invalid"
	default:
		return "other"
	}
}

func m4CompatWritePartialFailure(dir string, failure *m4CompatPartialFailure) error {
	if failure == nil || failure.SchemaVersion != 1 || failure.EndpointProfile != "compatible_endpoint" || !failure.ModelOverride || failure.Turn < 1 || failure.TestDurationMS < 0 || failure.TestDurationMS > int64(m4CompatTimeout/time.Millisecond) || failure.ProviderDispatches < 0 || failure.ProviderDispatches > 8 || failure.Retries != 0 {
		return errors.New("partial_failure_invalid")
	}
	if failure.Leg != "direct_messages" && failure.Leg != "gateway_responses_to_messages" {
		return errors.New("partial_failure_invalid")
	}
	if failure.HTTPStatus != nil && (*failure.HTTPStatus < 100 || *failure.HTTPStatus > 599) {
		return errors.New("partial_failure_invalid")
	}
	if failure.HTTPStatus != nil && failure.HTTPStatusScope != "provider" && failure.HTTPStatusScope != "gateway" {
		return errors.New("partial_failure_invalid")
	}
	if failure.HTTPStatus == nil && failure.HTTPStatusScope != "unknown" {
		return errors.New("partial_failure_invalid")
	}
	switch failure.Terminal {
	case "tool_use", "end_turn", "max_tokens", "stop_sequence", "completed", "missing", "other":
	default:
		return errors.New("partial_failure_invalid")
	}
	switch failure.ToolClass {
	case "weather", "clock", "other":
	default:
		return errors.New("partial_failure_invalid")
	}
	switch failure.Category {
	case "unexpected_tool_name", "unexpected_tool_arguments", "tool_round_missing", "final_turn_missing", "terminal_missing", "event_invalid", "stream_read", "message_shape_invalid", "other":
	default:
		return errors.New("partial_failure_invalid")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(failure, "", "  ")
	if err != nil {
		return errors.New("partial_failure_encode")
	}
	return os.WriteFile(filepath.Join(dir, "partial-failure.json"), append(encoded, '\n'), 0600)
}

func m4CompatNewPartialFailure(leg string, turn, status, dispatches int, terminal, toolClass, category string) *m4CompatPartialFailure {
	failure := &m4CompatPartialFailure{SchemaVersion: 1, EndpointProfile: "compatible_endpoint", ModelOverride: true, Leg: leg, Turn: turn, ProviderDispatches: dispatches, Terminal: m4CompatTerminalClass(terminal), ToolClass: toolClass, Category: category, Retries: 0, HTTPStatusScope: "unknown"}
	if status >= 100 && status <= 599 {
		failure.HTTPStatus = &status
		failure.HTTPStatusScope = "gateway"
		if leg == "direct_messages" {
			failure.HTTPStatusScope = "provider"
		}
	}
	return failure
}

func m4CompatRunDirectTools(client *http.Client, endpoint, key string, budget *m4CompatCallBudget) (m4CompatToolBatch, error) {
	batch := m4CompatToolBatch{}
	dispatchesBefore := budget.calls.Load()
	history := []any{map[string]any{"role": "user", "content": "Use weather for Paris, then use clock for UTC in a separate tool turn. After both results, do not call any more tools; briefly summarize using only those results."}}
	var rounds []m4CompatRound
	for turn := range 3 {
		choice := any(map[string]any{"type": "tool", "name": m4CompatExpectedTool(turn + 1)})
		tools := any(m4CompatTools())
		if turn == 2 {
			choice = map[string]any{"type": "auto"}
		}
		body, _ := json.Marshal(map[string]any{"model": m4CompatModel, "max_tokens": m4CompatMax, "stream": true, "temperature": 0, "messages": history, "tools": tools, "tool_choice": choice})
		resp, err := m4CompatPostMessages(client, endpoint, key, budget, body)
		if err != nil {
			batch.Failure = m4CompatNewPartialFailure("direct_messages", turn+1, 0, int(budget.calls.Load()-dispatchesBefore), "missing", "other", "other")
			return batch, errors.New(m4CompatTransportDiagnostic(err, 0))
		}
		round, parseErr := m4CompatReadMessagesRound(resp.Body, resp.StatusCode)
		_ = resp.Body.Close()
		if parseErr != nil || resp.StatusCode != http.StatusOK {
			status := resp.StatusCode
			category := m4CompatFailureCategory(parseErr)
			if parseErr == nil {
				category = "other"
			}
			batch.Failure = m4CompatNewPartialFailure("direct_messages", turn+1, status, int(budget.calls.Load()-dispatchesBefore), round.Terminal, "other", category)
			if parseErr != nil {
				return batch, fmt.Errorf("direct_messages_round_failed: %s", category)
			}
			return batch, fmt.Errorf("direct_messages_http_status=%d", resp.StatusCode)
		}
		rounds = append(rounds, round)
		batch.Requests = append(batch.Requests, m4CompatRequestRecord{Run: 1, Attempt: 1, Timeout: 30, Tokens: m4CompatMax, Status: resp.StatusCode, Stream: true, Path: "/v1/messages", Purpose: "tool_round"})
		if turn < 2 {
			if len(round.Calls) != 1 || round.Terminal != "tool_use" {
				status := resp.StatusCode
				batch.Failure = m4CompatNewPartialFailure("direct_messages", turn+1, status, int(budget.calls.Load()-dispatchesBefore), round.Terminal, "other", "tool_round_missing")
				return batch, errors.New("direct_tool_round_missing")
			}
			call := round.Calls[0]
			if err := m4CompatValidateToolCall(call, m4CompatExpectedTool(turn+1)); err != nil {
				status := resp.StatusCode
				batch.Failure = m4CompatNewPartialFailure("direct_messages", turn+1, status, int(budget.calls.Load()-dispatchesBefore), round.Terminal, m4CompatToolClass(call.Name), m4CompatFailureCategory(err))
				return batch, errors.New(m4CompatFailureCategory(err))
			}
			result, err := m4CompatToolResult(call)
			if err != nil {
				status := resp.StatusCode
				batch.Failure = m4CompatNewPartialFailure("direct_messages", turn+1, status, int(budget.calls.Load()-dispatchesBefore), round.Terminal, m4CompatToolClass(call.Name), m4CompatFailureCategory(err))
				return batch, err
			}
			ordinal := len(batch.Links)/2 + 1
			batch.Links = append(batch.Links, fmt.Sprintf("call_%d", ordinal), fmt.Sprintf("result_for_call_%d", ordinal))
			history = append(history,
				map[string]any{"role": "assistant", "content": round.Content},
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": call.ID, "content": result}}})
		} else if len(round.Calls) != 0 || round.Terminal != "end_turn" || round.Text == "" {
			status := resp.StatusCode
			toolClass := "other"
			if len(round.Calls) > 0 {
				toolClass = m4CompatToolClass(round.Calls[0].Name)
			}
			batch.Failure = m4CompatNewPartialFailure("direct_messages", turn+1, status, int(budget.calls.Load()-dispatchesBefore), round.Terminal, toolClass, "final_turn_missing")
			return batch, errors.New("direct_final_turn_missing")
		}
	}
	batch.Usage = m4CompatUsageAvailability(rounds)
	return batch, nil
}

func m4CompatResponsesRequest(t *testing.T, client *http.Client, fixture *translationLifecycleFixture, input []any, toolChoice any) (*http.Response, error) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": "client-model", "stream": true, "max_output_tokens": m4CompatMax, "temperature": 0, "input": input, "tools": []map[string]any{{"type": "function", "name": "weather", "description": "Get fixture weather", "parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]string{"type": "string"}}, "required": []string{"city"}, "additionalProperties": false}}, {"type": "function", "name": "clock", "description": "Get fixture clock", "parameters": map[string]any{"type": "object", "properties": map[string]any{"zone": map[string]string{"type": "string"}}, "required": []string{"zone"}, "additionalProperties": false}}}, "tool_choice": toolChoice})
	req, err := http.NewRequest(http.MethodPost, fixture.gateway.URL+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("gateway_request_build")
	}
	req.Header.Set("Authorization", "Bearer "+fixture.secret)
	req.Header.Set("Content-Type", "application/json")
	return client.Do(req)
}

func m4CompatRunResponsesTools(t *testing.T, fixture *translationLifecycleFixture, client *http.Client, budget *m4CompatCallBudget) (m4CompatToolBatch, error) {
	t.Helper()
	batch := m4CompatToolBatch{}
	dispatchesBefore := budget.calls.Load()
	history := []any{map[string]any{"type": "message", "role": "user", "content": "Use weather for Paris, then use clock for UTC in a separate tool turn. After both results, do not call any more tools; briefly summarize using only those results."}}
	var rounds []m4CompatRound
	for turn := range 3 {
		choice := any(map[string]any{"type": "function", "name": m4CompatExpectedTool(turn + 1)})
		if turn == 2 {
			choice = "auto"
		}
		resp, err := m4CompatResponsesRequest(t, client, fixture, history, choice)
		if err != nil {
			batch.Failure = m4CompatNewPartialFailure("gateway_responses_to_messages", turn+1, 0, int(budget.calls.Load()-dispatchesBefore), "missing", "other", "other")
			return batch, err
		}
		round, parseErr := m4CompatReadResponsesRound(resp.Body, resp.StatusCode)
		_ = resp.Body.Close()
		if parseErr != nil || resp.StatusCode != http.StatusOK {
			status := resp.StatusCode
			category := m4CompatFailureCategory(parseErr)
			if parseErr == nil {
				category = "other"
			}
			batch.Failure = m4CompatNewPartialFailure("gateway_responses_to_messages", turn+1, status, int(budget.calls.Load()-dispatchesBefore), round.Terminal, "other", category)
			if parseErr != nil {
				return batch, fmt.Errorf("gateway_responses_round_failed: %s", category)
			}
			return batch, fmt.Errorf("gateway_responses_http_status=%d", resp.StatusCode)
		}
		select {
		case finalized := <-fixture.finalized:
			if finalized.Outcome != core.OutcomeSucceeded {
				status := resp.StatusCode
				batch.Failure = m4CompatNewPartialFailure("gateway_responses_to_messages", turn+1, status, int(budget.calls.Load()-dispatchesBefore), round.Terminal, "other", "other")
				return batch, errors.New("gateway_tool_round_attempt_failed")
			}
		case <-time.After(m4CompatTimeout):
			status := resp.StatusCode
			batch.Failure = m4CompatNewPartialFailure("gateway_responses_to_messages", turn+1, status, int(budget.calls.Load()-dispatchesBefore), round.Terminal, "other", "other")
			return batch, errors.New("gateway_tool_round_finalization_missing")
		}
		rounds = append(rounds, round)
		batch.Requests = append(batch.Requests, m4CompatRequestRecord{Run: 1, Attempt: 1, Timeout: 30, Tokens: m4CompatMax, Status: resp.StatusCode, Stream: true, Path: "/v1/responses", Purpose: "tool_round"})
		if turn < 2 {
			if len(round.Calls) != 1 {
				status := resp.StatusCode
				batch.Failure = m4CompatNewPartialFailure("gateway_responses_to_messages", turn+1, status, int(budget.calls.Load()-dispatchesBefore), round.Terminal, "other", "tool_round_missing")
				return batch, errors.New("gateway_tool_round_missing")
			}
			call := round.Calls[0]
			if err := m4CompatValidateToolCall(call, m4CompatExpectedTool(turn+1)); err != nil {
				status := resp.StatusCode
				batch.Failure = m4CompatNewPartialFailure("gateway_responses_to_messages", turn+1, status, int(budget.calls.Load()-dispatchesBefore), round.Terminal, m4CompatToolClass(call.Name), m4CompatFailureCategory(err))
				return batch, errors.New(m4CompatFailureCategory(err))
			}
			result, err := m4CompatToolResult(call)
			if err != nil {
				status := resp.StatusCode
				batch.Failure = m4CompatNewPartialFailure("gateway_responses_to_messages", turn+1, status, int(budget.calls.Load()-dispatchesBefore), round.Terminal, m4CompatToolClass(call.Name), m4CompatFailureCategory(err))
				return batch, err
			}
			ordinal := len(batch.Links)/2 + 1
			batch.Links = append(batch.Links, fmt.Sprintf("call_%d", ordinal), fmt.Sprintf("result_for_call_%d", ordinal))
			for _, item := range round.OutputItems {
				history = append(history, item)
			}
			history = append(history, map[string]any{"type": "function_call_output", "call_id": call.ID, "output": result})
		} else if len(round.Calls) != 0 || round.Terminal != "response.completed" || round.Text == "" {
			status := resp.StatusCode
			toolClass := "other"
			if len(round.Calls) > 0 {
				toolClass = m4CompatToolClass(round.Calls[0].Name)
			}
			batch.Failure = m4CompatNewPartialFailure("gateway_responses_to_messages", turn+1, status, int(budget.calls.Load()-dispatchesBefore), round.Terminal, toolClass, "final_turn_missing")
			return batch, errors.New("gateway_final_turn_missing")
		}
	}
	batch.Usage = m4CompatUsageAvailability(rounds)
	return batch, nil
}

func m4CompatTranslatedCancellation(t *testing.T, endpoint, key string, client *http.Client, budget *m4CompatCallBudget, cancelled chan struct{}) int {
	t.Helper()
	beforeCalls := budget.calls.Load()
	doer := &m4CompatProviderDoer{endpoint: endpoint, key: key, client: client, budget: budget, cancelled: cancelled}
	fixture := newTranslationLifecycleFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), doer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := json.Marshal(map[string]any{"model": "client-model", "stream": true, "max_output_tokens": m4CompatMax, "input": "Reply with a brief harmless sentence.", "tools": []any{}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fixture.gateway.URL+"/v1/responses", bytes.NewReader(request))
	if err != nil {
		t.Fatal("cancellation_request_build")
	}
	req.Header.Set("Authorization", "Bearer "+fixture.secret)
	req.Header.Set("Content-Type", "application/json")
	response, err := fixture.gateway.Client().Do(req)
	if err != nil {
		t.Fatal("cancellation_request_start")
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		t.Fatalf("cancellation_http_status=%d", response.StatusCode)
	}
	// Cancel only after a text delta reached the client; remote compute
	// cancellation is recorded as unknown, never inferred from transport.
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	seenEvent := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			var event struct {
				Type string `json:"type"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event.Type == "response.output_text.delta" {
				seenEvent = true
				break
			}
		}
	}
	if err := scanner.Err(); err != nil && !seenEvent {
		cancel()
		_ = response.Body.Close()
		t.Fatal("cancellation_stream_read_failed")
	}
	if !seenEvent {
		cancel()
		_ = response.Body.Close()
		t.Fatal("cancellation_stream_event_missing")
	}
	cancel()
	_ = response.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(m4CompatTimeout):
		t.Fatal("provider_context_not_cancelled")
	}
	fixture.assertCancelledOnce(t)
	if budget.calls.Load()-beforeCalls != 1 {
		t.Fatalf("cancellation_provider_attempts=%d", budget.calls.Load()-beforeCalls)
	}
	return response.StatusCode
}

func m4CompatBatchArtifacts(dir string, direct, gateway m4CompatToolBatch, cancellationDispatches, cancellationStatus int) error {
	if len(direct.Requests) != 3 || len(gateway.Requests) != 3 || cancellationDispatches != 1 || cancellationStatus != http.StatusOK {
		return errors.New("batch_scenario_count")
	}
	source, err := os.ReadFile("m4_compatible_batch_test.go")
	if err != nil {
		return err
	}
	sum := sha256.Sum256(source)
	digest := hex.EncodeToString(sum[:])
	stamp := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	client := map[string]string{"name": "Go http.Client", "version": runtime.Version()}
	provider := map[string]string{"api": "anthropic-messages", "api_version": "2023-06-01"}
	limits := map[string]int{"timeout_seconds": 30, "max_output_tokens": m4CompatMax, "client_runs": 1}
	toRequests := func(records []m4CompatRequestRecord) []map[string]any {
		out := make([]map[string]any, 0, len(records))
		for _, r := range records {
			out = append(out, map[string]any{"run": r.Run, "attempt": r.Attempt, "timeout_seconds": r.Timeout, "max_output_tokens": r.Tokens, "status": r.Status, "stream": r.Stream, "api_path": r.Path, "purpose": r.Purpose})
		}
		return out
	}
	baseObs := func(batch m4CompatToolBatch, terminal string, dispatches int) map[string]any {
		return map[string]any{"terminal": terminal, "tool_links": batch.Links, "usage": batch.Usage, "target_dispatches": dispatches}
	}
	directDoc := map[string]any{"schema_version": 1, "leg": "direct_messages", "endpoint_profile": "compatible_endpoint", "test_only_model_override": true, "captured_at_utc": stamp, "source_revision": digest, "client": client, "gateway": nil, "provider": provider, "scenario": "two_rounds", "fixture_id": "m4-041-tool-two-rounds-v1", "backend_model": m4CompatModel, "limits": limits, "requests": toRequests(direct.Requests), "outcome": "completed", "observations": baseObs(direct, "message_stop", len(direct.Requests))}
	gwObs := baseObs(gateway, "response.completed", len(gateway.Requests)+cancellationDispatches)
	gwObs["cancellation"] = map[string]any{"client_cancelled": true, "provider_context_cancelled": true, "remote_compute": "unknown", "provider_dispatches": cancellationDispatches, "retries": 0, "accounting_finalizations": 1}
	gwRequests := append([]m4CompatRequestRecord(nil), gateway.Requests...)
	gwRequests = append(gwRequests, m4CompatRequestRecord{Run: 1, Attempt: 1, Timeout: 30, Tokens: m4CompatMax, Status: cancellationStatus, Stream: true, Path: "/v1/responses", Purpose: "cancellation"})
	gatewayDoc := map[string]any{"schema_version": 1, "leg": "gateway_responses_to_messages", "endpoint_profile": "compatible_endpoint", "test_only_model_override": true, "captured_at_utc": stamp, "source_revision": digest, "client": client, "gateway": map[string]string{"client_protocol": responsesProtocol, "route_model": m4CompatModel, "source_revision": digest}, "provider": provider, "scenario": "two_rounds", "fixture_id": "m4-041-tool-two-rounds-v1", "backend_model": m4CompatModel, "limits": limits, "requests": toRequests(gwRequests), "outcome": "completed", "observations": gwObs}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for name, doc := range map[string]any{"direct.json": directDoc, "gateway.json": gatewayDoc} {
		encoded, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), append(encoded, '\n'), 0600); err != nil {
			return err
		}
	}
	return nil
}

func TestM4CompatibleExecutedToolBatchTLSH2AndCancellationSettlement(t *testing.T) {
	var calls atomic.Int32
	var protocolMismatch atomic.Bool
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn := int(calls.Add(1))
		if r.ProtoMajor != 2 {
			protocolMismatch.Store(true)
		}
		var request struct {
			Model    string            `json:"model"`
			Max      int               `json:"max_tokens"`
			Stream   bool              `json:"stream"`
			Messages []json.RawMessage `json:"messages"`
			Tools    []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Choice json.RawMessage `json:"tool_choice"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != m4CompatModel || !m4CompatMockProviderRequestValid(turn, request.Max, request.Stream, request.Tools, request.Choice, request.Messages) {
			t.Errorf("executed_batch_provider_request_invalid")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, m4CompatFakeMessages(turn))
	}))
	backend.EnableHTTP2 = true
	backend.StartTLS()
	defer backend.Close()
	base, ok := backend.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("local_tls_transport_unavailable")
	}
	client, transport := m4CompatHTTPClient(base)
	defer transport.CloseIdleConnections()
	if transport.Proxy != nil || client.CheckRedirect == nil {
		t.Fatal("executed_batch_transport_security_changed")
	}
	budget := &m4CompatCallBudget{}
	direct, err := m4CompatRunDirectTools(client, backend.URL+"/v1/messages", "synthetic-offline-only", budget)
	if err != nil {
		t.Fatalf("offline_direct_tool_batch: %v", err)
	}
	provider := &m4CompatProviderDoer{endpoint: backend.URL + "/v1/messages", key: "synthetic-offline-only", client: client, budget: budget}
	fixture := newTranslationLifecycleFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), provider)
	gatewayClient := fixture.gateway.Client()
	gatewayClient.Timeout = m4CompatTimeout
	translated, err := m4CompatRunResponsesTools(t, fixture, gatewayClient, budget)
	if err != nil {
		t.Fatalf("offline_translated_tool_batch: %v", err)
	}
	if len(direct.Requests) != 3 || len(translated.Requests) != 3 || len(direct.Links) != 4 || len(translated.Links) != 4 || direct.Usage != "reported_or_unknown" || (translated.Usage != "reported_or_unknown" && translated.Usage != "unknown") {
		t.Fatalf("offline_tool_batch_evidence_mismatch direct=%+v gateway=%+v", direct, translated)
	}
	if calls.Load() != 6 || budget.calls.Load() != 6 || protocolMismatch.Load() {
		t.Fatalf("offline_tool_batch_transport calls=%d budget=%d h2_mismatch=%t", calls.Load(), budget.calls.Load(), protocolMismatch.Load())
	}

	started, cancelled := make(chan struct{}), make(chan struct{}, 1)
	var cancellationProtocolMismatch atomic.Bool
	cancelServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			cancellationProtocolMismatch.Store(true)
		}
		close(started)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"cancel now\"}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	cancelServer.EnableHTTP2 = true
	cancelServer.StartTLS()
	defer cancelServer.Close()
	cancelTransport := cancelServer.Client().Transport.(*http.Transport)
	cancelClient, cancelClone := m4CompatHTTPClient(cancelTransport)
	defer cancelClone.CloseIdleConnections()
	cancellationStatus := m4CompatTranslatedCancellation(t, cancelServer.URL+"/v1/messages", "synthetic-offline-only", cancelClient, budget, cancelled)
	select {
	case <-started:
	default:
		t.Fatal("offline_cancellation_upstream_not_started")
	}
	if budget.calls.Load() != 7 {
		t.Fatalf("offline_batch_calls=%d", budget.calls.Load())
	}
	if cancellationProtocolMismatch.Load() {
		t.Fatal("offline_cancellation_did_not_use_http2")
	}
	if dir := os.Getenv("PESTIROUTE_M4_COMPAT_BATCH_FIXTURE_DIR"); dir != "" {
		if err := m4CompatBatchArtifacts(dir, direct, translated, 1, cancellationStatus); err != nil {
			t.Fatal("offline_batch_artifact_write")
		}
	}
}

func TestM4CompatiblePartialFailureIsSanitized(t *testing.T) {
	private := "PRIVATE_NAME arg-secret call-private response-secret key-secret"
	status := http.StatusOK
	failure := m4CompatNewPartialFailure("direct_messages", 1, status, 1, "tool_use", m4CompatToolClass(private), "unexpected_tool_name")
	failure.TestDurationMS = 37
	dir := t.TempDir()
	if err := m4CompatWritePartialFailure(dir, failure); err != nil {
		t.Fatal("partial_failure_write")
	}
	data, err := os.ReadFile(filepath.Join(dir, "partial-failure.json"))
	if err != nil {
		t.Fatal("partial_failure_read")
	}
	for _, marker := range []string{"PRIVATE_NAME", "arg-secret", "call-private", "response-secret", "key-secret"} {
		if bytes.Contains(data, []byte(marker)) {
			t.Fatal("private_data_in_partial_failure")
		}
	}
	if !bytes.Contains(data, []byte(`"http_status": 200`)) || !bytes.Contains(data, []byte(`"test_duration_ms": 37`)) || !bytes.Contains(data, []byte(`"provider_dispatches": 1`)) || !bytes.Contains(data, []byte(`"tool_class": "other"`)) {
		t.Fatal("partial_failure_metadata_missing")
	}
	bad := *failure
	bad.Terminal = private
	if err := m4CompatWritePartialFailure(dir, &bad); err == nil {
		t.Fatal("partial_failure_accepted_unallowlisted_terminal")
	}
}

func TestM4CompatibleToolNamesOrderAndArgumentsAreExact(t *testing.T) {
	tests := []struct {
		label, name, expected, input string
		ok                           bool
	}{
		{"weather exact", "weather", "weather", `{"city":"Paris"}`, true},
		{"clock exact", "clock", "clock", `{"zone":"UTC"}`, true},
		{"prefixed weather", "namespace.weather", "weather", `{"city":"Paris"}`, false},
		{"unknown tool", "forecast", "weather", `{"city":"Paris"}`, false},
		{"wrong expected order", "clock", "weather", `{"zone":"UTC"}`, false},
		{"wrong arguments", "weather", "weather", `{"city":"London"}`, false},
		{"extra arguments", "weather", "weather", `{"city":"Paris","zone":"UTC"}`, false},
		{"duplicate arguments", "weather", "weather", `{"city":"Paris","city":"Paris"}`, false},
	}
	for _, test := range tests {
		t.Run(test.label, func(t *testing.T) {
			call := m4CompatToolCall{ID: "must-not-be-logged", Name: test.name, Input: json.RawMessage(test.input)}
			err := m4CompatValidateToolCall(call, test.expected)
			if (err == nil) != test.ok {
				t.Fatal("tool name/order/arguments acceptance mismatch")
			}
			if !test.ok && m4CompatToolClass(call.Name) != "other" && call.Name != "weather" && call.Name != "clock" {
				t.Fatal("non-allowlisted tool name classification")
			}
		})
	}
	if !m4CompatChoiceMatches(json.RawMessage(`{"type":"tool","name":"weather"}`), 1) || !m4CompatChoiceMatches(json.RawMessage(`{"type":"tool","name":"clock"}`), 2) || !m4CompatChoiceMatches(json.RawMessage(`{"type":"auto"}`), 3) {
		t.Fatal("explicit per-turn named tool choice not recognized")
	}
	if m4CompatChoiceMatches(json.RawMessage(`{"type":"tool","name":"namespace.weather"}`), 1) || m4CompatChoiceMatches(json.RawMessage(`"none"`), 3) {
		t.Fatal("prefixed tool choice or final none shape was accepted")
	}
}

func TestM4CompatibleFinalTurnKeepsToolsAndAutoChoice(t *testing.T) {
	messages := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"use the tools"}`),
		json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"fixture-weather","name":"weather","input":{"city":"Paris"}}]}`),
		json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"fixture-weather","content":"result"}]}`),
		json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"fixture-clock","name":"clock","input":{"zone":"UTC"}}]}`),
		json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"fixture-clock","content":"result"}]}`),
	}
	tools := []struct {
		Name string `json:"name"`
	}{{Name: "weather"}, {Name: "clock"}}
	if m4CompatMockProviderRequestValid(3, m4CompatMax, true, nil, json.RawMessage(`"none"`), messages) {
		t.Fatal("previous none/tools-null final request shape was accepted")
	}
	if !m4CompatMockProviderRequestValid(3, m4CompatMax, true, tools, json.RawMessage(`{"type":"auto"}`), messages) || !m4CompatMockProviderRequestValid(6, m4CompatMax, true, tools, json.RawMessage(`{"type":"auto"}`), messages) {
		t.Fatal("correct final auto choice with declared tools/history was rejected")
	}
}

func TestM4CompatibleFinalExtraToolFailsWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn := int(calls.Add(1))
		var request struct {
			Model    string            `json:"model"`
			Max      int               `json:"max_tokens"`
			Stream   bool              `json:"stream"`
			Messages []json.RawMessage `json:"messages"`
			Tools    []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Choice json.RawMessage `json:"tool_choice"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != m4CompatModel || !m4CompatMockProviderRequestValid(turn, request.Max, request.Stream, request.Tools, request.Choice, request.Messages) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		mockTurn := (turn-1)%3 + 1
		if mockTurn == 3 {
			mockTurn = 1 // An extra declared tool call on the final auto-choice turn.
		}
		_, _ = io.WriteString(w, m4CompatFakeMessages(mockTurn))
	}))
	backend.EnableHTTP2 = true
	backend.StartTLS()
	defer backend.Close()
	client, transport := m4CompatHTTPClient(backend.Client().Transport.(*http.Transport))
	defer transport.CloseIdleConnections()
	budget := &m4CompatCallBudget{}

	direct, err := m4CompatRunDirectTools(client, backend.URL+"/v1/messages", "synthetic-offline-only", budget)
	if err == nil || err.Error() != "direct_final_turn_missing" || direct.Failure == nil || direct.Failure.Turn != 3 || direct.Failure.ProviderDispatches != 3 || direct.Failure.Category != "final_turn_missing" || direct.Failure.HTTPStatus == nil || *direct.Failure.HTTPStatus != http.StatusOK {
		t.Fatal("direct final extra tool was not reported truthfully")
	}

	provider := &m4CompatProviderDoer{endpoint: backend.URL + "/v1/messages", key: "synthetic-offline-only", client: client, budget: budget}
	fixture := newTranslationLifecycleFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), provider)
	translated, err := m4CompatRunResponsesTools(t, fixture, fixture.gateway.Client(), budget)
	if err == nil || err.Error() != "gateway_final_turn_missing" || translated.Failure == nil || translated.Failure.Turn != 3 || translated.Failure.ProviderDispatches != 3 || translated.Failure.Category != "final_turn_missing" || translated.Failure.HTTPStatus == nil || *translated.Failure.HTTPStatus != http.StatusOK {
		t.Fatal("translated final extra tool was not reported truthfully")
	}
	if calls.Load() != 6 || budget.calls.Load() != 6 {
		t.Fatal("unexpected retry after final extra tool")
	}
}

func TestM4CompatibleUnexpectedToolsStopWithoutAnotherDispatch(t *testing.T) {
	cases := []struct {
		label, name, input, category string
	}{
		{"prefixed", "namespace.weather", `\"city\":\"Paris\"`, "unexpected_tool_name"},
		{"unknown", "PRIVATE_TOOL_NAME", `\"city\":\"Paris\"`, "unexpected_tool_name"},
		{"invalid_arguments", "weather", `\"city\":\"PRIVATE_ARG\"`, "unexpected_tool_arguments"},
	}
	for _, test := range cases {
		t.Run(test.label, func(t *testing.T) {
			var calls atomic.Int32
			backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				body := strings.Replace(m4CompatFakeMessages(1), `"name":"weather"`, `"name":"`+test.name+`"`, 1)
				body = strings.Replace(body, `\"city\":\"Paris\"`, test.input, 1)
				_, _ = io.WriteString(w, body)
			}))
			backend.EnableHTTP2 = true
			backend.StartTLS()
			defer backend.Close()
			client, transport := m4CompatHTTPClient(backend.Client().Transport.(*http.Transport))
			defer transport.CloseIdleConnections()
			budget := &m4CompatCallBudget{}
			batch, err := m4CompatRunDirectTools(client, backend.URL+"/v1/messages", "synthetic-offline-only", budget)
			if err == nil || err.Error() != test.category || calls.Load() != 1 || budget.calls.Load() != 1 {
				t.Fatalf("unexpected tool handling mismatch err=%v calls=%d budget=%d", err, calls.Load(), budget.calls.Load())
			}
			wantClass := "other"
			if test.name == "weather" {
				wantClass = "weather"
			}
			if batch.Failure == nil || batch.Failure.ToolClass != wantClass || batch.Failure.ProviderDispatches != 1 || batch.Failure.HTTPStatus == nil || *batch.Failure.HTTPStatus != http.StatusOK {
				t.Fatal("unexpected tool failure diagnostics incomplete")
			}
			dir := t.TempDir()
			if err := m4CompatWritePartialFailure(dir, batch.Failure); err != nil {
				t.Fatal("partial failure write")
			}
			diagnostic, err := os.ReadFile(filepath.Join(dir, "partial-failure.json"))
			if err != nil {
				t.Fatal("partial failure read")
			}
			for _, private := range []string{"namespace.weather", "PRIVATE_TOOL_NAME", "PRIVATE_ARG", "fixture-weather", "fixture", "synthetic-offline-only"} {
				if bytes.Contains(diagnostic, []byte(private)) {
					t.Fatal("raw response content in partial failure")
				}
			}
		})
	}
}

func m4CompatFakeMessages(turn int) string {
	usage := `"usage":{"input_tokens":9}`
	outputUsage := `,"usage":{"output_tokens":2}`
	if turn == 1 || turn == 4 {
		usage = `"usage":{"input_tokens":7}`
	}
	if turn == 3 || turn == 6 {
		usage = `"usage":{}`
		outputUsage = ""
	}
	prefix := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{" + usage + "}}\n\n"
	switch turn {
	case 1, 4:
		return prefix + "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"fixture-weather\",\"name\":\"weather\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"Paris\\\"}\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}" + outputUsage + "}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	case 2, 5:
		return prefix + "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"fixture-clock\",\"name\":\"clock\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"zone\\\":\\\"UTC\\\"}\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}" + outputUsage + "}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	case 3, 6:
		return prefix + "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Weather and time recorded.\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	default:
		return ""
	}
}

type m4CompatChunkReader struct {
	data []byte
	size int
}

func (r *m4CompatChunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), min(len(r.data), r.size))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

type m4CompatReadError struct {
	data []byte
	read bool
}

func (r *m4CompatReadError) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		return copy(p, r.data), errors.New("synthetic_read_error")
	}
	return 0, errors.New("synthetic_read_error")
}

func TestM4CompatibleMessagesSSEEvents(t *testing.T) {
	terminal := ": heartbeat\r\nevent: ping\r\ndata: {\"type\":\"ping\"}\r\n\r\nevent: message_delta\r\ndata:{\"type\":\r\ndata: \"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\r\n\r\nevent: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\ndata: [DONE]\r\n\r\n"
	chunked := &m4CompatChunkReader{data: []byte(terminal), size: 1}
	round, err := m4CompatReadMessagesRound(chunked, http.StatusOK)
	if err != nil || round.Terminal != "end_turn" {
		t.Fatalf("chunked multiline message_stop parse: round=%+v err=%v", round, err)
	}

	afterTerminal := &m4CompatReadError{data: []byte("data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")}
	round, err = m4CompatReadMessagesRound(afterTerminal, http.StatusOK)
	if err != nil || round.Terminal != "end_turn" {
		t.Fatalf("terminal should stop before subsequent read error: round=%+v err=%v", round, err)
	}
}

func TestM4CompatibleMessagesSSERejectsMalformedOrMissingTerminal(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "done is not terminal", body: "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: [DONE]\n\n", want: "messages_event_json"},
		{name: "truncated before stop", body: "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n", want: "messages_terminal_missing"},
		{name: "malformed event", body: "data: {\"type\":\n\n", want: "messages_event_json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := m4CompatReadMessagesRound(strings.NewReader(test.body), http.StatusOK)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %s", err, test.want)
			}
		})
	}
}

func TestM4CompatibleExecutedTransportRefusesRedirect(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client, transport := m4CompatHTTPClient(nil)
	defer transport.CloseIdleConnections()
	resp, err := client.Get(redirect.URL)
	if err != nil {
		t.Fatal("redirect_fixture_request")
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || targetCalls.Load() != 0 {
		t.Fatalf("redirect_followed status=%d target_calls=%d", resp.StatusCode, targetCalls.Load())
	}
}
