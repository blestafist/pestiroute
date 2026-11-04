# M10 Antigravity Offline Compatibility

## Evidence boundary

This matrix describes the deterministic offline candidate implemented in
`internal/connector/antigravity`, not a Google-supported API or provider
compatibility claim. M10-004 exercised only injected fake transport and
synthetic fixtures. M10-045 made one read-only metadata request and observed
availability flags only; it did not test inference, account eligibility, or
gateway compatibility. See [binding](M10-BINDING.md), [slice decision](M10-SLICE-DECISION.md),
and [handoff](M10-HANDOFF.md).

| Area | Offline candidate behavior | Evidence ceiling |
| --- | --- | --- |
| Client protocol/mode | `openai.responses.v1`, translation mode only | Connector admission; no native mode or other protocol claim |
| Input | Non-empty text or supported Responses message/function-call history, `stream: true`; models `gemini-2.5-pro`, `gemini-2.5-flash`, `gemini-3.8-flash-high`, `gemini-3.8-flash-medium`, `gemini-3.8-flash-low`, `gemini-pro-agent`, `claude-sonnet-4-6`, `claude-opus-4-6-thinking`, or `gpt-oss-120b-medium` | 2.5 behavior has synthetic verification; other listed IDs are exact source-listed or metadata-available candidates admitted offline only. Bare aliases and unadmitted IDs are rejected; no inference compatibility or signature-dependent continuity claim |
| Backend destination | `POST https://antigravity.synthetic.invalid/v1internal/projects/{project}:generateContent?alt=sse` through injected fake-only transport | Reserved `.invalid` fixture destination; external destinations and redirects rejected; never a production endpoint |
| Capabilities | `llm.streaming` is a local translation capability; `llm.tools` is `Supported` only on 2.5 models; other models' tools/reasoning, `auth.oauth`, and `usage.exact` remain `Unknown` | Metadata availability does not satisfy inference capability evidence or route eligibility |
| Stream events | The first non-empty candidate text causes one `response.created`, followed by `response.output_text.delta` event(s); later text emits deltas without another created event. The synthetic terminal marker emits `response.completed` whether or not text appeared. A terminal-only upstream stream therefore emits `response.completed` without `response.created` or deltas. | Verified synthetic behavior and Responses encoder acceptance; no claim of complete Responses lifecycle conformance |
| Event omissions | No `response.output_item.added`, `response.content_part.added`, `response.output_text.done`, `response.content_part.done`, or `response.output_item.done` | Clients requiring item/part lifecycle or text-done events are not established as compatible |
| Usage | Reads available prompt/candidate token counts from the synthetic terminal frame; missing counts remain unknown | Synthetic mapping only; `usage.exact` remains Unknown |
| Errors/replay | 401/403 map to `unauthenticated` (`false / unsafe`); 429 to `rate_limited` (`true / unknown`); 5xx may be retryable (`true / unknown`). No connector-internal retry. | Status classification is not safe-replay evidence; ambiguous transport failures remain unknown and unsafe for fallback |
| Missing-project onboarding | Disabled by default; explicit `auth continue --allow-onboard` permits only the unique managed `free-tier` after the pinned CLI tier/consent checks. A single POST and bounded same-origin GET polling are durably request-budgeted. | Experimental, source-derived behavior only. No live provider/account eligibility or Google compatibility claim; remains Unknown and requires independent review before live use |

Admission fails closed for unsupported protocol/mode, model, input, malformed
or duplicate-key JSON, missing project unless the separately documented explicit
onboarding opt-in applies, invalid/missing scoped OAuth access token, and
unavailable/non-fake transport. Function declarations
and supported function history are candidate-translated; orphan, duplicate,
interleaved and dangling calls fail before dispatch. Reasoning/signatures,
multimodal inputs, structured output, OAuth exchange/refresh, and live
authentication are unsupported or Unknown. Project discovery/onboarding is a
synthetic experimental auth candidate and does not change Execute's configured
project.
These constraints and the exact retry dispositions are specified in the
[binding](M10-BINDING.md); this matrix does not promote them to live behavior.
No standard Responses carrier with verifiable origin/scope for Gemini thought
signatures is established. Consequently, fullworking live tool loops on
signature-enforcing Thinking models remain blocked despite the offline
history mapping. The connector execution allowlist includes exact metadata-
available `gemini-3.8-flash-high`, `gemini-3.8-flash-medium`, and
`gemini-3.8-flash-low` in addition to `gemini-2.5-pro` and `gemini-2.5-flash`;
bare `gemini-3.8-flash` and unavailable probed 3.1 Pro IDs remain rejected.
This is not a thinking/non-thinking classification or signature-enforcement
gate, nor proof of inference compatibility. Reasoning or signature metadata in submitted
history is rejected before dispatch; a signature first
seen in an upstream frame is rejected before that frame is delivered, after the
request has already been dispatched. Do not describe this as model-wide
pre-dispatch detection. Offline follow-up may research carrier mapping or
`skip_thought_signature_validator`; neither behavior is implemented, authorized,
or treated as a safe bypass.

Under [DEC-035](../project/DECISIONS.md#dec-035--offline-exact-wire-admission-for-antigravity-claude-and-gpt-oss-ids),
the exact source-listed IDs `claude-sonnet-4-6`,
`claude-opus-4-6-thinking`, and `gpt-oss-120b-medium` are also admitted for
offline translation only. They use the same Gemini-wire envelope, not Anthropic
Messages or OpenAI Chat, do not inherit Flash thinking configuration, and have
Unknown tools/reasoning capabilities. No alias or rewriting is performed.

## Verification references

Offline behavior is covered by `internal/connector/antigravity` tests, including
`TestResponsesToolHistoryCandidateTranslationAndCausalRejection`, and the
gateway tool-cycle/cancellation test. Deterministic tests establish only
the fake fixture behavior described above. Auth, project, and signature-replay
scopes require separate cards and evidence before implementation;
live work requires separately recorded authority and provider evidence.
