package sqlite

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestReviewedSchemaV5UpgradePreservesAuthentication(t *testing.T) {
	_, key := credentialFixture(t, filepath.Join(t.TempDir(), "key-fixture.db"))
	db := openTestDB(t, filepath.Join(t.TempDir(), "v5.db"))
	ctx := context.Background()
	if err := migrate(ctx, db, schemaMigrations[:5]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO accounts(id,connector,created_at,updated_at) VALUES ('account','test',1,1)`); err != nil {
		t.Fatal(err)
	}
	credentials := NewCredentials(db)
	credential, err := credentials.Create(ctx, sealCredential(t, key, "credential", "account", []byte("old-token")))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sessions := NewAuthSessions(db)
	created, err := sessions.CreateInteractiveSession(ctx, "session", "account", "test", 1, now.Add(time.Minute), []byte("state"), key, "v1", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	loaded, state, err := sessions.GetInteractiveSessionDecrypted(ctx, "session", key, now)
	if err != nil || string(state) != "state" || !bytes.Equal(loaded.Nonce, created.Nonce) || !bytes.Equal(loaded.Ciphertext, created.Ciphertext) {
		t.Fatalf("upgrade altered active session: %v", err)
	}
	got, err := credentials.Get(ctx, "account", credential.ID)
	if err != nil || got.Revision != credential.Revision || !bytes.Equal(got.Ciphertext, credential.Ciphertext) {
		t.Fatalf("upgrade altered credential: %v", err)
	}
	if err := sessions.ClaimInteractiveSession(ctx, "session", "account", "test", 1, loaded.Nonce, now); err != nil {
		t.Fatal(err)
	}
}

func TestReviewedRequestFinishPreservesFallbackAndIsIdempotent(t *testing.T) {
	ledger, ctx, key, policy := admissionFixture(t, 10, 100)
	request, attempt, reservation := admissionRecords(key, policy, "request", "first", 5)
	if err := ledger.Admit(ctx, request, attempt, reservation); err != nil {
		t.Fatal(err)
	}
	if err := ledger.FinishRequest(ctx, request.ID); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("unsettled request finished: %v", err)
	}
	at := time.Now()
	if err := ledger.RecordDispatchIntent(ctx, attempt.ID, at); err != nil {
		t.Fatal(err)
	}
	terminal := TerminalAttempt{AttemptID: attempt.ID, State: "failed", FinishedAt: at, Usage: UsageRecord{AttemptID: attempt.ID, Source: "unknown", Completeness: "unknown", RecordedAt: at}}
	if err := ledger.FinalizeAttempt(ctx, terminal); err != nil {
		t.Fatal(err)
	}
	second := attempt
	second.ID = "second"
	second.Ordinal = 0
	if err := ledger.BeginAttempt(ctx, second, ReservationRecord{AttemptID: second.ID, EstimatedTokens: 5}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.FinishRequest(ctx, request.ID); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("fallback hold was ignored: %v", err)
	}
	if err := ledger.RecordDispatchIntent(ctx, second.ID, at); err != nil {
		t.Fatal(err)
	}
	terminal.AttemptID, terminal.Usage.AttemptID, terminal.State = second.ID, second.ID, "succeeded"
	if err := ledger.FinalizeAttempt(ctx, terminal); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- ledger.FinishRequest(ctx, request.ID) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := ledger.GetRequest(ctx, request.ID)
	if err != nil || got.State != "succeeded" || got.FinishedAt == nil {
		t.Fatalf("finished request: %+v %v", got, err)
	}
	second.ID = "late"
	second.Ordinal = 0
	if err := ledger.BeginAttempt(ctx, second, ReservationRecord{AttemptID: second.ID, EstimatedTokens: 5}); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("closed request admitted a late attempt: %v", err)
	}
	if err := ledger.FinishRequest(ctx, "never-admitted"); err != nil {
		t.Fatal(err)
	}
}

func TestReviewedAccountDisableInvalidatesAuthenticationAtomically(t *testing.T) {
	db, key, sessions, credentials := authSessionFixture(t, filepath.Join(t.TempDir(), "disable.db"))
	ctx := context.Background()
	now := time.Now()
	credential, err := credentials.Create(ctx, sealCredential(t, key, "credential", "account", []byte("old-token")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.CreateInteractiveSession(ctx, "session", "account", "test", 1, now.Add(time.Minute), []byte("state"), key, "v1", now); err != nil {
		t.Fatal(err)
	}
	if err := sessions.CreateRefreshMarker(ctx, "marker", "account", "test", 1, now); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAccounts(db).SetEnabled(ctx, "account", false); err != nil {
		t.Fatal(err)
	}
	var lifecycle string
	var ciphertext []byte
	if err := db.QueryRow(`SELECT lifecycle,ciphertext FROM auth_sessions WHERE id='session'`).Scan(&lifecycle, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "consumed" || ciphertext != nil {
		t.Fatalf("disabled account retained session: %s %x", lifecycle, ciphertext)
	}
	if err := db.QueryRow(`SELECT lifecycle FROM auth_sessions WHERE id='marker'`).Scan(&lifecycle); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "uncertain" {
		t.Fatalf("disabled account refresh %s", lifecycle)
	}
	replacement := sealCredential(t, key, "credential", "account", []byte("candidate"))
	replacement.Revision = 1
	if _, err := sessions.ResolveRefreshAndReplaceCredentials(ctx, "marker", replacement, now); err == nil {
		t.Fatal("disabled account accepted refresh")
	}
	if _, err := sessions.ReplaceCredentialsAndResolveUncertain(ctx, replacement, now); err == nil {
		t.Fatal("disabled account accepted reauthentication")
	}
	if _, err := sessions.CreateInteractiveSession(ctx, "late-session", "account", "test", 1, now.Add(time.Minute), []byte("state"), key, "v1", now); err == nil {
		t.Fatal("disabled account created session")
	}
	if _, _, err := sessions.GetInteractiveSessionDecrypted(ctx, "session", key, now); !errors.Is(err, ErrAuthSessionUnavailable) {
		t.Fatal(err)
	}
	got, err := credentials.Get(ctx, "account", credential.ID)
	if err != nil || got.Revision != 1 {
		t.Fatalf("disabled credential changed: %+v %v", got, err)
	}
	if _, err := NewAccounts(db).SetEnabled(ctx, "account", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sessions.GetInteractiveSessionDecrypted(ctx, "session", key, now); !errors.Is(err, ErrAuthSessionUnavailable) {
		t.Fatal("reenabling revived consumed session")
	}
}

func TestReviewedAuthContinuationClaimPreventsConcurrentAndCrashReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim.db")
	db, key, sessions, credentials := authSessionFixture(t, path)
	ctx := context.Background()
	now := time.Now()
	if _, err := credentials.Create(ctx, sealCredential(t, key, "credential", "account", []byte("old-token"))); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.CreateInteractiveSession(ctx, "session", "account", "test", 1, now.Add(time.Minute), []byte("state"), key, "v1", now); err != nil {
		t.Fatal(err)
	}
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	loaded, _, err := sessions.GetInteractiveSessionDecrypted(ctx, "session", key, now)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, repo := range []*AuthSessions{sessions, NewAuthSessions(other)} {
		go func() {
			<-start
			errs <- repo.ClaimInteractiveSession(ctx, "session", "account", "test", 1, loaded.Nonce, now)
		}()
	}
	close(start)
	winners, denied := 0, 0
	for range 2 {
		err := <-errs
		if err == nil {
			winners++
		} else if errors.Is(err, ErrAuthSessionUnavailable) {
			denied++
		} else {
			t.Fatal(err)
		}
	}
	if winners != 1 || denied != 1 {
		t.Fatalf("claim winners=%d denied=%d", winners, denied)
	}
	if _, _, err := NewAuthSessions(other).GetInteractiveSessionDecrypted(ctx, "session", key, now); !errors.Is(err, ErrAuthSessionUnavailable) {
		t.Fatalf("claimed state is replayable: %v", err)
	}
	if err := sessions.AdvanceInteractiveSession(ctx, "session", "account", "test", 1, now.Add(time.Minute), []byte("next-state"), key, "v1", now); err != nil {
		t.Fatal(err)
	}
	if err := NewAuthSessions(other).ClaimInteractiveSession(ctx, "session", "account", "test", 1, loaded.Nonce, now); !errors.Is(err, ErrAuthSessionUnavailable) {
		t.Fatalf("stale reader claimed advanced state: %v", err)
	}
	advanced, _, err := sessions.GetInteractiveSessionDecrypted(ctx, "session", key, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.ClaimInteractiveSession(ctx, "session", "account", "test", 1, advanced.Nonce, now); err != nil {
		t.Fatal(err)
	}
	// Model an owner disappearing after exchange but before persistence. Reopening
	// alone cannot revive the claim; exclusive startup recovery consumes its state.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAuthSessions(other).RecoverAuthSessions(ctx, now); err != nil {
		t.Fatal(err)
	}
	var lifecycle string
	var ciphertext []byte
	if err := other.QueryRow(`SELECT lifecycle,ciphertext FROM auth_sessions WHERE id='session'`).Scan(&lifecycle, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "consumed" || ciphertext != nil {
		t.Fatalf("recovered continuation %s %x", lifecycle, ciphertext)
	}
	if _, _, err := NewAuthSessions(other).GetInteractiveSessionDecrypted(ctx, "session", key, now); !errors.Is(err, ErrAuthSessionUnavailable) {
		t.Fatal("recovery revived state")
	}
}
