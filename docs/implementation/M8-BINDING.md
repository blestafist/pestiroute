# M8 Claude Code Execution Binding

## Current implementation notice

M8-001–007 implemented the offline candidate described below. The user accepted
its integration in [DEC-020](../project/DECISIONS.md#dec-020--accept-m8-offline-integration-and-defer-live-enablement).
The authoritative current capability/evidence ceiling is [M8-HANDOFF](M8-HANDOFF.md).
The design audit below is retained as historical M8-001 evidence: its “proposed”,
“not implemented” and readiness wording describes that audit revision, not the
current code. It must not be used to infer live readiness. Execute/auth remain
synthetic, YAML remains rejected, capabilities remain Unknown, and production
transport stays disabled. Live binding and enablement belong to M8-008/009.

## Historical M8-001 design audit

## Current binding and evidence ceiling

| Concern | Current code / contract | Boundary for M8 |
| --- | --- | --- |
| Component | `claudecode.Connector` implements `core.Connector`; descriptor declares only `authenticate`. Init strictly accepts profile `claude-code-messages-v1`; OAuth endpoints are restricted to HTTPS `.invalid` hosts. | No operation, profile, mode, or provider selection from request data. `execute`, `models`, and `estimate_usage` remain undeclared. |
| Protocol | Only `openai.responses.v1` and translation mode are admitted by the private translator. It rejects unknown top-level fields, malformed/duplicate JSON, unsupported history/state, and semantics without direct mapping. | Responses remains the client protocol; no universal message API, native mode, silent field drop, model rewrite, or adapter-side repeat translation. |
| Request subset | Private `translateRequest` currently constructs a streaming Messages candidate for text, supported data-URL images, function declarations/choice, max output tokens, temperature, and instructions. It emits an Authorization bearer header and fixed Messages URL, but does not dispatch. | Candidate behavior only. Exact public subset must be fixture-pinned; unsupported inputs fail before transport. No passthrough of client headers or caller-controlled endpoint. |
| Credential bundle | Auth code stores the named `oauth` JSON bundle `{access_token, refresh_token, expires_at}`. Refresh parses it as `oauthCredentials`; it requires a refresh token but does not reject an empty access token on read. | Proposed Execute projection: `Get("oauth")` only, decode the bundle strictly, require a non-empty `access_token`, and copy only that token to the request. Missing access, empty bytes/token, malformed/trailing/unknown-field JSON, or unavailable credentials fail pre-dispatch as unauthenticated (`Retryable=false`, `RetryDisposition=unsafe`). Never use refresh token or expiry as an access-token substitute. This is an M8 Execute rule, not a current public path. |
| Beta header | `translateRequest` receives beta as an argument; it requires non-empty and rejects CR/LF, then emits `anthropic-beta`. Tests pass `synthetic-beta`. `componentConfig` has no beta field and no runtime source supplies one. `M7-BINDING` says “configured” but does not bind a concrete source/value. | Explicit unresolved gap: no beta value may be invented or pulled from request data. Before dispatch is considered, settle the authoritative offline fixture value/source and validation (at minimum one non-empty safe header value, no CR/LF, no controls); until then missing beta is pre-dispatch unsupported configuration, not a guessed default. |
| Endpoint | Private translator currently targets the production-looking `https://api.anthropic.com/v1/messages?beta=true`; it does not dispatch. | For any separately authorized offline-only Execute, replace this with a fixed `https://api.anthropic.invalid/v1/messages?beta=true` constant: no endpoint config/override and no request-derived host. The reserved synthetic target prevents an ordinary direct HTTP transport from reaching the provider. Redirects must be disabled before `Do`; the generic `HTTPDoer` contract cannot enforce or inspect its internal redirect/proxy policy. If that cannot be guaranteed by the runtime transport, public dispatch remains blocked—post-response redirect detection is too late. This does not alter M7's source-derived production endpoint claim. |
| HTTP/account | Runtime invocation receives selected-account `Credentials`, scoped `Transport`, and logger. Auth token exchanges use scoped transport and read at most `authBodyLimit` (64 KiB). | Use only per-invocation services; no ambient client, account fallback, secret logging, transport retention, or Connector secret persistence. |
| Stream | `messagesStream` parses bounded SSE incrementally, recognizes a finite event set, checks ordering/indexes, emits Responses frames incrementally, requires `message_stop`, and bounds retained projection to 1 MiB. `Close` closes the owned reader; `Next` propagates context cancellation. | Preserve `Head → Body* → exactly one Complete → EOF`; no full-response buffering. Malformed/unknown/truncated frames fail, partial usage stays unknown/partial, cancellation closes upstream IO, and consumer close releases it. Validate observable behavior through public Execute before claiming it. |
| Failure/retry | Core contract requires explicit safe retry disposition and no commit; ambiguous delivery is not safe. Current public `Execute` always fails closed with `connector_unavailable`, `RetryUnknown`. | Pre-dispatch validation errors are non-retryable; absent credential/transport is unavailable before dispatch. After `Do` begins, transport errors are ambiguous/unknown (never safe by status alone). No Connector retry or replay; no fallback after commit. |
| Usage | SSE candidate extracts input tokens at `message_start` and output tokens at `message_delta`; missing values remain nil. `usage.exact` and all declared capabilities remain `Unknown`. | Final usage only from valid terminal completion, nullable and conservative on interruption. Synthetic fixtures do not promote `llm.streaming`, tools, reasoning, OAuth, or exact usage. |
| Composition | Protected startup registers `pestiroute.claudecode.messages`; default composition has no runtime transport. YAML validation still rejects this implementation as unsupported, and no production YAML route is enabled. M7-013 verifies the YAML fail-closed state. | Protected fixture composition is implemented; keep YAML default-rejected. A config value alone must never make it routable. Live enablement remains separate reviewed work. |

### Proposed HTTP result/error matrix (not implemented or accepted)

This applies only to a future offline fake transport. It follows CONTRACT's
distinction between transience and replay safety; **no status alone is safe to
replay**. All `Do` errors after invocation begins are conservatively ambiguous.

| Observation | Proposed outcome | Retryable / disposition |
| --- | --- | --- |
| Invalid protocol/mode/payload, missing or malformed credential/beta | Fail before `Do`; invalid/unsupported or unauthenticated | `false / unsafe` |
| Missing transport | Fail before `Do`; unavailable | `false / unknown` |
| `Do` error, nil response, nil body, or invalid status | Fail; close any body that exists | `false / unknown` |
| 3xx | Do not follow; fail unavailable | `false / unknown`; transport must disable automatic redirects before dispatch |
| 401/403 | Unauthenticated; drain at most 64 KiB then close | `false / unsafe` |
| 429 | Rate limited; bounded error-body read/close; parse Retry-After only under existing Connector rules | `true / unknown` |
| Other non-2xx (including 5xx) | Unavailable; bounded error-body read/close | `true / unknown` for 5xx; otherwise `false / unknown`; no status grants safe replay |
| 2xx with wrong media type, unsupported charset/encoding, or invalid SSE start | Reject before forwarding body; close body | `false / unknown` |
| Valid SSE later malformed, read-failed, or EOF before `message_stop` | After Head, fail/incomplete Complete once, close body; never success | `false / unknown` |

Proposed success validation is `Content-Type: text/event-stream` with absent or
UTF-8 charset, and absent/`identity` `Content-Encoding`; other encodings are
rejected unless a reviewed transport binding proves normalized decompression.
Error bodies are never exposed/logged and are read through a 64 KiB limit (reuse
of `authBodyLimit` is a candidate, not a current Execute guarantee). Close every
non-nil body on every path; close the successful stream body on terminal,
failure, cancellation, or consumer close. Current Execute has no HTTP/status,
media-type, encoding, or Execute credential-projection behavior; M8-002 must
test these cases if later authorized.

The code paths are `internal/connector/claudecode/{component,auth,translate,sse}.go`,
`internal/core/{connector,execution}.go`, and gateway config validation. The
normative lifecycle/error rules are in the [Connector contract](CONTRACT.md#connector-contract),
[stream lifecycle](CONTRACT.md#execution-response-and-streamframe),
[error/retry model](CONTRACT.md#error-model), and
[scoped services/usage](CONTRACT.md#usage-and-scoped-runtime-services). See
[M7-BINDING](M7-BINDING.md) for the upstream-source provenance/evidence limits.

## Candidate public-execution flow (not authorized yet)

1. Core selects a connector/account and invokes `Execute` with the admitted
   Responses payload, attempt scope, and invocation-scoped services.
2. Connector rejects unready state, wrong protocol/mode, canceled context,
   missing scoped credentials/transport, or unsupported request semantics before
   dispatch; it obtains only the selected account's credential and translates
   once.
3. If separately approved, Connector builds the fixed `.invalid` Messages
   request and calls only the invocation's transport after obtaining a strictly
   validated `oauth.access_token` and an approved beta value. Request context
   owns cancellation/deadline; no hidden retry or redirect. Before `Do`,
   dispatch is known not to have begun. Once `Do` begins, a transport error is
   ambiguous unless the transport can prove non-delivery. The generic service
   interface's redirect/proxy guarantees are not established; implementation
   readiness requires an enforceable no-external-dispatch transport boundary.
4. For a valid upstream response, Connector owns/closes its body and exposes a
   bounded incremental stream. It validates every frame and event order, emits
   available body frames without waiting for EOF, and yields a terminal
   completion only after valid `message_stop`. Read failure, malformed frames,
   cancellation, partial close, or missing terminal event cannot become success.
5. Core/adapters retain contract ownership of commit, fallback eligibility and
   exactly-once attempt accounting. No retry follows commit; ambiguous delivery
   never becomes safe merely because an HTTP status looks transient.

This flow is not public behavior today: Execute remains fail-closed, and current
private translation/SSE tests do not prove a public transport or lifecycle path.

## Explicit semantic ceiling

The current candidate has no modeled `previous_response_id`, background/resource
state, tool-result history, parallel-call guarantees, or lossless reasoning
semantics. Output-side `thinking` parsing is not evidence of supported reasoning.
Only the tested mapping subset could be considered for an offline fake execution
slice. Capabilities stay Unknown; no account/model scope is eligible for required
capabilities on synthetic evidence. No live entitlement, endpoint acceptance,
OAuth registration, source-use permission, or compatibility is established.

## Decision table

| Proposed behavior | M7 baseline / authority | M8 decision |
| --- | --- | --- |
| Public `Execute` sends to a controllable synthetic fake and returns candidate stream | Execute intentionally returns unavailable; DEC-018/019 authorize only their enumerated M7 cards, and prohibit provider calls / production routing | Requires explicit additional accountable offline-only authorization before M8-002. No real upstream; fake transport only, non-routable. |
| Use runtime-scoped credentials and HTTP service | Existing Connector contract and M7 implementation | Required if separately authorized; no ambient client, arbitrary endpoint, account fallback, or Connector-owned secret storage. |
| Add retry, fallback, shared Core behavior, auth plumbing, endpoint configuration, composition or route | Existing contract and parallel M9 ownership; no M8 card authorizes these changes | Out of scope. Coordinate before any shared gateway/Core/spec edits; separate cards/decision required. |
| Promote capabilities / claim Claude compatibility or enable live traffic | M7 evidence is synthetic; DEC-018/019 keep capabilities Unknown and routing disabled | Prohibited absent distinct live authority, evidence, review, compatibility matrix and release decision. |

## Required decision before implementation readiness

DEC-018 granted bounded offline work only to M7-009–013; DEC-019 extended that
only to M7-016, M7-010 and M7-013. Neither authorizes M8 public execution.
M8-002 must remain DRAFT (or BLOCKED if assigned before resolution) until an
accountable decision explicitly grants the minimum offline scope and preserves
all live/credential/capability/routing prohibitions. M8-003 additionally needs
its bounded lifecycle scope and checks settled. An accepted decision should be
recorded in `DECISIONS.md` and synchronized with this binding, the M8 plan and
task cards before readiness; this document does not assert that approval.

### ADR proposal for accountable review — not an accepted decision

- **Context:** Existing public Execute is fail-closed and prior offline
  authorization is restricted to named M7 tasks. Existing private translation
  and SSE mechanics have synthetic tests but no public execution/lifecycle proof.
- **Proposed decision:** If the accountable user chooses to extend scope,
  authorize only M8-002/M8-003 offline execution against deterministic fake
  transport, with synthetic credentials, supported fixture inputs, and a fixed
  `.invalid` endpoint; keep connector uncomposed, gateway rejection in force,
  all capabilities Unknown, and no production/live claims. Require an enforced
  no-redirect/no-external-dispatch boundary before `Do`, not merely a fake in
  tests. The generic `HTTPDoer` contract currently does not prove that boundary;
  if it cannot be enforced without a separately authorized contract/runtime
  change, this proposed execution scope must not proceed.
- **Alternatives:** Keep Execute fail-closed and stop after design; or seek live
  authorization now. The former avoids expanding authority; the latter is not
  supported by current evidence and is outside this proposal.
- **Compatibility/impact:** No CONTRACT semantic change is proposed. Public
  Connector behavior would change from unconditional fail-closed to bounded
  fixture execution only if the offline scope is accepted; production composition
  and routing remain unchanged. Any contract deviation discovered during
  implementation requires its own accepted ADR and synchronized specifications.
- **Verification if accepted:** offline fake-only public Execute and lifecycle
  race tests; prove fixed `.invalid` target, no redirect/external dispatch,
  approved beta/credential validation, supported-input rejection before
  dispatch, bounded error handling/body closure, incremental delivery,
  cancellation/close, malformed/truncated stream failure, one terminal
  accounting result, default gateway rejection, and the repository checks in
  the implementation cards. No real-provider checks.

### Unresolved technical gates (block M8-002 readiness)

- `HTTPDoer` is only `Do(*http.Request)`; its API cannot require a no-redirect
  policy or establish whether a proxy can route a synthetic hostname externally.
  The fixed `.invalid` destination blocks ordinary direct resolution but does
  not prove arbitrary configured transport behavior. Keep Execute unimplemented
  until an enforceable fake-only boundary is available; any shared contract/Core
  change needs separate ownership and authorization.
- `anthropic-beta` has no configured source or concrete mandatory value in the
  current component. M7's “configured” wording is not implementation evidence;
  existing translation tests use a synthetic value. Accountable/source evidence
  must settle a safe offline fixture value and validation before M8-002 is READY.

## References

- [M7 feasibility disposition](tasks/M7-005.md), [M7 conformance audit](tasks/M7-013.md),
  [M7 OAuth/translation binding](M7-BINDING.md), [M3 auth binding](M3-AUTH.md).
- [M8 plan](M8-CLAUDE-INTEGRATION.md), [DEC-018](../project/DECISIONS.md#dec-018--authorize-bounded-offline-m7-candidate-work),
  [DEC-019](../project/DECISIONS.md#dec-019--accept-manual-pasted-oauth-callback-input).
