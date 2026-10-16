# M9 Chat Completions binding

This is the M9 language/protocol binding for the existing [v1 execution
contract](CONTRACT.md) and [Go component binding](M2-BINDING.md), not a semantic
contract change. It binds only native Chat Completions; it does not convert
Chat to Responses, claim support for every OpenAI-compatible server, or enable
the endpoint. No ADR or `CONTRACT.md` change is required: the existing contract
already admits an exact client protocol, opaque payload, scoped capabilities,
native Connector and adapter-owned HTTP framing.

## Authority and source baseline

Public protocol references checked 2026-10-16 (mutable provider documentation;
this date records access, not an asserted publication/version date):

- [OpenAI Chat Completions create reference](https://platform.openai.com/docs/api-reference/chat/create)
  (`POST /chat/completions`; request/response shapes, tools, streaming and
  `stream_options.include_usage`).
- [OpenAI Chat Completions streaming events](https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events)
  (stream chunk and terminal event examples).
- [Pinned raw OpenAPI source (`openai/openai-openapi`, revision
  `6ca9cb2b2dd5fbe766c32f3ab898e2e479c29840`)](https://raw.githubusercontent.com/openai/openai-openapi/6ca9cb2b2dd5fbe766c32f3ab898e2e479c29840/openapi.yaml)
  (`/chat/completions`; publisher commit dated 2026-10-16, retrieved
  2026-10-16). This is the machine-readable source baseline; API-reference
  pages remain mutable.
- [WHATWG Server-Sent Events](https://html.spec.whatwg.org/multipage/server-sent-events.html)
  (event framing rules).

The public Chat API is the format authority, not a guarantee that a compatible
backend/model accepts every public parameter. Backend support remains scoped and
must be evidenced independently.

## Component and route binding

| Item | Exact binding |
| --- | --- |
| Adapter package | `internal/adapter/chatcompletions` (new, separate from Responses) |
| Adapter descriptor ID | `pestiroute.chat-completions.native` |
| Protocol | `openai.chat-completions.v1` |
| Client endpoint | `POST /v1/chat/completions` |
| Native Connector package | `internal/connector/chatcompletions` (new, separate from Responses) |
| Native Connector descriptor ID | `pestiroute.chat-completions.native` |
| Connector type / auth | Existing `api` / scoped `bearer` binding; no new auth/storage model |
| Transport | Native OpenAI Chat Completions JSON/SSE over the existing runtime HTTP transport |

Adapter and Connector descriptor IDs are in separate component-kind
namespaces; runtime instance IDs remain configured and distinct. This binding
does not make the Responses Connector Chat-compatible. The native Connector
declares only `openai.chat-completions.v1` and sends this protocol's unchanged
request body to its configured Chat endpoint (default API path shape
`/v1/chat/completions`). A URL is not proof of protocol support: initialization
validates its Chat path/configuration; it does not probe or infer support from
provider name. Exact protocol/model/account route selection remains Core-owned.

## Decode, admitted shapes and capability requirements

Ingress uses the existing configured request/header bounds, requires one
`application/json` content type and identity content encoding, and rejects
invalid JSON, duplicate keys at any nesting level, trailing JSON values,
non-object top-level values, absent/empty/non-string `model`, malformed known
fields and a missing/empty/non-array `messages`. `messages` is a non-empty
array of objects with exactly one recognized `role`: `system`, `developer`,
`user`, `assistant`, `tool`, or deprecated `function`. Role-specific content,
IDs, names, assistant history, tool results, ordering and unknown message
properties are opaque and are not normalized. The Adapter inspects only the
following well-known structures to enforce the declared subset and derive
requirements; it retains the complete original body bytes (including order,
whitespace and unrecognized properties) and never reconstructs a request.

| Field/representation | Admitted shape and behavior | Requirement / rejection |
| --- | --- | --- |
| `model`, `messages`, `stream` | Non-empty model string; non-empty messages array as above; `stream` absent, null, or boolean | Null is public-schema nullable and means the default non-streaming behavior; only true requires `llm.streaming`, false adds none |
| Message content by role | `system`/`developer`: string or text-part array. `user`: string or array of text/image/audio/file parts. `assistant`: optional string/null or text/refusal-part array. `tool`: string or text-part array and non-empty `tool_call_id`. Deprecated `function`: string/null `content` and non-empty `name`. Other message fields stay opaque. | Malformed known part rejects; role-incompatible or unknown part type is unsupported (not silently dropped); `file` is rejected below |
| Function-tool history | Assistant `tool_calls` entries must be `{id:string,type:"function",function:{name:string,arguments:string}}`; a `tool` result has a non-empty `tool_call_id`. Deprecated assistant `function_call` is `{name:string,arguments:string}`; deprecated `function` role result remains opaque. | Any function call/result history requires `llm.tools`; unknown/custom tool-call types reject |
| `tools` | If present, array of `{type:"function",function:{name:string,description?:string,parameters?:object,strict?:boolean|null}}`; empty array adds no requirement. Schemas are opaque JSON values. | Non-empty array requires `llm.tools`; custom tools and malformed/unknown tool definitions reject |
| Deprecated `functions` | If present, array of `{name:string,description?:string,parameters?:object}`; empty adds none. Body is forwarded unchanged. | Non-empty array requires `llm.tools` |
| `tool_choice` | String `none`, `auto`, or `required`; or named function choice `{type:"function",function:{name:string}}`. Deprecated `function_call` selector accepts `none`, `auto`, or `{name:string}`. | Any non-`none` choice requires `llm.tools`; other/custom/allowed-tools forms reject |
| `parallel_tool_calls` | Optional boolean, including false | `true` requires `llm.tools.parallel` (and `llm.tools` if a tool/function definition or history is also present); wrong type rejects |
| `stream_options` | Null/absent means no stream options; otherwise object with optional boolean `include_usage` and `include_obfuscation`; recognized values are validated. | Non-null object requires `stream:true`; malformed known values reject; no capability beyond `llm.streaming` |
| `reasoning_effort` | Null/absent or one of `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max` | Any non-null value requires `llm.reasoning`; other values reject |
| `response_format` | `{type:"text"}`, `{type:"json_object"}`, or `{type:"json_schema",json_schema:{name:string,description?:string,schema?:object,strict?:boolean|null}}` | JSON modes require `llm.structured_output`; malformed/other format rejects |
| Image input | User content part `{type:"image_url",image_url:{url:string,detail?:"auto"|"low"|"high"|"original"}}` | Requires `llm.vision`; malformed shape rejects |
| Audio input/history/output | User part `{type:"input_audio",input_audio:{data:string,format:"wav"|"mp3"}}`; assistant audio history `{audio:{id:string}}`; top-level output absent/null or `{audio:{format:"wav"|"aac"|"mp3"|"flac"|"opus"|"pcm16",voice:string|{id:string}}}`; or `modalities` including `audio`. Payload strings stay opaque. | Null audio means absent; non-null audio shape or audio modality requires `llm.audio`; malformed recognized shape rejects |
| File content part | Chat file part (`type:"file"`) | Explicitly unsupported in this binding; reject before dispatch (there is no file capability in the v1 vocabulary) |
| `modalities` | Absent/null means unspecified; otherwise unique array containing only `text` and/or `audio` | Null adds no requirement; `audio` requires `llm.audio`; unknown/duplicate values reject |
| `web_search_options` | Any non-null value is not admitted | Reject as unsupported: no applicable v1 capability/translation is defined |
| `prediction` | Preserve as an opaque body field; do not inspect or rewrite its content | No gateway capability is claimed; backend acceptance is not inferred |

Every other public Chat request parameter and unknown extension (including
sampling/length controls, metadata, prompt-cache fields, safety identifiers,
and output-affecting options not listed above) remains opaque pass-through: no
adapter validation, capability inference, stripping, defaulting or rewrite.
Provider rejection is returned under normal Connector error handling. `null`
is treated as absent only for fields explicitly nullable above: `stream`,
`stream_options`, `reasoning_effort`, `audio`, and `modalities` (plus
role-specific message fields as listed). These null semantics follow the
pinned OpenAI OpenAPI schema. Null on other inspected fields is invalid. Duplicate
keys remain invalid. The list is a gateway admission subset, not a claim that
every backend/model accepts it.

An absent requirement is unknown, not supported. Only `supported` from both
Adapter and Connector at exact `(openai.chat-completions.v1, native, model,
account)` scope satisfies a request; no provider-name inference, implicit
alias, route declaration override, or fallback to `openai.responses.v1` is
allowed. `Metadata.Streaming` mirrors the recognized boolean; absent is
unspecified. `Payload.Protocol` is exactly `openai.chat-completions.v1` and
`ContentType` is `application/json`. Core derives account selection only from
trusted config/policy, never request metadata.

## Connector execution and usage

The Connector performs one native request per `Execute`, using the original
payload bytes and selected account credentials/transport. It may set transport
headers required by the endpoint, but does not rewrite the body, translate
messages/tools, retry, change account, or turn the protocol into Responses. Its
response `Head.Protocol` must exactly equal the admitted Chat protocol.
Non-success provider responses remain opaque body bytes with classified error
metadata; ambiguous delivery is unsafe to retry. Core owns retry eligibility
and never retries after Adapter commitment.

For JSON responses, emit the upstream body bytes unchanged as one or more Body
frames and map Chat `usage` to the contract `UsageReport` as follows:

| Chat usage path | Contract handling |
| --- | --- |
| `usage.prompt_tokens` | `InputTokens` |
| `usage.completion_tokens` | `OutputTokens` |
| `usage.prompt_tokens_details.cached_tokens` | `CachedTokens` |
| `usage.completion_tokens_details.reasoning_tokens` | `ReasoningTokens` |
| `usage.total_tokens` | Ignore; v1 `UsageReport` has no total field, and input/output already supply the billable totals |
| `usage.prompt_tokens_details.audio_tokens`, `.text_tokens`, `.image_tokens`, `.cache_write_tokens` | Ignore; no corresponding contract counters |
| `usage.completion_tokens_details.accepted_prediction_tokens`, `.rejected_prediction_tokens`, `.audio_tokens`, `.text_tokens` | Ignore; no corresponding contract counters |
| Other provider usage extensions | Ignore; do not invent contract fields |

Input/output counts must both be present, integral and non-negative. Optional
cached/reasoning detail counts are reported only when integral, non-negative and
no greater than their input/output totals. Duplicate or malformed recognized
usage fields make usage unknown; missing `usage` is unknown, not zero. A valid
report uses source `provider` and completeness `complete`. Cached/reasoning
details are subsets that may already be included in input/output: never add
them again. For SSE, inspect
events incrementally, forward every source byte unchanged, and retain final
usage only from the optional usage-only chunk (`choices: []`) when
`stream_options.include_usage` was requested or the provider supplies it.
Usage-only chunks are still forwarded as ordinary response bytes. An interrupted
stream missing that chunk has unknown/partial usage; it is not a free execution.
Core finalizes once from `Complete` and any already observed usage.

## HTTP response framing, commitment and failures

- JSON success/error representation from the Connector is already Chat-format;
  Adapter writes the `Head` status/content type, then Body bytes without
  decoding or re-encoding. Gateway errors before a stream/head use the existing
  safe JSON gateway-error envelope.
- Streaming response uses `Content-Type: text/event-stream` and native SSE
  bytes, delivered incrementally under existing bounded backpressure. A
  protocol event is delimited by a blank line; multiple `data:` lines in an
  event are joined according to SSE. Adapter does not parse or synthesize Chat
  chunks.
- `[DONE]` is the Chat stream terminal marker, normally `data: [DONE]\n\n`.
  It is Connector/provider protocol content, not a Core frame or usage signal.
  Forward it exactly once as received; Complete follows only after a valid
  upstream terminal sequence. Missing `[DONE]`, malformed event syntax/JSON,
  or EOF mid-event yields incomplete/failure, never fabricated `[DONE]` or
  success. For an already committed stream, terminate the transport without a
  replacement status/body; an in-band error is allowed only if the source
  protocol/provider supplied one and the Connector preserves it.
- When requested, an optional final usage-only SSE chunk (`choices: []`, usage
  populated) precedes `[DONE]`. It does not replace the terminal marker.
- Provider HTTP failure before commit is classified before `Head`, retaining
  only safe retry metadata; a committed provider error is forwarded without
  replay. A client write error/disconnect cancels the stream/request context,
  closes the upstream body, and finalizes the attempt cancelled/incomplete once.
- Required execution lifecycle remains `Head → Body* → Complete → EOF`.
  A missing marker, malformed/truncated stream or upstream failure must not be
  turned into `Complete(succeeded)`; preserve known partial usage and the
  classified cause.

## Protected YAML and admin behavior

Protected Chat routing is additive to the strict [M3 protected YAML schema](M3-CONFIG.md#protected-yaml-schema-version-1),
not a new config mode or admin API. M9-003 may admit exactly this additional
connector implementation/protocol; all existing top-level YAML sections and
fields, duplicate/null/unknown-field rejection, startup isolation, policy,
budget, account, target, and retry rules remain as specified by M3:

```yaml
connectors:
  - id: chat-upstream                 # unique connector instance ID
    kind: connector
    implementation: pestiroute.chat-completions.native
    protocols: [openai.chat-completions.v1]
    settings:
      base_url: https://api.example/v1/chat/completions
      upstream_protocol: openai.chat-completions.v1
      mode: native
      credential_env: CHAT_UPSTREAM_TOKEN
      max_request_body_bytes: 1048576
      max_request_header_bytes: 16384
      connect_timeout: 2s
      tls_handshake_timeout: 2s
      response_header_timeout: 5s
      stream_idle_timeout: 30s
routes:
  - id: chat-model
    protocol: openai.chat-completions.v1
    mode: native
    model: backend-model
    adapter: pestiroute.chat-completions.native
    policy: standard
    budget: {unknown_estimate: reject}
    targets: [{connector: chat-upstream, account: chat-account}]
```

The connector entry has exactly the existing protected component fields
`id`, `kind`, `implementation`, `protocols`, and `settings`; `process` is not
accepted for this in-process built-in. `protocols` has exactly one value as
shown. The Chat connector settings are exactly the listed keys above:
`base_url` is the full POST endpoint and must be an absolute URL with a
non-empty host, no opaque component, userinfo, query, forced empty query or
fragment; HTTPS is required except HTTP is permitted only for a numeric
loopback IP. Its escaped path must end in exactly
`/v1/chat/completions` (allowing a deployment prefix before that suffix, but no
trailing slash).
`upstream_protocol` must exactly match the singleton `protocols` value and
`mode` is exactly `native`. `credential_env` is a non-empty environment
variable name whose value must exist and be non-empty at startup; only scoped
bearer auth is supported. The two request limits and all four Go transport
durations are required and positive. Reject every additional or
Responses-specific setting; secrets are not literal YAML values. The server's
`max_request_bytes` remains the ingress cap, while Connector body/header limits
are enforced against the admitted body and original request headers before
dispatch, as in M3.

The route's `adapter` is the built-in implementation ID, not an Adapter
instance reference: the composition root registers the Chat Adapter under that
implementation. `targets[].connector` is the configured connector instance ID;
`targets[].account` is an existing enabled SQLite account belonging to it.
`policy`, required `budget`, optional `requirements`, and optional `retry` keep
their M3 meanings. Protocol, mode, model and adapter are exact; a route must
not mix Responses protocol with Chat implementation. There is no implicit
Chat route or compatibility alias. M9-003 must extend strict validation and
composition allowlists for this exact implementation/protocol and fail startup
on unknown or mixed settings; it does not change storage schema or the other
M3 accepted formats.

Use existing M3 virtual-key authentication, authorization, model/connector
policy, limits, reservation/reconciliation, scoped account credential access,
and one-time attempt finalization. Do not introduce a new admin endpoint,
resource, or protocol-specific credential path: administrators continue to
manage keys/accounts/policies through the existing local CLI and SQLite-backed
state, and route topology remains startup YAML. A wrong/missing route,
account, policy, component, or unsupported capability fails closed before
provider dispatch; no cross-route to Responses. Shared gateway/config changes
are M9-003 ownership; M9-001 changes no runtime schema or `CONTRACT.md`.

M8 does not own M9 files. M9 owns this document/cards and new
`internal/adapter/chatcompletions/**` plus `internal/connector/chatcompletions/**`.
Shared gateway/config (`cmd/gateway/main.go`, `cmd/gateway/yaml_config.go`,
`cmd/gateway/protected_startup.go`) is deferred to M9-003 and requires coordinated
single ownership before editing.

## M9-002 fixture and check contract

The first slice uses only deterministic local fake-upstream fixtures, no real
provider or credentials:

1. A JSON request/response with unknown nested fields and whitespace; assert
   request and response bytes are unchanged and protocol/model/stream metadata
   are exact.
2. A gated SSE with two Chat chunks and `[DONE]`; assert first bytes reach the
   client before releasing the second event, event order and raw bytes are
   unchanged, and cancellation closes upstream.
3. A tool-call delta split across chunks (index/id/name/argument fragments),
   final tool-call result and subsequent tool message submitted unchanged in a
   second round; include multiple tool indexes to exercise parallel deltas.
4. `include_usage` usage-only chunk before `[DONE]`; assert bytes preserved and
   final token accounting extracted once. Also verify missing usage remains
   unknown.
5. Malformed JSON/event, unterminated event, EOF without `[DONE]`, upstream
   pre-head error and post-head failure; assert appropriate failure/incomplete
   outcome, no fabricated terminal marker or success, and no retry after
   commit.
6. Client disconnect cancels the fake upstream; unsupported required
   capability/wrong-protocol route does not dispatch.

These are behavior requirements, not claims of implementation. The adapter
test command is `GOFLAGS=-p=1 go test -race -count=1 ./internal/adapter/chatcompletions`;
the native Connector and gateway regression commands are added to M9-002/003
when those packages and routes exist. Existing Responses regressions remain
required under the M9-001 card.
