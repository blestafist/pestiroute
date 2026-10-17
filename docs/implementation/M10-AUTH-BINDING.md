# M10 Antigravity OAuth Refresh Binding

## Status and evidence ceiling

This is an offline candidate binding, not an Antigravity/Google OAuth protocol
claim. Official documentation reviewed for M10 describes sign-in UX but does not
document OAuth client authorization, refresh endpoint/grant, token schema,
scopes, expiry rules, or personal-versus-enterprise refresh behavior. Those
facts remain **Unknown**; live refresh and endpoint discovery are **Blocked**.
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

## Unresolved / blocked

Official OAuth endpoints, authorized client and scopes, grant parameters,
refresh response/rotation guarantees, expiry and freshness policy, account
identity, personal-subscription versus Gemini Enterprise/Cloud eligibility, and
live exchange behavior remain **Unknown** or **Blocked** as applicable. The
plugin scope discrepancy is unresolved. No endpoint, scope, refresh token
format, quota policy, or account class is inferred from plugin code. Do not
promote capabilities, compose production auth, or perform live calls from this
binding. Resolve with current authoritative documentation and separately
authorized, reviewable evidence before any live implementation or claim.

## References

- [M10 research and pinned source inventory](M10-RESEARCH.md)
- [M10 offline execution binding](M10-BINDING.md)
- NoeFabris pinned refresh implementation:
  [`src/plugin/token.ts`](https://github.com/NoeFabris/opencode-antigravity-auth/blob/16e0056431d0a1291ee66e5938c732720b13a851/src/plugin/token.ts)
- [Connector and credential-expiry contract](CONTRACT.md#connector-contract)
- [M3 provider-neutral auth runtime binding](M3-AUTH.md#ownership-and-concurrency)
- [DEC-010](../project/DECISIONS.md#dec-010--carry-credential-expiry-with-auth-results)
