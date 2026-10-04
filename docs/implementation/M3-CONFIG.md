# M3 Protected Configuration and Retry Admission

This describes implemented protected startup and retry admission. M3-020
records loader evidence; M3-037–M3-041 record candidate/fallback evidence. The accepted
[v1 contract](CONTRACT.md) and the M3 persistence and runtime notes
([storage](M3-STORAGE.md), [runtime](M3-RUNTIME.md)) remain authoritative.

## Protected YAML schema, version 1

Protected startup selects this format explicitly (for example, by the
operator-provided config path/format); it is never inferred by falling back
from a failed protected load to a development JSON mode. YAML is a single
document, UTF-8, bounded by the loader's documented size limit, and parsed
with duplicate-key detection. Reject unknown keys at every level, duplicate
keys, explicit nulls, aliases, anchors, merge keys, multiple documents,
unsupported versions, and values of the wrong type. Do not coerce strings to
numbers, numbers to strings, or YAML booleans/nulls to strings. Environment
expansion and implicit secret interpolation are not part of this schema.

All six named top-level sections and the `version` key are required and have
exactly these shapes:

| Key | Type and required fields |
| --- | --- |
| `version` | Integer, exactly `1`. |
| `server` | Mapping: `listen` non-empty TCP address string; `max_request_bytes` positive integer; `shutdown_timeout` positive Go duration string. |
| `storage` | Mapping: `driver` string, exactly `sqlite`; `path` non-empty path string to a local SQLite database. |
| `secrets` | Mapping: `master_key_file` non-empty path string to the externally provisioned key file. |
| `connectors` | Non-empty sequence of connector mappings described below. |
| `routes` | Non-empty sequence of route mappings described below. |
| `policies` | Non-empty mapping from non-empty local policy-reference names to non-empty SQLite key-policy IDs. These names are configuration references, not policy definitions or credentials. |

The table has seven rows because `version` is a scalar in addition to the six
named sections. No other top-level key is allowed.

### Connector entries

Each connector mapping has exactly:

| Field | Type and constraint |
| --- | --- |
| `id` | Non-empty unique instance ID. |
| `kind` | String, exactly `connector`. |
| `implementation` | String; `pestiroute.responses.native` or `pestiroute.anthropic.messages`. |
| `protocols` | Non-empty sequence of unique declared protocol strings; currently exactly `openai.responses.v1` for either supported implementation. |
| `settings` | Mapping owned and validated by the selected Connector implementation. Native Responses accepts `base_url`, `upstream_protocol`, `mode`, `credential_env`, positive request byte limits, and positive transport durations as detailed below. Anthropic Messages accepts non-empty `model` (configured client-side route model), `account_id` (selected SQLite account ID), and `credential_id` (selected SQLite credential record ID scoped to that account); credential values remain in protected SQLite and are never YAML fields. Reject settings belonging to another implementation or unknown fields. |

Connector-specific endpoint, authentication, transport, and size settings
stay inside `settings`; Core does not interpret URLs, provider protocols,
credentials, or transport behavior. Core validates only generic component
identity/kind and declared protocol compatibility. A future implementation
may define its own settings schema; it does not gain support merely by being
named in YAML. Adapter identity is the implemented
`pestiroute.responses.native`; route protocol/mode spellings are
`openai.responses.v1` and `native` for native routes or `translation` for
Anthropic Messages routes (not the draft example's `openai.responses/v1`).
Unsupported implementations and modes fail startup.

### Route entries

Each route mapping has exactly:

| Field | Type and constraint |
| --- | --- |
| `id` | Non-empty unique route ID. |
| `protocol` | String, `openai.responses.v1`. |
| `mode` | String, `native` for native routes or `translation` for Anthropic Messages routes. |
| `model` | Non-empty exact model ID; native mode does not substitute aliases. |
| `adapter` | Adapter implementation identifier; initially exactly `pestiroute.responses.native`, resolved by the composition root to the built-in Adapter. |
| `policy` | Name resolving through top-level `policies` to an existing SQLite key-policy ID. This controls principal authorization and RPM/TPM limits; it is not the route's estimate budget. |
| `budget` | Required route-owned estimate budget mapping described below; not stored in or inherited from the key policy. |
| `requirements` | Optional sequence of unique non-empty capability names; only `supported` satisfies a requirement. |
| `targets` | Non-empty ordered sequence of target mappings. |
| `retry` | Optional mapping described below; defaults to one attempt and no fallback. |

The schema has no user-defined Adapter component list: the initial composition
root registers the native Responses Adapter. Thus the route's `adapter` is its
implementation identifier, not an arbitrary Adapter instance ID. Connector
`id` values are the configured instance IDs validated by Core.

Each target has exactly `connector` (registered connector instance ID) and
`account` (stable SQLite account ID). The account must exist, be enabled, and
belong to that connector instance. A missing/stale account, policy, Adapter,
Connector, or incompatible protocol/mode is a startup/admission error; do not
create mutable accounts or policy state from YAML. Policies and accounts are
managed through SQLite/CLI. YAML contains IDs only, never credentials,
plaintext secrets, virtual keys, or mutable policy contents.

`policies` maps a local reference name to an existing persistent key-policy
ID; each route chooses one such reference. The SQLite policy snapshot controls
key authorization and key-scoped RPM/TPM limits at admission. The mapping is
topology, not a cached policy snapshot. On each subsequent request,
key/policy/account state and revisions are read/rechecked for admission; CLI
changes take effect without topology reload. An in-flight request keeps its
admitted immutable policy snapshot, while every fallback candidate must still
satisfy that snapshot and its current enabled-account/route eligibility.

### Per-route estimate budget

Every route has a required `budget` mapping with exactly:

| Field | Type and constraint |
| --- | --- |
| `unknown_estimate` | String, exactly `reject` or `reserve`. |
| `conservative_tokens` | Positive integer, required only when `unknown_estimate: reserve`; forbidden with `reject`. |

This YAML mapping constructs the M3-RUNTIME `RouteBudget` (`UnknownEstimate`
and `ConservativeTokens`). It is owned by the route, not by the SQLite
key-policy referenced by `policy`; there is no implicit policy/default
inheritance. Before admission, Core asks the selected Connector to estimate
outside a storage transaction. For an unsupported, missing, unknown, negative,
or overflowing estimate, `reject` fails pre-admission; `reserve` reserves the
configured positive `conservative_tokens`. Missing or invalid budget values
fail closed; zero is never a reservation. This estimate fallback is distinct
from attempt retry/fallback and from settlement of unknown actual usage.

For a route targeting `pestiroute.anthropic.messages` in `translation` mode,
`unknown_estimate: reserve` additionally requires `conservative_tokens >= 4096`.
This matches the Connector's maximum sent `max_tokens` ceiling (M4-008),
including the omitted-output default; YAML validation enforces the floor while
Core remains opaque to request payload bytes. `reject` budgets keep their
existing shape and are not subject to the reserve floor.

### Retry mapping and candidate limits

`retry` has exactly `max_attempts` (integer >= 1, default `1`) and `deadline`
(positive Go-duration string, required when `max_attempts` is greater than
one; otherwise omitted or positive). The deadline bounds the complete request
and all attempts from initial admission; it cannot be extended on retry.
Attempt count cannot exceed either this bound or the number of distinct
configured targets. Targets are considered once, in YAML order: no cycling,
repeat of an already-attempted target, or implicit target discovery. With
`max_attempts: 1`, retries/fallback are disabled regardless of target count.

Every candidate is separately admitted before execution: it must be an
explicit target on the selected route, authorized by the admitted policy,
backed by an enabled account, and satisfy the request's exact protocol/model
and required capabilities. The candidate Connector's own body and header
limits are applied to the actual admitted body and original ingress headers;
another target's larger limits do not grant permission. The selected
Connector's estimate is obtained outside storage transactions. Each actual
fallback attempt gets a distinct attempt record and token reservation in the
same TPM window; accepted-request RPM is counted once, while dispatched
attempts settle independently. Unknown estimates follow the route budget's
explicit `reject` or positive fixed conservative reservation rule, never
zero. A candidate failing any check is not executed.

Advance only after an uncommitted attempt whose Connector-reported error is
both `Retryable = true` and `RetryDisposition = safe`, while request policy,
attempt/deadline bounds, candidate eligibility, and budget admission still
permit the next target. A safe classified pre-execution rejection may qualify.
An unsafe or unknown delivery outcome (including disconnect after possible
body delivery) terminates without fallback. Any committed `Head` terminates
without fallback, even if later error metadata says retryable. Core uses
contract error metadata; it never parses provider errors, request payloads,
or SSE to decide retry safety. See the [v1 error model](CONTRACT.md#error-model)
and [fallback boundary](../project/ARCHITECTURE.md#fallback-and-retries).

## Stateful and unknown affinity

Adapter-owned trusted request metadata reports session-bound/stateful status
and affinity evidence; Core does not inspect `previous_response_id`, provider
payload bytes, or SSE. A request known to be session-bound, or whose affinity
is unknown, is eligible only when the effective route has exactly one target.
It is never allowed to advance to another account, Connector instance, or
backend. If multiple targets are configured, reject such a request before
dispatch unless routing has already reduced it to one target using verified
affinity evidence. M3 has no such affinity directory, so absent/unknown
evidence cannot reduce the set: fail closed with no execution. A failure on
the singleton target terminates; it cannot cycle or retry that same target.
This preserves the [stateful request boundary](../project/ARCHITECTURE.md#stateful-responses).

## Startup isolation and compatibility

The existing legacy single-target JSON and strict M2 topology JSON remain
development compatibility modes only, and both bind inference exclusively
to numeric loopback (`127.0.0.1` or `::1`). They normalize to the existing
internal M2 runtime configuration: the legacy form becomes its one native
Responses route; M2 topology retains its explicitly declared components and
routes. Neither JSON mode implies SQLite persistence, virtual-key
authentication, or protected M3 guarantees. No hostname such as `localhost`
or non-loopback listener qualifies.

Protected YAML requires SQLite and the external master-key file before the
listener becomes ready. Storage open/migration/recovery, key loading,
policy/account/reference validation, or authentication initialization errors
abort startup and fail readiness. They never trigger legacy/M2 JSON parsing,
unauthenticated development mode, an alternate database, or a silent default
policy. The master key remains external and is not copied into YAML or SQLite;
see [M3-STORAGE key handling](M3-STORAGE.md#state-and-schema).

## Normalization and retry walkthroughs

These concrete inputs trace the three explicit startup paths (port values are
illustrative). The implemented legacy and topology field shapes are documented
in [CONFIGURATION.md](CONFIGURATION.md#implemented-m2-startup-json-development-compatibility) and its
[runnable M2 procedure](LOCAL-M2.md); protected YAML follows the schema above.

```json
{"listen":"127.0.0.1:8080","upstream_endpoint":"https://backend.example/v1/responses","upstream_credential_env":"BACKEND_KEY","max_request_body_bytes":1048576,"max_request_header_bytes":16384,"connect_timeout":"2s","tls_handshake_timeout":"2s","response_header_timeout":"5s","stream_idle_timeout":"30s"}
```

The legacy loader validates each required field and loopback listener, then
normalizes endpoint/credential/limits/timeouts to the built-in native
Connector and one built-in native Adapter route (`openai.responses.v1`,
`native`, `gpt-5.4-mini`). It invents no DB/key/policy state. Non-loopback or
invalid legacy input fails startup, not fallback to another format.

```json
{"listen":"127.0.0.1:8080","shutdown_timeout":"5s","components":[{"id":"adapter","implementation":"pestiroute.responses.native","kind":"adapter"},{"id":"upstream","implementation":"pestiroute.responses.native","kind":"connector","endpoint":"http://127.0.0.1:9000/v1/responses","credential_env":"BACKEND_KEY","max_request_body_bytes":1048576,"max_request_header_bytes":16384,"connect_timeout":"2s","tls_handshake_timeout":"2s","response_header_timeout":"5s","stream_idle_timeout":"30s"}],"routes":[{"protocol":"openai.responses.v1","mode":"native","model":"model-a","account":"account-a","adapter":"adapter","connector":"upstream"}]}
```

The strict M2 loader checks exact fields/component kinds and route references,
then retains only those declared components and routes in its internal
topology. It creates no SQLite entities and adds no M3 auth semantics. Invalid
JSON fails startup.

```yaml
version: 1
server: {listen: "127.0.0.1:8080", max_request_bytes: 1048576, shutdown_timeout: 5s}
storage: {driver: sqlite, path: ./data/gateway.db}
secrets: {master_key_file: ./secrets/master.key}
connectors:
  - id: upstream
    kind: connector
    implementation: pestiroute.responses.native
    protocols: [openai.responses.v1]
    settings:
      base_url: https://backend.example/v1
      upstream_protocol: openai.responses.v1
      mode: native
      credential_env: BACKEND_KEY
      max_request_body_bytes: 1048576
      max_request_header_bytes: 16384
      connect_timeout: 2s
      tls_handshake_timeout: 2s
      response_header_timeout: 5s
      stream_idle_timeout: 30s
routes:
  - id: model-a
    protocol: openai.responses.v1
    mode: native
    model: model-a
    adapter: pestiroute.responses.native
    policy: standard
    budget: {unknown_estimate: reject}
    targets: [{connector: upstream, account: account-a}]
policies: {standard: policy-id-a}
```

The explicit protected loader validates version/schema and reference integrity,
opens required SQLite, loads the external key, and verifies account `account-a`
and policy `policy-id-a` before readiness. It produces topology referencing
mutable entity IDs, not embedded credentials or a policy snapshot. Any failure
aborts startup; it does not retry another config mode.

1. **Legacy JSON:** as above, all required values normalize to one native
   route without persistence/auth state.
2. **M2 topology JSON:** as above, only declared components and route identity
   normalize; account references retain the M2 behavior and do not imply
   persistent M3 auth.
3. **Protected YAML:** as above, references are checked against SQLite and the
   external master key is required before readiness.
4. **Ordered retry:** route targets A then B, `max_attempts: 2`, deadline
   remaining. A returns an explicitly retryable, safe rejection before
   `Head`; after A is finalized and B passes its own enabled-account,
   capability, body/header, estimate and TPM-reservation checks, B may execute
   once. A disconnect with unsafe/unknown disposition terminates; a failure
   after `Head` terminates. Neither case invokes B. B is never attempted if
   request deadline or attempt limit is exhausted.
5. **Stateful/unknown affinity:** the same two-target route receives trusted
   stateful metadata (e.g. adapter identified `previous_response_id`) or
   unknown affinity. With no verified affinity directory, effective target
   set is not provably a singleton; reject before attempt A. If configuration
   itself has one target, A may execute, but any safe failure terminates and
   target B cannot be attempted or synthesized.

These behaviors are implemented and covered by deterministic fixtures. See
[M3-020](tasks/M3-020.md) for loader evidence, [M3-037](tasks/M3-037.md) for
candidate selection, and [M3-046](tasks/M3-046.md) for final audit results.
A policy-excluded primary is skipped before admission; an eligible configured
secondary can be selected even when retry is disabled. The native Connector
leaves ambiguous transport delivery unknown, so it does not automatically retry
such failures.
