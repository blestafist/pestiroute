package antigravity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

const authResponseLimit = 64 << 10

type authContinuation struct {
	Verifier string `json:"verifier"`
	State    string `json:"state"`
}

func readAuthResponse(ctx context.Context, body io.ReadCloser) ([]byte, error) {
	var closeOnce sync.Once
	closeBody := func() { closeOnce.Do(func() { _ = body.Close() }) }
	stopWatcher, watcherDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			closeBody()
		case <-stopWatcher:
		}
	}()
	data, err := io.ReadAll(io.LimitReader(body, authResponseLimit+1))
	close(stopWatcher)
	<-watcherDone
	closeBody()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || len(data) > authResponseLimit {
		return nil, invalidAuthResponse()
	}
	return data, nil
}

func (c *Connector) authenticateStart(ctx context.Context, cfg oauthConfig) (core.AuthResult, *core.GatewayError) {
	if ctx.Err() != nil {
		return core.AuthResult{}, contextError(ctx.Err())
	}
	verifierBytes, stateBytes := make([]byte, 32), make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, verifierBytes); err != nil {
		return core.AuthResult{}, unavailable()
	}
	if _, err := io.ReadFull(rand.Reader, stateBytes); err != nil {
		return core.AuthResult{}, unavailable()
	}
	verifier, state := base64.RawURLEncoding.EncodeToString(verifierBytes), base64.RawURLEncoding.EncodeToString(stateBytes)
	hash := sha256.Sum256([]byte(verifier))
	continuation, _ := json.Marshal(authContinuation{Verifier: verifier, State: state})
	endpoint, _ := url.Parse(cfg.AuthorizationEndpoint)
	query := endpoint.Query()
	query.Set("response_type", "code")
	query.Set("client_id", cfg.ClientID)
	query.Set("redirect_uri", cfg.RedirectURI)
	query.Set("scope", strings.Join(cfg.Scopes, " "))
	query.Set("access_type", "offline")
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(hash[:]))
	query.Set("code_challenge_method", "S256")
	query.Set("state", state)
	endpoint.RawQuery = query.Encode()
	return core.AuthResult{Supported: true, State: string(continuation), NextAction: "continue", UserAction: &core.AuthUserAction{VerificationURI: endpoint.String()}}, nil
}

func (c *Connector) authenticateContinue(ctx context.Context, cfg oauthConfig, req core.AuthRequest, services core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	var continuation authContinuation
	d := json.NewDecoder(bytes.NewReader(req.State))
	d.DisallowUnknownFields()
	if !uniqueJSON(req.State) || d.Decode(&continuation) != nil || d.Decode(new(any)) != io.EOF || continuation.Verifier == "" || continuation.State == "" || len(continuation.Verifier) > 128 || len(continuation.State) > 128 {
		return core.AuthResult{}, invalid("invalid_auth_state")
	}
	callback, err := url.Parse(req.CallbackInput)
	redirect, _ := url.Parse(cfg.RedirectURI)
	if err != nil || callback == nil || redirect == nil || !callback.IsAbs() || callback.Scheme != redirect.Scheme || callback.Host != redirect.Host || callback.EscapedPath() != redirect.EscapedPath() || callback.User != nil || callback.Fragment != "" || len(req.CallbackInput) > 4096 {
		return core.AuthResult{}, invalid("invalid_auth_callback")
	}
	values, err := url.ParseQuery(callback.RawQuery)
	if err != nil || len(values) != 2 || len(values["code"]) != 1 || len(values["state"]) != 1 || values.Get("code") == "" || values.Get("state") != continuation.State {
		return core.AuthResult{}, invalid("invalid_auth_callback")
	}
	if services.Transport == nil {
		return core.AuthResult{}, unavailable()
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {cfg.ClientID}, "code": {values.Get("code")}, "redirect_uri": {cfg.RedirectURI}, "code_verifier": {continuation.Verifier}}
	if cfg.ClientSecret != "" {
		form.Set("client_secret", cfg.ClientSecret)
	}
	return c.exchangeAuthCode(ctx, cfg.TokenEndpoint, services.Transport, form)
}

func (c *Connector) exchangeAuthCode(ctx context.Context, endpoint string, transport core.HTTPDoer, form url.Values) (core.AuthResult, *core.GatewayError) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return core.AuthResult{}, invalidAuthResponse()
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
	if ctx.Err() != nil {
		closeResponse(resp)
		return core.AuthResult{}, contextError(ctx.Err())
	}
	data, err := readAuthResponse(ctx, resp.Body)
	if err != nil {
		var gatewayError *core.GatewayError
		if errors.As(err, &gatewayError) {
			return core.AuthResult{}, gatewayError
		}
		return core.AuthResult{}, contextError(ctx.Err())
	}
	if resp.StatusCode != http.StatusOK {
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest:
			return core.AuthResult{}, unauthenticated()
		case resp.StatusCode == http.StatusTooManyRequests:
			return core.AuthResult{}, &core.GatewayError{Code: "auth_rate_limited", Category: core.CategoryRateLimited, RetryDisposition: core.RetryUnknown, Message: "Antigravity authentication is rate limited"}
		case resp.StatusCode >= http.StatusInternalServerError:
			return core.AuthResult{}, unavailable()
		default:
			return core.AuthResult{}, invalidAuthResponse()
		}
	}
	if !uniqueJSON(data) {
		return core.AuthResult{}, invalidAuthResponse()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return core.AuthResult{}, invalidAuthResponse()
	}
	for key := range fields {
		if key != "access_token" && key != "refresh_token" && key != "expires_in" && key != "token_type" {
			return core.AuthResult{}, invalidAuthResponse()
		}
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&token) != nil || d.Decode(new(any)) != io.EOF || token.ExpiresIn <= 0 || token.ExpiresIn > int64(time.Duration(1<<63-1)/time.Second) {
		return core.AuthResult{}, invalidAuthResponse()
	}
	candidate, valid := validateOAuthCandidate(token.AccessToken, token.RefreshToken, fields["refresh_token"] != nil, false)
	if !valid {
		return core.AuthResult{}, invalidAuthResponse()
	}
	if strings.TrimSpace(token.TokenType) == "" && fields["token_type"] != nil {
		return core.AuthResult{}, invalidAuthResponse()
	}
	expires := time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).UTC()
	bundle, _ := json.Marshal(candidate)
	if ctx.Err() != nil {
		return core.AuthResult{}, contextError(ctx.Err())
	}
	return core.AuthResult{Supported: true, Credentials: map[string][]byte{"oauth": bundle}, CredentialExpiresAt: &expires}, nil
}

func (c *Connector) authenticateRefresh(ctx context.Context, cfg oauthConfig, services core.InvocationServices) (core.AuthResult, *core.GatewayError) {
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
	if services.Transport == nil {
		return core.AuthResult{}, unavailable()
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {prior.RefreshToken}}
	if cfg.ClientID != "" {
		form.Set("client_id", cfg.ClientID)
	}
	if cfg.ClientSecret != "" {
		form.Set("client_secret", cfg.ClientSecret)
	}
	endpoint := cfg.TokenEndpoint
	if endpoint == "" {
		endpoint = "https://oauth.synthetic.invalid/token"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return core.AuthResult{}, unavailable()
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := services.Transport.Do(req)
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
	if ctx.Err() != nil {
		closeResponse(resp)
		return core.AuthResult{}, contextError(ctx.Err())
	}
	data, err := readAuthResponse(ctx, resp.Body)
	if err != nil {
		var gatewayError *core.GatewayError
		if errors.As(err, &gatewayError) {
			return core.AuthResult{}, gatewayError
		}
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
		if key != "access_token" && key != "refresh_token" && key != "expires_in" && key != "token_type" {
			return core.AuthResult{}, invalidAuthResponse()
		}
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&result) != nil || d.Decode(new(any)) != io.EOF || result.ExpiresIn <= 0 || result.ExpiresIn > int64((time.Duration(1<<63-1))/time.Second) {
		return core.AuthResult{}, invalidAuthResponse()
	}
	if strings.TrimSpace(result.TokenType) == "" && fields["token_type"] != nil {
		return core.AuthResult{}, invalidAuthResponse()
	}
	if ctx.Err() != nil {
		return core.AuthResult{}, contextError(ctx.Err())
	}
	refreshToken := prior.RefreshToken
	if fields["refresh_token"] != nil {
		refreshToken = result.RefreshToken
	}
	candidate, valid := validateOAuthCandidate(result.AccessToken, refreshToken, true, true)
	if !valid {
		return core.AuthResult{}, invalidAuthResponse()
	}
	expires := time.Now().Add(time.Duration(result.ExpiresIn) * time.Second).UTC()
	bundle, err := json.Marshal(candidate)
	if err != nil {
		return core.AuthResult{}, invalidAuthResponse()
	}
	if ctx.Err() != nil {
		return core.AuthResult{}, contextError(ctx.Err())
	}
	return core.AuthResult{Supported: true, Credentials: map[string][]byte{"oauth": bundle}, CredentialExpiresAt: &expires}, nil
}

func invalidAuthResponse() *core.GatewayError {
	return &core.GatewayError{Code: "invalid_response", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "Antigravity authentication response is invalid"}
}
