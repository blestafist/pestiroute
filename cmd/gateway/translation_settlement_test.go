package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	responsesadapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/connector/anthropic"
	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestTranslationSettlementPersistsProviderUsage(t *testing.T) {
	for _, tc := range []struct {
		name, blockType, blockStart, blockDelta, stop string
		wantOutcome, wantState, completeness          string
	}{
		{
			name: "text success", blockType: "text",
			blockStart: `"type":"text"`, blockDelta: `"type":"text_delta","text":"done"`, stop: "end_turn",
			wantOutcome: "response.completed", wantState: "succeeded", completeness: "complete",
		},
		{
			name: "tool response", blockType: "tool_use",
			blockStart: `"type":"tool_use","id":"call-1","name":"lookup"`, blockDelta: `"type":"input_json_delta","partial_json":"{\"q\":\"x\"}"`, stop: "tool_use",
			wantOutcome: "response.completed", wantState: "succeeded", completeness: "complete",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"cache_read_input_tokens\":2,\"cache_creation_input_tokens\":3,\"reasoning_tokens\":99}}}\n\n")
				_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{"+tc.blockStart+"}}\n\n")
				_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{"+tc.blockDelta+"}}\n\n")
				_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
				_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\""+tc.stop+"\"},\"usage\":{\"output_tokens\":4}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer backend.Close()
			f := newTranslationLifecycleFixture(t, backend.Config.Handler)
			req, err := f.request(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if tc.blockType == "tool_use" {
				req.Body = io.NopCloser(strings.NewReader(`{"model":"client-model","stream":true,"input":"hello","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`))
				req.ContentLength = -1
			}
			resp, err := f.gateway.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(body), tc.wantOutcome) {
				t.Fatalf("translated response status=%d err=%v body=%s", resp.StatusCode, err, body)
			}
			rows, err := sqlite.NewLedger(f.prepared.runtimeDB).QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 2})
			if err != nil || len(rows) != 1 || len(rows[0].Attempts) != 1 {
				t.Fatalf("translated ledger rows=%+v err=%v", rows, err)
			}
			row := rows[0].Attempts[0]
			if row.Attempt.State != tc.wantState || row.Usage == nil || row.Usage.InputTokens == nil || *row.Usage.InputTokens != 10 || row.Usage.OutputTokens == nil || *row.Usage.OutputTokens != 4 || row.Usage.ReasoningTokens != nil || row.Usage.CachedTokens == nil || *row.Usage.CachedTokens != 2 || row.Usage.Completeness != tc.completeness || row.Reservation.State != "settled" || row.Reservation.ActualTokens == nil || *row.Reservation.ActualTokens != 14 || row.Reservation.EffectiveCharge != 14 {
				t.Fatalf("translated durable settlement=%+v", row)
			}
		})
	}
}

func TestTranslationPartialUsageSettlesConservativelyAfterReopen(t *testing.T) {
	// The fake knows input_tokens, but omits both cache counters required to
	// normalize the inclusive Responses input total; the total must remain NULL.
	const partial = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":6}}}\n\n"
	doer := anthropicDoerFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(partial)), Request: r}, nil
	})
	f := newTranslationLifecycleFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), doer)
	req, err := f.request(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.gateway.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "response.incomplete") {
		t.Fatalf("truncated response status=%d err=%v body=%s", resp.StatusCode, err, body)
	}
	if got := f.accounting.snapshot(); got.finalize != 1 {
		t.Fatalf("partial translation finalized %d times", got.finalize)
	}
	result := <-f.finalized
	if result.Usage.InputTokens != nil || result.Usage.OutputTokens != nil || result.Usage.Source != "unknown" || result.Usage.Completeness != "partial" {
		t.Fatalf("omitted partial counters should remain unknown: outcome=%s usage=%+v", result.Outcome, result.Usage)
	}
	if err := f.prepared.runtimeDB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(f.prepared.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := sqlite.NewLedger(db).QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 2})
	if err != nil || len(rows) != 1 || len(rows[0].Attempts) != 1 {
		t.Fatalf("reopened partial ledger rows=%+v err=%v", rows, err)
	}
	row := rows[0].Attempts[0]
	if row.Attempt.State != "interrupted" || row.Usage == nil || row.Usage.InputTokens != nil || row.Usage.OutputTokens != nil || row.Usage.Source != "unknown" || row.Usage.Completeness != "partial" || row.Reservation.State != "conservative" || row.Reservation.EffectiveCharge < row.Reservation.EstimatedTokens {
		t.Fatalf("reopened truncated settlement state=%s usage=%+v reservation=%+v", row.Attempt.State, row.Usage, row.Reservation)
	}
}

type encodeErrorProbe struct {
	core.ProtocolAdapter
	err chan error
}

func (p *encodeErrorProbe) Encode(ctx context.Context, client core.ClientResponse, gatewayErr *core.GatewayError, response core.ExecutionResponse) error {
	err := p.ProtocolAdapter.Encode(ctx, client, gatewayErr, response)
	select {
	case p.err <- err:
	default:
	}
	return err
}

func TestTranslationPersistenceFailureSuppressesInternalCompleteAndFailsClosed(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	const completed = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	doer := anthropicDoerFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(completed)), Request: r}, nil
	})
	adapterProbe := &encodeErrorProbe{ProtocolAdapter: responsesadapter.NewAdapter(), err: make(chan error, 1)}
	factory := func(item topologyComponent, _ core.HTTPDoer) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapterProbe
		}
		return nil
	}
	f := newTranslationLifecycleFixtureConfiguredWithFactory(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), nil, nil, factory, doer)
	req, err := f.request(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	responseDone := make(chan struct {
		body []byte
		resp *http.Response
		err  error
	}, 1)
	go func() {
		resp, err := f.gateway.Client().Do(req)
		if err != nil {
			responseDone <- struct {
				body []byte
				resp *http.Response
				err  error
			}{err: err}
			return
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		responseDone <- struct {
			body []byte
			resp *http.Response
			err  error
		}{body: body, resp: resp, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("translated request did not reach Connector transport")
	}
	if err := f.prepared.runtimeDB.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	var result struct {
		body []byte
		resp *http.Response
		err  error
	}
	select {
	case result = <-responseDone:
	case <-time.After(3 * time.Second):
		t.Fatal("translated request did not terminate after settlement failure")
	}
	if result.err != nil || result.resp.StatusCode != http.StatusOK || !strings.Contains(string(result.body), "response.completed") {
		t.Fatalf("opaque terminal body status=%v err=%v completed=%t", responseStatus(result.resp), result.err, strings.Contains(string(result.body), "response.completed"))
	}
	select {
	case encodeErr := <-adapterProbe.err:
		var failure *core.GatewayError
		if !errors.As(encodeErr, &failure) || failure.Code != "accounting_unavailable" {
			t.Fatalf("Adapter received internal Complete instead of accounting failure: %v", encodeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Adapter did not observe terminal accounting failure")
	}
	if f.ready.Load() {
		t.Fatal("gateway remained ready after translated persistence failure")
	}
	probe, err := f.gateway.Client().Get(f.gateway.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_ = probe.Body.Close()
	if probe.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz status=%d, want 503", probe.StatusCode)
	}
	db, err := sqlite.Open(f.prepared.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := sqlite.NewLedger(db).QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 2})
	if err != nil || len(rows) != 1 || rows[0].Request.State != "admitted" || len(rows[0].Attempts) != 1 {
		_ = db.Close()
		t.Fatalf("failed settlement must not persist request success: rows=%+v err=%v", rows, err)
	}
	attempt := rows[0].Attempts[0]
	if attempt.Attempt.State != "intent" || attempt.Reservation.State != "held" || attempt.Usage != nil {
		_ = db.Close()
		t.Fatalf("failed settlement fabricated terminal accounting: %+v", attempt)
	}
	_ = db.Close()
	for _, model := range []string{"model-a", "client-model"} {
		body := `{"model":"` + model + `","stream":true,"input":"after failure"}`
		followup, err := http.NewRequest(http.MethodPost, f.gateway.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		followup.Header.Set("Authorization", "Bearer "+f.secret)
		followup.Header.Set("Content-Type", "application/json")
		resp, err := f.gateway.Client().Do(followup)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("post-failure model %s status=%d, want 503", model, resp.StatusCode)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls after persistence failure=%d, want 1", calls.Load())
	}
}

type safeFirstAnthropicConnector struct {
	*anthropic.Connector
	safeReject bool
}

func (c safeFirstAnthropicConnector) Execute(ctx context.Context, in core.ExecutionRequest, scope core.AttemptScope, services core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	if c.safeReject {
		return core.ExecutionResponse{}, &core.GatewayError{Code: "safe_fixture_rejection", Category: core.CategoryUnavailable, Retryable: true, RetryDisposition: core.RetrySafe}
	}
	return c.Connector.Execute(ctx, in, scope, services)
}

func TestTranslationFallbackSettlesEachDurableAttempt(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	doer := anthropicDoerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Api-Key") == "synthetic-anthropic-key-b" {
			secondCalls.Add(1)
		} else {
			firstCalls.Add(1)
		}
		const complete = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(complete)), Request: r}, nil
	})
	factory := func(item topologyComponent, _ core.HTTPDoer) core.Component {
		if item.Kind == core.ComponentConnector && item.ID == "anthropic" {
			return safeFirstAnthropicConnector{Connector: anthropic.NewConnector(), safeReject: true}
		}
		return nil
	}
	f := newTranslationFallbackFixtureWithFactory(t, doer, factory)
	if len(f.prepared.Routes) != 3 || f.prepared.Routes[1].RetryMaxAttempts != 2 || f.prepared.Routes[1].RetryDeadline <= 0 {
		t.Fatalf("fallback route not composed with retry: %+v", f.prepared.Routes)
	}
	req, err := f.request(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.gateway.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "response.completed") || firstCalls.Load() != 0 || secondCalls.Load() != 1 {
		var finalized []core.AttemptResult
		for len(f.finalized) > 0 {
			finalized = append(finalized, <-f.finalized)
		}
		t.Fatalf("fallback response status=%d err=%v first/second upstream calls=%d/%d accounting=%+v finalized=%+v body=%s", resp.StatusCode, err, firstCalls.Load(), secondCalls.Load(), f.accounting.snapshot(), finalized, body)
	}
	rows, err := sqlite.NewLedger(f.prepared.runtimeDB).QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 2})
	if err != nil || len(rows) != 1 || rows[0].Request.State != "succeeded" || len(rows[0].Attempts) != 2 {
		t.Fatalf("fallback ledger rows=%+v err=%v", rows, err)
	}
	first, second := rows[0].Attempts[0], rows[0].Attempts[1]
	if first.Attempt.Ordinal > second.Attempt.Ordinal {
		first, second = second, first
	}
	if rows[0].Request.ID == "" || first.Attempt.State != "failed" || first.Reservation.State != "conservative" || first.Reservation.EffectiveCharge != first.Reservation.EstimatedTokens || second.Attempt.State != "succeeded" || second.Reservation.State != "settled" || second.Reservation.EffectiveCharge != 2 || f.accounting.snapshot() != (accountingCallCountsSnapshot{admit: 1, begin: 1, intent: 2, finalize: 2}) {
		t.Fatalf("fallback settlement states=%s/%s usage=%+v reservations=%s:%d/%s:%d estimates=%d/%d calls=%+v", first.Attempt.State, second.Attempt.State, second.Usage, first.Reservation.State, first.Reservation.EffectiveCharge, second.Reservation.State, second.Reservation.EffectiveCharge, first.Reservation.EstimatedTokens, second.Reservation.EstimatedTokens, f.accounting.snapshot())
	}
	var requestCount, attemptCount int
	if err := f.prepared.runtimeDB.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&requestCount); err != nil {
		t.Fatal(err)
	}
	if err := f.prepared.runtimeDB.QueryRow(`SELECT COUNT(*) FROM attempts`).Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if requestCount != 1 || attemptCount != 2 {
		t.Fatalf("fallback RPM/request accounting requests=%d attempts=%d", requestCount, attemptCount)
	}
}

func TestTranslationRestartRecoverySettlesUncommittedIntent(t *testing.T) {
	f := newTranslationLifecycleFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ctx := context.Background()
	principal, err := sqlite.NewVirtualKeys(f.prepared.runtimeDB).Verify(ctx, f.secret)
	if err != nil {
		t.Fatal(err)
	}
	ledger := sqlite.NewLedger(f.prepared.runtimeDB)
	requestID, attemptID := "translation-crash-request", "translation-crash-attempt"
	accepted := time.Now()
	attempt := sqlite.AttemptRecord{
		ID: attemptID, RequestID: requestID, Ordinal: 1, AccountID: "anthropic-account",
		Connector: "anthropic", RouteID: "translation-route", BudgetPolicy: "reserve",
		EstimateTokens: 4096, EstimateMethod: "conservative", State: "reserved",
	}
	if err := ledger.Admit(ctx,
		sqlite.RequestRecord{ID: requestID, VirtualKeyID: principal.KeyID, KeyRevision: principal.KeyRevision, PolicyID: principal.PolicyID, PolicyRevision: principal.PolicyRevision, Protocol: responsesProtocol, Model: "client-model", RouteID: "translation-route", AcceptedAt: accepted, State: "admitted"},
		attempt, sqlite.ReservationRecord{AttemptID: attemptID, EstimatedTokens: 4096}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.RecordDispatchIntent(ctx, attemptID, accepted.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := f.prepared.runtimeDB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(f.prepared.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	recovered := sqlite.NewLedger(db)
	summary, err := recovered.Recover(ctx)
	if err != nil || summary.AttemptsInterrupted != 1 || summary.RequestsInterrupted != 1 || summary.AttemptsReserved != 0 || summary.AttemptsIntent != 0 {
		t.Fatalf("translation recovery summary=%+v err=%v", summary, err)
	}
	request, reqErr := recovered.GetRequest(ctx, requestID)
	row, rowErr := recovered.QueryRequests(ctx, sqlite.RequestFilter{Limit: 2})
	if reqErr != nil || rowErr != nil || len(row) != 1 || len(row[0].Attempts) != 1 {
		t.Fatalf("recovered request=%+v rows=%+v errors=%v/%v", request, row, reqErr, rowErr)
	}
	a := row[0].Attempts[0]
	if request.State != "interrupted" || a.Attempt.State != "interrupted" || a.Usage == nil || a.Usage.InputTokens != nil || a.Usage.OutputTokens != nil || a.Usage.Source != "unknown" || a.Reservation.State != "conservative" || a.Reservation.EffectiveCharge != 4096 {
		t.Fatalf("recovered translated rows request=%+v attempt=%+v usage=%+v reservation=%+v", request, a.Attempt, a.Usage, a.Reservation)
	}
	second, err := recovered.Recover(ctx)
	if err != nil || second != summary {
		t.Fatalf("repeated translation recovery summary=%+v err=%v want=%+v", second, err, summary)
	}
}

func responseStatus(r *http.Response) any {
	if r == nil {
		return "no response"
	}
	return r.StatusCode
}
