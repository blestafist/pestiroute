package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/blestafist/pestiroute/internal/connector/antigravity"
	"github.com/blestafist/pestiroute/internal/connector/claudecode"
	"github.com/blestafist/pestiroute/internal/connector/codex"
	"github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func runAuthAdmin(env adminEnvironment, db *sql.DB, key secure.MasterKey, args []string) error {
	debug := false
	if len(args) > 0 && args[0] == "--debug" {
		debug = true
		args = args[1:]
	}
	if len(args) == 0 || isHelp(args[0]) {
		_, _ = io.WriteString(env.stdout, "usage: gateway admin [--db PATH] [--master-key PATH] auth [--debug] <start --account ID|continue --session ID [--callback-stdin] [--allow-onboard]|refresh --account ID>\n  --callback-stdin reads a pasted authorization code or callback URL without echoing it\n  --allow-onboard opts in to experimental Antigravity managed-project onboarding\n")
		return nil
	}
	if args[0] != "start" && args[0] != "continue" && args[0] != "refresh" {
		return errors.New("admin: unknown auth command")
	}
	fs := adminFlags("auth " + args[0])
	accountID, sessionID := fs.String("account", "", "account ID"), fs.String("session", "", "opaque continuation handle")
	callbackStdin := fs.Bool("callback-stdin", false, "read a pasted authorization code or callback URL from stdin")
	allowOnboard := fs.Bool("allow-onboard", false, "allow experimental Antigravity managed-project onboarding")
	if err := parseAdminFlags(fs, args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || args[0] == "continue" && (*sessionID == "" || *accountID != "") || args[0] != "continue" && (*accountID == "" || *sessionID != "") || args[0] != "continue" && (*callbackStdin || *allowOnboard) {
		return errors.New("admin: invalid auth command arguments")
	}
	var callbackInput string
	if *callbackStdin {
		if env.stdin == nil {
			return errors.New("admin: invalid callback input")
		}
		input, err := bufio.NewReaderSize(io.LimitReader(env.stdin, 4098), 4098).ReadString('\n')
		if err != nil && err != io.EOF {
			return errors.New("admin: invalid callback input")
		}
		callbackInput = strings.TrimSuffix(input, "\n")
		callbackInput = strings.TrimSuffix(callbackInput, "\r")
		if len(callbackInput) > 4096 {
			return errors.New("admin: invalid callback input")
		}
		if debug {
			kind := "raw_code"
			if strings.Contains(callbackInput, "://") {
				kind = "callback_url"
			}
			_, _ = fmt.Fprintf(env.stderr, "authentication input: kind=%s length=%d\n", kind, len(callbackInput))
		}
	}
	accounts, credentials := sqlite.NewAccounts(db), sqlite.NewCredentials(db)
	registry := env.authRegistry
	if registry == nil {
		var err error
		registry, err = defaultAdminAuthRegistry(env.ctx, accounts, db, *accountID, args[0] == "continue", *sessionID, *allowOnboard)
		if err != nil {
			return err
		}
		defer registry.Close(context.Background())
	}
	refs, err := authCredentialRefs(env.ctx, db, *accountID, args[0] == "continue", *sessionID)
	if err != nil {
		return err
	}
	store, err := newSQLiteAuthCoordinatorStore(accounts, credentials, sqlite.NewAuthSessions(db), key, "v1", refs)
	if err != nil {
		return errors.New("admin: authentication unavailable")
	}
	var connectorID string
	useDefaultTransport := len(refs) > 0
	for targetAccountID := range refs {
		if err := db.QueryRowContext(env.ctx, `SELECT connector FROM accounts WHERE id=? AND enabled=1`, targetAccountID).Scan(&connectorID); err != nil {
			return errors.New("admin: authentication connector is not configured")
		}
		if !useDefaultAdminAuthTransport(connectorID) {
			useDefaultTransport = false
		}
	}
	if *allowOnboard && (!isAntigravityAuthConnector(connectorID) || args[0] != "continue") {
		return errors.New("admin: onboarding opt-in is only valid for Antigravity continue")
	}
	var transport core.HTTPDoer
	if useDefaultTransport {
		transport = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}
	if env.authServices != nil {
		if testServices := env.authServices.ForAttempt(core.AttemptScope{AccountID: *accountID}); testServices.Transport != nil {
			transport = testServices.Transport
		}
	}
	if isAntigravityAuthConnector(connectorID) && os.Getenv("PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT") == "1" {
		budgets := sqlite.NewOperatorRequestBudgets(db)
		if err := budgets.VerifyCeiling(env.ctx, sqlite.AntigravityOperatorBudgetID, 1000); err != nil {
			return errors.New("admin: request budget unavailable")
		}
		transport = core.BudgetedHTTPDoer{Next: transport, Budget: budgets, Account: sqlite.AntigravityOperatorBudgetID}
	}
	var authLogger *slog.Logger
	if debug {
		authLogger = slog.New(slog.NewTextHandler(env.stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	services := adminAuthServicesFunc(func(scope core.AttemptScope) core.InvocationServices {
		return core.InvocationServices{Transport: transport, Credentials: adminAuthCredentialAccess{store: store, accountID: scope.AccountID}, Logger: authLogger}
	})
	coordinator, err := core.NewAuthCoordinator(registry, store, services, 10*time.Minute)
	if err != nil {
		return errors.New("admin: authentication unavailable")
	}
	var session core.AuthSession
	switch args[0] {
	case "start":
		session, err = coordinator.Start(env.ctx, *accountID)
	case "continue":
		session, err = coordinator.Continue(env.ctx, *sessionID, callbackInput)
	case "refresh":
		var updated core.AuthCredentials
		updated, err = coordinator.Refresh(env.ctx, *accountID)
		if err == nil {
			_, _ = fmt.Fprintf(env.stdout, "authentication refreshed revision=%d\n", updated.Revision)
			return nil
		}
	}
	if err != nil {
		if errors.Is(err, core.ErrAuthPersistence) {
			if debug {
				_, _ = io.WriteString(env.stderr, "authentication diagnostic: reason=persistence_failed\n")
			}
			return errors.New("admin: authentication persistence failed")
		}
		if errors.Is(err, core.ErrAuthUnavailable) {
			if debug {
				var diagnostic *core.GatewayError
				if errors.As(err, &diagnostic) {
					status := ""
					if strings.HasPrefix(diagnostic.OriginalError, "HTTP status ") {
						status = " " + diagnostic.OriginalError
					}
					_, _ = fmt.Fprintf(env.stderr, "authentication diagnostic: reason=%s%s\n", diagnostic.Code, status)
				}
			}
			return errors.New("admin: authentication unsupported or unavailable")
		}
		if errors.Is(err, core.ErrAuthRevisionMismatch) {
			if debug {
				_, _ = io.WriteString(env.stderr, "authentication diagnostic: reason=revision_mismatch\n")
			}
			return errors.New("admin: authentication credential revision mismatch")
		}
		if errors.Is(err, context.Canceled) {
			if debug {
				_, _ = io.WriteString(env.stderr, "authentication diagnostic: reason=cancelled\n")
			}
			return errors.New("admin: authentication operation failed")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			if debug {
				_, _ = io.WriteString(env.stderr, "authentication diagnostic: reason=deadline_exceeded\n")
			}
			return errors.New("admin: authentication operation failed")
		}
		return errors.New("admin: authentication operation failed")
	}
	if session.ID == "" {
		_, _ = io.WriteString(env.stdout, "authentication complete\n")
		return nil
	}
	_, _ = fmt.Fprintf(env.stdout, "authentication requires continuation session=%s expires_at=%s\n", session.ID, session.ExpiresAt.UTC().Format(time.RFC3339))
	if action := session.UserAction; action != nil {
		if action.UserCode == "" && action.PollInterval == 0 {
			_, _ = fmt.Fprintf(env.stdout, "verification_uri=%q\n", action.VerificationURI)
		} else {
			_, _ = fmt.Fprintf(env.stdout, "verification_uri=%q user_code=%q interval_seconds=%g\n", action.VerificationURI, action.UserCode, action.PollInterval.Seconds())
		}
	}
	return nil
}

func defaultAdminAuthRegistry(ctx context.Context, accounts *sqlite.Accounts, db *sql.DB, accountID string, continuing bool, sessionID string, allowOnboard bool) (*core.Registry, error) {
	if continuing {
		if err := db.QueryRowContext(ctx, `SELECT account_id FROM auth_sessions WHERE id=? AND kind='interactive' AND lifecycle='active' AND expires_at>?`, sessionID, time.Now().UnixMilli()).Scan(&accountID); err != nil {
			return nil, errors.New("admin: authentication session unavailable")
		}
	}
	account, err := accounts.Get(ctx, accountID)
	if err != nil || !account.Enabled || account.ID != accountID || (account.Connector != "responses" && account.Connector != "codex" && !isClaudeCodeAuthConnector(account.Connector) && !isAntigravityAuthConnector(account.Connector)) {
		return nil, errors.New("admin: authentication connector is not configured")
	}
	registry, err := core.NewRegistry(map[core.ComponentKind]core.APIVersion{core.ComponentConnector: {Major: 1}}, nil)
	if err != nil {
		return nil, errors.New("admin: authentication unavailable")
	}
	var connector core.Connector
	var config []byte
	if account.Connector == "codex" {
		connector = codex.NewConnector()
		config = fmt.Appendf(nil, `{"model":"gpt-5.4-mini","account_id":%q,"profile":"codex-responses-http-sse-v1"}`, accountID)
	} else if isClaudeCodeAuthConnector(account.Connector) {
		connector = claudecode.NewConnector()
		config = []byte(`{"profile":"claude-code-messages-v1","oauth":{"authorization_endpoint":"https://auth.synthetic.invalid/oauth/authorize","token_endpoint":"https://token.synthetic.invalid/oauth/token","redirect_uri":"http://127.0.0.1:9999/callback","client_id":"synthetic-client","scope":"synthetic:offline"}}`)
	} else if isAntigravityAuthConnector(account.Connector) {
		connector = antigravity.NewConnector()
		oauth := map[string]any{}
		for key, envName := range map[string]string{"client_id": "PESTIROUTE_ANTIGRAVITY_CLIENT_ID", "client_secret": "PESTIROUTE_ANTIGRAVITY_CLIENT_SECRET", "auth_endpoint": "PESTIROUTE_ANTIGRAVITY_AUTH_ENDPOINT", "token_endpoint": "PESTIROUTE_ANTIGRAVITY_TOKEN_ENDPOINT", "redirect_uri": "PESTIROUTE_ANTIGRAVITY_REDIRECT_URI"} {
			if value := os.Getenv(envName); value != "" {
				oauth[key] = value
			}
		}
		if scopes := strings.Fields(os.Getenv("PESTIROUTE_ANTIGRAVITY_SCOPES")); len(scopes) > 0 {
			oauth["scopes"] = scopes
		}
		cfg := map[string]any{"profile": "antigravity-v1internal", "project": "admin-auth", "allow_onboard": allowOnboard}
		if len(oauth) > 0 {
			cfg["oauth"] = oauth
		}
		config, _ = json.Marshal(cfg)
	} else {
		connector = responses.NewConnector()
		// The built-in native Connector's authentication operation is unsupported;
		// this inert loopback transport config permits honest capability invocation only.
		config = fmt.Appendf(nil, `{"model":"gpt-5.4-mini","account_id":%q,"transport":{"endpoint":"http://127.0.0.1:1"}}`, accountID)
	}
	if err := registry.Register(core.InstanceID(account.Connector), connector, core.ComponentConnector); err != nil {
		return nil, errors.New("admin: authentication unavailable")
	}
	if err := registry.Init(ctx, core.InstanceID(account.Connector), core.ComponentConfig{Data: config}); err != nil {
		_ = registry.Close(context.Background())
		return nil, errors.New("admin: authentication unavailable")
	}
	return registry, nil
}

type adminAuthServicesFunc func(core.AttemptScope) core.InvocationServices

func (f adminAuthServicesFunc) ForAttempt(scope core.AttemptScope) core.InvocationServices {
	return f(scope)
}

type adminAuthCredentialAccess struct {
	store     *sqliteAuthCoordinatorStore
	accountID string
}

func (a adminAuthCredentialAccess) Get(ctx context.Context, name string) ([]byte, error) {
	if a.store == nil || a.accountID == "" || len(a.store.refs[a.accountID]) != 1 {
		return nil, core.ErrCredentialUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	credentials, err := a.store.AuthCredentials(ctx, a.accountID)
	if err != nil {
		return nil, core.ErrCredentialUnavailable
	}
	value := credentials.Values[name]
	if len(value) == 0 {
		return nil, core.ErrCredentialUnavailable
	}
	return append([]byte(nil), value...), nil
}

func authCredentialRefs(ctx context.Context, db *sql.DB, accountID string, continuing bool, sessionID string) (map[string]map[string]string, error) {
	credentialName := "bearer"
	if continuing {
		err := db.QueryRowContext(ctx, `SELECT account_id FROM auth_sessions WHERE id=? AND kind='interactive' AND lifecycle='active' AND expires_at>?`, sessionID, time.Now().UnixMilli()).Scan(&accountID)
		if err != nil {
			return nil, errors.New("admin: authentication session unavailable")
		}
	}
	var connector string
	if err := db.QueryRowContext(ctx, `SELECT connector FROM accounts WHERE id=? AND enabled=1`, accountID).Scan(&connector); err != nil {
		return nil, errors.New("admin: authentication connector is not configured")
	}
	if connector == "codex" || isClaudeCodeAuthConnector(connector) || isAntigravityAuthConnector(connector) {
		credentialName = "oauth"
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM credentials WHERE account_id=? ORDER BY id`, accountID)
	if err != nil {
		return nil, errors.New("admin: authentication credentials unavailable")
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.New("admin: authentication credentials unavailable")
		}
		ids = append(ids, id)
	}
	if rows.Err() != nil || len(ids) != 1 {
		return nil, errors.New("admin: authentication requires exactly one configured credential")
	}
	return map[string]map[string]string{accountID: {credentialName: ids[0]}}, nil
}

func isClaudeCodeAuthConnector(connector string) bool {
	return connector == "claudecode" || connector == "pestiroute.claudecode.messages"
}

func isAntigravityAuthConnector(connector string) bool {
	return connector == "antigravity" || connector == "pestiroute.antigravity.v1internal"
}

func useDefaultAdminAuthTransport(connector string) bool {
	if isClaudeCodeAuthConnector(connector) {
		return false
	}
	if isAntigravityAuthConnector(connector) {
		return os.Getenv("PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT") == "1"
	}
	return true
}
