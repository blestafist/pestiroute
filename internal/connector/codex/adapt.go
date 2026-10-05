package codex

import (
	"bytes"
	"encoding/json"

	"github.com/blestafist/pestiroute/internal/core"
)

const encryptedReasoningInclude = "reasoning.encrypted_content"

// adaptRequest returns native bytes untouched and constructs translation output
// from RawMessages so protocol items and admitted extensions are not modeled.
func adaptRequest(req core.ExecutionRequest, mode string) ([]byte, *core.GatewayError) {
	invalid := func(message string) ([]byte, *core.GatewayError) {
		return nil, connectorError("invalid_request", core.CategoryInvalidRequest, message)
	}
	if mode == core.ModeNative {
		return req.Payload.Body, nil
	}
	if mode != core.ModeTranslation {
		return nil, connectorError("unsupported_mode", core.CategoryUnsupportedFeature, "Unsupported execution mode")
	}
	body := req.Payload.Body
	if len(body) > maxResponsesBodyBytes || !uniqueResponsesJSON(body) {
		return invalid("Invalid Responses request JSON")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return invalid("Invalid Responses request JSON")
	}
	if raw, ok := fields["prompt_cache_key"]; ok {
		var key string
		if json.Unmarshal(raw, &key) != nil || len(key) == 0 || len(key) > 256 {
			return invalid("Invalid Responses prompt cache key")
		}
		for i := 0; i < len(key); i++ {
			if key[i] < 0x20 || key[i] > 0x7e {
				return invalid("Invalid Responses prompt cache key")
			}
		}
	}
	if _, ok := fields["stream"]; !ok {
		fields["stream"] = json.RawMessage("true")
	}
	if _, ok := fields["store"]; !ok {
		fields["store"] = json.RawMessage("false")
	}
	var include []json.RawMessage
	if raw, ok := fields["include"]; ok {
		if err := json.Unmarshal(raw, &include); err != nil || include == nil {
			return invalid("Invalid Responses include field")
		}
	}
	found := false
	for _, raw := range include {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '"' {
			return invalid("Invalid Responses include field")
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return invalid("Invalid Responses include field")
		}
		found = found || value == encryptedReasoningInclude
	}
	if !found {
		include = append(include, json.RawMessage(`"`+encryptedReasoningInclude+`"`))
	}
	encodedInclude, err := json.Marshal(include)
	if err != nil {
		return invalid("Invalid Responses include field")
	}
	fields["include"] = encodedInclude
	adapted, err := json.Marshal(fields)
	if err != nil || len(adapted) > maxResponsesBodyBytes {
		return invalid("Adapted Responses request exceeds the profile size limit")
	}
	return adapted, nil
}
