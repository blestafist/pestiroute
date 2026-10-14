package ipc

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"golang.org/x/net/http/httpguts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func DescriptorTo(v core.Descriptor) *gen.Descriptor {
	o := &gen.Descriptor{Id: v.ID, Kind: string(v.Kind), ImplementationVersion: v.ImplementationVersion, Protocols: append([]string(nil), v.Protocols...), Operations: append([]string(nil), v.Operations...), ConnectorType: v.ConnectorType, AuthMethods: append([]string(nil), v.AuthMethods...)}
	for _, x := range v.APIVersions {
		o.ApiVersions = append(o.ApiVersions, &gen.Version{Major: uint32(x.Major), Minor: uint32(x.Minor)})
	}
	return o
}
func DescriptorFrom(v *gen.Descriptor) (core.Descriptor, error) {
	if v == nil {
		return core.Descriptor{}, ErrMalformed
	}
	o := core.Descriptor{ID: v.Id, Kind: core.ComponentKind(v.Kind), ImplementationVersion: v.ImplementationVersion, Protocols: append([]string(nil), v.Protocols...), Operations: append([]string(nil), v.Operations...), ConnectorType: v.ConnectorType, AuthMethods: append([]string(nil), v.AuthMethods...)}
	for _, x := range v.ApiVersions {
		if x == nil || x.Major > 65535 || x.Minor > 65535 {
			return core.Descriptor{}, ErrMalformed
		}
		o.APIVersions = append(o.APIVersions, core.APIVersion{Major: uint16(x.Major), Minor: uint16(x.Minor)})
	}
	if o.Kind != core.ComponentAdapter && o.Kind != core.ComponentConnector {
		return core.Descriptor{}, ErrMalformed
	}
	if err := o.Validate(o.Kind); err != nil {
		return core.Descriptor{}, fmt.Errorf("%w: descriptor", ErrMalformed)
	}
	return o, nil
}
func ConfigTo(v core.ComponentConfig) *gen.ComponentConfig {
	return &gen.ComponentConfig{Data: append([]byte(nil), v.Data...)}
}
func ConfigFrom(v *gen.ComponentConfig) (core.ComponentConfig, error) {
	if v == nil {
		return core.ComponentConfig{}, ErrMalformed
	}
	return core.ComponentConfig{Data: append([]byte(nil), v.Data...)}, nil
}
func HealthTo(v core.Health) *gen.Health {
	return &gen.Health{State: string(v.State), Diagnostic: v.Diagnostic}
}
func HealthFrom(v *gen.Health) (core.Health, error) {
	if v == nil {
		return core.Health{}, ErrMalformed
	}
	switch core.HealthState(v.State) {
	case core.HealthReady, core.HealthUnavailable, core.HealthUnknown:
	default:
		return core.Health{}, ErrMalformed
	}
	return core.Health{State: core.HealthState(v.State), Diagnostic: v.Diagnostic}, nil
}
func ScopeTo(v core.CapabilityScope) *gen.CapabilityScope {
	return &gen.CapabilityScope{Protocol: v.Protocol, Mode: v.Mode, Model: v.Model, AccountId: v.AccountID}
}
func ScopeFrom(v *gen.CapabilityScope) (core.CapabilityScope, error) {
	if v == nil {
		return core.CapabilityScope{}, ErrMalformed
	}
	return core.CapabilityScope{Protocol: v.Protocol, Mode: v.Mode, Model: v.Model, AccountID: v.AccountId}, nil
}
func capValuesTo(v map[core.Capability]core.CapabilityState) []*gen.CapabilityValue {
	out := make([]*gen.CapabilityValue, 0, len(v))
	for k, s := range v {
		out = append(out, &gen.CapabilityValue{Key: string(k), State: string(s)})
	}
	return out
}
func capValuesFrom(v []*gen.CapabilityValue) (map[core.Capability]core.CapabilityState, error) {
	if v == nil {
		return nil, nil
	}
	out := make(map[core.Capability]core.CapabilityState, len(v))
	for _, x := range v {
		if x == nil {
			return nil, ErrMalformed
		}
		k := core.Capability(x.Key)
		switch core.CapabilityState(x.State) {
		case core.Supported, core.Unsupported, core.Unknown:
		default:
			return nil, ErrMalformed
		}
		if _, ok := out[k]; ok {
			return nil, ErrMalformed
		}
		out[k] = core.CapabilityState(x.State)
	}
	return out, nil
}
func CapabilityResultTo(v core.CapabilityResult) *gen.CapabilityResult {
	return &gen.CapabilityResult{Values: capValuesTo(v.Values)}
}
func CapabilityResultFrom(v *gen.CapabilityResult) (core.CapabilityResult, error) {
	if v == nil {
		return core.CapabilityResult{}, ErrMalformed
	}
	m, e := capValuesFrom(v.Values)
	return core.CapabilityResult{Values: m}, e
}
func ModelQueryTo(v core.ModelQuery) *gen.ModelQuery {
	return &gen.ModelQuery{Protocol: v.Protocol, Mode: v.Mode, AccountId: v.AccountID}
}
func ModelQueryFrom(v *gen.ModelQuery) (core.ModelQuery, error) {
	if v == nil {
		return core.ModelQuery{}, ErrMalformed
	}
	return core.ModelQuery{Protocol: v.Protocol, Mode: v.Mode, AccountID: v.AccountId}, nil
}
func ModelsResultTo(v core.ModelsResult) *gen.ModelsResult {
	o := &gen.ModelsResult{Supported: v.Supported}
	for _, m := range v.Models {
		x := &gen.ModelInfo{Id: m.ID, Capabilities: capValuesTo(m.Capabilities)}
		if m.Available != nil {
			b := *m.Available
			x.Available = &b
		}
		o.Models = append(o.Models, x)
	}
	return o
}
func ModelsResultFrom(v *gen.ModelsResult) (core.ModelsResult, error) {
	if v == nil {
		return core.ModelsResult{}, ErrMalformed
	}
	o := core.ModelsResult{Supported: v.Supported}
	if v.Models != nil {
		o.Models = make([]core.ModelInfo, 0, len(v.Models))
	}
	for _, m := range v.Models {
		if m == nil {
			return core.ModelsResult{}, ErrMalformed
		}
		c, e := capValuesFrom(m.Capabilities)
		if e != nil {
			return core.ModelsResult{}, e
		}
		x := core.ModelInfo{ID: m.Id, Capabilities: c}
		if m.Available != nil {
			b := *m.Available
			x.Available = &b
		}
		o.Models = append(o.Models, x)
	}
	return o, nil
}
func UsageQueryTo(v core.UsageQuery) *gen.UsageQuery {
	return &gen.UsageQuery{Protocol: v.Protocol, Mode: v.Mode, Model: v.Model, AccountId: v.AccountID, Payload: &gen.RawPayload{Protocol: v.Payload.Protocol, ContentType: v.Payload.ContentType}}
}
func UsageQueryFrom(v *gen.UsageQuery, body []byte) (core.UsageQuery, error) {
	if v == nil || v.Payload == nil {
		return core.UsageQuery{}, ErrMalformed
	}
	return core.UsageQuery{Protocol: v.Protocol, Mode: v.Mode, Model: v.Model, AccountID: v.AccountId, Payload: core.RawPayload{Protocol: v.Payload.Protocol, ContentType: v.Payload.ContentType, Body: append([]byte(nil), body...)}}, nil
}
func UsageTo(v *core.UsageReport) *gen.UsageReport {
	if v == nil {
		return nil
	}
	return &gen.UsageReport{InputTokens: cloneInt64(v.InputTokens), OutputTokens: cloneInt64(v.OutputTokens), ReasoningTokens: cloneInt64(v.ReasoningTokens), CachedTokens: cloneInt64(v.CachedTokens), Source: string(v.Source), Completeness: string(v.Completeness)}
}
func UsageFrom(v *gen.UsageReport) *core.UsageReport {
	if v == nil {
		return nil
	}
	return &core.UsageReport{InputTokens: cloneInt64(v.InputTokens), OutputTokens: cloneInt64(v.OutputTokens), ReasoningTokens: cloneInt64(v.ReasoningTokens), CachedTokens: cloneInt64(v.CachedTokens), Source: core.UsageSource(v.Source), Completeness: core.UsageCompleteness(v.Completeness)}
}
func cloneInt64(p *int64) *int64 {
	if p == nil {
		return nil
	}
	x := *p
	return &x
}
func validateUsage(v *gen.UsageReport) error {
	if v == nil {
		return nil
	}
	switch core.UsageSource(v.Source) {
	case core.UsageProvider, core.UsageEstimate, core.UsageUnknown:
	default:
		return ErrMalformed
	}
	switch core.UsageCompleteness(v.Completeness) {
	case core.UsageComplete, core.UsagePartial, core.UsageUnknownCompleteness:
	default:
		return ErrMalformed
	}
	return nil
}
func EstimateTo(v core.EstimateResult) *gen.EstimateResult {
	return &gen.EstimateResult{Supported: v.Supported, Known: v.Known, Usage: UsageTo(v.Usage), Method: v.Method}
}
func EstimateFrom(v *gen.EstimateResult) (core.EstimateResult, error) {
	if v == nil {
		return core.EstimateResult{}, ErrMalformed
	}
	if err := validateUsage(v.Usage); err != nil {
		return core.EstimateResult{}, err
	}
	return core.EstimateResult{Supported: v.Supported, Known: v.Known, Usage: UsageFrom(v.Usage), Method: v.Method}, nil
}
func AuthRequestTo(v core.AuthRequest) *gen.AuthRequest {
	return &gen.AuthRequest{AccountId: v.AccountID, Action: v.Action, State: append([]byte(nil), v.State...)}
}
func AuthRequestFrom(v *gen.AuthRequest) (core.AuthRequest, error) {
	if v == nil {
		return core.AuthRequest{}, ErrMalformed
	}
	return core.AuthRequest{AccountID: v.AccountId, Action: v.Action, State: append([]byte(nil), v.State...)}, nil
}
func AuthResultTo(v core.AuthResult) *gen.AuthResult {
	o := &gen.AuthResult{Supported: v.Supported, State: []byte(v.State), NextAction: v.NextAction, Credentials: cloneBytesMap(v.Credentials)}
	if v.UserAction != nil {
		o.UserAction = &gen.AuthUserAction{VerificationUri: v.UserAction.VerificationURI, UserCode: v.UserAction.UserCode, PollIntervalNs: int64(v.UserAction.PollInterval)}
	}
	if v.CredentialExpiresAt != nil {
		o.CredentialExpiresAt = timestamppb.New(v.CredentialExpiresAt.UTC())
	}
	return o
}
func AuthResultFrom(v *gen.AuthResult) (core.AuthResult, error) {
	if v == nil {
		return core.AuthResult{}, ErrMalformed
	}
	o := core.AuthResult{Supported: v.Supported, State: string(v.State), NextAction: v.NextAction, Credentials: cloneBytesMap(v.Credentials)}
	if v.UserAction != nil {
		o.UserAction = &core.AuthUserAction{VerificationURI: v.UserAction.VerificationUri, UserCode: v.UserAction.UserCode, PollInterval: time.Duration(v.UserAction.PollIntervalNs)}
	}
	if v.CredentialExpiresAt != nil {
		if e := v.CredentialExpiresAt.CheckValid(); e != nil {
			return core.AuthResult{}, fmt.Errorf("%w: timestamp", ErrMalformed)
		}
		t := v.CredentialExpiresAt.AsTime().UTC()
		o.CredentialExpiresAt = &t
	}
	return o, nil
}

func cloneBytesMap(in map[string][]byte) map[string][]byte {
	if in == nil {
		return nil
	}
	out := make(map[string][]byte, len(in))
	for k, v := range in {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

func MetadataTo(v core.RequestMetadata) (*gen.RequestMetadata, error) {
	if len(v.Extensions) != 0 {
		return nil, fmt.Errorf("%w: undeclared extensions", ErrMalformed)
	}
	o := &gen.RequestMetadata{Headers: make(map[string]*gen.StringList, len(v.Headers)), AffinityKnown: v.AffinityKnown, SessionBound: v.SessionBound, IngressHeaderBytes: v.IngressHeaderBytes}
	for k, values := range v.Headers {
		o.Headers[k] = &gen.StringList{Values: append([]string(nil), values...)}
	}
	if v.Streaming != nil {
		b := *v.Streaming
		o.Streaming = &b
	}
	return o, nil
}
func MetadataFrom(v *gen.RequestMetadata) (core.RequestMetadata, error) {
	if v == nil {
		return core.RequestMetadata{}, ErrMalformed
	}
	if len(v.Extensions) != 0 {
		return core.RequestMetadata{}, fmt.Errorf("%w: undeclared extensions", ErrMalformed)
	}
	o := core.RequestMetadata{AffinityKnown: v.AffinityKnown, SessionBound: v.SessionBound, IngressHeaderBytes: v.IngressHeaderBytes}
	if v.Headers != nil {
		o.Headers = make(map[string][]string, len(v.Headers))
	}
	for k, x := range v.Headers {
		if x == nil {
			return core.RequestMetadata{}, ErrMalformed
		}
		o.Headers[k] = append([]string(nil), x.Values...)
	}
	if v.Streaming != nil {
		b := *v.Streaming
		o.Streaming = &b
	}
	return o, nil
}
func ExecutionRequestTo(v core.ExecutionRequest) (*gen.ExecutionRequest, error) {
	m, e := MetadataTo(v.Metadata)
	if e != nil {
		return nil, e
	}
	c := make([]string, 0, len(v.Capabilities))
	for k := range v.Capabilities {
		c = append(c, string(k))
	}
	return &gen.ExecutionRequest{Id: v.ID, Model: v.Model, Capabilities: c, Metadata: m, Payload: &gen.RawPayload{Protocol: v.Payload.Protocol, ContentType: v.Payload.ContentType}}, nil
}
func ExecutionRequestFrom(v *gen.ExecutionRequest, body []byte) (core.ExecutionRequest, error) {
	if v == nil || v.Metadata == nil || v.Payload == nil {
		return core.ExecutionRequest{}, ErrMalformed
	}
	m, e := MetadataFrom(v.Metadata)
	if e != nil {
		return core.ExecutionRequest{}, e
	}
	o := core.ExecutionRequest{ID: v.Id, Model: v.Model, Metadata: m, Payload: core.RawPayload{Protocol: v.Payload.Protocol, ContentType: v.Payload.ContentType, Body: append([]byte(nil), body...)}}
	if v.Capabilities != nil {
		o.Capabilities = make(map[core.Capability]struct{}, len(v.Capabilities))
	}
	for _, c := range v.Capabilities {
		if _, ok := o.Capabilities[core.Capability(c)]; ok {
			return core.ExecutionRequest{}, ErrMalformed
		}
		o.Capabilities[core.Capability(c)] = struct{}{}
	}
	return o, nil
}
func AttemptTo(v core.AttemptScope) *gen.AttemptScope {
	return &gen.AttemptScope{Id: v.ID, AccountId: v.AccountID, Mode: v.Mode}
}
func AttemptFrom(v *gen.AttemptScope) (core.AttemptScope, error) {
	if v == nil {
		return core.AttemptScope{}, ErrMalformed
	}
	return core.AttemptScope{ID: v.Id, AccountID: v.AccountId, Mode: v.Mode}, nil
}

func GatewayErrorTo(v *core.GatewayError) *gen.GatewayError {
	if v == nil {
		return nil
	}
	o := &gen.GatewayError{Code: v.Code, Category: string(v.Category), Retryable: v.Retryable, RetryDisposition: string(v.RetryDisposition), Provider: v.Provider, OriginalError: v.OriginalError, Message: v.Message}
	if v.RetryAfter != nil {
		n := int64(*v.RetryAfter)
		o.RetryAfterNs = &n
	}
	return o
}
func GatewayErrorFrom(v *gen.GatewayError) (*core.GatewayError, error) {
	if v == nil {
		return nil, nil
	}
	switch core.ErrorCategory(v.Category) {
	case core.CategoryInvalidRequest, core.CategoryUnsupportedFeature, core.CategoryUnauthenticated, core.CategoryPermissionDenied, core.CategoryRateLimited, core.CategoryUnavailable, core.CategoryTimeout, core.CategoryCancelled, core.CategoryInternal:
	default:
		return nil, ErrMalformed
	}
	d := core.RetryDisposition(v.RetryDisposition)
	if d != "" && d != core.RetrySafe && d != core.RetryUnsafe && d != core.RetryUnknown {
		return nil, ErrMalformed
	}
	o := &core.GatewayError{Code: v.Code, Category: core.ErrorCategory(v.Category), Retryable: v.Retryable, RetryDisposition: d, Provider: v.Provider, OriginalError: v.OriginalError, Message: v.Message}
	if v.RetryAfterNs != nil {
		x := time.Duration(*v.RetryAfterNs)
		o.RetryAfter = &x
	}
	return o, nil
}
func HeadTo(v *core.HeadFrame) (*gen.HeadFrame, error) {
	o := &gen.HeadFrame{Protocol: v.Protocol, ContentType: v.ContentType, Headers: make(map[string]*gen.StringList, len(v.Headers)), Error: GatewayErrorTo(v.Error)}
	for k, x := range v.Headers {
		o.Headers[k] = &gen.StringList{Values: append([]string(nil), x...)}
	}
	if v.HTTPStatus != nil {
		if *v.HTTPStatus < math.MinInt32 || *v.HTTPStatus > math.MaxInt32 {
			return nil, ErrMalformed
		}
		x := int32(*v.HTTPStatus)
		o.HttpStatus = &x
	}
	return o, nil
}
func HeadFrom(v *gen.HeadFrame) (core.HeadFrame, error) {
	if v == nil {
		return core.HeadFrame{}, ErrMalformed
	}
	o := core.HeadFrame{Protocol: v.Protocol, ContentType: v.ContentType, Headers: make(map[string][]string)}
	for k, x := range v.Headers {
		if x == nil {
			return core.HeadFrame{}, ErrMalformed
		}
		o.Headers[k] = append([]string(nil), x.Values...)
	}
	if v.HttpStatus != nil {
		x := int(*v.HttpStatus)
		o.HTTPStatus = &x
	}
	e, err := GatewayErrorFrom(v.Error)
	if err != nil {
		return core.HeadFrame{}, err
	}
	o.Error = e
	return o, nil
}
func CompleteTo(v *core.CompleteFrame) *gen.CompleteFrame {
	return &gen.CompleteFrame{Outcome: string(v.Outcome), Usage: UsageTo(v.Usage), Error: GatewayErrorTo(v.Error)}
}
func CompleteFrom(v *gen.CompleteFrame) (core.CompleteFrame, error) {
	if v == nil {
		return core.CompleteFrame{}, ErrMalformed
	}
	if err := validateUsage(v.Usage); err != nil {
		return core.CompleteFrame{}, err
	}
	out := core.Outcome(v.Outcome)
	switch out {
	case core.OutcomeSucceeded, core.OutcomeFailed, core.OutcomeCancelled, core.OutcomeIncomplete:
	default:
		return core.CompleteFrame{}, ErrMalformed
	}
	e, err := GatewayErrorFrom(v.Error)
	if err != nil {
		return core.CompleteFrame{}, err
	}
	if (out == core.OutcomeSucceeded) != (e == nil) {
		return core.CompleteFrame{}, ErrMalformed
	}
	return core.CompleteFrame{Outcome: out, Usage: UsageFrom(v.Usage), Error: e}, nil
}
func StreamFrameTo(v core.StreamFrame) (*gen.StreamFrame, error) {
	o := &gen.StreamFrame{Type: string(v.Type)}
	switch v.Type {
	case core.FrameHead:
		if v.Head == nil || v.Body != nil || v.Complete != nil {
			return nil, ErrMalformed
		}
		h, err := HeadTo(v.Head)
		if err != nil {
			return nil, err
		}
		o.Head = h
	case core.FrameBody:
		return nil, fmt.Errorf("%w: body must use BodyChunk", ErrMalformed)
	case core.FrameComplete:
		if v.Complete == nil || v.Head != nil || v.Body != nil {
			return nil, ErrMalformed
		}
		o.Complete = CompleteTo(v.Complete)
	default:
		return nil, ErrMalformed
	}
	return o, nil
}
func StreamFrameFrom(v *gen.StreamFrame) (core.StreamFrame, error) {
	if v == nil {
		return core.StreamFrame{}, ErrMalformed
	}
	switch core.FrameType(v.Type) {
	case core.FrameHead:
		if v.Head == nil || v.Complete != nil {
			return core.StreamFrame{}, ErrMalformed
		}
		x, e := HeadFrom(v.Head)
		return core.StreamFrame{Type: core.FrameHead, Head: &x}, e
	case core.FrameBody:
		return core.StreamFrame{}, fmt.Errorf("%w: body must use BodyChunk", ErrMalformed)
	case core.FrameComplete:
		if v.Complete == nil || v.Head != nil {
			return core.StreamFrame{}, ErrMalformed
		}
		x, e := CompleteFrom(v.Complete)
		return core.StreamFrame{Type: core.FrameComplete, Complete: &x}, e
	default:
		return core.StreamFrame{}, ErrMalformed
	}
}

const MaxHTTPHeaderBytes = 16 << 10

func BodyChunkTo(b []byte) (*gen.BodyChunk, error) {
	if len(b) == 0 || len(b) > MaxFragmentBytes {
		return nil, ErrMalformed
	}
	return &gen.BodyChunk{Data: append([]byte(nil), b...)}, nil
}
func BodyChunkFrom(v *gen.BodyChunk) ([]byte, error) {
	if v == nil || len(v.Data) == 0 || len(v.Data) > MaxFragmentBytes {
		return nil, ErrMalformed
	}
	return append([]byte(nil), v.Data...), nil
}

func HTTPRequestTo(v *http.Request) (*gen.HTTPRequest, error) {
	if v == nil || v.URL == nil {
		return nil, ErrMalformed
	}
	headers, err := httpHeadersTo(v.Header)
	if err != nil {
		return nil, err
	}
	n := v.ContentLength
	return &gen.HTTPRequest{Method: v.Method, Url: v.URL.String(), Host: v.Host, Headers: headers, ContentLength: &n}, nil
}
func HTTPRequestFrom(v *gen.HTTPRequest, body io.ReadCloser) (*http.Request, error) {
	if v == nil {
		return nil, ErrMalformed
	}
	u, err := url.Parse(v.Url)
	if err != nil {
		return nil, fmt.Errorf("%w: HTTP URL", ErrMalformed)
	}
	headers, err := httpHeadersFrom(v.Headers)
	if err != nil {
		return nil, err
	}
	o := &http.Request{Method: v.Method, URL: u, Host: v.Host, Header: headers, Body: body}
	if v.ContentLength != nil {
		o.ContentLength = *v.ContentLength
	}
	return o, nil
}
func HTTPResponseTo(v *http.Response) (*gen.HTTPResponse, error) {
	if v == nil || v.StatusCode < math.MinInt32 || v.StatusCode > math.MaxInt32 {
		return nil, ErrMalformed
	}
	headers, err := httpHeadersTo(v.Header)
	if err != nil {
		return nil, err
	}
	status := int32(v.StatusCode)
	length := v.ContentLength
	return &gen.HTTPResponse{StatusCode: status, Headers: headers, ContentLength: &length}, nil
}
func HTTPResponseFrom(v *gen.HTTPResponse, body io.ReadCloser) (*http.Response, error) {
	if v == nil {
		return nil, ErrMalformed
	}
	headers, err := httpHeadersFrom(v.Headers)
	if err != nil {
		return nil, err
	}
	o := &http.Response{StatusCode: int(v.StatusCode), Header: headers, Body: body}
	if v.ContentLength != nil {
		o.ContentLength = *v.ContentLength
	}
	return o, nil
}
func httpHeadersTo(in http.Header) (map[string]*gen.StringList, error) {
	out := make(map[string]*gen.StringList, len(in))
	var size uint64
	for k, values := range in {
		if !httpguts.ValidHeaderFieldName(k) {
			return nil, ErrMalformed
		}
		if uint64(len(k)) > MaxHTTPHeaderBytes-size {
			return nil, ErrMalformed
		}
		size += uint64(len(k))
		for _, v := range values {
			if !httpguts.ValidHeaderFieldValue(v) {
				return nil, ErrMalformed
			}
			if uint64(len(v)) > MaxHTTPHeaderBytes-size {
				return nil, ErrMalformed
			}
			size += uint64(len(v))
		}
		out[k] = &gen.StringList{Values: append([]string(nil), values...)}
	}
	return out, nil
}
func httpHeadersFrom(in map[string]*gen.StringList) (http.Header, error) {
	out := make(http.Header, len(in))
	var size uint64
	for k, list := range in {
		if list == nil {
			return nil, ErrMalformed
		}
		if !httpguts.ValidHeaderFieldName(k) {
			return nil, ErrMalformed
		}
		if uint64(len(k)) > MaxHTTPHeaderBytes-size {
			return nil, ErrMalformed
		}
		size += uint64(len(k))
		for _, v := range list.Values {
			if !httpguts.ValidHeaderFieldValue(v) {
				return nil, ErrMalformed
			}
			if uint64(len(v)) > MaxHTTPHeaderBytes-size {
				return nil, ErrMalformed
			}
			size += uint64(len(v))
		}
		out[k] = append([]string(nil), list.Values...)
	}
	return out, nil
}
