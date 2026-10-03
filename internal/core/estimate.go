package core

import (
	"context"
	"errors"
)

// RouteBudget controls how incomplete Connector estimates are admitted.
type RouteBudget struct {
	UnknownEstimate    string
	ConservativeTokens int64
}

const (
	UnknownEstimateReject  = "reject"
	UnknownEstimateReserve = "reserve"
)

// ResolvedEstimate is the token reservation resolved before admission.
type ResolvedEstimate struct {
	Tokens int64
	Method string
}

var ErrEstimateRejected = errors.New("usage estimate unavailable and route policy rejects unknown estimates")

// ResolveEstimate calls the selected Connector with the invocation's scoped
// services, then resolves its estimate against the configured route budget.
func ResolveEstimate(ctx context.Context, connector Connector, query UsageQuery, services InvocationServices, budget RouteBudget) (ResolvedEstimate, *GatewayError) {
	if err := validateRouteBudget(budget); err != nil {
		return ResolvedEstimate{}, estimateGatewayError("invalid_route_budget", CategoryInternal, "Route estimate budget is invalid")
	}
	if err := ctx.Err(); err != nil {
		return ResolvedEstimate{}, estimateContextError(err)
	}
	result, gatewayErr := connector.EstimateUsage(ctx, query, services)
	if gatewayErr != nil {
		return ResolvedEstimate{}, gatewayErr
	}
	if err := ctx.Err(); err != nil {
		return ResolvedEstimate{}, estimateContextError(err)
	}
	if result.Supported && result.Known && result.Usage != nil && result.Usage.InputTokens != nil && result.Usage.OutputTokens != nil {
		input, output := *result.Usage.InputTokens, *result.Usage.OutputTokens
		if total, ok := checkedTokenAdd(input, output); ok && total > 0 {
			return ResolvedEstimate{Tokens: total, Method: result.Method}, nil
		}
	}
	if budget.UnknownEstimate == UnknownEstimateReserve {
		return ResolvedEstimate{Tokens: budget.ConservativeTokens}, nil
	}
	return ResolvedEstimate{}, &GatewayError{
		Code: "estimate_unavailable", Category: CategoryInvalidRequest,
		Message: ErrEstimateRejected.Error(),
	}
}

func validateRouteBudget(budget RouteBudget) error {
	switch budget.UnknownEstimate {
	case UnknownEstimateReject:
		if budget.ConservativeTokens == 0 {
			return nil
		}
	case UnknownEstimateReserve:
		if budget.ConservativeTokens > 0 {
			return nil
		}
	default:
	}
	return ErrInvalidAdmissionValue
}

func estimateContextError(err error) *GatewayError {
	if errors.Is(err, context.DeadlineExceeded) {
		return estimateGatewayError("estimate_timeout", CategoryTimeout, "Usage estimation timed out")
	}
	return estimateGatewayError("estimate_cancelled", CategoryCancelled, "Usage estimation cancelled")
}

func estimateGatewayError(code string, category ErrorCategory, message string) *GatewayError {
	return &GatewayError{Code: code, Category: category, Message: message}
}
