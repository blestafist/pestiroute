// Package responses implements the HTTP boundary for native Responses requests.
package responses

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"mime"
	"net/http"
	"strings"

	"github.com/blestafist/pestiroute/internal/core"
)

const (
	protocol = "openai.responses.v1"
	model    = "gpt-4.1-mini-2025-04-14"
)

func invalid(message string) *core.GatewayError {
	return &core.GatewayError{Code: "invalid_request", Category: core.CategoryInvalidRequest, Message: message}
}

func unsupported(message string) *core.GatewayError {
	return &core.GatewayError{Code: "unsupported_feature", Category: core.CategoryUnsupportedFeature, Message: message}
}

// Decode validates ingress before execution; maxBodyBytes and maxHeaderBytes
// come from validated startup configuration. The returned body is never rewritten.
func Decode(r *http.Request, maxBodyBytes, maxHeaderBytes int64) (core.ExecutionRequest, *core.GatewayError) {
	var empty core.ExecutionRequest
	if r == nil || r.Body == nil || maxBodyBytes <= 0 || maxHeaderBytes <= 0 {
		return empty, invalid("Invalid request")
	}
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return empty, unsupported("Unsupported request media type")
	}
	media, params, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || media != "application/json" || len(params) > 1 || (len(params) == 1 && !strings.EqualFold(params["charset"], "utf-8")) {
		return empty, unsupported("Unsupported request media type")
	}
	for _, encoding := range r.Header.Values("Content-Encoding") {
		if !strings.EqualFold(encoding, "identity") {
			return empty, unsupported("Unsupported content encoding")
		}
	}
	if r.ContentLength > maxBodyBytes {
		return empty, invalid("Request body too large")
	}
	headers := make(map[string][]string)
	hop := map[string]bool{"connection": true, "keep-alive": true, "proxy-connection": true, "transfer-encoding": true, "te": true, "trailer": true, "upgrade": true, "content-length": true, "content-encoding": true}
	for _, value := range r.Header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			hop[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	var headerBytes int64
	for name, values := range r.Header {
		headerBytes += int64(len(name))
		for _, value := range values {
			headerBytes += int64(len(value))
		}
		if headerBytes > maxHeaderBytes {
			return empty, invalid("Request headers too large")
		}
		lower := strings.ToLower(name)
		if hop[lower] || lower == "cookie" || lower == "cookie2" || lower == "set-cookie" || lower == "openai-organization" || lower == "openai-project" || strings.Contains(lower, "auth") || (lower != "idempotency-key" && strings.Contains(lower, "key")) || strings.Contains(lower, "secret") || strings.Contains(lower, "credential") || strings.Contains(lower, "token") || strings.HasPrefix(lower, "proxy-") {
			continue
		}
		headers[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
	readLimit := maxBodyBytes
	if readLimit < math.MaxInt64 {
		readLimit++
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, readLimit))
	if err != nil {
		return empty, invalid("Cannot read request body")
	}
	if int64(len(body)) > maxBodyBytes {
		return empty, invalid("Request body too large")
	}
	fields := make(map[string]json.RawMessage)
	dec := json.NewDecoder(bytes.NewReader(body))
	if first, err := dec.Token(); err != nil || first != json.Delim('{') {
		return empty, invalid("Invalid JSON request")
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return empty, invalid("Invalid JSON request")
		}
		name, ok := key.(string)
		if !ok {
			return empty, invalid("Invalid JSON request")
		}
		if _, duplicate := fields[name]; duplicate {
			return empty, invalid("Duplicate request field")
		}
		var value json.RawMessage
		if dec.Decode(&value) != nil {
			return empty, invalid("Invalid JSON request")
		}
		if !uniqueJSON(value) {
			return empty, invalid("Duplicate or invalid nested request field")
		}
		fields[name] = value
	}
	if last, err := dec.Token(); err != nil || last != json.Delim('}') {
		return empty, invalid("Invalid JSON request")
	}
	if dec.Decode(new(any)) != io.EOF {
		return empty, invalid("Invalid JSON request")
	}
	var selected string
	if json.Unmarshal(fields["model"], &selected) != nil || selected != model {
		return empty, invalid("Invalid model")
	}
	req := core.ExecutionRequest{Model: selected, Capabilities: make(map[core.Capability]struct{}), Metadata: core.RequestMetadata{Headers: headers}, Payload: core.RawPayload{Protocol: protocol, ContentType: "application/json", Body: body}}
	if raw, ok := fields["stream"]; ok {
		var value bool
		if json.Unmarshal(raw, &value) != nil || string(raw) == "null" {
			return empty, invalid("Invalid stream")
		}
		req.Metadata.Streaming = &value
		if value {
			req.Capabilities["llm.streaming"] = struct{}{}
		}
	}
	tools := false
	if raw, ok := fields["tools"]; ok {
		var value []json.RawMessage
		if json.Unmarshal(raw, &value) != nil || value == nil {
			return empty, invalid("Invalid tools")
		}
		tools = len(value) > 0
		if tools {
			for _, tool := range value {
				var descriptor struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(tool, &descriptor) != nil || descriptor.Type == "" {
					return empty, invalid("Invalid tool")
				}
			}
			req.Capabilities["llm.tools"] = struct{}{}
		}
	}
	if raw, ok := fields["parallel_tool_calls"]; ok {
		var value bool
		if json.Unmarshal(raw, &value) != nil || string(raw) == "null" {
			return empty, invalid("Invalid parallel_tool_calls")
		}
		if value && tools {
			req.Capabilities["llm.tools.parallel"] = struct{}{}
		}
	}
	if raw, ok := fields["reasoning"]; ok {
		var value map[string]json.RawMessage
		if json.Unmarshal(raw, &value) != nil || value == nil {
			return empty, invalid("Invalid reasoning")
		}
		if len(value) == 0 {
			return empty, unsupported("Unrecognized reasoning shape")
		}
		req.Capabilities["llm.reasoning"] = struct{}{}
	}
	if raw, ok := fields["text"]; ok {
		var text map[string]json.RawMessage
		if json.Unmarshal(raw, &text) != nil || text == nil {
			return empty, invalid("Invalid text")
		}
		if format, ok := text["format"]; ok {
			var spec struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(format, &spec) != nil || spec.Type == "" {
				return empty, invalid("Invalid text format")
			}
			switch spec.Type {
			case "text":
			case "json_schema", "json_object":
				req.Capabilities["llm.structured_output"] = struct{}{}
			default:
				return empty, unsupported("Unknown text format")
			}
		}
	}
	if raw, ok := fields["input"]; ok {
		if e := inspectInput(raw, req.Capabilities); e != nil {
			return empty, e
		}
	}
	return req, nil
}

// uniqueJSON checks opaque subtrees too: a later duplicate key must not
// change how the upstream interprets the bytes used for capability admission.
func uniqueJSON(raw json.RawMessage) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if !uniqueValue(dec) {
		return false
	}
	_, err := dec.Token()
	return err == io.EOF
}

func uniqueValue(dec *json.Decoder) bool {
	token, err := dec.Token()
	if err != nil {
		return false
	}
	switch token {
	case json.Delim('{'):
		seen := make(map[string]struct{})
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return false
			}
			name, ok := key.(string)
			if !ok {
				return false
			}
			if _, exists := seen[name]; exists {
				return false
			}
			seen[name] = struct{}{}
			if !uniqueValue(dec) {
				return false
			}
		}
		end, err := dec.Token()
		return err == nil && end == json.Delim('}')
	case json.Delim('['):
		for dec.More() {
			if !uniqueValue(dec) {
				return false
			}
		}
		end, err := dec.Token()
		return err == nil && end == json.Delim(']')
	case json.Delim('}'), json.Delim(']'):
		return false
	default:
		return true
	}
}

func inspectInput(raw json.RawMessage, capabilities map[core.Capability]struct{}) *core.GatewayError {
	var text string
	if json.Unmarshal(raw, &text) == nil && string(raw) != "null" {
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || items == nil {
		return invalid("Invalid input")
	}
	for _, item := range items {
		var object map[string]json.RawMessage
		if json.Unmarshal(item, &object) != nil || object == nil {
			return invalid("Invalid input item")
		}
		if rawType, ok := object["type"]; ok {
			var kind string
			if json.Unmarshal(rawType, &kind) != nil || kind == "" {
				return invalid("Invalid input item type")
			}
			switch kind {
			case "input_image", "image_url":
				capabilities["llm.vision"] = struct{}{}
			case "input_audio", "audio":
				capabilities["llm.audio"] = struct{}{}
			}
		}
		// Tool output may carry image parts in an output array instead of content.
		for _, field := range []string{"content", "output"} {
			content, ok := object[field]
			if !ok {
				continue
			}
			if json.Unmarshal(content, &text) == nil && string(content) != "null" {
				continue
			}
			var parts []json.RawMessage
			if json.Unmarshal(content, &parts) != nil || parts == nil {
				return invalid("Invalid input " + field)
			}
			for _, part := range parts {
				var p struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(part, &p) != nil || p.Type == "" {
					return invalid("Invalid input content item")
				}
				switch p.Type {
				case "input_image", "image_url":
					capabilities["llm.vision"] = struct{}{}
				case "input_audio", "audio":
					capabilities["llm.audio"] = struct{}{}
				case "input_text", "text", "output_text", "refusal", "input_file":
				default:
					// Unknown native content parts remain opaque.
				}
			}
		}
	}
	return nil
}
