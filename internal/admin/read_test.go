package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestUsageHTTPReadsLedgerPagesFiltersAndSummary(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	policy, err := sqlite.NewKeyPolicies(db).Create(ctx, sqlite.CreateKeyPolicyParams{ID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: "acct", Connector: "conn", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: "acct-b", Connector: "conn", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	keyB, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	ledger := sqlite.NewLedger(db)
	base := time.Date(2026, 10, 17, 12, 0, 0, 123456000, time.UTC)
	for i := range 3 {
		id := fmt.Sprintf("req-%d", i)
		accepted := base.Add(time.Duration(i) * time.Second)
		if err := ledger.CreateRequest(ctx, sqlite.RequestRecord{ID: id, VirtualKeyID: key.ID, KeyRevision: 1, PolicyID: policy.ID, PolicyRevision: 1, AcceptedAt: accepted, Protocol: "p", Model: "m", RouteID: "r", State: "admitted"}); err != nil {
			t.Fatal(err)
		}
		attemptID := "att-" + id
		if err := ledger.CreateAttempt(ctx, sqlite.AttemptRecord{ID: attemptID, RequestID: id, Ordinal: 1, AccountID: "acct", Connector: "conn", RouteID: "r", BudgetPolicy: "known", EstimateTokens: 20, EstimateMethod: "test", State: "reserved"}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO reservations(attempt_id,estimated_tokens,state) VALUES(?,20,'held')`, attemptID); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			in, out := int64(11), int64(7)
			finished := accepted.Add(time.Millisecond)
			if err := ledger.FinalizeAttempt(ctx, sqlite.TerminalAttempt{AttemptID: attemptID, State: "succeeded", Committed: true, Usage: sqlite.UsageRecord{AttemptID: attemptID, InputTokens: &in, OutputTokens: &out, Source: "fixture", Completeness: "complete", RecordedAt: base.Add(123 * time.Millisecond)}, FinishedAt: finished}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `UPDATE reservations SET actual_tokens=18,effective_charge=17,state='settled' WHERE attempt_id=?`, attemptID); err != nil {
				t.Fatal(err)
			}
			pending := sqlite.AttemptRecord{ID: "att-pending", RequestID: id, Ordinal: 2, AccountID: "acct", Connector: "conn", RouteID: "r", BudgetPolicy: "known", EstimateTokens: 20, EstimateMethod: "test", State: "reserved"}
			if err := ledger.CreateAttempt(ctx, pending); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO reservations(attempt_id,estimated_tokens,state) VALUES(?,20,'held')`, pending.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	crossRequest := sqlite.RequestRecord{ID: "req-cross", VirtualKeyID: key.ID, KeyRevision: 1, PolicyID: policy.ID, PolicyRevision: 1, AcceptedAt: base.Add(3 * time.Second), Protocol: "p", Model: "m", RouteID: "r", State: "admitted"}
	if err := ledger.CreateRequest(ctx, crossRequest); err != nil {
		t.Fatal(err)
	}
	if err := ledger.CreateAttempt(ctx, sqlite.AttemptRecord{ID: "att-cross", RequestID: crossRequest.ID, Ordinal: 1, AccountID: "acct-b", Connector: "conn", RouteID: "r", BudgetPolicy: "known", EstimateTokens: 6, EstimateMethod: "test", State: "reserved"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO reservations(attempt_id,estimated_tokens,effective_charge,state) VALUES('att-cross',6,3,'conservative')`); err != nil {
		t.Fatal(err)
	}
	otherRequest := sqlite.RequestRecord{ID: "req-other", VirtualKeyID: keyB.ID, KeyRevision: 1, PolicyID: policy.ID, PolicyRevision: 1, AcceptedAt: base.Add(4 * time.Second), Protocol: "p", Model: "m", RouteID: "r", State: "admitted"}
	if err := ledger.CreateRequest(ctx, otherRequest); err != nil {
		t.Fatal(err)
	}
	if err := ledger.CreateAttempt(ctx, sqlite.AttemptRecord{ID: "att-other", RequestID: otherRequest.ID, Ordinal: 1, AccountID: "acct", Connector: "conn", RouteID: "r", BudgetPolicy: "known", EstimateTokens: 5, EstimateMethod: "test", State: "reserved"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO reservations(attempt_id,estimated_tokens,actual_tokens,effective_charge,state) VALUES('att-other',5,NULL,4,'conservative')`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PESTIROUTE_ADMIN_TEST_TOKEN", "operator")
	h, err := New(Config{Address: "127.0.0.1:9093", TokenEnv: "PESTIROUTE_ADMIN_TEST_TOKEN", ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second, DB: db})
	if err != nil {
		t.Fatal(err)
	}
	get := func(target string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r.Host = "127.0.0.1:9093"
		r.Header.Set("Authorization", "Bearer operator")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	var page struct {
		Items                []map[string]json.RawMessage `json:"items"`
		Limit, Offset, Total int
	}
	w := get("/admin/v1/usage/requests?limit=1&offset=1&key_id=" + key.ID + "&account_id=acct")
	if w.Code != 200 {
		t.Fatalf("requests status=%d body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Limit != 1 || page.Offset != 1 || page.Total != 3 || len(page.Items) != 1 {
		t.Fatalf("request page=%s", w.Body.String())
	}
	var reqID string
	if err := json.Unmarshal(page.Items[0]["id"], &reqID); err != nil {
		t.Fatal(err)
	}
	if reqID != "req-1" {
		t.Fatalf("request page order: %q", reqID)
	}
	for _, tc := range []struct {
		target, id string
		total      int
	}{
		{"/admin/v1/usage/requests?key_id=" + keyB.ID, "req-other", 1},
		{"/admin/v1/usage/requests?account_id=acct-b", "req-cross", 1},
		{"/admin/v1/usage/requests?key_id=" + keyB.ID + "&account_id=acct-b", "", 0},
	} {
		w = get(tc.target)
		if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(fmt.Sprintf(`"total":%d`, tc.total))) {
			t.Fatalf("independent request filter %s: %d %s", tc.target, w.Code, w.Body.String())
		}
		if tc.id != "" && !bytes.Contains(w.Body.Bytes(), []byte(`"id":"`+tc.id+`"`)) {
			t.Fatalf("request filter %s omitted %s: %s", tc.target, tc.id, w.Body.String())
		}
	}
	w = get("/admin/v1/usage/attempts?account_id=acct&since=" + base.Format(time.RFC3339Nano) + "&until=" + base.Add(time.Second).Format(time.RFC3339Nano) + "&limit=1")
	if w.Code != 200 {
		t.Fatalf("attempts status=%d body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Items) != 1 {
		t.Fatalf("attempt page=%s", w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"recorded_at":"2026-10-17T12:00:00.246Z"`)) {
		t.Fatalf("recorded_at precision lost: %s", w.Body.String())
	}
	for _, field := range []string{`"effective_charge":17`, `"actual_tokens":18`, `"reasoning_tokens":null`} {
		if !bytes.Contains(w.Body.Bytes(), []byte(field)) {
			t.Fatalf("attempt response missing %s: %s", field, w.Body.String())
		}
	}
	for _, tc := range []struct {
		target, id string
		total      int
	}{
		{"/admin/v1/usage/attempts?request_id=req-other", "att-other", 1},
		{"/admin/v1/usage/attempts?account_id=acct-b", "att-cross", 1},
		{"/admin/v1/usage/attempts?request_id=req-other&account_id=acct-b", "", 0},
	} {
		w = get(tc.target)
		if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(fmt.Sprintf(`"total":%d`, tc.total))) {
			t.Fatalf("independent attempt filter %s: %d %s", tc.target, w.Code, w.Body.String())
		}
		if tc.id != "" && !bytes.Contains(w.Body.Bytes(), []byte(`"id":"`+tc.id+`"`)) {
			t.Fatalf("attempt filter %s omitted %s: %s", tc.target, tc.id, w.Body.String())
		}
	}
	w = get("/admin/v1/usage/attempts?since=2026-10-17T12:00:00.123456Z&until=2026-10-17T12:00:00.123999Z")
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"total":0`)) {
		t.Fatalf("fractional bounds included earlier millisecond event: %s", w.Body.String())
	}
	w = get("/admin/v1/usage/attempts?since=2026-10-17T12:00:00.123Z&until=2026-10-17T12:00:00.123999Z")
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"total":1`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"id":"att-pending"`)) {
		t.Fatalf("inclusive upper bound did not include stored millisecond event: %s", w.Body.String())
	}
	for _, tc := range []struct{ target, expected string }{
		{"/admin/v1/usage/requests?key_id=" + key.ID + "&since=2026-10-17T12:00:00.123456Z&until=2026-10-17T12:00:00.123999Z", `"total":0`},
		{"/admin/v1/usage/requests?key_id=" + key.ID + "&since=2026-10-17T12:00:00.123Z&until=2026-10-17T12:00:00.123999Z", `"total":1`},
	} {
		w = get(tc.target)
		if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(tc.expected)) {
			t.Fatalf("request bounds %s: %d %s", tc.target, w.Code, w.Body.String())
		}
	}
	w = get("/admin/v1/usage/summary?key_id=" + key.ID + "&since=2026-10-17T12:00:00.123456Z&until=2026-10-17T12:00:00.123999Z")
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"requests":0`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"attempts":0`)) {
		t.Fatalf("fractional summary lower bound: %d %s", w.Code, w.Body.String())
	}
	w = get("/admin/v1/usage/summary?key_id=" + key.ID + "&since=2026-10-17T12:00:00.123Z&until=2026-10-17T12:00:00.123999Z")
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"requests":1`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"attempts":2`)) {
		t.Fatalf("summary inclusive truncated upper bound: %d %s", w.Code, w.Body.String())
	}
	w = get("/admin/v1/usage/summary?key_id=" + key.ID)
	if w.Code != 200 {
		t.Fatalf("summary status=%d body=%s", w.Code, w.Body.String())
	}
	for _, field := range []string{`"input_tokens":null`, `"output_tokens":null`, `"reasoning_tokens":null`, `"cached_tokens":null`, `"actual_tokens":null`} {
		if !bytes.Contains(w.Body.Bytes(), []byte(field)) {
			t.Fatalf("summary missing %s: %s", field, w.Body.String())
		}
	}
	for _, field := range []string{`"requests":4`, `"attempts":5`, `"estimated_tokens":86`, `"effective_charge":20`} {
		if !bytes.Contains(w.Body.Bytes(), []byte(field)) {
			t.Fatalf("summary missing %s: %s", field, w.Body.String())
		}
	}
	for _, tc := range []struct {
		target string
		checks []string
	}{
		{"/admin/v1/usage/summary?key_id=" + keyB.ID, []string{`"requests":1`, `"attempts":1`, `"estimated_tokens":5`, `"effective_charge":4`}},
		{"/admin/v1/usage/summary?account_id=acct-b", []string{`"requests":1`, `"attempts":1`, `"estimated_tokens":6`, `"effective_charge":3`}},
		{"/admin/v1/usage/summary?key_id=" + keyB.ID + "&account_id=acct-b", []string{`"requests":0`, `"attempts":0`, `"estimated_tokens":0`, `"effective_charge":0`}},
	} {
		w = get(tc.target)
		if w.Code != 200 {
			t.Fatalf("summary filter %s: %d %s", tc.target, w.Code, w.Body.String())
		}
		for _, check := range tc.checks {
			if !bytes.Contains(w.Body.Bytes(), []byte(check)) {
				t.Fatalf("summary filter %s missing %s: %s", tc.target, check, w.Body.String())
			}
		}
	}
}
