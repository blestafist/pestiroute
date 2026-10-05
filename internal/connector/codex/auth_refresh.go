package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

func (c *Connector) authenticateRefresh(ctx context.Context, services core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	if services.Credentials == nil {
		return core.AuthResult{}, connectorError("auth_invalid_credentials", core.CategoryPermissionDenied, "Authentication credentials are unavailable")
	}
	priorBytes, err := services.Credentials.Get(ctx, "oauth")
	if err != nil {
		if ctx.Err() != nil {
			return core.AuthResult{}, connectorError("auth_cancelled", core.CategoryUnavailable, "Authentication request was cancelled")
		}
		return core.AuthResult{}, connectorError("auth_invalid_credentials", core.CategoryPermissionDenied, "Authentication credentials are unavailable")
	}
	bundle, err := decodeOAuthBundle(priorBytes)
	if err != nil {
		return core.AuthResult{}, connectorError("auth_invalid_credentials", core.CategoryPermissionDenied, "Authentication credentials are invalid")
	}
	if bundle.RefreshToken == "" {
		return core.AuthResult{}, connectorError("auth_missing_refresh_token", core.CategoryPermissionDenied, "Refresh credential is unavailable")
	}
	c.mu.Lock()
	baseURL := c.authURL
	c.mu.Unlock()
	if services.Transport == nil {
		return core.AuthResult{}, connectorError("auth_transport_unavailable", core.CategoryUnavailable, "Authentication transport is unavailable")
	}
	if baseURL == "" {
		baseURL = defaultAuthURL
	}
	base, err := url.Parse(baseURL)
	if err != nil || !validAuthBaseURL(base) {
		return core.AuthResult{}, connectorError("auth_transport_unavailable", core.CategoryUnavailable, "Authentication endpoint is invalid")
	}
	base.Path = strings.TrimRight(base.Path, "/") + authTokenPath
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {bundle.RefreshToken},
		"client_id":     {deviceClientID},
	}
	requestCtx, cancel := context.WithTimeout(ctx, authStartTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, base.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return core.AuthResult{}, connectorError("auth_request_failed", core.CategoryUnavailable, "Authentication request could not be created")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
	if resp.StatusCode != http.StatusOK {
		return core.AuthResult{}, authRejectedError(resp.StatusCode)
	}
	fields, err := scanJSONObject(data, "access_token", "refresh_token", "id_token", "expires_in", "token_type")
	if err != nil {
		return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
	}
	var provider struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	expiresIn, lifetimeErr := tokenLifetime(fields)
	if err := json.Unmarshal(data, &provider); err != nil || provider.AccessToken == "" || lifetimeErr != nil || (fields["refresh_token"] != nil && provider.RefreshToken == "") {
		return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
	}
	identity, err := accountIDFromTokens(provider.IDToken, provider.AccessToken)
	if err != nil || identity == "" {
		return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
	}
	if identity != bundle.AccountID {
		return core.AuthResult{}, connectorError("scope_mismatch", core.CategoryPermissionDenied, "Authentication identity does not match configured target")
	}
	refreshToken := bundle.RefreshToken
	if fields["refresh_token"] != nil {
		refreshToken = provider.RefreshToken
	}
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second).UTC()
	encoded, err := encodeOAuthBundle(oauthBundle{
		Version: oauthBundleVersion, AccessToken: provider.AccessToken, RefreshToken: refreshToken,
		AccountID: bundle.AccountID, ExpiresAt: expiresAt,
	})
	if err != nil {
		return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
	}
	return core.AuthResult{Supported: true, Credentials: map[string][]byte{"oauth": encoded}, CredentialExpiresAt: &expiresAt}, nil
}
