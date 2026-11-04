package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

// AuthCoordinator is the existing generic Core auth lifecycle; HTTP owns no
// provider semantics or credential storage.
type AuthCoordinator interface {
	Start(context.Context, string) (core.AuthSession, error)
	Continue(context.Context, string, string) (core.AuthSession, error)
	Refresh(context.Context, string) (core.AuthCredentials, error)
}

func registerAuthHandlers(mux *http.ServeMux, c Config) {
	unavailable := func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, 503, "authentication_unconfigured", "Authentication unavailable")
	}
	if c.Auth == nil {
		for _, route := range []string{"POST /admin/v1/auth/sessions", "POST /admin/v1/auth/sessions/{session_id}/continue", "POST /admin/v1/auth/accounts/{account_id}/refresh"} {
			mux.HandleFunc(route, unavailable)
		}
		return
	}
	write := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	mux.HandleFunc("POST /admin/v1/auth/sessions", func(w http.ResponseWriter, r *http.Request) {
		if !requireJSON(w, r) {
			return
		}
		var in struct {
			AccountID string `json:"account_id"`
		}
		if decodeStrict(r, &in) != nil || in.AccountID == "" {
			writeError(w, 400, "invalid_request", "Invalid request")
			return
		}
		session, err := c.Auth.Start(r.Context(), in.AccountID)
		if err != nil {
			authStartError(w, err, in.AccountID, c.DB)
			return
		}
		write(w, authResult(session))
	})
	mux.HandleFunc("POST /admin/v1/auth/sessions/{session_id}/continue", func(w http.ResponseWriter, r *http.Request) {
		if !requireJSON(w, r) {
			return
		}
		var in struct {
			CallbackInput string `json:"callback_input"`
		}
		if decodeStrict(r, &in) != nil || len(in.CallbackInput) > 4096 {
			writeError(w, 400, "invalid_request", "Invalid request")
			return
		}
		session, err := c.Auth.Continue(r.Context(), r.PathValue("session_id"), in.CallbackInput)
		if err != nil {
			authError(w, err, "", c.DB)
			return
		}
		write(w, authResult(session))
	})
	mux.HandleFunc("POST /admin/v1/auth/accounts/{account_id}/refresh", func(w http.ResponseWriter, r *http.Request) {
		if len(rawBody(r)) != 0 {
			writeError(w, 400, "invalid_request", "Invalid request")
			return
		}
		accountID := r.PathValue("account_id")
		credentials, err := c.Auth.Refresh(r.Context(), accountID)
		for _, secret := range credentials.Values {
			clear(secret)
		}
		if err != nil {
			authError(w, err, accountID, c.DB)
			return
		}
		write(w, map[string]any{"status": "refreshed", "revision": credentials.Revision})
	})
}

func authStartError(w http.ResponseWriter, err error, accountID string, db *sql.DB) {
	if errors.Is(err, core.ErrAuthUnavailable) {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Authentication unavailable")
		return
	}
	authError(w, err, accountID, db)
}

func authResult(session core.AuthSession) map[string]any {
	if session.ID == "" {
		return map[string]any{"status": "complete", "session_id": nil, "expires_at": nil, "user_action": nil}
	}
	var action any
	if session.UserAction != nil {
		a := session.UserAction
		var code, interval any
		if a.UserCode != "" {
			code = a.UserCode
		}
		if a.PollInterval > 0 {
			interval = a.PollInterval.Seconds()
		}
		action = map[string]any{"verification_uri": a.VerificationURI, "user_code": code, "poll_interval_seconds": interval}
	}
	return map[string]any{"status": "continue", "session_id": session.ID, "expires_at": session.ExpiresAt.UTC().Format(time.RFC3339Nano), "user_action": action}
}

func authError(w http.ResponseWriter, err error, accountID string, db *sql.DB) {
	switch {
	case errors.Is(err, core.ErrAccountUnavailable):
		if db != nil {
			var enabled bool
			if e := db.QueryRow(`SELECT enabled FROM accounts WHERE id=?`, accountID).Scan(&enabled); e == nil {
				if enabled {
					writeError(w, 503, "unavailable", "Authentication unavailable")
				} else {
					writeError(w, 400, "account_disabled", "Account disabled")
				}
				return
			}
		}
		writeError(w, 404, "not_found", "Not found")
	case errors.Is(err, core.ErrAuthRevisionMismatch):
		writeError(w, 409, "revision_conflict", "Authentication state conflict")
	case errors.Is(err, core.ErrRefreshMarkerConflict):
		writeError(w, 409, "revision_conflict", "Authentication state conflict")
	case errors.Is(err, core.ErrAuthUnavailable):
		writeError(w, 409, "revision_conflict", "Authentication state conflict")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, 503, "unavailable", "Authentication unavailable")
	default:
		writeError(w, 500, "internal_error", "Internal server error")
	}
}
