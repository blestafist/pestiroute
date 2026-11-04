# M10 Antigravity Research

Research date: **2026-10-17**. This is a documentation audit, not a compatibility
claim. No credentials were used, and no login or model/provider request was made.

## Sources and reuse boundary

| Source | Inspected revision / date | License and use |
| --- | --- | --- |
| [Google Antigravity Authenticate](https://antigravity.google/docs/install) | Live official documentation, accessed 2026-10-17; the page publishes no revision identifier. | Documentation only; no source-code reuse grant is established here. |
| [Google Antigravity OAuth client metadata](https://antigravity.google/oauth/client-metadata.json) | Live official-host metadata, accessed 2026-10-18; JSON publishes the client ID, name, redirect URI, grant/response types, and `token_endpoint_auth_method: none`. | Public client metadata only; it does not establish third-party client authorization or supported OAuth/backend protocol. |
| [Google Antigravity Plans](https://antigravity.google/docs/plans) and [Gemini Enterprise](https://antigravity.google/docs/enterprise) | Live official documentation, accessed 2026-10-17; no immutable page revision exposed. | Documentation only; no source-code reuse grant is established here. |
| [Official `google-antigravity/antigravity-cli`](https://github.com/google-antigravity/antigravity-cli) | Default-branch commit `e8a400403c82b7eb27a333784dc3c37985aa622e`, dated 2026-10-16; tree inspected through the GitHub API on 2026-10-17. | GitHub API reports no license; pinned tree contains README, changelog and examples, not CLI implementation or a LICENSE file. **No code reuse permitted on this evidence.** |
| [NoeFabris `opencode-antigravity-auth`](https://github.com/NoeFabris/opencode-antigravity-auth) | Commit [`16e0056431d0a1291ee66e5938c732720b13a851`](https://github.com/NoeFabris/opencode-antigravity-auth/commit/16e0056431d0a1291ee66e5938c732720b13a851), 2026-08-27; OAuth authorization-code [`oauth.ts`](https://github.com/NoeFabris/opencode-antigravity-auth/blob/16e0056431d0a1291ee66e5938c732720b13a851/src/antigravity/oauth.ts), refresh [`token.ts`](https://github.com/NoeFabris/opencode-antigravity-auth/blob/16e0056431d0a1291ee66e5938c732720b13a851/src/plugin/token.ts), project, request, stream, schema and [API-spec](https://github.com/NoeFabris/opencode-antigravity-auth/blob/16e0056431d0a1291ee66e5938c732720b13a851/docs/ANTIGRAVITY_API_SPEC.md) files inspected 2026-10-17. That API-spec says last updated 2025-12-13 and claims direct testing; this audit did not independently verify it. | Pinned `LICENSE` is MIT (copyright 2025 Jens); repo metadata marks repository archived. Source study/adaptation is MIT-licensed subject to its notice/conditions; no Google protocol or service authorization follows from this license. |
| [Rahularya01 `opencode-antigravity`](https://github.com/Rahularya01/opencode-antigravity) | Commit [`874ef4d6cc44f852401e6298790c6b34804e2978`](https://github.com/Rahularya01/opencode-antigravity/commit/874ef4d6cc44f852401e6298790c6b34804e2978), 2026-10-02; auth, client, request-transform, model and stream files inspected 2026-10-17. | Pinned `LICENSE` is MIT (copyright 2026 Rahul Arya). Source study/adaptation is MIT-licensed subject to its notice/conditions; no Google protocol or service authorization follows from this license. |
| [PestiRoute 9Router migration map](../references/9router-migration.md) | Map cites `14bb4f241350076984ad9a2ec639dd6cf7ae9de0`, dated 2026-09-28; map checked 2026-10-17. Its referenced `.refs/9Router` checkout is absent from this worktree. | Map itself records source license and redistribution rights **UNKNOWN**. Separately, M10-013 inspected the pinned MIT 9Router revision below for OAuth client emulation only. |
| [decolua/9router](https://github.com/decolua/9router/tree/87a74e1bda3677193c6f71a7e2100c90311f0689) | Commit `87a74e1bda3677193c6f71a7e2100c90311f0689`, tag `v0.5.104` resolves to that commit; pinned `LICENSE` is MIT. OAuth files inspected 2026-10-18. | Source study only; MIT does not establish Google approval, supported protocol, or permission to use the embedded OAuth client. No user credential store was accessed. |

The official documentation is mutable and lacks page-level commit/version metadata;
the access date is the reproducible bound, not a claim that it is immutable. The
official CLI repository's public revision is reproducible, but contains no usable
implementation source and offers no detected license. Both OpenCode candidates
below have pinned MIT licenses. Any code adaptation must retain required MIT
copyright/license notices; describing a rewrite as “clean-room” does not waive
license obligations if source expression is copied. Google API/service terms and
eligibility are separate and unresolved. Do not reuse a plugin's OAuth client
credentials or represent undocumented client identifiers, scopes, endpoints, or
headers as Google-supported protocol.

## Product and authentication distinctions

- **Personal subscription:** Official docs describe personal Google Accounts
  (Free, Google AI Pro, or Ultra) signing in to Antigravity. Plans distinguish
  quota and model access by Google AI plan; this account entitlement is not a
  Gemini API key or a Google Cloud enterprise project. Quotas and availability
  can change. [Authenticate](https://antigravity.google/docs/install#personal-google-account),
  [Plans](https://antigravity.google/docs/plans).
- **Gemini Enterprise / Google Cloud:** Business sign-in selects an assigned
  Gemini Enterprise license and its project/location; documentation also
  describes a Google Cloud project/API consumption route with billing and
  administrator setup. SSO, WIF/BYOID and ADC are separate documented enterprise
  options, not evidence that personal-subscription OAuth works for them.
  [Enterprise sign-in](https://antigravity.google/docs/enterprise#sign-in-and-license-selection),
  [Google Cloud project setup](https://antigravity.google/docs/enterprise#google-cloud-project-and-api-setup).
- **Gemini API key:** The CLI's documented `GEMINI_API_KEY` mode requires
  `modelProvider: gemini`, sends model requests directly to the Gemini API and
  does not establish an Antigravity account session. It is not a subscription
  token exchange or proof of Antigravity backend compatibility.
  [Authenticate](https://antigravity.google/docs/install#using-a-gemini-api-key).
- **CLI auth UX:** Official docs describe local OS keyring reuse and browser
  sign-in, plus remote SSH flow: open its unique URL locally, sign in, then paste
  the displayed code into the remote terminal. The published client metadata
  identifies its metadata URL as a public client ID and lists the official
  callback URI. These establish UX/metadata only; token exchange, scopes,
  PKCE/state behavior, refresh, account eligibility and backend compatibility
  remain **Unknown**. [Remote SSH OAuth flow](https://antigravity.google/docs/install#remote-ssh-oauth-flow),
  [client metadata](https://antigravity.google/oauth/client-metadata.json).

### Pinned 9Router OAuth client emulation (M10-013)

This is source-derived behavior of an unofficial MIT client, not Google protocol
documentation. At `decolua/9router` commit
[`87a74e1bda3677193c6f71a7e2100c90311f0689`](https://github.com/decolua/9router/tree/87a74e1bda3677193c6f71a7e2100c90311f0689)
(`v0.5.104`), `open-sse/providers/registry/antigravity.js:77-86` defines
authorization endpoint `https://accounts.google.com/o/oauth2/v2/auth`, token
endpoint `https://oauth2.googleapis.com/token`, and scopes `cloud-platform`,
`userinfo.email`, `userinfo.profile`, `cclog`, and `experimentsandconfigs` (each
with the Google auth scope prefix); lines 44-45 bind the public client ID and
client-auth constant. `open-sse/providers/shared.js:99-102` duplicates that
client pair. `src/lib/oauth/providers/antigravity.js:5-16,18-31` and
`src/lib/oauth/services/antigravity.js:20-51` show authorization-code URL
construction and form-encoded exchange with both client identity fields.
`src/lib/oauth/utils/server.js:16-27` and
`src/lib/oauth/services/antigravity.js:233-246,283-284` show a dynamically assigned
localhost callback on `/callback` (the service passes that exact URI to both
authorization and exchange). Authorization uses `response_type=code`, state,
`access_type=offline`, and `prompt=consent`; token exchange is a
`grant_type=authorization_code` form carrying `client_id`, client-auth
constant, code, and that redirect URI. The OAuth builder/exchange has no PKCE
challenge or verifier parameter. The public client ID and embedded client-auth
value are distributed application constants, distinct from each user's
access/refresh tokens; the client-auth value is intentionally not reproduced in
this document or task evidence.

PestiRoute adopts the pinned public client identity, endpoints, scope list and
`prompt=consent`; it retains PKCE S256 as an intentional safety addition not
present in 9Router. It does not bundle the observed client secret. The known
emulated client requires an operator-configured secret and fails closed before
token exchange/refresh when absent; explicitly configured public clients may
omit a secret. The pinned source binds a dynamically assigned port after
starting a localhost listener (`services/oauth.js:35-50` and
`services/antigravity.js:233-246,283-284`). PestiRoute starts no callback
listener: when no redirect override is configured, `auth start` briefly binds
loopback port 0, releases it, and records the resulting dynamic loopback URI in
opaque continuation state. The effective redirect URI (configured override or
generated candidate) is used unchanged for authorization, callback validation,
and token exchange, including across separate `start` and `continue` commands
and connector configuration changes. The operator pastes the complete callback
URL, including code and state, from the browser address bar via
`--callback-stdin`.

Releasing the port does not reserve it or establish callback ownership; another
local process may observe/interfere with the code or cause denial of service.
PKCE S256 keeps the verifier protected and prevents redemption by an interceptor
without it, but does not provide callback confidentiality or availability.
Dynamic redirect registration/acceptance remains unverified. Do not describe
this as a working or source-identical OAuth client or as universally safe; live
authorization remains blocked pending provider evidence and operator authority.
`prompt=consent` is only an
authorization request parameter; it does not guarantee offline access or a
refresh token, and the pinned auth-code flow does not establish refresh-grant
behavior. The operator-reported browser `401 invalid_client` was not
independently reproduced and this offline correction is not claimed to resolve
it. Account lookup/onboarding fallbacks and retries in 9Router remain excluded.

### Account and project mapping

For personal subscription sign-in, official material identifies a personal Google
Account and plan, but does not document a stable account identifier, entitlement
API, quota interface, or project-discovery protocol: **Unknown**. For enterprise,
the user selects an assigned license/project, or enters a project and location;
CLI docs also document `--project PROJECT_ID`. The project/location selection is
therefore an explicit onboarding input, not evidence that a connector should
discover, provision, or silently substitute a project. Ownership, IAM checks,
project lookup APIs, onboarding side effects and account-to-project persistence
remain **Unknown** for a PestiRoute connector. See the [enterprise guide](https://antigravity.google/docs/enterprise#sign-in-and-license-selection)
and [CLI auth docs](https://antigravity.google/docs/install#gemini-enterprise--google-cloud-account).

## Protocol evidence classification

| Area | Classification | Evidence and limit |
| --- | --- | --- |
| Supported public inference wire contract | **Unknown** | Current official product, CLI and enterprise docs describe product/auth usage, not a supported third-party inference API or a subscription-backed connector contract. API-key and enterprise Cloud APIs are distinct products. |
| OAuth wire (authorization, exchange, refresh, scopes, account identity) | **Unknown** | Official docs describe browser/SSH sign-in UX, not endpoints, grant parameters, client credentials, scopes, PKCE, state validation or token schema. The migration map's older 9Router-specific OAuth claims point to an unavailable checkout and cannot verify current behavior. |
| Request envelope, model IDs, headers, endpoint/version | **Unknown** | No current official source reviewed specifies an Antigravity subscription backend request envelope, supported direct endpoint, model-ID mapping, or required protocol headers. |
| Streaming format, event order, terminal/usage semantics | **Unknown** | No current official source reviewed specifies backend streaming transport, framing, usage extraction, cancellation or finalization semantics. CLI UX and model listings do not establish these wire properties. |
| Enterprise project/license selection | **Documented UX only** | Enterprise docs say select assigned license/project/location, or choose Other and provide project/location; CLI documents `--project`. The underlying discovery/API/onboarding protocol and side effects are **Unknown**. |
| 9Router `v1internal` envelope / stream / project behavior | **Unverified historical secondary claim** | `9router-migration.md` maps these to commit `14bb4f241350076984ad9a2ec639dd6cf7ae9de0`, but its `.refs/9Router` source checkout is not present here and its license is unknown. Do not treat this as current official evidence or reuse permission. |

## OpenCode plugin audit: reusable observations, not verified contract

The following are source observations at the pinned MIT revisions above. These
projects are unofficial clients, not Google documentation. Their claims and
successful-looking source/tests are not independent live-provider evidence; no
provider calls or login attempts were made for this audit. No source code is copied
into PestiRoute by this research.

| Topic | Pinned-source observation | Evidence ceiling / Go-rewrite guidance |
| --- | --- | --- |
| OAuth client and scopes | Both candidates implement Google OAuth authorization-code with PKCE S256, loopback callback, offline access, state, token exchange/refresh and user-info lookup ([Rahul `oauth.ts`](https://github.com/Rahularya01/opencode-antigravity/blob/874ef4d6cc44f852401e6298790c6b34804e2978/src/auth/oauth.ts), [Noe `oauth.ts`](https://github.com/NoeFabris/opencode-antigravity-auth/blob/16e0056431d0a1291ee66e5938c732720b13a851/src/antigravity/oauth.ts)). Rahul's scope list includes `aicode`, `cloud-platform`, `userinfo.email`, `userinfo.profile`, `cclog`, `experimentsandconfigs`; Noe's pinned [`constants.ts`](https://github.com/NoeFabris/opencode-antigravity-auth/blob/16e0056431d0a1291ee66e5938c732720b13a851/src/constants.ts) omits `aicode`. Both pin code containing OAuth client identity and a client secret (not reproduced here). The official metadata endpoint lists its own URL as `client_id` and `https://antigravity.google/oauth-callback` as redirect URI, but does not document the authorization/exchange flow or authorize third-party use. | Scope discrepancy and undocumented OAuth protocol leave client authorization/compatibility **Unknown**. Do not copy plugin credentials or treat the public ID as approval. Use PKCE/state validation and runtime-owned credential storage. |
| Account/project discovery | Both POST `v1internal:loadCodeAssist` with bearer auth/client metadata and parse `cloudaicompanionProject` as string or `{id}` ([Rahul `client.ts`](https://github.com/Rahularya01/opencode-antigravity/blob/874ef4d6cc44f852401e6298790c6b34804e2978/src/client/client.ts), [Noe `project.ts`](https://github.com/NoeFabris/opencode-antigravity-auth/blob/16e0056431d0a1291ee66e5938c732720b13a851/src/plugin/project.ts)). Their sources also retry, fall back, or call `onboardUser`. | Third-party-only evidence; not an official API. M10-012 limits its offline candidate to one discovery query, encrypted credential metadata, no onboarding/retry/fallback, and no substitution of the explicitly configured Execute project. Live compatibility remains Unknown. |
| Request envelope | Rahul's pinned [`transform.ts`](https://github.com/Rahularya01/opencode-antigravity/blob/874ef4d6cc44f852401e6298790c6b34804e2978/src/stream/transform.ts) builds `{project, model, request, requestType, userAgent, requestId}` and nests Gemini-style `contents`, `systemInstruction.parts`, `generationConfig`, `tools`, plus `sessionId` and `labels`; tool results become user-role `functionResponse` parts. Noe's pinned [`request.ts`](https://github.com/NoeFabris/opencode-antigravity-auth/blob/16e0056431d0a1291ee66e5938c732720b13a851/src/plugin/request.ts) wraps GenerateContent JSON and rewrites model/action URL. | This matches the old migration map's general `v1internal` description but is still unsupported by current official docs. Treat fields, headers, model IDs and action paths as candidate-only observations. Transform explicitly in the Connector; no provider schema in Core. |
| Streaming | Both parse `data:` SSE frames whose JSON contains a `response` wrapper and `candidates[].content.parts`; usage may arrive on response chunks ([Rahul `stream.ts`](https://github.com/Rahularya01/opencode-antigravity/blob/874ef4d6cc44f852401e6298790c6b34804e2978/src/stream/stream.ts), [Noe streaming transformer](https://github.com/NoeFabris/opencode-antigravity-auth/blob/16e0056431d0a1291ee66e5938c732720b13a851/src/plugin/core/streaming/transformer.ts)). Rahul's parser carries incomplete lines across reads, flushes final unterminated bytes, deduplicates cumulative tool-argument updates without collapsing distinct explicit IDs, and propagates abort/reader cancellation. Noe has a TransformStream SSE transformer and signature/thinking accumulation. | Useful deterministic test hypotheses for incremental parsing, first-event timing, final usage, cancellation and parallel-tool distinction. These parsers do not establish server framing guarantees, terminal-event requirements or delivery/retry safety. |
| Thought/signature state | Both keep signatures opaque on thinking/function-call parts ([Rahul request transform](https://github.com/Rahularya01/opencode-antigravity/blob/874ef4d6cc44f852401e6298790c6b34804e2978/src/stream/transform.ts), [Noe request transform](https://github.com/NoeFabris/opencode-antigravity-auth/blob/16e0056431d0a1291ee66e5938c732720b13a851/src/plugin/request.ts)). Rahul links `thoughtSignature` to tool calls and includes a source-defined skip sentinel for absent signatures. Noe caches signatures/thinking across SSE chunks ([transformer](https://github.com/NoeFabris/opencode-antigravity-auth/blob/16e0056431d0a1291ee66e5938c732720b13a851/src/plugin/core/streaming/transformer.ts)) and keys request recovery by session/model/project/conversation; it also uses sentinel/strip/warm-up recovery paths. | Preserve opaque signature bytes with history only if verified required; never synthesize, alter or cross-account/session/model/project-replay them based solely on plugin behavior. Sentinel acceptance is not established by official docs; classify it **Unknown**. Signature state/replay semantics need dedicated evidence before a tool slice. |

These projects also warn that unofficial OAuth proxies may violate Google terms and
put accounts at risk. Their MIT code license does not grant Google API entitlement,
authorize bypassing product restrictions, or resolve account suspension risk.
The archived NoeFabris revision and Rahularya01's smaller, more recently updated
revision are both useful research inputs; neither is adopted as a supported or
production-ready implementation. Any later source-derived code needs normal
license-notice review, source/provenance tracking and separate Google-service
policy review.

No unsupported claim is promoted from the old migration map or plugin code to a
Google-supported protocol fact. This audit resolves the source-research gate but
does **not** authorize a production connector, live verification, or use of the
third-party OAuth client credentials.

## PestiRoute boundary and next evidence

The existing [Connector contract](CONTRACT.md#connector-contract) keeps provider
authentication and wire translation inside a Connector; Core owns selected
account, secret persistence, refresh coordination and lifecycle, while the
Connector owns provider protocol details. `Authenticate` uses opaque continuation
state and separately validated user-action presentation. The existing
[Codex](../../internal/connector/codex/auth_start.go) and
[Claude Code](../../internal/connector/claudecode/auth.go) flows are implementation
patterns, not evidence of Antigravity's auth protocol. Preserve those boundaries;
do not infer Google-specific behavior from them.

Before binding any execution slice, settle applicable Google terms/account
eligibility and pin the intended connector/client version. Treat the MIT plugins'
OAuth, discovery, envelope, SSE and signature behavior only as offline
implementation hypotheses. Resolve entitlement, auth/refresh, account/project
mapping, incremental stream, usage, signature continuity and safe cancellation/
retry semantics with reproducible authorized evidence. These protocol and
live-compatibility questions remain **Unknown**. No provider call or login was
needed or authorized for this audit.

## Missing-project onboarding source audit (M10-022)

Audit date: 2026-10-24. Public source fetch only; no provider endpoint, OAuth,
account, or private local credential activity.

| Source | Revision and inspected files | Observed schema/flow | Confidence |
| --- | --- | --- | --- |
| Google `google-gemini/gemini-cli` | `cd1b45c2db0a5ea7b7fc3b67c1ba5879601d6005`, `packages/core/src/code_assist/codeAssist.ts:21-35`, `setup.ts:213-332`, `server.ts:257-264,415-467,524-537`, `types.ts:93-105,162-195` | This is an actual CLI setup flow, not just types: codeAssist calls `setupUser`. When `currentTier` exists but project is absent, setup uses explicit environment project if supplied; otherwise reports ineligible/project-required and does not onboard. When `currentTier` is absent, it selects `allowedTiers[].isDefault`, else legacy fallback; the free managed tier sends `onboardUser` with `cloudaicompanionProject` omitted. It polls operation by GET `v1internal/{operationName}` every 5 seconds until done (unbounded in this CLI implementation). LRO `error` is not handled explicitly in setup; PestiRoute must fail closed. Types expose tier privacy/TOS/user-project indicators. | High for behavior in this pinned Gemini CLI flow; not proof that its client metadata, eligibility, or flow is supported for Antigravity accounts. [codeAssist.ts](https://github.com/google-gemini/gemini-cli/blob/cd1b45c2db0a5ea7b7fc3b67c1ba5879601d6005/packages/core/src/code_assist/codeAssist.ts), [setup.ts](https://github.com/google-gemini/gemini-cli/blob/cd1b45c2db0a5ea7b7fc3b67c1ba5879601d6005/packages/core/src/code_assist/setup.ts), [server.ts](https://github.com/google-gemini/gemini-cli/blob/cd1b45c2db0a5ea7b7fc3b67c1ba5879601d6005/packages/core/src/code_assist/server.ts), [types.ts](https://github.com/google-gemini/gemini-cli/blob/cd1b45c2db0a5ea7b7fc3b67c1ba5879601d6005/packages/core/src/code_assist/types.ts). |
| `kimws/gemini-oauth` | `35b51d2d85180c181b1eb64e818dce32b043f400`, `src/project.ts:143-163,209-245`, `src/constants.ts` | Calls `loadCodeAssist`; prefers `free-tier`, else default, else first `allowedTiers`; calls `onboardUser` with chosen tier and project hint; response may be immediate `done` or contain operation `name`; polls via GET `${base}/${name}` with bearer auth; 12 iterations and 2s delay in constants; returns nested project ID only when done. Does not inspect `hasAcceptedTos`, privacy notice, or ineligible reasons before choosing in this flow. | High for source observation; low for wire compatibility; unofficial. [Pinned project.ts](https://github.com/kimws/gemini-oauth/blob/35b51d2d85180c181b1eb64e818dce32b043f400/src/project.ts), [constants.ts](https://github.com/kimws/gemini-oauth/blob/35b51d2d85180c181b1eb64e818dce32b043f400/src/constants.ts). |
| `decolua/9router` | `87a74e1bda3677193c6f71a7e2100c90311f0689`, `open-sse/services/projectId.js:173-190,204-248`, `src/lib/oauth/providers/antigravity.js:45-82` | Corrected audit: project service returns immediately if project exists; if missing, chooses a default allowed tier (legacy fallback), then calls onboarding. It sends `{tierId, metadata}`, expects immediate done or repeats the onboarding POST up to 2 by default (configurable), with 12-second delay plus jitter; it does not GET an LRO operation. Separate post-exchange code has fire-and-forget onboarding when project already exists. | High for pinned source; Antigravity-labeled unofficial implementation only, no provider confirmation. [Pinned projectId.js](https://github.com/decolua/9router/blob/87a74e1bda3677193c6f71a7e2100c90311f0689/open-sse/services/projectId.js), [antigravity.js](https://github.com/decolua/9router/blob/87a74e1bda3677193c6f71a7e2100c90311f0689/src/lib/oauth/providers/antigravity.js). |

## Antigravity discovery/onboarding identity correction (M10-027)

Audit date: 2026-10-29. Read-only public-source inspection of the pinned
`decolua/9router` commit above; no endpoint, account, token, or private local
state was accessed. The exact provider-specific `postExchange` path in
[`antigravity.js`](https://github.com/decolua/9router/blob/87a74e1bda3677193c6f71a7e2100c90311f0689/src/lib/oauth/providers/antigravity.js)
constructs a `loadHeaders` object with the Antigravity desktop UA and
`x-request-source: local`; its `loadCodeAssist` POST sends
`JSON.stringify({metadata})`, then its `onboardUser` POST reuses those same
headers and sends `{tierId, metadata}`. The pinned
[`appConstants.js`](https://github.com/decolua/9router/blob/87a74e1bda3677193c6f71a7e2100c90311f0689/open-sse/config/appConstants.js)
defines the UA as `antigravity/ide/2.22.0 darwin/arm64`, `ideType` as 9,
`pluginType` as 2, and platform enum 3 as Linux AMD64; its metadata builder
computes platform from the running host, so `{ideType:9,platform:3,pluginType:2}`
is the Linux AMD64 result. PestiRoute pins that requested source-observed value;
it is not a claim that platform 3 describes the UA's macOS ARM host. The source
computes platform dynamically on other hosts. The separate
`projectId.js` service uses a different header constant and has configurable
POST retries; it is not the canonical call path used to pin these headers.
`kimws/gemini-oauth` does not establish this Antigravity identity.

The earlier use of Gemini CLI `IDE_UNSPECIFIED`/string metadata and the claim
that this client identity was absent are corrected for Antigravity discovery and
onboarding only. Whether client identity changes account tier advertisement or
eligibility is plausible but unproven. PestiRoute retains stricter existing
guards and bounded one-POST/same-origin-GET behavior; this source correction is
not a compatibility finding or permission for live calls. In diagnostics,
`ineligible` means the selected default candidate and `aggregate_ineligible`
means either recognized candidate is listed; neither exposes raw reason text.
Only `UNSUPPORTED_CLIENT` maps to fixed safe enum `unsupportedclient`.

Conclusion: bounded experimental onboarding is a source-derived candidate, not
universal Google approval or an Antigravity compatibility claim.
The official CLI has a concrete missing-project flow only when `currentTier` is
absent; when `currentTier` exists without a project it fails closed or requires
an explicit project. The Antigravity-labeled 9router source also enters onboarding
when project is missing, but retries POST rather than polling. The selected
candidate follows the inspected pinned-source request metadata and separately
uses bounded same-origin GET operation polling; the 9router retrying POST flow
is not copied. It rejects unresolved current tier, paid or user-defined-project
tiers, and any terms/privacy condition lacking explicit authorized acceptance.
Keep default `auth_project_missing`, persist no credential before project
confirmation, and make no claim that the wire is supported by Google or known to
work for the user's still-uninspected account tier; actual account tier is
unknown. DEC-025 records this bounded
experimental scope; durable-budget implementation/review is downstream and is
not a research-task blocker. Live dispatch remains gated on the required review.
See [DEC-025](../project/DECISIONS.md#dec-025--bound-experimental-antigravity-managed-project-onboarding)
and the [M10 auth binding](M10-AUTH-BINDING.md#missing-project-onboarding-audit-m10-022).
