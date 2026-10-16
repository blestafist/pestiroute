package ipc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestM6CodecRoundTrip(t *testing.T) {
	want := &gen.EstimateResult{Supported: true, Known: false, Method: string(bytes.Repeat([]byte("m"), 200<<10))}
	id := Identity{Epoch: 7, CallID: 11, RecordID: 13, Type: "EstimateResult"}
	var got *gen.EstimateResult
	a := Assembler{identity: id}
	if err := WriteRecord(id, want, func(f *gen.RecordFragment) error {
		if len(f.Data) > MaxFragmentBytes {
			t.Fatal("oversized fragment")
		}
		return a.Add(f)
	}); err != nil {
		t.Fatal(err)
	}
	got = &gen.EstimateResult{}
	if err := a.Decode(got); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(want, got) {
		t.Fatal("record changed during fragmentation")
	}
}

func TestM6CodecGenericControlValues(t *testing.T) {
	large := strings.Repeat("c", 100<<10)
	values := []struct {
		typ string
		msg proto.Message
	}{
		{"ExecuteStart", &gen.ExecuteStart{Request: &gen.ExecutionRequest{Id: large, Metadata: &gen.RequestMetadata{}, Payload: &gen.RawPayload{}}, Scope: &gen.AttemptScope{Id: large}}},
		{"InputComplete", &gen.InputComplete{}}, {"HTTPBodyComplete", &gen.HTTPBodyComplete{}},
		{"HTTPRequest", &gen.HTTPRequest{Method: large, Url: "https://example.invalid", Host: large}},
		{"HTTPResponse", &gen.HTTPResponse{StatusCode: 207}},
		{"CredentialLookup", &gen.CredentialLookup{Name: large}},
		{"CredentialValue", &gen.CredentialValue{Value: []byte(large)}},
		{"LogRecord", &gen.LogRecord{Level: large, Message: large, Attributes: map[string]string{large: large}}},
		{"Handshake", &gen.Handshake{ProtocolId: large, IpcVersion: &gen.Version{Major: 1, Minor: 2}}},
		{"CapabilitySet", &gen.CapabilitySet{Values: []string{large, large}}},
	}
	for _, tc := range values {
		t.Run(tc.typ, func(t *testing.T) {
			dst := tc.msg.ProtoReflect().Type().New().Interface()
			roundTrip(t, tc.msg, dst, tc.typ)
		})
	}
}

func TestM6CodecRejectsMalformed(t *testing.T) {
	id := Identity{Epoch: 1, CallID: 2, RecordID: 3, Type: "Health"}
	f := &gen.RecordFragment{Epoch: 1, CallId: 2, RecordId: 3, RecordType: "Health", TotalLength: 2, Offset: 1, Data: []byte("ab"), Final: true}
	if err := (&Assembler{identity: id}).Add(f); !errors.Is(err, ErrMalformed) {
		t.Fatalf("got %v", err)
	}
}

func TestM6CodecAdversarialFragments(t *testing.T) {
	id := Identity{Epoch: 31, CallID: 41, RecordID: 51, Type: "EstimateResult"}
	var original []*gen.RecordFragment
	want := &gen.EstimateResult{Supported: true, Method: strings.Repeat("z", 3*MaxFragmentBytes)}
	if err := WriteRecord(id, want, func(f *gen.RecordFragment) error {
		original = append(original, proto.Clone(f).(*gen.RecordFragment))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(original) < 3 {
		t.Fatalf("test needs multiple fragments, got %d", len(original))
	}
	tests := map[string]func([]*gen.RecordFragment) []*gen.RecordFragment{
		"wrong epoch":   func(fs []*gen.RecordFragment) []*gen.RecordFragment { fs[0].Epoch++; return fs },
		"wrong call":    func(fs []*gen.RecordFragment) []*gen.RecordFragment { fs[0].CallId++; return fs },
		"changed total": func(fs []*gen.RecordFragment) []*gen.RecordFragment { fs[1].TotalLength++; return fs },
		"duplicate": func(fs []*gen.RecordFragment) []*gen.RecordFragment {
			return []*gen.RecordFragment{fs[0], fs[0], fs[1], fs[2]}
		},
		"skipped": func(fs []*gen.RecordFragment) []*gen.RecordFragment { return []*gen.RecordFragment{fs[0], fs[2]} },
		"reordered": func(fs []*gen.RecordFragment) []*gen.RecordFragment {
			return []*gen.RecordFragment{fs[1], fs[0], fs[2]}
		},
		"trailing": func(fs []*gen.RecordFragment) []*gen.RecordFragment {
			return append(fs, proto.Clone(fs[len(fs)-1]).(*gen.RecordFragment))
		},
		"truncated stream": func(fs []*gen.RecordFragment) []*gen.RecordFragment { return fs[:len(fs)-1] },
		"truncated fragment bytes": func(fs []*gen.RecordFragment) []*gen.RecordFragment {
			fs[0].Data = fs[0].Data[:len(fs[0].Data)-1]
			return fs
		},
		"claimed length overflow": func(fs []*gen.RecordFragment) []*gen.RecordFragment { fs[0].TotalLength = math.MaxUint64; return fs },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fs := cloneFragments(original)
			fs = mutate(fs)
			if err := Reassemble(id, fs, &gen.EstimateResult{}); !errors.Is(err, ErrMalformed) {
				t.Fatalf("accepted malformed fragments: %v", err)
			}
		})
	}
	var a Assembler
	if err := a.Add(original[0]); !errors.Is(err, ErrMalformed) {
		t.Fatal("unbound assembler accepted record")
	}
}

func TestM6CodecLimitsRejectFragments(t *testing.T) {
	id := Identity{Epoch: 1, CallID: 2, RecordID: 3, Type: "EstimateResult"}
	cases := []*gen.RecordFragment{
		{Epoch: 1, CallId: 2, RecordId: 3, RecordType: id.Type, TotalLength: 1, Sequence: 1, Final: true, Data: []byte("x")},
		{Epoch: 1, CallId: 2, RecordId: 4, RecordType: id.Type, TotalLength: 1, Final: true, Data: []byte("x")},
		{Epoch: 1, CallId: 2, RecordId: 3, RecordType: "Other", TotalLength: 1, Final: true, Data: []byte("x")},
		{Epoch: 1, CallId: 2, RecordId: 3, RecordType: id.Type, TotalLength: 1, Offset: 1, Final: true, Data: []byte("x")},
		{Epoch: 1, CallId: 2, RecordId: 3, RecordType: id.Type, TotalLength: 1, Final: false, Data: []byte("x")},
		{Epoch: 1, CallId: 2, RecordId: 3, RecordType: id.Type, TotalLength: 1, Final: true, Data: bytes.Repeat([]byte("x"), MaxFragmentBytes+1)},
	}
	for i, f := range cases {
		a := Assembler{identity: id}
		if err := a.Add(f); !errors.Is(err, ErrMalformed) {
			t.Errorf("case %d: %v", i, err)
		}
	}
	var called bool
	if err := WriteRecord(Identity{Type: "Wrong"}, &gen.EstimateResult{}, func(*gen.RecordFragment) error { called = true; return nil }); !errors.Is(err, ErrMalformed) || called {
		t.Fatalf("type mismatch not rejected atomically: %v", err)
	}
}

func TestM6CodecLimits(t *testing.T) {
	if MaxWireFrameBytes != 84<<10 || maxMessageBytes+grpcPrefixBytes != 84<<10 {
		t.Fatal("wire ceiling drift")
	}
	frameAtLimit := wireFrameAtSize(t, MaxWireFrameBytes)
	if err := ValidateSerializedMessage(frameAtLimit[grpcPrefixBytes:]); err != nil || len(frameAtLimit) != MaxWireFrameBytes || binary.BigEndian.Uint32(frameAtLimit[1:grpcPrefixBytes]) != uint32(len(frameAtLimit)-grpcPrefixBytes) {
		t.Fatalf("exact gRPC frame size %d: %v", len(frameAtLimit), err)
	}
	frameOverLimit := wireFrameAtSize(t, MaxWireFrameBytes+1)
	if err := ValidateSerializedMessage(frameOverLimit[grpcPrefixBytes:]); !errors.Is(err, ErrMalformed) || len(frameOverLimit) != MaxWireFrameBytes+1 || binary.BigEndian.Uint32(frameOverLimit[1:grpcPrefixBytes]) != uint32(len(frameOverLimit)-grpcPrefixBytes) {
		t.Fatalf("oversize gRPC frame size %d: %v", len(frameOverLimit), err)
	}
	body := bytes.Repeat([]byte{0, 0xff}, 40<<10)
	assembler := BodyAssembler{routeLimit: uint64(len(body))}
	if err := WriteBody(body, uint64(len(body)), assembler.Add); err != nil {
		t.Fatal(err)
	}
	got, err := assembler.Finish()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("body mismatch: %v", err)
	}
	if err = WriteBody(body, uint64(len(body)-1), func([]byte) error { return nil }); !errors.Is(err, ErrMalformed) {
		t.Fatalf("route bound: %v", err)
	}
	tooLong := BodyAssembler{routeLimit: uint64(len(body) - 1)}
	if err := WriteBody(body, uint64(len(body)), tooLong.Add); !errors.Is(err, ErrMalformed) {
		t.Fatal("inbound body over route bound accepted")
	}
	if err := (&BodyAssembler{routeLimit: MaxFragmentBytes + 1}).Add(make([]byte, MaxFragmentBytes+1)); !errors.Is(err, ErrMalformed) {
		t.Fatal("oversize inbound BodyChunk accepted")
	}
}

func TestM6CodecExtensions(t *testing.T) {
	if _, err := MetadataTo(core.RequestMetadata{Extensions: map[string]any{"x": []byte("opaque")}}); !errors.Is(err, ErrMalformed) {
		t.Fatalf("extensions accepted: %v", err)
	}
	if _, err := MetadataTo(core.RequestMetadata{}); err != nil {
		t.Fatal(err)
	}
}

func TestM6CodecPresenceParity(t *testing.T) {
	zero := int64(0)
	usage := &core.UsageReport{InputTokens: &zero, Source: core.UsageUnknown, Completeness: core.UsageUnknownCompleteness}
	got := UsageFrom(UsageTo(usage))
	if got.InputTokens == nil || *got.InputTokens != 0 || got.OutputTokens != nil {
		t.Fatal("usage nil/zero presence changed")
	}
	status := 0
	head := core.HeadFrame{HTTPStatus: &status}
	wire, err := HeadTo(&head)
	if err != nil || wire.HttpStatus == nil || *wire.HttpStatus != 0 {
		t.Fatal("HTTP status presence changed")
	}
	decoded, err := HeadFrom(wire)
	if err != nil || decoded.HTTPStatus == nil || *decoded.HTTPStatus != 0 {
		t.Fatal("HTTP status round trip failed")
	}
	zeroDuration := time.Duration(0)
	e := &core.GatewayError{Category: core.CategoryInternal, RetryAfter: &zeroDuration}
	we := GatewayErrorTo(e)
	de, err := GatewayErrorFrom(we)
	if err != nil || de.RetryAfter == nil || *de.RetryAfter != 0 {
		t.Fatal("retry-after presence changed")
	}
}

func TestM6RequestBodyChunking(t *testing.T)       { testBodyBoundary(t, 1<<20) }
func TestM6EstimateUsageBodyChunking(t *testing.T) { testBodyBoundary(t, 1<<20) }
func TestM6PayloadCarriersOverFrameBoundary(t *testing.T) {
	body := bytes.Repeat([]byte{0, 0xff}, 50<<10)
	a := BodyAssembler{routeLimit: uint64(len(body))}
	if err := WriteBody(body, uint64(len(body)), a.Add); err != nil {
		t.Fatal(err)
	}
	got, err := a.Finish()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("payload: %v", err)
	}
}
func TestM6CollectionResultsChunking(t *testing.T) {
	want := &gen.ModelsResult{Supported: true}
	for i := 0; i < 500; i++ {
		want.Models = append(want.Models, &gen.ModelInfo{Id: string(bytes.Repeat([]byte("x"), 200)), Capabilities: []*gen.CapabilityValue{{Key: "llm.tools", State: "unknown"}}})
	}
	available := false
	want.Models[0].Available = &available
	roundTrip(t, want, &gen.ModelsResult{}, "ModelsResult")
}
func TestM6ControlRecordFragmentation(t *testing.T) {
	want := &gen.HeadFrame{Protocol: string(bytes.Repeat([]byte("p"), 100<<10)), ContentType: "application/octet-stream"}
	roundTrip(t, want, &gen.HeadFrame{}, "HeadFrame")
}
func TestM6ControlFragmentRejectsMalformed(t *testing.T) { TestM6CodecRejectsMalformed(t) }
func TestM6ControlRecordAtomicity(t *testing.T) {
	id := Identity{Epoch: 1, CallID: 2, RecordID: 3, Type: "EstimateResult"}
	a := Assembler{identity: id}
	if err := a.Add(&gen.RecordFragment{Epoch: 1, CallId: 2, RecordId: 3, RecordType: id.Type, TotalLength: 1, Final: false}); err != nil {
		t.Fatal(err)
	}
	if err := a.Decode(&gen.EstimateResult{}); !errors.Is(err, ErrMalformed) {
		t.Fatalf("partial record decoded: %v", err)
	}
	bad := Assembler{identity: id}
	if err := bad.Add(&gen.RecordFragment{Epoch: 1, CallId: 2, RecordId: 3, RecordType: id.Type, TotalLength: 1, Final: true, Data: []byte{0x80}}); err != nil {
		t.Fatal(err)
	}
	dst := &gen.EstimateResult{Method: "keep"}
	if err := bad.Decode(dst); !errors.Is(err, ErrMalformed) || dst.Method != "keep" {
		t.Fatalf("malformed value was exposed: %q %v", dst.Method, err)
	}
}

func TestM6ControlRecordLargeFieldsRoundTrip(t *testing.T) {
	large := string(bytes.Repeat([]byte("k"), 90<<10))
	checks := []struct {
		name  string
		value proto.Message
	}{
		{"CapabilityScope", &gen.CapabilityScope{Protocol: large, Mode: "native", Model: large, AccountId: large}},
		{"ModelQuery", &gen.ModelQuery{Protocol: large, Mode: large, AccountId: large}},
		{"HeadFrame", &gen.HeadFrame{Protocol: large, ContentType: large, Headers: map[string]*gen.StringList{large: {Values: []string{large, large}}}}},
		{"GatewayError", &gen.GatewayError{Code: large, Category: "unavailable", RetryDisposition: "unknown", Provider: large, OriginalError: large, Message: large}},
		{"Descriptor", &gen.Descriptor{Id: large, Kind: "connector", ImplementationVersion: large, Protocols: []string{large}, Operations: []string{large}, ConnectorType: large, AuthMethods: []string{large}}},
		{"AuthRequest", &gen.AuthRequest{AccountId: large, Action: large, State: bytes.Repeat([]byte{0, 0xff}, 50<<10)}},
		{"AuthResult", &gen.AuthResult{Supported: true, State: []byte(large), NextAction: large, UserAction: &gen.AuthUserAction{VerificationUri: large, UserCode: large, PollIntervalNs: 1}, Credentials: map[string][]byte{large: bytes.Repeat([]byte{0, 0xff}, 50<<10)}}},
		{"ExecutionRequest", &gen.ExecutionRequest{Id: large, Model: large, Capabilities: []string{large}, Metadata: &gen.RequestMetadata{Headers: map[string]*gen.StringList{large: {Values: []string{large}}}}, Payload: &gen.RawPayload{Protocol: large, ContentType: large}}},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			dst := tc.value.ProtoReflect().Type().New().Interface()
			roundTrip(t, tc.value, dst, tc.name)
		})
	}
	config := core.ComponentConfig{Data: bytes.Repeat([]byte{0, 0xff}, 50<<10)}
	wire := ConfigTo(config)
	roundTrip(t, wire, &gen.ComponentConfig{}, "ComponentConfig")
	got, err := ConfigFrom(wire)
	if err != nil || !bytes.Equal(got.Data, config.Data) {
		t.Fatalf("config bytes: %v", err)
	}
	meta := core.RequestMetadata{Headers: map[string][]string{large: {large, large}}, Streaming: new(false), AffinityKnown: true, SessionBound: true, IngressHeaderBytes: 12}
	wmeta, err := MetadataTo(meta)
	if err != nil {
		t.Fatal(err)
	}
	gotMeta, err := MetadataFrom(wmeta)
	if err != nil || gotMeta.Streaming == nil || *gotMeta.Streaming || !gotMeta.AffinityKnown || !gotMeta.SessionBound || gotMeta.IngressHeaderBytes != 12 || len(gotMeta.Headers[large]) != 2 || gotMeta.Headers[large][1] != large {
		t.Fatalf("metadata parity: %v", err)
	}
	request := core.ExecutionRequest{ID: large, Model: large, Capabilities: map[core.Capability]struct{}{core.Capability(large): {}}, Metadata: meta, Payload: core.RawPayload{Protocol: large, ContentType: large, Body: []byte{0, 0xff}}}
	wireRequest, err := ExecutionRequestTo(request)
	if err != nil {
		t.Fatal(err)
	}
	decodedRequest, err := ExecutionRequestFrom(wireRequest, request.Payload.Body)
	if err != nil || decodedRequest.ID != request.ID || decodedRequest.Model != request.Model || len(decodedRequest.Capabilities) != 1 || !bytes.Equal(decodedRequest.Payload.Body, request.Payload.Body) {
		t.Fatalf("execution parity: %v", err)
	}
	expiry := time.Date(2030, 1, 2, 3, 4, 5, 0, time.FixedZone("test", 3600))
	auth := core.AuthResult{Supported: true, State: large, NextAction: "continue", UserAction: &core.AuthUserAction{VerificationURI: large, UserCode: large, PollInterval: time.Second}, Credentials: map[string][]byte{"k": {0, 0xff}}, CredentialExpiresAt: &expiry}
	gotAuth, err := AuthResultFrom(AuthResultTo(auth))
	if err != nil || gotAuth.UserAction.PollInterval != time.Second || !gotAuth.CredentialExpiresAt.Equal(expiry) || !bytes.Equal(gotAuth.Credentials["k"], auth.Credentials["k"]) {
		t.Fatalf("auth parity: %v", err)
	}
}

func TestM6GoValueConvertersParity(t *testing.T) {
	large := strings.Repeat("v", 90<<10)
	body := []byte{0, 0xff, 1, 0xfe}
	version := core.APIVersion{Major: 1, Minor: 2}
	descriptor := core.Descriptor{ID: "connector", Kind: core.ComponentConnector, ImplementationVersion: "v1", APIVersions: []core.APIVersion{version}, Protocols: []string{"proto.v1"}, Operations: []string{"op"}, ConnectorType: "api", AuthMethods: []string{"oauth"}}
	if got, err := DescriptorFrom(decodeRecord(t, DescriptorTo(descriptor))); err != nil || !reflect.DeepEqual(got, descriptor) {
		t.Fatalf("descriptor parity: %#v %v", got, err)
	}
	config := core.ComponentConfig{Data: bytes.Repeat(body, 50<<10)}
	if got, err := ConfigFrom(decodeRecord(t, ConfigTo(config))); err != nil || !reflect.DeepEqual(got, config) {
		t.Fatalf("config parity: %v", err)
	}
	health := core.Health{State: core.HealthUnknown, Diagnostic: large}
	if got, err := HealthFrom(decodeRecord(t, HealthTo(health))); err != nil || !reflect.DeepEqual(got, health) {
		t.Fatalf("health parity: %v", err)
	}
	scope := core.CapabilityScope{Protocol: large, Mode: large, Model: large, AccountID: large}
	if got, err := ScopeFrom(decodeRecord(t, ScopeTo(scope))); err != nil || !reflect.DeepEqual(got, scope) {
		t.Fatalf("scope parity: %v", err)
	}
	capResult := core.CapabilityResult{Values: map[core.Capability]core.CapabilityState{"cap.supported": core.Supported, "cap.unsupported": core.Unsupported, "cap.unknown": core.Unknown}}
	if got, err := CapabilityResultFrom(decodeRecord(t, CapabilityResultTo(capResult))); err != nil || !reflect.DeepEqual(got, capResult) {
		t.Fatalf("capability result parity: %v", err)
	}
	modelQuery := core.ModelQuery{Protocol: large, Mode: large, AccountID: large}
	if got, err := ModelQueryFrom(decodeRecord(t, ModelQueryTo(modelQuery))); err != nil || !reflect.DeepEqual(got, modelQuery) {
		t.Fatalf("model query parity: %v", err)
	}
	available := false
	models := core.ModelsResult{Supported: true, Models: []core.ModelInfo{{ID: large, Capabilities: capResult.Values, Available: &available}, {ID: "nil availability"}}}
	if got, err := ModelsResultFrom(decodeRecord(t, ModelsResultTo(models))); err != nil || !reflect.DeepEqual(got, models) {
		t.Fatalf("models parity: %v", err)
	}
	usage := core.UsageReport{InputTokens: new(int64), OutputTokens: nil, ReasoningTokens: new(int64), CachedTokens: nil, Source: core.UsageProvider, Completeness: core.UsagePartial}
	query := core.UsageQuery{Protocol: large, Mode: large, Model: large, AccountID: large, MaxBodyBytes: 100, Payload: core.RawPayload{Protocol: large, ContentType: large, Body: body}}
	queryWire := UsageQueryTo(query)
	chunkedQuery := decodeRecord(t, queryWire)
	queryBody := BodyAssembler{routeLimit: 100}
	if err := WriteBody(query.Payload.Body, 100, queryBody.Add); err != nil {
		t.Fatal(err)
	}
	reassembled, err := queryBody.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := UsageQueryFrom(chunkedQuery, reassembled); err != nil || !reflect.DeepEqual(got, query) {
		t.Fatalf("usage query parity: %v", err)
	}
	if _, err := UsageQueryFrom(&gen.UsageQuery{Payload: &gen.RawPayload{}, MaxBodyBytes: -1}, nil); !errors.Is(err, ErrMalformed) {
		t.Fatalf("negative route body bound accepted: %v", err)
	}
	usageReport := &usage
	if got := UsageFrom(decodeRecord(t, UsageTo(usageReport))); !reflect.DeepEqual(got, usageReport) {
		t.Fatal("usage report parity")
	}
	estimate := core.EstimateResult{Supported: true, Known: false, Usage: usageReport, Method: large}
	if got, err := EstimateFrom(decodeRecord(t, EstimateTo(estimate))); err != nil || !reflect.DeepEqual(got, estimate) {
		t.Fatalf("estimate parity: %v", err)
	}
	authRequest := core.AuthRequest{AccountID: large, Action: "continue", State: body, CallbackInput: " https://example.invalid/callback?code=synthetic "}
	if got, err := AuthRequestFrom(decodeRecord(t, AuthRequestTo(authRequest))); err != nil || !reflect.DeepEqual(got, authRequest) {
		t.Fatalf("auth request parity: %v", err)
	}
	expiry := time.Date(2032, 4, 5, 6, 7, 8, 0, time.FixedZone("zone", 3600))
	authResult := core.AuthResult{Supported: true, State: string([]byte{0xff, 0, 0xfe}), NextAction: "continue", UserAction: &core.AuthUserAction{VerificationURI: "https://example.invalid", UserCode: "opaque", PollInterval: time.Second}, Credentials: map[string][]byte{"credential": body}, CredentialExpiresAt: &expiry}
	if got, err := AuthResultFrom(decodeRecord(t, AuthResultTo(authResult))); err != nil || got.State != authResult.State || got.Supported != authResult.Supported || got.NextAction != authResult.NextAction || !reflect.DeepEqual(got.UserAction, authResult.UserAction) || !reflect.DeepEqual(got.Credentials, authResult.Credentials) || !got.CredentialExpiresAt.Equal(expiry) {
		t.Fatalf("auth result parity: %v", err)
	}
	metadata := core.RequestMetadata{Headers: map[string][]string{"X-Repeat": {"first", "second"}}, Streaming: new(false), AffinityKnown: true, SessionBound: true, IngressHeaderBytes: 23, Extensions: map[string]any{}}
	metadataWire, err := MetadataTo(metadata)
	if err != nil {
		t.Fatal(err)
	}
	gotMetadata, err := MetadataFrom(decodeRecord(t, metadataWire))
	if err != nil || !reflect.DeepEqual(gotMetadata.Headers, metadata.Headers) || gotMetadata.Streaming == nil || *gotMetadata.Streaming || gotMetadata.AffinityKnown != metadata.AffinityKnown || gotMetadata.SessionBound != metadata.SessionBound || gotMetadata.IngressHeaderBytes != metadata.IngressHeaderBytes || len(gotMetadata.Extensions) != 0 {
		t.Fatalf("metadata parity: %v", err)
	}
	request := core.ExecutionRequest{ID: large, Model: large, Capabilities: map[core.Capability]struct{}{"llm.tools": {}, core.Capability(large): {}}, Metadata: metadata, Payload: core.RawPayload{Protocol: large, ContentType: large, Body: body}}
	requestWire, err := ExecutionRequestTo(request)
	if err != nil {
		t.Fatal(err)
	}
	gotRequest, err := ExecutionRequestFrom(decodeRecord(t, requestWire), body)
	if err != nil || !reflect.DeepEqual(gotRequest.ID, request.ID) || gotRequest.Model != request.Model || !reflect.DeepEqual(gotRequest.Capabilities, request.Capabilities) || !reflect.DeepEqual(gotRequest.Metadata.Headers, metadata.Headers) || !bytes.Equal(gotRequest.Payload.Body, body) {
		t.Fatalf("execution request parity: %v", err)
	}
	attempt := core.AttemptScope{ID: large, AccountID: large, Mode: large}
	if got, err := AttemptFrom(decodeRecord(t, AttemptTo(attempt))); err != nil || !reflect.DeepEqual(got, attempt) {
		t.Fatalf("attempt parity: %v", err)
	}
	delay := time.Duration(0)
	gatewayErr := &core.GatewayError{Code: large, Category: core.CategoryRateLimited, Retryable: true, RetryDisposition: core.RetryUnknown, Provider: large, OriginalError: large, Message: large, RetryAfter: &delay}
	if got, err := GatewayErrorFrom(decodeRecord(t, GatewayErrorTo(gatewayErr))); err != nil || !reflect.DeepEqual(got, gatewayErr) {
		t.Fatalf("gateway error parity: %v", err)
	}
	status := 0
	head := core.HeadFrame{Protocol: large, ContentType: large, HTTPStatus: &status, Headers: map[string][]string{large: {large, large}}, Error: gatewayErr}
	if got, err := HeadFrom(decodeRecord(t, mustHeadTo(t, &head))); err != nil || !reflect.DeepEqual(got, head) {
		t.Fatalf("head parity: %v", err)
	}
	complete := core.CompleteFrame{Outcome: core.OutcomeFailed, Usage: usageReport, Error: gatewayErr}
	if got, err := CompleteFrom(decodeRecord(t, CompleteTo(&complete))); err != nil || !reflect.DeepEqual(got, complete) {
		t.Fatalf("complete parity: %v", err)
	}
	streamWire, err := StreamFrameTo(core.StreamFrame{Type: core.FrameHead, Head: &head})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := StreamFrameFrom(decodeRecord(t, streamWire)); err != nil || got.Type != core.FrameHead || !reflect.DeepEqual(got.Head, &head) {
		t.Fatalf("stream head parity: %v", err)
	}
	if _, err := StreamFrameTo(core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: body}}); !errors.Is(err, ErrMalformed) {
		t.Fatal("typed response Body was accepted")
	}
	if _, err := StreamFrameFrom(&gen.StreamFrame{Type: string(core.FrameBody)}); !errors.Is(err, ErrMalformed) {
		t.Fatal("typed inbound Body bypass accepted")
	}
	if err := WriteRecord(Identity{Type: "BodyChunk"}, &gen.BodyChunk{Data: body}, func(*gen.RecordFragment) error { return nil }); !errors.Is(err, ErrMalformed) {
		t.Fatal("BodyChunk was accepted inside typed-record fragments")
	}
	if err := Reassemble(Identity{Type: "BodyChunk"}, []*gen.RecordFragment{{RecordType: "BodyChunk", TotalLength: 0, Final: true}}, &gen.BodyChunk{}); !errors.Is(err, ErrMalformed) {
		t.Fatal("inbound typed BodyChunk bypass accepted")
	}
	streamComplete, err := StreamFrameTo(core.StreamFrame{Type: core.FrameComplete, Complete: &complete})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := StreamFrameFrom(decodeRecord(t, streamComplete)); err != nil || got.Type != core.FrameComplete || !reflect.DeepEqual(got.Complete, &complete) {
		t.Fatalf("stream complete parity: %v", err)
	}
}

func TestM6CapabilityResultCollectionParity(t *testing.T) {
	want := core.CapabilityResult{Values: make(map[core.Capability]core.CapabilityState)}
	for i := 0; i < 1200; i++ {
		want.Values[core.Capability(fmt.Sprintf("capability-%04d-%s", i, strings.Repeat("x", 90)))] = []core.CapabilityState{core.Supported, core.Unsupported, core.Unknown}[i%3]
	}
	wire := CapabilityResultTo(want)
	if proto.Size(wire) <= 84<<10 {
		t.Fatalf("test collection too small: %d", proto.Size(wire))
	}
	got, err := CapabilityResultFrom(decodeRecord(t, wire))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("capability collection mismatch (%d values): %v", len(got.Values), err)
	}
}

func TestM6HTTPMetadataAndBodyCarriers(t *testing.T) {
	u, err := url.Parse("https://example.invalid/path?q=1")
	if err != nil {
		t.Fatal(err)
	}
	requestBody := strings.Repeat("request\x00\xff", 15000)
	req := &http.Request{Method: http.MethodPost, URL: u, Host: "override.invalid", Header: http.Header{"X-Repeat": {"first", "second"}}, ContentLength: int64(len(requestBody)), Body: io.NopCloser(strings.NewReader(requestBody))}
	reqWire, err := HTTPRequestTo(req)
	if err != nil {
		t.Fatal(err)
	}
	reqWire = decodeRecord(t, reqWire)
	reqGot, err := HTTPRequestFrom(reqWire, io.NopCloser(strings.NewReader(requestBody)))
	if err != nil {
		t.Fatal(err)
	}
	if reqGot.Method != req.Method || reqGot.URL.String() != req.URL.String() || reqGot.Host != req.Host || reqGot.ContentLength != req.ContentLength || !reflect.DeepEqual(reqGot.Header, req.Header) {
		t.Fatal("HTTP request metadata parity failed")
	}
	responseBody := strings.Repeat("response\x00\xff", 16000)
	resp := &http.Response{StatusCode: 207, Header: http.Header{"X-Repeat": {"a", "b"}}, ContentLength: int64(len(responseBody)), Body: io.NopCloser(strings.NewReader(responseBody))}
	respWire, err := HTTPResponseTo(resp)
	if err != nil {
		t.Fatal(err)
	}
	respGot, err := HTTPResponseFrom(decodeRecord(t, respWire), io.NopCloser(strings.NewReader(responseBody)))
	if err != nil {
		t.Fatal(err)
	}
	if respGot.StatusCode != resp.StatusCode || respGot.ContentLength != resp.ContentLength || !reflect.DeepEqual(respGot.Header, resp.Header) {
		t.Fatal("HTTP response metadata parity failed")
	}
	for _, tc := range []struct{ name, want string }{{"request", requestBody}, {"response", responseBody}} {
		t.Run(tc.name, func(t *testing.T) {
			var got []byte
			calls := 0
			err := WriteBodyReader(strings.NewReader(tc.want), func(chunk []byte) error {
				calls++
				carrier, e := BodyChunkTo(chunk)
				if e != nil {
					return e
				}
				data, e := BodyChunkFrom(carrier)
				got = append(got, data...)
				return e
			})
			if err != nil || !bytes.Equal(got, []byte(tc.want)) || calls < 2 {
				t.Fatalf("incremental body parity/calls %d: %v", calls, err)
			}
		})
	}
	if _, err := BodyChunkTo(make([]byte, MaxFragmentBytes+1)); !errors.Is(err, ErrMalformed) {
		t.Fatal("oversize body carrier accepted")
	}
	if _, err := HTTPRequestTo(&http.Request{URL: u, Header: http.Header{"X-Large": {strings.Repeat("h", MaxHTTPHeaderBytes+1)}}}); !errors.Is(err, ErrMalformed) {
		t.Fatal("over-limit HTTP headers accepted")
	}
}

type gatedHTTPBodyReader struct {
	release <-chan struct{}
	sent    bool
}

func (r *gatedHTTPBodyReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, []byte("x")), nil
	}
	<-r.release
	return 0, io.EOF
}

func TestM6HTTPBodyReaderForwardsBeforeNextRead(t *testing.T) {
	release := make(chan struct{})
	first := make(chan []byte, 1)
	done := make(chan error, 1)
	go func() {
		done <- WriteBodyReader(&gatedHTTPBodyReader{release: release}, func(chunk []byte) error { first <- append([]byte(nil), chunk...); return nil })
	}()
	select {
	case got := <-first:
		if !bytes.Equal(got, []byte("x")) {
			t.Fatalf("first chunk = %q", got)
		}
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("writer waited for the reader's later data or EOF")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestM6CodecRejectsUnknownEnumsAndInvalidPresence(t *testing.T) {
	if _, err := HealthFrom(&gen.Health{State: "future"}); !errors.Is(err, ErrMalformed) {
		t.Fatal("unknown health enum accepted")
	}
	if _, err := CapabilityResultFrom(&gen.CapabilityResult{Values: []*gen.CapabilityValue{{Key: "x", State: "future"}}}); !errors.Is(err, ErrMalformed) {
		t.Fatal("unknown capability enum accepted")
	}
	if _, err := GatewayErrorFrom(&gen.GatewayError{Category: "future"}); !errors.Is(err, ErrMalformed) {
		t.Fatal("unknown error category accepted")
	}
	if _, err := CompleteFrom(&gen.CompleteFrame{Outcome: string(core.OutcomeSucceeded), Error: &gen.GatewayError{Category: string(core.CategoryInternal)}}); !errors.Is(err, ErrMalformed) {
		t.Fatal("inconsistent completion accepted")
	}
	if _, err := GatewayErrorFrom(&gen.GatewayError{Category: string(core.CategoryInternal), RetryDisposition: "future"}); !errors.Is(err, ErrMalformed) {
		t.Fatal("unknown retry disposition accepted")
	}
	if _, err := AuthResultFrom(&gen.AuthResult{CredentialExpiresAt: &timestamppb.Timestamp{Seconds: math.MaxInt64}}); !errors.Is(err, ErrMalformed) {
		t.Fatal("invalid auth timestamp accepted")
	}
	if _, err := HTTPRequestFrom(&gen.HTTPRequest{Url: "http://%zz"}, nil); !errors.Is(err, ErrMalformed) {
		t.Fatal("malformed URL accepted")
	}
	if _, err := MetadataFrom(&gen.RequestMetadata{Headers: map[string]*gen.StringList{"x": nil}}); !errors.Is(err, ErrMalformed) {
		t.Fatal("nil header value accepted")
	}
}

func testBodyBoundary(t *testing.T, limit int) {
	t.Helper()
	body := bytes.Repeat([]byte{0, 0xff}, limit/2)
	a := BodyAssembler{routeLimit: uint64(limit)}
	if err := WriteBody(body, uint64(limit), a.Add); err != nil {
		t.Fatal(err)
	}
	got, err := a.Finish()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("body mismatch: %v", err)
	}
}
func roundTrip(t *testing.T, want, dst proto.Message, typ string) {
	t.Helper()
	id := Identity{Epoch: 4, CallID: 5, RecordID: 6, Type: typ}
	a := Assembler{identity: id}
	if err := WriteRecord(id, want, a.Add); err != nil {
		t.Fatal(err)
	}
	if err := a.Decode(dst); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(want, dst) {
		t.Fatal("typed record mismatch")
	}
}

func decodeRecord[T proto.Message](t *testing.T, want T) T {
	t.Helper()
	id := Identity{Epoch: 11, CallID: 12, RecordID: 13, Type: string(want.ProtoReflect().Descriptor().Name())}
	dst := want.ProtoReflect().Type().New().Interface().(T)
	var fragments []*gen.RecordFragment
	if err := WriteRecord(id, want, func(f *gen.RecordFragment) error { fragments = append(fragments, f); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := Reassemble(id, fragments, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}
func mustHeadTo(t *testing.T, v *core.HeadFrame) *gen.HeadFrame {
	t.Helper()
	out, err := HeadTo(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func cloneFragments(in []*gen.RecordFragment) []*gen.RecordFragment {
	out := make([]*gen.RecordFragment, len(in))
	for i, f := range in {
		out[i] = proto.Clone(f).(*gen.RecordFragment)
	}
	return out
}

func recordEnvelopeAtSize(t *testing.T, size int) []byte {
	t.Helper()
	lo, hi := 0, size
	for lo <= hi {
		mid := lo + (hi-lo)/2
		f := &gen.RecordFragment{Epoch: 1, CallId: 2, RecordId: 3, RecordType: "wire-limit-test", TotalLength: uint64(mid), Data: make([]byte, mid)}
		encoded, err := proto.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case len(encoded) == size:
			return encoded
		case len(encoded) < size:
			lo = mid + 1
		default:
			hi = mid - 1
		}
	}
	t.Fatalf("could not construct protobuf RecordFragment with serialized size %d", size)
	return nil
}

func wireFrameAtSize(t *testing.T, size int) []byte {
	t.Helper()
	message := recordEnvelopeAtSize(t, size-grpcPrefixBytes)
	frame := make([]byte, grpcPrefixBytes, len(message)+grpcPrefixBytes)
	binary.BigEndian.PutUint32(frame[1:], uint32(len(message)))
	return append(frame, message...)
}
