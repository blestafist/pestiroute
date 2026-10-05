package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/connector/codex"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

type codexAuthServices func(core.AttemptScope) core.InvocationServices

func (f codexAuthServices) ForAttempt(scope core.AttemptScope) core.InvocationServices {
	return f(scope)
}

type codexAuthDoer func(*http.Request) (*http.Response, error)

func (f codexAuthDoer) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func (f codexAuthDoer) Do(r *http.Request) (*http.Response, error)        { return f(r) }

type observedAuthStore struct {
	core.AuthCoordinatorStore
	credentialReads  chan struct{}
	onCredentialRead func()
}

func (s *observedAuthStore) AuthCredentials(ctx context.Context, accountID string) (core.AuthCredentials, error) {
	credentials, err := s.AuthCoordinatorStore.AuthCredentials(ctx, accountID)
	s.credentialReads <- struct{}{}
	if s.onCredentialRead != nil {
		s.onCredentialRead()
	}
	return credentials, err
}

type lockWaitContext struct {
	context.Context
	armed   atomic.Bool
	waiting chan struct{}
	once    sync.Once
}

func (c *lockWaitContext) Done() <-chan struct{} {
	if c.armed.Load() {
		c.once.Do(func() { close(c.waiting) })
	}
	return c.Context.Done()
}

func TestCodexConcurrentRefresh(t *testing.T) {
	t.Run("same account coalesces and cancelled waiters do not exchange", func(t *testing.T) {
		started, release := make(chan struct{}, 1), make(chan struct{})
		var calls atomic.Int32
		transport := codexAuthDoer(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			started <- struct{}{}
			<-release
			return codexRefreshResponse(r, t)
		})
		coordinator, dbPath, store := newCodexRefreshRuntime(t, transport, "account-a")
		results := make(chan core.AuthCredentials, 20)
		errs := make(chan error, 20)
		go func() {
			value, err := coordinator.ResolveFreshCredentials(context.Background(), "account-a", time.Minute)
			results <- value
			errs <- err
		}()
		select {
		case <-started:
		case err := <-errs:
			t.Fatalf("initial refresh failed before transport: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("initial refresh did not reach transport")
		}
		<-store.credentialReads
		<-store.credentialReads
		baseCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		waitCtx := &lockWaitContext{Context: baseCtx, waiting: make(chan struct{})}
		store.onCredentialRead = func() { waitCtx.armed.Store(true) }
		waitResult := make(chan error, 1)
		go func() {
			_, err := coordinator.ResolveFreshCredentials(waitCtx, "account-a", time.Minute)
			waitResult <- err
		}()
		select {
		case <-waitCtx.waiting:
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled waiter did not reach account-lock wait")
		}
		cancel()
		if err := <-waitResult; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled lock waiter error=%v", err)
		}
		<-store.credentialReads // discard the cancelled waiter's initial stale read
		assertRefreshMarkerCount(t, dbPath, 1)
		for range 19 {
			go func() {
				value, err := coordinator.ResolveFreshCredentials(context.Background(), "account-a", time.Minute)
				results <- value
				errs <- err
			}()
		}
		for range 19 {
			select {
			case <-store.credentialReads:
			case <-time.After(5 * time.Second):
				t.Fatal("same-account waiter did not read the stale generation")
			}
		}
		close(release)
		var revision int64
		for range 20 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
			got := <-results
			if revision == 0 {
				revision = got.Revision
			}
			if got.Revision != revision || got.Revision != 2 {
				t.Fatalf("different acknowledged generation: %#v", got)
			}
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("exchange calls=%d, want 1", got)
		}
		assertRefreshMarkerCount(t, dbPath, 0)
	})

	t.Run("accounts progress independently and retain scoped credentials", func(t *testing.T) {
		stalled, release := make(chan struct{}, 1), make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		var callsA, callsB atomic.Int32
		transport := codexAuthDoer(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(body))
			accountID := ""
			switch form.Get("refresh_token") {
			case "refresh-account-a":
				accountID = "account-a"
				callsA.Add(1)
				stalled <- struct{}{}
				<-release
			case "refresh-account-b":
				accountID = "account-b"
				callsB.Add(1)
			default:
				t.Errorf("unexpected scoped refresh token %q", form.Get("refresh_token"))
			}
			return codexRefreshResponseToken(r, t, "synthetic-"+accountID+"-access")
		})
		coordinator, _, _ := newCodexRefreshRuntime(t, transport, "account-a", "account-b")
		type refreshResult struct {
			credentials core.AuthCredentials
			err         error
		}
		aDone := make(chan refreshResult, 1)
		go func() {
			credentials, err := coordinator.ResolveFreshCredentials(context.Background(), "account-a", time.Minute)
			aDone <- refreshResult{credentials, err}
		}()
		<-stalled
		bCredentials, err := coordinator.ResolveFreshCredentials(context.Background(), "account-b", time.Minute)
		if err != nil {
			t.Fatalf("account b blocked by a: %v", err)
		}
		var bBundle map[string]any
		if err = json.Unmarshal(bCredentials.Values["oauth"], &bBundle); err != nil || bBundle["account_id"] != "account-b" || bBundle["access_token"] != "synthetic-account-b-access" {
			t.Fatalf("account b received another account's refresh result: bundle=%v err=%v", bBundle, err)
		}
		if callsB.Load() != 1 {
			t.Fatalf("account b exchange calls=%d", callsB.Load())
		}
		releaseOnce.Do(func() { close(release) })
		aResult := <-aDone
		if aResult.err != nil {
			t.Fatal(aResult.err)
		}
		var aBundle map[string]any
		if err := json.Unmarshal(aResult.credentials.Values["oauth"], &aBundle); err != nil || aBundle["account_id"] != "account-a" || aBundle["access_token"] != "synthetic-account-a-access" {
			t.Fatalf("account a received another account's refresh result: bundle=%v err=%v", aBundle, err)
		}
		if callsA.Load() != 1 {
			t.Fatalf("account a exchange calls=%d", callsA.Load())
		}
	})

	t.Run("independent database handles contend through durable marker", func(t *testing.T) {
		started, release := make(chan struct{}, 1), make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		var calls atomic.Int32
		transport := codexAuthDoer(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			started <- struct{}{}
			<-release
			return codexRefreshResponse(r, t)
		})
		first, dbPath, firstStore := newCodexRefreshRuntime(t, transport, "account-a")
		second := newCodexRefreshCoordinatorOnDB(t, dbPath, transport, "account-a")
		firstResult := make(chan error, 1)
		go func() {
			credentials, err := first.ResolveFreshCredentials(context.Background(), "account-a", time.Minute)
			if err == nil && credentials.Revision != 2 {
				err = errors.New("first coordinator did not receive revision 2")
			}
			firstResult <- err
		}()
		<-started
		<-firstStore.credentialReads
		<-firstStore.credentialReads
		secondResult := make(chan error, 1)
		go func() {
			_, err := second.ResolveFreshCredentials(context.Background(), "account-a", time.Minute)
			secondResult <- err
		}()
		if err := <-secondResult; !errors.Is(err, core.ErrRefreshMarkerConflict) {
			t.Fatalf("independent coordinator marker contention error=%v", err)
		}
		releaseOnce.Do(func() { close(release) })
		if err := <-firstResult; err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 1 {
			t.Fatalf("independent coordinators exchange calls=%d, want 1", calls.Load())
		}
		assertRefreshMarkerCount(t, dbPath, 0)
	})

	t.Run("external CAS wins without retry or overwrite", func(t *testing.T) {
		started, release := make(chan struct{}, 1), make(chan struct{})
		var calls atomic.Int32
		transport := codexAuthDoer(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			started <- struct{}{}
			<-release
			return codexRefreshResponse(r, t)
		})
		coordinator, databases, _ := newCodexRefreshRuntime(t, transport, "account-a")
		result := make(chan error, 1)
		go func() {
			_, err := coordinator.ResolveFreshCredentials(context.Background(), "account-a", time.Minute)
			result <- err
		}()
		<-started
		db, err := sqlite.Open(databases)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		store := sqlite.NewCredentials(db)
		row, err := store.Get(context.Background(), "account-a", "credential-account-a")
		if err != nil {
			t.Fatal(err)
		}
		key, err := secure.LoadMasterKey(filepath.Join(filepath.Dir(databases), "master.key"))
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := secure.Seal(key, 1, "key-v1", "credentials", row.ID, row.AccountID, []byte(`{"external":"newer"}`))
		if err != nil {
			t.Fatal(err)
		}
		row.FormatVersion, row.KeyVersion, row.Nonce, row.Ciphertext = envelope.FormatVersion, envelope.KeyVersion, envelope.Nonce, envelope.Ciphertext
		if _, err = store.Update(context.Background(), row); err != nil {
			t.Fatal(err)
		}
		close(release)
		if err = <-result; !errors.Is(err, core.ErrAuthRevisionMismatch) && !errors.Is(err, core.ErrAuthPersistence) {
			t.Fatalf("stale refresh error=%v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("exchange calls=%d", calls.Load())
		}
		stored, err := store.GetDecrypted(context.Background(), "account-a", row.ID, key)
		if err != nil || string(stored) != `{"external":"newer"}` {
			t.Fatalf("external credential overwritten: %q err=%v", stored, err)
		}
		var lifecycle, reason string
		if err = db.QueryRow(`SELECT lifecycle,quarantine_reason FROM auth_sessions WHERE account_id=? AND kind='refresh'`, "account-a").Scan(&lifecycle, &reason); err != nil {
			t.Fatal(err)
		}
		if lifecycle != "uncertain" || reason != "ambiguous_result" {
			t.Fatalf("external CAS refresh marker lifecycle=%q reason=%q", lifecycle, reason)
		}
	})
}

func assertRefreshMarkerCount(t *testing.T, dbPath string, want int) {
	t.Helper()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM auth_sessions WHERE account_id=? AND kind='refresh'`, "account-a").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("refresh marker count=%d, want %d", count, want)
	}
}

func codexRefreshResponse(r *http.Request, t *testing.T) (*http.Response, error) {
	return codexRefreshResponseToken(r, t, "synthetic-access-not-jwt")
}

func codexRefreshResponseToken(r *http.Request, t *testing.T, accessToken string) (*http.Response, error) {
	t.Helper()
	body, _ := io.ReadAll(r.Body)
	if len(body) > 0 {
		form, err := url.ParseQuery(string(body))
		if err != nil || r.Method != http.MethodPost || form.Get("grant_type") != "refresh_token" {
			t.Errorf("invalid Codex refresh request")
		}
	}
	response, _ := json.Marshal(map[string]any{"access_token": accessToken, "expires_in": 3600, "token_type": "Bearer"})
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(stringsReader(string(response))), Request: r}, nil
}

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }

func newCodexRefreshRuntime(t *testing.T, transport core.HTTPDoer, accountIDs ...string) (*core.AuthCoordinator, string, *observedAuthStore) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "auth.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	accounts, credentials, sessions := sqlite.NewAccounts(db), sqlite.NewCredentials(db), sqlite.NewAuthSessions(db)
	keyPath := filepath.Join(dir, "master.key")
	if err = os.WriteFile(keyPath, []byte("01234567890123456789012345678901"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	refs := make(map[string]map[string]string)
	registry, err := core.NewRegistry(map[core.ComponentKind]core.APIVersion{core.ComponentConnector: {Major: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, accountID := range accountIDs {
		if _, err = accounts.Create(ctx, sqlite.Account{ID: accountID, Connector: "codex-" + accountID, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		plain, _ := json.Marshal(map[string]any{"version": 1, "access_token": "expired-access", "refresh_token": "refresh-" + accountID, "account_id": accountID, "expires_at": time.Now().Add(-time.Hour)})
		envelope, sealErr := secure.Seal(key, 1, "key-v1", "credentials", "credential-"+accountID, accountID, plain)
		if sealErr != nil {
			t.Fatal(sealErr)
		}
		stale := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
		if _, err = credentials.Create(ctx, sqlite.Credential{ID: "credential-" + accountID, AccountID: accountID, FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext, ExpiresAt: &stale}); err != nil {
			t.Fatal(err)
		}
		connector := codex.NewConnector()
		config, _ := json.Marshal(map[string]string{"model": "model", "account_id": accountID, "profile": "codex-responses-http-sse-v1"})
		if err = registry.Register(core.InstanceID("codex-"+accountID), connector, core.ComponentConnector); err != nil {
			t.Fatal(err)
		}
		if err = registry.Init(ctx, core.InstanceID("codex-"+accountID), core.ComponentConfig{Data: config}); err != nil {
			t.Fatal(err)
		}
		if health := registry.Health(ctx, core.InstanceID("codex-"+accountID)); health.State != core.HealthReady {
			t.Fatalf("Codex connector health=%+v", health)
		}
		refs[accountID] = map[string]string{"oauth": "credential-" + accountID}
	}
	store, err := newSQLiteAuthCoordinatorStore(accounts, credentials, sessions, key, "key-v1", refs)
	if err != nil {
		t.Fatal(err)
	}
	observedStore := &observedAuthStore{AuthCoordinatorStore: store, credentialReads: make(chan struct{}, 256)}
	coordinator, err := core.NewAuthCoordinator(registry, observedStore, codexAuthServices(func(core.AttemptScope) core.InvocationServices { return core.InvocationServices{Transport: transport} }), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator, dbPath, observedStore
}

func newCodexRefreshCoordinatorOnDB(t *testing.T, dbPath string, transport core.HTTPDoer, accountID string) *core.AuthCoordinator {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	key, err := secure.LoadMasterKey(filepath.Join(filepath.Dir(dbPath), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	accounts, credentials, sessions := sqlite.NewAccounts(db), sqlite.NewCredentials(db), sqlite.NewAuthSessions(db)
	connector := codex.NewConnector()
	config, _ := json.Marshal(map[string]string{"model": "model", "account_id": accountID, "profile": "codex-responses-http-sse-v1"})
	registry, err := core.NewRegistry(map[core.ComponentKind]core.APIVersion{core.ComponentConnector: {Major: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	instanceID := core.InstanceID("codex-" + accountID)
	if err = registry.Register(instanceID, connector, core.ComponentConnector); err != nil {
		t.Fatal(err)
	}
	if err = registry.Init(ctx, instanceID, core.ComponentConfig{Data: config}); err != nil {
		t.Fatal(err)
	}
	store, err := newSQLiteAuthCoordinatorStore(accounts, credentials, sessions, key, "key-v1", map[string]map[string]string{accountID: {"oauth": "credential-" + accountID}})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := core.NewAuthCoordinator(registry, store, codexAuthServices(func(core.AttemptScope) core.InvocationServices {
		return core.InvocationServices{Transport: transport}
	}), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}
