# M10 Antigravity OAuth Refresh Binding

## Status and evidence ceiling

This is an offline candidate binding, not an Antigravity/Google OAuth protocol
claim. Official documentation reviewed for M10 describes sign-in UX but does not
document OAuth client authorization, refresh endpoint/grant, token schema,
scopes, expiry rules, or personal-versus-enterprise refresh behavior. A pinned
unofficial client source observation informs candidate auth defaults; official
support/authorization and live behavior remain **Unknown**, and live refresh and
endpoint discovery remain **Blocked**.
No login, credentials, or provider requests were used or authorized. The
synthetic `.invalid` exchange below is a local test boundary only, not a
configurable production endpoint.

Research provenance and source limitations are recorded in
[M10-RESEARCH](M10-RESEARCH.md#product-and-authentication-distinctions). The
unofficial MIT observations are pinned to Rahularya01
[`oauth.ts`](https://github.com/Rahularya01/opencode-antigravity/blob/874ef4d6cc44f852401e6298790c6b34804e2978/src/auth/oauth.ts)
for refresh, and NoeFabris
[`token.ts`](https://github.com/NoeFabris/opencode-antigravity-auth/blob/16e0056431d0a1291ee66e5938c732720b13a851/src/plugin/token.ts)
for refresh. NoeFabris
[`oauth.ts`](https://github.com/NoeFabris/opencode-antigravity-auth/blob/16e0056431d0a1291ee66e5938c732720b13a851/src/antigravity/oauth.ts)
is authorization-code exchange, not refresh; it is not cited as refresh evidence.
These pinned unofficial observations are hypotheses, not official or independent
live evidence; do not copy their OAuth client credentials or treat their
persistence formats as contract.

## Source observations, not provider facts

Observed Google OAuth callback URLs may include `scope`, `authuser`, `prompt`,
and `hd`; M10's offline parser accepts these only as an explicit connector-local
allowlist under [DEC-023](../project/DECISIONS.md#dec-023--bounded-antigravity-oauth-callback-metadata).
This is compatibility handling, not a claim that Google always returns them or
that `hd` (a domain-related value) is safe to log. Values are discarded.

RFC 9207 additionally defines the optional authorization-response `iss`
parameter; it is not required from authorization servers that do not support
RFC 9207. Under [DEC-024](../project/DECISIONS.md#dec-024--verify-optional-antigravity-oauth-callback-issuer),
Antigravity accepts it only once and only when its once-decoded value is exactly
`https://accounts.google.com`, Google's published OpenID Connect issuer. The
seven-key limit includes `code`, `state`, and all five optional metadata keys.
Absent `iss` remains compatible; present malformed or untrusted values fail
before exchange. Callback metadata is discarded and never logged or persisted.

- Rahul's pinned `refreshAntigravityAccessToken` and NoeFabris's pinned
  `refreshAccessToken` each POST form-encoded
  `grant_type=refresh_token`, `refresh_token`, and client identity fields to
  `https://oauth2.googleapis.com/token`. This URL and request shape are
  third-party observations only; no approved Antigravity endpoint or client has
  been established.
- Both refresh implementations read `access_token` and `expires_in`, accept an
  optional returned `refresh_token`, and retain the previous refresh token when
  rotation is omitted. Rahul subtracts a five-minute margin from its local
  expiry; Noe's `calculateTokenExpiry` uses its own local calculation. Noe
  stores refresh token and project identifiers in a packed plugin-specific
  value. These differences are not PestiRoute's credential schema, trusted
  lifetime policy, or evidence either behavior is correct for Google.
- The public shape worth exercising offline is therefore only a synthetic
  hypothesis: form-encoded refresh request; successful JSON with a non-empty
  access token and positive `expires_in`; optional rotated refresh token; and
  bounded, non-success/malformed response cases. No exact live field requirement
  or token lifetime is asserted.

## Credential layout compatible with current Execute

The current `internal/connector/antigravity/execute.go` reads one named
`oauth` credential and strictly decodes exactly `{"access_token":"..."}`;
unknown fields are rejected. Thus
`{"access_token":"...","refresh_token":"..."}` is currently rejected
before transport dispatch (covered by `execute_test.go`). The SQLite auth
adapter also requires exactly one credential reference and exactly one
replacement candidate (`cmd/gateway/auth_store.go:151-162`), and stores one
expiry for that generation (`:171-177`). Do not design around multiple names or
require a shared store redesign.

The selected future candidate format is one JSON object in that single named
`oauth` value:

```json
{"access_token":"<synthetic-or-issued-access-token>","refresh_token":"<synthetic-or-issued-refresh-token>"}
```

This is not accepted by today's Execute parser and is not a current runtime
capability. A future, narrowly scoped Execute change must extend its strict
parser to accept exactly the known `access_token` and optional `refresh_token`
string fields, require a non-empty access token, reject duplicate keys, empty
refresh values when present, malformed/trailing JSON and all unknown fields,
and project **only** `access_token` into the outbound Authorization header.
Execute must never use, send, log, or refresh with the refresh token. The
optional field keeps existing access-only `oauth` fixtures/credentials
compatible; `Authenticate(Action=refresh)` separately requires a non-empty
refresh token from this same selected-account bundle. If a successful exchange
omits a rotated refresh token, it carries forward the prior token in the new
complete bundle.

`Authenticate(refresh)` returns exactly one replacement candidate named
`oauth`, containing the complete new JSON bundle. The existing adapter can
atomically replace that one encrypted value and its single `CredentialExpiresAt`
under the existing revision CAS; no extra store entry, expiry column, or
multi-entry persistence behavior is implied. The selected-account scoped
credential resolution and whole-generation CAS follow the existing
[`CONTRACT`](CONTRACT.md#connector-contract),
[`DEC-010`](../project/DECISIONS.md#dec-010--carry-credential-expiry-with-auth-results),
and [`M3-AUTH`](M3-AUTH.md#ownership-and-concurrency). Provider field meanings
remain Connector-private. No shared store change or contract ADR is proposed by
this documentation task.

## PestiRoute Authenticate refresh binding

The binding is the existing provider-neutral
[`Connector.Authenticate`](CONTRACT.md#connector-contract) operation with
`AuthRequest.Action="refresh"`; it adds no operation or contract semantics.
Follow [DEC-010](../project/DECISIONS.md#dec-010--carry-credential-expiry-with-auth-results),
[M3-AUTH](M3-AUTH.md#ownership-and-concurrency), and the existing
[offline connector binding](M10-BINDING.md#project-and-credential-scope):

1. The runtime coordinator selects one enabled account and connector, reads its
   encrypted credential generation and revision, then creates the durable
   in-progress refresh marker before provider work. It supplies that account's
   credentials only through invocation-scoped `InvocationServices.Credentials`.
   The `refresh` resolver may expose that selected account's expired bundle to
   this Authenticate invocation; it never weakens ordinary credential access.
2. The connector strictly parses the single `oauth` bundle, validates its
   required refresh material, and performs at most one exchange using only the
   injected deterministic fake transport at the synthetic `.invalid` endpoint.
   No project discovery, onboarding, account fallback, or retry is part of
   refresh. Exact provider bundle keys and exchange details remain unbound until
   authorized evidence exists. `AuthRequest.State` and callback input are empty.
3. A synthetic success returns a complete replacement credential generation in
   single `AuthResult.Credentials["oauth"]` replacement bundle, with no continuation `State`, `NextAction`, or
   `UserAction`. It returns an explicit, nonzero absolute
   `AuthResult.CredentialExpiresAt` derived by the connector from the trusted
   synthetic exchange response. Expiry is metadata, not parsed from a token or
   guessed from a default. The Antigravity candidate profile requires this field
   on every successful replacement; no exact safety margin is chosen here
   without provider evidence.
4. The coordinator validates the result, then atomically CAS-replaces the
   selected account's encrypted credential bundle **and expiry** at the expected
   revision, advances revision, and clears the marker. Only acknowledged
   persistence is success. It does not merge candidate fields into the old
   bundle. Per-account coordination may coalesce waiters only by reloading the
   latest revision and applying the selected profile freshness margin; the
   cross-process CAS remains mandatory. No database transaction spans exchange.
5. If exchange may have begun but errors, is cancelled, returns unsupported or
   incomplete output, or persistence/CAS fails, discard candidates and quarantine
   the marker as uncertain. Never retry the refresh token, expose a success, or
   treat stale CAS as permission to repeat. Existing M3 restart recovery and
   explicit successful reauthentication rules apply.

`Execute` stays strictly outside this state machine: it reads only an unexpired
selected-account bundle through `services.Credentials.Get(ctx, "oauth")`, rejects
expired credentials, never calls `Authenticate` or refreshes internally, and
does not retry. A 401 is `unauthenticated`, non-retryable and unsafe (`false /
unsafe`); it does not prove a refresh is safe. This distinction follows the
[contract's expiry-gated Execute access](CONTRACT.md#connector-contract) and
[current execution binding](M10-BINDING.md#project-and-credential-scope).

## Synthetic offline fixtures and cases

Fixture provenance labels are exact and mandatory:

- `SYNTHETIC_ONLY_NOT_PROVIDER_CAPTURE`: locally authored request/response
  exchanges for successful rotation, omitted rotated refresh token, expiry
  boundary, malformed/missing access token, invalid/missing `expires_in`, HTTP
  error, cancellation, and ambiguous transport failure. Use conspicuously fake
  token strings and `.invalid` URLs only.
- `PINNED_UNOFFICIAL_SOURCE_OBSERVATION_NOT_PROVIDER_EVIDENCE`: source-derived
  notes cited above; never serialize copied real client IDs/secrets or claim
  live behavior.
- `OFFICIAL_DOCS_AUTH_UX_ONLY_NOT_OAUTH_PROTOCOL`: official sign-in/product
  documentation; it provides no token exchange/refresh facts.

The implementation task must include a synthetic integration case that invokes
Authenticate refresh through the fake exchange, persists the single returned
`oauth` bundle and `CredentialExpiresAt` by CAS, then executes using that same
persisted generation. Assert Execute sends only the access token in its
Authorization header and no refresh token or unrelated credential material
appears in the outgoing request. Parser negative tests must reject unknown
fields, duplicate keys, malformed/trailing JSON, and invalid token values before
dispatch. Across the suite also assert exactly one fake refresh exchange and
zero Execute-initiated refreshes; expired access is unavailable to Execute but
refresh-only resolution is account-scoped; expiry is persisted in the same CAS
as the encrypted generation; stale CAS, malformed or ambiguous exchange,
cancellation, and failed persistence do not retry or expose candidate success;
and refresh results carrying continuation state/action are quarantined. These
are future offline test requirements, not implementation completed by this
document.

## Browser / remote-SSH authorization-code candidate (M10-012/M10-013)

The offline implementation supports `Connector.Authenticate` `start`/`continue`
with PKCE S256. Official Antigravity CLI documentation describes remote-SSH
authentication as a unique authorization URL opened in a local browser, followed
by pasting the displayed code into the remote terminal. The official public
client-metadata document
[`oauth/client-metadata.json`](https://antigravity.google/oauth/client-metadata.json)
identifies its own URL as a public `client_id` and lists
`https://antigravity.google/oauth-callback` as a redirect URI. The offline
candidate uses the public distributed client identity, token endpoint and scope
list observed in pinned 9Router v0.5.104 source (see
[M10-RESEARCH](M10-RESEARCH.md#pinned-9router-oauth-client-emulation-m10-013));
that source includes client-authentication material, which PestiRoute does not
bundle. The known emulated client requires an operator-configured secret before
token exchange or refresh; explicit public-client configurations may omit one.
No user credentials are copied or reproduced in evidence. These third-party
observations do not establish OAuth approval, supported protocol, account
eligibility, or backend access. The published metadata URL is not used as
`client_id`.

`start` defaults to the source-pinned public client ID, endpoints, and scope
list, plus `prompt=consent`. Unless an operator configures a redirect URI, it
briefly binds loopback port 0 to obtain a dynamic candidate port, closes that
socket, and uses `http://localhost:<port>/callback`. The candidate URI is stored
in opaque Connector continuation state and the exact value is used for
authorization, DEC-019 callback validation, and token exchange, even if
configuration changes between start and continue. PestiRoute does not run or
claim a callback listener; releasing the port does not reserve it or establish
ownership. In the remote-browser / server split, operators paste the complete
callback URL from the browser address bar via `--callback-stdin`. An unrelated
local process may observe/interfere with the code or cause denial of service.
PKCE S256 keeps the verifier out of the browser and protects token redemption
from an interceptor lacking that verifier; it does not provide callback
confidentiality or availability. Dynamic-port registration/acceptance by Google
is unverified and remains a live operator gate. This offline candidate is not a
working-client or universal callback-safety claim. An SSH tunnel alone cannot
receive the callback; forwarding only applies if a real listener is bound at
the forwarded endpoint, which PestiRoute does not provide.
PestiRoute also continues to require PKCE S256 although the inspected 9Router
flow omits PKCE; this is an intentional safety addition, not an emulation claim.
The authorization URL includes `prompt=consent`, matching the source. That
parameter does not guarantee a refresh token; the pinned authorization-code
source does not establish refresh-grant behavior, so refresh remains an
unverified candidate.

The Antigravity CLI and runbook direct operators to paste the displayed code;
PestiRoute's loopback candidate instead requires the full callback URL as above.
The Connector locally accepts either a trimmed 1–512-byte visible-ASCII code
(no whitespace or `:`, `?`, `#`, `&`) or a strict DEC-019 callback URL. URL mode
compares raw scheme, authority and escaped path byte-for-byte; requires exactly
one code and expected state; rejects every extra/malformed parameter; and uses
the exact redirect URI. Malformed URLs are never reinterpreted as codes. The
runtime continues to forward the existing string unchanged, without parsing or
provider branching. Both modes remain bound to the runtime's single-use persisted
auth claim and PKCE verifier/challenge. Codes/URLs are never emitted to logs/output.
Exchange and discovery use invocation-scoped transport. After
exchange, one bounded `loadCodeAssist` query uses the validated configured
 inference origin and may extract a strictly validated `cloudaicompanionProject`
 ID into encrypted credential metadata. Discovery and explicitly opted-in
 onboarding use the pinned 9router Antigravity identity: `User-Agent:
 antigravity/ide/2.22.0 darwin/arm64`, `X-Request-Source: local`, and numeric
 metadata `{ideType: 9, platform: 3, pluginType: 2}`. This is an unofficial,
 source-pinned emulation, not Google-supported identity. It does not change the
 explicitly configured Execute project or inference request identity. Discovery
 failure/missing project fails closed unless onboarding is separately opted in;
 no retries, alternate endpoint, project substitution, or account fallback
 occurs. Token exchange and refresh
share the source-pinned default token endpoint. PestiRoute retains the public
client identity but bundles no client secret; the known emulated client fails
closed before exchange or refresh unless an operator configures its secret.
Explicit custom client IDs may be used without a secret for public-client
configurations. Gateway real HTTP transport remains disabled unless explicitly
opted in. OAuth/discovery compatibility remains
Unknown; capability `auth.oauth` stays Unknown and no live call is authorized.

## Unresolved / blocked

Official OAuth endpoints, authorized client and scopes, grant parameters,
refresh response/rotation guarantees, expiry and freshness policy, account
identity, personal-subscription versus Gemini Enterprise/Cloud eligibility, and
live exchange behavior remain **Unknown** or **Blocked** as applicable. The
`loadCodeAssist` and opt-in `onboardUser` request/response shapes are offline
candidates derived from pinned MIT source, not official protocol evidence. The
client identity corrects earlier Gemini CLI metadata claims: the pinned
Antigravity-labeled 9router call uses the fixed Antigravity UA and request-source
header with numeric metadata `9/3/2`; this particular platform value is not a
host-dynamic or macOS-arm64 enum claim. Candidate-specific `ineligible` telemetry
is distinct from aggregate `aggregate_ineligible`; raw provider reason strings
are never exposed. `UNSUPPORTED_CLIENT` maps only to fixed safe code
`unsupportedclient`. All opt-in guards remain fail-closed, including present
`currentTier`/`paidTier`, user-project requirement, terms, privacy, budget and
one-POST/12-poll limits. The plugin scope discrepancy is unresolved. Do not promote
capabilities or make a production-provider compatibility claim. Perform live
calls only after separate operator authorization and reviewable evidence.

## Missing-project onboarding audit (M10-022)

[DEC-025](../project/DECISIONS.md#dec-025--bound-experimental-antigravity-managed-project-onboarding)
accepts only a bounded experimental candidate, not Google protocol support or
Antigravity compatibility. Default behavior remains fail-closed on missing/null
`cloudaicompanionProject`; no onboarding without an explicit operator opt-in.
Source audit and provenance are in
[M10-RESEARCH](M10-RESEARCH.md#missing-project-onboarding-source-audit-m10-022).
The 2026-10-25 report that discovery advertised `standard-tier` as default
explains the local `free-tier`-only gate; it is not retained as a private fixture
or evidence of billing or provider compatibility. The explicit bounded flow
accepts only one known candidate (`free-tier` or `standard-tier`) with
`isDefault: true`. Multiple defaults or duplicate candidate entries are
ambiguous; never use legacy, unknown, non-default, or arbitrary fallback tiers.
An existing `currentTier` with no project, paid tier, provider ineligibility,
user-project requirement, privacy notice, or unaccepted/invalid terms fails
closed. User-project flag may be absent/null/false; terms absent/true; privacy
notice absent/null/false. The `standard-tier` label does not prove free billing
or zero charge; this onboarding is bounded to the account's existing
entitlements, has no billing/payment/API-enablement/resource-creation/user-project
endpoint, and is not authorization for new billing. Evaluate all discovered
candidate eligibility flags before reporting a diagnostic.

Under that explicit opt-in, experimental flow sends exactly one `onboardUser`
POST with selected tier and metadata, omitting `cloudaicompanionProject`. Accept
an immediate terminal success or a valid LRO name. Poll only by GET to the same
configured origin under `/v1internal/` plus a strictly validated relative
operation path; reject absolute URLs, authority changes, query/fragment,
backslash, dot-segment/traversal, or malformed/encoded path components. Bound to
at most 12 GET polls, fixed 2-second interval, and a 30-second overall context
deadline (whichever comes first); honor cancellation before each dispatch. Do
not retry POST or replay any ambiguous outcome. Fail closed on HTTP/JSON error,
terminal LRO error, missing/invalid operation name, deadline/cancellation, or
missing/invalid terminal project. Only after the returned project identifier
passes validation and account binding verification may the existing atomic
encrypted credential write persist credentials. Any failure leaves credentials
unsaved.
Safe stage codes may be `auth_onboarding_started`,
`auth_onboarding_polling`, `auth_onboarding_failed`, `auth_tier_ineligible`, and
`request_budget_exceeded`; expose fixed code/stage only, never provider text,
operation name, project value, callback, or credential.

Before a live test, an operator-owned durable global counter must cover every
provider HTTP operation for the authorized test account: auth/token exchange,
discovery, onboarding, polls, inference, refresh, and retries. Each potential
dispatch atomically consumes one slot before network handoff under cross-process
serialization. Exhaustion fails closed. Do not refund once dispatch may have
started, including cancellation, process death, timeout, or unknown response.
Count conservatively if transport cannot distinguish pre-dispatch failure from
an ambiguous outcome. The ceiling is <=1000, not permission to consume it; use
the fewest calls. Durable-budget implementation and review are downstream of
this research task and required before live dispatch. No account switch, paid
resources, billing changes, or destructive action is authorized.

Synthetic-only future fixture requirements (not captures or implemented tests):

| Case | Input / expected outcome | Safe assertion |
| --- | --- | --- |
| absent/null project + eligible default tier | Existing behavior fails closed; explicit opt-in sends one POST with project omitted | No dispatch by default; no credentials saved before project confirmation |
| operation in-progress then done | Fake returns valid relative operation name then terminal response with fake project ID | Same-origin GET only; max 12 polls / 2s / 30s; persist only after verified project |
| ineligible tier | Synthetic denied/ineligible result | Fixed `auth_tier_ineligible`; no payload echo or credential save |
| poll timeout/cancellation | Fake remains pending or context cancels | Stop; no retry/replay; any possible dispatch remains charged |

`onboardUser` and its LRO shape remain unverified third-party observations; no
endpoint, tier, interval, or retry policy is promoted to supported compatibility.

## References

- [M10 research and pinned source inventory](M10-RESEARCH.md)
- [M10 offline execution binding](M10-BINDING.md)
- NoeFabris pinned refresh implementation:
  [`src/plugin/token.ts`](https://github.com/NoeFabris/opencode-antigravity-auth/blob/16e0056431d0a1291ee66e5938c732720b13a851/src/plugin/token.ts)
- [Connector and credential-expiry contract](CONTRACT.md#connector-contract)
- [M3 provider-neutral auth runtime binding](M3-AUTH.md#ownership-and-concurrency)
- [DEC-010](../project/DECISIONS.md#dec-010--carry-credential-expiry-with-auth-results)
