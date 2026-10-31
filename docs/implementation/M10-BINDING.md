# M10 Antigravity Candidate Text/Stream Binding

## Status and evidence ceiling

This is the normative design for the bounded M10 connector, not a claim of
Google-supported protocol compatibility. M10-010 authorizes implementation of an
explicitly opted-in HTTPS inference transport, but no live request or provider
test is part of automated acceptance. Endpoint, envelope and event details below
remain unofficial candidate observations; supported wire facts remain **Unknown**
as recorded in [M10 research](M10-RESEARCH.md). No remote/provider capability is promoted.
Third-party OpenCode source observations are MIT-licensed research hypotheses;
any source-derived implementation must retain applicable notices and must not
reuse plugin OAuth client credentials. M10-004's fake-only boundary covered its
initial vertical slice; M10-010 adds opt-in runtime transport while retaining
deterministic fake-only acceptance tests subject to M10-003 fixture provenance.

## Component and admitted execution

| Property | Binding |
| --- | --- |
| Component | Private `antigravity.Connector`, implementing the existing `core.Connector` contract; stable descriptor ID `pestiroute.antigravity.v1internal` is a proposed implementation identity, not a provider identity. |
| Profile | `antigravity-v1internal`, selected only by trusted component configuration. It names this candidate translation, not a supported Google API. |
| Client protocol | Exactly `openai.responses.v1`; translation mode only. Native mode, other protocol versions, and implicit protocol inference are rejected. |
| Work in scope | Text prompts or supported text/function-call history, candidate function declarations/tool-choice mapping, and an incrementally translated Responses stream of text and candidate function-call outputs. History translation is stateless per Execute; thought-signature-dependent continuation remains unsupported. One Execute is one attempt for the runtime-selected account. |
| Capabilities | `llm.streaming` is `supported` only for `openai.responses.v1` translation, meaning this exact implementation scope is available and deterministically verified. It does not claim remote entitlement or provider support. `auth.oauth`, `usage.exact`, and all other capabilities remain `unknown`. |

The existing `core.Connector.Execute(context, request, attemptScope,
invocationServices)` is sufficient. Connector-private translation and streaming
parsing do not require Core to know Antigravity, HTTP/SSE, prompt fields, or
provider events. No contract ADR is needed for this binding. A future need to
preserve tool/signature replay state across executions would require separate
contract and compatibility review before implementation.

## Request admission and candidate translation

The admitted `RawPayload` remains immutable. The Connector requires the exact
protocol above, translation mode, a live invocation context, and a bounded valid
JSON object. Accept `model` (allowlisted string), `input` (non-empty string or supported Responses item array),
`stream: true` (required), and only the function declarations/tool-choice forms
described in [M10-TOOL-BINDING](M10-TOOL-BINDING.md#candidate-translation). Reject malformed JSON, duplicate keys, trailing
values, unknown top-level fields, invalid field types, false/missing stream, and
unsupported semantics before transport dispatch. Do not silently drop fields.
The adapter owns client-protocol decoding; the Connector-owned private parser
validates the supported subset and does not introduce a shared message schema.

Reject unsupported tool options, images, audio, video, other multimodal input,
structured-output constraints, unsupported reasoning/state fields, and
unrecognized extension fields as `unsupported_feature` before dispatch.
Supported message, function-call and function-call-output history is translated
by M10-016 with per-request causal ID/name matching; orphan, duplicate,
interleaved, malformed, or dangling calls fail before dispatch. Function results
retain their JSON value in the candidate response wrapper. Candidate function-call responses are translated by M10-015 as
complete `name`/object-`args`/optional-`id` parts; unsupported shapes fail rather
than being silently dropped. This is an offline candidate based on pinned plugin
source, not a provider guarantee. Thought signatures remain unsupported/Unknown:
do not synthesize, discard-and-continue, or claim continuity. Model identifiers are accepted only
from an explicit allowlist; no model rewriting, guessed aliases, or
provider-model claim is implied.

The private backend request is a candidate-only translation based on pinned MIT
plugin observations: Rahularya01 `opencode-antigravity` revision
`874ef4d6cc44f852401e6298790c6b34804e2978` (`src/client/client.ts`,
`src/stream/transform.ts`, `src/utils/util.ts`, `src/types/enums.ts`) and
decolua/9router revision `87a74e1bda3677193c6f71a7e2100c90311f0689`
(`open-sse/providers/shared.js:95-97`,
`open-sse/providers/registry/antigravity.js:27`,
`open-sse/executors/antigravity.js:153-159,331-344`). Use outer keys `project`,
`model`, `request`, `userAgent`, and `requestId`; `request` contains `contents`
and admitted `tools`. Do not send `requestType: agent`. Generate a fresh request
ID per invocation; its format is not verified. Map the text prompt to one
user-role content with one text part and bind the project from the selected
stored OAuth credential's `project_id`, never from connector configuration or
generated fallback. Existing configured `project` is accepted only as an
optional, deprecated compatibility field and has no inference effect. Missing or
invalid stored project fails before HTTP dispatch. Do not include
system-instruction, session, labels, or multimodal fields. The default endpoint
candidate is `https://daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse`;
the explicit inference identity is `User-Agent: antigravity/ide/2.22.0 darwin/arm64`
and no `X-Request-Source` header. Keep
`/v1internal/projects/{project}:generateContent?alt=sse` only for the
`antigravity.synthetic.invalid` fixture destination. Project semantics,
endpoint/action compatibility, headers, envelope acceptance, and provider wire
support remain **Unknown**. Do not copy source expression or credentials.

## Project and credential scope

Project identity for Execute comes only from the selected stored OAuth credential:
a single non-empty, strictly validated `project_id` is bound to the selected
account. Missing or invalid credential metadata fails before dispatch; there is
no configuration fallback or generated project. The prior connector `project`
setting remains optional and accepted for configuration compatibility, but is
ignored for inference. Discovery metadata never changes the selected account.
The offline auth candidate may make one bounded `loadCodeAssist` discovery request after code
exchange against the same validated configured inference origin used by this
connector (including its trusted endpoint override) and retain a valid returned
project ID as credential metadata. This third-party-derived behavior is not an
official API claim; failure or a missing ID fails authentication closed. No
`onboardUser`, retry/fallback endpoint,
managed-project default, project substitution, or account fallback is permitted.
Project and endpoint semantics remain Unknown.

At Execute, obtain only the runtime-scoped `oauth` credential bundle for the
selected account. Strictly decode a JSON object containing a non-empty
`access_token`, optional non-empty `refresh_token`, and optional validated
`project_id` metadata; reject duplicate/malformed/trailing or unknown fields.
`project_id` is required for inference even though refresh/auth parsing may admit
its absence. Project only `access_token` into the candidate request's
authorization header. Never use refresh material, credentials from another
account, ambient identity, or a plugin-owned OAuth client for Execute.
Credential acquisition/refresh is connector-owned and remains an offline
candidate; absent credentials fail before dispatch as `unauthenticated`.

## Transport opt-in and stream lifecycle

`Execute` uses the connector-owned, redirect-disabled runtime HTTP client only
when `services.Transport` is the exact connector-provided doer and
`PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT=1`; an arbitrary `HTTPDoer` is never an
opt-in seam. Otherwise it accepts only the existing deterministic offline-test
transport and rejects ambient transport before dispatch. The endpoint is an HTTPS origin, defaulting to the candidate host above
and optionally configured per connector; the trusted
`PESTIROUTE_ANTIGRAVITY_ENDPOINT` environment override takes precedence. The
gateway-created client refuses redirects.
Opt-in permits an operator-controlled inference request; it does not authorize
or imply live testing, Google support, account eligibility, or capability
promotion. Automated checks use synthetic `.invalid` endpoints and make zero
external network/provider calls.

The research observed candidate SSE frames with `data:` JSON containing a
`response` object and candidate text in `candidates[].content.parts`. This is a
test hypothesis, not a server guarantee. The implemented terminal evidence is
either synthetic `response.done: true`, or candidate `finishReason: STOP`,
optionally followed by one usage-only frame, then clean EOF. After `STOP`, reject
further candidate content and conflicting finish markers; after the optional
usage frame, reject all further events. Usage metadata alone is never terminal
evidence. Truncated or partial SSE framing fails, including after `STOP`. Parts
marked `thought: true` with non-empty valid UTF-8 text and no
`thoughtSignature` are translated incrementally into a Responses `reasoning`
item with a `summary_text` part. The item occupies the next output index in
actual candidate/part encounter order, including when ordinary text appeared
first. Its text/part/item done events are emitted exactly once before a later
message or function call and at terminal finish; item IDs are unique within the
response and are not reused. The complete provider frame is validated before
any events or state changes from that frame are committed. Any present
`thoughtSignature` is rejected atomically, including explicit JSON `null`;
signature-bearing reasoning and inbound reasoning history/tool continuity
remain unsupported. Google's public [Gemini thinking documentation](https://ai.google.dev/gemini-api/docs/thinking)
describes public thought summaries and signatures, but does not establish
Antigravity support. See [DEC-028](../project/DECISIONS.md#dec-028--translate-unsigned-antigravity-thought-summaries-incrementally)
for pinned 9router implementation references.
An unsigned part with present empty `text` is a no-op, including an empty
`thought: true` part: it creates no output item or delta and adds no retained
bytes. Empty parts may occur before, between, or after real parts without
changing order, usage extraction, incremental delivery, or bounded retention.
Any present `thoughtSignature` (including JSON `null`) and any text plus
`functionCall` combination remain atomic fail-closed errors, including the
empty-text case. A terminal stream with no actual output fails closed; it never
fabricates a successful empty completion. These narrow source-derived semantics
are recorded in [DEC-029](../project/DECISIONS.md#dec-029--ignore-unsigned-empty-antigravity-text-parts); the provider cause of M10-028 remains unknown.
Invalid-content debug logs retain the `precommit_invalid_content` wire category
and may include only fixed subkinds `done_with_candidates`,
`text_with_function_call`, or `invalid_utf8_text`.
For
`gemini-2.5-flash` only, the candidate request envelope sets
`generationConfig.thinkingConfig.thinkingBudget` to `0` and
`includeThoughts` to `false`; `gemini-2.5-pro` and other models remain unchanged.
This is an offline, source-derived compatibility default, not a Google-supported
Antigravity guarantee. Candidate function calls are complete part values, not inferred
argument-growth fragments: each JSON object is streamed through the Responses
added/delta/done item lifecycle, and final output retains text, unsigned
reasoning summaries, and calls in candidate/part order. Retained text,
arguments, and summaries share the existing 1 MiB bound. The
Connector parses incrementally with bounded line/event and retained-state
buffers; it forwards translated client-protocol bytes as soon as
validated content is available and must not wait for whole-response EOF. It must
not expose provider SSE bytes as Responses output or require the Protocol Adapter
to translate them again.

Return an `ExecutionResponse` with exactly `Head → Body* → Complete → EOF`.
`Head` identifies `openai.responses.v1` and the correct client content type;
each non-empty `Body` contains already translated Responses stream bytes.
`Complete(succeeded)` follows only the terminal evidence above and carries final
usage only when known. Missing usage remains nil/unknown, never zero by
assumption. No frames follow `Complete` and no EOF-before-Complete success is
allowed. Do not aggregate the full response or merge protocol bytes with
accounting/control metadata.

Propagate the runtime context to the transport and parser. Cancellation before
dispatch prevents `Do`; after dispatch cancel/close the response body and release
parser resources promptly. Closing the consumer stream is idempotent and stops
upstream work. A cancellation or stream failure after Head ends as
`Complete(cancelled|incomplete)` when orderly termination remains possible;
otherwise Core finalizes the attempt using available partial/unknown usage.
Never convert a truncated/malformed stream into success. Core/Adapter own commit
and exactly-once finalization; the Connector does not retry.

## Error, HTTP, and retry binding

Retryable indicates potential transience; it does not establish safe replay. The
Connector must set retry disposition explicitly and must never internally replay.

| Observation | Result | Retryable / disposition |
| --- | --- | --- |
| Invalid protocol/mode/body, unsupported input, invalid project or credential | Pre-dispatch `invalid_request`, `unsupported_feature`, or `unauthenticated` as appropriate | `false / unsafe` |
| Missing scoped transport or real-transport opt-in unavailable | Pre-dispatch `unavailable` | `false / unknown` |
| Transport `Do` error, nil response/body, or invalid status after invocation starts | Fail; close any available body. Delivery may be ambiguous. | `false / unknown` |
| Redirect response | Reject and close; transport must not follow redirects | `false / unknown` |
| 401 | `unauthenticated`; bounded error-body parse then close | `false / unsafe` |
| 403 | `permission_denied`; bounded error-body parse then close | `false / unsafe` |
| 429 | `rate_limited`; bounded error-body parse then close; parse Retry-After only via existing Connector rules | `true / unknown` |
| Other non-2xx | Bounded error-body parse then close; 5xx may be transient, other statuses are not assumed transient | 5xx `true / unknown`; otherwise `false / unknown` |
| 2xx with invalid content type/encoding or malformed initial stream data | Reject before Head, close response | `false / unknown` |
| Malformed/truncated/read-failed stream after Head | Incomplete/failed terminal outcome if possible; no replacement response | `false / unknown` |

Error bodies are read at most 8192 bytes and never logged, returned, or retained.
Only the fixed upstream status enums `UNAUTHENTICATED`, `PERMISSION_DENIED`,
`RESOURCE_EXHAUSTED`, and `INVALID_ARGUMENT` affect classification; malformed,
duplicate-key, unknown, or absent values fall back to HTTP status. Public errors
contain only fixed gateway codes/messages, never upstream text or tokens. Every
non-nil response body is closed on all paths. Status alone never marks replay safe. Core may fallback only before
commit when `Retryable=true`, disposition is `safe`, and policy/deadline permit
it. This binding classifies ambiguous post-`Do` failures as unknown/unsafe for
fallback. Once the adapter receives Head and commits the response, no fallback
or replacement response is permitted regardless of later status, usage, or
transport failure. The synthetic fixtures must test pre-dispatch rejection,
first-event delivery before completion, cancellation, bounded reads, malformed
EOF, ambiguous `Do` failure, and no replay after commit.

## References

- [M10 brief](M10-ANTIGRAVITY.md), [M10 research](M10-RESEARCH.md)
- [Internal contract: execution stream](CONTRACT.md#execution-response-and-streamframe), [Connector](CONTRACT.md#connector-contract), [errors](CONTRACT.md#error-model), [scoped services and usage](CONTRACT.md#usage-and-scoped-runtime-services)
- [M2 language binding](M2-BINDING.md), [M8 lifecycle/error binding pattern](M8-BINDING.md)
