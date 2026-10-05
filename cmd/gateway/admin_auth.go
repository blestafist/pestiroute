package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func runAuthAdmin(env adminEnvironment, db *sql.DB, key secure.MasterKey, args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		_, _ = io.WriteString(env.stdout, "usage: gateway admin [--db PATH] [--master-key PATH] auth <start --account ID|continue --session ID|refresh --account ID>\n")
		return nil
	}
	if args[0] != "start" && args[0] != "continue" && args[0] != "refresh" {
		return errors.New("admin: unknown auth command")
	}
	fs := adminFlags("auth " + args[0])
	accountID, sessionID := fs.String("account", "", "account ID"), fs.String("session", "", "opaque continuation handle")
	if err := parseAdminFlags(fs, args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || args[0] == "continue" && (*sessionID == "" || *accountID != "") || args[0] != "continue" && (*accountID == "" || *sessionID != "") {
		return errors.New("admin: invalid auth command arguments")
	}
	accounts, credentials := sqlite.NewAccounts(db), sqlite.NewCredentials(db)
	registry := env.authRegistry
	if registry == nil {
		var err error
		registry, err = defaultAdminAuthRegistry(env.ctx, accounts, db, *accountID, args[0] == "continue", *sessionID)
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
	services := env.authServices
	if services == nil {
		services = adminAuthServicesFunc(func(core.AttemptScope) core.InvocationServices { return core.InvocationServices{} })
	}
	coordinator, err := core.NewAuthCoordinator(registry, store, services, 10*time.Minute)
	if err != nil {
		return errors.New("admin: authentication unavailable")
	}
	var session core.AuthSession
	switch args[0] {
	case "start":
		session, err = coordinator.Start(env.ctx, *accountID)
	case "continue":
		session, err = coordinator.Continue(env.ctx, *sessionID)
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
			return errors.New("admin: authentication persistence failed")
		}
		if errors.Is(err, core.ErrAuthUnavailable) {
			return errors.New("admin: authentication unsupported or unavailable")
		}
		if errors.Is(err, core.ErrAuthRevisionMismatch) {
			return errors.New("admin: authentication credential revision mismatch")
		}
		return errors.New("admin: authentication operation failed")
	}
	if session.ID == "" {
		_, _ = io.WriteString(env.stdout, "authentication complete\n")
		return nil
	}
	_, _ = fmt.Fprintf(env.stdout, "authentication requires continuation session=%s expires_at=%s\n", session.ID, session.ExpiresAt.UTC().Format(time.RFC3339))
	if action := session.UserAction; action != nil {
		_, _ = fmt.Fprintf(env.stdout, "verification_uri=%q user_code=%q interval_seconds=%g\n", action.VerificationURI, action.UserCode, action.PollInterval.Seconds())
	}
	return nil
}

func defaultAdminAuthRegistry(ctx context.Context, accounts *sqlite.Accounts, db *sql.DB, accountID string, continuing bool, sessionID string) (*core.Registry, error) {
	if continuing {
		if err := db.QueryRowContext(ctx, `SELECT account_id FROM auth_sessions WHERE id=? AND kind='interactive' AND lifecycle='active' AND expires_at>?`, sessionID, time.Now().UnixMilli()).Scan(&accountID); err != nil {
			return nil, errors.New("admin: authentication session unavailable")
		}
	}
	account, err := accounts.Get(ctx, accountID)
	if err != nil || account.Connector != "responses" {
		return nil, errors.New("admin: authentication connector is not configured")
	}
	registry, err := core.NewRegistry(map[core.ComponentKind]core.APIVersion{core.ComponentConnector: {Major: 1}}, nil)
	if err != nil {
		return nil, errors.New("admin: authentication unavailable")
	}
	connector := responses.NewConnector()
	if err := registry.Register("responses", connector, core.ComponentConnector); err != nil {
		return nil, errors.New("admin: authentication unavailable")
	}
	// The built-in native Connector's authentication operation is unsupported;
	// this inert loopback transport config permits honest capability invocation only.
	config := []byte(fmt.Sprintf(`{"model":"gpt-5.4-mini","account_id":%q,"transport":{"endpoint":"http://127.0.0.1:1"}}`, accountID))
	if err := registry.Init(ctx, "responses", core.ComponentConfig{Data: config}); err != nil {
		_ = registry.Close(context.Background())
		return nil, errors.New("admin: authentication unavailable")
	}
	return registry, nil
}

type adminAuthServicesFunc func(core.AttemptScope) core.InvocationServices

func (f adminAuthServicesFunc) ForAttempt(scope core.AttemptScope) core.InvocationServices {
	return f(scope)
}

func authCredentialRefs(ctx context.Context, db *sql.DB, accountID string, continuing bool, sessionID string) (map[string]map[string]string, error) {
	if continuing {
		err := db.QueryRowContext(ctx, `SELECT account_id FROM auth_sessions WHERE id=? AND kind='interactive' AND lifecycle='active' AND expires_at>?`, sessionID, time.Now().UnixMilli()).Scan(&accountID)
		if err != nil {
			return nil, errors.New("admin: authentication session unavailable")
		}
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
	return map[string]map[string]string{accountID: {"bearer": ids[0]}}, nil
}
