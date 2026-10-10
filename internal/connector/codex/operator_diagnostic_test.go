package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
	"github.com/google/uuid"
)

const diagnosticBodyLimit = 1 << 20

const diagnosticResultDirectory = "/tmp/opencode/pestiroute-live-evidence"

var diagnosticResultName = regexp.MustCompile(`^luna-[0-9]+(?:-completion)?\.json$`)

type diagnosticRequestProfile uint8

const (
	diagnosticProfileStandard diagnosticRequestProfile = iota + 1
	diagnosticProfileLunaLite
	diagnosticProfileLunaLitePlainText
	diagnosticProfileLunaLiteCompletion
	diagnosticProfileLunaLiteHighCompletion
)

func parseDiagnosticRequestProfile(value string) (diagnosticRequestProfile, bool) {
	switch value {
	case "standard":
		return diagnosticProfileStandard, true
	case "luna-lite":
		return diagnosticProfileLunaLite, true
	case "luna-lite-plain-text":
		return diagnosticProfileLunaLitePlainText, true
	case "luna-lite-completion":
		return diagnosticProfileLunaLiteCompletion, true
	case "luna-lite-high-completion":
		return diagnosticProfileLunaLiteHighCompletion, true
	default:
		return 0, false
	}
}

func diagnosticRequestBody(profile diagnosticRequestProfile, threadID string) ([]byte, error) {
	if profile == diagnosticProfileStandard {
		return []byte(`{"model":"gpt-5.4-mini","stream":true,"store":false,"instructions":"","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Reply with the word OK."}]}]}`), nil
	}
	if profile != diagnosticProfileLunaLite && profile != diagnosticProfileLunaLitePlainText && !isCompletionDiagnosticProfile(profile) {
		return nil, fmt.Errorf("unsupported diagnostic profile")
	}
	threadUUID, err := uuid.Parse(threadID)
	if err != nil {
		return nil, fmt.Errorf("invalid diagnostic thread id")
	}
	tools := []any{}
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		return nil, fmt.Errorf("diagnostic request construction failed")
	}
	prefixNamespace := uuid.NewSHA1(uuid.NameSpaceOID, []byte(threadUUID.String()))
	toolsID := uuid.NewSHA1(prefixNamespace, toolsJSON)
	userText := "Reply with the word OK."
	if profile == diagnosticProfileLunaLiteHighCompletion {
		userText = "What is 137 × 293? Reply with only the number."
	}
	input := []any{
		map[string]any{"type": "additional_tools", "id": "at_" + toolsID.String(), "role": "developer", "tools": tools},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": userText}}},
	}
	body := map[string]any{
		"model":               "gpt-6-luna",
		"stream":              true,
		"store":               false,
		"instructions":        "",
		"input":               input,
		"tool_choice":         "auto",
		"parallel_tool_calls": false,
		"include":             []string{"reasoning.encrypted_content"},
	}
	if profile == diagnosticProfileLunaLite || isCompletionDiagnosticProfile(profile) {
		effort := "medium"
		if profile == diagnosticProfileLunaLiteHighCompletion {
			effort = "high"
		}
		body["reasoning"] = map[string]any{"effort": effort, "context": "all_turns"}
	}
	return json.Marshal(body)
}

func applyDiagnosticProfileHeaders(req *http.Request, profile diagnosticRequestProfile, threadID string) {
	if profile != diagnosticProfileLunaLite && profile != diagnosticProfileLunaLitePlainText && !isCompletionDiagnosticProfile(profile) {
		return
	}
	req.Header.Set("originator", "pestiroute")
	req.Header.Set("User-Agent", "PestiRoute")
	req.Header.Set("session-id", threadID)
	req.Header.Set("thread-id", threadID)
	req.Header.Set("x-client-request-id", threadID)
	req.Header.Set("x-openai-internal-codex-responses-lite", "true")
}

func isCompletionDiagnosticProfile(profile diagnosticRequestProfile) bool {
	return profile == diagnosticProfileLunaLiteCompletion || profile == diagnosticProfileLunaLiteHighCompletion
}

type diagnosticError struct {
	Detail string `json:"detail"`
	Error  struct {
		Code    string `json:"code"`
		Type    string `json:"type"`
		Param   string `json:"param"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	} `json:"error"`
}

// classifyDiagnosticError inspects provider text in memory and returns only fixed labels.
func classifyDiagnosticError(body []byte) (reason, code, field string) {
	reason, code, field = "unknown", "other", "other"
	var e diagnosticError
	if json.Unmarshal(body, &e) != nil {
		return
	}
	switch e.Error.Code {
	case "invalid_request_error", "unsupported_parameter", "unsupported_value", "model_not_found", "insufficient_quota", "rate_limit_exceeded", "server_error":
		code = e.Error.Code
	}
	switch e.Error.Param {
	case "model", "input", "instructions", "stream", "store", "max_output_tokens", "reasoning", "reasoning.effort", "reasoning.context", "include", "tools", "tool_choice", "parallel_tool_calls":
		field = e.Error.Param
	}
	message := strings.ToLower(e.Error.Message)
	switch {
	case strings.Contains(message, "instruction") && (strings.Contains(message, "required") || strings.Contains(message, "missing")):
		reason = "instructions_missing"
	case strings.Contains(message, "model") && (strings.Contains(message, "unsupported") || strings.Contains(message, "not found") || strings.Contains(message, "unavailable")):
		reason = "model_unsupported"
	case strings.Contains(message, "unsupported") && (field != "other" || strings.Contains(message, "parameter") || strings.Contains(message, "field")):
		reason = "unsupported_field"
	case strings.Contains(message, "input") && (strings.Contains(message, "invalid") || strings.Contains(message, "required")):
		reason = "invalid_input"
	case strings.Contains(message, "temporarily") || strings.Contains(message, "server error"):
		reason = "service_error"
	}
	detail := e.Detail
	if detail == "" {
		detail = e.Error.Detail
	}
	if detailReason, detailField := classifyDiagnosticDetail(detail); detailReason != "unknown" {
		reason = detailReason
		if e.Error.Param == "" && detailField != "other" {
			field = detailField
		}
	}
	return
}

func classifyDiagnosticDetail(detail string) (reason, field string) {
	reason, field = "unknown", "other"
	detail = strings.ToLower(detail)
	switch {
	case strings.Contains(detail, "instructions required"), strings.Contains(detail, "instructions are required"), strings.Contains(detail, "instructions is required"), strings.Contains(detail, "missing required parameter: instructions"):
		return "instructions_missing", "instructions"
	case strings.Contains(detail, "unsupported parameter") && strings.Contains(detail, "max_output_tokens"):
		return "unsupported_field", "max_output_tokens"
	case strings.Contains(detail, "model not found"), strings.Contains(detail, "model is not found"), strings.Contains(detail, "model unsupported"), strings.Contains(detail, "unsupported model"):
		return "model_unsupported", "model"
	case strings.Contains(detail, "invalid input"), strings.Contains(detail, "input is invalid"), strings.Contains(detail, "input is required"):
		return "invalid_input", "input"
	}
	return reason, field
}

// classifyDiagnosticTransportError uses only wrapped error types and fixed labels.
// It is called only for client.Do failures, never for HTTP provider error responses.
func classifyDiagnosticTransportError(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, syscall.ETIMEDOUT) {
		return "deadline"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "deadline"
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return "dns_failure"
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return "tls_verification"
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		return "tls_verification"
	}
	if _, ok := errors.AsType[x509.HostnameError](err); ok {
		return "tls_verification"
	}
	if _, ok := errors.AsType[x509.CertificateInvalidError](err); ok {
		return "tls_verification"
	}
	if _, ok := errors.AsType[*tls.RecordHeaderError](err); ok {
		return "tls_protocol_error"
	}
	if _, ok := errors.AsType[tls.RecordHeaderError](err); ok {
		return "tls_protocol_error"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "unexpected_eof"
	}
	if errors.Is(err, io.EOF) {
		return "connection_closed"
	}
	if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
		return "permission_denied"
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return "connection_reset"
	}
	opErr, opErrFound := errors.AsType[*net.OpError](err)
	if (opErrFound && (opErr.Op == "dial" || opErr.Op == "connect")) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return "connect_failure"
	}
	var requestError *url.Error
	if errors.As(err, &requestError) && requestError.Err != nil {
		return classifyDiagnosticHTTPErrorText(requestError.Err.Error())
	}
	return "unknown"
}

// classifyDiagnosticHTTPErrorText maps only known net/http parse phrases to
// fixed labels. The text is inspected in memory and is never returned or logged.
func classifyDiagnosticHTTPErrorText(text string) string {
	text = strings.ToLower(text)
	switch {
	case strings.Contains(text, "malformed http response"), strings.Contains(text, "malformed http version"), strings.Contains(text, "malformed http status code"):
		return "malformed_http_response"
	case strings.Contains(text, "invalid header") || strings.Contains(text, "malformed header") || strings.Contains(text, "malformed mime header") || strings.Contains(text, "invalid mime header"):
		return "invalid_header"
	case strings.Contains(text, "content-length"):
		return "content_length"
	case strings.Contains(text, "proxy error"):
		return "proxy_error"
	default:
		return "unknown"
	}
}

func diagnosticProtocolLabel(protocol string) string {
	switch protocol {
	case "http/1.1":
		return "http1"
	case "h2":
		return "http2"
	case "":
		return "none"
	default:
		return "other"
	}
}

func diagnosticNextProtosLabel(protocols []string) string {
	if len(protocols) == 0 {
		return "none"
	}
	if len(protocols) == 1 {
		return diagnosticProtocolLabel(protocols[0])
	}
	return "other"
}

// diagnosticRequestPhases records only callback milestones. These are local
// observations, not proof that a server received or processed the request.
type diagnosticRequestPhases struct {
	connected          atomic.Bool
	tlsHandshakeDone   atomic.Bool
	tlsHandshakeOK     atomic.Bool
	tlsProtocol        atomic.Uint32
	requestHeadersSent atomic.Bool
	requestWriteDone   atomic.Bool
	requestWriteOK     atomic.Bool
}

func (p *diagnosticRequestPhases) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		ConnectDone: func(_, _ string, err error) {
			if err == nil {
				p.connected.Store(true)
			}
		},
		GotConn: func(httptrace.GotConnInfo) { p.connected.Store(true) },
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			p.tlsHandshakeDone.Store(true)
			p.tlsProtocol.Store(diagnosticProtocolCode(diagnosticProtocolLabel(state.NegotiatedProtocol)))
			if err == nil {
				p.tlsHandshakeOK.Store(true)
			}
		},
		WroteHeaders: func() { p.requestHeadersSent.Store(true) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			p.requestWriteDone.Store(true)
			if info.Err == nil {
				p.requestWriteOK.Store(true)
			}
		},
	}
}

func diagnosticProtocolCode(protocol string) uint32 {
	switch protocol {
	case "http1":
		return 1
	case "http2":
		return 2
	case "other":
		return 3
	default:
		return 0
	}
}

func diagnosticProtocolName(code uint32) string {
	switch code {
	case 1:
		return "http1"
	case 2:
		return "http2"
	case 3:
		return "other"
	default:
		return "none"
	}
}

func TestDiagnosticTraceCapturesNegotiatedALPN(t *testing.T) {
	phases := &diagnosticRequestPhases{}
	phases.trace().TLSHandshakeDone(tls.ConnectionState{NegotiatedProtocol: "h2"}, nil)
	if !phases.tlsHandshakeDone.Load() || !phases.tlsHandshakeOK.Load() || diagnosticProtocolName(phases.tlsProtocol.Load()) != "http2" {
		t.Fatal("TLS trace did not retain only the negotiated HTTP/2 label")
	}
}

func (p *diagnosticRequestPhases) log(t *testing.T, prefix string, transport *http.Transport) {
	t.Helper()
	var nextProtos []string
	if transport.TLSClientConfig != nil {
		nextProtos = transport.TLSClientConfig.NextProtos
	}
	t.Logf("%s connect_complete=%t tls_handshake_finished=%t tls_handshake_complete=%t tls_negotiated_protocol=%s transport_alpn_config=%s request_headers_written=%t request_write_finished=%t request_write_complete=%t",
		prefix, p.connected.Load(), p.tlsHandshakeDone.Load(), p.tlsHandshakeOK.Load(), diagnosticProtocolName(p.tlsProtocol.Load()), diagnosticNextProtosLabel(nextProtos), p.requestHeadersSent.Load(), p.requestWriteDone.Load(), p.requestWriteOK.Load())
}

type diagnosticResult struct {
	SchemaVersion         int                     `json:"schema_version"`
	Profile               string                  `json:"profile"`
	Leg                   string                  `json:"leg,omitempty"`
	Model                 string                  `json:"model,omitempty"`
	ReasoningEffort       string                  `json:"reasoning_effort,omitempty"`
	ReasoningContext      string                  `json:"reasoning_context,omitempty"`
	Status                *int                    `json:"status"`
	Reason                string                  `json:"reason"`
	Code                  string                  `json:"code"`
	Field                 string                  `json:"field"`
	Transport             string                  `json:"transport"`
	ConnectComplete       bool                    `json:"connect_complete"`
	TLSHandshakeFinished  bool                    `json:"tls_handshake_finished"`
	TLSHandshakeComplete  bool                    `json:"tls_handshake_complete"`
	TLSNegotiatedProtocol string                  `json:"tls_negotiated_protocol"`
	TransportALPNConfig   string                  `json:"transport_alpn_config"`
	RequestHeadersWritten bool                    `json:"request_headers_written"`
	RequestWriteFinished  bool                    `json:"request_write_finished"`
	RequestWriteComplete  bool                    `json:"request_write_complete"`
	BodyReadResult        string                  `json:"body_read_result"`
	OutputObserved        bool                    `json:"output_observed"`
	ClientCanceled        bool                    `json:"client_canceled"`
	Completion            *diagnosticStreamResult `json:"completion,omitempty"`
}

type diagnosticEventRun struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

type diagnosticStreamResult struct {
	DrainedToEOF       bool                 `json:"drained_to_eof"`
	StreamStatus       string               `json:"stream_status"`
	TerminalStatus     string               `json:"terminal_status"`
	EventOrder         []diagnosticEventRun `json:"event_order"`
	ReasoningItemCount int                  `json:"reasoning_item_count"`
	UsageObserved      bool                 `json:"usage_observed"`
	ReasoningTokens    *int64               `json:"reasoning_tokens"`
	OutputTextObserved bool                 `json:"output_text_observed"`
}

func (p *diagnosticRequestPhases) result(profile diagnosticRequestProfile, transport *http.Transport) diagnosticResult {
	var nextProtos []string
	if transport.TLSClientConfig != nil {
		nextProtos = transport.TLSClientConfig.NextProtos
	}
	profileName := "standard"
	if profile == diagnosticProfileLunaLite {
		profileName = "luna-lite"
	} else if profile == diagnosticProfileLunaLitePlainText {
		profileName = "luna-lite-plain-text"
	} else if profile == diagnosticProfileLunaLiteCompletion {
		profileName = "luna-lite-completion"
	} else if profile == diagnosticProfileLunaLiteHighCompletion {
		profileName = "luna-lite-high-completion"
	}
	result := diagnosticResult{
		SchemaVersion: 1, Profile: profileName, Reason: "unknown", Code: "other", Field: "other", Transport: "none", BodyReadResult: "not_observed",
		ConnectComplete: p.connected.Load(), TLSHandshakeFinished: p.tlsHandshakeDone.Load(),
		TLSHandshakeComplete: p.tlsHandshakeOK.Load(), TLSNegotiatedProtocol: diagnosticProtocolName(p.tlsProtocol.Load()),
		TransportALPNConfig: diagnosticNextProtosLabel(nextProtos), RequestHeadersWritten: p.requestHeadersSent.Load(),
		RequestWriteFinished: p.requestWriteDone.Load(), RequestWriteComplete: p.requestWriteOK.Load(),
	}
	if isCompletionDiagnosticProfile(profile) {
		result.Leg, result.Model = "direct", "gpt-6-luna"
		result.ReasoningEffort, result.ReasoningContext = "medium", "all_turns"
		if profile == diagnosticProfileLunaLiteHighCompletion {
			result.ReasoningEffort = "high"
		}
	}
	return result
}

func validDiagnosticResult(result diagnosticResult) bool {
	if result.SchemaVersion != 1 || (result.Profile != "standard" && result.Profile != "luna-lite" && result.Profile != "luna-lite-plain-text" && result.Profile != "luna-lite-completion" && result.Profile != "luna-lite-high-completion") {
		return false
	}
	if result.Profile == "luna-lite-completion" || result.Profile == "luna-lite-high-completion" {
		wantEffort := "medium"
		if result.Profile == "luna-lite-high-completion" {
			wantEffort = "high"
		}
		if result.Leg != "direct" || result.Model != "gpt-6-luna" || result.ReasoningEffort != wantEffort || result.ReasoningContext != "all_turns" {
			return false
		}
	} else if result.Leg != "" || result.Model != "" || result.ReasoningEffort != "" || result.ReasoningContext != "" {
		return false
	}
	if result.Status != nil && (*result.Status < 100 || *result.Status > 599) {
		return false
	}
	if !oneOf(result.Reason, "accepted", "unknown", "instructions_missing", "model_unsupported", "unsupported_field", "invalid_input", "service_error") {
		return false
	}
	if !oneOf(result.Code, "other", "invalid_request_error", "unsupported_parameter", "unsupported_value", "model_not_found", "insufficient_quota", "rate_limit_exceeded", "server_error") {
		return false
	}
	if !oneOf(result.Field, "other", "model", "input", "instructions", "stream", "store", "max_output_tokens", "reasoning", "reasoning.effort", "reasoning.context", "include", "tool_choice", "parallel_tool_calls") {
		return false
	}
	if !oneOf(result.Transport, "none", "dns_failure", "connect_failure", "tls_verification", "tls_protocol_error", "unexpected_eof", "connection_closed", "permission_denied", "connection_reset", "deadline", "canceled", "malformed_http_response", "invalid_header", "content_length", "proxy_error", "unknown") {
		return false
	}
	if !oneOf(result.TLSNegotiatedProtocol, "none", "http1", "http2", "other") || !oneOf(result.TransportALPNConfig, "none", "http1", "http2", "other") {
		return false
	}
	if !oneOf(result.BodyReadResult, "not_observed", "initial_chunk", "complete", "read_failure", "size_limit") {
		return false
	}
	if (result.Transport == "none") != (result.Status != nil) {
		return false
	}
	if (result.Profile == "luna-lite-completion" || result.Profile == "luna-lite-high-completion") && result.Status != nil && *result.Status >= 200 && *result.Status < 300 && result.Completion == nil {
		return false
	}
	if result.Completion != nil && !validDiagnosticStreamResult(*result.Completion) {
		return false
	}
	return true
}

func validDiagnosticStreamResult(result diagnosticStreamResult) bool {
	if !oneOf(result.StreamStatus, "complete", "failed", "incomplete", "read_failure", "size_limit", "output_limit", "malformed", "trailing_data") {
		return false
	}
	if !oneOf(result.TerminalStatus, "none", "completed", "failed", "incomplete", "trailing_data") || result.ReasoningItemCount < 0 {
		return false
	}
	if result.ReasoningTokens != nil && *result.ReasoningTokens < 0 {
		return false
	}
	if (result.StreamStatus == "complete") != (result.DrainedToEOF && result.TerminalStatus == "completed") {
		return false
	}
	for _, run := range result.EventOrder {
		if run.Count <= 0 || !oneOf(run.Type, "response.created", "response.in_progress", "response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done", "response.output_text.delta", "response.output_text.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.completed", "response.failed", "response.incomplete", "error", "unknown") {
			return false
		}
	}
	return true
}

func oneOf(value string, allowed ...string) bool {
	return slices.Contains(allowed, value)
}

func diagnosticOutputFlags(observed int64, err error) (outputObserved, clientCanceled bool) {
	return observed > 0, observed == 256 && err == nil
}

func writeDiagnosticResult(path string, result diagnosticResult) error {
	if !validDiagnosticResult(result) || validateDiagnosticResultPath(path) != nil {
		return errors.New("invalid diagnostic result")
	}
	return writeDiagnosticResultFile(path, result)
}

func validateDiagnosticResultPath(path string) error {
	if filepath.Dir(path) != diagnosticResultDirectory {
		return errors.New("invalid diagnostic result path")
	}
	return validateDiagnosticResultPathInDirectory(path, diagnosticResultDirectory)
}

func validateDiagnosticResultPathInDirectory(path, directory string) error {
	if filepath.Dir(path) != directory || !diagnosticResultName.MatchString(filepath.Base(path)) {
		return errors.New("invalid diagnostic result path")
	}
	if err := ensureDiagnosticResultDirectory(directory); err != nil {
		return errors.New("diagnostic result directory unavailable")
	}
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("diagnostic result already exists or is unavailable")
	}
	return nil
}

func ensureDiagnosticResultDirectory(directory string) error {
	parent := filepath.Dir(directory)
	if err := verifyDiagnosticDirectory(parent, false); err != nil {
		return err
	}
	if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return verifyDiagnosticDirectory(directory, true)
}

func verifyDiagnosticDirectory(path string, private bool) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("unsafe diagnostic directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("unsafe diagnostic directory owner")
	}
	permissions := info.Mode().Perm()
	if (private && permissions != 0700) || (!private && permissions&0022 != 0) {
		return errors.New("unsafe diagnostic directory permissions")
	}
	return nil
}

func writeDiagnosticResultFile(path string, result diagnosticResult) error {
	data, err := json.Marshal(result)
	if err != nil {
		return errors.New("diagnostic result encoding failed")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("diagnostic result file unavailable")
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return errors.New("diagnostic result permissions failed")
	}
	n, err := file.Write(data)
	if err != nil || n != len(data) {
		_ = file.Close()
		_ = os.Remove(path)
		return errors.New("diagnostic result write failed")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return errors.New("diagnostic result sync failed")
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return errors.New("diagnostic result close failed")
	}
	return nil
}

func writeDiagnosticResultInDirectory(path, directory string, result diagnosticResult) error {
	if !validDiagnosticResult(result) || validateDiagnosticResultPathInDirectory(path, directory) != nil {
		return errors.New("invalid diagnostic result")
	}
	return writeDiagnosticResultFile(path, result)
}

func observeDiagnosticHTTPResponse(resp *http.Response, result diagnosticResult, save func(diagnosticResult) error) (diagnosticResult, error) {
	status := resp.StatusCode
	result.Status, result.Transport = &status, "none"
	if status >= 200 && status < 300 {
		observed, readErr := io.CopyN(io.Discard, resp.Body, 256)
		result.Reason, result.BodyReadResult = "accepted", "initial_chunk"
		result.OutputObserved, result.ClientCanceled = diagnosticOutputFlags(observed, readErr)
		if errors.Is(readErr, io.EOF) {
			result.BodyReadResult = "complete"
		} else if readErr != nil {
			result.BodyReadResult = "read_failure"
			if save != nil && save(result) != nil {
				return result, errors.New("diagnostic result save failed")
			}
			return result, errors.New("diagnostic response body read failed")
		}
		if save != nil && save(result) != nil {
			return result, errors.New("diagnostic result save failed")
		}
		return result, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, diagnosticBodyLimit+1))
	if err != nil {
		clear(body)
		result.BodyReadResult = "read_failure"
		if save != nil && save(result) != nil {
			return result, errors.New("diagnostic result save failed")
		}
		return result, errors.New("diagnostic response body read failed")
	}
	if len(body) > diagnosticBodyLimit {
		clear(body)
		result.BodyReadResult = "size_limit"
		if save != nil && save(result) != nil {
			return result, errors.New("diagnostic result save failed")
		}
		return result, errors.New("diagnostic response body exceeded limit")
	}
	result.BodyReadResult = "complete"
	result.Reason, result.Code, result.Field = classifyDiagnosticError(body)
	clear(body)
	if save != nil && save(result) != nil {
		return result, errors.New("diagnostic result save failed")
	}
	return result, nil
}

func observeDiagnosticSSE(body io.Reader) diagnosticStreamResult {
	result := diagnosticStreamResult{StreamStatus: "incomplete", TerminalStatus: "none", EventOrder: []diagnosticEventRun{}}
	limited := &io.LimitedReader{R: body, N: diagnosticBodyLimit + 1}
	reader := bufio.NewReaderSize(limited, 4096)
	var total int64
	var eventName string
	var data []byte
	dataLines := false
	terminalSeen := false
	outputLimit := false
	outputBytes := 0

	processEvent := func() {
		if !dataLines {
			eventName, data, dataLines = "", data[:0], false
			return
		}
		categoryName := eventName
		var payload struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
			Text  string `json:"text"`
			Part  struct {
				Text string `json:"text"`
			} `json:"part"`
			Item struct {
				Type string `json:"type"`
			} `json:"item"`
			Response struct {
				Status string `json:"status"`
			} `json:"response"`
		}
		validJSON := json.Unmarshal(data, &payload) == nil
		if categoryName == "" && validJSON {
			categoryName = payload.Type
		}
		category := diagnosticEventCategory(categoryName)
		appendDiagnosticEvent(&result.EventOrder, category)
		if terminalSeen {
			result.TerminalStatus = "trailing_data"
			result.StreamStatus = "trailing_data"
		} else {
			if category == "response.output_item.added" && validJSON && payload.Item.Type == "reasoning" {
				result.ReasoningItemCount++
			}
			if usage, found, _ := observeUsage(data); found {
				result.UsageObserved = true
				result.ReasoningTokens = usage.ReasoningTokens
			}
			if terminal := observeTerminal(eventName, data); terminal != nil {
				terminalSeen = true
				switch category {
				case "response.completed":
					if validJSON && payload.Type == category && payload.Response.Status == "completed" {
						result.TerminalStatus = "completed"
					} else {
						result.TerminalStatus = "incomplete"
					}
				case "response.failed", "error":
					result.TerminalStatus = "failed"
				default:
					result.TerminalStatus = "incomplete"
				}
			}
		}
		if validJSON && category == "response.output_text.delta" && len(payload.Delta) > 0 {
			result.OutputTextObserved = true
			// Count only byte lengths; never retain output text in the result.
			// The caller cancels at the first frame that would exceed the cap.
			outputBytes += len(payload.Delta)
			if outputBytes > 256 {
				outputLimit = true
			}
		} else if validJSON && category == "response.output_text.done" && len(payload.Text) > outputBytes {
			result.OutputTextObserved = true
			outputBytes = len(payload.Text)
			outputLimit = outputBytes > 256
		} else if validJSON && category == "response.content_part.done" && len(payload.Part.Text) > outputBytes {
			result.OutputTextObserved = true
			outputBytes = len(payload.Part.Text)
			outputLimit = outputBytes > 256
		}
		clear(data)
		eventName, data, dataLines = "", data[:0], false
	}

	for {
		line, err := reader.ReadString('\n')
		total += int64(len(line))
		if total > diagnosticBodyLimit {
			result.StreamStatus = "size_limit"
			return result
		}
		if len(line) > 0 {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			if len(line) == 0 {
				processEvent()
				if outputLimit {
					result.StreamStatus = "output_limit"
					return result
				}
			} else if line[0] != ':' {
				field, value, hasValue := strings.Cut(line, ":")
				if hasValue && strings.HasPrefix(value, " ") {
					value = value[1:]
				}
				switch field {
				case "event":
					eventName = value
				case "data":
					dataLines = true
					data = append(data, value...)
					data = append(data, '\n')
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) == 0 && !dataLines && eventName == "" {
				result.DrainedToEOF = true
				if result.TerminalStatus == "completed" {
					result.StreamStatus = "complete"
				} else if result.TerminalStatus == "failed" {
					result.StreamStatus = "failed"
				}
			} else if errors.Is(err, io.EOF) {
				result.StreamStatus = "malformed"
			} else {
				result.StreamStatus = "read_failure"
			}
			return result
		}
	}
}

func diagnosticEventCategory(name string) string {
	if oneOf(name, "response.created", "response.in_progress", "response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done", "response.output_text.delta", "response.output_text.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.completed", "response.failed", "response.incomplete", "error") {
		return name
	}
	return "unknown"
}

func appendDiagnosticEvent(events *[]diagnosticEventRun, category string) {
	if last := len(*events) - 1; last >= 0 && (*events)[last].Type == category {
		(*events)[last].Count++
		return
	}
	*events = append(*events, diagnosticEventRun{Type: category, Count: 1})
}

func cancelDiagnosticStreamOnOutputLimit(cancel context.CancelFunc, stream diagnosticStreamResult) bool {
	if stream.StreamStatus != "output_limit" {
		return false
	}
	cancel()
	return true
}

func notifyDiagnosticConnectDone(ctx context.Context, err error) {
	if trace := httptrace.ContextClientTrace(ctx); trace != nil && trace.ConnectDone != nil {
		trace.ConnectDone("tcp", "", err)
	}
}

func openDiagnosticDB(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	u := &url.URL{Scheme: "file", Path: abs}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(ON)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("database unavailable")
	}
	return db, nil
}

// TestOperatorInferenceDiagnostic is opt-in and performs at most one direct POST.
func TestOperatorInferenceDiagnostic(t *testing.T) {
	dbPath, keyPath, account := os.Getenv("PESTIROUTE_DIAG_DB"), os.Getenv("PESTIROUTE_DIAG_KEY"), os.Getenv("PESTIROUTE_DIAG_ACCOUNT")
	profile, profileOK := parseDiagnosticRequestProfile(os.Getenv("PESTIROUTE_DIAG_PROFILE"))
	if dbPath == "" || keyPath == "" || account == "" || !profileOK {
		t.Skip("operator diagnostic requires explicit PESTIROUTE_DIAG_* settings")
	}
	resultPath := os.Getenv("PESTIROUTE_DIAGNOSTIC_RESULT")
	if resultPath != "" && validateDiagnosticResultPath(resultPath) != nil {
		t.Fatal("diagnostic result destination unavailable")
	}
	key, err := crypto.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal("diagnostic credential checkpoint failed")
	}
	db, err := openDiagnosticDB(dbPath)
	if err != nil {
		t.Fatal("diagnostic database checkpoint failed")
	}
	defer db.Close()
	repo := sqlite.NewCredentials(db)
	row, err := repo.Get(context.Background(), account, "oauth")
	if err != nil || row.ExpiresAt == nil || !row.ExpiresAt.After(time.Now()) {
		t.Fatal("diagnostic credential checkpoint failed")
	}
	plain, err := repo.GetDecrypted(context.Background(), account, "oauth", key)
	if err != nil {
		t.Fatal("diagnostic credential checkpoint failed")
	}
	defer clear(plain)
	bundle, err := decodeOAuthBundle(plain)
	if err != nil || !bundle.ExpiresAt.After(time.Now()) || bundle.ExpiresAt.UnixMilli() != row.ExpiresAt.UnixMilli() {
		t.Fatal("diagnostic credential checkpoint failed")
	}
	threadID, err := uuid.NewRandom()
	if err != nil {
		t.Fatal("diagnostic request construction failed")
	}
	requestBody, err := diagnosticRequestBody(profile, threadID.String())
	if err != nil {
		t.Fatal("diagnostic request construction failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexResponsesEndpoint, bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal("diagnostic request construction failed")
	}
	req.Header.Set("Authorization", "Bearer "+bundle.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", bundle.AccountID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")
	applyDiagnosticProfileHeaders(req, profile, threadID.String())
	phases := &diagnosticRequestPhases{}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), phases.trace()))
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	configureCodexTransport(transport)
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer transport.CloseIdleConnections()
	saveResult := func(result diagnosticResult) error {
		if resultPath == "" {
			return nil
		}
		return writeDiagnosticResult(resultPath, result)
	}
	resp, err := client.Do(req)
	if err != nil {
		result := phases.result(profile, transport)
		result.Transport = classifyDiagnosticTransportError(err)
		if saveResult(result) != nil {
			t.Fatal("diagnostic result save failed")
		}
		phases.log(t, "diagnostic_transport_failure="+result.Transport, transport)
		t.FailNow()
	}
	defer resp.Body.Close()
	result := phases.result(profile, transport)
	if isCompletionDiagnosticProfile(profile) && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		status := resp.StatusCode
		stream := observeDiagnosticSSE(resp.Body)
		result.Status, result.Transport, result.Reason = &status, "none", "accepted"
		result.Completion = &stream
		result.OutputObserved = stream.OutputTextObserved
		result.ClientCanceled = cancelDiagnosticStreamOnOutputLimit(cancel, stream)
		switch {
		case stream.DrainedToEOF:
			result.BodyReadResult = "complete"
		case stream.StreamStatus == "size_limit":
			result.BodyReadResult = "size_limit"
		case stream.StreamStatus == "read_failure":
			result.BodyReadResult = "read_failure"
		default:
			result.BodyReadResult = "initial_chunk"
		}
		if saveResult(result) != nil {
			t.Fatal("diagnostic result save failed")
		}
		phases.log(t, fmt.Sprintf("diagnostic_stream status=%d stream=%s terminal=%s reasoning_items=%d usage=%t reasoning_usage=%t", status, stream.StreamStatus, stream.TerminalStatus, stream.ReasoningItemCount, stream.UsageObserved, stream.ReasoningTokens != nil), transport)
		return
	}
	result, observeErr := observeDiagnosticHTTPResponse(resp, result, saveResult)
	if observeErr != nil {
		t.Fatal("diagnostic response observation failed")
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		phases.log(t, "diagnostic_result status=accepted reason=accepted code=other field=other", transport)
		return
	}
	phases.log(t, fmt.Sprintf("diagnostic_result status=%d reason=%s code=%s field=%s", resp.StatusCode, result.Reason, result.Code, result.Field), transport)
}

func TestClassifyDiagnosticErrorPrivacy(t *testing.T) {
	tests := []struct{ body, reason, code, field string }{
		{`{"error":{"message":"Instructions missing: PRIVATE","code":"invalid_request_error","param":"instructions"}}`, "instructions_missing", "invalid_request_error", "instructions"},
		{`{"error":{"message":"model is not found","code":"model_not_found","param":"model"}}`, "model_unsupported", "model_not_found", "model"},
		{`{"error":{"message":"PRIVATE account secret"}}`, "unknown", "other", "other"},
		{`not json PRIVATE`, "unknown", "other", "other"},
	}
	for _, tc := range tests {
		reason, code, field := classifyDiagnosticError([]byte(tc.body))
		if reason != tc.reason || code != tc.code || field != tc.field {
			t.Fatalf("unexpected fixed diagnostic labels: %s %s %s", reason, code, field)
		}
		if strings.Contains(strings.Join([]string{reason, code, field}, " "), "PRIVATE") {
			t.Fatal("diagnostic labels contained provider text")
		}
	}
}

func TestDiagnosticLunaLiteProfileShapeOffline(t *testing.T) {
	for _, tc := range []struct {
		value diagnosticRequestProfile
		want  string
	}{
		{diagnosticProfileStandard, "gpt-5.4-mini"},
		{diagnosticProfileLunaLite, "gpt-6-luna"},
		{diagnosticProfileLunaLitePlainText, "gpt-6-luna"},
		{diagnosticProfileLunaLiteCompletion, "gpt-6-luna"},
		{diagnosticProfileLunaLiteHighCompletion, "gpt-6-luna"},
	} {
		body, err := diagnosticRequestBody(tc.value, "00000000-0000-4000-8000-000000000001")
		if err != nil {
			t.Fatal("fixed diagnostic profile did not build")
		}
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal("fixed diagnostic profile was not valid JSON")
		}
		if request["model"] != tc.want || request["stream"] != true || request["store"] != false {
			t.Fatal("diagnostic profile core fields differed")
		}
		if tc.value == diagnosticProfileStandard {
			continue
		}
		if request["instructions"] != "" || request["tools"] != nil {
			t.Fatal("Lite top-level fields differed")
		}
		input, ok := request["input"].([]any)
		if !ok || len(input) != 2 {
			t.Fatal("Lite profile input prefix or message was missing")
		}
		prefix, ok := input[0].(map[string]any)
		if !ok || prefix["type"] != "additional_tools" || prefix["role"] != "developer" || prefix["tools"] == nil {
			t.Fatal("Lite additional-tools prefix differed")
		}
		tools, ok := prefix["tools"].([]any)
		if !ok || len(tools) != 0 {
			t.Fatal("Lite additional-tools list was not empty")
		}
		if _, ok := prefix["id"].(string); !ok || !strings.HasPrefix(prefix["id"].(string), "at_") {
			t.Fatal("Lite additional-tools identity was missing")
		}
		message, ok := input[1].(map[string]any)
		if !ok || message["type"] != "message" || message["role"] != "user" {
			t.Fatal("Lite user message shape differed")
		}
		if tc.value == diagnosticProfileLunaLite || isCompletionDiagnosticProfile(tc.value) {
			reasoning, ok := request["reasoning"].(map[string]any)
			wantEffort := "medium"
			if tc.value == diagnosticProfileLunaLiteHighCompletion {
				wantEffort = "high"
			}
			if !ok || reasoning["effort"] != wantEffort || reasoning["context"] != "all_turns" {
				t.Fatal("existing Lite reasoning profile changed")
			}
			if tc.value == diagnosticProfileLunaLiteHighCompletion {
				if request["include"] == nil || request["summary"] != nil {
					t.Fatal("high-effort proof profile changed encrypted-content markers")
				}
				content := input[1].(map[string]any)["content"].([]any)
				part := content[0].(map[string]any)
				if part["text"] != "What is 137 × 293? Reply with only the number." {
					t.Fatal("high-effort proof prompt differed")
				}
			}
		} else {
			fullBody, err := diagnosticRequestBody(diagnosticProfileLunaLite, "00000000-0000-4000-8000-000000000001")
			if err != nil {
				t.Fatal("known Lite profile did not build")
			}
			var fullRequest map[string]any
			if err := json.Unmarshal(fullBody, &fullRequest); err != nil {
				t.Fatal("known Lite profile was not valid JSON")
			}
			delete(fullRequest, "reasoning")
			if !reflect.DeepEqual(request, fullRequest) || len(request) != len(fullRequest) {
				t.Fatal("plain-text preflight was not exactly the reviewed Lite profile minus reasoning")
			}
			content, ok := message["content"].([]any)
			if !ok || len(content) != 1 {
				t.Fatal("minimal Lite user input differed")
			}
			part, ok := content[0].(map[string]any)
			if !ok || part["type"] != "input_text" || part["text"] != "Reply with the word OK." {
				t.Fatal("minimal Lite user text differed")
			}
		}
	}
	for _, value := range []string{"", "gpt-5.4-mini", "gpt-6-luna", "arbitrary"} {
		if _, ok := parseDiagnosticRequestProfile(value); ok {
			t.Fatal("diagnostic profile selection accepted an unlisted value")
		}
	}
	for _, value := range []string{"standard", "luna-lite", "luna-lite-plain-text", "luna-lite-completion", "luna-lite-high-completion"} {
		if _, ok := parseDiagnosticRequestProfile(value); !ok {
			t.Fatal("fixed diagnostic profile was not accepted")
		}
	}
	for _, param := range []string{"reasoning", "reasoning.effort", "reasoning.context", "include", "tools", "tool_choice", "parallel_tool_calls"} {
		body, err := json.Marshal(map[string]any{"error": map[string]string{"message": "unsupported value", "code": "unsupported_value", "param": param}})
		if err != nil {
			t.Fatal("synthetic diagnostic error did not encode")
		}
		reason, code, field := classifyDiagnosticError(body)
		if reason != "unsupported_field" || code != "unsupported_value" || field != param {
			t.Fatalf("allowlisted field classification=%s/%s/%s for %s", reason, code, field, param)
		}
	}
	secretBody := []byte(`{"error":{"message":"unsupported value","code":"unsupported_value","param":"PRIVATE_TOKEN=secret"}}`)
	reason, code, field := classifyDiagnosticError(secretBody)
	if reason != "unknown" || code != "unsupported_value" || field != "other" {
		t.Fatal("unrecognized provider field escaped the fixed allowlist")
	}
	req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	threadID := "00000000-0000-4000-8000-000000000001"
	for _, profile := range []diagnosticRequestProfile{diagnosticProfileLunaLite, diagnosticProfileLunaLitePlainText, diagnosticProfileLunaLiteCompletion, diagnosticProfileLunaLiteHighCompletion} {
		req.Header = make(http.Header)
		applyDiagnosticProfileHeaders(req, profile, threadID)
		if req.Header.Get("x-openai-internal-codex-responses-lite") != "true" ||
			req.Header.Get("originator") != "pestiroute" ||
			req.Header.Get("User-Agent") != "PestiRoute" ||
			req.Header.Get("session-id") != threadID || req.Header.Get("thread-id") != threadID ||
			req.Header.Get("x-client-request-id") != threadID {
			t.Fatal("Lite profile headers differed")
		}
	}
	phases := &diagnosticRequestPhases{}
	result := phases.result(diagnosticProfileLunaLitePlainText, &http.Transport{})
	status := http.StatusOK
	result.Status, result.Reason, result.BodyReadResult = &status, "accepted", "initial_chunk"
	if result.Profile != "luna-lite-plain-text" || !validDiagnosticResult(result) {
		t.Fatal("new Lite preflight result profile was not accepted by the fixed artifact schema")
	}
	high := phases.result(diagnosticProfileLunaLiteHighCompletion, &http.Transport{})
	high.Status, high.Reason, high.BodyReadResult = &status, "accepted", "complete"
	high.Completion = &diagnosticStreamResult{DrainedToEOF: true, StreamStatus: "complete", TerminalStatus: "completed", EventOrder: []diagnosticEventRun{{Type: "response.completed", Count: 1}}}
	if !validDiagnosticResult(high) {
		t.Fatal("fixed high-effort direct completion evidence schema was rejected")
	}
}

func TestDiagnosticCompletionSSEObservationOffline(t *testing.T) {
	input := strings.Join([]string{
		`event: response.created`, `data: {"type":"response.created"}`, ``,
		`event: response.output_item.added`, `data: {"type":"response.output_item.added","item":{"id":"PRIVATE_ID","type":"reasoning","encrypted_content":"PRIVATE_CIPHERTEXT"}}`, ``,
		`event: response.reasoning_summary_text.delta`, `data: {"type":"response.reasoning_summary_text.delta","delta":"PRIVATE_REASONING_TEXT"}`, ``,
		`event: response.output_text.delta`, `data: {"type":"response.output_text.delta","delta":"OK"}`, ``,
		`event: response.completed`, `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":4,"output_tokens":3,"output_tokens_details":{"reasoning_tokens":2}}}}`, ``,
		``,
	}, "\n")
	got := observeDiagnosticSSE(strings.NewReader(input))
	wantOrder := []diagnosticEventRun{{Type: "response.created", Count: 1}, {Type: "response.output_item.added", Count: 1}, {Type: "response.reasoning_summary_text.delta", Count: 1}, {Type: "response.output_text.delta", Count: 1}, {Type: "response.completed", Count: 1}}
	if !got.DrainedToEOF || got.StreamStatus != "complete" || got.TerminalStatus != "completed" || got.ReasoningItemCount != 1 || !got.UsageObserved || got.ReasoningTokens == nil || *got.ReasoningTokens != 2 || !got.OutputTextObserved || !reflect.DeepEqual(got.EventOrder, wantOrder) {
		t.Fatalf("unexpected sanitized stream observation: %+v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil || strings.Contains(string(encoded), "PRIVATE") || strings.Contains(string(encoded), "OK") {
		t.Fatal("stream observation retained response payload or text")
	}
}

func TestDiagnosticCompletionSSEBoundsAndUnknownPrivacy(t *testing.T) {
	t.Run("unknown event", func(t *testing.T) {
		got := observeDiagnosticSSE(strings.NewReader("event: PRIVATE_EVENT\ndata: {\"type\":\"PRIVATE_EVENT\",\"private\":\"PRIVATE_PAYLOAD\"}\n\n"))
		if len(got.EventOrder) != 1 || got.EventOrder[0].Type != "unknown" || got.StreamStatus != "incomplete" {
			t.Fatalf("unknown event was not safely mapped: %+v", got)
		}
		encoded, _ := json.Marshal(got)
		if strings.Contains(string(encoded), "PRIVATE") {
			t.Fatal("unknown event name or data escaped the safe enum")
		}
	})
	t.Run("output limit", func(t *testing.T) {
		payload, err := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": strings.Repeat("x", 257)})
		if err != nil {
			t.Fatal("synthetic stream fixture failed")
		}
		stream := "event: response.output_text.delta\ndata: " + string(payload) + "\n\n"
		got := observeDiagnosticSSE(strings.NewReader(stream))
		if got.StreamStatus != "output_limit" || !got.OutputTextObserved || got.DrainedToEOF {
			t.Fatalf("output cap was not enforced: %+v", got)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if !cancelDiagnosticStreamOnOutputLimit(cancel, got) || !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatal("output cap did not cancel the bounded stream request")
		}
	})
	t.Run("output done limit without deltas", func(t *testing.T) {
		payload, err := json.Marshal(map[string]string{"type": "response.output_text.done", "text": strings.Repeat("x", 257)})
		if err != nil {
			t.Fatal("synthetic stream fixture failed")
		}
		stream := "event: response.output_text.done\ndata: " + string(payload) + "\n\n"
		got := observeDiagnosticSSE(strings.NewReader(stream))
		if got.StreamStatus != "output_limit" || !got.OutputTextObserved || got.DrainedToEOF {
			t.Fatalf("output-done cap was not enforced: %+v", got)
		}
	})
	t.Run("body limit", func(t *testing.T) {
		got := observeDiagnosticSSE(strings.NewReader(strings.Repeat("x", diagnosticBodyLimit+1)))
		if got.StreamStatus != "size_limit" || got.DrainedToEOF {
			t.Fatalf("body cap was not enforced: %+v", got)
		}
	})
	t.Run("failed terminal and EOF", func(t *testing.T) {
		got := observeDiagnosticSSE(strings.NewReader("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{}}\n\n"))
		if got.StreamStatus != "failed" || got.TerminalStatus != "failed" || !got.DrainedToEOF {
			t.Fatalf("failed terminal evidence was lost: %+v", got)
		}
	})
}

func TestDiagnosticCompletionErrorRetainsSanitizedHTTPClassification(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal("private test directory setup failed")
	}
	status := http.StatusBadRequest
	result := diagnosticResult{SchemaVersion: 1, Profile: "luna-lite-completion", Leg: "direct", Model: "gpt-6-luna", ReasoningEffort: "medium", ReasoningContext: "all_turns", Reason: "unknown", Code: "other", Field: "other", Transport: "none", Status: &status, TLSNegotiatedProtocol: "none", TransportALPNConfig: "none", BodyReadResult: "not_observed"}
	response := &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"unsupported_value","param":"reasoning.context","message":"PRIVATE provider text"}}`))}
	path := filepath.Join(directory, "luna-6-completion.json")
	result, err := observeDiagnosticHTTPResponse(response, result, func(result diagnosticResult) error {
		return writeDiagnosticResultInDirectory(path, directory, result)
	})
	if err != nil || result.Status == nil || *result.Status != status || result.Code != "unsupported_value" || result.Field != "reasoning.context" || result.Reason != "unknown" {
		t.Fatalf("known safe HTTP classification was not retained: %+v err=%v", result, err)
	}
	encoded, err := os.ReadFile(path)
	info, statErr := os.Stat(path)
	if err != nil || statErr != nil || info.Mode().Perm() != 0600 || strings.Contains(string(encoded), "PRIVATE") || !json.Valid(encoded) {
		t.Fatal("HTTP error artifact was invalid or exposed provider text")
	}
	var saved diagnosticResult
	if err := json.Unmarshal(encoded, &saved); err != nil || saved.Status == nil || *saved.Status != status || saved.Code != "unsupported_value" || saved.Field != "reasoning.context" || !validDiagnosticResult(saved) {
		t.Fatal("durable HTTP error artifact lost its fixed cause")
	}
}

func TestClassifyDiagnosticDetailPrivacy(t *testing.T) {
	tests := []struct {
		body, reason, field string
	}{
		{`{"detail":"Instructions are required"}`, "instructions_missing", "instructions"},
		{`{"detail":"Request rejected: instructions are required, please configure them. PRIVATE_TOKEN=secret"}`, "instructions_missing", "instructions"},
		{`{"error":{"detail":"unsupported parameter: max_output_tokens"}}`, "unsupported_field", "max_output_tokens"},
		{`{"error":{"detail":"Invalid request! Unsupported parameter 'max_output_tokens'. PRIVATE_TOKEN=secret.invalid"}}`, "unsupported_field", "max_output_tokens"},
		{`{"error":{"param":"store","detail":"Unsupported parameter max_output_tokens"}}`, "unsupported_field", "store"},
		{`{"detail":"model not found"}`, "model_unsupported", "model"},
		{`{"detail":"The selected model is not found. PRIVATE=secret"}`, "model_unsupported", "model"},
		{`{"detail":"input is invalid"}`, "invalid_input", "input"},
		{`{"detail":"PRIVATE secret with max_output_tokens and unsupported parameter"}`, "unsupported_field", "max_output_tokens"},
		{`{"detail":"PRIVATE secret: instructions are required for secret.invalid"}`, "instructions_missing", "instructions"},
	}
	for _, tc := range tests {
		reason, _, field := classifyDiagnosticError([]byte(tc.body))
		if reason != tc.reason || field != tc.field {
			t.Fatalf("detail classification=%s/%s, want %s/%s", reason, field, tc.reason, tc.field)
		}
		if strings.Contains(strings.Join([]string{reason, field}, " "), "PRIVATE") || strings.Contains(strings.Join([]string{reason, field}, " "), "secret") {
			t.Fatal("detail classification exposed provider text")
		}
		if field != "other" && field != "instructions" && field != "max_output_tokens" && field != "model" && field != "input" && field != "store" {
			t.Fatal("detail classification emitted a non-allowlisted field")
		}
	}
}

type poisonedDiagnosticError struct {
	cause error
	seen  *atomic.Bool
}

func (e *poisonedDiagnosticError) Error() string {
	e.seen.Store(true)
	return "PRIVATE_URL=secret.invalid PRIVATE_CERT=secret"
}
func (e *poisonedDiagnosticError) Unwrap() error { return e.cause }

type diagnosticTimeoutError struct{ seen *atomic.Bool }

func (e *diagnosticTimeoutError) Error() string {
	e.seen.Store(true)
	return "PRIVATE_TIMEOUT_DETAIL=secret"
}
func (*diagnosticTimeoutError) Timeout() bool   { return true }
func (*diagnosticTimeoutError) Temporary() bool { return true }

func TestClassifyDiagnosticTransportErrorPrivacy(t *testing.T) {
	private := "PRIVATE_URL=secret.invalid PRIVATE_CERT=secret"
	stringified := &atomic.Bool{}
	poison := func(err error) error { return &poisonedDiagnosticError{cause: err, seen: stringified} }
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"dns through url wrapper", &url.Error{Op: "Post", URL: "https://" + private, Err: poison(&net.DNSError{Err: private, Name: private})}, "dns_failure"},
		{"connect", poison(&net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{}, Err: errors.New(private)}), "connect_failure"},
		{"certificate verification", &url.Error{Op: "Post", URL: private, Err: poison(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}})}, "tls_verification"},
		{"deadline", poison(context.DeadlineExceeded), "deadline"},
		{"generic timeout before dial classification", poison(&net.OpError{Op: "dial", Err: &diagnosticTimeoutError{seen: stringified}}), "deadline"},
		{"canceled", poison(context.Canceled), "canceled"},
		{"reset", poison(fmt.Errorf("%s: %w", private, syscall.ECONNRESET)), "connection_reset"},
		{"broken pipe", poison(syscall.EPIPE), "connection_reset"},
		{"refused", poison(syscall.ECONNREFUSED), "connect_failure"},
		{"network unreachable", poison(syscall.ENETUNREACH), "connect_failure"},
		{"host unreachable", poison(syscall.EHOSTUNREACH), "connect_failure"},
		{"permission denied", poison(syscall.EACCES), "permission_denied"},
		{"operation not permitted", poison(syscall.EPERM), "permission_denied"},
		{"unexpected eof", poison(io.ErrUnexpectedEOF), "unexpected_eof"},
		{"eof", poison(io.EOF), "connection_closed"},
		{"unknown", poison(errors.New(private)), "unknown"},
		{"unknown text fallback", &url.Error{Op: "Post", URL: private, Err: errors.New(private)}, "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stringified.Store(false)
			got := classifyDiagnosticTransportError(tc.err)
			if got != tc.want {
				t.Fatalf("transport class=%s, want %s", got, tc.want)
			}
			if stringified.Load() {
				t.Fatal("transport classifier inspected an error string")
			}
			if strings.Contains(got, "PRIVATE") || strings.Contains(got, "secret") {
				t.Fatal("transport class exposed wrapped error details")
			}
		})
	}
	if reason, code, field := classifyDiagnosticError([]byte(`{"error":{"message":"unsupported parameter","code":"unsupported_parameter","param":"store"}}`)); reason != "unsupported_field" || code != "unsupported_parameter" || field != "store" {
		t.Fatalf("HTTP provider error classification changed: %s %s %s", reason, code, field)
	}
	for _, tc := range []struct{ text, want string }{
		{"net/http: HTTP/1.x transport connection broken: malformed HTTP response PRIVATE", "malformed_http_response"},
		{"net/http: invalid header field PRIVATE", "invalid_header"},
		{"malformed MIME header line PRIVATE", "invalid_header"},
		{"net/http: invalid Content-Length PRIVATE", "content_length"},
		{"proxy error PRIVATE_URL=secret", "proxy_error"},
		{"PRIVATE_URL=secret", "unknown"},
	} {
		if got := classifyDiagnosticHTTPErrorText(tc.text); got != tc.want || strings.Contains(got, "PRIVATE") || strings.Contains(got, "secret") {
			t.Fatalf("HTTP parser text class=%q want %q", got, tc.want)
		}
	}
}

func TestDiagnosticHTTPTraceInjectedFailures(t *testing.T) {
	assertPhases := func(t *testing.T, phases *diagnosticRequestPhases, connected, tlsDone, tlsOK, headers, writeDone, writeOK bool) {
		t.Helper()
		if phases.connected.Load() != connected || phases.tlsHandshakeDone.Load() != tlsDone ||
			phases.tlsHandshakeOK.Load() != tlsOK || phases.requestHeadersSent.Load() != headers ||
			phases.requestWriteDone.Load() != writeDone || phases.requestWriteOK.Load() != writeOK {
			t.Fatalf("phase flags connect=%t tls_finished=%t tls_complete=%t headers=%t write_finished=%t write_complete=%t",
				phases.connected.Load(), phases.tlsHandshakeDone.Load(), phases.tlsHandshakeOK.Load(),
				phases.requestHeadersSent.Load(), phases.requestWriteDone.Load(), phases.requestWriteOK.Load())
		}
	}
	newRequest := func(t *testing.T, target string, phases *diagnosticRequestPhases) *http.Request {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			t.Fatal("request construction failed")
		}
		return req.WithContext(httptrace.WithClientTrace(req.Context(), phases.trace()))
	}

	t.Run("injected connect refusal", func(t *testing.T) {
		phases := &diagnosticRequestPhases{}
		transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			err := &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
			notifyDiagnosticConnectDone(ctx, err)
			return nil, err
		}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport}
		_, err := client.Do(newRequest(t, "http://diagnostic.invalid/", phases))
		if err == nil || classifyDiagnosticTransportError(err) != "connect_failure" {
			t.Fatal("injected dial failure was not classified")
		}
		assertPhases(t, phases, false, false, false, false, false, false)
	})

	t.Run("injected tls protocol failure", func(t *testing.T) {
		phases := &diagnosticRequestPhases{}
		transport := &http.Transport{Proxy: nil, TLSHandshakeTimeout: time.Second, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			clientConn, serverConn := net.Pipe()
			notifyDiagnosticConnectDone(ctx, nil)
			go func() {
				defer serverConn.Close()
				header := make([]byte, 5)
				if _, err := io.ReadFull(serverConn, header); err != nil {
					return
				}
				_, _ = io.CopyN(io.Discard, serverConn, int64(binary.BigEndian.Uint16(header[3:5])))
				_, _ = io.WriteString(serverConn, "not tls\r\n")
			}()
			return clientConn, nil
		}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport}
		_, err := client.Do(newRequest(t, "https://diagnostic.invalid/", phases))
		if err == nil || classifyDiagnosticTransportError(err) != "tls_protocol_error" {
			t.Fatal("injected TLS failure did not produce expected fixed class")
		}
		assertPhases(t, phases, true, true, false, false, false, false)
	})

	t.Run("response eof after request write", func(t *testing.T) {
		phases := &diagnosticRequestPhases{}
		transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			clientConn, serverConn := net.Pipe()
			notifyDiagnosticConnectDone(ctx, nil)
			go func() {
				defer serverConn.Close()
				reader := bufio.NewReader(serverConn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil || line == "\r\n" {
						break
					}
				}
				_, _ = io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nContent-Length: 1\r\n")
			}()
			return clientConn, nil
		}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport}
		_, err := client.Do(newRequest(t, "http://diagnostic.invalid/", phases))
		if err == nil || classifyDiagnosticTransportError(err) != "unexpected_eof" {
			t.Fatal("injected response EOF was not classified")
		}
		assertPhases(t, phases, true, false, false, true, true, true)
	})

	for _, tc := range []struct{ name, response, want string }{
		{"malformed status response", "not an HTTP response PRIVATE", "malformed_http_response"},
		{"invalid content length", "HTTP/1.1 200 OK\r\nContent-Length: invalid\r\n\r\n", "content_length"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phases := &diagnosticRequestPhases{}
			transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				clientConn, serverConn := net.Pipe()
				notifyDiagnosticConnectDone(ctx, nil)
				go func() {
					defer serverConn.Close()
					reader := bufio.NewReader(serverConn)
					for {
						line, err := reader.ReadString('\n')
						if err != nil || line == "\r\n" {
							break
						}
					}
					_, _ = io.WriteString(serverConn, tc.response)
				}()
				return clientConn, nil
			}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport}
			_, err := client.Do(newRequest(t, "http://diagnostic.invalid/", phases))
			if err == nil || classifyDiagnosticTransportError(err) != tc.want {
				t.Fatalf("HTTP parse failure class=%q, want %q", classifyDiagnosticTransportError(err), tc.want)
			}
		})
	}
}

func TestDiagnosticProtocolLabelsAreAllowlisted(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"http/1.1", "http1"}, {"h2", "http2"}, {"", "none"}, {"PRIVATE_PROTOCOL", "other"},
	} {
		if got := diagnosticProtocolLabel(tc.input); got != tc.want {
			t.Fatalf("protocol label=%q, want %q", got, tc.want)
		}
	}
	for _, tc := range []struct {
		input []string
		want  string
	}{
		{nil, "none"}, {[]string{"http/1.1"}, "http1"}, {[]string{"h2"}, "http2"}, {[]string{"h2", "http/1.1"}, "other"}, {[]string{"PRIVATE"}, "other"},
	} {
		if got := diagnosticNextProtosLabel(tc.input); got != tc.want {
			t.Fatalf("transport ALPN label=%q, want %q", got, tc.want)
		}
	}
}

func TestDiagnosticResultFileIsSanitizedExclusiveAndPrivate(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal("private test directory setup failed")
	}
	status := 200
	result := diagnosticResult{
		SchemaVersion: 1, Profile: "luna-lite", Status: &status, Reason: "accepted", Code: "other", Field: "other", Transport: "none", BodyReadResult: "initial_chunk",
		TLSNegotiatedProtocol: "http1", TransportALPNConfig: "http1", OutputObserved: true,
	}
	path := filepath.Join(directory, "luna-2.json")
	if err := writeDiagnosticResultInDirectory(path, directory, result); err != nil {
		t.Fatal("sanitized result write failed")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("diagnostic result was not created mode 0600")
	}
	data, err := os.ReadFile(path)
	if err != nil || !json.Valid(data) || bytes.Contains(data, []byte("PRIVATE")) {
		t.Fatal("diagnostic result was not safe JSON")
	}
	if err := validateDiagnosticResultPathInDirectory(path, directory); err == nil {
		t.Fatal("existing diagnostic result path was accepted")
	}
	result.Reason = "PRIVATE_PROVIDER_TEXT"
	if err := writeDiagnosticResultInDirectory(filepath.Join(directory, "luna-3.json"), directory, result); err == nil {
		t.Fatal("unknown diagnostic label was written")
	}
}

func TestDiagnosticResultRejectsUnsafeDirectoryPermissions(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal("test directory setup failed")
	}
	if err := validateDiagnosticResultPathInDirectory(filepath.Join(directory, "luna-3.json"), directory); err == nil {
		t.Fatal("world-readable diagnostic directory was accepted")
	}
}

type diagnosticFaultingBody struct{}

func (diagnosticFaultingBody) Read(p []byte) (int, error) {
	return copy(p, []byte("PRIVATE_RESPONSE_BODY")), errors.New("PRIVATE_PROVIDER_READ_ERROR")
}

func TestDiagnosticHTTPBodyFailureArtifactsRetainOnlySanitizedStatus(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal("private test directory setup failed")
	}
	for _, tc := range []struct {
		name, file, bodyReadResult string
		body                       io.Reader
	}{
		{"reader failure", "luna-7.json", "read_failure", diagnosticFaultingBody{}},
		{"oversize response", "luna-8.json", "size_limit", strings.NewReader(strings.Repeat("PRIVATE_RESPONSE_BODY", diagnosticBodyLimit/21+2))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(directory, tc.file)
			result := diagnosticResult{
				SchemaVersion: 1, Profile: "luna-lite", Reason: "unknown", Code: "other", Field: "other", Transport: "none",
				TLSNegotiatedProtocol: "none", TransportALPNConfig: "none", BodyReadResult: "not_observed",
			}
			response := &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(tc.body)}
			result, observeErr := observeDiagnosticHTTPResponse(response, result, func(result diagnosticResult) error {
				return writeDiagnosticResultInDirectory(path, directory, result)
			})
			if observeErr == nil || result.Status == nil || *result.Status != http.StatusTooManyRequests || result.BodyReadResult != tc.bodyReadResult {
				t.Fatal("body failure did not preserve its fixed result classification")
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("body failure artifact missing or not mode 0600")
			}
			data, err := os.ReadFile(path)
			if err != nil || !json.Valid(data) || bytes.Contains(data, []byte("PRIVATE")) {
				t.Fatal("body failure artifact was invalid or exposed provider data")
			}
			var saved diagnosticResult
			if err := json.Unmarshal(data, &saved); err != nil || saved.Status == nil || *saved.Status != http.StatusTooManyRequests || saved.BodyReadResult != tc.bodyReadResult {
				t.Fatal("saved result lost the HTTP status or safe body-read classification")
			}
		})
	}
}

func TestDiagnosticOutputFlagsReflectBoundedObservation(t *testing.T) {
	for _, tc := range []struct {
		observed int64
		err      error
		output   bool
		canceled bool
	}{
		{0, io.EOF, false, false},
		{32, io.EOF, true, false},
		{256, nil, true, true},
	} {
		output, canceled := diagnosticOutputFlags(tc.observed, tc.err)
		if output != tc.output || canceled != tc.canceled {
			t.Fatal("diagnostic output flags did not match bounded local observation")
		}
	}
}
