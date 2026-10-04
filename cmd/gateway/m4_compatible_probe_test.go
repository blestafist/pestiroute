package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	adapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	anthropic "github.com/blestafist/pestiroute/internal/connector/anthropic"
	responses "github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

const (
	m4CompatModel   = "cc/claude-sonnet-5-5"
	m4CompatBaseURL = "https://ai.pestit.pl/v1"
	m4CompatMax     = 256
	m4CompatTimeout = 30 * time.Second
)

func m4CompatMessagesEndpoint(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "ai.pestit.pl") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("endpoint_authority")
	}
	p := u.EscapedPath()
	if strings.Contains(p, "%") || strings.Contains(p, "..") || (p != "/v1" && p != "/v1/messages") {
		return "", errors.New("endpoint_path")
	}
	u.Path, u.RawPath = "/v1/messages", ""
	return u.String(), nil
}

// m4CompatOverrideBackendModel is harness-only; production translation remains pinned.
func m4CompatOverrideBackendModel(body []byte) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, errors.New("translated_json")
	}
	var model string
	if err := json.Unmarshal(payload["model"], &model); err != nil || model != "claude-opus-5-5" {
		return nil, errors.New("translated_model")
	}
	encoded, _ := json.Marshal(m4CompatModel)
	payload["model"] = encoded
	return json.Marshal(payload)
}

func m4CompatReadHandoff(path string, max int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0600 {
		return nil, errors.New("handoff_metadata")
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() || before.Size() == 0 || before.Size() > max {
		return nil, errors.New("handoff_metadata")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("handoff_open")
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Mode().Perm() != 0600 {
		return nil, errors.New("handoff_changed")
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || len(data) == 0 || int64(len(data)) > max {
		return nil, errors.New("handoff_read")
	}
	return bytes.TrimSpace(data), nil
}

func TestM4CompatibleURLValidation(t *testing.T) {
	for _, tc := range []struct {
		base, want string
		bad        bool
	}{
		{m4CompatBaseURL, "https://ai.pestit.pl/v1/messages", false},
		{"https://ai.pestit.pl/v1/messages", "https://ai.pestit.pl/v1/messages", false},
		{"https://other.example/v1", "", true},
		{"https://ai.pestit.pl/v2", "", true},
		{"https://ai.pestit.pl/v1?x=1", "", true},
		{"https://user@ai.pestit.pl/v1", "", true},
		{"http://ai.pestit.pl/v1", "", true},
		{"https://ai.pestit.pl/v1/../v1", "", true},
	} {
		got, err := m4CompatMessagesEndpoint(tc.base)
		if tc.bad && err == nil || !tc.bad && (err != nil || got != tc.want) {
			t.Errorf("endpoint validation mismatch for fixture class bad=%t", tc.bad)
		}
	}
}

type m4CompatCallBudget struct{ calls atomic.Int32 }

func (b *m4CompatCallBudget) take() bool {
	for {
		current := b.calls.Load()
		if current >= 2 {
			return false
		}
		if b.calls.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func TestM4CompatibleProbeBoundsAndRedaction(t *testing.T) {
	if m4CompatMax != 256 || m4CompatTimeout != 30*time.Second || m4CompatModel != "cc/claude-sonnet-5-5" {
		t.Fatal("probe_profile_bounds_changed")
	}
	budget := &m4CompatCallBudget{}
	first, second, third := budget.take(), budget.take(), budget.take()
	if !first || !second || third || budget.calls.Load() != 2 {
		t.Fatal("provider_call_cap_not_enforced")
	}
	updated, err := m4CompatOverrideBackendModel([]byte(`{"model":"claude-opus-5-5","max_tokens":256}`))
	if err != nil || !bytes.Contains(updated, []byte(`"model":"cc/claude-sonnet-5-5"`)) {
		t.Fatal("compatible_model_override_failed")
	}
	if _, err := m4CompatOverrideBackendModel([]byte(`{"model":"wrong-model"}`)); err == nil {
		t.Fatal("unexpected_source_model_accepted")
	}
	dir := t.TempDir()
	if err := m4CompatWriteArtifacts(dir, http.StatusOK, http.StatusOK); err != nil {
		t.Fatal("artifact_fixture_write")
	}
	if fixtureDir := os.Getenv("PESTIROUTE_M4_COMPAT_ARTIFACT_FIXTURE_DIR"); fixtureDir != "" {
		if err := m4CompatWriteArtifacts(fixtureDir, http.StatusOK, http.StatusOK); err != nil {
			t.Fatal("artifact_fixture_export")
		}
	}
	for _, name := range []string{"direct.json", "gateway.json"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal("artifact_fixture_read")
		}
		text := string(data)
		if !strings.Contains(text, `"endpoint_profile": "compatible_endpoint"`) || !strings.Contains(text, `"test_only_model_override": true`) || !strings.Contains(text, m4CompatModel) {
			t.Fatalf("artifact_profile_or_model_missing: %s", name)
		}
		for _, forbidden := range []string{"x-api-key", "Authorization", "Bearer ", "synthetic-anthropic-key", "prompt"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("artifact_redaction_failed: %s", name)
			}
		}
	}
}

func TestM4CompatibleHandoffMetadataValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "handoff")
	if err := os.WriteFile(path, []byte("fixture-value\n"), 0600); err != nil {
		t.Fatal("handoff_fixture_write")
	}
	value, err := m4CompatReadHandoff(path, 128)
	if err != nil || string(value) != "fixture-value" {
		t.Fatal("secure_handoff_read_failed")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal("handoff_fixture_chmod")
	}
	if _, err := m4CompatReadHandoff(path, 128); err == nil {
		t.Fatal("handoff_permissive_mode_accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal("handoff_fixture_chmod")
	}
	link := filepath.Join(dir, "symlink")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal("handoff_fixture_symlink")
	}
	if _, err := m4CompatReadHandoff(link, 128); err == nil {
		t.Fatal("handoff_symlink_accepted")
	}
}

type m4CompatRoundTrip func(*http.Request) (*http.Response, error)

func (f m4CompatRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestM4CompatibleDirectRequestSingleAttempt(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: m4CompatRoundTrip(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		var body struct {
			Model  string `json:"model"`
			Max    int    `json:"max_tokens"`
			Stream bool   `json:"stream"`
		}
		if req.GetBody != nil || req.Header.Get("x-api-key") != "synthetic-only" || json.NewDecoder(req.Body).Decode(&body) != nil || body.Model != m4CompatModel || body.Max != m4CompatMax || !body.Stream {
			t.Error("direct_request_contract_mismatch")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("event: message_stop\ndata: {}\n\n")), Header: make(http.Header), Request: req}, nil
	})}
	budget := &m4CompatCallBudget{}
	status, terminal := m4CompatDirectWithClient(t, client, "https://ai.pestit.pl/v1/messages", "synthetic-only", budget)
	if status != http.StatusOK || !terminal || calls.Load() != 1 || budget.calls.Load() != 1 {
		t.Fatal("direct_single_attempt_fixture_failed")
	}
}

func TestM4CompatibleGatewaySingleAttempt(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Model  string `json:"model"`
			Max    int    `json:"max_tokens"`
			Stream bool   `json:"stream"`
		}
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "synthetic-only" || json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != m4CompatModel || body.Max != m4CompatMax || !body.Stream {
			t.Error("gateway_translation_contract_mismatch")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"smoke-ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer backend.Close()
	budget := &m4CompatCallBudget{}
	status, terminal, upstreamStatus := m4CompatGateway(t, backend.URL+"/v1/messages", "synthetic-only", budget)
	if status != http.StatusOK || upstreamStatus != http.StatusOK || !terminal || calls.Load() != 1 || budget.calls.Load() != 1 {
		t.Fatal("gateway_single_attempt_fixture_failed")
	}
}

func TestM4CompatibleLiveProbe(t *testing.T) {
	if os.Getenv("PESTIROUTE_M4_COMPAT_LIVE") != "1" {
		t.Skip("live probe requires explicit PESTIROUTE_M4_COMPAT_LIVE=1")
	}
	baseBytes, err := m4CompatReadHandoff("/tmp/opencode/anthropic-base-url", 2048)
	if err != nil {
		t.Fatal("handoff_base_metadata")
	}
	endpoint, err := m4CompatMessagesEndpoint(string(baseBytes))
	if err != nil || string(baseBytes) != m4CompatBaseURL {
		t.Fatal("handoff_base_rejected")
	}
	key, err := m4CompatReadHandoff("/tmp/opencode/anthropic-api-key", 16384)
	if err != nil || len(key) == 0 {
		t.Fatal("handoff_credential_rejected")
	}

	budget := &m4CompatCallBudget{}
	directStatus, directTerminal := m4CompatDirect(t, endpoint, string(key), budget)
	if !directTerminal {
		t.Fatalf("direct_stream_terminal_missing http_status=%d", directStatus)
	}
	gatewayStatus, gatewayTerminal, upstreamStatus := m4CompatGateway(t, endpoint, string(key), budget)
	if !gatewayTerminal {
		t.Fatalf("gateway_stream_terminal_missing http_status=%d provider_http_status=%d", gatewayStatus, upstreamStatus)
	}
	if dir := os.Getenv("PESTIROUTE_SMOKE_M4_ARTIFACT_DIR"); dir != "" {
		if err := m4CompatWriteArtifacts(dir, directStatus, gatewayStatus); err != nil {
			t.Fatal("artifact_write_failed")
		}
	}
	t.Logf("compatible_probe complete direct_status=%d gateway_status=%d provider_calls=%d retries=0", directStatus, gatewayStatus, budget.calls.Load())
}

func m4CompatDirect(t *testing.T, endpoint, key string, budget *m4CompatCallBudget) (int, bool) {
	t.Helper()
	client, tr := m4CompatHTTPClient(nil)
	defer tr.CloseIdleConnections()
	return m4CompatDirectWithClient(t, client, endpoint, key, budget)
}

func m4CompatHTTPClient(base *http.Transport) (*http.Client, *http.Transport) {
	if base == nil {
		base = http.DefaultTransport.(*http.Transport)
	}
	tr := base.Clone()
	tr.Proxy = nil
	// Leave protocol negotiation paired with the transport's HTTP/2 handlers.
	client := &http.Client{Transport: tr, Timeout: m4CompatTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return client, tr
}

func m4CompatDirectWithClient(t *testing.T, client *http.Client, endpoint, key string, budget *m4CompatCallBudget) (int, bool) {
	t.Helper()
	if !budget.take() {
		t.Fatal("provider_call_cap")
	}
	ctx, cancel := context.WithTimeout(context.Background(), m4CompatTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"model": m4CompatModel, "max_tokens": m4CompatMax, "stream": true, "temperature": 0, "messages": []any{map[string]string{"role": "user", "content": "Reply exactly: smoke-ok."}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal("direct_request_build")
	}
	req.GetBody = nil
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream")
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(m4CompatTransportDiagnostic(err, time.Since(started)))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("direct_http_status=%d", resp.StatusCode)
	}
	return resp.StatusCode, m4CompatHasEvent(resp.Body, "message_stop")
}

type m4CompatTransportClassification struct {
	stage    string
	category string
}

func m4CompatTransportDiagnostic(err error, elapsed time.Duration) string {
	detail := m4CompatTransportDetail(err)
	return fmt.Sprintf("direct_transport_failure stage=%s category=%s duration_ms=%d", detail.stage, detail.category, elapsed.Milliseconds())
}

func m4CompatTransportDetail(err error) m4CompatTransportClassification {
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var record *tls.RecordHeaderError
	var alert tls.AlertError
	var op *net.OpError
	if strings.Contains(err.Error(), "malformed HTTP response") {
		return m4CompatTransportClassification{"response", "malformed_http"}
	}
	if errors.As(err, &dns) {
		return m4CompatTransportClassification{"dns", "dns_resolution"}
	}
	if errors.As(err, &cert) {
		return m4CompatTransportClassification{"tls_handshake", "tls_certificate"}
	}
	if errors.As(err, &record) || errors.As(err, &alert) {
		return m4CompatTransportClassification{"tls_handshake", "tls_handshake"}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return m4CompatTransportClassification{"response", "unexpected_eof"}
	}
	if errors.As(err, &op) {
		switch op.Op {
		case "dial", "connect":
			if errors.Is(err, syscall.ECONNREFUSED) {
				return m4CompatTransportClassification{"connect", "connection_refused"}
			}
			if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) {
				return m4CompatTransportClassification{"connect", "network_unreachable"}
			}
			if m4CompatTimeoutError(err) {
				return m4CompatTransportClassification{"connect", "timeout"}
			}
			return m4CompatTransportClassification{"connect", "network_error"}
		case "read":
			if m4CompatTimeoutError(err) {
				return m4CompatTransportClassification{"read", "timeout"}
			}
			return m4CompatTransportClassification{"read", "network_error"}
		case "write":
			if m4CompatTimeoutError(err) {
				return m4CompatTransportClassification{"write", "timeout"}
			}
			return m4CompatTransportClassification{"write", "network_error"}
		}
	}
	if m4CompatTimeoutError(err) {
		return m4CompatTransportClassification{"request", "timeout"}
	}
	return m4CompatTransportClassification{"request", "network_error"}
}

func m4CompatTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func TestM4CompatibleTransportErrorClassification(t *testing.T) {
	const private = "PRIVATE_MARKER https://secret.example/path?token=not-for-logs"
	tests := []struct {
		name, stage, category string
		err                   error
	}{
		{"dns", "dns", "dns_resolution", &net.DNSError{Err: private, Name: private}},
		{"refused", "connect", "connection_refused", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}},
		{"unreachable", "connect", "network_unreachable", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ENETUNREACH}},
		{"connect_timeout", "connect", "timeout", &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}},
		{"read_timeout", "read", "timeout", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}},
		{"tls_certificate", "tls_handshake", "tls_certificate", &tls.CertificateVerificationError{Err: errors.New(private)}},
		{"tls_handshake", "tls_handshake", "tls_handshake", tls.AlertError(40)},
		{"malformed_response", "response", "malformed_http", errors.New("malformed HTTP response " + private)},
		{"unexpected_eof", "response", "unexpected_eof", io.ErrUnexpectedEOF},
		{"unknown", "request", "network_error", errors.New(private)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := &url.Error{Op: "POST", URL: private, Err: tc.err}
			got := m4CompatTransportDetail(err)
			if got.stage != tc.stage || got.category != tc.category {
				t.Fatalf("unexpected classification stage=%s category=%s", got.stage, got.category)
			}
			message := m4CompatTransportDiagnostic(err, 123*time.Millisecond)
			if strings.Contains(message, private) || strings.Contains(message, "secret.example") || strings.Contains(message, "not-for-logs") {
				t.Fatal("private transport details leaked")
			}
			if !strings.Contains(message, "stage="+tc.stage) || !strings.Contains(message, "category="+tc.category) || !strings.Contains(message, "duration_ms=123") {
				t.Fatal("safe diagnostic fields missing")
			}
		})
	}
}

func TestM4CompatibleHTTP2TransportDirectAndGateway(t *testing.T) {
	var hits atomic.Int32
	var protocolMismatch atomic.Bool
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			protocolMismatch.Store(true)
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"smoke-ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	backend.EnableHTTP2 = true
	backend.StartTLS()
	defer backend.Close()

	base, ok := backend.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("local_tls_transport_unavailable")
	}
	client, transport := m4CompatHTTPClient(base)
	defer transport.CloseIdleConnections()
	budget := &m4CompatCallBudget{}
	if status, terminal := m4CompatDirectWithClient(t, client, backend.URL+"/v1/messages", "synthetic-offline-only", budget); status != http.StatusOK || !terminal {
		t.Fatal("direct_http2_fixture_failed")
	}
	status, terminal, upstreamStatus := m4CompatGatewayWithProviderClient(t, backend.URL+"/v1/messages", "synthetic-offline-only", budget, client)
	if status != http.StatusOK || upstreamStatus != http.StatusOK || !terminal {
		t.Fatal("gateway_http2_fixture_failed")
	}
	if hits.Load() != 2 || budget.calls.Load() != 2 || protocolMismatch.Load() {
		t.Fatalf("http2_fixture_invariant_failed hits=%d calls=%d protocol_mismatch=%t", hits.Load(), budget.calls.Load(), protocolMismatch.Load())
	}
}

func m4CompatGateway(t *testing.T, endpoint, key string, budget *m4CompatCallBudget) (int, bool, int) {
	t.Helper()
	client, transport := m4CompatHTTPClient(nil)
	defer transport.CloseIdleConnections()
	return m4CompatGatewayWithProviderClient(t, endpoint, key, budget, client)
}

func m4CompatGatewayWithProviderClient(t *testing.T, endpoint, key string, budget *m4CompatCallBudget, providerClient *http.Client) (int, bool, int) {
	t.Helper()
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic-offline-only")
	if !budget.take() {
		t.Fatal("provider_call_cap")
	}
	ctx := context.Background()
	_, dbPath, keyPath := protectedFixture(t)
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal("gateway_database_open")
	}
	account := sqlite.Account{ID: "compat-anthropic", Connector: "anthropic", Enabled: true}
	if _, err := sqlite.NewAccounts(db).Create(ctx, account); err != nil {
		t.Fatal("gateway_account_create")
	}
	master, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal("gateway_master_key")
	}
	envelope, err := secure.Seal(master, 2, "test-v1", "credentials", "compat-credential", account.ID, []byte(key))
	if err != nil {
		t.Fatal("gateway_credential_seal")
	}
	if _, err = sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "compat-credential", AccountID: account.ID, FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext}); err != nil {
		t.Fatal("gateway_credential_store")
	}
	policies := sqlite.NewKeyPolicies(db)
	policy, err := policies.GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal("gateway_policy_read")
	}
	policy, err = policies.Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{"model-a", m4CompatModel}, Connectors: []string{"upstream", "anthropic"}, RPM: 2, TPM: 100000})
	if err != nil {
		t.Fatal("gateway_policy_update")
	}
	issued, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal("gateway_key_create")
	}
	if err := db.Close(); err != nil {
		t.Fatal("gateway_database_close")
	}

	parsed, _ := url.Parse(endpoint)
	var calls atomic.Int32
	var upstreamStatus atomic.Int32
	providerDoer := anthropicDoerFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) != 1 || req.GetBody != nil {
			return nil, errors.New("provider_call_cap")
		}
		var translated struct {
			Model  string `json:"model"`
			Max    int    `json:"max_tokens"`
			Stream bool   `json:"stream"`
		}
		data, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
		decodeErr := json.Unmarshal(data, &translated)
		credentialMatch := req.Header.Get("x-api-key") == key
		if err != nil || decodeErr != nil || translated.Model != "claude-opus-5-5" || translated.Max != m4CompatMax || !translated.Stream || !credentialMatch {
			t.Errorf("translated_request_validation read_ok=%t json_ok=%t expected_source_model=%t token_cap_match=%t stream=%t credential_match=%t", err == nil, decodeErr == nil, translated.Model == "claude-opus-5-5", translated.Max == m4CompatMax, translated.Stream, credentialMatch)
			return nil, errors.New("translated_request_bounds")
		}
		data, err = m4CompatOverrideBackendModel(data)
		if err != nil {
			return nil, errors.New("compatible_model_override")
		}
		copy := req.Clone(req.Context())
		copy.URL = parsed
		copy.Host = parsed.Host
		copy.Body = io.NopCloser(bytes.NewReader(data))
		copy.GetBody = nil
		copy.ContentLength = int64(len(data))
		resp, err := providerClient.Do(copy)
		if err == nil {
			upstreamStatus.Store(int32(resp.StatusCode))
		}
		return resp, err
	})
	base := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0").protected
	base.Connectors = append(base.Connectors, protectedConnector{ID: "anthropic", Kind: "connector", Implementation: "pestiroute.anthropic.messages", Settings: nativeSettings{Model: m4CompatModel, AccountID: account.ID, CredentialID: "compat-credential"}})
	base.Routes = append(base.Routes, protectedRoute{ID: "compat-route", Protocol: responsesProtocol, Mode: "translation", Model: m4CompatModel, Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(4096)}, Targets: []routeTarget{{Connector: "anthropic", Account: account.ID}}})
	base.Policies["standard"] = policy.ID
	prepared, err := prepareProtectedConfig(ctx, config{protected: base, DatabasePath: dbPath, MasterKeyFile: keyPath})
	if err != nil {
		t.Fatal("gateway_config_prepare")
	}
	ready, draining := atomic.Bool{}, atomic.Bool{}
	ready.Store(true)
	h, closeComponents, err := composeHandlerWithFactory(prepared, &ready, &draining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		if item.Implementation == "pestiroute.anthropic.messages" {
			return anthropicConnectorWithDoer{Connector: anthropic.NewConnector(), doer: providerDoer}
		}
		return responses.NewConnector()
	})
	if err != nil {
		t.Fatal("gateway_compose")
	}
	t.Cleanup(func() { _ = closeComponents(context.Background()) })
	server := httptest.NewServer(h)
	defer server.Close()
	requestCtx, cancel := context.WithTimeout(context.Background(), m4CompatTimeout)
	defer cancel()
	requestBody, _ := json.Marshal(map[string]any{"model": m4CompatModel, "stream": true, "max_output_tokens": m4CompatMax, "input": "Reply exactly: smoke-ok."})
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal("gateway_request_build")
	}
	req.Header.Set("Authorization", "Bearer "+issued.Secret)
	req.Header.Set("Content-Type", "application/json")
	client := server.Client()
	client.Timeout = m4CompatTimeout
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("gateway_transport_failure category=%T", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway_http_status=%d provider_http_status=%d provider_calls=%d", resp.StatusCode, upstreamStatus.Load(), calls.Load())
	}
	terminal := m4CompatHasEvent(resp.Body, "response.completed")
	if calls.Load() != 1 {
		t.Fatalf("gateway_provider_call_count=%d", calls.Load())
	}
	return resp.StatusCode, terminal, int(upstreamStatus.Load())
}

func m4CompatHasEvent(body io.Reader, event string) bool {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	found := false
	for scanner.Scan() {
		if scanner.Text() == "event: "+event {
			found = true
		}
	}
	return found && scanner.Err() == nil
}

func m4CompatWriteArtifacts(dir string, directStatus, gatewayStatus int) error {
	if directStatus != http.StatusOK || gatewayStatus != http.StatusOK {
		return errors.New("non_success")
	}
	source, err := os.ReadFile("m4_compatible_probe_test.go")
	if err != nil {
		return err
	}
	digest := sha256.Sum256(source)
	revision := hex.EncodeToString(digest[:])
	stamp := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	client := map[string]string{"name": "Go http.Client", "version": runtime.Version()}
	limits := map[string]int{"timeout_seconds": 30, "max_output_tokens": m4CompatMax, "client_runs": 1}
	provider := map[string]string{"api": "anthropic-messages", "api_version": "2023-06-01"}
	observations := map[string]any{"terminal": "message_stop", "tool_links": []string{}, "usage": "unknown", "target_dispatches": 1}
	direct := map[string]any{"schema_version": 1, "leg": "direct_messages", "endpoint_profile": "compatible_endpoint", "test_only_model_override": true, "captured_at_utc": stamp, "source_revision": revision, "client": client, "gateway": nil, "provider": provider, "scenario": "plain_text", "fixture_id": "m4-040-plain-text-v1", "backend_model": m4CompatModel, "limits": limits, "requests": []map[string]any{{"run": 1, "attempt": 1, "timeout_seconds": 30, "max_output_tokens": m4CompatMax, "status": directStatus, "stream": true, "api_path": "/v1/messages"}}, "outcome": "completed", "observations": observations}
	gwObservations := map[string]any{"terminal": "response.completed", "tool_links": []string{}, "usage": "unknown", "target_dispatches": 1}
	gateway := map[string]any{"schema_version": 1, "leg": "gateway_responses_to_messages", "endpoint_profile": "compatible_endpoint", "test_only_model_override": true, "captured_at_utc": stamp, "source_revision": revision, "client": client, "gateway": map[string]string{"client_protocol": responsesProtocol, "route_model": m4CompatModel, "source_revision": revision}, "provider": provider, "scenario": "plain_text", "fixture_id": "m4-040-plain-text-v1", "backend_model": m4CompatModel, "limits": limits, "requests": []map[string]any{{"run": 1, "attempt": 1, "timeout_seconds": 30, "max_output_tokens": m4CompatMax, "status": gatewayStatus, "stream": true, "api_path": "/v1/responses"}}, "outcome": "completed", "observations": gwObservations}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for name, doc := range map[string]any{"direct.json": direct, "gateway.json": gateway} {
		encoded, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), append(encoded, '\n'), 0600); err != nil {
			return err
		}
	}
	return nil
}
