package antigravity

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

// Explicitly opt-in; do not run during offline review or baseline verification.
func TestAntigravityOperatorModelAvailabilityLiveProbe(t *testing.T) {
	root := os.Getenv("PESTIROUTE_ANTIGRAVITY_MODEL_PROBE_ROOT")
	if root == "" || os.Getenv("PESTIROUTE_ANTIGRAVITY_MODEL_PROBE_CONFIRM") != "1" {
		t.Skip("separate explicit live-probe authorization required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := sqlite.Open(filepath.Join(root, "gateway.db"))
	if err != nil {
		t.Fatal("operator database unavailable")
	}
	defer db.Close()
	accountID := "antigravity-local"
	account, err := sqlite.NewAccounts(db).Get(ctx, accountID)
	if err != nil || account.Connector != "antigravity" || !account.Enabled {
		t.Fatal("operator account unavailable")
	}
	key, err := crypto.LoadMasterKey(filepath.Join(root, "master.key"))
	if err != nil {
		t.Fatal("operator key unavailable")
	}
	credentials := sqlite.NewCredentials(db)
	row, err := credentials.Get(ctx, accountID, "oauth")
	if err != nil || row.ExpiresAt == nil || !row.ExpiresAt.After(time.Now()) {
		t.Fatal("unexpired operator credential unavailable")
	}
	plain, err := credentials.GetDecrypted(ctx, accountID, row.ID, key)
	if err != nil {
		t.Fatal("operator credential unavailable")
	}
	defer clear(plain)
	creds, valid := parseOAuthCredentials(plain, true)
	if !valid || !projectPattern.MatchString(creds.ProjectID) {
		t.Fatal("operator credential invalid")
	}
	budget := sqlite.NewOperatorRequestBudgets(db)
	const budgetCeiling = int64(1000)
	if err := budget.VerifyCeiling(ctx, sqlite.AntigravityOperatorBudgetID, budgetCeiling); err != nil {
		t.Fatal("operator request budget unavailable")
	}
	var before int64
	if err := db.QueryRowContext(ctx, `SELECT consumed FROM operator_request_budgets WHERE account_id=?`, sqlite.AntigravityOperatorBudgetID).Scan(&before); err != nil || before < 0 || before >= budgetCeiling {
		t.Fatal("operator request budget precondition failed")
	}
	connector := NewConnector()
	connectorConfig, _ := json.Marshal(config{Profile: profile})
	if err := connector.Init(ctx, core.ComponentConfig{Data: connectorConfig}); err != nil {
		t.Fatal("Antigravity transport initialization failed")
	}
	defer connector.Close(context.Background())
	result, err := connector.fetchAvailableModels(ctx, connector.HTTPDoer(), budget, sqlite.AntigravityOperatorBudgetID, creds.AccessToken, creds.ProjectID, *row.ExpiresAt)
	var after int64
	if scanErr := db.QueryRowContext(context.Background(), `SELECT consumed FROM operator_request_budgets WHERE account_id=?`, sqlite.AntigravityOperatorBudgetID).Scan(&after); scanErr != nil || after != before+1 || after > budgetCeiling {
		t.Fatal("operator request budget result invalid")
	}
	if err != nil {
		t.Fatalf("model availability probe failed (HTTP %d)", result.Status)
	}
	output, err := json.Marshal(result)
	if err != nil {
		t.Fatal("model availability result unavailable")
	}
	t.Log(string(output))
}
