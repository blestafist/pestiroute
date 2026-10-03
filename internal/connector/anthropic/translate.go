package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"

	"github.com/blestafist/pestiroute/internal/core"
)

const backendModel = "claude-opus-5-5"

type messagesRequest struct {
	Model       string              `json:"model"`
	Stream      bool                `json:"stream"`
	MaxTokens   int                 `json:"max_tokens"`
	Temperature *json.Number        `json:"temperature,omitempty"`
	System      []systemTextBlock   `json:"system,omitempty"`
	Tools       []messagesTool      `json:"tools,omitempty"`
	ToolChoice  *messagesToolChoice `json:"tool_choice,omitempty"`
	Messages    []textMessage       `json:"messages"`
}

type messagesToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

type messagesTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type systemTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type textMessage struct {
	Role    string      `json:"role"`
	Content []textBlock `json:"content"`
}

type textBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   *string         `json:"content,omitempty"`
}

func translateRequest(body []byte) (messagesRequest, *core.GatewayError) {
	fields, err := topLevelFields(body)
	if err != nil {
		return messagesRequest{}, invalidTranslation("Invalid request body")
	}
	for name := range fields {
		switch name {
		case "model", "input", "instructions", "stream", "max_output_tokens", "temperature", "store", "include", "tools", "tool_choice", "parallel_tool_calls":
		default:
			return messagesRequest{}, invalidTranslation("Unsupported request field")
		}
	}
	var out messagesRequest
	if json.Unmarshal(fields["model"], &out.Model) != nil || out.Model == "" || string(fields["model"]) == "null" {
		return messagesRequest{}, invalidTranslation("Invalid model")
	}
	// The client model is validated against the configured route in Execute;
	// Anthropic receives only the explicitly bound backend model.
	out.Model = backendModel
	if raw, ok := fields["stream"]; !ok || string(raw) != "true" {
		return messagesRequest{}, invalidTranslation("Streaming is required")
	}
	out.Stream = true
	out.MaxTokens = 4096
	if raw, ok := fields["max_output_tokens"]; ok {
		number, ok := jsonNumber(raw)
		value, valid := exactRational(number)
		if !ok || !valid {
			return messagesRequest{}, invalidTranslation("Invalid max_output_tokens")
		}
		if !value.IsInt() || value.Sign() <= 0 || value.Cmp(big.NewRat(4096, 1)) > 0 {
			return messagesRequest{}, invalidTranslation("Invalid max_output_tokens")
		}
		out.MaxTokens = int(value.Num().Int64())
	}
	if raw, ok := fields["temperature"]; ok {
		number, ok := jsonNumber(raw)
		value, valid := exactRational(number)
		if !ok || !valid || value.Sign() < 0 || value.Cmp(big.NewRat(1, 1)) > 0 {
			return messagesRequest{}, invalidTranslation("Invalid temperature")
		}
		out.Temperature = &number
	}
	if raw, ok := fields["store"]; ok && string(raw) != "false" {
		return messagesRequest{}, invalidTranslation("Unsupported store value")
	}
	if raw, ok := fields["include"]; ok {
		var include []string
		if json.Unmarshal(raw, &include) != nil || include == nil || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
			return messagesRequest{}, invalidTranslation("Unsupported include value")
		}
	}
	if raw, ok := fields["tools"]; ok {
		tools, err := translateTools(raw)
		if err != nil {
			return messagesRequest{}, invalidTranslation("Invalid tools")
		}
		out.Tools = tools
	}
	parallelDisabled := false
	if raw, ok := fields["parallel_tool_calls"]; ok {
		if string(raw) == "false" {
			parallelDisabled = true
		} else if string(raw) != "true" {
			return messagesRequest{}, invalidTranslation("Invalid parallel_tool_calls")
		}
	}
	toolChoiceNone := false
	if raw, ok := fields["tool_choice"]; ok {
		choice, err := translateToolChoice(raw, out.Tools)
		if err != nil {
			return messagesRequest{}, invalidTranslation("Invalid tool_choice")
		}
		if choice != nil {
			choice.DisableParallelToolUse = parallelDisabled
			out.ToolChoice = choice
			if choice.Type == "none" {
				toolChoiceNone = true
				out.Tools = nil
				out.ToolChoice = nil
			}
		}
	} else if parallelDisabled && len(out.Tools) > 0 {
		out.ToolChoice = &messagesToolChoice{Type: "auto", DisableParallelToolUse: true}
	}
	if parallelDisabled && len(out.Tools) == 0 && !toolChoiceNone {
		return messagesRequest{}, invalidTranslation("parallel_tool_calls requires tools")
	}
	if raw, ok := fields["instructions"]; ok {
		var instructions string
		if json.Unmarshal(raw, &instructions) != nil || instructions == "" || string(raw) == "null" {
			return messagesRequest{}, invalidTranslation("Invalid instructions")
		}
		out.System = append(out.System, systemTextBlock{Type: "text", Text: instructions})
	}
	rawInput, ok := fields["input"]
	if !ok {
		return messagesRequest{}, invalidTranslation("Invalid input")
	}
	var input string
	if json.Unmarshal(rawInput, &input) == nil && string(rawInput) != "null" {
		if input == "" {
			return messagesRequest{}, invalidTranslation("Invalid input")
		}
		out.Messages = []textMessage{{Role: "user", Content: []textBlock{{Type: "text", Text: input}}}}
		return out, nil
	}
	var items []json.RawMessage
	if json.Unmarshal(rawInput, &items) != nil || items == nil || len(items) == 0 {
		return messagesRequest{}, invalidTranslation("Invalid input")
	}
	conversationStarted := false
	callIDs := make(map[string]struct{})
	pendingCallIDs := make(map[string]struct{})
	resultIDs := make(map[string]struct{})
	resultsStarted := false
	for _, raw := range items {
		var kind string
		if json.Unmarshal(rawType(raw), &kind) != nil {
			return messagesRequest{}, invalidTranslation("Invalid input item")
		}
		if kind == "function_call" || kind == "function_call_output" {
			if kind == "function_call" && len(pendingCallIDs) > 0 && resultsStarted {
				return messagesRequest{}, invalidTranslation("Ambiguous function history chronology")
			}
			item, callID, err := translateFunctionHistoryItem(raw, kind, callIDs, pendingCallIDs, resultIDs)
			if err != nil {
				return messagesRequest{}, invalidTranslation("Invalid function history")
			}
			role := "assistant"
			if kind == "function_call_output" {
				role = "user"
				resultsStarted = true
				delete(pendingCallIDs, callID)
				if len(pendingCallIDs) == 0 {
					resultsStarted = false
				}
			} else {
				pendingCallIDs[callID] = struct{}{}
			}
			appendHistoryBlock(&out.Messages, role, item)
			conversationStarted = true
			continue
		}
		if len(pendingCallIDs) > 0 {
			return messagesRequest{}, invalidTranslation("Ambiguous function history chronology")
		}
		item, err := object(raw, "type", "role", "content", "status", "annotations", "logprobs")
		if err != nil {
			return messagesRequest{}, invalidTranslation("Invalid input message")
		}
		var role string
		if json.Unmarshal(item["type"], &kind) != nil || kind != "message" ||
			json.Unmarshal(item["role"], &role) != nil {
			return messagesRequest{}, invalidTranslation("Invalid input message")
		}
		if status, exists := item["status"]; exists {
			if role != "assistant" {
				return messagesRequest{}, invalidTranslation("Invalid assistant status")
			}
			var value string
			if json.Unmarshal(status, &value) != nil || value != "completed" {
				return messagesRequest{}, invalidTranslation("Invalid assistant status")
			}
		}
		for _, name := range []string{"annotations", "logprobs"} {
			if raw, exists := item[name]; exists {
				if role != "assistant" {
					return messagesRequest{}, invalidTranslation("Unsupported message metadata")
				}
				var value []json.RawMessage
				if json.Unmarshal(raw, &value) != nil || value == nil || len(value) != 0 {
					return messagesRequest{}, invalidTranslation("Unsupported message metadata")
				}
			}
		}
		content, err := translateContent(item["content"], role)
		if err != nil {
			return messagesRequest{}, invalidTranslation("Unsupported message content")
		}
		switch role {
		case "developer":
			if conversationStarted {
				return messagesRequest{}, invalidTranslation("Developer messages must precede conversation history")
			}
			for _, block := range content {
				out.System = append(out.System, systemTextBlock{Type: "text", Text: block.Text})
			}
		case "user", "assistant":
			conversationStarted = true
			for _, block := range content {
				appendHistoryBlock(&out.Messages, role, block)
			}
		default:
			return messagesRequest{}, invalidTranslation("Unsupported message role")
		}
	}
	if len(out.Messages) == 0 {
		return messagesRequest{}, invalidTranslation("At least one user or assistant message is required")
	}
	return out, nil
}

func rawType(raw json.RawMessage) json.RawMessage {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil {
		return nil
	}
	return item["type"]
}

func translateFunctionHistoryItem(raw json.RawMessage, kind string, callIDs, pendingCallIDs, resultIDs map[string]struct{}) (textBlock, string, error) {
	if kind == "function_call" {
		if !uniqueJSONValue(raw) {
			return textBlock{}, "", fmt.Errorf("duplicate function call field")
		}
		item, err := object(raw, "type", "id", "call_id", "name", "arguments", "status")
		if err != nil {
			return textBlock{}, "", err
		}
		var id, callID, name, arguments string
		if rawID, exists := item["id"]; exists && (json.Unmarshal(rawID, &id) != nil || id == "") {
			return textBlock{}, "", fmt.Errorf("invalid function item id")
		}
		if status, exists := item["status"]; exists {
			var value string
			if json.Unmarshal(status, &value) != nil || value != "completed" {
				return textBlock{}, "", fmt.Errorf("function call is not completed")
			}
		}
		if json.Unmarshal(item["call_id"], &callID) != nil || callID == "" ||
			json.Unmarshal(item["name"], &name) != nil || !validToolName(name) ||
			json.Unmarshal(item["arguments"], &arguments) != nil || !uniqueJSONValue(json.RawMessage(arguments)) {
			return textBlock{}, "", fmt.Errorf("invalid function call")
		}
		var input map[string]json.RawMessage
		if json.Unmarshal([]byte(arguments), &input) != nil || input == nil {
			return textBlock{}, "", fmt.Errorf("function arguments must be an object")
		}
		if _, exists := callIDs[callID]; exists {
			return textBlock{}, "", fmt.Errorf("duplicate call id")
		}
		callIDs[callID] = struct{}{}
		return textBlock{Type: "tool_use", ID: callID, Name: name, Input: json.RawMessage(arguments)}, callID, nil
	}
	if !uniqueJSONValue(raw) {
		return textBlock{}, "", fmt.Errorf("duplicate function output field")
	}
	item, err := object(raw, "type", "id", "call_id", "output")
	if err != nil {
		return textBlock{}, "", err
	}
	var id, callID, output string
	if rawID, exists := item["id"]; exists && (json.Unmarshal(rawID, &id) != nil || id == "") {
		return textBlock{}, "", fmt.Errorf("invalid function output item id")
	}
	if json.Unmarshal(item["call_id"], &callID) != nil || callID == "" ||
		json.Unmarshal(item["output"], &output) != nil {
		return textBlock{}, "", fmt.Errorf("invalid function output")
	}
	if id != "" && id == callID {
		return textBlock{}, "", fmt.Errorf("output item id is not a call id")
	}
	if _, exists := callIDs[callID]; !exists {
		return textBlock{}, "", fmt.Errorf("orphan function output")
	}
	if _, exists := resultIDs[callID]; exists {
		return textBlock{}, "", fmt.Errorf("duplicate function output")
	}
	if _, exists := pendingCallIDs[callID]; !exists {
		return textBlock{}, "", fmt.Errorf("function output is not pending")
	}
	resultIDs[callID] = struct{}{}
	return textBlock{Type: "tool_result", ToolUseID: callID, Content: &output}, callID, nil
}

func appendHistoryBlock(messages *[]textMessage, role string, block textBlock) {
	if len(*messages) == 0 || (*messages)[len(*messages)-1].Role != role {
		*messages = append(*messages, textMessage{Role: role})
	}
	last := &(*messages)[len(*messages)-1]
	last.Content = append(last.Content, block)
}

func translateToolChoice(raw json.RawMessage, tools []messagesTool) (*messagesToolChoice, error) {
	if len(tools) == 0 || !uniqueJSONValue(raw) {
		return nil, fmt.Errorf("tool choice requires valid tools")
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		switch value {
		case "auto":
			return &messagesToolChoice{Type: "auto"}, nil
		case "required":
			return &messagesToolChoice{Type: "any"}, nil
		case "none":
			return &messagesToolChoice{Type: "none"}, nil
		default:
			return nil, fmt.Errorf("unknown tool choice")
		}
	}
	fields, err := object(raw, "type", "name")
	if err != nil {
		return nil, err
	}
	var kind, name string
	if json.Unmarshal(fields["type"], &kind) != nil || kind != "function" ||
		json.Unmarshal(fields["name"], &name) != nil || !validToolName(name) {
		return nil, fmt.Errorf("invalid named tool choice")
	}
	for _, tool := range tools {
		if tool.Name == name {
			return &messagesToolChoice{Type: "tool", Name: name}, nil
		}
	}
	return nil, fmt.Errorf("unknown tool name")
}

func translateTools(raw json.RawMessage) ([]messagesTool, error) {
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil || tools == nil {
		return nil, fmt.Errorf("expected tools array")
	}
	seen := make(map[string]struct{}, len(tools))
	out := make([]messagesTool, 0, len(tools))
	for _, rawTool := range tools {
		if !uniqueJSONValue(rawTool) {
			return nil, fmt.Errorf("duplicate tool field")
		}
		tool, err := object(rawTool, "type", "name", "description", "parameters", "strict")
		if err != nil {
			return nil, err
		}
		var kind string
		if json.Unmarshal(tool["type"], &kind) != nil || kind != "function" {
			return nil, fmt.Errorf("unsupported tool type")
		}
		var name string
		if json.Unmarshal(tool["name"], &name) != nil || !validToolName(name) {
			return nil, fmt.Errorf("invalid tool name")
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("duplicate tool name")
		}
		seen[name] = struct{}{}
		if strict, exists := tool["strict"]; exists {
			var value bool
			if json.Unmarshal(strict, &value) != nil || string(strict) == "null" || value {
				return nil, fmt.Errorf("strict schemas are unsupported")
			}
		}
		var description string
		if rawDescription, exists := tool["description"]; exists {
			if json.Unmarshal(rawDescription, &description) != nil || string(rawDescription) == "null" {
				return nil, fmt.Errorf("invalid tool description")
			}
		}
		parameters := tool["parameters"]
		if !validSchemaObject(parameters) {
			return nil, fmt.Errorf("parameters must be a valid schema object")
		}
		out = append(out, messagesTool{Name: name, Description: description, InputSchema: bytes.Clone(parameters)})
	}
	return out, nil
}

func validToolName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, char := range name {
		if !('a' <= char && char <= 'z') && !('A' <= char && char <= 'Z') &&
			!('0' <= char && char <= '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func validSchemaObject(raw json.RawMessage) bool {
	var schema map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &schema) != nil || schema == nil {
		return false
	}
	return uniqueJSONValue(raw)
}

// uniqueJSONValue rejects duplicate object keys recursively without interpreting
// schema keywords, so valid schema semantics remain opaque and unchanged.
func uniqueJSONValue(raw json.RawMessage) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var parse func() bool
	parse = func() bool {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return true
		}
		switch delim {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return false
				}
				name, ok := key.(string)
				if !ok {
					return false
				}
				if _, exists := seen[name]; exists || !parse() {
					return false
				}
				seen[name] = struct{}{}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for decoder.More() {
				if !parse() {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	if !parse() {
		return false
	}
	return decoder.Decode(new(any)) == io.EOF
}

func jsonNumber(raw json.RawMessage) (json.Number, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return "", false
	}
	number, ok := value.(json.Number)
	return number, ok
}

func exactRational(number json.Number) (*big.Rat, bool) {
	text := number.String()
	if index := strings.IndexAny(text, "eE"); index >= 0 {
		exponent, err := strconv.ParseInt(text[index+1:], 10, 32)
		if err != nil || exponent < -4096 || exponent > 4096 {
			return nil, false
		}
	}
	value, ok := new(big.Rat).SetString(text)
	return value, ok
}

// topLevelFields rejects duplicate top-level names while leaving nested object
// parsing behavior unchanged.
func topLevelFields(body []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("expected request object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("invalid request field")
		}
		if _, exists := fields[name]; exists {
			return nil, fmt.Errorf("duplicate request field")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		fields[name] = raw
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, fmt.Errorf("invalid request object")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("trailing request data")
	}
	return fields, nil
}

func translateContent(raw json.RawMessage, role string) ([]textBlock, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("missing content")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil && string(raw) != "null" {
		if text == "" || role == "assistant" {
			return nil, fmt.Errorf("empty content")
		}
		return []textBlock{{Type: "text", Text: text}}, nil
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil || parts == nil || len(parts) == 0 {
		return nil, fmt.Errorf("invalid content")
	}
	wantType := "input_text"
	if role == "assistant" {
		wantType = "output_text"
	} else if role != "user" && role != "developer" {
		return nil, fmt.Errorf("invalid role")
	}
	blocks := make([]textBlock, 0, len(parts))
	for _, part := range parts {
		fields, err := object(part, "type", "text")
		if err != nil {
			return nil, err
		}
		var kind, text string
		if json.Unmarshal(fields["type"], &kind) != nil || kind != wantType ||
			json.Unmarshal(fields["text"], &text) != nil || text == "" {
			return nil, fmt.Errorf("invalid text part")
		}
		blocks = append(blocks, textBlock{Type: "text", Text: text})
	}
	return blocks, nil
}

func object(raw json.RawMessage, allowed ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("expected object")
	}
	for name := range fields {
		found := false
		for _, candidate := range allowed {
			if name == candidate {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unsupported field")
		}
	}
	return fields, nil
}

func invalidTranslation(message string) *core.GatewayError {
	return &core.GatewayError{Code: "invalid_request", Category: core.CategoryInvalidRequest, Message: message}
}
