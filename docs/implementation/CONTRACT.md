# Connector Contract

## Purpose

This contract describes the execution of an opaque protocol request. It does not contain general `Message`, `Tool`, `Reasoning`, or provider-specific methods. The provided Go signatures are an interface draft that will be refined in M2 with a working native connector and mock backend.

```go
type Connector interface {
    Describe(ctx context.Context) (Descriptor, error)
    Authenticate(ctx context.Context, req AuthRequest, rt Runtime) (AuthResult, error)
    EstimateUsage(ctx context.Context, req Request, rt Runtime) (UsageEstimate, error)
    Execute(ctx context.Context, req Request, rt Runtime) (Stream, error)
    Models(ctx context.Context, rt Runtime) ([]Model, error)
    Health(ctx context.Context, rt Runtime) (HealthStatus, error)
}

type Request struct {
    Protocol string
    Headers  map[string][]string
    Body     []byte
    Metadata Metadata
}

type Metadata struct {
    RequestID    string
    AttemptID    string
    Model        string
    Streaming    bool
    Requirements []Capability
}

type Stream interface {
    Next(ctx context.Context) (Frame, error)
    Close() error
}
```

`Execute` performs a single attempt with an already selected account. The connector does not perform implicit fallback between accounts. Context cancellation propagates to HTTP requests and runtime calls; `Close` is idempotent and releases resources even with a partially read response. Concurrent `Execute` calls must be safe; a single stream is read by a single consumer.

## Descriptor and Models

The descriptor contains a stable ID, implementation version, contract version, connector type, accepted protocols, and authentication methods. Types are API Connector, OpenAI-Compatible Connector, Agent Protocol Connector, and Local Runtime Connector; the last two distinguish client-protocol emulation from local inference. An Agent Protocol Connector does not necessarily run an agent or the official client. The implementation version and IPC/SDK version are distinct values. `Models` returns available models for a configured instance/account along with their capabilities, not a universal description of internal model architecture.

Capabilities have three values: `supported`, `unsupported`, `unknown`. A request requirement is satisfied only by the first. Final support is determined by a combination of protocol, execution mode, connector, model, and account; a broad connector declaration should not override a specific model limitation.

```yaml
id: openai-compatible
type: openai-compatible
version: "0.1.0"
contract_version: "1"
protocols:
  accepts: [openai.responses/v1]
capabilities:
  streaming: supported
  tools: unknown
  parallel_tools: unknown
auth: [api_key]
```

This is the canonical draft manifest vocabulary: `supported`, `unsupported`, and `unknown`. Namespaced capability examples in the [Extensibility Model](../project/EXTENSIBILITY.md#2-capability-system) are extension concepts, not a second manifest schema. Validate one chosen format strictly in the first implementation.

## Execution Stream

`Frame` is a tagged union of transport and control messages, not LLM events. It has exactly one of these variants:

| Variant | Data | Semantics |
| --- | --- | --- |
| `Head` | HTTP status and response headers | Once, before any body chunk |
| `Body` | Non-empty set of bytes | Original or already translated connector response |
| `Complete` | Outcome and final UsageReport | Once; closes transport lifecycle |

The valid sequence is `Head → Body* → Complete → EOF`. `Complete` can describe an upstream HTTP error, not just successful generation. Before `Head`, the connector may return a typed error; after `Head`, an unexpected `Next` error indicates an incomplete stream. EOF without `Complete` is also considered incomplete.

Body chunks do not need to align with SSE event boundaries. Core must not reinterpret their contents. Non-streaming JSON is transmitted through the same mechanism. With native passthrough, the connector may parse events in parallel for usage tracking, but the transmitted bytes remain unchanged.

## Usage

`UsageEstimate` contains a known estimate of input/output budget, an unknown value indicator, and an estimation method. `UsageReport` contains nullable counters `input_tokens`, `output_tokens`, `reasoning_tokens`, `cached_tokens`, a source (`provider`, `estimate`, `unknown`), and a completeness indicator. An unknown value is distinct from zero.

Input/output counters are considered top-level quantities. Cached and reasoning tokens are details that may already be included in the totals and cannot be unconditionally added again. The connector normalizes only accounting semantics and preserves backend-specific details in namespaced diagnostics when necessary.

If the client disconnects before receiving usage, Core saves the partial/unknown outcome and applied estimate separately. The absence of usage does not turn an executed request into a free one and does not allow deletion of the reservation without a trace.

## Errors

A typed error describes an infrastructure category: `invalid_request`, `unsupported_feature`, `unauthenticated`, `permission_denied`, `rate_limited`, `unavailable`, `timeout`, `cancelled`, or `internal`. Additionally, retry disposition, optional retry delay, and a client-safe message are transmitted.

A native connector can return the original upstream error as `Head` and `Body`, preserving the external protocol. If Core considers fallback, the decision and retry metadata must be available before forwarding `Head` to the client. A translation connector converts the upstream error to northbound-compatible bytes; the gateway's own errors are encoded by the northbound adapter.

## Runtime Services and Auth

Runtime provides selected instance/account context, scoped credential access, HTTP transport, logger, and common auth runtime. It does not pass the entire database or credentials of other accounts to the connector. The connector can use secrets during invocation but does not store them independently.

`Authenticate` works as a state machine: start a flow, continue after callback/device polling, or perform a refresh. `AuthResult` describes the next action, completion with updated credentials, or an error. Specific endpoints, scopes, and token exchanges remain inside the connector. Core is responsible for state validation, PKCE storage, auth session lifetime, and atomic result persistence.

Refresh is serialized for a single account. When rotating a refresh token, the new value is saved atomically with its expiry; a parallel request must not overwrite it with the old value. A storage error after token exchange is recorded as a separate auth failure.

## Transition to IPC

Opaque request bytes, metadata, descriptor, and the same stream frames are transferred via IPC. For Runtime calls from an external connector, a separate scoped host-service channel is required; a single server-streaming `Execute` RPC is insufficient. In M6, the lifecycle of both directions, authentication of the local channel, and access revocation upon attempt completion need to be verified.

The handshake verifies major contract version compatibility before executing requests. An unknown control frame cannot be skipped the same way as an unknown JSON field: the transport contract must remain unambiguous. Payload compatibility is ensured by its opacity; IPC compatibility is ensured by separate versioning rules.
