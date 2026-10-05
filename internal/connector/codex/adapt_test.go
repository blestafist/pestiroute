package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestAdaptRequestNativePreservesExactBytes(t *testing.T) {
	body := []byte("{ \"model\":\"gpt-5.4-mini\",\"input\":\"x\",\"stream\":true,\"store\":false }\n")
	req := core.ExecutionRequest{Model: "gpt-5.4-mini", Payload: core.RawPayload{Body: body}}
	got, ge := adaptRequest(req, core.ModeNative)
	if ge != nil || !bytes.Equal(got, body) {
		t.Fatalf("adaptRequest() = %q, %v; want exact original bytes", got, ge)
	}
}

func TestAdaptRequestTranslationPreservesFieldsAndInput(t *testing.T) {
	for _, name := range []string{"request-positive.json", "request-tools-history.json", "request-reasoning-item.json", "request-unknown-fields.json"} {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("testdata", "responses", name))
			if err != nil {
				t.Fatal(err)
			}
			original := append([]byte(nil), body...)
			req := core.ExecutionRequest{Model: "gpt-5.4-mini", Payload: core.RawPayload{Body: body}}
			got, ge := adaptRequest(req, core.ModeTranslation)
			if ge != nil {
				t.Fatal(ge)
			}
			if !bytes.Equal(body, original) {
				t.Fatal("adaptRequest mutated the input body")
			}
			var before, after map[string]json.RawMessage
			if err := json.Unmarshal(original, &before); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(got, &after); err != nil {
				t.Fatalf("invalid adapted JSON: %v", err)
			}
			for _, field := range []string{"input", "tools", "tool_choice", "prompt_cache_key", "optional_extension", "vendor_hint"} {
				if raw, ok := before[field]; ok && !bytes.Equal(after[field], raw) {
					t.Errorf("field %q changed: got %s, want %s", field, after[field], raw)
				}
			}
			if string(after["stream"]) != "true" || string(after["store"]) != "false" {
				t.Errorf("defaults: stream=%s store=%s", after["stream"], after["store"])
			}
			var include []string
			if err := json.Unmarshal(after["include"], &include); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(include, []string{encryptedReasoningInclude}) {
				found := false
				for _, item := range include {
					found = found || item == encryptedReasoningInclude
				}
				if !found {
					t.Errorf("missing encrypted reasoning include: %v", include)
				}
			}
		})
	}
}

func TestAdaptRequestTranslationRejectsInvalidCacheKeyAndOversize(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{name: "cache key control byte", body: []byte(`{"prompt_cache_key":"bad\nkey"}`)},
		{name: "cache key too long", body: []byte(`{"prompt_cache_key":"` + string(bytes.Repeat([]byte{'a'}, 257)) + `"}`)},
		{name: "oversized", body: bytes.Repeat([]byte{' '}, maxResponsesBodyBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ge := adaptRequest(core.ExecutionRequest{Payload: core.RawPayload{Body: tc.body}}, core.ModeTranslation); ge == nil {
				t.Errorf("adaptRequest accepted invalid input of %d bytes", len(tc.body))
			}
		})
	}
}

func TestAdaptRequestTranslationRejectsMalformedIncludeEntries(t *testing.T) {
	for _, include := range []string{`null`, `1`, `false`, `{}`, `"string"`, `[null]`, `[1]`, `[false]`, `[{}]`} {
		t.Run(include, func(t *testing.T) {
			body := []byte(`{"include":` + include + `}`)
			if _, ge := adaptRequest(core.ExecutionRequest{Payload: core.RawPayload{Body: body}}, core.ModeTranslation); ge == nil {
				t.Errorf("adaptRequest accepted malformed include %s", include)
			}
		})
	}
}

func TestAdaptRequestTranslationPreservesExplicitControlsAndCacheKey(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4-mini","input":[{"type":"function_call","call_id":"call-1","arguments":"{}"}],"stream":true,"store":false,"include":["existing","reasoning.encrypted_content"],"tool_choice":{"type":"function","name":"lookup"},"parallel_tool_calls":true,"max_output_tokens":123,"prompt_cache_key":"cache-key"}`)
	got, ge := adaptRequest(core.ExecutionRequest{Payload: core.RawPayload{Body: body}}, core.ModeTranslation)
	if ge != nil {
		t.Fatal(ge)
	}
	var before, after map[string]json.RawMessage
	if err := json.Unmarshal(body, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &after); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"input", "tool_choice", "parallel_tool_calls", "max_output_tokens", "prompt_cache_key"} {
		if !bytes.Equal(after[field], before[field]) {
			t.Errorf("field %q changed: got %s, want %s", field, after[field], before[field])
		}
	}
	var include []string
	if err := json.Unmarshal(after["include"], &include); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(include, []string{"existing", encryptedReasoningInclude}) {
		t.Errorf("include changed or duplicated: %v", include)
	}
}
