package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/connector/codex"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
	"github.com/blestafist/pestiroute/internal/testutil/codexfixtures"
)

const operatorGatewayToolOptIn = "PESTIROUTE_DIAG_GATEWAY_TOOL_ROUNDTRIP"

type gatewayToolResult struct {
	Status                    int            `json:"status"`
	RequestStatuses           []int          `json:"request_statuses"`
	Terminal                  string         `json:"terminal"`
	Terminals                 []string       `json:"terminals"`
	Usage                     string         `json:"usage"`
	InputTokens               int64          `json:"input_tokens"`
	OutputTokens              int64          `json:"output_tokens"`
	Dispatches                *int32         `json:"target_dispatches,omitempty"`
	DialAttempts              int32          `json:"network_dial_attempts"`
	Retries                   int            `json:"retries"`
	Calls                     int            `json:"calls"`
	SyntheticToolResultLinked bool           `json:"synthetic_tool_result_linked"`
	AnswerObserved            bool           `json:"answer_observed"`
	DeltaSnapshotEqual        bool           `json:"delta_snapshot_equal"`
	NormalizedMarkerEqual     bool           `json:"normalized_marker_equal"`
	FailureCode               string         `json:"error_code,omitempty"`
	FailureField              string         `json:"error_field,omitempty"`
	ReaderResult              string         `json:"reader_result"`
	ResponseBytes             int            `json:"response_bytes"`
	EventCounts               map[string]int `json:"event_counts"`
	TransportClass            string         `json:"transport_class"`
}

const gatewayToolStageEnv = "PESTIROUTE_GATEWAY_TOOL_STAGE"

const gatewayToolFunctionFixture = codexfixtures.FunctionInitial
const gatewayToolCustomFixture = codexfixtures.CustomInitial

// TestOperatorGatewayToolRoundtripLive is deliberately inert without explicit operator authorization.
func TestOperatorGatewayToolRoundtripLive(t *testing.T) {
	if os.Getenv(operatorGatewayToolOptIn) != "authorized" {
		t.Skip("set explicit gateway-tool roundtrip opt-in to authorize up to four upstream requests")
	}
	paths, err := gatewayToolPathsFromEnv()
	if err != nil {
		t.Fatal("operator gateway-tool inputs or destinations invalid")
	}
	reserved, err := reserveGatewayToolArtifacts(paths)
	if err != nil {
		t.Fatal("operator gateway-tool artifacts are not safely available")
	}
	defer reserved.closeAndRemove()
	account, credential, _, _ := loadOperatorPairCredential(t, paths.db, paths.key, "selected-A")
	db, issued := provisionOperatorPairGatewayWithRPM(t, account, credential, 4)
	connector := codex.NewConnector()
	counter := &operatorPairDispatchCounter{}
	doer := gatewayToolHTTPClient(connector, counter, "")
	if _, ok := doer.(*http.Client); !ok {
		t.Fatal("instrumentation did not preserve *http.Client")
	}
	server, closeGateway := startOperatorPairGateway(t, db, paths.key, account.ID, connector, doer)
	defer closeGateway()
	defer connector.Close(context.Background())

	for _, stage := range gatewayToolStagePlan(paths.stage) {
		kind := [...]string{"function", "custom"}[stage]
		if stage == 1 && paths.stage == "both" && (reserved.records[0].Status != 200 || reserved.records[0].Terminal != "completed") {
			break
		}
		start := counter.dispatches.Load()
		result, err := gatewayToolStage(t, server.URL, issued.Secret, kind)
		result.DialAttempts = counter.dispatches.Load() - start
		if err == nil {
			dispatches := int32(2) // Two completed HTTP/SSE responses prove two target POSTs.
			result.Dispatches = &dispatches
		} else if result.DialAttempts == 0 {
			zero := int32(0)
			result.Dispatches = &zero
		}
		if err != nil || result.Status != 200 || result.Terminal != "completed" || result.Usage != "reported" || result.Calls != 1 || result.Dispatches == nil || *result.Dispatches != 2 || result.DialAttempts != 2 || !result.SyntheticToolResultLinked || !result.AnswerObserved || !result.NormalizedMarkerEqual || len(result.RequestStatuses) != 2 || result.RequestStatuses[0] != 200 || result.RequestStatuses[1] != 200 || len(result.Terminals) != 2 || result.Terminals[0] != "completed" || result.Terminals[1] != "completed" {
			_ = reserved.write(stage, result)
			t.Fatalf("gateway %s stage stopped safely: status=%d terminal=%s usage=%s calls=%d target_dispatches=%v network_dials=%d", kind, result.Status, result.Terminal, result.Usage, result.Calls, result.Dispatches, result.DialAttempts)
		}
		reserved.records[stage] = result
		if err := reserved.write(stage, result); err != nil {
			t.Fatal("sanitized gateway artifact could not be written")
		}
	}
}

// Use the same concrete-client transport instrumentation in live and fake runs.
// Codex deliberately rejects generic HTTPDoer wrappers before issuing a request.
func gatewayToolHTTPClient(connector *codex.Connector, counter *operatorPairDispatchCounter, loopback string) core.HTTPDoer {
	client, ok := connector.HTTPDoer().(*http.Client)
	if !ok || client == nil {
		return nil
	}
	if loopback != "" {
		transport, ok := client.Transport.(*http.Transport)
		if !ok {
			return nil
		}
		transport = transport.Clone()
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", loopback)
		}
		client.Transport = transport
	}
	return instrumentOperatorPairClient(connector, counter)
}

type gatewayToolPaths struct{ db, key, function, custom, stage string }

func gatewayToolPathsFromEnv() (gatewayToolPaths, error) {
	p := gatewayToolPaths{db: os.Getenv("PESTIROUTE_DIAG_DB"), key: os.Getenv("PESTIROUTE_DIAG_KEY"), function: os.Getenv("PESTIROUTE_STAGE1_FUNCTION_RESULT"), custom: os.Getenv("PESTIROUTE_STAGE2_CUSTOM_RESULT"), stage: os.Getenv(gatewayToolStageEnv)}
	if p.stage == "" {
		p.stage = "both"
	}
	if p.db == "" || p.key == "" || os.Getenv("PESTIROUTE_DIAG_ACCOUNT") != "selected-A" || (p.stage != "both" && p.stage != "custom") || filepath.Dir(p.function) != "/tmp/opencode/pestiroute-tool-evidence" || filepath.Dir(p.custom) != filepath.Dir(p.function) || filepath.Base(p.function) != "gateway-function-live-v5.json" || filepath.Base(p.custom) != "gateway-custom-live-v5.json" {
		return p, errors.New("invalid operator paths")
	}
	return p, nil
}

func gatewayToolStagePlan(stage string) []int {
	if stage == "custom" {
		return []int{1}
	}
	return []int{0, 1}
}

type gatewayToolArtifacts struct {
	files   [2]*os.File
	paths   [2]string
	records [2]gatewayToolResult
}

func reserveGatewayToolArtifacts(p gatewayToolPaths) (*gatewayToolArtifacts, error) {
	if err := prepareOperatorPairArtifactDirectory(filepath.Dir(p.function)); err != nil {
		return nil, err
	}
	r := &gatewayToolArtifacts{paths: [2]string{p.function, p.custom}}
	stage := p.stage
	if stage == "" {
		stage = "both"
	}
	indices := gatewayToolStagePlan(stage)
	if stage != "both" && stage != "custom" {
		return nil, errors.New("invalid stage")
	}
	for _, i := range indices {
		path := r.paths[i]
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			r.closeAndRemove()
			return nil, err
		}
		if err = f.Chmod(0600); err != nil {
			_ = f.Close()
			r.closeAndRemove()
			return nil, err
		}
		r.files[i] = f
	}
	return r, nil
}
func (r *gatewayToolArtifacts) closeAndRemove() {
	for i, f := range r.files {
		if f != nil {
			_ = f.Close()
			_ = os.Remove(r.paths[i])
		}
	}
}
func (r *gatewayToolArtifacts) write(i int, v gatewayToolResult) error {
	if i < 0 || i >= len(r.files) ||
		!oneOperatorPairValue(v.ReaderResult, "", "http_rejected", "complete", "failed", "incomplete", "error", "missing_terminal", "malformed", "size_limit", "read_failure", "event_mismatch", "trailing_event", "output_limit", "missing_usage", "missing_call", "unexpected_call", "invalid_call", "multiple_calls", "transport_error") ||
		!oneOperatorPairValue(v.Terminal, "", "none", "completed", "failed", "incomplete", "error") ||
		!oneOperatorPairValue(v.Usage, "", "unknown", "reported") ||
		!oneOperatorPairValue(v.TransportClass, "", "none", "unknown", "other", "deadline", "canceled") || v.ResponseBytes < 0 || v.ResponseBytes > 2*operatorPairBodyLimit ||
		!oneOperatorPairValue(v.FailureCode, "", "other", "invalid_request_error", "unsupported_parameter", "unsupported_value", "model_not_found", "insufficient_quota", "rate_limit_exceeded", "rate_limit_error", "server_error", "upstream_unavailable") ||
		!oneOperatorPairValue(v.FailureField, "", "other", "model", "input", "instructions", "stream", "store", "max_output_tokens", "reasoning", "reasoning.effort", "reasoning.context", "include", "tool_choice", "parallel_tool_calls") {
		return errors.New("invalid sanitized gateway result")
	}
	for label, count := range v.EventCounts {
		if !oneOperatorPairValue(label, append(gatewayToolEventTypes, "unknown")...) || count < 1 || count > 2*operatorPairBodyLimit {
			return errors.New("invalid sanitized event counts")
		}
	}
	for _, terminal := range v.Terminals {
		if !oneOperatorPairValue(terminal, "completed", "failed", "incomplete", "error") {
			return errors.New("invalid sanitized terminal list")
		}
	}
	data, err := json.Marshal(struct {
		Schema int               `json:"schema_version"`
		Stage  string            `json:"stage"`
		Model  string            `json:"model"`
		At     string            `json:"captured_at_utc"`
		Result gatewayToolResult `json:"result"`
	}{1, [...]string{"function", "custom"}[i], operatorPairModel, time.Now().UTC().Format("2006-01-02T15:04:05Z"), v})
	if err != nil {
		return err
	}
	f := r.files[i]
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	err = f.Close()
	r.files[i] = nil
	return err
}

func TestOperatorGatewayToolFailureArtifactSanitized(t *testing.T) {
	const private = "PRIVATE_PROVIDER_DETAIL_MUST_NOT_PERSIST"
	resp := &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"upstream_unavailable","message":"` + private + `"}}`))}
	obs := operatorPairObservation{status: resp.StatusCode, reason: "unknown", errorCode: "other", errorField: "other", bodyReadResult: "not_observed", transport: "none"}
	observeOperatorPairFailureBody(resp, &obs)
	if obs.errorCode != "upstream_unavailable" || obs.reason != "service_error" {
		t.Fatal("fixed upstream failure code was not classified")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal("private output directory setup failed")
	}
	path := filepath.Join(dir, "failure.json")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("exclusive artifact setup failed")
	}
	a := &gatewayToolArtifacts{files: [2]*os.File{f, nil}, paths: [2]string{path, "unused"}}
	if err := a.write(0, gatewayToolResult{Status: obs.status, FailureCode: obs.errorCode, FailureField: obs.errorField}); err != nil {
		t.Fatal("sanitized artifact write failed")
	}
	info, err := os.Stat(path)
	data, readErr := os.ReadFile(path)
	if err != nil || readErr != nil || info.Mode().Perm() != 0600 || bytes.Contains(data, []byte(private)) || !bytes.Contains(data, []byte(`"error_code":"upstream_unavailable"`)) {
		t.Fatal("failure artifact permissions, enum or redaction differed")
	}
	ratePath := filepath.Join(dir, "rate-limit.json")
	rateFile, err := os.OpenFile(ratePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("rate-limit artifact setup failed")
	}
	rateArtifact := &gatewayToolArtifacts{files: [2]*os.File{rateFile, nil}, paths: [2]string{ratePath, "unused"}}
	if err := rateArtifact.write(0, gatewayToolResult{FailureCode: "rate_limit_exceeded", ReaderResult: "http_rejected"}); err != nil {
		t.Fatal("bounded rate-limit error code was rejected")
	}
	if err := rateArtifact.write(0, gatewayToolResult{FailureCode: private}); err == nil {
		t.Fatal("arbitrary error code was accepted")
	}
}

func TestOperatorGatewayToolArtifactsReservedBeforeUse(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal("private artifact directory setup failed")
	}
	p := gatewayToolPaths{function: filepath.Join(dir, "function.json"), custom: filepath.Join(dir, "custom.json")}
	first, err := reserveGatewayToolArtifacts(p)
	if err != nil {
		t.Fatal("exclusive artifact reservation failed")
	}
	defer first.closeAndRemove()
	if _, err := reserveGatewayToolArtifacts(p); err == nil {
		t.Fatal("existing artifact destinations were accepted")
	}
}

func TestOperatorGatewayToolPathsRequireFreshArtifactNames(t *testing.T) {
	t.Setenv("PESTIROUTE_DIAG_DB", "/unread/database")
	t.Setenv("PESTIROUTE_DIAG_KEY", "/unread/key")
	t.Setenv("PESTIROUTE_DIAG_ACCOUNT", "selected-A")
	t.Setenv(gatewayToolStageEnv, "")
	t.Setenv("PESTIROUTE_STAGE1_FUNCTION_RESULT", "/tmp/opencode/pestiroute-tool-evidence/gateway-function-live-v2.json")
	t.Setenv("PESTIROUTE_STAGE2_CUSTOM_RESULT", "/tmp/opencode/pestiroute-tool-evidence/gateway-custom-live-v2.json")
	if _, err := gatewayToolPathsFromEnv(); err == nil {
		t.Fatal("previously used artifact paths were accepted")
	}
	t.Setenv("PESTIROUTE_STAGE1_FUNCTION_RESULT", "/tmp/opencode/pestiroute-tool-evidence/gateway-function-live-v3.json")
	t.Setenv("PESTIROUTE_STAGE2_CUSTOM_RESULT", "/tmp/opencode/pestiroute-tool-evidence/gateway-custom-live-v3.json")
	if _, err := gatewayToolPathsFromEnv(); err == nil {
		t.Fatal("contaminated artifact paths were accepted")
	}
	t.Setenv("PESTIROUTE_STAGE1_FUNCTION_RESULT", "/tmp/opencode/pestiroute-tool-evidence/gateway-function-live-v5.json")
	t.Setenv("PESTIROUTE_STAGE2_CUSTOM_RESULT", "/tmp/opencode/pestiroute-tool-evidence/gateway-custom-live-v5.json")
	if _, err := gatewayToolPathsFromEnv(); err != nil {
		t.Fatal("fresh artifact paths were rejected")
	}
	t.Setenv(gatewayToolStageEnv, "custom-only")
	if _, err := gatewayToolPathsFromEnv(); err == nil {
		t.Fatal("unsupported stage was accepted")
	}
	t.Setenv(gatewayToolStageEnv, "custom")
	if p, err := gatewayToolPathsFromEnv(); err != nil || !reflect.DeepEqual(gatewayToolStagePlan(p.stage), []int{1}) {
		t.Fatal("custom-only stage was not selected")
	}
}

func TestOperatorGatewayToolStageAssemblyAndCustomArtifactPreflight(t *testing.T) {
	if !reflect.DeepEqual(gatewayToolStagePlan("both"), []int{0, 1}) || !reflect.DeepEqual(gatewayToolStagePlan("custom"), []int{1}) {
		t.Fatal("stage assembly exceeded its request budget")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal("private artifact directory setup failed")
	}
	paths := gatewayToolPaths{function: filepath.Join(dir, "function-v5.json"), custom: filepath.Join(dir, "custom-v5.json"), stage: "custom"}
	t.Setenv(gatewayToolStageEnv, "custom")
	reserved, err := reserveGatewayToolArtifacts(paths)
	if err != nil {
		t.Fatal("custom artifact reservation failed")
	}
	defer reserved.closeAndRemove()
	if reserved.files[0] != nil || reserved.files[1] == nil {
		t.Fatal("custom-only preflight reserved a function artifact")
	}
	if _, err := os.Stat(paths.function); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("custom-only preflight touched the function artifact")
	}
	if _, err := reserveGatewayToolArtifacts(paths); err == nil {
		t.Fatal("custom artifact collision was not rejected before credential loading")
	}
}

type gatewayToolCapture struct {
	history [][]byte
	callID  string
}

func gatewayToolStage(t *testing.T, endpoint, key, kind string) (gatewayToolResult, error) {
	t.Helper()
	var out gatewayToolResult
	first := gatewayToolRequest(kind, nil)
	var previous *gatewayToolCapture
	defer func() {
		if previous != nil {
			for _, item := range previous.history {
				clear(item)
			}
		}
	}()
	for round := 0; round < 2; round++ {
		body := first
		if round == 1 {
			body = gatewayToolRequest(kind, previous)
			if len(body) == 0 || len(body) > operatorPairBodyLimit || !json.Valid(body) {
				return out, errors.New("offline follow-up body is malformed")
			}
			out.SyntheticToolResultLinked = gatewayToolResultLinked(kind, body, previous)
		}
		defer clear(body)
		ctx, cancel := context.WithTimeout(t.Context(), operatorPairTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/responses", bytes.NewReader(body))
		if err != nil {
			cancel()
			return out, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := (&http.Client{Timeout: operatorPairTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			cancel()
			out.ReaderResult = "transport_error"
			out.TransportClass = classifyGatewayToolReadError(err)
			return out, errors.New("gateway request transport failed")
		}
		out.Status = resp.StatusCode
		out.RequestStatuses = append(out.RequestStatuses, resp.StatusCode)
		if resp.StatusCode != http.StatusOK {
			out.ReaderResult = "http_rejected"
			observation := operatorPairObservation{status: resp.StatusCode, reason: "unknown", errorCode: "other", errorField: "other", bodyReadResult: "not_observed", transport: "none"}
			observeOperatorPairFailureBody(resp, &observation)
			out.FailureCode, out.FailureField = observation.errorCode, observation.errorField
			_ = resp.Body.Close()
			cancel()
			return out, fmt.Errorf("gateway HTTP rejection")
		}
		observed, err := readGatewayToolSSE(resp.Body)
		defer clearGatewayToolStream(observed)
		_ = resp.Body.Close()
		cancel()
		out.Terminal = observed.Terminal
		if observed.Terminal != "none" {
			out.Terminals = append(out.Terminals, observed.Terminal)
		}
		out.Usage = observed.Usage
		out.InputTokens += observed.InputTokens
		out.OutputTokens += observed.OutputTokens
		out.AnswerObserved = out.AnswerObserved || observed.Answer != ""
		out.DeltaSnapshotEqual = out.DeltaSnapshotEqual || observed.DeltaSnapshotEqual
		out.ReaderResult = observed.ReaderResult
		out.ResponseBytes += observed.BodyBytes
		out.TransportClass = observed.TransportClass
		if out.EventCounts == nil {
			out.EventCounts = map[string]int{}
		}
		for event, count := range observed.Events {
			out.EventCounts[event] += count
		}
		if observed.FailureCode != "" {
			out.FailureCode, out.FailureField = observed.FailureCode, observed.FailureField
		}
		if err != nil {
			return out, err
		}
		if round == 1 {
			expected := "synthetic-alpha"
			if kind == "custom" {
				expected = "synthetic-echo-ok"
			}
			out.NormalizedMarkerEqual = strings.TrimSpace(observed.Answer) == expected
		}
		if round == 0 && observed.CallID == "" {
			out.ReaderResult = "missing_call"
			return out, errors.New("provider did not emit one required tool call")
		}
		if round == 1 && observed.Calls != 0 {
			out.ReaderResult = "unexpected_call"
			return out, errors.New("unexpected follow-up tool call")
		}
		if round == 0 {
			if len(observed.Arguments) > operatorPairTextLimit || len(observed.Input) > operatorPairTextLimit || !safeGatewayCallID(observed.CallID) {
				out.ReaderResult = "invalid_call"
				return out, errors.New("tool input limit")
			}
			if kind == "function" {
				var args struct {
					Key string `json:"key"`
				}
				if observed.Name != "smoke_lookup" || json.Unmarshal([]byte(observed.Arguments), &args) != nil || args.Key != "alpha" {
					out.ReaderResult = "invalid_call"
					return out, errors.New("unexpected function call")
				}
			} else if observed.Name != "synthetic_echo" || observed.Input != "pestiRoute marker" {
				out.ReaderResult = "invalid_call"
				return out, errors.New("unexpected custom call")
			}
			out.Calls += observed.Calls
			previous = &gatewayToolCapture{history: observed.History, callID: observed.CallID}
		}
	}
	return out, nil
}

func gatewayToolResultLinked(kind string, body []byte, previous *gatewayToolCapture) bool {
	if previous == nil {
		return false
	}
	if previous.callID == "" {
		return false
	}
	output := "synthetic-alpha"
	if kind == "custom" {
		output = "synthetic-echo-ok"
	}
	encodedOutput, _ := json.Marshal(output)
	return bytes.Contains(body, []byte(`"call_id":"`+previous.callID+`"`)) && bytes.Contains(body, append([]byte(`"output":`), encodedOutput...))
}

func gatewayToolRequest(kind string, previous *gatewayToolCapture) []byte {
	prefix := gatewayToolFunctionFixture
	if kind == "custom" {
		prefix = gatewayToolCustomFixture
	}
	if previous == nil {
		return []byte(prefix)
	}
	output := "synthetic-alpha"
	if kind == "custom" {
		output = "synthetic-echo-ok"
	}
	callType := "function_call_output"
	if kind == "custom" {
		callType = "custom_tool_call_output"
	}
	result, _ := json.Marshal(map[string]string{"type": callType, "call_id": previous.callID, "output": output})
	base := strings.TrimSuffix(prefix, "]}")
	for _, item := range previous.history {
		base += "," + string(item)
	}
	base += "," + string(result) + `]}`
	if len(base) > operatorPairBodyLimit {
		return nil
	}
	return []byte(base)
}

func mustJSONString(v string) string { b, _ := json.Marshal(v); return string(b) }

type gatewayToolStream struct {
	History                                       [][]byte
	CallID, Name, Arguments, Input                string
	Answer                                        string
	Terminal, Usage, ReaderResult, TransportClass string
	FailureCode, FailureField                     string
	BodyBytes                                     int
	InputTokens, OutputTokens                     int64
	Calls                                         int
	Events                                        map[string]int
	DeltaSnapshotEqual                            bool
}

type gatewayToolTextStream struct {
	outputIndex  *int
	contentIndex *int
	delta        strings.Builder
	deltaSeen    bool
	snapshot     string
	snapshotSeen bool
}

func gatewayToolTextKey(itemID string, outputIndex, contentIndex *int) string {
	index := func(v *int) string {
		if v == nil {
			return "-"
		}
		return fmt.Sprint(*v)
	}
	if itemID != "" {
		return "item\x00" + itemID + "\x00" + index(contentIndex)
	}
	if outputIndex != nil || contentIndex != nil {
		return "index\x00" + index(outputIndex) + "\x00" + index(contentIndex)
	}
	return "unindexed"
}

func gatewayToolTextAnswer(streams []*gatewayToolTextStream) (answer string, paired bool, failure string) {
	allOutputIndexed := len(streams) > 1
	for _, stream := range streams {
		allOutputIndexed = allOutputIndexed && stream.outputIndex != nil
	}
	if allOutputIndexed {
		allContentIndexed := true
		for _, stream := range streams {
			allContentIndexed = allContentIndexed && stream.contentIndex != nil
		}
		sort.SliceStable(streams, func(i, j int) bool {
			a, b := streams[i], streams[j]
			if *a.outputIndex != *b.outputIndex {
				return *a.outputIndex < *b.outputIndex
			}
			return allContentIndexed && *a.contentIndex < *b.contentIndex
		})
	}
	var visible strings.Builder
	allDeltasMatched := true
	for _, stream := range streams {
		fragment := stream.snapshot
		if stream.deltaSeen {
			fragment = stream.delta.String()
			if stream.snapshotSeen {
				paired = true
				if stream.snapshot != fragment {
					return "", false, "event_mismatch"
				}
			} else {
				allDeltasMatched = false
			}
		}
		if visible.Len()+len(fragment) > operatorPairTextLimit {
			return "", paired, "output_limit"
		}
		visible.WriteString(fragment)
	}
	return visible.String(), paired && allDeltasMatched, ""
}

func gatewayToolJSONEqual(got, want []byte) bool {
	var gotJSON, wantJSON any
	return json.Unmarshal(got, &gotJSON) == nil && json.Unmarshal(want, &wantJSON) == nil && reflect.DeepEqual(gotJSON, wantJSON)
}

var gatewayToolEventTypes = []string{"response.created", "response.in_progress", "response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done", "response.output_text.delta", "response.output_text.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.custom_tool_call_input.delta", "response.completed", "response.failed", "response.incomplete", "error"}

func readGatewayToolSSE(body io.Reader) (gatewayToolStream, error) {
	s := gatewayToolStream{Terminal: "none", Usage: "unknown", ReaderResult: "incomplete", TransportClass: "none", Events: map[string]int{}}
	reader := bufio.NewReaderSize(io.LimitReader(body, operatorPairBodyLimit+1), 4096)
	var eventName string
	var data []byte
	dataSeen := false
	seenCalls := map[string]bool{}
	textByKey := map[string]*gatewayToolTextStream{}
	var textStreams []*gatewayToolTextStream
	deltaBytes := 0
	defer func() { clear(data) }()
	fail := func(result string) (gatewayToolStream, error) {
		s.ReaderResult = result
		return s, errors.New("SSE observation stopped")
	}
	for {
		line, readErr := reader.ReadString('\n')
		s.BodyBytes += len(line)
		if s.BodyBytes > operatorPairBodyLimit {
			return fail("size_limit")
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			s.ReaderResult = "read_failure"
			s.TransportClass = classifyGatewayToolReadError(readErr)
			return s, errors.New("SSE read failed")
		}
		if errors.Is(readErr, io.EOF) && line != "" {
			return fail("malformed")
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" && dataSeen {
			var env map[string]json.RawMessage
			if json.Unmarshal(data, &env) != nil {
				return fail("malformed")
			}
			var typ string
			if json.Unmarshal(env["type"], &typ) != nil || typ == "" {
				return fail("malformed")
			}
			name := eventName
			if name == "" {
				name = typ
			}
			if name != typ {
				return fail("event_mismatch")
			}
			label := name
			if !oneOperatorPairValue(label, gatewayToolEventTypes...) {
				label = "unknown"
			}
			s.Events[label]++
			if s.Terminal != "none" {
				return fail("trailing_event")
			}
			if oneOperatorPairValue(name, "response.output_item.added", "response.output_item.done") {
				var e struct {
					Item json.RawMessage `json:"item"`
				}
				if json.Unmarshal(data, &e) != nil || len(e.Item) == 0 || !json.Valid(e.Item) {
					return fail("malformed")
				}
				var item struct {
					Type      string `json:"type"`
					ID        string `json:"id"`
					CallID    string `json:"call_id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
					Input     string `json:"input"`
				}
				if json.Unmarshal(e.Item, &item) != nil {
					return fail("malformed")
				}
				if item.Type == "function_call" || item.Type == "custom_tool_call" {
					key := item.CallID
					if key == "" {
						key = item.ID
					}
					if key == "" {
						key = "unknown-call"
					}
					if !seenCalls[key] {
						seenCalls[key] = true
						s.Calls++
					}
					if name == "response.output_item.done" {
						if s.CallID != "" {
							return fail("multiple_calls")
						}
						s.CallID, s.Name, s.Arguments, s.Input = item.CallID, item.Name, item.Arguments, item.Input
						s.History = append(s.History, bytes.Clone(e.Item))
					}
				} else if name == "response.output_item.done" && (item.Type == "reasoning" || item.Type == "message") {
					s.History = append(s.History, bytes.Clone(e.Item))
				}
			}
			var usageEnvelope struct {
				Usage    json.RawMessage `json:"usage"`
				Response struct {
					Usage json.RawMessage `json:"usage"`
				} `json:"response"`
			}
			if json.Unmarshal(data, &usageEnvelope) == nil {
				usage := usageEnvelope.Response.Usage
				if len(usage) == 0 {
					usage = usageEnvelope.Usage
				}
				if len(usage) > 0 && string(usage) != "null" {
					var u map[string]json.RawMessage
					var a, b int64
					if json.Unmarshal(usage, &u) == nil && json.Unmarshal(u["input_tokens"], &a) == nil && json.Unmarshal(u["output_tokens"], &b) == nil && a >= 0 && b >= 0 {
						s.Usage = "reported"
						s.InputTokens, s.OutputTokens = a, b
					}
				}
			}
			switch name {
			case "response.output_text.delta", "response.output_text.done", "response.content_part.done":
				var text struct {
					ItemID       string `json:"item_id"`
					OutputIndex  *int   `json:"output_index"`
					ContentIndex *int   `json:"content_index"`
					Delta        string `json:"delta"`
					Text         string `json:"text"`
					Part         struct {
						Text string `json:"text"`
					} `json:"part"`
				}
				if json.Unmarshal(data, &text) != nil {
					return fail("malformed")
				}
				key := gatewayToolTextKey(text.ItemID, text.OutputIndex, text.ContentIndex)
				stream := textByKey[key]
				if stream == nil {
					stream = &gatewayToolTextStream{outputIndex: text.OutputIndex, contentIndex: text.ContentIndex}
					textByKey[key] = stream
					textStreams = append(textStreams, stream)
				}
				if name == "response.output_text.delta" {
					if deltaBytes+len(text.Delta) > operatorPairTextLimit {
						return fail("output_limit")
					}
					deltaBytes += len(text.Delta)
					stream.deltaSeen = true
					stream.delta.WriteString(text.Delta)
				} else {
					snapshot := text.Text
					if name == "response.content_part.done" {
						snapshot = text.Part.Text
					}
					if stream.snapshotSeen && stream.snapshot != snapshot {
						return fail("event_mismatch")
					}
					stream.snapshot, stream.snapshotSeen = snapshot, true
				}
			case "response.completed", "response.failed", "response.incomplete", "error":
				var terminal struct {
					Response struct {
						Status string          `json:"status"`
						Usage  json.RawMessage `json:"usage"`
						Error  json.RawMessage `json:"error"`
					} `json:"response"`
					Usage json.RawMessage `json:"usage"`
					Error json.RawMessage `json:"error"`
				}
				if json.Unmarshal(data, &terminal) != nil {
					return fail("malformed")
				}
				usage := terminal.Response.Usage
				if len(usage) == 0 {
					usage = terminal.Usage
				}
				if len(usage) > 0 && string(usage) != "null" {
					var u map[string]json.RawMessage
					var a, b int64
					if json.Unmarshal(usage, &u) == nil && json.Unmarshal(u["input_tokens"], &a) == nil && json.Unmarshal(u["output_tokens"], &b) == nil && a >= 0 && b >= 0 {
						s.Usage = "reported"
						s.InputTokens, s.OutputTokens = a, b
					}
				}
				switch name {
				case "response.completed":
					if terminal.Response.Status == "completed" {
						s.Terminal = "completed"
					} else {
						s.Terminal = "incomplete"
					}
				case "response.failed":
					s.Terminal = "failed"
				case "response.incomplete":
					s.Terminal = "incomplete"
				case "error":
					s.Terminal = "error"
				}
				if s.Terminal != "completed" {
					failure := terminal.Error
					if len(failure) == 0 {
						failure = terminal.Response.Error
					}
					if len(failure) > 0 {
						wrapper := append(append([]byte(`{"error":`), failure...), '}')
						o := operatorPairObservation{errorCode: "other", errorField: "other"}
						o.reason, o.errorCode, o.errorField = classifyOperatorPairFailureBody(wrapper)
						clear(wrapper)
						s.FailureCode, s.FailureField = o.errorCode, o.errorField
					}
					s.ReaderResult = s.Terminal
				}
			}
			clear(data)
			data = nil
			eventName = ""
			dataSeen = false
		} else if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			if dataSeen {
				data = append(data, '\n')
			}
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")...)
			dataSeen = true
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	var textFailure string
	s.Answer, s.DeltaSnapshotEqual, textFailure = gatewayToolTextAnswer(textStreams)
	if textFailure != "" {
		return fail(textFailure)
	}
	if s.Terminal == "none" {
		s.ReaderResult = "missing_terminal"
		return s, errors.New("terminal missing")
	}
	if s.Terminal != "completed" {
		return s, errors.New("non-completed terminal")
	}
	if s.Usage != "reported" {
		s.ReaderResult = "missing_usage"
		return s, errors.New("usage missing")
	}
	s.ReaderResult = "complete"
	return s, nil
}

func clearGatewayToolStream(s gatewayToolStream) {
	for _, item := range s.History {
		clear(item)
	}
}

func safeGatewayCallID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
func classifyGatewayToolReadError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "other"
}

// Offline test exercises both roundtrips through protected Core and the real connector.
func TestOperatorGatewayToolSynthetic(t *testing.T) {
	_, _, keyPath := protectedFixture(t)
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"function", "custom"} {
		account := sqlite.Account{ID: "synthetic-tool-account", Connector: "codex", Enabled: true}
		expires := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
		plain := []byte(`{"version":1,"access_token":"synthetic-access","account_id":"synthetic-account","expires_at":"` + expires.Format(time.RFC3339Nano) + `"}`)
		envelope, err := secure.Seal(key, 1, "v1", "credentials", "oauth", account.ID, plain)
		if err != nil {
			t.Fatal(err)
		}
		cred := sqlite.Credential{ID: "oauth", AccountID: account.ID, FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext, ExpiresAt: &expires}
		var calls atomic.Int32
		upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer r.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(r.Body, operatorPairBodyLimit+1))
			n := calls.Add(1)
			if r.Method != "POST" || r.URL.Path != operatorPairDirectPath || r.Header.Get("Authorization") != "Bearer synthetic-access" || r.Header.Get("ChatGPT-Account-Id") != "synthetic-account" {
				t.Errorf("synthetic upstream scope mismatch")
			}
			var request map[string]json.RawMessage
			var model string
			var stream, store, parallel bool
			var reasoning struct {
				Effort  string `json:"effort"`
				Context string `json:"context"`
			}
			if json.Unmarshal(body, &request) != nil || json.Unmarshal(request["model"], &model) != nil || json.Unmarshal(request["stream"], &stream) != nil || json.Unmarshal(request["store"], &store) != nil || json.Unmarshal(request["parallel_tool_calls"], &parallel) != nil || json.Unmarshal(request["reasoning"], &reasoning) != nil || model != operatorPairModel || !stream || store || parallel || reasoning.Effort != "high" || reasoning.Context != "all_turns" || !bytes.Contains(body, []byte(`"syntax":"lark"`)) && kind == "custom" {
				t.Errorf("synthetic Lite request profile or actual POST differs")
			}
			if n == 2 && (!bytes.Contains(body, []byte(`"call_id":"synthetic-call"`)) || !bytes.Contains(body, []byte(`"encrypted_content":"synthetic-ciphertext"`))) {
				t.Errorf("follow-up history not preserved")
			}
			if n == 2 && (!bytes.Contains(body, []byte(`"ext":{"keep":true}`)) || !bytes.Contains(body, []byte(`"output":"synthetic-alpha"`)) && kind == "function") {
				t.Errorf("opaque tool item or fixed result was not preserved")
			}
			if n == 1 && !gatewayToolJSONEqual(body, gatewayToolRequest(kind, nil)) {
				t.Errorf("captured initial request differs from the reviewed direct-probe fixture")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			io.WriteString(w, gatewayToolSyntheticSSE(kind, n))
		}))
		connector := codex.NewConnector()
		counter := &operatorPairDispatchCounter{}
		c := gatewayToolHTTPClient(connector, counter, upstream.Listener.Addr().String())
		if c == nil {
			t.Fatal("Codex HTTP client instrumentation unavailable")
		}
		db, issued := provisionOperatorPairGatewayWithRPM(t, account, cred, 4)
		server, closeFn := startOperatorPairGateway(t, db, keyPath, account.ID, connector, c)
		result, runErr := gatewayToolStage(t, server.URL, issued.Secret, kind)
		if counter.dispatches.Load() == 2 {
			dispatches := int32(2)
			result.Dispatches = &dispatches
		}
		result.DialAttempts = counter.dispatches.Load()
		if runErr != nil || result.Status != 200 || result.Terminal != "completed" || result.Usage != "reported" || result.Calls != 1 || result.Dispatches == nil || *result.Dispatches != 2 || calls.Load() != 2 || result.DialAttempts != 2 || !result.SyntheticToolResultLinked || !result.AnswerObserved || !result.DeltaSnapshotEqual || !result.NormalizedMarkerEqual || len(result.RequestStatuses) != 2 || result.RequestStatuses[0] != 200 || result.RequestStatuses[1] != 200 || len(result.Terminals) != 2 || result.Terminals[0] != "completed" || result.Terminals[1] != "completed" {
			t.Fatalf("synthetic %s roundtrip failed: status=%d terminal=%s usage=%s calls=%d reader=%s code=%s field=%s upstream=%d err=%v", kind, result.Status, result.Terminal, result.Usage, result.Calls, result.ReaderResult, result.FailureCode, result.FailureField, calls.Load(), runErr)
		}
		closeFn()
		upstream.Close()
		_ = connector.Close(context.Background())
	}
}

func TestOperatorGatewayToolSyntheticFourRequestsThenRPMRejection(t *testing.T) {
	_, _, keyPath := protectedFixture(t)
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	account := sqlite.Account{ID: "synthetic-tool-session", Connector: "codex", Enabled: true}
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	plain := []byte(`{"version":1,"access_token":"synthetic-access","account_id":"synthetic-account","expires_at":"` + expires.Format(time.RFC3339Nano) + `"}`)
	envelope, err := secure.Seal(key, 1, "v1", "credentials", "oauth", account.ID, plain)
	if err != nil {
		t.Fatal(err)
	}
	credential := sqlite.Credential{ID: "oauth", AccountID: account.ID, FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext, ExpiresAt: &expires}
	var calls atomic.Int32
	var functionCalls, customCalls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(r.Body, operatorPairBodyLimit+1))
		n := calls.Add(1)
		kind := "function"
		var round int32
		if bytes.Contains(body, []byte(`"syntax":"lark"`)) {
			kind = "custom"
			round = customCalls.Add(1)
		} else {
			round = functionCalls.Add(1)
		}
		if r.Method != http.MethodPost || r.URL.Path != operatorPairDirectPath || r.Header.Get("Authorization") != "Bearer synthetic-access" || r.Header.Get("ChatGPT-Account-Id") != "synthetic-account" {
			t.Errorf("synthetic upstream scope mismatch")
		}
		if !gatewayToolJSONEqual(body, gatewayToolRequest(kind, nil)) && (n == 1 || n == 3) {
			t.Errorf("initial %s request differs from the shared direct fixture", kind)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, gatewayToolSyntheticSSE(kind, round))
	}))
	defer upstream.Close()
	connector := codex.NewConnector()
	defer connector.Close(context.Background())
	counter := &operatorPairDispatchCounter{}
	doer := gatewayToolHTTPClient(connector, counter, upstream.Listener.Addr().String())
	db, issued := provisionOperatorPairGatewayWithRPM(t, account, credential, 4)
	server, closeGateway := startOperatorPairGateway(t, db, keyPath, account.ID, connector, doer)
	defer closeGateway()
	for _, kind := range []string{"function", "custom"} {
		result, runErr := gatewayToolStage(t, server.URL, issued.Secret, kind)
		if runErr != nil || result.Status != 200 || result.Terminal != "completed" || result.Usage != "reported" || result.Calls != 1 || !result.SyntheticToolResultLinked || !result.DeltaSnapshotEqual || !result.NormalizedMarkerEqual || len(result.RequestStatuses) != 2 || result.RequestStatuses[0] != 200 || result.RequestStatuses[1] != 200 {
			t.Fatalf("synthetic %s roundtrip failed: status=%d reader=%s upstream=%d err=%v", kind, result.Status, result.ReaderResult, calls.Load(), runErr)
		}
	}
	if calls.Load() != 4 || counter.dispatches.Load() != 4 {
		t.Fatalf("four-request synthetic session sent upstream=%d dials=%d", calls.Load(), counter.dispatches.Load())
	}
	ctx, cancel := context.WithTimeout(t.Context(), operatorPairTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(gatewayToolRequest("custom", nil)))
	if err != nil {
		t.Fatal("fifth request setup failed")
	}
	req.Header.Set("Authorization", "Bearer "+issued.Secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: operatorPairTimeout}).Do(req)
	if err != nil {
		t.Fatal("fifth request did not receive a gateway rejection")
	}
	defer resp.Body.Close()
	observation := operatorPairObservation{status: resp.StatusCode, errorCode: "other", errorField: "other", bodyReadResult: "not_observed"}
	observeOperatorPairFailureBody(resp, &observation)
	if resp.StatusCode != http.StatusTooManyRequests || observation.errorCode != "rate_limit_exceeded" || calls.Load() != 4 || counter.dispatches.Load() != 4 {
		t.Fatalf("fifth request was not zero-upstream RPM rejection: status=%d code=%s upstream=%d", resp.StatusCode, observation.errorCode, calls.Load())
	}
}

func TestGatewayToolSSEObservations(t *testing.T) {
	call := `{"type":"function_call","id":"fc1","call_id":"call1","name":"smoke_lookup","arguments":"{\"key\":\"alpha\"}"}`
	completed := "data: {\"type\":\"response.output_item.done\",\"item\":" + call + "}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"
	t.Run("type-only completed call without reasoning", func(t *testing.T) {
		got, err := readGatewayToolSSE(strings.NewReader(completed))
		defer clearGatewayToolStream(got)
		if err != nil || got.ReaderResult != "complete" || got.Terminal != "completed" || got.Calls != 1 || got.CallID != "call1" || len(got.History) != 1 || got.Usage != "reported" {
			t.Fatalf("valid completed observation not retained: result=%s terminal=%s calls=%d", got.ReaderResult, got.Terminal, got.Calls)
		}
	})
	t.Run("multiline data joins with newline", func(t *testing.T) {
		stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\ndata: \"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
		got, err := readGatewayToolSSE(strings.NewReader(stream))
		if err != nil || got.ReaderResult != "complete" {
			t.Fatal("valid multiline JSON was not joined with newline")
		}
		stream = "data: {\"type\":\n" + "data: \"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
		got, err = readGatewayToolSSE(strings.NewReader(stream))
		if err != nil || got.ReaderResult != "complete" {
			t.Fatal("valid newline-joined JSON rejected")
		}
	})
	for _, terminal := range []string{"response.failed", "response.incomplete", "error"} {
		t.Run(terminal, func(t *testing.T) {
			stream := "event: " + terminal + "\ndata: {\"type\":" + mustJSONString(terminal) + ",\"error\":{\"code\":\"unsupported_value\",\"param\":\"tool_choice\"}}\n\n"
			got, err := readGatewayToolSSE(strings.NewReader(stream))
			if err == nil || got.Terminal != map[string]string{"response.failed": "failed", "response.incomplete": "incomplete", "error": "error"}[terminal] || got.FailureCode != "unsupported_value" || got.FailureField != "tool_choice" {
				t.Fatalf("failure observation lost or unsafe: terminal=%s code=%s field=%s", got.Terminal, got.FailureCode, got.FailureField)
			}
		})
	}
	for _, tc := range []struct{ name, stream, want string }{
		{"mismatch", "event: response.failed\ndata: {\"type\":\"response.completed\"}\n\n", "event_mismatch"},
		{"malformed", "data: {broken}\n\n", "malformed"},
		{"missing terminal", "data: {\"type\":\"response.output_item.done\",\"item\":" + call + "}\n\n", "missing_terminal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readGatewayToolSSE(strings.NewReader(tc.stream))
			if err == nil || got.ReaderResult != tc.want {
				t.Fatalf("got %s, want %s", got.ReaderResult, tc.want)
			}
		})
	}
	t.Run("partial call retained on transport read failure", func(t *testing.T) {
		stream := "data: {\"type\":\"response.output_item.done\",\"item\":" + call + "}\n\n"
		got, err := readGatewayToolSSE(&gatewayToolFaultReader{data: []byte(stream), err: errors.New("private read detail")})
		defer clearGatewayToolStream(got)
		if err == nil || got.Calls != 1 || got.CallID != "call1" || got.ReaderResult != "read_failure" || got.TransportClass != "other" {
			t.Fatal("partial observations lost on reader failure")
		}
	})
	t.Run("split deltas and matching snapshots assemble once", func(t *testing.T) {
		stream := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"message-a\",\"output_index\":1,\"content_index\":0,\"delta\":\"marker\"}\n\n" +
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"message-a\",\"output_index\":1,\"content_index\":0,\"delta\":\"-ok\"}\n\n" +
			"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"item_id\":\"message-a\",\"content_index\":0,\"text\":\"marker-ok\"}\n\n" +
			"event: response.content_part.done\ndata: {\"type\":\"response.content_part.done\",\"item_id\":\"message-a\",\"content_index\":0,\"part\":{\"text\":\"marker-ok\"}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
		got, err := readGatewayToolSSE(strings.NewReader(stream))
		if err != nil || got.Answer != "marker-ok" || !got.DeltaSnapshotEqual {
			t.Fatal("matching snapshots were duplicated instead of validating the deltas")
		}
	})
	t.Run("snapshot fallback without item ids", func(t *testing.T) {
		stream := "data: {\"type\":\"response.content_part.done\",\"part\":{\"text\":\"fallback\"}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
		got, err := readGatewayToolSSE(strings.NewReader(stream))
		if err != nil || got.Answer != "fallback" || got.DeltaSnapshotEqual {
			t.Fatal("snapshot fallback without optional item identifiers failed")
		}
	})
	t.Run("text items assemble in output order", func(t *testing.T) {
		stream := "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"second\",\"output_index\":1,\"content_index\":0,\"delta\":\"B\"}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"first\",\"output_index\":0,\"content_index\":0,\"delta\":\"A\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
		got, err := readGatewayToolSSE(strings.NewReader(stream))
		if err != nil || got.Answer != "AB" {
			t.Fatal("text item output order was not preserved")
		}
	})
	for _, tc := range []struct{ name, firstDelta, snapshot string }{
		{"conflicting snapshot", "marker", "different"},
		{"duplicated delta", "markermarker", "marker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"output_index\":0,\"content_index\":0,\"delta\":" + mustJSONString(tc.firstDelta) + "}\n\n" +
				"data: {\"type\":\"response.output_text.done\",\"item_id\":\"m\",\"output_index\":0,\"content_index\":0,\"text\":" + mustJSONString(tc.snapshot) + "}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
			got, err := readGatewayToolSSE(strings.NewReader(stream))
			if err == nil || got.ReaderResult != "event_mismatch" || got.Answer != "" {
				t.Fatal("conflicting or duplicated visible text was not rejected safely")
			}
		})
	}
	t.Run("visible output limit", func(t *testing.T) {
		stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":" + mustJSONString(strings.Repeat("x", operatorPairTextLimit+1)) + "}\n\n"
		got, err := readGatewayToolSSE(strings.NewReader(stream))
		if err == nil || got.ReaderResult != "output_limit" || got.Answer != "" {
			t.Fatal("oversized visible output was not rejected safely")
		}
	})
	t.Run("duplicate snapshots do not count against visible output", func(t *testing.T) {
		const text = "small marker"
		stream := "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"output_index\":0,\"content_index\":0,\"delta\":" + mustJSONString(text) + "}\n\n" +
			"data: {\"type\":\"response.output_text.done\",\"item_id\":\"m\",\"output_index\":0,\"content_index\":0,\"text\":" + mustJSONString(text) + "}\n\n" +
			"data: {\"type\":\"response.content_part.done\",\"item_id\":\"m\",\"output_index\":0,\"content_index\":0,\"part\":{\"text\":" + mustJSONString(text) + "}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
		got, err := readGatewayToolSSE(strings.NewReader(stream))
		if err != nil || got.ReaderResult != "complete" || got.Answer != text || !got.DeltaSnapshotEqual {
			t.Fatal("snapshot copies inflated the visible output or failed validation")
		}
	})
	t.Run("snapshot fallback respects visible output limit", func(t *testing.T) {
		stream := "data: {\"type\":\"response.output_text.done\",\"text\":" + mustJSONString(strings.Repeat("x", operatorPairTextLimit+1)) + "}\n\n"
		got, err := readGatewayToolSSE(strings.NewReader(stream))
		if err == nil || got.ReaderResult != "output_limit" || got.Answer != "" {
			t.Fatal("oversized snapshot fallback was not rejected safely")
		}
	})
}

type gatewayToolFaultReader struct {
	data []byte
	err  error
}

func (r *gatewayToolFaultReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	err := r.err
	r.err = nil
	if err != nil {
		return 0, err
	}
	return 0, io.EOF
}

func gatewayToolSyntheticSSE(kind string, round int32) string {
	if round == 1 {
		item := `{"type":"function_call","id":"synthetic-item","call_id":"synthetic-call","name":"smoke_lookup","arguments":"{\"key\":\"alpha\"}","ext":{"keep":true}}`
		if kind == "custom" {
			item = `{"type":"custom_tool_call","id":"synthetic-item","call_id":"synthetic-call","name":"synthetic_echo","input":"pestiRoute marker","ext":{"keep":true}}`
		}
		return "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"synthetic-reasoning\",\"summary\":[],\"encrypted_content\":\"synthetic-ciphertext\",\"ext\":1}}\n\nevent: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"id\":\"synthetic-message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Calling tool.\"}],\"ext\":true}}\n\nevent: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":" + item + "}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	}
	answer := "synthetic-alpha"
	if kind == "custom" {
		answer = "synthetic-echo-ok"
	}
	const itemID = "synthetic-answer-item"
	var sse strings.Builder
	fmt.Fprintf(&sse, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":%q,\"output_index\":0,\"content_index\":0,\"delta\":%s}\n\n", itemID, mustJSONString(answer[:len(answer)/2]))
	fmt.Fprintf(&sse, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":%q,\"output_index\":0,\"content_index\":0,\"delta\":%s}\n\n", itemID, mustJSONString(answer[len(answer)/2:]))
	fmt.Fprintf(&sse, "event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"item_id\":%q,\"content_index\":0,\"text\":%s}\n\n", itemID, mustJSONString(answer))
	fmt.Fprintf(&sse, "event: response.content_part.done\ndata: {\"type\":\"response.content_part.done\",\"item_id\":%q,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":%s}}\n\n", itemID, mustJSONString(answer))
	fmt.Fprintf(&sse, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	return sse.String()
}
