package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strings"

	"github.com/blestafist/pestiroute/internal/core"
)

type offlineTransport interface {
	core.HTTPDoer
	antigravityOfflineFakeOnly()
}
type oauthCredentials struct {
	AccessToken string `json:"access_token"`
}

func (c *Connector) Execute(ctx context.Context, req core.ExecutionRequest, scope core.AttemptScope, services core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	if e := c.validate(ctx, req.Payload.Protocol, scope.Mode); e != nil {
		return core.ExecutionResponse{}, e
	}
	c.mu.Lock()
	cfg := c.cfg
	c.mu.Unlock()
	transport, ok := services.Transport.(offlineTransport)
	if !ok || nilTransport(transport) {
		return core.ExecutionResponse{}, unavailable()
	}
	mediaType, _, mediaErr := mime.ParseMediaType(req.Payload.ContentType)
	if mediaErr != nil || mediaType != "application/json" {
		return core.ExecutionResponse{}, invalid("invalid_request")
	}
	input, model, e := admit(req.Payload.Body)
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
	var creds oauthCredentials
	d := json.NewDecoder(bytes.NewReader(secret))
	d.DisallowUnknownFields()
	if !uniqueJSON(secret) || d.Decode(&creds) != nil || d.Decode(new(any)) != io.EOF || strings.TrimSpace(creds.AccessToken) == "" || strings.ContainsAny(creds.AccessToken, "\r\n") {
		return core.ExecutionResponse{}, unauthenticated()
	}
	body, err := json.Marshal(map[string]any{
		"project": cfg.Project, "model": model, "request": map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": input}}}}},
		"requestType": "agent", "userAgent": "pestiroute-offline-fixture/1", "requestId": "synthetic-request-001",
	})
	if err != nil {
		return core.ExecutionResponse{}, invalid("invalid_request")
	}
	url := "https://antigravity.synthetic.invalid/v1internal/projects/" + cfg.Project + ":generateContent?alt=sse"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
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
	return core.ExecutionResponse{Stream: newStream(resp.Body)}, nil
}

func admit(body []byte) (string, string, *core.GatewayError) {
	if len(body) == 0 || len(body) > maxRequestBytes || !uniqueJSON(body) {
		return "", "", invalid("invalid_request")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return "", "", invalid("invalid_request")
	}
	for k := range fields {
		if k != "model" && k != "input" && k != "stream" {
			return "", "", unsupported()
		}
	}
	var in struct {
		Model  string `json:"model"`
		Input  string `json:"input"`
		Stream *bool  `json:"stream"`
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF || in.Stream == nil || !*in.Stream || strings.TrimSpace(in.Input) == "" {
		return "", "", invalid("invalid_request")
	}
	if in.Model != "gemini-2.5-pro" && in.Model != "gemini-2.5-flash" {
		return "", "", invalid("invalid_request")
	}
	return in.Input, in.Model, nil
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
func nilTransport(t offlineTransport) bool {
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
