package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
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
	"github.com/blestafist/pestiroute/internal/connector/codex"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
	"github.com/google/uuid"
)

const (
	operatorPairModel      = "gpt-6-luna"
	operatorPairProfile    = "codex-responses-http-sse-lite-v1"
	operatorPairThreadID   = "b8b20d25-3b76-43cc-a561-861c95cf32d1"
	operatorPairPath       = "/tmp/opencode/pestiroute-live-evidence"
	operatorPairBodyLimit  = 1 << 20
	operatorPairTextLimit  = 256
	operatorPairTimeout    = 30 * time.Second
	operatorPairDirectPath = "/backend-api/codex/responses"
)

var operatorPairRequest = []byte(`{"model":"gpt-6-luna","stream":true,"store":false,"instructions":"","reasoning":{"effort":"high","context":"all_turns"},"include":["reasoning.encrypted_content"],"input":[{"type":"additional_tools","id":"at_48517110-6418-52c6-9cd2-cd8b64e6037d","role":"developer","tools":[]},{"type":"message","role":"user","content":[{"type":"input_text","text":"What is 137 × 293? Reply with only the number."}]}],"tool_choice":"auto","parallel_tool_calls":false}`)

type operatorPairCredential struct {
	Version     int       `json:"version"`
	AccessToken string    `json:"access_token"`
	AccountID   string    `json:"account_id"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type operatorPairObservation struct {
	status         int
	eventOrder     []string
	created        int
	deltas         int
	completed      int
	reasoningItems int
	outputBytes    int
	usage          string
	terminal       string
	bodyBytes      int
	earlyDelivery  bool
	reason         string
	errorCode      string
	errorField     string
	bodyReadResult string
	transport      string
}

type operatorPairDispatchCounter struct{ dispatches atomic.Int32 }

func instrumentOperatorPairClient(connector *codex.Connector, counter *operatorPairDispatchCounter) core.HTTPDoer {
	client, _ := connector.HTTPDoer().(*http.Client)
	if client == nil {
		return nil
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		return nil
	}
	transport = transport.Clone()
	dial := transport.DialContext
	if dial == nil {
		dial = (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		counter.dispatches.Add(1)
		return dial(ctx, network, address)
	}
	client.Transport = transport
	return client
}

type operatorPairArtifact struct {
	SchemaVersion int               `json:"schema_version"`
	Leg           string            `json:"leg"`
	Profile       string            `json:"endpoint_profile"`
	CapturedAt    string            `json:"captured_at_utc"`
	Client        map[string]string `json:"client"`
	Model         string            `json:"model"`
	Scenario      string            `json:"scenario"`
	FixtureID     string            `json:"fixture_id"`
	Limits        map[string]int    `json:"limits"`
	Requests      []map[string]any  `json:"requests"`
	Outcome       string            `json:"outcome"`
	Observations  map[string]any    `json:"observations"`
}

type operatorPairFailureArtifact struct {
	SchemaVersion int    `json:"schema_version"`
	Leg           string `json:"leg"`
	Profile       string `json:"endpoint_profile"`
	CapturedAt    string `json:"captured_at_utc"`
	Model         string `json:"model"`
	Status        *int   `json:"status"`
	Reason        string `json:"reason"`
	Code          string `json:"code"`
	Field         string `json:"field"`
	BodyRead      string `json:"body_read_result"`
	Transport     string `json:"transport"`
}

// TestOperatorPairedInference is opt-in and makes at most one Lite-profile
// direct inference POST, followed by at most one protected gateway POST.
func TestOperatorPairedInference(t *testing.T) {
	dbPath := os.Getenv("PESTIROUTE_DIAG_DB")
	keyPath := os.Getenv("PESTIROUTE_DIAG_KEY")
	accountAlias := os.Getenv("PESTIROUTE_DIAG_ACCOUNT")
	directArtifact := os.Getenv("PESTIROUTE_PAIR_DIRECT_RESULT")
	gatewayArtifact := os.Getenv("PESTIROUTE_PAIR_GATEWAY_RESULT")
	if dbPath == "" || keyPath == "" || accountAlias == "" || directArtifact == "" || gatewayArtifact == "" {
		t.Skip("operator paired inference requires explicit PESTIROUTE_DIAG_* and PESTIROUTE_PAIR_* settings")
	}
	if accountAlias != "selected-A" || validateOperatorPairArtifactPaths(directArtifact, gatewayArtifact) != nil {
		t.Fatal("operator paired inference checkpoint failed")
	}
	if err := prepareOperatorPairArtifactDirectory(filepath.Dir(directArtifact)); err != nil || filepath.Dir(directArtifact) != filepath.Dir(gatewayArtifact) {
		t.Fatal("operator paired inference artifact destination unavailable")
	}
	if _, err := os.Lstat(directArtifact); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("direct artifact destination already exists or is unavailable")
	}
	if _, err := os.Lstat(gatewayArtifact); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("gateway artifact destination already exists or is unavailable")
	}
	directFailureArtifact := strings.TrimSuffix(directArtifact, ".json") + "-failure.json"
	gatewayFailureArtifact := strings.TrimSuffix(gatewayArtifact, ".json") + "-failure.json"
	for _, path := range []string{directFailureArtifact, gatewayFailureArtifact} {
		if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failure artifact destination already exists or is unavailable")
		}
	}

	account, credential, providerAccountID, bearer := loadOperatorPairCredential(t, dbPath, keyPath, accountAlias)
	direct, err := runOperatorPairDirect(t, providerAccountID, bearer)
	bearer = ""
	if err != nil {
		if writeOperatorPairFailureArtifact(directFailureArtifact, "direct_codex", direct) != nil {
			t.Fatal("direct leg stopped; sanitized failure capture could not be persisted")
		}
		t.Fatalf("direct leg stopped safely: status=%d reason=%s code=%s field=%s body_read=%s transport=%s", direct.status, direct.reason, direct.errorCode, direct.errorField, direct.bodyReadResult, direct.transport)
	}
	if direct.status != http.StatusOK || direct.terminal != "completed" {
		t.Fatalf("direct leg stopped safely: status=%d terminal=%s", direct.status, direct.terminal)
	}

	dbPathCopy, issued := provisionOperatorPairGateway(t, account, credential)
	connector := codex.NewConnector()
	counting := &operatorPairDispatchCounter{}
	doer := instrumentOperatorPairClient(connector, counting)
	defer connector.Close(context.Background())
	server, closeGateway := startOperatorPairGateway(t, dbPathCopy, keyPath, account.ID, connector, doer)
	defer closeGateway()
	gateway, err := runOperatorPairGateway(t, server.URL, issued.Secret)
	if err != nil {
		if writeOperatorPairFailureArtifact(gatewayFailureArtifact, "gateway_responses_to_codex", gateway) != nil {
			t.Fatal("gateway leg stopped; sanitized failure capture could not be persisted")
		}
		t.Fatalf("gateway leg stopped safely: status=%d reason=%s code=%s field=%s body_read=%s transport=%s", gateway.status, gateway.reason, gateway.errorCode, gateway.errorField, gateway.bodyReadResult, gateway.transport)
	}
	if gateway.status != http.StatusOK || gateway.terminal != "completed" {
		t.Fatalf("gateway leg stopped safely: status=%d terminal=%s", gateway.status, gateway.terminal)
	}
	if counting.dispatches.Load() != 1 {
		t.Fatalf("gateway dispatch count mismatch: target_dispatches=%d", counting.dispatches.Load())
	}
	if direct.usage != gateway.usage || direct.reasoningItems == 0 || direct.reasoningItems != gateway.reasoningItems || !bytes.Equal([]byte(strings.Join(direct.eventOrder, "\n")), []byte(strings.Join(gateway.eventOrder, "\n"))) {
		t.Fatalf("pair observations differ: direct_usage=%s gateway_usage=%s direct_events=%v gateway_events=%v", direct.usage, gateway.usage, direct.eventOrder, gateway.eventOrder)
	}
	if len(direct.eventOrder) < 3 || len(gateway.eventOrder) < 3 || direct.created != 1 || gateway.created != 1 || direct.completed != 1 || gateway.completed != 1 || direct.deltas < 1 || gateway.deltas < 1 {
		t.Fatalf("strict artifact event sequence mismatch: direct created/delta/completed=%d/%d/%d gateway=%d/%d/%d", direct.created, direct.deltas, direct.completed, gateway.created, gateway.deltas, gateway.completed)
	}

	directRecord := newOperatorPairArtifact("direct_codex", operatorPairDirectPath, direct)
	gatewayRecord := newOperatorPairArtifact("gateway_responses_to_codex", "/v1/responses", gateway)
	if err := writeOperatorPairArtifact(directArtifact, directRecord); err != nil {
		t.Fatal("direct sanitized artifact write failed")
	}
	if err := writeOperatorPairArtifact(gatewayArtifact, gatewayRecord); err != nil {
		_ = os.Remove(directArtifact)
		t.Fatal("gateway sanitized artifact write failed")
	}
	t.Logf("pair complete: direct_status=%d gateway_status=%d direct_dispatches=1 gateway_dispatches=%d retries=0 direct_events=%v gateway_events=%v direct_usage=%s gateway_usage=%s direct_output_bytes=%d gateway_output_bytes=%d client=Go-http.Client runtime=%s", direct.status, gateway.status, counting.dispatches.Load(), direct.eventOrder, gateway.eventOrder, direct.usage, gateway.usage, direct.outputBytes, gateway.outputBytes, runtime.Version())
}

func TestOperatorPairCredentialCheckpoint(t *testing.T) {
	dbPath, keyPath, accountAlias := os.Getenv("PESTIROUTE_DIAG_DB"), os.Getenv("PESTIROUTE_DIAG_KEY"), os.Getenv("PESTIROUTE_DIAG_ACCOUNT")
	if dbPath == "" || keyPath == "" || accountAlias == "" {
		t.Skip("operator credential checkpoint requires explicit PESTIROUTE_DIAG_* settings")
	}
	_, _, _, _ = loadOperatorPairCredential(t, dbPath, keyPath, accountAlias)
	t.Log("selected credential decrypted in memory and validated; no provider request made")
}

func loadOperatorPairCredential(t *testing.T, dbPath, keyPath, accountAlias string) (sqlite.Account, sqlite.Credential, string, string) {
	t.Helper()
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal("credential checkpoint failed")
	}
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		t.Fatal("database checkpoint failed")
	}
	u := &url.URL{Scheme: "file", Path: abs}
	query := u.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "query_only(ON)")
	u.RawQuery = query.Encode()
	source, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal("database checkpoint failed")
	}
	source.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = source.Close() })
	ctx := context.Background()
	if err := source.PingContext(ctx); err != nil {
		t.Fatal("database checkpoint failed")
	}
	account, err := sqlite.NewAccounts(source).Get(ctx, accountAlias)
	if err != nil || !account.Enabled || account.Connector != "codex" {
		t.Fatal("account checkpoint failed")
	}
	credentials := sqlite.NewCredentials(source)
	credential, err := credentials.Get(ctx, account.ID, "oauth")
	if err != nil || credential.ExpiresAt == nil || !credential.ExpiresAt.After(time.Now()) {
		t.Fatal("credential checkpoint failed")
	}
	plain, err := credentials.GetDecrypted(ctx, account.ID, credential.ID, key)
	if err != nil {
		t.Fatal("credential checkpoint failed")
	}
	defer clear(plain)
	var bundle operatorPairCredential
	if json.Unmarshal(plain, &bundle) != nil || bundle.AccessToken == "" || bundle.AccountID == "" || strings.ContainsAny(bundle.AccountID, "\r\n") || !bundle.ExpiresAt.After(time.Now()) || bundle.ExpiresAt.UnixMilli() != credential.ExpiresAt.UnixMilli() {
		t.Fatal("credential checkpoint failed")
	}
	if !bundle.ExpiresAt.After(time.Now().Add(time.Minute + 2*operatorPairTimeout)) {
		t.Fatal("credential freshness checkpoint failed")
	}
	const isolatedAccountID = "m51-pair-isolated-account"
	envelope, err := secure.Seal(key, credential.FormatVersion, credential.KeyVersion, "credentials", credential.ID, isolatedAccountID, plain)
	if err != nil {
		t.Fatal("isolated credential envelope creation failed")
	}
	credential.AccountID, credential.Nonce, credential.Ciphertext = isolatedAccountID, envelope.Nonce, envelope.Ciphertext
	account.ID = isolatedAccountID
	return account, credential, bundle.AccountID, bundle.AccessToken
}

func runOperatorPairDirect(t *testing.T, providerAccountID, bearer string) (operatorPairObservation, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), operatorPairTimeout)
	defer cancel()
	req, err := newOperatorPairDirectRequest(ctx, "https://chatgpt.com"+operatorPairDirectPath, providerAccountID, bearer)
	if err != nil {
		return operatorPairObservation{}, errors.New("request construction failure")
	}
	connector := codex.NewConnector()
	defer connector.Close(context.Background())
	resp, err := connector.HTTPDoer().Do(req)
	if err != nil {
		return operatorPairObservation{reason: "unknown", errorCode: "other", errorField: "other", bodyReadResult: "not_observed", transport: "unknown"}, errors.New("transport failure; stopped without retry")
	}
	defer resp.Body.Close()
	result := operatorPairObservation{status: resp.StatusCode, usage: "unknown", reason: "unknown", errorCode: "other", errorField: "other", bodyReadResult: "not_observed", transport: "none"}
	if resp.StatusCode != http.StatusOK {
		observeOperatorPairFailureBody(resp, &result)
		return result, errors.New("provider rejected request; stopped without retry")
	}
	if err := readOperatorPairSSE(resp.Body, &result); err != nil {
		result.bodyReadResult = "stream_failure"
		return result, errors.New("provider stream did not complete within bounds")
	}
	return result, nil
}

func newOperatorPairDirectRequest(ctx context.Context, endpoint, providerAccountID, bearer string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(operatorPairRequest))
	if err != nil {
		return nil, err
	}
	for name, value := range map[string]string{
		"Authorization": "Bearer " + bearer, "ChatGPT-Account-Id": providerAccountID,
		"Content-Type": "application/json", "Accept": "text/event-stream", "Accept-Encoding": "identity",
		"x-openai-internal-codex-responses-lite": "true", "originator": "pestiroute", "User-Agent": "PestiRoute",
	} {
		req.Header.Set(name, value)
	}
	identity, err := uuid.NewRandom()
	if err != nil {
		return nil, errors.New("request identity unavailable")
	}
	for _, name := range []string{"session-id", "thread-id", "x-client-request-id"} {
		req.Header.Set(name, identity.String())
	}
	return req, nil
}

func runOperatorPairGateway(t *testing.T, gatewayURL, virtualKey string) (operatorPairObservation, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), operatorPairTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/v1/responses", bytes.NewReader(operatorPairRequest))
	if err != nil {
		return operatorPairObservation{}, errors.New("request construction failure")
	}
	req.Header.Set("Authorization", "Bearer "+virtualKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")
	client := &http.Client{Timeout: operatorPairTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return operatorPairObservation{reason: "unknown", errorCode: "other", errorField: "other", bodyReadResult: "not_observed", transport: "unknown"}, errors.New("gateway transport failure; stopped without retry")
	}
	defer resp.Body.Close()
	result := operatorPairObservation{status: resp.StatusCode, usage: "unknown", reason: "unknown", errorCode: "other", errorField: "other", bodyReadResult: "not_observed", transport: "none"}
	if resp.StatusCode != http.StatusOK {
		observeOperatorPairFailureBody(resp, &result)
		return result, errors.New("gateway rejected request; stopped without retry")
	}
	if err := readOperatorPairSSE(resp.Body, &result); err != nil {
		result.bodyReadResult = "stream_failure"
		return result, errors.New("gateway stream did not complete within bounds")
	}
	return result, nil
}

func observeOperatorPairFailureBody(resp *http.Response, result *operatorPairObservation) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, operatorPairBodyLimit+1))
	defer clear(body)
	if err != nil {
		result.bodyReadResult = "read_failure"
		return
	}
	if len(body) > operatorPairBodyLimit {
		result.bodyReadResult = "size_limit"
		return
	}
	result.bodyReadResult = "complete"
	result.reason, result.errorCode, result.errorField = classifyOperatorPairFailureBody(body)
}

func classifyOperatorPairFailureBody(body []byte) (reason, code, field string) {
	reason, code, field = "unknown", "other", "other"
	var payload struct {
		Detail string `json:"detail"`
		Error  struct {
			Code   string `json:"code"`
			Type   string `json:"type"`
			Param  string `json:"param"`
			Detail string `json:"detail"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return
	}
	switch payload.Error.Code {
	case "invalid_request_error", "unsupported_parameter", "unsupported_value", "model_not_found", "insufficient_quota", "rate_limit_exceeded", "server_error", "upstream_unavailable":
		code = payload.Error.Code
	}
	switch payload.Error.Param {
	case "model", "input", "instructions", "stream", "store", "max_output_tokens", "reasoning", "reasoning.effort", "reasoning.context", "include", "tool_choice", "parallel_tool_calls":
		field = payload.Error.Param
	}
	if code == "other" {
		switch payload.Error.Type {
		case "invalid_request_error", "server_error", "rate_limit_error":
			code = payload.Error.Type
		}
	}
	switch code {
	case "invalid_request_error":
		reason = "invalid_request"
	case "unsupported_parameter", "unsupported_value":
		reason = "unsupported_field"
	case "model_not_found":
		reason = "model_unsupported"
	case "insufficient_quota":
		reason = "quota_exhausted"
	case "rate_limit_exceeded":
		reason = "rate_limited"
	case "server_error":
		reason = "service_error"
	case "upstream_unavailable":
		reason = "service_error"
	case "rate_limit_error":
		reason = "rate_limited"
	}
	detail := payload.Detail
	if detail == "" {
		detail = payload.Error.Detail
	}
	if detailReason, detailCode, detailField := classifyOperatorPairDetail(detail); detailReason != "unknown" {
		reason = detailReason
		if code == "other" {
			code = detailCode
		}
		if payload.Error.Param == "" && field == "other" {
			field = detailField
		}
	}
	return
}

func classifyOperatorPairDetail(detail string) (reason, code, field string) {
	reason, code, field = "unknown", "other", "other"
	lower := strings.ToLower(detail)
	switch {
	case strings.Contains(lower, "instructions required"), strings.Contains(lower, "instructions are required"), strings.Contains(lower, "missing required parameter: instructions"):
		return "invalid_request", "invalid_request_error", "instructions"
	case strings.Contains(lower, "unsupported parameter"), strings.Contains(lower, "unsupported value"):
		reason, code = "unsupported_field", "unsupported_value"
	case strings.Contains(lower, "model not found"), strings.Contains(lower, "unsupported model"):
		return "model_unsupported", "model_not_found", "model"
	case strings.Contains(lower, "invalid input"), strings.Contains(lower, "input is required"):
		return "invalid_request", "invalid_request_error", "input"
	default:
		return
	}
	for _, candidate := range []string{"max_output_tokens", "reasoning.context", "reasoning.effort", "parallel_tool_calls", "tool_choice", "instructions", "reasoning", "include", "model", "input", "stream", "store"} {
		if strings.Contains(lower, candidate) {
			field = candidate
			break
		}
	}
	return
}

func writeOperatorPairFailureArtifact(path, leg string, observation operatorPairObservation) error {
	if leg != "direct_codex" && leg != "gateway_responses_to_codex" {
		return errors.New("invalid failure leg")
	}
	if !oneOperatorPairValue(observation.reason, "unknown", "invalid_request", "unsupported_field", "model_unsupported", "quota_exhausted", "rate_limited", "service_error") ||
		!oneOperatorPairValue(observation.errorCode, "other", "invalid_request_error", "unsupported_parameter", "unsupported_value", "model_not_found", "insufficient_quota", "rate_limit_exceeded", "rate_limit_error", "server_error", "upstream_unavailable") ||
		!oneOperatorPairValue(observation.errorField, "other", "model", "input", "instructions", "stream", "store", "max_output_tokens", "reasoning", "reasoning.effort", "reasoning.context", "include", "tool_choice", "parallel_tool_calls") ||
		!oneOperatorPairValue(observation.bodyReadResult, "not_observed", "complete", "read_failure", "size_limit", "stream_failure") ||
		!oneOperatorPairValue(observation.transport, "none", "unknown") {
		return errors.New("invalid failure observation")
	}
	if observation.status == 0 && (observation.bodyReadResult != "not_observed" || observation.transport != "unknown") || observation.status != 0 && observation.transport != "none" {
		return errors.New("inconsistent failure observation")
	}
	var status *int
	if observation.status != 0 {
		status = &observation.status
	}
	record := operatorPairFailureArtifact{SchemaVersion: 1, Leg: leg, Profile: "subscription_codex", CapturedAt: time.Now().UTC().Format("2006-01-02T15:04:05Z"), Model: operatorPairModel,
		Status: status, Reason: observation.reason, Code: observation.errorCode, Field: observation.errorField, BodyRead: observation.bodyReadResult, Transport: observation.transport}
	data, err := json.Marshal(record)
	if err != nil {
		return errors.New("failure artifact encoding failed")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("failure artifact create failed")
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return errors.New("failure artifact permissions failed")
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return errors.New("failure artifact write failed")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return errors.New("failure artifact sync failed")
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return errors.New("failure artifact close failed")
	}
	return nil
}

func oneOperatorPairValue(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func readOperatorPairSSE(body io.Reader, result *operatorPairObservation) error {
	reader := bufio.NewReaderSize(io.LimitReader(body, operatorPairBodyLimit+1), 1)
	var event string
	var data []byte
	for {
		line, err := reader.ReadString('\n')
		result.bodyBytes += len(line)
		if result.bodyBytes > operatorPairBodyLimit || len(data)+len(line) > operatorPairBodyLimit {
			return errors.New("response size limit")
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return errors.New("stream read failure")
		}
		if errors.Is(err, io.EOF) && line != "" {
			return errors.New("unterminated SSE line")
		}
		trimmed := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if trimmed == "" {
			if len(data) > 0 {
				if err := consumeOperatorPairEvent(event, data, result); err != nil {
					return err
				}
			}
			event, data = "", data[:0]
		} else if strings.HasPrefix(trimmed, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		} else if strings.HasPrefix(trimmed, "data:") {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(trimmed, "data:"), " ")...)
		}
		if err == io.EOF {
			break
		}
	}
	if result.created != 1 || result.completed != 1 || result.deltas < 1 || result.terminal != "completed" || !result.earlyDelivery {
		return errors.New("stream missing required ordered lifecycle")
	}
	return nil
}

func consumeOperatorPairEvent(event string, data []byte, result *operatorPairObservation) error {
	switch event {
	case "response.created", "response.in_progress", "response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done", "response.output_text.delta", "response.output_text.done", "response.completed":
	default:
		return errors.New("unsupported event category")
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil {
		return errors.New("invalid event JSON")
	}
	var kind string
	if json.Unmarshal(envelope["type"], &kind) != nil || kind != event {
		return errors.New("event type mismatch")
	}
	result.eventOrder = append(result.eventOrder, event)
	switch event {
	case "response.created":
		result.created++
	case "response.output_item.added":
		var item struct {
			Type             string          `json:"type"`
			EncryptedContent json.RawMessage `json:"encrypted_content"`
		}
		if json.Unmarshal(envelope["item"], &item) == nil && item.Type == "reasoning" {
			var encrypted string
			if len(item.EncryptedContent) == 0 || json.Unmarshal(item.EncryptedContent, &encrypted) != nil || encrypted == "" {
				return errors.New("reasoning item lacks encrypted content")
			}
			result.reasoningItems++
		}
	case "response.output_text.delta":
		var delta string
		if json.Unmarshal(envelope["delta"], &delta) != nil {
			return errors.New("invalid text delta")
		}
		result.outputBytes += len(delta)
		if result.outputBytes > operatorPairTextLimit {
			return errors.New("client output-text limit")
		}
		result.deltas++
		result.earlyDelivery = result.created == 1 && result.completed == 0
	case "response.completed":
		result.completed++
		var response struct {
			Status string          `json:"status"`
			Usage  json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(envelope["response"], &response) != nil || response.Status != "completed" {
			return errors.New("non-completed terminal response")
		}
		result.terminal = response.Status
		if len(response.Usage) > 0 && string(response.Usage) != "null" {
			var usage map[string]json.RawMessage
			if json.Unmarshal(response.Usage, &usage) == nil {
				var input, output int64
				if json.Unmarshal(usage["input_tokens"], &input) == nil && json.Unmarshal(usage["output_tokens"], &output) == nil && input >= 0 && output >= 0 {
					result.usage = "reported"
				}
			}
		}
	}
	return nil
}

func provisionOperatorPairGateway(t *testing.T, account sqlite.Account, credential sqlite.Credential) (string, sqlite.IssuedVirtualKey) {
	return provisionOperatorPairGatewayWithRPM(t, account, credential, 2)
}

// The tool fixture opts into four requests (two two-round trips); paired baseline stays RPM=2.
func provisionOperatorPairGatewayWithRPM(t *testing.T, account sqlite.Account, credential sqlite.Credential, rpm int64) (string, sqlite.IssuedVirtualKey) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal("isolated gateway fixture unavailable")
	}
	dbPath := filepath.Join(dir, "gateway.db")
	target, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal("isolated gateway database unavailable")
	}
	ctx := context.Background()
	if err := sqlite.Migrate(ctx, target); err != nil {
		_ = target.Close()
		t.Fatal("isolated gateway database migration failed")
	}
	if _, err := sqlite.NewAccounts(target).Create(ctx, account); err != nil {
		_ = target.Close()
		t.Fatalf("isolated gateway account setup failed: %v", err)
	}
	if _, err := sqlite.NewCredentials(target).Create(ctx, credential); err != nil {
		_ = target.Close()
		t.Fatal("isolated encrypted credential setup failed")
	}
	policy, err := sqlite.NewKeyPolicies(target).Create(ctx, sqlite.CreateKeyPolicyParams{ID: "m51-pair-policy", Enabled: true, Models: []string{operatorPairModel}, Connectors: []string{"codex"}, RPM: rpm, TPM: 8192})
	if err != nil {
		_ = target.Close()
		t.Fatal("isolated gateway policy setup failed")
	}
	issued, err := sqlite.NewVirtualKeys(target).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		_ = target.Close()
		t.Fatal("isolated gateway key setup failed")
	}
	if err := target.Close(); err != nil {
		t.Fatal("isolated gateway database close failed")
	}
	return dbPath, issued
}

func startOperatorPairGateway(t *testing.T, dbPath, keyPath, accountID string, connector *codex.Connector, doer core.HTTPDoer) (*httptest.Server, func()) {
	t.Helper()
	listen := "127.0.0.1:0"
	p := &protectedConfig{
		Server:  protectedServer{Listen: listen, MaxRequestBytes: operatorPairBodyLimit, ShutdownTimeout: "1s"},
		Storage: protectedStorage{Driver: "sqlite", Path: dbPath}, Secrets: protectedSecrets{MasterKeyFile: keyPath},
		Connectors: []protectedConnector{{ID: "codex", Kind: "connector", Implementation: "pestiroute.codex.responses", Protocols: []string{responsesProtocol}, Settings: nativeSettings{Profile: operatorPairProfile, Model: operatorPairModel, AccountID: accountID, CredentialID: "oauth"}}},
		Routes:     []protectedRoute{{ID: "codex-route", Protocol: responsesProtocol, Mode: "native", Model: operatorPairModel, Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(4096)}, Targets: []routeTarget{{Connector: "codex", Account: accountID}}}},
		Policies:   map[string]string{"standard": "m51-pair-policy"},
	}
	prepared, err := prepareProtectedConfig(context.Background(), config{protected: p, DatabasePath: dbPath, MasterKeyFile: keyPath, Listen: listen, ShutdownTimeout: "1s"})
	if err != nil {
		t.Fatal("isolated gateway startup failed")
	}
	ready, draining := atomic.Bool{}, atomic.Bool{}
	ready.Store(true)
	h, closeComponents, err := composeHandlerWithFactory(prepared, &ready, &draining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		if item.Implementation == "pestiroute.codex.responses" {
			return codexCompositionConnector{Connector: connector, doer: doer}
		}
		return nil
	})
	if err != nil {
		_ = prepared.runtimeDB.Close()
		_ = prepared.processLock.Close()
		t.Fatal("isolated gateway composition failed")
	}
	server := httptest.NewServer(h)
	closeFn := func() {
		server.Close()
		_ = closeComponents(context.Background())
		_ = prepared.runtimeDB.Close()
		_ = prepared.processLock.Close()
	}
	return server, closeFn
}

func newOperatorPairArtifact(leg, apiPath string, observation operatorPairObservation) operatorPairArtifact {
	return operatorPairArtifact{
		SchemaVersion: 1, Leg: leg, Profile: "subscription_codex", CapturedAt: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		Client: map[string]string{"name": "Go http.Client", "version": operatorPairGoReleaseVersion()},
		Model:  operatorPairModel, Scenario: "plain_text", FixtureID: "m51-plain-text-v1",
		Limits:   map[string]int{"timeout_seconds": 30, "max_output_tokens": operatorPairTextLimit, "client_runs": 1},
		Requests: []map[string]any{{"run": 1, "attempt": 1, "timeout_seconds": 30, "max_output_tokens": operatorPairTextLimit, "status": observation.status, "stream": true, "api_path": apiPath}},
		Outcome:  "completed",
		Observations: map[string]any{
			"early_delivery": observation.earlyDelivery, "event_order": observation.eventOrder, "tool_links": []string{},
			"usage": observation.usage, "target_dispatches": 1, "retries": 0, "client_status": observation.status,
			"reasoning_observation": map[bool]string{true: "encrypted_content_present_redacted", false: "not_applicable"}[observation.reasoningItems > 0],
		},
	}
}

func operatorPairGoReleaseVersion() string {
	version := strings.TrimPrefix(runtime.Version(), "go")
	if suffix := strings.IndexByte(version, '-'); suffix >= 0 {
		version = version[:suffix]
	}
	return version
}

func validateOperatorPairArtifactPaths(direct, gateway string) error {
	if filepath.Dir(direct) != operatorPairPath || filepath.Dir(gateway) != operatorPairPath {
		return errors.New("invalid artifact path")
	}
	directPrefix, ok := operatorPairArtifactPrefix(filepath.Base(direct), "-direct.json", "direct.json")
	if !ok {
		return errors.New("invalid direct artifact path")
	}
	gatewayPrefix, ok := operatorPairArtifactPrefix(filepath.Base(gateway), "-gateway.json", "gateway.json")
	if !ok || directPrefix != gatewayPrefix {
		return errors.New("artifact paths must share one safe pair prefix")
	}
	return nil
}

func operatorPairArtifactPrefix(name, suffix, unprefixed string) (string, bool) {
	if name == unprefixed {
		return "", true
	}
	if !strings.HasSuffix(name, suffix) {
		return "", false
	}
	prefix := strings.TrimSuffix(name, suffix)
	if prefix == "" {
		return "", false
	}
	for _, char := range prefix {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
			return "", false
		}
	}
	return prefix, true
}

func prepareOperatorPairArtifactDirectory(dir string) error {
	parent, err := os.Lstat(filepath.Dir(dir))
	if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || parent.Mode().Perm()&0022 != 0 {
		return errors.New("unsafe artifact parent")
	}
	parentOwner, ok := parent.Sys().(*syscall.Stat_t)
	if !ok || int(parentOwner.Uid) != os.Geteuid() {
		return errors.New("unsafe artifact owner")
	}
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return errors.New("unsafe artifact directory")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Geteuid() {
		return errors.New("unsafe artifact owner")
	}
	return nil
}

func writeOperatorPairArtifact(path string, artifact operatorPairArtifact) error {
	data, err := json.Marshal(artifact)
	if err != nil {
		return errors.New("artifact encoding failed")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("artifact create failed")
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return errors.New("artifact permissions failed")
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return errors.New("artifact write failed")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return errors.New("artifact sync failed")
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return errors.New("artifact close failed")
	}
	return nil
}

func TestOperatorPairSSEBoundedAndSanitized(t *testing.T) {
	var request map[string]any
	if err := json.Unmarshal(operatorPairRequest, &request); err != nil || request["model"] != operatorPairModel || request["stream"] != true || request["store"] != false || request["max_output_tokens"] != nil || request["reasoning"] == nil || request["include"] == nil || request["tool_choice"] != "auto" || request["parallel_tool_calls"] != false {
		t.Fatal("fixed Lite profile request differed")
	}
	items, ok := request["input"].([]any)
	if !ok || len(items) != 2 || items[0].(map[string]any)["type"] != "additional_tools" || items[0].(map[string]any)["id"] != "at_48517110-6418-52c6-9cd2-cd8b64e6037d" || items[0].(map[string]any)["role"] != "developer" || items[0].(map[string]any)["tools"].([]any) == nil || items[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "What is 137 × 293? Reply with only the number." {
		t.Fatal("fixed Lite request prefix or short math prompt differed")
	}
	threadID, err := uuid.Parse(operatorPairThreadID)
	if err != nil {
		t.Fatal("fixed Lite thread ID is invalid")
	}
	toolsID := uuid.NewSHA1(uuid.NewSHA1(uuid.NameSpaceOID, []byte(threadID.String())), []byte("[]"))
	if items[0].(map[string]any)["id"] != "at_"+toolsID.String() {
		t.Fatal("additional_tools ID is not derived from the fixed thread and empty tool list")
	}
	if err := validateOperatorPairArtifactPaths(filepath.Join(operatorPairPath, "direct.json"), filepath.Join(operatorPairPath, "gateway.json")); err != nil {
		t.Fatalf("plain pair artifact names rejected: %v", err)
	}
	if err := validateOperatorPairArtifactPaths(filepath.Join(operatorPairPath, "retry2-direct.json"), filepath.Join(operatorPairPath, "retry2-gateway.json")); err != nil {
		t.Fatalf("fresh pair prefix rejected: %v", err)
	}
	if validateOperatorPairArtifactPaths(filepath.Join(operatorPairPath, "one-direct.json"), filepath.Join(operatorPairPath, "two-gateway.json")) == nil {
		t.Fatal("mismatched artifact prefixes accepted")
	}
	var got operatorPairObservation
	body := strings.Join([]string{
		"event: response.created", `data: {"type":"response.created"}`, "",
		"event: response.output_item.added", `data: {"type":"response.output_item.added","item":{"type":"reasoning","encrypted_content":"synthetic-ciphertext"}}`, "",
		"event: response.output_text.delta", `data: {"type":"response.output_text.delta","delta":"OK"}`, "",
		"event: response.completed", `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":2,"output_tokens":1}}}`, "", "",
	}, "\n")
	if err := readOperatorPairSSE(strings.NewReader(body), &got); err != nil {
		t.Fatal("bounded synthetic SSE rejected")
	}
	if got.status != 0 || got.outputBytes != 2 || got.usage != "reported" || got.reasoningItems != 1 || !got.earlyDelivery || got.bodyBytes > operatorPairBodyLimit {
		t.Fatal("synthetic SSE observations differed")
	}
	tooMuch := fmt.Sprintf("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", strings.Repeat("x", operatorPairTextLimit+1))
	var capped operatorPairObservation
	if readOperatorPairSSE(strings.NewReader(tooMuch), &capped) == nil {
		t.Fatal("output text safety cap was not enforced")
	}
	var oversized operatorPairObservation
	if readOperatorPairSSE(strings.NewReader(strings.Repeat("x", operatorPairBodyLimit+2)), &oversized) == nil || oversized.bodyBytes <= operatorPairBodyLimit {
		t.Fatal("response byte bound was not enforced")
	}
}

func TestOperatorPairDirectRequestCapturesKnownLiteIdentity(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != operatorPairDirectPath {
			t.Errorf("unexpected direct request target")
		}
		if r.Header.Get("originator") != "pestiroute" || r.Header.Get("User-Agent") != "PestiRoute" || r.Header.Get("x-openai-internal-codex-responses-lite") != "true" {
			t.Errorf("trusted Lite identity headers differ")
		}
		var id string
		for _, header := range []string{"session-id", "thread-id", "x-client-request-id"} {
			got := r.Header.Get(header)
			parsed, err := uuid.Parse(got)
			if err != nil || parsed.Version() != 4 {
				t.Errorf("%s is not a safe random UUID", header)
			}
			if id == "" {
				id = got
			} else if got != id {
				t.Errorf("correlation IDs are not shared")
			}
		}
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	req, err := newOperatorPairDirectRequest(t.Context(), server.URL+operatorPairDirectPath, "provider-account", "synthetic-token")
	if err != nil {
		t.Fatal("fixed direct request construction failed")
	}
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal("captured direct POST failed")
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent || !bytes.Equal(received, operatorPairRequest) {
		t.Fatal("captured direct POST differed from the fixed Lite bytes")
	}
}

func TestAssembledLiteGatewayCapturesExactUpstreamPOST(t *testing.T) {
	_, _, keyPath := protectedFixture(t)
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal("test key unavailable")
	}
	account := sqlite.Account{ID: "lite-capture-account", Connector: "codex", Enabled: true}
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	plain, err := json.Marshal(operatorPairCredential{Version: 1, AccessToken: "synthetic-access", AccountID: "provider-account", ExpiresAt: expires})
	if err != nil {
		t.Fatal("synthetic credential unavailable")
	}
	envelope, err := secure.Seal(key, 1, "v1", "credentials", "oauth", account.ID, plain)
	if err != nil {
		t.Fatal("synthetic credential sealing failed")
	}
	credential := sqlite.Credential{ID: "oauth", AccountID: account.ID, FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext, ExpiresAt: &expires}
	var captured []byte
	var requests atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		if req.Method != http.MethodPost || req.URL.Path != operatorPairDirectPath || req.Header.Get("Authorization") != "Bearer synthetic-access" || req.Header.Get("ChatGPT-Account-Id") != "provider-account" {
			t.Errorf("assembled gateway upstream request scope differed")
		}
		if req.Header.Get("originator") != "pestiroute" || req.Header.Get("User-Agent") != "PestiRoute" || req.Header.Get("x-openai-internal-codex-responses-lite") != "true" {
			t.Errorf("assembled gateway trusted Lite headers differed")
		}
		var correlation string
		for _, name := range []string{"session-id", "thread-id", "x-client-request-id"} {
			value := req.Header.Get(name)
			parsed, parseErr := uuid.Parse(value)
			if parseErr != nil || parsed.Version() != 4 {
				t.Errorf("assembled gateway %s is not UUIDv4", name)
			}
			if correlation == "" {
				correlation = value
			} else if value != correlation {
				t.Errorf("assembled gateway correlation IDs differ")
			}
		}
		captured, _ = io.ReadAll(req.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	defer upstream.Close()
	connector := codex.NewConnector()
	defer connector.Close(context.Background())
	client := connector.HTTPDoer().(*http.Client)
	transport := client.Transport.(*http.Transport).Clone()
	// The loopback fake uses a synthetic certificate; production verification is
	// unchanged because this transport exists only in this test.
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", upstream.Listener.Addr().String())
	}
	client.Transport = transport
	dbPath, issued := provisionOperatorPairGateway(t, account, credential)
	server, closeGateway := startOperatorPairGateway(t, dbPath, keyPath, account.ID, connector, client)
	defer closeGateway()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(operatorPairRequest))
	if err != nil {
		t.Fatal("gateway request construction failed")
	}
	req.Header.Set("Authorization", "Bearer "+issued.Secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("originator", "spoofed")
	req.Header.Set("session-id", "spoofed")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal("assembled gateway POST failed")
	}
	responseBody, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || requests.Load() != 1 || !bytes.Equal(captured, operatorPairRequest) {
		t.Fatalf("assembled Lite POST capture differs: status=%d sends=%d body_equal=%t response=%s", response.StatusCode, requests.Load(), bytes.Equal(captured, operatorPairRequest), responseBody)
	}
}

func TestOperatorPairArtifactIsStrictlySanitized(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal("private artifact directory setup failed")
	}
	observation := operatorPairObservation{status: 200, eventOrder: []string{"response.created", "response.output_item.added", "response.output_text.delta", "response.output_item.done", "response.completed"}, created: 1, deltas: 1, completed: 1, reasoningItems: 1, usage: "reported", terminal: "completed", earlyDelivery: true}
	path := filepath.Join(dir, "direct.json")
	if err := writeOperatorPairArtifact(path, newOperatorPairArtifact("direct_codex", operatorPairDirectPath, observation)); err != nil {
		t.Fatal("sanitized artifact write failed")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("artifact was not private")
	}
	data, err := os.ReadFile(path)
	if err != nil || !json.Valid(data) || bytes.Contains(data, []byte("PRIVATE")) || bytes.Contains(data, []byte("Bearer")) {
		t.Fatal("artifact contained invalid or sensitive data")
	}
}

func TestOperatorPairFailureCapturePersistsOnlyFixedLabels(t *testing.T) {
	const privateText = "private-user-prompt-must-not-survive"
	response := &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"unsupported_value","param":"reasoning.context","message":"` + privateText + `"}}`))}
	observation := operatorPairObservation{status: response.StatusCode, reason: "unknown", errorCode: "other", errorField: "other", bodyReadResult: "not_observed", transport: "none"}
	observeOperatorPairFailureBody(response, &observation)
	if observation.reason != "unsupported_field" || observation.errorCode != "unsupported_value" || observation.errorField != "reasoning.context" || observation.bodyReadResult != "complete" {
		t.Fatalf("sanitized failure classification differs: %+v", observation)
	}
	path := filepath.Join(t.TempDir(), "failure.json")
	if err := writeOperatorPairFailureArtifact(path, "direct_codex", observation); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("failure artifact permissions differ")
	}
	data, err := os.ReadFile(path)
	if err != nil || !json.Valid(data) || bytes.Contains(data, []byte(privateText)) || bytes.Contains(data, []byte("message")) {
		t.Fatal("failure artifact retained provider text")
	}
	if err := writeOperatorPairFailureArtifact(path, "direct_codex", observation); err == nil {
		t.Fatal("failure artifact overwrite was accepted")
	}
	for _, tc := range []struct {
		body, reason, code, field string
	}{
		{`{"error":{"type":"invalid_request_error","detail":"Missing required parameter: instructions PRIVATE"}}`, "invalid_request", "invalid_request_error", "instructions"},
		{`{"detail":"unsupported value for reasoning.context PRIVATE"}`, "unsupported_field", "unsupported_value", "reasoning.context"},
		{`{"error":{"type":"rate_limit_error","message":"PRIVATE"}}`, "rate_limited", "rate_limit_error", "other"},
		{`{"error":{"code":"upstream_unavailable","message":"PRIVATE"}}`, "service_error", "upstream_unavailable", "other"},
	} {
		reason, code, field := classifyOperatorPairFailureBody([]byte(tc.body))
		if reason != tc.reason || code != tc.code || field != tc.field || strings.Contains(strings.Join([]string{reason, code, field}, ","), "PRIVATE") {
			t.Fatalf("safe HTTP failure classification differed: %s/%s/%s", reason, code, field)
		}
		if tc.code == "rate_limit_error" {
			observation := operatorPairObservation{status: http.StatusTooManyRequests, reason: reason, errorCode: code, errorField: field, bodyReadResult: "complete", transport: "none"}
			if err := writeOperatorPairFailureArtifact(filepath.Join(t.TempDir(), "rate-limit.json"), "direct_codex", observation); err != nil {
				t.Fatalf("fixed error.type label could not be captured: %v", err)
			}
		}
	}
}
