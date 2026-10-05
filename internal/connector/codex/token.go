package codex

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"time"
)

const (
	oauthBundleVersion = 1
	maxJWTClaimsBytes  = 64 << 10
)

// oauthBundle is Connector-private plaintext; callers persist it only as an
// opaque encrypted credential envelope.
type oauthBundle struct {
	Version      int       `json:"version"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	AccountID    string    `json:"account_id"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func encodeOAuthBundle(bundle oauthBundle) ([]byte, error) {
	if err := validateOAuthBundle(bundle); err != nil {
		return nil, err
	}
	return json.Marshal(bundle)
}

func decodeOAuthBundle(data []byte) (oauthBundle, error) {
	var bundle oauthBundle
	if _, err := scanJSONObject(data, "version", "access_token", "refresh_token", "account_id", "expires_at"); err != nil {
		return oauthBundle{}, errors.New("invalid OAuth credential bundle")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&bundle); err != nil {
		return oauthBundle{}, errors.New("invalid OAuth credential bundle")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return oauthBundle{}, errors.New("invalid OAuth credential bundle")
	}
	if err := validateOAuthBundle(bundle); err != nil {
		return oauthBundle{}, err
	}
	return bundle, nil
}

func validateOAuthBundle(bundle oauthBundle) error {
	if bundle.Version != oauthBundleVersion || bundle.AccessToken == "" ||
		bundle.AccountID == "" || bundle.ExpiresAt.IsZero() {
		return errors.New("incomplete or unsupported OAuth credential bundle")
	}
	return nil
}

type jwtClaims struct {
	AccountID         string `json:"chatgpt_account_id"`
	NamespacedAccount string `json:"https://api.openai.com/auth.chatgpt_account_id"`
	Auth              struct {
		AccountID string `json:"chatgpt_account_id"`
	} `json:"https://api.openai.com/auth"`
	Organizations json.RawMessage `json:"organizations"`
}

// scanJSONObject rejects duplicate keys and case-folded aliases of relevant
// fields while leaving unknown fields available to ordinary JSON consumers.
func scanJSONObject(data []byte, fields ...string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("invalid JSON object")
	}
	known := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		known[field] = struct{}{}
	}
	values := make(map[string]json.RawMessage)
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return nil, errors.New("invalid JSON object")
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errors.New("invalid JSON object")
		}
		if _, duplicate := values[key]; duplicate {
			return nil, errors.New("duplicate JSON object key")
		}
		if _, exact := known[key]; !exact {
			for field := range known {
				if strings.EqualFold(key, field) {
					return nil, errors.New("non-canonical JSON field")
				}
			}
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, errors.New("invalid JSON object")
		}
		values[key] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, errors.New("invalid JSON object")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("invalid JSON object")
	}
	return values, nil
}

// OpenCode V2's pinned openai.ts (0bef0eab) omits token_type and reads
// expires_in ?? 3600; explicit malformed values remain rejected here.
func tokenLifetime(fields map[string]json.RawMessage) (int64, error) {
	if raw, ok := fields["token_type"]; ok {
		var explicit string
		if err := json.Unmarshal(raw, &explicit); err != nil || explicit == "" || !strings.EqualFold(explicit, "Bearer") {
			return 0, errors.New("invalid OAuth token type")
		}
	}
	seconds := int64(3600)
	if raw, ok := fields["expires_in"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &seconds) != nil || seconds <= 0 {
			return 0, errors.New("invalid OAuth token lifetime")
		}
	}
	if seconds > math.MaxInt64/int64(time.Second) {
		return 0, errors.New("invalid OAuth token lifetime")
	}
	return seconds, nil
}

func accountIDFromTokens(idToken, accessToken string) (string, error) {
	idAccount, err := accountIDFromToken(idToken)
	if err != nil {
		return "", err
	}
	accessAccount, err := accountIDFromAccessToken(accessToken)
	if err != nil {
		return "", err
	}
	if idAccount != "" && accessAccount != "" && idAccount != accessAccount {
		return "", errors.New("ambiguous OAuth account identity")
	}
	if idAccount != "" {
		return idAccount, nil
	}
	if accessAccount != "" {
		return accessAccount, nil
	}
	return "", errors.New("OAuth account identity is missing")
}

func accountIDFromToken(token string) (string, error) {
	return accountIDFromJWT(token, false)
}

func accountIDFromAccessToken(token string) (string, error) {
	return accountIDFromJWT(token, true)
}

func accountIDFromJWT(token string, allowOpaque bool) (string, error) {
	if token == "" {
		return "", nil
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		if allowOpaque {
			return "", nil
		}
		return "", errors.New("malformed OAuth token")
	}
	if parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", errors.New("malformed OAuth token")
	}
	if len(parts[1]) > (maxJWTClaimsBytes+2)/3*4 {
		return "", errors.New("invalid OAuth token claims")
	}
	if _, err := base64.RawURLEncoding.DecodeString(parts[0]); err != nil {
		return "", errors.New("malformed OAuth token")
	}
	if _, err := base64.RawURLEncoding.DecodeString(parts[2]); err != nil {
		return "", errors.New("malformed OAuth token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) > maxJWTClaimsBytes {
		return "", errors.New("invalid OAuth token claims")
	}
	var claims jwtClaims
	objects, err := scanJSONObject(payload, "chatgpt_account_id", "https://api.openai.com/auth.chatgpt_account_id", "https://api.openai.com/auth", "organizations")
	if err != nil {
		return "", errors.New("invalid OAuth token claims")
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", errors.New("invalid OAuth token claims")
	}
	if raw, ok := objects["https://api.openai.com/auth"]; ok {
		if _, err := scanJSONObject(raw, "chatgpt_account_id"); err != nil {
			return "", errors.New("invalid OAuth token claims")
		}
	}
	accountID := ""
	for _, candidate := range []string{claims.AccountID, claims.Auth.AccountID, claims.NamespacedAccount} {
		if candidate == "" {
			continue
		}
		if accountID != "" && accountID != candidate {
			return "", errors.New("conflicting OAuth account identity claims")
		}
		accountID = candidate
	}
	if accountID != "" {
		return accountID, nil
	}
	if raw, ok := objects["organizations"]; ok {
		var organizations []json.RawMessage
		if err := json.Unmarshal(raw, &organizations); err != nil {
			return "", errors.New("invalid OAuth token claims")
		}
		firstID := ""
		for i, organization := range organizations {
			fields, err := scanJSONObject(organization, "id")
			if err != nil {
				return "", errors.New("invalid OAuth token claims")
			}
			if i == 0 {
				if rawID, exists := fields["id"]; exists {
					if err := json.Unmarshal(rawID, &firstID); err != nil {
						return "", errors.New("invalid OAuth token claims")
					}
				}
			}
		}
		if firstID != "" {
			return firstID, nil
		}
	}
	return "", nil
}
