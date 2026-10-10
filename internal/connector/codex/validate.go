package codex

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/blestafist/pestiroute/internal/core"
)

const maxResponsesBodyBytes = 1 << 20

// validateResponsesProfile only inspects fields whose semantics this profile owns.
// It never returns or edits the body, which remains opaque to Core and unchanged in native mode.
func validateResponsesProfile(req core.ExecutionRequest, mode, configuredModel string) *core.GatewayError {
	return validateResponsesProfileForProfile(req, mode, configuredModel, profile)
}

func validateResponsesProfileForProfile(req core.ExecutionRequest, mode, configuredModel, selectedProfile string) *core.GatewayError {
	invalid := func(message string) *core.GatewayError {
		return connectorError("invalid_request", core.CategoryInvalidRequest, message)
	}
	unsupported := func(message string) *core.GatewayError {
		return connectorError("unsupported_feature", core.CategoryUnsupportedFeature, message)
	}
	body := req.Payload.Body
	if len(body) > maxResponsesBodyBytes {
		return invalid("Responses request exceeds the profile size limit")
	}
	if !uniqueResponsesJSON(body) {
		return invalid("Invalid Responses request JSON")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return invalid("Invalid Responses request JSON")
	}
	for name := range fields {
		for _, governed := range []string{"model", "input", "stream", "store", "tools", "tool_choice", "parallel_tool_calls", "background", "previous_response_id", "conversation", "context_management", "compaction_trigger", "max_output_tokens", "temperature", "top_p", "presence_penalty", "frequency_penalty", "text", "truncation", "modalities", "audio", "image", "video", "file_search", "attachments"} {
			if name != governed && strings.EqualFold(name, governed) {
				return invalid("Invalid Responses request field")
			}
		}
	}
	var model string
	if json.Unmarshal(fields["model"], &model) != nil || model == "" || model != req.Model || model != configuredModel {
		return invalid("Responses request model does not match the configured route")
	}
	if raw, ok := fields["input"]; !ok || len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return invalid("Responses request input is required")
	}
	streamRaw, hasStream := fields["stream"]
	if hasStream {
		var stream bool
		if json.Unmarshal(streamRaw, &stream) != nil {
			return invalid("Invalid Responses streaming preference")
		}
		if !stream {
			return unsupported("Non-streaming Responses requests are unsupported")
		}
	} else if mode == core.ModeNative {
		return invalid("Native Responses requests require stream:true")
	}
	storeRaw, hasStore := fields["store"]
	if hasStore {
		var store bool
		if json.Unmarshal(storeRaw, &store) != nil {
			return invalid("Invalid Responses store preference")
		}
		if store {
			return unsupported("Stored Responses are unsupported")
		}
	} else if mode == core.ModeNative {
		return invalid("Native Responses requests require store:false")
	}
	for _, name := range []string{"background", "previous_response_id", "conversation", "context_management", "compaction_trigger", "max_output_tokens", "temperature", "top_p", "presence_penalty", "frequency_penalty", "text", "truncation", "modalities", "audio", "image", "video", "file_search", "attachments"} {
		if raw, ok := fields[name]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			if name == "background" && bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
				continue
			}
			return unsupported("Responses request uses an unsupported feature")
		}
	}
	if unsupportedInputType(fields["input"]) {
		return unsupported("Responses request uses an unsupported resource or modality")
	}
	toolNames := make(map[string]bool)
	customToolNames := make(map[string]bool)
	allowCustom := selectedProfile == liteProfile && mode == core.ModeNative && configuredModel == liteModel
	if raw, ok := fields["tools"]; ok {
		if ge := collectFunctionToolNames(raw, toolNames, customToolNames, false, invalid, unsupported); ge != nil {
			return ge
		}
	}
	if raw, ok := fields["input"]; ok {
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) == nil {
			for _, item := range items {
				var value map[string]json.RawMessage
				if json.Unmarshal(item, &value) != nil {
					continue
				}
				var kind string
				if json.Unmarshal(value["type"], &kind) != nil || kind != "additional_tools" {
					continue
				}
				var role string
				if json.Unmarshal(value["role"], &role) != nil || role != "developer" {
					return invalid("Invalid additional_tools role")
				}
				if ge := collectFunctionToolNames(value["tools"], toolNames, customToolNames, allowCustom, invalid, unsupported); ge != nil {
					return ge
				}
			}
		}
	}
	if raw, ok := fields["tool_choice"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var choice any
		if json.Unmarshal(raw, &choice) != nil {
			return invalid("Invalid Responses tool choice")
		}
		switch choice := choice.(type) {
		case string:
			if choice != "auto" && choice != "none" && choice != "required" {
				return unsupported("Responses tool choice is not supported")
			}
		case map[string]any:
			name, ok := choice["name"].(string)
			if choice["type"] != "function" || !ok || name == "" || !toolNames[name] {
				return unsupported("Responses tool choice is not supported")
			}
		default:
			return unsupported("Responses tool choice is not supported")
		}
	}
	if len(customToolNames) > 0 {
		var choice string
		if raw, ok := fields["tool_choice"]; !ok || json.Unmarshal(raw, &choice) != nil || choice != "auto" {
			return unsupported("Custom tools require tool_choice auto")
		}
	}
	if raw, ok := fields["parallel_tool_calls"]; ok {
		var parallel bool
		if json.Unmarshal(raw, &parallel) != nil {
			return invalid("Invalid Responses parallel tool preference")
		}
		if parallel {
			return unsupported("Parallel function calls are unsupported")
		}
	}
	return nil
}

func collectFunctionToolNames(raw json.RawMessage, names, customNames map[string]bool, allowCustom bool, invalid, unsupported func(string) *core.GatewayError) *core.GatewayError {
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil || tools == nil {
		return invalid("Invalid Responses tools")
	}
	for _, rawTool := range tools {
		var tool map[string]json.RawMessage
		if json.Unmarshal(rawTool, &tool) != nil || tool == nil {
			return invalid("Invalid Responses tool definition")
		}
		var kind string
		if json.Unmarshal(tool["type"], &kind) != nil || kind == "" {
			return invalid("Invalid Responses tool type")
		}
		if kind == "namespace" {
			var namespace string
			if json.Unmarshal(tool["name"], &namespace) != nil || namespace == "" {
				return invalid("Invalid Responses tool namespace")
			}
			if namespace != "functions" {
				return unsupported("Only the functions tool namespace is supported")
			}
			if ge := collectNamespaceFunctionNames(tool["tools"], names, customNames, allowCustom, invalid, unsupported); ge != nil {
				return ge
			}
			continue
		}
		if kind != "function" {
			return unsupported("Only function tools are supported")
		}
		if ge := addFunctionToolName(tool, names, customNames, invalid); ge != nil {
			return ge
		}
	}
	return nil
}

func collectNamespaceFunctionNames(raw json.RawMessage, names, customNames map[string]bool, allowCustom bool, invalid, unsupported func(string) *core.GatewayError) *core.GatewayError {
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil || tools == nil {
		return invalid("Invalid functions namespace tools")
	}
	for _, rawTool := range tools {
		var tool map[string]json.RawMessage
		var kind string
		if json.Unmarshal(rawTool, &tool) != nil || tool == nil || json.Unmarshal(tool["type"], &kind) != nil || kind == "" {
			return invalid("Invalid functions namespace tool")
		}
		switch kind {
		case "function":
			if ge := addFunctionToolName(tool, names, customNames, invalid); ge != nil {
				return ge
			}
		case "custom":
			if !allowCustom {
				return unsupported("Custom tools are supported only in the Codex Lite functions namespace")
			}
			if ge := addCustomToolName(tool, names, customNames, invalid, unsupported); ge != nil {
				return ge
			}
		default:
			return unsupported("Only function and custom tools are supported in the functions namespace")
		}
	}
	return nil
}

func addFunctionToolName(tool map[string]json.RawMessage, names, customNames map[string]bool, invalid func(string) *core.GatewayError) *core.GatewayError {
	var name string
	if json.Unmarshal(tool["name"], &name) != nil || strings.TrimSpace(name) == "" {
		return invalid("Invalid function tool name")
	}
	if rawParameters, exists := tool["parameters"]; exists && !bytes.Equal(bytes.TrimSpace(rawParameters), []byte("null")) {
		var parameters map[string]json.RawMessage
		if json.Unmarshal(rawParameters, &parameters) != nil || parameters == nil {
			return invalid("Invalid function tool parameters")
		}
	}
	if names[name] || customNames[name] {
		return invalid("Duplicate function tool name")
	}
	names[name] = true
	return nil
}

func addCustomToolName(tool map[string]json.RawMessage, names, customNames map[string]bool, invalid, unsupported func(string) *core.GatewayError) *core.GatewayError {
	// Treat a non-empty official grammar as opaque; do not become a grammar compiler.
	var name, description string
	descriptionRaw, hasDescription := tool["description"]
	if json.Unmarshal(tool["name"], &name) != nil || strings.TrimSpace(name) == "" || !hasDescription || bytes.Equal(bytes.TrimSpace(descriptionRaw), []byte("null")) || json.Unmarshal(descriptionRaw, &description) != nil {
		return invalid("Invalid custom tool name or description")
	}
	if names[name] || customNames[name] {
		return invalid("Duplicate tool name")
	}
	if _, exists := tool["parameters"]; exists {
		return unsupported("Custom tools do not use function parameters")
	}
	if raw, exists := tool["defer_loading"]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var deferred bool
		if json.Unmarshal(raw, &deferred) != nil {
			return invalid("Invalid custom tool defer_loading")
		}
	}
	var format map[string]json.RawMessage
	if json.Unmarshal(tool["format"], &format) != nil || format == nil {
		return invalid("Invalid custom tool format")
	}
	var formatType, syntax, definition string
	if json.Unmarshal(format["type"], &formatType) != nil || json.Unmarshal(format["syntax"], &syntax) != nil || json.Unmarshal(format["definition"], &definition) != nil || definition == "" {
		return invalid("Invalid custom tool format")
	}
	if formatType != "grammar" || syntax != "lark" {
		return unsupported("Only the proven Codex Lark grammar custom format is supported")
	}
	customNames[name] = true
	return nil
}

func unsupportedInputType(raw json.RawMessage) bool {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil || item == nil {
			return false
		}
		items = []json.RawMessage{raw}
	}
	for _, rawItem := range items {
		var item map[string]json.RawMessage
		if json.Unmarshal(rawItem, &item) != nil || item == nil {
			continue
		}
		rawType, hasType := item["type"]
		var kind string
		if hasType && json.Unmarshal(rawType, &kind) != nil {
			continue
		}
		if isUnsupportedInputType(kind) {
			return true
		}
		// Typed messages and untyped shorthand messages use content regardless
		// of role. Unknown typed items remain opaque.
		if (kind == "message" || !hasType) && unsupportedResourceArray(item["content"]) {
			return true
		}
		if kind == "function_call_output" && unsupportedResourceArray(item["output"]) {
			return true
		}
	}
	return false
}

func unsupportedResourceArray(raw json.RawMessage) bool {
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return false
	}
	return unsupportedResourceBlocks(blocks)
}

func unsupportedResourceBlocks(blocks []json.RawMessage) bool {
	for _, rawBlock := range blocks {
		var block struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(rawBlock, &block) == nil && isUnsupportedInputType(block.Type) {
			return true
		}
	}
	return false
}

func isUnsupportedInputType(kind string) bool {
	switch kind {
	case "input_image", "input_audio", "input_video", "input_file", "computer", "file_search_call", "web_search_call", "code_interpreter_call", "image_generation_call", "local_shell_call", "mcp_list_tools", "mcp_call":
		return true
	default:
		return false
	}
}

func uniqueResponsesJSON(body []byte) bool {
	d := json.NewDecoder(bytes.NewReader(body))
	if !uniqueResponsesValue(d) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

func uniqueResponsesValue(d *json.Decoder) bool {
	token, err := d.Token()
	if err != nil {
		return false
	}
	switch token {
	case json.Delim('{'):
		seen := make(map[string]struct{})
		for d.More() {
			key, err := d.Token()
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
			if !uniqueResponsesValue(d) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim('}')
	case json.Delim('['):
		for d.More() {
			if !uniqueResponsesValue(d) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim(']')
	case json.Delim('}'), json.Delim(']'):
		return false
	default:
		return true
	}
}
