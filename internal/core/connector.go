package core

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

type ModelQuery struct {
	Protocol  string
	Mode      string
	AccountID string
}

type ModelInfo struct {
	ID           string
	Capabilities map[Capability]CapabilityState
	Available    *bool
}

type ModelsResult struct {
	Supported bool
	Models    []ModelInfo
}

type UsageQuery struct {
	Protocol  string
	Mode      string
	Model     string
	AccountID string
	Payload   RawPayload
}

type EstimateResult struct {
	Supported bool
	Known     bool
	Usage     *UsageReport
	Method    string
}

type AuthRequest struct {
	AccountID string
	Action    string
	State     []byte
}

type AuthResult struct {
	Supported           bool
	State               string
	NextAction          string
	UserAction          *AuthUserAction
	Credentials         map[string][]byte
	CredentialExpiresAt *time.Time
}

// AuthUserAction contains safe operator-facing authentication instructions.
type AuthUserAction struct {
	VerificationURI string
	UserCode        string
	PollInterval    time.Duration
}

// CredentialAccess exposes credentials for the selected account only.
type CredentialAccess interface {
	Get(context.Context, string) ([]byte, error)
}

// HTTPDoer is the invocation-scoped, runtime-controlled HTTP transport.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// InvocationServices are valid only for one invocation and must not be retained.
type InvocationServices struct {
	Credentials CredentialAccess
	Transport   HTTPDoer
	Logger      *slog.Logger
}

// Connector executes one selected attempt and provides scoped support operations.
type Connector interface {
	Component
	Execute(context.Context, ExecutionRequest, AttemptScope, InvocationServices) (ExecutionResponse, *GatewayError)
	Models(context.Context, ModelQuery, InvocationServices) (ModelsResult, *GatewayError)
	EstimateUsage(context.Context, UsageQuery, InvocationServices) (EstimateResult, *GatewayError)
	Authenticate(context.Context, AuthRequest, InvocationServices) (AuthResult, *GatewayError)
}
