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

Candidates covering [M0](ROADMAP.md#m0--project-foundation) and its handoff to M1. FND-001 through FND-006 are DONE.

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [FND-001](tasks/FND-001.md) | DONE | GPT-6 Sol (ses_f127f196fffe6OhKnJp1Tx7tO8) | — | Reproducible Go scaffold: module identity, pinned toolchain, minimal `gateway` entrypoint and clean-checkout build instructions | Go scaffold established with module identity, artifact-free offline build and explicit non-serving entrypoint | `GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build -o /dev/null ./...` with Go 1.27.1 from a clean checkout |
| [FND-002](tasks/FND-002.md) | DONE | GPT-6 Sol (ses_f121d2a4bffeR1da0gHcR5hHED) | FND-001 | Runnable startup slice: minimal configuration, health/readiness, clear startup failures and bounded graceful shutdown | Probe-only HTTP server with JSON/flag startup, /healthz and /readyz endpoints, invalid input failure reporting, and signal-driven bounded shutdown | `GOTOOLCHAIN=local go test -race ./...` and CLI probe/signal/invalid-config runs pass |
| [FND-003](tasks/FND-003.md) | DONE | GPT-6 Sol (ses_f120de8a5ffe5h4hBv3AO2aUP2) | FND-001 | Controllable local fake upstream for M1: JSON and incremental SSE, request capture, gated delivery, connection failures and cancellation observation | Controllable loopback HTTP fixture with request byte/header capture, gated incremental SSE streaming, mid-stream drop, and client cancellation observation | `GOTOOLCHAIN=local go test -race ./internal/testutil/fakeupstream/...` |
| [FND-004](tasks/FND-004.md) | DONE | GPT-6 Sol (ses_f1205c26dffezzHtzMCYSiwL0T) | FND-001 | Shared local/CI verification baseline: formatting, vet, tests, race detection and build on the pinned toolchain | Executable shared offline check script and pinned Go CI workflow; local suite and failure detection verified, hosted CI unrun | `./scripts/check.sh` with Go 1.27.1 |
| [FND-005](tasks/FND-005.md) | DONE | GPT-6 Sol (ses_f12616adbffeeLzbhovjYlZ5v0) | — | Select the M1 compatibility baseline: Responses-native backend, client/version and reproducible tool/parallel-tool scenario | OpenAI public Responses / gpt-4.1-mini-2025-04-14 with OpenCode V2 2.0.6 selected on source evidence; live gate remains M1 | Review source-linked baseline note and reproducible smoke procedure against the card's acceptance checklist |
| [FND-006](tasks/FND-006.md) | DONE | GPT-6 Sol (ses_f11faaaf0ffeBsSBNpzuIgJknJ) | FND-002, FND-003, FND-004, FND-005 | Verify M0 acceptance and prepare the M1 handoff from actual scaffold, fixture and check results | Local assembled M0 checks, isolated clean-build lifecycle, and M1 candidate registration verified | `./scripts/check.sh` passes, gateway probe/failure/shutdown verified, and M1 candidate breakdown prepared |

### Planner Handoff

Prepare cards just ahead of execution using [the template](tasks/TEMPLATE.md). Keep dependent outcomes high-level until preceding results support concrete package names, configuration fields, fixture controls and commands. FND-001 fixes module/toolchain choices; FND-002 resolves the startup configuration subset after the scaffold. The [M3 YAML example](CONFIGURATION.md#proposed-yaml) is not an M0 implementation checklist.

FND-002, FND-003 and FND-004 can proceed independently after the scaffold. FND-005 is a research/planning deliverable and can proceed independently of code. FND-006 checks their combined outcome before handing off to M1. Preserve existing IDs if refinement requires splitting a candidate; register additional bounded work rather than silently repurposing an ID.

M0 readiness describes the implemented startup service, not inference availability or provider health. The fake upstream is a test fixture; its controls should support observable byte capture and deterministic synchronization rather than timing-only assertions. The actual client-to-gateway-to-upstream regression belongs to M1 under the accepted v1 contract.

FND-005 records a specific intended backend/model and client/version, selection evidence, access prerequisites and the smoke scenario. Selection does not require a paid/live inference run or establish verified support; real execution remains an M1 acceptance gate. Missing access must be recorded for that gate, while local foundation work stays independent. FND-004 establishes checks early; FND-006 reruns them against the assembled foundation so an earlier green scaffold CI is not mistaken for verified M0 behavior.

## Next Planning Boundary

FND-006 verifies every [M0 acceptance criterion](ROADMAP.md#m0--project-foundation) and uses the selected baseline to outline bounded M1 candidates. The Planner prepares executable M1 cards when their prerequisites and checks are concrete. The first end-to-end regression must cover unknown-field preservation, early chunk delivery, and cancellation. Later milestones are not implicitly ready because they appear in ROADMAP.

## M1 — Transparent Responses Proxy (planning candidates)

M1 is decomposed into 27 bounded cards. **Assign one READY card per worker session, never this milestone or a dependency chain.** Read only that card's linked sections and affected code; stop at its acceptance boundary. Dependencies are not extra scope. The Planner refreshes each dependent DRAFT against actual predecessor results before promotion, recording concrete package/test names where needed. Only M1-004 is initially READY. All local work is independent of real API credentials.

Existing IDs retain their purpose: M1-001 is now fixed-response composition after component work, M1-002 is the native SSE integration gate, and M1-003 compares separately captured live evidence. They no longer authorize implementing their prerequisites. The order below is dependency-oriented, not numeric. M2 registry/conformance, M3 storage/auth/accounting, translation, and automatic fallback remain outside M1.

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [M1-004](tasks/M1-004.md) | DONE | GPT-6 Sol (ses_f113f5c46ffeR9yYsJZYuHRbVi) | FND-006 | Specify the minimal M1 binding and local startup choices against v1, resolving component handoff questions | Native binding/startup note linked from STACK; contract mapping, paper walkthrough, reviewer corrections and checks verified | Manual contract/scenario/link review, `./scripts/check.sh`, `git diff --check` |
| [M1-005](tasks/M1-005.md) | ACTIVE | GPT-6 Sol (ses_f112e23b0ffeF8EhX6FodWIS6H) | M1-004 | Minimal Go execution envelope, frames, errors and cancellable stream boundary | — | `go test -race ./internal/core/...` and `./scripts/check.sh` |
| [M1-006](tasks/M1-006.md) | DRAFT | — | M1-004 | Single-target startup configuration, limits and runtime-owned environment credential | — | Configuration validation tests and `./scripts/check.sh` |
| [M1-007](tasks/M1-007.md) | DRAFT | — | M1-005 | Bounded Responses Decode with byte-preserving envelope extraction | — | Decoder byte/invalid/limit tests and `./scripts/check.sh` |
| [M1-008](tasks/M1-008.md) | DRAFT | — | M1-005, M1-006 | Scoped native HTTP transport: selected credentials, no redirects or hidden replay | — | Loopback transport capture/replay tests and `./scripts/check.sh` |
| [M1-009](tasks/M1-009.md) | DRAFT | — | M1-008 | Fixed JSON native Connector frames and classified completion | — | Connector fixed-response lifecycle tests and `./scripts/check.sh` |
| [M1-010](tasks/M1-010.md) | DRAFT | — | M1-005 | Single-target Core dispatch, trusted IDs, frame lifecycle and one finalization | — | Scripted-connector Core tests and `./scripts/check.sh` |
| [M1-011](tasks/M1-011.md) | DRAFT | — | M1-005 | HTTP Encode for native frames and pre-head gateway errors | — | Encoder commit/byte/error tests and `./scripts/check.sh` |
| [M1-001](tasks/M1-001.md) | DRAFT | — | M1-006, M1-007, M1-009, M1-010, M1-011 | Compose POST /v1/responses and prove fixed-response end-to-end passthrough | — | Real loopback fixed-response gateway regression and `./scripts/check.sh` |
| [M1-012](tasks/M1-012.md) | DRAFT | — | M1-009 | Incremental native SSE Connector output and bounded terminal-outcome observation | — | Gated Connector SSE/outcome tests and `./scripts/check.sh` |
| [M1-013](tasks/M1-013.md) | DRAFT | — | M1-001, M1-012 | Flush native body frames to HTTP client before upstream completion | — | Real-socket first-event gate and `./scripts/check.sh` |
| [M1-002](tasks/M1-002.md) | DRAFT | — | M1-013 | Verify SSE split boundaries, event ordering and interleaved tool IDs end to end | — | Byte-exact split-event integration and `./scripts/check.sh` |
| [M1-014](tasks/M1-014.md) | DRAFT | — | M1-013 | Client disconnect/write failure cancels upstream and releases stream | — | Explicit client-cancel cleanup tests and `./scripts/check.sh` |
| [M1-015](tasks/M1-015.md) | DRAFT | — | M1-001 | Pre-commit rejection and transport-error behavior without replay | — | Rejection/drop request-count tests and `./scripts/check.sh` |
| [M1-016](tasks/M1-016.md) | DRAFT | — | M1-013, M1-015 | Post-commit failure closes response without replacement or retry | — | Late-drop/EOF integration and `./scripts/check.sh` |
| [M1-017](tasks/M1-017.md) | DRAFT | — | M1-014 | Bounded backpressure under a stalled downstream reader | — | Controlled slow-consumer/buffer-bound tests and `./scripts/check.sh` |
| [M1-018](tasks/M1-018.md) | DRAFT | — | M1-014, M1-016 | Phase-specific timeouts and deadline cancellation for active requests | — | Header/idle/deadline tests and `./scripts/check.sh` |
| [M1-019](tasks/M1-019.md) | DRAFT | — | M1-018 | Drain and bounded shutdown of active inference and Connector resources | — | Shutdown with gated active work and `./scripts/check.sh` |
| [M1-020](tasks/M1-020.md) | DRAFT | — | M1-016, M1-019 | Verify concurrent request isolation and exactly-once in-memory finalization | — | Race-tested competing termination scenarios and `./scripts/check.sh` |
| [M1-021](tasks/M1-021.md) | DRAFT | — | M1-001 | End-to-end credential/header isolation and encoded native response preservation | — | Header/compression capture matrix and `./scripts/check.sh` |
| [M1-022](tasks/M1-022.md) | DRAFT | — | M1-002, M1-017, M1-020, M1-021 | Assemble local M1 gate, runnable startup docs and measured native baseline | — | Clean local run, existing regressions, baseline procedure and `./scripts/check.sh` |
| [M1-023](tasks/M1-023.md) | DRAFT | — | M1-022 | Prepare exact-version disposable live-smoke setup and evidence capture procedure | — | Local fake-target dry run of documented procedure |
| [M1-024](tasks/M1-024.md) | DRAFT | — | M1-023 | Capture direct real-client single/parallel tool and continuation evidence | — | Versioned direct trace review; requires API access/credit |
| [M1-025](tasks/M1-025.md) | DRAFT | — | M1-024 | Capture gateway real-client single/parallel tool and continuation evidence | — | Versioned gateway trace review; requires API access/credit |
| [M1-026](tasks/M1-026.md) | DRAFT | — | M1-025 | Observe live direct/gateway streaming interruption and cancellation | — | Explicit interrupted-client traces and cleanup evidence; requires API access/credit |
| [M1-003](tasks/M1-003.md) | DRAFT | — | M1-024, M1-025, M1-026 | Compare versioned direct/gateway evidence and report supported/unknown observations | — | REFERENCES smoke checklist against sanitized traces |
| [M1-027](tasks/M1-027.md) | DRAFT | — | M1-022, M1-003 | Verify every M1 roadmap gate and hand off actual scope to M2 | — | Acceptance-to-evidence audit and `./scripts/check.sh` |
