package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestTranslationReviewMixedStreamFlushAndSettlement(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var calls atomic.Int32
	f := newTranslationLifecycleFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":7,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}}\n\n"+
			"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Checking weather.\"}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"index\":0}\n\n"+
			"event: content_block_start\ndata: {\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_weather\",\"name\":\"weather\",\"input\":{}}}\n\n"+
			"event: content_block_delta\ndata: {\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n"+
			"event: content_block_stop\ndata: {\"index\":1}\n\n"+
			"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":4}}\n\n"+
			"event: message_stop\ndata: {}\n\n")
	}))
	defer unblock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.gateway.URL+"/v1/responses", strings.NewReader(`{"model":"client-model","stream":true,"input":"hello","tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}]}`))
	req.Header.Set("Authorization", "Bearer "+f.secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.gateway.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	var output []any
	early, terminals, sequence := false, 0, 0
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event["sequence_number"] != float64(sequence) {
			t.Fatalf("sequence=%d event=%#v", sequence, event)
		}
		sequence++
		if event["type"] == "response.output_text.delta" {
			early = true
			unblock() // The upstream cannot complete before the client receives text.
		}
		if event["type"] == "response.failed" || event["type"] == "response.incomplete" {
			t.Fatalf("mixed stream failure: %#v", event)
		}
		if event["type"] == "response.completed" {
			terminals++
			output = event["response"].(map[string]any)["output"].([]any)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !early || terminals != 1 || len(output) != 2 || output[0].(map[string]any)["type"] != "message" || output[1].(map[string]any)["call_id"] != "call_weather" || calls.Load() != 1 {
		t.Fatalf("mixed lifecycle: early=%t terminals=%d output=%#v calls=%d", early, terminals, output, calls.Load())
	}
	select {
	case result := <-f.finalized:
		if result.Outcome != core.OutcomeSucceeded {
			t.Fatal(result)
		}
	case <-ctx.Done():
		t.Fatal("attempt did not finalize")
	}
	rows, err := sqlite.NewLedger(f.prepared.runtimeDB).QueryRequests(ctx, sqlite.RequestFilter{})
	if err != nil || len(rows) != 1 || len(rows[0].Attempts) != 1 || rows[0].Request.State != "succeeded" {
		t.Fatalf("durable settlement: rows=%+v err=%v", rows, err)
	}
	usage := rows[0].Attempts[0].Usage
	if usage == nil || usage.InputTokens == nil || *usage.InputTokens != 7 || usage.OutputTokens == nil || *usage.OutputTokens != 4 {
		t.Fatalf("settled usage: %+v", usage)
	}
	if counts := f.accounting.snapshot(); counts.admit != 1 || counts.intent != 1 || counts.finalize != 1 {
		t.Fatalf("accounting calls: %+v", counts)
	}
}
