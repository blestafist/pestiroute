package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type authCoordinatorTestConnector struct {
	minimalConnector
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	finish  chan struct{}
	respond func(AuthRequest) (AuthResult, *GatewayError)
}

func (c *authCoordinatorTestConnector) Descriptor() Descriptor {
	return Descriptor{ID: "test", Kind: ComponentConnector, ImplementationVersion: "1", APIVersions: []APIVersion{{Major: 1}}, Protocols: []string{"test"}, ConnectorType: "scripted"}
}
func (c *authCoordinatorTestConnector) Health(context.Context) Health {
	return Health{State: HealthReady}
}
func (c *authCoordinatorTestConnector) Authenticate(ctx context.Context, req AuthRequest, _ InvocationServices) (AuthResult, *GatewayError) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if c.respond != nil {
		return c.respond(req)
	}
	if req.Action == "refresh" {
		if c.entered != nil {
			select {
			case c.entered <- struct{}{}:
			default:
			}
		}
		if c.finish != nil {
			select {
			case <-c.finish:
			case <-ctx.Done():
				return AuthResult{}, nil
			}
		}
		return AuthResult{Supported: true, Credentials: map[string][]byte{"token": []byte("rotated")}}, nil
	}
	if req.Action == "continue" {
		return AuthResult{Supported: true, Credentials: map[string][]byte{"token": []byte("interactive")}}, nil
	}
	return AuthResult{Supported: true, State: "opaque-state", NextAction: "continue"}, nil
}

type authCoordinatorTestStore struct {
	claimErr      error
	mu            sync.Mutex
	accounts      map[string]AuthAccount
	credentials   map[string]AuthCredentials
	sessions      map[string]AuthSession
	states        map[string][]byte
	markers       map[string]bool
	readNotify    chan struct{}
	quarantine    string
	failResolve   bool
	staleResolve  bool
	afterMarker   func()
	invalidations int
}

func (s *authCoordinatorTestStore) AuthAccount(_ context.Context, id string) (AuthAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.accounts[id]
	if !ok {
		return AuthAccount{}, ErrAccountUnavailable
	}
	return v, nil
}
func (s *authCoordinatorTestStore) AuthCredentials(_ context.Context, id string) (AuthCredentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readNotify != nil {
		select {
		case s.readNotify <- struct{}{}:
		default:
		}
	}
	return cloneAuthCredentials(s.credentials[id]), nil
}
func (s *authCoordinatorTestStore) CreateAuthSession(_ context.Context, v AuthSession, state []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[v.ID] = v
	s.states[v.ID] = append([]byte(nil), state...)
	return nil
}
func (s *authCoordinatorTestStore) GetAuthSession(_ context.Context, id string, now time.Time) (AuthSession, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.sessions[id]
	if !ok || !v.ExpiresAt.After(now) {
		return AuthSession{}, nil, ErrAuthUnavailable
	}
	return v, append([]byte(nil), s.states[id]...), nil
}
func (s *authCoordinatorTestStore) AdvanceAuthSession(_ context.Context, v AuthSession, state []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.credentials[v.AccountID].Revision != v.Revision {
		delete(s.sessions, v.ID)
		delete(s.states, v.ID)
		return ErrAuthRevisionMismatch
	}
	s.states[v.ID] = append([]byte(nil), state...)
	return nil
}
func (s *authCoordinatorTestStore) ConsumeAuthSession(_ context.Context, v AuthSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, v.ID)
	delete(s.states, v.ID)
	return nil
}
func (s *authCoordinatorTestStore) FinishAuthSession(_ context.Context, v AuthSession, candidates AuthCredentials) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.credentials[v.AccountID].Revision != v.Revision {
		delete(s.sessions, v.ID)
		delete(s.states, v.ID)
		return ErrAuthRevisionMismatch
	}
	if len(candidates.Values) > 0 {
		creds := AuthCredentials{Revision: v.Revision + 1, Valid: true, Values: cloneSecretMap(candidates.Values), ExpiresAt: cloneTime(candidates.ExpiresAt)}
		s.credentials[v.AccountID] = creds
	}
	delete(s.sessions, v.ID)
	delete(s.states, v.ID)
	return nil
}
func (s *authCoordinatorTestStore) CreateRefreshMarker(_ context.Context, _ string, session AuthSession) error {
	s.mu.Lock()
	if s.markers[session.AccountID] {
		s.mu.Unlock()
		return ErrRefreshMarkerConflict
	}
	s.markers[session.AccountID] = true
	hook := s.afterMarker
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}
func (s *authCoordinatorTestStore) ClearRefreshMarker(_ context.Context, _ string, session AuthSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.markers, session.AccountID)
	return nil
}
func (s *authCoordinatorTestStore) QuarantineRefreshMarker(_ context.Context, _ string, _ AuthSession, reason string, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quarantine = reason
	return nil
}
func (s *authCoordinatorTestStore) ResolveRefresh(_ context.Context, _ string, v AuthSession, creds AuthCredentials) (AuthCredentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failResolve {
		return AuthCredentials{}, errors.New("injected storage failure")
	}
	if s.staleResolve {
		return AuthCredentials{}, ErrAuthRevisionMismatch
	}
	old := s.credentials[v.AccountID]
	if old.Revision != creds.Revision {
		return AuthCredentials{}, ErrAuthRevisionMismatch
	}
	creds.Revision++
	creds.Valid = true
	s.credentials[v.AccountID] = cloneAuthCredentials(creds)
	delete(s.markers, v.AccountID)
	return creds, nil
}
func (s *authCoordinatorTestStore) ReplaceAuthCredentials(_ context.Context, id string, revision int64, creds AuthCredentials) (AuthCredentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.credentials[id]
	if old.Revision != revision {
		return AuthCredentials{}, ErrAuthRevisionMismatch
	}
	creds.Revision++
	creds.Valid = true
	s.credentials[id] = cloneAuthCredentials(creds)
	return creds, nil
}
func (s *authCoordinatorTestStore) InvalidateAuth(context.Context, string, InstanceID, time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidations++
	return nil
}

type authServicesFunc func(AttemptScope) InvocationServices

func (f authServicesFunc) ForAttempt(s AttemptScope) InvocationServices { return f(s) }

func newAuthCoordinatorTest(t *testing.T, connector *authCoordinatorTestConnector, accounts ...string) (*AuthCoordinator, *authCoordinatorTestStore) {
	t.Helper()
	registry, err := NewRegistry(map[ComponentKind]APIVersion{ComponentConnector: {Major: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Register("connector", connector, ComponentConnector); err != nil {
		t.Fatal(err)
	}
	if err = registry.Init(context.Background(), "connector", ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	store := &authCoordinatorTestStore{accounts: map[string]AuthAccount{}, credentials: map[string]AuthCredentials{}, sessions: map[string]AuthSession{}, states: map[string][]byte{}, markers: map[string]bool{}}
	for _, id := range accounts {
		store.accounts[id] = AuthAccount{ID: id, ConnectorID: "connector", Enabled: true}
		store.credentials[id] = AuthCredentials{Revision: 1, Values: map[string][]byte{"token": []byte("old")}}
	}
	c, err := NewAuthCoordinator(registry, store, authServicesFunc(func(AttemptScope) InvocationServices { return InvocationServices{} }), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return c, store
}

func TestAuthCoordinatorUserActionProjectionAndRejection(t *testing.T) {
	valid := &AuthUserAction{VerificationURI: "https://example.test/device", UserCode: "AB CD", PollInterval: 5 * time.Second}
	t.Run("start projects transient action", func(t *testing.T) {
		connector := &authCoordinatorTestConnector{respond: func(AuthRequest) (AuthResult, *GatewayError) {
			return AuthResult{Supported: true, State: "opaque", NextAction: "continue", UserAction: valid}, nil
		}}
		c, store := newAuthCoordinatorTest(t, connector, "a")
		session, err := c.Start(context.Background(), "a")
		if err != nil || session.UserAction == nil || *session.UserAction != *valid {
			t.Fatalf("session=%+v err=%v", session, err)
		}
		if stored := store.sessions[session.ID]; stored.UserAction != nil {
			t.Fatalf("presentation was persisted: %+v", stored.UserAction)
		}
	})
	for name, action := range map[string]*AuthUserAction{
		"scheme":       {VerificationURI: "http://example.test", UserCode: "code", PollInterval: time.Second},
		"userinfo":     {VerificationURI: "https://user@example.test", UserCode: "code", PollInterval: time.Second},
		"space":        {VerificationURI: "https://example.test/a b", UserCode: "code", PollInterval: time.Second},
		"long URI":     {VerificationURI: "https://example.test/" + strings.Repeat("x", 2049), UserCode: "code", PollInterval: time.Second},
		"code control": {VerificationURI: "https://example.test", UserCode: "bad\ncode", PollInterval: time.Second},
		"long code":    {VerificationURI: "https://example.test", UserCode: strings.Repeat("x", 257), PollInterval: time.Second},
		"interval":     {VerificationURI: "https://example.test", UserCode: "code", PollInterval: 0},
	} {
		t.Run("start rejects "+name, func(t *testing.T) {
			connector := &authCoordinatorTestConnector{respond: func(AuthRequest) (AuthResult, *GatewayError) {
				return AuthResult{Supported: true, State: "secret-state", NextAction: "continue", UserAction: action}, nil
			}}
			c, store := newAuthCoordinatorTest(t, connector, "a")
			if session, err := c.Start(context.Background(), "a"); err == nil || session.ID != "" || len(store.sessions) != 0 {
				t.Fatalf("session=%+v err=%v stored=%d", session, err, len(store.sessions))
			}
		})
	}
	for name, result := range map[string]AuthResult{
		"completed":   {Supported: true, UserAction: valid},
		"unsupported": {UserAction: valid, NextAction: "continue"},
	} {
		t.Run("rejects action on "+name, func(t *testing.T) {
			connector := &authCoordinatorTestConnector{respond: func(AuthRequest) (AuthResult, *GatewayError) { return result, nil }}
			c, store := newAuthCoordinatorTest(t, connector, "a")
			if session, err := c.Start(context.Background(), "a"); err == nil || session.ID != "" || len(store.sessions) != 0 {
				t.Fatalf("session=%+v err=%v stored=%d", session, err, len(store.sessions))
			}
		})
	}
	t.Run("continue consumes malformed claimed result", func(t *testing.T) {
		connector := &authCoordinatorTestConnector{respond: func(req AuthRequest) (AuthResult, *GatewayError) {
			if req.Action == "start" {
				return AuthResult{Supported: true, State: "opaque", NextAction: "continue"}, nil
			}
			return AuthResult{Supported: true, State: "next-secret", NextAction: "continue", UserAction: &AuthUserAction{VerificationURI: "https://example.test", UserCode: "code", PollInterval: -time.Second}}, nil
		}}
		c, store := newAuthCoordinatorTest(t, connector, "a")
		session, err := c.Start(context.Background(), "a")
		if err != nil {
			t.Fatal(err)
		}
		if got, err := c.Continue(context.Background(), session.ID); err == nil || got.ID != "" || len(store.sessions) != 0 || len(store.states) != 0 {
			t.Fatalf("session=%+v err=%v remaining=%d", got, err, len(store.sessions))
		}
	})
	t.Run("continue projects pending action", func(t *testing.T) {
		connector := &authCoordinatorTestConnector{respond: func(req AuthRequest) (AuthResult, *GatewayError) {
			if req.Action == "start" {
				return AuthResult{Supported: true, State: "opaque", NextAction: "continue"}, nil
			}
			return AuthResult{Supported: true, State: "next", NextAction: "continue", UserAction: valid}, nil
		}}
		c, store := newAuthCoordinatorTest(t, connector, "a")
		started, err := c.Start(context.Background(), "a")
		if err != nil {
			t.Fatal(err)
		}
		continued, err := c.Continue(context.Background(), started.ID)
		if err != nil || continued.UserAction == nil || *continued.UserAction != *valid {
			t.Fatalf("session=%+v err=%v", continued, err)
		}
		if store.sessions[started.ID].UserAction != nil {
			t.Fatal("presentation was persisted on continuation")
		}
	})
	t.Run("refresh quarantines action result", func(t *testing.T) {
		connector := &authCoordinatorTestConnector{respond: func(AuthRequest) (AuthResult, *GatewayError) {
			return AuthResult{Supported: true, Credentials: map[string][]byte{"token": []byte("candidate")}, UserAction: valid}, nil
		}}
		c, store := newAuthCoordinatorTest(t, connector, "a")
		if _, err := c.Refresh(context.Background(), "a"); err == nil || store.quarantine != "ambiguous_result" {
			t.Fatalf("err=%v quarantine=%q", err, store.quarantine)
		}
	})
}

func TestAuthCoordinatorRefreshCoalescesSameAccount(t *testing.T) {
	connector := &authCoordinatorTestConnector{entered: make(chan struct{}, 1), finish: make(chan struct{})}
	c, s := newAuthCoordinatorTest(t, connector, "a")
	s.readNotify = make(chan struct{}, 4)
	var ok atomic.Int32
	errs := make(chan error, 2)
	go func() {
		_, e := c.Refresh(context.Background(), "a")
		if e == nil {
			ok.Add(1)
		}
		errs <- e
	}()
	<-connector.entered
	<-s.readNotify
	<-s.readNotify
	go func() {
		_, e := c.Refresh(context.Background(), "a")
		if e == nil {
			ok.Add(1)
		}
		errs <- e
	}()
	<-s.readNotify // waiter captured the old revision before blocking on the account lock
	close(connector.finish)
	for range 2 {
		if e := <-errs; e != nil {
			t.Fatal(e)
		}
	}
	connector.mu.Lock()
	calls := connector.calls
	connector.mu.Unlock()
	if calls != 1 || ok.Load() != 2 {
		t.Fatalf("calls=%d successful callers=%d, want one exchange and two successes", calls, ok.Load())
	}
	if got := s.credentials["a"].Revision; got != 2 {
		t.Fatalf("revision=%d, want 2", got)
	}
}

func TestAuthCoordinatorAccountsDoNotShareCredentialResults(t *testing.T) {
	connector := &authCoordinatorTestConnector{entered: make(chan struct{}, 2), finish: make(chan struct{})}
	c, s := newAuthCoordinatorTest(t, connector, "a", "b")
	errs := make(chan error, 2)
	for _, id := range []string{"a", "b"} {
		go func(account string) { _, err := c.Refresh(context.Background(), account); errs <- err }(id)
	}
	for range 2 {
		<-connector.entered
	}
	close(connector.finish)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if string(s.credentials["a"].Values["token"]) != "rotated" || string(s.credentials["b"].Values["token"]) != "rotated" {
		t.Fatal("refresh result was not persisted to each selected account")
	}
	if s.credentials["a"].Revision != 2 || s.credentials["b"].Revision != 2 {
		t.Fatalf("account revisions crossed: %#v", s.credentials)
	}
}

func TestAuthCoordinatorInteractiveSessionUsesOpaqueStoredState(t *testing.T) {
	connector := &authCoordinatorTestConnector{}
	c, s := newAuthCoordinatorTest(t, connector, "a")
	session, err := c.Start(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if session.ID == "" || session.ID == "opaque-state" || string(s.states[session.ID]) != "opaque-state" {
		t.Fatalf("session/state = %#v / %q", session, s.states[session.ID])
	}
	if _, err := c.Continue(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.sessions[session.ID]; ok {
		t.Fatal("completed session was not consumed")
	}
}

func TestAuthCoordinatorPersistsCredentialExpiry(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, flow := range []string{"start", "continue", "refresh"} {
		for _, expiryKind := range []string{"future", "zero", "past"} {
			t.Run(flow+"/"+expiryKind, func(t *testing.T) {
				expiry := now.Add(time.Hour)
				if expiryKind == "zero" {
					expiry = time.Time{}
				} else if expiryKind == "past" {
					expiry = now.Add(-time.Second)
				}
				connector := &authCoordinatorTestConnector{respond: func(req AuthRequest) (AuthResult, *GatewayError) {
					if flow == "continue" && req.Action == "start" {
						return AuthResult{Supported: true, State: "state", NextAction: "continue"}, nil
					}
					return AuthResult{Supported: true, CredentialExpiresAt: &expiry, Credentials: map[string][]byte{"token": []byte("new")}}, nil
				}}
				c, store := newAuthCoordinatorTest(t, connector, "a")
				c.clock = func() time.Time { return now }
				var err error
				switch flow {
				case "start":
					_, err = c.Start(context.Background(), "a")
				case "continue":
					var session AuthSession
					session, err = c.Start(context.Background(), "a")
					if err == nil {
						_, err = c.Continue(context.Background(), session.ID)
					}
				case "refresh":
					_, err = c.Refresh(context.Background(), "a")
				}
				if expiryKind == "future" {
					if err != nil {
						t.Fatal(err)
					}
					got := store.credentials["a"]
					if got.Revision != 2 || got.ExpiresAt == nil || !got.ExpiresAt.Equal(expiry) {
						t.Fatalf("stored credentials %#v", got)
					}
					return
				}
				if !errors.Is(err, ErrAuthUnavailable) {
					t.Fatalf("error=%v, want invalid expiry rejection", err)
				}
				got := store.credentials["a"]
				if got.Revision != 1 || got.ExpiresAt != nil || string(got.Values["token"]) != "old" {
					t.Fatalf("invalid expiry mutated credentials: %#v", got)
				}
			})
		}
	}
}

func TestAuthCoordinatorRejectsStaleContinuation(t *testing.T) {
	connector := &authCoordinatorTestConnector{}
	c, s := newAuthCoordinatorTest(t, connector, "a")
	session, err := c.Start(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	s.credentials["a"] = AuthCredentials{Revision: 2, Valid: true}
	if _, err = c.Continue(context.Background(), session.ID); !errors.Is(err, ErrAuthRevisionMismatch) {
		t.Fatalf("continue error=%v", err)
	}
	if _, ok := s.sessions[session.ID]; ok {
		t.Fatal("stale session was not consumed")
	}
}

func TestAuthCoordinatorPreflightCancellationClearsMarker(t *testing.T) {
	connector := &authCoordinatorTestConnector{}
	c, store := newAuthCoordinatorTest(t, connector, "a")
	ctx, cancel := context.WithCancel(context.Background())
	store.afterMarker = cancel
	if _, err := c.Refresh(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("refresh error=%v, want cancellation", err)
	}
	connector.mu.Lock()
	calls := connector.calls
	connector.mu.Unlock()
	if calls != 0 || len(store.markers) != 0 {
		t.Fatalf("calls=%d markers=%d, want no exchange and cleared marker", calls, len(store.markers))
	}
}

func TestAuthCoordinatorPostExchangeFailuresQuarantine(t *testing.T) {
	t.Run("cancellation after call", func(t *testing.T) {
		connector := &authCoordinatorTestConnector{entered: make(chan struct{}, 1), finish: make(chan struct{})}
		c, store := newAuthCoordinatorTest(t, connector, "a")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := c.Refresh(ctx, "a"); done <- err }()
		<-connector.entered
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("refresh error=%v", err)
		}
		if store.quarantine != "cancelled_after_call" {
			t.Fatalf("quarantine=%q", store.quarantine)
		}
		if store.credentials["a"].Revision != 1 {
			t.Fatal("cancelled candidate credentials were persisted")
		}
	})
	t.Run("credential write failure", func(t *testing.T) {
		connector := &authCoordinatorTestConnector{}
		c, store := newAuthCoordinatorTest(t, connector, "a")
		store.failResolve = true
		if _, err := c.Refresh(context.Background(), "a"); !errors.Is(err, ErrAuthPersistence) {
			t.Fatalf("refresh error=%v", err)
		}
		if store.quarantine != "persistence_failed" {
			t.Fatalf("quarantine=%q", store.quarantine)
		}
		if store.credentials["a"].Revision != 1 || string(store.credentials["a"].Values["token"]) != "old" {
			t.Fatal("failed write changed old credentials")
		}
	})
}

func TestAuthCoordinatorContinueAdvancesOpaqueStateThenFinishes(t *testing.T) {
	connector := &authCoordinatorTestConnector{}
	connector.respond = func(req AuthRequest) (AuthResult, *GatewayError) {
		switch {
		case req.Action == "start":
			return AuthResult{Supported: true, NextAction: "continue", State: "state-one"}, nil
		case req.Action == "continue" && string(req.State) == "state-one":
			return AuthResult{Supported: true, NextAction: "continue", State: "state-two"}, nil
		case req.Action == "continue" && string(req.State) == "state-two":
			return AuthResult{Supported: true, Credentials: map[string][]byte{"token": []byte("complete")}}, nil
		default:
			return AuthResult{}, nil
		}
	}
	c, store := newAuthCoordinatorTest(t, connector, "a")
	session, err := c.Start(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	expiry := session.ExpiresAt
	if next, err := c.Continue(context.Background(), session.ID); err != nil || next.ExpiresAt != expiry || string(store.states[session.ID]) != "state-two" {
		t.Fatalf("advance result=%#v state=%q err=%v", next, store.states[session.ID], err)
	}
	if _, err := c.Continue(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.sessions[session.ID]; ok || string(store.credentials["a"].Values["token"]) != "complete" {
		t.Fatal("terminal continue did not atomically persist/consume")
	}
}

func TestAuthCoordinatorConcurrentContinueReloadsStateUnderAccountLock(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var states []string
	connector := &authCoordinatorTestConnector{respond: func(req AuthRequest) (AuthResult, *GatewayError) {
		if req.Action == "start" {
			return AuthResult{Supported: true, NextAction: "continue", State: "state-one"}, nil
		}
		mu.Lock()
		states = append(states, string(req.State))
		mu.Unlock()
		if string(req.State) == "state-one" {
			close(entered)
			<-release
			return AuthResult{Supported: true, NextAction: "continue", State: "state-two"}, nil
		}
		return AuthResult{Supported: true}, nil
	}}
	c, _ := newAuthCoordinatorTest(t, connector, "a")
	session, err := c.Start(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	go func() { _, e := c.Continue(context.Background(), session.ID); errs <- e }()
	<-entered
	go func() { _, e := c.Continue(context.Background(), session.ID); errs <- e }()
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(states) != 2 || states[0] != "state-one" || states[1] != "state-two" {
		t.Fatalf("connector received states %q", states)
	}
}

func TestAuthCoordinatorAmbiguousConnectorFailuresAreQuarantinedAndSanitized(t *testing.T) {
	for _, tc := range []struct {
		name       string
		result     AuthResult
		callErr    *GatewayError
		wantReason string
	}{{"unsupported", AuthResult{}, nil, "ambiguous_result"}, {"connector error", AuthResult{}, &GatewayError{Code: "failure", Message: "secret-token-must-not-leak"}, "ambiguous_result"}} {
		t.Run(tc.name, func(t *testing.T) {
			connector := &authCoordinatorTestConnector{respond: func(AuthRequest) (AuthResult, *GatewayError) { return tc.result, tc.callErr }}
			c, store := newAuthCoordinatorTest(t, connector, "a")
			_, err := c.Refresh(context.Background(), "a")
			if err == nil || strings.Contains(err.Error(), "secret-token-must-not-leak") {
				t.Fatalf("refresh error=%v", err)
			}
			if store.quarantine != tc.wantReason {
				t.Fatalf("reason=%q", store.quarantine)
			}
		})
	}
}

func TestAuthCoordinatorStaleRefreshCASQuarantinesWithoutOverwrite(t *testing.T) {
	connector := &authCoordinatorTestConnector{}
	c, store := newAuthCoordinatorTest(t, connector, "a")
	store.staleResolve = true
	if _, err := c.Refresh(context.Background(), "a"); !errors.Is(err, ErrAuthPersistence) {
		t.Fatalf("refresh error=%v", err)
	}
	if store.quarantine != "ambiguous_result" || store.credentials["a"].Revision != 1 || string(store.credentials["a"].Values["token"]) != "old" {
		t.Fatalf("quarantine=%q credentials=%#v", store.quarantine, store.credentials["a"])
	}
}

func TestAuthCoordinatorContinueConnectorFailureConsumesAndSanitizes(t *testing.T) {
	connector := &authCoordinatorTestConnector{respond: func(req AuthRequest) (AuthResult, *GatewayError) {
		if req.Action == "start" {
			return AuthResult{Supported: true, NextAction: "continue", State: "private-state"}, nil
		}
		return AuthResult{}, &GatewayError{Code: "failed", Message: "private-state secret-value"}
	}}
	c, store := newAuthCoordinatorTest(t, connector, "a")
	session, err := c.Start(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Continue(context.Background(), session.ID); err == nil || strings.Contains(err.Error(), "private-state") || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("continue leaked connector data: %v", err)
	}
	if _, ok := store.sessions[session.ID]; ok {
		t.Fatal("failed continuation remained active")
	}
}

func TestAuthCoordinatorInvalidationDelegatesAccountAndConnectorScope(t *testing.T) {
	c, store := newAuthCoordinatorTest(t, &authCoordinatorTestConnector{}, "a")
	if err := c.Invalidate(context.Background(), "a", "connector"); err != nil {
		t.Fatal(err)
	}
	if store.invalidations != 1 {
		t.Fatalf("invalidations=%d", store.invalidations)
	}
}

func TestAuthCoordinatorInvalidRefreshResultQuarantines(t *testing.T) {
	for _, result := range []AuthResult{
		{Supported: true, NextAction: "continue", Credentials: map[string][]byte{"token": []byte("candidate")}},
		{Supported: true, NextAction: "unrecognized", Credentials: map[string][]byte{"token": []byte("candidate")}},
		{Supported: true, State: "unfinished-state", Credentials: map[string][]byte{"token": []byte("candidate")}},
		{Supported: true, Credentials: map[string][]byte{"token": {}}},
	} {
		connector := &authCoordinatorTestConnector{respond: func(AuthRequest) (AuthResult, *GatewayError) { return result, nil }}
		c, store := newAuthCoordinatorTest(t, connector, "a")
		if _, err := c.Refresh(context.Background(), "a"); !errors.Is(err, ErrAuthUnavailable) {
			t.Fatalf("invalid refresh accepted: %v", err)
		}
		if store.credentials["a"].Revision != 1 || store.quarantine != "ambiguous_result" {
			t.Fatalf("invalid refresh wrote credentials or lost quarantine: rev=%d reason=%s", store.credentials["a"].Revision, store.quarantine)
		}
	}
}

func (s *authCoordinatorTestStore) ClaimAuthSession(context.Context, AuthSession) error {
	return s.claimErr
}

func TestAuthCoordinatorContinueClaimFailureSuppressesProviderCall(t *testing.T) {
	connector := &authCoordinatorTestConnector{}
	c, store := newAuthCoordinatorTest(t, connector, "a")
	session, err := c.Start(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	store.claimErr = errors.New("controlled claim storage failure")
	if _, err := c.Continue(context.Background(), session.ID); !errors.Is(err, ErrAuthPersistence) {
		t.Fatalf("claim error=%v", err)
	}
	if connector.calls != 1 {
		t.Fatalf("failed durable claim invoked continuation: calls=%d", connector.calls)
	}
}
