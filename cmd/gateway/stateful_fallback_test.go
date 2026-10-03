package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/core"
)

type affinityFallbackConnector struct {
	mu       sync.Mutex
	calls    int
	requests []affinityRequest
	failure  *core.GatewayError
	probe    *affinityProbe
}

type affinityRequest struct {
	model         string
	body          []byte
	affinityKnown bool
	sessionBound  bool
}

type affinityProbe struct {
	keyAuth, capabilities, estimates, accountAuthorization atomic.Int32
	admit, beginAttempt, dispatchIntent, finalize          atomic.Int32
}

func (*affinityFallbackConnector) Descriptor() core.Descriptor {
	return core.Descriptor{ID: "test.affinity.connector", Kind: core.ComponentConnector, ImplementationVersion: "1.0.0", APIVersions: []core.APIVersion{{Major: 1}}, Protocols: []string{responsesProtocol}, ConnectorType: "api"}
}
func (*affinityFallbackConnector) Init(context.Context, core.ComponentConfig) error { return nil }
func (*affinityFallbackConnector) Health(context.Context) core.Health {
	return core.Health{State: core.HealthReady}
}
func (c *affinityFallbackConnector) Capabilities(context.Context, core.CapabilityScope) core.CapabilityResult {
	if c.probe != nil {
		c.probe.capabilities.Add(1)
	}
	return core.CapabilityResult{Values: map[core.Capability]core.CapabilityState{}}
}
func (*affinityFallbackConnector) Close(context.Context) error { return nil }
func (*affinityFallbackConnector) HTTPDoer() core.HTTPDoer     { return affinityFallbackDoer{} }
func (c *affinityFallbackConnector) Models(context.Context, core.ModelQuery, core.InvocationServices) (core.ModelsResult, *core.GatewayError) {
	return core.ModelsResult{}, nil
}
func (c *affinityFallbackConnector) EstimateUsage(context.Context, core.UsageQuery, core.InvocationServices) (core.EstimateResult, *core.GatewayError) {
	if c.probe != nil {
		c.probe.estimates.Add(1)
	}
	return core.EstimateResult{Supported: true, Known: true, Usage: &core.UsageReport{InputTokens: ptrInt64(10)}}, nil
}
func (c *affinityFallbackConnector) Authenticate(context.Context, core.AuthRequest, core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	return core.AuthResult{}, nil
}
func (c *affinityFallbackConnector) Execute(_ context.Context, req core.ExecutionRequest, _ core.AttemptScope, _ core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.requests = append(c.requests, affinityRequest{model: req.Model, body: bytes.Clone(req.Payload.Body), affinityKnown: req.Metadata.AffinityKnown, sessionBound: req.Metadata.SessionBound})
	if c.failure != nil {
		return core.ExecutionResponse{}, c.failure
	}
	return core.ExecutionResponse{Stream: &affinityResponseStream{}}, nil
}
func (c *affinityFallbackConnector) snapshot() (int, []affinityRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	requests := make([]affinityRequest, len(c.requests))
	for i := range c.requests {
		requests[i] = c.requests[i]
		requests[i].body = bytes.Clone(c.requests[i].body)
	}
	return c.calls, requests
}

type affinityFallbackDoer struct{}

func (affinityFallbackDoer) Do(*http.Request) (*http.Response, error) {
	panic("unexpected transport call")
}

type affinityResponseStream struct{ index int }

func (s *affinityResponseStream) Next(context.Context) (core.StreamFrame, error) {
	s.index++
	switch s.index {
	case 1:
		return core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: responsesProtocol, ContentType: "application/json"}}, nil
	case 2:
		return core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: []byte(`{"status":"completed"}`)}}, nil
	case 3:
		return core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded}}, nil
	default:
		return core.StreamFrame{}, io.EOF
	}
}
func (*affinityResponseStream) Close() error { return nil }

func affinityFallbackGateway(t *testing.T, candidates []*affinityFallbackConnector, probe *affinityProbe) *httptest.Server {
	t.Helper()
	if probe == nil {
		probe = &affinityProbe{}
	}
	c := config{Listen: "127.0.0.1:0", Components: []topologyComponent{{ID: "adapter", Implementation: "pestiroute.responses.native", Kind: core.ComponentAdapter}}}
	for i := range candidates {
		candidates[i].probe = probe
		id, account, env := core.InstanceID("connector-a"), "account-a", "AFFINITY_TEST_A"
		if i == 1 {
			id, account, env = "connector-b", "account-b", "AFFINITY_TEST_B"
		}
		t.Setenv(env, "synthetic")
		c.Components = append(c.Components, topologyComponent{ID: id, Implementation: "test.affinity", Kind: core.ComponentConnector, CredentialEnv: env, MaxBodyBytes: 1 << 20, MaxHeaderBytes: 8192})
		group := ""
		maxAttempts := 2
		if len(candidates) > 1 {
			group, maxAttempts = "affinity-fallback", 2
		}
		c.Routes = append(c.Routes, topologyRoute{Protocol: responsesProtocol, Mode: core.ModeNative, Model: "gpt-5.4-mini", Account: account, Adapter: "adapter", Connector: id, RouteID: "affinity-route", CandidateGroup: group, RetryMaxAttempts: maxAttempts, RetryDeadline: time.Minute, Budget: core.RouteBudget{UnknownEstimate: core.UnknownEstimateReserve, ConservativeTokens: 10}, BudgetPolicy: "reserve"})
	}
	c.keyStore = affinityKeyStore{probe: probe, principal: core.TrustedPrincipal{KeyID: "affinity-key", PolicyID: "affinity-policy", KeyRevision: 1, PolicyRevision: 1}}
	c.policyStore = testPolicyStore{snapshot: core.PolicySnapshot{ID: "affinity-policy", Revision: 1, Enabled: true,
		Models: []string{"gpt-5.4-mini"}, Connectors: []string{"connector-a", "connector-b"}}}
	c.accounting = affinityAccountingStore{probe: probe}
	c.accountAuthorizer = affinityAccountAuthorizer{probe: probe}
	var ready, draining atomic.Bool
	handler, closeComponents, err := composeHandlerWithFactory(c, &ready, &draining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return responses.NewAdapter()
		}
		if item.ID == "connector-b" {
			return candidates[1]
		}
		return candidates[0]
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(func() { server.Close(); _ = closeComponents(context.Background()) })
	return server
}

type affinityKeyStore struct {
	probe     *affinityProbe
	principal core.TrustedPrincipal
}

func (s affinityKeyStore) Verify(context.Context, string) (core.TrustedPrincipal, error) {
	if s.probe != nil {
		s.probe.keyAuth.Add(1)
	}
	return s.principal, nil
}

type affinityAccountAuthorizer struct{ probe *affinityProbe }

func (a affinityAccountAuthorizer) AuthorizeAccount(context.Context, string, string) error {
	if a.probe != nil {
		a.probe.accountAuthorization.Add(1)
	}
	return nil
}

type affinityAccountingStore struct{ probe *affinityProbe }

func (a affinityAccountingStore) Admit(context.Context, core.AccountingAdmission) error {
	a.probe.admit.Add(1)
	return nil
}
func (a affinityAccountingStore) BeginAttempt(context.Context, core.AccountingAdmission) error {
	a.probe.beginAttempt.Add(1)
	return nil
}
func (a affinityAccountingStore) RecordDispatchIntent(context.Context, string, time.Time) error {
	a.probe.dispatchIntent.Add(1)
	return nil
}
func (a affinityAccountingStore) FinalizeAttempt(context.Context, core.AccountingTerminal) error {
	a.probe.finalize.Add(1)
	return nil
}

func assertAffinityRejectedBeforeCandidates(t *testing.T, probe *affinityProbe) {
	t.Helper()
	if probe.keyAuth.Load() != 1 {
		t.Errorf("northbound key verification calls=%d, want 1", probe.keyAuth.Load())
	}
	if probe.capabilities.Load() != 0 || probe.estimates.Load() != 0 || probe.accountAuthorization.Load() != 0 ||
		probe.admit.Load() != 0 || probe.beginAttempt.Load() != 0 || probe.dispatchIntent.Load() != 0 || probe.finalize.Load() != 0 {
		t.Errorf("candidate operations before affinity rejection: capabilities=%d estimates=%d account-auth=%d admit=%d begin=%d intent=%d finalize=%d",
			probe.capabilities.Load(), probe.estimates.Load(), probe.accountAuthorization.Load(), probe.admit.Load(), probe.beginAttempt.Load(), probe.dispatchIntent.Load(), probe.finalize.Load())
	}
}

func affinityPost(t *testing.T, server *httptest.Server, body []byte) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer affinity-test-key")
	req.Header.Set("Content-Type", "application/json")
	return server.Client().Do(req)
}

func TestStatefulAffinityFallbackBoundaries(t *testing.T) {
	stateful := []struct {
		name string
		body string
	}{
		{"forged false claims stateful resource is stateless", `{"model":"gpt-5.4-mini","previous_response_id":"resp-1","session_bound":false,"affinity_known":true}`},
		{"conversation string", `{"model":"gpt-5.4-mini","conversation":"conv-1"}`},
		{"conversation object", `{"model":"gpt-5.4-mini","conversation":{"id":"conv-1"}}`},
	}
	safeFailure := &core.GatewayError{Code: "safe_test_rejection", Category: core.CategoryUnavailable, Retryable: true, RetryDisposition: core.RetrySafe}
	for _, tc := range stateful {
		t.Run("singleton safe failure "+tc.name, func(t *testing.T) {
			a := &affinityFallbackConnector{failure: safeFailure}
			server := affinityFallbackGateway(t, []*affinityFallbackConnector{a}, nil)
			resp, err := affinityPost(t, server, []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			calls, requests := a.snapshot()
			if calls != 1 || resp.StatusCode != http.StatusServiceUnavailable || len(requests) != 1 || string(requests[0].body) != tc.body || requests[0].model != "gpt-5.4-mini" || !requests[0].affinityKnown || !requests[0].sessionBound {
				t.Fatalf("status=%d calls=%d requests=%+v", resp.StatusCode, calls, requests)
			}
		})
		t.Run("multi target fail closed "+tc.name, func(t *testing.T) {
			a, b := &affinityFallbackConnector{}, &affinityFallbackConnector{}
			probe := &affinityProbe{}
			server := affinityFallbackGateway(t, []*affinityFallbackConnector{a, b}, probe)
			resp, err := affinityPost(t, server, []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			ac, _ := a.snapshot()
			bc, _ := b.snapshot()
			if resp.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(`"code":"unsupported_target"`)) || ac != 0 || bc != 0 {
				t.Fatalf("status=%d body=%s calls=%d/%d", resp.StatusCode, body, ac, bc)
			}
			assertAffinityRejectedBeforeCandidates(t, probe)
		})
	}

	unknown := []struct{ name, body string }{
		{"background request", `{"model":"gpt-5.4-mini","background":true}`},
		{"empty previous response ID", `{"model":"gpt-5.4-mini","previous_response_id":""}`},
		{"forged false metadata on unknown background request", `{"model":"gpt-5.4-mini","background":true,"session_bound":false,"affinity_known":true}`},
		{"forged false metadata on empty resource ID", `{"model":"gpt-5.4-mini","previous_response_id":"","session_bound":false,"affinity_known":true}`},
	}
	for _, body := range unknown {
		t.Run("singleton unknown-affinity safe failure "+body.name, func(t *testing.T) {
			a := &affinityFallbackConnector{failure: safeFailure}
			server := affinityFallbackGateway(t, []*affinityFallbackConnector{a}, nil)
			resp, err := affinityPost(t, server, []byte(body.body))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			calls, _ := a.snapshot()
			if resp.StatusCode != http.StatusServiceUnavailable || calls != 1 {
				t.Fatalf("status=%d calls=%d", resp.StatusCode, calls)
			}
		})
		t.Run("multi target unknown affinity "+body.name, func(t *testing.T) {
			a, b := &affinityFallbackConnector{}, &affinityFallbackConnector{}
			probe := &affinityProbe{}
			server := affinityFallbackGateway(t, []*affinityFallbackConnector{a, b}, probe)
			resp, err := affinityPost(t, server, []byte(body.body))
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			ac, _ := a.snapshot()
			bc, _ := b.snapshot()
			if resp.StatusCode != http.StatusBadRequest || !bytes.Contains(data, []byte(`"code":"unsupported_target"`)) || ac != 0 || bc != 0 {
				t.Fatalf("status=%d body=%s calls=%d/%d", resp.StatusCode, data, ac, bc)
			}
			assertAffinityRejectedBeforeCandidates(t, probe)
		})
	}

	for _, store := range []string{"true", "false", "null"} {
		t.Run("stateless fallback ignores forged true flags store="+store, func(t *testing.T) {
			a, b := &affinityFallbackConnector{failure: safeFailure}, &affinityFallbackConnector{}
			server := affinityFallbackGateway(t, []*affinityFallbackConnector{a, b}, nil)
			body := ` { "model" : "gpt-5.4-mini", "previous_response_id": null, "conversation": null, "background": false, "store": ` + store + `, "session_bound": true, "affinity_known": false, "unknown": { "nested" : [1, 2] } } `
			resp, err := affinityPost(t, server, []byte(body))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			ac, ar := a.snapshot()
			bc, br := b.snapshot()
			if resp.StatusCode != http.StatusOK || ac != 1 || bc != 1 || len(ar) != 1 || len(br) != 1 || string(ar[0].body) != body || string(br[0].body) != body || ar[0].model != "gpt-5.4-mini" || br[0].model != "gpt-5.4-mini" || !ar[0].affinityKnown || !br[0].affinityKnown || ar[0].sessionBound || br[0].sessionBound {
				t.Fatalf("status=%d attempts=%d/%d request metadata=%+v/%+v", resp.StatusCode, ac, bc, ar, br)
			}
		})
	}

	t.Run("singleton stateful native bytes", func(t *testing.T) {
		a := &affinityFallbackConnector{}
		server := affinityFallbackGateway(t, []*affinityFallbackConnector{a}, nil)
		body := []byte(" {\n  \"model\" : \"gpt-5.4-mini\", \"conversation\" : { \"id\" : \"conv-1\", \"future\" : {\"x\": [1, 2]} }, \"unknown\" : {\"nested\": {\"keep\": true}}\n} ")
		resp, err := affinityPost(t, server, body)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		calls, requests := a.snapshot()
		if resp.StatusCode != http.StatusOK || calls != 1 || len(requests) != 1 || !bytes.Equal(requests[0].body, body) || requests[0].model != "gpt-5.4-mini" || !requests[0].affinityKnown || !requests[0].sessionBound {
			t.Fatalf("status=%d calls=%d request=%+v", resp.StatusCode, calls, requests)
		}
	})
	t.Run("singleton unknown native bytes and routing metadata", func(t *testing.T) {
		for _, tc := range unknown {
			t.Run(tc.name, func(t *testing.T) {
				a := &affinityFallbackConnector{}
				server := affinityFallbackGateway(t, []*affinityFallbackConnector{a}, nil)
				body := []byte(" {\n  " + tc.body[1:len(tc.body)-1] + `, "unknown" : { "nested" : [1, 2] }` + "\n} ")
				resp, err := affinityPost(t, server, body)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				calls, requests := a.snapshot()
				if resp.StatusCode != http.StatusOK || calls != 1 || len(requests) != 1 || !bytes.Equal(requests[0].body, body) || requests[0].model != "gpt-5.4-mini" || requests[0].affinityKnown || requests[0].sessionBound {
					t.Fatalf("status=%d calls=%d request=%+v", resp.StatusCode, calls, requests)
				}
			})
		}
	})
}
