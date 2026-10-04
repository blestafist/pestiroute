# M4 Anthropic Translation Compatibility

This matrix describes the implemented, scoped profile—not general Anthropic or
OpenAI compatibility. `supported` means deterministic local evidence exists;
it does not mean every prompt, account, or current provider deployment works.
Live entitlement and wire compatibility are **unknown** pending M4-036/M4-037.
No live calls were made for this audit.

## Profile

| Scope item | Binding |
| --- | --- |
| Client protocol / mode | `openai.responses.v1` / `translation` |
| Client model | Exact configured Responses-side route model; no inference or rewrite |
| Backend | `POST https://api.anthropic.com/v1/messages`, `anthropic-version: 2023-06-01`, backend model `claude-opus-5-5` |
| Identity | One configured model and account per Connector instance; runtime supplies that account's credential; no implicit account fallback |
| Client baseline | OpenCode V2 CLI 2.0.6, HTTP Responses API; this is a binding baseline, not proof of external-client/live compatibility |
| Execution | Streaming only; Connector translates Messages JSON/SSE to Responses events; client owns tool execution and subsequent rounds |

## Capability matrix

The declaration column is what `Capabilities` and `Models` currently expose
for an exact matching protocol/mode/model/account scope. Missing entries are
`unknown` by [CONTRACT](CONTRACT.md#capability-model); unknown never passes
Core eligibility. “Profile result” distinguishes a feature deliberately
rejected locally from a feature whose support is merely unproven.

| CONTRACT capability | Profile result | Declared state | Evidence / limit |
| --- | --- | --- | --- |
| `llm.streaming` | Supported | `supported` | Synthetic SSE lifecycle and early-delivery checks: `TestExecuteStreamTranslateEarlyHeadAndUsage`, `TestExecuteBackpressureReadsOnlyOnDemand`, `TestResponsesEmitterLifecycle`; no live provider claim. |
| `llm.tools` | Supported for ordinary function tools and client-owned rounds | `supported` | Schema/choice/history and stream checks plus protected three-request round trip: `TestTranslateTools`, `TestTranslateToolChoiceAndParallelControls`, `TestExecuteToolStreamLifecycle`, `TestExecuteTwoToolStreamAndParallelControls`, `TestTranslationClientOwnedToolRounds`. Tool execution is never performed by the gateway. |
| `llm.tools.parallel` | Unknown; no end-to-end support claim | `unknown` | Core does not infer it from `llm.tools`; true parallel requirement is rejected before egress. Connector-local false control is supported only as an explicit disable setting, not as parallel capability. |
| `llm.reasoning` | Unsupported | `unknown` | Binding decision rejects reasoning input/history and provider thinking/redacted blocks; `TestReasoningRequestsFailClosed`, `TestReasoningProviderBlocksFailClosed`, `TestStreamThinkingBlocksFailClosedBeforeFollowingTool`, and gateway `TestTranslationClientOwnedToolRounds`. |
| `llm.structured_output` | Unknown / not implemented | `unknown` | `json_schema` and `json_object` requirements fail Core eligibility; no strict-output guarantee. Tool parameter schemas are separate and do not imply structured response output. |
| `llm.vision` | Unsupported by this profile | `unknown` | Non-text/image input is not translated and the scoped declaration does not claim vision. |
| `llm.audio` | Unsupported by this profile | `unknown` | Audio fields/content are not translated and the scoped declaration does not claim audio. |
| `auth.oauth` | Unsupported | `unknown` | Interactive `Authenticate` returns `Supported:false`; this profile uses a runtime-scoped API credential, not OAuth. |
| `usage.exact` | Unknown | `unknown` | Provider counters are normalized when present; missing counters remain unknown and local estimation is conservative. `TestExecuteUsageCacheCumulativeAndMissing`, `TestExecuteUsageRejectsNegativeAndOverflow`, and `TestTranslationSettlementPersistsProviderUsage` do not establish exact usage for every request/provider response. |

The current declaration intentionally contains only `llm.streaming` and
`llm.tools` in both `Capabilities` and the single configured model result.
Other vocabulary entries are omitted, not advertised as unsupported; where
local behavior explicitly rejects a feature, that policy does not change the
omitted declaration's `unknown` state.

## Request field policy and errors

Core evaluates adapter requirements before Connector translation. Requests
requiring a capability not declared supported fail admission with HTTP 400
`unsupported_capability` and no upstream dispatch. Malformed or unrepresentable
Connector-profile inputs return HTTP 400 `invalid_request` before dispatch. The
Adapter returns `unsupported_feature` for a syntactically valid but unknown text
format; malformed formats remain `invalid_request`.

| Field / shape | Policy and mapping | Rejection / evidence |
| --- | --- | --- |
| `model` | Required; must equal configured client route model; sent backend as `claude-opus-5-5`. | Malformed input is `invalid_request`; model/account/protocol scope mismatch fails closed before upstream as a routing/scope error. No model-name feature inference. |
| `stream` | Required literal `true`; sent true. | Omitted, false, null, or other value: `invalid_request`; non-streaming is not silently forced. |
| `input` string | Required non-empty; becomes one user text message. | Missing, empty, null or malformed: `invalid_request`. |
| `input` message array | Non-empty ordered `message` items. `user`/`assistant` text and prefix-only `developer` text map in order; assistant status may only be `completed`, with optional empty `annotations`/`logprobs`. Function call/result history maps to tool-use/result blocks with checked IDs and chronology. | Unsupported roles/content, `system` history, late developer messages, malformed chronology, duplicate IDs or extra item fields: `invalid_request`. |
| `instructions` | Optional non-empty string; appended as ordered system text before developer-prefix text. | Invalid value: `invalid_request`. |
| `max_output_tokens` | Optional positive integer, maximum 4096; default 4096; sent unchanged as `max_tokens`. Admission reserves against the effective bound. | Invalid/out-of-range or legacy `max_tokens`: `invalid_request`. |
| `temperature` | Optional exact JSON number in `[0,1]`; sent unchanged. Omission leaves backend default. | Invalid/out-of-range or unsupported controls (`top_p`, penalties, stop sequences): `invalid_request`. |
| `store` | Omitted or literal `false`; no server-side state is requested or forwarded. | True/other values: `invalid_request`. |
| `include` | Omitted or exactly `["reasoning.encrypted_content"]`; no-op hint only. | Other forms: `invalid_request`; this does not enable reasoning. |
| `tools` | Optional flat Responses function definitions; `name`, optional `description`, object `parameters`, optional `strict:false`. Schema JSON is forwarded to Messages `input_schema`; call/result history is translated; calls/results are forwarded to the client. | Chat-Completions nested form, hosted/non-function tools, extra fields, malformed/duplicate names/schema: `invalid_request`. `strict:true` is rejected; nested schema keywords are forwarded but enforcement is not promised. |
| `tool_choice` | Optional `auto`, `required`, `none`, or declared named function; mapped explicitly. `none` suppresses tools. | Unsupported shape/name or choice without valid tools: `invalid_request`. |
| `parallel_tool_calls` | Omitted or boolean. `false` maps to explicit backend disable. `true` requires `llm.tools.parallel` only when non-empty tools are supplied; true without tools is a no-op and does not require that capability. | With tools, true fails Core admission as `unsupported_capability` because parallel support is undeclared. Malformed values or false without tools (unless choice `none`): `invalid_request`. |
| `reasoning` | Not admitted. | Adapter detects a reasoning requirement and Core returns `unsupported_capability`; reasoning history/content reaching Connector fails `invalid_request`. Provider thinking/redacted blocks fail the stream closed; never flattened to text. |
| `text.format` (`json_schema`, `json_object`) | Not admitted; requires `llm.structured_output`. | Core returns `unsupported_capability` before Connector/upstream dispatch. Unknown format is Adapter `unsupported_feature`; malformed format is `invalid_request`. |
| Vision/audio, resource IDs and background/stateful fields | No image/audio mapping, stored response resources, `previous_response_id`, `conversation`, or background execution. | Image/audio capability requirements fail closed when recognized; unsupported resource/state fields fail Adapter or Connector validation (`invalid_request`) before dispatch. |
| Other top-level fields | No unknown extensions are silently discarded. | Connector rejects unrecognized fields as `invalid_request`. |

## Response and usage behavior

Locally tested text and function streams preserve incremental order and emit
Responses lifecycle snapshots with distinct response/item/call identities.
Normal completion requires valid Messages stop markers. `max_tokens` is
incomplete, not successful; malformed/missing terminal events, unknown semantic
blocks, and provider thinking fail rather than becoming successful text.
Provider SSE does not pass through unchanged. After client stream commitment,
errors terminate in-band when possible or close; they cannot be replaced with a
new HTTP response or retried. Usage fields remain nullable: absent, cumulative,
invalid, and overflow counters are not fabricated into exact values.

## Deterministic evidence index

These completed cards describe local fixtures and tests, not live provider
behavior. Linked cards record the concrete commands and result details.

| Evidence group | Completed cards / scope |
| --- | --- |
| Binding, fixtures, routing, request validation | [M4-001](tasks/M4-001.md), [M4-002](tasks/M4-002.md), [M4-003](tasks/M4-003.md), [M4-004](tasks/M4-004.md), [M4-005](tasks/M4-005.md), [M4-006](tasks/M4-006.md), [M4-007](tasks/M4-007.md), [M4-008](tasks/M4-008.md), [M4-009](tasks/M4-009.md), [M4-010](tasks/M4-010.md), [M4-011](tasks/M4-011.md), [M4-012](tasks/M4-012.md), [M4-013](tasks/M4-013.md), [M4-014](tasks/M4-014.md), [M4-015](tasks/M4-015.md), [M4-016](tasks/M4-016.md) |
| Function tools, history and client-owned rounds | [M4-017](tasks/M4-017.md), [M4-018](tasks/M4-018.md), [M4-019](tasks/M4-019.md), [M4-020](tasks/M4-020.md), [M4-021](tasks/M4-021.md), [M4-022](tasks/M4-022.md) |
| Usage, failure, cancellation, limits and accounting | [M4-023](tasks/M4-023.md), [M4-024](tasks/M4-024.md), [M4-025](tasks/M4-025.md), [M4-026](tasks/M4-026.md), [M4-027](tasks/M4-027.md), [M4-028](tasks/M4-028.md) |
| Reasoning decision, implementation and stream verification | [M4-029](tasks/M4-029.md), [M4-030](tasks/M4-030.md), [M4-031](tasks/M4-031.md) |

The primary capability proofs are `internal/connector/anthropic/component_test.go`
(`TestConnectorLifecycleScopeAndSupport`), the connector translation/stream tests
named above, and `cmd/gateway/translation_tool_rounds_test.go`
(`TestTranslationClientOwnedToolRounds`). That gateway test uses a local protected
fixture/fake Messages server; capability negatives assert 400 and unchanged
upstream request count. None proves live account entitlement or current wire
compatibility. M4-036/M4-037 own that separate evidence gate.
