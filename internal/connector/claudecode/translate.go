package claudecode

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/blestafist/pestiroute/internal/core"
)

const messagesURL = "https://api.anthropic.com/v1/messages?beta=true"

type messagesRequest struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	Stream      bool            `json:"stream"`
	System      []textBlock     `json:"system,omitempty"`
	Messages    []message       `json:"messages"`
	Tools       []tool          `json:"tools,omitempty"`
	ToolChoice  *toolChoice     `json:"tool_choice,omitempty"`
	Temperature json.RawMessage `json:"temperature,omitempty"`
}

type message struct {
	Role    string      `json:"role"`
	Content []textBlock `json:"content"`
}
type textBlock struct {
	Type   string          `json:"type"`
	Text   string          `json:"text,omitempty"`
	Source *imageSource    `json:"source,omitempty"`
	Name   string          `json:"name,omitempty"`
	Input  json.RawMessage `json:"input_schema,omitempty"`
}
type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}
type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}
type toolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type translatedRequest struct {
	URL    string
	Header http.Header
	Body   []byte
}

// translateRequest only constructs the offline wire candidate; it never dispatches.
func translateRequest(req core.ExecutionRequest, accessToken string, beta string) (translatedRequest, *core.GatewayError) {
	if req.Payload.Protocol != protocol || req.Payload.ContentType != "application/json" && !strings.HasPrefix(req.Payload.ContentType, "application/json;") {
		return translatedRequest{}, unsupported("Unsupported request protocol or content type")
	}
	fields, err := requestFields(req.Payload.Body)
	if err != nil {
		return translatedRequest{}, unsupported("Invalid or unsupported Responses request")
	}
	for key := range fields {
		switch key {
		case "model", "input", "instructions", "tools", "tool_choice", "max_output_tokens", "temperature", "stream":
		default:
			return translatedRequest{}, unsupported("Unsupported Responses request field")
		}
	}
	var model string
	if json.Unmarshal(fields["model"], &model) != nil || model == "" || len(fields["model"]) == 0 {
		return translatedRequest{}, unsupported("A model is required")
	}
	out := messagesRequest{Model: model, Stream: true, MaxTokens: 4096}
	if raw := fields["max_output_tokens"]; len(raw) != 0 {
		if json.Unmarshal(raw, &out.MaxTokens) != nil || out.MaxTokens < 1 {
			return translatedRequest{}, unsupported("Invalid max_output_tokens")
		}
	}
	if raw := fields["stream"]; len(raw) != 0 && string(raw) != "true" {
		return translatedRequest{}, unsupported("Streaming is required")
	}
	if raw := fields["temperature"]; len(raw) != 0 {
		var v float64
		if string(raw) == "null" || json.Unmarshal(raw, &v) != nil || v < 0 || v > 1 {
			return translatedRequest{}, unsupported("Invalid temperature")
		}
		out.Temperature = bytes.Clone(raw)
	}
	if raw := fields["instructions"]; len(raw) != 0 {
		var v string
		if json.Unmarshal(raw, &v) != nil || v == "" {
			return translatedRequest{}, unsupported("Invalid instructions")
		}
		out.System = append(out.System, textBlock{Type: "text", Text: v})
	}
	if raw := fields["input"]; len(raw) == 0 || translateInput(raw, &out) != nil {
		return translatedRequest{}, unsupported("Unsupported Responses input")
	}
	if raw := fields["tools"]; len(raw) != 0 && translateTools(raw, &out) != nil {
		return translatedRequest{}, unsupported("Unsupported tool definition")
	}
	if raw := fields["tool_choice"]; len(raw) != 0 && translateChoice(raw, &out) != nil {
		return translatedRequest{}, unsupported("Unsupported tool choice")
	}
	body, err := json.Marshal(out)
	if err != nil {
		return translatedRequest{}, unsupported("Invalid Responses request")
	}
	h := make(http.Header)
	h.Set("anthropic-version", "2023-06-01")
	if beta == "" || strings.ContainsAny(beta, "\r\n") || accessToken == "" || strings.ContainsAny(accessToken, "\r\n") {
		return translatedRequest{}, unsupported("Scoped Messages credentials and beta configuration are required")
	}
	h.Set("anthropic-beta", beta)
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("User-Agent", "PestiRoute")
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	return translatedRequest{URL: messagesURL, Header: h, Body: body}, nil
}

func requestFields(raw []byte) (map[string]json.RawMessage, error) {
	if !uniqueJSON(raw) {
		return nil, fmt.Errorf("malformed or ambiguous JSON")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, fmt.Errorf("expected object")
	}
	f := make(map[string]json.RawMessage)
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil, err
		}
		k, ok := t.(string)
		if !ok {
			return nil, fmt.Errorf("invalid key")
		}
		if _, exists := f[k]; exists {
			return nil, fmt.Errorf("duplicate key")
		}
		var v json.RawMessage
		if err := d.Decode(&v); err != nil {
			return nil, err
		}
		f[k] = v
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("trailing data")
	}
	return f, nil
}

func uniqueJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func() bool
	value = func() bool {
		t, err := d.Token()
		if err != nil {
			return false
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return false
				}
				name, ok := key.(string)
				if !ok || seen[name] || !value() {
					return false
				}
				seen[name] = true
			}
			end, err := d.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for d.More() {
				if !value() {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	return value() && d.Decode(new(any)) == io.EOF
}

func translateInput(raw json.RawMessage, out *messagesRequest) error {
	var text string
	if json.Unmarshal(raw, &text) == nil && text != "" {
		out.Messages = []message{{Role: "user", Content: []textBlock{{Type: "text", Text: text}}}}
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return fmt.Errorf("invalid input")
	}
	for _, item := range items {
		var f map[string]json.RawMessage
		if json.Unmarshal(item, &f) != nil || f == nil {
			return fmt.Errorf("invalid item")
		}
		var typ string
		if json.Unmarshal(f["type"], &typ) != nil {
			return fmt.Errorf("missing type")
		}
		switch typ {
		case "message":
			for k := range f {
				if k != "type" && k != "role" && k != "content" {
					return fmt.Errorf("unsupported message field")
				}
			}
			var role string
			if json.Unmarshal(f["role"], &role) != nil {
				return fmt.Errorf("role")
			}
			if role == "developer" {
				var s string
				if json.Unmarshal(f["content"], &s) != nil {
					return fmt.Errorf("developer content")
				}
				out.System = append(out.System, textBlock{Type: "text", Text: s})
				continue
			}
			if role != "user" && role != "assistant" {
				return fmt.Errorf("role")
			}
			blocks, err := contentBlocks(f["content"])
			if err != nil {
				return err
			}
			out.Messages = append(out.Messages, message{Role: role, Content: blocks})
		default:
			return fmt.Errorf("unsupported input item")
		}
	}
	if len(out.Messages) == 0 {
		return fmt.Errorf("empty history")
	}
	return nil
}

func contentBlocks(raw json.RawMessage) ([]textBlock, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return []textBlock{{Type: "text", Text: s}}, nil
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil || len(parts) == 0 {
		return nil, fmt.Errorf("content")
	}
	out := make([]textBlock, 0, len(parts))
	for _, p := range parts {
		var f map[string]json.RawMessage
		if json.Unmarshal(p, &f) != nil {
			return nil, fmt.Errorf("part")
		}
		var typ string
		_ = json.Unmarshal(f["type"], &typ)
		switch typ {
		case "input_text", "output_text":
			for k := range f {
				if k != "type" && k != "text" {
					return nil, fmt.Errorf("unknown text field")
				}
			}
			var text string
			if json.Unmarshal(f["text"], &text) != nil || text == "" {
				return nil, fmt.Errorf("text")
			}
			out = append(out, textBlock{Type: "text", Text: text})
		case "input_image":
			for k := range f {
				if k != "type" && k != "image_url" {
					return nil, fmt.Errorf("image field")
				}
			}
			var imageURL string
			if json.Unmarshal(f["image_url"], &imageURL) != nil {
				return nil, fmt.Errorf("image")
			}
			media, data, ok := strings.Cut(strings.TrimPrefix(imageURL, "data:"), ";base64,")
			if !ok || !strings.HasPrefix(imageURL, "data:") || (media != "image/jpeg" && media != "image/png" && media != "image/gif" && media != "image/webp") {
				return nil, fmt.Errorf("image URL")
			}
			decoded, err := base64.StdEncoding.DecodeString(data)
			if err != nil || len(decoded) == 0 {
				return nil, fmt.Errorf("image data")
			}
			out = append(out, textBlock{Type: "image", Source: &imageSource{Type: "base64", MediaType: media, Data: data}})
		default:
			return nil, fmt.Errorf("unsupported content")
		}
	}
	return out, nil
}

func translateTools(raw json.RawMessage, out *messagesRequest) error {
	var input []json.RawMessage
	if json.Unmarshal(raw, &input) != nil {
		return fmt.Errorf("tools")
	}
	seen := make(map[string]bool, len(input))
	for _, r := range input {
		var f map[string]json.RawMessage
		if json.Unmarshal(r, &f) != nil {
			return fmt.Errorf("tool")
		}
		for k := range f {
			if k != "type" && k != "name" && k != "description" && k != "parameters" {
				return fmt.Errorf("tool field")
			}
		}
		var typ, name, desc string
		if json.Unmarshal(f["type"], &typ) != nil || json.Unmarshal(f["name"], &name) != nil || typ != "function" || name == "" || len(f["parameters"]) == 0 {
			return fmt.Errorf("tool definition")
		}
		if seen[name] {
			return fmt.Errorf("duplicate tool name")
		}
		seen[name] = true
		if rawDescription, exists := f["description"]; exists && json.Unmarshal(rawDescription, &desc) != nil {
			return fmt.Errorf("tool description")
		}
		var schema map[string]json.RawMessage
		if json.Unmarshal(f["parameters"], &schema) != nil || schema == nil {
			return fmt.Errorf("schema")
		}
		out.Tools = append(out.Tools, tool{Name: name, Description: desc, InputSchema: bytes.Clone(f["parameters"])})
	}
	return nil
}

func translateChoice(raw json.RawMessage, out *messagesRequest) error {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			out.ToolChoice = &toolChoice{Type: "auto"}
		case "required":
			out.ToolChoice = &toolChoice{Type: "any"}
		case "none":
			return fmt.Errorf("Messages has no lossless tool_choice none mapping")
		default:
			return fmt.Errorf("choice")
		}
		return nil
	}
	var f map[string]json.RawMessage
	if json.Unmarshal(raw, &f) != nil || f == nil {
		return fmt.Errorf("choice")
	}
	for k := range f {
		if k != "type" && k != "name" {
			return fmt.Errorf("unknown choice field")
		}
	}
	var kind, name string
	if json.Unmarshal(f["type"], &kind) != nil || json.Unmarshal(f["name"], &name) != nil || kind != "function" || name == "" {
		return fmt.Errorf("choice")
	}
	for _, t := range out.Tools {
		if t.Name == name {
			out.ToolChoice = &toolChoice{Type: "tool", Name: name}
			return nil
		}
	}
	return fmt.Errorf("unknown tool")
}

func unsupported(message string) *core.GatewayError {
	return &core.GatewayError{Code: "unsupported_feature", Category: core.CategoryUnsupportedFeature, Message: message}
}
