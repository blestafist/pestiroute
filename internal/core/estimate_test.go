package core

import (
	"context"
	"math"
	"testing"
)

type estimateTestConnector struct {
	Connector
	result   EstimateResult
	err      *GatewayError
	calls    int
	query    UsageQuery
	services InvocationServices
	onCall   func()
}

func (c *estimateTestConnector) EstimateUsage(_ context.Context, query UsageQuery, services InvocationServices) (EstimateResult, *GatewayError) {
	c.calls++
	c.query, c.services = query, services
	if c.onCall != nil {
		c.onCall()
	}
	return c.result, c.err
}

func TestEstimateResolution(t *testing.T) {
	known := func(input, output int64) *UsageReport {
		return &UsageReport{InputTokens: &input, OutputTokens: &output}
	}
	tests := []struct {
		name   string
		result EstimateResult
		budget RouteBudget
		want   ResolvedEstimate
		code   string
	}{
		{"known with reject budget", EstimateResult{Supported: true, Known: true, Usage: known(4, 6), Method: "tokenizer"}, RouteBudget{UnknownEstimateReject, 0}, ResolvedEstimate{10, "tokenizer"}, ""},
		{"unsupported reserves", EstimateResult{}, RouteBudget{UnknownEstimateReserve, 13}, ResolvedEstimate{13, ""}, ""},
		{"unknown reserves", EstimateResult{Supported: true, Known: false}, RouteBudget{UnknownEstimateReserve, 13}, ResolvedEstimate{13, ""}, ""},
		{"nil usage reserves", EstimateResult{Supported: true, Known: true}, RouteBudget{UnknownEstimateReserve, 13}, ResolvedEstimate{13, ""}, ""},
		{"missing counter reserves", EstimateResult{Supported: true, Known: true, Usage: &UsageReport{InputTokens: new(int64(3))}}, RouteBudget{UnknownEstimateReserve, 13}, ResolvedEstimate{13, ""}, ""},
		{"negative reserves", EstimateResult{Supported: true, Known: true, Usage: known(-1, 3)}, RouteBudget{UnknownEstimateReserve, 13}, ResolvedEstimate{13, ""}, ""},
		{"overflow reserves", EstimateResult{Supported: true, Known: true, Usage: known(math.MaxInt64, 1)}, RouteBudget{UnknownEstimateReserve, 13}, ResolvedEstimate{13, ""}, ""},
		{"overflow rejects", EstimateResult{Supported: true, Known: true, Usage: known(math.MaxInt64, 1)}, RouteBudget{UnknownEstimateReject, 0}, ResolvedEstimate{}, "estimate_unavailable"},
		{"zero estimate reserves", EstimateResult{Supported: true, Known: true, Usage: known(0, 0)}, RouteBudget{UnknownEstimateReserve, 13}, ResolvedEstimate{13, ""}, ""},
		{"unknown rejects with zero reserve", EstimateResult{}, RouteBudget{UnknownEstimateReject, 0}, ResolvedEstimate{}, "estimate_unavailable"},
		{"reject budget forbids conservative tokens", EstimateResult{Supported: true, Known: true, Usage: known(4, 6)}, RouteBudget{UnknownEstimateReject, 13}, ResolvedEstimate{}, "invalid_route_budget"},
		{"invalid option", EstimateResult{}, RouteBudget{"other", 13}, ResolvedEstimate{}, "invalid_route_budget"},
		{"nonpositive reserve", EstimateResult{}, RouteBudget{UnknownEstimateReserve, 0}, ResolvedEstimate{}, "invalid_route_budget"},
	}
	payload := RawPayload{Protocol: "p", Body: []byte(`{"unknown":[1,2]}`)}
	credentials := &estimateTestCredentials{}
	services := InvocationServices{Credentials: credentials}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			connector := &estimateTestConnector{result: tt.result}
			got, gatewayErr := ResolveEstimate(context.Background(), connector, UsageQuery{Protocol: "p", Model: "m", Payload: payload}, services, tt.budget)
			if tt.code != "" {
				if gatewayErr == nil || gatewayErr.Code != tt.code {
					t.Fatalf("error = %#v, want code %q", gatewayErr, tt.code)
				}
				if tt.code == "invalid_route_budget" && connector.calls != 0 {
					t.Fatalf("invalid budget called estimator %d times", connector.calls)
				}
				return
			}
			if gatewayErr != nil || got != tt.want {
				t.Fatalf("got (%+v, %v), want (%+v, nil)", got, gatewayErr, tt.want)
			}
			if connector.calls != 1 || connector.services.Credentials != credentials || string(connector.query.Payload.Body) != string(payload.Body) {
				t.Fatalf("estimate did not receive scoped services and opaque payload: calls=%d query=%+v", connector.calls, connector.query)
			}
			if payload.Body[0] != '{' {
				t.Fatal("estimate resolution mutated caller payload")
			}
		})
	}
}

type estimateTestCredentials struct{}

func (*estimateTestCredentials) Get(context.Context, string) ([]byte, error) { return nil, nil }

func TestEstimateStopsOnConnectorErrorOrCancellation(t *testing.T) {
	connectorErr := &GatewayError{Code: "connector", Message: "failed"}
	connector := &estimateTestConnector{err: connectorErr}
	budget := RouteBudget{UnknownEstimateReserve, 5}
	if _, got := ResolveEstimate(context.Background(), connector, UsageQuery{}, InvocationServices{}, budget); got != connectorErr {
		t.Fatalf("connector error = %v, want original error", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, got := ResolveEstimate(ctx, connector, UsageQuery{}, InvocationServices{}, budget); got == nil || got.Code != "estimate_cancelled" {
		t.Fatalf("cancel error = %v", got)
	}
	if connector.calls != 1 {
		t.Fatalf("cancelled estimate called connector; calls=%d", connector.calls)
	}
	ctx, cancel = context.WithCancel(context.Background())
	connector = &estimateTestConnector{result: EstimateResult{}}
	connector.onCall = cancel
	if _, got := ResolveEstimate(ctx, connector, UsageQuery{}, InvocationServices{}, budget); got == nil || got.Code != "estimate_cancelled" {
		t.Fatalf("in-flight cancellation error = %v", got)
	}
}

func TestEstimateRejectDoesNotProduceZeroReservation(t *testing.T) {
	connector := &estimateTestConnector{result: EstimateResult{Supported: true, Known: true, Usage: &UsageReport{InputTokens: new(int64(0)), OutputTokens: new(int64(0))}}}
	_, err := ResolveEstimate(context.Background(), connector, UsageQuery{}, InvocationServices{}, RouteBudget{UnknownEstimateReject, 0})
	if err == nil || err.Code != "estimate_unavailable" {
		t.Fatalf("zero estimate error = %v", err)
	}
}
