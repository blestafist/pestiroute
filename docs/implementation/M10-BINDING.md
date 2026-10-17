# M10 Antigravity Offline Text/Stream Binding

## Status and evidence ceiling

This is the normative design for the bounded M10 offline candidate, not a claim
of Google-supported protocol compatibility. It authorizes no live request, login,
credential acquisition, production endpoint, or capability promotion. Current
wire facts remain **Unknown** as recorded in [M10 research](M10-RESEARCH.md).
Third-party OpenCode source observations are MIT-licensed research hypotheses;
any source-derived implementation must retain applicable notices and must not
reuse plugin OAuth client credentials. The subsequent slice is deterministic
fake-only and remains subject to M10-003 fixture provenance and review.

## Component and admitted execution

| Property | Binding |
| --- | --- |
| Component | Private `antigravity.Connector`, implementing the existing `core.Connector` contract; stable descriptor ID `pestiroute.antigravity.v1internal` is a proposed implementation identity, not a provider identity. |
| Profile | `antigravity-v1internal`, selected only by trusted component configuration. It names this candidate translation, not a supported Google API. |
| Client protocol | Exactly `openai.responses.v1`; translation mode only. Native mode, other protocol versions, and implicit protocol inference are rejected. |
| Work in scope | One text-only prompt producing an incrementally translated Responses stream through a deterministic fake transport. One Execute is one attempt for the runtime-selected account. |
| Capabilities | `llm.streaming`, `auth.oauth`, `usage.exact`, and all other capabilities remain `unknown` absent separately accepted evidence. A synthetic fixture cannot establish live eligibility. |

The existing `core.Connector.Execute(context, request, attemptScope,
invocationServices)` is sufficient. Connector-private translation and streaming
parsing do not require Core to know Antigravity, HTTP/SSE, prompt fields, or
provider events. No contract ADR is needed for this binding. A future need to
preserve tool/signature replay state across executions would require separate
contract and compatibility review before implementation.

## Request admission and candidate translation

The admitted `RawPayload` remains immutable. The Connector requires the exact
protocol above, translation mode, a live invocation context, and a bounded valid
JSON object. For this candidate accept only `model` (non-empty fixture-allowlisted
string), `input` (non-empty string), and `stream: true` (required). Reject malformed JSON, duplicate keys, trailing
values, unknown top-level fields, invalid field types, false/missing stream, and
unsupported semantics before transport dispatch. Do not silently drop fields.
The adapter owns client-protocol decoding; the Connector-owned private parser
validates the supported subset and does not introduce a shared message schema.

Reject tools/tool choice, tool history, images, audio, video, other multimodal
input, structured-output constraints, unsupported reasoning/state fields, and
unrecognized extension fields as `unsupported_feature` before dispatch. Tool
calls and thought signatures are unsupported/Unknown: do not synthesize,
discard-and-continue, or claim continuity. Model identifiers are accepted only
from an explicit fixture allowlist; no model rewriting, guessed aliases, or
provider-model claim is implied.

The private backend request is a candidate-only translation based on the pinned
MIT plugin observations. Its observed outer candidate shape is `project`,
`model`, `request`, `requestType`, `userAgent`, and `requestId`; `request`
contains the candidate Gemini-style `contents` text prompt. For this one-turn
slice, map the accepted prompt to one user-role content with one text part, and
use configured project plus the fixture-allowlisted model. Do not include
system-instruction, tool, session, label, or multimodal fields. Exact
`requestType`, user-agent/request-ID values, generation options, action path,
headers, and backend wire compatibility are unverified and must be explicitly
settled as synthetic fixture values in M10-003; none may be presented as
official API facts. Do not copy source expression or credentials from the
audited plugins.

## Project and credential scope

Project identity is mandatory trusted per-instance configuration for the offline
candidate: a single non-empty, strictly validated identifier is supplied
explicitly and bound to the selected connector/account. Missing or invalid
configuration fails initialization or the pre-dispatch validation path; it is
never inferred from credentials, environment, user identity, discovery, or a
request. No `loadCodeAssist`, project lookup, auto-provisioning/onboarding,
managed-project default, project substitution, or silent fallback is permitted.
The required project identifier's real-provider semantics remain Unknown.

At Execute, obtain only the runtime-scoped `oauth` credential bundle for the
selected account. Strictly decode one JSON object containing a non-empty
`access_token`; reject empty/malformed/trailing/unknown-field values. Project
only that access token into the candidate request's authorization header. Never
use refresh material, credentials from another account, ambient identity, or a
plugin-owned OAuth client. Credential acquisition/refresh is not implemented or
authorized by this task; absent credentials fail before dispatch as
`unauthenticated`.

## Offline transport and stream lifecycle

The implementation candidate must use only an injected deterministic fake
transport. No production host, configurable external endpoint, redirect, or
ambient HTTP client is permitted. If the implementation cannot enforce that
fake-only boundary before dispatch, it must fail closed. No provider call is an
acceptance check for this task.

The research observed candidate SSE frames with `data:` JSON containing a
`response` object and candidate text in `candidates[].content.parts`. This is a
test hypothesis, not a server guarantee. M10-003 fixtures must settle the
synthetic exact framing and terminal condition. The Connector parses
incrementally with bounded line/event and retained-state buffers; it forwards
translated client-protocol bytes as soon as validated content is available and
must not wait for whole-response EOF. It must not expose provider SSE bytes as
Responses output or require the Protocol Adapter to translate them again.

Return an `ExecutionResponse` with exactly `Head → Body* → Complete → EOF`.
`Head` identifies `openai.responses.v1` and the correct client content type;
each non-empty `Body` contains already translated Responses stream bytes.
`Complete(succeeded)` follows only a valid synthetic terminal event and carries
final usage only when known. Missing usage remains nil/unknown, never zero by
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
| Missing scoped transport or fake-only transport unavailable | Pre-dispatch `unavailable` | `false / unknown` |
| Transport `Do` error, nil response/body, or invalid status after invocation starts | Fail; close any available body. Delivery may be ambiguous. | `false / unknown` |
| Redirect response | Reject and close; transport must not follow redirects | `false / unknown` |
| 401/403 | `unauthenticated`; bounded error-body drain then close | `false / unsafe` |
| 429 | `rate_limited`; bounded drain/close; parse Retry-After only via existing Connector rules | `true / unknown` |
| Other non-2xx | Bounded drain/close; 5xx may be transient, other statuses are not assumed transient | 5xx `true / unknown`; otherwise `false / unknown` |
| 2xx with invalid content type/encoding or malformed initial stream data | Reject before Head, close response | `false / unknown` |
| Malformed/truncated/read-failed stream after Head | Incomplete/failed terminal outcome if possible; no replacement response | `false / unknown` |

Every error-body read is bounded and every non-nil response body is closed on
all paths. Status alone never marks replay safe. Core may fallback only before
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
