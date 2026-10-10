// Command authcompare creates a token-free, read-only comparison artifact for
// the pinned official Codex CLI and the saved M5.1-040 gateway credential.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

const officialVersion = "0.162.1"
const officialCLI = "/tmp/opencode/official-codex-cli/node_modules/.bin/codex"
const officialCLIHomeV3 = "/tmp/opencode/official-codex-auth-v3"
const officialSessionV3 = "/tmp/opencode/official-codex-session-v3"
const readyMetadataV3 = `{"schema_version":1,"session":"M5.1-050-v3","cli_version":"0.162.1"}`

type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

type gatewayBundle struct {
	Version      int       `json:"version"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	AccountID    string    `json:"account_id"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type cliAuth struct {
	AuthMode string `json:"auth_mode"`
	Tokens   tokens `json:"tokens"`
}

type failureStage string
type failureReason string

const (
	stageInput      failureStage = "input"
	stageCLI        failureStage = "cli"
	stageSession    failureStage = "session"
	stageCLIAuth    failureStage = "cli_auth"
	stageGateway    failureStage = "gateway"
	stageComparison failureStage = "comparison"
	stageArtifact   failureStage = "artifact"
)

const (
	reasonMissing           failureReason = "missing"
	reasonUnavailable       failureReason = "unavailable"
	reasonVersionMismatch   failureReason = "version_mismatch"
	reasonNotReady          failureReason = "not_ready"
	reasonUnsafeFile        failureReason = "unsafe_file"
	reasonUnreadable        failureReason = "unreadable"
	reasonMalformedJSON     failureReason = "malformed_json"
	reasonUnsupportedMode   failureReason = "unsupported_auth_mode"
	reasonInvalidFields     failureReason = "invalid_token_fields"
	reasonUnsafePath        failureReason = "unsafe_path"
	reasonKeyUnavailable    failureReason = "key_unavailable"
	reasonCredentialInvalid failureReason = "credential_invalid"
	reasonSnapshotChanged   failureReason = "snapshot_changed"
	reasonConditionsFailed  failureReason = "conditions_not_met"
	reasonWriteFailed       failureReason = "write_failed"
)

type comparisonFailure struct {
	stage  failureStage
	reason failureReason
}

func (e *comparisonFailure) Error() string { return "AUTH_COMPARE_FAILED" }

func failure(stage failureStage, reason failureReason) error {
	return &comparisonFailure{stage: stage, reason: reason}
}

func failureLine(err error) string {
	var classified *comparisonFailure
	if errors.As(err, &classified) && validFailureStage(classified.stage) && validFailureReason(classified.reason) {
		return fmt.Sprintf("AUTH_COMPARE_FAILED stage=%s reason=%s", classified.stage, classified.reason)
	}
	return "AUTH_COMPARE_FAILED stage=comparison reason=conditions_not_met"
}

func validFailureStage(stage failureStage) bool {
	switch stage {
	case stageInput, stageCLI, stageSession, stageCLIAuth, stageGateway, stageComparison, stageArtifact:
		return true
	default:
		return false
	}
}

func validFailureReason(reason failureReason) bool {
	switch reason {
	case reasonMissing, reasonUnavailable, reasonVersionMismatch, reasonNotReady, reasonUnsafeFile,
		reasonUnreadable, reasonMalformedJSON, reasonUnsupportedMode, reasonInvalidFields,
		reasonUnsafePath, reasonKeyUnavailable, reasonCredentialInvalid, reasonSnapshotChanged,
		reasonConditionsFailed, reasonWriteFailed:
		return true
	default:
		return false
	}
}

type snapshot struct {
	revision int64
	format   int
	key      string
	nonce    []byte
	cipher   []byte
	expires  *time.Time
}

type report struct {
	SchemaVersion int        `json:"schema_version"`
	CapturedAtUTC string     `json:"captured_at_utc"`
	Client        client     `json:"client"`
	GatewaySource string     `json:"gateway_baseline_source"`
	Comparison    comparison `json:"comparison"`
}

type client struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type comparison struct {
	CLIAuthPresent        bool `json:"cli_auth_present"`
	GatewayAuthPresent    bool `json:"gateway_auth_present"`
	SameAccount           bool `json:"same_account"`
	AccessValuesDistinct  bool `json:"access_values_distinct"`
	RefreshValuesDistinct bool `json:"refresh_values_distinct"`
	SeparateStore         bool `json:"separate_store"`
	GatewayStoreSame      bool `json:"gateway_store_unchanged"`
}

func openReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("invalid database path")
	}
	u := &url.URL{Scheme: "file", Path: abs}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(ON)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, errors.New("database open failed")
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, errors.New("database unavailable")
	}
	return db, nil
}

func takeSnapshot(ctx context.Context, repo *sqlite.Credentials, account string) (snapshot, []byte, error) {
	row, err := repo.Get(ctx, account, "oauth")
	if err != nil {
		return snapshot{}, nil, errors.New("gateway credential unavailable")
	}
	plain, err := repo.GetDecrypted(ctx, account, "oauth", masterKey)
	if err != nil {
		return snapshot{}, nil, errors.New("gateway credential decryption failed")
	}
	s := snapshot{revision: row.Revision, format: row.FormatVersion, key: row.KeyVersion,
		nonce: row.Nonce, cipher: row.Ciphertext, expires: row.ExpiresAt}
	return s, plain, nil
}

var masterKey crypto.MasterKey

func equalSnapshot(a, b snapshot) bool {
	return a.revision == b.revision && a.format == b.format && a.key == b.key &&
		string(a.nonce) == string(b.nonce) && string(a.cipher) == string(b.cipher) &&
		timePtrEqual(a.expires, b.expires)
}

func compareTokens(cli, gateway tokens, gatewayValid, separate, gatewayUnchanged bool) comparison {
	return comparison{CLIAuthPresent: cli.AccessToken != "" && cli.RefreshToken != "" && cli.AccountID != "",
		GatewayAuthPresent: gatewayValid, SameAccount: gatewayValid && cli.AccountID == gateway.AccountID,
		AccessValuesDistinct:  gatewayValid && cli.AccessToken != gateway.AccessToken,
		RefreshValuesDistinct: gatewayValid && cli.RefreshToken != gateway.RefreshToken,
		SeparateStore:         separate, GatewayStoreSame: gatewayUnchanged}
}

func comparisonPassed(value comparison) bool {
	return value.CLIAuthPresent && value.GatewayAuthPresent && value.SameAccount &&
		value.AccessValuesDistinct && value.RefreshValuesDistinct && value.SeparateStore &&
		value.GatewayStoreSame
}

func timePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func isWithin(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || (rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))))
}

func readCLIAuth(path string) (tokens, error) {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 || st.Size() > 1<<20 {
		return tokens{}, failure(stageCLIAuth, reasonUnsafeFile)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return tokens{}, failure(stageCLIAuth, reasonUnreadable)
	}
	defer clear(b)
	var value cliAuth
	decoder := json.NewDecoder(bytes.NewReader(b))
	if decoder.Decode(&value) != nil {
		return tokens{}, failure(stageCLIAuth, reasonMalformedJSON)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return tokens{}, failure(stageCLIAuth, reasonMalformedJSON)
	}
	if value.AuthMode != "chatgpt" {
		return tokens{}, failure(stageCLIAuth, reasonUnsupportedMode)
	}
	if value.Tokens.AccessToken == "" || value.Tokens.RefreshToken == "" || value.Tokens.AccountID == "" {
		return tokens{}, failure(stageCLIAuth, reasonInvalidFields)
	}
	return value.Tokens, nil
}

func validatePreparedSession(homePath, statePath string) error {
	for _, path := range []string{homePath, statePath} {
		st, err := os.Lstat(path)
		if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
			return errors.New("v3 auth session directory unsafe or unavailable")
		}
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			return errors.New("v3 auth session directory unavailable")
		}
		abs, err := filepath.Abs(path)
		if err != nil || canonical != abs {
			return errors.New("v3 auth session directory is not canonical")
		}
	}
	ready, err := readPrivateFile(filepath.Join(statePath, "ready.json"), 256)
	if err != nil || strings.TrimSpace(string(ready)) != readyMetadataV3 {
		clear(ready)
		return errors.New("v3 session readiness metadata unavailable")
	}
	clear(ready)
	entries, err := os.ReadDir(statePath)
	if err != nil || len(entries) != 3 {
		return errors.New("v3 session state contents invalid")
	}
	allowed := map[string]bool{"ready.json": true, "status.json": true, "attempted": true}
	for _, entry := range entries {
		if !allowed[entry.Name()] || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("v3 session state contents invalid")
		}
	}
	attempted, err := readPrivateFile(filepath.Join(statePath, "attempted"), 0)
	if err != nil || len(attempted) != 0 {
		clear(attempted)
		return errors.New("v3 one-attempt marker unavailable")
	}
	clear(attempted)
	statusBytes, err := readPrivateFile(filepath.Join(statePath, "status.json"), 4096)
	if err != nil {
		return errors.New("official CLI login is not complete")
	}
	defer clear(statusBytes)
	var status struct {
		Status     string `json:"status"`
		URL        string `json:"url"`
		DeviceCode string `json:"device_code"`
		PID        int    `json:"pid"`
		Exit       string `json:"exit"`
		ParseShape any    `json:"parse_shape"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(statusBytes)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&status) != nil || status.Status != "complete" {
		return errors.New("official CLI login is not complete")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("official CLI status metadata invalid")
	}
	return nil
}

func readPrivateFile(path string, max int64) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 || st.Size() > max {
		return nil, errors.New("private metadata unsafe or unavailable")
	}
	return os.ReadFile(path)
}

func saveReport(path string, value report) error {
	parent := filepath.Dir(path)
	st, err := os.Lstat(parent)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0o077 != 0 {
		return errors.New("artifact directory must be private")
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return errors.New("artifact encoding failed")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("artifact creation failed")
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = os.Remove(path)
		return errors.New("artifact write failed")
	}
	return nil
}

func run() error {
	dbPath := flag.String("gateway-db", "", "saved protected gateway database")
	keyPath := flag.String("master-key", "", "gateway master key")
	logicalAccount := flag.String("account", "", "gateway logical account selector")
	output := flag.String("output", "", "new sanitized artifact path in a private directory")
	flag.Parse()
	if *dbPath == "" || *keyPath == "" || *logicalAccount == "" || *output == "" {
		return failure(stageInput, reasonMissing)
	}
	versionCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	versionOutput, versionErr := exec.CommandContext(versionCtx, officialCLI, "--version").Output()
	if versionErr != nil || strings.TrimSpace(string(versionOutput)) != "codex-cli "+officialVersion {
		clear(versionOutput)
		return failure(stageCLI, reasonVersionMismatch)
	}
	clear(versionOutput)
	if err := validatePreparedSession(officialCLIHomeV3, officialSessionV3); err != nil {
		return failure(stageSession, reasonNotReady)
	}
	home := officialCLIHomeV3
	dbAbs, err := filepath.Abs(*dbPath)
	if err != nil {
		return failure(stageGateway, reasonUnsafePath)
	}
	dbCanonical, err := filepath.EvalSymlinks(dbAbs)
	if err != nil {
		return failure(stageGateway, reasonUnavailable)
	}
	if isWithin(dbCanonical, home) || isWithin(home, filepath.Dir(dbCanonical)) {
		return failure(stageGateway, reasonUnsafePath)
	}
	outputAbs, err := filepath.Abs(*output)
	if err != nil {
		return failure(stageArtifact, reasonUnsafePath)
	}
	outputParent, err := filepath.EvalSymlinks(filepath.Dir(outputAbs))
	if err != nil || isWithin(outputParent, home) || isWithin(outputParent, filepath.Dir(dbCanonical)) || isWithin(home, outputParent) || isWithin(filepath.Dir(dbCanonical), outputParent) {
		return failure(stageArtifact, reasonUnsafePath)
	}
	cliTokens, err := readCLIAuth(filepath.Join(home, "auth.json"))
	if err != nil {
		return err
	}
	masterKey, err = crypto.LoadMasterKey(*keyPath)
	if err != nil {
		return failure(stageGateway, reasonKeyUnavailable)
	}
	db, err := openReadOnly(dbAbs)
	if err != nil {
		return failure(stageGateway, reasonUnavailable)
	}
	defer db.Close()
	repo := sqlite.NewCredentials(db)
	ctx := context.Background()
	before, plain, err := takeSnapshot(ctx, repo, *logicalAccount)
	if err != nil {
		return failure(stageGateway, reasonUnavailable)
	}
	defer clear(plain)
	var bundle gatewayBundle
	validBundle := json.Unmarshal(plain, &bundle) == nil && bundle.Version == 1 && bundle.AccessToken != "" && bundle.RefreshToken != "" && bundle.AccountID != "" && !bundle.ExpiresAt.IsZero() && bundle.ExpiresAt.After(time.Now().UTC()) && before.expires != nil && bundle.ExpiresAt.UnixMilli() == before.expires.UnixMilli()
	after, afterPlain, snapshotErr := takeSnapshot(ctx, repo, *logicalAccount)
	if snapshotErr != nil {
		return failure(stageGateway, reasonUnavailable)
	}
	defer clear(afterPlain)
	storeSame := equalSnapshot(before, after)
	if !storeSame {
		return failure(stageGateway, reasonSnapshotChanged)
	}
	gwTokens := tokens{AccessToken: bundle.AccessToken, RefreshToken: bundle.RefreshToken, AccountID: bundle.AccountID}
	comparisons := compareTokens(cliTokens, gwTokens, validBundle, true, storeSame)
	if !validBundle {
		return failure(stageGateway, reasonCredentialInvalid)
	}
	if !comparisonPassed(comparisons) {
		return failure(stageComparison, reasonConditionsFailed)
	}
	report := report{SchemaVersion: 1, CapturedAtUTC: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		Client: client{Name: "Codex CLI", Version: officialVersion}, GatewaySource: "M5.1-040", Comparison: comparisons}
	if err := saveReport(*output, report); err != nil {
		return failure(stageArtifact, reasonWriteFailed)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, failureLine(err))
		os.Exit(1)
	}
	fmt.Println("AUTH_COMPARE_PASS")
}
