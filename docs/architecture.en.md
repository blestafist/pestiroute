# Architecture and Request Path

## Three Explicit Boundaries

The system has an external protocol adapter, Core infrastructure, and backend connectors. The adapter understands the client-facing OpenAI Responses contract, extracts minimal metadata, and formats gateway-level errors. Core only knows about the execution envelope, access policies, and execution state. Connectors understand the upstream and translate its protocol when necessary.

This resolves an important contradiction: the gateway provides an OpenAI-compatible API, but its routing, limits, and runtime are independent of the OpenAI JSON format. The northbound adapter is a separate package, not a set of conditional branches inside Core.

```text
HTTP client
    │
    ▼
Northbound adapter ── original body + metadata ──► Core executor
    ▲                                                 │
    │ status / headers / bytes                        │ attempt
    │                                                 ▼
    └────────────────────────────────────────── Connector runtime
                                                      │
                                                      ▼
                                                   Backend
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

Directories appear as features are implemented. Core and routing do not import `connectors/*` or `internal/northbound/openai`. Concrete implementations are wired only at the composition root. Shared helpers for OpenAI JSON and SSE are acceptable in the connector layer but should not become a universal LLM model.

## Request Lifecycle

The northbound adapter validates the virtual key, limits body size, and extracts `model`, `stream`, and other explicitly recognized requirements. Original bytes are preserved. Unknown fields remain in the payload; the adapter does not attempt to fully describe the Responses API with its own schema.

Core applies key constraints and finds suitable route targets based on protocol, model, capabilities, and account state. The connector estimates usage for the selected target; limits atomically reserve the available budget. An attempt is created, after which the runtime invokes `Execute`.

Before sending response headers, the connector reports status and a safe set of headers. Core then streams chunks with backpressure. On completion, usage and outcome are recorded, and the reservation is reconciled. Client disconnect cancels the upstream, closes the stream, and completes the attempt exactly once.

## Native Passthrough and Model Mapping

In native mode, the body passes through byte-for-byte unchanged. Replacing the gateway Authorization with upstream credentials, removing hop-by-hop headers, and recalculating transport headers are permitted. The passthrough guarantee does not mean forwarding all incoming headers to the provider.

Model mapping has two distinct meanings. **Route selection** based on the original model name does not change the payload and is compatible with passthrough. **Model name replacement** in JSON is already a transformation; it is performed by the connector in explicitly enabled translation/rewrite mode. A native mode configuration with a mismatched upstream model name is rejected at startup.

The generic `openai-compatible` connector cannot assume that Chat Completions support implies Responses support. Each instance explicitly declares the upstream protocol. Responses-native backends allow passthrough; Chat-only backends require a separate translator within the connector and their own capability matrix.

## Streaming and Backpressure

After the first response header, status and headers are considered committed. Core does not buffer the complete response or reorder chunks. Queues are bounded; a slow client slows upstream reads instead of allowing unbounded memory growth. The completion control message contains outcome and usage separately from user-facing bytes.

An error before commit can be converted to an HTTP error response. After commit, you cannot replace the response with new JSON and a different status code. The connector may emit a protocol-appropriate terminal error event; on a corrupted stream or process crash, the transport closes and the attempt is recorded as incomplete. Core does not synthesize provider-specific SSE events.

## Fallback and Retries

Fallback is only permitted before commit and with confirmed safe retry capability. Connection errors before sending the request, local connector unavailability, and explicitly classified upstream rejections are different cases. Connection loss after sending the body has unknown outcome and by default is not automatically retried, even if the client has not yet received any bytes.

The connector reports error category and retry disposition: `safe`, `unsafe`, or `unknown`. Core applies policy with attempt count limits and an overall deadline. Provider error codes and `Retry-After` headers are interpreted by the connector. After commit, fallback is forbidden: stitching together responses from different attempts violates tool IDs, event ordering, and usage tracking.

## Stateful Responses

`previous_response_id`, session resume, stored responses, and background execution do not automatically become portable across backends. In the first native implementation, they may pass through to the same upstream as opaque fields, but this is not a guarantee of full Responses resource API implementation.

Session-bound requests require affinity to the original connector instance, account, and backend. Until affinity is implemented, the configuration uses a single target for such scenarios and cross-account fallback is disabled. The translation connector explicitly rejects unsupported stateful features. Retrieval, deletion, cancellation by response ID, and background execution will receive separate scope after basic POST and streaming are complete.

## Isolation

Until M6, built-in first-party connectors execute in-process with the gateway under the same contract. This simplifies architecture validation but does not provide process isolation. External third-party connectors will be integrated after the process runtime becomes available.

Process isolation protects Core from connector crashes. CPU, memory, filesystem, and network access restrictions are separate sandbox capabilities; IPC transport alone does not provide such guarantees. The runtime should at minimum limit message sizes, queues, and process termination timeouts.
