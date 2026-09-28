# Architecture and Request Path

The [Extensibility Model](EXTENSIBILITY.md) defines component lifecycle, capabilities, transport boundaries, and versioning constraints for evolving Protocol Adapters and Connectors within this architecture.

The [Internal Execution Contract](../implementation/CONTRACT.md) is the normative v1 specification of the Protocol Adapter–Core Runtime–Connector boundary. Semantic changes require an ADR in [DECISIONS.md](DECISIONS.md#contract-adrs).

## Three Explicit Boundaries

The system has Client Protocol Adapters, Core infrastructure, and backend connectors. Each adapter understands its client-facing protocol, extracts minimal metadata, and formats gateway-level errors. OpenAI Responses is the primary northbound protocol; additional adapters extend the client-facing formats. Core only knows about the execution envelope, access policies, and execution state. Connectors understand the upstream and translate its protocol when necessary.

This resolves an important contradiction: the gateway provides an OpenAI-compatible API, but its routing, limits, and runtime are independent of the OpenAI JSON format. The northbound adapter is a separate package, not a set of conditional branches inside Core.

```text
Client
    │ OpenAI Responses / Chat Completions / Anthropic Messages / etc.
    ▼
Protocol Adapter
    │ internal execution envelope
    ▼
Core Runtime
    │
    ▼
Connector
    │
    ▼
Backend Provider
```

Connector output returns through Core as transport frames to the Protocol Adapter, which presents it in the client's protocol format.

## Client Protocol Adapters

Client Protocol Adapters are the northbound architectural layer, separate from Core and connectors. The design supports OpenAI Responses API, OpenAI Chat Completions API, Anthropic Messages API, and potentially Gemini-compatible APIs and other client protocols.

Each adapter is responsible for:

- Parsing incoming protocol requests.
- Validating protocol-specific fields while preserving opaque payloads and unknown extensions where allowed by that protocol.
- Converting requests into a minimal internal execution envelope.
- Encoding the client transport and gateway-level errors. Connector output already conforms to the admitted client protocol; the adapter preserves compatible body bytes and must not repeat semantic translation.

Core **MUST NOT** know OpenAI or Anthropic request structures, convert Responses to Chat Completions, or understand tool call or reasoning formats. It routes and executes opaque envelopes and manages transport lifecycle. Protocol adapters are not implemented inside Core or mixed with connectors.

### Minimal Internal Execution Envelope

The envelope is transport-oriented, not a universal LLM abstraction:

| Direction | Contents |
| --- | --- |
| Request | Protocol identifier, raw payload, and metadata required for routing and execution |
| Response | Stream frames, raw bytes wherever possible, and lifecycle events |

There is no universal `Message[]`, `Tool[]`, or `Reasoning{}` model. Tool calls, reasoning, and other protocol-specific content remain in opaque payloads across Core. This extends the northbound boundary using the existing [connector execution envelope](../implementation/CONTRACT.md), without changing the connector model.

Chat Completions is a **compatibility protocol**, while Responses API is the **primary agent-oriented protocol**. Both are first-class northbound interfaces; Chat Completions does not require a universal intermediate LLM representation or conversion inside Core.

## Protocol Adapter vs Connector

**Protocol Adapter** answers: **"How does a client talk to PestiRoute?"**

Examples: OpenAI Responses, Chat Completions, Anthropic Messages.

**Connector** answers: **"How does PestiRoute talk to a backend?"**

Examples: OpenAI API, Anthropic API, Claude Code protocol, Codex protocol, Ollama.

Connectors remain responsible for backend communication, provider authentication, provider-specific translation, provider-specific streaming, and provider-specific usage. Adapters own the client-facing protocol boundary; connectors retain backend translation under their declared accepted protocols. When a connector already returns client-compatible bytes, the adapter preserves them rather than translating them again. Adding an adapter does not imply that every connector supports that protocol; routing still respects declared protocols and capabilities.

### Example 1: OpenAI Responses Client

```text
OpenCode
    │ /v1/responses
    ▼
OpenAI Responses Adapter
    │
    ▼
Core
    │
    ▼
OpenAI Connector
    │
    ▼
OpenAI API
```

### Example 2: Chat Completions Compatibility

```text
Legacy Client
    │ /v1/chat/completions
    ▼
Chat Adapter
    │
    ▼
Core
    │
    ▼
Anthropic Connector
    │
    ▼
Anthropic API
```

### Example 3: Subscription Backend

```text
OpenCode
    │ /v1/responses
    ▼
Responses Adapter
    │
    ▼
Core
    │
    ▼
Claude Code Protocol Connector
    │
    ▼
Anthropic
```

## Proposed Project Structure

```text
cmd/gateway/                 composition root and startup
internal/northbound/openai/  Responses endpoint and external API errors
internal/core/               request lifecycle and orchestration
internal/routing/            routes, eligibility, and account selection
internal/limits/             admission control and reconciliation
internal/auth/               virtual keys and shared authentication runtime
internal/storage/sqlite/     repositories and migrations
internal/runtime/            registry and connector execution
internal/config/             configuration loading and validation
internal/observability/      logging and metrics
connector/                   shared contract and runtime services
connectors/                  backend implementations
conformance/                 shared test suite
testdata/                    fixtures without secrets
docs/                        project documentation
```

Directories appear as features are implemented. Core and routing do not import `connectors/*` or any client protocol adapter under `internal/northbound/*`. Concrete implementations are wired only at the composition root. Shared helpers for OpenAI JSON and SSE are acceptable in the connector layer but should not become a universal LLM model.

## Connector Types

1. **API Connector** — direct communication with official provider APIs: OpenAI API, Anthropic API, Gemini API.
2. **OpenAI-Compatible Connector** — connects existing backends exposing OpenAI-compatible APIs: OpenRouter, vLLM, Ollama OpenAI endpoint, LM Studio, Together, Groq.
3. **Agent Protocol Connector** — exposes existing AI client/subscription protocols: Claude Code, Codex, Gemini CLI, ACP-based agents. It reproduces how an official client communicates with its backend; it does not necessarily run that client or an agent. For example, `OpenAI Responses API → Claude Code Protocol Connector → Anthropic backend` does not run Claude Code. ACP may involve communication with an agent process.
4. **Local Runtime Connector** — connects local inference runtimes: Ollama native API, llama.cpp, vLLM native endpoints.

A connector may internally use direct API calls, protocol emulation, or local process communication. Core does not care which approach is used; payloads stay opaque across its boundary.

Reverse-engineered connectors are first-class citizens: subscription access, existing authentication flows, and recovered client/backend protocols belong within the same Connector boundary. They do not introduce provider-specific exceptions in Core Runtime.

## Request Lifecycle

The northbound boundary extracts the virtual key for verification by Core-owned authentication services, limits body size, and extracts `model`, `stream`, and other explicitly recognized requirements. Gateway credentials are not placed in the execution envelope or forwarded upstream. Original bytes are preserved. Unknown fields remain in the payload; the adapter does not attempt to fully describe the Responses API with its own schema.

Core applies key constraints and finds suitable route targets based on protocol, model, capabilities, and account state. The connector estimates usage for the selected target; limits atomically reserve the available budget. An attempt is created, after which the runtime invokes `Execute`.

Before sending response headers, the connector reports status and a safe set of headers. Core then streams chunks with backpressure. On completion, usage and outcome are recorded, and the reservation is reconciled. Client disconnect cancels the upstream, closes the stream, and completes the attempt exactly once.

## Native Passthrough and Model Mapping

In native mode, the body passes through byte-for-byte unchanged. Replacing the gateway Authorization with upstream credentials, removing hop-by-hop headers, and recalculating transport headers are permitted. The passthrough guarantee does not mean forwarding all incoming headers to the provider.

Model mapping has two distinct meanings. **Route selection** based on the original model name does not change the payload and is compatible with passthrough. **Model name replacement** in JSON is already a transformation; it is performed by the connector in explicitly enabled translation/rewrite mode. A native mode configuration with a mismatched upstream model name is rejected at startup.

The generic `openai-compatible` connector cannot assume that Chat Completions support implies Responses support. Each instance explicitly declares the upstream protocol. Responses-native backends allow passthrough; Chat-only backends require a separate translator within the connector and their own capability matrix.

## Streaming and Backpressure

After sending response headers, status and headers are considered committed. Core does not buffer the complete response or reorder chunks. Queues are bounded; a slow client slows upstream reads instead of allowing unbounded memory growth. The completion control message contains outcome and usage separately from user-facing bytes.

An error before commit can be converted to an HTTP error response. After commit, the response cannot be replaced with new JSON and a different status code. The connector may emit a protocol-appropriate terminal error event; on a corrupted stream or process crash, the transport closes and the attempt is recorded as incomplete. Core does not synthesize provider-specific SSE events.

## Fallback and Retries

Fallback is only permitted before commit and with confirmed safe retry capability. Connection errors before sending the request, local connector unavailability, and explicitly classified upstream rejections are different cases. Connection loss after sending the body has an unknown outcome and by default is not automatically retried, even if the client has not yet received any bytes.

The connector reports error category and retry disposition: `safe`, `unsafe`, or `unknown`. Core applies policy with attempt count limits and an overall deadline. Provider error codes and `Retry-After` headers are interpreted by the connector. After commit, fallback is forbidden: stitching together responses from different attempts violates tool IDs, event ordering, and usage tracking.

## Stateful Responses

`previous_response_id`, session resume, stored responses, and background execution do not automatically become portable across backends. In the first native implementation, they may pass through to the same upstream as opaque fields, but this is not a guarantee of full Responses resource API implementation.

Session-bound requests require affinity to the original connector instance, account, and backend. Until affinity is implemented, the configuration uses a single target for such scenarios and cross-account fallback is disabled. The translation connector explicitly rejects unsupported stateful features. Retrieval, deletion, cancellation by response ID, and background execution will be scoped separately after basic POST and streaming are complete.

## Isolation

Until M6, built-in first-party connectors execute in-process with the gateway under the same contract. This simplifies architecture validation but does not provide process isolation. External third-party connectors will be integrated after the process runtime becomes available.

Go native plugins are not used. External Connectors communicate with Core Runtime through a versioned IPC contract so a plugin failure cannot crash the gateway process.

Process isolation protects Core from connector crashes. CPU, memory, filesystem, and network access restrictions are separate sandbox capabilities; IPC transport alone does not provide such guarantees. The runtime should at minimum limit message sizes, queues, and process termination timeouts.
