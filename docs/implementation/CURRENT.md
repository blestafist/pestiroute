# Current State

## Verified Baseline

- Repository contains architecture and implementation documentation only.
- No Go module, application source, executable, test suite, CI workflow, or verified run command exists yet.
- [CONTRACT.md](CONTRACT.md) is the accepted v1 semantic boundary for Protocol Adapters, Core Runtime, and Connectors, with ADR change control. Language/IPC bindings are not implemented; configuration and stack choices remain drafts.
- Documentation is organized by project intent and implementation; the root [AGENTS.md](../../AGENTS.md) defines the agent entry path.

## Immediate Focus

Prepare the first M0 task for execution. The initial candidates are in [TASKS.md](TASKS.md#m0--foundation); the planner should create `tasks/FND-001.md` from [TEMPLATE.md](tasks/TEMPLATE.md), resolve the module path and supported Go version, and establish concrete checks before making it READY.

After the scaffold, build the startup/configuration path, controllable fake upstream and shared local/CI checks toward the [first vertical slice](ROADMAP.md#first-vertical-slice). The M0 candidates also cover baseline selection and an assembled-foundation verification handoff; real-provider smoke execution remains an M1 gate. Task statuses and dependencies live only in TASKS.

## Open Inputs

- Go module path and pinned toolchain version: resolve in FND-001.
- First real Responses-native backend and client/version for the smoke baseline: resolve during M0, before the M1 compatibility claim. Local scaffold work does not need provider credentials.
- Local 9Router/9Gateway revisions and source maps are recorded in [References](REFERENCES.md#migration-from-9router). Reproducible repository provenance and code-reuse licenses remain unresolved before code migration; live compatibility is unverified. These do not block M0.

Other architectural questions remain in [DECISIONS.md](../project/DECISIONS.md#questions-before-implementation). The `/next n` OpenCode command describes a bounded agent workflow; it does not provide a persistent scheduler or change task readiness rules. [Tooling adoption](TOOLING.md) records conditional future additions.

## Available Checks

`git diff --check` is available for patch whitespace. Documentation verification also checks local link targets/anchors, fenced blocks, and task registry consistency; no persistent checker command has been added.

Build, test, start, and CI commands will be recorded here when FND tasks establish and verify them. Commands in [STACK.md](STACK.md#build-and-workflow) describe the intended baseline.

## Update Rule

Replace this snapshot when actual capabilities, immediate focus, blockers, or verified commands change. Keep task completion evidence in its card and status in TASKS; do not append a running work log here.
