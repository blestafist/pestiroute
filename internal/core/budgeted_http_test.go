package core

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type budgetFunc func(context.Context, string) error

func (f budgetFunc) Reserve(ctx context.Context, account string) error { return f(ctx, account) }

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestBudgetedHTTPDoerReservesBeforeEveryDispatch(t *testing.T) {
	var reserved, dispatched int
	doer := BudgetedHTTPDoer{
		Account: "synthetic-account",
		Budget: budgetFunc(func(_ context.Context, account string) error {
			if account != "synthetic-account" {
				t.Fatalf("account=%q", account)
			}
			reserved++
			return nil
		}),
		Next: doerFunc(func(*http.Request) (*http.Response, error) {
			if reserved != dispatched+1 {
				t.Fatal("dispatch occurred before reservation")
			}
			dispatched++
			return nil, errors.New("ambiguous synthetic failure")
		}),
	}
	req, _ := http.NewRequest(http.MethodGet, "https://fixture.invalid/", nil)
	if _, err := doer.Do(req); err == nil {
		t.Fatal("expected fake transport error")
	}
	if _, err := doer.Do(req); err == nil {
		t.Fatal("expected fake transport error")
	}
	if reserved != 2 || dispatched != 2 {
		t.Fatalf("reserved=%d dispatched=%d", reserved, dispatched)
	}
	doer.Budget = budgetFunc(func(context.Context, string) error { return errors.New("store offline") })
	if _, err := doer.Do(req); !errors.Is(err, ErrRequestBudgetExceeded) || dispatched != 2 {
		t.Fatalf("fail-closed err=%v dispatched=%d", err, dispatched)
	}
}

func TestBudgetedHTTPDoerRedirectIsReturnedNotFollowed(t *testing.T) {
	var calls, reservations int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/second", http.StatusFound)
		}
	}))
	defer server.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	doer := BudgetedHTTPDoer{Next: client, Account: "synthetic-account", Budget: budgetFunc(func(context.Context, string) error { reservations++; return nil })}
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/start", nil)
	resp, err := doer.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if calls != 1 || reservations != 1 || resp.StatusCode != http.StatusFound {
		t.Fatalf("calls=%d reservations=%d status=%d", calls, reservations, resp.StatusCode)
	}
}
