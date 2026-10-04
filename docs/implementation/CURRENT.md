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

## Immediate focus: M4-044 DONE; DEC-008/D18 accepted; offline M4-037/M4-038 eligible; M4-036/M4-041 BLOCKED

The original [M4](ROADMAP.md#m4--first-translation-connector) is decomposed into
cards for Responses ↔ Anthropic Messages translation. Of 44 registered M4 rows in
[TASKS](TASKS.md), 40 are complete ([M4-001](tasks/M4-001.md) through [M4-035](tasks/M4-035.md),
[M4-039](tasks/M4-039.md), [M4-040](tasks/M4-040.md), [M4-042](tasks/M4-042.md), [M4-043](tasks/M4-043.md),
and [M4-044](tasks/M4-044.md)), 2 are BLOCKED ([M4-036](tasks/M4-036.md), [M4-041](tasks/M4-041.md)),
2 remain DRAFT ([M4-037](tasks/M4-037.md), [M4-038](tasks/M4-038.md)), and 0 are ACTIVE or READY.

[M4-044](tasks/M4-044.md) is closed DONE (reviewer PASS `ses_ef7d73665ffehMAw63fb7G4G6Q`).
Human governance authority explicitly accepted the DEC-008/D18 narrow deferral:
defer official Anthropic verification to allow offline M4 completion (M4-037, M4-038),
while keeping official M4-036 and compatible M4-041 strictly BLOCKED (no false passes),
and retaining official live verification as a mandatory pre-production release gate.
`DECISIONS.md` records accepted DEC-008/D18; `ROADMAP.md` aligns M4 offline gate criteria;
`TASKS.md` updates M4-037 dependencies to (M4-044, M4-032, M4-035); Core runtime behavior,
`CONTRACT.md`, and production code remain unchanged. M4-037/M4-038 remain DRAFT pending
next-task promotion after commit verification.

[M4-041](tasks/M4-041.md) is settled as BLOCKED. Its authorized live batch stopped after
one direct Messages request: actual HTTP 200, turn 1, terminal `tool_use`, safe tool class `other`,
category `unexpected_tool_name`, one dispatch, zero retries. Live tool cycles acceptance is
unmet; compatible 9router tool behavior is unsupported/unknown; no further live retry or
model credits will be spent. Sanitized failure evidence is preserved locally in non-repo/non-versioned
`/tmp/opencode/m4-041-batch.PTHIKQ/partial-failure.json` and credentials were deleted.
All M4-041 test harness, mock, script, and doc extensions remain pending in the uncommitted
working tree and are not part of any delivered task or committed branch; M4-041 is not DONE.
Official [M4-036](tasks/M4-036.md) remains strictly BLOCKED with no waiver.

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
