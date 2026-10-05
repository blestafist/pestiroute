package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

const (
	defaultAuthURL        = "https://auth.openai.com"
	authStartPath         = "/api/accounts/deviceauth/usercode"
	authPollPath          = "/api/accounts/deviceauth/token"
	authStartLimit        = 64 << 10
	authStartTimeout      = 30 * time.Second
	deviceClientID        = "app_EMoamEEZ73f0CkXaXp7hrann"
	defaultPollInterval   = 5 * time.Second
	maxDeviceAuthLifetime = 15 * time.Minute
)

var decimalSeconds = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

type deviceAuthContinuation struct {
	DeviceCode        string        `json:"device_code"`
	DeviceAuthID      string        `json:"device_auth_id,omitempty"`
	UserCode          string        `json:"user_code,omitempty"`
	VerificationURI   string        `json:"verification_uri,omitempty"`
	Interval          time.Duration `json:"interval,omitempty"`
	LastPolledAt      time.Time     `json:"last_polled_at"`
	AuthorizationCode string        `json:"authorization_code,omitempty"`
	CodeVerifier      string        `json:"code_verifier,omitempty"`
	ExpiresAt         time.Time     `json:"expires_at"`
}

type deviceAuthStartResponse struct {
	DeviceCode      string          `json:"device_code"`
	DeviceAuthID    string          `json:"device_auth_id"`
	UserCode        string          `json:"user_code"`
	VerificationURI json.RawMessage `json:"verification_uri"`
	Interval        json.RawMessage `json:"interval"`
	ExpiresIn       json.RawMessage `json:"expires_in"`
}

func (c *Connector) authenticateStart(ctx context.Context, services core.InvocationServices) (core.AuthResult, *core.GatewayError) {
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
	authBase := *base
	base.Path = strings.TrimRight(base.Path, "/") + authStartPath
	body, _ := json.Marshal(struct {
		ClientID string `json:"client_id"`
	}{ClientID: deviceClientID})
	requestCtx, cancel := context.WithTimeout(ctx, authStartTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, base.String(), bytes.NewReader(body))
	if err != nil {
		return core.AuthResult{}, connectorError("auth_request_failed", core.CategoryUnavailable, "Authentication request could not be created")
	}
	req.Header.Set("Content-Type", "application/json")
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
	if requestCtx.Err() != nil {
		return core.AuthResult{}, connectorError("auth_cancelled", core.CategoryUnavailable, "Authentication request was cancelled")
	}
	if resp.StatusCode != http.StatusOK {
		return core.AuthResult{}, connectorError("auth_rejected", core.CategoryUnavailable, "Authentication provider rejected the request")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, authStartLimit+1))
	if err != nil || len(data) > authStartLimit {
		return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
	}
	if requestCtx.Err() != nil {
		return core.AuthResult{}, connectorError("auth_cancelled", core.CategoryUnavailable, "Authentication request was cancelled")
	}
	var provider deviceAuthStartResponse
	if err := json.Unmarshal(data, &provider); err != nil {
		return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
	}
	deviceCode := provider.DeviceCode
	if deviceCode == "" {
		deviceCode = provider.DeviceAuthID
	}
	interval := defaultPollInterval
	if len(provider.Interval) != 0 {
		interval, err = parseDevicePollInterval(provider.Interval)
		if err != nil {
			return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
		}
	}
	expiresIn := maxDeviceAuthLifetime
	if len(provider.ExpiresIn) != 0 {
		var seconds int64
		if err := json.Unmarshal(provider.ExpiresIn, &seconds); err != nil || seconds <= 0 {
			return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
		}
		if seconds > int64(maxDeviceAuthLifetime/time.Second) {
			expiresIn = maxDeviceAuthLifetime
		} else {
			expiresIn = time.Duration(seconds) * time.Second
		}
	}
	verificationURI := ""
	if len(provider.VerificationURI) != 0 {
		if err := json.Unmarshal(provider.VerificationURI, &verificationURI); err != nil || !strings.HasPrefix(strings.TrimSpace(string(provider.VerificationURI)), `"`) {
			return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
		}
	} else {
		verificationBase := authBase
		verificationBase.Path = strings.TrimRight(verificationBase.Path, "/") + "/codex/device"
		verificationBase.RawPath = ""
		verificationURI = verificationBase.String()
	}
	if deviceCode == "" || !safeUserCode(provider.UserCode) || !safeVerificationURI(verificationURI) {
		return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
	}
	continuation, err := json.Marshal(deviceAuthContinuation{
		DeviceCode: deviceCode, DeviceAuthID: provider.DeviceAuthID, UserCode: provider.UserCode,
		VerificationURI: verificationURI, Interval: interval,
		ExpiresAt: time.Now().Add(expiresIn),
	})
	if err != nil {
		return core.AuthResult{}, connectorError("auth_invalid_response", core.CategoryUnavailable, "Authentication provider response was invalid")
	}
	return core.AuthResult{
		Supported:  true,
		State:      string(continuation),
		NextAction: "continue",
		UserAction: &core.AuthUserAction{VerificationURI: verificationURI, UserCode: provider.UserCode, PollInterval: interval},
	}, nil
}

func parseDevicePollInterval(raw json.RawMessage) (time.Duration, error) {
	value := strings.TrimSpace(string(raw))
	if len(value) >= 2 && value[0] == '"' {
		if err := json.Unmarshal(raw, &value); err != nil {
			return 0, err
		}
	}
	if !decimalSeconds.MatchString(value) {
		return 0, fmt.Errorf("invalid poll interval")
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || seconds <= 0 || math.IsInf(seconds, 0) || math.IsNaN(seconds) || seconds > float64((time.Duration(1<<63-1))/time.Second) {
		return 0, fmt.Errorf("invalid poll interval")
	}
	if seconds < 1 {
		seconds = 1
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func safeVerificationURI(value string) bool {
	u, err := url.Parse(value)
	if len(value) < 1 || len(value) > 2048 || err != nil || !u.IsAbs() || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func safeUserCode(value string) bool {
	if len(value) < 1 || len(value) > 256 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func validAuthBaseURL(base *url.URL) bool {
	if base == nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return false
	}
	if base.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(base.Hostname())
	return base.Scheme == "http" && ip != nil && ip.IsLoopback()
}

// HTTPDoer alone does not promise redirect behavior. RoundTrip returns the
// original 3xx response and therefore guarantees no redirected request is sent.
func doAuthNoRedirect(doer core.HTTPDoer, req *http.Request) (*http.Response, error) {
	var roundTripper http.RoundTripper
	switch transport := doer.(type) {
	case *http.Client:
		roundTripper = transport.Transport
		if roundTripper == nil {
			roundTripper = http.DefaultTransport
		}
	case http.RoundTripper:
		roundTripper = transport
	default:
		return nil, fmt.Errorf("invocation transport cannot guarantee redirect refusal")
	}
	return roundTripper.RoundTrip(req)
}
