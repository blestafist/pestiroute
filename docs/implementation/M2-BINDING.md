# M2 Go component binding

This is a language binding of the accepted [v1 contract](CONTRACT.md), not a
semantic extension. Existing opaque `ExecutionRequest`, `ExecutionResponse`,
`StreamFrame`, `GatewayError`, `UsageReport`, and `AttemptScope` remain the
execution vocabulary in `internal/core`; neither side imports a concrete
Adapter or Connector. Keep this boundary in-process for M2.

## Package ownership

`internal/core` owns the shared contract values, `Component` interfaces,
capability scope/results, and invocation service interfaces. It may depend on
the standard library only. Adapter packages own protocol Decode/Encode and
`ProtocolAdapter`; Connector packages own backend translation, provider
support operations, and `Connector`. `cmd/gateway` is the composition root:
it constructs implementations, validates descriptors, wires registry/routes,
and supplies runtime services. Avoid a second shared package while the
existing opaque values already live in `core`.

## Component API

Use distinct API versions for Adapter and Connector compatibility, independent
of protocol and implementation versions. `APIVersion` is the numeric pair
`{Major, Minor}` (unsigned 16-bit fields); `major.minor` is only its notation,
not a string parser or the implementation version. Compatibility requires the
exact runtime API pair to appear in the component's declared supported
versions; there is no implicit minor-version range. Major must be nonzero;
duplicate pairs are invalid. `ImplementationVersion` is a separate diagnostic
version string. An unsupported major/version rejects registration before Init.

```go
type ComponentKind string // "adapter" or "connector"
type APIVersion struct { Major, Minor uint16 }
type Descriptor struct {
    ID string                 // stable implementation ID from the descriptor contract
    Kind ComponentKind
    ImplementationVersion string // implementation version, diagnostic only
    APIVersions []APIVersion  // sorted, unique supported API versions
    Protocols []string        // exact versioned protocol IDs
    Operations []string       // supported extension operation IDs only
    ConnectorType string      // Connector only; otherwise empty
    AuthMethods []string      // Connector only; otherwise empty
}
type InstanceID string // runtime/config-owned identity; never supplied by a client
type ComponentConfig struct { Data []byte } // opaque bytes owned/schema-checked by the component
type ClientRequest struct { Transport any } // opaque client transport handle; Adapter-owned interpretation
type ClientResponse struct { Transport any } // opaque response transport handle; Adapter-owned interpretation
type HealthState string // ready, unavailable, unknown
type Health struct { State HealthState; Diagnostic string } // bounded, sanitized

type Component interface {
    Descriptor() Descriptor
    Init(context.Context, ComponentConfig) error
    Health(context.Context) Health
    Capabilities(context.Context, CapabilityScope) CapabilityResult
    Close(context.Context) error
}
type ProtocolAdapter interface {
    Component
    Protocol() string
    Decode(context.Context, ClientRequest) (ExecutionRequest, *GatewayError)
    Encode(context.Context, ClientResponse, *GatewayError, ExecutionResponse) error
}
type Connector interface {
    Component
    Execute(context.Context, ExecutionRequest, AttemptScope, InvocationServices) (ExecutionResponse, *GatewayError)
    Models(context.Context, ModelQuery, InvocationServices) (ModelsResult, *GatewayError)
    EstimateUsage(context.Context, UsageQuery, InvocationServices) (EstimateResult, *GatewayError)
    Authenticate(context.Context, AuthRequest, InvocationServices) (AuthResult, *GatewayError)
}
```

`Descriptor.ID` identifies the implementation; the runtime assigns a distinct
`InstanceID` to each configured instance, and routes reference that instance.
The registry rejects duplicate instance IDs, not reuse of an implementation ID.
`ComponentConfig.Data` is component-owned opaque configuration. The caller
treats it as read-only during Init; a component copies bytes it needs to retain.
It is not the gateway's full config or a secret store. `ClientRequest.Transport`
and `ClientResponse.Transport` are opaque
handles owned/interpreted only by the Adapter; `core` neither imports nor
interprets a concrete Adapter's transport type. Support-operation
query/result values are defined below and are not client protocol schemas.
`Decode` owns client validation and returns a client-safe `GatewayError`.
`Encode` receives either a pre-stream error or an execution response and owns
transport writes and cancellation; its `error` is the transport/encoding
failure, not a second client error response. A write error after commit cancels
the stream and is reflected in the attempt result, not a replacement response.
`Protocol()` must exactly agree with a declared
descriptor protocol. Connector `Execute` is exactly one attempt on the supplied
runtime-selected scope: no hidden replay or account fallback.

Descriptor validation requires a non-empty stable ID, expected kind, valid
API versions, unique protocol/operation/auth identifiers, and kind-appropriate
Connector fields. Descriptor is queryable before Init and must not perform
inference. Init is called once before admission; failure leaves the component
unavailable, but runtime still invokes idempotent Close to release partial
resources. Close first prevents new work; runtime drains under its deadline,
cancels remaining invocations, then closes components. Init/Close are
serialized against each other; Health, Capabilities, and request operations
must be concurrency-safe. Health is observational and never guarantees an
execution. Components may not retain invocation state across calls.

## Capability scope and support operations

```go
type CapabilityState string // supported, unsupported, unknown
type CapabilityScope struct {
    Protocol string
    Mode string // native or translation
    Model string
    AccountID string // runtime-selected; omitted where the operation has no account
}
type CapabilityResult struct { Values map[Capability]CapabilityState }
type ModelQuery struct { Protocol string; Mode string; AccountID string }
type ModelInfo struct { ID string; Capabilities map[Capability]CapabilityState; Available *bool }
type ModelsResult struct { Supported bool; Models []ModelInfo }
type UsageQuery struct { Protocol string; Mode string; Model string; AccountID string; Payload RawPayload }
type EstimateResult struct { Supported bool; Known bool; Usage *UsageReport; Method string }
type AuthRequest struct { AccountID string; Action string; State []byte }
type AuthResult struct { Supported bool; State string; NextAction string; Credentials map[string][]byte }
```

Missing capability entries mean `unknown`; only explicit `supported` satisfies
a request, and matching is exact across Adapter and selected Connector scope.
Model IDs/capabilities/availability describe only the requested protocol,
mode, and account; do not infer capability from provider identity. Unsupported
operations return `Supported:false` and no fabricated result; supported but
not established values use explicit `unknown` (including usage `Known:false`,
nil usage). `EstimateUsage`'s known estimate and method support admission
budgeting, not provider tokenization in Core. `Models` never fabricates a
model or availability. `Authenticate` returns an explicit unsupported result
when absent; otherwise it performs one start/continue/refresh state-machine
step. Runtime owns session state, persistence, refresh serialization, and
credential lifetime; never pass whole storage or another account's credentials.

`InvocationServices` are assembled by Core per selected invocation, not stored
on the component:

```go
type InvocationServices struct {
    Credentials CredentialAccess
    Transport HTTPDoer
    Logger *slog.Logger // request-scoped, redacting runtime logger
}
type CredentialAccess interface {
    Get(context.Context, string) ([]byte, error) // selected account/credential only
}
type HTTPDoer interface { Do(*http.Request) (*http.Response, error) }
```

Credential access is scoped to the selected account; the transport is a
runtime-controlled client with cancellation, policy, and redirect behavior
configured by Core; logging must not include payloads or secrets. Services are
valid only for the invocation and may not be retained. No secret storage,
refresh persistence, or auth-session database enters this binding (M3).

## Routes and startup compatibility

The routing values are runtime-owned in `internal/core`:

```go
type RouteLookupKey struct { Protocol, Mode, Model string }
type RouteIdentity struct { RouteLookupKey; AccountID string }
type SelectionContext struct { Mode, AccountID string } // trusted Core/config values
type Route struct {
    Identity RouteIdentity
    Adapter InstanceID
    Connector InstanceID
}
```

The request lookup key is exact `(protocol, mode, model)`, taken from the
decoded envelope and runtime-selected mode; it contains no account field and
client metadata cannot select an account. Core supplies a trusted
`SelectionContext.AccountID` from runtime policy/configuration, then resolves
the full route identity `(protocol, mode, model, account)` to one Adapter
instance and one Connector instance. Thus account is part of route identity,
not the client lookup key. M2 static routes name their account explicitly; if
Core has no selected account or no exact route, resolution fails closed. There
is no provider-name or wildcard fallback. Normalize nothing except the
documented mode value; compare all identifiers exactly. A duplicate full
identity is invalid even if both declarations name the same targets. Routes
for distinct account IDs are not conflicting because the trusted selection
context matches exactly one; absent account selection fails closed rather than
choosing by order or retrying across accounts.
Models and capabilities are static declarations in the loaded configuration
for M2: they are scoped to that exact route identity, validated against the
component descriptor, and never claim live/provider-verified support merely
because a fixture declares it. Runtime eligibility still requires `supported`
from both selected components.

Keep startup JSON strict and backward compatible. The legacy form remains the
M1 single configured native target and maps internally to one explicit route
using its existing protocol/model/account/credential selection and current
loopback-only inference gate. New M2 JSON adds a `components` collection
(instance ID, implementation ID, kind, implementation/config reference) and
`routes` collection (protocol, mode, model, account, Adapter instance ID,
Connector instance ID, capability map).
Unknown fields, partial component/route declarations, duplicate instance IDs,
invalid references, conflicting routes, or a mixture of legacy target fields
and the new topology reject startup; there is no implicit default target.
Probes retain their existing behavior. M2 inference remains loopback-only and native-only;
do not add listener authentication, translation, reload, or persisted
credentials here. Config values select component instances, not secrets.

## Attempt and stream lifecycle

Core creates one request/attempt identity and owns one finalizer per admitted
attempt. It attaches the selected account, attempt scope, and invocation
services to the Connector call; caller metadata never selects these. On Init
failure the instance remains unavailable, no request is admitted through it,
and partial resources are closed. A pre-Head rejection produces no committed
response. Before forwarding a classified `Head`, Core may use its error/retry
metadata to consider fallback only when the existing v1 conditions all hold:
retryable, explicitly safe, uncommitted, and policy/deadline/alternative
eligible. If it discards that stream for an allowed retry, it closes and
finalizes that attempt once. If it does not retry (including unsafe/unknown
disposition), forwarding `Head` commits; a rejected backend response may then
forward its opaque `Body*` and must end `Complete(failed)` with its cause. That
committed rejection is delivered as the response and cannot trigger fallback
or replacement. A pre-Head GatewayError has no stream/commit and is finalized
once as failed (or cancelled for cancellation). M2 adds no automatic retry.

For a delivered stream, the required sequence is `Head → Body* → Complete →
EOF`. Core validates each frame and forwards it under bounded backpressure;
the Adapter's Head handoff is the commit point. Cancellation/Close interrupts
both producer and consumer, releases resources, and finalizes once as
cancelled/incomplete with known partial or unknown usage. A Connector that
returns EOF before Complete, an invalid/duplicate/out-of-order frame, or a
non-EOF error is a contract failure: never synthesize success, never replay
after commit, and finalize once with incomplete outcome when not already
terminal.

Core's checked stream withholds the single `Complete` frame from its consumer,
performs one further source `Next` using the same invocation context, and
requires `io.EOF`. Only then does it return `Complete`; the consumer's next
call returns EOF. Thus externally visible order remains `Head → Body* →
Complete → EOF`, and the Complete outcome/error is not exposed ahead of
trailing-frame validation (Head error classification remains available before
commit as required for fallback decisions). Any frame after Complete,
malformed trailing frame, or non-EOF result is a contract failure; do not
forward that Complete or retain success. This holds only the one Complete
value, not response data. The bound
is the existing Core invocation context: request/client cancellation, any
request deadline, and runtime shutdown deadline; Core adds no independent
timeout. Stream.Next must honor that context, and Close interrupts/releases
the producer. Cancellation while checking EOF finalizes cancelled/incomplete
exactly once.
Current M1 `attemptStream` finalizes when it observes Complete and the current
checker does not verify the subsequent EOF; downstream implementation must
move terminal-success accounting behind this validation rather than preserve
that early-finalization behavior. On a valid EOF, the Complete outcome, cause,
and usage are finalized as supplied. On invalid trailing data, finalize the
attempt once as incomplete with the contract error and retain any usage already
observed from Complete; never record succeeded.

| Walkthrough | Binding path and terminal result |
| --- | --- |
| Successful execution | Adapter Decode → routed Connector Execute → validated Head/Body* → checked stream reads Complete then source EOF → checked stream returns Complete to Adapter → Adapter's next read gets EOF; Core records succeeded once only after source EOF. |
| Init failure | Registration/Init returns error → instance unavailable, no route admission; runtime calls idempotent Close to release partial resources. |
| Rejection | Pre-Head GatewayError is uncommitted and finalized failed; classified rejected Head is examined before handoff for v1-safe fallback, otherwise Head handoff commits and its Body* plus Complete(failed) are delivered without later fallback. |
| Cancellation | Request context cancellation or stream Close interrupts blocked Next/producer and Adapter write; Core finalizes cancelled/incomplete once, retaining only known partial usage; never claims safe replay from uncertain delivery. |
| Complete then illegal frame | Connector yields Complete then a frame (or non-EOF error) → checked stream rejects before returning Complete; no terminal completion delivered and no success finalization; close/cancel and finalize contract failure once as incomplete. |

## v1 mapping and migration

| v1 operation/behavior | Go owner and binding | Failure / unknown mapping |
| --- | --- | --- |
| Adapter component operations (`Descriptor`, `Init`, `Health`, `Capabilities`, `Close`) | `core.Component`, implemented by Adapter | Invalid descriptor/API rejects before Init; health `unknown`/`unavailable` never admits work |
| `Protocol()` | `core.ProtocolAdapter` | Mismatch with descriptor protocol rejects registration |
| `Decode(client request)` | `core.ProtocolAdapter.Decode` | `*core.GatewayError`; malformed client input is `invalid_request` |
| `Encode(response/error, transport)` | `core.ProtocolAdapter.Encode` | Receives client-safe `GatewayError`; transport `error` after commit cancels execution |
| Connector component operations | `core.Component`, implemented by Connector | As above; component identity and API compatibility checked before execution |
| `Execute(context, request)` | `core.Connector.Execute(context.Context, core.ExecutionRequest, core.AttemptScope, core.InvocationServices)` | pre-Head `*GatewayError`, or checked stream; one selected attempt only |
| `EstimateUsage` | `core.Connector.EstimateUsage` | `Supported:false` or `Known:false`, nil usage; never zero by default |
| `Models` | `core.Connector.Models` | `Supported:false`; no synthetic IDs/availability/capabilities |
| `Authenticate` | `core.Connector.Authenticate` | `Supported:false`; runtime persists returned credentials/state, not Connector |
| scoped capability lookup | `Component.Capabilities(scope)` | Missing map key is `unknown`; only exact `supported` passes |
| stream `Head/Body/Complete`, errors, usage, close | existing `core` frame and error types / `Stream` | Contract violation or premature EOF becomes incomplete, never fabricated success |

The current M1 `Dispatcher`, single `fixedTarget`, static target configuration,
and per-request `AttemptScope` are migration inputs, not new parallel
abstractions. Preserve their request ID, exact capability checks, pre-Head
commit, cancellation, and once-only finalization behavior while replacing
single-target wiring with validated component registration and explicit route
selection. Keep provider conversion and HTTP details in Connector packages;
Core remains byte-opaque. No frame variant, client/provider LLM schema,
released capability claim, automatic retry, or semantic contract change is
introduced by this binding.
