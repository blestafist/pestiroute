package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

const usageAdminHelp = `usage: gateway admin [--db PATH] [--master-key PATH] usage <requests|attempts|summary> [filters]
  requests [--key ID] [--account ID] [--since RFC3339] [--until RFC3339] [--limit N] [--offset N] [--json]
  attempts [--request ID] [--account ID] [--since RFC3339] [--until RFC3339] [--limit N] [--offset N] [--json]
  summary [--key ID] [--account ID] [--since RFC3339] [--until RFC3339] [--json]
`

func adminUsageHelpOrInvalid(env adminEnvironment) (bool, error) {
	if len(env.args) < 2 {
		return true, adminUsageError(env, "admin: usage requires requests, attempts, or summary")
	}
	if isHelp(env.args[1]) {
		_, _ = io.WriteString(env.stdout, usageAdminHelp)
		return true, nil
	}
	if env.args[1] != "requests" && env.args[1] != "attempts" && env.args[1] != "summary" {
		return true, adminUsageError(env, "admin: unknown usage command")
	}
	if slices.ContainsFunc(env.args[2:], isHelp) {
		_, _ = io.WriteString(env.stdout, usageAdminHelp)
		return true, nil
	}
	_, err := parseUsageFlags(env.args[1], env.args[2:])
	if err != nil {
		return true, adminUsageError(env, err.Error())
	}
	return false, nil
}

func adminUsageError(env adminEnvironment, message string) error {
	_, _ = io.WriteString(env.stderr, usageAdminHelp)
	return errors.New(message)
}

type usageFlags struct {
	key, account, request string
	since, until          *time.Time
	limit, offset         int
	json                  bool
}

func parseUsageFlags(command string, args []string) (usageFlags, error) {
	fs := adminFlags("usage " + command)
	v := usageFlags{limit: 50}
	fs.StringVar(&v.key, "key", "", "virtual key ID")
	fs.StringVar(&v.account, "account", "", "account ID")
	fs.StringVar(&v.request, "request", "", "request ID")
	since := fs.String("since", "", "inclusive RFC3339 lower bound")
	until := fs.String("until", "", "inclusive RFC3339 upper bound")
	fs.IntVar(&v.limit, "limit", 50, "page size (1-500)")
	fs.IntVar(&v.offset, "offset", 0, "page offset")
	fs.BoolVar(&v.json, "json", false, "emit JSON")
	if err := parseAdminFlags(fs, args); err != nil {
		return v, err
	}
	if fs.NArg() != 0 {
		return v, errors.New("admin: unexpected usage arguments")
	}
	if command == "attempts" && v.key != "" || command != "attempts" && v.request != "" {
		return v, errors.New("admin: filter not valid for usage command")
	}
	if command == "summary" && (wasAdminFlagSet(fs, "limit") || wasAdminFlagSet(fs, "offset")) {
		return v, errors.New("admin: summary does not support pagination")
	}
	if v.limit < 1 || v.limit > 500 || v.offset < 0 {
		return v, errors.New("admin: limit must be 1-500 and offset non-negative")
	}
	if *since != "" {
		t, err := time.Parse(time.RFC3339, *since)
		if err != nil {
			return v, errors.New("admin: --since must be RFC3339")
		}
		t = t.UTC()
		v.since = &t
	}
	if *until != "" {
		t, err := time.Parse(time.RFC3339, *until)
		if err != nil {
			return v, errors.New("admin: --until must be RFC3339")
		}
		t = t.UTC()
		v.until = &t
	}
	if v.since != nil && v.until != nil && v.since.After(*v.until) {
		return v, errors.New("admin: --since must not be after --until")
	}
	return v, nil
}

func runUsageAdmin(env adminEnvironment, db *sql.DB, args []string) error {
	if len(args) < 1 {
		return errors.New("admin: usage requires a command")
	}
	command := args[0]
	v, err := parseUsageFlags(command, args[1:])
	if err != nil {
		return err
	}
	ledger := sqlite.NewLedger(db)
	switch command {
	case "requests":
		list, err := ledger.QueryRequests(env.ctx, sqlite.RequestFilter{VirtualKeyID: v.key, AccountID: v.account, Since: v.since, Until: v.until, Limit: v.limit, Offset: v.offset})
		if err != nil {
			return errors.New("admin: cannot query usage requests")
		}
		return writeUsageJSONOrText(env.stdout, v.json, requestOutput(list), func() error { return printUsageRequests(env.stdout, list) })
	case "attempts":
		list, err := ledger.QueryAttempts(env.ctx, sqlite.AttemptFilter{RequestID: v.request, AccountID: v.account, Since: v.since, Until: v.until, Limit: v.limit, Offset: v.offset})
		if err != nil {
			return errors.New("admin: cannot query usage attempts")
		}
		return writeUsageJSONOrText(env.stdout, v.json, attemptOutput(list), func() error { return printUsageAttempts(env.stdout, list) })
	case "summary":
		s, err := ledger.QueryUsageSummary(env.ctx, v.key, v.account, v.since, v.until)
		if err != nil {
			return errors.New("admin: cannot query usage summary")
		}
		return writeUsageJSONOrText(env.stdout, v.json, summaryOutput(s), func() error { return printUsageSummary(env.stdout, s) })
	default:
		return errors.New("admin: unknown usage command")
	}
}

func writeUsageJSONOrText(w io.Writer, asJSON bool, value any, text func() error) error {
	if !asJSON {
		return text()
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	return enc.Encode(value)
}
func ptrTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}
func ptrInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
func requestOutput(items []sqlite.RequestUsage) any {
	out := make([]map[string]any, 0, len(items))
	for _, x := range items {
		q := x.Request
		attempts := attemptOutput(x.Attempts)
		out = append(out, map[string]any{"id": q.ID, "key_id": q.VirtualKeyID, "policy_id": q.PolicyID, "protocol": q.Protocol, "model": q.Model, "route_id": q.RouteID, "key_revision": q.KeyRevision, "policy_revision": q.PolicyRevision, "accepted_at": q.AcceptedAt.UTC().Format(time.RFC3339), "state": q.State, "finished_at": ptrTime(q.FinishedAt), "attempts": attempts})
	}
	return out
}
func attemptOutput(items []sqlite.AttemptUsage) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, x := range items {
		a := x.Attempt
		var usage any
		if x.Usage != nil {
			u := x.Usage
			usage = map[string]any{"input_tokens": ptrInt(u.InputTokens), "output_tokens": ptrInt(u.OutputTokens), "reasoning_tokens": ptrInt(u.ReasoningTokens), "cached_tokens": ptrInt(u.CachedTokens), "source": u.Source, "completeness": u.Completeness, "recorded_at": u.RecordedAt.UTC().Format(time.RFC3339)}
		}
		out = append(out, map[string]any{"id": a.ID, "request_id": a.RequestID, "ordinal": a.Ordinal, "account_id": a.AccountID, "connector": a.Connector, "route_id": a.RouteID, "state": a.State, "estimate_tokens": a.EstimateTokens, "committed": a.Committed, "error_category": a.ErrorCategory, "error_reason": a.ErrorReason, "dispatched_at": ptrTime(a.DispatchedAt), "finished_at": ptrTime(a.FinishedAt), "usage": usage, "reservation": map[string]any{"estimated_tokens": x.Reservation.EstimatedTokens, "actual_tokens": ptrInt(x.Reservation.ActualTokens), "effective_charge": x.Reservation.EffectiveCharge, "state": x.Reservation.State}})
	}
	return out
}
func summaryOutput(s sqlite.UsageSummary) any {
	return map[string]any{"requests": s.Requests, "attempts": s.Attempts, "input_tokens": ptrInt(s.InputTokens), "output_tokens": ptrInt(s.OutputTokens), "reasoning_tokens": ptrInt(s.ReasoningTokens), "cached_tokens": ptrInt(s.CachedTokens), "estimated_tokens": s.EstimatedTokens, "actual_tokens": ptrInt(s.ActualTokens), "effective_charge": s.EffectiveCharge}
}
func tokenText(v *int64) string {
	if v == nil {
		return "unknown"
	}
	return fmt.Sprint(*v)
}
func printUsageRequests(w io.Writer, items []sqlite.RequestUsage) error {
	for _, x := range items {
		q := x.Request
		if _, err := fmt.Fprintf(w, "request=%s key=%s model=%s route=%s state=%s accepted_at=%s attempts=%d\n", q.ID, q.VirtualKeyID, q.Model, q.RouteID, q.State, q.AcceptedAt.UTC().Format(time.RFC3339), len(x.Attempts)); err != nil {
			return err
		}
		for _, a := range x.Attempts {
			if err := printUsageAttempt(w, a); err != nil {
				return err
			}
		}
	}
	return nil
}
func printUsageAttempts(w io.Writer, items []sqlite.AttemptUsage) error {
	for _, a := range items {
		if err := printUsageAttempt(w, a); err != nil {
			return err
		}
	}
	return nil
}
func printUsageAttempt(w io.Writer, x sqlite.AttemptUsage) error {
	a := x.Attempt
	in, out, reasoning, cached := "unknown", "unknown", "unknown", "unknown"
	if x.Usage != nil {
		in = tokenText(x.Usage.InputTokens)
		out = tokenText(x.Usage.OutputTokens)
		reasoning = tokenText(x.Usage.ReasoningTokens)
		cached = tokenText(x.Usage.CachedTokens)
	}
	finished := "-"
	if a.FinishedAt != nil {
		finished = a.FinishedAt.UTC().Format(time.RFC3339)
	}
	_, err := fmt.Fprintf(w, "attempt=%s request=%s ordinal=%d account=%s connector=%s state=%s estimate=%d input=%s output=%s reasoning=%s cached=%s effective_charge=%d error=%s finished_at=%s\n", a.ID, a.RequestID, a.Ordinal, a.AccountID, a.Connector, a.State, a.EstimateTokens, in, out, reasoning, cached, x.Reservation.EffectiveCharge, strings.TrimSpace(stringValue(a.ErrorReason)), finished)
	return err
}
func stringValue(v *string) string {
	if v == nil {
		return "-"
	}
	return *v
}
func printUsageSummary(w io.Writer, s sqlite.UsageSummary) error {
	_, err := fmt.Fprintf(w, "requests=%d attempts=%d input=%s output=%s reasoning=%s cached=%s estimated=%d actual=%s effective_charge=%d\n", s.Requests, s.Attempts, tokenText(s.InputTokens), tokenText(s.OutputTokens), tokenText(s.ReasoningTokens), tokenText(s.CachedTokens), s.EstimatedTokens, tokenText(s.ActualTokens), s.EffectiveCharge)
	return err
}
