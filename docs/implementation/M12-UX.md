# M12 Operator UX and API Boundary

This is the M12-001 design for the six screens in [M12-WEBUI](M12-WEBUI.md).
It describes a local single-operator management UI, not a hosted console. Until
M11-001 freezes its OpenAPI binding, resource names below are conceptual needs,
not endpoint or DTO commitments. Mocks must be visibly labeled and must not imply
that unsupported operations are live.

## Shared shell and interaction rules

- A persistent header identifies the gateway/environment, connection state, and
  authenticated operator session. Primary navigation links Overview, Connections,
  Keys & Policies, Usage, Routing, and Settings & Diagnostics. The current page
  has a visible and programmatic selected state.
- On desktop, use a compact side navigation and a readable main column; tables may
  use the available width. At narrow widths, navigation becomes a labeled menu,
  data tables become stacked labeled records or scroll regions, and actions remain
  reachable without horizontal page scrolling.
- Use semantic headings, landmarks, labels, table headers, and buttons; associate
  errors with fields and announce async results/statuses. Focus is visible and
  moves to a dialog heading on open, stays within a modal, and returns to its
  trigger on close. Every workflow is operable by Tab/Shift+Tab, Enter/Space,
  Escape for dismissible dialogs, and arrow keys only for native/menu widgets that
  conventionally use them. Do not encode status by color alone.
- Support both light and dark themes with accessible contrast in each, as required
  by [M12-WEBUI](M12-WEBUI.md); preserve visible focus and status distinctions in
  both themes.
- Admin bearer tokens exist in memory only. Logout clears token and sensitive
  cached views. Newly issued virtual-key secrets appear once in a dismissible
  reveal/copy step, are never stored/logged, and disappear on navigation/logout.
  Credential input is write-only; never fetch or echo its current value.
- Confirm destructive revocation with the target and consequences; ordinary reads
  and reversible navigation need no confirmation. Keep filters and selected page
  across refreshes. Show timestamps/time zone and distinguish unavailable/null
  values from zero. Never display raw provider payloads, callbacks, credentials,
  or secret-bearing error text.

## Overview

**Layout:** Page title and gateway readiness summary; compact counts for enabled
accounts and recent local requests; recent request/error list with links into Usage;
component and configuration status. Label ledger-derived counts and their time
window. Do not show provider quota or live provider health unless M11 explicitly
provides such evidence (the current scope does not).

**Walkthrough:** Sign in if required → inspect readiness and time window → open a
recent failed request in Usage or an account in Connections. A zero count means
known zero; missing/unavailable counts are shown as unknown. Refresh is explicit
or bounded management polling only, never provider polling.

## Connections / Accounts

**Layout:** Account table/list with stable identifier, connector, enabled state,
declared model/capability information, credential expiry metadata, and revision.
Detail/create form separates non-secret account configuration from a write-only
credential replacement field. Auth actions are shown only when the API advertises
them; show safe user actions without exposing callback contents.

**Walkthrough:** Create account → enter supported fields/credential → review and
submit once → show result and returned metadata, not credential → optionally use
an advertised auth start/continue action → disable or replace credential from
account detail. Replacing/creating a credential is an explicit write, not a test
connection. Do not promise routing changes take effect without restart.

## Keys / Policies

**Layout:** Virtual-key list with label/id, status, policy reference/revision,
limits and created/revoked metadata; policy detail exposes restrictions in plain
language (models, connector targets, limits) and revision. Creation form captures
only supported restrictions and clearly states immutable/CAS behavior if binding
requires a new policy revision.

**Walkthrough:** Create restricted key → review restrictions → submit → show raw
key exactly once with copy control and explicit warning → require operator copy or
acknowledgment before dismissal. Never offer later secret retrieval. Revoke only
after an explicit confirmation naming the key; then refresh status. On a revision
conflict, preserve entered values and offer reload/review; never silently
overwrite. Unsupported policy editing is read-only or absent, not simulated.

## Usage

**Layout:** Bounded time range and account/model filters; paginated request rows
with outcome, timestamp, route, nullable usage, and attempt count; expandable
request details show attempts, selected account/route, errors, and fallback outcome
when available. Clearly identify ledger data and distinguish unknown usage from
zero. No body/prompt inspector.

**Walkthrough:** Choose time/filter → inspect requests → open a request → compare
attempt outcomes and fallback decision → return without losing filters/page.
Changing filters resets pagination. Empty results state the active filters; partial
data identifies which fields are unavailable. Do not synthesize totals from
missing counters.

## Routing

**Layout:** Read-only configured routes grouped by protocol/model alias and target
account/connector; list declared capability constraints and whether their status
is supported, unsupported, or unknown. Include a persistent notice that topology
is configuration-backed and changes currently require configuration/restart.

**Walkthrough:** Select a route → inspect target/capability constraints → follow
account links to Connections or related request links to Usage. Unknown capability
must not be rendered as supported. No route mutation, inferred fallback, or
provider probe is offered.

## Settings / Diagnostics

**Layout:** Version, readiness/components, configured management/inference
endpoints and non-secret settings; safe diagnostic messages with request/correlation
identifier where available. Separate authentication/session controls from runtime
configuration. Provide explicit logout and cache clearing.

**Walkthrough:** Review version/status → copy a safe diagnostic identifier →
correct local configuration using documented external steps if needed → logout.
Never render operator tokens, provider credentials, raw auth callbacks, prompt
payloads, arbitrary environment values, or secret-bearing server errors. Settings
that require restart are labeled and cannot be presented as hot-reloaded.

## State and Permission Matrix

Apply the screen-specific response below to the shared states. All screens retain
navigation and render an actionable, non-secret message; a failed subresource
must not erase successfully loaded unrelated data.

| Screen | Loading | Empty | Disconnected | Expired auth | Denied | Conflict | Partial data | Failed action |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Overview | Skeleton summary/list; announce loading | Explain no recent ledger activity; link setup | Explain management API unavailable; retry | Sign-in prompt; clear session data | Explain operator permission/access denial | Refresh affected summary; preserve page | Mark unavailable counters/components individually | Inline retry for refresh; keep prior safe snapshot marked stale |
| Connections / Accounts | Table/form loading state | Explain no accounts; create only supported type | Preserve list as stale; no writes claimed | Sign in; discard sensitive cached view | Explain account operation unavailable | Preserve form; reload revision before resubmit | Mark unavailable expiry/capability fields unknown | Preserve safe inputs; never echo credential; report whether write outcome is known |
| Keys / Policies | List/detail loading | Explain no keys/policies; offer supported creation | No create/revoke while disconnected | Clear secret/restricted cache and sign in | Explain denied operation without policy internals | Keep edits; compare latest revision before retry | Mark unavailable limits/restrictions unknown | For create, disclose ambiguous outcome and do not blindly resubmit; for revoke, refresh status |
| Usage | Loading rows and totals separately | State no records for selected range | Mark unavailable, retain filters | Sign in and clear sensitive cached views | Explain usage access denied | Refresh page/filter metadata | Show available rows and mark missing values unknown | Keep last results as stale; retry read without losing filters |
| Routing | Load routes and capability data | Explain no configured routes | Show stale config only if previously loaded, clearly marked | Sign in; do not retain sensitive cache | Explain topology unavailable | Not applicable/read-only; reject mutation affordance | Mark individual route/capability unknown | Retry read; no configuration write offered |
| Settings / Diagnostics | Load version/status sections | Explain optional diagnostic section unavailable | Distinguish gateway/API offline from UI failure | Return to sign-in and clear session | Show safe access-denied reason | Not applicable/read-only | Keep available sections; label missing fields | Preserve safe diagnostic context; offer retry/logout |

`Disconnected` means the management service cannot be reached, distinct from a
gateway component reporting not-ready. `Expired auth` clears the in-memory token
and sensitive cached data. `Denied` is an authorization response, not a generic
network error. `Conflict` is a revision/CAS failure, never an invitation to
overwrite. After a mutation timeout with unknown server outcome, explain the
ambiguity and require a fresh read before any retry that could duplicate a write.

## M11 API Dependencies and Mock Adapter

M11 owns the authenticated management listener, exact DTOs/paths/errors and the
static mount seam. M11-001's binding and OpenAPI are authoritative once accepted;
this inventory is a frontend capability checklist, not a competing contract.

| UI need | M11 surface / required behavior |
| --- | --- |
| Sign-in/session and logout | Bearer-authenticated management requests; token kept in memory. Listener/session setup details and origin/host failures follow accepted binding. |
| Overview and diagnostics | Status/version/readiness and effective topology, redacted and without provider probes. |
| Accounts | List/create/update/disable, revisions and expiry metadata; credential replacement write-only; auth lifecycle only through existing coordinator and advertised safe actions. |
| Keys and policies | Create/list/revoke, policy restrictions/revisions and CAS conflict; raw key only in create response. |
| Usage | Bounded request/attempt and aggregate reads, explicit time/filter/pagination semantics, nullable token counters, fallback/outcome detail where represented. |
| Routing | Read configured topology and capability tri-state; configuration changes require restart and remain read-only. |

Before M11-007 integration the UI uses an adapter against local view models. It
must expose the same UI operations and distinguish success, validation/denial,
revision conflict, unavailable/partial data, and unknown mutation outcome; fixtures
must include null counters and supported/unsupported/unknown capabilities. Fixture
mode has a persistent visible **DEMO DATA — NOT LIVE** label. Do not make mock
routes/DTOs look like a server binding, embed provider knowledge in the client,
store secrets, or fake unavailable M11 behavior. When the binding is accepted,
translate its DTOs at the isolated API-client boundary and remove fixture-only
claims; real integration waits for M11-007 on main.

## Asset Embedding and Fallback

M11 provides the static mount seam; M12 owns its UI asset package and build
sequence. Keep at least one small, checked-in fallback asset under the Go package
embed pattern so `go test ./...`, CLI/server builds, and source checkouts do not
depend on Node or prior generated files. A minimal `embed.FS` can include that
always-present fallback; production packaging runs the pinned frontend build and
places reproducible built assets in the package's embed directory before Go
release compilation. Do not require an absent generated directory in a Go embed
pattern, and do not silently serve the fallback as a production UI: identify it
as unavailable/not a built bundle (or return the mount seam's explicit unavailable
response). Runtime must never launch Node. The build/check sequence must test the
Go-only fallback path independently from the production asset build, and the
release path must verify that a real built bundle, not the fallback, is embedded.

Static SPA fallback must not swallow management API errors, inference paths, or
non-asset requests; asset absence is explicit. M12-006 verifies the packaged
bundle and Go-only tests without frontend artifacts.
