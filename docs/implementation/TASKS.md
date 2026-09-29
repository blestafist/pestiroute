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

These DRAFT rows are not executable assignments. The Planner prepares cards, concrete commands, and acceptance before promotion; the live smoke requires separate API access. All paths use the selected OpenAI public Responses baseline and the accepted v1 execution contract, not a temporary proxy contract.

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| M1-001 | DRAFT | — | FND-006 | Wire a single explicit Responses target through adapter, Core and native connector: bounded ingress, scoped upstream credential, opaque request/response bytes and contract-aligned stream lifecycle even for fixed JSON | — | Fake-upstream fixed-response integration: unknown fields/whitespace and response bytes preserved; oversize/invalid requests fail before upstream; credentials isolated |
| M1-002 | DRAFT | — | M1-001 | Exercise incremental native SSE path and cleanup: ordered flushed chunks, bounded backpressure, cancellation, pre-/post-commit failure without hidden replay or replacement response | — | Fake-upstream gated two-chunk integration: first event before completion, byte/event order and IDs preserved, disconnect cancels upstream, late drop does not retry |
| M1-003 | DRAFT | — | M1-002 | Run versioned direct-versus-gateway OpenCode 2.0.6 smoke with public Responses `gpt-4.1-mini-2025-04-14`: tools, parallel attempt, continuation and cancellation; sanitize evidence | — | Review versioned direct/gateway traces against REFERENCES smoke procedure and M1 roadmap gate; requires authorized API access/credit |
