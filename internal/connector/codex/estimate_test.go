package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/blestafist/pestiroute/internal/core"
)

func TestEstimateUsageUnknownResolutionAndNoSend(t *testing.T) {
	c := NewConnector()
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	data, err := json.Marshal(componentConfig{Model: "gpt-5.4-mini", AccountID: "account-a", Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Init(context.Background(), core.ComponentConfig{Data: data}); err != nil {
		t.Fatal(err)
	}
	sends := 0
	services := core.InvocationServices{Transport: authRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		sends++
		return nil, errors.New("unexpected provider request")
	})}
	query := core.UsageQuery{Protocol: protocol, Mode: core.ModeNative, Model: "gpt-5.4-mini", AccountID: "account-a"}
	result, ge := c.EstimateUsage(context.Background(), query, services)
	if ge != nil || result.Supported || result.Known || result.Usage != nil || result.Method != "" {
		t.Fatalf("EstimateUsage = %+v, %v; want honest unknown", result, ge)
	}
	for _, tc := range []struct {
		name   string
		budget core.RouteBudget
		want   int64
		code   string
	}{
		{name: "reject", budget: core.RouteBudget{UnknownEstimate: core.UnknownEstimateReject}, code: "estimate_unavailable"},
		{name: "reserve", budget: core.RouteBudget{UnknownEstimate: core.UnknownEstimateReserve, ConservativeTokens: 37}, want: 37},
		{name: "invalid budget", budget: core.RouteBudget{UnknownEstimate: core.UnknownEstimateReserve, ConservativeTokens: -1}, code: "invalid_route_budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := core.ResolveEstimate(context.Background(), c, query, services, tc.budget)
			if tc.code != "" {
				if err == nil || err.Code != tc.code {
					t.Fatalf("ResolveEstimate error = %v, want %s", err, tc.code)
				}
				return
			}
			if err != nil || got.Tokens != tc.want || got.Method != "" {
				t.Fatalf("ResolveEstimate = %+v, %v; want %d conservative tokens", got, err, tc.want)
			}
		})
	}
	if sends != 0 {
		t.Fatalf("estimation caused %d provider sends", sends)
	}
}

func TestEstimateUsageFailsClosed(t *testing.T) {
	query := core.UsageQuery{Protocol: protocol, Mode: core.ModeNative, Model: "gpt-5.4-mini", AccountID: "account-a"}
	if _, ge := NewConnector().EstimateUsage(context.Background(), query, core.InvocationServices{}); ge == nil || ge.Code != "connector_unavailable" {
		t.Fatalf("unready estimate error = %v", ge)
	}
	c := NewConnector()
	if err := c.Init(context.Background(), core.ComponentConfig{Data: mustCodexEstimateConfig(t)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	for _, tc := range []struct {
		name   string
		mutate func(*core.UsageQuery)
	}{
		{"protocol", func(q *core.UsageQuery) { q.Protocol = "other" }},
		{"mode", func(q *core.UsageQuery) { q.Mode = "other" }},
		{"model", func(q *core.UsageQuery) { q.Model = "other" }},
		{"account", func(q *core.UsageQuery) { q.AccountID = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := query
			tc.mutate(&q)
			if _, ge := c.EstimateUsage(context.Background(), q, core.InvocationServices{}); ge == nil {
				t.Fatal("scope mismatch was accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ge := c.EstimateUsage(ctx, query, core.InvocationServices{}); ge == nil || ge.Code != "estimate_cancelled" {
		t.Fatalf("cancelled estimate error = %v", ge)
	}
}

func mustCodexEstimateConfig(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(componentConfig{Model: "gpt-5.4-mini", AccountID: "account-a", Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
