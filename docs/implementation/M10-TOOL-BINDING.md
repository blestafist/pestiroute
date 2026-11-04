# M10 Antigravity Tool/History Binding

## Status and evidence ceiling

This is an offline candidate binding based on two pinned MIT plugin sources, not
a Google API specification or evidence of live behavior. The audit used Rahularya01
`opencode-antigravity` revision
[`874ef4d6cc44f852401e6298790c6b34804e2978`](https://github.com/Rahularya01/opencode-antigravity/tree/874ef4d6cc44f852401e6298790c6b34804e2978)
(`src/stream/transform.ts`, `src/stream/stream.ts`) and NoeFabris
`opencode-antigravity-auth` revision
[`16e0056431d0a1291ee66e5938c732720b13a851`](https://github.com/NoeFabris/opencode-antigravity-auth/tree/16e0056431d0a1291ee66e5938c732720b13a851)
(`src/plugin/request.ts`, `src/plugin/core/streaming/transformer.ts`). Their pinned
MIT licenses permit source adaptation subject to their notices and conditions;
this document copies no source expression. No credentials, live captures, login,
or provider calls were used. See [source/license context](M10-RESEARCH.md#sources-and-reuse-boundary).

Every wire shape below is a **candidate observation** from unofficial clients.
Official support, accepted fields, exact schema dialect, signature validation,
tool-call response ordering, and multi-turn behavior are **Unknown**. Live provider
behavior is **BLOCKED** pending applicable terms/eligibility and separately
authorized reproducible evidence. No remote/provider capability is promoted.

## Candidate translation

| Responses input | Candidate Gemini-style representation | Binding / limit |
| --- | --- | --- |
| Function tool declaration (`name`, optional `description`, JSON input schema) | `tools: [{functionDeclarations: [{name, description, parametersJsonSchema}]}]` (Rahul); Noe accepts/normalizes several function declaration shapes and may use `parameters` | Translate only inside the Connector. Candidate mapping is one declaration per function. Admit only the implemented JSON Schema subset (`type`, `description`, `title`, `properties`, `required`, `items`, `enum`, `minimum`/`maximum`, `minLength`/`maxLength`, `minItems`/`maxItems`, `additionalProperties`, and compilable `pattern`); apply validation recursively and reject unknown keywords or malformed constraints before dispatch. This subset is not a claim about provider dialect support. Do not silently omit a required client tool or schema property. |
| Automatic/required/none/specific `tool_choice` | Omitted `toolConfig` for automatic; `functionCallingConfig.mode` `ANY` or `NONE`; specific function adds `allowedFunctionNames` (Rahul) | Candidate only. Exact accepted enum/field spelling and enforcement are Unknown. Reject unsupported choice before dispatch rather than silently weakening it. |
| Assistant `function_call` (`name`, arguments, call ID) | Model-role `content.parts[]` with `functionCall: {name, args, id?}` | Preserve call chronology and association. Argument JSON and ID constraints are not established by official evidence. No invented IDs or signature sentinel. |
| `function_call_output` (call ID, output) | User-role `content.parts[]` with `functionResponse: {name, id?, response: {output: …}}` | Pair to the originating call by `call_id`; preserve causal order. Output serialization is candidate-only: do not coerce arbitrary structured output to text without a documented, lossless policy. |

M10-015 implements the response half for one Execute: candidate
`functionCall` parts are treated as complete call objects with a string `name`,
object `args`, and optional string `id`, as represented in the pinned Rahul
transformer. It serializes each complete argument object into a Responses delta
and done event; it does not infer argument fragments or merge repeated partial
calls. Calls and text are emitted in candidate/part order with one monotonically
assigned Responses `output_index` per output item. Missing IDs receive
connector-local per-response IDs; duplicate explicit IDs fail closed. This is
deterministic synthetic translation evidence only; provider acceptance and
whether a live server emits other shapes remain Unknown. Thought-marked parts or
any thought signature reject the frame before that frame emits content in the
current implementation. M10-040 defines only a future design candidate.

M10-016 adds the request-history half for text messages and
`function_call`/`function_call_output` items. It associates each result with its
originating call ID and name, preserves JSON arguments/results and item order,
coalesces adjacent same-role parts, and rejects orphan, duplicate, dangling,
interleaved, malformed, or signature/reasoning-bearing history before dispatch.
The implementation is stateless across Execute calls; it does not establish
signature-dependent continuation or live Gemini acceptance. The gateway adapter
still applies its Responses input validation before the Connector.

Rahul's pinned conversion coalesces adjacent same-role contents and puts a tool
result in a user-role function response; it emits a synthetic `Hello` user turn
when the first content role would otherwise be model. These are implementation
choices, not requirements to fabricate history. The offline candidate should
reject malformed or causally unpaired history before transport. It must preserve
all accepted distinct call IDs (including parallel calls) and must not collapse
calls by name or argument similarity. Exact Gemini constraints on same-turn
parallel calls, response grouping, and ordering remain **Unknown**.

## Thought signatures and state

The plugin observations conflict in policy and mechanism, so neither is a
protocol guarantee:

- Rahul reads `thoughtSignature` from provider metadata on reasoning/tool-call
  history, associates signatures with function-call parts, and uses
  `skip_thought_signature_validator` when no signature is present. Its stream
  code tracks calls and may use an argument-growth heuristic when IDs are absent.
- Noe accumulates thinking/signature data from separate stream events and caches
  it under session/model/project/conversation-derived keys. Its request path has
  signature recovery, filtering and sentinel paths, including warm-up behavior.

The sentinel's acceptance and safety are **Unknown**; it must not be generated,
substituted for missing state, or treated as proof of continuity. The OpenAI
Responses carrier exists, but no mechanism to verify a provider signature's
origin and account/model/project scope has been established. Do not assume
arbitrary metadata or unknown fields provide either. M10-040 defines a future,
locally authenticated opaque carrier gated by exact `include:
["reasoning.encrypted_content"]`; it does not establish Antigravity support or
implementation readiness. Never parse, synthesize, normalize, truncate, merge,
or replay signatures across scope boundaries. Fail closed if the carrier or
exact part association is absent; do not use a global pending/fallback signature.

The connector execution allowlist includes `gemini-2.5-pro`,
`gemini-2.5-flash`, and exact metadata-available IDs `gemini-3.8-flash-high`,
`gemini-3.8-flash-medium`, and `gemini-3.8-flash-low`; this model allowlist does
not classify models as thinking/non-thinking or gate on signature enforcement.
Metadata availability does not establish inference compatibility. Tool support
remains scoped to 2.5 models; 3.8 tool and reasoning-continuity capabilities
are Unknown. The connector rejects reasoning/signature-bearing submitted history
before dispatch, and rejects an upstream signature-bearing frame before
delivering that frame; the latter occurs after the request was dispatched. Thus
signature-dependent tool loops on signature-enforcing models are blocked, but
this is not a model-wide pre-dispatch gate or a live-provider compatibility
finding.

The v1 contract leaves request body bytes opaque to Core and permits translation
inside a Connector; no universal tool/signature type or Core parsing is needed.
However, this research has not established that Responses history can carry the
provider signature with verifiable origin and scope. The contract also exposes no
durable conversation/session state channel. Connector-local caches would invent
identity, lifetime, and isolation policy. Consequently signature-dependent
multi-request continuation remains **BLOCKED** for implementation: this local
carrier design is not evidence of provider support, and no durable session API
or Core contract amendment is presumed. No replay cache or single-use guarantee
is defined; legitimate resubmission of authenticated reasoning history must not
be rejected solely as reuse. See [DEC-031](../project/DECISIONS.md#dec-031--scope-antigravity-encrypted-reasoning-continuity) and
[contract payload and execution boundaries](CONTRACT.md#execution-request)
and [M10 identity constraints](M10-ANTIGRAVITY.md#state-and-identity-constraints).

## Synthetic fixture and offline test specification

All fixtures must be independently authored JSON/strings, tagged
`provenance=synthetic`, `source_observation=<repository>@<full-commit>`,
`provider_capture=false`, and `license=MIT` in the fixture manifest. They must
contain no credentials, real endpoint, private payload, or copied upstream code.
The existing M10 fixture manifest integrity/provenance test convention applies.

Required offline cases for a future separately scoped implementation:

1. Translate multiple function declarations, descriptions, nested schemas and
   each supported tool-choice mode; assert exact synthetic candidate request.
2. Translate two consecutive rounds: assistant call ID `call-round-1` → its
   matching output → assistant call ID `call-round-2` → its matching output →
   final assistant turn. Verify strict chronology, distinct IDs, exact per-call
   name/ID pairing, and output bytes/representation; no round may consume the
   other round's result.
3. Accept distinct parallel calls/results without collapsing/reordering; reject
   duplicate, missing, mismatched, or causally orphaned call/result identifiers
   before fake dispatch.
4. For a separately authorized M10-040 implementation, test each round's own
   locally authenticated carrier and exact signature association. Do not infer
   provider acceptance from byte-preserving synthetic round trips; reject an
   ambiguous trailer and prove wrong account/project/model/version isolation.
   No cache or replay-protection behavior is specified.
5. Reject unsupported schema/tool-choice/history fields pre-dispatch; malformed
   arguments, truncated history, and invalid signature representation never
   become an apparently valid tool execution.

M10-016 verifies sequential and parallel candidate history, pre-dispatch
rejection, and a fake-backend gateway tool cycle with incremental SSE and
cancellation. The full gateway cycle exercises string tool results; Connector
translation preserves arbitrary valid JSON result values exactly. These tests establish only deterministic translation and lifecycle behavior.
They cannot establish official wire conformance, signature acceptance, live
multi-turn success, capability support, or safe retries. Those remain **Unknown**
and **BLOCKED**, respectively. M10-010 admitted declarations and tool-choice;
M10-015 and M10-016 implement candidate response and supported history
translations. Function execution and signature-dependent continuation remain
unsupported. See the [text/stream binding](M10-BINDING.md).

Unsigned provider thought summaries are a separate output-only case under
[DEC-028](../project/DECISIONS.md#dec-028--translate-unsigned-antigravity-thought-summaries-incrementally)
and the [text/stream binding](M10-BINDING.md#transport-opt-in-and-stream-lifecycle).
They do not relax current implementation behavior: any present `thoughtSignature`
(including `null`) remains fail-closed, and neither signature-dependent tool
continuity nor inbound reasoning history is currently accepted. No signature is
stored, synthesized, discarded-and-continued, or replayed by current code.

### Encrypted continuity candidate (design only; not implemented)

For a future implementation, each Responses `reasoning` item remains in its
original input-array position. Its opaque `encrypted_content` carries one exact
signed source part using the version-1 schema in [M10-BINDING](M10-BINDING.md#encrypted-continuity-candidate-design-only-not-implemented).
Direct signatures bind to their actual part. A signature-only part is not a
separate-frame inference: it may bind only to the single immediately preceding
unsigned actual part in the same complete, source-ordered `content.parts[]`
array. Ambiguous, duplicate, non-adjacent, or cross-frame association fails
terminally after commit; do not fabricate signature/provider input. Signed
ordinary text stays visible with an adjacent continuation reasoning item;
thought and function-call signatures retain their exact source association and
ordering. Never copy signatures between calls, merge text/thought/call parts,
or trust modified visible history instead of authenticated canonical values.

Future tests cover output carrier timing (only completed reasoning item and
`response.completed`), incremental visible deltas, exact input placement, two
tool rounds, direct and adjacent signature-only association, visible signed
text, wrong account/project/model as pre-dispatch `invalid_request` with no
account fallback, fixed 24h TTL/5m future-clock bound, 1 MiB aggregate cap,
current-master/v1 derivation and old-carrier invalidation after master-key
rotation. All failures must have fixed safe diagnostics and zero provider calls
for invalid inbound carriers. Replaying the same valid history remains allowed;
no single-use/replay protection is promised and retry/fallback rules do not
change.

## References

- [M10 research and source audit](M10-RESEARCH.md#opencode-plugin-audit-reusable-observations-not-verified-contract)
- [DEC-031](../project/DECISIONS.md#dec-031--scope-antigravity-encrypted-reasoning-continuity), [OpenAI reasoning continuity guidance](https://platform.openai.com/docs/guides/reasoning#preserve-reasoning-without-stored-responses)
- [M10 text/stream binding](M10-BINDING.md)
- [M10 brief](M10-ANTIGRAVITY.md#state-and-identity-constraints)
- [Contract: execution request](CONTRACT.md#execution-request), [Connector](CONTRACT.md#connector-contract)
