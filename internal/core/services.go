package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
)

var ErrCredentialUnavailable = errors.New("credential unavailable")
var ErrServiceUnavailable = errors.New("invocation service unavailable")

// EnvironmentServices creates invocation-bound services from runtime-owned
// account and credential mappings. The maps are copied during construction.
type EnvironmentServices struct {
	credentials map[string]map[string]string
	transports  map[string]HTTPDoer
	logger      *slog.Logger
}

func NewEnvironmentServices(credentials map[string]map[string]string, transports map[string]HTTPDoer, logger *slog.Logger) *EnvironmentServices {
	accounts := make(map[string]map[string]string, len(credentials))
	for account, refs := range credentials {
		accounts[account] = make(map[string]string, len(refs))
		for name, env := range refs {
			accounts[account][name] = env
		}
	}
	clients := make(map[string]HTTPDoer, len(transports))
	for account, transport := range transports {
		clients[account] = transport
	}
	return &EnvironmentServices{credentials: accounts, transports: clients, logger: logger}
}

// ForAttempt binds services to the runtime-selected account; connectors get no
// provider handle and cannot select another account through client metadata.
func (p *EnvironmentServices) ForAttempt(scope AttemptScope) InvocationServices {
	refs := p.credentials[scope.AccountID]
	valuesByName := make(map[string][]byte, len(refs))
	values := make([]string, 0, len(refs))
	for name, env := range refs {
		if value, ok := os.LookupEnv(env); ok && value != "" {
			valuesByName[name] = []byte(value)
			values = append(values, value)
		}
	}
	var logger *slog.Logger
	if p.logger != nil {
		logger = slog.New(redactingHandler{next: p.logger.Handler(), secrets: newSecretSet(values)})
	}
	transport := p.transports[scope.AccountID]
	if transport == nil {
		transport = unavailableDoer{}
	}
	return InvocationServices{
		Credentials: environmentCredentialAccess{values: valuesByName},
		Transport:   transport,
		Logger:      logger,
	}
}

type environmentCredentialAccess struct{ values map[string][]byte }

func (a environmentCredentialAccess) Get(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, ok := a.values[name]
	if !ok {
		return nil, ErrCredentialUnavailable
	}
	return append([]byte(nil), value...), nil
}

type redactingHandler struct {
	next    slog.Handler
	secrets *secretSet
}

func (h redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	var attrs []slog.Attr
	record.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, redactAttr(attr, h.redact))
		return true
	})
	record = slog.NewRecord(record.Time, record.Level, h.redact(record.Message), record.PC)
	record.AddAttrs(attrs...)
	return h.next.Handle(ctx, record)
}

func (h redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i := range attrs {
		redacted[i] = redactAttr(attrs[i], h.redact)
	}
	return redactingHandler{next: h.next.WithAttrs(redacted), secrets: h.secrets}
}

func (h redactingHandler) WithGroup(name string) slog.Handler {
	return redactingHandler{next: h.next.WithGroup(h.redact(name)), secrets: h.secrets}
}

func (h redactingHandler) redact(value string) string {
	for _, secret := range h.secrets.values() {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}

type secretSet struct {
	mu         sync.RWMutex
	valuesList []string
}

func newSecretSet(values []string) *secretSet {
	return &secretSet{valuesList: append([]string(nil), values...)}
}

func (s *secretSet) add(value []byte) {
	if len(value) == 0 {
		return
	}
	s.mu.Lock()
	s.valuesList = append(s.valuesList, string(value))
	s.mu.Unlock()
}

func (s *secretSet) values() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.valuesList...)
}

func redactAttr(attr slog.Attr, redact func(string) string) slog.Attr {
	attr.Key = redact(attr.Key)
	attr.Value = attr.Value.Resolve()
	if attr.Value.Kind() == slog.KindString {
		attr.Value = slog.StringValue(redact(attr.Value.String()))
	} else if attr.Value.Kind() == slog.KindGroup {
		group := attr.Value.Group()
		for i := range group {
			group[i] = redactAttr(group[i], redact)
		}
		attr.Value = slog.GroupValue(group...)
	} else if attr.Value.Kind() == slog.KindAny {
		any := attr.Value.Any()
		value := fmt.Sprint(any)
		byteValue := reflect.ValueOf(any)
		if byteValue.IsValid() && byteValue.Kind() == reflect.Slice && byteValue.Type().Elem().Kind() == reflect.Uint8 {
			bytes := make([]byte, byteValue.Len())
			for i := range bytes {
				bytes[i] = byte(byteValue.Index(i).Uint())
			}
			value = string(bytes)
		}
		if redacted := redact(value); redacted != value {
			attr.Value = slog.StringValue(redacted)
		}
	}
	return attr
}

type unavailableDoer struct{}

func (unavailableDoer) Do(*http.Request) (*http.Response, error) {
	return nil, ErrServiceUnavailable
}
