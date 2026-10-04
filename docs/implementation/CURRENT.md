# Current State

## Verified baseline

M0–M3 are complete. [M3-046](tasks/M3-046.md) records the final gate audit,
review fixes and M4 handoff on source `d36a91a7e0869c286bed27e6eb525af183ad029d`.
The closure commit updates documentation only. Local verification used pinned
Go 1.27.1 on Linux/amd64; hosted PR CI is tracked separately in GitHub.

[CONTRACT.md](CONTRACT.md) remains the accepted v1 semantic boundary.
[M2-BINDING.md](M2-BINDING.md) retains managed components, scoped invocation
services, explicit native identity routes and checked Head/Body/Complete/EOF.
Core has no production imports of concrete Adapters, Connectors or parsers.

Protected version-1 YAML composes SQLite-backed virtual keys/policies/accounts,
AES-256-GCM credentials with an external permission-checked master key, atomic
RPM/TPM admission, per-attempt reservations/usage, durable dispatch intent,
request closure and conservative crash recovery. Persistence failures withdraw
readiness and block new protected requests across routes. Opt-in ordered fallback
requires explicit safe delivery before client commit, fresh candidate eligibility
and bounded attempts/deadline; it admits RPM once and charges each attempt.
Stateful/unknown-affinity requests require a configured singleton target.

Schema v6 adds durable claims for interactive auth continuation to the v5 auth
sessions. Protected startup recovers refresh markers to uncertain and consumes
ambiguous claimed continuations before listen. Account disable atomically
invalidates auth state. Scripted start/continue/refresh, revision CAS and
cross-process claims are verified; live provider auth remains M5.1.

## Immediate focus: M4-035 DONE (awaiting verified commit; batch 17/19 DONE)

The original [M4](ROADMAP.md#m4--first-translation-connector) is decomposed into
[38 bounded cards](TASKS.md#m4--first-translation-connector) for Responses ↔
Anthropic Messages translation. All 34 predecessor tasks ([M4-001](tasks/M4-001.md)
through [M4-034](tasks/M4-034.md)) are committed through `5b17760`. Batch progress:
17 of 19 objective DONE (total 35 DONE; 3 remaining in M4; 2 remaining in batch).

[M4-035](tasks/M4-035.md) (Prepare bounded direct-versus-gateway smoke capture)
acceptance is reviewed and closed DONE (reviewer PASS `ses_efb74802bffemwvER6Jm0YGNth`).
The reproducible offline procedure in [SMOKE-M4](SMOKE-M4.md) and its validator
harness (`scripts/smoke-m4-artifacts.py`) establish pinned direct Messages and
protected Responses-to-Messages translation legs, five matched comparison scenarios,
strict request/time/token caps, credential redaction rules, and loopback fake artifact
generation. Deliberate over-cap and credential-leak fixtures fail closed. Zero live
credentials or external network requests used. 35 tasks are DONE in the registry;
implementation is preserved. Per policy, no next card is prepared or promoted until
the M4-035 commit is cleanly created and verified.

Assign one READY card per worker. Refresh dependent DRAFTs against actual results
before promotion; do not implement their dependencies in one session. Local
fixtures and existing M3 services support development without real credentials.
Live capture is a separate later gate. [Tooling](TOOLING.md#m4-tooling-gate) retains
Context7, gopls and existing shell/Git checks without a new MCP installation.

Roadmap order remains unchanged: Anthropic in M4, Codex in M5.1. Reuse registry,
scoped services, authorization/admission/accounting and dispatch. The existing
generic translation mode is composed without provider branches in Core; request
transformation, provider SSE/errors and token normalization remain Connector-owned.

## Available checks

`./scripts/check.sh` passes formatting, vet, unit/race tests and offline build.
The final audit also passed uncached race-enabled conformance and gateway/Core/
SQLite suites, ten repeated focused accounting/access/auth races, and SQLite
checks with CGO disabled. Mandatory conformance has no skips; only two optional
parallel-tools scenarios skip. Exact commands and evidence are in [M3-046](tasks/M3-046.md).

[LOCAL-M4](LOCAL-M4.md) passes offline dual native/translation protected provisioning,
native loopback dispatch, local translation validation/rejections, rate limiting,
usage inspection, restart, and WAL-safe backup/restore integrity.
[LOCAL-M3](LOCAL-M3.md) passes protected provisioning, inference, limits, usage,
restart and consistent backup/restore. For an existing older M3 database: stop
the gateway, make a consistent backup and run `admin migrate` to v6 before
protected startup. Retain the matching external master key. Legacy/M2 JSON
remain loopback development compatibility modes; see [LOCAL-M2](LOCAL-M2.md).

## Evidence limits and open inputs

- Built-in Connectors include native Responses and protected Anthropic Messages
  translation. A translated instance has one account/model scope; protected YAML
  selects its SQLite credential explicitly with `credential_id`, while native
  settings retain `credential_env`. Inference uses scoped SQLite credentials.
- Native transport failures retain unknown delivery; safe fallback is proven
  with explicit deterministic fixtures, not a broader real-provider retry claim.
  Remaining translation gates/live compatibility, stateful cross-target affinity,
  live OAuth and IPC remain M4/M5/M6.
- Historical live evidence remains OpenCode 2.0.6 / OpenAI Responses /
  `gpt-5.4-mini`, with parallel tools unknown. M3 adds no live inference claim.
  Remote provider compute cancellation remains unverified.
- [References](REFERENCES.md#migration-from-9router) retains source maps; upstream
  provenance and code-reuse licenses must be resolved before copying source.
  Other unresolved design choices remain in [DECISIONS](../project/DECISIONS.md).
