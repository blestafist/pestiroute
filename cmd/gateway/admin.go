package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"syscall"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

const adminUsage = "usage: gateway admin [--db PATH] [--master-key PATH] <migrate|status|account|credential|policy|key|usage|auth> ...\n"
const maxAdminSecret = 64 << 10

type adminEnvironment struct {
	ctx          context.Context
	args         []string
	stdin        io.Reader
	stdout       io.Writer
	stderr       io.Writer
	authRegistry *core.Registry
	authServices core.AuthServicesFactory
}

func runAdmin(env adminEnvironment) error {
	if env.ctx == nil {
		env.ctx = context.Background()
	}
	if env.stdout == nil {
		env.stdout = io.Discard
	}
	if env.stderr == nil {
		env.stderr = io.Discard
	}
	if len(env.args) == 0 || isHelp(env.args[0]) {
		_, _ = io.WriteString(env.stdout, adminUsage)
		return nil
	}
	if env.args[0] == "usage" {
		if handled, err := adminUsageHelpOrInvalid(env); handled {
			return err
		}
	}
	if len(env.args) == 2 && isHelp(env.args[1]) && (env.args[0] == "migrate" || env.args[0] == "status") {
		_, _ = fmt.Fprintf(env.stdout, "%s  %s: no additional arguments\n", adminUsage, env.args[0])
		return nil
	}
	flags := flag.NewFlagSet("gateway admin", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dbPath := flags.String("db", "", "SQLite database path")
	keyPath := flags.String("master-key", "", "master-key file path")
	if err := flags.Parse(env.args); err != nil {
		_, _ = io.WriteString(env.stderr, adminUsage)
		return errors.New("admin: invalid arguments")
	}
	args := flags.Args()
	if len(args) > 0 && args[0] == "usage" {
		if handled, err := adminUsageHelpOrInvalid(adminEnvironment{ctx: env.ctx, args: args, stdin: env.stdin, stdout: env.stdout, stderr: env.stderr}); handled {
			return err
		}
	}
	if len(args) == 2 && isHelp(args[1]) && (args[0] == "migrate" || args[0] == "status") {
		_, _ = fmt.Fprintf(env.stdout, "%s  %s: no additional arguments\n", adminUsage, args[0])
		return nil
	}
	if len(args) == 0 || isHelp(args[0]) {
		_, _ = io.WriteString(env.stdout, adminUsage)
		return nil
	}
	if args[0] != "migrate" && args[0] != "status" && args[0] != "account" && args[0] != "credential" && args[0] != "policy" && args[0] != "key" && args[0] != "usage" && args[0] != "auth" {
		_, _ = io.WriteString(env.stderr, adminUsage)
		return errors.New("admin: unknown subcommand")
	}
	if *dbPath == "" || *keyPath == "" {
		_, _ = io.WriteString(env.stderr, adminUsage)
		return errors.New("admin: --db and --master-key are required")
	}
	key, err := secure.LoadMasterKey(*keyPath)
	if err != nil {
		return errors.New("admin: invalid master key file")
	}
	db, err := openAdminDatabase(*dbPath, args[0] == "migrate")
	if err != nil {
		return errors.New("admin: cannot open database")
	}
	defer db.Close()
	if args[0] == "migrate" {
		if len(args) != 1 {
			return errors.New("admin: migrate accepts no arguments")
		}
		if err := sqlite.Migrate(env.ctx, db); err != nil {
			return errors.New("admin: migration failed")
		}
		_, _ = fmt.Fprintln(env.stdout, "schema is current")
		return nil
	}
	if args[0] == "account" || args[0] == "credential" || args[0] == "policy" || args[0] == "key" || args[0] == "usage" || args[0] == "auth" {
		version, err := sqlite.SchemaVersion(env.ctx, db)
		if err != nil {
			return errors.New("admin: unsupported or unreadable schema")
		}
		if version != sqlite.CurrentSchemaVersion() {
			return errors.New("admin: schema is not fully migrated; run admin migrate")
		}
		if args[0] == "policy" || args[0] == "key" {
			return runKeyPolicyAdmin(env, db, args[0], args[1:])
		}
		if args[0] == "usage" {
			return runUsageAdmin(env, db, args[1:])
		}
		if args[0] == "auth" {
			return runAuthAdmin(env, db, key, args[1:])
		}
		return runAccountCredentialAdmin(env, db, key, args[0], args[1:])
	}
	if len(args) != 1 {
		return errors.New("admin: status accepts no arguments")
	}
	version, err := sqlite.SchemaVersion(env.ctx, db)
	if err != nil {
		return errors.New("admin: unsupported or unreadable schema")
	}
	_, _ = fmt.Fprintf(env.stdout, "schema version %d\n", version)
	return nil
}

func runAccountCredentialAdmin(env adminEnvironment, db *sql.DB, key secure.MasterKey, entity string, args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		_, _ = io.WriteString(env.stdout, adminUsage)
		return nil
	}
	switch entity {
	case "account":
		accounts := sqlite.NewAccounts(db)
		switch args[0] {
		case "create":
			fs := adminFlags("account create")
			id, connector := fs.String("id", "", "account ID"), fs.String("connector", "", "connector instance")
			disabled := fs.Bool("disabled", false, "create disabled")
			if err := parseAdminFlags(fs, args[1:]); err != nil {
				return err
			}
			if fs.NArg() != 0 || *id == "" || *connector == "" {
				return errors.New("admin: account id and connector are required")
			}
			account, err := accounts.Create(env.ctx, sqlite.Account{ID: *id, Connector: *connector, Enabled: !*disabled})
			if err != nil {
				return errors.New("admin: cannot create account (duplicate or invalid metadata)")
			}
			printAdminAccount(env.stdout, account)
		case "get", "enable", "disable":
			if len(args) != 2 || args[1] == "" {
				return errors.New("admin: account command requires one ID")
			}
			var account sqlite.Account
			var err error
			switch args[0] {
			case "get":
				account, err = accounts.Get(env.ctx, args[1])
			case "enable":
				account, err = accounts.SetEnabled(env.ctx, args[1], true)
			case "disable":
				account, err = accounts.SetEnabled(env.ctx, args[1], false)
			}
			if errors.Is(err, sqlite.ErrAccountNotFound) {
				return errors.New("admin: account not found")
			}
			if err != nil {
				return errors.New("admin: account operation failed")
			}
			printAdminAccount(env.stdout, account)
		case "list":
			fs := adminFlags("account list")
			connector := fs.String("connector", "", "connector instance")
			enabledText := fs.String("enabled", "", "enabled filter (true or false)")
			if err := parseAdminFlags(fs, args[1:]); err != nil {
				return err
			}
			if fs.NArg() != 0 {
				return errors.New("admin: account list accepts no positional arguments")
			}
			filter := sqlite.AccountFilter{Connector: *connector}
			enabledProvided := false
			fs.Visit(func(f *flag.Flag) {
				if f.Name == "enabled" {
					enabledProvided = true
				}
			})
			if enabledProvided {
				if *enabledText != "true" && *enabledText != "false" {
					return errors.New("admin: --enabled must be true or false")
				}
				value := *enabledText == "true"
				filter.Enabled = &value
			}
			list, err := accounts.List(env.ctx, filter)
			if err != nil {
				return errors.New("admin: cannot list accounts")
			}
			for _, account := range list {
				printAdminAccount(env.stdout, account)
			}
		default:
			return errors.New("admin: unknown account command")
		}
	case "credential":
		if args[0] != "create" && args[0] != "update" {
			return errors.New("admin: unknown credential command")
		}
		fs := adminFlags("credential " + args[0])
		accountID, id := fs.String("account", "", "account ID"), fs.String("id", "", "credential ID")
		expiresText := fs.String("expires-at", "", "RFC3339 expiration")
		expected := fs.Int64("expected-revision", 0, "expected credential revision")
		if err := parseAdminFlags(fs, args[1:]); err != nil {
			return err
		}
		expectedProvided := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "expected-revision" {
				expectedProvided = true
			}
		})
		if fs.NArg() != 0 || *accountID == "" || *id == "" || args[0] == "update" && *expected <= 0 || args[0] == "create" && expectedProvided {
			return errors.New("admin: invalid credential arguments")
		}
		var expires *time.Time
		if *expiresText != "" {
			value, err := time.Parse(time.RFC3339, *expiresText)
			if err != nil {
				return errors.New("admin: --expires-at must be RFC3339")
			}
			value = value.UTC()
			expires = &value
		}
		secret, err := readAdminSecret(env.stdin)
		if err != nil {
			return err
		}
		if len(secret) == 0 {
			zeroAdminSecret(secret)
			return errors.New("admin: empty secret input")
		}
		envelope, err := secure.Seal(key, 1, "v1", "credentials", *id, *accountID, secret)
		zeroAdminSecret(secret)
		if err != nil {
			return errors.New("admin: cannot encrypt credential")
		}
		credential := sqlite.Credential{ID: *id, AccountID: *accountID, FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext, ExpiresAt: expires}
		credentials := sqlite.NewCredentials(db)
		if args[0] == "create" {
			created, err := credentials.Create(env.ctx, credential)
			if err != nil {
				return errors.New("admin: cannot create credential (duplicate or account missing)")
			}
			printAdminCredential(env.stdout, created)
		} else {
			credential.Revision = *expected
			updated, err := credentials.Update(env.ctx, credential)
			if errors.Is(err, sqlite.ErrRevisionMismatch) {
				return errors.New("admin: credential revision mismatch")
			}
			if errors.Is(err, sqlite.ErrNotFound) {
				return errors.New("admin: credential not found")
			}
			if err != nil {
				return errors.New("admin: cannot update credential")
			}
			printAdminCredential(env.stdout, updated)
		}
	}
	return nil
}

func runKeyPolicyAdmin(env adminEnvironment, db *sql.DB, entity string, args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		_, _ = io.WriteString(env.stdout, adminUsage)
		return nil
	}
	policies, keys := sqlite.NewKeyPolicies(db), sqlite.NewVirtualKeys(db)
	switch entity {
	case "policy":
		switch args[0] {
		case "create":
			fs := adminFlags("policy create")
			id := fs.String("id", "", "policy ID")
			models, connectors := fs.String("models", "", "comma-separated exact model IDs"), fs.String("connectors", "", "comma-separated connector IDs")
			rpm, tpm := fs.Int64("rpm", 0, "requests per minute"), fs.Int64("tpm", 0, "tokens per minute")
			if err := parseAdminFlags(fs, args[1:]); err != nil {
				return err
			}
			if fs.NArg() != 0 {
				return errors.New("admin: policy create accepts no positional arguments")
			}
			modelList, err := parseAdminCSV(*models)
			if err != nil {
				return err
			}
			connectorList, err := parseAdminCSV(*connectors)
			if err != nil {
				return err
			}
			policy, err := policies.Create(env.ctx, sqlite.CreateKeyPolicyParams{ID: *id, Enabled: true, Models: modelList, Connectors: connectorList, RPM: *rpm, TPM: *tpm})
			if errors.Is(err, sqlite.ErrInvalidKeyPolicy) {
				return errors.New("admin: invalid key policy")
			}
			if err != nil {
				return errors.New("admin: cannot create key policy (duplicate or invalid metadata)")
			}
			printAdminPolicy(env.stdout, policy)
		case "get":
			if len(args) < 2 || args[1] == "" {
				return errors.New("admin: policy get requires an ID")
			}
			fs := adminFlags("policy get")
			revision := fs.Int64("revision", 0, "policy revision")
			if err := parseAdminFlags(fs, args[2:]); err != nil {
				return err
			}
			if fs.NArg() != 0 || *revision < 0 {
				return errors.New("admin: invalid policy get arguments")
			}
			var p sqlite.KeyPolicy
			var err error
			if *revision == 0 {
				p, err = policies.GetLatest(env.ctx, args[1])
			} else {
				p, err = policies.Get(env.ctx, args[1], *revision)
			}
			if errors.Is(err, sqlite.ErrKeyPolicyNotFound) {
				return errors.New("admin: key policy not found")
			}
			if err != nil {
				return errors.New("admin: cannot read key policy")
			}
			printAdminPolicy(env.stdout, p)
		case "list":
			fs := adminFlags("policy list")
			enabledText := fs.String("enabled", "", "enabled filter (true or false)")
			if err := parseAdminFlags(fs, args[1:]); err != nil {
				return err
			}
			if fs.NArg() != 0 {
				return errors.New("admin: policy list accepts no positional arguments")
			}
			filter := sqlite.KeyPolicyFilter{}
			provided := false
			fs.Visit(func(f *flag.Flag) {
				if f.Name == "enabled" {
					provided = true
				}
			})
			if provided {
				v, ok := parseAdminBool(*enabledText)
				if !ok {
					return errors.New("admin: --enabled must be true or false")
				}
				filter.Enabled = &v
			}
			list, err := policies.List(env.ctx, filter)
			if err != nil {
				return errors.New("admin: cannot list key policies")
			}
			for _, p := range list {
				printAdminPolicy(env.stdout, p)
			}
		case "update":
			if len(args) < 2 || args[1] == "" {
				return errors.New("admin: policy update requires an ID")
			}
			fs := adminFlags("policy update")
			expected := fs.Int64("expected-revision", 0, "expected policy revision")
			models := fs.String("models", "", "comma-separated exact model IDs")
			connectors := fs.String("connectors", "", "comma-separated connector IDs")
			rpm := fs.Int64("rpm", 0, "requests per minute")
			tpm := fs.Int64("tpm", 0, "tokens per minute")
			disabled := fs.Bool("disabled", false, "disable policy")
			if err := parseAdminFlags(fs, args[2:]); err != nil {
				return err
			}
			if fs.NArg() != 0 || *expected < 1 {
				return errors.New("admin: policy update requires --expected-revision")
			}
			current, err := policies.GetLatest(env.ctx, args[1])
			if errors.Is(err, sqlite.ErrKeyPolicyNotFound) {
				return errors.New("admin: key policy not found")
			}
			if err != nil {
				return errors.New("admin: cannot read key policy")
			}
			modelList, connectorList := current.Models, current.Connectors
			if wasAdminFlagSet(fs, "models") {
				modelList, err = parseAdminCSV(*models)
				if err != nil {
					return err
				}
			}
			if wasAdminFlagSet(fs, "connectors") {
				connectorList, err = parseAdminCSV(*connectors)
				if err != nil {
					return err
				}
			}
			if wasAdminFlagSet(fs, "rpm") && *rpm < 0 || wasAdminFlagSet(fs, "tpm") && *tpm < 0 {
				return errors.New("admin: policy limits must be non-negative")
			}
			if wasAdminFlagSet(fs, "rpm") {
				current.RPM = *rpm
			}
			if wasAdminFlagSet(fs, "tpm") {
				current.TPM = *tpm
			}
			p, err := policies.Update(env.ctx, args[1], *expected, sqlite.UpdateKeyPolicyParams{Enabled: current.Enabled && !*disabled, Models: modelList, Connectors: connectorList, RPM: current.RPM, TPM: current.TPM})
			if errors.Is(err, sqlite.ErrKeyPolicyRevisionMismatch) {
				return errors.New("admin: key policy revision mismatch")
			}
			if errors.Is(err, sqlite.ErrInvalidKeyPolicy) {
				return errors.New("admin: invalid key policy")
			}
			if errors.Is(err, sqlite.ErrKeyPolicyNotFound) {
				return errors.New("admin: key policy not found")
			}
			if err != nil {
				return errors.New("admin: cannot update key policy")
			}
			printAdminPolicy(env.stdout, p)
		default:
			return errors.New("admin: unknown policy command")
		}
	case "key":
		switch args[0] {
		case "create":
			fs := adminFlags("key create")
			policyID := fs.String("policy", "", "policy ID")
			policyRevision := fs.Int64("policy-revision", 0, "policy revision")
			if err := parseAdminFlags(fs, args[1:]); err != nil {
				return err
			}
			if fs.NArg() != 0 || *policyID == "" || *policyRevision < 0 {
				return errors.New("admin: key create requires a policy and valid revision")
			}
			var p sqlite.KeyPolicy
			var err error
			if *policyRevision == 0 {
				p, err = policies.GetLatest(env.ctx, *policyID)
			} else {
				p, err = policies.Get(env.ctx, *policyID, *policyRevision)
			}
			if errors.Is(err, sqlite.ErrKeyPolicyNotFound) {
				return errors.New("admin: key policy not found")
			}
			if err != nil {
				return errors.New("admin: cannot read key policy")
			}
			issued, err := keys.Create(env.ctx, sqlite.CreateVirtualKeyParams{PolicyID: p.ID, PolicyRevision: p.Revision})
			if err != nil {
				return errors.New("admin: cannot create virtual key")
			}
			printAdminIssuedKey(env.stdout, issued)
		case "get", "revoke":
			if len(args) != 2 || args[1] == "" {
				return fmt.Errorf("admin: key %s requires one ID", args[0])
			}
			var k sqlite.VirtualKey
			var err error
			if args[0] == "get" {
				k, err = keys.Get(env.ctx, args[1])
				if errors.Is(err, sqlite.ErrVirtualKeyNotFound) {
					k, err = keys.GetByKeyID(env.ctx, args[1])
				}
			} else {
				k, err = keys.Revoke(env.ctx, args[1])
			}
			if errors.Is(err, sqlite.ErrVirtualKeyNotFound) {
				return errors.New("admin: virtual key not found")
			}
			if err != nil {
				return errors.New("admin: cannot read or revoke virtual key")
			}
			printAdminVirtualKey(env.stdout, k)
		case "list":
			fs := adminFlags("key list")
			policy := fs.String("policy", "", "policy ID")
			enabledText := fs.String("enabled", "", "enabled filter")
			revokedText := fs.String("revoked", "", "revoked filter")
			if err := parseAdminFlags(fs, args[1:]); err != nil {
				return err
			}
			if fs.NArg() != 0 {
				return errors.New("admin: key list accepts no positional arguments")
			}
			filter := sqlite.VirtualKeyFilter{PolicyID: *policy}
			for name, text := range map[string]*string{"enabled": enabledText, "revoked": revokedText} {
				if wasAdminFlagSet(fs, name) {
					v, ok := parseAdminBool(*text)
					if !ok {
						return fmt.Errorf("admin: --%s must be true or false", name)
					}
					if name == "enabled" {
						filter.Enabled = &v
					} else {
						filter.Revoked = &v
					}
				}
			}
			list, err := keys.List(env.ctx, filter)
			if err != nil {
				return errors.New("admin: cannot list virtual keys")
			}
			for _, k := range list {
				printAdminVirtualKey(env.stdout, k)
			}
		case "update-policy":
			if len(args) < 2 || args[1] == "" {
				return errors.New("admin: key update-policy requires an ID")
			}
			fs := adminFlags("key update-policy")
			policy := fs.String("policy", "", "policy ID")
			policyRevision := fs.Int64("policy-revision", 0, "policy revision")
			expected := fs.Int64("expected-revision", 0, "expected key revision")
			if err := parseAdminFlags(fs, args[2:]); err != nil {
				return err
			}
			if fs.NArg() != 0 || *policy == "" || *policyRevision < 0 || *expected < 1 {
				return errors.New("admin: key update-policy requires --policy and --expected-revision")
			}
			var p sqlite.KeyPolicy
			var err error
			if *policyRevision == 0 {
				p, err = policies.GetLatest(env.ctx, *policy)
			} else {
				p, err = policies.Get(env.ctx, *policy, *policyRevision)
			}
			if errors.Is(err, sqlite.ErrKeyPolicyNotFound) {
				return errors.New("admin: key policy not found")
			}
			if err != nil {
				return errors.New("admin: cannot read key policy")
			}
			k, err := keys.UpdatePolicy(env.ctx, args[1], p.ID, p.Revision, *expected)
			if errors.Is(err, sqlite.ErrVirtualKeyRevisionMismatch) {
				return errors.New("admin: virtual key revision mismatch")
			}
			if errors.Is(err, sqlite.ErrVirtualKeyRevoked) {
				return errors.New("admin: virtual key revoked")
			}
			if errors.Is(err, sqlite.ErrVirtualKeyNotFound) {
				return errors.New("admin: virtual key not found")
			}
			if err != nil {
				return errors.New("admin: cannot update virtual key policy")
			}
			printAdminVirtualKey(env.stdout, k)
		default:
			return errors.New("admin: unknown key command")
		}
	}
	return nil
}

func parseAdminCSV(value string) ([]string, error) {
	if value == "" {
		return []string{}, nil
	}
	values := strings.Split(value, ",")
	for _, v := range values {
		if strings.TrimSpace(v) == "" {
			return nil, errors.New("admin: list values must not be empty")
		}
	}
	return values, nil
}
func parseAdminBool(value string) (bool, bool) {
	if value == "true" {
		return true, true
	}
	if value == "false" {
		return false, true
	}
	return false, false
}
func wasAdminFlagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}
func printAdminPolicy(w io.Writer, p sqlite.KeyPolicy) {
	models, _ := json.Marshal(p.Models)
	connectors, _ := json.Marshal(p.Connectors)
	_, _ = fmt.Fprintf(w, "id=%s revision=%d enabled=%t models=%s connectors=%s rpm=%d tpm=%d created_at=%s\n", p.ID, p.Revision, p.Enabled, models, connectors, p.RPM, p.TPM, p.CreatedAt.Format(time.RFC3339Nano))
}
func printAdminVirtualKey(w io.Writer, k sqlite.VirtualKey) {
	revokedAt := ""
	if k.RevokedAt != nil {
		revokedAt = k.RevokedAt.Format(time.RFC3339Nano)
	}
	_, _ = fmt.Fprintf(w, "id=%s key_id=%s policy_id=%s policy_revision=%d revision=%d enabled=%t revoked=%t created_at=%s revoked_at=%s\n", k.ID, k.KeyID, k.PolicyID, k.PolicyRevision, k.Revision, k.Enabled, k.Revoked, k.CreatedAt.Format(time.RFC3339Nano), revokedAt)
}
func printAdminIssuedKey(w io.Writer, k sqlite.IssuedVirtualKey) {
	_, _ = fmt.Fprintf(w, "secret=%s ", k.Secret)
	printAdminVirtualKey(w, k.VirtualKey)
}

func adminFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}
func parseAdminFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return errors.New("admin: invalid arguments")
	}
	return nil
}
func printAdminAccount(w io.Writer, a sqlite.Account) {
	_, _ = fmt.Fprintf(w, "id=%s connector=%s enabled=%t created_at=%s updated_at=%s\n", a.ID, a.Connector, a.Enabled, a.CreatedAt.Format(time.RFC3339Nano), a.UpdatedAt.Format(time.RFC3339Nano))
}
func printAdminCredential(w io.Writer, c sqlite.Credential) {
	_, _ = fmt.Fprintf(w, "id=%s account=%s revision=%d updated_at=%s\n", c.ID, c.AccountID, c.Revision, c.UpdatedAt.Format(time.RFC3339Nano))
}

func isHelp(arg string) bool { return arg == "--help" || arg == "-h" || arg == "help" }

func openAdminDatabase(path string, create bool) (*sql.DB, error) {
	flags := syscall.O_RDWR | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	fd, err := syscall.Open(path, flags, 0)
	if errors.Is(err, syscall.ENOENT) && create {
		fd, err = syscall.Open(path, flags|syscall.O_CREAT|syscall.O_EXCL, 0600)
	}
	if err != nil {
		return nil, errors.New("database file unavailable")
	}
	defer syscall.Close(fd)
	var opened syscall.Stat_t
	if err := syscall.Fstat(fd, &opened); err != nil || opened.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return nil, errors.New("database must be a regular file")
	}
	db, err := sqlite.Open(path)
	if err != nil {
		return nil, err
	}
	var current syscall.Stat_t
	if err := syscall.Lstat(path, &current); err != nil || current.Mode&syscall.S_IFMT != syscall.S_IFREG || current.Dev != opened.Dev || current.Ino != opened.Ino {
		_ = db.Close()
		return nil, errors.New("database path changed while opening")
	}
	return db, nil
}

func readAdminSecret(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, errors.New("admin: secret input unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(r, maxAdminSecret+1))
	if err != nil {
		zeroAdminSecret(data)
		return nil, errors.New("admin: cannot read secret input")
	}
	if len(data) > maxAdminSecret {
		zeroAdminSecret(data)
		return nil, errors.New("admin: secret input exceeds 64 KiB")
	}
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}
	return data, nil
}

func zeroAdminSecret(secret []byte) {
	// Best effort only: Go may retain copies outside this reachable slice.
	for i := range secret {
		secret[i] = 0
	}
}
