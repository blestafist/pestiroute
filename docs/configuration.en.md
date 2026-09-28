# Data and Configuration

## State Separation

YAML describes the desired topology: listener, connector instances, routes, and policy defaults. SQLite stores mutable state: accounts, credentials, virtual keys, auth sessions, attempts, and usage. Secrets in YAML are replaced with credential references. On startup, configuration validation must identify missing references and incompatible protocol/mode combinations before the first client request.

Hot reload is not required for the initial MVP. Configuration is applied in full at startup; administrative operations on keys and accounts use storage and runtime services. The initial management interface is local CLI commands; a separate admin API may be added later.

## Proposed YAML

This is the target schema for M3. Values in `settings` belong to the connector and are validated by it; Core does not interpret `base_url` or upstream protocol. Credentials and accounts from the example are pre-created through the administrative interface.

```yaml
version: 1

server:
  listen: "127.0.0.1:8080"
  max_request_bytes: 16777216

storage:
  driver: sqlite
  path: ./data/gateway.db

secrets:
  master_key_file: ./secrets/master.key

connectors:
  - id: primary
    implementation: openai-compatible
    settings:
      base_url: https://backend.example/v1
      upstream_protocol: openai.responses/v1
      mode: native

routes:
  - id: default-model
    match:
      protocol: openai.responses/v1
      model: model-a
    requirements: [streaming, tools]
    targets:
      - connector: primary
        account: primary-account
    retry:
      max_attempts: 1

policies:
  default:
    rpm: 60
    tpm: 100000
    unknown_usage: reject
```

Model names in a native route are passed without substitution. Route requirements are explicit operator requirements, not automatic inference that every request uses tools. Limit values are provided as examples, not as recommended limits for any specific provider.

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

The admin interface is separated from public inference endpoints. In the initial phase, a CLI with access to the local data directory is sufficient; key creation and revocation operations are logged without exposing secrets to the general log.

## Limits and Reconciliation

Admission control consists of atomic RPM checks and token budget reservation. In the MVP, RPM is counted by accepted client requests; upstream attempts are tracked separately so that fallback does not hide the actual load. TPM is counted by input + output, without double-counting cached/reasoning detail counters.

Token estimation does not guarantee a hard upper bound on upstream consumption. If the backend supports execution budget, the connector applies it according to the selected mode; native passthrough does not allow silently adding restrictions to the request body. After completion, the reservation is replaced with actual usage, and overages reduce the available budget for subsequent requests.

When estimates are unknown, explicit policies are required: `reject` or a fixed conservative reservation. Silent reservation of zero is prohibited. The exact conservative budget value is set by the operator for each route, not guessed by Core.

Reconciliation is idempotent based on attempt ID. If the gateway restarts with active reservations, startup recovery marks attempts as interrupted, preserves conservative charges, and does not present unknown upstream results as canceled executions. Refresh operations and limit checks do not keep SQL transactions open during network calls.
