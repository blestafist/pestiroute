# M7 Claude Code Connector Binding (offline candidate)

This document defines a deterministic implementation candidate under existing
[CONTRACT v1](CONTRACT.md), not evidence of compatibility, authorization, or
permission to use Claude subscription services. It is based on the source-only
[M7-006 research](../references/CLAUDE_CODE_CONNECTOR_RESEARCH.md#9router-claude-code-implementation-m7-006-source-only).
No live endpoint behavior is implied. The candidate stays non-routable and
default-gated until the external authorization and live-evidence gates in
[M7-005](tasks/M7-005.md) are separately satisfied. Synthetic tests prove only
the stated local mechanics.

## Component and profile

Implement as an API Connector under the existing component lifecycle and
execution contract. Descriptor reports a stable implementation identity,
implementation version, supported API versions, API connector type, and only
the `Authenticate` operation if implemented. It does not claim a provider
protocol, model, or capability based on its name. `Capabilities(scope)` reports
`unknown` for every capability, including `llm.streaming`, `llm.tools`,
`llm.tools.parallel`, `llm.reasoning`, `llm.structured_output`, `llm.vision`,
`auth.oauth`, and `usage.exact`; absent entries are also unknown. No
`supported` result is allowed on synthetic evidence.

Component configuration is parsed strictly during `Init`; unknown fields,
missing profile, and any profile other than the exact
`claude-code-messages-v1` value fail initialization. Profile is configuration,
not a Descriptor field or request-controlled selector. The profile accepts
only `openai.responses.v1` and declares explicit translation mode. There is no
native/pass-through mode, implicit fallback, model replacement, or
provider-specific Core behavior. Account selection and credentials come only
from scoped runtime services.

The candidate is not added to gateway composition and cannot be selected by
routes by default. Enabling routability is a separate reviewed decision after
the authorization and live evidence gates; changing configuration alone must
not promote capabilities or bypass this gate.

## Messages request binding

The candidate transport target derived from M7-006 is
`https://api.anthropic.com/v1/messages?beta=true`. Use POST with
`anthropic-version: 2023-06-01`, a configured `anthropic-beta` value, scoped
OAuth credential as `Authorization: Bearer <access token>`, JSON content/accept
headers, and the honest `User-Agent: PestiRoute`. Do not impersonate Claude
Code, copy Stainless/runtime identity headers, inject billing or identity
fields, cloak/rename tools, or synthesize account metadata. These values are
source-derived candidate mechanics only; the service's current acceptance and
authorization are unknown. Never log credentials or private payloads.

Translate the admitted Responses request body within the Connector to a
Messages body. Preserve system/developer instructions as ordered system text;
preserve input history order and roles when representing user and assistant
messages; preserve text and supported image content without flattening opaque
blocks; preserve tool declarations, names, descriptions, and JSON schemas;
map tool choice and generation controls only where a direct Messages semantic
equivalent exists. Do not silently drop unknown fields, tool results, or
unsupported response state. Reject requests whose semantics cannot be
represented losslessly with `unsupported_feature` before provider dispatch.
Do not put Responses-specific state (`previous_response_id`, background
execution, or other resource semantics) into Messages fields by guess. This
candidate does not claim complete Responses compatibility; exact field mapping
is limited to the explicitly supported fixture subset and must be versioned
with it. Output remains Responses-shaped; the Connector performs translation
once and the adapter must not translate it again.

## SSE and execution lifecycle

Read upstream SSE incrementally with bounded buffering and strict UTF-8,
`event:`/`data:` framing. Parse only the documented candidate Messages event
set: `message_start`, `content_block_start`, `content_block_delta`,
`content_block_stop`, `message_delta`, and `message_stop`. The native
`content_block_stop` event delimits completion of its indexed content block;
preserve its ordering when mapping block lifecycle events. A malformed frame,
invalid JSON, invalid event order/index, unsupported delta, or unknown event
fails the attempt; do not silently skip it or report successful completion.
EOF before `message_stop` is incomplete and must not produce `Complete`.
No success may be fabricated for an empty or truncated stream.

Map `message_start` to the Responses response head and initial metadata;
content-block start/delta/stop events to incremental output-item/content-part
events, including the native `content_block_stop` delimiter; `message_delta`
to final item/response metadata and stop reason; and `message_stop` to the
terminal Responses completion event. Preserve block indices and ordering.
Emit `Head` once, then each available `Body` frame as
soon as parsed, without waiting for upstream EOF; only after valid terminal
`message_stop` emit exactly one `Complete`, followed by EOF. The first output
`Body` must be deliverable before upstream completion. A valid terminal event
without body is still an orderly empty completion.

Request cancellation and client/consumer cancellation propagate through the
request context to both upstream request and response-body reads. On consumer
close, stop reading, cancel/close upstream IO, and release parser/buffer
resources. Bound retained bytes and channel/queue capacity independently of
stream length; block on slow consumers with context-aware sends rather than
buffering the full response. Parse failure, cancellation, partial close, and
upstream error terminate the stream and finalize the attempt once. No retry is
permitted after response commit; before commit, this Connector still performs
no hidden replay, and Core may retry only with explicit safe disposition under
the contract.

## Usage and completion

Build one final `Complete.UsageReport` from Messages usage fields: input tokens
from `message_start.message.usage.input_tokens` and output tokens from the
terminal `message_delta.usage.output_tokens`. Treat cache read/create counts as
subsets/details, not additive input totals; retain them only in namespaced
diagnostics if the binding can identify them without ambiguity. Do not infer
reasoning tokens, totals, or missing values. Missing/invalid/incomplete usage
is nullable/unknown, never zero. Mark source `provider` only for fields actually
reported by the upstream response and completeness according to the fields
present. `usage.exact` remains `unknown` irrespective of these mechanics.
Accounting is finalized once in `Complete`; stream failure before it is an
incomplete contract path, not successful zero usage.

## OAuth PKCE candidate binding

`Authenticate` uses only existing `start`, `continue`, and `refresh` actions.
The research observed 9Router source configured with authorize URL
`https://claude.ai/oauth/authorize`, token URL
`https://api.anthropic.com/v1/oauth/token`, client ID
`9d1c250a-e61b-44d9-88ed-5944d1962f5e`, scopes
`org:create_api_key user:profile user:inference`, `code=true`, and S256 PKCE.
These are source observations, not established registration, endpoint
acceptance, user consent, or authority to use them. Any implementation remains
disabled absent the separate authorization gate.

For an authorized offline state-machine candidate, `start` creates a
cryptographically random verifier, S256 challenge, and unpredictable state;
constructs an authorization URL with response type `code`, configured
redirect URI, scope, challenge/method, and state; and returns it only through a
safe `AuthUserAction` projection. This browser action has the HTTPS authorization
URL, empty `UserCode`, and zero `PollInterval`; do not invent a device code. The
URL necessarily carries the public challenge and state parameters needed by the
authorization protocol, but the verifier and credentials never enter
presentation. `AuthRequest.CallbackInput` provides the separate, transient
manual-paste path authorized by [DEC-019](../project/DECISIONS.md#dec-019--accept-manual-pasted-oauth-callback-input); `AuthRequest.State` remains only the runtime-loaded opaque Connector continuation state ([M3-AUTH](M3-AUTH.md#binding-the-existing-operation)).

Keep verifier, expected state, and exact redirect URI in encrypted opaque
Connector continuation state. On `continue`, the runtime supplies pasted
callback input separately, bounded to 1–4096 printable ASCII bytes and never
logged or persisted. The Connector alone parses it, rejects malformed or
ambiguous input. The protected configured redirect URI must be absolute, have a
non-empty authority, and have no query, fragment, or userinfo. Compare the
callback's raw scheme, raw authority, and escaped
path byte-for-byte with it, without normalization; reject userinfo, fragment,
malformed authority/escaping, and any mismatch. Strictly decode the query and
require exactly one non-empty, case-sensitive `code` and one non-empty,
case-sensitive `state`; reject duplicate keys, malformed encoding, OAuth error
parameters, and all unexpected parameters. Compare state after exactly one
strict query-decoding pass, byte-for-byte with protected expected state before
exchanging, and use the exact configured URI in the exchange. Fail closed
without reflecting or logging callback input, code, or provider error details.
The durable continuation claim makes each invoked attempt single-use; invalid or
ambiguous attempts consume it, and no automatic poll loop, retry, or replay is
allowed.

DEC-019 resolves the two contract expressiveness blockers and grants bounded
offline implementation authorization only for M7-016, M7-010, and M7-013. It
does not establish live interoperability, provider registration, entitlement,
or promote `auth.oauth` beyond `unknown`. The OAuth PKCE connector
implementation remains blocked on generic Go/admin/protobuf plumbing in
[M7-016](tasks/M7-016.md) before [M7-010](tasks/M7-010.md) can proceed. The
callback field, coordinator, admin input, and IPC implementation are not
implemented by this specification change. Any future auth flow must follow
[DEC-009](../project/DECISIONS.md#dec-009--separate-safe-user-actions-from-opaque-auth-state)
and [M3-AUTH](M3-AUTH.md): one explicit continuation per call, fixed runtime
session expiry, and conservative invalidation on cancellation/ambiguous
outcome. DEC-018 remains controlling: no live provider calls, login, real
credential handling/persistence, traffic capture, binary reverse engineering,
or production routing is authorized by this documentation change.

On a trusted successful exchange, return one opaque named credential bundle
containing the access token and refresh token; return absolute
`CredentialExpiresAt` only when derived from a trusted exchange expiry. Never
derive expiry by parsing a token. `refresh` uses only the selected account's
scoped refresh credentials, returns a complete replacement bundle, and must
not replay an ambiguous refresh. Provider refresh rotation, expiry semantics,
identity binding, and safe concurrency are unknown, so successful conformance
cannot be claimed from fixtures. Persist candidates only through Core's
acknowledged atomic credential path under
[DEC-010](../project/DECISIONS.md#dec-010--carry-credential-expiry-with-auth-results)
and [M3-AUTH](M3-AUTH.md); the Connector never stores credentials itself.

## Evidence boundary

Offline deterministic mechanics may be implemented and tested against
synthetic, versioned fixtures only. Such tests establish parsing, translation,
state transitions, cancellation, and accounting behavior for those fixtures;
they establish no endpoint interoperability, supported capability, service
authorization, terms applicability, or live subscription access. Live calls,
login, credential use, traffic capture, and binary reverse engineering remain
prohibited absent separately assigned and authorized work.

## References

- [M7-005 feasibility disposition](tasks/M7-005.md) and [M7-006 task evidence](tasks/M7-006.md).
- [M7-006 9Router source observations](../references/CLAUDE_CODE_CONNECTOR_RESEARCH.md#9router-claude-code-implementation-m7-006-source-only).
- [Connector contract](CONTRACT.md#connector-contract), [capabilities](CONTRACT.md#capability-model), [errors/retry](CONTRACT.md#error-model), [stream lifecycle](CONTRACT.md#execution-response-and-streamframe), and [usage](CONTRACT.md#usage-and-scoped-runtime-services).
- [Authentication contract](CONTRACT.md#connector-contract), [M3 auth binding](M3-AUTH.md), [DEC-009](../project/DECISIONS.md#dec-009--separate-safe-user-actions-from-opaque-auth-state), and [DEC-010](../project/DECISIONS.md#dec-010--carry-credential-expiry-with-auth-results).
