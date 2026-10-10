package codex

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testJWT(payload []byte) string {
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString([]byte("signature"))
}

func TestTokenMetadataAndCredentialCodec(t *testing.T) {
	const boundaryPrefix = `{"chatgpt_account_id":"account","extension":"`
	const boundarySuffix = `"}`
	claimsOfSize := func(size int) string {
		return boundaryPrefix + strings.Repeat("x", size-len(boundaryPrefix)-len(boundarySuffix)) + boundarySuffix
	}
	tests := []struct {
		name     string
		idClaims string
		access   string
		want     string
		wantErr  bool
	}{
		{name: "id token is checked first", idClaims: `{"chatgpt_account_id":"id-account"}`, access: testJWT([]byte(`{"sub":"user"}`)), want: "id-account"},
		{name: "agreeing account claims with organization fallback present", idClaims: `{"chatgpt_account_id":"primary","https://api.openai.com/auth.chatgpt_account_id":"primary","organizations":[{"id":"org"}]}`, want: "primary"},
		{name: "namespaced claim", idClaims: `{"https://api.openai.com/auth.chatgpt_account_id":"namespaced","organizations":[{"id":"org"}]}`, want: "namespaced"},
		{name: "nested namespaced auth claim", idClaims: `{"https://api.openai.com/auth":{"chatgpt_account_id":"nested"}}`, want: "nested"},
		{name: "root and nested account conflict", idClaims: `{"chatgpt_account_id":"root","https://api.openai.com/auth":{"chatgpt_account_id":"nested"}}`, wantErr: true},
		{name: "root and flat namespaced account conflict", idClaims: `{"chatgpt_account_id":"root","https://api.openai.com/auth.chatgpt_account_id":"flat"}`, wantErr: true},
		{name: "nested and flat namespaced account conflict", idClaims: `{"https://api.openai.com/auth":{"chatgpt_account_id":"nested"},"https://api.openai.com/auth.chatgpt_account_id":"flat"}`, wantErr: true},
		{name: "organization fallback", access: testJWT([]byte(`{"organizations":[{"id":"org"}]} `)), want: "org"},
		{name: "unknown JWT claim is supported", idClaims: `{"chatgpt_account_id":"known","extension":{"value":1}}`, want: "known"},
		{name: "access token fallback", access: testJWT([]byte(`{"chatgpt_account_id":"access"}`)), want: "access"},
		{name: "opaque access token with ID token identity", idClaims: `{"chatgpt_account_id":"id-account"}`, access: "opaque-access-token", want: "id-account"},
		{name: "account disagreement is rejected", idClaims: `{"chatgpt_account_id":"first"}`, access: testJWT([]byte(`{"chatgpt_account_id":"second"}`)), wantErr: true},
		{name: "missing identity", idClaims: `{"sub":"user"}`, wantErr: true},
		{name: "invalid claims JSON", idClaims: "not-json", wantErr: true},
		{name: "truncated token", idClaims: "e30.e30", wantErr: true},
		{name: "empty signature", idClaims: "e30.e30.", wantErr: true},
		{name: "oversized valid JSON payload", idClaims: claimsOfSize(maxJWTClaimsBytes + 1), wantErr: true},
		{name: "casevariant account claim", idClaims: `{"CHATGPT_ACCOUNT_ID":"secret-account"}`, wantErr: true},
		{name: "duplicate account claim", idClaims: `{"chatgpt_account_id":"secret-first","chatgpt_account_id":"secret-second"}`, wantErr: true},
		{name: "duplicate nested account claim", idClaims: `{"https://api.openai.com/auth":{"chatgpt_account_id":"first","chatgpt_account_id":"second"}}`, wantErr: true},
		{name: "malformed nested auth claims", idClaims: `{"https://api.openai.com/auth":[]}`, wantErr: true},
		{name: "casevariant namespaced claim", idClaims: `{"HTTPS://API.OPENAI.COM/AUTH.CHATGPT_ACCOUNT_ID":"secret-account"}`, wantErr: true},
		{name: "casevariant organization ID", idClaims: `{"organizations":[{"ID":"secret-account"}]}`, wantErr: true},
		{name: "duplicate organization ID", idClaims: `{"organizations":[{"id":"secret-first","id":"secret-second"}]}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idToken := tt.idClaims
			if idToken != "" {
				idToken = testJWT([]byte(idToken))
			}
			got, err := accountIDFromTokens(idToken, tt.access)
			if (err != nil) != tt.wantErr {
				t.Fatalf("accountIDFromTokens() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("accountIDFromTokens() = %q, want %q", got, tt.want)
			}
			if err != nil && (strings.Contains(err.Error(), "secret") ||
				(idToken != "" && strings.Contains(err.Error(), idToken)) ||
				(tt.access != "" && strings.Contains(err.Error(), tt.access))) {
				t.Fatalf("error leaked token material: %v", err)
			}
		})
	}
	boundary := claimsOfSize(maxJWTClaimsBytes)
	if got, err := accountIDFromTokens(testJWT([]byte(boundary)), ""); err != nil || got != "account" {
		t.Fatalf("64 KiB claims boundary: got %q, err %v", got, err)
	}

	expires := time.Date(2030, 1, 2, 3, 4, 5, 6000000, time.UTC)
	want := oauthBundle{Version: oauthBundleVersion, AccessToken: "access-secret", RefreshToken: "refresh-secret", AccountID: "account-1", ExpiresAt: expires}
	encoded, err := encodeOAuthBundle(want)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["version"] != float64(oauthBundleVersion) {
		t.Fatalf("bundle version = %v", raw["version"])
	}
	got, err := decodeOAuthBundle(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) || got != want {
		t.Fatalf("credential round trip mismatch: got %#v, want %#v", got, want)
	}

	for _, bad := range [][]byte{
		[]byte(`{"version":2,"access_token":"access-secret","account_id":"a","expires_at":"2030-01-02T03:04:05Z"}`),
		[]byte(`{"version":1,"access_token":"access-secret","account_id":"a","expires_at":"2030-01-02T03:04:05Z"} {}`),
		[]byte(`{"version":1,"access_token":"access-secret","account_id":"a"}`),
		[]byte(`{"version":1,"ACCESS_TOKEN":"secret-access","account_id":"a","expires_at":"2030-01-02T03:04:05Z"}`),
		[]byte(`{"version":1,"access_token":"secret-first","access_token":"secret-second","account_id":"a","expires_at":"2030-01-02T03:04:05Z"}`),
	} {
		if _, err := decodeOAuthBundle(bad); err == nil {
			t.Fatalf("decodeOAuthBundle(%q) unexpectedly succeeded", bad)
		} else if strings.Contains(err.Error(), "access-secret") || strings.Contains(err.Error(), "secret-") {
			t.Fatalf("codec error leaked secret: %v", err)
		}
	}
	invalid := want
	invalid.AccessToken = "secret-that-must-not-leak"
	invalid.Version++
	if _, err := encodeOAuthBundle(invalid); err == nil || strings.Contains(err.Error(), invalid.AccessToken) {
		t.Fatalf("invalid codec input error = %v", err)
	}
}
