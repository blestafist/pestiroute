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

## Browser OAuth and pasted-code candidate (M10-012)

The connector implements PKCE S256 start/continue. Official Antigravity docs
describe the remote-SSH URL → local browser → displayed code → terminal paste
UX. The official metadata publishes a public client ID and
`https://antigravity.google/oauth-callback` redirect; neither proves third-party
authorization or OAuth protocol compatibility. These are the default client ID
and redirect URI; no client secret is bundled. OAuth endpoint and scope defaults
remain unverified hypotheses. Optional client/endpoints/redirect/scopes
overrides remain available. The CLI/runbook direct operators to paste the
displayed code; the Connector accepts a raw code (trimmed, 1–512 visible-ASCII
bytes, no whitespace/URL delimiters) or a DEC-019 strict callback URL. Runtime
continues forwarding its existing string unchanged, with no provider branch.
Both modes use exact redirect URI, PKCE S256, and the existing runtime single-use
claim; neither can retry/replay.

After token exchange, one bounded `loadCodeAssist` query uses the validated
configured inference origin (including explicit trusted endpoint override),
stores only validated project metadata in the encrypted credential bundle, and
does not override the explicit Execute project. There is no onboarding, retry,
fallback endpoint, or account fallback. Token exchange and refresh share the
same candidate token endpoint default. Gateway HTTPS for auth/discovery/inference
requires explicit `PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT=1`, or an injected
deterministic test transport. Native CLI usage:

```sh
export PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT=1 # explicit HTTPS auth/inference transport opt-in
gateway admin auth start --account ACCOUNT_ID
# Follow verification_uri in a browser, then paste the displayed code:
gateway admin auth continue --session SESSION_ID --callback-stdin
```

The client ID override above is optional; omit it to use the published public
identifier. Codes/URLs are read without echo and never logged. Credentials and
project metadata are persisted in the existing encrypted account store. The
synthetic tests establish no provider eligibility, third-party authorization,
OAuth/discovery compatibility, or production support; capability `auth.oauth`
remains `Unknown`. No live OAuth is authorized by this handoff.

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
