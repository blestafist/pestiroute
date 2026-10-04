package codex

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
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
	AccountID         string          `json:"chatgpt_account_id"`
	NamespacedAccount string          `json:"https://api.openai.com/auth.chatgpt_account_id"`
	Organizations     json.RawMessage `json:"organizations"`
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

func accountIDFromTokens(idToken, accessToken string) (string, error) {
	idAccount, err := accountIDFromToken(idToken)
	if err != nil {
		return "", err
	}
	accessAccount, err := accountIDFromToken(accessToken)
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
	if token == "" {
		return "", nil
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
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
	objects, err := scanJSONObject(payload, "chatgpt_account_id", "https://api.openai.com/auth.chatgpt_account_id", "organizations")
	if err != nil {
		return "", errors.New("invalid OAuth token claims")
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", errors.New("invalid OAuth token claims")
	}
	if claims.AccountID != "" {
		return claims.AccountID, nil
	}
	if claims.NamespacedAccount != "" {
		return claims.NamespacedAccount, nil
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
