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

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

type testKeyStore struct {
	principal core.TrustedPrincipal
	err       error
	calls     atomic.Int32
	lastToken atomic.Value
}

type testPolicyStore struct{ snapshot core.PolicySnapshot }

func (s testPolicyStore) Snapshot(context.Context, core.TrustedPrincipal) (core.PolicySnapshot, error) {
	return s.snapshot, nil
}

type testAccountAuthorizer struct{}

func (testAccountAuthorizer) AuthorizeAccount(_ context.Context, account, connector string) error {
	if account == "AUTH_TEST_PROVIDER" && connector == "responses-connector" {
		return nil
	}
	return core.ErrPermissionDenied
}

type testAccountingStore struct{}

func (testAccountingStore) Admit(context.Context, core.AccountingAdmission) error         { return nil }
func (testAccountingStore) BeginAttempt(context.Context, core.AccountingAdmission) error  { return nil }
func (testAccountingStore) RecordDispatchIntent(context.Context, string, time.Time) error { return nil }
func (testAccountingStore) FinalizeAttempt(context.Context, core.AccountingTerminal) error {
	return nil
}

func (s *testKeyStore) Verify(_ context.Context, token string) (core.TrustedPrincipal, error) {
	s.calls.Add(1)
	s.lastToken.Store(token)
	return s.principal, s.err
}

func TestProtectedAuthAndCredentialSeparation(t *testing.T) {
	const (
		virtualKey = "prv_northbound-secret"
		provider   = "provider-only-secret"
		body       = `{"model":"gpt-5.4-mini","unknown":{"keep":[1,2]}}`
	)
	var upstreamCalls atomic.Int32
	var upstreamAuth string
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		upstreamAuth = r.Header.Get("Authorization")
		data, _ := io.ReadAll(r.Body)
		upstreamBody = string(data)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	store := &testKeyStore{principal: core.TrustedPrincipal{KeyID: "key-id", PolicyID: "policy-id", KeyRevision: 3, PolicyRevision: 7}}
	ready := &atomic.Bool{}
	policyStore := testPolicyStore{snapshot: core.PolicySnapshot{ID: "policy-id", Revision: 7, Enabled: true,
		Models: []string{"gpt-5.4-mini"}, Connectors: []string{"responses-connector"}}}
	h, closeHandler := handler(config{UpstreamEndpoint: upstream.URL + "/v1/responses", UpstreamCredentialEnv: "AUTH_TEST_PROVIDER",
		credential: secret(provider), MaxRequestBodyBytes: 4096, MaxRequestHeaderBytes: 4096, ConnectTimeout: "1s",
		TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s", StreamIdleTimeout: "1s", keyStore: store,
		policyStore: policyStore, accountAuthorizer: testAccountAuthorizer{}, accounting: testAccountingStore{},
		routeBudget: core.RouteBudget{UnknownEstimate: core.UnknownEstimateReserve, ConservativeTokens: 100}, budgetPolicy: "reserve", routeID: "auth-test-route"}, ready)
	defer closeHandler()
	server := httptest.NewServer(h)
	defer server.Close()

	for _, tc := range []struct {
		name, authorization, secondAuthorization string
		verifyErr                                error
		verify                                   bool
	}{
		{name: "missing"},
		{name: "non bearer", authorization: "Basic " + virtualKey},
		{name: "empty", authorization: "Bearer "},
		{name: "malformed", authorization: "Bearer bad,key"},
		{name: "duplicate authorization", authorization: "Bearer first", secondAuthorization: "Bearer second"},
		{name: "invalid", authorization: "Bearer invalid", verifyErr: sqlite.ErrInvalidVirtualKey, verify: true},
		{name: "disabled", authorization: "Bearer disabled", verifyErr: sqlite.ErrVirtualKeyDisabled, verify: true},
		{name: "revoked", authorization: "Bearer revoked", verifyErr: sqlite.ErrVirtualKeyRevoked, verify: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store.err = tc.verifyErr
			beforeVerify := store.calls.Load()
			r := httptest.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader("not json"))
			r.Header.Set("Content-Type", "application/json")
			if tc.authorization != "" {
				r.Header.Add("Authorization", tc.authorization)
			}
			if tc.secondAuthorization != "" {
				r.Header.Add("Authorization", tc.secondAuthorization)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"code":"invalid_api_key"`) || !strings.Contains(w.Body.String(), `"type":"unauthenticated"`) {
				t.Fatalf("response = %d %s", w.Code, w.Body.String())
			}
			if tc.authorization != "" && strings.Contains(w.Body.String(), tc.authorization) {
				t.Fatalf("authentication response exposed key details: %s", w.Body.String())
			}
			wantCalls := int32(0)
			if tc.verify {
				wantCalls = 1
			}
			if got := store.calls.Load() - beforeVerify; got != wantCalls {
				t.Fatalf("Verify calls = %d, want %d", got, wantCalls)
			}
			if got := upstreamCalls.Load(); got != 0 {
				t.Fatalf("failed authentication reached upstream %d times", got)
			}
		})
	}

	store.err = nil
	r := httptest.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+virtualKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("valid key response = %d %s", w.Code, w.Body.String())
	}
	if got := store.lastToken.Load(); got != virtualKey {
		t.Fatalf("verified token = %v, want exact presented token", got)
	}
	if upstreamCalls.Load() != 1 || upstreamBody != body || upstreamAuth != "Bearer "+provider {
		t.Fatalf("upstream calls/body/auth = %d %q %q", upstreamCalls.Load(), upstreamBody, upstreamAuth)
	}
	if strings.Contains(upstreamAuth, virtualKey) || strings.Contains(upstreamBody, virtualKey) {
		t.Fatal("northbound virtual key leaked upstream")
	}
}

func TestProtectedAuthInfrastructureFailureAndProbes(t *testing.T) {
	store := &testKeyStore{err: errors.New("storage unavailable")}
	ready := &atomic.Bool{}
	ready.Store(true)
	h, closeHandler := handler(config{UpstreamEndpoint: "http://127.0.0.1:1/v1/responses", UpstreamCredentialEnv: "AUTH_TEST_PROVIDER",
		credential: "provider", MaxRequestBodyBytes: 1024, MaxRequestHeaderBytes: 1024, ConnectTimeout: "1s",
		TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s", StreamIdleTimeout: "1s", keyStore: store}, ready)
	defer closeHandler()
	for _, path := range []string{"/healthz", "/readyz"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("probe %s without credentials returned %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4-mini"}`))
	r.Header.Set("Authorization", "Bearer valid-looking")
	r.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "storage unavailable") {
		t.Fatalf("storage failure response = %d %s", w.Code, w.Body.String())
	}
}

func (testAccountingStore) FinishRequest(ctx context.Context, id string) error { return nil }
