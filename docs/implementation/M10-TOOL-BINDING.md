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
substituted for missing state, or treated as proof of continuity. No compatible
Responses carrier for a provider thought signature, nor a mechanism to verify its
origin and account/model/project/conversation scope, has been established. Do not
assume arbitrary metadata or unknown fields provide either. Signature-dependent
continuation/replay is therefore **BLOCKED** until evidence establishes a
compatible carrier and verifies that binding. Never parse, synthesize, normalize,
truncate, or replay signatures across scope boundaries. Fail closed rather than
claiming signature preservation or continuity when the carrier or verification
mechanism is absent. Do not invent a contract field to fill this evidence gap.

The v1 contract leaves request body bytes opaque to Core and permits translation
inside a Connector; no universal tool/signature type or Core parsing is needed.
However, this research has not established that Responses history can carry the
provider signature with verifiable origin and scope. The contract also exposes no
durable conversation/session state channel. Connector-local caches would invent
identity, lifetime, and isolation policy. Consequently signature-dependent
multi-request continuation remains **BLOCKED**: no carrier, state mechanism, or
Core contract amendment is presumed. If later evidence requires a new cross-call
state channel, propose an ADR and compatibility review before changing the
contract. Until carrier and isolation are evidenced, do not implement or claim
signature-dependent replay. See [contract payload and execution boundaries](CONTRACT.md#execution-request)
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
4. For each of the two rounds, test its own signature association and constraints.
   Only if an evidence-backed Responses carrier and origin/scope verification
   mechanism are established may the fixture assert byte-for-byte preservation
   on that round's associated call. Until then mark signature-dependent replay
   **BLOCKED** and reject rather than invent a carrier or sentinel. If a later
   state mechanism is separately authorized, prove no cross-account/model/project/
   conversation replay or concurrent cache leakage.
5. Reject unsupported schema/tool-choice/history fields pre-dispatch; malformed
   arguments, truncated history, and invalid signature representation never
   become an apparently valid tool execution.

These tests establish only deterministic translation and lifecycle behavior.
They cannot establish official wire conformance, signature acceptance, live
multi-turn success, capability support, or safe retries. Those remain **Unknown**
and **BLOCKED**, respectively. M10-010 admits declarations and tool-choice only;
function-call response parsing, execution/history and signature-dependent
continuation remain unsupported. See the [text/stream binding](M10-BINDING.md).

## References

- [M10 research and source audit](M10-RESEARCH.md#opencode-plugin-audit-reusable-observations-not-verified-contract)
- [M10 text/stream binding](M10-BINDING.md)
- [M10 brief](M10-ANTIGRAVITY.md#state-and-identity-constraints)
- [Contract: execution request](CONTRACT.md#execution-request), [Connector](CONTRACT.md#connector-contract)
