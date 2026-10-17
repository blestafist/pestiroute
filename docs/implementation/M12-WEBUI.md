# M12 — WebUI

## Outcome

A complete usable operator UI for accounts, keys, policies, usage and runtime
visibility. Branch `dev-m12-webui`; first M12-001. Build in parallel with M11
against fixtures, then integrate the accepted API. This is a repository product
surface, not a hosted external website or a chat playground.

## UX scope

| Screen | Operator workflow |
| --- | --- |
| Overview | Readiness, enabled accounts, recent request volume and errors; distinguish local ledger usage from unknown provider quota |
| Connections / accounts | See connector/model/capability and credential expiry; create/disable an account, replace a credential, follow supported OAuth actions |
| Keys / policies | Create a restricted virtual key, copy its secret once, inspect policy/limits, revoke; show revision conflict and refresh path |
| Usage | Time/account/model filters, paginated requests, attempt detail and fallback outcome; null is unknown, never zero |
| Routing | Read configured protocol/model/account targets and capability constraints; explain that topology changes currently require configuration/restart |
| Settings / diagnostics | Version, endpoints and non-secret runtime settings; actionable errors without tokens, raw callbacks or prompt payloads |

Use a coherent navigation shell, restrained visual style and readable tables.
Desktop first, usable at narrow widths, keyboard access, labels/focus states,
light/dark themes and accessible contrast. Supply loading, empty, disconnected,
expired-auth, denied, conflict, partial-data and failed-action states. Mutation
confirmation is for destructive product actions such as key revocation; do not
insert confirmation into ordinary reads. Preserve filters/navigation on refetch.
A visible mock/demo label is mandatory when using fixtures; no fake live charts.

## Technical baseline

React + TypeScript + Vite in `web/`, pinned dependencies and committed lockfile.
No Next.js/server runtime, UI backend, CDN dependencies, provider SDKs or direct
provider requests. Keep the API client isolated; bind to M11's accepted OpenAPI
DTOs when available. Before it is integrated, a clearly labeled mock adapter
implements the UI's local view models, not a competing server contract.

One small component/style system, not a framework stack. Prefer built-in fetch
and local state until a concrete need justifies additional dependencies. Keep
admin bearer token in memory only; logout clears it and cached sensitive views.
Raw newly issued keys are transient, explicitly copyable and never logged or
stored. Credential fields are write-only. OAuth URLs/actions come from the API;
callback input never goes into URL/history, analytics or persistent browser state.

## Integration and packaging

M11 owns admin listener/auth and the static mount seam. M12 owns `web/**`, its
asset embedding Go package and dedicated browser tests. Production assets must
ship with the Go distribution (embed after a reproducible frontend build), with
no Node process at runtime. Go-only tests/builds must retain a documented path
without requiring a prior frontend build; choose the minimal embed/fallback
binding during design. No fallback may label a missing UI as a working bundle.
SPA asset routes cannot swallow API errors or inference endpoints. No automatic
polling of real providers; only bounded management API reads.

M12-005 waits for M11-007 on main before real integration; a DONE row in another
worktree is insufficient. M12-006 runs browser E2E against a real local gateway
and SQLite fixture: account/key creation, one-time secret, policy restriction,
request/usage visibility, revocation, logout and startup without UI configuration.
This is the acceptance gate for a working WebUI, not screenshots of mock screens.

## Out of scope

Chat playground, prompt/version management, provider traffic inspector, billing,
marketplace, cloud hosting, multi-user RBAC, and mutable hot-reloaded routing.
If API semantics change, update the shared contract in M11 first; never compensate
with browser-side provider logic or arbitrary commands.
