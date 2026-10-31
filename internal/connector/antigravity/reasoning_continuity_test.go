package antigravity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestSignedTextOptInEmitsAndReplaysAuthenticatedPart(t *testing.T) {
	codec, err := NewReasoningCodec([32]byte{1}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	scope := reasoningScope{AccountID: "account", ProjectID: "project", Model: "gemini-2.5-pro"}
	const upstream = `{"response":{"candidates":[{"content":{"parts":[{"text":"signed answer","thoughtSignature":"opaque"}]},"finishReason":"STOP"}]}}`
	input := "data: " + upstream + "\n\n"
	s := newReasoningStream(io.NopCloser(strings.NewReader(input)), "resp_test", "msg_test", nil, codec, true, scope)
	if frame, err := s.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
		t.Fatalf("head=%#v err=%v", frame, err)
	}
	var events []string
	for {
		frame, err := s.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameBody {
			events = append(events, string(frame.Body.Data))
		}
		if frame.Type == core.FrameComplete {
			break
		}
	}
	joined := strings.Join(events, "")
	delta, itemDone, completed := strings.Index(joined, "response.output_text.delta"), strings.Index(joined, "response.output_item.done"), strings.Index(joined, "response.completed")
	if delta < 0 || itemDone <= delta || completed <= itemDone || strings.Contains(joined[:delta], "encrypted_content") {
		t.Fatalf("event order/content invalid: %s", joined)
	}
	var output struct {
		Response struct {
			Output []map[string]any `json:"output"`
		} `json:"response"`
	}
	completedEvent := strings.LastIndex(joined, "data: ")
	if completedEvent < 0 {
		t.Fatal("missing completed response")
	}
	end := strings.Index(joined[completedEvent:], "\n")
	if err := json.Unmarshal([]byte(joined[completedEvent+len("data: "):completedEvent+end]), &output); err != nil {
		t.Fatal(err)
	}
	var carrier string
	for _, item := range output.Response.Output {
		if value, ok := item["encrypted_content"].(string); ok {
			carrier = value
		}
	}
	if carrier == "" {
		t.Fatal("completed response omitted carrier")
	}
	plain, err := codec.open([]byte(carrier), scope)
	if err != nil || plain.Source.PartKind != "text" || *plain.Source.Content != "signed answer" || string(mustBase64Decode(plain.Source.SignatureBytesB64)) != "opaque" {
		t.Fatalf("open=%+v err=%v", plain, err)
	}
	replay := []byte(`[{"type":"reasoning","id":"` + plain.Responses.ReasoningItemID + `","status":"completed","summary":[],"encrypted_content":` + string(mustJSON(carrier)) + `},{"type":"message","id":"msg_test","role":"assistant","status":"completed","content":[{"type":"output_text","text":"signed answer","annotations":[]}]}]`)
	contents, ge := translateInputScoped(replay, codec, scope)
	if ge != nil {
		t.Fatal(ge)
	}
	b, _ := json.Marshal(contents)
	if !strings.Contains(string(b), `"thoughtSignature":"opaque"`) || !strings.Contains(string(b), `"text":"signed answer"`) {
		t.Fatalf("provider replay lost original part: %s", b)
	}
}

func TestHTTPFakeProviderStreamsDeltaBeforeTerminalRelease(t *testing.T) {
	flushed, release, handlerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(handlerDone)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"streamed signed text\",\"thoughtSignature\":\"stream-signature\"}]},\"finishReason\":\"STOP\"}]}}\n\n")
		w.(http.Flusher).Flush()
		close(flushed)
		<-release
	}))
	defer upstream.Close()
	codec, err := NewReasoningCodec([32]byte{7}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	connector := NewConnectorWithReasoningCodec(codec)
	if err := connector.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"profile":"antigravity-v1internal","endpoint":"https://example.invalid","oauth":{"client_secret":"fixture"}}`)}); err != nil {
		t.Fatal(err)
	}
	defer connector.Close(context.Background())
	connector.cfg.Endpoint = upstream.URL
	service := core.InvocationServices{Transport: packageTestDoer(upstream.Client()), Credentials: credentials{data: []byte(`{"access_token":"fixture","project_id":"project"}`)}}
	result, ge := connector.Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"prompt","stream":true,"include":["reasoning.encrypted_content"]}`), core.AttemptScope{AccountID: "account", Mode: core.ModeTranslation}, service)
	if ge != nil {
		close(release)
		t.Fatal(ge)
	}
	select {
	case <-flushed:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("fake provider did not flush first frame")
	}
	frame, err := result.Stream.Next(context.Background())
	if err != nil || frame.Type != core.FrameHead {
		close(release)
		t.Fatalf("stream head=%v err=%v", frame.Type, err)
	}
	var events strings.Builder
	for !strings.Contains(events.String(), "response.output_text.delta") {
		frame, err = result.Stream.Next(context.Background())
		if err != nil {
			close(release)
			t.Fatalf("delta not delivered before terminal release: %v", err)
		}
		if frame.Type == core.FrameBody {
			events.Write(frame.Body.Data)
		}
	}
	if strings.Contains(events.String(), "response.completed") {
		close(release)
		t.Fatal("response completed before fake-provider release")
	}
	close(release)
	for {
		frame, err = result.Stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameBody {
			events.Write(frame.Body.Data)
		}
		if frame.Type == core.FrameComplete {
			break
		}
	}
	joined := events.String()
	if !strings.Contains(joined, "response.output_item.done") || !strings.Contains(joined, `"encrypted_content"`) || !strings.Contains(joined, "response.completed") {
		t.Fatalf("terminal carrier events missing: %s", joined)
	}
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("fake provider did not finish after release")
	}
}

func TestSignedPartsRemainFailClosedWithoutOptInAndScopeIsBound(t *testing.T) {
	codec, err := NewReasoningCodec([32]byte{2}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	scope := reasoningScope{AccountID: "account", ProjectID: "project", Model: "gemini-2.5-pro"}
	s := newReasoningStream(&chunkReadCloser{r: strings.NewReader("")}, "r", "m", nil, codec, false, scope)
	if err := s.consume([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"x","thoughtSignature":"sig"}]}}]}}`)); !errors.Is(err, errUnsupportedReasoning) {
		t.Fatalf("default signed part error=%v", err)
	}
	issued := time.Now()
	value := reasoningPlaintext{SchemaVersion: 1, Source: reasoningSource{CandidateIndex: 0, PartIndex: 0, PartKind: "thought", SignatureBytesB64: base64.StdEncoding.EncodeToString([]byte("sig")), Content: new("thought")}, Association: reasoningAssociation{Kind: "direct", SignaturePartIndex: 0, SourcePartIndex: 0}, Responses: reasoningResponses{ReasoningItemID: "rs", OutputIndex: 0}}
	carrier, err := codec.seal(value, scope, issued, issued.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	badScope := scope
	badScope.AccountID = "other"
	contents, ge := translateInputScoped(json.RawMessage(`[{"type":"reasoning","id":"rs","status":"completed","summary":[{"type":"summary_text","text":"thought"}],"encrypted_content":`+string(mustJSON(string(carrier)))+`}]`), codec, badScope)
	if ge == nil || ge.Code != "invalid_request" || contents != nil {
		t.Fatalf("scope mismatch admitted: contents=%v err=%v", contents, ge)
	}
}

func TestWrongKeyCarrierIsRejectedBeforeHTTP(t *testing.T) {
	var providerCalls = make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls <- struct{}{}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"response\":{\"done\":true}}\n\n")
	}))
	defer server.Close()
	correctCodec, err := NewReasoningCodec([32]byte{8}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	wrongCodec, err := NewReasoningCodec([32]byte{9}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	scope := reasoningScope{AccountID: "account", ProjectID: "project", Model: "gemini-2.5-pro"}
	issued := time.Now()
	messageID, reasoningID := "message", "reasoning"
	value := reasoningPlaintext{SchemaVersion: 1, Source: reasoningSource{CandidateIndex: 0, PartIndex: 0, PartKind: "text", SignatureBytesB64: base64.StdEncoding.EncodeToString([]byte("signature")), Content: new("signed")}, Association: reasoningAssociation{Kind: "direct", SignaturePartIndex: 0, SourcePartIndex: 0}, Responses: reasoningResponses{ReasoningItemID: reasoningID, OutputIndex: 1, AssociatedItemID: &messageID}}
	carrier, err := wrongCodec.seal(value, scope, issued, issued.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal([]any{
		map[string]any{"type": "reasoning", "id": reasoningID, "status": "completed", "summary": []any{}, "encrypted_content": string(carrier)},
		map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "signed", "annotations": []any{}}}},
	})
	connector := NewConnectorWithReasoningCodec(correctCodec)
	if err := connector.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"profile":"antigravity-v1internal","endpoint":"https://example.invalid","oauth":{"client_secret":"fixture"}}`)}); err != nil {
		t.Fatal(err)
	}
	defer connector.Close(context.Background())
	connector.cfg.Endpoint = server.URL
	services := core.InvocationServices{Transport: packageTestDoer(server.Client()), Credentials: credentials{data: []byte(`{"access_token":"fixture","project_id":"project"}`)}}
	_, ge := connector.Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":`+string(input)+`,"stream":true}`), core.AttemptScope{AccountID: "account", Mode: core.ModeTranslation}, services)
	if ge == nil || ge.Code != "invalid_request" {
		t.Fatalf("wrong-key carrier error=%v", ge)
	}
	select {
	case <-providerCalls:
		t.Fatal("wrong-key carrier dispatched to provider")
	default:
	}
}

func TestAdjacentSignatureAssociationAndAmbiguityFailClosed(t *testing.T) {
	codec, err := NewReasoningCodec([32]byte{4}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	scope := reasoningScope{AccountID: "account", ProjectID: "project", Model: "gemini-2.5-pro"}
	valid := newReasoningStream(io.NopCloser(strings.NewReader("")), "r", "m", nil, codec, true, scope)
	if err := valid.consume([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"adjacent text"},{"thoughtSignature":"signature"}]},"finishReason":"STOP"}]}}`)); err != nil {
		t.Fatal(err)
	}
	var carrier string
	for _, item := range valid.outputs {
		if value, ok := item["encrypted_content"].(string); ok {
			carrier = value
		}
	}
	plain, err := codec.open([]byte(carrier), scope)
	if err != nil || plain.Association.Kind != "adjacent" || plain.Association.SourcePartIndex != 0 || plain.Association.SignaturePartIndex != 1 {
		t.Fatalf("association=%+v err=%v", plain.Association, err)
	}
	thoughtStream := newReasoningStream(io.NopCloser(strings.NewReader("")), "r_thought", "m_thought", nil, codec, true, scope)
	if err := thoughtStream.consume([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"private thought","thought":true,"thoughtSignature":"thought-signature"}]},"finishReason":"STOP"}]}}`)); err != nil {
		t.Fatal(err)
	}
	var thoughtCarrier string
	for _, item := range thoughtStream.outputs {
		if value, ok := item["encrypted_content"].(string); ok {
			thoughtCarrier = value
		}
	}
	thoughtPlain, err := codec.open([]byte(thoughtCarrier), scope)
	if err != nil || thoughtPlain.Source.PartKind != "thought" || thoughtPlain.Source.Content == nil || *thoughtPlain.Source.Content != "private thought" {
		t.Fatalf("thought carrier=%+v err=%v", thoughtPlain, err)
	}
	var eventText strings.Builder
	for _, event := range thoughtStream.pending {
		eventText.Write(event)
	}
	joinedEvents := eventText.String()
	if !strings.Contains(joinedEvents, "response.reasoning_summary_text.delta") || strings.Index(joinedEvents, "response.output_item.done") < strings.Index(joinedEvents, "response.reasoning_summary_text.delta") {
		t.Fatal("thought summary was not incremental before carrier completion")
	}
	for _, raw := range []string{
		`{"response":{"candidates":[{"content":{"parts":[{"text":"a"},{"text":""},{"thoughtSignature":"signature"}]}}]}}`,
		`{"response":{"candidates":[{"content":{"parts":[{"text":"a","thoughtSignature":"same"},{"text":"b","thoughtSignature":"same"}]}}]}}`,
	} {
		invalid := newReasoningStream(io.NopCloser(strings.NewReader("")), "r", "m", nil, codec, true, scope)
		if err := invalid.consume([]byte(raw)); !errors.Is(err, errUnsupportedReasoning) {
			t.Fatalf("ambiguous signatures admitted: %v", err)
		}
	}
}

func TestAdmitRequiresExactReasoningIncludeOptIn(t *testing.T) {
	for _, body := range []string{
		`{"model":"gemini-2.5-pro","input":"x","stream":true,"include":["reasoning.encrypted_content"]}`,
		`{"model":"gemini-2.5-pro","input":"x","stream":true}`,
	} {
		if _, _, _, _, _, ge := admit([]byte(body)); ge != nil {
			t.Fatalf("admit %s: %v", body, ge)
		}
	}
	for _, include := range []string{`[]`, `null`, `["unknown"]`, `["reasoning.encrypted_content","unknown"]`} {
		body := `{"model":"gemini-2.5-pro","input":"x","stream":true,"include":` + include + `}`
		if _, _, _, _, _, ge := admit([]byte(body)); ge == nil || ge.Code != "invalid_request" {
			t.Fatalf("include %s accepted: %v", include, ge)
		}
	}
}

func TestHTTPFakeProviderClosedLoopReplaysSignedPart(t *testing.T) {
	const upstream = `data: {"response":{"candidates":[{"content":{"parts":[{"id":"provider-part-id","text":"signed answer","thoughtSignature":"opaque"},{"functionCall":{"name":"lookup","args":{"q":"x"},"id":"provider-call-id"},"thoughtSignature":"call-signature"}]},"finishReason":"STOP"}]}}` + "\n\n"
	var requests [][]byte
	var requestsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		requestsMu.Lock()
		requests = append(requests, body)
		requestsMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, upstream)
	}))
	defer server.Close()
	codec, err := NewReasoningCodec([32]byte{3}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	connector := NewConnectorWithReasoningCodec(codec)
	if err := connector.Init(context.Background(), core.ComponentConfig{Data: []byte(`{"profile":"antigravity-v1internal","endpoint":"https://example.invalid","oauth":{"client_secret":"fixture"}}`)}); err != nil {
		t.Fatal(err)
	}
	defer connector.Close(context.Background())
	connector.cfg.Endpoint = server.URL
	credential := credentials{data: []byte(`{"access_token":"fixture-token","project_id":"project"}`)}
	scope := core.AttemptScope{AccountID: "account", Mode: core.ModeTranslation}
	service := core.InvocationServices{Transport: packageTestDoer(server.Client()), Credentials: credential}
	run := func(body string) string {
		result, ge := connector.Execute(context.Background(), request(body), scope, service)
		if ge != nil {
			t.Fatal(ge)
		}
		if frame, err := result.Stream.Next(context.Background()); err != nil || frame.Type != core.FrameHead {
			t.Fatalf("stream head=%v err=%v", frame.Type, err)
		}
		var raw strings.Builder
		for {
			frame, err := result.Stream.Next(context.Background())
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if frame.Type == core.FrameBody {
				raw.Write(frame.Body.Data)
			}
			if frame.Type == core.FrameComplete {
				break
			}
		}
		return raw.String()
	}
	tools := `"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]`
	first := run(`{"model":"gemini-2.5-pro","input":"prompt","stream":true,"include":["reasoning.encrypted_content"],` + tools + `}`)
	var completed map[string]any
	lineStart := strings.LastIndex(first, "data: ")
	lineEnd := strings.Index(first[lineStart:], "\n")
	if lineStart < 0 || json.Unmarshal([]byte(first[lineStart+6:lineStart+lineEnd]), &completed) != nil {
		t.Fatalf("invalid first stream: %s", first)
	}
	response := completed["response"].(map[string]any)
	items := response["output"].([]any)
	var messageID string
	var replayItems []any
	var sourceKinds []string
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["type"] == "message" {
			messageID, _ = item["id"].(string)
		}
	}
	if messageID == "" {
		t.Fatalf("missing replay artifacts: %#v", items)
	}
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["type"] != "reasoning" {
			continue
		}
		carrier, _ := item["encrypted_content"].(string)
		plain, openErr := codec.open([]byte(carrier), reasoningScope{AccountID: scope.AccountID, ProjectID: "project", Model: "gemini-2.5-pro"})
		if openErr != nil {
			t.Fatal(openErr)
		}
		sourceKinds = append(sourceKinds, plain.Source.PartKind)
		replayItems = append(replayItems, map[string]any{"type": "reasoning", "id": item["id"], "status": "completed", "summary": item["summary"], "encrypted_content": carrier})
		switch plain.Source.PartKind {
		case "text":
			replayItems = append(replayItems, map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "signed answer", "annotations": []any{}}}})
		case "function_call":
			for _, visibleRaw := range items {
				visible := visibleRaw.(map[string]any)
				if visible["type"] == "function_call" {
					replayItems = append(replayItems, visible)
					break
				}
			}
		}
	}
	var reasonID, carrier string
	for _, value := range replayItems {
		item := value.(map[string]any)
		if reasonID == "" {
			reasonID, _ = item["id"].(string)
			carrier, _ = item["encrypted_content"].(string)
		}
	}
	if reasonID == "" || carrier == "" {
		t.Fatalf("signed output carriers missing: %#v kinds=%v", replayItems, sourceKinds)
	}
	badScope := scope
	badScope.AccountID = "another-account"
	invalidRequest := `{"model":"gemini-2.5-pro","input":` + string(mustJSON(replayItems)) + `,"stream":true}`
	checkRejected := func(body string, attemptScope core.AttemptScope, subtype bool) {
		t.Helper()
		if _, ge := connector.Execute(context.Background(), request(body), attemptScope, service); ge == nil || ge.Code != "invalid_request" || subtype && !strings.Contains(ge.Message, "continuation_scope_mismatch") {
			t.Fatalf("invalid continuation accepted/misclassified: err=%v", ge)
		}
		requestsMu.Lock()
		count := len(requests)
		requestsMu.Unlock()
		if count != 1 {
			t.Fatalf("invalid continuation dispatched: calls=%d", count)
		}
	}
	checkRejected(invalidRequest, badScope, true)
	cloneItems := func() []any {
		raw, _ := json.Marshal(replayItems)
		var cloned []any
		_ = json.Unmarshal(raw, &cloned)
		return cloned
	}
	tampered := cloneItems()
	firstReasoning := make(map[string]any)
	for _, value := range tampered {
		item := value.(map[string]any)
		if item["type"] == "reasoning" {
			firstReasoning = item
			break
		}
	}
	var carrierFields map[string]any
	if err := json.Unmarshal([]byte(firstReasoning["encrypted_content"].(string)), &carrierFields); err != nil {
		t.Fatal(err)
	}
	ciphertext := carrierFields["ciphertext"].(string)
	carrierFields["ciphertext"] = "A" + ciphertext[1:]
	firstReasoning["encrypted_content"] = string(mustJSON(carrierFields))
	checkRejected(`{"model":"gemini-2.5-pro","input":`+string(mustJSON(tampered))+`,"stream":true}`, scope, false)
	mutatedVisible := cloneItems()
	for _, value := range mutatedVisible {
		item := value.(map[string]any)
		if item["type"] == "message" {
			content := item["content"].([]any)[0].(map[string]any)
			content["text"] = "edited visible text"
			break
		}
	}
	checkRejected(`{"model":"gemini-2.5-pro","input":`+string(mustJSON(mutatedVisible))+`,"stream":true}`, scope, false)
	for _, mutation := range []struct {
		name  string
		apply func([]any) []any
	}{
		{"function name", func(items []any) []any {
			for _, raw := range items {
				item := raw.(map[string]any)
				if item["type"] == "function_call" {
					item["name"] = "changed"
					break
				}
			}
			return items
		}},
		{"function arguments", func(items []any) []any {
			for _, raw := range items {
				item := raw.(map[string]any)
				if item["type"] == "function_call" {
					item["arguments"] = `{"q":"changed"}`
					break
				}
			}
			return items
		}},
		{"function call id", func(items []any) []any {
			for _, raw := range items {
				item := raw.(map[string]any)
				if item["type"] == "function_call" {
					item["call_id"] = "changed-call"
					break
				}
			}
			return items
		}},
		{"reasoning item id", func(items []any) []any {
			for _, raw := range items {
				item := raw.(map[string]any)
				if item["type"] == "reasoning" {
					carrierValue, _ := item["encrypted_content"].(string)
					opened, openErr := codec.open([]byte(carrierValue), reasoningScope{AccountID: scope.AccountID, ProjectID: "project", Model: "gemini-2.5-pro"})
					if openErr == nil && opened.Source.PartKind == "function_call" {
						item["id"] = "changed-reasoning-id"
						break
					}
				}
			}
			return items
		}},
		{"duplicate reasoning item id", func(items []any) []any {
			for _, raw := range items {
				item := raw.(map[string]any)
				if item["type"] == "reasoning" {
					return append(items, item)
				}
			}
			return items
		}},
		{"colliding call id", func(items []any) []any {
			for _, raw := range items {
				item := raw.(map[string]any)
				if item["type"] == "function_call" {
					return append(items, item)
				}
			}
			return items
		}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			mutated := mutation.apply(cloneItems())
			checkRejected(`{"model":"gemini-2.5-pro","input":`+string(mustJSON(mutated))+`,"stream":true}`, scope, false)
		})
	}
	expiredAt := time.Now().Add(-48 * time.Hour)
	expiredCodec, err := NewReasoningCodec([32]byte{3}, func() time.Time { return expiredAt })
	if err != nil {
		t.Fatal(err)
	}
	expiredMessageID, expiredReasoningID := "msg_expired", "rs_expired"
	expiredValue := reasoningPlaintext{SchemaVersion: 1, Source: reasoningSource{CandidateIndex: 0, PartIndex: 0, PartKind: "text", SignatureBytesB64: base64.StdEncoding.EncodeToString([]byte("expired-signature")), Content: new("signed answer")}, Association: reasoningAssociation{Kind: "direct", SignaturePartIndex: 0, SourcePartIndex: 0}, Responses: reasoningResponses{ReasoningItemID: expiredReasoningID, OutputIndex: 1, AssociatedItemID: &expiredMessageID}}
	expiredCarrier, err := expiredCodec.seal(expiredValue, reasoningScope{AccountID: scope.AccountID, ProjectID: "project", Model: "gemini-2.5-pro"}, expiredAt, expiredAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	expiredHistory, _ := json.Marshal([]any{
		map[string]any{"type": "reasoning", "id": expiredReasoningID, "status": "completed", "summary": []any{}, "encrypted_content": string(expiredCarrier)},
		map[string]any{"type": "message", "id": expiredMessageID, "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "signed answer", "annotations": []any{}}}},
	})
	checkRejected(`{"model":"gemini-2.5-pro","input":`+string(expiredHistory)+`,"stream":true}`, scope, false)
	requestsMu.Lock()
	firstCallCount := len(requests)
	requestsMu.Unlock()
	if firstCallCount != 1 {
		t.Fatalf("scope mismatch dispatched to fake provider: calls=%d", firstCallCount)
	}
	replayItems = append(replayItems, map[string]any{"type": "function_call_output", "call_id": "provider-call-id", "output": map[string]any{"ok": true}})
	replay, _ := json.Marshal(replayItems)
	second := run(`{"model":"gemini-2.5-pro","input":` + string(replay) + `,"stream":true,"include":["reasoning.encrypted_content"],` + tools + `}`)
	if !strings.Contains(second, "response.completed") {
		t.Fatalf("closed loop did not complete: %s", second)
	}
	third := run(`{"model":"gemini-2.5-pro","input":` + string(replay) + `,"stream":true,"include":["reasoning.encrypted_content"],` + tools + `}`)
	if !strings.Contains(third, "response.completed") {
		t.Fatalf("valid history could not be resubmitted: %s", third)
	}
	requestsMu.Lock()
	secondCallCount := len(requests)
	secondRequest := append([]byte(nil), requests[1]...)
	requestsMu.Unlock()
	if secondCallCount != 3 {
		t.Fatalf("provider calls=%d, want 3 including replay resubmission", secondCallCount)
	}
	var outbound struct {
		Request struct {
			Contents []struct {
				Role  string           `json:"role"`
				Parts []map[string]any `json:"parts"`
			} `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(secondRequest, &outbound); err != nil {
		t.Fatal(err)
	}
	parts, _ := json.Marshal(outbound.Request.Contents)
	for _, expected := range []string{`"thoughtSignature":"opaque"`, `"text":"signed answer"`, `"id":"provider-part-id"`, `"thoughtSignature":"call-signature"`, `"functionCall"`, `"functionResponse"`} {
		if !strings.Contains(string(parts), expected) {
			var signatures []string
			for _, content := range outbound.Request.Contents {
				for _, part := range content.Parts {
					if v, ok := part["thoughtSignature"].(string); ok {
						signatures = append(signatures, v)
					}
				}
			}
			t.Fatalf("replayed upstream contents omit %s; signatures=%v; source-kinds=%v; content-count=%d", expected, signatures, sourceKinds, len(outbound.Request.Contents))
		}
	}
	if !strings.Contains(string(secondRequest), `"functionDeclarations"`) {
		t.Fatal("tool declarations were not replayed")
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
