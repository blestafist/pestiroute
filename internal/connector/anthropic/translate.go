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
	Model       string            `json:"model"`
	Stream      bool              `json:"stream"`
	MaxTokens   int               `json:"max_tokens"`
	Temperature *json.Number      `json:"temperature,omitempty"`
	System      []systemTextBlock `json:"system,omitempty"`
	Messages    []textMessage     `json:"messages"`
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
	Type string `json:"type"`
	Text string `json:"text"`
}

func translateRequest(body []byte) (messagesRequest, *core.GatewayError) {
	fields, err := topLevelFields(body)
	if err != nil {
		return messagesRequest{}, invalidTranslation("Invalid request body")
	}
	for name := range fields {
		switch name {
		case "model", "input", "instructions", "stream", "max_output_tokens", "temperature", "store", "include":
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
	for _, raw := range items {
		item, err := object(raw, "type", "role", "content", "status", "annotations", "logprobs")
		if err != nil {
			return messagesRequest{}, invalidTranslation("Invalid input message")
		}
		var kind, role string
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
			out.Messages = append(out.Messages, textMessage{Role: role, Content: content})
		default:
			return messagesRequest{}, invalidTranslation("Unsupported message role")
		}
	}
	if len(out.Messages) == 0 {
		return messagesRequest{}, invalidTranslation("At least one user or assistant message is required")
	}
	return out, nil
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
