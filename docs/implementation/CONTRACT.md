# Internal Execution Contract

## Authority and Scope

This is the normative **v1 semantic contract** between Protocol Adapters, Core Runtime, and Connectors. It defines data ownership, operations, and observable behavior; it is not implementation code, a language binding, or an IPC encoding. New providers and client protocols must fit this boundary without adding provider-specific methods or structures.

```text
Protocol Adapter ── ExecutionRequest ──► Core Runtime ──► Connector
Protocol Adapter ◄─ ExecutionResponse ── Core Runtime ◄── Connector
```

Core Runtime must not inspect payload bodies, interpret client/provider event formats, or perform semantic protocol conversion. No `Message[]`, `Tool[]`, `Reasoning{}`, `UniversalResponse`, `TextChunk`, or `ToolCallChunk` belongs in this contract. Rationale and change control are recorded in [DEC-001 through DEC-004](../project/DECISIONS.md#contract-adrs).

## Execution Request

`ExecutionRequest` describes one logical client request. Protocol Adapter decoding creates the envelope; Core Runtime supplies a trusted request identity and execution context. Every attempt retains the same request ID and receives a distinct runtime-owned attempt ID.

| Field | Meaning and ownership |
| --- | --- |
| `ID` | Non-empty, runtime-assigned request identity; a client correlation value cannot replace it |
| `Model` | Opaque client model identifier extracted by the adapter for routing; absence is allowed only when the declared protocol/route does not require one |
| `Capabilities` | Set of required capability identifiers; Core may add policy requirements, never silently drop client requirements |
| `Metadata` | `RequestMetadata`: bounded transport and routing metadata, not a second payload model |
| `Payload` | `RawPayload`, containing the client protocol and original body |

`RawPayload` has three fields:

| Field | Meaning |
| --- | --- |
| `Protocol` | Non-empty, versioned protocol identifier, matched exactly against declared support; never inferred from provider name |
| `ContentType` | Media type describing `Body`; no JSON or SSE assumption |
| `Body` | Opaque bytes, including an empty body where valid for the protocol |

`RequestMetadata` contains only selected transport headers, a streaming preference when relevant, and explicitly declared namespaced routing/diagnostic extensions. Missing optional values mean unspecified, not guessed defaults. Gateway credentials, provider secrets, tool schemas, and decoded conversation content must not be placed here. Hop-by-hop headers are excluded; metadata cannot grant access or select an unauthorized account.

Core Runtime owns attempt ID, selected Connector instance/account, execution mode, deadline/cancellation, and scoped runtime services outside the client-supplied envelope. Header and metadata limits are enforced at ingress. Undeclared required extensions are rejected; explicitly optional extensions cannot affect correctness when ignored.

The request is immutable once admitted. Native execution preserves `Body` byte-for-byte, including unknown fields. Route selection by `Model` does not rewrite it. A Connector may create a transformed backend request only in explicit translation/rewrite mode; it must not mutate the admitted request or change the declared client protocol. This makes successive attempts independent without introducing an intermediate LLM model.

## Execution Response and StreamFrame

`ExecutionResponse` is a single ordered, cancellable stream of `StreamFrame` values, not an aggregated LLM result. Non-streaming responses use the same contract. Frames have a discriminant `Type` and exactly one corresponding transport/control payload. `Data` contains opaque bytes only for `Body`; lifecycle metadata is structurally defined, never encoded as provider content for Core to parse.

| Frame type | Payload | Invariant |
| --- | --- | --- |
| `Head` | Response protocol identifier, content type, optional transport status, safe response headers, optional `GatewayError` | Exactly once before body; response protocol equals the admitted `Payload.Protocol`; HTTP status is used only for HTTP exchanges |
| `Body` | Non-empty `Data` bytes | Zero or more; original or already translated bytes for the declared client protocol |
| `Complete` | Outcome (`succeeded`, `failed`, `cancelled`, or `incomplete`), final `UsageReport`, optional `GatewayError` | Exactly once on an orderly stream termination; no following frames |

The lifecycle is `Head → Body* → Complete → EOF`. A pre-head failure may instead return `GatewayError` without a stream or before the first frame. A rejected backend response may use `Head` and `Body` to preserve its error body, ending with `Complete(failed)`. Error metadata needed for fallback must be available before forwarding `Head`; it cannot arrive only at completion.

`Complete(succeeded)` must not carry a GatewayError. Other outcomes must carry a classified cause; an error reported at Head cannot become success at Complete. Core must not derive success from HTTP status alone. An unexpected transport/contract failure is classified by Core if no Connector error is available.

Conceptual streaming roles map to these frames as follows; they are **not six additional frame types**:

| Role | v1 representation |
| --- | --- |
| `HEADERS` | `Head` |
| `DATA` | `Body.Data` |
| `CONTROL` | Transport lifecycle defined by frame ordering and `Complete` |
| `USAGE` | `Complete.UsageReport` |
| `ERROR` | `GatewayError` at a failure boundary or on `Head`/`Complete`; client error bodies stay in `Body` |
| `END` | `Complete`, followed by EOF |

Core Runtime forwards bytes with bounded backpressure; it must not buffer a whole response, reorder frames, or parse text, tools, reasoning, JSON, or SSE. Body boundaries need not align with protocol events or characters. Connectors handle provider streaming and translation; Protocol Adapters encode the declared client transport and preserve already-compatible body bytes without translating them again.

Commit occurs when the Protocol Adapter starts the client-visible response, before forwarding body data; for HTTP this means sending final response headers. Retry eligibility must be decided before Core forwards `Head` to the adapter. After that handoff Core treats the response as committed, even if the client has not received bytes yet. No fallback or replacement status/body response is allowed after commit.

Duplicate heads/completions, data before head or after completion, an unknown frame type, or EOF without completion are contract failures. An unexpected stream error after `Head` produces an incomplete attempt; it must not be converted into a successful completion. A failed transport may prevent the producer from sending `Complete`; Core finalizes accounting exactly once using available error/usage information. Finalization does not fabricate a frame or provider-specific event. A protocol-appropriate terminal error may be emitted at the protocol boundary only when its semantics allow it; otherwise the transport closes.

Streams have one consumer. Closing a stream is idempotent, cancels outstanding producer work, and releases resources even when partially read. Core owns cleanup of discarded retry attempts as well as delivered streams. Long-lived and agent-protocol output fits the same opaque data path; bidirectional session commands and async jobs require separately versioned extensions rather than new LLM frame types.

## Component Descriptor and Lifecycle

Both Protocol Adapters and Connectors expose the following conceptual operations. Names specify responsibilities, not concrete language signatures.

| Operation | Contract |
| --- | --- |
| `Descriptor()` | Stable component ID, component kind, implementation version, supported API versions, declared protocols, and supported extension operations; Connectors also declare connector type and authentication methods |
| `Init()` | Validate component-owned configuration and initialize scoped resources before accepting work; failure leaves the component unavailable |
| `Health()` | Report `ready`, `unavailable`, or `unknown` with bounded diagnostic context; never claim a request will succeed merely because health is ready |
| `Capabilities(scope)` | Report supported/unsupported/unknown capability values for the specified protocol, mode, model, and account context |
| `Close()` | Stop accepting work and release component resources after draining or cancelling active work under runtime policy |

Descriptor discovery is available before initialization and must not execute provider inference. Execution operations require successful initialization. Close is idempotent and must release partially initialized resources after failed initialization. Operations must be safe across concurrent requests; per-execution state must not leak between them. Component Close is distinct from closing one stream.

Configuration reload is not a v1 operation. Runtime shutdown stops admission, drains within its deadline, then cancels remaining work and closes components. Client disconnect, timeout, explicit cancellation, and component shutdown propagate through adapter, Core, Connector, and provider operations. An unconfirmed provider cancellation leaves the execution outcome uncertain, not safe to replay.

## Connector Contract

In addition to component operations, a Connector provides `Execute(context, request) → ExecutionResponse or GatewayError`. Context contains runtime-owned attempt/account selection and scoped services, not provider payload semantics. One Execute is one attempt on the selected account; the Connector must not perform hidden replay or cross-account fallback. Core Runtime owns retry policy and attempt creation.

The Connector accepts only declared request protocols. It translates to its Backend Provider in explicit translation mode and returns body bytes compatible with the admitted protocol. No public `ConvertProviderAToProviderB`, per-provider execution method, or universal message conversion service is permitted. Provider-specific helpers remain private to the Connector.

The existing support operations retain their responsibilities alongside the stable execution boundary:

| Operation | Result and constraint |
| --- | --- |
| `EstimateUsage` | Estimate, known/unknown indicator, and method for admission budgeting; provider tokenization remains in the Connector |
| `Models` | Scoped model IDs, capabilities, and availability; optional context-window/provider metadata, not universal model behavior |
| `Authenticate` | Authentication state-machine step: start, continue, or refresh; returns the next action or updated credentials for runtime persistence |

These operations must report unsupported or unknown explicitly where applicable rather than fabricate success, models, or zero usage. Provider auth endpoints, scopes, exchanges, and streaming/usage formats remain private to the Connector. API, OpenAI-Compatible, Agent Protocol, and Local Runtime remain the four Connector categories; category does not alter Execute semantics.

## Protocol Adapter Contract

A Protocol Adapter exposes component operations and the following client-boundary operations:

| Operation | Contract |
| --- | --- |
| `Protocol()` | Identify the versioned client protocol handled by this adapter, consistent with its Descriptor |
| `Decode(client request)` | Parse/validate the client request, preserve raw body bytes, and produce the envelope fields and required capabilities or a `GatewayError` |
| `Encode(response or error, client transport)` | Consume transport frames or a pre-stream error and emit the client protocol response while respecting commit, backpressure, and cancellation |

Decode does not choose a backend, acquire provider credentials, or translate into another client protocol as an internal standard. Encode handles framing and gateway errors; it does not infer LLM semantics from generic frame types. Connector output already conforms to the declared protocol, so encoding must not duplicate provider translation. Native response bodies remain byte-preserved.

Core Runtime performs access checks, routing, admission and accounting between Decode and Execute. Adapter validation cannot substitute for runtime authorization. Decode/Encode failures use the same error metadata and finalize associated work; client write failure cancels the Connector stream. Adding a client protocol requires an adapter and compatible Connector declarations, not a provider-specific Core branch.

## Capability Model

A `Capability` is a namespaced identifier, not a boolean property on a provider. Requests carry a set of requirements; components expose a scoped map of identifiers to `supported`, `unsupported`, or `unknown`. Missing entries are unknown. Only confirmed support satisfies a requirement; provider names never imply capabilities.

The initial vocabulary is `llm.streaming`, `llm.tools`, `llm.tools.parallel`, `llm.reasoning`, `llm.vision`, `llm.audio`, `llm.structured_output`, `auth.oauth`, and `usage.exact`. `session.resume`, `session.fork`, and `execution.async` are reserved extension identifiers: naming them does not define executable session/job operations.

Matching is exact: `llm.tools` does not imply `llm.tools.parallel`. Client-facing feature support must hold across the Protocol Adapter and selected Connector/protocol/model/account combination. Authentication capabilities apply to the Connector/account scope, not to every adapter. Availability can change and execution can still fail after eligibility checks.

New identifiers can be added with documented meaning and scope without adding provider branches to Core. Undeclared support never satisfies an unknown requirement. The earlier draft keys `streaming`, `tools`, `parallel_tools`, `reasoning`, `images`, `session_resume`, and `exact_usage` are superseded by the namespaced vocabulary; they are not implicit aliases in v1.

## Error Model

`GatewayError` is infrastructure metadata, separate from an opaque client/provider error body:

| Field | Meaning |
| --- | --- |
| `Code` | Stable gateway error identifier, not a raw provider error code |
| `Category` | `invalid_request`, `unsupported_feature`, `unauthenticated`, `permission_denied`, `rate_limited`, `unavailable`, `timeout`, `cancelled`, or `internal` |
| `Retryable` | Whether the condition may be transient; unspecified is treated as false |
| `RetryDisposition` | `safe`, `unsafe`, or `unknown` for replay of this attempt; unspecified is unknown |
| `Provider` | Optional opaque diagnostic identity; absent for errors without a provider |
| `OriginalError` | Optional sanitized diagnostic detail; never automatically exposed to the client |
| `Message` | Client-safe explanation |
| `RetryAfter` | Optional retry delay interpreted by the Connector from upstream hints |

Adapters classify client validation failures, Connectors classify provider failures, and Core classifies infrastructure failures. Core decisions use category and retry metadata, not provider identity or body parsing. Invalid requests, invalid tool schemas, and authentication failure are non-retryable with the same unchanged inputs/credentials. Rate limits and temporary outages may be retryable; a status code alone does not establish safe replay.

Fallback requires `Retryable = true`, `RetryDisposition = safe`, no commitment, eligible alternative targets, and permission under attempt/deadline/policy limits. An explicitly classified rejection before execution may meet these conditions; an ambiguous disconnect after request delivery does not. A retryable error after commit terminates the current attempt without fallback.

## Usage and Scoped Runtime Services

`UsageReport` has nullable `input_tokens`, `output_tokens`, `reasoning_tokens`, `cached_tokens`, source (`provider`, `estimate`, `unknown`), and completeness. Unknown is not zero. Input/output counters are totals; cached/reasoning details may already be included and must not be added twice. Backend-specific details may remain in namespaced diagnostics, not a universal response model.

The Connector normalizes accounting semantics and supplies final usage in Complete. On interruption Core retains partial/unknown usage and the applied estimate separately, reconciles reservations idempotently by attempt ID, and never treats missing usage as a free execution. Future incremental usage reporting requires a defined extension; repeated Complete frames are forbidden.

Scoped runtime services provide selected instance/account context, credential access, transport, logging, and shared authentication infrastructure. They do not expose the whole database or other accounts' credentials. Connectors may use credentials during invocation but never persist secrets independently. Core owns auth-session state, PKCE storage, lifetime, and atomic credential persistence; refresh is serialized per account. Storage failure after a successful token exchange is reported separately rather than claiming persisted success.

## Versioning and Change Control

The semantic v1 baseline is accepted before implementation; concrete language and IPC bindings must preserve it. Connector API, Adapter API, protocol, component implementation, and IPC versions are separate compatibility dimensions. Descriptors declare API compatibility; the runtime rejects unsupported major versions before execution.

**Every semantic contract change requires an ADR** in [DECISIONS.md](../project/DECISIONS.md), including new operations, required fields, frame variants, capability meanings, or changed lifecycle/error rules. Record motivation, alternatives, compatibility impact, and verification/migration requirements; update this document and affected specifications together. Editorial clarification without changed behavior does not require an ADR.

Additive optional extensions must be explicitly discoverable and keep older components valid within their supported API versions. Breaking changes require a new major API version and a compatibility/migration policy. Unknown required operations, capabilities, or control frames must never be silently accepted. Supporting future agent protocols does not grant unversioned freedom to reinterpret existing frames.

IPC transfers the same opaque payload and lifecycle semantics, with scoped runtime-service calls in the reverse direction where needed. Process handshake, authentication, limits, and cancellation must preserve this contract; selecting a transport must not create a second execution model. M2 validates the contract with native/fake connectors; M6 validates equivalent behavior across the process boundary.
