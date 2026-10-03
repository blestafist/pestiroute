package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

type durableCancelConnector struct {
	*accountingRaceConnector
	entered   chan struct{}
	cancelled chan struct{}
}

func (c *durableCancelConnector) Execute(ctx context.Context, _ core.ExecutionRequest, _ core.AttemptScope, _ core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	close(c.entered)
	<-ctx.Done()
	close(c.cancelled)
	return core.ExecutionResponse{}, &core.GatewayError{Code: "execution_cancelled", Category: core.CategoryCancelled}
}

func TestDurablePreHeadCancelAfterIntentSettlesConservatively(t *testing.T) {
	fixture := newDurableAccountingFixture(t, &durableCancelConnector{
		accountingRaceConnector: &accountingRaceConnector{descriptor: accountingRaceConnectorDescriptor("connector"), stream: newAccountingRaceStream()},
		entered:                 make(chan struct{}),
		cancelled:               make(chan struct{}),
	})
	done := make(chan *core.GatewayError, 1)
	go func() {
		_, gatewayErr := fixture.dispatcher.Execute(fixture.ctx, fixture.request)
		done <- gatewayErr
	}()
	<-fixture.connector.(*durableCancelConnector).entered
	fixture.cancel()
	select {
	case <-fixture.connector.(*durableCancelConnector).cancelled:
	case <-time.After(time.Second):
		t.Fatal("connector did not observe pre-Head cancellation")
	}
	if err := <-done; err == nil || err.Category != core.CategoryCancelled {
		t.Fatalf("cancelled Execute error = %+v", err)
	}
	assertDurableLedger(t, fixture, "cancelled", "conservative", 5, true)
}

func TestDurableComposedPreHeadCancelWritesNoStreamFrame(t *testing.T) {
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	_, dbPath, keyPath := protectedFixture(t)
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := sqlite.NewKeyPolicies(db).GetLatest(context.Background(), "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	issued, err := sqlite.NewVirtualKeys(db).Create(context.Background(), sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	upstreamStarted, releaseUpstream, upstreamDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseUpstream) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		close(upstreamStarted)
		select {
		case <-r.Context().Done():
		case <-releaseUpstream:
		}
	}))
	defer func() { release(); upstream.Close() }()
	address := freeTCPAddress(t)
	c := fixtureProtectedConfig(dbPath, keyPath, address)
	c.protected.Connectors[0].Settings.BaseURL = upstream.URL + "/v1"
	prepared, err := prepareProtectedConfig(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	calls := &accountingCallCounters{}
	prepared.accounting = &countedSQLiteAccountingStore{store: sqliteAccountingStore{ledger: sqlite.NewLedger(prepared.runtimeDB)}, calls: calls}
	var ready, draining atomic.Bool
	ready.Store(true)
	finalized := make(chan core.AttemptResult, 1)
	handler, closeComponents, err := composeHandler(prepared, &ready, &draining, func(result core.AttemptResult) { finalized <- result })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = closeComponents(ctx)
		_ = prepared.runtimeDB.Close()
		_ = prepared.processLock.Close()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://gateway.test/v1/responses", strings.NewReader(`{"model":"model-a","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+issued.Secret)
	request.Header.Set("Content-Type", "application/json")
	writer := &captureResponseWriter{header: make(http.Header)}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		handler.ServeHTTP(writer, request)
	}()
	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("pre-Head request did not reach upstream")
	}
	cancel()
	select {
	case <-serveDone:
	case <-time.After(2 * time.Second):
		t.Fatal("pre-Head request handler did not exit after cancellation")
	}
	release()
	select {
	case <-upstreamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("controlled pre-Head upstream handler did not exit")
	}
	select {
	case <-finalized:
	case <-time.After(2 * time.Second):
		t.Fatal("pre-Head attempt was not finalized")
	}
	if writer.flushes != 0 || strings.Contains(writer.body.String(), "event:") {
		t.Fatalf("pre-Head cancellation wrote stream frames: flushes=%d body=%q", writer.flushes, writer.body.String())
	}
	if got := calls.snapshot(); got != (accountingCallCountsSnapshot{admit: 1, intent: 1, finalize: 1}) {
		t.Fatalf("pre-Head accounting calls = %+v", got)
	}
	requests, err := sqlite.NewLedger(prepared.runtimeDB).QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 2})
	if err != nil || len(requests) != 1 || len(requests[0].Attempts) != 1 {
		t.Fatalf("pre-Head durable rows = %+v, %v", requests, err)
	}
	row := requests[0].Attempts[0]
	if row.Attempt.State != "cancelled" || row.Reservation.State != "conservative" || row.Reservation.EffectiveCharge != 10 || row.Usage == nil || !unknownUsage(row.Usage) {
		t.Fatalf("pre-Head settlement = %+v", row)
	}
}

func TestDurablePostHeadCancelAndStreamViolation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bad   bool
		close bool
	}{{name: "cancel"}, {name: "caller-close", close: true}, {name: "violation", bad: true}} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newDurableAccountingFixture(t, nil)
			response, gatewayErr := fixture.dispatcher.Execute(fixture.ctx, fixture.request)
			if gatewayErr != nil {
				t.Fatal(gatewayErr)
			}
			frame, err := response.Stream.Next(fixture.ctx)
			if err != nil || frame.Type != core.FrameHead {
				t.Fatalf("Head = %+v, %v", frame, err)
			}
			if fixture.stream.pulls.Load() != 1 || fixture.stream.active.Load() != 0 || fixture.stream.maxActive.Load() != 1 {
				t.Fatalf("source pulled without downstream demand after Head: total=%d active=%d max=%d", fixture.stream.pulls.Load(), fixture.stream.active.Load(), fixture.stream.maxActive.Load())
			}
			if got := fixture.accountingCalls.snapshot(); got != (accountingCallCountsSnapshot{admit: 1, intent: 1}) {
				t.Fatalf("accounting calls at Head = %+v", got)
			}
			if tc.bad {
				producerDone := make(chan struct{})
				go func() {
					defer close(producerDone)
					select {
					case fixture.stream.events <- accountingRaceEvent{frame: core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded}}}:
					case <-fixture.stream.closed:
						return
					}
					select {
					case fixture.stream.events <- accountingRaceEvent{frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: []byte("trailing")}}}:
					case <-fixture.stream.closed:
					}
				}()
				if _, err := response.Stream.Next(fixture.ctx); !errors.Is(err, core.ErrStreamContract) {
					t.Fatalf("stream violation error = %v", err)
				}
				assertProducerClosed(t, fixture.stream)
				select {
				case <-producerDone:
				case <-time.After(time.Second):
					t.Fatal("violation producer did not exit after teardown")
				}
				assertDurableLedger(t, fixture, "failed", "conservative", 5, true)
				requests, err := sqlite.NewLedger(mustOpenSQLite(t, fixture.dbPath)).QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 1})
				if err != nil || len(requests) != 1 || requests[0].Attempts[0].Attempt.ErrorCategory == nil || string(*requests[0].Attempts[0].Attempt.ErrorCategory) != string(core.CategoryInternal) {
					t.Fatalf("stream violation durable category = %+v, %v", requests, err)
				}
				if got := fixture.accountingCalls.snapshot(); got != (accountingCallCountsSnapshot{admit: 1, intent: 1, finalize: 1}) {
					t.Fatalf("stream violation accounting calls = %+v", got)
				}
				return
			}
			blocked := make(chan error, 1)
			go func() { _, err := response.Stream.Next(fixture.ctx); blocked <- err }()
			if tc.close {
				_ = response.Stream.Close()
			} else {
				fixture.cancel()
			}
			select {
			case err := <-blocked:
				if err == nil || (!tc.close && !errors.Is(err, context.Canceled)) {
					t.Fatalf("blocked Next error = %v", err)
				}
				if fixture.stream.active.Load() != 0 {
					t.Fatalf("source reader remained active after teardown: %d", fixture.stream.active.Load())
				}
			case <-time.After(time.Second):
				t.Fatal("blocked Next did not unblock after cancellation")
			}
			_ = response.Stream.Close()
			assertProducerClosed(t, fixture.stream)
			assertDurableLedger(t, fixture, "cancelled", "conservative", 5, true)
		})
	}
}

func assertProducerClosed(t *testing.T, stream *accountingRaceStream) {
	t.Helper()
	select {
	case <-stream.closed:
	case <-time.After(time.Second):
		t.Fatal("upstream producer was not closed")
	}
}

func TestDurableStreamFailureUnderSQLiteContention(t *testing.T) {
	fixture := newDurableAccountingFixture(t, nil)
	response, gatewayErr := fixture.dispatcher.Execute(fixture.ctx, fixture.request)
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	if _, err := response.Stream.Next(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if fixture.stream.pulls.Load() != 1 || fixture.stream.active.Load() != 0 || fixture.stream.maxActive.Load() != 1 {
		t.Fatalf("source pulls while downstream idle after Head: total=%d active=%d max=%d", fixture.stream.pulls.Load(), fixture.stream.active.Load(), fixture.stream.maxActive.Load())
	}
	if got := fixture.accountingCalls.snapshot(); got != (accountingCallCountsSnapshot{admit: 1, intent: 1}) {
		t.Fatalf("accounting calls after admission/intent = %+v", got)
	}
	lockDB := mustOpenSQLite(t, fixture.dbPath)
	conn, err := lockDB.Conn(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(fixture.ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK"); _ = conn.Close() }()
	sent := make(chan int, 3)
	attemptingSend := make(chan int, 3)
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for i := range 3 {
			attemptingSend <- i
			select {
			case fixture.stream.events <- accountingRaceEvent{frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: []byte{byte('a' + i)}}}}:
			case <-fixture.stream.closed:
				return
			}
			sent <- i + 1
		}
		select {
		case fixture.stream.events <- accountingRaceEvent{frame: core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded}}}:
		case <-fixture.stream.closed:
			return
		}
		select {
		case fixture.stream.events <- accountingRaceEvent{err: io.EOF}:
		case <-fixture.stream.closed:
		}
	}()
	<-attemptingSend
	for i := range 3 {
		pullBefore := fixture.stream.pulls.Load()
		frame, err := response.Stream.Next(fixture.ctx)
		if err != nil || frame.Type != core.FrameBody || len(frame.Body.Data) != 1 || frame.Body.Data[0] != byte('a'+i) {
			t.Fatalf("body %d under SQLite writer lock = %+v, %v", i, frame, err)
		}
		if got := fixture.stream.pulls.Load(); got != pullBefore+1 || fixture.stream.active.Load() != 0 || fixture.stream.maxActive.Load() != 1 {
			t.Fatalf("Body %d source pulls before=%d after=%d active=%d max=%d", i, pullBefore, got, fixture.stream.active.Load(), fixture.stream.maxActive.Load())
		}
		if got := fixture.accountingCalls.snapshot(); got != (accountingCallCountsSnapshot{admit: 1, intent: 1}) {
			t.Fatalf("accounting store called for Body %d: %+v", i, got)
		}
		<-sent
		if i < 2 {
			<-attemptingSend
			if got := fixture.stream.pulls.Load(); got != pullBefore+1 {
				t.Fatalf("upstream had speculative pull while client idle after Body %d: pulls=%d", i, got)
			}
			select {
			case n := <-sent:
				t.Fatalf("producer delivered event %d without a downstream pull", n)
			default:
			}
		}
	}
	if _, err := response.Stream.Next(fixture.ctx); err == nil {
		t.Fatal("terminal persistence contention was acknowledged as success")
	}
	assertProducerClosed(t, fixture.stream)
	if fixture.stream.active.Load() != 0 {
		t.Fatalf("source reader remained active after terminal storage failure: %d", fixture.stream.active.Load())
	}
	<-producerDone
	if got := fixture.accountingCalls.snapshot(); got != (accountingCallCountsSnapshot{admit: 1, intent: 1, finalize: 1}) {
		t.Fatalf("accounting calls after failed terminal write = %+v", got)
	}
	ledger := sqlite.NewLedger(fixture.db)
	requests, err := ledger.QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 10})
	if err != nil || len(requests) != 1 || len(requests[0].Attempts) != 1 {
		t.Fatalf("contended ledger rows = %+v, %v", requests, err)
	}
	row := requests[0].Attempts[0]
	if row.Attempt.State != "intent" || row.Reservation.State != "held" || row.Usage != nil {
		t.Fatalf("failed settlement falsely changed durable state: %+v", row)
	}
	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lockDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(fixture.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovery := sqlite.NewLedger(reopened)
	first, err := recovery.Recover(context.Background())
	if err != nil || first.AttemptsInterrupted != 1 || first.RequestsInterrupted != 1 || first.AttemptsIntent != 0 {
		t.Fatalf("recover failed terminal write = %+v, %v", first, err)
	}
	assertRecoveredContendedAttempt(t, reopened, recovery, requests[0].Request.ID, row.Attempt.ID, row.Reservation.EstimatedTokens)
	second, err := recovery.Recover(context.Background())
	if err != nil || second != first {
		t.Fatalf("second recovery summary = %+v, %v, want %+v", second, err, first)
	}
	assertRecoveredContendedAttempt(t, reopened, recovery, requests[0].Request.ID, row.Attempt.ID, row.Reservation.EstimatedTokens)
}

func TestDurableStreamFailureComposedAdapterWriteFailure(t *testing.T) {
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	_, dbPath, keyPath := protectedFixture(t)
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := sqlite.NewKeyPolicies(db).GetLatest(context.Background(), "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	issued, err := sqlite.NewVirtualKeys(db).Create(context.Background(), sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	upstreamStarted, upstreamCanceled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		flusher.Flush()
		close(upstreamStarted)
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()
	address := freeTCPAddress(t)
	c := fixtureProtectedConfig(dbPath, keyPath, address)
	c.protected.Connectors[0].Settings.BaseURL = upstream.URL + "/v1"
	c.protected.Connectors[0].Settings.ResponseHeaderTimeout = "2s"
	c.protected.Connectors[0].Settings.StreamIdleTimeout = "10s"
	prepared, err := prepareProtectedConfig(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	accountingCalls := &accountingCallCounters{}
	prepared.accounting = &countedSQLiteAccountingStore{store: sqliteAccountingStore{ledger: sqlite.NewLedger(prepared.runtimeDB)}, calls: accountingCalls}
	var ready, draining atomic.Bool
	ready.Store(true)
	finalized := make(chan core.AttemptResult, 1)
	handler, closeComponents, err := composeHandler(prepared, &ready, &draining, func(result core.AttemptResult) { finalized <- result })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = closeComponents(ctx)
		_ = prepared.runtimeDB.Close()
		_ = prepared.processLock.Close()
	})
	request, err := http.NewRequest(http.MethodPost, "http://gateway.test/v1/responses", strings.NewReader(`{"model":"model-a","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+issued.Secret)
	request.Header.Set("Content-Type", "application/json")
	var callsAtBody accountingCallCountsSnapshot
	writer := &failingResponseWriter{header: make(http.Header), beforeWrite: func() { callsAtBody = accountingCalls.snapshot() }}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		handler.ServeHTTP(writer, request)
	}()
	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("composed request did not reach upstream")
	}
	select {
	case <-serveDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Adapter handler did not return after downstream write failure")
	}
	if writer.writeCalls != 1 || writer.writeErr == nil || writer.statusCode != http.StatusOK || writer.headerCalls != 1 || callsAtBody != (accountingCallCountsSnapshot{admit: 1, intent: 1}) || accountingCalls.snapshot() != (accountingCallCountsSnapshot{admit: 1, intent: 1, finalize: 1}) {
		t.Fatalf("Adapter did not observe injected downstream write failure: %+v", writer)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("downstream disconnect did not cancel upstream")
	}
	select {
	case <-finalized:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnected composed attempt did not finalize")
	}
	requests, err := sqlite.NewLedger(prepared.runtimeDB).QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 10})
	if err != nil || len(requests) != 1 || len(requests[0].Attempts) != 1 {
		t.Fatalf("composed durable rows = %+v, %v", requests, err)
	}
	row := requests[0].Attempts[0]
	if row.Attempt.State != "cancelled" && row.Attempt.State != "interrupted" || row.Reservation.State != "conservative" || row.Reservation.EffectiveCharge != 10 || row.Usage == nil || !unknownUsage(row.Usage) {
		t.Fatalf("composed disconnect settlement = %+v", row)
	}
}

type failingResponseWriter struct {
	header      http.Header
	writeCalls  int
	writeErr    error
	statusCode  int
	headerCalls int
	beforeWrite func()
}

func (w *failingResponseWriter) Header() http.Header { return w.header }
func (w *failingResponseWriter) WriteHeader(code int) {
	w.headerCalls++
	w.statusCode = code
}
func (w *failingResponseWriter) Write([]byte) (int, error) {
	w.writeCalls++
	if w.beforeWrite != nil {
		w.beforeWrite()
	}
	w.writeErr = errors.New("controlled downstream write failure")
	return 0, w.writeErr
}
func (*failingResponseWriter) Flush() {}

type captureResponseWriter struct {
	header  http.Header
	body    strings.Builder
	status  int
	flushes int
}

func (w *captureResponseWriter) Header() http.Header            { return w.header }
func (w *captureResponseWriter) WriteHeader(code int)           { w.status = code }
func (w *captureResponseWriter) Write(data []byte) (int, error) { return w.body.Write(data) }
func (w *captureResponseWriter) Flush()                         { w.flushes++ }

func unknownUsage(usage *sqlite.UsageRecord) bool {
	return usage.InputTokens == nil && usage.OutputTokens == nil && usage.ReasoningTokens == nil && usage.CachedTokens == nil
}

func assertRecoveredContendedAttempt(t *testing.T, db *sql.DB, ledger *sqlite.Ledger, requestID, attemptID string, estimate int64) {
	t.Helper()
	request, requestErr := ledger.GetRequest(context.Background(), requestID)
	attempt, attemptErr := ledger.GetAttempt(context.Background(), attemptID)
	usage, usageErr := ledger.GetUsage(context.Background(), attemptID)
	reservation, reservationErr := ledger.GetReservation(context.Background(), attemptID)
	if requestErr != nil || attemptErr != nil || usageErr != nil || reservationErr != nil || request.State != "interrupted" || attempt.State != "interrupted" || attempt.ErrorReason == nil || *attempt.ErrorReason != "interrupted" || !unknownUsage(&usage) || reservation.State != "conservative" || reservation.EffectiveCharge != estimate {
		t.Fatalf("recovered contended attempt = request:%+v attempt:%+v usage:%+v reservation:%+v errors:%v/%v/%v/%v", request, attempt, usage, reservation, requestErr, attemptErr, usageErr, reservationErr)
	}
	var usages, reservations int
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_records WHERE attempt_id=?`, attemptID).Scan(&usages); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM reservations WHERE attempt_id=?`, attemptID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if usages != 1 || reservations != 1 {
		t.Fatalf("recovery duplicated accounting rows: usage=%d reservations=%d", usages, reservations)
	}
}

type durableAccountingFixture struct {
	ctx             context.Context
	cancel          context.CancelFunc
	dbPath          string
	db              *sql.DB
	connector       core.Connector
	stream          *accountingRaceStream
	dispatcher      *core.Dispatcher
	request         core.ExecutionRequest
	accountingCalls *accountingCallCounters
}

type accountingCallCounters struct {
	admit, begin, intent, finalize atomic.Int32
}

type accountingCallCountsSnapshot struct {
	admit, begin, intent, finalize int32
}

func (c *accountingCallCounters) snapshot() accountingCallCountsSnapshot {
	return accountingCallCountsSnapshot{admit: c.admit.Load(), begin: c.begin.Load(), intent: c.intent.Load(), finalize: c.finalize.Load()}
}

type countedSQLiteAccountingStore struct {
	store sqliteAccountingStore
	calls *accountingCallCounters
}

func (s *countedSQLiteAccountingStore) Admit(ctx context.Context, in core.AccountingAdmission) error {
	s.calls.admit.Add(1)
	return s.store.Admit(ctx, in)
}
func (s *countedSQLiteAccountingStore) BeginAttempt(ctx context.Context, in core.AccountingAdmission) error {
	s.calls.begin.Add(1)
	return s.store.BeginAttempt(ctx, in)
}
func (s *countedSQLiteAccountingStore) RecordDispatchIntent(ctx context.Context, id string, at time.Time) error {
	s.calls.intent.Add(1)
	return s.store.RecordDispatchIntent(ctx, id, at)
}
func (s *countedSQLiteAccountingStore) FinalizeAttempt(ctx context.Context, in core.AccountingTerminal) error {
	s.calls.finalize.Add(1)
	return s.store.FinalizeAttempt(ctx, in)
}

func newDurableAccountingFixture(t *testing.T, connector core.Connector) *durableAccountingFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	dbPath := filepath.Join(t.TempDir(), "durable-cancel.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	policies := sqlite.NewKeyPolicies(db)
	policy, err := policies.Create(ctx, sqlite.CreateKeyPolicyParams{ID: "policy", Models: []string{"model"}, Connectors: []string{"connector"}, RPM: 5, TPM: 100})
	if err != nil {
		t.Fatal(err)
	}
	key, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	accounts := sqlite.NewAccounts(db)
	if _, err := accounts.Create(ctx, sqlite.Account{ID: "account", Connector: "connector", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	stream := newAccountingRaceStream()
	if connector == nil {
		connector = &accountingRaceConnector{descriptor: accountingRaceConnectorDescriptor("connector"), stream: stream}
	}
	registry, err := core.NewRegistry(map[core.ComponentKind]core.APIVersion{core.ComponentAdapter: {Major: 1}, core.ComponentConnector: {Major: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &accountingRaceAdapter{descriptor: core.Descriptor{ID: "adapter", Kind: core.ComponentAdapter, ImplementationVersion: "test", APIVersions: []core.APIVersion{{Major: 1}}, Protocols: []string{accountingRaceProtocol}, Operations: []string{"decode", "encode"}}}
	for _, component := range []core.Component{adapter, connector} {
		id := core.InstanceID(component.Descriptor().ID)
		if err := registry.Register(id, component, component.Descriptor().Kind); err != nil {
			t.Fatal(err)
		}
		if err := registry.Init(ctx, id, core.ComponentConfig{}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { cancel(); _ = registry.Close(context.Background()); _ = db.Close() })
	route := core.Route{Identity: core.RouteIdentity{RouteLookupKey: core.RouteLookupKey{Protocol: accountingRaceProtocol, Mode: core.ModeNative, Model: "model"}, AccountID: "account"}, Adapter: "adapter", Connector: "connector", MaxBodyBytes: 1024, MaxHeaderBytes: 1024}
	table, err := core.NewRouteTable([]core.Route{route}, registry)
	if err != nil {
		t.Fatal(err)
	}
	principal := core.TrustedPrincipal{KeyID: key.ID, PolicyID: policy.ID, KeyRevision: 1, PolicyRevision: policy.Revision}
	accountingCalls := &accountingCallCounters{}
	dispatcher := &core.Dispatcher{Routes: table, Services: raceServices{}, Policies: sqlitePolicyStore{policies: policies}, Accounts: sqliteAccountAuthorizer{accounts: accounts}, Accounting: &countedSQLiteAccountingStore{store: sqliteAccountingStore{ledger: sqlite.NewLedger(db)}, calls: accountingCalls}, Budget: core.RouteBudget{UnknownEstimate: core.UnknownEstimateReject}, BudgetPolicy: "known", RouteID: "route", AccountID: "account"}
	return &durableAccountingFixture{ctx: core.WithTrustedPrincipal(ctx, principal), cancel: cancel, dbPath: dbPath, db: db, connector: connector, stream: stream, dispatcher: dispatcher, request: core.ExecutionRequest{Model: "model", Payload: core.RawPayload{Protocol: accountingRaceProtocol, Body: []byte("opaque")}, Metadata: core.RequestMetadata{AffinityKnown: true, IngressHeaderBytes: 1}}, accountingCalls: accountingCalls}
}

func assertDurableLedger(t *testing.T, fixture *durableAccountingFixture, state, reservation string, charge int64, usage bool) {
	t.Helper()
	requests, err := sqlite.NewLedger(mustOpenSQLite(t, fixture.dbPath)).QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 10})
	if err != nil || len(requests) != 1 || len(requests[0].Attempts) != 1 {
		t.Fatalf("durable request/attempt = %+v, %v", requests, err)
	}
	row := requests[0].Attempts[0]
	if row.Attempt.State != state || row.Reservation.State != reservation || row.Reservation.EffectiveCharge != charge || (row.Usage != nil) != usage {
		t.Fatalf("durable terminal mismatch: request=%+v attempt=%+v reservation=%+v usage=%+v", requests[0].Request, row.Attempt, row.Reservation, row.Usage)
	}
	if usage && !unknownUsage(row.Usage) {
		t.Fatalf("unknown usage was not NULL: %+v", row.Usage)
	}
}

func mustOpenSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
