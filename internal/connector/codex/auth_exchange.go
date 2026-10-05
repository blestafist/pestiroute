package codex

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

const authTokenPath = "/oauth/token"

func (c *Connector) exchangeDeviceAuthorizationCode(ctx context.Context, code, verifier string, services core.InvocationServices) (core.AuthResult, *core.GatewayError) {
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
	base.Path = strings.TrimRight(base.Path, "/") + authTokenPath
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"https://auth.openai.com/deviceauth/callback"},
		"client_id":     {deviceClientID},
		"code_verifier": {verifier},
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
		return core.AuthResult{}, connectorError("auth_rejected", core.CategoryUnavailable, "Authentication provider rejected the request")
	}
	if _, err := scanJSONObject(data, "access_token", "refresh_token", "id_token", "expires_in", "token_type"); err != nil {
		return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
	}
	var provider struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(data, &provider); err != nil || provider.AccessToken == "" || provider.RefreshToken == "" ||
		!strings.EqualFold(provider.TokenType, "Bearer") || provider.ExpiresIn <= 0 || provider.ExpiresIn > math.MaxInt64/int64(time.Second) {
		return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
	}
	accountID, err := accountIDFromTokens(provider.IDToken, provider.AccessToken)
	if err != nil || accountID == "" {
		return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
	}
	c.mu.Lock()
	configuredAccount := c.accountID
	c.mu.Unlock()
	if accountID != configuredAccount {
		return core.AuthResult{}, connectorError("scope_mismatch", core.CategoryPermissionDenied, "Authentication identity does not match configured target")
	}
	expiresAt := time.Now().Add(time.Duration(provider.ExpiresIn) * time.Second).UTC()
	encoded, err := encodeOAuthBundle(oauthBundle{
		Version: oauthBundleVersion, AccessToken: provider.AccessToken, RefreshToken: provider.RefreshToken,
		AccountID: accountID, ExpiresAt: expiresAt,
	})
	if err != nil {
		return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
	}
	return core.AuthResult{
		Supported: true, Credentials: map[string][]byte{"oauth": encoded}, CredentialExpiresAt: &expiresAt,
	}, nil
}
