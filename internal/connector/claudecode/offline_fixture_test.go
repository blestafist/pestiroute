package claudecode

import (
	"net/http"
	"testing"
)

type fixtureDoerFunc func(*http.Request) (*http.Response, error)

func (f fixtureDoerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestOfflineFixtureTransportRestrictsFixedMessagesTarget(t *testing.T) {
	calls := 0
	transport := NewOfflineFakeTransport(fixtureDoerFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.String() != messagesURL {
			t.Fatalf("fixture received unexpected URL %s", req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK}, nil
	}))
	for _, rawURL := range []string{messagesURL, "https://api.anthropic.com/v1/messages?beta=true", "https://api.anthropic.invalid/other"} {
		req, err := http.NewRequest(http.MethodPost, rawURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = transport.Do(req)
		if rawURL == messagesURL && err != nil {
			t.Fatalf("fixed fixture target rejected: %v", err)
		}
		if rawURL != messagesURL && err == nil {
			t.Fatalf("unsafe target error = %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("fixture calls=%d, want one", calls)
	}
}
