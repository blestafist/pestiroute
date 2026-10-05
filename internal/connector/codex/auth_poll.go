package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

func (c *Connector) authenticateContinue(ctx context.Context, state string, services core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	var continuation deviceAuthContinuation
	if state == "" || json.Unmarshal([]byte(state), &continuation) != nil ||
		continuation.ExpiresAt.IsZero() {
		return core.AuthResult{}, connectorError("auth_invalid_state", core.CategoryInvalidRequest, "Authentication continuation state is invalid")
	}
	if !time.Now().Before(continuation.ExpiresAt) {
		return core.AuthResult{}, connectorError("auth_expired", core.CategoryUnavailable, "Authentication session has expired")
	}
	if continuation.AuthorizationCode != "" || continuation.CodeVerifier != "" {
		if continuation.AuthorizationCode == "" || continuation.CodeVerifier == "" || continuation.DeviceCode != "" {
			return core.AuthResult{}, connectorError("auth_invalid_state", core.CategoryInvalidRequest, "Authentication continuation state is invalid")
		}
		return c.exchangeDeviceAuthorizationCode(ctx, continuation.AuthorizationCode, continuation.CodeVerifier, services)
	}
	if continuation.DeviceCode == "" ||
		continuation.UserCode == "" || !safeUserCode(continuation.UserCode) || !safeVerificationURI(continuation.VerificationURI) ||
		continuation.Interval < time.Second {
		return core.AuthResult{}, connectorError("auth_invalid_state", core.CategoryInvalidRequest, "Authentication continuation state is invalid")
	}
	if !continuation.LastPolledAt.IsZero() {
		wait := time.Until(continuation.LastPolledAt.Add(continuation.Interval))
		if wait > 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			expiresIn := time.Until(continuation.ExpiresAt)
			if expiresIn <= wait {
				expiryTimer := time.NewTimer(expiresIn)
				defer expiryTimer.Stop()
				select {
				case <-ctx.Done():
					return core.AuthResult{}, connectorError("auth_cancelled", core.CategoryUnavailable, "Authentication request was cancelled")
				case <-expiryTimer.C:
					return core.AuthResult{}, connectorError("auth_expired", core.CategoryUnavailable, "Authentication session has expired")
				case <-timer.C:
				}
			} else {
				select {
				case <-ctx.Done():
					return core.AuthResult{}, connectorError("auth_cancelled", core.CategoryUnavailable, "Authentication request was cancelled")
				case <-timer.C:
				}
			}
		}
	}
	if !time.Now().Before(continuation.ExpiresAt) {
		return core.AuthResult{}, connectorError("auth_expired", core.CategoryUnavailable, "Authentication session has expired")
	}
	if err := ctx.Err(); err != nil {
		return core.AuthResult{}, connectorError("auth_cancelled", core.CategoryUnavailable, "Authentication request was cancelled")
	}
	if services.Transport == nil {
		return core.AuthResult{}, connectorError("auth_transport_unavailable", core.CategoryUnavailable, "Authentication transport is unavailable")
	}
	c.mu.Lock()
	baseURL := c.authURL
	c.mu.Unlock()
	if baseURL == "" {
		baseURL = defaultAuthURL
	}
	base, err := url.Parse(baseURL)
	if err != nil || !validAuthBaseURL(base) {
		return core.AuthResult{}, connectorError("auth_transport_unavailable", core.CategoryUnavailable, "Authentication endpoint is invalid")
	}
	base.Path = strings.TrimRight(base.Path, "/") + authPollPath
	body, _ := json.Marshal(struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
	}{DeviceAuthID: continuation.DeviceCode, UserCode: continuation.UserCode})
	requestCtx, cancel := context.WithTimeout(ctx, authStartTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, base.String(), bytes.NewReader(body))
	if err != nil {
		return core.AuthResult{}, connectorError("auth_request_failed", core.CategoryUnavailable, "Authentication request could not be created")
	}
	req.Header.Set("Content-Type", "application/json")
	pollStartedAt := time.Now()
	resp, err := doAuthNoRedirect(services.Transport, req)
	if err != nil {
		if requestCtx.Err() != nil {
			return core.AuthResult{}, connectorError("auth_cancelled", core.CategoryUnavailable, "Authentication request was cancelled")
		}
		return core.AuthResult{}, connectorError("auth_request_failed", core.CategoryUnavailable, "Authentication request failed")
	}
	if resp == nil || resp.Body == nil {
		return core.AuthResult{}, connectorError("auth_request_failed", core.CategoryUnavailable, "Authentication request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, authStartLimit+1))
	if err != nil || len(data) > authStartLimit {
		return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
	}
	if requestCtx.Err() != nil {
		return core.AuthResult{}, connectorError("auth_cancelled", core.CategoryUnavailable, "Authentication request was cancelled")
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return pendingPollResult(continuation, pollStartedAt), nil
	}
	if resp.StatusCode != http.StatusOK {
		return core.AuthResult{}, connectorError("auth_rejected", core.CategoryUnavailable, "Authentication provider rejected the request")
	}
	var provider struct {
		Error             string          `json:"error"`
		AuthorizationCode string          `json:"authorization_code"`
		CodeVerifier      string          `json:"code_verifier"`
		Interval          json.RawMessage `json:"interval"`
	}
	if err := json.Unmarshal(data, &provider); err != nil {
		return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
	}
	switch provider.Error {
	case "authorization_pending":
		return pendingPollResult(continuation, pollStartedAt), nil
	case "slow_down":
		interval, err := parseDevicePollInterval(provider.Interval)
		if err != nil {
			return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
		}
		continuation.Interval = interval
		return pendingPollResult(continuation, pollStartedAt), nil
	case "":
		if provider.AuthorizationCode == "" || provider.CodeVerifier == "" {
			return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
		}
		continuation.AuthorizationCode = provider.AuthorizationCode
		continuation.CodeVerifier = provider.CodeVerifier
		continuation.DeviceCode = ""
		continuation.DeviceAuthID = ""
		continuation.UserCode = ""
		continuation.VerificationURI = ""
		continuation.Interval = 0
		continuation.LastPolledAt = time.Time{}
		encoded, err := json.Marshal(continuation)
		if err != nil {
			return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
		}
		return core.AuthResult{Supported: true, NextAction: "continue", State: string(encoded)}, nil
	default:
		return core.AuthResult{}, connectorError("auth_rejected", core.CategoryUnavailable, "Authentication provider rejected the request")
	}
}

func pendingPollResult(continuation deviceAuthContinuation, polledAt time.Time) core.AuthResult {
	continuation.LastPolledAt = polledAt
	encoded, _ := json.Marshal(continuation)
	return core.AuthResult{
		Supported: true, NextAction: "continue", State: string(encoded),
		UserAction: &core.AuthUserAction{VerificationURI: continuation.VerificationURI, UserCode: continuation.UserCode, PollInterval: continuation.Interval},
	}
}
