# Current State

## Verified Baseline

M0, M1 and all 34 M2 tasks are complete. [M2-034](tasks/M2-034.md) maps every milestone gate to executed evidence on source revision `0da7ffd`; its closure commit changes documentation only. Hosted CI passed on the preceding `9cadb53` revision; the final source was checked locally on Go 1.27.1.

[CONTRACT.md](CONTRACT.md) remains the accepted v1 semantic boundary. [M2-BINDING.md](M2-BINDING.md) is implemented in-process: managed components and registry lifecycle, scoped invocation services, explicit native identity routes, common Connector dispatch, validated Head/Body/Complete/EOF, and bounded in-memory attempt observations. Core has no production imports of concrete Adapters, Connectors or protocol parsers. [M1-BINDING.md](M1-BINDING.md) retains the historical startup baseline.

The running gateway accepts legacy single-target and strict M2 topology JSON on numeric loopback. Selected-route body/header limits and capability restrictions are enforced before Execute. Complete is withheld until producer EOF validates; trailing frames/errors cannot expose successful completion. Execution remains one attempt per request, with no automatic fallback, model rewriting, reload or session APIs.

## Immediate Focus

Plan the first M3 storage slice against the existing `InvocationServices`, runtime-selected account/credential references, `AttemptResult` and nullable `UsageReport`. Define SQLite persistence/migrations, encrypted credential storage and master-key handling, idempotent attempt/usage writes and interrupted-attempt recovery. Keep database transactions out of network operations; prepare concrete worker cards only after these boundaries and checks are resolved. This is one planning action, not M3 implementation readiness or a speculative task chain.

[ROADMAP.md](ROADMAP.md#m3--access-accounts-and-accounting) owns M3 outcomes. SQLite, durable credentials/usage, virtual keys, admission limits, reservations/reconciliation, OAuth persistence/refresh and safe automatic fallback remain unimplemented. New protocol translation and IPC remain later milestones; the [M3 YAML schema](CONFIGURATION.md#proposed-yaml) is still draft.

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
