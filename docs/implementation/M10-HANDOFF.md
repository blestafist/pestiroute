# M10 Antigravity Offline Handoff

## Disposition

M10-001–004 delivered the reviewed offline candidate: provenance-labelled
synthetic fixtures, a fail-closed fake-only connector, one translated Responses
text prompt, incremental SSE-to-Responses text streaming, synthetic usage and
HTTP error mapping, cancellation, and deterministic tests. The only backend
destination is
`https://antigravity.synthetic.invalid/v1internal/projects/{project}:generateContent?alt=sse`;
all transport is injected and non-fake transports, other destinations, and
redirects are rejected. This is not a production endpoint.

M10-005 records the narrow client compatibility and event omissions in the
[matrix](M10-COMPATIBILITY.md). Fake tests and acceptance by
`responsesAdapter.Encode` demonstrate this offline stream can pass through that
encoder; they do not prove broad client compatibility. The live provider
contract, live authentication, account eligibility, capability support, and
gateway routing eligibility remain unverified. `llm.streaming`, `auth.oauth`,
`usage.exact`, and all other capabilities remain `Unknown`; no live claims or
provider calls are authorized by this handoff.

## Error, replay, and lifecycle boundary

Pre-dispatch validation and missing/invalid credentials fail closed. 401/403
map to `unauthenticated` and are not retryable; 429 and 5xx may be retryable but
have `unknown` replay disposition. Transport errors after invocation may be
ambiguous and also remain unknown. The connector never retries internally;
status alone does not establish safe replay, and no fallback is justified by
this offline evidence. Streaming success requires the synthetic terminal event;
malformed or truncated streams do not become success. Core/Adapter retain
commit, cancellation and exactly-once finalization responsibilities.

## Planner handoff

Use the M10 [brief](M10-ANTIGRAVITY.md) and [roadmap](ROADMAP.md#m10--google-antigravity-connector)
to decompose future work only when evidence is ready:

1. Research and bind auth-bundle parsing, refresh lifecycle, account/project
   association, and required authority; prove offline lifecycle behavior before
   any separately authorized live gate.
2. Research project discovery/onboarding semantics and side effects before
   designing an offline testable slice; do not invent endpoints or provisioning.
3. Research multi-round tool calls and thought-signature/history preservation;
   define an offline fake/conformance check and resolve any opaque-contract
   state requirement before implementation.

These are planning candidates, not registered READY tasks or implementation
authorization. Planner must create bounded cards, dependencies, and executable
checks before work starts. No live execution is part of these candidates. Any
later live gate needs explicit operator authority, applicable versioned
provider evidence, independent review, and separately recorded acceptance.
