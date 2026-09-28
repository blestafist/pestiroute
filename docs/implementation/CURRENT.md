# Current State

## Verified Baseline

- Repository contains architecture and implementation documentation only.
- No Go module, application source, executable, test suite, CI workflow, or verified run command exists yet.
- [CONTRACT.md](CONTRACT.md) is the accepted v1 semantic boundary for Protocol Adapters, Core Runtime, and Connectors, with ADR change control. Language/IPC bindings are not implemented; configuration and stack choices remain drafts.
- Documentation is organized by project intent and implementation; the root [AGENTS.md](../../AGENTS.md) defines the agent entry path.

## Immediate Focus

The independent [FND-001](tasks/FND-001.md) scaffold and [FND-005](tasks/FND-005.md) M1 baseline selection are READY for separate workers. Claim each in [TASKS.md](TASKS.md#m0--foundation) before execution. FND-001 uses module `github.com/blestafist/pestiroute` and Go 1.27.1; FND-005 selects and documents the backend/client combination without claiming live compatibility.

After the scaffold, build the startup/configuration path, controllable fake upstream and shared local/CI checks toward the [first vertical slice](ROADMAP.md#first-vertical-slice). The M0 candidates also cover baseline selection and an assembled-foundation verification handoff; real-provider smoke execution remains an M1 gate. Task statuses and dependencies live only in TASKS.

## Open Inputs

- First real Responses-native backend and client/version for the smoke baseline: resolve during M0, before the M1 compatibility claim. Local scaffold work does not need provider credentials.
- Local 9Router/9Gateway revisions and source maps are recorded in [References](REFERENCES.md#migration-from-9router). Reproducible repository provenance and code-reuse licenses remain unresolved before code migration; live compatibility is unverified. These do not block M0.

Other architectural questions remain in [DECISIONS.md](../project/DECISIONS.md#questions-before-implementation). Future harness/planner automation is not specified yet.

## Available Checks

`git diff --check` is available for patch whitespace. Documentation verification also checks local link targets/anchors, fenced blocks, and task registry consistency; no persistent checker command has been added.

Build, test, start, and CI commands will be recorded here when FND tasks establish and verify them. Commands in [STACK.md](STACK.md#build-and-workflow) describe the intended baseline.

## Update Rule

Replace this snapshot when actual capabilities, immediate focus, blockers, or verified commands change. Keep task completion evidence in its card and status in TASKS; do not append a running work log here.
