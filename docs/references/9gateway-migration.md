# Source

Repository: local `.refs/9Gateway` (standalone policy proxy in front of 9router).
Revision/commit: `704d456b8da0434ce408d8f4873201f17c5b6f58`.
Date: 2026-09-28 (research date; source commit date 2026-09-19).

Source citations below are relative to `.refs/9Gateway/`; this untracked local checkout is needed to follow them. Project license: **UNKNOWN** (no root license file found; references to Bifrost's license do not license this repository). Tests were located, not run.

# Purpose

Extract gateway policy, access and accounting experience for PestiRoute Core without adopting the old `Client → 9Gateway → 9router → Provider` deployment (`README.md:3-12`). See [PestiRoute's runtime contract](../implementation/CONTRACT.md#usage-and-scoped-runtime-services).

# Existing capabilities

## Research status

**KNOWN** — the cited source paths cover virtual keys, policies, limits, accounting, persistence and lifecycle. This is a code-level finding, not a passing-test claim.

## Migration status

**READY** — the documented key, admission, settlement and lifecycle behavior can inform Core planning. **NEEDS_VALIDATION** — source tests, crash/restart guarantees, multi-instance semantics and code-reuse rights. 9router coupling and gateway-side protocol parsing are **NOT_APPLICABLE** to migration.

## PestiRoute implementation status

**NOT_STARTED** — no Core implementation follows from this source map.

## Live verification status

**UNKNOWN** — neither the source test suite nor a live 9router integration was run for this research.

| Concern | Evidence in 9Gateway | Finding |
| --- | --- | --- |
| Virtual/API keys | `internal/auth/key.go:14-26,80-101`; `internal/auth/snapshot.go:75-89,175-224`; `internal/storage/migrations/001_api_keys.sql:1-14` | One-time random `sk-gw-` key issuance; stores prefix and peppered HMAC digest, not raw key; snapshot lookup verifies digest, enabled status and expiry. Admin create/update publishes a policy snapshot after persistence (`internal/httpserver/admin.go:258-455`). |
| RPM / TPM / concurrency / budgets | `internal/auth/policy.go:71-111,199-269`; `internal/limiter/request.go:20-29,58-131`; `internal/limiter/lease.go:140-152,224-255` | Per-key fixed request windows (RPM-like), token windows (TPM-like), model allow/deny, concurrency and total/day/month cost budgets. Multi-resource preflight reserves a lease and rolls back on rejection; request rejection may include `Retry-After` (`internal/httpserver/server.go:675-775`). These are configured windows, not proof of universal 60-second RPM/TPM semantics. |
| Usage tracking | `internal/httpserver/server.go:1027-1084`; `internal/httpserver/usage_observation.go:28-33,333-445,468-496` | Estimated/fallback token admission; bounded asynchronous JSON/SSE observation can reconcile token and cost reservations. Unsupported, malformed, oversized or dropped observations retain a conservative charge. History preserves nullable unknown usage/cost (`internal/storage/migrations/007_requests.sql:82-99`). |
| Storage | `internal/storage/storage.go:40-48,124-172,226-279`; `internal/storage/usage_accumulator.go:24-25,67-99`; `internal/storage/budget_accumulator.go:14-16,51-85` | Transactional embedded SQLite migrations for keys, limiter aggregates, requests and optional bodies; committed usage/budget deltas are asynchronously accumulated. Request windows are process-local; committed token/budget buckets (not active reservations) are restored on startup (`cmd/gateway/main.go:115-200`). |
| Lifecycle / transport | `cmd/gateway/main.go:48-79,202-256,386-478`; `internal/httpserver/authentication.go:34-64`; `internal/httpserver/server.go:379-395,819-939` | Startup opens/restores storage and workers; shutdown drains requests/workers before closing storage. Public `/v1/*` authenticates gateway bearer keys then proxies to a single configured upstream URL with substituted upstream bearer credentials. Transport has specific Chat `stream:false` SSE-to-JSON compatibility (`docs/architecture/transport.md:62-82`). |

Local tests cover auth rejection/cancellation (`internal/httpserver/authentication_test.go`), token/budget admission and SSE reconciliation (`internal/httpserver/t091_token_admission_test.go`, `internal/httpserver/t110_budget_preflight_test.go`, `internal/httpserver/t113_transparent_sse_cost_test.go`), persisted accounting (`internal/httpserver/t100_integration_test.go`) and streaming/EOF (`internal/httpserver/server_test.go`). Their results at this revision are **UNKNOWN**.

# Reusable knowledge

**REUSE as Core design evidence:** key hashing and one-time disclosure, immutable per-key policy publication, atomic resource admission/rollback, conservative settlement with unknown usage, durable aggregate recovery and explicit shutdown ownership. Keep accounting off the stream's critical delivery path. These are behavioral lessons, not drop-in implementations.

# Not migrated

**DO NOT MIGRATE:** single upstream 9router URL, bearer replacement as the complete backend credential model, or 9router-specific deployment (`README.md:3-12`; `internal/httpserver/server.go:379-395,819-837`). Do not bring protocol-specific request/SSE parsing, Chat SSE aggregation, price extraction or monolithic policy/transport orchestration into Core (`internal/httpserver/server.go:622-775,926-939`; `internal/httpserver/usage_observation.go:333-445`). Do not assume process-local counters or asynchronously persisted deltas satisfy future multi-process/durability requirements without separate validation.

# Target mapping

| PestiRoute layer | Mapping, not an implementation claim |
| --- | --- |
| Protocol Adapter | Own client bearer-key extraction and client-protocol decoding/encoding, including any explicitly scoped framing compatibility; pass identity for Core authorization. |
| Core Runtime | Own key verification, policies, limits, account-scoped credentials, storage, admission/reconciliation, history and lifecycle; consume Connector-reported usage or explicit estimates, not upstream bytes. |
| Connector | Own provider transport, provider auth, stream processing, token estimation and usage extraction, with scoped credentials supplied by Core. 9Gateway has no reusable provider Connector here. |

# Unknowns

- 9Gateway's project license and rights to reuse code; tests and live integration results at this revision.
- Whether per-process request counters and async persistence meet PestiRoute's desired restart/multi-instance guarantees; crash-loss envelope for queued deltas.
- Actual supported usage formats and price accuracy across provider/client versions; the source's bounded observer cannot establish `usage.exact` for every execution.
