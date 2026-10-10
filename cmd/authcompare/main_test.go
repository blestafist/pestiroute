package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompareTokensPrivacyBooleans(t *testing.T) {
	cli := tokens{AccessToken: "cli-access-synthetic", RefreshToken: "cli-refresh-synthetic", AccountID: "same-account"}
	gw := tokens{AccessToken: "gateway-access-synthetic", RefreshToken: "gateway-refresh-synthetic", AccountID: "same-account"}
	got := compareTokens(cli, gw, true, true, true)
	if !got.CLIAuthPresent || !got.GatewayAuthPresent || !got.SameAccount || !got.AccessValuesDistinct || !got.RefreshValuesDistinct || !got.SeparateStore || !got.GatewayStoreSame {
		t.Fatal("matching isolated synthetic credentials were not represented by true booleans")
	}
	for name, mutate := range map[string]func(*tokens, *tokens, *bool, *bool){
		"account mismatch":     func(c, g *tokens, _, _ *bool) { c.AccountID = "other-account" },
		"shared access token":  func(c, g *tokens, _, _ *bool) { c.AccessToken = g.AccessToken },
		"shared refresh token": func(c, g *tokens, _, _ *bool) { c.RefreshToken = g.RefreshToken },
		"same store":           func(_, _ *tokens, separate, _ *bool) { *separate = false },
		"gateway row changed":  func(_, _ *tokens, _, unchanged *bool) { *unchanged = false },
	} {
		t.Run(name, func(t *testing.T) {
			c, g, separate, unchanged := cli, gw, true, true
			mutate(&c, &g, &separate, &unchanged)
			r := compareTokens(c, g, true, separate, unchanged)
			if r.SameAccount && r.AccessValuesDistinct && r.RefreshValuesDistinct && r.SeparateStore && r.GatewayStoreSame {
				t.Fatal("mismatch was not detected")
			}
		})
	}
}

func TestComparisonArtifactRequiresAllConditionsAndContainsNoCredentials(t *testing.T) {
	passed := comparison{CLIAuthPresent: true, GatewayAuthPresent: true, SameAccount: true,
		AccessValuesDistinct: true, RefreshValuesDistinct: true, SeparateStore: true, GatewayStoreSame: true}
	if !comparisonPassed(passed) {
		t.Fatal("all comparison predicates should permit a sanitized artifact")
	}
	mutations := []func(*comparison){
		func(c *comparison) { c.CLIAuthPresent = false },
		func(c *comparison) { c.GatewayAuthPresent = false },
		func(c *comparison) { c.SameAccount = false },
		func(c *comparison) { c.AccessValuesDistinct = false },
		func(c *comparison) { c.RefreshValuesDistinct = false },
		func(c *comparison) { c.SeparateStore = false },
		func(c *comparison) { c.GatewayStoreSame = false },
	}
	for _, mutate := range mutations {
		result := passed
		mutate(&result)
		if comparisonPassed(result) {
			t.Fatal("failed comparison predicate allowed artifact creation")
		}
	}
	value := report{SchemaVersion: 1, CapturedAtUTC: "2026-01-01T00:00:00Z",
		Client: client{Name: "Codex CLI", Version: officialVersion}, GatewaySource: "M5.1-040", Comparison: passed}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal("synthetic artifact encoding failed")
	}
	for _, secret := range []string{"synthetic-access", "synthetic-refresh", "synthetic-account", "private@example.invalid"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("sanitized comparison artifact contains a synthetic secret or identifier")
		}
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal("private artifact directory setup failed")
	}
	path := filepath.Join(dir, "comparison.json")
	if err := saveReport(path, value); err != nil {
		t.Fatal("sanitized synthetic artifact save failed")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("comparison artifact is not private")
	}
	saved, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(saved), "synthetic-") || strings.Contains(string(saved), "private@example.invalid") {
		t.Fatal("saved comparison artifact contains private values")
	}
	if err := saveReport(path, value); err == nil {
		t.Fatal("comparison artifact overwrite was accepted")
	}
}

func TestFailureDiagnosticsAreFixedEnums(t *testing.T) {
	if got := failureLine(failure(stageCLIAuth, reasonUnsupportedMode)); got != "AUTH_COMPARE_FAILED stage=cli_auth reason=unsupported_auth_mode" {
		t.Fatalf("unexpected fixed failure diagnostic: %q", got)
	}
	for _, err := range []error{
		errors.New("/private/path synthetic-secret"),
		&comparisonFailure{stage: failureStage("/private/path"), reason: failureReason("synthetic-secret")},
	} {
		if got := failureLine(err); strings.Contains(got, "/private") || strings.Contains(got, "synthetic-secret") || got != "AUTH_COMPARE_FAILED stage=comparison reason=conditions_not_met" {
			t.Fatal("invalid diagnostic values escaped the fixed enum")
		}
	}
}

func TestReadCLIAuthRequiresPinnedShapeAndPrivateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	valid := `{"auth_mode":"chatgpt","tokens":{"id_token":"eyJsynthetic.jwt.secret","access_token":"synthetic-a","refresh_token":"synthetic-r","account_id":"synthetic-account"}}`
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal("fixture write failed")
	}
	if got, err := readCLIAuth(path); err != nil || got.AccountID != "synthetic-account" {
		t.Fatal("expected supported synthetic auth metadata")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal("fixture chmod failed")
	}
	if _, err := readCLIAuth(path); err == nil {
		t.Fatal("insecure auth file mode accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal("fixture chmod failed")
	}
	if err := os.WriteFile(path, []byte(`{"auth_mode":"Other","tokens":{}}`), 0600); err != nil {
		t.Fatal("fixture replacement failed")
	}
	if _, err := readCLIAuth(path); err == nil {
		t.Fatal("unsupported auth schema accepted")
	}
}

func TestReadCLIAuthModeAndPoisonFailuresAreSanitized(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	for name, fixture := range map[string]struct{ input, reason string }{
		"uppercase mode": {`{"auth_mode":"ChatGPT","tokens":{"access_token":"synthetic-a","refresh_token":"synthetic-r","account_id":"synthetic-account"}}`, "unsupported_auth_mode"},
		"missing token":  {`{"auth_mode":"chatgpt","tokens":{"access_token":"synthetic-a","refresh_token":"synthetic-r"}}`, "invalid_token_fields"},
		"poison values":  {`{"auth_mode":"chatgpt","tokens":{"access_token":"eyJsynthetic.jwt.secret","refresh_token":"synthetic-private-token"}}`, "invalid_token_fields"},
		"malformed json": {`{"auth_mode":"chatgpt","tokens":{"access_token":"eyJsynthetic.jwt.secret","refresh_token":"synthetic-private-token"}`, "malformed_json"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(fixture.input), 0600); err != nil {
				t.Fatal("fixture write failed")
			}
			_, err := readCLIAuth(path)
			if err == nil {
				t.Fatal("unsupported or incomplete auth metadata accepted")
			}
			line := failureLine(err)
			if strings.Contains(line, "synthetic") || strings.Contains(line, "private-id") || !strings.HasPrefix(line, "AUTH_COMPARE_FAILED stage=cli_auth reason=") {
				t.Fatal("auth diagnostic leaked private fixture values or had wrong stage")
			}
			if !strings.HasSuffix(line, "reason="+fixture.reason) {
				t.Fatal("auth diagnostic reason mismatch")
			}
		})
	}
}

func TestValidatePreparedV3SessionMetadata(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "auth-v3")
	state := filepath.Join(base, "state-v3")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal("home fixture creation failed")
	}
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal("state fixture creation failed")
	}
	write := func(path, value string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), mode); err != nil {
			t.Fatal("metadata fixture write failed")
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal("metadata fixture chmod failed")
		}
	}
	ready := filepath.Join(state, "ready.json")
	status := filepath.Join(state, "status.json")
	attempted := filepath.Join(state, "attempted")
	write(ready, readyMetadataV3+"\n", 0600)
	write(status, `{"status":"complete","url":"https://auth.openai.com/codex/device","device_code":"ABCD-EF23","pid":123}`, 0600)
	write(attempted, "", 0600)
	if err := validatePreparedSession(home, state); err != nil {
		t.Fatal("prepared completed v3 session rejected")
	}
	write(status, `{"status":"waiting"}`, 0600)
	if err := validatePreparedSession(home, state); err == nil {
		t.Fatal("incomplete CLI auth status accepted")
	}
	write(status, `{"status":"complete"}`, 0644)
	if err := validatePreparedSession(home, state); err == nil {
		t.Fatal("insecure CLI auth status metadata accepted")
	}
	write(status, `{"status":"complete"}`, 0600)
	write(ready, `{"schema_version":1,"session":"M5.1-050-v2","cli_version":"0.162.1"}`, 0600)
	if err := validatePreparedSession(home, state); err == nil {
		t.Fatal("wrong session readiness metadata accepted")
	}
}

func TestAuthCompareUsesFixedV3Paths(t *testing.T) {
	if officialCLIHomeV3 != "/tmp/opencode/official-codex-auth-v3" || officialSessionV3 != "/tmp/opencode/official-codex-session-v3" {
		t.Fatal("auth comparator paths are not the pinned v3 paths")
	}
}

func TestCredentialSnapshotComparison(t *testing.T) {
	expires := time.Now().UTC()
	a := snapshot{revision: 2, format: 1, key: "key-v1", nonce: []byte("n"), cipher: []byte("sealed"), expires: &expires}
	b := a
	b.nonce = append([]byte(nil), a.nonce...)
	b.cipher = append([]byte(nil), a.cipher...)
	if !equalSnapshot(a, b) {
		t.Fatal("identical credential snapshots differ")
	}
	b.revision++
	if equalSnapshot(a, b) {
		t.Fatal("revision change was not detected")
	}
	b = a
	b.cipher = []byte("different-sealed")
	if equalSnapshot(a, b) {
		t.Fatal("sealed credential mutation was not detected")
	}
}
