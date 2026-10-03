package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/connector/anthropic"
	responses "github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

type translationLifecycleFixture struct {
	gateway    *httptest.Server
	prepared   config
	secret     string
	finalized  chan core.AttemptResult
	accounting *accountingCallCounters
}

func newTranslationLifecycleFixture(t *testing.T, backend http.Handler, injected ...core.HTTPDoer) *translationLifecycleFixture {
	t.Helper()
	_, dbPath, keyPath := protectedFixture(t)
	upstream := httptest.NewServer(backend)
	t.Cleanup(upstream.Close)
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	account := sqlite.Account{ID: "anthropic-account", Connector: "anthropic", Enabled: true}
	if _, err := sqlite.NewAccounts(db).Create(ctx, account); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := secure.Seal(key, 2, "test-v1", "credentials", "anthro-key", account.ID, []byte("synthetic-anthropic-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "anthro-key", AccountID: account.ID, FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	policies := sqlite.NewKeyPolicies(db)
	policy, err := policies.GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	policy, err = policies.Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{"model-a", "client-model"}, Connectors: []string{"upstream", "anthropic"}, RPM: 20, TPM: 100000})
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
	backendURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0").protected
	p.Connectors[0].Settings.BaseURL = upstream.URL + "/v1"
	p.Connectors = append(p.Connectors, protectedConnector{ID: "anthropic", Kind: "connector", Implementation: "pestiroute.anthropic.messages", Settings: nativeSettings{Model: "client-model", AccountID: account.ID, CredentialID: "anthro-key"}})
	p.Routes = append(p.Routes, protectedRoute{ID: "translation-route", Protocol: responsesProtocol, Mode: "translation", Model: "client-model", Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(4096)}, Targets: []routeTarget{{Connector: "anthropic", Account: account.ID}}})
	prepared, err := prepareProtectedConfig(ctx, config{protected: p, DatabasePath: dbPath, MasterKeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	accounting := &accountingCallCounters{}
	prepared.accounting = &countedSQLiteAccountingStore{store: sqliteAccountingStore{ledger: sqlite.NewLedger(prepared.runtimeDB)}, calls: accounting}
	var ready, draining atomic.Bool
	ready.Store(true)
	finalized := make(chan core.AttemptResult, 2)
	var doer core.HTTPDoer = anthropicDoerFunc(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		copy.URL = backendURL.ResolveReference(&url.URL{Path: "/v1/messages"})
		copy.Host = copy.URL.Host
		return upstream.Client().Do(copy)
	})
	if len(injected) > 0 {
		doer = injected[0]
	}
	h, closeComponents, err := composeHandlerWithFactory(prepared, &ready, &draining, func(result core.AttemptResult) { finalized <- result }, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		if item.Implementation == "pestiroute.anthropic.messages" {
			return anthropicConnectorWithDoer{Connector: anthropic.NewConnector(), doer: doer}
		}
		return responses.NewConnector()
	})
	if err != nil {
		t.Fatal(err)
	}
	var gateway *httptest.Server
	t.Cleanup(func() {
		if gateway != nil {
			gateway.Close()
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = closeComponents(closeCtx)
		_ = prepared.runtimeDB.Close()
		_ = prepared.processLock.Close()
	})
	gateway = httptest.NewServer(h)
	return &translationLifecycleFixture{gateway: gateway, prepared: prepared, secret: issued.Secret, finalized: finalized, accounting: accounting}
}

func TestTranslationPreHeadCancellationDurableRows(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	doer := anthropicDoerFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		close(cancelled)
		return nil, req.Context().Err()
	})
	f := newTranslationLifecycleFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), doer)
	ctx, cancel := context.WithCancel(context.Background())
	req, err := f.request(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.gateway.Client().Do(req); done <- err }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("translated request did not reach the controlled upstream")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("pre-Head cancellation did not reach the controlled upstream")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("pre-Head cancellation returned an HTTP response")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pre-Head client request did not return")
	}
	f.assertCancelledOnce(t)
}

func (f *translationLifecycleFixture) request(ctx context.Context) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.gateway.URL+"/v1/responses", strings.NewReader(`{"model":"client-model","stream":true,"input":"hello"}`))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.secret)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func (f *translationLifecycleFixture) assertCancelledOnce(t *testing.T) {
	t.Helper()
	select {
	case result := <-f.finalized:
		if result.Outcome != core.OutcomeCancelled {
			t.Fatalf("translation outcome = %q, want cancelled: %+v", result.Outcome, result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("translated attempt did not finalize")
	}
	if got := f.accounting.snapshot(); got.admit != 1 || got.intent != 1 || got.finalize != 1 {
		t.Fatalf("translation accounting calls = %+v, want exactly one attempt/finalization", got)
	}
	select {
	case extra := <-f.finalized:
		t.Fatalf("translated request finalized more than once: %+v", extra)
	default:
	}
	var requests []sqlite.RequestUsage
	deadline := time.Now().Add(2 * time.Second)
	for {
		var err error
		requests, err = sqlite.NewLedger(f.prepared.runtimeDB).QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(requests) == 1 && len(requests[0].Attempts) == 1 && requests[0].Request.FinishedAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("durable translated request/attempt rows did not settle: %+v", requests)
		}
		time.Sleep(time.Millisecond)
	}
	row := requests[0]
	if row.Request.State == "succeeded" || row.Request.FinishedAt == nil {
		t.Fatalf("durable request is not one terminal cancellation: %+v", row.Request)
	}
	if state := row.Attempts[0].Attempt.State; state != "cancelled" && state != "interrupted" {
		t.Fatalf("durable attempt state = %q, want cancellation/interruption", state)
	}
	if category := row.Attempts[0].Attempt.ErrorCategory; category == nil || *category != string(core.CategoryCancelled) {
		t.Fatalf("durable attempt error category = %v, want cancelled", category)
	}
}

func TestTranslationCancellationDurableRows(t *testing.T) {
	upstreamStarted, upstreamCancelled := make(chan struct{}), make(chan struct{})
	f := newTranslationLifecycleFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
		w.(http.Flusher).Flush()
		close(upstreamStarted)
		<-r.Context().Done()
		close(upstreamCancelled)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := f.request(ctx)
	if err != nil {
		t.Fatal(err)
	}
	response, err := f.gateway.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	var received strings.Builder
	for !strings.Contains(received.String(), "response.output_text.delta") {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("translated delta unavailable: %v, body=%q", err, received.String())
		}
		received.WriteString(line)
	}
	cancel()
	_ = response.Body.Close()
	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("translated cancellation request did not reach upstream")
	}
	select {
	case <-upstreamCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("translated client cancellation did not stop upstream")
	}
	f.assertCancelledOnce(t)
}

func TestTranslationWriterFailureCancelsAndSettlesDurably(t *testing.T) {
	upstreamStarted, upstreamCancelled := make(chan struct{}), make(chan struct{})
	f := newTranslationLifecycleFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
		w.(http.Flusher).Flush()
		close(upstreamStarted)
		<-r.Context().Done()
		close(upstreamCancelled)
	}))
	req, err := f.request(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writer := &failingResponseWriter{header: make(http.Header)}
	done := make(chan struct{})
	go func() { defer close(done); f.gateway.Config.Handler.ServeHTTP(writer, req) }()
	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("translated writer-failure request did not reach upstream")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("translated handler did not return after writer failure")
	}
	select {
	case <-upstreamCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("translated writer failure did not cancel upstream")
	}
	if writer.writeCalls != 1 || writer.writeErr == nil {
		t.Fatalf("translated response writer = %+v", writer)
	}
	f.assertCancelledOnce(t)
}

func TestTranslationTimeoutCallerDeadlineCancelsAndSettlesDurably(t *testing.T) {
	upstreamStarted, upstreamCancelled := make(chan struct{}), make(chan struct{})
	f := newTranslationLifecycleFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
		w.(http.Flusher).Flush()
		close(upstreamStarted)
		<-r.Context().Done()
		close(upstreamCancelled)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, err := f.request(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.gateway.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	var received strings.Builder
	for !strings.Contains(received.String(), "response.output_text.delta") {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("translated delta unavailable before caller deadline: %v, body=%q", err, received.String())
		}
		received.WriteString(line)
	}
	select {
	case <-upstreamStarted:
	case <-time.After(time.Second):
		t.Fatal("translated deadline request did not reach upstream")
	}
	_, _ = io.Copy(io.Discard, reader)
	_ = resp.Body.Close()
	select {
	case <-upstreamCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("translated caller deadline did not cancel upstream")
	}
	f.assertCancelledOnce(t)
}
