package admin

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func registerReadHandlers(mux *http.ServeMux, c Config) {
	write := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	mux.HandleFunc("GET /admin/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		readiness := c.Readiness
		if readiness == "" {
			readiness = "unknown"
		}
		components, routes := c.Components, c.Routes
		if components == nil {
			components = []Component{}
		}
		if routes == nil {
			routes = []Route{}
		}
		write(w, map[string]any{"version": c.Version, "readiness": readiness, "components": components, "routes": routes})
	})
	for _, endpoint := range []string{"requests", "attempts", "summary"} {
		name := endpoint
		mux.HandleFunc("GET /admin/v1/usage/"+name, func(w http.ResponseWriter, r *http.Request) { usageRead(w, r, c.DB, name, write) })
	}
}

func usageRead(w http.ResponseWriter, r *http.Request, db *sql.DB, endpoint string, write func(http.ResponseWriter, any)) {
	key, account, since, until, limit, offset, ok := parseUsageQuery(r, endpoint)
	if endpoint == "attempts" && key != "" {
		ok = false
	}
	if !ok {
		writeError(w, 400, "invalid_request", "Invalid request")
		return
	}
	if db == nil {
		writeError(w, 500, "internal_error", "Internal server error")
		return
	}
	ledger := sqlite.NewLedger(db)
	ctx := r.Context()
	from, to := ceilMillis(since), until
	switch endpoint {
	case "requests":
		items, err := ledger.QueryRequests(ctx, sqlite.RequestFilter{VirtualKeyID: key, AccountID: account, Since: from, Until: to, Limit: limit, Offset: offset})
		if err != nil {
			writeError(w, 500, "internal_error", "Internal server error")
			return
		}
		var total int
		err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests q WHERE (?='' OR q.virtual_key_id=?) AND (?='' OR EXISTS(SELECT 1 FROM attempts a WHERE a.request_id=q.id AND a.account_id=?)) AND (? IS NULL OR q.accepted_at>=?) AND (? IS NULL OR q.accepted_at<=?)`, key, key, account, account, nullableMillis(from), nullableMillis(from), nullableMillis(to), nullableMillis(to)).Scan(&total)
		if err != nil {
			writeError(w, 500, "internal_error", "Internal server error")
			return
		}
		out := make([]map[string]any, 0, len(items))
		for _, x := range items {
			q := x.Request
			out = append(out, map[string]any{"id": q.ID, "key_id": q.VirtualKeyID, "policy_id": q.PolicyID, "protocol": q.Protocol, "model": q.Model, "route_id": q.RouteID, "key_revision": q.KeyRevision, "policy_revision": q.PolicyRevision, "accepted_at": q.AcceptedAt.UTC().Format(time.RFC3339Nano), "state": q.State, "finished_at": timePtr(q.FinishedAt), "attempts": attemptDTO(x.Attempts)})
		}
		write(w, map[string]any{"items": out, "limit": limit, "offset": offset, "total": total})
	case "attempts":
		request := r.URL.Query().Get("request_id")
		items, err := ledger.QueryAttempts(ctx, sqlite.AttemptFilter{RequestID: request, AccountID: account, Since: from, Until: to, Limit: limit, Offset: offset})
		if err != nil {
			writeError(w, 500, "internal_error", "Internal server error")
			return
		}
		var total int
		err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts a JOIN requests q ON q.id=a.request_id WHERE (?='' OR a.request_id=?) AND (?='' OR a.account_id=?) AND (? IS NULL OR COALESCE(a.dispatched_at,a.finished_at,q.accepted_at)>=?) AND (? IS NULL OR COALESCE(a.dispatched_at,a.finished_at,q.accepted_at)<=?)`, request, request, account, account, nullableMillis(from), nullableMillis(from), nullableMillis(to), nullableMillis(to)).Scan(&total)
		if err != nil {
			writeError(w, 500, "internal_error", "Internal server error")
			return
		}
		write(w, map[string]any{"items": attemptDTO(items), "limit": limit, "offset": offset, "total": total})
	case "summary":
		s, err := ledger.QueryUsageSummary(ctx, key, account, from, to)
		if err != nil {
			writeError(w, 500, "internal_error", "Internal server error")
			return
		}
		write(w, map[string]any{"requests": s.Requests, "attempts": s.Attempts, "input_tokens": s.InputTokens, "output_tokens": s.OutputTokens, "reasoning_tokens": s.ReasoningTokens, "cached_tokens": s.CachedTokens, "estimated_tokens": s.EstimatedTokens, "actual_tokens": s.ActualTokens, "effective_charge": s.EffectiveCharge})
	}
}

func nullableMillis(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}
func ceilMillis(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	millis := t.UnixMilli()
	rounded := time.UnixMilli(millis)
	if rounded.Before(*t) {
		millis++
	}
	result := time.UnixMilli(millis).UTC()
	return &result
}
func timePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func attemptDTO(items []sqlite.AttemptUsage) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, x := range items {
		a := x.Attempt
		var usage any
		if x.Usage != nil {
			u := x.Usage
			usage = map[string]any{"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens, "reasoning_tokens": u.ReasoningTokens, "cached_tokens": u.CachedTokens, "source": u.Source, "completeness": u.Completeness, "recorded_at": u.RecordedAt.UTC().Format(time.RFC3339Nano)}
		}
		out = append(out, map[string]any{"id": a.ID, "request_id": a.RequestID, "ordinal": a.Ordinal, "account_id": a.AccountID, "connector": a.Connector, "route_id": a.RouteID, "state": a.State, "estimate_tokens": a.EstimateTokens, "committed": a.Committed, "error_category": a.ErrorCategory, "error_reason": a.ErrorReason, "dispatched_at": timePtr(a.DispatchedAt), "finished_at": timePtr(a.FinishedAt), "usage": usage, "reservation": map[string]any{"estimated_tokens": x.Reservation.EstimatedTokens, "actual_tokens": x.Reservation.ActualTokens, "effective_charge": x.Reservation.EffectiveCharge, "state": x.Reservation.State}})
	}
	return out
}

func parseUsageQuery(r *http.Request, endpoint string) (string, string, *time.Time, *time.Time, int, int, bool) {
	allowed := map[string]bool{"account_id": true, "since": true, "until": true}
	if endpoint == "requests" || endpoint == "summary" {
		allowed["key_id"] = true
	}
	if endpoint == "attempts" {
		allowed["request_id"] = true
	}
	if endpoint != "summary" {
		allowed["limit"], allowed["offset"] = true, true
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", "", nil, nil, 0, 0, false
	}
	for key, vals := range query {
		if !allowed[key] || len(vals) != 1 {
			return "", "", nil, nil, 0, 0, false
		}
		if vals[0] == "" {
			return "", "", nil, nil, 0, 0, false
		}
	}
	q := query
	limit, offset := 50, 0
	if v := q.Get("limit"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 1 || n > 500 {
			return "", "", nil, nil, 0, 0, false
		}
		limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 0 {
			return "", "", nil, nil, 0, 0, false
		}
		offset = n
	}
	parse := func(k string) (*time.Time, bool) {
		v := q.Get(k)
		if v == "" {
			return nil, true
		}
		t, e := time.Parse(time.RFC3339, v)
		if e != nil {
			return nil, false
		}
		u := t.UTC()
		return &u, true
	}
	since, ok := parse("since")
	if !ok {
		return "", "", nil, nil, 0, 0, false
	}
	until, ok := parse("until")
	if !ok || since != nil && until != nil && since.After(*until) {
		return "", "", nil, nil, 0, 0, false
	}
	return q.Get("key_id"), q.Get("account_id"), since, until, limit, offset, true
}
