package antigravity

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

type oauthCredentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ProjectID    string `json:"project_id,omitempty"`
}

func parseOAuthCredentials(raw []byte, requireRefresh bool) (oauthCredentials, bool) {
	if !uniqueJSON(raw) {
		return oauthCredentials{}, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return oauthCredentials{}, false
	}
	for key := range fields {
		if key != "access_token" && key != "refresh_token" && key != "project_id" {
			return oauthCredentials{}, false
		}
	}
	var result oauthCredentials
	if json.Unmarshal(fields["access_token"], &result.AccessToken) != nil || strings.TrimSpace(result.AccessToken) == "" || strings.ContainsAny(result.AccessToken, "\r\n") {
		return oauthCredentials{}, false
	}
	refreshRaw, hasRefresh := fields["refresh_token"]
	if hasRefresh && (json.Unmarshal(refreshRaw, &result.RefreshToken) != nil || strings.TrimSpace(result.RefreshToken) == "" || strings.ContainsAny(result.RefreshToken, "\r\n")) {
		return oauthCredentials{}, false
	}
	if requireRefresh && !hasRefresh {
		return oauthCredentials{}, false
	}
	if projectRaw, ok := fields["project_id"]; ok && (json.Unmarshal(projectRaw, &result.ProjectID) != nil || !projectPattern.MatchString(result.ProjectID)) {
		return oauthCredentials{}, false
	}
	return result, true
}

func validateOAuthCandidate(accessToken, refreshToken string, includeRefresh, requireRefresh bool) (oauthCredentials, bool) {
	fields := map[string]string{"access_token": accessToken}
	if includeRefresh {
		fields["refresh_token"] = refreshToken
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return oauthCredentials{}, false
	}
	return parseOAuthCredentials(raw, requireRefresh)
}

func (c *Connector) Execute(ctx context.Context, req core.ExecutionRequest, scope core.AttemptScope, services core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	if e := c.validate(ctx, req.Payload.Protocol, scope.Mode); e != nil {
		return core.ExecutionResponse{}, e
	}
	c.mu.Lock()
	cfg := c.cfg
	c.mu.Unlock()
	transport, ok := offlineTestTransport(services.Transport)
	if !ok && os.Getenv("PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT") == "1" {
		c.mu.Lock()
		runtimeTransport := c.doer
		c.mu.Unlock()
		transport = runtimeTransport
		runtimeDoer, isRuntimeDoer := services.Transport.(*runtimeHTTPDoer)
		ok = runtimeTransport != nil && isRuntimeDoer && runtimeDoer == runtimeTransport
	}
	if !ok {
		return core.ExecutionResponse{}, unavailable()
	}
	mediaType, _, mediaErr := mime.ParseMediaType(req.Payload.ContentType)
	if mediaErr != nil || mediaType != "application/json" {
		return core.ExecutionResponse{}, invalid("invalid_request")
	}
	input, model, tools, toolConfig, e := admit(req.Payload.Body)
	if e != nil {
		return core.ExecutionResponse{}, e
	}
	if services.Credentials == nil {
		return core.ExecutionResponse{}, unauthenticated()
	}
	secret, err := services.Credentials.Get(ctx, "oauth")
	if ctx.Err() != nil {
		return core.ExecutionResponse{}, contextError(ctx.Err())
	}
	if err != nil {
		return core.ExecutionResponse{}, unauthenticated()
	}
	creds, valid := parseOAuthCredentials(secret, false)
	if !valid {
		return core.ExecutionResponse{}, unauthenticated()
	}
	request := map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": input}}}}}
	if len(tools) != 0 {
		request["tools"] = tools
	}
	if toolConfig != nil {
		request["toolConfig"] = toolConfig
	}
	var requestID [16]byte
	if _, err := rand.Read(requestID[:]); err != nil {
		return core.ExecutionResponse{}, unavailable()
	}
	executionID := hex.EncodeToString(requestID[:])
	body, err := json.Marshal(map[string]any{
		"project": cfg.Project, "model": model, "request": request,
		"requestType": "agent", "userAgent": "antigravity", "requestId": "agent/" + executionID + "/" + time.Now().UTC().Format("20060102T150405.000Z"),
	})
	if err != nil {
		return core.ExecutionResponse{}, invalid("invalid_request")
	}
	requestURL := cfg.Endpoint + "/v1internal:streamGenerateContent?alt=sse"
	endpoint, _ := url.Parse(cfg.Endpoint)
	if strings.EqualFold(endpoint.Hostname(), "antigravity.synthetic.invalid") {
		requestURL = cfg.Endpoint + "/v1internal/projects/" + cfg.Project + ":generateContent?alt=sse"
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return core.ExecutionResponse{}, invalid("invalid_request")
	}
	httpReq.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if ctx.Err() != nil {
		return core.ExecutionResponse{}, contextError(ctx.Err())
	}
	resp, err := transport.Do(httpReq)
	if err != nil {
		closeResponse(resp)
		if ctx.Err() != nil {
			return core.ExecutionResponse{}, contextError(ctx.Err())
		}
		return core.ExecutionResponse{}, &core.GatewayError{Code: "upstream_unavailable", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "Antigravity transport failed"}
	}
	if resp == nil || resp.Body == nil {
		return core.ExecutionResponse{}, &core.GatewayError{Code: "upstream_unavailable", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "Antigravity response is invalid"}
	}
	if ctx.Err() != nil {
		_ = resp.Body.Close()
		return core.ExecutionResponse{}, contextError(ctx.Err())
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8192))
		_ = resp.Body.Close()
		return core.ExecutionResponse{}, statusError(resp.StatusCode)
	}
	mediaType, params, parseErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if parseErr != nil || mediaType != "text/event-stream" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) || (resp.Header.Get("Content-Encoding") != "" && !strings.EqualFold(resp.Header.Get("Content-Encoding"), "identity")) {
		_ = resp.Body.Close()
		return core.ExecutionResponse{}, &core.GatewayError{Code: "invalid_response", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "Antigravity response is invalid"}
	}
	return core.ExecutionResponse{Stream: newStream(resp.Body, "resp_"+executionID, "msg_"+executionID)}, nil
}

func admit(body []byte) (string, string, []any, map[string]any, *core.GatewayError) {
	if len(body) == 0 || len(body) > maxRequestBytes || !uniqueJSON(body) {
		return "", "", nil, nil, invalid("invalid_request")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return "", "", nil, nil, invalid("invalid_request")
	}
	for k := range fields {
		if k != "model" && k != "input" && k != "stream" && k != "tools" && k != "tool_choice" {
			return "", "", nil, nil, unsupported()
		}
	}
	var in struct {
		Model      string          `json:"model"`
		Input      string          `json:"input"`
		Stream     *bool           `json:"stream"`
		Tools      json.RawMessage `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF || in.Stream == nil || !*in.Stream || strings.TrimSpace(in.Input) == "" {
		return "", "", nil, nil, invalid("invalid_request")
	}
	if in.Model != "gemini-2.5-pro" && in.Model != "gemini-2.5-flash" {
		return "", "", nil, nil, invalid("invalid_request")
	}
	tools, names, err := functionTools(in.Tools)
	if err != nil {
		return "", "", nil, nil, invalid("invalid_request")
	}
	toolConfig, err := functionToolChoice(in.ToolChoice, names)
	if err != nil {
		return "", "", nil, nil, unsupported()
	}
	return in.Input, in.Model, tools, toolConfig, nil
}

func functionTools(raw json.RawMessage) ([]any, map[string]bool, error) {
	if len(raw) == 0 {
		return nil, nil, nil
	}
	var declarations []json.RawMessage
	if json.Unmarshal(raw, &declarations) != nil || declarations == nil {
		return nil, nil, errors.New("tools must be an array")
	}
	if len(declarations) == 0 {
		return nil, nil, nil
	}
	functions := make([]any, 0, len(declarations))
	names := make(map[string]bool, len(declarations))
	for _, declaration := range declarations {
		var fields map[string]json.RawMessage
		if json.Unmarshal(declaration, &fields) != nil || fields == nil {
			return nil, nil, errors.New("tool declaration must be an object")
		}
		for key := range fields {
			if key != "type" && key != "name" && key != "description" && key != "parameters" {
				return nil, nil, errors.New("unsupported function declaration field")
			}
		}
		var tool struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal(declaration, &tool) != nil || tool.Type != "function" || !validFunctionName(tool.Name) || names[tool.Name] || !validJSONSchema(tool.Parameters) {
			return nil, nil, errors.New("invalid function declaration")
		}
		if description, ok := fields["description"]; ok {
			if _, valid := schemaString(description); !valid {
				return nil, nil, errors.New("invalid function description")
			}
		}
		names[tool.Name] = true
		functions = append(functions, map[string]any{"name": tool.Name, "description": tool.Description, "parametersJsonSchema": json.RawMessage(tool.Parameters)})
	}
	return []any{map[string]any{"functionDeclarations": functions}}, names, nil
}

func validFunctionName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !(r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func validJSONSchema(raw json.RawMessage) bool {
	if len(raw) == 0 || !uniqueJSON(raw) {
		return false
	}
	var schema map[string]json.RawMessage
	if json.Unmarshal(raw, &schema) != nil || schema == nil {
		return false
	}
	var kind string
	if json.Unmarshal(schema["type"], &kind) != nil || kind != "object" {
		return false
	}
	return validateSchemaObject(schema)
}

func validSchemaNode(raw json.RawMessage) bool {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("true")) || bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
		return true
	}
	var schema map[string]json.RawMessage
	if json.Unmarshal(raw, &schema) != nil || schema == nil {
		return false
	}
	return validateSchemaObject(schema)
}

func validateSchemaObject(schema map[string]json.RawMessage) bool {
	for key := range schema {
		switch key {
		case "type", "description", "title", "properties", "required", "items", "enum", "minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems", "additionalProperties", "pattern":
		default:
			return false
		}
	}
	if raw, ok := schema["type"]; ok {
		var value string
		if json.Unmarshal(raw, &value) != nil || !validSchemaType(value) {
			return false
		}
	}
	for _, key := range []string{"description", "title"} {
		if raw, ok := schema[key]; ok {
			if _, valid := schemaString(raw); !valid {
				return false
			}
		}
	}
	properties := map[string]json.RawMessage{}
	if raw, ok := schema["properties"]; ok {
		if json.Unmarshal(raw, &properties) != nil || properties == nil {
			return false
		}
		for _, child := range properties {
			if !validSchemaNode(child) {
				return false
			}
		}
	}
	if raw, ok := schema["required"]; ok {
		var names []string
		if json.Unmarshal(raw, &names) != nil || names == nil {
			return false
		}
		seen := map[string]bool{}
		for _, name := range names {
			if name == "" || seen[name] {
				return false
			}
			if _, exists := properties[name]; !exists {
				return false
			}
			seen[name] = true
		}
	}
	if raw, ok := schema["items"]; ok {
		if !validSchemaNode(raw) {
			return false
		}
	}
	if raw, ok := schema["additionalProperties"]; ok && !validSchemaNode(raw) {
		return false
	}
	if raw, ok := schema["enum"]; ok {
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil || len(values) == 0 {
			return false
		}
		for _, value := range values {
			if !uniqueJSON(value) {
				return false
			}
		}
	}
	for _, key := range []string{"minimum", "maximum"} {
		if raw, ok := schema[key]; ok {
			if _, err := strconv.ParseFloat(string(raw), 64); err != nil {
				return false
			}
		}
	}
	if min, ok := schema["minimum"]; ok {
		if max, exists := schema["maximum"]; exists {
			lo, _ := strconv.ParseFloat(string(min), 64)
			hi, _ := strconv.ParseFloat(string(max), 64)
			if lo > hi {
				return false
			}
		}
	}
	for _, key := range []string{"minLength", "maxLength", "minItems", "maxItems"} {
		if raw, ok := schema[key]; ok {
			n, err := strconv.Atoi(string(raw))
			if err != nil || n < 0 {
				return false
			}
		}
	}
	for _, pair := range [][2]string{{"minLength", "maxLength"}, {"minItems", "maxItems"}} {
		if lo, ok := schema[pair[0]]; ok {
			if hi, exists := schema[pair[1]]; exists {
				min, _ := strconv.Atoi(string(lo))
				max, _ := strconv.Atoi(string(hi))
				if min > max {
					return false
				}
			}
		}
	}
	if raw, ok := schema["pattern"]; ok {
		pattern, valid := schemaString(raw)
		if !valid {
			return false
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return false
		}
	}
	return true
}

func schemaString(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var value string
	if json.Unmarshal(trimmed, &value) != nil {
		return "", false
	}
	return value, true
}

func validSchemaType(value string) bool {
	switch value {
	case "object", "array", "string", "number", "integer", "boolean", "null":
		return true
	default:
		return false
	}
}

func functionToolChoice(raw json.RawMessage, names map[string]bool) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var choice string
	if json.Unmarshal(raw, &choice) == nil {
		switch choice {
		case "auto":
			return nil, nil
		case "none":
			return map[string]any{"functionCallingConfig": map[string]any{"mode": "NONE"}}, nil
		case "required":
			if len(names) == 0 {
				return nil, errors.New("required tool choice needs functions")
			}
			return map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY"}}, nil
		default:
			return nil, errors.New("unsupported tool choice")
		}
	}
	var specific struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil || len(fields) != 2 || json.Unmarshal(raw, &specific) != nil || specific.Type != "function" || !names[specific.Name] {
		return nil, errors.New("unsupported tool choice")
	}
	return map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY", "allowedFunctionNames": []string{specific.Name}}}, nil
}
func unsupported() *core.GatewayError {
	return &core.GatewayError{Code: "unsupported_feature", Category: core.CategoryUnsupportedFeature, RetryDisposition: core.RetryUnsafe, Message: "Antigravity request feature is unsupported"}
}
func unauthenticated() *core.GatewayError {
	return &core.GatewayError{Code: "unauthenticated", Category: core.CategoryUnauthenticated, RetryDisposition: core.RetryUnsafe, Message: "Antigravity credentials are unavailable or invalid"}
}
func statusError(status int) *core.GatewayError {
	switch {
	case status == 401 || status == 403:
		return unauthenticated()
	case status == 429:
		return &core.GatewayError{Code: "rate_limited", Category: core.CategoryRateLimited, Retryable: true, RetryDisposition: core.RetryUnknown, Message: "Antigravity request was rate limited"}
	case status >= 500 && status <= 599:
		return &core.GatewayError{Code: "upstream_unavailable", Category: core.CategoryUnavailable, Retryable: true, RetryDisposition: core.RetryUnknown, Message: "Antigravity service is unavailable"}
	default:
		return &core.GatewayError{Code: "upstream_unavailable", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "Antigravity request failed"}
	}
}
func closeResponse(r *http.Response) {
	if r != nil && r.Body != nil {
		_ = r.Body.Close()
	}
}
func offlineTestTransport(doer core.HTTPDoer) (core.HTTPDoer, bool) {
	switch value := doer.(type) {
	case *OfflineTestTransport:
		return value, value != nil
	case packageTestTransport:
		return value, !nilTransport(value.doer)
	default:
		return nil, false
	}
}

func nilTransport(t core.HTTPDoer) bool {
	if t == nil {
		return true
	}
	v := reflect.ValueOf(t)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}
