# M10 Antigravity Offline Compatibility

## Evidence boundary

This matrix describes the deterministic offline candidate implemented in
`internal/connector/antigravity`, not a Google-supported API or provider
compatibility claim. M10-004 exercised only injected fake transport and
synthetic fixtures. No live endpoint, account, authentication flow, or gateway
eligibility was tested. See [binding](M10-BINDING.md), [slice decision](M10-SLICE-DECISION.md),
and [handoff](M10-HANDOFF.md).

| Area | Offline candidate behavior | Evidence ceiling |
| --- | --- | --- |
| Client protocol/mode | `openai.responses.v1`, translation mode only | Connector admission; no native mode or other protocol claim |
| Input | One non-empty text `input`, `stream: true`, model `gemini-2.5-pro` or `gemini-2.5-flash` | Synthetic allowlist only; other fields/features rejected |
| Backend destination | `POST https://antigravity.synthetic.invalid/v1internal/projects/{project}:generateContent?alt=sse` through injected fake-only transport | Reserved `.invalid` fixture destination; external destinations and redirects rejected; never a production endpoint |
| Capabilities | `llm.streaming`, `auth.oauth`, `usage.exact` are `Unknown`; other capabilities are not established | Fixture success does not satisfy live capability evidence or route eligibility |
| Stream events | The first non-empty candidate text causes one `response.created`, followed by `response.output_text.delta` event(s); later text emits deltas without another created event. The synthetic terminal marker emits `response.completed` whether or not text appeared. A terminal-only upstream stream therefore emits `response.completed` without `response.created` or deltas. | Verified synthetic behavior and Responses encoder acceptance; no claim of complete Responses lifecycle conformance |
| Event omissions | No `response.output_item.added`, `response.content_part.added`, `response.output_text.done`, `response.content_part.done`, or `response.output_item.done` | Clients requiring item/part lifecycle or text-done events are not established as compatible |
| Usage | Reads available prompt/candidate token counts from the synthetic terminal frame; missing counts remain unknown | Synthetic mapping only; `usage.exact` remains Unknown |
| Errors/replay | 401/403 map to `unauthenticated` (`false / unsafe`); 429 to `rate_limited` (`true / unknown`); 5xx may be retryable (`true / unknown`). No connector-internal retry. | Status classification is not safe-replay evidence; ambiguous transport failures remain unknown and unsafe for fallback |

Admission fails closed for unsupported protocol/mode, model, input, malformed
or duplicate-key JSON, unknown fields, missing project, invalid/missing scoped
OAuth access token, and unavailable/non-fake transport. Tools, tool history,
reasoning/signatures, multimodal inputs, structured output, project discovery,
OAuth exchange/refresh, and live authentication are unsupported or Unknown.
These constraints and the exact retry dispositions are specified in the
[binding](M10-BINDING.md); this matrix does not promote them to live behavior.

## Verification references

Offline behavior is covered by `internal/connector/antigravity` tests, including
`TestResponsesEncoderAcceptsConnectorStream`. Deterministic tests establish only
the fake fixture behavior described above. The next auth, project, and
tools/history scopes require separate cards and evidence before implementation;
live work requires separately recorded authority and provider evidence.
