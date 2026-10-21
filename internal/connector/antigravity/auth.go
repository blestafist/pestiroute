package antigravity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/blestafist/pestiroute/internal/core"
)

const authResponseLimit = 64 << 10

type authContinuation struct {
	Verifier    string `json:"verifier"`
	State       string `json:"state"`
	RedirectURI string `json:"redirect_uri"`
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
	redirectURI := cfg.RedirectURI
	if redirectURI == "" {
		var err error
		redirectURI, err = ephemeralLoopbackRedirectURI()
		if err != nil {
			return core.AuthResult{}, unavailable()
		}
	}
	hash := sha256.Sum256([]byte(verifier))
	continuation, _ := json.Marshal(authContinuation{Verifier: verifier, State: state, RedirectURI: redirectURI})
	endpoint, _ := url.Parse(cfg.AuthorizationEndpoint)
	query := endpoint.Query()
	query.Set("response_type", "code")
	query.Set("client_id", cfg.ClientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("scope", strings.Join(cfg.Scopes, " "))
	query.Set("access_type", "offline")
	query.Set("prompt", "consent")
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(hash[:]))
	query.Set("code_challenge_method", "S256")
	query.Set("state", state)
	endpoint.RawQuery = query.Encode()
	return core.AuthResult{Supported: true, State: string(continuation), NextAction: "continue", UserAction: &core.AuthUserAction{VerificationURI: endpoint.String()}}, nil
}

func (c *Connector) authenticateContinue(ctx context.Context, cfg oauthConfig, discoveryOrigin string, req core.AuthRequest, services core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	if requiresConfiguredClientSecret(cfg) {
		return core.AuthResult{}, unauthenticated()
	}
	var continuation authContinuation
	d := json.NewDecoder(bytes.NewReader(req.State))
	d.DisallowUnknownFields()
	if !uniqueJSON(req.State) || d.Decode(&continuation) != nil || d.Decode(new(any)) != io.EOF || continuation.Verifier == "" || continuation.State == "" || len(continuation.Verifier) > 128 || len(continuation.State) > 128 || !validRedirectURI(continuation.RedirectURI) {
		return core.AuthResult{}, invalid("invalid_auth_state")
	}
	if len(req.CallbackInput) > 4096 || strings.IndexFunc(req.CallbackInput, unicode.IsControl) >= 0 {
		return core.AuthResult{}, invalid("callback_malformed")
	}
	trimmed := strings.TrimSpace(req.CallbackInput)
	input := trimmed
	if input == "" {
		return core.AuthResult{}, invalid("invalid_auth_callback")
	}
	code := input
	parsed, parseErr := url.Parse(input)
	looksLikeURL := strings.Contains(input, "://") || parseErr == nil && parsed.Scheme != ""
	if looksLikeURL {
		if input != req.CallbackInput {
			return core.AuthResult{}, invalid("callback_malformed")
		}
		callback, err := url.Parse(input)
		redirect, _ := url.Parse(continuation.RedirectURI)
		if err != nil || callback == nil || redirect == nil || !callback.IsAbs() || callback.Opaque != "" || callback.User != nil || callback.Fragment != "" || strings.Contains(input, "#") {
			return core.AuthResult{}, invalid("callback_malformed")
		}
		if rawScheme(input) != rawScheme(continuation.RedirectURI) || rawAuthority(input) != rawAuthority(continuation.RedirectURI) || callback.EscapedPath() != redirect.EscapedPath() {
			return core.AuthResult{}, invalid("callback_redirect_mismatch")
		}
		values, err := url.ParseQuery(callback.RawQuery)
		if err != nil {
			return core.AuthResult{}, invalid("callback_malformed")
		}
		if _, exists := values["error"]; exists {
			return core.AuthResult{}, invalid("callback_error_param")
		}
		if len(values) > 6 {
			return core.AuthResult{}, invalid("callback_query_keys")
		}
		allowed := map[string]bool{"code": true, "state": true, "scope": true, "authuser": true, "prompt": true, "hd": true}
		for key, items := range values {
			if !allowed[key] || len(items) != 1 {
				return core.AuthResult{}, invalid("callback_query_keys")
			}
		}
		if len(values["code"]) != 1 || len(values["state"]) != 1 || values.Get("code") == "" {
			return core.AuthResult{}, invalid("callback_query_keys")
		}
		if values.Get("state") != continuation.State {
			return core.AuthResult{}, invalid("callback_state_mismatch")
		}
		code = values.Get("code")
	} else if !validPastedOAuthCode(input) {
		return core.AuthResult{}, invalid("callback_malformed")
	}
	if code == "" || strings.TrimSpace(code) == "" || len(code) > 4096 || strings.IndexFunc(code, unicode.IsControl) >= 0 {
		return core.AuthResult{}, invalid("invalid_auth_callback")
	}
	if services.Transport == nil {
		return core.AuthResult{}, unavailable()
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {cfg.ClientID}, "code": {code}, "redirect_uri": {continuation.RedirectURI}, "code_verifier": {continuation.Verifier}}
	if cfg.ClientSecret != "" {
		form.Set("client_secret", cfg.ClientSecret)
	}
	tokenEndpoint := cfg.TokenEndpoint
	if tokenEndpoint == "" {
		tokenEndpoint = defaultTokenEndpoint
	}
	return c.exchangeAuthCode(ctx, tokenEndpoint, discoveryOrigin, services.Transport, form)
}

func rawAuthority(value string) string {
	colon := strings.IndexByte(value, ':')
	if colon < 0 || !strings.HasPrefix(value[colon+1:], "//") {
		return ""
	}
	start := colon + 3
	end := strings.IndexAny(value[start:], "/?#")
	if end < 0 {
		return value[start:]
	}
	return value[start : start+end]
}

func rawScheme(value string) string {
	colon := strings.IndexByte(value, ':')
	if colon < 0 {
		return ""
	}
	return value[:colon]
}

func ephemeralLoopbackRedirectURI() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return "", err
	}
	return "http://localhost:" + strconv.Itoa(port) + "/callback", nil
}

func validPastedOAuthCode(code string) bool {
	if len(code) == 0 || len(code) > 512 || strings.Contains(code, "://") {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		if c < 0x21 || c > 0x7e || c == '?' || c == '#' || c == '&' || c == ':' {
			return false
		}
	}
	return true
}

func requiresConfiguredClientSecret(cfg oauthConfig) bool {
	return cfg.ClientID == defaultClientID && cfg.ClientSecret == ""
}

func (c *Connector) exchangeAuthCode(ctx context.Context, endpoint, discoveryOrigin string, transport core.HTTPDoer, form url.Values) (core.AuthResult, *core.GatewayError) {
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
		return core.AuthResult{}, authTransportError()
	}
	if resp == nil || resp.Body == nil {
		return core.AuthResult{}, authTransportError()
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
		return tokenHTTPError(resp.StatusCode)
	}
	if !uniqueJSON(data) {
		return core.AuthResult{}, invalidAuthResponse()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return core.AuthResult{}, invalidAuthResponse()
	}
	for key := range fields {
		if key != "access_token" && key != "refresh_token" && key != "expires_in" && key != "token_type" && key != "scope" && key != "id_token" {
			return core.AuthResult{}, invalidAuthResponse()
		}
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
		IDToken      string `json:"id_token"`
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
	if !validTokenMetadata(fields) || !strings.EqualFold(token.TokenType, "Bearer") {
		return core.AuthResult{}, invalidAuthResponse()
	}
	projectID, ge := discoverProject(ctx, discoveryOrigin, transport, token.AccessToken)
	if ge != nil {
		return core.AuthResult{}, ge
	}
	candidate.ProjectID = projectID
	expires := time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).UTC()
	bundle, _ := json.Marshal(candidate)
	if ctx.Err() != nil {
		return core.AuthResult{}, contextError(ctx.Err())
	}
	return core.AuthResult{Supported: true, Credentials: map[string][]byte{"oauth": bundle}, CredentialExpiresAt: &expires}, nil
}

func (c *Connector) authenticateRefresh(ctx context.Context, cfg oauthConfig, services core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	if requiresConfiguredClientSecret(cfg) {
		return core.AuthResult{}, unauthenticated()
	}
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
		endpoint = defaultTokenEndpoint
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
		return core.AuthResult{}, authTransportError()
	}
	if resp == nil || resp.Body == nil {
		return core.AuthResult{}, authTransportError()
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
		return tokenHTTPError(resp.StatusCode)
	}
	if !uniqueJSON(data) {
		return core.AuthResult{}, invalidAuthResponse()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return core.AuthResult{}, invalidAuthResponse()
	}
	for key := range fields {
		if key != "access_token" && key != "refresh_token" && key != "expires_in" && key != "token_type" && key != "scope" && key != "id_token" {
			return core.AuthResult{}, invalidAuthResponse()
		}
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
		IDToken      string `json:"id_token"`
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&result) != nil || d.Decode(new(any)) != io.EOF || result.ExpiresIn <= 0 || result.ExpiresIn > int64((time.Duration(1<<63-1))/time.Second) {
		return core.AuthResult{}, invalidAuthResponse()
	}
	if !validTokenMetadata(fields) || !strings.EqualFold(result.TokenType, "Bearer") {
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
	candidate.ProjectID = prior.ProjectID
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

func discoverProject(ctx context.Context, endpoint string, transport core.HTTPDoer, accessToken string) (string, *core.GatewayError) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body := []byte(`{"metadata":{"ideType":"IDE_UNSPECIFIED","platform":"PLATFORM_UNSPECIFIED","pluginType":"GEMINI"}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1internal:loadCodeAssist", bytes.NewReader(body))
	if err != nil {
		return "", invalidAuthResponse()
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := transport.Do(req)
	if err != nil {
		closeResponse(resp)
		if ctx.Err() != nil {
			return "", contextError(ctx.Err())
		}
		return "", authTransportError()
	}
	if resp == nil || resp.Body == nil {
		return "", authTransportError()
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8192))
		closeResponse(resp)
		return "", discoveryHTTPError(resp.StatusCode)
	}
	data, err := readAuthResponse(ctx, resp.Body)
	if err != nil {
		if ctx.Err() != nil {
			return "", contextError(ctx.Err())
		}
		var gatewayError *core.GatewayError
		if errors.As(err, &gatewayError) {
			return "", gatewayError
		}
		return "", invalidAuthResponse()
	}
	if !uniqueJSON(data) {
		return "", invalidAuthResponse()
	}
	var response struct {
		Project json.RawMessage `json:"cloudaicompanionProject"`
	}
	if json.Unmarshal(data, &response) != nil {
		return "", invalidAuthResponse()
	}
	if len(response.Project) == 0 || string(response.Project) == "null" {
		return "", &core.GatewayError{Code: "auth_project_missing", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "Authentication project data is missing"}
	}
	var projectID string
	if json.Unmarshal(response.Project, &projectID) != nil {
		var project struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(response.Project, &project) != nil {
			return "", invalidAuthResponse()
		}
		projectID = project.ID
	}
	if !projectPattern.MatchString(projectID) {
		return "", invalidAuthResponse()
	}
	return projectID, nil
}

func invalidAuthResponse() *core.GatewayError {
	return &core.GatewayError{Code: "auth_invalid_response", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "Authentication response is invalid"}
}

func validTokenMetadata(fields map[string]json.RawMessage) bool {
	if raw, ok := fields["scope"]; ok {
		var scope string
		if json.Unmarshal(raw, &scope) != nil || !validOAuthScope(scope) {
			return false
		}
	}
	if raw, ok := fields["id_token"]; ok {
		var idToken string
		if json.Unmarshal(raw, &idToken) != nil || idToken == "" || strings.IndexFunc(idToken, unicode.IsControl) >= 0 {
			return false
		}
	}
	return true
}

func validOAuthScope(scope string) bool {
	if scope == "" {
		return false
	}
	for _, token := range strings.Split(scope, " ") {
		if token == "" {
			return false
		}
		for i := 0; i < len(token); i++ {
			c := token[i]
			if !(c == 0x21 || c >= 0x23 && c <= 0x5b || c >= 0x5d && c <= 0x7e) {
				return false
			}
		}
	}
	return true
}

func authTransportError() *core.GatewayError {
	return &core.GatewayError{Code: "auth_transport_unavailable", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "Authentication transport is unavailable"}
}

func tokenHTTPError(status int) (core.AuthResult, *core.GatewayError) {
	return core.AuthResult{}, authHTTPError(status, "Authentication request failed")
}

func discoveryHTTPError(status int) *core.GatewayError {
	return authHTTPError(status, "Authentication discovery request failed")
}

func authHTTPError(status int, message string) *core.GatewayError {
	code, category := "auth_request_failed", core.CategoryUnavailable
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusBadRequest {
		code, category = "auth_rejected", core.CategoryUnauthenticated
	} else if status == http.StatusTooManyRequests {
		code, category = "auth_rate_limited", core.CategoryRateLimited
	}
	ge := &core.GatewayError{Code: code, Category: category, RetryDisposition: core.RetryUnknown, Message: message}
	if status >= 100 && status <= 599 {
		ge.OriginalError = fmt.Sprintf("HTTP status %03d", status)
	}
	return ge
}
