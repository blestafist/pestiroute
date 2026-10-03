package core

import (
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

type accountingTestConnector struct {
	dispatchConnector
	order      *[]string
	stream     Stream
	executeErr *GatewayError
	cancel     context.CancelFunc
	waitCancel bool
	cancelSeen chan struct{}
	estimates  int
	executions int
}

func (c *accountingTestConnector) EstimateUsage(context.Context, UsageQuery, InvocationServices) (EstimateResult, *GatewayError) {
	c.estimates++
	*c.order = append(*c.order, "estimate")
	return EstimateResult{Supported: true, Known: true, Usage: &UsageReport{InputTokens: new(int64(2)), OutputTokens: new(int64(3)), Source: UsageEstimate, Completeness: UsageComplete}, Method: "fixture"}, nil
}

func (c *accountingTestConnector) Execute(ctx context.Context, in ExecutionRequest, _ AttemptScope, _ InvocationServices) (ExecutionResponse, *GatewayError) {
	c.executions++
	*c.order = append(*c.order, "execute")
	if c.waitCancel {
		<-ctx.Done()
		if c.cancelSeen != nil {
			close(c.cancelSeen)
		}
	}
	if c.cancel != nil {
		c.cancel()
	}
	if c.executeErr != nil {
		return ExecutionResponse{}, c.executeErr
	}
	if c.stream != nil {
		return ExecutionResponse{Stream: c.stream}, nil
	}
	usage := &UsageReport{InputTokens: new(int64(4)), OutputTokens: new(int64(6)), Source: UsageProvider, Completeness: UsageComplete}
	return ExecutionResponse{Stream: &scriptedStream{frames: []StreamFrame{
		head(), {Type: FrameBody, Body: &BodyFrame{Data: append([]byte(nil), in.Payload.Body...)}},
		{Type: FrameComplete, Complete: &CompleteFrame{Outcome: OutcomeSucceeded, Usage: usage}},
	}}}, nil
}

type accountingTestStore struct {
	order            *[]string
	admitted         AccountingAdmission
	terminal         AccountingTerminal
	persisted        bool
	admitErr         error
	beginErr         error
	intentErr        error
	finalizeErr      error
	requestFinishErr error
	cancelAdmit      context.CancelFunc
	finalized        chan AccountingTerminal
	admits           int
	intents          int
	finals           int
	begins           int
	terminals        []AccountingTerminal
	admissions       []AccountingAdmission
}

func (s *accountingTestStore) Admit(_ context.Context, admission AccountingAdmission) error {
	*s.order = append(*s.order, "admit")
	s.admits++
	s.admitted = admission
	s.admissions = append(s.admissions, admission)
	if s.cancelAdmit != nil {
		s.cancelAdmit()
	}
	return s.admitErr
}
func (s *accountingTestStore) BeginAttempt(_ context.Context, admission AccountingAdmission) error {
	*s.order = append(*s.order, "begin_attempt")
	s.begins++
	s.admitted = admission
	s.admissions = append(s.admissions, admission)
	if s.beginErr != nil {
		return s.beginErr
	}
	return s.admitErr
}
func (s *accountingTestStore) RecordDispatchIntent(context.Context, string, time.Time) error {
	*s.order = append(*s.order, "intent")
	s.intents++
	return s.intentErr
}
func (s *accountingTestStore) FinalizeAttempt(_ context.Context, terminal AccountingTerminal) error {
	*s.order = append(*s.order, "finalize")
	s.terminal = terminal
	s.terminals = append(s.terminals, terminal)
	s.finals++
	if s.finalized != nil {
		s.finalized <- terminal
	}
	s.persisted = s.finalizeErr == nil
	return s.finalizeErr
}

func accountingDispatcher(t *testing.T, ctx context.Context, connector *accountingTestConnector, store *accountingTestStore) *Dispatcher {
	t.Helper()
	registry, err := NewRegistry(map[ComponentKind]APIVersion{ComponentAdapter: {Major: 1}, ComponentConnector: {Major: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := CapabilityScope{Protocol: m1Protocol, Mode: ModeNative, Model: "model", AccountID: "account"}
	caps := map[CapabilityScope]CapabilityResult{scope: {Values: map[Capability]CapabilityState{}}}
	adapterDescriptor := validDescriptor(ComponentAdapter)
	adapterDescriptor.ID, adapterDescriptor.Protocols = "adapter", []string{m1Protocol}
	connectorDescriptor := validDescriptor(ComponentConnector)
	connectorDescriptor.ID, connectorDescriptor.Protocols = "connector", []string{m1Protocol}
	adapter := &testAdapter{registryComponent: registryComponent{descriptor: adapterDescriptor}, caps: caps}
	connector.dispatchConnector.registryComponent.descriptor = connectorDescriptor
	connector.dispatchConnector.caps = caps
	for id, component := range map[InstanceID]Component{"adapter": adapter, "connector": connector} {
		if err := registry.Register(id, component, component.Descriptor().Kind); err != nil {
			t.Fatal(err)
		}
		if err := registry.Init(ctx, id, ComponentConfig{}); err != nil {
			t.Fatal(err)
		}
	}
	routes, err := NewRouteTable([]Route{{Identity: RouteIdentity{RouteLookupKey: RouteLookupKey{Protocol: m1Protocol, Mode: ModeNative, Model: "model"}, AccountID: "account"}, Adapter: "adapter", Connector: "connector"}}, registry)
	if err != nil {
		t.Fatal(err)
	}
	return &Dispatcher{
		Routes: routes, Services: &testServices{}, Policies: &authorizationPolicyStore{snapshot: PolicySnapshot{ID: "policy", Revision: 3, Enabled: true, Models: []string{"model"}, Connectors: []string{"connector"}}},
		Accounts: authorizationAccount{}, Accounting: store, Budget: RouteBudget{UnknownEstimate: UnknownEstimateReject}, BudgetPolicy: "known", RouteID: "accounting-test-route", AccountID: "account",
	}
}

func TestDispatchAccountingAdmissionIntentSettlement(t *testing.T) {
	ctx := authContext()
	var order []string
	store := &accountingTestStore{order: &order}
	connector := &accountingTestConnector{order: &order}
	d := accountingDispatcher(t, ctx, connector, store)
	response, gatewayErr := d.Execute(ctx, ExecutionRequest{Model: "model", Payload: RawPayload{Protocol: m1Protocol, Body: []byte(`opaque`)}})
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	var body []byte
	for {
		frame, err := response.Stream.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == FrameHead && (store.admits != 1 || store.intents != 1 || store.finals != 0) {
			t.Fatalf("ledger calls at Head: admissions=%d intents=%d finalizations=%d", store.admits, store.intents, store.finals)
		}
		if frame.Type == FrameBody {
			if store.admits != 1 || store.intents != 1 || store.finals != 0 {
				t.Fatalf("ledger calls during Body delivery: admissions=%d intents=%d finalizations=%d", store.admits, store.intents, store.finals)
			}
			body = append(body, frame.Body.Data...)
		}
		if frame.Type == FrameComplete && store.finals != 1 {
			t.Fatalf("finalizations at Complete = %d, want 1", store.finals)
		}
	}
	if string(body) != "opaque" {
		t.Fatalf("body = %q", body)
	}
	if want := []string{"estimate", "admit", "intent", "execute", "finalize"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("operation order = %v, want %v", order, want)
	}
	if store.admitted.EstimateTokens != 5 || store.admitted.EstimateMethod != "fixture" || store.finals != 1 || store.terminal.Outcome != OutcomeSucceeded || *store.terminal.Usage.InputTokens != 4 {
		t.Fatalf("admission/terminal accounting = %+v / %+v", store.admitted, store.terminal)
	}
}

func TestCandidateAuthorizationPrecedesEstimateAndExecute(t *testing.T) {
	ctx := authContext()
	var order []string
	registry, err := NewRegistry(map[ComponentKind]APIVersion{ComponentAdapter: {Major: 1}, ComponentConnector: {Major: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var routes []Route
	connectors := make([]*accountingTestConnector, 2)
	for i, account := range []string{"account-a", "account-b"} {
		scope := CapabilityScope{Protocol: m1Protocol, Mode: ModeNative, Model: "model", AccountID: account}
		adapterDescriptor := validDescriptor(ComponentAdapter)
		adapterDescriptor.ID, adapterDescriptor.Protocols = "adapter-"+account, []string{m1Protocol}
		connectorDescriptor := validDescriptor(ComponentConnector)
		connectorDescriptor.ID, connectorDescriptor.Protocols = "connector-"+account, []string{m1Protocol}
		caps := map[CapabilityScope]CapabilityResult{scope: {Values: map[Capability]CapabilityState{}}}
		adapter := &testAdapter{registryComponent: registryComponent{descriptor: adapterDescriptor}, caps: caps}
		connectors[i] = &accountingTestConnector{order: &order}
		connectors[i].dispatchConnector.registryComponent.descriptor = connectorDescriptor
		connectors[i].dispatchConnector.caps = caps
		for id, component := range map[InstanceID]Component{InstanceID(adapterDescriptor.ID): adapter, InstanceID(connectorDescriptor.ID): connectors[i]} {
			if err := registry.Register(id, component, component.Descriptor().Kind); err != nil {
				t.Fatal(err)
			}
			if err := registry.Init(ctx, id, ComponentConfig{}); err != nil {
				t.Fatal(err)
			}
		}
		routes = append(routes, Route{
			Identity: RouteIdentity{RouteLookupKey: RouteLookupKey{Protocol: m1Protocol, Mode: ModeNative, Model: "model"}, AccountID: account},
			Adapter:  InstanceID(adapterDescriptor.ID), Connector: InstanceID(connectorDescriptor.ID), CandidateGroup: "route",
			MaxBodyBytes: 1024, MaxHeaderBytes: 1024,
		})
	}
	table, err := NewRouteTable(routes, registry)
	if err != nil {
		t.Fatal(err)
	}
	store := &accountingTestStore{order: &order}
	d := &Dispatcher{
		Routes: table, Services: &testServices{}, Policies: &authorizationPolicyStore{snapshot: PolicySnapshot{
			ID: "policy", Revision: 3, Enabled: true, Models: []string{"model"}, Connectors: []string{"connector-account-b"},
		}}, Accounts: authorizationAccount{}, Accounting: store, Budget: RouteBudget{UnknownEstimate: UnknownEstimateReject},
		BudgetPolicy: "known", RouteID: "candidate-test-route", AccountID: "account-a",
	}
	in := ExecutionRequest{Model: "model", Payload: RawPayload{Protocol: m1Protocol, Body: []byte("opaque")}, Metadata: RequestMetadata{AffinityKnown: true, IngressHeaderBytes: 1}}
	for _, metadata := range []RequestMetadata{
		{AffinityKnown: false, IngressHeaderBytes: 1},
		{AffinityKnown: true, SessionBound: true, IngressHeaderBytes: 1},
	} {
		in.Metadata = metadata
		if _, gatewayErr := d.Execute(ctx, in); gatewayErr == nil || gatewayErr.Code != "unsupported_target" {
			t.Fatalf("affinity gate error = %#v", gatewayErr)
		}
	}
	in.Metadata = RequestMetadata{AffinityKnown: true}
	if candidates, err := d.AuthorizedCandidates(ctx, in); !errors.Is(err, ErrCandidateLimits) || len(candidates) != 0 {
		t.Fatalf("missing ingress-header measurement candidates=%+v error=%v", candidates, err)
	}
	if _, gatewayErr := d.Execute(ctx, in); gatewayErr == nil || gatewayErr.Code != "invalid_request" {
		t.Fatalf("missing ingress-header measurement dispatch error=%#v", gatewayErr)
	}
	in.Metadata = RequestMetadata{AffinityKnown: true, IngressHeaderBytes: 1}
	eligible, err := d.AuthorizedCandidates(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(eligible) != 1 || eligible[0].Route.Identity.AccountID != "account-b" {
		t.Fatalf("authorized candidates = %+v", eligible)
	}
	if len(order) != 0 || store.admits != 0 || store.intents != 0 || store.finals != 0 || connectors[0].estimates != 0 || connectors[0].executions != 0 || connectors[1].estimates != 0 || connectors[1].executions != 0 {
		t.Fatalf("candidate selection must not estimate or execute: order=%v targets=%+v / %+v", order, connectors[0], connectors[1])
	}
}

func TestExecuteFallbackUsesDurablePerAttemptAccounting(t *testing.T) {
	ctx := authContext()
	var order []string
	registry, err := NewRegistry(map[ComponentKind]APIVersion{ComponentAdapter: {Major: 1}, ComponentConnector: {Major: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	routes := make([]Route, 0, 2)
	connectors := make([]*accountingTestConnector, 2)
	for i, account := range []string{"account-a", "account-b"} {
		scope := CapabilityScope{Protocol: m1Protocol, Mode: ModeNative, Model: "model", AccountID: account}
		caps := map[CapabilityScope]CapabilityResult{scope: {Values: map[Capability]CapabilityState{}}}
		adapterDescriptor := validDescriptor(ComponentAdapter)
		adapterDescriptor.ID, adapterDescriptor.Protocols = "adapter-"+account, []string{m1Protocol}
		connectorDescriptor := validDescriptor(ComponentConnector)
		connectorDescriptor.ID, connectorDescriptor.Protocols = "connector-"+account, []string{m1Protocol}
		adapter := &testAdapter{registryComponent: registryComponent{descriptor: adapterDescriptor}, caps: caps}
		connectors[i] = &accountingTestConnector{order: &order}
		connectors[i].dispatchConnector.registryComponent.descriptor = connectorDescriptor
		connectors[i].dispatchConnector.caps = caps
		if i == 0 {
			connectors[i].executeErr = &GatewayError{Code: "upstream_unavailable", Category: CategoryUnavailable, Retryable: true, RetryDisposition: RetrySafe}
		}
		for id, component := range map[InstanceID]Component{InstanceID(adapterDescriptor.ID): adapter, InstanceID(connectorDescriptor.ID): connectors[i]} {
			if err := registry.Register(id, component, component.Descriptor().Kind); err != nil {
				t.Fatal(err)
			}
			if err := registry.Init(ctx, id, ComponentConfig{}); err != nil {
				t.Fatal(err)
			}
		}
		routes = append(routes, Route{
			Identity: RouteIdentity{RouteLookupKey: RouteLookupKey{Protocol: m1Protocol, Mode: ModeNative, Model: "model"}, AccountID: account},
			Adapter:  InstanceID(adapterDescriptor.ID), Connector: InstanceID(connectorDescriptor.ID), CandidateGroup: "fallback",
			MaxBodyBytes: 1024, MaxHeaderBytes: 1024,
		})
	}
	table, err := NewRouteTable(routes, registry)
	if err != nil {
		t.Fatal(err)
	}
	store := &accountingTestStore{order: &order}
	d := &Dispatcher{
		Routes: table, Services: &testServices{}, Policies: &authorizationPolicyStore{snapshot: PolicySnapshot{ID: "policy", Revision: 3, Enabled: true, Models: []string{"model"}, Connectors: []string{"connector-account-a", "connector-account-b"}}},
		Accounts: authorizationAccount{}, Accounting: store, Budget: RouteBudget{UnknownEstimate: UnknownEstimateReject}, BudgetPolicy: "known", RouteID: "fallback-route", AccountID: "account-a",
		RetryMaxAttempts: 2, RetryDeadline: time.Second,
	}
	in := ExecutionRequest{Model: "model", Payload: RawPayload{Protocol: m1Protocol, Body: []byte("opaque")}, Metadata: RequestMetadata{AffinityKnown: true, IngressHeaderBytes: 1}}
	policyStore := d.Policies.(*authorizationPolicyStore)
	policyStore.snapshot.Connectors = []string{"connector-account-b"}
	for _, attemptLimit := range []int{1, 2} {
		d.RetryMaxAttempts = attemptLimit
		d.RetryDeadline = 0
		if attemptLimit > 1 {
			d.RetryDeadline = time.Second
		}
		selected, err := d.Execute(ctx, in)
		if err != nil {
			t.Fatalf("eligible target after excluded primary (limit %d): %v", attemptLimit, err)
		}
		for {
			_, err := selected.Stream.Next(ctx)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if connectors[0].executions != 0 || connectors[1].executions != attemptLimit || store.begins != 0 || store.admitted.AccountID != "account-b" {
			t.Fatalf("ineligible primary was not skipped: executions=%d/%d begins=%d selected=%s", connectors[0].executions, connectors[1].executions, store.begins, store.admitted.AccountID)
		}
	}
	policyStore.snapshot.Connectors = []string{"connector-account-a", "connector-account-b"}
	d.RetryMaxAttempts, d.RetryDeadline = 2, time.Second
	connectors[1].executions = 0
	store = &accountingTestStore{order: &order}
	d.Accounting = store
	response, gatewayErr := d.Execute(ctx, in)
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	for {
		_, err := response.Stream.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if connectors[0].executions != 1 || connectors[1].executions != 1 || store.admits != 1 || store.begins != 1 || store.intents != 2 || store.finals != 2 {
		t.Fatalf("fallback lifecycle: execute=%d/%d admit=%d begin=%d intent=%d finalize=%d order=%v", connectors[0].executions, connectors[1].executions, store.admits, store.begins, store.intents, store.finals, order)
	}
	first, second := store.terminals[0], store.terminals[1]
	if first.AttemptID == second.AttemptID || first.Outcome != OutcomeFailed || first.Committed || second.Outcome != OutcomeSucceeded || !second.Committed || store.admitted.RequestID == "" || store.admitted.AccountID != "account-b" ||
		len(store.admissions) != 2 || store.admissions[0].RequestID != store.admissions[1].RequestID || store.admissions[0].AttemptID == store.admissions[1].AttemptID {
		t.Fatalf("fallback identities/settlements: first=%+v second=%+v admission=%+v", first, second, store.admitted)
	}
	connectors[0].executeErr = nil
	rejectedHead := &GatewayError{Code: "rejected_head", Category: CategoryUnavailable, Retryable: true, RetryDisposition: RetrySafe}
	connectors[0].stream = &scriptedStream{frames: []StreamFrame{
		{Type: FrameHead, Head: &HeadFrame{Protocol: m1Protocol, Error: rejectedHead}},
		{Type: FrameComplete, Complete: &CompleteFrame{Outcome: OutcomeFailed, Error: rejectedHead}},
	}}
	response, gatewayErr = d.Execute(ctx, in)
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	for {
		_, err := response.Stream.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if connectors[1].executions != 2 {
		t.Fatalf("safe rejected Head did not fallback: executions=%d", connectors[1].executions)
	}
	connectors[0].stream = nil
	store.beginErr = ErrAdmissionLimit
	connectors[0].executeErr = &GatewayError{Code: "upstream_unavailable", Category: CategoryUnavailable, Retryable: true, RetryDisposition: RetrySafe}
	_, gatewayErr = d.Execute(ctx, in)
	if gatewayErr == nil || gatewayErr.Code != "rate_limit_exceeded" || connectors[1].executions != 2 {
		t.Fatalf("fallback TPM rejection did not stop before Execute: err=%#v fallback executions=%d", gatewayErr, connectors[1].executions)
	}
	store.beginErr = nil
	cancelCtx, cancel := context.WithCancel(ctx)
	connectors[0].executeErr = &GatewayError{Code: "upstream_unavailable", Category: CategoryUnavailable, Retryable: true, RetryDisposition: RetrySafe}
	connectors[0].cancel = cancel
	_, gatewayErr = d.Execute(cancelCtx, in)
	connectors[0].cancel = nil
	if gatewayErr == nil || gatewayErr.Category != CategoryCancelled || connectors[1].executions != 2 {
		t.Fatalf("cancelled attempt replayed fallback: err=%#v fallback executions=%d", gatewayErr, connectors[1].executions)
	}
	d.RetryDeadline = 100 * time.Millisecond
	connectors[0].cancelSeen = make(chan struct{})
	connectors[0].waitCancel = true
	connectors[0].executeErr = &GatewayError{Code: "late_safe_error", Category: CategoryUnavailable, Retryable: true, RetryDisposition: RetrySafe}
	_, gatewayErr = d.Execute(ctx, in)
	select {
	case <-connectors[0].cancelSeen:
	default:
		t.Fatal("pre-Head retry deadline did not cancel the active attempt")
	}
	connectors[0].waitCancel = false
	if gatewayErr == nil || gatewayErr.Category != CategoryTimeout || connectors[1].executions != 2 {
		t.Fatalf("pre-Head deadline did not stop fallback: err=%#v fallback executions=%d", gatewayErr, connectors[1].executions)
	}
	connectors[0].executeErr = &GatewayError{Code: "upstream_unavailable", Category: CategoryUnavailable, Retryable: true, RetryDisposition: RetrySafe}
	connectors[1].stream = &scriptedStream{frames: []StreamFrame{head(), {Type: FrameBody, Body: &BodyFrame{Data: []byte("after-deadline")}}, {Type: FrameComplete, Complete: &CompleteFrame{Outcome: OutcomeSucceeded, Usage: &UsageReport{Source: UsageProvider, Completeness: UsageComplete}}}}}
	response, gatewayErr = d.Execute(ctx, in)
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	frame, streamErr := response.Stream.Next(ctx)
	if streamErr != nil || frame.Type != FrameHead {
		t.Fatalf("prefetched Head = %+v, %v", frame, streamErr)
	}
	time.Sleep(2 * d.RetryDeadline)
	if _, streamErr = response.Stream.Next(ctx); !errors.Is(streamErr, context.DeadlineExceeded) {
		t.Fatalf("configured request deadline did not terminate committed stream: %v", streamErr)
	}
	parentCtx, parentCancel := context.WithCancel(ctx)
	connectors[1].stream = &scriptedStream{frames: []StreamFrame{head(), {Type: FrameBody, Body: &BodyFrame{Data: []byte("cancel me")}}, {Type: FrameComplete, Complete: &CompleteFrame{Outcome: OutcomeSucceeded, Usage: &UsageReport{Source: UsageProvider, Completeness: UsageComplete}}}}}
	response, gatewayErr = d.Execute(parentCtx, in)
	if gatewayErr != nil {
		parentCancel()
		t.Fatal(gatewayErr)
	}
	parentCancel()
	if _, streamErr = response.Stream.Next(parentCtx); !errors.Is(streamErr, context.Canceled) {
		t.Fatalf("parent cancellation did not reach committed stream: %v", streamErr)
	}
	d.RetryMaxAttempts = 1
	connectors[0].executeErr = nil
	connectors[0].stream = &scriptedStream{frames: []StreamFrame{head(), {Type: FrameBody, Body: &BodyFrame{Data: []byte("single attempt")}}, {Type: FrameComplete, Complete: &CompleteFrame{Outcome: OutcomeSucceeded, Usage: &UsageReport{Source: UsageProvider, Completeness: UsageComplete}}}}}
	response, gatewayErr = d.Execute(ctx, in)
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	if frame, streamErr = response.Stream.Next(ctx); streamErr != nil || frame.Type != FrameHead {
		t.Fatalf("single-attempt Head = %+v, %v", frame, streamErr)
	}
	time.Sleep(2 * d.RetryDeadline)
	if _, streamErr = response.Stream.Next(ctx); !errors.Is(streamErr, context.DeadlineExceeded) || connectors[1].executions != 4 {
		t.Fatalf("max_attempts=1 deadline/retry bound: err=%v alternate executions=%d", streamErr, connectors[1].executions)
	}
	d.RetryMaxAttempts = 2
	d.RetryDeadline = time.Second
	connectors[1].stream = nil
	connectors[0].stream = nil
	committedFailure := &GatewayError{Code: "late_failure", Category: CategoryUnavailable, Retryable: true, RetryDisposition: RetrySafe}
	connectors[0].executeErr = nil
	connectors[0].stream = &scriptedStream{frames: []StreamFrame{
		head(), {Type: FrameComplete, Complete: &CompleteFrame{Outcome: OutcomeFailed, Error: committedFailure}},
	}}
	response, gatewayErr = d.Execute(ctx, in)
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	for {
		_, err := response.Stream.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if connectors[1].executions != 4 {
		t.Fatalf("committed failure replayed fallback: executions=%d", connectors[1].executions)
	}
	connectors[0].stream = nil
	connectors[0].executeErr = &GatewayError{Code: "safe_prehead_failure", Category: CategoryUnavailable, Retryable: true, RetryDisposition: RetrySafe}
	store.finalizeErr = AccountingStorageFailure{Err: errors.New("ledger unavailable")}
	_, gatewayErr = d.Execute(ctx, in)
	if gatewayErr == nil || gatewayErr.Code != "accounting_unavailable" || connectors[1].executions != 4 {
		t.Fatalf("pre-Head settlement failure replayed fallback: err=%#v fallback executions=%d", gatewayErr, connectors[1].executions)
	}
	primaryCalls := connectors[0].executions
	_, gatewayErr = d.Execute(ctx, in)
	if gatewayErr == nil || gatewayErr.Code != "accounting_unavailable" || connectors[0].executions != primaryCalls || connectors[1].executions != 4 {
		t.Fatalf("post-commit settlement failure did not latch fail-closed: err=%#v executions=%d/%d", gatewayErr, connectors[0].executions, connectors[1].executions)
	}
}

func TestRejectedHeadFinalizesUncommittedBeforeRetry(t *testing.T) {
	rejection := &GatewayError{Code: "temporary_rejection", Category: CategoryUnavailable, Retryable: true, RetryDisposition: RetrySafe}
	var finalized AttemptResult
	finalizations := 0
	stream := &attemptStream{
		source: NewCheckedStream(&scriptedStream{frames: []StreamFrame{
			{Type: FrameHead, Head: &HeadFrame{Protocol: m1Protocol, Error: rejection}},
			{Type: FrameComplete, Complete: &CompleteFrame{Outcome: OutcomeFailed, Error: rejection}},
		}}, m1Protocol),
		ctx:                    context.Background(),
		result:                 AttemptResult{RequestID: "request", Scope: AttemptScope{ID: "attempt", AccountID: "account", Mode: ModeNative}, Outcome: OutcomeIncomplete},
		finish:                 func(result AttemptResult) { finalized = result; finalizations++ },
		allowRejectedHeadRetry: true,
	}
	_, err := stream.Next(context.Background())
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr != rejection || finalizations != 1 || finalized.Committed || finalized.Outcome != OutcomeFailed {
		t.Fatalf("rejected Head lifecycle: err=%v finalizations=%d result=%+v", err, finalizations, finalized)
	}
	_ = stream.Close()
	if finalizations != 1 {
		t.Fatalf("rejected attempt finalized %d times after Close", finalizations)
	}
}

func TestDispatchAccountingRejectsBeforeExecute(t *testing.T) {
	t.Run("limit rejection", func(t *testing.T) {
		var order []string
		store := &accountingTestStore{order: &order, admitErr: ErrAdmissionLimit}
		connector := &accountingTestConnector{order: &order}
		d := accountingDispatcher(t, authContext(), connector, store)
		_, gatewayErr := d.Execute(authContext(), ExecutionRequest{Model: "model", Payload: RawPayload{Protocol: m1Protocol}})
		if gatewayErr == nil || gatewayErr.Category != CategoryRateLimited || gatewayErr.Code != "rate_limit_exceeded" {
			t.Fatalf("admission rejection = %#v, want rate_limited", gatewayErr)
		}
		if store.admits != 1 || store.intents != 0 || store.finals != 0 || slices.Contains(order, "execute") {
			t.Fatalf("admission rejection side effects: order=%v store=%+v", order, store)
		}
	})
	t.Run("dispatch intent failure", func(t *testing.T) {
		var order []string
		store := &accountingTestStore{order: &order, intentErr: errors.New("intent write unavailable")}
		connector := &accountingTestConnector{order: &order}
		d := accountingDispatcher(t, authContext(), connector, store)
		_, gatewayErr := d.Execute(authContext(), ExecutionRequest{Model: "model", Payload: RawPayload{Protocol: m1Protocol}})
		if gatewayErr == nil || gatewayErr.Category != CategoryUnavailable || gatewayErr.Code != "accounting_unavailable" {
			t.Fatalf("intent failure = %#v, want accounting_unavailable", gatewayErr)
		}
		if store.intents != 1 || store.finals != 1 || slices.Contains(order, "execute") || store.terminal.Committed {
			t.Fatalf("intent failure side effects: order=%v store=%+v", order, store)
		}
	})
	t.Run("cancelled before intent", func(t *testing.T) {
		ctx, cancel := context.WithCancel(authContext())
		defer cancel()
		var order []string
		store := &accountingTestStore{order: &order, cancelAdmit: cancel}
		connector := &accountingTestConnector{order: &order}
		d := accountingDispatcher(t, ctx, connector, store)
		_, gatewayErr := d.Execute(ctx, ExecutionRequest{Model: "model", Payload: RawPayload{Protocol: m1Protocol}})
		if gatewayErr == nil || gatewayErr.Category != CategoryCancelled {
			t.Fatalf("pre-intent cancellation = %#v, want cancelled", gatewayErr)
		}
		if store.admits != 1 || store.intents != 0 || store.finals != 1 || store.terminal.Outcome != OutcomeCancelled || slices.Contains(order, "execute") {
			t.Fatalf("pre-intent cancellation side effects: order=%v store=%+v", order, store)
		}
	})
}

func TestDispatchAccountingStorageFailureBeforeExecute(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func(*accountingTestStore)
	}{
		{name: "admission", store: func(s *accountingTestStore) {
			s.admitErr = AccountingStorageFailure{Err: errors.New("private database detail")}
		}},
		{name: "intent", store: func(s *accountingTestStore) {
			s.intentErr = AccountingStorageFailure{Err: errors.New("private database detail")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var order []string
			store := &accountingTestStore{order: &order}
			tc.store(store)
			connector := &accountingTestConnector{order: &order}
			d := accountingDispatcher(t, authContext(), connector, store)
			_, gatewayErr := d.Execute(authContext(), ExecutionRequest{Model: "model", Payload: RawPayload{Protocol: m1Protocol}})
			if gatewayErr == nil || gatewayErr.Code != "accounting_unavailable" || gatewayErr.Category != CategoryUnavailable || gatewayErr.Retryable || gatewayErr.Message != "Request accounting is unavailable" {
				t.Fatalf("storage error = %#v, want sanitized accounting_unavailable", gatewayErr)
			}
			if slices.Contains(order, "execute") {
				t.Fatalf("storage failure executed upstream: %v", order)
			}
			before := store.admits
			_, nextErr := d.Execute(authContext(), ExecutionRequest{Model: "model", Payload: RawPayload{Protocol: m1Protocol}})
			if nextErr == nil || nextErr.Code != "accounting_unavailable" || store.admits != before || slices.Contains(order, "execute") {
				t.Fatalf("degraded dispatcher admitted later request: err=%#v order=%v store=%+v", nextErr, order, store)
			}
		})
	}
}

func TestDispatchAccountingStorageFailureAfterCommitFailClosed(t *testing.T) {
	var order []string
	store := &accountingTestStore{order: &order, finalizeErr: AccountingStorageFailure{Err: errors.New("private database detail")}}
	connector := &accountingTestConnector{order: &order}
	d := accountingDispatcher(t, authContext(), connector, store)
	response, gatewayErr := d.Execute(authContext(), ExecutionRequest{Model: "model", Payload: RawPayload{Protocol: m1Protocol, Body: []byte("opaque")}})
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	frame, err := response.Stream.Next(authContext())
	if err != nil || frame.Type != FrameHead {
		t.Fatalf("Head = %#v, %v", frame, err)
	}
	frame, err = response.Stream.Next(authContext())
	if err != nil || frame.Type != FrameBody || string(frame.Body.Data) != "opaque" {
		t.Fatalf("Body = %#v, %v", frame, err)
	}
	frame, err = response.Stream.Next(authContext())
	if err == nil || frame.Type != "" || err.Error() != "Request accounting is unavailable" || store.finals != 1 || store.persisted || !store.terminal.Committed {
		t.Fatalf("failed terminal persistence = frame %#v, err %v, terminal %+v", frame, err, store.terminal)
	}
	before := store.admits
	_, nextErr := d.Execute(authContext(), ExecutionRequest{Model: "model", Payload: RawPayload{Protocol: m1Protocol}})
	executeCalls := 0
	for _, operation := range order {
		if operation == "execute" {
			executeCalls++
		}
	}
	if nextErr == nil || nextErr.Code != "accounting_unavailable" || store.admits != before || executeCalls != 1 {
		t.Fatalf("terminal failure did not fail closed: err=%#v order=%v store=%+v", nextErr, order, store)
	}
}

func TestDispatchAccountingNonStorageFailuresDoNotDegrade(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*accountingTestStore)
	}{
		{name: "admission conflict", set: func(s *accountingTestStore) { s.admitErr = errors.New("ledger conflict") }},
		{name: "intent not found", set: func(s *accountingTestStore) { s.intentErr = errors.New("ledger record not found") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var order []string
			store := &accountingTestStore{order: &order}
			tc.set(store)
			connector := &accountingTestConnector{order: &order}
			d := accountingDispatcher(t, authContext(), connector, store)
			_, gatewayErr := d.Execute(authContext(), ExecutionRequest{Model: "model", Payload: RawPayload{Protocol: m1Protocol}})
			if gatewayErr == nil || gatewayErr.Code != "accounting_unavailable" || slices.Contains(order, "execute") {
				t.Fatalf("non-storage failure = %#v, order=%v", gatewayErr, order)
			}
			admissions := store.admits
			_, _ = d.Execute(authContext(), ExecutionRequest{Model: "model", Payload: RawPayload{Protocol: m1Protocol}})
			if store.admits != admissions+1 {
				t.Fatalf("non-storage failure degraded dispatcher: admissions %d -> %d", admissions, store.admits)
			}
		})
	}
}

func TestDispatchAccountingCancelledAdmissionDoesNotDegrade(t *testing.T) {
	ctx, cancel := context.WithCancel(authContext())
	var order []string
	store := &accountingTestStore{order: &order, admitErr: context.Canceled, cancelAdmit: cancel}
	connector := &accountingTestConnector{order: &order}
	d := accountingDispatcher(t, ctx, connector, store)
	_, gatewayErr := d.Execute(ctx, ExecutionRequest{Model: "model", Payload: RawPayload{Protocol: m1Protocol}})
	if gatewayErr == nil || gatewayErr.Category != CategoryCancelled || slices.Contains(order, "execute") {
		t.Fatalf("cancelled admission = %#v, order=%v", gatewayErr, order)
	}
	_, _ = d.Execute(authContext(), ExecutionRequest{Model: "model", Payload: RawPayload{Protocol: m1Protocol}})
	if store.admits != 2 {
		t.Fatalf("cancelled admission degraded dispatcher: admissions=%d", store.admits)
	}
}

func TestDispatchAccountingTrailingFrameFinalizesOnce(t *testing.T) {
	var order []string
	store := &accountingTestStore{order: &order}
	connector := &accountingTestConnector{order: &order, stream: &scriptedStream{frames: []StreamFrame{
		head(), {Type: FrameComplete, Complete: &CompleteFrame{Outcome: OutcomeSucceeded}}, body(),
	}}}
	d := accountingDispatcher(t, authContext(), connector, store)
	response, gatewayErr := d.Execute(authContext(), ExecutionRequest{Model: "model", Payload: RawPayload{Protocol: m1Protocol}})
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	if _, err := response.Stream.Next(authContext()); err != nil {
		t.Fatal(err)
	}
	if _, err := response.Stream.Next(authContext()); !errors.Is(err, ErrStreamContract) {
		t.Fatalf("trailing frame error = %v", err)
	}
	if store.finals != 1 || store.terminal.Outcome != OutcomeFailed {
		t.Fatalf("trailing-frame settlement = %+v, finals=%d", store.terminal, store.finals)
	}
	_ = response.Stream.Close()
	if store.finals != 1 {
		t.Fatalf("trailing-frame finalizations after Close = %d", store.finals)
	}
}

type accountingBlockedStream struct {
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
	step    int
}

func (s *accountingBlockedStream) Next(ctx context.Context) (StreamFrame, error) {
	s.step++
	if s.step == 1 {
		return head(), nil
	}
	close(s.entered)
	select {
	case <-ctx.Done():
		return StreamFrame{}, ctx.Err()
	case <-s.closed:
		return StreamFrame{}, context.Canceled
	}
}
func (s *accountingBlockedStream) Close() error { s.once.Do(func() { close(s.closed) }); return nil }

func TestDispatchAccountingCancellationFinalizesOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(authContext())
	defer cancel()
	var order []string
	store := &accountingTestStore{order: &order, finalized: make(chan AccountingTerminal, 2)}
	blocked := &accountingBlockedStream{entered: make(chan struct{}), closed: make(chan struct{})}
	connector := &accountingTestConnector{order: &order, stream: blocked}
	d := accountingDispatcher(t, ctx, connector, store)
	response, gatewayErr := d.Execute(ctx, ExecutionRequest{Model: "model", Payload: RawPayload{Protocol: m1Protocol}})
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	if _, err := response.Stream.Next(ctx); err != nil {
		t.Fatal(err)
	}
	nextErr := make(chan error, 1)
	go func() { _, err := response.Stream.Next(ctx); nextErr <- err }()
	<-blocked.entered
	cancel()
	if err := <-nextErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Body read = %v", err)
	}
	select {
	case terminal := <-store.finalized:
		if terminal.Outcome != OutcomeCancelled {
			t.Fatalf("terminal outcome = %q", terminal.Outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatcher cancellation did not finalize")
	}
	_ = response.Stream.Close()
	if store.finals != 1 || store.intents != 1 {
		t.Fatalf("cancellation settlement counts: intents=%d finalizations=%d", store.intents, store.finals)
	}
}

func TestDispatchAccountingStreamCancellationAndTrailingFrameOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames []StreamFrame
		close  bool
		want   Outcome
	}{
		{name: "cancellation", frames: []StreamFrame{head()}, close: true, want: OutcomeCancelled},
		{name: "trailing frame", frames: []StreamFrame{head(), {Type: FrameComplete, Complete: &CompleteFrame{Outcome: OutcomeSucceeded}}, body()}, want: OutcomeFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finals := 0
			s := &attemptStream{
				source: &scriptedStream{frames: tc.frames}, ctx: context.Background(),
				result: AttemptResult{RequestID: "request", Scope: AttemptScope{ID: "attempt"}, Outcome: OutcomeIncomplete},
				persist: func(result AttemptResult) error {
					finals++
					if result.Outcome != tc.want {
						t.Errorf("settled outcome = %q, want %q", result.Outcome, tc.want)
					}
					return nil
				},
			}
			if tc.close {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := s.Next(context.Background()); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Next(context.Background()); err == nil {
					t.Fatal("trailing frame accepted")
				}
			}
			if finals != 1 {
				t.Fatalf("terminal settlement count = %d, want 1", finals)
			}
			_ = s.Close()
			if finals != 1 {
				t.Fatalf("repeated close settled %d times", finals)
			}
		})
	}
}

func (s *accountingTestStore) FinishRequest(ctx context.Context, id string) error {
	return s.requestFinishErr
}

func TestDispatchAccountingRequestFinishFailureSuppressesComplete(t *testing.T) {
	var order []string
	store := &accountingTestStore{order: &order, requestFinishErr: AccountingStorageFailure{Err: errors.New("private finish detail")}}
	connector := &accountingTestConnector{order: &order}
	d := accountingDispatcher(t, authContext(), connector, store)
	var healthFailed bool
	d.OnAccountingFailure = func() { healthFailed = true }
	response, gatewayErr := d.Execute(authContext(), ExecutionRequest{Model: "model", Payload: RawPayload{Protocol: m1Protocol, Body: []byte("opaque")}})
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	for _, want := range []FrameType{FrameHead, FrameBody} {
		frame, err := response.Stream.Next(authContext())
		if err != nil || frame.Type != want {
			t.Fatalf("frame %+v err=%v", frame, err)
		}
	}
	frame, err := response.Stream.Next(authContext())
	if err == nil || frame.Type != "" || !healthFailed || store.finals != 1 {
		t.Fatalf("request finish failure exposed Complete: %+v err=%v health=%t finals=%d", frame, err, healthFailed, store.finals)
	}
	_, gatewayErr = d.Execute(authContext(), ExecutionRequest{Model: "model", Payload: RawPayload{Protocol: m1Protocol}})
	if gatewayErr == nil || gatewayErr.Code != "accounting_unavailable" || store.admits != 1 {
		t.Fatalf("request finish failure admitted new work: %+v admits=%d", gatewayErr, store.admits)
	}
}
