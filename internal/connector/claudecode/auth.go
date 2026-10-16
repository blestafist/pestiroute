package claudecode

import (
	"bytes"
	"context"
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

const authBodyLimit = 64 << 10

type oauthContinuation struct {
	Verifier      string `json:"verifier"`
	ExpectedState string `json:"state"`
	RedirectURI   string `json:"redirect_uri"`
}

type oauthCredentials struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
}

func validOAuthConfig(cfg oauthConfig) bool {
	a, err := url.Parse(cfg.AuthorizationEndpoint)
	if err != nil || !syntheticEndpoint(a) {
		return false
	}
	t, err := url.Parse(cfg.TokenEndpoint)
	if err != nil || !syntheticEndpoint(t) {
		return false
	}
	r, err := url.Parse(cfg.RedirectURI)
	return err == nil && r.IsAbs() && r.Host != "" && r.User == nil && r.RawQuery == "" && !r.ForceQuery && r.Fragment == "" && cfg.ClientID != "" && cfg.Scope != ""
}

// Synthetic endpoints use HTTPS under the reserved .invalid domain to prevent provider dispatch.
func syntheticEndpoint(u *url.URL) bool {
	if u == nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Hostname() == "" || u.Scheme != "https" {
		return false
	}
	return strings.HasSuffix(strings.ToLower(u.Hostname()), ".invalid")
}

func (c *Connector) authStart(ctx context.Context, cfg oauthConfig) (core.AuthResult, *core.GatewayError) {
	if ge := contextError(ctx); ge != nil {
		return core.AuthResult{}, ge
	}
	verifierBytes := make([]byte, 32)
	stateBytes := make([]byte, 32)
	c.mu.Lock()
	if _, err := io.ReadFull(c.entropy, verifierBytes); err != nil {
		c.mu.Unlock()
		return authFailure("auth_entropy_failed", core.CategoryInternal)
	}
	if _, err := io.ReadFull(c.entropy, stateBytes); err != nil {
		c.mu.Unlock()
		return authFailure("auth_entropy_failed", core.CategoryInternal)
	}
	c.mu.Unlock()
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	state := base64.RawURLEncoding.EncodeToString(stateBytes)
	challengeBytes := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])
	continuation, err := json.Marshal(oauthContinuation{Verifier: verifier, ExpectedState: state, RedirectURI: cfg.RedirectURI})
	if err != nil {
		return authFailure("auth_invalid_state", core.CategoryInvalidRequest)
	}
	authorize, _ := url.Parse(cfg.AuthorizationEndpoint)
	query := authorize.Query()
	query.Set("response_type", "code")
	query.Set("client_id", cfg.ClientID)
	query.Set("redirect_uri", cfg.RedirectURI)
	query.Set("scope", cfg.Scope)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("state", state)
	authorize.RawQuery = query.Encode()
	return core.AuthResult{Supported: true, State: string(continuation), NextAction: "continue", UserAction: &core.AuthUserAction{VerificationURI: authorize.String()}}, nil
}

func (c *Connector) authContinue(ctx context.Context, cfg oauthConfig, req core.AuthRequest, services core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	if len(req.State) == 0 {
		return authFailure("auth_invalid_state", core.CategoryInvalidRequest)
	}
	var continuation oauthContinuation
	if json.Unmarshal(req.State, &continuation) != nil || continuation.Verifier == "" || continuation.ExpectedState == "" || continuation.RedirectURI != cfg.RedirectURI {
		return authFailure("auth_invalid_state", core.CategoryInvalidRequest)
	}
	code, state, ok := parseCallback(req.CallbackInput, continuation.RedirectURI)
	if !ok || state != continuation.ExpectedState {
		return authFailure("auth_invalid_callback", core.CategoryInvalidRequest)
	}
	if services.Transport == nil {
		return authFailure("auth_transport_unavailable", core.CategoryUnavailable)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {cfg.ClientID}, "code": {code}, "redirect_uri": {continuation.RedirectURI}, "code_verifier": {continuation.Verifier}}
	return c.tokenExchange(ctx, cfg, services.Transport, form)
}

func parseCallback(raw, redirect string) (string, string, bool) {
	if len(raw) == 0 || len(raw) > 4096 || strings.Contains(raw, "#") {
		return "", "", false
	}
	for i := range raw {
		if raw[i] < 0x20 || raw[i] > 0x7e {
			return "", "", false
		}
	}
	callback, err := url.Parse(raw)
	_, cfgErr := url.Parse(redirect)
	callbackScheme, callbackAuthority, callbackPath, callbackRawOK := rawRedirectParts(raw)
	configScheme, configAuthority, configPath, configRawOK := rawRedirectParts(redirect)
	if err != nil || cfgErr != nil || !callbackRawOK || !configRawOK || !callback.IsAbs() || callback.User != nil || callback.Fragment != "" || callbackScheme != configScheme || callbackAuthority != configAuthority || callbackPath != configPath || callback.RawQuery == "" {
		return "", "", false
	}
	var code, state string
	seen := map[string]bool{}
	for part := range strings.SplitSeq(callback.RawQuery, "&") {
		if part == "" {
			return "", "", false
		}
		k, v, found := strings.Cut(part, "=")
		if !found {
			return "", "", false
		}
		key, err1 := url.QueryUnescape(k)
		value, err2 := url.QueryUnescape(v)
		if err1 != nil || err2 != nil || seen[key] {
			return "", "", false
		}
		seen[key] = true
		if value == "" {
			return "", "", false
		}
		switch key {
		case "code":
			code = value
		case "state":
			state = value
		default:
			return "", "", false
		}
	}
	return code, state, code != "" && state != "" && len(seen) == 2
}

func rawRedirectParts(value string) (scheme, authority, path string, ok bool) {
	colon := strings.IndexByte(value, ':')
	if colon <= 0 || !strings.HasPrefix(value[colon+1:], "//") {
		return "", "", "", false
	}
	rest := value[colon+3:]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	if end == 0 {
		return "", "", "", false
	}
	pathEnd := len(rest)
	if q := strings.IndexByte(rest[end:], '?'); q >= 0 {
		pathEnd = end + q
	}
	if hash := strings.IndexByte(rest[end:], '#'); hash >= 0 && end+hash < pathEnd {
		pathEnd = end + hash
	}
	return value[:colon], rest[:end], rest[end:pathEnd], true
}

func (c *Connector) authRefresh(ctx context.Context, cfg oauthConfig, services core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	if services.Credentials == nil || services.Transport == nil {
		return authFailure("auth_credentials_unavailable", core.CategoryUnauthenticated)
	}
	encoded, err := services.Credentials.Get(ctx, "oauth")
	if ctx.Err() != nil {
		return authContextFailure(ctx.Err())
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return authFailure("auth_timeout", core.CategoryTimeout)
		}
		if errors.Is(err, context.Canceled) {
			return authFailure("auth_cancelled", core.CategoryCancelled)
		}
		return authFailure("auth_credentials_unavailable", core.CategoryUnauthenticated)
	}
	var current oauthCredentials
	if json.Unmarshal(encoded, &current) != nil || current.RefreshToken == "" {
		return authFailure("auth_credentials_invalid", core.CategoryUnauthenticated)
	}
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {cfg.ClientID}, "refresh_token": {current.RefreshToken}}
	return c.tokenExchange(ctx, cfg, services.Transport, form)
}

func (c *Connector) tokenExchange(ctx context.Context, cfg oauthConfig, transport core.HTTPDoer, form url.Values) (core.AuthResult, *core.GatewayError) {
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, cfg.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return authFailure("auth_request_failed", core.CategoryInternal)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if requestCtx.Err() != nil {
		return authContextFailure(requestCtx.Err())
	}
	resp, err := transport.Do(req)
	if err != nil {
		if requestCtx.Err() != nil {
			return authContextFailure(requestCtx.Err())
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return authFailure("auth_timeout", core.CategoryTimeout)
		}
		if errors.Is(err, context.Canceled) {
			return authFailure("auth_cancelled", core.CategoryCancelled)
		}
		return authFailure("auth_transport_failed", core.CategoryUnavailable)
	}
	if resp == nil || resp.Body == nil {
		return authFailure("auth_transport_failed", core.CategoryUnavailable)
	}
	if requestCtx.Err() != nil {
		_ = resp.Body.Close()
		return authContextFailure(requestCtx.Err())
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return authProviderFailure(resp.StatusCode)
	}
	var closeOnce sync.Once
	closeBody := func() { closeOnce.Do(func() { _ = resp.Body.Close() }) }
	defer closeBody()
	stopBodyWatcher := make(chan struct{})
	bodyWatcherDone := make(chan struct{})
	go func() {
		defer close(bodyWatcherDone)
		select {
		case <-requestCtx.Done():
			closeBody()
		case <-stopBodyWatcher:
		}
	}()
	body, bodyErr := io.ReadAll(io.LimitReader(resp.Body, authBodyLimit+1))
	close(stopBodyWatcher)
	<-bodyWatcherDone
	if requestCtx.Err() != nil {
		return authContextFailure(requestCtx.Err())
	}
	if bodyErr != nil || len(body) > authBodyLimit {
		return authFailure("auth_invalid_response", core.CategoryUnavailable)
	}
	var token tokenResponse
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&token) != nil || decoder.Decode(new(any)) != io.EOF || token.AccessToken == "" || token.RefreshToken == "" || token.ExpiresIn <= 0 || token.ExpiresIn > int64((time.Duration(1<<63-1))/time.Second) {
		return authFailure("auth_invalid_response", core.CategoryUnavailable)
	}
	now := c.clock()
	expiry := now.Add(time.Duration(token.ExpiresIn) * time.Second)
	if !expiry.After(now) {
		return authFailure("auth_invalid_response", core.CategoryUnavailable)
	}
	bundle, err := json.Marshal(oauthCredentials{AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, ExpiresAt: expiry})
	if err != nil {
		return authFailure("auth_invalid_response", core.CategoryInternal)
	}
	return core.AuthResult{Supported: true, Credentials: map[string][]byte{"oauth": bundle}, CredentialExpiresAt: &expiry}, nil
}

func authFailure(code string, category core.ErrorCategory) (core.AuthResult, *core.GatewayError) {
	return core.AuthResult{}, &core.GatewayError{Code: code, Category: category, Message: "Authentication request failed"}
}

func authContextFailure(err error) (core.AuthResult, *core.GatewayError) {
	if errors.Is(err, context.DeadlineExceeded) {
		return authFailure("auth_timeout", core.CategoryTimeout)
	}
	return authFailure("auth_cancelled", core.CategoryCancelled)
}

func authProviderFailure(status int) (core.AuthResult, *core.GatewayError) {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusBadRequest:
		return authFailure("auth_provider_rejected", core.CategoryUnauthenticated)
	case status == http.StatusTooManyRequests:
		return authFailure("auth_provider_rate_limited", core.CategoryRateLimited)
	case status >= http.StatusInternalServerError && status <= 599:
		return authFailure("auth_provider_unavailable", core.CategoryUnavailable)
	default:
		return authFailure("auth_provider_rejected", core.CategoryUnavailable)
	}
}
