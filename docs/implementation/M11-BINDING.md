# M11 Administrative HTTP Binding

This is the normative HTTP binding for the additive management API accepted by
[DEC-021](../project/DECISIONS.md#dec-021--plan-a-local-administration-api-and-webui).
The machine-readable counterpart is [admin.openapi.yaml](../api/admin.openapi.yaml).
It does not change inference, Core, connector, or current CLI behavior.

## Listener and operator authentication

The admin listener is separately configured and opt-in; it is never mounted on
the inference listener. Its default bind is numeric loopback (`127.0.0.1` or
`[::1]`); a non-loopback bind requires TLS. A trusted TLS-terminating reverse
proxy can be supported only when explicitly configured; the initial listener
does not define such a proxy mode. No default credential is created. One
operator bearer token is provisioned out-of-band via a permission-
restricted token file or environment reference. It is independent of virtual
inference keys. All `/admin/v1` API requests require
`Authorization: Bearer <token>` over TLS (loopback may use HTTP); token
comparison is constant-time. Tokens are never returned, logged, placed in URLs,
cookies, or persisted by the browser. Rotate by replacing the configured token
and restarting the listener.
The configured token and bearer credential use RFC 6750 `b64token` syntax:
ASCII letters/digits and `-._~+/`, followed only by optional `=` padding.

Validate `Host` against the listener's configured host/address (and explicitly
configured reverse-proxy hostnames); reject unknown hosts to prevent DNS
rebinding. For browser requests, accept same-origin only, or an explicitly
configured origin allowlist; reject `Origin: null` and unconfigured origins.
Never emit wildcard credentialed CORS. A browser-origin state-changing request
must include bearer authorization and pass the same Origin validation. Non-
browser clients with no Origin still require bearer auth. API errors and logs
must not echo credentials, callback payloads, or secret input. Bound body sizes
and request/read/write/idle timeouts. Return generic server errors without
storage/provider details.

Static assets are a distinct, optional mount seam at `/admin/` for M12. Assets
may be served without bearer auth and contain no embedded credentials; they do
not expose API routes anonymously. The UI keeps the operator token in memory,
clears it on logout, and never uses localStorage, cookies, or query parameters.
Unknown asset paths do not fall through to the API.

## Wire conventions and errors

JSON uses `application/json`, UTF-8, snake_case names, RFC3339 UTC timestamps,
and explicit `null` for unavailable optional counters/times. Unknown request
fields are rejected. Maximum JSON request body is 64 KiB, except credential
create/replace (up to 64 KiB secret plus JSON envelope overhead) and auth
callback input (4 KiB). Collection responses use `{ "items": [...], "limit": n,
"offset": n, "total": n }`; default limit 50, maximum 500, offset >= 0.
Accounts and keys order by `created_at ASC, id ASC`; policies order by `id ASC`.
Requests order by `accepted_at ASC, id ASC`; attempts order by effective event
time `COALESCE(dispatched_at, finished_at, request.accepted_at) ASC, id ASC`.
Time range bounds are inclusive, and `since <= until`.

Errors use `{ "error": { "code": string, "message": string,
"request_id": string } }`. Codes are stable machine identifiers; messages are
safe and contain no secret/provider payload. Use 400 for malformed input, 401
for absent/invalid operator token, 403 for disallowed host/origin or operation,
404 for absent resources, 409 for revision/state conflicts or duplicate IDs,
413 for oversized input, 415 for unsupported media type, 429 for bounded
admission/rate rejection, and 500 for sanitized internal failures. No stack,
SQL, connector response body, or opaque auth state is returned.

Writes are atomic at the store boundary. Revisioned updates require
`expected_revision`; stale revisions return 409 `revision_conflict` and do not
partially apply. Account enable/disable is not revisioned by the existing store.
Policy versions are immutable: update creates a new revision
using the expected current revision. Virtual-key policy reassignment requires
the expected key revision and an existing policy revision. Resource IDs are
opaque strings; secrets are not identifiers.

## DTO and surface contract

### Status and effective topology

`GET /admin/v1/status` returns gateway version, readiness, component health and
declared capabilities, plus the effective configured routes. It performs no
provider requests. Topology is a read-only startup snapshot: components expose
only IDs, kind, implementation/version, health and declared protocol/capability
values; routes expose protocol, mode, model alias, adapter/connector/account
identity and supported route metadata. Remove credential references, endpoints,
private paths and secret-bearing configuration. Unknown readiness remains
`unknown`; do not infer readiness from one component or probe upstreams.

### Accounts and credentials

Accounts contain `id`, `connector`, `enabled`, `created_at`, and
`updated_at`. List filters are connector and enabled; create takes ID, connector
and optional enabled; get and enable/disable are supported. No arbitrary
connector configuration is writable through this API. Credentials are
write-only: create or replace submits `secret` and optional expiry; responses
contain only ID, account ID, revision, expiry and update time. There is no
credential read/list/delete route. Replacement requires `expected_revision`;
creation does not. Secret bytes must be encrypted through the existing
credential store and cleared from transient buffers. No credential is decrypted
for an API response.

### Virtual keys and policies

Policies expose ID, immutable revision, enabled, exact model and connector
allowlists, RPM/TPM limits and creation time. List/get supports enabled filter
and optional revision. Create makes revision 1; update requires
`expected_revision`, carries forward omitted values, and appends a revision.
The existing CLI's update operation cannot re-enable a disabled policy; the
HTTP API likewise exposes disable-only updates. Virtual keys expose internal ID, public key ID, policy ID
and revision, key revision, enabled/revoked state, and timestamps—never the
secret or digest. Create pins a selected policy revision and returns the raw
`secret` exactly once in that response. List/get/revoke and policy reassignment
are supported; reassignment requires `expected_revision` and explicit target
policy revision. Revoke is idempotent. There is no re-enable endpoint in the
existing CLI or this API; storage-layer capabilities are not the limit.

### Usage

`GET /admin/v1/usage/requests` supports key ID, account ID, inclusive since/
until, limit and offset. Each request includes IDs, protocol/model/route,
policy/key revision, state, accepted/finished times and bounded attempts.
`GET /admin/v1/usage/attempts` supports request ID, account ID and the same
time/page filters; it returns attempt state, ordinal, account/connector/route,
commit flag, estimate, error category/reason, timestamps, usage and reservation
summary. `GET /admin/v1/usage/summary` supports key/account/time filters but no
pagination and returns request/attempt counts, estimated/effective charges and
nullable actual/input/output/reasoning/cached token totals. A missing usage row
or unknown token count is `null`, never zero. Ledger records contain no client
payload or prompt.

### Authentication lifecycle

`POST /admin/v1/auth/sessions` starts an existing coordinator flow for an
account. `POST /admin/v1/auth/sessions/{session_id}/continue` advances the
opaque handle and accepts only bounded transient `callback_input`; refresh is
`POST /admin/v1/auth/accounts/{account_id}/refresh`. Responses expose only
status, opaque session handle, expiry and sanitized `user_action` fields
(verification URI, user code, polling interval); never connector state or
credentials. Handles are durable, expiring, account-bound one-time claims under
DEC-007; stale, replayed, expired, disabled-account or revision-conflicting
continuations fail closed. Refresh and continuation each invoke the existing
coordinator once; no API polling loop or retry is added. Unsupported/unconfigured
auth remains unavailable. Callback input is transient and is neither persisted
nor logged. No provider-specific path is introduced.

## Effect and restart semantics

| Operation | Effect |
| --- | --- |
| Account create/enable/disable | SQLite mutation is immediate; enabled state is consulted by existing account/auth operations. Does not reload routes or connector configuration. |
| Credential create/replace | Encrypted SQLite mutation is immediate; future scoped credential resolution sees the new revision. In-flight work is not rewritten. |
| Policy create/update | New immutable policy revision is immediate in SQLite; existing virtual keys remain pinned until explicitly reassigned. |
| Key create/reassign/revoke | SQLite mutation is immediate for subsequent admission; no inference listener restart. |
| Usage reads | Read-only ledger snapshot; no runtime effect. |
| Auth start/continue/refresh | Existing coordinator/store effects are immediate and durable as specified by DEC-007/009/010; no hidden retry. |
| Listener/token/TLS/Host/origin/static mount configuration | Startup-only; restart required. |
| Effective routes/components/configuration | Read-only in M11; edit protected configuration and restart to apply. |

No API call reloads routing, connector instances, or listener configuration.
Existing M3 storage validation, encryption, accounting, and auth replay rules
remain authoritative. This binding makes no live-provider support claim.
