# Current State

## Verified Baseline

- FND-001 Go module scaffold, FND-002 probe-only server, FND-003 controllable local fake upstream fixture, and FND-004 shared local/CI workflow are verified locally. M1-001 end-to-end loopback POST /v1/responses fixed JSON passthrough is verified. Hosted CI has not run.
- FND-005 records the historical M1 compatibility baseline selection in [References](REFERENCES.md#m1-compatibility-baseline); the current smoke model is `gpt-5.4-mini` (synchronized locally under M1-028; real inference remains an M1 smoke gate).
- [CONTRACT.md](CONTRACT.md) is the accepted v1 semantic boundary for Protocol Adapters, Core Runtime, and Connectors, with ADR change control. [M1-BINDING.md](M1-BINDING.md) specifies the minimal Go execution signatures, startup JSON subset, capability checks, and bounded streaming observer. The scoped M1 Go execution path is implemented; the complete component/support-operation Go binding is M2 work, and IPC is deferred to M6. The M3 configuration schema remains draft.
- Documentation is organized by project intent and implementation; the root [AGENTS.md](../../AGENTS.md) defines the agent entry path.

## Immediate Focus

M1 is complete; [M1-027](tasks/M1-027.md#completion-evidence) maps its gates to deterministic regressions and scoped live evidence. The live baseline is OpenCode 2.0.6 / OpenAI Responses / `gpt-5.4-mini`: tools and reasoning have recorded scoped support; `llm.tools.parallel` remains unknown. Supplementary paired runs have clean `21d5c13` provenance; the original M1-025 revision is unknown. Live in-client HTTP abort/internal gateway teardown and remote compute cancellation remain unverified; local disconnect/cancellation proofs pass. Preserve these limits when generalizing declarations.

[M2 planning](TASKS.md#m2--connector-api-and-conformance-baseline) contains 34 bounded cards based on completed M1 code. Tasks [M2-002](tasks/M2-002.md) through [M2-009](tasks/M2-009.md) (`218d1c9`), [M2-010](tasks/M2-010.md) (`965cdfa`), [M2-011](tasks/M2-011.md) (`f25a449`), [M2-012](tasks/M2-012.md) (`dd817f3`), [M2-013](tasks/M2-013.md) (`787936a`), and [M2-014](tasks/M2-014.md) (`8938808`) are committed clean. In the active 10-task batch M2-010..M2-019 (7/10 committed clean), [M2-015](tasks/M2-015.md) (`d08b31f`), [M2-016](tasks/M2-016.md) (`a654f08`), and [M2-017](tasks/M2-017.md) (bounded in-memory attempt observations) are verified DONE (8/10 verified); immediate focus advances to [M2-018](tasks/M2-018.md) (validate terminal EOF before finalizing stream success).

M2 extends the existing envelope/frames/errors. It adds managed components/registry, scoped services, explicit native identity routes, one-attempt observations and native/scripted conformance. Keep SQLite, durable secrets/usage, virtual keys, limits, OAuth flows and automatic fallback for M3; new protocol translation and IPC remain later work. All planned M2 verification uses deterministic local fixtures.

## Open Inputs

- M2 needs no real-provider credentials or new live smoke. Broader live parallel/cancellation support remains unverified; existing M1 evidence does not establish it. Local native fixtures must not broaden real-account declarations.
- Local 9Router/9Gateway revisions and source maps are recorded in [References](REFERENCES.md#migration-from-9router). Reproducible repository provenance and code-reuse licenses remain unresolved before code migration; live compatibility is unverified. These do not block M2 interface/conformance work; no source migration is planned here.

Other architectural questions remain in [DECISIONS.md](../project/DECISIONS.md#questions-before-implementation). The `/next n` OpenCode command describes a bounded agent workflow; it does not provide a persistent scheduler or change task readiness rules. [Tooling adoption](TOOLING.md) records conditional future additions.

## Available Checks

`git diff --check` is available for patch whitespace. Documentation verification also checks local link targets/anchors, fenced blocks, and task registry consistency; no persistent checker command has been added.

With local Go 1.27.1, `./scripts/check.sh` passes: whitespace, `gofmt -l cmd internal`, vet, tests, race tests, and offline artifact-free build. In an isolated HEAD export, offline build and direct binary startup passed: `/healthz` and `/readyz` returned 200; SIGTERM exited 0 in <3s; malformed addresses and timeouts returned non-zero with stderr. M1-022 additionally verifies isolated-source build and fake-target POST using synthetic credentials; `go test -count=1 -v -run 'Test.*Baseline' ./cmd/gateway/...` records local TTFB/heap/goroutine observations (see task evidence; no budgets). M1-023 verifies exact OpenCode 2.0.6 plural-provider loopback HTTP/SSE probe via `OPENCODE_BIN=<2.0.6 binary> ./scripts/smoke-m1-dry-run.sh` inside an isolated loopback-only user/network namespace. `GOTOOLCHAIN=local go run ./cmd/gateway -listen invalid:address` also failed clearly. `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race ./internal/testutil/fakeupstream/...` passes byte capture, gated SSE, drops and cancellation. CI workflow invokes the same script on pinned Go 1.27.1; hosted execution and actionlint remain unrun. `gofmt -l .` reports an unrelated pre-existing file under `.refs/9Router/`.

## Update Rule

Replace this snapshot when actual capabilities, immediate focus, blockers, or verified commands change. Keep task completion evidence in its card and status in TASKS; do not append a running work log here.
