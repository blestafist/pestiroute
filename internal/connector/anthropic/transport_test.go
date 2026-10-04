package anthropic

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/blestafist/pestiroute/internal/core"
)

type credentialStub string

func (c credentialStub) Get(_ context.Context, name string) ([]byte, error) {
	if name != "bearer" && name != "api_key" {
		return nil, errors.New("unexpected credential name")
	}
	return []byte(c), nil
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func testTransportWithBaseURL(base string) *Transport {
	endpoint, err := testMessagesEndpointForBase(base)
	return newTransport(endpoint, err)
}

func testMessagesEndpointForBase(base string) (string, error) {
	if base == "" {
		return messagesEndpoint, nil
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(base, "#") || u.Opaque != "" {
		return "", errors.New("invalid ANTHROPIC_BASE_URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return "", errors.New("invalid ANTHROPIC_BASE_URL")
	}
	p := strings.TrimSuffix(u.EscapedPath(), "/")
	if strings.Contains(p, "%") || (p != "" && path.Clean(p) != p) || strings.Contains(p, "..") {
		return "", errors.New("invalid ANTHROPIC_BASE_URL")
	}
	switch p {
	case "", "/":
		u.Path = "/v1/messages"
	case "/v1":
		u.Path = "/v1/messages"
	case "/messages":
		u.Path = "/messages"
	case "/v1/messages":
		u.Path = "/v1/messages"
	default:
		return "", errors.New("invalid ANTHROPIC_BASE_URL")
	}
	u.RawPath = ""
	return u.String(), nil
}

func TestTransportRequestHeadersAndCredentialScope(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "")
	transport := NewTransport()
	defer transport.Close()
	if !transport.transport.DisableCompression || transport.transport.ForceAttemptHTTP2 || transport.client.CheckRedirect == nil {
		t.Fatal("transport permits compression, HTTP/2 retry path, or redirects")
	}
	metadata := core.RequestMetadata{Headers: map[string][]string{
		"Authorization": {"Bearer virtual"}, "X-Api-Key": {"forged"}, "Anthropic-Version": {"bad"},
		"Anthropic-Beta": {"bad"}, "Cookie": {"secret"}, "Host": {"evil"},
		"Content-Length": {"99"}, "Content-Type": {"text/plain"}, "Accept-Encoding": {"gzip"},
		"Connection": {"X-Private, keep-alive"}, "X-Private": {"blocked"}, "User-Agent": {"client"},
		"X-Unlisted": {"blocked"},
	}}
	in := core.ExecutionRequest{Payload: core.RawPayload{Body: []byte(`{"stream":true}`)}, Metadata: metadata}
	for _, accountKey := range []string{"account-a-key", "account-b-key"} {
		resp, err := transport.Do(context.Background(), in, core.InvocationServices{
			Credentials: credentialStub(accountKey),
			Transport: doerFunc(func(req *http.Request) (*http.Response, error) {
				if got := req.Header.Get("X-Api-Key"); got != accountKey {
					t.Errorf("credential = %q, want selected account credential", got)
				}
				if req.URL.String() != messagesEndpoint || req.Method != http.MethodPost || req.GetBody != nil {
					t.Errorf("request target/body replay: %s %s GetBody-present=%t", req.Method, req.URL, req.GetBody != nil)
				}
				for name, want := range map[string]string{"Anthropic-Version": "2023-06-01", "Content-Type": "application/json", "Accept": "text/event-stream", "User-Agent": "client"} {
					if got := req.Header.Get(name); got != want {
						t.Errorf("%s = %q, want %q", name, got, want)
					}
				}
				for _, name := range []string{"Authorization", "Anthropic-Beta", "Cookie", "X-Private", "X-Unlisted", "Accept-Encoding"} {
					if got := req.Header.Get(name); got != "" {
						t.Errorf("unsafe %s forwarded: %q", name, got)
					}
				}
				body, _ := io.ReadAll(req.Body)
				if string(body) != `{"stream":true}` {
					t.Errorf("body = %q", body)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: req}, nil
			}),
		})
		if err != nil || resp == nil {
			t.Fatalf("Do: response=%v error=%v", resp, err)
		}
		resp.Body.Close()
	}
}

func TestTransportBaseURLNormalizationAndValidation(t *testing.T) {
	for _, test := range []struct{ base, want string }{
		{"https://ai.pestit.pl/v1", "https://ai.pestit.pl/v1/messages"},
		{"https://ai.pestit.pl", "https://ai.pestit.pl/v1/messages"},
		{"https://ai.pestit.pl/messages", "https://ai.pestit.pl/messages"},
		{"http://127.0.0.1:8123/v1", "http://127.0.0.1:8123/v1/messages"},
		{"http://[::1]:8123/v1", "http://[::1]:8123/v1/messages"},
	} {
		got, err := testMessagesEndpointForBase(test.base)
		if err != nil || got != test.want {
			t.Errorf("testMessagesEndpointForBase(%q) = %q, %v; want %q", test.base, got, err, test.want)
		}
	}
	for _, base := range []string{
		"http://example.com/v1", "http://localhost:8123/v1", "https://user:pass@example.com/v1", "https://example.com/v1/../other",
		"https://example.com/v1?redirect=1", "https://example.com/v1?", "https://example.com/v1#", "https://example.com/v2",
	} {
		if _, err := testMessagesEndpointForBase(base); err == nil {
			t.Errorf("testMessagesEndpointForBase(%q) accepted unsafe/unsupported URL", base)
		}
	}
	t.Setenv("ANTHROPIC_BASE_URL", "https://ai.pestit.pl/v1")
	transport := testTransportWithBaseURL(os.Getenv("ANTHROPIC_BASE_URL"))
	defer transport.Close()
	req, err := transport.Request(context.Background(), core.ExecutionRequest{}, "synthetic-key")
	if err != nil || req.URL.String() != "https://ai.pestit.pl/v1/messages" {
		t.Fatalf("configured request target = %v, %v", req, err)
	}
}

func TestNewTransportIgnoresEnvironmentBaseURL(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "https://ai.pestit.pl/v1")
	transport := NewTransport()
	defer transport.Close()
	req, err := transport.Request(context.Background(), core.ExecutionRequest{}, "synthetic-key")
	if err != nil || req.URL.String() != messagesEndpoint {
		t.Fatalf("production transport target = %v, %v; want official default", req, err)
	}
}

func TestTransportNoRedirectOrCredentialLeak(t *testing.T) {
	transport := NewTransport()
	defer transport.Close()
	calls := 0
	transport.client.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://elsewhere.invalid/"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})
	resp, err := transport.Do(context.Background(), core.ExecutionRequest{}, core.InvocationServices{Credentials: credentialStub("sensitive-key")})
	if err != nil || resp == nil || resp.StatusCode != http.StatusFound || calls != 1 {
		t.Fatalf("redirect response=%v calls=%d error=%v", resp, calls, err)
	}
	resp.Body.Close()
	calls = 0
	_, err = transport.Do(context.Background(), core.ExecutionRequest{}, core.InvocationServices{
		Credentials: credentialStub("sensitive-key"),
		Transport: doerFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("sensitive-key leaked")
		}),
	})
	if err == nil || strings.Contains(err.Error(), "sensitive-key") || calls != 1 {
		t.Fatalf("unsafe transport error/retry: err=%v calls=%d", err, calls)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportCancellation(t *testing.T) {
	transport := NewTransport()
	defer transport.Close()
	started := make(chan struct{})
	transport.client.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := transport.Do(ctx, core.ExecutionRequest{}, core.InvocationServices{Credentials: credentialStub("key")})
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled request returned no error")
	}
}
