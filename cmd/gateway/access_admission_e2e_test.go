package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func startAccessAdmissionFixture(t *testing.T, rpm, tpm int64, upstreamURL string) (*http.Client, string, string, string, string, func(...string)) {
	t.Helper()
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "configured-connector-secret")
	_, dbPath, keyPath := protectedFixture(t)
	keyOutput, err := runAdminTest(t, dbPath, keyPath, nil, "key", "create", "--policy", "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	var secret, keyID string
	for _, field := range strings.Fields(keyOutput) {
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch name {
		case "secret":
			secret = value
		case "id":
			keyID = value
		}
	}
	if secret == "" || keyID == "" {
		t.Fatalf("admin key create returned incomplete metadata: %q", keyOutput)
	}
	admin := func(args ...string) {
		t.Helper()
		if output, err := runAdminTest(t, dbPath, keyPath, nil, args...); err != nil {
			t.Fatalf("admin %v: %v (%s)", args, err, output)
		}
	}
	admin("policy", "update", "policy-id-a", "--expected-revision", "1", "--rpm", fmt.Sprint(rpm), "--tpm", fmt.Sprint(tpm))
	admin("key", "update-policy", keyID, "--policy", "policy-id-a", "--policy-revision", "2", "--expected-revision", "1")
	address := freeTCPAddress(t)
	c := fixtureProtectedConfig(dbPath, keyPath, address)
	c.protected.Connectors[0].Settings.BaseURL = upstreamURL + "/v1"
	c.protected.Connectors[0].Settings.ResponseHeaderTimeout = "15s"
	c.protected.Connectors[0].Settings.StreamIdleTimeout = "15s"
	prepared, err := prepareProtectedConfig(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	var ready, draining atomic.Bool
	h, closeComponents, err := composeHandler(prepared, &ready, &draining, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = closeComponents(ctx)
		_ = prepared.runtimeDB.Close()
		_ = prepared.processLock.Close()
		server.Close()
	})
	client := &http.Client{Timeout: 30 * time.Second}
	return client, server.URL, secret, keyID, dbPath, func(args ...string) { admin(args...) }
}

func accessRequest(t *testing.T, client *http.Client, endpoint, secret, model string) (*http.Response, []byte) {
	t.Helper()
	authorization := ""
	if secret != "" {
		authorization = "Bearer " + secret
	}
	return accessRequestWithAuthorization(t, client, endpoint, authorization, model)
}

func accessRequestWithAuthorization(t *testing.T, client *http.Client, endpoint, authorization, model string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint+"/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"hi"}`, model)))
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func TestAccessRevocation(t *testing.T) {
	var calls atomic.Int32
	var upstreamAuth atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		upstreamAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"completed"}`)
	}))
	defer upstream.Close()
	client, endpoint, secret, keyID, dbPath, admin := startAccessAdmissionFixture(t, 10, 1000, upstream.URL)
	for _, authorization := range []string{"", "Basic malformed-token", "Bearer malformed-token"} {
		invalid, body := accessRequestWithAuthorization(t, client, endpoint, authorization, "model-a")
		if invalid.StatusCode != http.StatusUnauthorized || calls.Load() != 0 || authorization != "" && strings.Contains(string(body), authorization) {
			t.Fatalf("invalid bearer %q: status=%d calls=%d body=%s", authorization, invalid.StatusCode, calls.Load(), body)
		}
	}
	resp, _ := accessRequest(t, client, endpoint, secret, "model-a")
	if resp.StatusCode != http.StatusOK || calls.Load() != 1 || upstreamAuth.Load() != "Bearer synthetic" {
		t.Fatalf("valid key: status=%d calls=%d upstream auth=%v", resp.StatusCode, calls.Load(), upstreamAuth.Load())
	}
	admin("account", "disable", "account-a")
	resp, body := accessRequest(t, client, endpoint, secret, "model-a")
	if resp.StatusCode != http.StatusForbidden || calls.Load() != 1 || strings.Contains(string(body), secret) {
		t.Fatalf("disabled account: status=%d calls=%d body=%s", resp.StatusCode, calls.Load(), body)
	}
	admin("account", "enable", "account-a")
	admin("key", "revoke", keyID)
	resp, body = accessRequest(t, client, endpoint, secret, "model-a")
	if resp.StatusCode != http.StatusUnauthorized || calls.Load() != 1 || strings.Contains(string(body), secret) {
		t.Fatalf("revoked key: status=%d calls=%d body=%s", resp.StatusCode, calls.Load(), body)
	}
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var requests, reservations, usage int
	if err := db.QueryRow(`SELECT count(*) FROM requests`).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM reservations`).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM usage_records`).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if requests != 1 || reservations != 1 || usage != 1 {
		t.Fatalf("rejected requests changed durable accounting: requests=%d reservations=%d usage=%d", requests, reservations, usage)
	}
}

func TestLivePolicyUpdate(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"completed"}`)
	}))
	defer upstream.Close()
	client, endpoint, secret, keyID, _, admin := startAccessAdmissionFixture(t, 10, 1000, upstream.URL)
	resp, _ := accessRequest(t, client, endpoint, secret, "model-a")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("baseline status %d", resp.StatusCode)
	}
	admin("policy", "update", "policy-id-a", "--expected-revision", "2", "--models", "other-model")
	admin("key", "update-policy", keyID, "--policy", "policy-id-a", "--policy-revision", "3", "--expected-revision", "2")
	resp, body := accessRequest(t, client, endpoint, secret, "model-a")
	if resp.StatusCode != http.StatusForbidden || strings.Contains(string(body), secret) || calls.Load() != 1 {
		t.Fatalf("revoked scope: status=%d calls=%d body=%s", resp.StatusCode, calls.Load(), body)
	}
	admin("policy", "update", "policy-id-a", "--expected-revision", "3", "--models", "model-a", "--connectors", "other-connector")
	admin("key", "update-policy", keyID, "--policy", "policy-id-a", "--policy-revision", "4", "--expected-revision", "3")
	resp, body = accessRequest(t, client, endpoint, secret, "model-a")
	if resp.StatusCode != http.StatusForbidden || strings.Contains(string(body), secret) || calls.Load() != 1 {
		t.Fatalf("forbidden connector: status=%d calls=%d body=%s", resp.StatusCode, calls.Load(), body)
	}
}

func durableAdmissionSnapshot(t *testing.T, db *sql.DB) []string {
	t.Helper()
	var snapshot []string
	for _, table := range []string{"requests", "attempts", "reservations", "usage_records"} {
		rows, err := db.Query(`SELECT * FROM ` + table + ` ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			dest := make([]any, len(columns))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			snapshot = append(snapshot, fmt.Sprintf("%s:%#v", table, values))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return snapshot
}

func TestAdmissionConcurrentLimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rpm, tpm  int64
		wantAdmit int
	}{
		{name: "rpm", rpm: 3, tpm: 1000, wantAdmit: 3},
		{name: "tpm", rpm: 100, tpm: 30, wantAdmit: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			var auth atomic.Value
			entered := make(chan struct{}, 8)
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Errorf("read upstream request: %v", err)
					return
				}
				calls.Add(1)
				auth.Store(r.Header.Get("Authorization"))
				entered <- struct{}{}
				<-release
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"completed"}`)
			}))
			defer upstream.Close()
			defer releaseGate()
			client, endpoint, secret, keyID, dbPath, admin := startAccessAdmissionFixture(t, tc.rpm, tc.tpm, upstream.URL)
			const workers = 8
			start := make(chan struct{})
			statuses := make(chan int, workers)
			var wg sync.WaitGroup
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					resp, body := accessRequest(t, client, endpoint, secret, "model-a")
					if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusTooManyRequests {
						t.Errorf("unexpected admission response %d: %s", resp.StatusCode, body)
					}
					if strings.Contains(string(body), secret) {
						t.Errorf("client response leaked virtual key")
					}
					statuses <- resp.StatusCode
				}()
			}
			close(start)
			admitted, rejected, outcomes := 0, 0, 0
			for outcomes < workers {
				select {
				case <-entered:
					admitted++
					outcomes++
				case status := <-statuses:
					if status != http.StatusTooManyRequests {
						t.Fatalf("request completed before release with status %d", status)
					}
					rejected++
					outcomes++
				}
			}
			if admitted != tc.wantAdmit || rejected != workers-tc.wantAdmit || int(calls.Load()) != tc.wantAdmit {
				t.Fatalf("held=%d rejected=%d upstream=%d, want %d/%d/%d", admitted, rejected, calls.Load(), tc.wantAdmit, workers-tc.wantAdmit, tc.wantAdmit)
			}
			if got := auth.Load(); got != "Bearer synthetic" {
				t.Fatalf("upstream credential = %v", got)
			}
			db, err := sqlite.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			type heldAttempt struct{ requestID, attemptID string }
			var held []heldAttempt
			rows, err := db.Query(`SELECT q.id,q.virtual_key_id,q.key_revision,q.policy_id,q.policy_revision,q.model,q.state,a.id,a.request_id,a.account_id,a.connector,a.estimate_tokens,a.state,r.estimated_tokens,r.state,r.effective_charge FROM requests q JOIN attempts a ON a.request_id=q.id JOIN reservations r ON r.attempt_id=a.id ORDER BY q.id`)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var requestID, rowKeyID, policyID, model, requestState, attemptID, attemptRequestID, accountID, connector, attemptState, reservationState string
				var keyRevision, policyRevision, estimate, estimateHeld int64
				var charge sql.NullInt64
				if err := rows.Scan(&requestID, &rowKeyID, &keyRevision, &policyID, &policyRevision, &model, &requestState, &attemptID, &attemptRequestID, &accountID, &connector, &estimate, &attemptState, &estimateHeld, &reservationState, &charge); err != nil {
					t.Fatal(err)
				}
				if rowKeyID != keyID || keyRevision != 2 || policyID != "policy-id-a" || policyRevision != 2 || model != "model-a" || requestState != "admitted" || attemptRequestID != requestID || accountID != "account-a" || connector != "upstream" || estimate != 10 || estimateHeld != 10 || attemptState != "intent" || reservationState != "held" || charge.Valid {
					t.Fatalf("unexpected held admission: request=%s attempt=%s key=%s/%d policy=%s/%d account=%s connector=%s estimate=%d/%d states=%s/%s charge=%v", requestID, attemptID, rowKeyID, keyRevision, policyID, policyRevision, accountID, connector, estimate, estimateHeld, attemptState, reservationState, charge)
				}
				held = append(held, heldAttempt{requestID: requestID, attemptID: attemptID})
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			var heldTotal, usageRows int64
			if err := db.QueryRow(`SELECT COALESCE(sum(estimated_tokens),0) FROM reservations WHERE state='held'`).Scan(&heldTotal); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM usage_records`).Scan(&usageRows); err != nil {
				t.Fatal(err)
			}
			if len(held) != tc.wantAdmit || heldTotal != int64(tc.wantAdmit)*10 || heldTotal > tc.tpm || usageRows != 0 {
				t.Fatalf("pre-release admitted=%d held=%d usage=%d limit=%d", len(held), heldTotal, usageRows, tc.tpm)
			}
			beforeReject := durableAdmissionSnapshot(t, db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			extra, body := accessRequest(t, client, endpoint, secret, "model-a")
			if extra.StatusCode != http.StatusTooManyRequests || strings.Contains(string(body), secret) || int(calls.Load()) != tc.wantAdmit {
				t.Fatalf("extra rejection status=%d upstream=%d body=%s", extra.StatusCode, calls.Load(), body)
			}
			db, err = sqlite.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			afterReject := durableAdmissionSnapshot(t, db)
			if !slices.Equal(beforeReject, afterReject) {
				t.Fatal("rejected request changed existing durable admission rows")
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			releaseGate()
			wg.Wait()
			close(statuses)
			completed := 0
			for status := range statuses {
				if status != http.StatusOK {
					t.Errorf("admitted request final status %d", status)
				} else {
					completed++
				}
			}
			if completed != tc.wantAdmit {
				t.Fatalf("completed responses=%d, want %d", completed, tc.wantAdmit)
			}
			db, err = sqlite.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			ledger := sqlite.NewLedger(db)
			finalized := false
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for !finalized {
				finalized = true
				for _, item := range held {
					reservation, err := ledger.GetReservation(context.Background(), item.attemptID)
					if err != nil {
						t.Fatal(err)
					}
					request, requestErr := ledger.GetRequest(context.Background(), item.requestID)
					if requestErr != nil {
						t.Fatal(requestErr)
					}
					if reservation.State == "held" || request.State == "admitted" {
						finalized = false
						break
					}
				}
				if finalized {
					break
				}
				select {
				case <-ticker.C:
				case <-deadline.C:
					t.Fatal("admitted attempt reservations did not settle after upstream release")
				}
			}
			for _, item := range held {
				request, err := ledger.GetRequest(context.Background(), item.requestID)
				if err != nil || request.VirtualKeyID != keyID || request.KeyRevision != 2 || request.PolicyID != "policy-id-a" || request.PolicyRevision != 2 || request.Model != "model-a" || request.State == "admitted" || request.FinishedAt == nil {
					t.Fatalf("final request identity/lifecycle %+v, %v", request, err)
				}
				attempt, err := ledger.GetAttempt(context.Background(), item.attemptID)
				if err != nil {
					t.Fatal(err)
				}
				if attempt.RequestID != item.requestID {
					t.Fatalf("final attempt request %q, want %q", attempt.RequestID, item.requestID)
				}
				if attempt.AccountID != "account-a" || attempt.Connector != "upstream" || attempt.EstimateTokens != 10 {
					t.Fatalf("final attempt account=%q connector=%q estimate=%d", attempt.AccountID, attempt.Connector, attempt.EstimateTokens)
				}
				if attempt.State == "reserved" || attempt.State == "intent" {
					t.Fatalf("attempt not terminal after upstream release: state=%q", attempt.State)
				}
				usage, err := ledger.GetUsage(context.Background(), item.attemptID)
				if err != nil || usage.Source != "unknown" || usage.Completeness != "unknown" || usage.InputTokens != nil || usage.OutputTokens != nil || usage.ReasoningTokens != nil || usage.CachedTokens != nil {
					t.Fatalf("final usage %+v, %v", usage, err)
				}
				reservation, err := ledger.GetReservation(context.Background(), item.attemptID)
				if err != nil || reservation.EstimatedTokens != 10 || reservation.State != "conservative" || reservation.EffectiveCharge != 10 || reservation.ActualTokens != nil || reservation.ReconciledAt == nil {
					t.Fatalf("final reservation %+v, %v", reservation, err)
				}
			}
			for _, table := range []string{"requests", "attempts", "reservations", "usage_records"} {
				var rows int
				if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&rows); err != nil {
					t.Fatal(err)
				}
				if rows != tc.wantAdmit {
					t.Fatalf("final %s rows=%d, want exactly %d", table, rows, tc.wantAdmit)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			admin("policy", "update", "policy-id-a", "--expected-revision", "2", "--rpm", "100", "--tpm", "1000")
			admin("key", "update-policy", keyID, "--policy", "policy-id-a", "--policy-revision", "3", "--expected-revision", "2")
			resp, body := accessRequest(t, client, endpoint, secret, "model-a")
			if resp.StatusCode != http.StatusOK || strings.Contains(string(body), secret) || calls.Load() != int32(tc.wantAdmit+1) {
				t.Fatalf("authorized quota update did not restore admission: status=%d upstream=%d body=%s", resp.StatusCode, calls.Load(), body)
			}
		})
	}
}
