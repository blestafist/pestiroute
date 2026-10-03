package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestAdminUsageHelpAndInvalidArgumentsDoNotOpenFiles(t *testing.T) {
	dbPath, keyPath := filepath.Join(t.TempDir(), "missing.db"), filepath.Join(t.TempDir(), "missing.key")
	for _, usageArgs := range [][]string{{"usage", "--help"}, {"usage", "requests", "--limit", "501"}, {"usage", "summary", "--since", "nope"}, {"usage", "other"}} {
		args := append([]string{"--db", dbPath, "--master-key", keyPath}, usageArgs...)
		var out, stderr bytes.Buffer
		err := runAdmin(adminEnvironment{args: args, stdout: &out, stderr: &stderr})
		if len(usageArgs) > 1 && usageArgs[1] == "--help" {
			if err != nil || !strings.Contains(out.String(), "usage: gateway admin") {
				t.Fatalf("help: %v %q", err, out.String())
			}
		} else if err == nil {
			t.Fatalf("accepted args %q", args)
		}
		if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
			t.Fatalf("opened database: %v", err)
		}
		if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
			t.Fatalf("opened key: %v", err)
		}
	}
}

func TestAdminUsageJSONIsNullableAndSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath, keyPath := filepath.Join(dir, "usage.db"), filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0x39}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	policy, err := sqlite.NewKeyPolicies(db).Create(ctx, sqlite.CreateKeyPolicyParams{ID: "policy"})
	if err != nil {
		t.Fatal(err)
	}
	issued, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	account, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: "account", Connector: "connector", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	ledger := sqlite.NewLedger(db)
	q := sqlite.RequestRecord{ID: "request", VirtualKeyID: issued.ID, KeyRevision: 1, PolicyID: policy.ID, PolicyRevision: policy.Revision, AcceptedAt: now, Protocol: "responses", Model: "model", RouteID: "route", State: "admitted"}
	if err := ledger.CreateRequest(ctx, q); err != nil {
		t.Fatal(err)
	}
	a := sqlite.AttemptRecord{ID: "attempt", RequestID: q.ID, Ordinal: 1, AccountID: account.ID, Connector: "connector", RouteID: "route", BudgetPolicy: "fixed", EstimateTokens: 10, EstimateMethod: "test", State: "reserved"}
	if err := ledger.CreateAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO reservations(attempt_id,estimated_tokens,state) VALUES (?,?,'held')`, a.ID, a.EstimateTokens); err != nil {
		t.Fatal(err)
	}
	if err := ledger.RecordDispatchIntent(ctx, a.ID, now); err != nil {
		t.Fatal(err)
	}
	finished := now.Add(time.Second)
	usage := sqlite.UsageRecord{AttemptID: a.ID, Source: "unknown", Completeness: "unknown", RecordedAt: finished}
	if err := ledger.FinalizeAttempt(ctx, sqlite.TerminalAttempt{AttemptID: a.ID, State: "interrupted", Usage: usage, FinishedAt: finished}); err != nil {
		t.Fatal(err)
	}
	commands := map[string][]string{
		"requests": {"usage", "requests", "--key", issued.ID, "--json"},
		"attempts": {"usage", "attempts", "--request", "request", "--account", "account", "--json"},
		"summary":  {"usage", "summary", "--key", issued.ID, "--json"},
	}
	before := make(map[string]string, len(commands))
	for name, command := range commands {
		var output, errOutput bytes.Buffer
		args := append([]string{"--db", dbPath, "--master-key", keyPath}, command...)
		if err := runAdmin(adminEnvironment{ctx: ctx, args: args, stdout: &output, stderr: &errOutput}); err != nil {
			t.Fatalf("pre-reopen %s: %v %s", name, err, errOutput.String())
		}
		before[name] = output.String()
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	args := []string{"--db", dbPath, "--master-key", keyPath, "usage", "requests", "--key", issued.ID, "--json"}
	if err := runAdmin(adminEnvironment{ctx: ctx, args: args, stdout: &out, stderr: &stderr}); err != nil {
		t.Fatalf("run usage: %v stderr=%q", err, stderr.String())
	}
	if out.String() != before["requests"] {
		t.Fatalf("request output changed after reopen: before=%q after=%q", before["requests"], out.String())
	}
	var requests []map[string]any
	if err := json.Unmarshal(out.Bytes(), &requests); err != nil {
		t.Fatalf("invalid JSON: %v %q", err, out.String())
	}
	if len(requests) != 1 || requests[0]["accepted_at"] != "2026-10-03T12:00:00Z" {
		t.Fatalf("request output: %#v", requests)
	}
	attempts := requests[0]["attempts"].([]any)
	attempt := attempts[0].(map[string]any)
	gotUsage := attempt["usage"].(map[string]any)
	if gotUsage["input_tokens"] != nil || gotUsage["output_tokens"] != nil || attempt["account_id"] != "account" || strings.Contains(out.String(), "secret") {
		t.Fatalf("usage output: %s", out.String())
	}
	out.Reset()
	args = []string{"--db", dbPath, "--master-key", keyPath, "usage", "summary", "--key", issued.ID, "--json"}
	if err := runAdmin(adminEnvironment{ctx: ctx, args: args, stdout: &out, stderr: &stderr}); err != nil {
		t.Fatal(err)
	}
	if out.String() != before["summary"] {
		t.Fatalf("summary output changed after reopen: before=%q after=%q", before["summary"], out.String())
	}
	var summary map[string]any
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["requests"] != float64(1) || summary["input_tokens"] != nil || summary["actual_tokens"] != nil || summary["estimated_tokens"] != float64(10) || summary["effective_charge"] != float64(10) {
		t.Fatalf("summary output: %s", out.String())
	}
	out.Reset()
	args = []string{"--db", dbPath, "--master-key", keyPath, "usage", "attempts", "--request", "request", "--account", "account", "--json"}
	if err := runAdmin(adminEnvironment{ctx: ctx, args: args, stdout: &out, stderr: &stderr}); err != nil {
		t.Fatal(err)
	}
	if out.String() != before["attempts"] {
		t.Fatalf("attempt output changed after reopen: before=%q after=%q", before["attempts"], out.String())
	}
	var attemptRows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &attemptRows); err != nil || len(attemptRows) != 1 || attemptRows[0]["ordinal"] != float64(1) {
		t.Fatalf("attempt output: %s err=%v", out.String(), err)
	}
	out.Reset()
	args = []string{"--db", dbPath, "--master-key", keyPath, "usage", "attempts", "--request", "request"}
	if err := runAdmin(adminEnvironment{ctx: ctx, args: args, stdout: &out, stderr: &stderr}); err != nil || !strings.Contains(out.String(), "ordinal=1") || !strings.Contains(out.String(), "input=unknown") || strings.Contains(out.String(), "secret=") {
		t.Fatalf("text attempt output: %v %q", err, out.String())
	}
	db, err = sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger = sqlite.NewLedger(db)
	overflowRequest := q
	overflowRequest.ID = "overflow-request"
	overflowRequest.AcceptedAt = now.Add(2 * time.Second)
	if err := ledger.CreateRequest(ctx, overflowRequest); err != nil {
		t.Fatal(err)
	}
	overflowAttempt := sqlite.AttemptRecord{ID: "overflow-attempt", RequestID: overflowRequest.ID, Ordinal: 1, AccountID: account.ID, Connector: "connector", RouteID: "route", BudgetPolicy: "fixed", EstimateTokens: 1, EstimateMethod: "test", State: "reserved"}
	if err := ledger.CreateAttempt(ctx, overflowAttempt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO reservations(attempt_id,estimated_tokens,state) VALUES (?,1,'held')`, overflowAttempt.ID); err != nil {
		t.Fatal(err)
	}
	if err := ledger.RecordDispatchIntent(ctx, overflowAttempt.ID, overflowRequest.AcceptedAt); err != nil {
		t.Fatal(err)
	}
	input, output := int64(1), int64(0)
	overflowUsage := sqlite.UsageRecord{AttemptID: overflowAttempt.ID, InputTokens: &input, OutputTokens: &output, Source: "provider", Completeness: "complete", RecordedAt: finished}
	if err := ledger.FinalizeAttempt(ctx, sqlite.TerminalAttempt{AttemptID: overflowAttempt.ID, State: "succeeded", Committed: true, Usage: overflowUsage, FinishedAt: finished}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE usage_records SET input_tokens=9223372036854775807 WHERE attempt_id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	args = []string{"--db", dbPath, "--master-key", keyPath, "usage", "summary", "--key", issued.ID, "--json"}
	err = runAdmin(adminEnvironment{ctx: ctx, args: args, stdout: &out, stderr: &stderr})
	if err == nil || err.Error() != "admin: cannot query usage summary" || out.Len() != 0 || strings.Contains(err.Error()+stderr.String(), "SQLITE") {
		t.Fatalf("overflow response err=%v stdout=%q stderr=%q", err, out.String(), stderr.String())
	}
}
