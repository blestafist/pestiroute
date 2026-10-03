package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
)

func protectedFixture(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath, keyPath := filepath.Join(dir, "gateway.db"), filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyPath, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewAccounts(db).Create(context.Background(), sqlite.Account{ID: "account-a", Connector: "upstream", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewKeyPolicies(db).Create(context.Background(), sqlite.CreateKeyPolicyParams{ID: "policy-id-a", Enabled: true, Models: []string{"model-a"}, Connectors: []string{"upstream"}, RPM: 10, TPM: 1000}); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := secure.Seal(key, 2, "test-v1", "credentials", "PROTECTED_TEST_CREDENTIAL", "account-a", []byte("synthetic"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(db).Create(context.Background(), sqlite.Credential{ID: "PROTECTED_TEST_CREDENTIAL", AccountID: "account-a", FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, dbPath, keyPath
}

func fixtureProtectedConfig(dbPath, keyPath, listen string) config {
	return config{DatabasePath: dbPath, MasterKeyFile: keyPath, Listen: listen, ShutdownTimeout: "1s", protected: &protectedConfig{
		Server:  protectedServer{Listen: listen, MaxRequestBytes: 1 << 20, ShutdownTimeout: "1s"},
		Storage: protectedStorage{Driver: "sqlite", Path: dbPath}, Secrets: protectedSecrets{MasterKeyFile: keyPath},
		Connectors: []protectedConnector{{ID: "upstream", Kind: "connector", Implementation: "pestiroute.responses.native", Settings: nativeSettings{
			BaseURL: "http://127.0.0.1:9999/v1", UpstreamProtocol: responsesProtocol, Mode: "native", CredentialEnv: "PROTECTED_TEST_CREDENTIAL",
			MaxRequestBodyBytes: 1 << 20, MaxRequestHeaderBytes: 8192, ConnectTimeout: "1s", TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s", StreamIdleTimeout: "1s",
		}}},
		Routes:   []protectedRoute{{ID: "route-a", Protocol: responsesProtocol, Mode: "native", Model: "model-a", Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(10)}, Targets: []routeTarget{{Connector: "upstream", Account: "account-a"}}}},
		Policies: map[string]string{"standard": "policy-id-a"},
	}}
}

func ptrInt64(v int64) *int64 { return &v }

func TestProtectedStartupValidationAndRecovery(t *testing.T) {
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	_, dbPath, keyPath := protectedFixture(t)
	seedRecoveryRows(t, dbPath)
	c, err := prepareProtectedConfig(context.Background(), fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	if c.runtimeDB == nil || c.processLock == nil || c.keyStore == nil || len(c.Routes) != 1 {
		t.Fatal("protected startup did not compose persistent runtime services")
	}
	assertProtectedRecovery(t, c.runtimeDB)
	if err := c.runtimeDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.processLock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedMultiTargetAffinityGateAndOrdinaryRequest(t *testing.T) {
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	t.Setenv("PROTECTED_TEST_CREDENTIAL_B", "synthetic-b")
	_, dbPath, keyPath := protectedFixture(t)
	ctx := context.Background()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	accounts, policies := sqlite.NewAccounts(db), sqlite.NewKeyPolicies(db)
	if _, err := accounts.Create(ctx, sqlite.Account{ID: "account-b", Connector: "upstream-b", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	policy, err := policies.GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	policy, err = policies.Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{
		Enabled: true, Models: policy.Models, Connectors: []string{"upstream", "upstream-b"}, RPM: policy.RPM, TPM: policy.TPM,
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := secure.Seal(key, 2, "test-v1", "credentials", "PROTECTED_TEST_CREDENTIAL_B", "account-b", []byte("synthetic-b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{
		ID: "PROTECTED_TEST_CREDENTIAL_B", AccountID: "account-b", FormatVersion: envelope.FormatVersion,
		KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext,
	}); err != nil {
		t.Fatal(err)
	}
	issued, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var primaryCalls, secondaryCalls atomic.Int32
	upstream := func(calls *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"completed"}`)
		}))
	}
	primary, secondary := upstream(&primaryCalls), upstream(&secondaryCalls)
	defer primary.Close()
	defer secondary.Close()
	c := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
	c.protected.Connectors[0].Settings.BaseURL = primary.URL + "/v1"
	second := c.protected.Connectors[0]
	second.ID = "upstream-b"
	second.Settings.BaseURL = secondary.URL + "/v1"
	second.Settings.CredentialEnv = "PROTECTED_TEST_CREDENTIAL_B"
	c.protected.Connectors = append(c.protected.Connectors, second)
	c.protected.Routes[0].Targets = append(c.protected.Routes[0].Targets, routeTarget{Connector: second.ID, Account: "account-b"})
	prepared, err := prepareProtectedConfig(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Routes) != 2 || prepared.Routes[0].CandidateGroup == "" || prepared.Routes[0].CandidateGroup != prepared.Routes[1].CandidateGroup {
		t.Fatalf("protected target group not normalized: %+v", prepared.Routes)
	}
	var ready, draining atomic.Bool
	handler, closeComponents, err := composeHandler(prepared, &ready, &draining, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeComponents(context.Background()) }()
	server := httptest.NewServer(handler)
	defer server.Close()
	post := func(body string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+issued.Secret)
		req.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return response, data
	}
	response, body := post(`{"model":"model-a","previous_response_id":"resp-1","session_bound":false}`)
	if response.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(`"code":"unsupported_target"`)) || primaryCalls.Load() != 0 || secondaryCalls.Load() != 0 {
		t.Fatalf("session-bound request status=%d body=%s upstream calls=%d/%d", response.StatusCode, body, primaryCalls.Load(), secondaryCalls.Load())
	}
	response, body = post(`{"model":"model-a","input":"stored","store":true}`)
	if response.StatusCode != http.StatusOK || primaryCalls.Load() != 1 || secondaryCalls.Load() != 0 {
		t.Fatalf("store=true request status=%d body=%s upstream calls=%d/%d", response.StatusCode, body, primaryCalls.Load(), secondaryCalls.Load())
	}
	response, body = post(`{"model":"model-a","input":"ordinary"}`)
	if response.StatusCode != http.StatusOK || primaryCalls.Load() != 2 || secondaryCalls.Load() != 0 {
		t.Fatalf("ordinary stored-response request status=%d body=%s upstream calls=%d/%d", response.StatusCode, body, primaryCalls.Load(), secondaryCalls.Load())
	}
}

func TestProtectedStartupFailsBeforeListenerOnInvalidReferencesOrLock(t *testing.T) {
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	_, dbPath, keyPath := protectedFixture(t)
	lock, err := sqlite.AcquireProcessLock(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepareProtectedConfig(context.Background(), fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")); err == nil {
		t.Fatal("second gateway acquired protected startup")
	}
	_ = lock.Close()
	c := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
	c.protected.Routes[0].Targets[0].Account = "missing"
	if _, err := prepareProtectedConfig(context.Background(), c); err == nil {
		t.Fatal("missing account reference passed protected startup")
	}
	lock, err = sqlite.AcquireProcessLock(dbPath)
	if err != nil {
		t.Fatalf("failed reference validation leaked process lock: %v", err)
	}
	_ = lock.Close()
	badKey := filepath.Join(t.TempDir(), "bad.key")
	if err := os.WriteFile(badKey, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareProtectedConfig(context.Background(), fixtureProtectedConfig(dbPath, badKey, "127.0.0.1:0")); err == nil {
		t.Fatal("invalid master key passed protected startup")
	}
	unmigratedPath := filepath.Join(t.TempDir(), "unmigrated.db")
	unmigrated, err := sqlite.Open(unmigratedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := unmigrated.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareProtectedConfig(context.Background(), fixtureProtectedConfig(unmigratedPath, keyPath, "127.0.0.1:0")); err == nil {
		t.Fatal("unmigrated database passed protected startup")
	}
}

func TestProtectedStartupReferenceFailureDoesNotBindListener(t *testing.T) {
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	_, dbPath, keyPath := protectedFixture(t)
	address := freeTCPAddress(t)
	configPath := writeProtectedYAML(t, dbPath, keyPath, address, "http://127.0.0.1:9999/v1/responses", "1s")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("account-a"), []byte("missing-account"), 1)
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"-config-format", "yaml", "-config", configPath}); err == nil {
		t.Fatal("protected startup accepted missing account")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("failed protected startup opened listener: %v", err)
	}
	_ = listener.Close()
}

func TestProtectedShutdownReleasesDatabaseLock(t *testing.T) {
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	_, dbPath, keyPath := protectedFixture(t)
	seedRecoveryRows(t, dbPath)
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	_ = probe.Close()
	configPath := filepath.Join(t.TempDir(), "gateway.yaml")
	yaml := fmt.Sprintf(`version: 1
server: {listen: %q, max_request_bytes: 1048576, shutdown_timeout: 1s}
storage: {driver: sqlite, path: %q}
secrets: {master_key_file: %q}
connectors:
  - id: upstream
    kind: connector
    implementation: pestiroute.responses.native
    protocols: [openai.responses.v1]
    settings: {base_url: http://127.0.0.1:9999/v1, upstream_protocol: openai.responses.v1, mode: native, credential_env: PROTECTED_TEST_CREDENTIAL, max_request_body_bytes: 1048576, max_request_header_bytes: 8192, connect_timeout: 1s, tls_handshake_timeout: 1s, response_header_timeout: 1s, stream_idle_timeout: 1s}
routes:
  - id: route-a
    protocol: openai.responses.v1
    mode: native
    model: model-a
    adapter: pestiroute.responses.native
    policy: standard
    budget: {unknown_estimate: reserve, conservative_tokens: 10}
    targets: [{connector: upstream, account: account-a}]
policies: {standard: policy-id-a}
`, address, dbPath, keyPath)
	if err := os.WriteFile(configPath, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config-format", "yaml", "-config", configPath}) }()
	client := &http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(3 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		response, err := client.Get("http://" + address + "/readyz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		select {
		case err := <-done:
			t.Fatalf("protected gateway exited before readiness: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("protected gateway did not become ready")
	}
	checkDB, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	assertProtectedRecovery(t, checkDB)
	_ = checkDB.Close()
	if err := sqliteLockHeld(dbPath); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("protected shutdown did not finish")
	}
	lock, err := sqlite.AcquireProcessLock(dbPath)
	if err != nil {
		t.Fatalf("shutdown did not release database lock: %v", err)
	}
	_ = lock.Close()
}

const (
	protectedUndispatchedAttempt = "protected-undispatched"
	protectedIntentAttempt       = "protected-intent"
)

func seedRecoveryRows(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	policy, err := sqlite.NewKeyPolicies(db).GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	key, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	ledger := sqlite.NewLedger(db)
	admit := func(requestID, attemptID string) {
		q := sqlite.RequestRecord{ID: requestID, VirtualKeyID: key.ID, KeyRevision: 1, PolicyID: policy.ID, PolicyRevision: policy.Revision, Protocol: responsesProtocol, Model: "model-a", RouteID: "route-a", State: "admitted"}
		a := sqlite.AttemptRecord{ID: attemptID, RequestID: requestID, Ordinal: 1, AccountID: "account-a", Connector: "upstream", RouteID: "route-a", BudgetPolicy: "reserve", EstimateTokens: 10, EstimateMethod: "fixture", State: "reserved"}
		r := sqlite.ReservationRecord{AttemptID: attemptID, EstimatedTokens: 10}
		if err := ledger.Admit(ctx, q, a, r); err != nil {
			t.Fatal(err)
		}
	}
	admit("protected-undispatched-request", protectedUndispatchedAttempt)
	admit("protected-intent-request", protectedIntentAttempt)
	if err := ledger.RecordDispatchIntent(ctx, protectedIntentAttempt, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func assertProtectedRecovery(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	ledger := sqlite.NewLedger(db)
	for _, tc := range []struct {
		attempt, request, state, holdState string
		charge                             int64
	}{{protectedUndispatchedAttempt, "protected-undispatched-request", "failed", "released", 0}, {protectedIntentAttempt, "protected-intent-request", "interrupted", "conservative", 10}} {
		attempt, err := ledger.GetAttempt(ctx, tc.attempt)
		if err != nil || attempt.State != tc.state {
			t.Fatalf("recovered attempt %s = %+v, %v", tc.attempt, attempt, err)
		}
		request, err := ledger.GetRequest(ctx, tc.request)
		if err != nil || request.State != tc.state {
			t.Fatalf("recovered request %s = %+v, %v", tc.request, request, err)
		}
		reservation, err := ledger.GetReservation(ctx, tc.attempt)
		if err != nil || reservation.State != tc.holdState || reservation.EffectiveCharge != tc.charge || reservation.ReconciledAt == nil {
			t.Fatalf("recovered reservation %s = %+v, %v", tc.attempt, reservation, err)
		}
	}
	if usage, err := ledger.GetUsage(ctx, protectedUndispatchedAttempt); !errors.Is(err, sqlite.ErrLedgerNotFound) || usage.AttemptID != "" {
		t.Fatalf("undispatched attempt usage = %+v, %v", usage, err)
	}
	usage, err := ledger.GetUsage(ctx, protectedIntentAttempt)
	if err != nil || usage.Source != "unknown" || usage.Completeness != "unknown" || usage.InputTokens != nil || usage.OutputTokens != nil {
		t.Fatalf("intent recovery usage = %+v, %v", usage, err)
	}
}

func TestProtectedStartupLiveAdminUpdatesAffectNextAdmission(t *testing.T) {
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	_, dbPath, keyPath := protectedFixture(t)
	binary := buildGatewayBinary(t)
	keyOutput, err := runAdminProcess(t, binary, dbPath, keyPath, "key", "create", "--policy", "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(keyOutput)
	secret := strings.TrimPrefix(fields[0], "secret=")
	var keyID string
	for _, field := range fields[1:] {
		name, value, ok := strings.Cut(field, "=")
		if ok && name == "id" {
			keyID = value
		}
	}
	if !strings.HasPrefix(secret, "prv_") || keyID == "" {
		t.Fatalf("unexpected key create result %q", keyOutput)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"completed"}`)
	}))
	defer upstream.Close()
	address := freeTCPAddress(t)
	configPath := writeProtectedYAML(t, dbPath, keyPath, address, upstream.URL+"/v1", "1s")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config-format", "yaml", "-config", configPath}) }()
	waitProtectedReady(t, address, done)
	post := func() *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "http://"+address+"/v1/responses", strings.NewReader(`{"model":"model-a","input":"hi"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := post()
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("baseline request status %d", response.StatusCode)
	}
	admin := func(args ...string) {
		t.Helper()
		if _, err := runAdminProcess(t, binary, dbPath, keyPath, args...); err != nil {
			t.Fatalf("admin %v: %v", args, err)
		}
	}
	admin("account", "disable", "account-a")
	response = post()
	accountBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden || !bytes.Contains(accountBody, []byte(`"code":"permission_denied"`)) {
		t.Fatalf("disabled account admission status=%d body=%s", response.StatusCode, accountBody)
	}
	admin("account", "enable", "account-a")
	admin("policy", "update", "policy-id-a", "--expected-revision", "1", "--disabled")
	admin("key", "update-policy", keyID, "--policy", "policy-id-a", "--expected-revision", "1")
	response = post()
	policyBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden || !bytes.Contains(policyBody, []byte(`"code":"permission_denied"`)) {
		t.Fatalf("disabled policy admission status=%d body=%s", response.StatusCode, policyBody)
	}
	admin("key", "revoke", keyID)
	response = post()
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked key admission status=%d", response.StatusCode)
	}
	cancelProtected(t, cancel, done)
}

func TestProtectedShutdownDrainsActiveRequestAndClosesResources(t *testing.T) {
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	_, dbPath, keyPath := protectedFixture(t)
	keyOutput, err := runAdminTest(t, dbPath, keyPath, nil, "key", "create", "--policy", "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.TrimPrefix(strings.Fields(keyOutput)[0], "secret=")
	started, gate := make(chan struct{}), make(chan struct{})
	var gateOnce sync.Once
	releaseGate := func() { gateOnce.Do(func() { close(gate) }) }
	defer releaseGate()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-gate:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"completed"}`)
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	address := freeTCPAddress(t)
	configPath := writeProtectedYAML(t, dbPath, keyPath, address, upstream.URL+"/v1/responses", "1s")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config-format", "yaml", "-config", configPath}) }()
	waitProtectedReady(t, address, done)
	type responseResult struct {
		status int
		err    error
	}
	requestDone := make(chan responseResult, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, "http://"+address+"/v1/responses", strings.NewReader(`{"model":"model-a","input":"hi"}`))
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			requestDone <- responseResult{err: err}
			return
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		response.Body.Close()
		requestDone <- responseResult{status: response.StatusCode, err: readErr}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not reach gated upstream")
	}
	begin := time.Now()
	cancel()
	time.Sleep(40 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("gateway closed before draining active request: %v", err)
	default:
	}
	if err := sqliteLockHeld(dbPath); err != nil {
		t.Fatal("gateway released its process lock before the active request drained")
	}
	releaseGate()
	select {
	case result := <-requestDone:
		if result.err != nil || result.status != http.StatusOK {
			t.Fatalf("active request did not complete during drain: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("active request did not finish within drain grace")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("shutdown did not respect its bounded drain")
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("shutdown exceeded bounded drain+close allowance: %s", elapsed)
	}
	lock, err := sqlite.AcquireProcessLock(dbPath)
	if err != nil {
		t.Fatalf("shutdown retained process lock: %v", err)
	}
	_ = lock.Close()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("shutdown left database unusable: %v", err)
	}
	_ = db.Close()
}

func TestProtectedShutdownTimeoutCancelsRequestAndClosesResources(t *testing.T) {
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	_, dbPath, keyPath := protectedFixture(t)
	keyOutput, err := runAdminTest(t, dbPath, keyPath, nil, "key", "create", "--policy", "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.TrimPrefix(strings.Fields(keyOutput)[0], "secret=")
	gate := make(chan struct{})
	upstream := fakeupstream.New(fakeupstream.Response{Status: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"status":"completed"}`), HeaderGate: gate})
	defer upstream.Close()
	address := freeTCPAddress(t)
	configPath := writeProtectedYAML(t, dbPath, keyPath, address, upstream.URL+"/v1/responses", "150ms")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config-format", "yaml", "-config", configPath}) }()
	waitProtectedReady(t, address, done)
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		req, _ := http.NewRequest(http.MethodPost, "http://"+address+"/v1/responses", strings.NewReader(`{"model":"model-a","input":"hi"}`))
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()
	captured := receiveRequest(t, upstream)
	cancel()
	select {
	case <-captured.Cancelled:
	case <-time.After(time.Second):
		close(gate)
		t.Fatal("protected shutdown deadline did not cancel upstream work")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expired protected drain returned %v, want deadline exceeded", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("protected shutdown exceeded bounded drain/close grace")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("protected client request remained active after timeout")
	}
	lock, err := sqlite.AcquireProcessLock(dbPath)
	if err != nil {
		t.Fatalf("timeout shutdown retained database lock: %v", err)
	}
	_ = lock.Close()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("timeout shutdown left database unusable: %v", err)
	}
	_ = db.Close()
}

func sqliteLockHeld(path string) error {
	lock, err := sqlite.AcquireProcessLock(path)
	if err != nil {
		return nil
	}
	_ = lock.Close()
	return fmt.Errorf("gateway did not retain process lock after readiness")
}

func freeTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}

func writeProtectedYAML(t *testing.T, dbPath, keyPath, listen, endpoint, timeout string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	yaml := fmt.Sprintf(`version: 1
server: {listen: %q, max_request_bytes: 1048576, shutdown_timeout: %s}
storage: {driver: sqlite, path: %q}
secrets: {master_key_file: %q}
connectors:
  - id: upstream
    kind: connector
    implementation: pestiroute.responses.native
    protocols: [openai.responses.v1]
    settings: {base_url: %q, upstream_protocol: openai.responses.v1, mode: native, credential_env: PROTECTED_TEST_CREDENTIAL, max_request_body_bytes: 1048576, max_request_header_bytes: 8192, connect_timeout: 1s, tls_handshake_timeout: 1s, response_header_timeout: 1s, stream_idle_timeout: 1s}
routes:
  - id: route-a
    protocol: openai.responses.v1
    mode: native
    model: model-a
    adapter: pestiroute.responses.native
    policy: standard
    budget: {unknown_estimate: reserve, conservative_tokens: 10}
    targets: [{connector: upstream, account: account-a}]
policies: {standard: policy-id-a}
`, listen, timeout, dbPath, keyPath, endpoint)
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitProtectedReady(t *testing.T, address string, done <-chan error) {
	t.Helper()
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 100 * time.Millisecond, Transport: transport}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get("http://" + address + "/readyz")
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case err := <-done:
			t.Fatalf("protected gateway exited before readiness: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("protected gateway did not become ready")
}

func cancelProtected(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("protected shutdown did not finish")
	}
}

func buildGatewayBinary(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "pestiroute")
	cmd := exec.Command("go", "build", "-o", binary, ".")
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build gateway CLI: %v: %s", err, output)
	}
	return binary
}

func runAdminProcess(t *testing.T, binary, dbPath, keyPath string, args ...string) (string, error) {
	t.Helper()
	commandArgs := []string{"admin", "--db", dbPath, "--master-key", keyPath}
	commandArgs = append(commandArgs, args...)
	cmd := exec.Command(binary, commandArgs...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}
