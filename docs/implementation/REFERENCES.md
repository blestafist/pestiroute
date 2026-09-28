# References and Research Process

## How to Use Sources

Below are research starting points, not a promise to support all described capabilities. When implementing a connector, record the date, API/client version, or commit of the source, and convert the required behavior into fixtures. API documentation, official client code, and observed traces answer different questions; one source does not replace the others.

## Protocols and Official Implementations

| Source | What to Study |
| --- | --- |
| [OpenAI Responses API](https://platform.openai.com/docs/api-reference/responses) | Request/response contract, tool items, usage, stateful operations |
| [OpenAI streaming events](https://platform.openai.com/docs/api-reference/responses-streaming) | Event lifecycle, deltas, completion and error events |
| [OpenAI Python client](https://github.com/openai/openai-python) | Client transport, auth, and exact compatibility scope of the selected version |
| [Anthropic API documentation](https://docs.anthropic.com/) | Messages, streaming, tools, token counting, and usage |
| [Anthropic Python SDK](https://github.com/anthropics/anthropic-sdk-python) | Client implementation, streaming patterns, and tool use examples |
| [Gemini API](https://ai.google.dev/gemini-api/docs) | Official API, multimodal inputs, tools, and streaming |
| [Gemini CLI](https://github.com/google-gemini/gemini-cli) | Real client protocol and auth flows of a specific version |
| [Ollama documentation](https://docs.ollama.com/) | Native API and actual scope of its OpenAI compatibility |
| [vLLM documentation](https://docs.vllm.ai/) | Serving endpoints, supported fields, and model limitations |
| [llama.cpp](https://github.com/ggml-org/llama.cpp) | Server implementation and supported compatibility endpoints |
| [Agent Client Protocol](https://agentclientprotocol.com/) | Agent-process lifecycle and distinction from provider API |

OpenAI-compatible does not mean full support for the Responses API. For each upstream, endpoint, streaming, tools, reasoning, and session semantics must be verified separately. Similarly, the official public API does not necessarily match the backend protocol of a subscription client.

## Infrastructure

| Source | Application |
| --- | --- |
| [Go net/http](https://pkg.go.dev/net/http) | Streaming, transports, cancellation, and server lifecycle |
| [Go context](https://pkg.go.dev/context) | Passing deadlines and cancellation signals between layers |
| [SQLite WAL](https://www.sqlite.org/wal.html) | Concurrent access and operational storage characteristics |
| [SQLite Online Backup](https://www.sqlite.org/backup.html) | Consistent backups of an active database |
| [gRPC flow control](https://grpc.io/docs/guides/flow-control/) | Backpressure for external runtime |
| [Protocol Buffers](https://protobuf.dev/programming-guides/) | Evolving control contracts without changing opaque payloads |
| [OAuth 2.0, RFC 6749](https://www.rfc-editor.org/rfc/rfc6749) | General roles and authorization flow lifecycle |
| [PKCE, RFC 7636](https://www.rfc-editor.org/rfc/rfc7636) | Common component of auth runtime |
| [Device Authorization, RFC 8628](https://www.rfc-editor.org/rfc/rfc8628) | Foundation for device flows where used by the backend |
| [SSE specification](https://html.spec.whatwg.org/multipage/server-sent-events.html) | Framing, multiline data, and event stream handling |

## Migration from 9Router

Local source research is recorded in the [9Router map](../references/9router-migration.md), [9Gateway map](../references/9gateway-migration.md), and [source-evidence matrix](../references/compatibility-matrix.md), including inspected commit IDs. These maps refer to ignored local checkouts, not vendored dependencies or verified PestiRoute capabilities. The inspected `9router-go` checkout is distinct from its cited upstream `decolua/9router`. Before copying code, establish reproducible repository provenance and resolve the unknown licenses; before compatibility claims, validate the selected client/provider versions. The project name alone is insufficient for source selection.

**9Router migration principle: provider code is migrated, not architecture.** Reuse provider adapters, OAuth flows, request builders, stream parsers, usage extraction, model handling, and protocol knowledge. Do not migrate old routing, old core abstractions, or old internal LLM models.

For each adapter, investigate auth flow, request assembly, stream parser, usage extraction, model ID handling, and known regression tests. First, form a minimal trace and list of features, then adapt the code to the new connector contract. Every migrated connector must pass direct-vs-gateway compatibility tests.

## Connector Research Template

A brief note should contain backend/client version, protocol source, auth methods, supported northbound features, native/translation mode, retry semantics, and statefulness. It concludes with a minimal reproducible request/response fixture and a list of still-unknown properties. This provides a sufficient basis for estimating implementation milestones.
