package codex

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

type headerCredentials []byte

func (c headerCredentials) Get(context.Context, string) ([]byte, error) {
	return append([]byte(nil), c...), nil
}

func TestBuildRequestHeaders(t *testing.T) {
	for _, account := range []string{"account-a", "account-b"} {
		t.Run(account, func(t *testing.T) {
			token := "secret-" + account
			credential, err := encodeOAuthBundle(oauthBundle{
				Version: oauthBundleVersion, AccessToken: token, AccountID: account,
				ExpiresAt: time.Now().Add(time.Hour),
			})
			if err != nil {
				t.Fatal(err)
			}
			inbound := map[string][]string{
				"authorization":         {"Bearer spoof"},
				"AUTHORIZATION":         {"Bearer second-spoof"},
				"CHATGPT-ACCOUNT-ID":    {"other-account"},
				"chatgpt-account-id":    {"another-account"},
				"Session-Id":            {"session-spoof"},
				"X-Codex-Beta-Features": {"remote_compaction_v2"},
				"Cookie":                {"session=secret"},
				"Cookie2":               {"session=secret"},
				"OpenAI-Organization":   {"org-spoof"},
				"OpenAI-Project":        {"project-spoof"},
				"X-Api-Key":             {"api-key-spoof"},
				"OpenAI-Beta":           {"responses_websockets=2026-02-06"},
				"Content-Type":          {"text/plain"},
				"Accept":                {"*/*"},
				"Accept-Encoding":       {"gzip"},
				"Host":                  {"attacker.invalid"},
				"Content-Length":        {"9"},
				"Connection":            {"X-Private-Hop"},
				"X-Private-Hop":         {"blocked"},
				"Keep-Alive":            {"timeout=10"},
				"Proxy-Authenticate":    {"Basic"},
				"Proxy-Authorization":   {"secret"},
				"Proxy-Connection":      {"keep-alive"},
				"TE":                    {"trailers"},
				"Trailer":               {"X-Trailer"},
				"Trailers":              {"X-Trailers"},
				"Transfer-Encoding":     {"chunked"},
				"Upgrade":               {"websocket"},
				"Tracestate":            {"vendor=value"},
				"User-Agent":            {"PestiRoute-test/1"},
				"Traceparent":           {"00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"},
			}
			headers, err := buildRequestHeaders(t.Context(), inbound, core.InvocationServices{Credentials: headerCredentials(credential)}, account)
			if err != nil {
				t.Fatal("unexpected header error")
			}
			want := http.Header{
				"Authorization":      {"Bearer " + token},
				"Chatgpt-Account-Id": {account},
				"Content-Type":       {"application/json"},
				"Accept":             {"text/event-stream"},
				"Accept-Encoding":    {"identity"},
				"Tracestate":         {"vendor=value"},
				"User-Agent":         {"PestiRoute-test/1"},
			}
			want["Traceparent"] = []string{"00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"}
			if !reflect.DeepEqual(headers, want) {
				t.Fatalf("headers = %#v, want %#v", headers, want)
			}
			for name := range inbound {
				if strings.HasPrefix(strings.ToLower(name), "x-codex-") || strings.EqualFold(name, "session-id") || strings.EqualFold(name, "cookie") {
					if headers.Get(name) != "" {
						t.Errorf("spoofed header %q forwarded", name)
					}
				}
			}
		})
	}
}

func TestBuildRequestHeadersRejectsInvalidCredential(t *testing.T) {
	secret := "never-print-this-token"
	valid, err := encodeOAuthBundle(oauthBundle{
		Version: oauthBundleVersion, AccessToken: secret, AccountID: "account-a", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		credential []byte
		account    string
	}{
		"missing":   {nil, "account-a"},
		"malformed": {[]byte(secret), "account-a"},
		"expired":   {mustOAuthBundle(t, secret, "account-a", time.Now().Add(-time.Second)), "account-a"},
		"mismatch":  {valid, "account-b"},
		"token control character": {
			mustOAuthBundle(t, "token\r\nInjected: value", "account-a", time.Now().Add(time.Hour)), "account-a",
		},
		"account control character": {
			mustOAuthBundle(t, secret, "account-a\nInjected: value", time.Now().Add(time.Hour)), "account-a\nInjected: value",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := buildRequestHeaders(t.Context(), nil, core.InvocationServices{Credentials: headerCredentials(test.credential)}, test.account)
			if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "account-a") {
				t.Fatalf("expected opaque credential failure, got %v", err)
			}
		})
	}
}

func TestBuildRequestHeadersHonorsConnectionNominations(t *testing.T) {
	credential := mustOAuthBundle(t, "access-token", "account-a", time.Now().Add(time.Hour))
	headers, err := buildRequestHeaders(t.Context(), map[string][]string{
		"Connection":  {"traceparent"},
		"Traceparent": {"00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"},
	}, core.InvocationServices{Credentials: headerCredentials(credential)}, "account-a")
	if err != nil {
		t.Fatal("unexpected header error")
	}
	if got := headers.Get("Traceparent"); got != "" {
		t.Fatalf("Connection-nominated traceparent was forwarded: %q", got)
	}
}

func TestBuildRequestHeadersEnforcesAggregateLimit(t *testing.T) {
	credential := mustOAuthBundle(t, "access-token", "account-a", time.Now().Add(time.Hour))
	values := make([]string, 70)
	for i := range values {
		values[i] = strings.Repeat("a", 256)
	}
	if _, err := buildRequestHeaders(t.Context(), map[string][]string{"Traceparent": values}, core.InvocationServices{Credentials: headerCredentials(credential)}, "account-a"); err == nil {
		t.Fatal("oversized aggregate request headers accepted")
	}
}

func mustOAuthBundle(t *testing.T, token, account string, expiry time.Time) []byte {
	t.Helper()
	bundle, err := encodeOAuthBundle(oauthBundle{Version: oauthBundleVersion, AccessToken: token, AccountID: account, ExpiresAt: expiry})
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}
