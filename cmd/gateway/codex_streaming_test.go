package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/connector/codex"
	"github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestCodexSettlementStreamingAndTerminalUsage(t *testing.T) {
	_, dbPath, keyPath := protectedFixture(t)
	ctx := context.Background()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const accountID, connectorID, model = "codex-account", "codex", "codex-model"
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: accountID, Connector: connectorID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	bundle := fmt.Sprintf(`{"version":1,"access_token":%q,"refresh_token":"synthetic-refresh","account_id":%q,"expires_at":%q}`, codexTestJWT(accountID), accountID, expires.Format(time.RFC3339Nano))
	sealed, err := secure.Seal(key, 1, "v1", "credentials", "oauth", accountID, []byte(bundle))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "oauth", AccountID: accountID, FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext, ExpiresAt: &expires}); err != nil {
		t.Fatal(err)
	}
	policy, err := sqlite.NewKeyPolicies(db).GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	policy, err = sqlite.NewKeyPolicies(db).Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{model, "model-a"}, Connectors: []string{connectorID, "upstream"}, RPM: 20, TPM: 10000})
	if err != nil {
		t.Fatal(err)
	}
	issued, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		name, terminal, state string
		input, output         *int64
		completeness          string
	}
	input, output := int64(7), int64(3)
	cases := []outcome{
		{name: "completed", terminal: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"input_tokens_details\":{\"cached_tokens\":2},\"output_tokens_details\":{\"reasoning_tokens\":1}}}}\n\n", state: "succeeded", input: &input, output: &output, completeness: "complete"},
		{name: "incomplete", terminal: "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n", state: "incomplete", input: &input, output: &output, completeness: "partial"},
		{name: "failed", terminal: "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n", state: "failed", input: &input, output: &output, completeness: "partial"},
		{name: "error", terminal: "event: error\ndata: {\"type\":\"error\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n", state: "failed", input: &input, output: &output, completeness: "partial"},
		{name: "eof", state: "incomplete", input: &input, output: new(int64(0)), completeness: "partial"},
	}
	started := make([]chan struct{}, len(cases))
	release := make([]chan struct{}, len(cases))
	var upstreamCalls atomic.Int32
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := int(upstreamCalls.Add(1)) - 1
		if idx >= len(cases) {
			t.Errorf("unexpected upstream request %d", idx+1)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+codexTestJWT(accountID) || r.Header.Get("ChatGPT-Account-Id") != accountID {
			t.Errorf("Codex credential scope mismatch")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, codexStreamPrefix)
		w.(http.Flusher).Flush()
		close(started[idx])
		<-release[idx]
		_, _ = io.WriteString(w, cases[idx].terminal)
	}))
	defer backend.Close()
	for i := range cases {
		started[i], release[i] = make(chan struct{}), make(chan struct{})
	}
	addr := backend.Listener.Addr().String()
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}
	defer transport.CloseIdleConnections()
	doer := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	p := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0").protected
	p.Connectors = append(p.Connectors, protectedConnector{ID: connectorID, Kind: "connector", Implementation: "pestiroute.codex.responses", Protocols: []string{responsesProtocol}, Settings: nativeSettings{Profile: "codex-responses-http-sse-v1", Model: model, AccountID: accountID}})
	p.Routes = append(p.Routes, protectedRoute{ID: "codex-route", Protocol: responsesProtocol, Mode: "native", Model: model, Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(100)}, Targets: []routeTarget{{Connector: connectorID, Account: accountID}}})
	prepared, err := prepareProtectedConfig(ctx, config{protected: p, DatabasePath: dbPath, MasterKeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	calls := &accountingCallCounters{}
	prepared.accounting = &countedSQLiteAccountingStore{store: sqliteAccountingStore{ledger: sqlite.NewLedger(prepared.runtimeDB)}, calls: calls}
	ready, draining := atomic.Bool{}, atomic.Bool{}
	ready.Store(true)
	h, closeComponents, err := composeHandlerWithFactory(prepared, &ready, &draining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		if item.Implementation == "pestiroute.codex.responses" {
			return codexCompositionConnector{Connector: codex.NewConnector(), doer: doer}
		}
		return responses.NewConnector()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeComponents(context.Background()) })
	gateway := httptest.NewServer(h)
	defer gateway.Close()
	client := gateway.Client()
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"input":"hello"}`, model)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+issued.Secret)
			req.Header.Set("Content-Type", "application/json")
			respCh := make(chan *http.Response, 1)
			errCh := make(chan error, 1)
			go func() {
				resp, err := client.Do(req)
				if err != nil {
					errCh <- err
					return
				}
				respCh <- resp
			}()
			select {
			case <-started[i]:
			case err := <-errCh:
				t.Fatal(err)
			case <-time.After(5 * time.Second):
				t.Fatal("upstream did not emit early delta")
			}
			var resp *http.Response
			select {
			case resp = <-respCh:
			case err := <-errCh:
				t.Fatal(err)
			case <-time.After(5 * time.Second):
				t.Fatal("gateway did not commit real-socket response before upstream completion")
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			reader := bufio.NewReader(resp.Body)
			gotPrefix := make([]byte, len(codexStreamPrefix))
			if _, err := io.ReadFull(reader, gotPrefix); err != nil || string(gotPrefix) != codexStreamPrefix {
				t.Fatalf("early SSE bytes=%q err=%v", gotPrefix, err)
			}
			select {
			case <-release[i]:
				t.Fatal("upstream barrier unexpectedly released")
			default:
			}
			close(release[i])
			rest, err := io.ReadAll(reader)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if got := string(gotPrefix) + string(rest); got != codexStreamPrefix+tc.terminal {
				t.Fatalf("SSE bytes changed\ngot:  %q\nwant: %q", got, codexStreamPrefix+tc.terminal)
			}
		})
	}
	if got := calls.snapshot(); got != (accountingCallCountsSnapshot{admit: int32(len(cases)), intent: int32(len(cases)), finalize: int32(len(cases))}) {
		t.Fatalf("accounting calls = %+v", got)
	}
	rows, err := sqlite.NewLedger(prepared.runtimeDB).QueryRequests(ctx, sqlite.RequestFilter{Model: model, Limit: 10})
	if err != nil || len(rows) != len(cases) {
		t.Fatalf("ledger requests=%d err=%v", len(rows), err)
	}
	wantLedger := make(map[string]int)
	for _, tc := range cases {
		wantLedger[fmt.Sprintf("%s/%s/%s/%s", durableState(tc.state), tc.completeness, tokenValue(tc.input), tokenValue(tc.output))]++
	}
	gotLedger := make(map[string]int)
	for _, row := range rows {
		if len(row.Attempts) != 1 || row.Request.State != row.Attempts[0].Attempt.State || row.Request.FinishedAt == nil || row.Attempts[0].Attempt.FinishedAt == nil || row.Attempts[0].Usage == nil {
			t.Fatalf("ledger request=%+v attempts=%+v", row.Request, row.Attempts)
		}
		usage := row.Attempts[0].Usage
		reservation := row.Attempts[0].Reservation
		if reservation.State != "settled" && reservation.State != "conservative" {
			t.Errorf("request %s reservation state=%q, want durable settlement", row.Request.ID, reservation.State)
		}
		gotLedger[fmt.Sprintf("%s/%s/%s/%s", row.Request.State, usage.Completeness, tokenValue(usage.InputTokens), tokenValue(usage.OutputTokens))]++
		if row.Request.State == "succeeded" {
			if usage.CachedTokens == nil || *usage.CachedTokens != 2 || usage.ReasoningTokens == nil || *usage.ReasoningTokens != 1 || reservation.State != "settled" || reservation.ActualTokens == nil || *reservation.ActualTokens != 10 || reservation.EffectiveCharge != 10 {
				t.Errorf("complete Codex usage was not durably settled exactly: usage=%+v reservation=%+v", usage, reservation)
			}
		} else if row.Request.State == "interrupted" || row.Request.State == "failed" || row.Request.State == "incomplete" {
			if reservation.EffectiveCharge < reservation.EstimatedTokens {
				t.Errorf("partial/unknown Codex usage undercharged reservation: %+v", reservation)
			}
		}
	}
	for key, count := range wantLedger {
		if gotLedger[key] != count {
			t.Errorf("ledger settlement for %q: got %d records, want %d", key, gotLedger[key], count)
		}
	}
	for key, count := range gotLedger {
		if wantLedger[key] != count {
			t.Errorf("unexpected ledger settlement %q: %d records", key, count)
		}
	}
	if got := upstreamCalls.Load(); got != int32(len(cases)) {
		t.Fatalf("upstream calls=%d want=%d", got, len(cases))
	}
}

func TestCodexSettlementRestartReconcilesIntentOnce(t *testing.T) {
	_, dbPath, _ := protectedFixture(t)
	ctx := context.Background()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const accountID, connectorID, model = "codex-account", "codex", "codex-model"
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: accountID, Connector: connectorID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	policies := sqlite.NewKeyPolicies(db)
	policy, err := policies.GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	policy, err = policies.Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{model}, Connectors: []string{connectorID}, RPM: 20, TPM: 10000})
	if err != nil {
		t.Fatal(err)
	}
	issued, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := sqlite.NewVirtualKeys(db).Verify(ctx, issued.Secret)
	if err != nil {
		t.Fatal(err)
	}
	ledger := sqlite.NewLedger(db)
	requestID, attemptID := "codex-crash-request", "codex-crash-attempt"
	now := time.Now().UTC()
	if err := ledger.Admit(ctx,
		sqlite.RequestRecord{ID: requestID, VirtualKeyID: principal.KeyID, KeyRevision: principal.KeyRevision, PolicyID: principal.PolicyID, PolicyRevision: principal.PolicyRevision, Protocol: responsesProtocol, Model: model, RouteID: "codex-route", AcceptedAt: now, State: "admitted"},
		sqlite.AttemptRecord{ID: attemptID, RequestID: requestID, Ordinal: 1, AccountID: accountID, Connector: connectorID, RouteID: "codex-route", BudgetPolicy: "reserve", EstimateTokens: 100, EstimateMethod: "conservative", State: "reserved"},
		sqlite.ReservationRecord{AttemptID: attemptID, EstimatedTokens: 100}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.RecordDispatchIntent(ctx, attemptID, now.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	recovered := sqlite.NewLedger(db)
	first, err := recovered.Recover(ctx)
	if err != nil || first.AttemptsInterrupted != 1 || first.RequestsInterrupted != 1 {
		t.Fatalf("Codex crash recovery summary=%+v err=%v", first, err)
	}
	second, err := recovered.Recover(ctx)
	if err != nil || second != first {
		t.Fatalf("repeated Codex recovery=%+v err=%v, first=%+v", second, err, first)
	}
	rows, err := recovered.QueryRequests(ctx, sqlite.RequestFilter{Model: model, Limit: 2})
	if err != nil || len(rows) != 1 || len(rows[0].Attempts) != 1 {
		t.Fatalf("recovered Codex rows=%+v err=%v", rows, err)
	}
	got := rows[0].Attempts[0]
	if rows[0].Request.State != "interrupted" || got.Attempt.State != "interrupted" || got.Usage == nil || got.Usage.Completeness != "unknown" || got.Reservation.State != "conservative" || got.Reservation.EffectiveCharge != 100 {
		t.Fatalf("Codex recovery fabricated free work or lost conservative charge: request=%+v attempt=%+v usage=%+v reservation=%+v", rows[0].Request, got.Attempt, got.Usage, got.Reservation)
	}
}

const codexStreamPrefix = "event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"early\"}\n\n"

func tokenValue(v *int64) string {
	if v == nil {
		return "unknown"
	}
	return fmt.Sprint(*v)
}

func durableState(state string) string {
	if state == "incomplete" {
		return "interrupted"
	}
	return state
}
