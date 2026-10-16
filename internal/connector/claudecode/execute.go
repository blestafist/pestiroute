package claudecode

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

// offlineHTTPDoer is deliberately package-private: ordinary runtime transports
// cannot opt in to this candidate-only dispatch path.
type offlineHTTPDoer interface {
	core.HTTPDoer
	claudeCodeOfflineFakeOnly()
}

func (c *Connector) execute(ctx context.Context, req core.ExecutionRequest, scope core.AttemptScope, services core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	c.mu.Lock()
	ready := c.state == core.HealthReady
	c.mu.Unlock()
	if !ready {
		return core.ExecutionResponse{}, unavailableError()
	}
	if ge := contextError(ctx); ge != nil {
		return core.ExecutionResponse{}, ge
	}
	if req.Payload.Protocol != protocol {
		return core.ExecutionResponse{}, &core.GatewayError{Code: "unsupported_protocol", Category: core.CategoryUnsupportedFeature, Message: "Unsupported response protocol"}
	}
	if scope.Mode != core.ModeTranslation {
		return core.ExecutionResponse{}, &core.GatewayError{Code: "unsupported_mode", Category: core.CategoryUnsupportedFeature, Message: "Unsupported execution mode"}
	}
	transport, ok := services.Transport.(offlineHTTPDoer)
	if !ok || isNilOfflineTransport(transport) {
		return core.ExecutionResponse{}, &core.GatewayError{Code: "connector_unavailable", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "Offline Claude Code execution transport is unavailable"}
	}
	if services.Credentials == nil {
		return core.ExecutionResponse{}, executionError("credentials_unavailable", core.CategoryUnauthenticated, false, core.RetryUnsafe)
	}
	encoded, err := services.Credentials.Get(ctx, "oauth")
	if ctx.Err() != nil {
		return core.ExecutionResponse{}, contextError(ctx)
	}
	if err != nil {
		return core.ExecutionResponse{}, executionError("credentials_unavailable", core.CategoryUnauthenticated, false, core.RetryUnsafe)
	}
	var credentials oauthCredentials
	d := json.NewDecoder(bytes.NewReader(encoded))
	d.DisallowUnknownFields()
	if d.Decode(&credentials) != nil || d.Decode(new(any)) != io.EOF || credentials.AccessToken == "" || strings.ContainsAny(credentials.AccessToken, "\r\n") {
		return core.ExecutionResponse{}, executionError("credentials_invalid", core.CategoryUnauthenticated, false, core.RetryUnsafe)
	}
	translated, ge := translateRequest(req, credentials.AccessToken, "synthetic-beta")
	if ge != nil {
		return core.ExecutionResponse{}, ge
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, translated.URL, bytes.NewReader(translated.Body))
	if err != nil {
		return core.ExecutionResponse{}, executionError("invalid_request", core.CategoryInvalidRequest, false, core.RetryUnsafe)
	}
	httpReq.Header = translated.Header.Clone()
	if ge := contextError(ctx); ge != nil {
		return core.ExecutionResponse{}, ge
	}
	resp, err := transport.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return core.ExecutionResponse{}, contextError(ctx)
		}
		return core.ExecutionResponse{}, executionError("upstream_unavailable", core.CategoryUnavailable, false, core.RetryUnknown)
	}
	if resp == nil || resp.Body == nil {
		return core.ExecutionResponse{}, executionError("upstream_unavailable", core.CategoryUnavailable, false, core.RetryUnknown)
	}
	if ctx.Err() != nil {
		_ = resp.Body.Close()
		return core.ExecutionResponse{}, contextError(ctx)
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, authBodyLimit))
		_ = resp.Body.Close()
		return core.ExecutionResponse{}, responseStatusError(resp.StatusCode)
	}
	if !validSSEHeaders(resp.Header) {
		_ = resp.Body.Close()
		return core.ExecutionResponse{}, executionError("invalid_response", core.CategoryUnavailable, false, core.RetryUnknown)
	}
	return core.ExecutionResponse{Stream: newMessagesStream(resp.Body)}, nil
}

func isNilOfflineTransport(transport offlineHTTPDoer) bool {
	if transport == nil {
		return true
	}
	v := reflect.ValueOf(transport)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func validSSEHeaders(header http.Header) bool {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || mediaType != "text/event-stream" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) {
		return false
	}
	encoding := strings.TrimSpace(header.Get("Content-Encoding"))
	return encoding == "" || strings.EqualFold(encoding, "identity")
}

func responseStatusError(status int) *core.GatewayError {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return executionError("upstream_unauthorized", core.CategoryUnauthenticated, false, core.RetryUnsafe)
	case status == http.StatusTooManyRequests:
		return executionError("upstream_rate_limited", core.CategoryRateLimited, true, core.RetryUnknown)
	case status >= 500 && status <= 599:
		return executionError("upstream_unavailable", core.CategoryUnavailable, true, core.RetryUnknown)
	default:
		return executionError("upstream_unavailable", core.CategoryUnavailable, false, core.RetryUnknown)
	}
}

func executionError(code string, category core.ErrorCategory, retryable bool, disposition core.RetryDisposition) *core.GatewayError {
	return &core.GatewayError{Code: code, Category: category, Retryable: retryable, RetryDisposition: disposition, Message: "Claude Code execution failed"}
}
