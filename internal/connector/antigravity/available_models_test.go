package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type availableModelsDoer struct {
	calls  int
	status int
	body   string
	budget *availableModelsBudget
	err    error
}

func (d *availableModelsDoer) Do(req *http.Request) (*http.Response, error) {
	d.calls++
	if d.budget == nil || d.budget.reservations != 1 || req.Method != http.MethodPost || req.URL.String() != availableModelsURL || req.Header.Get("User-Agent") != antigravityAuthUserAgent || req.Header.Get("X-Client-Name") != "antigravity" || req.Header.Get("X-Client-Version") != "2.22.0" || req.Header.Get("Authorization") != "Bearer synthetic-token" {
		return nil, io.ErrUnexpectedEOF
	}
	requestBody, err := io.ReadAll(req.Body)
	if err != nil || string(requestBody) != `{"project":"stored-project"}` {
		return nil, io.ErrUnexpectedEOF
	}
	if d.err != nil {
		return nil, d.err
	}
	return &http.Response{StatusCode: d.status, Body: io.NopCloser(strings.NewReader(d.body)), Header: make(http.Header)}, nil
}

type availableModelsBudget struct {
	reservations int
	err          error
}

func (b *availableModelsBudget) Reserve(_ context.Context, account string) error {
	if account != "synthetic-account" {
		return io.ErrUnexpectedEOF
	}
	b.reservations++
	if b.err != nil {
		return b.err
	}
	return nil
}

func TestFetchAvailableModelsBoundedSingleDispatch(t *testing.T) {
	budget := &availableModelsBudget{}
	doer := &availableModelsDoer{status: http.StatusOK, body: `{"models":{"gemini-2.5-pro":{},"gemini-2.5-flash":{},"private-canary-model":{"quota":"private-canary"}}}`, budget: budget}
	got, err := (&Connector{}).fetchAvailableModels(context.Background(), doer, budget, "synthetic-account", "synthetic-token", "stored-project", time.Now().Add(time.Hour))
	if err != nil || doer.calls != 1 || budget.reservations != 1 || got.Status != 200 || !got.Models["gemini-2.5-pro"] || !got.Models["gemini-2.5-flash"] || got.UnknownCount != 1 {
		t.Fatalf("bounded probe failed: calls=%d status=%d unknown=%d err=%v", doer.calls, got.Status, got.UnknownCount, err)
	}
	if _, ok := got.Models["private-canary-model"]; ok {
		t.Fatal("unknown model ID escaped projection")
	}
	output, err := json.Marshal(got)
	if err != nil || strings.Contains(string(output), "private-canary") || strings.Contains(string(output), "quota") {
		t.Fatal("private model metadata escaped projection")
	}
	t.Log(string(output))
	items := make([]string, 201)
	for i := range items {
		items[i] = fmt.Sprintf("\"m%d\":{}", i)
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{"duplicate", `{"models":{"a":{},"a":{}}}`},
		{"malformed JSON", `{"models":{}`},
		{"duplicate nested key", `{"models":{"a":{"x":1,"x":2}}}`},
		{"wrong models type", `{"models":[]}`},
		{"wrong value type", `{"models":{"a":[]}}`},
		{"too many", `{"models":{` + strings.Join(items, ",") + `}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseAvailableModels([]byte(test.body)); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
	if _, err := parseAvailableModels([]byte(`{"models":{}}` + strings.Repeat(" ", maxAvailableModelsResponse))); err == nil {
		t.Fatal("oversized response accepted")
	}
	largeBudget := &availableModelsBudget{}
	largeDoer := &availableModelsDoer{status: http.StatusOK, body: `{"models":{}}` + strings.Repeat(" ", maxAvailableModelsResponse), budget: largeBudget}
	if _, err := (&Connector{}).fetchAvailableModels(context.Background(), largeDoer, largeBudget, "synthetic-account", "synthetic-token", "stored-project", time.Now().Add(time.Hour)); err == nil || largeDoer.calls != 1 || largeBudget.reservations != 1 {
		t.Fatal("oversized HTTP response was not rejected after one reserved request")
	}
}

func TestFetchAvailableModelsFailsClosedBeforeExpiredCredentialDispatch(t *testing.T) {
	budget := &availableModelsBudget{}
	doer := &availableModelsDoer{status: http.StatusOK, body: `{"models":{}}`, budget: budget}
	if _, err := (&Connector{}).fetchAvailableModels(context.Background(), doer, budget, "synthetic-account", "synthetic-token", "stored-project", time.Now().Add(-time.Second)); err == nil || doer.calls != 0 || budget.reservations != 0 {
		t.Fatalf("expired credential dispatched: calls=%d", doer.calls)
	}
}

func TestFetchAvailableModelsSanitizesHTTPFailure(t *testing.T) {
	budget := &availableModelsBudget{}
	doer := &availableModelsDoer{status: http.StatusServiceUnavailable, body: `private-canary`, budget: budget}
	got, err := (&Connector{}).fetchAvailableModels(context.Background(), doer, budget, "synthetic-account", "synthetic-token", "stored-project", time.Now().Add(time.Hour))
	if err == nil || strings.Contains(err.Error(), "private-canary") || got.Status != http.StatusServiceUnavailable || doer.calls != 1 || budget.reservations != 1 {
		t.Fatalf("unsafe or incomplete failure: status=%d calls=%d err=%v", got.Status, doer.calls, err)
	}
}

func TestFetchAvailableModelsBudgetFailureDoesNotDispatch(t *testing.T) {
	budget := &availableModelsBudget{err: errors.New("budget-private-canary")}
	doer := &availableModelsDoer{budget: budget}
	_, err := (&Connector{}).fetchAvailableModels(context.Background(), doer, budget, "synthetic-account", "synthetic-token", "stored-project", time.Now().Add(time.Hour))
	if err == nil || strings.Contains(err.Error(), "private-canary") || budget.reservations != 1 || doer.calls != 0 {
		t.Fatalf("budget failure was unsafe or dispatched: reservations=%d calls=%d err=%v", budget.reservations, doer.calls, err)
	}
}

func TestFetchAvailableModelsTransportFailureReservesOnce(t *testing.T) {
	budget := &availableModelsBudget{}
	doer := &availableModelsDoer{budget: budget, err: errors.New("transport-private-canary")}
	_, err := (&Connector{}).fetchAvailableModels(context.Background(), doer, budget, "synthetic-account", "synthetic-token", "stored-project", time.Now().Add(time.Hour))
	if err == nil || strings.Contains(err.Error(), "private-canary") || budget.reservations != 1 || doer.calls != 1 {
		t.Fatalf("transport failure was unsafe or dispatched more than once: reservations=%d calls=%d err=%v", budget.reservations, doer.calls, err)
	}
}
