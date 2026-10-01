package core

import "context"

type ClientRequest struct{ Transport any }

type ClientResponse struct{ Transport any }

// ProtocolAdapter owns client-protocol parsing and response formatting.
type ProtocolAdapter interface {
	Component
	Protocol() string
	Decode(context.Context, ClientRequest) (ExecutionRequest, *GatewayError)
	Encode(context.Context, ClientResponse, *GatewayError, ExecutionResponse) error
}
