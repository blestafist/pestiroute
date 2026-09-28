# Atomic Task Backlog

This is the single task and dependency registry. Each row represents one bounded deliverable. [ROADMAP.md](ROADMAP.md) owns milestone goals; task cards in `tasks/<ID>.md` own execution details. This file alone owns status, owner, dependencies, scope summary, result summary, and primary check.

## Registry Rules

- IDs are stable (`FND-001`, then area-specific sequences); never reuse an ID. Group rows by milestone.
- Create cards using [tasks/TEMPLATE.md](tasks/TEMPLATE.md). Link the ID once its card exists; keep unplanned IDs as plain text rather than broken links.
- **Scope** describes the bounded change, not permission to overwrite unrelated work. **Result** records actual completion, not the intended outcome. **Check** is a verification entry point; the card supplies full acceptance and evidence.
- Use `—` for no owner/dependencies/result. A descriptive check for a DRAFT must become an executable command or explicit reproducible manual check before READY. Never invent a passing test or unavailable command.
- Dependencies reference registered IDs and must form an acyclic graph. All dependencies must be DONE before READY or ACTIVE. If a prerequisite is reopened, dependent pending work returns to DRAFT or BLOCKED for reassessment.
- Split work that spans unrelated results or cannot be verified independently. Future milestones remain in ROADMAP until the planner can define bounded tasks.

## Status Lifecycle

| Status | Meaning |
| --- | --- |
| DRAFT | Planning candidate; card, acceptance, dependencies, or check still needs preparation |
| READY | Card exists; scope, acceptance, check and required decisions are clear; dependencies DONE |
| ACTIVE | Claimed by an identifiable owner and being executed |
| BLOCKED | Cannot proceed; card records reason and unblock condition |
| DONE | Acceptance verified; result and evidence recorded |

Normal path: `DRAFT → READY → ACTIVE → DONE`. BLOCKED returns to READY when its blocker is resolved and dependencies are satisfied. Set an owner when moving to ACTIVE; no concurrent ownership is implied. This is a manual documentation convention; harness scheduling and automation will be designed separately.

## M0 — Foundation

High-level planning candidates covering [M0](ROADMAP.md#m0--project-foundation) and its handoff to M1. All remain DRAFT: the Planner will turn each bounded outcome into a task card, resolve relevant choices, and define concrete acceptance and checks before READY. No task cards have been prepared and no implementation work is claimed complete.

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| FND-001 | DRAFT | — | — | Reproducible Go scaffold: module identity, pinned toolchain, minimal `gateway` entrypoint and clean-checkout build instructions | — | Documented build succeeds on the selected toolchain from a clean checkout |
| FND-002 | DRAFT | — | FND-001 | Runnable startup slice: minimal configuration, health/readiness, clear startup failures and bounded graceful shutdown | — | Local lifecycle scenarios cover valid/invalid configuration, readiness and shutdown |
| FND-003 | DRAFT | — | FND-001 | Controllable local fake upstream for M1: JSON and incremental SSE, gated delivery, connection failures and cancellation observation | — | Deterministic HTTP scenarios prove controlled delivery, failures and cleanup without provider credentials |
| FND-004 | DRAFT | — | FND-001 | Shared local/CI verification baseline: formatting, vet, tests, race detection and build on the pinned toolchain | — | The same documented checks run locally and in CI |
| FND-005 | DRAFT | — | — | Select the M1 compatibility baseline: Responses-native backend, client/version and reproducible tool/parallel-tool scenario | — | Baseline choice, prerequisites and intended smoke procedure are recorded; unknown compatibility is explicit |
| FND-006 | DRAFT | — | FND-002, FND-003, FND-004, FND-005 | Verify M0 acceptance and prepare the M1 handoff from actual scaffold, fixture and check results | — | Clean-checkout M0 verification evidence and bounded M1 planning candidates match the implemented foundation |

### Planner Handoff

Prepare cards just ahead of execution using [the template](tasks/TEMPLATE.md). Keep these outcomes high-level until the existing code and preceding results support concrete package names, configuration fields, fixture controls and commands. Resolve module/toolchain choices in FND-001 and the startup configuration subset in FND-002; the [M3 YAML example](CONFIGURATION.md#proposed-yaml) is not an M0 implementation checklist.

FND-002, FND-003 and FND-004 can proceed independently after the scaffold. FND-005 is a research/planning deliverable and can proceed independently of code. FND-006 checks their combined outcome before handing off to M1. Preserve existing IDs if refinement requires splitting a candidate; register additional bounded work rather than silently repurposing an ID.

## Next Planning Boundary

FND-006 verifies every [M0 acceptance criterion](ROADMAP.md#m0--project-foundation) and uses the selected baseline to outline bounded M1 candidates. The Planner prepares executable M1 cards when their prerequisites and checks are concrete. The first end-to-end regression must cover unknown-field preservation, early chunk delivery, and cancellation. Later milestones are not implicitly ready because they appear in ROADMAP.
