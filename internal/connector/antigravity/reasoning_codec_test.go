package antigravity

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReasoningCodecScopeTamperAndBounds(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	key := [32]byte{1, 2, 3}
	codec, err := NewReasoningCodec(key, func() time.Time { return now })
	if err != nil {
		t.Fatal("codec construction failed")
	}
	scope := reasoningScope{AccountID: "acct", ProjectID: "project", Model: "model"}
	item := "msg_1"
	value := reasoningPlaintext{SchemaVersion: 1, Source: reasoningSource{CandidateIndex: 0, PartIndex: 2, PartKind: "text", SignatureBytesB64: "c2ln", Content: new("opaque text")}, Association: reasoningAssociation{Kind: "direct", SignaturePartIndex: 2, SourcePartIndex: 2}, Responses: reasoningResponses{ReasoningItemID: "rs_1", OutputIndex: 1, AssociatedItemID: &item}}
	issued, expires := now, now.Add(24*time.Hour)
	carrier, err := codec.seal(value, scope, issued, expires)
	if err != nil {
		t.Fatal("seal failed")
	}
	if got, err := codec.open(carrier, scope); err != nil || got.Source.Content == nil || *got.Source.Content != "opaque text" {
		t.Fatal("valid carrier rejected")
	}
	atLimit := append(append([]byte(nil), carrier...), bytes.Repeat([]byte{' '}, reasoningCarrierLimit-len(carrier))...)
	if _, err := codec.open(atLimit, scope); err != nil {
		t.Fatal("carrier exactly at the byte limit rejected")
	}
	for _, wrong := range []reasoningScope{{AccountID: "other", ProjectID: scope.ProjectID, Model: scope.Model}, {AccountID: scope.AccountID, ProjectID: "other", Model: scope.Model}, {AccountID: scope.AccountID, ProjectID: scope.ProjectID, Model: "other"}} {
		if _, err := codec.open(carrier, wrong); !errors.Is(err, ErrContinuationScopeMismatch) {
			t.Fatal("wrong scope was not safely rejected")
		}
	}
	for _, mutate := range []func(map[string]any){
		func(m map[string]any) { m["cryptosuite"] = "other" },
		func(m map[string]any) { m["model"] = "other" },
		func(m map[string]any) { m["part_index"] = float64(3) },
		func(m map[string]any) { m["private_canary"] = "SENSITIVE_CANARY" },
		func(m map[string]any) { m["nonce"] = "AA" },
		func(m map[string]any) { m["ciphertext"] = strings.Repeat("A", 48) },
	} {
		var m map[string]any
		if json.Unmarshal(carrier, &m) != nil {
			t.Fatal("invalid test carrier")
		}
		mutate(m)
		changed, _ := json.Marshal(m)
		if _, err := codec.open(changed, scope); err == nil || err.Error() != ErrReasoningCarrier.Error() && err.Error() != ErrContinuationScopeMismatch.Error() {
			t.Fatal("tampered carrier accepted")
		}
	}
	if _, err := codec.open(bytes.Repeat([]byte{'x'}, reasoningCarrierLimit+1), scope); !errors.Is(err, ErrReasoningCarrier) {
		t.Fatal("oversized carrier was not rejected")
	}
	if _, err := codec.seal(value, scope, issued, expires.Add(time.Second)); !errors.Is(err, ErrReasoningCarrier) {
		t.Fatal("TTL over 24 hours was accepted")
	}
	if _, err := codec.seal(value, scope, issued, now); !errors.Is(err, ErrReasoningCarrier) {
		t.Fatal("zero TTL was accepted")
	}
	if _, err := codec.seal(value, scope, now.Add(5*time.Minute), now.Add(6*time.Minute)); err != nil {
		t.Fatal("five-minute future issuance boundary rejected")
	}
	if _, err := codec.seal(value, scope, now.Add(5*time.Minute+time.Second), now.Add(6*time.Minute)); !errors.Is(err, ErrReasoningCarrier) {
		t.Fatal("future issuance beyond five minutes accepted")
	}
	rotatedCodec, err := NewReasoningCodec([32]byte{9}, func() time.Time { return now })
	if err != nil {
		t.Fatal("rotated codec construction failed")
	}
	if _, err := rotatedCodec.open(carrier, scope); err == nil {
		t.Fatal("carrier survived purpose-key rotation")
	}
	current := now
	clockCodec, err := NewReasoningCodec(key, func() time.Time { return current })
	if err != nil {
		t.Fatal("clock codec construction failed")
	}
	clockCarrier, err := clockCodec.seal(value, scope, now, expires)
	if err != nil {
		t.Fatal("clock carrier seal failed")
	}
	current = expires
	if _, err := clockCodec.open(clockCarrier, reasoningScope{AccountID: "ACCOUNT_SECRET_CANARY", ProjectID: scope.ProjectID, Model: scope.Model}); !errors.Is(err, ErrContinuationScopeMismatch) || err.Error() != ErrContinuationScopeMismatch.Error() {
		t.Fatal("scope rejection after expiry was not fixed and scope-first")
	}
	if _, err := clockCodec.open(clockCarrier, scope); err == nil {
		t.Fatal("carrier accepted at exact expiry")
	}
}

func TestReasoningCodecStrictSchemaAndConcurrentUse(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	codec, err := NewReasoningCodec([32]byte{4}, func() time.Time { return now })
	if err != nil {
		t.Fatal("codec construction failed")
	}
	if NewConnectorWithReasoningCodec(codec).reasoning != codec {
		t.Fatal("connector did not retain the injected scoped codec")
	}
	scope := reasoningScope{AccountID: "a", ProjectID: "p", Model: "m"}
	value := reasoningPlaintext{SchemaVersion: 1, Source: reasoningSource{CandidateIndex: 0, PartIndex: 0, PartKind: "thought", SignatureBytesB64: "c2ln", Content: new("thought")}, Association: reasoningAssociation{Kind: "direct", SignaturePartIndex: 0, SourcePartIndex: 0}, Responses: reasoningResponses{ReasoningItemID: "rs_2", OutputIndex: 0}}
	carrier, err := codec.seal(value, scope, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal("seal failed")
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(carrier, &m) != nil {
		t.Fatal("invalid test carrier")
	}
	plain := []byte(`{"schema_version":1,"source":{"candidate_index":0,"part_index":0,"part_kind":"thought","signature_bytes_b64":"c2ln","content":"x","function_call":null},"association":{"kind":"direct","signature_part_index":0,"source_part_index":0},"responses":{"reasoning_item_id":"rs","output_index":0,"associated_item_id":null,"associated_call_id":null}}`)
	if requiredReasoningFields(plain) {
		t.Fatal("missing required nullable fields accepted")
	}
	if uniqueJSON([]byte(`{"schema_version":1,"schema_version":1}`)) {
		t.Fatal("duplicate key accepted")
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if _, err := codec.open(carrier, scope); err != nil {
					t.Errorf("concurrent open failed")
					return
				}
			}
		}()
	}
	wg.Wait()
}
