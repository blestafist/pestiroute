package core

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type testAccountReader struct {
	mu       sync.Mutex
	accounts map[string]RuntimeAccount
	err      error
}

func (r *testAccountReader) GetAccount(ctx context.Context, id string) (RuntimeAccount, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeAccount{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return RuntimeAccount{}, r.err
	}
	account, ok := r.accounts[id]
	if !ok {
		return RuntimeAccount{}, ErrAccountUnavailable
	}
	return account, nil
}

type testCredentialReader struct {
	mu     sync.Mutex
	values map[string]RuntimeCredential
	err    error
}

type gatedCredentialReader struct {
	entered chan struct{}
	release chan struct{}
}

func (r gatedCredentialReader) GetCredential(context.Context, string, string) (RuntimeCredential, error) {
	close(r.entered)
	<-r.release
	return RuntimeCredential{Value: []byte("value")}, nil
}

func (r *testCredentialReader) GetCredential(ctx context.Context, account, id string) (RuntimeCredential, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeCredential{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return RuntimeCredential{}, r.err
	}
	value, ok := r.values[account+"/"+id]
	if !ok {
		return RuntimeCredential{}, ErrCredentialUnavailable
	}
	return value, nil
}

func TestPersistentServicesScopeExpiryRefreshAndCopy(t *testing.T) {
	accounts := &testAccountReader{accounts: map[string]RuntimeAccount{"a": {Enabled: true}, "b": {Enabled: true}}}
	credentials := &testCredentialReader{values: map[string]RuntimeCredential{
		"a/id-a": {Value: []byte("secret-a")}, "b/id-b": {Value: []byte("secret-b")},
		"a/expired": {Value: []byte("expired"), ExpiresAt: timeRef(time.Now().Add(-time.Second))},
	}}
	var logs bytes.Buffer
	provider := NewPersistentServices(accounts, credentials,
		map[string]map[string]string{"a": {"token": "id-a", "old": "expired"}, "b": {"token": "id-b"}}, nil,
		slog.New(slog.NewTextHandler(&logs, nil)))
	serviceA := provider.ForAttempt(AttemptScope{AccountID: "a"})
	first, err := serviceA.Credentials.Get(context.Background(), "token")
	if err != nil || string(first) != "secret-a" {
		t.Fatalf("credential = %q, %v", first, err)
	}
	first[0] = 'X'
	second, err := serviceA.Credentials.Get(context.Background(), "token")
	if err != nil || string(second) != "secret-a" {
		t.Fatalf("cached credential = %q, %v", second, err)
	}
	if _, err := serviceA.Credentials.Get(context.Background(), "id-b"); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("unreferenced credential error = %v", err)
	}
	if _, err := serviceA.Credentials.Get(context.Background(), "old"); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("expired credential error = %v", err)
	}
	serviceA.Logger.WithGroup("scope").Info("diagnostic", "message", "secret-a", slog.Group("inner", "token", "secret-a"))
	if strings.Contains(logs.String(), "secret-a") || !strings.Contains(logs.String(), "[REDACTED]") {
		t.Fatalf("secret leaked: %s", logs.String())
	}

	accounts.mu.Lock()
	accounts.accounts["a"] = RuntimeAccount{Enabled: false}
	accounts.mu.Unlock()
	if _, err := provider.ForAttempt(AttemptScope{AccountID: "a"}).Credentials.Get(context.Background(), "token"); !errors.Is(err, ErrAccountUnavailable) {
		t.Fatalf("disabled account error = %v", err)
	}
	accounts.mu.Lock()
	accounts.accounts["a"] = RuntimeAccount{Enabled: true}
	accounts.mu.Unlock()
	credentials.mu.Lock()
	credentials.values["a/id-a"] = RuntimeCredential{Value: []byte("secret-new")}
	credentials.mu.Unlock()
	newInvocation := provider.ForAttempt(AttemptScope{AccountID: "a"})
	updated, err := newInvocation.Credentials.Get(context.Background(), "token")
	if err != nil || string(updated) != "secret-new" {
		t.Fatalf("updated invocation credential = %q, %v", updated, err)
	}
	if value, err := serviceA.Credentials.Get(context.Background(), "token"); err != nil || string(value) != "secret-a" {
		t.Fatalf("in-flight snapshot changed = %q, %v", value, err)
	}
}

func TestPersistentServicesFailClosedAndHonorCancellation(t *testing.T) {
	accounts := &testAccountReader{accounts: map[string]RuntimeAccount{"disabled": {Enabled: false}, "enabled": {Enabled: true}}}
	credentials := &testCredentialReader{values: map[string]RuntimeCredential{"enabled/id": {Value: []byte("ok")}}}
	provider := NewPersistentServices(accounts, credentials, map[string]map[string]string{"enabled": {"token": "id"}}, nil, nil)
	if _, err := provider.ForAttempt(AttemptScope{AccountID: "missing"}).Credentials.Get(context.Background(), "token"); !errors.Is(err, ErrAccountUnavailable) {
		t.Fatalf("missing account error = %v", err)
	}
	if _, err := provider.ForAttempt(AttemptScope{AccountID: "disabled"}).Credentials.Get(context.Background(), "token"); !errors.Is(err, ErrAccountUnavailable) {
		t.Fatalf("disabled account error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.ForAttempt(AttemptScope{AccountID: "enabled"}).Credentials.Get(cancelled, "token"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled lookup = %v", err)
	}
	credentials.err = errors.New("sensitive storage diagnostic")
	if _, err := provider.ForAttempt(AttemptScope{AccountID: "enabled"}).Credentials.Get(context.Background(), "token"); !errors.Is(err, ErrCredentialUnavailable) || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("decryption error was not sanitized: %v", err)
	}
}

func TestPersistentServicesCancellationWhileLookupIsBusy(t *testing.T) {
	reader := gatedCredentialReader{entered: make(chan struct{}), release: make(chan struct{})}
	provider := NewPersistentServices(&testAccountReader{accounts: map[string]RuntimeAccount{"a": {Enabled: true}}}, reader,
		map[string]map[string]string{"a": {"token": "id"}}, nil, nil)
	access := provider.ForAttempt(AttemptScope{AccountID: "a"}).Credentials
	firstDone := make(chan error, 1)
	go func() { _, err := access.Get(context.Background(), "token"); firstDone <- err }()
	<-reader.entered
	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() { _, err := access.Get(ctx, "token"); secondDone <- err }()
	cancel()
	select {
	case err := <-secondDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting lookup error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled lookup remained blocked behind another resolution")
	}
	close(reader.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first lookup error = %v", err)
	}
}

func timeRef(value time.Time) *time.Time { return &value }
