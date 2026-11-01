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

const (
	antigravityLiveRequestDeadline = 90 * time.Second
	antigravityLiveExpiryMargin    = time.Minute
)

func antigravityLiveLifetimeSufficient(remaining time.Duration) bool {
	return remaining >= 2*antigravityLiveRequestDeadline+antigravityLiveExpiryMargin
}

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
	remaining := time.Duration(0)
	if row.ExpiresAt != nil {
		remaining = time.Until(*row.ExpiresAt)
	}
	unexpired := remaining > 0
	lifetimeSufficient := antigravityLiveLifetimeSufficient(remaining)
	t.Logf("reference_valid=true tokens_present=%t project_valid=%t unexpired=%t lifetime_sufficient=%t", valid, project, unexpired, lifetimeSufficient)
	if !valid || !project || !unexpired || !lifetimeSufficient {
		t.Fatal("metadata readiness failed")
	}
}

func TestAntigravityLiveLifetimeMarginBoundary(t *testing.T) {
	minimum := 2*antigravityLiveRequestDeadline + antigravityLiveExpiryMargin
	for _, tc := range []struct {
		name      string
		remaining time.Duration
		want      bool
	}{
		{name: "below threshold", remaining: minimum - time.Nanosecond, want: false},
		{name: "exact threshold", remaining: minimum, want: true},
		{name: "above threshold", remaining: minimum + time.Nanosecond, want: true},
		{name: "zero", remaining: 0, want: false},
		{name: "expired", remaining: -time.Nanosecond, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := antigravityLiveLifetimeSufficient(tc.remaining); got != tc.want {
				t.Fatalf("sufficient(%s)=%t, want %t", tc.remaining, got, tc.want)
			}
		})
	}
}
