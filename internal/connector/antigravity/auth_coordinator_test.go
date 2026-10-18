package antigravity

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

type coordinatorFixtureStore struct {
	core.AuthCoordinatorStore
	mu          sync.Mutex
	accounts    map[string]core.AuthAccount
	credentials map[string]core.AuthCredentials
	markers     map[string]bool
	quarantine  map[string]bool
	resolveErr  error
	sessions    map[string]core.AuthSession
	states      map[string][]byte
	claimed     map[string]bool
}

func (s *coordinatorFixtureStore) AuthAccount(_ context.Context, id string) (core.AuthAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.accounts[id]
	if !ok {
		return core.AuthAccount{}, core.ErrAccountUnavailable
	}
	return v, nil
}
func (s *coordinatorFixtureStore) AuthCredentials(_ context.Context, id string) (core.AuthCredentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneCoordinatorCredentials(s.credentials[id]), nil
}
func (s *coordinatorFixtureStore) HasQuarantinedRefresh(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quarantine[id], nil
}
func (s *coordinatorFixtureStore) CreateRefreshMarker(_ context.Context, _ string, session core.AuthSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markers[session.AccountID] {
		return core.ErrRefreshMarkerConflict
	}
	s.markers[session.AccountID] = true
	return nil
}
func (s *coordinatorFixtureStore) ClearRefreshMarker(_ context.Context, _ string, session core.AuthSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.markers, session.AccountID)
	return nil
}
func (s *coordinatorFixtureStore) QuarantineRefreshMarker(_ context.Context, _ string, session core.AuthSession, _ string, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quarantine[session.AccountID] = true
	delete(s.markers, session.AccountID)
	return nil
}
func (s *coordinatorFixtureStore) ResolveRefresh(_ context.Context, _ string, session core.AuthSession, candidate core.AuthCredentials) (core.AuthCredentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resolveErr != nil {
		return core.AuthCredentials{}, s.resolveErr
	}
	current := s.credentials[session.AccountID]
	if current.Revision != candidate.Revision {
		return core.AuthCredentials{}, core.ErrAuthRevisionMismatch
	}
	candidate.Revision++
	candidate.Valid = true
	s.credentials[session.AccountID] = cloneCoordinatorCredentials(candidate)
	delete(s.markers, session.AccountID)
	return cloneCoordinatorCredentials(candidate), nil
}

func (s *coordinatorFixtureStore) CreateAuthSession(_ context.Context, session core.AuthSession, state []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[session.ID] = session
	s.states[session.ID] = append([]byte(nil), state...)
	return nil
}
func (s *coordinatorFixtureStore) GetAuthSession(_ context.Context, id string, now time.Time) (core.AuthSession, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok || s.claimed[id] || !session.ExpiresAt.After(now) {
		return core.AuthSession{}, nil, core.ErrAuthUnavailable
	}
	return session, append([]byte(nil), s.states[id]...), nil
}
func (s *coordinatorFixtureStore) ClaimAuthSession(_ context.Context, session core.AuthSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[session.ID]; !ok || s.claimed[session.ID] {
		return core.ErrAuthUnavailable
	}
	s.claimed[session.ID] = true
	return nil
}
func (s *coordinatorFixtureStore) AdvanceAuthSession(_ context.Context, session core.AuthSession, state []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[session.ID]; !ok {
		return core.ErrAuthUnavailable
	}
	s.states[session.ID] = append([]byte(nil), state...)
	delete(s.claimed, session.ID)
	return nil
}
func (s *coordinatorFixtureStore) ConsumeAuthSession(_ context.Context, session core.AuthSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, session.ID)
	delete(s.states, session.ID)
	delete(s.claimed, session.ID)
	return nil
}
func (s *coordinatorFixtureStore) FinishAuthSession(_ context.Context, session core.AuthSession, candidate core.AuthCredentials) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.credentials[session.AccountID]
	if current.Revision != session.Revision {
		delete(s.sessions, session.ID)
		delete(s.states, session.ID)
		return core.ErrAuthRevisionMismatch
	}
	if len(candidate.Values) > 0 {
		candidate.Revision++
		candidate.Valid = true
		s.credentials[session.AccountID] = cloneCoordinatorCredentials(candidate)
	}
	delete(s.sessions, session.ID)
	delete(s.states, session.ID)
	delete(s.claimed, session.ID)
	return nil
}

func cloneCoordinatorCredentials(v core.AuthCredentials) core.AuthCredentials {
	values := make(map[string][]byte, len(v.Values))
	for k, b := range v.Values {
		values[k] = append([]byte(nil), b...)
	}
	v.Values = values
	if v.ExpiresAt != nil {
		expires := *v.ExpiresAt
		v.ExpiresAt = &expires
	}
	return v
}

type coordinatorServices func(core.AttemptScope) core.InvocationServices

func (f coordinatorServices) ForAttempt(scope core.AttemptScope) core.InvocationServices {
	return f(scope)
}

type expiredAwareCredentials struct{ generation core.AuthCredentials }

func (a expiredAwareCredentials) Get(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !a.generation.Valid || a.generation.ExpiresAt == nil || !a.generation.ExpiresAt.After(time.Now()) {
		return nil, core.ErrCredentialUnavailable
	}
	value, ok := a.generation.Values[name]
	if !ok {
		return nil, core.ErrCredentialUnavailable
	}
	return append([]byte(nil), value...), nil
}

func coordinatorFixture(t *testing.T, store *coordinatorFixtureStore, transport *fakeDoer, conn core.Connector) *core.AuthCoordinator {
	t.Helper()
	registry, err := core.NewRegistry(map[core.ComponentKind]core.APIVersion{core.ComponentConnector: {Major: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("antigravity", conn, core.ComponentConnector); err != nil {
		t.Fatal(err)
	}
	if err := registry.Init(context.Background(), "antigravity", core.ComponentConfig{Data: []byte(`{"profile":"antigravity-v1internal","project":"synthetic-project","oauth":{"client_secret":"SYNTHETIC_CLIENT_SECRET"}}`)}); err != nil {
		t.Fatal(err)
	}
	coordinator, err := core.NewAuthCoordinator(registry, store, coordinatorServices(func(core.AttemptScope) core.InvocationServices {
		return core.InvocationServices{Transport: packageTestDoer(transport)}
	}), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func TestBrowserCoordinatorConsumesAmbiguousContinuationBeforeRedispatch(t *testing.T) {
	ctx := context.Background()
	store := newCoordinatorFixtureStore(nil)
	connector := NewConnector()
	config := []byte(`{"profile":"antigravity-v1internal","project":"synthetic-project","oauth":{"client_id":"synthetic-client","auth_endpoint":"https://auth.synthetic.invalid/authorize","token_endpoint":"https://oauth.synthetic.invalid/token","redirect_uri":"http://localhost:8080/callback"}}`)
	if err := connector.Init(ctx, core.ComponentConfig{Data: config}); err != nil {
		t.Fatal(err)
	}
	registry, err := core.NewRegistry(map[core.ComponentKind]core.APIVersion{core.ComponentConnector: {Major: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Register("antigravity", connector, core.ComponentConnector); err != nil {
		t.Fatal(err)
	}
	if err = registry.Init(ctx, "antigravity", core.ComponentConfig{Data: config}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close(context.Background()) })
	transport := &fakeDoer{err: errors.New("synthetic ambiguous response")}
	services := coordinatorServices(func(core.AttemptScope) core.InvocationServices {
		return core.InvocationServices{Transport: packageTestDoer(transport)}
	})
	coordinator, err := core.NewAuthCoordinator(registry, store, services, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	session, err := coordinator.Start(ctx, "account-a")
	if err != nil {
		t.Fatal(err)
	}
	verify, err := url.Parse(session.UserAction.VerificationURI)
	if err != nil {
		t.Fatal(err)
	}
	callback := "http://localhost:8080/callback?code=synthetic-code&state=" + url.QueryEscape(verify.Query().Get("state"))
	if _, err = coordinator.Continue(ctx, session.ID, callback); err == nil || transport.calls != 1 {
		t.Fatalf("ambiguous exchange: err=%v calls=%d", err, transport.calls)
	}
	transport.err = nil
	transport.response = response(http.StatusOK, `{"access_token":"SYNTHETIC_ACCESS","expires_in":3600,"token_type":"Bearer"}`)
	if _, err = coordinator.Continue(ctx, session.ID, callback); err == nil || transport.calls != 1 {
		t.Fatalf("consumed state dispatched again: err=%v calls=%d", err, transport.calls)
	}
	stored, _ := store.AuthCredentials(ctx, "account-a")
	if stored.Revision != 4 || !strings.Contains(string(stored.Values["oauth"]), "SYNTHETIC_A_REFRESH") {
		t.Fatalf("uncertain callback persisted credentials: %+v", stored)
	}
}

func TestBrowserCoordinatorRejectsNewlineRefreshCandidateWithoutPersistence(t *testing.T) {
	ctx := context.Background()
	store := newCoordinatorFixtureStore(nil)
	connector := NewConnector()
	config := []byte(`{"profile":"antigravity-v1internal","project":"synthetic-project","oauth":{"client_id":"synthetic-client","auth_endpoint":"https://auth.synthetic.invalid/authorize","token_endpoint":"https://oauth.synthetic.invalid/token","redirect_uri":"http://localhost:8080/callback"}}`)
	if err := connector.Init(ctx, core.ComponentConfig{Data: config}); err != nil {
		t.Fatal(err)
	}
	registry, err := core.NewRegistry(map[core.ComponentKind]core.APIVersion{core.ComponentConnector: {Major: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Register("antigravity", connector, core.ComponentConnector); err != nil {
		t.Fatal(err)
	}
	if err = registry.Init(ctx, "antigravity", core.ComponentConfig{Data: config}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close(context.Background()) })
	transport := &fakeDoer{response: response(http.StatusOK, `{"access_token":"SYNTHETIC_ACCESS","refresh_token":"SYNTHETIC_BAD\nTOKEN","expires_in":3600,"token_type":"Bearer"}`)}
	services := coordinatorServices(func(core.AttemptScope) core.InvocationServices {
		return core.InvocationServices{Transport: packageTestDoer(transport)}
	})
	coordinator, err := core.NewAuthCoordinator(registry, store, services, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	session, err := coordinator.Start(ctx, "account-a")
	if err != nil {
		t.Fatal(err)
	}
	verify, err := url.Parse(session.UserAction.VerificationURI)
	if err != nil {
		t.Fatal(err)
	}
	callback := "http://localhost:8080/callback?code=synthetic-code&state=" + url.QueryEscape(verify.Query().Get("state"))
	if _, err = coordinator.Continue(ctx, session.ID, callback); err == nil || transport.calls != 1 {
		t.Fatalf("invalid candidate: err=%v calls=%d", err, transport.calls)
	}
	credentials, _ := store.AuthCredentials(ctx, "account-a")
	if credentials.Revision != 4 || !strings.Contains(string(credentials.Values["oauth"]), "SYNTHETIC_A_REFRESH") {
		t.Fatalf("invalid candidate persisted: %+v", credentials)
	}
	if _, err = coordinator.Continue(ctx, session.ID, callback); err == nil || transport.calls != 1 {
		t.Fatalf("invalid session dispatched again: err=%v calls=%d", err, transport.calls)
	}
}

func newCoordinatorFixtureStore(resolveErr error) *coordinatorFixtureStore {
	expired := time.Now().Add(-time.Minute)
	return &coordinatorFixtureStore{
		accounts: map[string]core.AuthAccount{
			"account-a": {ID: "account-a", ConnectorID: "antigravity", Enabled: true},
			"account-b": {ID: "account-b", ConnectorID: "antigravity", Enabled: true},
		},
		credentials: map[string]core.AuthCredentials{
			"account-a": {Revision: 4, Valid: true, Values: map[string][]byte{"oauth": []byte(`{"access_token":"SYNTHETIC_A_EXPIRED_ACCESS","refresh_token":"SYNTHETIC_A_REFRESH"}`)}, ExpiresAt: &expired},
			"account-b": {Revision: 9, Valid: true, Values: map[string][]byte{"oauth": []byte(`{"access_token":"SYNTHETIC_B_ACCESS","refresh_token":"SYNTHETIC_B_REFRESH"}`)}, ExpiresAt: &expired},
		},
		markers: map[string]bool{}, quarantine: map[string]bool{}, resolveErr: resolveErr,
		sessions: map[string]core.AuthSession{}, states: map[string][]byte{}, claimed: map[string]bool{},
	}
}

func TestRefreshCoordinatorCASReloadAndAccountScope(t *testing.T) {
	body, err := os.ReadFile("testdata/oauth-refresh-rotated.json")
	if err != nil {
		t.Fatal(err)
	}
	refresh := &fakeDoer{response: &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}}
	store := newCoordinatorFixtureStore(nil)
	coordinator := coordinatorFixture(t, store, refresh, ready(t))

	before, _ := store.AuthCredentials(context.Background(), "account-a")
	blocked := &fakeDoer{response: response(http.StatusOK, "data: {\"response\":{\"done\":true}}\n\n")}
	_, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{Transport: packageTestDoer(blocked), Credentials: expiredAwareCredentials{generation: before}})
	if ge == nil || ge.Category != core.CategoryUnauthenticated || blocked.calls != 0 {
		t.Fatalf("expired generation reached Execute: error=%v calls=%d", ge, blocked.calls)
	}

	resolved, err := coordinator.Refresh(context.Background(), "account-a")
	if err != nil || !resolved.Valid || resolved.Revision != 5 || resolved.ExpiresAt == nil || !resolved.ExpiresAt.After(time.Now()) || refresh.calls != 1 {
		t.Fatalf("refresh resolve=%+v error=%v exchange calls=%d", resolved, err, refresh.calls)
	}
	formBytes, _ := io.ReadAll(refresh.request.Body)
	form, err := url.ParseQuery(string(formBytes))
	if err != nil || form.Get("refresh_token") != "SYNTHETIC_A_REFRESH" {
		t.Fatalf("wrong account refresh material %q (%v)", formBytes, err)
	}
	loaded, _ := store.AuthCredentials(context.Background(), "account-a")
	if loaded.Revision != 5 || !loaded.ExpiresAt.Equal(*resolved.ExpiresAt) || string(loaded.Values["oauth"]) != string(resolved.Values["oauth"]) {
		t.Fatalf("CAS generation/expiry not persisted together: loaded=%+v resolved=%+v", loaded, resolved)
	}
	untouched, _ := store.AuthCredentials(context.Background(), "account-b")
	if untouched.Revision != 9 || !strings.Contains(string(untouched.Values["oauth"]), "SYNTHETIC_B_REFRESH") {
		t.Fatalf("other account changed: %+v", untouched)
	}

	executeTransport := &fakeDoer{response: response(http.StatusOK, "data: {\"response\":{\"done\":true}}\n\n")}
	out, ge := ready(t).Execute(context.Background(), request(`{"model":"gemini-2.5-pro","input":"x","stream":true}`), core.AttemptScope{Mode: core.ModeTranslation}, core.InvocationServices{Transport: packageTestDoer(executeTransport), Credentials: expiredAwareCredentials{generation: loaded}})
	if ge != nil || executeTransport.calls != 1 || executeTransport.request.Header.Get("Authorization") != "Bearer SYNTHETIC_ROTATED_ACCESS_TOKEN" || refresh.calls != 1 {
		t.Fatalf("reloaded execution error=%v refresh calls=%d execute calls=%d", ge, refresh.calls, executeTransport.calls)
	}
	for name, values := range executeTransport.request.Header {
		if strings.Contains(strings.ToLower(name), "refresh") || strings.Contains(strings.Join(values, " "), "SYNTHETIC_A_REFRESH") {
			t.Fatalf("refresh credential leaked in header %q", name)
		}
	}
	executeBody, err := io.ReadAll(executeTransport.request.Body)
	if err != nil || strings.Contains(string(executeBody), "SYNTHETIC_A_REFRESH") || strings.Contains(string(executeBody), "SYNTHETIC_B_") {
		t.Fatalf("credential leaked in execute body: %q (%v)", executeBody, err)
	}
	_ = out.Stream.Close()
}

func TestRefreshCoordinatorQuarantinesUncertainPersistenceAndInvalidResults(t *testing.T) {
	for _, tc := range []struct {
		name       string
		persistErr error
		invalid    bool
	}{
		{name: "stale CAS", persistErr: core.ErrAuthRevisionMismatch},
		{name: "failed persistence", persistErr: errors.New("synthetic store failure")},
		{name: "continuation returned", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newCoordinatorFixtureStore(tc.persistErr)
			var connector core.Connector = ready(t)
			if tc.invalid {
				connector = continuationAuthConnector{Connector: connector}
			}
			transport := &fakeDoer{response: &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"access_token":"SYNTHETIC_ROTATED_ACCESS_TOKEN","expires_in":60}`))}}
			coordinator := coordinatorFixture(t, store, transport, connector)
			resolved, err := coordinator.Refresh(context.Background(), "account-a")
			if err == nil || resolved.Valid || len(resolved.Values) != 0 || transport.calls != 1 || !store.quarantine["account-a"] {
				t.Fatalf("candidate escaped: resolved=%+v error=%v calls=%d quarantined=%t", resolved, err, transport.calls, store.quarantine["account-a"])
			}
			old, _ := store.AuthCredentials(context.Background(), "account-a")
			if old.Revision != 4 || !strings.Contains(string(old.Values["oauth"]), "SYNTHETIC_A_REFRESH") {
				t.Fatalf("uncertain candidate replaced prior generation: %+v", old)
			}
		})
	}
}

type continuationAuthConnector struct{ core.Connector }

func (c continuationAuthConnector) Authenticate(ctx context.Context, req core.AuthRequest, services core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	result, ge := c.Connector.Authenticate(ctx, req, services)
	if ge == nil && result.Supported {
		result.State = "unexpected-state"
	}
	return result, ge
}

func TestDescriptorDeclaresAuthenticateWithoutPromotingCapability(t *testing.T) {
	c := ready(t)
	descriptor := c.Descriptor()
	if !slices.Contains(descriptor.Operations, "authenticate") {
		t.Fatalf("descriptor operations=%v", descriptor.Operations)
	}
	capabilities := c.Capabilities(context.Background(), core.CapabilityScope{Protocol: protocol, Mode: core.ModeTranslation})
	if capabilities.Values["auth.oauth"] != core.Unknown {
		t.Fatalf("auth capability promoted: %+v", capabilities.Values)
	}
}
