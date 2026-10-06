# Data and Configuration

## Implemented startup formats

Protected YAML startup is implemented; the operational walkthrough is
[LOCAL-M3.md](LOCAL-M3.md). The schema, validation rules, retry behavior and
startup isolation contract are in [M3-CONFIG.md](M3-CONFIG.md). M3 protected
startup uses persistent SQLite entities and an external master-key file.

Legacy single-target and strict topology JSON remain numeric-loopback
development compatibility modes. They do not provide SQLite-backed keys,
policies, or durable accounting; see [LOCAL-M2.md](LOCAL-M2.md).

## Implemented M2 startup JSON (development compatibility)

The JSON format is strict and does not configure protected M3 persistence. See [the local M2 procedure](LOCAL-M2.md) for a runnable loopback example.

An M2 configuration uses `listen` (inference must bind numeric loopback `127.0.0.1` or `::1`), optional positive Go-duration `shutdown_timeout` (default `5s`), and both non-empty arrays `components` and `routes`. These cannot be mixed with legacy `upstream_*`, request-limit, or timeout fields. JSON keys are exact and case-sensitive; unknown keys and any JSON `null` are rejected. The one-MiB configuration limit and complete validation are enforced at startup.

Each component has `id`, `implementation` (`pestiroute.responses.native`), and `kind` (`adapter` or `connector`). IDs must be unique. Adapter components have no connector settings. Connector components additionally require an `endpoint` that is an absolute endpoint whose path ends with `/v1/responses` (HTTPS, or HTTP only for a numeric loopback IP host), `credential_env` naming a set, non-empty environment variable, positive `max_request_body_bytes` and `max_request_header_bytes`, and positive Go durations `connect_timeout`, `tls_handshake_timeout`, `response_header_timeout`, and `stream_idle_timeout`.

Each route requires `protocol` (`openai.responses.v1`), `mode` (`native`), non-empty `model`, `account`, and references to a registered adapter and connector of the matching kinds. Route identities must be unique. Optional `capabilities` maps non-empty capability names to `supported`, `unsupported`, or `unknown`; declarations do not establish provider-wide support. The runtime uses the explicitly configured identity routes; no automatic fallback is configured.

Capability declarations are passed to the selected Connector for its exact protocol/mode/model/account scope. `unsupported` and `unknown` restrict admission even when the historical baseline supports that feature. Numeric-loopback HTTP fixtures can explicitly declare additional supported capabilities; external endpoints retain the established M1 support ceiling, including unknown parallel tools. Adapter support describes the native client format independently of the opaque model ID; both Adapter and Connector must still support every requested capability.

Ingress Decode is bounded by the largest configured request limits so it can identify a route. Before Execute, the gateway checks the selected Connector's body and header limits against the admitted body size and original HTTP headers, including headers stripped from upstream forwarding. Another route's larger limits cannot authorize an oversized request; chunked bodies are checked by their actual admitted size.

The old single-target JSON form remains supported: `upstream_endpoint`, `upstream_credential_env`, positive body/header limits, and all four positive connector timeouts are required together. It normalizes to the built-in Responses adapter/connector and one native `gpt-5.4-mini` route. It must not be mixed with `components` or `routes`. As with M2 topology, inference listens only on numeric loopback and endpoint rules apply. This compatibility form is not the M3 schema.

## State Separation

Protected YAML describes the listener, connector instances, routes, per-route estimate budgets, and references to persistent policies. SQLite stores mutable state: accounts, encrypted credentials, virtual keys, auth sessions, policies, attempts, and usage. Secrets in YAML are replaced with references; startup validates references before serving. [M3-CONFIG](M3-CONFIG.md) owns the schema.

The detailed entity keys, relations, migrations, durable accounting boundary, and crash recovery policy are specified in [M3-STORAGE.md](M3-STORAGE.md).

The Codex Connector's existing `componentConfig.Profile` accepts the standard
`codex-responses-http-sse-v1` profile by default. The opt-in
`codex-responses-http-sse-lite-v1` profile is specified only for configured
model `gpt-6-luna` and the bounded plain-text smoke flow; it does not change the
standard profile or credential configuration. Protected configuration rejects
other models and non-native routes. Lite advertises `llm.reasoning: Supported`
only at its exact native protocol/model/account scope after offline
byte-preservation tests and the reviewed direct evidence; tools/parallel and
standard-profile reasoning remain `Unknown`. No working gateway route is
claimed until a matched pair is reviewed. See [DEC-011](../project/DECISIONS.md#dec-011--opt-in-codex-responses-lite-text-profile) and the [M5.1 binding](M5.1-BINDING.md#opt-in-responses-lite-text-profile-dec-011).
Lite-only originator, honest user-agent, and per-attempt correlation UUIDs are
Connector-generated ephemeral headers, not persisted sessions; client spoofing
is suppressed and the standard profile's header set stays unchanged.

Hot reload is not required for the initial MVP. Configuration is applied in full at startup; administrative operations on keys and accounts use storage and runtime services. The initial management interface is local CLI commands; a separate admin API may be added later.

## Proposed YAML

The versioned YAML schema, route budget, retry policy, entity references, and
startup rules are specified in [M3-CONFIG](M3-CONFIG.md). Its route policy
reference selects the persistent key policy; the route estimate budget is
separate configuration. Run [LOCAL-M3](LOCAL-M3.md) for migration, provisioning,
startup, usage, recovery and backup/restore commands.

Legacy and M2 topology JSON remain
numeric-loopback development compatibility modes; they do not silently
downgrade from failed protected startup. See [M3-CONFIG startup isolation](M3-CONFIG.md#startup-isolation-and-compatibility).

## Core Entities

| Entity | Purpose and Key Fields |
| --- | --- |
| `accounts` | Connector instance, credential reference, enabled state, health/cooldown state |
| `credentials` | Encrypted payload, format version, key version, expiry and revision |
| `virtual_keys` | Public ID, digest, enabled/revoked state, policy and timestamps |
| `key_policies` | Model/connector allowlists, RPM, TPM and additional restrictions |
| `auth_sessions` | Flow ID, account, lifetime, and protected temporary state |
| `requests` | Request ID, virtual key ID, route and final outcome |
| `attempts` | Attempt ID, account, timings, commit state, error and retry reason |
| `usage_records` | Counters, source, completeness, estimate, and attempt association |
| `reservations` | Reserved budget, lifecycle, and reconciliation state |
| `schema_migrations` | Applied schema versions |

The model is designed for a single gateway process. All timestamps are stored in UTC. Request/attempt identifiers enable investigating fallback behavior without storing prompts. Account deletion must not cascade-delete historical usage records.

## Virtual Keys and Access

A virtual key identifies the policy used to verify models and connector targets before execution. Model alias restrictions apply to the client-provided name; connector restrictions are checked for both the initial target and fallback. Unknown or revoked keys are rejected before contacting upstream.

The administrative interface is separated from public inference endpoints. In the initial phase, a CLI with access to the local data directory is sufficient; key creation and revocation operations are logged without exposing secrets to the general log.

## Limits and Reconciliation

Admission control consists of atomic RPM checks and token budget reservation. In the MVP, RPM is counted by accepted client requests; upstream attempts are tracked separately so that fallback does not hide the actual load. TPM is counted by input + output, without double-counting cached/reasoning detail counters.

Token estimation does not guarantee a hard upper bound on upstream consumption. If the backend supports execution budget, the connector applies it according to the selected mode; native passthrough does not allow silently adding restrictions to the request body. After completion, the reservation is replaced with actual usage, and overages reduce the available budget for subsequent requests.

When estimates are unknown, explicit policies are required: `reject` or a fixed conservative reservation. Silent reservation of zero is prohibited. The exact conservative budget value is set by the operator for each route, not guessed by Core.

Reconciliation is idempotent based on attempt ID. If the gateway restarts with active reservations, startup recovery marks attempts as interrupted, preserves conservative charges, and does not present unknown upstream results as canceled executions. Refresh operations and limit checks do not keep SQL transactions open during network calls.
