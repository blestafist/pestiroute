package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sync"
	"time"
)

var (
	ErrAuthUnavailable       = errors.New("authentication unavailable")
	ErrAuthPersistence       = errors.New("authentication persistence failed")
	ErrAuthRevisionMismatch  = errors.New("authentication credential revision mismatch")
	ErrRefreshMarkerConflict = errors.New("refresh marker conflict")
)

type AuthAccount struct {
	ID          string
	ConnectorID InstanceID
	Enabled     bool
}

type AuthCredentials struct {
	Revision  int64
	Values    map[string][]byte
	ExpiresAt *time.Time
	Valid     bool
}

type AuthSession struct {
	ID, AccountID string
	ConnectorID   InstanceID
	Revision      int64
	ExpiresAt     time.Time
	// ContinuationVersion is an opaque store-owned CAS token for the loaded state.
	ContinuationVersion string
	// UserAction is transient safe presentation data; it is never persisted.
	UserAction *AuthUserAction
}

// AuthCoordinatorStore is the runtime persistence seam. Implementations protect
// opaque state and candidate credentials at rest and make each named mutation atomic.
type AuthCoordinatorStore interface {
	AuthAccount(context.Context, string) (AuthAccount, error)
	AuthCredentials(context.Context, string) (AuthCredentials, error)
	HasQuarantinedRefresh(context.Context, string) (bool, error)
	CreateAuthSession(context.Context, AuthSession, []byte) error
	GetAuthSession(context.Context, string, time.Time) (AuthSession, []byte, error)
	// ClaimAuthSession durably excludes another continuation before provider exchange.
	ClaimAuthSession(context.Context, AuthSession) error
	AdvanceAuthSession(context.Context, AuthSession, []byte) error
	ConsumeAuthSession(context.Context, AuthSession) error
	FinishAuthSession(context.Context, AuthSession, AuthCredentials) error
	// CreateRefreshMarker maps storage uniqueness collisions to ErrRefreshMarkerConflict.
	CreateRefreshMarker(context.Context, string, AuthSession) error
	ClearRefreshMarker(context.Context, string, AuthSession) error
	QuarantineRefreshMarker(context.Context, string, AuthSession, string, int64) error
	ResolveRefresh(context.Context, string, AuthSession, AuthCredentials) (AuthCredentials, error)
	// ReplaceAuthCredentials performs revision CAS and resolves quarantine atomically.
	ReplaceAuthCredentials(context.Context, string, int64, AuthCredentials) (AuthCredentials, error)
	InvalidateAuth(context.Context, string, InstanceID, time.Time) error
}

type AuthServicesFactory interface {
	ForAttempt(AttemptScope) InvocationServices
}

type AuthCoordinator struct {
	registry   *Registry
	store      AuthCoordinatorStore
	services   AuthServicesFactory
	clock      func() time.Time
	random     io.Reader
	sessionTTL time.Duration
	locksMu    sync.Mutex
	locks      map[string]chan struct{}
}

type authCredentialAccess struct{ values map[string][]byte }

func (a authCredentialAccess) Get(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, ok := a.values[name]
	if !ok {
		return nil, ErrCredentialUnavailable
	}
	return append([]byte(nil), value...), nil
}

func NewAuthCoordinator(registry *Registry, store AuthCoordinatorStore, services AuthServicesFactory, sessionTTL time.Duration) (*AuthCoordinator, error) {
	if registry == nil || store == nil || services == nil || sessionTTL <= 0 {
		return nil, fmt.Errorf("authentication coordinator dependencies and positive session TTL are required")
	}
	return &AuthCoordinator{registry: registry, store: store, services: services, clock: time.Now, random: rand.Reader, sessionTTL: sessionTTL, locks: make(map[string]chan struct{})}, nil
}

func (c *AuthCoordinator) Start(ctx context.Context, accountID string) (AuthSession, error) {
	account, connector, services, creds, err := c.prepare(ctx, accountID)
	if err != nil {
		return AuthSession{}, err
	}
	result, callErr := connector.Authenticate(ctx, AuthRequest{AccountID: account.ID, Action: "start"}, services)
	if callErr != nil || !result.Supported || !validNextAction(result.NextAction) {
		return AuthSession{}, authCallError(ctx, callErr)
	}
	if result.UserAction != nil && !validAuthUserAction(result) {
		return AuthSession{}, ErrAuthUnavailable
	}
	if result.NextAction == "" {
		if len(result.Credentials) > 0 {
			if !validCredentialExpiry(result.CredentialExpiresAt, c.clock()) {
				return AuthSession{}, ErrAuthUnavailable
			}
			if _, err := c.replaceCredentials(ctx, account.ID, creds, result.Credentials, result.CredentialExpiresAt); err != nil {
				return AuthSession{}, err
			}
		}
		return AuthSession{}, nil
	}
	session, err := c.newSession(account, creds.Revision)
	if err != nil {
		return AuthSession{}, err
	}
	if err := c.store.CreateAuthSession(ctx, session, []byte(result.State)); err != nil {
		return AuthSession{}, authPersistence(err)
	}
	session.UserAction = cloneAuthUserAction(result.UserAction)
	return session, nil
}

func (c *AuthCoordinator) Continue(ctx context.Context, sessionID string) (AuthSession, error) {
	session, state, err := c.store.GetAuthSession(ctx, sessionID, c.clock())
	if err != nil {
		return AuthSession{}, ErrAuthUnavailable
	}
	unlock, err := c.lockAccount(ctx, session.AccountID)
	if err != nil {
		clear(state)
		if c.discardSession(session) != nil {
			return AuthSession{}, ErrAuthPersistence
		}
		return AuthSession{}, err
	}
	defer unlock()
	// Reload under the account lock: another continuation may have consumed or
	// advanced the state while this caller was waiting.
	clear(state)
	session, state, err = c.store.GetAuthSession(ctx, sessionID, c.clock())
	if err != nil {
		return AuthSession{}, ErrAuthUnavailable
	}
	defer clear(state)
	account, connector, services, creds, err := c.prepare(ctx, session.AccountID)
	if err != nil {
		if c.discardSession(session) != nil {
			return AuthSession{}, ErrAuthPersistence
		}
		return AuthSession{}, err
	}
	if account.ConnectorID != session.ConnectorID || creds.Revision != session.Revision {
		if c.discardSession(session) != nil {
			return AuthSession{}, ErrAuthPersistence
		}
		return AuthSession{}, ErrAuthRevisionMismatch
	}
	if err := ctx.Err(); err != nil {
		if c.discardSession(session) != nil {
			return AuthSession{}, ErrAuthPersistence
		}
		return AuthSession{}, err
	}
	if err := c.store.ClaimAuthSession(ctx, session); err != nil {
		if errors.Is(err, ErrAuthUnavailable) {
			return AuthSession{}, ErrAuthUnavailable
		}
		return AuthSession{}, authPersistence(err)
	}
	if err := ctx.Err(); err != nil {
		_ = c.discardSession(session)
		return AuthSession{}, err
	}
	result, callErr := connector.Authenticate(ctx, AuthRequest{AccountID: account.ID, Action: "continue", State: state}, services)
	if callErr != nil || !result.Supported || !validNextAction(result.NextAction) {
		if c.discardSession(session) != nil {
			return AuthSession{}, ErrAuthPersistence
		}
		return AuthSession{}, authCallError(ctx, callErr)
	}
	if result.UserAction != nil && !validAuthUserAction(result) {
		if c.discardSession(session) != nil {
			return AuthSession{}, ErrAuthPersistence
		}
		return AuthSession{}, ErrAuthUnavailable
	}
	if !validCredentialExpiry(result.CredentialExpiresAt, c.clock()) {
		if c.discardSession(session) != nil {
			return AuthSession{}, ErrAuthPersistence
		}
		return AuthSession{}, ErrAuthUnavailable
	}
	if result.NextAction == "" {
		if err := c.store.FinishAuthSession(ctx, session, AuthCredentials{Values: cloneSecretMap(result.Credentials), ExpiresAt: cloneTime(result.CredentialExpiresAt)}); err != nil {
			if errors.Is(err, ErrAuthRevisionMismatch) {
				_ = c.discardSession(session)
				return AuthSession{}, ErrAuthRevisionMismatch
			}
			_ = c.discardSession(session)
			return AuthSession{}, authPersistence(err)
		}
		return AuthSession{}, nil
	}
	if err := c.store.AdvanceAuthSession(ctx, session, []byte(result.State)); err != nil {
		if errors.Is(err, ErrAuthRevisionMismatch) {
			_ = c.discardSession(session)
			return AuthSession{}, ErrAuthRevisionMismatch
		}
		_ = c.discardSession(session)
		return AuthSession{}, authPersistence(err)
	}
	session.UserAction = cloneAuthUserAction(result.UserAction)
	return session, nil
}

func (c *AuthCoordinator) Refresh(ctx context.Context, accountID string) (AuthCredentials, error) {
	return c.resolveCredentials(ctx, accountID, 0, false)
}

// ResolveFreshCredentials reloads the selected account under its lock and
// refreshes credentials unless the persisted expiry exceeds the supplied margin.
func (c *AuthCoordinator) ResolveFreshCredentials(ctx context.Context, accountID string, margin time.Duration) (AuthCredentials, error) {
	if margin < 0 {
		return AuthCredentials{}, ErrAuthUnavailable
	}
	return c.resolveCredentials(ctx, accountID, margin, true)
}

func (c *AuthCoordinator) resolveCredentials(ctx context.Context, accountID string, margin time.Duration, requireFresh bool) (AuthCredentials, error) {
	account, err := c.store.AuthAccount(ctx, accountID)
	if err != nil || !account.Enabled || account.ID != accountID || account.ConnectorID == "" {
		return AuthCredentials{}, ErrAccountUnavailable
	}
	observed, err := c.store.AuthCredentials(ctx, accountID)
	if err != nil {
		return AuthCredentials{}, authPersistence(err)
	}
	unlock, err := c.lockAccount(ctx, accountID)
	if err != nil {
		return AuthCredentials{}, err
	}
	defer unlock()
	quarantined, err := c.store.HasQuarantinedRefresh(ctx, accountID)
	if err != nil {
		return AuthCredentials{}, authPersistence(err)
	}
	if quarantined {
		return AuthCredentials{}, ErrAuthUnavailable
	}
	account, connector, services, creds, err := c.prepare(ctx, accountID)
	if err != nil {
		return AuthCredentials{}, err
	}
	if (requireFresh && credentialFresh(creds, c.clock(), margin)) ||
		(!requireFresh && creds.Revision != observed.Revision && creds.Valid) {
		return cloneAuthCredentials(creds), nil
	}
	if err := ctx.Err(); err != nil {
		return AuthCredentials{}, err
	}
	marker, err := c.newSession(account, creds.Revision)
	if err != nil {
		return AuthCredentials{}, err
	}
	if err := c.store.CreateRefreshMarker(ctx, marker.ID, marker); err != nil {
		if errors.Is(err, ErrRefreshMarkerConflict) {
			return AuthCredentials{}, ErrRefreshMarkerConflict
		}
		return AuthCredentials{}, authPersistence(err)
	}
	if err := ctx.Err(); err != nil {
		clearErr := c.store.ClearRefreshMarker(context.Background(), marker.ID, marker)
		if clearErr != nil {
			return AuthCredentials{}, authPersistence(clearErr)
		}
		return AuthCredentials{}, err
	}
	services.Credentials = authCredentialAccess{values: cloneSecretMap(creds.Values)}
	if services.Logger != nil {
		secrets := newSecretSet(nil)
		for _, value := range creds.Values {
			secrets.add(value)
		}
		services.Logger = slog.New(redactingHandler{next: services.Logger.Handler(), secrets: secrets})
	}
	result, callErr := connector.Authenticate(ctx, AuthRequest{AccountID: account.ID, Action: "refresh"}, services)
	if callErr != nil || !result.Supported || result.NextAction != "" || result.State != "" || result.UserAction != nil || !validCredentialCandidates(result.Credentials) {
		reason := "ambiguous_result"
		if ctx.Err() != nil {
			reason = "cancelled_after_call"
		}
		if err := c.store.QuarantineRefreshMarker(context.Background(), marker.ID, marker, reason, creds.Revision); err != nil {
			return AuthCredentials{}, authPersistence(err)
		}
		return AuthCredentials{}, authCallError(ctx, callErr)
	}
	if !validCredentialExpiry(result.CredentialExpiresAt, c.clock()) {
		if err := c.store.QuarantineRefreshMarker(context.Background(), marker.ID, marker, "ambiguous_result", creds.Revision); err != nil {
			return AuthCredentials{}, authPersistence(err)
		}
		return AuthCredentials{}, ErrAuthUnavailable
	}
	if len(result.Credentials) == 0 {
		if err := c.store.QuarantineRefreshMarker(context.Background(), marker.ID, marker, "ambiguous_result", creds.Revision); err != nil {
			return AuthCredentials{}, authPersistence(err)
		}
		return AuthCredentials{}, ErrAuthUnavailable
	}
	replacement := AuthCredentials{Revision: creds.Revision, Values: cloneSecretMap(result.Credentials), ExpiresAt: cloneTime(result.CredentialExpiresAt)}
	resolved, err := c.store.ResolveRefresh(ctx, marker.ID, marker, replacement)
	if err != nil {
		reason := "persistence_failed"
		if errors.Is(err, ErrAuthRevisionMismatch) {
			reason = "ambiguous_result"
		}
		if quarantineErr := c.store.QuarantineRefreshMarker(context.Background(), marker.ID, marker, reason, creds.Revision); quarantineErr != nil {
			return AuthCredentials{}, authPersistence(errors.Join(err, quarantineErr))
		}
		return AuthCredentials{}, authPersistence(err)
	}
	return cloneAuthCredentials(resolved), nil
}

func (c *AuthCoordinator) Invalidate(ctx context.Context, accountID string, connectorID InstanceID) error {
	return c.store.InvalidateAuth(ctx, accountID, connectorID, c.clock())
}

func (c *AuthCoordinator) discardSession(session AuthSession) error {
	if err := c.store.ConsumeAuthSession(context.Background(), session); err != nil {
		return authPersistence(err)
	}
	return nil
}

func (c *AuthCoordinator) prepare(ctx context.Context, accountID string) (AuthAccount, Connector, InvocationServices, AuthCredentials, error) {
	if accountID == "" {
		return AuthAccount{}, nil, InvocationServices{}, AuthCredentials{}, ErrAccountUnavailable
	}
	account, err := c.store.AuthAccount(ctx, accountID)
	if err != nil || !account.Enabled || account.ID != accountID || account.ConnectorID == "" {
		return AuthAccount{}, nil, InvocationServices{}, AuthCredentials{}, ErrAccountUnavailable
	}
	component, _, ready := c.registry.Admit(ctx, account.ConnectorID)
	connector, ok := component.(Connector)
	if !ready || !ok {
		return AuthAccount{}, nil, InvocationServices{}, AuthCredentials{}, ErrAuthUnavailable
	}
	creds, err := c.store.AuthCredentials(ctx, accountID)
	if err != nil {
		return AuthAccount{}, nil, InvocationServices{}, AuthCredentials{}, authPersistence(err)
	}
	return account, connector, c.services.ForAttempt(AttemptScope{AccountID: accountID}), cloneAuthCredentials(creds), nil
}

func (c *AuthCoordinator) replaceCredentials(ctx context.Context, accountID string, before AuthCredentials, candidates map[string][]byte, expiresAt *time.Time) (AuthCredentials, error) {
	updated, err := c.store.ReplaceAuthCredentials(ctx, accountID, before.Revision, AuthCredentials{Revision: before.Revision, Values: cloneSecretMap(candidates), ExpiresAt: cloneTime(expiresAt)})
	if err != nil {
		if errors.Is(err, ErrAuthRevisionMismatch) {
			return AuthCredentials{}, ErrAuthRevisionMismatch
		}
		return AuthCredentials{}, authPersistence(err)
	}
	return updated, nil
}

func (c *AuthCoordinator) newSession(account AuthAccount, revision int64) (AuthSession, error) {
	var bytes [32]byte
	if _, err := io.ReadFull(c.random, bytes[:]); err != nil {
		return AuthSession{}, fmt.Errorf("generate auth session id: %w", err)
	}
	now := c.clock().UTC().Truncate(time.Millisecond)
	return AuthSession{ID: hex.EncodeToString(bytes[:]), AccountID: account.ID, ConnectorID: account.ConnectorID, Revision: revision, ExpiresAt: now.Add(c.sessionTTL).Truncate(time.Millisecond)}, nil
}

func (c *AuthCoordinator) lockAccount(ctx context.Context, id string) (func(), error) {
	c.locksMu.Lock()
	lock := c.locks[id]
	if lock == nil {
		lock = make(chan struct{}, 1)
		c.locks[id] = lock
	}
	c.locksMu.Unlock()
	select {
	case lock <- struct{}{}:
		return func() { <-lock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func validNextAction(action string) bool { return action == "" || action == "continue" }
func validAuthUserAction(result AuthResult) bool {
	action := result.UserAction
	if action == nil || !result.Supported || result.NextAction != "continue" || action.PollInterval <= 0 ||
		!printableASCII(action.VerificationURI, 1, 2048, false) || !printableASCII(action.UserCode, 1, 256, true) {
		return false
	}
	u, err := url.Parse(action.VerificationURI)
	return err == nil && u.IsAbs() && u.Scheme == "https" && u.Host != "" && u.User == nil
}
func printableASCII(value string, min, max int, spaces bool) bool {
	if len(value) < min || len(value) > max {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e || !spaces && value[i] == ' ' {
			return false
		}
	}
	return true
}
func cloneAuthUserAction(value *AuthUserAction) *AuthUserAction {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func authCallError(ctx context.Context, _ *GatewayError) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return ErrAuthUnavailable
}
func authPersistence(error) error { return ErrAuthPersistence }
func cloneSecretMap(in map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(in))
	for k, v := range in {
		out[k] = append([]byte(nil), v...)
	}
	return out
}
func cloneAuthCredentials(in AuthCredentials) AuthCredentials {
	in.Values = cloneSecretMap(in.Values)
	in.ExpiresAt = cloneTime(in.ExpiresAt)
	return in
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func validCredentialExpiry(value *time.Time, now time.Time) bool {
	return value == nil || (!value.IsZero() && value.After(now))
}
func credentialFresh(credentials AuthCredentials, now time.Time, margin time.Duration) bool {
	if !credentials.Valid || credentials.ExpiresAt == nil {
		return false
	}
	return credentials.ExpiresAt.After(now.Add(margin))
}
func clear(values []byte) {
	for i := range values {
		values[i] = 0
	}
}

func validCredentialCandidates(values map[string][]byte) bool {
	if len(values) == 0 {
		return false
	}
	for name, value := range values {
		if name == "" || len(value) == 0 {
			return false
		}
	}
	return true
}
