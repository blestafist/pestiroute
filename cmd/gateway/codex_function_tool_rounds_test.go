package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/connector/codex"
	"github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

type codexFunctionRoundsConnector struct{ codexCompositionConnector }

func (codexFunctionRoundsConnector) Capabilities(context.Context, core.CapabilityScope) core.CapabilityResult {
	return core.CapabilityResult{Values: map[core.Capability]core.CapabilityState{
		"llm.streaming": core.Supported,
		"llm.tools":     core.Supported,
	}}
}

func TestCodexFunctionToolRoundsClientOwnedHistory(t *testing.T) {
	_, dbPath, keyPath := protectedFixture(t)
	ctx := context.Background()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const accountID, connectorID, model = "codex-account", "codex", "codex-model"
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: accountID, Connector: connectorID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	bundle := fmt.Sprintf(`{"version":1,"access_token":%q,"refresh_token":"synthetic-refresh","account_id":%q,"expires_at":%q}`, codexTestJWT(accountID), accountID, expires.Format(time.RFC3339Nano))
	sealed, err := secure.Seal(key, 1, "v1", "credentials", "oauth", accountID, []byte(bundle))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "oauth", AccountID: accountID, FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext, ExpiresAt: &expires}); err != nil {
		t.Fatal(err)
	}
	policy, err := sqlite.NewKeyPolicies(db).GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	policy, err = sqlite.NewKeyPolicies(db).Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{model, "model-a"}, Connectors: []string{connectorID, "upstream"}, RPM: 20, TPM: 10000})
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

	const firstSSE = "event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\nevent: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_item_round1\",\"type\":\"function_call\",\"call_id\":\"call_round1\",\"name\":\"lookup\",\"arguments\":\"\",\"status\":\"in_progress\"}}\n\nevent: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_item_round1\",\"output_index\":0,\"delta\":\"{\\\"key\\\":\\\"alpha\\\"}\"}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"working\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}}\n\n"
	const secondSSE = "event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"finished\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":9,\"output_tokens\":2}}}\n\n"
	prefixes := []string{
		"event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\nevent: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_item_round1\",\"type\":\"function_call\",\"call_id\":\"call_round1\",\"name\":\"lookup\",\"arguments\":\"\",\"status\":\"in_progress\"}}\n\n",
		"event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n",
	}
	requests := make(chan string, 2)
	started := []chan struct{}{make(chan struct{}), make(chan struct{})}
	release := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var upstreamCalls atomic.Int32
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			return
		}
		requests <- string(body)
		if r.Header.Get("Authorization") != "Bearer "+codexTestJWT(accountID) || r.Header.Get("ChatGPT-Account-Id") != accountID {
			t.Errorf("Codex credential scope mismatch")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		idx := int(upstreamCalls.Add(1)) - 1
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, prefixes[idx])
		w.(http.Flusher).Flush()
		close(started[idx])
		<-release[idx]
		if idx == 0 {
			_, _ = io.WriteString(w, strings.TrimPrefix(firstSSE, prefixes[idx]))
		} else {
			_, _ = io.WriteString(w, strings.TrimPrefix(secondSSE, prefixes[idx]))
		}
	}))
	defer backend.Close()
	defer func() {
		for _, ch := range release {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	}()
	addr := backend.Listener.Addr().String()
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}
	defer transport.CloseIdleConnections()
	doer := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	p := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0").protected
	p.Connectors = append(p.Connectors, protectedConnector{ID: connectorID, Kind: "connector", Implementation: "pestiroute.codex.responses", Protocols: []string{responsesProtocol}, Settings: nativeSettings{Profile: "codex-responses-http-sse-v1", Model: model, AccountID: accountID}})
	p.Routes = append(p.Routes, protectedRoute{ID: "codex-route", Protocol: responsesProtocol, Mode: "native", Model: model, Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(100)}, Targets: []routeTarget{{Connector: connectorID, Account: accountID}}})
	prepared, err := prepareProtectedConfig(ctx, config{protected: p, DatabasePath: dbPath, MasterKeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	ready, draining := atomic.Bool{}, atomic.Bool{}
	ready.Store(true)
	h, closeComponents, err := composeHandlerWithFactory(prepared, &ready, &draining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		if item.Implementation == "pestiroute.codex.responses" {
			return codexFunctionRoundsConnector{codexCompositionConnector{Connector: codex.NewConnector(), doer: doer}}
		}
		return responses.NewConnector()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeComponents(context.Background()) })
	gateway := httptest.NewServer(h)
	defer gateway.Close()
	client := gateway.Client()
	const tools = `"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"key":{"type":"string"}},"required":["key"]}}]`
	firstRequest := fmt.Sprintf(`{"model":%q,"stream":true,"store":false,%s,"tool_choice":{"type":"function","name":"lookup"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Find alpha."}]}]}`, model, tools)
	secondRequest := fmt.Sprintf(`{"model":%q,"stream":true,"store":false,%s,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Find alpha."}]},{"type":"function_call","id":"fc_item_round1","call_id":"call_round1","name":"lookup","arguments":"{  \"key\" : \"alpha \\\"quoted\\\" \\\\ path\" }","client_extension":{"trace":[1,{"label":"preserve"}]}},{"type":"function_call_output","call_id":"call_round1","output":"alpha-result"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"working"}],"client_message_extension":{"revision":7}},{"type":"message","role":"user","content":[{"type":"input_text","text":"Now finish."}]}]}`, model, tools)
	for i, body := range []string{firstRequest, secondRequest} {
		req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+issued.Secret)
		req.Header.Set("Content-Type", "application/json")
		respCh := make(chan *http.Response, 1)
		errCh := make(chan error, 1)
		go func() {
			resp, err := client.Do(req)
			if err != nil {
				errCh <- err
				return
			}
			respCh <- resp
		}()
		select {
		case <-started[i]:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d upstream did not emit early stream bytes", i+1)
		}
		var resp *http.Response
		select {
		case resp = <-respCh:
		case err := <-errCh:
			t.Fatal(err)
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d gateway buffered response until upstream completion", i+1)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("round %d status=%d", i+1, resp.StatusCode)
		}
		gotPrefix := make([]byte, len(prefixes[i]))
		if _, err := io.ReadFull(resp.Body, gotPrefix); err != nil || string(gotPrefix) != prefixes[i] {
			t.Fatalf("round %d early SSE=%q err=%v", i+1, gotPrefix, err)
		}
		select {
		case <-release[i]:
			t.Fatalf("round %d upstream barrier unexpectedly released", i+1)
		default:
		}
		close(release[i])
		rest, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("round %d read response: %v", i+1, err)
		}
		got := append(gotPrefix, rest...)
		want := firstSSE
		if i == 1 {
			want = secondSSE
		}
		if string(got) != want {
			t.Fatalf("round %d SSE changed\ngot: %q\nwant:%q", i+1, got, want)
		}
	}
	firstCaptured, secondCaptured := <-requests, <-requests
	decodeRound := func(encoded string) (struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		ToolChoice json.RawMessage   `json:"tool_choice"`
		Input      []json.RawMessage `json:"input"`
	}, error) {
		var round struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			ToolChoice json.RawMessage   `json:"tool_choice"`
			Input      []json.RawMessage `json:"input"`
		}
		err := json.Unmarshal([]byte(encoded), &round)
		return round, err
	}
	clientFirst, err := decodeRound(firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	upstreamFirst, err := decodeRound(firstCaptured)
	if err != nil {
		t.Fatal(err)
	}
	if len(clientFirst.Tools) != 1 || clientFirst.Tools[0].Name != "lookup" || len(upstreamFirst.Tools) != 1 || upstreamFirst.Tools[0].Name != clientFirst.Tools[0].Name {
		t.Fatalf("tool definition changed: client=%+v upstream=%+v", clientFirst.Tools, upstreamFirst.Tools)
	}
	var selectedTool struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(upstreamFirst.ToolChoice, &selectedTool) != nil || selectedTool.Type != "function" || selectedTool.Name != clientFirst.Tools[0].Name {
		t.Fatalf("tool choice does not identify declared function: %s", upstreamFirst.ToolChoice)
	}
	clientSecond, err := decodeRound(secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	upstreamSecond, err := decodeRound(secondCaptured)
	if err != nil {
		t.Fatal(err)
	}
	if upstreamSecond.ToolChoice != nil || len(clientSecond.Input) != 5 || len(upstreamSecond.Input) != len(clientSecond.Input) {
		t.Fatalf("second-round input shape/choice changed: client=%d upstream=%d choice=%s", len(clientSecond.Input), len(upstreamSecond.Input), upstreamSecond.ToolChoice)
	}
	for i := range clientSecond.Input {
		if !bytes.Equal(clientSecond.Input[i], upstreamSecond.Input[i]) {
			t.Fatalf("second-round history item %d changed or reordered: got %s want %s", i, upstreamSecond.Input[i], clientSecond.Input[i])
		}
	}
	var call struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Extension json.RawMessage `json:"client_extension"`
	}
	var result struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	if json.Unmarshal(upstreamSecond.Input[1], &call) != nil || json.Unmarshal(upstreamSecond.Input[2], &result) != nil {
		t.Fatal("could not decode captured call/result history")
	}
	const wantArguments = `{  "key" : "alpha \"quoted\" \\ path" }`
	if call.Type != "function_call" || call.ID != "fc_item_round1" || call.CallID != "call_round1" || call.ID == call.CallID || call.Name != clientFirst.Tools[0].Name || call.Arguments != wantArguments {
		t.Fatalf("captured function call lost identity/name/raw argument fidelity: %+v", call)
	}
	if result.Type != "function_call_output" || result.CallID != call.CallID || result.Output != "alpha-result" || len(call.Extension) == 0 {
		t.Fatalf("captured result is not linked to call or history extension lost: call=%+v result=%+v", call, result)
	}
	if !bytes.Contains(upstreamSecond.Input[3], []byte(`"client_message_extension":{"revision":7}`)) {
		t.Fatalf("unknown message-history extension lost: %s", upstreamSecond.Input[3])
	}
	unsupportedHistory := fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"offline-fixture"}]}]}`, model)
	badReq, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(unsupportedHistory))
	if err != nil {
		t.Fatal(err)
	}
	badReq.Header.Set("Authorization", "Bearer "+issued.Secret)
	badReq.Header.Set("Content-Type", "application/json")
	badResp, err := client.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	badBody, _ := io.ReadAll(badResp.Body)
	_ = badResp.Body.Close()
	var badResult struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(badBody, &badResult) != nil || badResp.StatusCode != http.StatusBadRequest || badResult.Error.Code != "unsupported_capability" || upstreamCalls.Load() != 2 {
		t.Fatalf("unsupported resource history status=%d code=%q upstream sends=%d, want unsupported-capability rejection and zero sends", badResp.StatusCode, badResult.Error.Code, upstreamCalls.Load())
	}
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d, want two independent rounds", got)
	}
	rows, err := sqlite.NewLedger(prepared.runtimeDB).QueryRequests(ctx, sqlite.RequestFilter{Model: model, Limit: 10})
	if err != nil || len(rows) != 2 {
		t.Fatalf("ledger requests=%d err=%v", len(rows), err)
	}
	for _, row := range rows {
		if row.Request.State != "succeeded" || len(row.Attempts) != 1 || row.Attempts[0].Attempt.State != "succeeded" || row.Request.FinishedAt == nil || row.Attempts[0].Attempt.FinishedAt == nil || row.Attempts[0].Usage == nil {
			attemptState := "missing"
			if len(row.Attempts) == 1 {
				attemptState = row.Attempts[0].Attempt.State
			}
			t.Errorf("round not independently settled: request state=%s attempt state=%s", row.Request.State, attemptState)
		}
	}
}
