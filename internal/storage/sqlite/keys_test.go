package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func virtualKeyFixture(t *testing.T) (string, *sql.DB, *VirtualKeys) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.db")
	db := openTestDB(t, path)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(`INSERT INTO key_policies(id, revision, enabled, models, connectors, rpm, tpm, created_at)
		VALUES ('policy-a', 1, 1, '[]', '[]', 10, 100, 1000), ('policy-a', 2, 1, '[]', '[]', 20, 200, 1001)`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return path, db, NewVirtualKeys(db)
}

func TestVirtualKeyLifecycleAndRestart(t *testing.T) {
	path, db, repo := virtualKeyFixture(t)
	ctx := context.Background()
	issued, err := repo.Create(ctx, CreateVirtualKeyParams{PolicyID: "policy-a", PolicyRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if issued.ID == issued.KeyID || issued.Digest == issued.Secret || issued.Secret == "" || issued.Revision != 1 || !issued.Enabled || issued.Revoked {
		t.Fatalf("invalid issuance metadata: %v", issued)
	}
	if strings.Contains(fmt.Sprintf("%+v", issued), issued.Secret) || strings.Contains(fmt.Sprintf("%#v", issued), issued.Secret) || strings.Contains(issued.String(), issued.Secret) {
		t.Fatal("issued key formatter exposed secret")
	}
	for name, values := range map[string][3]string{
		"internal ID":       {issued.ID, "another-public-id", "another-digest"},
		"public identifier": {"another-internal-id", issued.KeyID, "another-digest"},
		"digest":            {"another-internal-id", "another-public-id", issued.Digest},
	} {
		id, keyID, digest := values[0], values[1], values[2]
		if _, err := db.Exec(`INSERT INTO virtual_keys(id, key_id, digest, policy_id, policy_revision, revision, enabled, revoked, created_at)
			VALUES (?, ?, ?, 'policy-a', 1, 1, 1, 0, 1000)`, id, keyID, digest); err == nil {
			t.Fatalf("duplicate %s accepted", name)
		}
	}
	if got, err := repo.GetByKeyID(ctx, issued.KeyID); err != nil || got.ID != issued.ID || strings.Contains(fmt.Sprintf("%+v", got), issued.Secret) {
		t.Fatalf("safe public lookup = %#v, %v", got, err)
	}
	listed, err := repo.List(ctx, VirtualKeyFilter{PolicyID: "policy-a"})
	if err != nil || len(listed) != 1 || strings.Contains(fmt.Sprintf("%#v", listed), issued.Secret) {
		t.Fatalf("safe metadata list = %#v, %v", listed, err)
	}
	principal, err := repo.Verify(ctx, issued.Secret)
	if err != nil || principal != (TrustedPrincipal{KeyID: issued.KeyID, PolicyID: "policy-a", KeyRevision: 1, PolicyRevision: 1}) {
		t.Fatalf("Verify = %#v, %v", principal, err)
	}
	for _, invalid := range []string{"", "malformed", "prv_0000000000000000000000000000000000000000000"} {
		if _, err := repo.Verify(ctx, invalid); !errors.Is(err, ErrInvalidVirtualKey) || strings.Contains(err.Error(), issued.Secret) {
			t.Fatalf("Verify invalid error = %v", err)
		}
	}
	if _, err := repo.Create(ctx, CreateVirtualKeyParams{PolicyID: "missing", PolicyRevision: 1}); err == nil {
		t.Fatal("Create accepted missing policy")
	}
	var createdType string
	if err := db.QueryRow(`SELECT typeof(created_at) FROM virtual_keys WHERE id = ?`, issued.ID).Scan(&createdType); err != nil || createdType != "integer" || issued.CreatedAt.Location() != time.UTC || issued.CreatedAt.Nanosecond()%int(time.Millisecond) != 0 {
		t.Fatalf("created_at type/value = %q, %v", createdType, err)
	}
	falseValue := false
	if keys, err := repo.List(ctx, VirtualKeyFilter{PolicyID: "policy-a", Enabled: &falseValue}); err != nil || len(keys) != 0 {
		t.Fatalf("filtered list = %#v, %v", keys, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo = NewVirtualKeys(db)
	got, err := repo.Get(ctx, issued.ID)
	if err != nil || got.ID != issued.ID || got.KeyID != issued.KeyID || got.Digest != issued.Digest || !got.CreatedAt.Equal(issued.CreatedAt) {
		t.Fatalf("Get after reopen = %#v, %v", got, err)
	}
	updated, err := repo.UpdatePolicy(ctx, issued.ID, "policy-a", 2, 1)
	if err != nil || updated.Revision != 2 || updated.PolicyRevision != 2 {
		t.Fatalf("UpdatePolicy = %#v, %v", updated, err)
	}
	if _, err := repo.UpdatePolicy(ctx, issued.ID, "policy-a", 1, 1); !errors.Is(err, ErrVirtualKeyRevisionMismatch) {
		t.Fatalf("stale UpdatePolicy error = %v", err)
	}
	updated, err = repo.SetEnabled(ctx, issued.ID, false)
	if err != nil || updated.Enabled || updated.Revision != 3 {
		t.Fatalf("SetEnabled(false) = %#v, %v", updated, err)
	}
	if _, err := repo.Verify(ctx, issued.Secret); !errors.Is(err, ErrVirtualKeyDisabled) {
		t.Fatalf("Verify disabled error = %v", err)
	}
	if _, err := repo.SetEnabled(ctx, issued.ID, true); err != nil {
		t.Fatal(err)
	}
	revoked, err := repo.Revoke(ctx, issued.ID)
	if err != nil || !revoked.Revoked || revoked.RevokedAt == nil || revoked.Revision != 5 || revoked.RevokedAt.Location() != time.UTC {
		t.Fatalf("Revoke = %#v, %v", revoked, err)
	}
	var revokedType string
	if err := db.QueryRow(`SELECT typeof(revoked_at) FROM virtual_keys WHERE id = ?`, issued.ID).Scan(&revokedType); err != nil || revokedType != "integer" {
		t.Fatalf("revoked_at type = %q, %v", revokedType, err)
	}
	revokedAgain, err := repo.Revoke(ctx, issued.ID)
	if err != nil || revokedAgain.Revision != revoked.Revision || !revokedAgain.RevokedAt.Equal(*revoked.RevokedAt) {
		t.Fatalf("repeat Revoke = %#v, %v", revokedAgain, err)
	}
	if _, err := repo.SetEnabled(ctx, issued.ID, true); !errors.Is(err, ErrVirtualKeyRevoked) {
		t.Fatalf("re-enable revoked error = %v", err)
	}
	if _, err := repo.UpdatePolicy(ctx, issued.ID, "policy-a", 2, revoked.Revision); !errors.Is(err, ErrVirtualKeyRevoked) {
		t.Fatalf("mutate revoked policy error = %v", err)
	}
	if _, err := repo.Verify(ctx, issued.Secret); !errors.Is(err, ErrVirtualKeyRevoked) {
		t.Fatalf("Verify revoked error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err = NewVirtualKeys(db).Get(ctx, issued.ID)
	if err != nil || !got.Revoked || got.Revision != revoked.Revision || got.RevokedAt == nil || !got.RevokedAt.Equal(*revoked.RevokedAt) {
		t.Fatalf("revocation after reopen = %#v, %v", got, err)
	}
	for _, name := range []string{path, path + "-wal"} {
		data, err := os.ReadFile(name)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if strings.Contains(string(data), issued.Secret) {
			t.Fatalf("plaintext secret persisted in %s", name)
		}
	}
}

func TestVirtualKeyConcurrentPolicyCASAndSafeErrors(t *testing.T) {
	_, _, repo := virtualKeyFixture(t)
	ctx := context.Background()
	issued, err := repo.Create(ctx, CreateVirtualKeyParams{PolicyID: "policy-a", PolicyRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	const contenders = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes, mismatches := 0, 0
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := repo.UpdatePolicy(ctx, issued.ID, "policy-a", 2, 1)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrVirtualKeyRevisionMismatch):
				mismatches++
			default:
				t.Errorf("UpdatePolicy error = %v", err)
			}
		}()
	}
	wg.Wait()
	if successes != 1 || mismatches != contenders-1 {
		t.Fatalf("CAS outcomes: successes=%d mismatches=%d", successes, mismatches)
	}
	if _, err := repo.Get(ctx, "missing-id"); !errors.Is(err, ErrVirtualKeyNotFound) || strings.Contains(err.Error(), issued.Secret) {
		t.Fatalf("safe missing error = %v", err)
	}
}
