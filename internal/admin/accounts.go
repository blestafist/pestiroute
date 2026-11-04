package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

type accountDTO struct {
	ID        string `json:"id"`
	Connector string `json:"connector"`
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func accountView(a sqlite.Account) accountDTO {
	return accountDTO{a.ID, a.Connector, a.Enabled, a.CreatedAt.UTC().Format(time.RFC3339Nano), a.UpdatedAt.UTC().Format(time.RFC3339Nano)}
}

func credentialView(c sqlite.Credential) map[string]any {
	return map[string]any{"id": c.ID, "account_id": c.AccountID, "revision": c.Revision, "expires_at": timePtr(c.ExpiresAt), "updated_at": c.UpdatedAt.UTC().Format(time.RFC3339Nano)}
}

func registerAccountHandlers(mux *http.ServeMux, c Config) {
	if c.DB == nil {
		unavailable := func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, 500, "internal_error", "Internal server error")
		}
		for _, pattern := range []string{"GET /admin/v1/accounts", "POST /admin/v1/accounts", "GET /admin/v1/accounts/{account_id}", "PUT /admin/v1/accounts/{account_id}/enabled", "POST /admin/v1/accounts/{account_id}/credentials", "PUT /admin/v1/accounts/{account_id}/credentials/{credential_id}"} {
			mux.HandleFunc(pattern, unavailable)
		}
		return
	}
	write := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	accounts := sqlite.NewAccounts(c.DB)
	credentials := sqlite.NewCredentials(c.DB)
	mux.HandleFunc("GET /admin/v1/accounts", func(w http.ResponseWriter, r *http.Request) {
		q, err := url.ParseQuery(r.URL.RawQuery)
		allowed := map[string]bool{"connector": true, "enabled": true, "limit": true, "offset": true}
		valid := err == nil
		for k, values := range q {
			if !allowed[k] || len(values) != 1 || (k != "connector" && values[0] == "") {
				valid = false
			}
		}
		limit, offset := 50, 0
		if value := q.Get("limit"); value != "" {
			limit, err = strconv.Atoi(value)
			valid = valid && err == nil && limit >= 1 && limit <= 500
		}
		if value := q.Get("offset"); value != "" {
			offset, err = strconv.Atoi(value)
			valid = valid && err == nil && offset >= 0
		}
		var enabled *bool
		if value, ok := q["enabled"]; ok {
			if value[0] != "true" && value[0] != "false" {
				valid = false
			} else {
				parsed := value[0] == "true"
				enabled = &parsed
			}
		}
		if !valid {
			writeError(w, 400, "invalid_request", "Invalid request")
			return
		}
		items, err := accounts.List(r.Context(), sqlite.AccountFilter{Connector: q.Get("connector"), Enabled: enabled})
		if err != nil {
			writeError(w, 500, "internal_error", "Internal server error")
			return
		}
		total := len(items)
		sliceOffset := min(offset, total)
		end := min(sliceOffset+limit, total)
		out := make([]accountDTO, 0, end-sliceOffset)
		for _, item := range items[sliceOffset:end] {
			out = append(out, accountView(item))
		}
		write(w, map[string]any{"items": out, "limit": limit, "offset": offset, "total": total})
	})
	mux.HandleFunc("POST /admin/v1/accounts", func(w http.ResponseWriter, r *http.Request) {
		if !requireJSON(w, r) {
			return
		}
		var in struct {
			ID        string          `json:"id"`
			Connector string          `json:"connector"`
			Enabled   json.RawMessage `json:"enabled"`
		}
		if decodeStrict(r, &in) != nil || strings.TrimSpace(in.ID) == "" || strings.TrimSpace(in.Connector) == "" {
			writeError(w, 400, "invalid_request", "Invalid request")
			return
		}
		enabled := true
		if len(in.Enabled) > 0 {
			if string(in.Enabled) == "null" || json.Unmarshal(in.Enabled, &enabled) != nil {
				writeError(w, 400, "invalid_request", "Invalid request")
				return
			}
		}
		account, err := accounts.Create(r.Context(), sqlite.Account{ID: in.ID, Connector: in.Connector, Enabled: enabled})
		if err != nil {
			if duplicate(err) {
				writeError(w, 409, "conflict", "Conflict")
			} else {
				writeError(w, 500, "internal_error", "Internal server error")
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		write(w, accountView(account))
	})
	mux.HandleFunc("GET /admin/v1/accounts/{account_id}", func(w http.ResponseWriter, r *http.Request) {
		account, err := accounts.Get(r.Context(), r.PathValue("account_id"))
		if accountError(w, err) {
			return
		}
		write(w, accountView(account))
	})
	mux.HandleFunc("PUT /admin/v1/accounts/{account_id}/enabled", func(w http.ResponseWriter, r *http.Request) {
		if !requireJSON(w, r) {
			return
		}
		var in struct {
			Enabled *bool `json:"enabled"`
		}
		if decodeStrict(r, &in) != nil || in.Enabled == nil {
			writeError(w, 400, "invalid_request", "Invalid request")
			return
		}
		account, err := accounts.SetEnabled(r.Context(), r.PathValue("account_id"), *in.Enabled)
		if accountError(w, err) {
			return
		}
		write(w, accountView(account))
	})
	mux.HandleFunc("POST /admin/v1/accounts/{account_id}/credentials", func(w http.ResponseWriter, r *http.Request) {
		if !requireJSON(w, r) {
			return
		}
		in, ok := parseCredentialBody(rawBody(r), false)
		if !ok || strings.TrimSpace(in.id) == "" || len(in.secret) == 0 || len(in.secret) > 64<<10 {
			clear(in.secret)
			writeError(w, 400, "invalid_request", "Invalid request")
			return
		}
		defer clear(in.secret)
		if account, err := accounts.Get(r.Context(), r.PathValue("account_id")); err != nil {
			accountError(w, err)
			return
		} else {
			_ = account
		}
		expires, ok := parseExpiry(in.expiresAt)
		if !ok {
			writeError(w, 400, "invalid_request", "Invalid request")
			return
		}
		envelope, err := secure.Seal(c.MasterKey, 1, "v1", "credentials", in.id, r.PathValue("account_id"), in.secret)
		if err != nil {
			writeError(w, 500, "internal_error", "Internal server error")
			return
		}
		stored, err := credentials.Create(r.Context(), sqlite.Credential{ID: in.id, AccountID: r.PathValue("account_id"), FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext, ExpiresAt: expires})
		if err != nil {
			if duplicate(err) {
				writeError(w, 409, "conflict", "Conflict")
			} else {
				writeError(w, 500, "internal_error", "Internal server error")
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		write(w, credentialView(stored))
	})
	mux.HandleFunc("PUT /admin/v1/accounts/{account_id}/credentials/{credential_id}", func(w http.ResponseWriter, r *http.Request) {
		if !requireJSON(w, r) {
			return
		}
		in, ok := parseCredentialBody(rawBody(r), true)
		if !ok || len(in.secret) == 0 || len(in.secret) > 64<<10 || in.expectedRevision < 1 {
			clear(in.secret)
			writeError(w, 400, "invalid_request", "Invalid request")
			return
		}
		defer clear(in.secret)
		accountID, id := r.PathValue("account_id"), r.PathValue("credential_id")
		if _, err := accounts.Get(r.Context(), accountID); err != nil {
			accountError(w, err)
			return
		}
		expires, ok := parseExpiry(in.expiresAt)
		if !ok {
			writeError(w, 400, "invalid_request", "Invalid request")
			return
		}
		envelope, err := secure.Seal(c.MasterKey, 1, "v1", "credentials", id, accountID, in.secret)
		if err != nil {
			writeError(w, 500, "internal_error", "Internal server error")
			return
		}
		stored, err := credentials.Update(r.Context(), sqlite.Credential{ID: id, AccountID: accountID, FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext, ExpiresAt: expires, Revision: in.expectedRevision})
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, 404, "not_found", "Not found")
			return
		}
		if errors.Is(err, sqlite.ErrRevisionMismatch) {
			writeError(w, 409, "revision_conflict", "Revision conflict")
			return
		}
		if err != nil {
			writeError(w, 500, "internal_error", "Internal server error")
			return
		}
		write(w, credentialView(stored))
	})
}

func decodeStrict(r *http.Request, dst any) error {
	data := rawBody(r)
	typeOf := reflect.TypeOf(dst).Elem()
	allowed := make(map[string]bool, typeOf.NumField())
	for i := range typeOf.NumField() {
		name := strings.Split(typeOf.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			allowed[name] = true
		}
	}
	if !exactObjectKeys(data, allowed) {
		return errors.New("invalid object keys")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func exactObjectKeys(data []byte, allowed map[string]bool) bool {
	if !json.Valid(data) || data[skipJSONSpace(data, 0)] != '{' {
		return false
	}
	fields := make(map[string]bool, len(allowed))
	i := skipJSONSpace(data, 0) + 1
	for {
		i = skipJSONSpace(data, i)
		if data[i] == '}' {
			return true
		}
		end, ok := scanJSONString(data, i)
		if !ok {
			return false
		}
		var key string
		if json.Unmarshal(data[i:end], &key) != nil || !allowed[key] || fields[key] {
			return false
		}
		fields[key] = true
		i = skipJSONSpace(data, end)
		if data[i] != ':' {
			return false
		}
		end, ok = scanJSONValue(data, skipJSONSpace(data, i+1))
		if !ok {
			return false
		}
		i = skipJSONSpace(data, end)
		if data[i] == '}' {
			return true
		}
		if data[i] != ',' {
			return false
		}
		i++
	}
}

type credentialInput struct {
	id               string
	secret           []byte
	expiresAt        *string
	expectedRevision int64
}

func rawBody(r *http.Request) []byte {
	if body, ok := r.Context().Value(rawBodyKey{}).(*[]byte); ok && body != nil {
		return *body
	}
	return nil
}

func requireJSON(w http.ResponseWriter, r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Unsupported media type")
		return false
	}
	return true
}

func parseCredentialBody(data []byte, replace bool) (credentialInput, bool) {
	var out credentialInput
	if !json.Valid(data) || data[skipJSONSpace(data, 0)] != '{' {
		return out, false
	}
	fields := make(map[string][]byte, 4)
	i := skipJSONSpace(data, 0) + 1
	for {
		i = skipJSONSpace(data, i)
		if data[i] == '}' {
			break
		}
		keyEnd, ok := scanJSONString(data, i)
		if !ok {
			return out, false
		}
		var key string
		if json.Unmarshal(data[i:keyEnd], &key) != nil {
			return out, false
		}
		i = skipJSONSpace(data, keyEnd)
		if data[i] != ':' {
			return out, false
		}
		start := skipJSONSpace(data, i+1)
		end, ok := scanJSONValue(data, start)
		if !ok {
			return out, false
		}
		if _, exists := fields[key]; exists {
			return out, false
		}
		fields[key] = data[start:end]
		i = skipJSONSpace(data, end)
		if data[i] == ',' {
			i++
			continue
		}
		if data[i] != '}' {
			return out, false
		}
		break
	}
	allowed := map[string]bool{"secret": true, "expires_at": true}
	if replace {
		allowed["expected_revision"] = true
	} else {
		allowed["id"] = true
	}
	for key := range fields {
		if !allowed[key] {
			return out, false
		}
	}
	secretRaw, ok := fields["secret"]
	if !ok {
		return out, false
	}
	out.secret, ok = decodeJSONStringBytes(secretRaw)
	if !ok {
		clear(out.secret)
		return credentialInput{}, false
	}
	if raw, ok := fields["expires_at"]; ok && !bytes.Equal(raw, []byte("null")) {
		var expiry string
		if json.Unmarshal(raw, &expiry) != nil {
			clear(out.secret)
			return credentialInput{}, false
		}
		out.expiresAt = &expiry
	}
	if replace {
		raw, exists := fields["expected_revision"]
		if !exists || json.Unmarshal(raw, &out.expectedRevision) != nil {
			clear(out.secret)
			return credentialInput{}, false
		}
	} else {
		raw, exists := fields["id"]
		if !exists || json.Unmarshal(raw, &out.id) != nil {
			clear(out.secret)
			return credentialInput{}, false
		}
	}
	return out, true
}

func skipJSONSpace(data []byte, i int) int {
	for i < len(data) && (data[i] == ' ' || data[i] == '\n' || data[i] == '\r' || data[i] == '\t') {
		i++
	}
	return i
}

func scanJSONString(data []byte, i int) (int, bool) {
	if i >= len(data) || data[i] != '"' {
		return 0, false
	}
	for i++; i < len(data); i++ {
		switch data[i] {
		case '\\':
			i++
		case '"':
			return i + 1, true
		}
	}
	return 0, false
}

func scanJSONValue(data []byte, i int) (int, bool) {
	if i >= len(data) {
		return 0, false
	}
	if data[i] == '"' {
		return scanJSONString(data, i)
	}
	depth := 0
	quoted, escaped := false, false
	for j := i; j < len(data); j++ {
		b := data[j]
		if quoted {
			if escaped {
				escaped = false
			} else if b == '\\' {
				escaped = true
			} else if b == '"' {
				quoted = false
			}
			continue
		}
		switch b {
		case '"':
			quoted = true
		case '{', '[':
			depth++
		case '}', ']':
			if depth == 0 {
				return j, true
			}
			depth--
		case ',', ' ', '\n', '\r', '\t':
			if depth == 0 {
				return j, true
			}
		}
	}
	return len(data), depth == 0 && !quoted
}

func decodeJSONStringBytes(raw []byte) ([]byte, bool) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil, false
	}
	out := make([]byte, 0, len(raw)-2)
	for i := 1; i < len(raw)-1; {
		if raw[i] != '\\' {
			_, size := utf8.DecodeRune(raw[i : len(raw)-1])
			if size == 0 || size == 1 && raw[i] >= utf8.RuneSelf {
				clear(out)
				return nil, false
			}
			out = append(out, raw[i:i+size]...)
			i += size
			continue
		}
		i++
		if i >= len(raw)-1 {
			clear(out)
			return nil, false
		}
		switch raw[i] {
		case '"', '\\', '/':
			out = append(out, raw[i])
			i++
		case 'b':
			out = append(out, '\b')
			i++
		case 'f':
			out = append(out, '\f')
			i++
		case 'n':
			out = append(out, '\n')
			i++
		case 'r':
			out = append(out, '\r')
			i++
		case 't':
			out = append(out, '\t')
			i++
		case 'u':
			if i+4 >= len(raw) {
				clear(out)
				return nil, false
			}
			a, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
			if err != nil {
				clear(out)
				return nil, false
			}
			r := rune(a)
			i += 5
			if utf16.IsSurrogate(r) {
				if r < 0xD800 || r > 0xDBFF || i+5 >= len(raw) || raw[i] != '\\' || raw[i+1] != 'u' {
					clear(out)
					return nil, false
				}
				b, err := strconv.ParseUint(string(raw[i+2:i+6]), 16, 16)
				if err != nil {
					clear(out)
					return nil, false
				}
				r = utf16.DecodeRune(r, rune(b))
				if r == utf8.RuneError {
					clear(out)
					return nil, false
				}
				i += 6
			}
			out = utf8.AppendRune(out, r)
		default:
			clear(out)
			return nil, false
		}
	}
	return out, true
}

func parseExpiry(value *string) (*time.Time, bool) {
	if value == nil {
		return nil, true
	}
	t, err := time.Parse(time.RFC3339, *value)
	if err != nil {
		return nil, false
	}
	t = t.UTC()
	return &t, true
}

func accountError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sqlite.ErrAccountNotFound) {
		writeError(w, 404, "not_found", "Not found")
	} else {
		writeError(w, 500, "internal_error", "Internal server error")
	}
	return true
}

func duplicate(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}
