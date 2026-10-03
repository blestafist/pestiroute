package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func policyFixture(t *testing.T, path string) (*sql.DB, *KeyPolicies) {
	t.Helper()
	db := openTestDB(t, path)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, NewKeyPolicies(db)
}

func TestKeyPolicyLifecycleValidationAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policies.db")
	db, repo := policyFixture(t, path)
	ctx := context.Background()
	models := []string{"model-z", "model-a"}
	connectors := []string{"connector-b"}
	created, err := repo.Create(ctx, CreateKeyPolicyParams{ID: "policy-a", Models: models, Connectors: connectors, RPM: 7, TPM: 900})
	if err != nil || created.Revision != 1 || !created.Enabled || created.CreatedAt.Location() != time.UTC || created.CreatedAt.Nanosecond()%int(time.Millisecond) != 0 {
		t.Fatalf("Create() = %#v, %v", created, err)
	}
	var createdType string
	if err := db.QueryRow(`SELECT typeof(created_at) FROM key_policies WHERE id = ? AND revision = 1`, created.ID).Scan(&createdType); err != nil || createdType != "integer" {
		t.Fatalf("created_at storage type = %q, %v", createdType, err)
	}
	models[0], connectors[0] = "mutated", "mutated"
	created.Models[0] = "mutated"
	var modelsJSON, connectorsJSON string
	if err := db.QueryRow(`SELECT models, connectors FROM key_policies WHERE id = ? AND revision = 1`, created.ID).Scan(&modelsJSON, &connectorsJSON); err != nil || modelsJSON != `["model-z","model-a"]` || connectorsJSON != `["connector-b"]` {
		t.Fatalf("persisted JSON = %q, %q, err %v", modelsJSON, connectorsJSON, err)
	}
	// Nil allowlists remain explicit deny-all JSON arrays, not null or wildcards.
	emptyPolicy, err := repo.Create(ctx, CreateKeyPolicyParams{ID: "policy-empty"})
	if err != nil || emptyPolicy.Models == nil || emptyPolicy.Connectors == nil {
		t.Fatalf("empty policy = %#v, %v", emptyPolicy, err)
	}
	generatedIDPolicy, err := repo.Create(ctx, CreateKeyPolicyParams{})
	if err != nil || generatedIDPolicy.ID == "" || generatedIDPolicy.Revision != 1 {
		t.Fatalf("generated-ID policy = %#v, %v", generatedIDPolicy, err)
	}
	if err := db.QueryRow(`SELECT models, connectors FROM key_policies WHERE id = ?`, emptyPolicy.ID).Scan(&modelsJSON, &connectorsJSON); err != nil || modelsJSON != `[]` || connectorsJSON != `[]` {
		t.Fatalf("empty allowlists persisted as %q, %q, err %v", modelsJSON, connectorsJSON, err)
	}
	for _, params := range []CreateKeyPolicyParams{
		{ID: "negative-rpm", RPM: -1}, {ID: "negative-tpm", TPM: -1}, {ID: "blank-model", Models: []string{"  "}},
		{ID: "blank-connector", Connectors: []string{"\t"}}, {ID: "   "},
	} {
		if _, err := repo.Create(ctx, params); !errors.Is(err, ErrInvalidKeyPolicy) {
			t.Errorf("Create(%#v) error = %v, want ErrInvalidKeyPolicy", params, err)
		}
	}
	if _, err := repo.Get(ctx, "missing", 1); !errors.Is(err, ErrKeyPolicyNotFound) {
		t.Fatalf("Get missing error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo = NewKeyPolicies(db)
	got, err := repo.Get(ctx, "policy-a", 1)
	if err != nil || got.Models[0] != "model-z" || got.Connectors[0] != "connector-b" || got.RPM != 7 || got.TPM != 900 || !got.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("Get after reopen = %#v, %v", got, err)
	}
}

func TestKeyPolicyImmutableRevisionsSnapshotAndDefensiveCopies(t *testing.T) {
	db, repo := policyFixture(t, filepath.Join(t.TempDir(), "policies.db"))
	ctx := context.Background()
	models, connectors := []string{"model-a"}, []string{"connector-a"}
	first, err := repo.Create(ctx, CreateKeyPolicyParams{ID: "policy", Models: models, Connectors: connectors, RPM: 2, TPM: 100})
	if err != nil {
		t.Fatal(err)
	}
	models[0], connectors[0] = "caller-mutated", "caller-mutated"
	updated, err := repo.Update(ctx, first.ID, 1, UpdateKeyPolicyParams{Enabled: false, Models: []string{"model-b"}, Connectors: []string{}, RPM: 4, TPM: 200})
	if err != nil || updated.Revision != 2 || updated.Enabled || updated.Connectors == nil {
		t.Fatalf("Update() = %#v, %v", updated, err)
	}
	old, err := repo.Get(ctx, "policy", 1)
	if err != nil || old.Models[0] != "model-a" || old.Connectors[0] != "connector-a" {
		t.Fatalf("historical revision = %#v, %v", old, err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM key_policies WHERE id = 'policy'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("revision row count = %d, %v", count, err)
	}
	latest, err := repo.GetLatest(ctx, "policy")
	if err != nil || latest.Revision != 2 {
		t.Fatalf("GetLatest() = %#v, %v", latest, err)
	}
	snapshot, err := repo.Snapshot(ctx, TrustedPrincipal{PolicyID: "policy", PolicyRevision: 2})
	if err != nil || snapshot.Revision != 2 || snapshot.Connectors == nil {
		t.Fatalf("Snapshot() = %#v, %v", snapshot, err)
	}
	snapshot.Models[0] = "snapshot-mutated"
	got, err := repo.Get(ctx, "policy", 2)
	if err != nil || got.Models[0] != "model-b" {
		t.Fatalf("mutating Snapshot affected policy: %#v, %v", got, err)
	}
	got.Models[0] = "get-mutated"
	listed, err := repo.List(ctx, KeyPolicyFilter{})
	if err != nil || len(listed) != 1 || listed[0].Models[0] != "model-b" {
		t.Fatalf("List() = %#v, %v", listed, err)
	}
	listed[0].Models[0] = "list-mutated"
	if _, err := repo.Update(ctx, "policy", 1, UpdateKeyPolicyParams{}); !errors.Is(err, ErrKeyPolicyRevisionMismatch) {
		t.Fatalf("stale update error = %v", err)
	}
	if _, err := repo.Snapshot(ctx, TrustedPrincipal{PolicyID: "policy", PolicyRevision: 99}); !errors.Is(err, ErrKeyPolicyNotFound) {
		t.Fatalf("missing snapshot error = %v", err)
	}
	if _, err := db.Exec(`INSERT INTO virtual_keys (id, key_id, digest, policy_id, policy_revision, created_at) VALUES ('key', 'key-id', 'digest', 'policy', 1, 1)`); err != nil {
		t.Fatalf("link key to historical revision: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM key_policies WHERE id = 'policy' AND revision = 1`); err == nil {
		t.Fatal("referenced policy revision deletion was not restricted")
	}
}

func TestKeyPolicyConcurrentCASAndInvalidRawRows(t *testing.T) {
	db, repo := policyFixture(t, filepath.Join(t.TempDir(), "policies.db"))
	ctx := context.Background()
	if _, err := repo.Create(ctx, CreateKeyPolicyParams{ID: "race"}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	success, stale := 0, 0
	for range 2 {
		wg.Go(func() {
			<-start
			_, err := repo.Update(ctx, "race", 1, UpdateKeyPolicyParams{Enabled: true, Models: []string{}, Connectors: []string{}, RPM: 1, TPM: 1})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				success++
			case errors.Is(err, ErrKeyPolicyRevisionMismatch):
				stale++
			default:
				t.Errorf("concurrent Update() error = %v", err)
			}
		})
	}
	close(start)
	wg.Wait()
	if success != 1 || stale != 1 {
		t.Fatalf("CAS results: success %d stale %d", success, stale)
	}
	if _, err := db.Exec(`INSERT INTO key_policies (id, revision, enabled, models, connectors, rpm, tpm, created_at) VALUES ('bad', 1, 1, '{', '[]', 0, 0, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO key_policies (id, revision, enabled, models, connectors, rpm, tpm, created_at) VALUES ('bad-list', 1, 1, '["  "]', '[]', 0, 0, 1)`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"bad", "bad-list"} {
		if _, err := repo.Get(ctx, id, 1); !errors.Is(err, ErrInvalidKeyPolicy) {
			t.Errorf("malformed raw row %q error = %v, want ErrInvalidKeyPolicy", id, err)
		}
	}
}
