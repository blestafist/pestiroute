package anthropic

import (
	"encoding/json"
	"fmt"

	"github.com/blestafist/pestiroute/internal/core"
)

type messagesRequest struct {
	System   []systemTextBlock `json:"system,omitempty"`
	Messages []textMessage     `json:"messages"`
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
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return messagesRequest{}, invalidTranslation("Invalid request body")
	}
	var out messagesRequest
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
