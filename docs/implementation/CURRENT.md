# Current State

## Verified Baseline

- FND-001 Go scaffold is verified: module `github.com/blestafist/pestiroute`, `cmd/gateway` entrypoint with explicit non-serving exit, and root README with artifact-free offline build instructions. No server, test suite, CI workflow, or provider integration yet.
- FND-005 M1 compatibility baseline is selected on source evidence in [References](REFERENCES.md#m1-compatibility-baseline) (OpenAI public Responses / `gpt-4.1-mini-2025-04-14` with OpenCode V2 2.0.6); real inference execution remains an M1 smoke gate.
- [CONTRACT.md](CONTRACT.md) is the accepted v1 semantic boundary for Protocol Adapters, Core Runtime, and Connectors, with ADR change control. Language/IPC bindings are not implemented; configuration and stack choices remain drafts.
- Documentation is organized by project intent and implementation; the root [AGENTS.md](../../AGENTS.md) defines the agent entry path.

## Immediate Focus

[FND-001](tasks/FND-001.md) and [FND-005](tasks/FND-005.md) are DONE. Prerequisite FND-001 is satisfied, unblocking preparation of dependent M0 tasks [FND-002](TASKS.md#m0--foundation) (startup slice), FND-003 (fake upstream), and FND-004 (verification baseline).

After the scaffold, build the startup/configuration path, controllable fake upstream and shared local/CI checks toward the [first vertical slice](ROADMAP.md#first-vertical-slice). The M0 candidates also cover baseline selection and an assembled-foundation verification handoff; real-provider smoke execution remains an M1 gate. Task statuses and dependencies live only in TASKS.

## Open Inputs

- The selected OpenAI public Responses / OpenCode V2 baseline requires M1 access and live verification; account access/credit and actual parallel/cancellation behavior are unknown. Local scaffold work does not need provider credentials.
- Local 9Router/9Gateway revisions and source maps are recorded in [References](REFERENCES.md#migration-from-9router). Reproducible repository provenance and code-reuse licenses remain unresolved before code migration; live compatibility is unverified. These do not block M0.

Other architectural questions remain in [DECISIONS.md](../project/DECISIONS.md#questions-before-implementation). The `/next n` OpenCode command describes a bounded agent workflow; it does not provide a persistent scheduler or change task readiness rules. [Tooling adoption](TOOLING.md) records conditional future additions.

## Available Checks

`git diff --check` is available for patch whitespace. Documentation verification also checks local link targets/anchors, fenced blocks, and task registry consistency; no persistent checker command has been added.

With local Go 1.27.1, `GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build -o /dev/null ./...` is verified and leaves no binary. `GOTOOLCHAIN=local go run ./cmd/gateway` exits 1 with an explicit non-serving notice. `gofmt -l cmd/gateway/main.go`, `GOTOOLCHAIN=local go vet ./...`, `GOTOOLCHAIN=local go test ./...`, and `GOTOOLCHAIN=local go test -race ./...` pass (no tests yet). No CI exists.

## Update Rule

Replace this snapshot when actual capabilities, immediate focus, blockers, or verified commands change. Keep task completion evidence in its card and status in TASKS; do not append a running work log here.
