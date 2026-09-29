# Current State

## Verified Baseline

- FND-001 Go module scaffold, FND-002 probe-only server, FND-003 controllable local fake upstream fixture, and FND-004 shared local/CI workflow are verified locally. Hosted CI has not run. No inference endpoint or provider integration exists yet.
- FND-005 M1 compatibility baseline is selected on source evidence in [References](REFERENCES.md#m1-compatibility-baseline) (OpenAI public Responses / `gpt-4.1-mini-2025-04-14` with OpenCode V2 2.0.6); real inference execution remains an M1 smoke gate.
- [CONTRACT.md](CONTRACT.md) is the accepted v1 semantic boundary for Protocol Adapters, Core Runtime, and Connectors, with ADR change control. [M1-BINDING.md](M1-BINDING.md) specifies the minimal Go execution signatures, startup JSON subset, capability checks, and bounded streaming observer. Language/IPC bindings are not implemented; M3 configuration schema remains draft.
- Documentation is organized by project intent and implementation; the root [AGENTS.md](../../AGENTS.md) defines the agent entry path.

## Immediate Focus

[FND-001](tasks/FND-001.md) through [FND-006](tasks/FND-006.md), [M1-004](tasks/M1-004.md), and [M1-005](tasks/M1-005.md) are DONE.

Next: plan and promote eligible candidates [M1-006](tasks/M1-006.md) (single-target startup configuration, depends on M1-004) and [M1-007](tasks/M1-007.md) (bounded Responses Decode, depends on M1-005). M1 has [27 bounded cards](TASKS.md#m1--transparent-responses-proxy-planning-candidates); assign one READY card per worker session, never the milestone. Real-provider smoke execution remains a separate M1 gate. Task statuses and dependencies live only in TASKS.

## Open Inputs

- The selected OpenAI public Responses / OpenCode V2 baseline requires M1 access and live verification; account access/credit and actual parallel/cancellation behavior are unknown. Local scaffold work does not need provider credentials.
- Local 9Router/9Gateway revisions and source maps are recorded in [References](REFERENCES.md#migration-from-9router). Reproducible repository provenance and code-reuse licenses remain unresolved before code migration; live compatibility is unverified. These do not block M0.

Other architectural questions remain in [DECISIONS.md](../project/DECISIONS.md#questions-before-implementation). The `/next n` OpenCode command describes a bounded agent workflow; it does not provide a persistent scheduler or change task readiness rules. [Tooling adoption](TOOLING.md) records conditional future additions.

## Available Checks

`git diff --check` is available for patch whitespace. Documentation verification also checks local link targets/anchors, fenced blocks, and task registry consistency; no persistent checker command has been added.

With local Go 1.27.1, `./scripts/check.sh` passes: whitespace, `gofmt -l cmd internal`, vet, tests, race tests, and offline artifact-free build. In an isolated HEAD export, offline build and direct binary startup passed: `/healthz` and `/readyz` returned 200; SIGTERM exited 0 in <3s; malformed addresses and timeouts returned non-zero with stderr. `GOTOOLCHAIN=local go run ./cmd/gateway -listen invalid:address` also failed clearly. `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race ./internal/testutil/fakeupstream/...` passes byte capture, gated SSE, drops and cancellation. CI workflow invokes the same script on pinned Go 1.27.1; hosted execution and actionlint remain unrun. `gofmt -l .` reports an unrelated pre-existing file under `.refs/9Router/`.

## Update Rule

Replace this snapshot when actual capabilities, immediate focus, blockers, or verified commands change. Keep task completion evidence in its card and status in TASKS; do not append a running work log here.
