package core

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestEnvironmentServicesAreAccountScopedCancellableAndRedacted(t *testing.T) {
	t.Setenv("PESTI_TEST_ACCOUNT_A", "synthetic-secret-a")
	t.Setenv("PESTI_TEST_ACCOUNT_B", "synthetic-secret-b")
	var logs bytes.Buffer
	provider := NewEnvironmentServices(map[string]map[string]string{
		"account-a": {"token": "PESTI_TEST_ACCOUNT_A"},
		"account-b": {"token": "PESTI_TEST_ACCOUNT_B"},
	}, nil, slog.New(slog.NewTextHandler(&logs, nil)))

	servicesA := provider.ForAttempt(AttemptScope{AccountID: "account-a"})
	servicesB := provider.ForAttempt(AttemptScope{AccountID: "account-b"})
	var wg sync.WaitGroup
	for _, test := range []struct {
		services InvocationServices
		want     string
	}{{servicesA, "synthetic-secret-a"}, {servicesB, "synthetic-secret-b"}} {
		wg.Add(1)
		go func(services InvocationServices, want string) {
			defer wg.Done()
			got, err := services.Credentials.Get(context.Background(), "token")
			if err != nil || string(got) != want {
				t.Errorf("credential = %q, %v; want selected account value", got, err)
			}
			if _, err := services.Credentials.Get(context.Background(), "account-b-token"); !errors.Is(err, ErrCredentialUnavailable) {
				t.Errorf("cross-account credential error = %v", err)
			}
			services.Logger.Info("diagnostic", "detail", want, "error", errors.New(want), "list", []string{want}, "key-"+want, "value")
		}(test.services, test.want)
	}
	wg.Wait()
	if strings.Contains(logs.String(), "synthetic-secret-") || !strings.Contains(logs.String(), "[REDACTED]") {
		t.Fatalf("logger failed secret redaction: %s", logs.String())
	}
	servicesA.Logger.WithGroup("group-synthetic-secret-a").Info("resolved", slog.Any("value", resolvedSecret("synthetic-secret-a")))
	if strings.Contains(logs.String(), "synthetic-secret-") {
		t.Fatalf("logger leaked resolved value or group name: %s", logs.String())
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := servicesA.Credentials.Get(cancelled, "token"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled credential lookup error = %v", err)
	}
	if _, err := servicesA.Credentials.Get(context.Background(), "missing"); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("missing credential error = %v", err)
	}
}

type resolvedSecret string

func (s resolvedSecret) LogValue() slog.Value { return slog.StringValue(string(s)) }

func TestEnvironmentServicesMissingCredentialIsExplicit(t *testing.T) {
	provider := NewEnvironmentServices(map[string]map[string]string{"account": {"token": "PESTI_TEST_UNSET"}}, nil, nil)
	services := provider.ForAttempt(AttemptScope{AccountID: "account"})
	if _, err := services.Credentials.Get(context.Background(), "token"); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("unset credential error = %v", err)
	}
	if _, err := services.Transport.Do(nil); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("missing-account transport error = %v", err)
	}
}

type contextCheckingDoer struct{ observed chan struct{} }

func (d contextCheckingDoer) Do(request *http.Request) (*http.Response, error) {
	<-request.Context().Done()
	close(d.observed)
	return nil, request.Context().Err()
}

func TestInvocationTransportReceivesRequestCancellation(t *testing.T) {
	observed := make(chan struct{})
	services := NewEnvironmentServices(nil, map[string]HTTPDoer{"account": contextCheckingDoer{observed}}, nil).
		ForAttempt(AttemptScope{AccountID: "account"})
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := services.Transport.Do(request); !errors.Is(err, context.Canceled) {
		t.Fatalf("transport error = %v", err)
	}
	select {
	case <-observed:
	default:
		t.Fatal("transport did not observe request cancellation")
	}
}

func TestInvocationCredentialAndLoggerShareSnapshot(t *testing.T) {
	t.Setenv("PESTI_TEST_SNAPSHOT", "sek-123")
	var logs bytes.Buffer
	services := NewEnvironmentServices(map[string]map[string]string{"account": {"token": "PESTI_TEST_SNAPSHOT"}}, nil,
		slog.New(slog.NewTextHandler(&logs, nil))).ForAttempt(AttemptScope{AccountID: "account"})
	t.Setenv("PESTI_TEST_SNAPSHOT", "sek-mutated")
	got, err := services.Credentials.Get(context.Background(), "token")
	if err != nil || string(got) != "sek-123" {
		t.Fatalf("snapshot credential = %q, %v", got, err)
	}
	services.Logger.Info("credential", "value", string(got))
	if strings.Contains(logs.String(), "sek-123") || !strings.Contains(logs.String(), "[REDACTED]") {
		t.Fatalf("snapshot logger leaked returned credential: %s", logs.String())
	}
}

type namedCredentialBytes []byte

func TestCredentialBytesRedactedByTextAndJSONHandlers(t *testing.T) {
	const secret = "credential-bytes-sek-123"
	t.Setenv("PESTI_TEST_BYTE_SECRET", secret)
	for _, test := range []struct {
		name    string
		handler func(*bytes.Buffer) slog.Handler
	}{
		{"text", func(out *bytes.Buffer) slog.Handler { return slog.NewTextHandler(out, nil) }},
		{"json", func(out *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(out, nil) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			services := NewEnvironmentServices(map[string]map[string]string{
				"account": {"token": "PESTI_TEST_BYTE_SECRET"},
			}, nil, slog.New(test.handler(&output))).ForAttempt(AttemptScope{AccountID: "account"})
			credential, err := services.Credentials.Get(context.Background(), "token")
			if err != nil {
				t.Fatal(err)
			}
			services.Logger.Info("credential", slog.Any("bytes", credential), slog.Any("named_bytes", namedCredentialBytes(credential)))
			if strings.Contains(output.String(), secret) || !strings.Contains(output.String(), "[REDACTED]") {
				t.Fatalf("credential bytes leaked or were not redacted: %s", output.String())
			}
		})
	}
}
