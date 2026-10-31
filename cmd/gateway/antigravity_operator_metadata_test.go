package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

// Explicitly opt-in, read-only, zero HTTP; emit flags, never credential values.
func TestAntigravityOperatorMetadataProbe(t *testing.T) {
	root := os.Getenv("PESTIROUTE_ANTIGRAVITY_METADATA_PROBE_ROOT")
	if root == "" {
		t.Skip("operator metadata probe not selected")
	}
	db, err := openOperatorProbeDB(filepath.Join(root, "gateway.db"))
	if err != nil {
		t.Fatal("metadata database unavailable")
	}
	defer db.Close()
	key, err := crypto.LoadMasterKey(filepath.Join(root, "master.key"))
	if err != nil {
		t.Fatal("metadata key unavailable")
	}
	ctx := context.Background()
	refs, err := authCredentialRefs(ctx, db, "antigravity-local", false, "")
	if err != nil || refs["antigravity-local"]["oauth"] != "oauth" {
		t.Fatal("metadata reference invalid")
	}
	repo := sqlite.NewCredentials(db)
	row, err := repo.Get(ctx, "antigravity-local", "oauth")
	if err != nil {
		t.Fatal("metadata row unavailable")
	}
	plain, err := repo.GetDecrypted(ctx, "antigravity-local", "oauth", key)
	if err != nil {
		t.Fatal("metadata decryption unavailable")
	}
	defer clear(plain)
	var bundle struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ProjectID    string `json:"project_id"`
	}
	if json.Unmarshal(plain, &bundle) != nil {
		t.Fatal("metadata bundle invalid")
	}
	valid := bundle.AccessToken != "" && bundle.RefreshToken != ""
	project := strings.TrimSpace(bundle.ProjectID) != ""
	unexpired := row.ExpiresAt != nil && row.ExpiresAt.After(time.Now())
	t.Logf("reference_valid=true tokens_present=%t project_valid=%t unexpired=%t", valid, project, unexpired)
	if !valid || !project || !unexpired {
		t.Fatal("metadata readiness failed")
	}
}
