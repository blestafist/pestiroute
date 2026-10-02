# Current State

## Verified Baseline

M0, M1 and all 34 M2 tasks are complete. [M2-034](tasks/M2-034.md) maps every milestone gate to executed evidence on source revision `0da7ffd`; its closure commit changes documentation only. Hosted CI passed on the preceding `9cadb53` revision; the final source was checked locally on Go 1.27.1.

[CONTRACT.md](CONTRACT.md) remains the accepted v1 semantic boundary. [M2-BINDING.md](M2-BINDING.md) is implemented in-process: managed components and registry lifecycle, scoped invocation services, explicit native identity routes, common Connector dispatch, validated Head/Body/Complete/EOF, and bounded in-memory attempt observations. Core has no production imports of concrete Adapters, Connectors or protocol parsers. [M1-BINDING.md](M1-BINDING.md) retains the historical startup baseline.

The running gateway accepts legacy single-target and strict M2 topology JSON on numeric loopback. Selected-route body/header limits and capability restrictions are enforced before Execute. Complete is withheld until producer EOF validates; trailing frames/errors cannot expose successful completion. Execution remains one attempt per request, with no automatic fallback, model rewriting, reload or session APIs.

## Immediate Focus

[M3-001](tasks/M3-001.md), [M3-002](tasks/M3-002.md), and [M3-003](tasks/M3-003.md) are DONE. SQLite driver `modernc.org/sqlite` v1.60.1 is validated and pinned; `internal/storage/sqlite.Open` establishes the file-backed connection pragma seam (WAL, `synchronous=FULL`, foreign keys, 500 ms busy timeout); M3 runtime access, admission, and durable accounting architecture is specified in `M3-RUNTIME.md` and synchronized with `M3-STORAGE.md`.

The next eligible registered candidate in sequence is [M3-004](tasks/M3-004.md) (specify M3 configuration and retry admission, depends on M3-003), currently DRAFT. Additional independent candidates with satisfied prerequisites eligible for planning refinement are [M3-005](tasks/M3-005.md) (depends on M3-002), [M3-009](tasks/M3-009.md) (depends on M3-001), and [M3-022](tasks/M3-022.md) (depends on M3-003). All remaining M3 cards stay DRAFT until prerequisites are DONE and cards are promoted. Do not implement a dependency chain in one session or use the optional `TryRecord` observation sink as the durable ledger.

Persistent accounts/credentials/usage, virtual keys, RPM/TPM reservations/reconciliation, recovery, protected YAML startup and bounded fallback are planned, not implemented. The [YAML example](CONFIGURATION.md#proposed-yaml) remains draft pending M3-004. OAuth runtime sessions/serialization use scripted validation in M3; real provider flows remain M5.1. Stateful cross-target affinity, protocol translation and IPC remain outside M3.

Use the existing project agents, atomic cards, Context7, gopls and file/shell/Go checks. [The M3 tooling gate](TOOLING.md#m3-tooling-gate) recommends an optional local SQLite CLI; no new MCP/plugin is required. The old machine-local tool inventory has not been reverified here.

## Evidence Limits and Open Inputs

- Historical live M1 evidence is OpenCode 2.0.6 / OpenAI Responses / `gpt-5.4-mini`: tools and reasoning have scoped support; `llm.tools.parallel` remains unknown. Supplementary paired runs have clean `21d5c13` provenance; the original M1-025 revision is unknown. No new live inference was required for M2.
- Live in-client HTTP abort/internal gateway teardown and remote provider compute cancellation remain unverified. Local request cancellation, stream teardown and exactly-once finalization are proven by deterministic fixtures.
- Explicit numeric-loopback fixture declarations apply only to their configured scope. External endpoints retain the established M1 capability ceiling; local tests do not broaden real-account support.
- [References](REFERENCES.md#migration-from-9router) records local 9Router/9Gateway revisions/source maps. Reproducible upstream provenance and code-reuse licenses remain unresolved before source migration; M2 migrated no external source.

Other unresolved design questions belong in [DECISIONS.md](../project/DECISIONS.md#questions-before-implementation). The `/next n` command is a bounded workflow, not a persistent scheduler. [TOOLING.md](TOOLING.md) records navigation tools and conditional future additions.

## Available Checks

`./scripts/check.sh` passes whitespace, formatting (`cmd`, `internal`), vet, unit tests, race tests and offline artifact-free build on pinned Go 1.27.1. `git diff --check` verifies patch whitespace. Documentation checks cover local links/anchors, fences and task dependencies; no persistent documentation checker has been added.

The final audit ran `go test -race -count=1 -v ./internal/conformance/...` and ten repeated focused route-admission, terminal-stream and concurrent-dispatch race runs (exact command in [M2-034](tasks/M2-034.md#verification)). Only the two optional parallel-tools slots skip; no mandatory gate skips. The [LOCAL-M2 procedure](LOCAL-M2.md) passes offline build, multi-route/component dispatch and legacy single-target startup with synthetic loopback targets. Full gateway regressions retain M1 byte preservation, incremental delivery, header/credential isolation, cancellation and no-replay behavior.

Historical live and resource-baseline procedures remain in their M1 task cards. The CI workflow runs the same shared script on pinned Go 1.27.1. Actionlint remains unrun. `gofmt -l .` reports an unrelated pre-existing file under `.refs/9Router/`; shared checks deliberately target production/test code in `cmd` and `internal`.

## Update Rule

Replace this snapshot when delivered capabilities, immediate focus, blockers or verified commands change. Put durable rationale in DECISIONS, task evidence in its card, and history in Git.
