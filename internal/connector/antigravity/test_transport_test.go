package antigravity

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/blestafist/pestiroute/internal/core"
)

// packageTestDoer admits package-local scripted fakes through the concrete
// transport allowlist. It is intentionally test-only and unexported.
func packageTestDoer(doer core.HTTPDoer) core.HTTPDoer {
	return packageTestTransport{doer: doer}
}

func TestOfflineTestTransportRejectsNetworkAndNilInputs(t *testing.T) {
	if _, err := NewOfflineTestTransport(); err == nil {
		t.Fatal("empty response script accepted")
	}
	transport, err := NewOfflineTestTransport(OfflineTestResponse{StatusCode: http.StatusOK, Body: []byte("scripted")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = transport.Do(nil); err == nil {
		t.Fatal("nil request accepted")
	}
	if _, err = transport.Do(&http.Request{URL: mustTestURL(t, "https://example.com/"), Header: make(http.Header)}); err == nil {
		t.Fatal("network URL accepted")
	}
	if _, err = (*OfflineTestTransport)(nil).Do(nil); err == nil {
		t.Fatal("nil transport accepted")
	}
	if requests := transport.Requests(); len(requests) != 0 {
		t.Fatalf("rejected calls were captured: %+v", requests)
	}
	response, err := transport.Do(&http.Request{Method: http.MethodGet, URL: mustTestURL(t, "https://unit.synthetic.invalid/test"), Header: make(http.Header), Body: io.NopCloser(strings.NewReader("captured"))})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "scripted" {
		t.Fatalf("response body=%q err=%v", body, err)
	}
	requests := transport.Requests()
	if len(requests) != 1 || string(requests[0].Body) != "captured" {
		t.Fatalf("captured requests=%+v", requests)
	}
	if _, err = transport.Do(&http.Request{Method: http.MethodGet, URL: mustTestURL(t, "https://unit.synthetic.invalid/extra")}); err == nil {
		t.Fatal("request beyond finite script accepted")
	}
}

func mustTestURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
