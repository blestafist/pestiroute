package chatcompletions

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

func invalid(s string) *core.GatewayError {
	return &core.GatewayError{Code: "invalid_request", Category: core.CategoryInvalidRequest, Message: s}
}
func unsupported(s string) *core.GatewayError {
	return &core.GatewayError{Code: "unsupported_feature", Category: core.CategoryUnsupportedFeature, Message: s}
}

func Decode(r *http.Request, maxBody, maxHeader int64) (core.ExecutionRequest, *core.GatewayError) {
	var zero core.ExecutionRequest
	if r == nil || r.Body == nil || maxBody <= 0 || maxHeader <= 0 {
		return zero, invalid("Invalid request")
	}
	ct := r.Header.Values("Content-Type")
	if len(ct) != 1 {
		return zero, unsupported("Unsupported request media type")
	}
	media, params, e := mime.ParseMediaType(ct[0])
	if e != nil || media != "application/json" || len(params) > 1 || (len(params) == 1 && !strings.EqualFold(params["charset"], "utf-8")) {
		return zero, unsupported("Unsupported request media type")
	}
	for _, v := range r.Header.Values("Content-Encoding") {
		if !strings.EqualFold(v, "identity") {
			return zero, unsupported("Unsupported content encoding")
		}
	}
	var hs int64
	for n, vs := range r.Header {
		hs += int64(len(n))
		for _, v := range vs {
			hs += int64(len(v))
		}
	}
	if hs > maxHeader || r.ContentLength > maxBody {
		return zero, invalid("Request exceeds configured limits")
	}
	lim := maxBody
	if lim < math.MaxInt64 {
		lim++
	}
	body, e := io.ReadAll(io.LimitReader(r.Body, lim))
	if e != nil || int64(len(body)) > maxBody {
		return zero, invalid("Cannot read request body")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	tok, e := d.Token()
	if e != nil || tok != json.Delim('{') {
		return zero, invalid("Invalid JSON request")
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		key, e := d.Token()
		if e != nil {
			return zero, invalid("Invalid JSON request")
		}
		k, ok := key.(string)
		if !ok {
			return zero, invalid("Invalid JSON request")
		}
		if _, ok = fields[k]; ok {
			return zero, invalid("Duplicate request field")
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil || !unique(raw) {
			return zero, invalid("Duplicate or invalid nested request field")
		}
		fields[k] = raw
	}
	if end, e := d.Token(); e != nil || end != json.Delim('}') || d.Decode(new(any)) != io.EOF {
		return zero, invalid("Invalid JSON request")
	}
	var model string
	if json.Unmarshal(fields["model"], &model) != nil || strings.TrimSpace(model) == "" {
		return zero, invalid("Invalid model")
	}
	var messages []json.RawMessage
	if json.Unmarshal(fields["messages"], &messages) != nil || len(messages) == 0 {
		return zero, invalid("Invalid messages")
	}
	caps := map[core.Capability]struct{}{}
	for _, m := range messages {
		if ge := inspectMessage(m, caps); ge != nil {
			return zero, ge
		}
	}
	if raw, ok := fields["stream"]; ok && string(raw) != "null" {
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return zero, invalid("Invalid stream")
		}
		v := b
		zero.Metadata.Streaming = &v
		if b {
			caps["llm.streaming"] = struct{}{}
		}
	}
	if raw, ok := fields["stream_options"]; ok && string(raw) != "null" {
		var x map[string]json.RawMessage
		if json.Unmarshal(raw, &x) != nil || x == nil {
			return zero, invalid("Invalid stream_options")
		}
		for _, n := range []string{"include_usage", "include_obfuscation"} {
			if q, ok := x[n]; ok {
				var b bool
				if json.Unmarshal(q, &b) != nil {
					return zero, invalid("Invalid stream_options")
				}
			}
		}
		if zero.Metadata.Streaming == nil || !*zero.Metadata.Streaming {
			return zero, invalid("stream_options requires stream:true")
		}
	}
	if raw, ok := fields["tools"]; ok {
		var x []json.RawMessage
		if json.Unmarshal(raw, &x) != nil || x == nil {
			return zero, invalid("Invalid tools")
		}
		for _, tool := range x {
			if !validTool(tool) {
				return zero, unsupported("Unsupported tool definition")
			}
		}
		if len(x) > 0 {
			caps["llm.tools"] = struct{}{}
		}
	}
	if raw, ok := fields["functions"]; ok {
		var x []json.RawMessage
		if json.Unmarshal(raw, &x) != nil || x == nil {
			return zero, invalid("Invalid functions")
		}
		for _, fn := range x {
			if !validFunction(fn) {
				return zero, invalid("Invalid function definition")
			}
		}
		if len(x) > 0 {
			caps["llm.tools"] = struct{}{}
		}
	}
	if raw, ok := fields["tool_choice"]; ok {
		if string(raw) == "null" {
			return zero, invalid("Invalid tool_choice")
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if s != "none" && s != "auto" && s != "required" {
				return zero, unsupported("Unsupported tool_choice")
			}
			if s != "none" {
				caps["llm.tools"] = struct{}{}
			}
		} else {
			var choice struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(raw, &choice) != nil || choice.Type != "function" || choice.Function.Name == "" {
				return zero, unsupported("Unsupported tool_choice")
			}
			caps["llm.tools"] = struct{}{}
		}
	}
	if raw, ok := fields["function_call"]; ok {
		if string(raw) == "null" {
			return zero, invalid("Invalid function_call")
		}
		var selector string
		if json.Unmarshal(raw, &selector) == nil {
			if !oneOf(selector, "none", "auto") {
				return zero, unsupported("Unsupported function_call")
			}
			if selector != "none" {
				caps["llm.tools"] = struct{}{}
			}
		} else {
			var choice struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(raw, &choice) != nil || choice.Name == "" {
				return zero, unsupported("Unsupported function_call")
			}
			caps["llm.tools"] = struct{}{}
		}
	}
	if raw, ok := fields["parallel_tool_calls"]; ok {
		var b bool
		if string(raw) == "null" || json.Unmarshal(raw, &b) != nil {
			return zero, invalid("Invalid parallel_tool_calls")
		}
		if b {
			caps["llm.tools.parallel"] = struct{}{}
		}
	}
	if raw, ok := fields["reasoning_effort"]; ok && string(raw) != "null" {
		var s string
		if json.Unmarshal(raw, &s) != nil || !oneOf(s, "none", "minimal", "low", "medium", "high", "xhigh", "max") {
			return zero, invalid("Invalid reasoning_effort")
		}
		caps["llm.reasoning"] = struct{}{}
	}
	if raw, ok := fields["response_format"]; ok {
		var x struct {
			Type   string          `json:"type"`
			Schema json.RawMessage `json:"json_schema"`
		}
		if json.Unmarshal(raw, &x) != nil {
			return zero, invalid("Invalid response_format")
		}
		switch x.Type {
		case "text":
		case "json_object":
			caps["llm.structured_output"] = struct{}{}
		case "json_schema":
			var schema struct {
				Name        string          `json:"name"`
				Description *string         `json:"description"`
				Body        json.RawMessage `json:"schema"`
				Strict      json.RawMessage `json:"strict"`
			}
			if json.Unmarshal(x.Schema, &schema) != nil || schema.Name == "" || (len(schema.Body) > 0 && string(schema.Body) != "null" && schema.Body[0] != '{') {
				return zero, invalid("Invalid json_schema response_format")
			}
			if len(schema.Strict) > 0 && string(schema.Strict) != "null" {
				var b bool
				if json.Unmarshal(schema.Strict, &b) != nil {
					return zero, invalid("Invalid json_schema strict value")
				}
			}
			caps["llm.structured_output"] = struct{}{}
		default:
			return zero, unsupported("Unsupported response_format")
		}
	}
	if raw, ok := fields["audio"]; ok && string(raw) != "null" {
		var audio struct {
			Format string          `json:"format"`
			Voice  json.RawMessage `json:"voice"`
		}
		if json.Unmarshal(raw, &audio) != nil || !oneOf(audio.Format, "wav", "aac", "mp3", "flac", "opus", "pcm16") || len(audio.Voice) == 0 {
			return zero, invalid("Invalid audio output")
		}
		var voice string
		var voiceObject struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(audio.Voice, &voice) == nil {
			if voice == "" {
				return zero, invalid("Invalid audio voice")
			}
		} else if json.Unmarshal(audio.Voice, &voiceObject) != nil || voiceObject.ID == "" {
			return zero, invalid("Invalid audio voice")
		}
		caps["llm.audio"] = struct{}{}
	}
	if raw, ok := fields["modalities"]; ok && string(raw) != "null" {
		var x []string
		if json.Unmarshal(raw, &x) != nil || x == nil {
			return zero, invalid("Invalid modalities")
		}
		seen := map[string]bool{}
		for _, v := range x {
			if seen[v] || !oneOf(v, "text", "audio") {
				return zero, invalid("Invalid modalities")
			}
			seen[v] = true
			if v == "audio" {
				caps["llm.audio"] = struct{}{}
			}
		}
	}
	if raw, ok := fields["web_search_options"]; ok && string(raw) != "null" {
		return zero, unsupported("web_search_options is unsupported")
	}
	// This format has no file capability. Reject file parts before any dispatch.
	zero.Model = model
	zero.Capabilities = caps
	zero.Payload = core.RawPayload{Protocol: protocol, ContentType: "application/json", Body: body}
	zero.Metadata.Headers = safeHeaders(r.Header)
	zero.Metadata.IngressHeaderBytes = hs
	return zero, nil
}
func oneOf(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}
func unique(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	if !uniqueValue(d) {
		return false
	}
	_, e := d.Token()
	return e == io.EOF
}
func uniqueValue(d *json.Decoder) bool {
	t, e := d.Token()
	if e != nil {
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
			k, e := d.Token()
			if e != nil {
				return false
			}
			s, ok := k.(string)
			if !ok || seen[s] {
				return false
			}
			seen[s] = true
			if !uniqueValue(d) {
				return false
			}
		}
		x, e := d.Token()
		return e == nil && x == json.Delim('}')
	case '[':
		for d.More() {
			if !uniqueValue(d) {
				return false
			}
		}
		x, e := d.Token()
		return e == nil && x == json.Delim(']')
	}
	return false
}
func safeHeaders(h http.Header) map[string][]string {
	out := map[string][]string{}
	blocked := map[string]bool{"connection": true, "keep-alive": true, "proxy-connection": true, "transfer-encoding": true, "te": true, "trailer": true, "upgrade": true, "content-length": true, "content-encoding": true, "authorization": true, "cookie": true, "cookie2": true, "set-cookie": true}
	for _, v := range h.Values("Connection") {
		for _, n := range strings.Split(v, ",") {
			blocked[strings.ToLower(strings.TrimSpace(n))] = true
		}
	}
	for n, v := range h {
		l := strings.ToLower(n)
		if !blocked[l] && l != "openai-organization" && l != "openai-project" && !strings.Contains(l, "auth") && (l == "idempotency-key" || !strings.Contains(l, "key")) && !strings.Contains(l, "secret") && !strings.Contains(l, "token") && !strings.Contains(l, "credential") && !strings.HasPrefix(l, "proxy-") {
			out[http.CanonicalHeaderKey(n)] = append([]string(nil), v...)
		}
	}
	return out
}
func inspectMessage(raw []byte, c map[core.Capability]struct{}) *core.GatewayError {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return invalid("Invalid message")
	}
	var role string
	if json.Unmarshal(m["role"], &role) != nil || !oneOf(role, "system", "developer", "user", "assistant", "tool", "function") {
		return invalid("Invalid message role")
	}
	if role == "tool" {
		var id string
		if json.Unmarshal(m["tool_call_id"], &id) != nil || id == "" {
			return invalid("Invalid tool result")
		}
		c["llm.tools"] = struct{}{}
	}
	if role == "function" {
		var name string
		if json.Unmarshal(m["name"], &name) != nil || name == "" {
			return invalid("Invalid function result")
		}
	}
	if role == "assistant" {
		if raw, ok := m["audio"]; ok {
			var audio struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &audio) != nil || audio.ID == "" {
				return invalid("Invalid assistant audio history")
			}
			c["llm.audio"] = struct{}{}
		}
		if raw, ok := m["tool_calls"]; ok {
			var calls []json.RawMessage
			if json.Unmarshal(raw, &calls) != nil {
				return invalid("Invalid tool_calls")
			}
			if len(calls) > 0 {
				c["llm.tools"] = struct{}{}
			}
			for _, call := range calls {
				var x struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string  `json:"name"`
						Arguments *string `json:"arguments"`
					} `json:"function"`
				}
				if json.Unmarshal(call, &x) != nil || x.ID == "" || x.Type != "function" || x.Function.Name == "" || x.Function.Arguments == nil {
					return unsupported("Unsupported tool-call history")
				}
			}
		}
	}
	content, hasContent := m["content"]
	if (role == "system" || role == "developer" || role == "user" || role == "tool" || role == "function") && !hasContent {
		return invalid("Message content is required")
	}
	if hasContent && string(content) == "null" && role != "assistant" && role != "function" {
		return invalid("Null message content is not admitted for this role")
	}
	if role == "function" {
		var text *string
		if json.Unmarshal(content, &text) != nil {
			return invalid("Invalid function content")
		}
		return nil
	}
	if raw, ok := m["content"]; ok && string(raw) != "null" {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return nil
		}
		var parts []json.RawMessage
		if json.Unmarshal(raw, &parts) != nil || parts == nil {
			return invalid("Invalid message content")
		}
		for _, p := range parts {
			var x struct {
				Type  string          `json:"type"`
				Image json.RawMessage `json:"image_url"`
				Audio json.RawMessage `json:"input_audio"`
			}
			if json.Unmarshal(p, &x) != nil || x.Type == "" {
				return invalid("Invalid content part")
			}
			switch x.Type {
			case "text":
				if role != "system" && role != "developer" && role != "user" && role != "assistant" && role != "tool" {
					return unsupported("Unsupported content part for message role")
				}
				var part map[string]json.RawMessage
				var text string
				if json.Unmarshal(p, &part) != nil || json.Unmarshal(part["text"], &text) != nil {
					return invalid("Invalid text content part")
				}
			case "refusal":
				if role != "assistant" {
					return unsupported("Unsupported content part for message role")
				}
			case "image_url":
				if role != "user" {
					return unsupported("Unsupported content part for message role")
				}
				var image struct {
					URL    string `json:"url"`
					Detail string `json:"detail"`
				}
				var part map[string]json.RawMessage
				if json.Unmarshal(p, &part) != nil || json.Unmarshal(part["image_url"], &image) != nil || image.URL == "" || (image.Detail != "" && !oneOf(image.Detail, "auto", "low", "high", "original")) {
					return invalid("Invalid image content part")
				}
				var imageFields map[string]json.RawMessage
				_ = json.Unmarshal(part["image_url"], &imageFields)
				if detail, ok := imageFields["detail"]; ok && string(detail) == "null" {
					return invalid("Invalid image detail")
				}
				c["llm.vision"] = struct{}{}
			case "input_audio":
				if role != "user" {
					return unsupported("Unsupported content part for message role")
				}
				var audio struct {
					Data   string `json:"data"`
					Format string `json:"format"`
				}
				var part map[string]json.RawMessage
				if json.Unmarshal(p, &part) != nil || json.Unmarshal(part["input_audio"], &audio) != nil || audio.Data == "" || !oneOf(audio.Format, "wav", "mp3") {
					return invalid("Invalid audio content part")
				}
				c["llm.audio"] = struct{}{}
			case "file":
				return unsupported("File content is unsupported")
			default:
				return unsupported("Unsupported message content part")
			}
		}
	}
	if role == "assistant" {
		if raw, ok := m["function_call"]; ok && string(raw) != "null" {
			var call struct {
				Name      string  `json:"name"`
				Arguments *string `json:"arguments"`
			}
			if json.Unmarshal(raw, &call) != nil || call.Name == "" || call.Arguments == nil {
				return invalid("Invalid function_call")
			}
			c["llm.tools"] = struct{}{}
		}
	}
	return nil
}
func validTool(raw []byte) bool {
	var x struct {
		Type     string          `json:"type"`
		Function json.RawMessage `json:"function"`
	}
	if json.Unmarshal(raw, &x) != nil || x.Type != "function" {
		return false
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil || !onlyFields(top, "type", "function") {
		return false
	}
	var f map[string]json.RawMessage
	if json.Unmarshal(x.Function, &f) != nil || f == nil {
		return false
	}
	if !onlyFields(f, "name", "description", "parameters", "strict") {
		return false
	}
	var name string
	if json.Unmarshal(f["name"], &name) != nil || name == "" {
		return false
	}
	if d, ok := f["description"]; ok {
		var s string
		if json.Unmarshal(d, &s) != nil {
			return false
		}
	}
	if p, ok := f["parameters"]; ok && !jsonObject(p) {
		return false
	}
	if s, ok := f["strict"]; ok && string(s) != "null" {
		var b bool
		if json.Unmarshal(s, &b) != nil {
			return false
		}
	}
	return true
}

func validFunction(raw []byte) bool {
	var f map[string]json.RawMessage
	if json.Unmarshal(raw, &f) != nil || f == nil || !onlyFields(f, "name", "description", "parameters") {
		return false
	}
	var name string
	if json.Unmarshal(f["name"], &name) != nil || name == "" {
		return false
	}
	if d, ok := f["description"]; ok {
		var s string
		if json.Unmarshal(d, &s) != nil {
			return false
		}
	}
	if p, ok := f["parameters"]; ok && !jsonObject(p) {
		return false
	}
	return true
}
func onlyFields(m map[string]json.RawMessage, allowed ...string) bool {
	for k := range m {
		if !oneOf(k, allowed...) {
			return false
		}
	}
	return true
}
func jsonObject(raw []byte) bool {
	trim := bytes.TrimSpace(raw)
	return len(trim) > 0 && trim[0] == '{'
}
