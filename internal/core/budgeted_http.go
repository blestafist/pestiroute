package core

import (
	"context"
	"errors"
	"net/http"
)

var ErrRequestBudgetExceeded = errors.New("request_budget_exceeded")

type HTTPRequestBudget interface {
	Reserve(context.Context, string) error
}

// BudgetedHTTPDoer permanently consumes one slot before delegating each call.
type BudgetedHTTPDoer struct {
	Next    HTTPDoer
	Budget  HTTPRequestBudget
	Account string
}

func (d BudgetedHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	if req == nil || d.Next == nil || d.Budget == nil || d.Account == "" {
		return nil, ErrRequestBudgetExceeded
	}
	if err := d.Budget.Reserve(req.Context(), d.Account); err != nil {
		return nil, ErrRequestBudgetExceeded
	}
	return d.Next.Do(req)
}

func (d BudgetedHTTPDoer) UnderlyingHTTPDoer() HTTPDoer { return d.Next }
