# Current State

## Verified Baseline

- Repository contains architecture and implementation documentation only.
- No Go module, application source, executable, test suite, CI workflow, or verified run command exists yet.
- Protocol Adapter and Connector boundaries are documented; interface signatures, configuration, and stack choices remain design drafts.
- Documentation is organized by project intent and implementation; the root [AGENTS.md](../../AGENTS.md) defines the agent entry path.

## Immediate Focus

Prepare the first M0 task for execution. The initial candidates are in [TASKS.md](TASKS.md#m0--foundation); the planner should create `tasks/FND-001.md` from [TEMPLATE.md](tasks/TEMPLATE.md), resolve the module path and supported Go version, and establish concrete checks before making it READY.

After the scaffold, build the startup/configuration path and controllable fake upstream toward the [first vertical slice](ROADMAP.md#first-vertical-slice). Task statuses and dependencies live only in TASKS.

## Open Inputs

- Go module path and pinned toolchain version: resolve in FND-001.
- First real Responses-native backend and client/version for the smoke baseline: resolve during M0, before the M1 compatibility claim. Local scaffold work does not need provider credentials.
- 9Router repository, revision, license, and source map: required before migration, not before M0.

Other architectural questions remain in [DECISIONS.md](../project/DECISIONS.md#questions-before-implementation). Future harness/planner automation is not specified yet.

## Available Checks

`git diff --check` is available for patch whitespace. Documentation verification also checks local link targets/anchors, fenced blocks, and task registry consistency; no persistent checker command has been added.

Build, test, start, and CI commands will be recorded here when FND tasks establish and verify them. Commands in [STACK.md](STACK.md#build-and-workflow) describe the intended baseline.

## Update Rule

Replace this snapshot when actual capabilities, immediate focus, blockers, or verified commands change. Keep task completion evidence in its card and status in TASKS; do not append a running work log here.
