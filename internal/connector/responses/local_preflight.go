package responses

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/blestafist/pestiroute/internal/core"
)

const (
	profileOllama = "ollama-v0.40.3"
	profileVLLM   = "vllm-v0.31.0"
	profileLlama  = "llama.cpp-v0.6.0"
)

func validLocalRuntimeProfile(profile string) bool {
	switch profile {
	case profileOllama, profileVLLM, profileLlama:
		return true
	default:
		return false
	}
}

// preflight interprets only the reviewed significant fields for a pinned local
// profile. Admitted requests continue through the opaque transport unchanged.
func (c *Connector) preflight(req core.ExecutionRequest, scope core.AttemptScope) *core.GatewayError {
	c.mu.Lock()
	profile := c.runtimeProfile
	capabilities := c.capabilitiesLocked(core.CapabilityScope{
		Protocol: req.Payload.Protocol, Mode: scope.Mode, Model: req.Model, AccountID: scope.AccountID,
	}).Values
	c.mu.Unlock()
	if !validLocalRuntimeProfile(profile) {
		return gatewayError("connector_unavailable", core.CategoryUnavailable, "Local runtime profile is unavailable")
	}
	if err := rejectDuplicateJSONKeys(req.Payload.Body); err != nil {
		return gatewayError("invalid_request", core.CategoryInvalidRequest, "Invalid Responses request")
	}
	var body map[string]json.RawMessage
	d := json.NewDecoder(bytes.NewReader(req.Payload.Body))
	if err := d.Decode(&body); err != nil || body == nil {
		return gatewayError("invalid_request", core.CategoryInvalidRequest, "Invalid Responses request")
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return gatewayError("invalid_request", core.CategoryInvalidRequest, "Invalid Responses request")
	}

	unsupported := func() *core.GatewayError {
		return gatewayError("unsupported_semantics", core.CategoryUnsupportedFeature, "Selected local runtime profile does not support a requested Responses feature")
	}
	switch profile {
	case profileOllama:
		if nonEmpty(body["previous_response_id"]) || nonEmpty(body["conversation"]) || truthy(body["background"]) || nonEmpty(body["include"]) || nonEmpty(body["tool_choice"]) {
			return unsupported()
		}
	case profileVLLM:
		// This connector has no local response-store enablement setting.
		if truthy(body["store"]) || truthy(body["background"]) || nonEmpty(body["previous_response_id"]) {
			return unsupported()
		}
	case profileLlama:
		if nonEmpty(body["previous_response_id"]) || nonEmpty(body["tool_choice"]) {
			return unsupported()
		}
	}

	if !validTextField(body["text"]) {
		return unsupported()
	}

	required := make(map[core.Capability]struct{}, len(req.Capabilities)+4)
	for capability := range req.Capabilities {
		required[capability] = struct{}{}
	}
	if hasFunctionTools(body["tools"]) || nonEmpty(body["tool_choice"]) {
		required["llm.tools"] = struct{}{}
	}
	if truthy(body["parallel_tool_calls"]) {
		required["llm.tools.parallel"] = struct{}{}
	}
	if nonEmpty(body["reasoning"]) {
		required["llm.reasoning"] = struct{}{}
	}
	if structuredOutputFormat(body["text"]) {
		required["llm.structured_output"] = struct{}{}
	}
	for modality := range inputModalities(body["input"]) {
		required[modality] = struct{}{}
	}
	for capability := range required {
		if capabilities[capability] != core.Supported {
			return unsupported()
		}
	}
	if profile == profileLlama && (hasInputFile(body["input"]) || hasNonFunctionTools(body["tools"])) {
		return unsupported()
	}
	return nil
}

func structuredOutputFormat(raw json.RawMessage) bool {
	var text struct {
		Format struct {
			Type string `json:"type"`
		} `json:"format"`
	}
	if json.Unmarshal(raw, &text) != nil || text.Format.Type == "" {
		return false
	}
	return text.Format.Type == "json_schema" || text.Format.Type == "json_object"
}

func validTextField(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var text map[string]json.RawMessage
	if json.Unmarshal(raw, &text) != nil || text == nil {
		return false
	}
	if format, ok := text["format"]; ok {
		var spec struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(format, &spec) != nil || (spec.Type != "text" && spec.Type != "json_schema" && spec.Type != "json_object") {
			return false
		}
	}
	return true
}

func nonEmpty(raw json.RawMessage) bool {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return true
	}
	switch value := value.(type) {
	case string:
		return value != ""
	case []any:
		return len(value) > 0
	case map[string]any:
		return len(value) > 0
	case bool:
		return value
	default:
		return true
	}
}

func truthy(raw json.RawMessage) bool { return nonEmpty(raw) }

func hasFunctionTools(raw json.RawMessage) bool {
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return false
	}
	for _, tool := range tools {
		var kind string
		_ = json.Unmarshal(tool["type"], &kind)
		if kind == "function" {
			return true
		}
	}
	return false
}

func hasNonFunctionTools(raw json.RawMessage) bool {
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return false
	}
	for _, tool := range tools {
		var kind string
		_ = json.Unmarshal(tool["type"], &kind)
		if kind != "function" {
			return true
		}
	}
	return false
}

func hasInputFile(raw json.RawMessage) bool {
	var input []map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil {
		return false
	}
	for _, item := range input {
		var kind string
		_ = json.Unmarshal(item["type"], &kind)
		if kind == "input_file" {
			return true
		}
	}
	return false
}

func inputModalities(raw json.RawMessage) map[core.Capability]struct{} {
	capabilities := make(map[core.Capability]struct{})
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if kind, ok := value["type"].(string); ok {
				switch {
				case strings.Contains(kind, "image"):
					capabilities["llm.vision"] = struct{}{}
				case strings.Contains(kind, "audio"), strings.Contains(kind, "video"):
					capabilities["llm.audio"] = struct{}{}
				}
			}
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return capabilities
	}
	walk(value)
	return capabilities
}
