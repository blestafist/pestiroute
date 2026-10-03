package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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
	return newTranslationLifecycleFixtureWithConnSetup(t, backend, nil, injected...)
}

func newTranslationLifecycleFixtureWithConnSetup(t *testing.T, backend http.Handler, setupConn func(net.Conn), injected ...core.HTTPDoer) *translationLifecycleFixture {
	return newTranslationLifecycleFixtureConfigured(t, backend, setupConn, nil, injected...)
}

func newTranslationLifecycleFixtureConfigured(t *testing.T, backend http.Handler, setupConn func(net.Conn), configure func(context.Context, *sql.DB, secure.MasterKey, *protectedConfig) error, injected ...core.HTTPDoer) *translationLifecycleFixture {
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
	backendURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0").protected
	p.Connectors[0].Settings.BaseURL = upstream.URL + "/v1"
	p.Connectors = append(p.Connectors, protectedConnector{ID: "anthropic", Kind: "connector", Implementation: "pestiroute.anthropic.messages", Settings: nativeSettings{Model: "client-model", AccountID: account.ID, CredentialID: "anthro-key"}})
	p.Routes = append(p.Routes, protectedRoute{ID: "translation-route", Protocol: responsesProtocol, Mode: "translation", Model: "client-model", Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(4096)}, Targets: []routeTarget{{Connector: "anthropic", Account: account.ID}}})
	if configure != nil {
		if err := configure(ctx, db, key, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
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
	gateway = httptest.NewUnstartedServer(h)
	if setupConn != nil {
		gateway.Config.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
			setupConn(conn)
			return ctx
		}
	}
	gateway.Start()
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

func TestTranslationFallbackHTTPRejectionsDoNotSwitchAccounts(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var first, second atomic.Int32
			doer := anthropicDoerFunc(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("X-Api-Key") == "synthetic-anthropic-key" {
					first.Add(1)
				} else {
					second.Add(1)
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"private provider detail"}`)), Request: req}, nil
			})
			f := newTranslationFallbackFixture(t, doer)
			req, err := f.request(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			resp, err := f.gateway.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if first.Load() != 1 || second.Load() != 0 {
				t.Fatalf("upstream account calls = %d/%d, want 1/0", first.Load(), second.Load())
			}
			wantStatus := status
			if status >= 500 {
				wantStatus = http.StatusServiceUnavailable
			}
			if resp.StatusCode != wantStatus || strings.Contains(string(body), "private provider detail") {
				t.Fatalf("HTTP %d body=%s; want sanitized status %d", resp.StatusCode, body, wantStatus)
			}
			select {
			case result := <-f.finalized:
				if result.Outcome != core.OutcomeFailed {
					t.Fatalf("attempt outcome = %q, want failed", result.Outcome)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("translated attempt did not finalize")
			}
			if got := f.accounting.snapshot(); got.admit != 1 || got.intent != 1 || got.finalize != 1 {
				t.Fatalf("accounting calls = %+v, want one admitted/finalized attempt", got)
			}
			rows, err := sqlite.NewLedger(f.prepared.runtimeDB).QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 2})
			if err != nil || len(rows) != 1 || len(rows[0].Attempts) != 1 {
				t.Fatalf("durable attempts = %+v, err=%v; want exactly one", rows, err)
			}
		})
	}
}

func newTranslationFallbackFixture(t *testing.T, doer core.HTTPDoer) *translationLifecycleFixture {
	t.Helper()
	return newTranslationLifecycleFixtureConfigured(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), nil, func(ctx context.Context, db *sql.DB, key secure.MasterKey, p *protectedConfig) error {
		const accountID, credentialID = "anthropic-account-b", "anthro-key-b"
		if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: accountID, Connector: "anthropic-b", Enabled: true}); err != nil {
			return err
		}
		envelope, err := secure.Seal(key, 2, "test-v1", "credentials", credentialID, accountID, []byte("synthetic-anthropic-key-b"))
		if err != nil {
			return err
		}
		if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: credentialID, AccountID: accountID, FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext}); err != nil {
			return err
		}
		p.Connectors = append(p.Connectors, protectedConnector{ID: "anthropic-b", Kind: "connector", Implementation: "pestiroute.anthropic.messages", Settings: nativeSettings{Model: "client-model", AccountID: accountID, CredentialID: credentialID}})
		p.Routes[1].Targets = append(p.Routes[1].Targets, routeTarget{Connector: "anthropic-b", Account: accountID})
		policy, err := sqlite.NewKeyPolicies(db).GetLatest(ctx, "policy-id-a")
		if err != nil {
			return err
		}
		if _, err := sqlite.NewKeyPolicies(db).Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: policy.Models, Connectors: append(policy.Connectors, "anthropic-b"), RPM: 20, TPM: 100000}); err != nil {
			return err
		}
		attempts := 2
		p.Routes[1].Retry = &retryConfig{MaxAttempts: &attempts, Deadline: "5s"}
		return nil
	}, doer)
}

func TestTranslationFallbackCommittedStreamFailureDoesNotSwitchAccounts(t *testing.T) {
	var first, second atomic.Int32
	doer := anthropicDoerFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("X-Api-Key") == "synthetic-anthropic-key" {
			first.Add(1)
		} else {
			second.Add(1)
		}
		body := "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"private provider detail\"}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	f := newTranslationFallbackFixture(t, doer)
	req, err := f.request(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.gateway.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if first.Load() != 1 || second.Load() != 0 {
		t.Fatalf("upstream account calls = %d/%d, want 1/0", first.Load(), second.Load())
	}
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("response.failed")) || strings.Contains(string(body), "private provider detail") {
		t.Fatalf("HTTP %d body=%s; want sanitized committed failure", resp.StatusCode, body)
	}
	select {
	case result := <-f.finalized:
		if result.Outcome != core.OutcomeFailed {
			t.Fatalf("attempt outcome = %q, want failed", result.Outcome)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("committed translated attempt did not finalize")
	}
	if got := f.accounting.snapshot(); got.admit != 1 || got.intent != 1 || got.finalize != 1 {
		t.Fatalf("accounting calls = %+v, want one admitted/finalized attempt", got)
	}
}

func TestTranslationFallbackLocalAndAmbiguousFailuresDoNotSwitchAccounts(t *testing.T) {
	t.Run("local validation", func(t *testing.T) {
		var calls atomic.Int32
		doer := anthropicDoerFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, errors.New("unexpected upstream call")
		})
		f := newTranslationFallbackFixture(t, doer)
		req, err := f.request(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		invalid := `{"model":"client-model","input":[{"type":"message","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"hi"}]}]}`
		req.Body = io.NopCloser(strings.NewReader(invalid))
		req.ContentLength = int64(len(invalid))
		resp, err := f.gateway.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || calls.Load() != 0 {
			t.Fatalf("HTTP %d upstream calls=%d, want local 400 and no dispatch", resp.StatusCode, calls.Load())
		}
	})
	t.Run("ambiguous transport failure", func(t *testing.T) {
		var first, second atomic.Int32
		doer := anthropicDoerFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("X-Api-Key") == "synthetic-anthropic-key" {
				first.Add(1)
			} else {
				second.Add(1)
			}
			return nil, errors.New("synthetic transport failure")
		})
		f := newTranslationFallbackFixture(t, doer)
		req, err := f.request(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		resp, err := f.gateway.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || first.Load() != 1 || second.Load() != 0 {
			t.Fatalf("HTTP %d upstream account calls=%d/%d, want unavailable and 1/0", resp.StatusCode, first.Load(), second.Load())
		}
	})
}

func TestTranslationFallbackStatefulRequestFailsClosedAcrossAccounts(t *testing.T) {
	var calls atomic.Int32
	doer := anthropicDoerFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected upstream call")
	})
	f := newTranslationFallbackFixture(t, doer)
	req, err := f.request(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stateful := `{"model":"client-model","previous_response_id":"resp-1","input":"hello","stream":true}`
	req.Body = io.NopCloser(strings.NewReader(stateful))
	req.ContentLength = int64(len(stateful))
	resp, err := f.gateway.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(`"code":"unsupported_target"`)) || calls.Load() != 0 {
		t.Fatalf("HTTP %d upstream calls=%d body=%s; want pre-dispatch affinity rejection", resp.StatusCode, calls.Load(), body)
	}
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

func TestTranslationShutdownDrainsActiveStream(t *testing.T) {
	continueStream := make(chan struct{})
	started := make(chan struct{})
	f := newTranslationLifecycleFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"early\"}}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-continueStream:
			_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		case <-r.Context().Done():
		}
	}))
	responseDone := make(chan error, 1)
	go func() {
		req, _ := f.request(context.Background())
		resp, err := f.gateway.Client().Do(req)
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		responseDone <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("translated stream did not reach the backend")
	}
	shutdownDone := make(chan error, 1)
	shutdownStarted := make(chan struct{})
	f.gateway.Config.RegisterOnShutdown(func() { close(shutdownStarted) })
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		shutdownDone <- f.gateway.Config.Shutdown(ctx)
	}()
	select {
	case <-shutdownStarted:
	case <-time.After(time.Second):
		t.Fatal("translated gateway shutdown did not start")
	}
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown did not drain active translated stream: %v", err)
	default:
	}
	close(continueStream)
	select {
	case err := <-responseDone:
		if err != nil {
			t.Fatalf("drained translated response: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("translated response did not finish during drain")
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("translated gateway did not finish draining")
	}
	if got := f.accounting.snapshot(); got.admit != 1 || got.intent != 1 || got.finalize != 1 {
		t.Fatalf("drained translated accounting = %+v", got)
	}
}

func TestTranslationShutdownExpiryCancelsActiveStream(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	f := newTranslationLifecycleFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"early\"}}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	responseDone := make(chan struct{})
	go func() {
		defer close(responseDone)
		req, _ := f.request(context.Background())
		resp, err := f.gateway.Client().Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("translated stream did not reach the backend")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := f.gateway.Config.Shutdown(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired translated drain = %v, want deadline exceeded", err)
	}
	if err := f.gateway.Config.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("expired shutdown did not cancel translated upstream")
	}
	select {
	case <-responseDone:
	case <-time.After(time.Second):
		t.Fatal("translated client remained active after forced close")
	}
	f.assertCancelledOnce(t)
}

func TestTranslationSlowConsumerBackpressureResumesInOrder(t *testing.T) {
	first, second := strings.Repeat("a", 512<<10), strings.Repeat("b", 512<<10)
	steps := []string{
		anthropicSSE("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`),
		anthropicSSE("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`),
		anthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+first+`"}}`),
		anthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+second+`"}}`),
		anthropicSSE("content_block_stop", `{"type":"content_block_stop","index":0}`),
		anthropicSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`),
		anthropicSSE("message_stop", `{"type":"message_stop"}`),
	}
	upstreamBody := newStepSSEBody(steps)
	var upstreamCalls atomic.Int32
	writeBufferSet := make(chan error, 1)
	f := newTranslationLifecycleFixtureWithConnSetup(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), func(conn net.Conn) {
		writeBufferSet <- conn.(*net.TCPConn).SetWriteBuffer(32 << 10)
	}, anthropicDoerFunc(func(*http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: upstreamBody}, nil
	}))
	body := `{"model":"client-model","stream":true,"input":"hello"}`
	conn, err := net.Dial("tcp", f.gateway.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(60 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).SetReadBuffer(64 << 10); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "POST /v1/responses HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", f.gateway.Listener.Addr(), f.secret, len(body), body); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("translated response status = %d", response.StatusCode)
	}
	if err := <-writeBufferSet; err != nil {
		t.Fatalf("limit test gateway TCP write buffer: %v", err)
	}
	go upstreamBody.releaseThrough(2)
	bodyReader := bufio.NewReader(response.Body)
	var consumed []byte
	for {
		event, err := readGatewaySSE(bodyReader)
		if err != nil {
			t.Fatal(err)
		}
		consumed = append(consumed, event...)
		if strings.Contains(string(event), "event: response.content_part.added") {
			break
		}
	}
	waitStepProgress(t, upstreamBody, 3)
	select {
	case <-upstreamBody.started[3]:
		t.Fatal("gateway read the next provider event while the downstream TCP client was stalled")
	case <-time.After(250 * time.Millisecond):
	}
	go upstreamBody.releaseThrough(len(steps) - 1)
	remaining, err := io.ReadAll(bodyReader)
	if err != nil {
		t.Fatalf("read resumed response: %v (upstream events %d/%d)", err, upstreamBody.completed.Load(), len(steps))
	}
	_ = response.Body.Close()
	consumed = append(consumed, remaining...)
	waitStepProgress(t, upstreamBody, len(steps))
	var deltas []string
	for _, event := range strings.Split(string(consumed), "\n\n") {
		if !strings.Contains(event, "event: response.output_text.delta") {
			continue
		}
		for _, line := range strings.Split(event, "\n") {
			if strings.HasPrefix(line, "data: ") {
				var payload struct {
					Delta string `json:"delta"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
					t.Fatal(err)
				}
				deltas = append(deltas, payload.Delta)
			}
		}
	}
	if len(deltas) != 2 || deltas[0] != first || deltas[1] != second {
		t.Fatalf("resumed text deltas are not ordered/verbatim: count=%d", len(deltas))
	}
	if !strings.Contains(string(consumed), "event: response.completed") {
		t.Fatal("resumed translated stream did not complete after the ordered deltas")
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want exactly one", upstreamCalls.Load())
	}
}

func TestTranslationExhaustionFailsAndFinalizesOnce(t *testing.T) {
	const chunk = 600 << 10
	for _, kind := range []string{"text", "arguments", "blocks"} {
		t.Run(kind, func(t *testing.T) {
			var events []string
			reqBody := `{"model":"client-model","stream":true,"input":"hello"}`
			if kind == "text" {
				events = []string{
					anthropicSSE("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`),
					anthropicSSE("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`),
					anthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+strings.Repeat("x", chunk)+`"}}`),
					anthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+strings.Repeat("y", chunk)+`"}}`),
				}
			} else if kind == "arguments" {
				reqBody = `{"model":"client-model","stream":true,"input":"hello","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"tool_choice":"auto"}`
				events = []string{
					anthropicSSE("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`),
					anthropicSSE("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call-1","name":"f"}}`),
					anthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"`+strings.Repeat("x", chunk)+`"}}`),
					anthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"`+strings.Repeat("y", chunk)+`"}}`),
				}
			} else {
				var tools strings.Builder
				tools.WriteString(`{"model":"client-model","stream":true,"input":"hello","tools":[`)
				for i := 0; i < 65; i++ {
					if i > 0 {
						tools.WriteByte(',')
					}
					fmt.Fprintf(&tools, `{"type":"function","name":"f%d","parameters":{"type":"object"}}`, i)
				}
				tools.WriteString(`],"tool_choice":"auto"}`)
				reqBody = tools.String()
				events = append(events, anthropicSSE("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`))
				for i := 0; i < 65; i++ {
					events = append(events,
						anthropicSSE("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"call-%d","name":"f%d"}}`, i, i, i)),
						anthropicSSE("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i)))
				}
			}
			var calls atomic.Int32
			f := newTranslationLifecycleFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), anthropicDoerFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(strings.Join(events, "")))}, nil
			}))
			req, err := f.request(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			req.Body = io.NopCloser(strings.NewReader(reqBody))
			req.ContentLength = int64(len(reqBody))
			response, err := f.gateway.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil {
				t.Fatalf("read failed translation: %v", err)
			}
			if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "event: response.failed") || strings.Contains(string(body), "event: response.completed") || calls.Load() != 1 {
				preview := body
				if len(preview) > 240 {
					preview = preview[:240]
				}
				t.Fatalf("exhaustion response status=%d completed=%t upstream calls=%d body-prefix=%q", response.StatusCode, strings.Contains(string(body), "event: response.completed"), calls.Load(), preview)
			}
			select {
			case result := <-f.finalized:
				if result.Outcome != core.OutcomeFailed {
					t.Fatalf("exhaustion finalized as %q, want failed", result.Outcome)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("exhausted attempt did not finalize")
			}
			if got := f.accounting.snapshot(); got.admit != 1 || got.intent != 1 || got.finalize != 1 {
				t.Fatalf("exhaustion accounting = %+v, want exactly one finalization", got)
			}
			requests, err := sqlite.NewLedger(f.prepared.runtimeDB).QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 2})
			if err != nil || len(requests) != 1 || len(requests[0].Attempts) != 1 || requests[0].Request.FinishedAt == nil || requests[0].Request.State == "succeeded" || requests[0].Attempts[0].Attempt.State != "failed" {
				t.Fatalf("durable exhausted request/attempt rows = %+v, err=%v", requests, err)
			}
			select {
			case extra := <-f.finalized:
				t.Fatalf("exhausted request finalized again: %+v", extra)
			default:
			}
		})
	}
}

func anthropicSSE(event, data string) string { return "event: " + event + "\ndata: " + data + "\n\n" }

type stepSSEBody struct {
	steps     []string
	started   []chan struct{}
	gates     []chan struct{}
	gateOnce  []sync.Once
	index     int
	offset    int
	completed atomic.Int32
}

func newStepSSEBody(steps []string) *stepSSEBody {
	body := &stepSSEBody{steps: steps, started: make([]chan struct{}, len(steps)), gates: make([]chan struct{}, len(steps)), gateOnce: make([]sync.Once, len(steps))}
	for i := range steps {
		body.started[i], body.gates[i] = make(chan struct{}), make(chan struct{})
	}
	return body
}

func (b *stepSSEBody) Read(p []byte) (int, error) {
	if b.index == len(b.steps) {
		return 0, io.EOF
	}
	if b.offset == 0 {
		close(b.started[b.index])
		<-b.gates[b.index]
	}
	n := copy(p, b.steps[b.index][b.offset:])
	b.offset += n
	if b.offset == len(b.steps[b.index]) {
		b.offset = 0
		b.index++
		b.completed.Add(1)
	}
	return n, nil
}

func (b *stepSSEBody) releaseThrough(index int) {
	for i := 0; i <= index && i < len(b.steps); i++ {
		<-b.started[i]
		b.gateOnce[i].Do(func() { close(b.gates[i]) })
	}
}

func (b *stepSSEBody) Close() error {
	for i := range b.steps {
		b.gateOnce[i].Do(func() { close(b.gates[i]) })
	}
	return nil
}

func waitStepProgress(t *testing.T, body *stepSSEBody, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if int(body.completed.Load()) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("upstream completed %d/%d gated SSE events", body.completed.Load(), want)
}

func readGatewaySSE(reader *bufio.Reader) ([]byte, error) {
	var event []byte
	for {
		line, err := reader.ReadBytes('\n')
		event = append(event, line...)
		if err != nil {
			return event, err
		}
		if len(line) == 1 && line[0] == '\n' {
			return event, nil
		}
	}
}
