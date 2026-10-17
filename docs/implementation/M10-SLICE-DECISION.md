# M10 Offline Slice Decision

## Decision: GO, fake-only

Proceed with one offline `openai.responses.v1` translated text prompt and
incremental text/stream response through a deterministic injected fake. This is
an implementation GO for M10-004 only, not a supported Google protocol, live
provider authorization, or capability evidence. The existing contract remains
unchanged; runtime Execute/composition is out of scope here.

The transport boundary is fixed to
`POST https://antigravity.synthetic.invalid/v1internal/projects/{project}:generateContent?alt=sse`.
The host is reserved `.invalid`; M10-004 must reject all other destinations,
redirects and non-fake transports before network dispatch. No live or ambient
network access is permitted. `request-positive.json` records a candidate-only
outer request shape (`project`, `model`, `request.contents`, `requestType`,
`userAgent`, `requestId`), not an official API schema. For these fixtures,
synthetic `data:` JSON envelopes contain `response.candidates[].content.parts`;
`response.done: true` is the sole successful terminal marker and the usage
fixture gives usage only on that terminal chunk. This fixture convention is not
a claim about server framing or terminal semantics. Text chunks are otherwise
incremental; EOF without the marker and malformed JSON are failures, never
success.

The fake will also classify 401/403 as `unauthenticated` with
`Retryable=false` / `RetryDisposition=unsafe`, 429 as `rate_limited` with
`Retryable=true` / `unknown`, and 5xx as potentially retryable / `unknown`.
These classifications never authorize connector-internal replay.

The offline fixture model allowlist is exactly `gemini-2.5-pro` and
`gemini-2.5-flash`. Fixture project, prompt, request ID, response and error
values are synthetic. No account credentials are represented.

## Explicitly outside the slice / Unknown

Tools and function calls, thought signatures or replay state, multimodal input,
system instructions, OAuth exchange/refresh, live endpoints, enterprise project
discovery or auto-provisioning are unsupported and remain Unknown. Do not infer
provider support, eligibility, authentication capability or production
readiness from fixture success. Revisit only with a separately scoped task and
appropriate evidence/authorization.

## Provenance and license

Fixture payloads are synthetic and independently authored from candidate
observations in MIT-licensed OpenCode projects; they contain no live captures,
credentials, proprietary client secrets, binary assets, or copied implementation
code. Observations are not Google documentation or verified wire behavior.

- NoeFabris, `opencode-antigravity-auth`, pinned revision
  [`16e0056431d0a1291ee66e5938c732720b13a851`](https://github.com/NoeFabris/opencode-antigravity-auth/tree/16e0056431d0a1291ee66e5938c732720b13a851),
  MIT License, copyright 2025 Jens.
- Rahularya01, `opencode-antigravity`, pinned revision
  [`874ef4d6cc44f852401e6298790c6b34804e2978`](https://github.com/Rahularya01/opencode-antigravity/tree/874ef4d6cc44f852401e6298790c6b34804e2978),
  MIT License, copyright 2026 Rahul Arya.

MIT licensing permits reuse subject to each upstream license's conditions; it
does not grant Google API entitlement, protocol support, account eligibility or
service authorization. The full source audit and unknowns are recorded in
[M10 research](M10-RESEARCH.md); this decision retains the MIT attribution and
license notices and does not reproduce upstream source code.
