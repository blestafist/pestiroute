# M10 Antigravity Connector Handoff

## Disposition

M10-001–004 delivered the reviewed offline candidate: provenance-labelled
synthetic fixtures, one translated Responses text prompt, incremental
SSE-to-Responses text streaming, synthetic usage and HTTP error mapping,
cancellation, and deterministic tests. M10-010 extends it with trusted endpoint
and project configuration, candidate function declarations/tool-choice mapping,
gateway Responses routing, and standard HTTP transport opt-in via
`PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT=1`. The default endpoint candidate is
`https://cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse`;
the synthetic `.invalid` endpoint retains its fixture path. The production host
and wire shape come from pinned unofficial community sources, not a supported
Google API contract; endpoint, project semantics, and eligibility remain
unverified. Automated tests make no provider or external network calls.

M10-005 records the narrow client compatibility and event omissions in the
[matrix](M10-COMPATIBILITY.md). Fake tests and acceptance by
`responsesAdapter.Encode` demonstrate this offline stream can pass through that
encoder; they do not prove broad client compatibility. The live provider
contract, live authentication, account eligibility, and production route
eligibility remain unverified. `llm.streaming` is `Supported` only for the
deterministically verified `openai.responses.v1` translation implementation
scope; this is not remote entitlement or provider support. `auth.oauth`,
`usage.exact`, and all other capabilities remain `Unknown`; no live claims or
provider calls are authorized by this handoff.

## Browser OAuth and pasted-callback candidate (M10-012/M10-013)

The official Antigravity metadata URL is not PestiRoute's `client_id`. Offline
defaults now follow the client identity, token/auth endpoints, scopes, and
`prompt=consent` observed in pinned 9Router
v0.5.104 source; see [source evidence](M10-RESEARCH.md#pinned-9router-oauth-client-emulation-m10-013)
and the [auth binding](M10-AUTH-BINDING.md#browser--remote-ssh-authorization-code-candidate-m10-012m10-013).
PestiRoute bundles no client secret. The known emulated client fails closed
before token exchange or refresh without an operator-configured secret; an
explicit custom public client may be configured without one. Do not include
configured client secrets or user credentials in operator reports.

9Router uses a dynamically allocated localhost callback with a listener.
PestiRoute has no listener: its `http://localhost:8080/callback` redirect is a
bounded fixed-port candidate, not source-identical or verified as registered.
The user would need to paste the entire callback URL (code and state); an
unrelated local process may receive it. **Live authorization is blocked** pending
safe callback handling and a reviewed listener-scope decision. No live 401
resolution is claimed. The existing parser still enforces the configured
redirect, state, and PKCE S256 (an intentional addition beyond 9Router); see
[LOCAL-M10](LOCAL-M10.md#oauth-client-and-remote-ssh-code-handling) for the
operator evidence ceiling. Do not run live auth from this handoff.

After token exchange, one bounded `loadCodeAssist` query uses the validated
configured inference origin, stores only validated project metadata in the
encrypted credential bundle, and does not override the explicit Execute project.
There is no onboarding, retry, fallback endpoint, or account fallback. Gateway
real HTTPS transport still requires explicit opt-in, but that opt-in is not
authorization and this handoff provides no live OAuth commands. See
[LOCAL-M10](LOCAL-M10.md#oauth-client-and-remote-ssh-code-handling) for the
blocked live procedure and [M10-AUTH-BINDING](M10-AUTH-BINDING.md#browser--remote-ssh-authorization-code-candidate-m10-012m10-013)
for the candidate details. Synthetic tests establish no provider eligibility,
third-party authorization, OAuth/discovery compatibility, or production support;
capability `auth.oauth` remains `Unknown`. No live OAuth is authorized by this
handoff.

## Error, replay, and lifecycle boundary

Pre-dispatch validation and missing/invalid credentials fail closed. 401/403
map to `unauthenticated` and are not retryable; 429 and 5xx may be retryable but
have `unknown` replay disposition. Transport errors after invocation may be
ambiguous and also remain unknown. The connector never retries internally;
status alone does not establish safe replay, and no fallback is justified by
this offline evidence. Streaming success requires the synthetic terminal event;
malformed or truncated streams do not become success. Core/Adapter retain
commit, cancellation and exactly-once finalization responsibilities. Success
requires synthetic `response.done: true` or candidate `finishReason: STOP` then
clean EOF; usage alone is not terminal evidence. Thought/signature-bearing parts
are rejected before their frame can emit text.

## Planner handoff

Use the M10 [brief](M10-ANTIGRAVITY.md) and [roadmap](ROADMAP.md#m10--google-antigravity-connector)
to decompose future work only when evidence is ready:

1. Any live OAuth/discovery validation requires a separate reviewed authorization,
   current provider evidence, and a finite operator procedure; this handoff does
   not authorize it.
2. Research multi-round tool calls and thought-signature/history preservation;
   define an offline fake/conformance check and resolve any opaque-contract
   state requirement before implementation.

These are planning candidates, not registered READY tasks or implementation
authorization. Planner must create bounded cards, dependencies, and executable
checks before work starts. No live execution is part of these candidates. Any
later live gate needs explicit operator authority, applicable versioned
provider evidence, independent review, and separately recorded acceptance.
