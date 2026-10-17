package antigravity

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

const authResponseLimit = 64 << 10

func (c *Connector) authenticateRefresh(ctx context.Context, services core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	if services.Credentials == nil {
		return core.AuthResult{}, unauthenticated()
	}
	secret, err := services.Credentials.Get(ctx, "oauth")
	if err != nil || ctx.Err() != nil {
		if ctx.Err() != nil {
			return core.AuthResult{}, contextError(ctx.Err())
		}
		return core.AuthResult{}, unauthenticated()
	}
	prior, valid := parseOAuthCredentials(secret, true)
	if !valid {
		return core.AuthResult{}, unauthenticated()
	}
	transport, ok := offlineTestTransport(services.Transport)
	if !ok {
		return core.AuthResult{}, unavailable()
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {prior.RefreshToken}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth.synthetic.invalid/token", strings.NewReader(form.Encode()))
	if err != nil {
		return core.AuthResult{}, unavailable()
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := transport.Do(req)
	if err != nil {
		closeResponse(resp)
		if ctx.Err() != nil {
			return core.AuthResult{}, contextError(ctx.Err())
		}
		return core.AuthResult{}, unavailable()
	}
	if resp == nil || resp.Body == nil {
		return core.AuthResult{}, unavailable()
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, authResponseLimit+1))
	if err != nil || len(data) > authResponseLimit {
		return core.AuthResult{}, invalidAuthResponse()
	}
	if ctx.Err() != nil {
		return core.AuthResult{}, contextError(ctx.Err())
	}
	if resp.StatusCode != http.StatusOK {
		return core.AuthResult{}, unauthenticated()
	}
	if !uniqueJSON(data) {
		return core.AuthResult{}, invalidAuthResponse()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return core.AuthResult{}, invalidAuthResponse()
	}
	for key := range fields {
		if key != "access_token" && key != "refresh_token" && key != "expires_in" {
			return core.AuthResult{}, invalidAuthResponse()
		}
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&result) != nil || d.Decode(new(any)) != io.EOF || strings.TrimSpace(result.AccessToken) == "" || strings.ContainsAny(result.AccessToken, "\r\n") || (fields["refresh_token"] != nil && (strings.TrimSpace(result.RefreshToken) == "" || strings.ContainsAny(result.RefreshToken, "\r\n"))) || result.ExpiresIn <= 0 || result.ExpiresIn > int64((time.Duration(1<<63-1))/time.Second) {
		return core.AuthResult{}, invalidAuthResponse()
	}
	if ctx.Err() != nil {
		return core.AuthResult{}, contextError(ctx.Err())
	}
	refreshToken := prior.RefreshToken
	if fields["refresh_token"] != nil {
		refreshToken = result.RefreshToken
	}
	expires := time.Now().Add(time.Duration(result.ExpiresIn) * time.Second).UTC()
	bundle, err := json.Marshal(oauthCredentials{AccessToken: result.AccessToken, RefreshToken: refreshToken})
	if err != nil {
		return core.AuthResult{}, invalidAuthResponse()
	}
	return core.AuthResult{Supported: true, Credentials: map[string][]byte{"oauth": bundle}, CredentialExpiresAt: &expires}, nil
}

func invalidAuthResponse() *core.GatewayError {
	return &core.GatewayError{Code: "invalid_response", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "Antigravity authentication response is invalid"}
}
