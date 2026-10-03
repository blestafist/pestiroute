package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

// sqliteAuthCoordinatorStore is composition glue: Core owns the interface and
// the adapter maps its provider-neutral operations onto encrypted SQLite repos.
type sqliteAuthCoordinatorStore struct {
	accounts    *sqlite.Accounts
	credentials *sqlite.Credentials
	sessions    *sqlite.AuthSessions
	key         crypto.MasterKey
	keyVersion  string
	refs        map[string]map[string]string
	clock       func() time.Time
}

func newSQLiteAuthCoordinatorStore(accounts *sqlite.Accounts, credentials *sqlite.Credentials, sessions *sqlite.AuthSessions, key crypto.MasterKey, keyVersion string, refs map[string]map[string]string) (*sqliteAuthCoordinatorStore, error) {
	if accounts == nil || credentials == nil || sessions == nil || keyVersion == "" {
		return nil, fmt.Errorf("authentication storage dependencies required")
	}
	copyRefs := make(map[string]map[string]string, len(refs))
	for account, values := range refs {
		copyRefs[account] = make(map[string]string, len(values))
		maps.Copy(copyRefs[account], values)
	}
	return &sqliteAuthCoordinatorStore{accounts: accounts, credentials: credentials, sessions: sessions, key: key, keyVersion: keyVersion, refs: copyRefs, clock: time.Now}, nil
}

func (s *sqliteAuthCoordinatorStore) AuthAccount(ctx context.Context, id string) (core.AuthAccount, error) {
	a, err := s.accounts.Get(ctx, id)
	if err != nil {
		return core.AuthAccount{}, core.ErrAccountUnavailable
	}
	return core.AuthAccount{ID: a.ID, ConnectorID: core.InstanceID(a.Connector), Enabled: a.Enabled}, nil
}
func (s *sqliteAuthCoordinatorStore) AuthCredentials(ctx context.Context, account string) (core.AuthCredentials, error) {
	ids := s.refs[account]
	if len(ids) == 0 {
		return core.AuthCredentials{}, core.ErrCredentialUnavailable
	}
	values := make(map[string][]byte, len(ids))
	var revision int64
	valid := true
	for name, id := range ids {
		row, err := s.credentials.Get(ctx, account, id)
		if err != nil {
			return core.AuthCredentials{}, core.ErrCredentialUnavailable
		}
		if revision != 0 && revision != row.Revision {
			return core.AuthCredentials{}, core.ErrAuthRevisionMismatch
		}
		revision = row.Revision
		plain, err := crypto.Open(s.key, crypto.Envelope{FormatVersion: row.FormatVersion, KeyVersion: row.KeyVersion, Nonce: row.Nonce, Ciphertext: row.Ciphertext}, "credentials", row.ID, row.AccountID)
		if err != nil {
			return core.AuthCredentials{}, core.ErrCredentialUnavailable
		}
		values[name] = plain
		if len(plain) == 0 || (row.ExpiresAt != nil && !row.ExpiresAt.After(s.clock())) {
			valid = false
		}
	}
	return core.AuthCredentials{Revision: revision, Values: values, Valid: valid}, nil
}
func (s *sqliteAuthCoordinatorStore) CreateAuthSession(ctx context.Context, v core.AuthSession, state []byte) error {
	_, err := s.sessions.CreateInteractiveSession(ctx, v.ID, v.AccountID, string(v.ConnectorID), v.Revision, v.ExpiresAt, state, s.key, s.keyVersion, s.clock())
	return err
}
func (s *sqliteAuthCoordinatorStore) GetAuthSession(ctx context.Context, id string, now time.Time) (core.AuthSession, []byte, error) {
	v, state, err := s.sessions.GetInteractiveSessionDecrypted(ctx, id, s.key, now)
	if err != nil {
		return core.AuthSession{}, nil, core.ErrAuthUnavailable
	}
	return core.AuthSession{ID: v.ID, AccountID: v.AccountID, ConnectorID: core.InstanceID(v.Connector), Revision: v.ExpectedCredentialRevision, ExpiresAt: v.ExpiresAt, ContinuationVersion: hex.EncodeToString(v.Nonce)}, state, nil
}
func (s *sqliteAuthCoordinatorStore) AdvanceAuthSession(ctx context.Context, v core.AuthSession, state []byte) error {
	err := s.sessions.AdvanceInteractiveSession(ctx, v.ID, v.AccountID, string(v.ConnectorID), v.Revision, v.ExpiresAt, state, s.key, s.keyVersion, s.clock())
	return mapAuthStoreError(err)
}
func (s *sqliteAuthCoordinatorStore) ConsumeAuthSession(ctx context.Context, v core.AuthSession) error {
	return s.sessions.ConsumeInteractiveSession(ctx, v.ID, v.AccountID, s.clock())
}
func (s *sqliteAuthCoordinatorStore) FinishAuthSession(ctx context.Context, v core.AuthSession, candidates map[string][]byte) error {
	var replacement *sqlite.Credential
	if len(candidates) > 0 {
		row, err := s.credentialReplacement(ctx, v.AccountID, v.Revision, candidates)
		if err != nil {
			return err
		}
		replacement = &row
	}
	err := s.sessions.FinishInteractiveSession(ctx, v.ID, v.AccountID, string(v.ConnectorID), v.Revision, replacement, s.clock())
	return mapAuthStoreError(err)
}
func (s *sqliteAuthCoordinatorStore) CreateRefreshMarker(ctx context.Context, id string, v core.AuthSession) error {
	err := s.sessions.CreateRefreshMarker(ctx, id, v.AccountID, string(v.ConnectorID), v.Revision, s.clock())
	if errors.Is(err, sqlite.ErrRefreshMarkerConflict) {
		return core.ErrRefreshMarkerConflict
	}
	return err
}
func (s *sqliteAuthCoordinatorStore) ClearRefreshMarker(ctx context.Context, id string, v core.AuthSession) error {
	return s.sessions.ClearRefreshMarker(ctx, id, v.AccountID)
}
func (s *sqliteAuthCoordinatorStore) QuarantineRefreshMarker(ctx context.Context, id string, v core.AuthSession, reason string, revision int64) error {
	return s.sessions.QuarantineRefreshMarker(ctx, id, v.AccountID, reason, revision, s.clock())
}
func (s *sqliteAuthCoordinatorStore) ResolveRefresh(ctx context.Context, id string, v core.AuthSession, candidates core.AuthCredentials) (core.AuthCredentials, error) {
	replacement, err := s.credentialReplacement(ctx, v.AccountID, v.Revision, candidates.Values)
	if err != nil {
		return core.AuthCredentials{}, err
	}
	updated, err := s.sessions.ResolveRefreshAndReplaceCredentials(ctx, id, replacement, s.clock())
	if err != nil {
		return core.AuthCredentials{}, mapAuthStoreError(err)
	}
	return core.AuthCredentials{Revision: updated.Revision, Values: cloneAuthValues(candidates.Values), Valid: true}, nil
}
func (s *sqliteAuthCoordinatorStore) ReplaceAuthCredentials(ctx context.Context, account string, revision int64, candidates core.AuthCredentials) (core.AuthCredentials, error) {
	replacement, err := s.credentialReplacement(ctx, account, revision, candidates.Values)
	if err != nil {
		return core.AuthCredentials{}, err
	}
	updated, err := s.sessions.ReplaceCredentialsAndResolveUncertain(ctx, replacement, s.clock())
	if err != nil {
		return core.AuthCredentials{}, mapAuthStoreError(err)
	}
	return core.AuthCredentials{Revision: updated.Revision, Values: cloneAuthValues(candidates.Values), Valid: true}, nil
}
func (s *sqliteAuthCoordinatorStore) InvalidateAuth(ctx context.Context, account string, connector core.InstanceID, now time.Time) error {
	return s.sessions.Invalidate(ctx, account, string(connector), now)
}

func (s *sqliteAuthCoordinatorStore) credentialReplacement(ctx context.Context, account string, revision int64, candidates map[string][]byte) (sqlite.Credential, error) {
	refs := s.refs[account]
	if len(refs) != 1 || len(candidates) != 1 {
		return sqlite.Credential{}, core.ErrAuthUnavailable
	}
	var name, id string
	for n, v := range refs {
		name, id = n, v
	}
	plain, ok := candidates[name]
	if !ok || len(plain) == 0 {
		return sqlite.Credential{}, core.ErrAuthUnavailable
	}
	old, err := s.credentials.Get(ctx, account, id)
	if err != nil {
		return sqlite.Credential{}, core.ErrCredentialUnavailable
	}
	if old.Revision != revision {
		return sqlite.Credential{}, core.ErrAuthRevisionMismatch
	}
	envelope, err := crypto.Seal(s.key, 1, s.keyVersion, "credentials", id, account, plain)
	if err != nil {
		return sqlite.Credential{}, core.ErrAuthPersistence
	}
	old.FormatVersion, old.KeyVersion, old.Nonce, old.Ciphertext = envelope.FormatVersion, envelope.KeyVersion, envelope.Nonce, envelope.Ciphertext
	old.ExpiresAt = nil
	return old, nil
}
func cloneAuthValues(in map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(in))
	for k, v := range in {
		out[k] = append([]byte(nil), v...)
	}
	return out
}
func mapAuthStoreError(err error) error {
	if errors.Is(err, sqlite.ErrRevisionMismatch) {
		return core.ErrAuthRevisionMismatch
	}
	if errors.Is(err, sqlite.ErrRefreshMarkerConflict) {
		return core.ErrRefreshMarkerConflict
	}
	return err
}

var _ core.AuthCoordinatorStore = (*sqliteAuthCoordinatorStore)(nil)

func (s *sqliteAuthCoordinatorStore) ClaimAuthSession(ctx context.Context, v core.AuthSession) error {
	nonce, err := hex.DecodeString(v.ContinuationVersion)
	if err != nil || len(nonce) == 0 {
		return core.ErrAuthUnavailable
	}
	err = s.sessions.ClaimInteractiveSession(ctx, v.ID, v.AccountID, string(v.ConnectorID), v.Revision, nonce, s.clock())
	if errors.Is(err, sqlite.ErrAuthSessionUnavailable) {
		return core.ErrAuthUnavailable
	}
	return mapAuthStoreError(err)
}
