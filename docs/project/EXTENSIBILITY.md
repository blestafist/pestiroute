# Extensibility Model

PestiRoute is designed around replaceable components with explicit boundaries:

```text
Client Protocol
        |
Protocol Adapter
        |
Core Runtime
        |
Connector
        |
Backend Provider
```

The Core Runtime is the execution runtime: it manages infrastructure and execution without interpreting provider payloads. A Protocol Adapter owns the client-facing API format. A Connector owns Backend Provider communication and provider-specific translation. Raw payloads remain opaque to the Core Runtime, and native passthrough preserves body bytes.

This document extends the [architecture](ARCHITECTURE.md) with extension points intended to avoid future breaking changes. It defines architectural constraints, not concrete interfaces, wire formats, or implementation milestones. The normative [internal execution contract](../implementation/CONTRACT.md) owns the v1 Adapter–Core Runtime–Connector boundary; optional extensions do not change the connector model or introduce a universal LLM abstraction.

## 1. Component Lifecycle

All Protocol Adapters and Connectors are managed components. The Core Runtime manages their lifecycle independently of their client or provider protocols.

The conceptual component operations are:

| Operation | Responsibility |
| --- | --- |
| `Descriptor()` | Describe identity, API compatibility, supported protocols, and capabilities |
| `Init()` | Initialize the component and establish readiness before accepting work |
| `Health()` | Report current health and availability |
| `Close()` | Shut down gracefully and release owned resources |

These are lifecycle concepts, not additions to a language-specific Connector interface. Component shutdown is distinct from closing an individual execution stream. Graceful shutdown stops admission of new work and allows active work to finish or be cancelled under runtime policy.

Configuration reload is a future optional lifecycle extension. Reload support must be declared rather than assumed, and its effect on active executions must be defined before it is enabled. Existing components must remain usable without implementing reload.

## 2. Capability System

Features must not be hardcoded into the Core Runtime. Capabilities are dynamic properties exposed by Protocol Adapters and Connectors, qualified by protocol, execution mode, model, account, and current availability where relevant.

Illustrative capability names include:

```text
llm.streaming
llm.tools
llm.tools.parallel
llm.reasoning
llm.vision
llm.audio
llm.structured_output
session.resume
session.fork
auth.oauth
usage.exact
```

Routing and validation rely on declared capabilities and request requirements, not provider-name branches such as `if provider == anthropic`. The rule is “requires capability X.” The Protocol Adapter validates client-protocol fields and identifies requirements; the Core Runtime checks capability metadata without interpreting tool schemas, reasoning content, or other payload semantics. The Connector determines backend-specific support.

The existing three-valued semantics remain: `supported`, `unsupported`, and `unknown`. Only confirmed support satisfies a requirement. End-to-end support depends on the applicable Protocol Adapter, Connector, protocol, model, and account; a provider-wide claim is insufficient. Component-level capabilities such as authentication are evaluated in their relevant scope rather than required of every layer.

The [contract capability model](../implementation/CONTRACT.md#capability-model) fixes the namespaced vocabulary and three-valued support semantics; earlier short draft keys are superseded under DEC-004. New identifiers require documented scope and meaning without teaching Core their LLM semantics. An unrecognized capability must not be treated as supported. Changes in availability affect eligibility; they do not authorize unsafe retries of active work.

## 3. Execution Envelope

The internal boundary between Protocol Adapters and Connectors is an opaque execution envelope passed through the Core Runtime. Conceptually, an `ExecutionRequest` contains only transport and runtime information:

- Request ID.
- Protocol identifier.
- Model identifier for routing.
- Raw payload.
- Metadata needed for transport and execution management.
- Required capabilities.

The payload remains opaque to the Core Runtime. Metadata does not replace or reconstruct it. Responses consist of transport frames, raw bytes wherever possible, and lifecycle information.

The internal contract must not introduce `Message[]`, `Tool[]`, `Reasoning{}`, or `UniversalResponse`. Such structures leak provider assumptions into the runtime and make new protocols depend on a universal LLM model. Protocol-specific structures belong at the Protocol Adapter or Connector boundary, according to their existing responsibilities.

## 4. Streaming Model

Streaming is transport-oriented. The Core Runtime must not understand text chunks, tool calls, or reasoning chunks. It preserves ordering and backpressure and manages commitment, cancellation, and completion without parsing LLM content.

The conceptual stream concerns are:

| Concern | Meaning |
| --- | --- |
| `HEADERS` | Response status and transport headers |
| `DATA` | Opaque body bytes, preferably unchanged |
| `CONTROL` | Execution lifecycle information |
| `USAGE` | Accounting metadata supplied by the Connector |
| `ERROR` | Common error metadata, separate from protocol-specific error bodies |
| `END` | Terminal execution outcome |

These are conceptual roles, not six new required frame variants. The current contract retains `Head → Body* → Complete → EOF`: `Head` carries headers, `Body` carries data, and `Complete` carries terminal control, outcome, and usage. Typed errors and protocol-compatible error bytes retain their existing roles. Any future frame extension requires explicit API compatibility rules.

Connectors translate Backend Provider streams into internal transport frames and retain provider-specific translation and usage extraction. Protocol Adapters present those frames as OpenAI Responses events, Chat Completion chunks, or Anthropic streaming events. Already client-compatible bytes pass through unchanged; adapters do not repeat connector translation. Body frames need not align with protocol event boundaries.

Usage and control information are not universal LLM events. The Core Runtime does not synthesize provider-specific SSE events. Existing commit and fallback rules still apply: after commitment, an error cannot replace the response or trigger a new backend attempt.

## 5. Error Model

Errors share an infrastructure contract so the Core Runtime can make routing and fallback decisions without parsing provider error bodies. Error metadata includes:

- A stable error code.
- A retryable flag indicating whether the condition may be transient.
- Provider identity, when applicable, as diagnostic metadata.
- The original error, when available, retained for diagnostics with secrets excluded.
- An infrastructure category such as invalid request, authentication failure, rate limit, or unavailability.

Protocol Adapters classify client-protocol validation errors; Connectors classify Backend Provider errors. Provider-specific codes and retry hints are interpreted by the Connector. The Protocol Adapter controls client-facing error representation; internal diagnostics are not automatically exposed to clients.

Rate limits and temporary provider outages are examples of potentially retryable conditions. Invalid requests, invalid tool schemas, and authentication failures are non-retryable for the same unchanged request and credentials.

The retryable flag does **not** establish that replay is safe. The existing retry disposition (`safe`, `unsafe`, or `unknown`) remains authoritative for attempt safety. Fallback requires a confirmed safe retry, an uncommitted response, and permission under attempt limits and deadlines. An ambiguous outcome after sending a request must not become retryable merely because the provider is unavailable. Provider identity is diagnostic, not a reason for provider-specific branches in the Core Runtime.

## 6. Context and Cancellation

Request identity, deadline, and cancellation context propagate through every layer:

```text
Client
   |
Protocol Adapter
   |
Core Runtime
   |
Connector
   |
Backend Provider
```

Client disconnect, timeout, or explicit cancellation must stop associated work and propagate to provider operations through the Connector. A Backend Provider shutdown is reported back through the same execution lifecycle as an error or incomplete outcome. Component shutdown must likewise finish or cancel active executions under runtime policy.

Cancellation releases streams and execution resources and completes accounting and the attempt lifecycle exactly once. It does not prove that the Backend Provider stopped processing or that replay is safe. If the provider cannot confirm cancellation, the outcome remains uncertain under the existing retry rules.

## 7. Session Support

Most APIs are request/response based. Some agent protocols, including Claude Code, Codex, and ACP agents, may expose session-oriented behavior. This does not imply that their Connectors run the official client or an agent process.

`session.resume` and `session.fork` reserve optional extension points. Session identifiers and provider state remain opaque; the Core Runtime may manage affinity and lifecycle metadata without interpreting conversation history. Neither capability is assumed for every agent protocol or Connector.

Session support must preserve binding to the appropriate Connector instance, account, and Backend Provider. Resume or fork does not imply portability across providers or permission for cross-account fallback. The existing stateful Responses restrictions apply until session support is separately specified. No session operations or persistence design are introduced here.

## 8. Model Metadata Registry

A model metadata registry provides discoverable routing information without hardcoding model behavior. Metadata may include:

- Model ID.
- Provider identity.
- Context window, when known.
- Capabilities.
- Availability.

This is metadata only, not a universal model abstraction. Connector model discovery and configured routing information can supply it; this concept does not add provider-specific methods to the Connector contract. Missing information stays unknown rather than being inferred from provider or model names.

Capabilities and availability may vary by account, endpoint, and protocol. Metadata must retain that scope and may change over time. A context-window value does not move tokenization or provider-specific counting into the Core Runtime. Model-name rewriting remains an explicit Connector transformation, not a registry side effect.

## 9. Secrets Abstraction

The Core Runtime owns secret storage, credential references, and access control. Connectors request credentials through a scoped secrets abstraction provided by the runtime; they must not manage secret storage or depend on its implementation.

Provider authentication behavior, including OAuth endpoints, scopes, exchanges, and refresh logic, remains in the Connector. Persistence and shared authentication lifecycle remain runtime responsibilities. Connectors receive only credentials appropriate to the selected account and execution context.

The abstraction leaves room for local encrypted storage, Vault, and cloud secret managers. Choosing a storage backend must not require changing the Connector API or introducing provider logic into the Core Runtime.

## 10. Plugin/API Versioning

Versioning distinguishes independent compatibility dimensions:

| Version | What it describes |
| --- | --- |
| Connector API version | The execution and runtime-service contract used by a Connector |
| Adapter API version | The client-boundary and runtime contract used by a Protocol Adapter |
| Protocol version | The client-facing or Backend Provider protocol supported by the component |
| Component implementation version | A release of a particular adapter or connector |
| IPC transport version | The process-boundary contract where applicable |

A new component release or provider protocol version does not automatically require a new runtime API version. Descriptors declare supported API compatibility and protocols; the Core Runtime checks compatibility before admitting component work. Protocol compatibility and payload opacity are separate from plugin API compatibility.

The goal is for new Core Runtime versions to continue supporting old plugins within explicitly supported API versions. Additive optional capabilities must not become new mandatory operations for existing plugins. Breaking changes require a new major API version and an explicit compatibility or migration policy; unsupported major versions are rejected before execution. Compatibility is declared, not assumed indefinitely.

Unknown optional metadata may be tolerated only where the contract allows it. Unknown required capabilities or lifecycle/control frames must not be silently ignored. Adapter API versioning does not itself prescribe an adapter process model; existing connector isolation rules remain in force.

## 11. Async Execution (Future)

`execution.async` reserves an optional extension point for Backend Providers that may support starting a job, polling status, and retrieving its result.

It is distinct from streaming and session support. Future async execution would retain opaque job references and payloads, with explicit lifecycle, ownership, cancellation, and accounting semantics. A synchronous-only component remains valid without this capability. The capability alone does not promise durable execution, Responses background resources, or session portability.

No async operations or implementation schedule are defined here. They require a separately specified, version-compatible extension before use.
