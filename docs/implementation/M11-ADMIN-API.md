# M11 — Administrative HTTP API

## Outcome

A small authenticated management API for the local gateway, reusing the existing
CLI's storage/runtime semantics. Branch `dev-m11-admin-api`; first M11-001.
This is planned work; main currently has an administrative CLI, not this API.
[DEC-021](../project/DECISIONS.md#dec-021--plan-a-local-administration-api-and-webui)
accepts the scope. Freeze the HTTP binding in M11-001 before handlers or a real
frontend client depend on it.

## Binding to settle first

Write `M11-BINDING.md` plus a machine-readable OpenAPI description under
`docs/api/`. Define concrete DTOs, methods/paths, nullable values, pagination,
filters, errors and revision preconditions for the following bounded surface:

| Surface | Result and exclusions |
| --- | --- |
| Status and effective topology | Version/readiness, components, declared capabilities and configured routes; redact secrets; no provider requests |
| Accounts and credential metadata | List/create/update/disable accounts; write-only credential replacement; identifiers/revisions/expiry, never decrypt-and-return |
| Virtual keys and policies | Create/list/revoke, immutable policy revisions/CAS using existing stores; raw key revealed only in create response |
| Usage | Bounded request/attempt lists and aggregate queries from the ledger; nullable token counters and explicit time/filter semantics |
| Auth lifecycle | Existing start/continue/refresh coordinator; opaque session handle and safe UserAction, transient callback input; no new provider-specific endpoint |

No live quota scraping, billing estimates, raw payloads, arbitrary SQL, shelling
out to the CLI, direct browser SQLite access, new secret store, or Core protocol
types. Unsupported/unconfigured connector auth stays unavailable; M8 remains
synthetic and gated. Credential submission is an explicit write, not a test call.

## Listener and browser authentication baseline

Separate opt-in management listener, loopback by default. Never expose admin
handlers on the public inference mux by accident. One operator token loaded
from a file/environment reference, no default token and no inference-key reuse.
Require bearer auth for management requests; static UI assets may be public but
contain no credentials. The browser holds the operator token in memory only
and clears it on logout; no query-string, localStorage or cookie persistence.

Bind exact Host/Origin rules in M11-001; deny unconfigured cross-origin access,
DNS rebinding and browser-origin writes without the authenticated header. Limit
bodies/timeouts and sanitize errors/logs. External binding requires an explicit
operator setting and documented TLS/reverse-proxy configuration. Keep this a
single-operator local UI: RBAC, enterprise SSO and multi-tenant sessions are out
of scope. Review whether the existing CLI/store helpers need extraction into a
small shared management package; do not create a generic control framework.

## Runtime changes and concurrency

Reuse existing account/key/policy/credential operations and their validation.
Document which writes take effect immediately and which require restart;
never claim a storage update reloads routing. Route configuration is read-only
in this milestone. OAuth continuations retain durable one-time claims and
account binding. Existing error/replay safety and accounting rules are unchanged.
If any semantic Core contract change is necessary, stop that slice for an ADR.

## Ownership and verification

M11 owns new management Go package(s), `docs/api/**`, `M11-*`, its task cards and
single-writer composition/config changes in `cmd/gateway/{main,yaml_config,protected_startup}.go`.
Other lanes must arrange sequential changes there. Provide a narrow static asset
mount seam for M12 without making Go tests depend on Node or generated files.
Do not commit generated build output containing source-map secrets.

Use local `httptest` plus file-backed SQLite to compare API behavior with CLI
semantics. Verify auth/origin/host denials, redaction, one-time keys, CAS conflicts,
revocation, request bounds, nullable usage and auth session replay. Cover existing
Responses and Chat regressions and run hosted full CI. M11-007 publishes the
binding compatibility and frontend integration handoff. M12's mock UI does not
wait for API implementation; real integration waits for this handoff on main.
