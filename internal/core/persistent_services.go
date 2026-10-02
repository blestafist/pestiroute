package core

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

var ErrAccountUnavailable = errors.New("account unavailable")

type RuntimeAccount struct{ Enabled bool }

type RuntimeAccountReader interface {
	GetAccount(context.Context, string) (RuntimeAccount, error)
}

type RuntimeCredential struct {
	Value     []byte
	ExpiresAt *time.Time
}

type RuntimeCredentialReader interface {
	GetCredential(context.Context, string, string) (RuntimeCredential, error)
}

// PersistentServices resolves only the credential references configured for
// each selected account. Storage and decryption implementations remain outside Core.
type PersistentServices struct {
	accounts    RuntimeAccountReader
	credentials RuntimeCredentialReader
	refs        map[string]map[string]string
	transports  map[string]HTTPDoer
	logger      *slog.Logger
}

func NewPersistentServices(accounts RuntimeAccountReader, credentials RuntimeCredentialReader, refs map[string]map[string]string, transports map[string]HTTPDoer, logger *slog.Logger) *PersistentServices {
	refCopy := make(map[string]map[string]string, len(refs))
	for account, values := range refs {
		refCopy[account] = make(map[string]string, len(values))
		for name, id := range values {
			refCopy[account][name] = id
		}
	}
	transportCopy := make(map[string]HTTPDoer, len(transports))
	for account, transport := range transports {
		transportCopy[account] = transport
	}
	return &PersistentServices{accounts: accounts, credentials: credentials, refs: refCopy, transports: transportCopy, logger: logger}
}

func (p *PersistentServices) ForAttempt(scope AttemptScope) InvocationServices {
	secrets := newSecretSet(nil)
	var logger *slog.Logger
	if p.logger != nil {
		logger = slog.New(redactingHandler{next: p.logger.Handler(), secrets: secrets})
	}
	transport := p.transports[scope.AccountID]
	if transport == nil {
		transport = unavailableDoer{}
	}
	return InvocationServices{Credentials: &persistentCredentialAccess{
		accountID: scope.AccountID, refs: p.refs[scope.AccountID], accounts: p.accounts,
		credentials: p.credentials, secrets: secrets, resolved: make(map[string][]byte), lock: make(chan struct{}, 1),
	}, Transport: transport, Logger: logger}
}

type persistentCredentialAccess struct {
	accountID   string
	refs        map[string]string
	accounts    RuntimeAccountReader
	credentials RuntimeCredentialReader
	secrets     *secretSet
	resolved    map[string][]byte
	lock        chan struct{}
}

func (a *persistentCredentialAccess) Get(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case a.lock <- struct{}{}:
	}
	defer func() { <-a.lock }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if value, ok := a.resolved[name]; ok {
		return append([]byte(nil), value...), nil
	}
	if a.accounts == nil {
		return nil, ErrAccountUnavailable
	}
	account, err := a.accounts.GetAccount(ctx, a.accountID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrAccountUnavailable
	}
	if !account.Enabled {
		return nil, ErrAccountUnavailable
	}
	id, ok := a.refs[name]
	if !ok || id == "" || a.credentials == nil {
		return nil, ErrCredentialUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	credential, err := a.credentials.GetCredential(ctx, a.accountID, id)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrCredentialUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if credential.ExpiresAt != nil && !credential.ExpiresAt.After(time.Now()) {
		return nil, ErrCredentialUnavailable
	}
	if len(credential.Value) == 0 {
		return nil, ErrCredentialUnavailable
	}
	value := append([]byte(nil), credential.Value...)
	a.secrets.add(value)
	a.resolved[name] = value
	return append([]byte(nil), value...), nil
}
