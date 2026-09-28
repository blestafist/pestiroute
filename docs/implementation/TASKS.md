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

Initial planning candidates decomposed from the existing M0 scope. No task cards have been prepared and no implementation work is claimed complete.

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| FND-001 | DRAFT | — | — | Go module, pinned toolchain, minimal `gateway` entrypoint, clean-checkout build instructions | — | Clean-checkout build on pinned Go; exact command set in card |
| FND-002 | DRAFT | — | FND-001 | Minimal startup configuration, health/readiness endpoints, invalid-config failure and graceful shutdown | — | Local startup/lifecycle integration scenarios; exact command set in card |
| FND-003 | DRAFT | — | FND-001 | Controllable fake upstream: JSON/SSE, gated chunks, disconnects, cancellation observation | — | Deterministic fake-upstream behavior checks; exact command set in card |
| FND-004 | DRAFT | — | FND-002, FND-003 | CI baseline for formatting, vet, tests, race detection and build | — | Same documented checks pass locally and in CI |

## Next Planning Boundary

Before closing M0, verify every [M0 acceptance criterion](ROADMAP.md#m0--project-foundation), select the real-client/backend baseline from CURRENT, and decompose the M1 native proxy into bounded cards. The first end-to-end regression must cover unknown-field preservation, early chunk delivery, and cancellation. Later milestones are not implicitly ready because they appear in ROADMAP.
