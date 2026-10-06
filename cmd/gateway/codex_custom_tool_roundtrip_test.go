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
	responseconnector "github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
	"github.com/google/uuid"
)

func TestCodexLiteCustomToolRoundtripOpaqueAndIncremental(t *testing.T) {
	_, dbPath, keyPath := protectedFixture(t)
	ctx := context.Background()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const accountID, connectorID, standardAccount, standardConnector, model = "custom-account", "codex-custom", "custom-standard-account", "codex-custom-standard", "gpt-6-luna"
	for _, account := range []sqlite.Account{{ID: accountID, Connector: connectorID, Enabled: true}, {ID: standardAccount, Connector: standardConnector, Enabled: true}} {
		if _, err := sqlite.NewAccounts(db).Create(ctx, account); err != nil {
			t.Fatal(err)
		}
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
	policy, err = sqlite.NewKeyPolicies(db).Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{model, "gpt-5.4-mini", "model-a"}, Connectors: []string{connectorID, standardConnector, "upstream"}, RPM: 30, TPM: 20000})
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

	const functionCall1 = `{ "type":"function_call", "id":"fc-item-1", "call_id":"f-call-1", "name":"lookup", "arguments":"{  \"key\" : \"alpha\" }", "vendor_ext":{"utf8":"<>&é"} }`
	const customCall1 = `{ "type":"custom_tool_call", "id":"ct-item-1", "call_id":"c-call-1", "name":"synthetic_echo", "input":"pestiRoute marker", "vendor_ext":{"keep":[1,"<>&é"]} }`
	const functionCall2 = `{"type":"function_call","id":"fc-item-2","call_id":"f-call-2","name":"lookup","arguments":"{\"key\":\"beta\"}"}`
	const customCall2 = `{"type":"custom_tool_call","id":"ct-item-2","call_id":"c-call-2","name":"synthetic_echo","input":"pestiRoute marker","unknown":"preserve"}`
	const encryptedReasoning = `{ "type":"reasoning", "id":"rs-opaque", "summary":[{"type":"summary_text","text":"kept"}], "encrypted_content":"cipher<>&é==", "opaque_ext":{"v":1} }`
	const lookupTool = `{"type":"function","name":"lookup","description":"Lookup","parameters":{"type":"object","properties":{"key":{"type":"string"}},"required":["key"]}}`
	const customTool = `{"type":"custom","name":"synthetic_echo","description":"Echo fixed marker","format":{"type":"grammar","syntax":"lark","definition":"start: \"pestiRoute marker\""}}`
	tools := `{"type":"namespace","name":"functions","description":"","tools":[` + lookupTool + `,` + customTool + `]}`
	additional := `{"type":"additional_tools","role":"developer","tools":[` + tools + `]}`
	firstRequest := fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"parallel_tool_calls":false,"tool_choice":"auto","reasoning":{"effort":"high","context":"all_turns"},"include":["reasoning.encrypted_content"],"input":[%s,%s,{"type":"message","role":"user","content":[{"type":"input_text","text":"Use the tools."}]}]}`, model, additional, encryptedReasoning)
	secondRequest := fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"parallel_tool_calls":false,"tool_choice":"auto","reasoning":{"effort":"high","context":"all_turns"},"include":["reasoning.encrypted_content"],"input":[%s,%s,{"type":"message","role":"user","content":"Use the tools."},%s,%s,%s,%s,%s,%s,%s,%s,{"type":"message","role":"assistant","content":[{"type":"output_text","text":"working"}],"unknown":{"retain":true}},{"type":"message","role":"user","content":[{"type":"input_text","text":"Finish."}]}]}`, model, additional, encryptedReasoning, functionCall1, customCall1, functionCall2, customCall2,
		`{"type":"function_call_output","call_id":"f-call-1","output":"alpha-result"}`,
		`{"type":"custom_tool_call_output","call_id":"c-call-1","output":"synthetic-echo-ok","ext":"<>&é"}`,
		`{"type":"function_call_output","call_id":"f-call-2","output":"beta-result"}`,
		`{"type":"custom_tool_call_output","call_id":"c-call-2","output":"synthetic-echo-ok"}`)
	const prefix1 = "event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\n"
	const prefix2 = "event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":0}}}\n\n"
	firstSSE := prefix1 +
		"event: response.custom_tool_call_input.delta\ndata: {\"type\":\"response.custom_tool_call_input.delta\",\"delta\":\"pestiRoute \"}\n\n" +
		"event: response.custom_tool_call_input.delta\ndata: {\"type\":\"response.custom_tool_call_input.delta\",\"delta\":\"marker\"}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":" + functionCall1 + "}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":" + customCall1 + "}\n\n" +
		"event: response.custom_tool_call_input.delta\ndata: {\"type\":\"response.custom_tool_call_input.delta\",\"delta\":\"pestiRoute marker\"}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":" + functionCall2 + "}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":" + customCall2 + "}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":4}}}\n\n"
	customDeltaEarly := "event: response.custom_tool_call_input.delta\ndata: {\"type\":\"response.custom_tool_call_input.delta\",\"delta\":\"pestiRoute \"}\n\n"
	secondSSE := prefix2 + "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"custom round complete\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":12,\"output_tokens\":3}}}\n\n"
	started := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	release := []chan struct{}{make(chan struct{}), make(chan struct{})}
	customDeltaSent, finishFirst := make(chan struct{}), make(chan struct{})
	canceled := make(chan struct{})
	captured := make(chan struct {
		body []byte
		head http.Header
	}, 3)
	var upstreamCalls atomic.Int32
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read fake Codex request: %v", err)
			return
		}
		captured <- struct {
			body []byte
			head http.Header
		}{body, r.Header.Clone()}
		idx := int(upstreamCalls.Add(1)) - 1
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		if idx == 2 {
			_, _ = io.WriteString(w, prefix1)
			flusher.Flush()
			close(started[idx])
			<-r.Context().Done()
			close(canceled)
			return
		}
		prefix, full := prefix1, firstSSE
		if idx == 1 {
			prefix, full = prefix2, secondSSE
		}
		_, _ = io.WriteString(w, prefix)
		flusher.Flush()
		close(started[idx])
		<-release[idx]
		remainder := strings.TrimPrefix(full, prefix)
		if idx == 0 {
			_, _ = io.WriteString(w, customDeltaEarly)
			flusher.Flush()
			close(customDeltaSent)
			<-finishFirst
			remainder = strings.TrimPrefix(remainder, customDeltaEarly)
		}
		_, _ = io.WriteString(w, remainder)
		flusher.Flush()
	}))
	defer backend.Close()
	addr := backend.Listener.Addr().String()
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}
	defer transport.CloseIdleConnections()
	doer := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	p := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0").protected
	p.Connectors = append(p.Connectors,
		protectedConnector{ID: connectorID, Kind: "connector", Implementation: "pestiroute.codex.responses", Protocols: []string{responsesProtocol}, Settings: nativeSettings{Profile: "codex-responses-http-sse-lite-v1", Model: model, AccountID: accountID}},
		protectedConnector{ID: standardConnector, Kind: "connector", Implementation: "pestiroute.codex.responses", Protocols: []string{responsesProtocol}, Settings: nativeSettings{Profile: "codex-responses-http-sse-v1", Model: "gpt-5.4-mini", AccountID: standardAccount}})
	p.Routes = append(p.Routes,
		protectedRoute{ID: "custom-route", Protocol: responsesProtocol, Mode: "native", Model: model, Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(100)}, Targets: []routeTarget{{Connector: connectorID, Account: accountID}}},
		protectedRoute{ID: "custom-standard-route", Protocol: responsesProtocol, Mode: "native", Model: "gpt-5.4-mini", Adapter: "pestiroute.responses.native", Policy: "standard", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(100)}, Targets: []routeTarget{{Connector: standardConnector, Account: standardAccount}}})
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
			return codexCompositionConnector{Connector: codex.NewConnector(), doer: doer}
		}
		return responseconnector.NewConnector()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeComponents(context.Background()) }()
	gateway := httptest.NewServer(h)
	defer gateway.Close()
	client := gateway.Client()

	deny := func(name, modelID, body, code string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+issued.Secret)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s request: %v", name, err)
		}
		result, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		var got struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if readErr != nil || json.Unmarshal(result, &got) != nil || resp.StatusCode != http.StatusBadRequest || got.Error.Code != code || upstreamCalls.Load() != 0 {
			t.Fatalf("%s %s status=%d code=%q upstream=%d read=%v", name, modelID, resp.StatusCode, got.Error.Code, upstreamCalls.Load(), readErr)
		}
	}
	invalid := []struct {
		name, model, body, code string
	}{
		{"standard profile", "gpt-5.4-mini", `{"model":"gpt-5.4-mini","stream":true,"store":false,"tool_choice":"auto","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[` + customTool + `]}]}]}`, "unsupported_capability"},
		{"direct custom kind", model, fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"tool_choice":"auto","input":[{"type":"additional_tools","role":"developer","tools":[%s]}]}`, model, customTool), "unsupported_feature"},
		{"non-functions namespace", model, fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"hosted","tools":[%s]}]}]}`, model, customTool), "unsupported_feature"},
		{"hosted tool", model, fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"tool_choice":"auto","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[{"type":"web_search"}]}]}]}`, model), "unsupported_feature"},
		{"unproven grammar", model, strings.Replace(fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"tool_choice":"auto","input":[{"type":"additional_tools","role":"developer","tools":[%s]}]}`, model, tools), `"lark"`, `"regex"`, 1), "unsupported_feature"},
		{"forced custom choice", model, fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"tool_choice":{"type":"custom","name":"synthetic_echo"},"input":[{"type":"additional_tools","role":"developer","tools":[%s]}]}`, model, tools), "unsupported_feature"},
		{"parallel required", model, fmt.Sprintf(`{"model":%q,"stream":true,"store":false,"parallel_tool_calls":true,"tool_choice":"auto","input":[{"type":"additional_tools","role":"developer","tools":[%s]}]}`, model, tools), "unsupported_capability"},
	}
	for _, tc := range invalid {
		deny(tc.name, tc.model, tc.body, tc.code)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatal("preflight rejection reached fake upstream")
	}

	for round, body := range []string{firstRequest, secondRequest} {
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
		case <-started[round]:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d did not reach fake upstream", round+1)
		}
		var resp *http.Response
		select {
		case resp = <-respCh:
		case err := <-errCh:
			t.Fatal(err)
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d response was buffered", round+1)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("round %d HTTP status %d", round+1, resp.StatusCode)
		}
		prefix := prefix1
		if round == 1 {
			prefix = prefix2
		}
		early := make([]byte, len(prefix))
		if _, err := io.ReadFull(resp.Body, early); err != nil || !bytes.Equal(early, []byte(prefix)) {
			t.Fatalf("round %d early SSE mismatch: %v", round+1, err)
		}
		close(release[round])
		if round == 0 {
			select {
			case <-customDeltaSent:
			case <-time.After(5 * time.Second):
				t.Fatal("custom input delta was not flushed before completion")
			}
			delta := make([]byte, len(customDeltaEarly))
			if _, err := io.ReadFull(resp.Body, delta); err != nil || !bytes.Equal(delta, []byte(customDeltaEarly)) {
				t.Fatalf("custom input delta was not delivered incrementally: %v", err)
			}
			close(finishFirst)
			early = append(early, delta...)
		}
		rest, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("round %d SSE read: %v", round+1, err)
		}
		want := firstSSE
		if round == 1 {
			want = secondSSE
		}
		if !bytes.Equal(append(early, rest...), []byte(want)) {
			t.Fatalf("round %d SSE bytes changed", round+1)
		}
	}

	for round, want := range []string{firstRequest, secondRequest} {
		got := <-captured
		if !bytes.Equal(got.body, []byte(want)) {
			t.Fatalf("round %d native request body changed", round+1)
		}
		h := got.head
		if h.Get("Authorization") != "Bearer "+codexTestJWT(accountID) || h.Get("ChatGPT-Account-Id") != accountID || h.Get("x-openai-internal-codex-responses-lite") != "true" || h.Get("originator") != "pestiroute" || h.Get("User-Agent") != "PestiRoute" || h.Get("Accept-Encoding") != "identity" {
			t.Fatalf("round %d scoped Lite headers differed", round+1)
		}
		ids := []string{h.Get("session-id"), h.Get("thread-id"), h.Get("x-client-request-id")}
		parsed, err := uuid.Parse(ids[0])
		if err != nil || parsed.String() != ids[0] || ids[0] != ids[1] || ids[1] != ids[2] {
			t.Fatalf("round %d Lite identity UUID invalid", round+1)
		}
	}
	for _, fragment := range [][]byte{[]byte(functionCall1), []byte(customCall1), []byte(functionCall2), []byte(customCall2), []byte(encryptedReasoning), []byte(`"ext":"<>&é"`)} {
		if !bytes.Contains([]byte(secondRequest), fragment) {
			t.Fatalf("second-round opaque history missing %s", fragment)
		}
	}
	for _, output := range []string{`"call_id":"f-call-1"`, `"call_id":"c-call-1"`, `"call_id":"f-call-2"`, `"call_id":"c-call-2"`} {
		if !strings.Contains(secondRequest, output) {
			t.Fatalf("second-round output link missing %s", output)
		}
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	cancelReq, err := http.NewRequestWithContext(cancelCtx, http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(firstRequest))
	if err != nil {
		t.Fatal(err)
	}
	cancelReq.Header.Set("Authorization", "Bearer "+issued.Secret)
	cancelReq.Header.Set("Content-Type", "application/json")
	cancelRespCh := make(chan *http.Response, 1)
	cancelErrCh := make(chan error, 1)
	go func() {
		resp, err := client.Do(cancelReq)
		if err != nil {
			cancelErrCh <- err
			return
		}
		cancelRespCh <- resp
	}()
	select {
	case <-started[2]:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel probe did not reach fake upstream")
	}
	var cancelResp *http.Response
	select {
	case cancelResp = <-cancelRespCh:
	case err := <-cancelErrCh:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("cancel response buffered")
	}
	cancelPrefix := make([]byte, len(prefix1))
	if _, err := io.ReadFull(cancelResp.Body, cancelPrefix); err != nil || !bytes.Equal(cancelPrefix, []byte(prefix1)) {
		t.Fatalf("cancel first event unavailable: %v", err)
	}
	cancel()
	_ = cancelResp.Body.Close()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("client cancellation did not cancel upstream custom turn")
	}
	if upstreamCalls.Load() != 3 {
		t.Fatalf("fake sends=%d, want two complete rounds and one canceled turn", upstreamCalls.Load())
	}
}
