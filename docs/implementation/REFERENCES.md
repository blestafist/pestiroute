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

## M4 Translation Sources

The [M4 planning brief](M4-TRANSLATION.md#source-selection) links official Messages
create/stream/tool/thinking/cache/error documentation and the Responses streaming
reference inspected on 2026-10-03. M4-001 selects the exact direct API/model/client
profile; M4-002 records versioned fixture provenance and reuse attribution.
These mutable source pages do not establish PestiRoute live compatibility.
Codex subscription research belongs to M5.1 and must not supply Anthropic auth,
request fields, stream events or usage semantics by analogy.

## M1 Compatibility Baseline

**Historical selection (FND-005, source review 2026-09-29; no inference run):** OpenAI public Responses / `gpt-4.1-mini-2025-04-14` with OpenCode V2 CLI 2.0.6 was selected on source evidence. This records the original decision, not the current M1 smoke model.

**Current smoke baseline:** OpenAI **public API** `POST https://api.openai.com/v1/responses`, model `gpt-5.4-mini` (not a ChatGPT subscription endpoint), with **OpenCode V2 CLI 2.0.6**. [OpenAI model details](https://platform.openai.com/docs/models/gpt-5.4-mini) identify the current model and API. This migration is configuration synchronization only; it does not establish live behavior or feature support. [Function calling](https://platform.openai.com/docs/guides/function-calling) and [HTTP streaming](https://platform.openai.com/docs/guides/streaming-responses) describe API behavior generally, not verified behavior for this exact client/model/account combination.

**Client source:** [OpenCode V2 installation](https://opencode.ai/v2/docs/) listed the 2.0.6 CLI binary on 2026-09-29; install that exact release and confirm `opencode --version` before the smoke. [V2 providers](https://opencode.ai/v2/docs/providers#packages) names `@opencode/ai/providers/openai/responses` (do **not** select `/openai/chat` or a subscription login); its [endpoint override](https://opencode.ai/v2/docs/providers#endpoint) uses `providers.openai.settings.baseURL`, and its [WebSockets section](https://opencode.ai/v2/docs/providers#websockets) allows `settings.transport: "http"` to keep the gate on HTTP/SSE. [V2 provider accounts](https://opencode.ai/v2/docs/cli/providers#environment) documents server-process credentials and saved-account precedence. Pinned [OpenCode v2.0.6 tag](https://github.com/anomalyco/opencode/tree/b084acc55ea2cdb50e9c2ec49a8d9ab3608d43ad): its [Responses tests](https://github.com/anomalyco/opencode/blob/b084acc55ea2cdb50e9c2ec49a8d9ab3608d43ad/packages/ai/test/provider/openai-responses.test.ts#L377-L398) map parallel-tool settings to `parallel_tool_calls`; [interleaved-call test](https://github.com/anomalyco/opencode/blob/b084acc55ea2cdb50e9c2ec49a8d9ab3608d43ad/packages/ai/test/provider/openai-responses.test.ts#L2345-L2387) retains separate call IDs; [tool execution](https://github.com/anomalyco/opencode/blob/b084acc55ea2cdb50e9c2ec49a8d9ab3608d43ad/packages/core/src/session/runner/step.ts#L99-L142) forks local tools and joins them, while the [runner](https://github.com/anomalyco/opencode/blob/b084acc55ea2cdb50e9c2ec49a8d9ab3608d43ad/packages/core/src/session/runner/llm.ts#L187-L249) prepares subsequent steps from session history and the [transcript builder](https://github.com/anomalyco/opencode/blob/b084acc55ea2cdb50e9c2ec49a8d9ab3608d43ad/packages/core/src/session/runner/to-llm-message.ts#L187-L206) includes tool calls with their results. The [Responses continuation test](https://github.com/anomalyco/opencode/blob/b084acc55ea2cdb50e9c2ec49a8d9ab3608d43ad/packages/ai/test/provider/openai-responses.test.ts#L590-L635) checks `call_id`-linked result input on a next request. These are client implementation/test evidence, **not** a live OpenCode 2.0.6 run with this model; M1 must verify the actual request path, tool continuation and parallel delivery from traces. This selects the agent client named in [success criteria](../project/README.md#success-criteria), not the Codex subscription protocol.

**Access guardrails:** only run a live probe when an authorized OpenAI Platform API credential is explicitly supplied and available quota is confirmed within the user-authorized 2.5 million free tokens per day. Zero paid spend is authorized: fail closed if authentication/access is missing, quota cannot be confirmed, or any paid billing could occur; never infer authorization from ambient credentials or a ChatGPT login. Do not inherit saved client accounts; supply a key only to the client process and gateway upstream through separate, explicitly configured channels. Never put secrets in project config, commands, traces or fixtures. Environment credential/quota status is not evidence of permission to incur charges.

**M1 procedure (not executed):** use a disposable project containing two harmless, distinct text files (for example `alpha.txt` and `beta.txt`), with no private data. For both legs use OpenCode 2.0.6, the same `openai/gpt-5.4-mini` model and identical project, agent, prompt, permissions and provider settings; pin `modelID` to `gpt-5.4-mini` in a disposable `opencode.jsonc` if the catalog lacks it. Configure the OpenAI provider's Responses runtime explicitly (`package: "@opencode/ai/providers/openai/responses"`), `settings.transport: "http"`, and no unplanned variant/body overrides; check actual outgoing URL/path and model ID before claiming the run. First use the default OpenAI API base URL; then change **only** `providers.openai.settings.baseURL` to the M1 gateway's documented local `/v1` base URL, keeping model ID and HTTP settings fixed. Apply the access guardrails above before either leg. Use separate fresh sessions; do not compare generated prose across independent runs.

Prompt: “Read alpha.txt and beta.txt with separate read calls; request both without waiting for one result before requesting the other. Report their two first lines and identify each filename.” Observe a completed single-tool call and its subsequent model continuation first (prompt “Read alpha.txt and report its first line”), then the parallel prompt. If the model makes only sequential calls, retry the same documented prompt/settings in both legs and record that parallel behavior was **not** demonstrated; do not infer parallel support from the API's permission to call multiple tools. For each leg record sanitized request path, model and `stream` setting, first SSE event arrival **before completion**, event order, per-item `id`/`output_index` and `call_id`, complete argument streams, tool execution/results and subsequent request(s), final `response.completed` or error, and per-round usage when available. Confirm each tool output is associated with its own call ID and item ordering is preserved; compare structures and relationships, not generated text or newly generated IDs between legs. Repeat with a longer streaming response and explicitly cancel/close the client request after its first event; check upstream cancellation/cleanup and absence of a second response or retry. If the OpenCode UI hides raw events or does not cancel HTTP on session interruption, capture a sanitized transport trace and use the M1 deterministic fake upstream to establish the lifecycle invariant; report real-client cancellation as unverified until observed.

For M1, put sanitized `direct.json` and `gateway.json` trace metadata/observations plus `README.md` (scenario, sources, versions and expected invariants) under `docs/implementation/evidence/m1/openai-responses/gpt-5.4-mini/opencode-2.0.6/` (create only when the smoke runs), grouped by connector/protocol/client/API version per [TESTING](TESTING.md#fixtures-and-traces); never commit keys, headers, prompts from private projects or raw account identifiers. Unknown until M1: actual OpenCode 2.0.6 wire protocol and model availability for the account, real parallel invocation, SSE first-event timing, cancellation propagation, rate limits and direct-versus-gateway equivalence. Model documentation does not establish support for all Responses fields, reasoning, background jobs or session resume; do not declare any capability `supported` from this selection.

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

Separate the evidence for each compatibility claim:

- **Source evidence:** what documentation states or code implements, with the exact repository/document, revision or API version, and relevant location. An implementation in another project does not prove PestiRoute compatibility.
- **Runtime evidence:** the scenario actually executed, date, tested revisions and client/backend versions, result, and fixture or check reference. Distinguish deterministic local checks from real-backend smoke tests; results cover only the tested combination and behavior.
- **Unknowns:** unverified properties, conflicting evidence, and the next check needed to resolve them.

These are research evidence categories, not replacements for the contract's capability statuses. Source evidence alone must not promote an unverified capability to `supported`.
