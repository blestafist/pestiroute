package main

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func prepareProtectedConfig(ctx context.Context, c config) (config, error) {
	p := c.protected
	key, err := secure.LoadMasterKey(p.Secrets.MasterKeyFile)
	if err != nil {
		return config{}, errors.New("invalid runtime master key file")
	}
	lock, err := sqlite.AcquireProcessLock(p.Storage.Path)
	if err != nil {
		return config{}, errors.New("cannot acquire gateway database lock")
	}
	fail := func(dbClose func() error, cause error) (config, error) {
		if dbClose != nil {
			cause = errors.Join(cause, dbClose())
		}
		cause = errors.Join(cause, lock.Close())
		return config{}, cause
	}
	db, err := sqlite.Open(p.Storage.Path)
	if err != nil {
		return fail(nil, errors.New("cannot open runtime database"))
	}
	version, err := sqlite.SchemaVersion(ctx, db)
	if err != nil || version != sqlite.CurrentSchemaVersion() {
		return fail(db.Close, errors.New("runtime database is not fully migrated"))
	}
	if _, err := sqlite.NewLedger(db).Recover(ctx); err != nil {
		return fail(db.Close, fmt.Errorf("recover runtime database: %w", err))
	}
	accounts := sqlite.NewAccounts(db)
	policies := sqlite.NewKeyPolicies(db)
	for _, route := range p.Routes {
		for _, target := range route.Targets {
			account, err := accounts.Get(ctx, target.Account)
			if err != nil || !account.Enabled || account.Connector != target.Connector {
				return fail(db.Close, fmt.Errorf("route %q references an unavailable account", route.ID))
			}
		}
		policy, err := policies.GetLatest(ctx, p.Policies[route.Policy])
		if err != nil || !policy.Enabled || !slices.Contains(policy.Models, route.Model) {
			return fail(db.Close, fmt.Errorf("route %q references an incompatible policy", route.ID))
		}
		for _, target := range route.Targets {
			if !slices.Contains(policy.Connectors, target.Connector) {
				return fail(db.Close, fmt.Errorf("route %q references an incompatible policy", route.ID))
			}
		}
	}
	for _, id := range p.Policies {
		policy, err := policies.GetLatest(ctx, id)
		if err != nil || !policy.Enabled {
			return fail(db.Close, fmt.Errorf("configured policy %q is unavailable", id))
		}
	}

	components := make([]topologyComponent, 0, len(p.Connectors))
	for _, item := range p.Connectors {
		s := item.Settings
		components = append(components, topologyComponent{ID: core.InstanceID(item.ID), Implementation: item.Implementation, Kind: core.ComponentConnector,
			Endpoint: s.BaseURL, CredentialEnv: s.CredentialEnv, MaxBodyBytes: s.MaxRequestBodyBytes, MaxHeaderBytes: s.MaxRequestHeaderBytes,
			ConnectTimeout: s.ConnectTimeout, TLSTimeout: s.TLSHandshakeTimeout, HeaderTimeout: s.ResponseHeaderTimeout, IdleTimeout: s.StreamIdleTimeout})
	}
	adapterAdded := false
	routes := make([]topologyRoute, 0, len(p.Routes))
	for _, route := range p.Routes {
		if len(route.Targets) != 1 {
			return fail(db.Close, fmt.Errorf("route %q requires one target until bounded fallback is implemented", route.ID))
		}
		if !adapterAdded {
			components = append(components, topologyComponent{ID: "responses-adapter", Implementation: "pestiroute.responses.native", Kind: core.ComponentAdapter})
			adapterAdded = true
		}
		target := route.Targets[0]
		connector := components[0]
		for _, candidate := range components {
			if candidate.ID == core.InstanceID(target.Connector) {
				connector = candidate
				break
			}
		}
		if p.Server.MaxRequestBytes < connector.MaxBodyBytes {
			connector.MaxBodyBytes = p.Server.MaxRequestBytes
			for i := range components {
				if components[i].ID == connector.ID {
					components[i].MaxBodyBytes = connector.MaxBodyBytes
				}
			}
		}
		budget := core.RouteBudget{UnknownEstimate: route.Budget.UnknownEstimate}
		if route.Budget.ConservativeTokens != nil {
			budget.ConservativeTokens = *route.Budget.ConservativeTokens
		}
		routes = append(routes, topologyRoute{Protocol: route.Protocol, Mode: core.ModeNative, Model: route.Model, Account: target.Account,
			Adapter: "responses-adapter", Connector: core.InstanceID(target.Connector), Budget: budget, BudgetPolicy: route.Budget.UnknownEstimate, RouteID: route.ID})
	}
	c.DatabasePath, c.MasterKeyFile = p.Storage.Path, p.Secrets.MasterKeyFile
	c.Listen, c.ShutdownTimeout = p.Server.Listen, p.Server.ShutdownTimeout
	c.Components, c.Routes, c.Credentials = components, routes, nil
	c.keyStore = sqliteVirtualKeyStore{keys: sqlite.NewVirtualKeys(db)}
	c.policyStore = sqlitePolicyStore{policies: policies}
	c.accountAuthorizer = sqliteAccountAuthorizer{accounts: accounts}
	c.runtimeDB, c.runtimeKey, c.processLock = db, key, lock
	c.protected = nil
	return c, nil
}
