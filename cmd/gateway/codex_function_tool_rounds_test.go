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

func TestCodexLiteFunctionToolRoundsClientOwnedHistory(t *testing.T) {
	_, dbPath, keyPath := protectedFixture(t)
	ctx := context.Background()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const accountID, connectorID, model = "codex-account", "codex", "gpt-6-luna"
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
	const standardAccountID = "codex-standard-account"
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: standardAccountID, Connector: "codex-standard", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	standardBundle := fmt.Sprintf(`{"version":1,"access_token":%q,"refresh_token":"synthetic-refresh","account_id":%q,"expires_at":%q}`, codexTestJWT(standardAccountID), standardAccountID, expires.Format(time.RFC3339Nano))
	standardSealed, err := secure.Seal(key, 1, "v1", "credentials", "oauth-standard", standardAccountID, []byte(standardBundle))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "oauth-standard", AccountID: standardAccountID, FormatVersion: standardSealed.FormatVersion, KeyVersion: standardSealed.KeyVersion, Nonce: standardSealed.Nonce, Ciphertext: standardSealed.Ciphertext, ExpiresAt: &expires}); err != nil {
		t.Fatal(err)
	}
	policy, err := sqlite.NewKeyPolicies(db).GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	policy, err = sqlite.NewKeyPolicies(db).Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{model, "gpt-5.4-mini", "model-a"}, Connectors: []string{connectorID, "codex-standard", "upstream"}, RPM: 20, TPM: 10000})
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

	// Two interleaved client-owned calls must survive even with parallel_tool_calls=false.
	rawFirstSSE := "event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\nevent: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_item_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"\",\"status\":\"in_progress\"}}\n\nevent: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_item_1\",\"output_index\":0,\"delta\":\"{\\\"key\\\":\"}\n\nevent: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"id\":\"fc_item_2\",\"type\":\"function_call\",\"call_id\":\"call_2\",\"name\":\"lookup\",\"arguments\":\"\",\"status\":\"in_progress\"}}\n\nevent: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_item_2\",\"output_index\":1,\"delta\":\"{\\\"key\\\":\\\"beta\\\"}\"}\n\nevent: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_item_1\",\"output_index\":0,\"delta\":\"alpha\\\"}\n\nevent: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_item_2\",\"output_index\":1,\"arguments\":\"{\\\"key\\\":\\\"beta\\\"}\"}\n\nevent: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_item_1\",\"output_index\":0,\"arguments\":\"{\\\"key\\\":\\\"alpha\\\"}\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}}\n\n"
	firstEvents := strings.Split(rawFirstSSE, "\n\n")
	firstPrefix := strings.Join(firstEvents[:2], "\n\n") + "\n\n"
	firstCallDelta := firstEvents[2] + "\n\n"
	secondCallDelta := strings.Join(firstEvents[3:5], "\n\n") + "\n\n"
	firstTail := strings.Join(firstEvents[5:9], "\n\n") + "\n\n"
	firstSSE := rawFirstSSE
	const secondSSE = "event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"finished\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":9,\"output_tokens\":2}}}\n\n"
	prefixes := []string{
		"event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\nevent: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_item_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"\",\"status\":\"in_progress\"}}\n\n",
		"event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n",
		"event: response.in_progress\ndata: {\"type\":\"response.in_progress\"}\n\n",
	}
	prefixes[0] = firstPrefix
	requests := make(chan string, 3)
	started := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	release := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	deltaObserved := []chan struct{}{make(chan struct{}), make(chan struct{})}
	fixtureCanceled := make(chan struct{})
	var upstreamCalls atomic.Int32
	var productionProbe atomic.Bool
	var productionCalls atomic.Int32
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if productionProbe.Load() {
			productionCalls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
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
		if idx == 2 {
			<-r.Context().Done()
			close(fixtureCanceled)
			return
		}
		if idx == 0 {
			_, _ = io.WriteString(w, firstCallDelta)
			w.(http.Flusher).Flush()
			select {
			case <-deltaObserved[0]:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, secondCallDelta)
			w.(http.Flusher).Flush()
			select {
			case <-deltaObserved[1]:
			case <-r.Context().Done():
				return
			}
		}
		<-release[idx]
		if idx == 0 {
			_, _ = io.WriteString(w, firstTail)
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
	p.Connectors = append(p.Connectors, protectedConnector{ID: connectorID, Kind: "connector", Implementation: "pestiroute.codex.responses", Protocols: []string{responsesProtocol}, Settings: nativeSettings{Profile: "codex-responses-http-sse-lite-v1", Model: model, AccountID: accountID}})
	p.Connectors = append(p.Connectors, protectedConnector{ID: "codex-standard", Kind: "connector", Implementation: "pestiroute.codex.responses", Protocols: []string{responsesProtocol}, Settings: nativeSettings{Profile: "codex-responses-http-sse-v1", Model: "gpt-5.4-mini", AccountID: standardAccountID}})
	p.Routes = append(p.Routes, protectedRoute{ID: "codex-route", Protocol: responsesProtocol, Mode: "native", Model: model, Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(100)}, Targets: []routeTarget{{Connector: connectorID, Account: accountID}}})
	p.Routes = append(p.Routes, protectedRoute{ID: "codex-standard-route", Protocol: responsesProtocol, Mode: "native", Model: "gpt-5.4-mini", Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(100)}, Targets: []routeTarget{{Connector: "codex-standard", Account: standardAccountID}}})
	prepared, err := prepareProtectedConfig(ctx, config{protected: p, DatabasePath: dbPath, MasterKeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the production connector's scoped capabilities, not a test override.
	productionConfig := prepared
	productionConfig.runtimeDB, productionConfig.runtimeKey = nil, secure.MasterKey{}
	productionConfig.keyStore, productionConfig.policyStore = nil, nil
	productionConfig.accountAuthorizer, productionConfig.accounting = nil, nil
	productionConfig.processLock = nil
	productionReady, productionDraining := atomic.Bool{}, atomic.Bool{}
	productionReady.Store(true)
	productionHandler, closeProduction, err := composeHandlerWithFactory(productionConfig, &productionReady, &productionDraining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		if item.Implementation == "pestiroute.codex.responses" {
			return codexCompositionConnector{Connector: codex.NewConnector(), doer: doer}
		}
		return responses.NewConnector()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeProduction(context.Background()) })
	productionGateway := httptest.NewServer(productionHandler)
	t.Cleanup(productionGateway.Close)
	productionProbe.Store(true)
	productionRequest, err := http.NewRequest(http.MethodPost, productionGateway.URL+"/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"parallel_tool_calls":true,"tools":[{"type":"function","name":"lookup"}],"input":"Require parallel calls."}`, model)))
	if err != nil {
		t.Fatal(err)
	}
	productionRequest.Header.Set("Authorization", "Bearer "+issued.Secret)
	productionRequest.Header.Set("Content-Type", "application/json")
	productionResponse, err := productionGateway.Client().Do(productionRequest)
	if err != nil {
		t.Fatal(err)
	}
	productionBody, _ := io.ReadAll(productionResponse.Body)
	_ = productionResponse.Body.Close()
	var productionResult struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(productionBody, &productionResult) != nil || productionResponse.StatusCode != http.StatusBadRequest || productionResult.Error.Code != "unsupported_capability" || productionCalls.Load() != 0 {
		t.Fatalf("production Codex parallel request status=%d code=%q upstream sends=%d, want Unknown rejection before send", productionResponse.StatusCode, productionResult.Error.Code, productionCalls.Load())
	}
	embeddedParallel, err := http.NewRequest(http.MethodPost, productionGateway.URL+"/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"parallel_tool_calls":true,"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]},{"type":"message","role":"user","content":"Require parallel calls."}]}`, model)))
	if err != nil {
		t.Fatal(err)
	}
	embeddedParallel.Header.Set("Authorization", "Bearer "+issued.Secret)
	embeddedParallel.Header.Set("Content-Type", "application/json")
	embeddedResponse, err := productionGateway.Client().Do(embeddedParallel)
	if err != nil {
		t.Fatal(err)
	}
	embeddedBody, _ := io.ReadAll(embeddedResponse.Body)
	_ = embeddedResponse.Body.Close()
	if json.Unmarshal(embeddedBody, &productionResult) != nil || embeddedResponse.StatusCode != http.StatusBadRequest || productionResult.Error.Code != "unsupported_capability" || productionCalls.Load() != 0 {
		t.Fatalf("embedded parallel request status=%d code=%q upstream sends=%d, want Unknown rejection before send", embeddedResponse.StatusCode, productionResult.Error.Code, productionCalls.Load())
	}
	standardRequest, err := http.NewRequest(http.MethodPost, productionGateway.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.4-mini","stream":true,"store":false,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"input":"Require a tool."}`))
	if err != nil {
		t.Fatal(err)
	}
	standardRequest.Header.Set("Authorization", "Bearer "+issued.Secret)
	standardRequest.Header.Set("Content-Type", "application/json")
	standardResponse, err := productionGateway.Client().Do(standardRequest)
	if err != nil {
		t.Fatal(err)
	}
	standardBody, _ := io.ReadAll(standardResponse.Body)
	_ = standardResponse.Body.Close()
	if json.Unmarshal(standardBody, &productionResult) != nil || standardResponse.StatusCode != http.StatusBadRequest || productionResult.Error.Code != "unsupported_capability" || productionCalls.Load() != 0 {
		t.Fatalf("standard-profile tool request status=%d code=%q upstream sends=%d, want Unknown rejection before send", standardResponse.StatusCode, productionResult.Error.Code, productionCalls.Load())
	}
	productionProbe.Store(false)
	productionGateway.Close()
	if err := closeProduction(context.Background()); err != nil {
		t.Fatal(err)
	}

	ready, draining := atomic.Bool{}, atomic.Bool{}
	ready.Store(true)
	h, closeComponents, err := composeHandlerWithFactory(prepared, &ready, &draining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		if item.Implementation == "pestiroute.codex.responses" {
			return codexCompositionConnector{Connector: codex.NewConnector(), doer: doer}
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
	const tools = `{"type":"namespace","name":"functions","description":"","tools":[{"type":"function","name":"lookup","description":"Lookup","strict":false,"parameters":{"type":"object","properties":{"key":{"type":"string"}},"required":["key"]}}]}`
	additional := `{"type":"additional_tools","role":"developer","tools":[` + tools + `]}`
	const encryptedReasoning = `{"type":"reasoning","id":"rs_opaque","summary":[{"type":"summary_text","text":"Opaque summary"}],"encrypted_content":"ciphertext/opaque+==","vendor_extension":{"keep":true}}`
	firstRequest := fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"parallel_tool_calls":false,"tool_choice":{"type":"function","name":"lookup"},"input":[%s,%s,{"type":"message","role":"user","content":[{"type":"input_text","text":"Find alpha and beta."}]}]}`, model, additional, encryptedReasoning)
	secondRequest := fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"parallel_tool_calls":false,"input":[%s,%s,{"type":"message","role":"user","content":[{"type":"input_text","text":"Find alpha and beta."}]},{"type":"function_call","id":"fc_item_1","call_id":"call_1","name":"lookup","arguments":"{  \"key\" : \"alpha \\\"quoted\\\" \\\\ path\" }","client_extension":{"trace":[1,{"label":"preserve"}]}},{"type":"function_call_output","call_id":"call_1","output":"alpha-result"},{"type":"function_call","id":"fc_item_2","call_id":"call_2","name":"lookup","arguments":"{\"key\":\"beta\"}"},{"type":"function_call_output","call_id":"call_2","output":"beta-result"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"working"}],"client_message_extension":{"revision":7}},{"type":"message","role":"user","content":[{"type":"input_text","text":"Now finish."}]}]}`, model, additional, encryptedReasoning)
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
		var observedDeltas []byte
		if i == 0 {
			for stage, chunk := range []string{firstCallDelta, secondCallDelta} {
				gotDelta := make([]byte, len(chunk))
				if _, err := io.ReadFull(resp.Body, gotDelta); err != nil || string(gotDelta) != chunk {
					t.Fatalf("call delta stage %d was not flushed intact: got=%q err=%v", stage+1, gotDelta, err)
				}
				observedDeltas = append(observedDeltas, gotDelta...)
				close(deltaObserved[stage])
			}
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
		got := append(append(gotPrefix, observedDeltas...), rest...)
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
		ToolChoice        json.RawMessage   `json:"tool_choice"`
		ParallelToolCalls *bool             `json:"parallel_tool_calls"`
		Input             []json.RawMessage `json:"input"`
	}, error) {
		var round struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			ToolChoice        json.RawMessage   `json:"tool_choice"`
			ParallelToolCalls *bool             `json:"parallel_tool_calls"`
			Input             []json.RawMessage `json:"input"`
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
	if clientFirst.ParallelToolCalls == nil || *clientFirst.ParallelToolCalls || upstreamFirst.ParallelToolCalls == nil || *upstreamFirst.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls=false was not forwarded: client=%v upstream=%v", clientFirst.ParallelToolCalls, upstreamFirst.ParallelToolCalls)
	}
	if len(clientFirst.Tools) != 0 || len(upstreamFirst.Tools) != 0 || !bytes.Equal(clientFirst.Input[0], upstreamFirst.Input[0]) || !bytes.Contains(upstreamFirst.Input[0], []byte(`"name":"functions"`)) {
		t.Fatalf("additional_tools definition changed: client=%s upstream=%s", clientFirst.Input[0], upstreamFirst.Input[0])
	}
	var selectedTool struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(upstreamFirst.ToolChoice, &selectedTool) != nil || selectedTool.Type != "function" || selectedTool.Name != "lookup" {
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
	if upstreamSecond.ToolChoice != nil || len(clientSecond.Input) != 9 || len(upstreamSecond.Input) != len(clientSecond.Input) || clientSecond.ParallelToolCalls == nil || *clientSecond.ParallelToolCalls || upstreamSecond.ParallelToolCalls == nil || *upstreamSecond.ParallelToolCalls {
		t.Fatalf("second-round input shape/options changed: client=%d upstream=%d choice=%s parallel client=%v upstream=%v", len(clientSecond.Input), len(upstreamSecond.Input), upstreamSecond.ToolChoice, clientSecond.ParallelToolCalls, upstreamSecond.ParallelToolCalls)
	}
	for i := range clientSecond.Input {
		if !bytes.Equal(clientSecond.Input[i], upstreamSecond.Input[i]) {
			t.Fatalf("second-round history item %d changed or reordered: got %s want %s", i, upstreamSecond.Input[i], clientSecond.Input[i])
		}
	}
	const wantArguments = `{  "key" : "alpha \"quoted\" \\ path" }`
	for index, want := range []struct{ item, call, args, output string }{
		{"fc_item_1", "call_1", wantArguments, "alpha-result"},
		{"fc_item_2", "call_2", `{"key":"beta"}`, "beta-result"},
	} {
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
		if json.Unmarshal(upstreamSecond.Input[3+index*2], &call) != nil || json.Unmarshal(upstreamSecond.Input[4+index*2], &result) != nil {
			t.Fatalf("could not decode call/result pair %d", index)
		}
		if call.Type != "function_call" || call.ID != want.item || call.CallID != want.call || call.ID == call.CallID || call.Name != "lookup" || call.Arguments != want.args || result.Type != "function_call_output" || result.CallID != want.call || result.Output != want.output {
			t.Fatalf("call/result pair %d lost identity, arguments or result: call=%+v result=%+v", index, call, result)
		}
		if index == 0 && len(call.Extension) == 0 {
			t.Fatal("unknown function-call extension lost")
		}
	}
	if !bytes.Contains(upstreamSecond.Input[7], []byte(`"client_message_extension":{"revision":7}`)) {
		t.Fatalf("unknown message-history extension lost: %s", upstreamSecond.Input[7])
	}
	if !bytes.Equal(upstreamSecond.Input[1], []byte(encryptedReasoning)) {
		t.Fatalf("encrypted reasoning history changed: %s", upstreamSecond.Input[1])
	}
	resourceInputs := []string{}
	roles := []string{`"role":"user",`, `"role":"assistant",`, `"role":"developer",`, `"role":"system",`, ""}
	for _, itemType := range []string{`"type":"message",`, ""} {
		for _, role := range roles {
			for _, resource := range []string{"input_image", "input_audio", "input_file"} {
				item := fmt.Sprintf(`{%s%s"content":[{"type":%q}]}`, itemType, role, resource)
				resourceInputs = append(resourceInputs, "["+item+"]")
			}
		}
	}
	for _, resource := range []string{"input_image", "input_audio", "input_file", "input_video"} {
		resourceInputs = append(resourceInputs, fmt.Sprintf(`[{"type":%q}]`, resource))
	}
	resourceInputs = append(resourceInputs, `[{"type":"function_call_output","call_id":"call_offline","output":[{"type":"input_image"},{"type":"input_audio"},{"type":"input_file"}]}]`)
	for _, input := range resourceInputs {
		unsupportedHistory := fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"input":%s}`, model, input)
		badCtx, badCancel := context.WithTimeout(ctx, 5*time.Second)
		badReq, err := http.NewRequestWithContext(badCtx, http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(unsupportedHistory))
		if err != nil {
			badCancel()
			t.Fatal(err)
		}
		badReq.Header.Set("Authorization", "Bearer "+issued.Secret)
		badReq.Header.Set("Content-Type", "application/json")
		badResp, err := client.Do(badReq)
		if err != nil {
			t.Fatal(err)
		}
		badBody, readErr := io.ReadAll(badResp.Body)
		_ = badResp.Body.Close()
		badCancel()
		var badResult struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if readErr != nil || json.Unmarshal(badBody, &badResult) != nil || badResp.StatusCode != http.StatusBadRequest || badResult.Error.Code != "unsupported_capability" && badResult.Error.Code != "unsupported_feature" || upstreamCalls.Load() != 2 {
			t.Fatalf("unsupported resource history=%s status=%d code=%q read err=%v upstream sends=%d, want fail-closed rejection and zero sends", input, badResp.StatusCode, badResult.Error.Code, readErr, upstreamCalls.Load())
		}
	}
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d, want two independent rounds", got)
	}
	rows, err := sqlite.NewLedger(prepared.runtimeDB).QueryRequests(ctx, sqlite.RequestFilter{Model: model, State: "succeeded", Limit: 10})
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
	cancelCtx, cancelRequest := context.WithCancel(ctx)
	cancelReq, err := http.NewRequestWithContext(cancelCtx, http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"input":"cancel after early event"}`, model)))
	if err != nil {
		t.Fatal(err)
	}
	cancelReq.Header.Set("Authorization", "Bearer "+issued.Secret)
	cancelReq.Header.Set("Content-Type", "application/json")
	cancelResponse := make(chan *http.Response, 1)
	cancelError := make(chan error, 1)
	go func() {
		resp, err := client.Do(cancelReq)
		if err != nil {
			cancelError <- err
			return
		}
		cancelResponse <- resp
	}()
	select {
	case <-started[2]:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation fixture did not flush its first event")
	}
	var cancelResp *http.Response
	select {
	case cancelResp = <-cancelResponse:
	case err := <-cancelError:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation request did not return its early stream")
	}
	cancelPrefix := make([]byte, len(prefixes[2]))
	if _, err := io.ReadFull(cancelResp.Body, cancelPrefix); err != nil || string(cancelPrefix) != prefixes[2] {
		t.Fatalf("cancellation fixture prefix=%q err=%v", cancelPrefix, err)
	}
	cancelRequest()
	_ = cancelResp.Body.Close()
	select {
	case <-fixtureCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("client cancellation did not stop the fake upstream fixture")
	}
	if got := upstreamCalls.Load(); got != 3 {
		t.Fatalf("fake upstream dispatches=%d, want 3 including the canceled request", got)
	}
}
