# M11 administrative API handoff

## Disposition

M11-001–007 deliver the frozen administrative HTTP binding, isolated opt-in
listener, authenticated read/write API, and gateway-composed auth lifecycle.
The API is additive: it does not reload routing or alter the inference API.
M11 makes no live-provider or quota/billing claim. M12 owns `web/**`, the
embedded asset implementation, and browser tests; this handoff exposes only the
optional static-handler seam.

## Startup and authentication

Configure the listener in protected gateway JSON/YAML under `admin`. The actual
configuration requires `listen` (host:port), exactly one of `token_file` or
`token_env`, and positive `read_timeout`, `write_timeout`, and `idle_timeout`.
Optional keys are `allowed_hosts`, `allowed_origins`, `max_request_bytes`
(default 65536), `tls_cert_file`, and `tls_key_file` (configured together).
There are no admin-listener CLI flags, and the config has no
`enabled`, `bind`, or nested `tls.*` fields: omitting `admin` leaves the listener
off. Example YAML:

```yaml
admin:
  listen: 127.0.0.1:9091
  token_file: /run/pestiroute/admin-token
  allowed_hosts: []
  allowed_origins: []
  max_request_bytes: 65536
  read_timeout: 10s
  write_timeout: 30s
  idle_timeout: 60s
```

Use a permission-restricted token file or named environment variable; never
reuse an inference key. Default bind for the admin HTTP handler is numeric
loopback, and every non-loopback bind requires both TLS certificate and key.
There is no reverse-proxy TLS termination mode in this implementation. Tokens
must satisfy RFC 6750 `b64token`; API requests require
`Authorization: Bearer <token>`. Comparison is constant-time. Rotate by
replacing the source and restarting. No admin endpoints are added to the
inference listener.

Requests must use an allowed Host. Browser origins must be same-origin or in
`allowed_origins`; `Origin: null` and unlisted origins are denied. Cross-origin
preflight is limited to the allowed origin and `Authorization, Content-Type`;
credentialed wildcard CORS is not emitted. Static assets are public but contain
no credentials. The browser must keep the bearer token in memory only and clear
it on logout; do not use URLs, cookies, or persistent browser storage.

## Static asset seam

`internal/admin.Config.Static` accepts an optional `http.Handler`. The handler
mounts beneath `/admin/`; with no configured handler paths return 404. Paths
under `/admin/v1` never fall through to static serving. M11 does not embed or
ship UI assets. M12 can supply the handler from its asset package; Go-only
builds and tests must remain independent of Node or generated assets.

## HTTP API reference

All routes below are under `/admin/v1`, require bearer authentication, and
return the binding's `{error:{code,message,request_id}}` envelope on failure.
Malformed/unknown request fields are 400, missing/invalid auth 401, disallowed
Host/Origin 403, missing resource 404, conflicts 409, oversized bodies 413,
wrong media type 415, and sanitized internal failures 500. Unconfigured auth
returns 503. Exact DTOs and parameter bounds are defined in
[`admin.openapi.yaml`](../api/admin.openapi.yaml) and the normative
[`M11-BINDING.md`](M11-BINDING.md).

| Method and path | Request | Success | Notes |
| --- | --- | --- | --- |
| GET `/status` | — | 200 `Status` | Startup topology snapshot; no provider probes |
| GET `/accounts` | connector, enabled, limit, offset | 200 `AccountPage` | |
| POST `/accounts` | `AccountCreate` | 201 `Account` | |
| GET `/accounts/{account_id}` | — | 200 `Account` | |
| PUT `/accounts/{account_id}/enabled` | `EnabledUpdate` | 200 `Account` | |
| POST `/accounts/{account_id}/credentials` | `CredentialCreate` | 201 `Credential` | Secret write-only; metadata only returned |
| PUT `/accounts/{account_id}/credentials/{credential_id}` | `CredentialReplace` | 200 `Credential` | `expected_revision`; 409 on stale revision |
| GET `/policies` | enabled, limit, offset | 200 `PolicyPage` | |
| POST `/policies` | `PolicyWrite` | 201 `Policy` | Creates revision 1 |
| GET `/policies/{policy_id}` | optional revision | 200 `Policy` | Immutable revision |
| PUT `/policies/{policy_id}` | `PolicyUpdate` | 200 `Policy` | `expected_revision`; creates revision; disable-only |
| GET `/keys` | policy_id, enabled, revoked, limit, offset | 200 `KeyPage` | Secrets/digests omitted |
| POST `/keys` | `KeyCreate` | 201 `IssuedKey` | Raw secret returned once |
| GET `/keys/{key_id}` | — | 200 `VirtualKey` | |
| DELETE `/keys/{key_id}` | — | 200 `VirtualKey` | Revoke; idempotent |
| PUT `/keys/{key_id}/policy` | `KeyPolicyUpdate` | 200 `VirtualKey` | Revision-checked reassignment |
| GET `/usage/requests` | key_id, account_id, since, until, limit, offset | 200 `RequestPage` | Inclusive time bounds |
| GET `/usage/attempts` | request_id, account_id, since, until, limit, offset | 200 `AttemptPage` | Inclusive time bounds |
| GET `/usage/summary` | key_id, account_id, since, until | 200 `UsageSummary` | No model filter; unknown token counts are null |
| POST `/auth/sessions` | `AuthStart` | 200 `AuthResult` | Existing generic coordinator |
| POST `/auth/sessions/{session_id}/continue` | `AuthContinue` | 200 `AuthResult` | Callback transient, max 4 KiB |
| POST `/auth/accounts/{account_id}/refresh` | no body | 200 `AuthRefresh` | Existing coordinator, no retry |

## Effects and privacy

Account, credential, policy, key, and auth writes are durable SQLite operations
and affect subsequent operations as specified by the binding; none reloads
routes, connector instances, or listener settings. Listener/token/TLS/Host/
origin configuration is startup-only. Policy history is immutable, credential
secrets are encrypted by the existing store and never read back by HTTP, and
virtual-key secrets appear only in the creation response. Usage reads expose
ledger metadata, never prompts or client payloads. Callback input, bearer
tokens, credential secrets, and key secrets must not enter logs or error
responses.

## M12 integration gate

Consume the OpenAPI DTOs and binding rather than inferring a second contract.
Before browser integration, run the focused package/gateway tests and the full
pinned repository gate from the M11-007 task card. The deterministic gateway
tests use local SQLite/fake dependencies; they do not constitute live-provider
verification. M12 acceptance remains a real local gateway + SQLite browser
E2E covering account/key creation, one-time key display, policy restriction,
usage visibility, revocation, logout, and startup without UI configuration.
