package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

const accountingRaceProtocol = "openai.responses.v1"

type accountingRaceAdapter struct{ descriptor core.Descriptor }

func (a *accountingRaceAdapter) Descriptor() core.Descriptor                    { return a.descriptor.Clone() }
func (*accountingRaceAdapter) Init(context.Context, core.ComponentConfig) error { return nil }
func (*accountingRaceAdapter) Health(context.Context) core.Health {
	return core.Health{State: core.HealthReady}
}
func (*accountingRaceAdapter) Capabilities(context.Context, core.CapabilityScope) core.CapabilityResult {
	return core.CapabilityResult{Values: map[core.Capability]core.CapabilityState{}}
}
func (*accountingRaceAdapter) Close(context.Context) error { return nil }
func (a *accountingRaceAdapter) Protocol() string          { return accountingRaceProtocol }
func (*accountingRaceAdapter) Decode(context.Context, core.ClientRequest) (core.ExecutionRequest, *core.GatewayError) {
	return core.ExecutionRequest{}, nil
}
func (*accountingRaceAdapter) Encode(context.Context, core.ClientResponse, *core.GatewayError, core.ExecutionResponse) error {
	return nil
}

type accountingRaceConnector struct {
	descriptor  core.Descriptor
	stream      *accountingRaceStream
	rejectFirst bool
	calls       atomic.Int32
}

func (c *accountingRaceConnector) Descriptor() core.Descriptor                    { return c.descriptor.Clone() }
func (*accountingRaceConnector) Init(context.Context, core.ComponentConfig) error { return nil }
func (*accountingRaceConnector) Health(context.Context) core.Health {
	return core.Health{State: core.HealthReady}
}
func (*accountingRaceConnector) Capabilities(context.Context, core.CapabilityScope) core.CapabilityResult {
	return core.CapabilityResult{Values: map[core.Capability]core.CapabilityState{}}
}
func (*accountingRaceConnector) Close(context.Context) error { return nil }
func (c *accountingRaceConnector) Execute(context.Context, core.ExecutionRequest, core.AttemptScope, core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	if c.calls.Add(1) == 1 && c.rejectFirst {
		return core.ExecutionResponse{}, &core.GatewayError{Code: "safe_fixture_rejection", Category: core.CategoryUnavailable, Retryable: true, RetryDisposition: core.RetrySafe}
	}
	return core.ExecutionResponse{Stream: c.stream}, nil
}
func (*accountingRaceConnector) Models(context.Context, core.ModelQuery, core.InvocationServices) (core.ModelsResult, *core.GatewayError) {
	return core.ModelsResult{}, nil
}
func (*accountingRaceConnector) EstimateUsage(context.Context, core.UsageQuery, core.InvocationServices) (core.EstimateResult, *core.GatewayError) {
	return core.EstimateResult{Supported: true, Known: true, Usage: &core.UsageReport{InputTokens: new(int64(2)), OutputTokens: new(int64(3)), Source: core.UsageEstimate, Completeness: core.UsageComplete}, Method: "race-fixture"}, nil
}
func (*accountingRaceConnector) Authenticate(context.Context, core.AuthRequest, core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	return core.AuthResult{}, nil
}

type accountingRaceEvent struct {
	frame core.StreamFrame
	err   error
}

type accountingRaceStream struct {
	mu        sync.Mutex
	first     bool
	events    chan accountingRaceEvent
	closed    chan struct{}
	closeOne  sync.Once
	pulls     atomic.Int32
	active    atomic.Int32
	maxActive atomic.Int32
}

func newAccountingRaceStream() *accountingRaceStream {
	return &accountingRaceStream{events: make(chan accountingRaceEvent), closed: make(chan struct{})}
}

func (s *accountingRaceStream) Next(ctx context.Context) (core.StreamFrame, error) {
	s.pulls.Add(1)
	active := s.active.Add(1)
	for old := s.maxActive.Load(); active > old && !s.maxActive.CompareAndSwap(old, active); old = s.maxActive.Load() {
	}
	defer s.active.Add(-1)
	s.mu.Lock()
	if !s.first {
		s.first = true
		s.mu.Unlock()
		return core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: accountingRaceProtocol}}, nil
	}
	s.mu.Unlock()
	select {
	case event := <-s.events:
		return event.frame, event.err
	case <-s.closed:
		return core.StreamFrame{}, io.ErrClosedPipe
	case <-ctx.Done():
		return core.StreamFrame{}, ctx.Err()
	}
}

func (s *accountingRaceStream) Close() error {
	s.closeOne.Do(func() { close(s.closed) })
	return nil
}

func TestFallbackSQLiteDispatcherStreamFinalizationRaces(t *testing.T) {
	for _, terminalError := range []bool{false, true} {
		name := "complete"
		if terminalError {
			name = "producer-error"
		}
		t.Run(name, func(t *testing.T) {
			for iteration := range 8 {
				t.Run(string(rune('a'+iteration)), func(t *testing.T) {
					fallbackSQLiteStreamRace(t, terminalError, false)
				})
			}
		})
	}
}

func TestFallbackSQLiteDispatcherAccountingBudgetFailure(t *testing.T) {
	fallbackSQLiteStreamRace(t, false, true)
}

func fallbackSQLiteStreamRace(t *testing.T, terminalError, exhaustBudget bool) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "fallback-race.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	policies := sqlite.NewKeyPolicies(db)
	tpm := int64(100)
	if exhaustBudget {
		tpm = 5
	}
	policy, err := policies.Create(ctx, sqlite.CreateKeyPolicyParams{ID: "policy", Models: []string{"model"}, Connectors: []string{"connector-a", "connector-b", "connector-c"}, RPM: 1, TPM: tpm})
	if err != nil {
		t.Fatal(err)
	}
	key, err := sqlite.NewVirtualKeys(db).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	accounts := sqlite.NewAccounts(db)
	for _, target := range []struct{ id, connector string }{{"account-a", "connector-a"}, {"account-b", "connector-b"}, {"account-c", "connector-c"}} {
		if _, err := accounts.Create(ctx, sqlite.Account{ID: target.id, Connector: target.connector, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	stream := newAccountingRaceStream()
	registry, err := core.NewRegistry(map[core.ComponentKind]core.APIVersion{
		core.ComponentAdapter: {Major: 1}, core.ComponentConnector: {Major: 1},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &accountingRaceAdapter{descriptor: core.Descriptor{
		ID: "adapter", Kind: core.ComponentAdapter, ImplementationVersion: "test", APIVersions: []core.APIVersion{{Major: 1}},
		Protocols: []string{accountingRaceProtocol}, Operations: []string{"decode", "encode"},
	}}
	connectors := []*accountingRaceConnector{
		{descriptor: accountingRaceConnectorDescriptor("connector-a"), stream: newAccountingRaceStream(), rejectFirst: true},
		{descriptor: accountingRaceConnectorDescriptor("connector-b"), stream: stream},
		{descriptor: accountingRaceConnectorDescriptor("connector-c"), stream: newAccountingRaceStream()},
	}
	for _, component := range []core.Component{adapter, connectors[0], connectors[1], connectors[2]} {
		id := core.InstanceID(component.Descriptor().ID)
		if err := registry.Register(id, component, component.Descriptor().Kind); err != nil {
			t.Fatal(err)
		}
		if err := registry.Init(ctx, id, core.ComponentConfig{}); err != nil {
			t.Fatal(err)
		}
	}
	defer registry.Close(context.Background())
	routes := make([]core.Route, 3)
	for i, account := range []string{"account-a", "account-b", "account-c"} {
		routes[i] = core.Route{
			Identity: core.RouteIdentity{RouteLookupKey: core.RouteLookupKey{Protocol: accountingRaceProtocol, Mode: core.ModeNative, Model: "model"}, AccountID: account},
			Adapter:  "adapter", Connector: core.InstanceID("connector-" + string(rune('a'+i))), CandidateGroup: "fallback",
			MaxBodyBytes: 1024, MaxHeaderBytes: 1024,
		}
	}
	table, err := core.NewRouteTable(routes, registry)
	if err != nil {
		t.Fatal(err)
	}
	requestCtx := core.WithTrustedPrincipal(ctx, core.TrustedPrincipal{KeyID: key.ID, PolicyID: policy.ID, KeyRevision: 1, PolicyRevision: policy.Revision})
	raceCtx, cancel := context.WithCancel(requestCtx)
	defer cancel()
	dispatcher := &core.Dispatcher{
		Routes: table, Services: raceServices{}, Policies: sqlitePolicyStore{policies: policies}, Accounts: sqliteAccountAuthorizer{accounts: accounts},
		Accounting: sqliteAccountingStore{ledger: sqlite.NewLedger(db)}, Budget: core.RouteBudget{UnknownEstimate: core.UnknownEstimateReject},
		BudgetPolicy: "known", RouteID: "fallback-route", AccountID: "account-a", RetryMaxAttempts: 3, RetryDeadline: time.Second,
	}
	response, gatewayErr := dispatcher.Execute(raceCtx, core.ExecutionRequest{
		Model: "model", Payload: core.RawPayload{Protocol: accountingRaceProtocol, Body: []byte("opaque")},
		Metadata: core.RequestMetadata{AffinityKnown: true, IngressHeaderBytes: 1},
	})
	if exhaustBudget {
		if gatewayErr == nil || gatewayErr.Category != core.CategoryRateLimited || connectors[0].calls.Load() != 1 || connectors[1].calls.Load() != 0 || connectors[2].calls.Load() != 0 {
			t.Fatalf("second attempt budget result=%#v executions=%d/%d/%d", gatewayErr, connectors[0].calls.Load(), connectors[1].calls.Load(), connectors[2].calls.Load())
		}
		requests, err := sqlite.NewLedger(db).QueryRequests(ctx, sqlite.RequestFilter{Limit: 10})
		if err != nil || len(requests) != 1 || len(requests[0].Attempts) != 1 {
			t.Fatalf("budget-rejected fallback ledger = %+v, %v", requests, err)
		}
		reservation := requests[0].Attempts[0].Reservation
		if reservation.State != "conservative" || reservation.EffectiveCharge != 5 || requests[0].Attempts[0].Attempt.Ordinal != 1 {
			t.Fatalf("budget-rejected fallback settlement = %+v", requests[0].Attempts[0])
		}
		return
	}
	if gatewayErr != nil {
		t.Fatalf("execute fallback: code=%q category=%q message=%q", gatewayErr.Code, gatewayErr.Category, gatewayErr.Message)
	}
	if got := connectors[0].calls.Load(); got != 1 {
		t.Fatalf("primary executions = %d, want 1", got)
	}
	if got := connectors[1].calls.Load(); got != 1 {
		t.Fatalf("fallback executions = %d, want 1", got)
	}
	if got := connectors[2].calls.Load(); got != 0 {
		t.Fatalf("committed fallback stream failure invoked eligible third candidate %d times", got)
	}
	frame, err := response.Stream.Next(raceCtx)
	if err != nil || frame.Type != core.FrameHead {
		t.Fatalf("fallback Head = %+v, %v", frame, err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, 3)
	wg.Add(3)
	go func() {
		defer wg.Done()
		<-start
		_, err := response.Stream.Next(raceCtx)
		results <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_ = response.Stream.Close()
	}()
	go func() {
		defer wg.Done()
		<-start
		cancel()
	}()
	complete := accountingRaceEvent{frame: core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{
		Outcome: core.OutcomeSucceeded, Usage: &core.UsageReport{InputTokens: new(int64(4)), OutputTokens: new(int64(6)), Source: core.UsageProvider, Completeness: core.UsageComplete},
	}}}
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		<-start
		if terminalError {
			select {
			case stream.events <- accountingRaceEvent{err: errors.New("controlled producer failure")}:
			case <-stream.closed:
			case <-raceCtx.Done():
			}
			return
		}
		select {
		case stream.events <- complete:
		case <-stream.closed:
			return
		case <-raceCtx.Done():
			return
		}
		select {
		case stream.events <- accountingRaceEvent{err: io.EOF}:
		case <-stream.closed:
		case <-raceCtx.Done():
		}
	}()
	close(start)
	wg.Wait()
	<-producerDone
	close(results)
	for range results { /* both Next and Close have settled */
	}
	_ = response.Stream.Close()
	assertProducerClosed(t, stream)
	if stream.active.Load() != 0 {
		t.Fatalf("fallback stream reader remained active after cancellation/Close: %d", stream.active.Load())
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if got := connectors[2].calls.Load(); got != 0 {
		t.Fatalf("committed stream failure invoked eligible third fallback candidate %d times", got)
	}
	db, err = sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ledger := sqlite.NewLedger(db)
	requests, err := ledger.QueryRequests(ctx, sqlite.RequestFilter{Limit: 10})
	if err != nil || len(requests) != 1 {
		t.Fatalf("request rows = %d, %v", len(requests), err)
	}
	attempts, err := ledger.QueryAttempts(ctx, sqlite.AttemptFilter{RequestID: requests[0].Request.ID, Limit: 10})
	if err != nil || len(attempts) != 2 {
		t.Fatalf("attempt rows = %d, %v", len(attempts), err)
	}
	slices.SortFunc(attempts, func(a, b sqlite.AttemptUsage) int {
		return int(a.Attempt.Ordinal - b.Attempt.Ordinal)
	})
	if attempts[0].Attempt.Ordinal != 1 || attempts[1].Attempt.Ordinal != 2 || attempts[0].Attempt.ID == attempts[1].Attempt.ID {
		t.Fatalf("attempt identities/ordinals: %+v / %+v", attempts[0].Attempt, attempts[1].Attempt)
	}
	firstReservation, err := ledger.GetReservation(ctx, attempts[0].Attempt.ID)
	if err != nil || firstReservation.State != "conservative" || firstReservation.EffectiveCharge != 5 {
		t.Fatalf("safe primary reservation = %+v, %v", firstReservation, err)
	}
	secondReservation, err := ledger.GetReservation(ctx, attempts[1].Attempt.ID)
	if err != nil || secondReservation.State == "held" || secondReservation.ReconciledAt == nil {
		t.Fatalf("fallback reservation = state:%s charge:%d attempt-state:%s usage-present:%t err:%v", secondReservation.State, secondReservation.EffectiveCharge, attempts[1].Attempt.State, attempts[1].Usage != nil, err)
	}
	if terminalError && secondReservation.State != "conservative" || !terminalError && secondReservation.State != "conservative" && secondReservation.State != "settled" {
		t.Fatalf("fallback charge/state = %+v (terminal error %v)", secondReservation, terminalError)
	}
	if secondReservation.State == "settled" && secondReservation.EffectiveCharge != 10 || secondReservation.State == "conservative" && secondReservation.EffectiveCharge != 5 {
		t.Fatalf("fallback effective charge = %+v", secondReservation)
	}
	if attempts[1].Usage == nil {
		t.Fatal("fallback usage record missing")
	}
	if attempts[0].Usage == nil {
		t.Fatal("primary usage record missing")
	}
	summary, err := ledger.QueryUsageSummary(ctx, key.ID, "", nil, nil)
	if err != nil || summary.Requests != 1 || summary.Attempts != 2 || summary.EffectiveCharge != firstReservation.EffectiveCharge+secondReservation.EffectiveCharge {
		t.Fatalf("reopened usage conservation = %+v, %v", summary, err)
	}
	blocked := core.AccountingAdmission{
		RequestID: "another-request", AttemptID: "another-attempt", KeyID: key.ID, KeyRevision: 1,
		PolicyID: policy.ID, PolicyRevision: policy.Revision, Protocol: accountingRaceProtocol, Model: "model", RouteID: "fallback-route",
		AccountID: "account-a", Connector: "connector-a", EstimateTokens: 1, EstimateMethod: "race-fixture", BudgetPolicy: "known",
	}
	if err := (sqliteAccountingStore{ledger: ledger}).Admit(ctx, blocked); !errors.Is(err, core.ErrAdmissionLimit) {
		t.Fatalf("second RPM admission = %v, want limit", err)
	}
}

func accountingRaceConnectorDescriptor(id string) core.Descriptor {
	return core.Descriptor{ID: id, Kind: core.ComponentConnector, ImplementationVersion: "test", APIVersions: []core.APIVersion{{Major: 1}},
		Protocols: []string{accountingRaceProtocol}, Operations: []string{"execute", "estimate_usage"}, ConnectorType: "api", AuthMethods: []string{"api_key"}}
}

type raceServices struct{}

func (raceServices) ForAttempt(core.AttemptScope) core.InvocationServices {
	return core.InvocationServices{}
}
