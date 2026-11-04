package antigravity

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/blestafist/pestiroute/internal/core"
)

const offlineTestBodyLimit = 1 << 20
const offlineTestMaxResponses = 16

// OfflineTestResponse is one bounded synthetic response served by an
// OfflineTestTransport. Responses are consumed in order; no HTTP client or
// caller-provided transport is used.
type OfflineTestResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// OfflineTestRequest is a captured request with its body read by the test
// transport. Header and Body are copies owned by the returned snapshot.
type OfflineTestRequest struct {
	Method string
	URL    string
	Header http.Header
	Body   []byte
}

// OfflineTestTransport serves only its finite in-memory response script and
// captures requests. It accepts only HTTPS requests to synthetic.invalid hosts.
type OfflineTestTransport struct {
	mu        sync.Mutex
	responses []OfflineTestResponse
	requests  []OfflineTestRequest
	next      int
}

// NewOfflineTestTransport creates a deterministic transport from bounded
// response values. It rejects an empty script, invalid status codes, and
// oversized response bodies.
func NewOfflineTestTransport(responses ...OfflineTestResponse) (*OfflineTestTransport, error) {
	if len(responses) == 0 || len(responses) > offlineTestMaxResponses {
		return nil, errors.New("offline test transport requires a bounded response script")
	}
	cloned := make([]OfflineTestResponse, len(responses))
	totalBodyBytes := 0
	for i, response := range responses {
		if response.StatusCode < 100 || response.StatusCode > 599 || len(response.Body) > offlineTestBodyLimit {
			return nil, errors.New("invalid offline test response")
		}
		totalBodyBytes += len(response.Body)
		if totalBodyBytes > offlineTestBodyLimit {
			return nil, errors.New("offline test response script exceeds size limit")
		}
		cloned[i] = OfflineTestResponse{
			StatusCode: response.StatusCode,
			Header:     response.Header.Clone(),
			Body:       append([]byte(nil), response.Body...),
		}
	}
	return &OfflineTestTransport{responses: cloned}, nil
}

func (t *OfflineTestTransport) Do(req *http.Request) (*http.Response, error) {
	if t == nil || req == nil || req.URL == nil || req.URL.Scheme != "https" || req.URL.User != nil || !strings.HasSuffix(strings.ToLower(req.URL.Hostname()), ".synthetic.invalid") {
		return nil, errors.New("offline test transport rejected request")
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	var body []byte
	if req.Body != nil {
		data, err := io.ReadAll(io.LimitReader(req.Body, offlineTestBodyLimit+1))
		_ = req.Body.Close()
		if err != nil || len(data) > offlineTestBodyLimit {
			return nil, errors.New("offline test request body invalid")
		}
		body = data
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.next >= len(t.responses) {
		return nil, errors.New("offline test response script exhausted")
	}
	t.requests = append(t.requests, OfflineTestRequest{Method: req.Method, URL: req.URL.String(), Header: req.Header.Clone(), Body: body})
	response := t.responses[t.next]
	t.next++
	return &http.Response{StatusCode: response.StatusCode, Header: response.Header.Clone(), Body: io.NopCloser(bytes.NewReader(response.Body)), Request: req}, nil
}

type packageTestTransport struct{ doer core.HTTPDoer }

func (t packageTestTransport) Do(req *http.Request) (*http.Response, error) {
	if nilTransport(t.doer) {
		return nil, errors.New("offline test transport has no scripted doer")
	}
	return t.doer.Do(req)
}

// Requests returns a deep copy of captured requests in dispatch order.
func (t *OfflineTestTransport) Requests() []OfflineTestRequest {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	requests := make([]OfflineTestRequest, len(t.requests))
	for i, request := range t.requests {
		requests[i] = OfflineTestRequest{Method: request.Method, URL: request.URL, Header: request.Header.Clone(), Body: append([]byte(nil), request.Body...)}
	}
	return requests
}
