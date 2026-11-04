package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

type policyDTO struct {
	ID         string   `json:"id"`
	Revision   int64    `json:"revision"`
	Enabled    bool     `json:"enabled"`
	Models     []string `json:"models"`
	Connectors []string `json:"connectors"`
	RPM        int64    `json:"rpm"`
	TPM        int64    `json:"tpm"`
	CreatedAt  string   `json:"created_at"`
}

type virtualKeyDTO struct {
	ID             string  `json:"id"`
	KeyID          string  `json:"key_id"`
	PolicyID       string  `json:"policy_id"`
	PolicyRevision int64   `json:"policy_revision"`
	Revision       int64   `json:"revision"`
	Enabled        bool    `json:"enabled"`
	Revoked        bool    `json:"revoked"`
	CreatedAt      string  `json:"created_at"`
	RevokedAt      *string `json:"revoked_at"`
}

func policyView(p sqlite.KeyPolicy) policyDTO {
	return policyDTO{p.ID, p.Revision, p.Enabled, p.Models, p.Connectors, p.RPM, p.TPM, p.CreatedAt.UTC().Format(time.RFC3339Nano)}
}

func virtualKeyView(k sqlite.VirtualKey) virtualKeyDTO {
	var revokedAt *string
	if k.RevokedAt != nil {
		value := k.RevokedAt.UTC().Format(time.RFC3339Nano)
		revokedAt = &value
	}
	return virtualKeyDTO{k.ID, k.KeyID, k.PolicyID, k.PolicyRevision, k.Revision, k.Enabled, k.Revoked,
		k.CreatedAt.UTC().Format(time.RFC3339Nano), revokedAt}
}

func registerPolicyKeyHandlers(mux *http.ServeMux, c Config) {
	if c.DB == nil {
		unavailable := func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, 500, "internal_error", "Internal server error")
		}
		for _, pattern := range []string{"GET /admin/v1/policies", "POST /admin/v1/policies", "GET /admin/v1/policies/{policy_id}", "PUT /admin/v1/policies/{policy_id}", "GET /admin/v1/keys", "POST /admin/v1/keys", "GET /admin/v1/keys/{key_id}", "DELETE /admin/v1/keys/{key_id}", "PUT /admin/v1/keys/{key_id}/policy"} {
			mux.HandleFunc(pattern, unavailable)
		}
		return
	}
	policies, keys := sqlite.NewKeyPolicies(c.DB), sqlite.NewVirtualKeys(c.DB)
	write := func(w http.ResponseWriter, value any, status int) {
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
		}
		_ = json.NewEncoder(w).Encode(value)
	}
	mux.HandleFunc("GET /admin/v1/policies", func(w http.ResponseWriter, r *http.Request) {
		q, limit, offset, valid := pageQuery(r.URL, map[string]bool{"enabled": true})
		var enabled *bool
		if raw, ok := q["enabled"]; ok {
			v := raw[0] == "true"
			enabled = &v
		}
		if !valid {
			writeError(w, 400, "invalid_request", "Invalid request")
			return
		}
		items, err := policies.List(r.Context(), sqlite.KeyPolicyFilter{Enabled: enabled})
		if err != nil {
			writeError(w, 500, "internal_error", "Internal server error")
			return
		}
		out := make([]policyDTO, 0, min(limit, max(0, len(items)-offset)))
		for _, p := range page(items, limit, offset) {
			out = append(out, policyView(p))
		}
		write(w, map[string]any{"items": out, "limit": limit, "offset": offset, "total": len(items)}, 200)
	})
	mux.HandleFunc("POST /admin/v1/policies", func(w http.ResponseWriter, r *http.Request) {
		if !requireJSON(w, r) {
			return
		}
		var in struct {
			ID         string   `json:"id"`
			Models     []string `json:"models"`
			Connectors []string `json:"connectors"`
			RPM        *int64   `json:"rpm"`
			TPM        *int64   `json:"tpm"`
		}
		if decodeStrict(r, &in) != nil || strings.TrimSpace(in.ID) == "" || in.Models == nil || in.Connectors == nil || in.RPM == nil || in.TPM == nil || *in.RPM < 0 || *in.TPM < 0 {
			writeError(w, 400, "invalid_request", "Invalid request")
			return
		}
		p, err := policies.Create(r.Context(), sqlite.CreateKeyPolicyParams{ID: in.ID, Models: in.Models, Connectors: in.Connectors, RPM: *in.RPM, TPM: *in.TPM})
		if errors.Is(err, sqlite.ErrInvalidKeyPolicy) {
			writeError(w, 400, "invalid_request", "Invalid request")
		} else if duplicate(err) {
			writeError(w, 409, "conflict", "Conflict")
		} else if err != nil {
			writeError(w, 500, "internal_error", "Internal server error")
		} else {
			write(w, policyView(p), 201)
		}
	})
	mux.HandleFunc("GET /admin/v1/policies/{policy_id}", func(w http.ResponseWriter, r *http.Request) {
		q, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || !onlyQuery(q, "revision") || len(q["revision"]) > 1 {
			writeError(w, 400, "invalid_request", "Invalid request")
			return
		}
		revision := int64(0)
		if value, ok := q["revision"]; ok {
			if value[0] == "" {
				writeError(w, 400, "invalid_request", "Invalid request")
				return
			}
			revision, err = strconv.ParseInt(value[0], 10, 64)
			if err != nil || revision < 1 {
				writeError(w, 400, "invalid_request", "Invalid request")
				return
			}
		}
		var p sqlite.KeyPolicy
		if revision == 0 {
			p, err = policies.GetLatest(r.Context(), r.PathValue("policy_id"))
		} else {
			p, err = policies.Get(r.Context(), r.PathValue("policy_id"), revision)
		}
		if errors.Is(err, sqlite.ErrKeyPolicyNotFound) {
			writeError(w, 404, "not_found", "Not found")
		} else if err != nil {
			writeError(w, 500, "internal_error", "Internal server error")
		} else {
			write(w, policyView(p), 200)
		}
	})
	mux.HandleFunc("PUT /admin/v1/policies/{policy_id}", func(w http.ResponseWriter, r *http.Request) {
		if !requireJSON(w, r) {
			return
		}
		var in struct {
			Expected   int64     `json:"expected_revision"`
			Models     *[]string `json:"models"`
			Connectors *[]string `json:"connectors"`
			RPM        *int64    `json:"rpm"`
			TPM        *int64    `json:"tpm"`
			Enabled    *bool     `json:"enabled"`
		}
		if decodeStrict(r, &in) != nil || hasNullField(rawBody(r), "models", "connectors", "rpm", "tpm", "enabled") || in.Expected < 1 || in.RPM != nil && *in.RPM < 0 || in.TPM != nil && *in.TPM < 0 || in.Enabled != nil && *in.Enabled {
			writeError(w, 400, "invalid_request", "Invalid request")
			return
		}
		id := r.PathValue("policy_id")
		current, err := policies.GetLatest(r.Context(), id)
		if errors.Is(err, sqlite.ErrKeyPolicyNotFound) {
			writeError(w, 404, "not_found", "Not found")
			return
		}
		if err != nil {
			writeError(w, 500, "internal_error", "Internal server error")
			return
		}
		if current.Revision != in.Expected {
			writeError(w, 409, "revision_conflict", "Revision conflict")
			return
		}
		if in.Models != nil {
			current.Models = *in.Models
		}
		if in.Connectors != nil {
			current.Connectors = *in.Connectors
		}
		if in.RPM != nil {
			current.RPM = *in.RPM
		}
		if in.TPM != nil {
			current.TPM = *in.TPM
		}
		if in.Enabled != nil {
			current.Enabled = *in.Enabled
		}
		p, err := policies.Update(r.Context(), id, in.Expected, sqlite.UpdateKeyPolicyParams{Enabled: current.Enabled, Models: current.Models, Connectors: current.Connectors, RPM: current.RPM, TPM: current.TPM})
		policyError(w, err, write, p)
	})
	registerKeyHandlers(mux, policies, keys, write)
}

func registerKeyHandlers(mux *http.ServeMux, policies *sqlite.KeyPolicies, keys *sqlite.VirtualKeys, write func(http.ResponseWriter, any, int)) {
	mux.HandleFunc("GET /admin/v1/keys", func(w http.ResponseWriter, r *http.Request) {
		q, limit, offset, valid := pageQuery(r.URL, map[string]bool{"policy_id": true, "enabled": true, "revoked": true})
		var enabled, revoked *bool
		for key, target := range map[string]**bool{"enabled": &enabled, "revoked": &revoked} {
			if raw, ok := q[key]; ok {
				v := raw[0] == "true"
				*target = &v
			}
		}
		if !valid {
			writeError(w, 400, "invalid_request", "Invalid request")
			return
		}
		items, err := keys.List(r.Context(), sqlite.VirtualKeyFilter{PolicyID: q.Get("policy_id"), Enabled: enabled, Revoked: revoked})
		if err != nil {
			writeError(w, 500, "internal_error", "Internal server error")
			return
		}
		out := make([]virtualKeyDTO, 0, min(limit, max(0, len(items)-offset)))
		for _, k := range page(items, limit, offset) {
			out = append(out, virtualKeyView(k))
		}
		write(w, map[string]any{"items": out, "limit": limit, "offset": offset, "total": len(items)}, 200)
	})
	mux.HandleFunc("POST /admin/v1/keys", func(w http.ResponseWriter, r *http.Request) {
		if !requireJSON(w, r) {
			return
		}
		var in struct {
			PolicyID       string `json:"policy_id"`
			PolicyRevision int64  `json:"policy_revision"`
		}
		if decodeStrict(r, &in) != nil || strings.TrimSpace(in.PolicyID) == "" || in.PolicyRevision < 1 {
			writeError(w, 400, "invalid_request", "Invalid request")
			return
		}
		p, err := policies.Get(r.Context(), in.PolicyID, in.PolicyRevision)
		if errors.Is(err, sqlite.ErrKeyPolicyNotFound) {
			writeError(w, 404, "not_found", "Not found")
			return
		}
		if err != nil {
			writeError(w, 500, "internal_error", "Internal server error")
			return
		}
		issued, err := keys.Create(r.Context(), sqlite.CreateVirtualKeyParams{PolicyID: p.ID, PolicyRevision: p.Revision})
		if err != nil {
			writeError(w, 500, "internal_error", "Internal server error")
			return
		}
		write(w, map[string]any{"secret": issued.Secret, "key": virtualKeyView(issued.VirtualKey)}, 201)
	})
	mux.HandleFunc("GET /admin/v1/keys/{key_id}", func(w http.ResponseWriter, r *http.Request) {
		k, err := keys.Get(r.Context(), r.PathValue("key_id"))
		if errors.Is(err, sqlite.ErrVirtualKeyNotFound) {
			k, err = keys.GetByKeyID(r.Context(), r.PathValue("key_id"))
		}
		keyError(w, err, write, k)
	})
	mux.HandleFunc("DELETE /admin/v1/keys/{key_id}", func(w http.ResponseWriter, r *http.Request) {
		k, err := keys.Revoke(r.Context(), r.PathValue("key_id"))
		keyError(w, err, write, k)
	})
	mux.HandleFunc("PUT /admin/v1/keys/{key_id}/policy", func(w http.ResponseWriter, r *http.Request) {
		if !requireJSON(w, r) {
			return
		}
		var in struct {
			PolicyID       string `json:"policy_id"`
			PolicyRevision int64  `json:"policy_revision"`
			Expected       int64  `json:"expected_revision"`
		}
		if decodeStrict(r, &in) != nil || strings.TrimSpace(in.PolicyID) == "" || in.PolicyRevision < 1 || in.Expected < 1 {
			writeError(w, 400, "invalid_request", "Invalid request")
			return
		}
		p, err := policies.Get(r.Context(), in.PolicyID, in.PolicyRevision)
		if errors.Is(err, sqlite.ErrKeyPolicyNotFound) {
			writeError(w, 404, "not_found", "Not found")
			return
		}
		if err != nil {
			writeError(w, 500, "internal_error", "Internal server error")
			return
		}
		k, err := keys.UpdatePolicy(r.Context(), r.PathValue("key_id"), p.ID, p.Revision, in.Expected)
		keyError(w, err, write, k)
	})
}

func pageQuery(u *url.URL, filters map[string]bool) (url.Values, int, int, bool) {
	q, err := url.ParseQuery(u.RawQuery)
	valid := err == nil
	allowed := map[string]bool{"limit": true, "offset": true}
	for k := range filters {
		allowed[k] = true
	}
	for k, values := range q {
		if !allowed[k] || len(values) != 1 || values[0] == "" {
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
	for key := range filters {
		if value, ok := q[key]; ok && (key == "enabled" || key == "revoked") && value[0] != "true" && value[0] != "false" {
			valid = false
		}
	}
	return q, limit, offset, valid
}

func onlyQuery(q url.Values, allowed ...string) bool {
	for k, v := range q {
		if !slicesContains(allowed, k) || len(v) != 1 || v[0] == "" {
			return false
		}
	}
	return true
}

func hasNullField(data []byte, names ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return true
	}
	for _, name := range names {
		if value, ok := fields[name]; ok && string(value) == "null" {
			return true
		}
	}
	return false
}
func slicesContains(values []string, value string) bool {
	return slices.Contains(values, value)
}
func page[T any](items []T, limit, offset int) []T {
	start := min(offset, len(items))
	return items[start:min(start+limit, len(items))]
}

func keyError(w http.ResponseWriter, err error, write func(http.ResponseWriter, any, int), k sqlite.VirtualKey) {
	switch {
	case err == nil:
		write(w, virtualKeyView(k), 200)
	case errors.Is(err, sqlite.ErrVirtualKeyNotFound):
		writeError(w, 404, "not_found", "Not found")
	case errors.Is(err, sqlite.ErrVirtualKeyRevisionMismatch), errors.Is(err, sqlite.ErrVirtualKeyRevoked):
		writeError(w, 409, "revision_conflict", "Revision conflict")
	default:
		writeError(w, 500, "internal_error", "Internal server error")
	}
}

func policyError(w http.ResponseWriter, err error, write func(http.ResponseWriter, any, int), p sqlite.KeyPolicy) {
	switch {
	case err == nil:
		write(w, policyView(p), 200)
	case errors.Is(err, sqlite.ErrKeyPolicyNotFound):
		writeError(w, 404, "not_found", "Not found")
	case errors.Is(err, sqlite.ErrKeyPolicyRevisionMismatch):
		writeError(w, 409, "revision_conflict", "Revision conflict")
	case errors.Is(err, sqlite.ErrInvalidKeyPolicy):
		writeError(w, 400, "invalid_request", "Invalid request")
	default:
		writeError(w, 500, "internal_error", "Internal server error")
	}
}
