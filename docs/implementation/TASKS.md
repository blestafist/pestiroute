# Atomic Task Backlog

This is the active task and dependency registry. Together with [the completed archive](archive/TASKS-DONE.md), it is the single registry: each ID has exactly one row. Each row represents one bounded deliverable. [ROADMAP.md](ROADMAP.md) owns milestone goals; task cards in `tasks/<ID>.md` own execution details. Registry rows own status, owner, dependencies, scope summary, result summary, and primary check. Cards never duplicate these fields.

## Registry Rules

- IDs are stable (`FND-001`, then area-specific sequences); never reuse an ID. Group rows by milestone.
- Create cards using [tasks/TEMPLATE.md](tasks/TEMPLATE.md). Link the ID once its card exists; keep unplanned IDs as plain text rather than broken links.
- **Scope** describes the bounded change, not permission to overwrite unrelated work. **Result** records actual completion, not the intended outcome. **Check** is a verification entry point; the card supplies full acceptance and evidence.
- Use `—` for no owner/dependencies/result. A descriptive check for a DRAFT must become an executable command or explicit reproducible manual check before READY. Never invent a passing test or unavailable command.
- Dependencies reference registered IDs in the active registry or completed archive and must form an acyclic graph. All dependencies must be DONE before READY or ACTIVE. If a prerequisite is reopened, dependent pending work returns to DRAFT or BLOCKED for reassessment.
- Split work that spans unrelated results or cannot be verified independently. Future milestones remain in ROADMAP until the planner can define bounded tasks.

## Status Lifecycle

| Status | Meaning |
| --- | --- |
| DRAFT | Planning candidate; card, acceptance, dependencies, or check still needs preparation |
| READY | Card exists; scope, acceptance, check and required decisions are clear; dependencies DONE |
| ACTIVE | Claimed by an identifiable owner and being executed |
| BLOCKED | Cannot proceed; card records reason and unblock condition |
| DONE | Acceptance verified; result and evidence recorded |

Normal path: `DRAFT → READY → ACTIVE → DONE`. BLOCKED returns to READY when its blocker is resolved and dependencies are satisfied. Set an owner when moving to ACTIVE; no concurrent ownership is implied. Move completed rows to the archive during a milestone handoff; keep pending rows active and card paths stable. This is a manual documentation convention; harness scheduling and automation will be designed separately.

## M0 — Foundation

Completed rows and historical handoff: [archive](archive/TASKS-DONE.md).

### Planner Handoff

Historical M0 handoff: [archived section](archive/TASKS-DONE.md#planner-handoff). Current execution starts from [CURRENT](CURRENT.md).

## M1 — Transparent Responses Proxy (planning candidates)

Completed rows and historical handoff: [archive](archive/TASKS-DONE.md).

## M2 — Connector API and Conformance Baseline

Completed rows and historical handoff: [archive](archive/TASKS-DONE.md).

## M3 — Access, Accounts and Accounting

Completed rows and historical handoff: [archive](archive/TASKS-DONE.md).

## M4 — First Translation Connector

Completed rows and historical handoff: [archive](archive/TASKS-DONE.md).

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [M4-036](tasks/M4-036.md) | BLOCKED | — | M4-035 | Capture versioned real-provider smoke evidence | Blocked: requires live Anthropic credentials and external network authorization | [Direct proof](tasks/M4-036.md#verification) |
| [M4-041](tasks/M4-041.md) | BLOCKED | — | M4-040 | Extend compatible endpoint test harness for tool cycles, usage, and cancellation | Pre-live PASS `ses_ef883c23affe4sZeexy7zsSxy6`; one authorized direct call returned HTTP 200 but failed with allowlisted `other`/`unexpected_tool_name`; no retry or later calls; sanitized failure artifact retained and handoffs cleaned; live tool cycles unmet and BLOCKED; M4-036 remains BLOCKED | `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -v -count=1 ./cmd/gateway -run '^TestM4Compatible'`, validator self-test, `./scripts/check.sh`, `git diff --check`; [evidence](tasks/M4-041.md#completion-evidence) |

## M5.1 — Codex Subscription Connector

Completed rows and historical handoff: [archive](archive/TASKS-DONE.md).

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [M5.1-049](tasks/M5.1-049.md) | BLOCKED | GPT-6 Luna (M5.1-049 worker) | M5.1-040, M5.1-048 | Verify live gateway OAuth token refresh and atomic credential rotation | DEC-012-approved deferral; read-only preflight proves credential fresh; natural refresh window is 2026-10-15 21:04:55.103 UTC; no refresh/provider calls made | [Preflight evidence](tasks/M5.1-049.md#completion-evidence); due-only procedure in card |
| [M5.1-051](tasks/M5.1-051.md) | DRAFT | — | M5.1-041, M5.1-048 | Verify live multi-round encrypted reasoning continuation and replay | DEC-012-approved deferral; scoped two-request live gate remains unrun and is not authorized until separately READY, assigned, explicitly authorized, and independently reviewed | [Diagnostic procedure](tasks/M5.1-051.md#verification) |

## M5.2 — Local Runtime Connectors

Completed rows and historical handoff: [archive](archive/TASKS-DONE.md).

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [M5.2-025](tasks/M5.2-025.md) | BLOCKED | — | M5.2-003, M5.2-023 | Capture vLLM direct-versus-gateway evidence | Blocked: vLLM v0.31.0 runtime, compatible GPU, and model server unavailable in environment; unblock requires provisioned vLLM server on 127.0.0.1:8000 and operator authority | [Acceptance and verification](tasks/M5.2-025.md#verification) |

## M6 — External Plugin Runtime

All 33 M6 tasks (001–032 plus audit follow-up 033) are verified DONE and retained in the [completed registry](archive/TASKS-DONE.md#m6--external-plugin-runtime). See [acceptance/handoff](M6-HANDOFF.md), [offline operations](LOCAL-M6.md) and [integration guide](M6-INTEGRATION.md). The five older live gates above remain active under their existing requirements.

## M7 — Claude Code Subscription Connector Research

This milestone establishes whether a bounded Claude Code subscription Connector is feasible. Research tasks (M7-001 through M7-006) are DONE; M7-005 recorded an evidence-bound NO-GO disposition for implementation. M7-007's offline binding and M7-008's synthetic fixtures were completed before the explicit accountable authorization recorded in [DEC-018](../project/DECISIONS.md#dec-018--authorize-bounded-offline-m7-candidate-work); that earlier work is not represented as previously authorized implementation. DEC-018 now permits only offline synthetic implementation/testing for registered candidates M7-009 through M7-013, with no milestone expansion. All capabilities remain `Unknown`; synthetic fixture success is not provider proof; the connector is non-routable and default-gated. Live provider calls, login, real credential handling/persistence, and production routing remain prohibited absent separate authority and review.

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [M7-001](tasks/M7-001.md) | DONE | GPT-6 Luna (M7-001 worker) / ses_ec00c9f31ffebm71U5Egt2AHBR (reviewer) / Planner closure | — | Pin Claude Code client/backend sources, versions and reuse licenses | Source baseline pins Claude Code CLI 2.1.300/release commit; backend protocol/version unavailable; published usage, billing and terms statements inventoried; reviewer PASS ses_ec00c9f31ffebm71U5Egt2AHBR | [Research baseline](tasks/M7-001.md#verification) |
| [M7-002](tasks/M7-002.md) | DONE | GPT-6 Luna (M7-002 worker) / ses_ebf94a223ffe6OME8Fats8p5tr (reviewer) / Planner closure | M7-001 | Map authentication, credential lifecycle and account identity | Pinned-source auth lifecycle and host-owned auth comparison mapped with explicit unknowns; structural gaps documented; reviewer PASS ses_ebf94a223ffe6OME8Fats8p5tr | `git diff --check`, manual verification against pinned sources; [Auth research](tasks/M7-002.md#verification) |
| [M7-003](tasks/M7-003.md) | DONE | GPT-6 Luna (M7-003 worker) / ses_ebf8aca36ffe69oxLpbEt0yG4a (reviewer) / Planner closure | M7-001 | Map request, response and streaming protocol | Offline request/response/stream map records unpublished wire details as unknown and compares gaps to CONTRACT; reviewer PASS ses_ebf8aca36ffe69oxLpbEt0yG4a | `git diff --check`, manual verification against pinned sources; [Wire research](tasks/M7-003.md#verification) |
| [M7-004](tasks/M7-004.md) | DONE | GPT-6 Luna (M7-004 worker) / ses_ebf84116dffe9LL7w00YuAdr9S (reviewer) / Planner closure | M7-002, M7-003 | Assess capabilities, statefulness, retries and cancellation | Scoped compatibility and execution safety matrix maps CONTRACT capabilities to unknown; client-owned tool orchestration distinguished from backend; statefulness/unknown affinity mapped to existing fail-closed singleton routing; post-commit fallback prohibited; reviewer PASS ses_ebf84116dffe9LL7w00YuAdr9S | `git diff --check`, manual verification against pinned sources; [Capability matrix](tasks/M7-004.md#verification) |
| [M7-005](tasks/M7-005.md) | DONE | GPT-6 Luna (M7-005 worker) / ses_ebf793fcaffeKfDoZYWWFka49Q (reviewer) / Planner closure | M7-002, M7-003, M7-004 | Record reviewed feasibility decision and next-step scope | Evidence-bound NO-GO disposition for implementation on 2026-10-15 evidence; source/reuse authorization and backend protocol unestablished; all capabilities remain unknown; explicit revisit conditions documented; no implementation task created; reviewer PASS ses_ebf793fcaffeKfDoZYWWFka49Q | `git diff --check`, manual verification against pinned sources; [Go/no-go handoff](tasks/M7-005.md#verification) |
| [M7-006](tasks/M7-006.md) | DONE | GPT-6 Luna (M7-006 worker) / ses_ebf419acaffeis1nHF16zPuJEY (reviewer) / Planner closure | M7-005 | Research public 9Router Claude Code technical implementation | Public 9Router Claude Code implementation mapped in CLAUDE_CODE_CONNECTOR_RESEARCH.md; decolua/9router (MIT) and companion luqman-v1/9router-go (license unknown) paths cited; technical mechanisms distinguished from live interoperability and authorization; reviewer PASS ses_ebf419acaffeis1nHF16zPuJEY | `git diff --check`, relative links/anchors and pinned-source citation verification; [Research evidence](tasks/M7-006.md#verification) |
| [M7-007](tasks/M7-007.md) | DONE | GPT-6 Luna (M7-007 worker) / ses_ebedc6896ffek3ZDArbmN9P9X8 (reviewer) / Planner closure | M7-006 | Specify Claude Code profile, wire translation, and OAuth PKCE binding | Offline binding drafted in M7-BINDING.md; OAuth capability remains Unknown; DEC-009 user code and M3-AUTH callback input documented as v1 limitations; downstream interactive auth excluded pending ADR; reviewer PASS ses_ebedc6896ffek3ZDArbmN9P9X8 | `git diff --check`, specification audit; [M7-007 verification](tasks/M7-007.md#verification) |
| [M7-008](tasks/M7-008.md) | DONE | GPT-6 Luna (M7-008 worker) / ses_ebec8e6d6ffeaYfn75uqhj4XGR (reviewer) / Planner closure | M7-007 | Add versioned synthetic Claude Code OAuth and Messages protocol fixtures | Versioned synthetic OAuth/Messages fixture set with SHA-256 manifest and offline integrity/content regression test; native content_block_stop allowed in M7-BINDING; reviewer PASS ses_ebec8e6d6ffeaYfn75uqhj4XGR; synthetic success is not provider proof | `go test -race -v -count=1 ./internal/connector/claudecode -run '^TestFixturesIntegrity$'`; [M7-008 verification](tasks/M7-008.md#verification) |
| [M7-009](tasks/M7-009.md) | DONE | GPT-6 Luna (M7-009 worker) / ses_ebcd8e09cffePvObLzxqeIolH5 (reviewer) / Planner closure | M7-007, M7-008, M7-014 | Implement Claude Code connector lifecycle, Health, and compile-complete contract stubs | Offline connector lifecycle, Health, strict profile validation, and contract stubs in internal/connector/claudecode; all capabilities Unknown; zero invented models; reviewer PASS ses_ebcd8e09cffePvObLzxqeIolH5 | `go test -race -v -count=1 ./internal/connector/claudecode -run '^TestComponentLifecycle.*'`; [M7-009 verification](tasks/M7-009.md#verification) |
| [M7-010](tasks/M7-010.md) | DONE | GPT-6 Luna (M7-010 worker) / ses_ebac7fe52ffenVqO7W7tSR5Pop (reviewer) / Planner closure | M7-008, M7-009, M7-016 | Implement offline OAuth PKCE exchange and token refresh state machine | Offline synthetic PKCE/refresh with typed redacted failures; Core durable claim owns replay prevention; reviewer PASS ses_ebac7fe52ffenVqO7W7tSR5Pop | `go test -race -v -count=1 ./internal/connector/claudecode -run '^TestAuth.*'`; [M7-010 evidence](tasks/M7-010.md#completion-evidence) |
| [M7-011](tasks/M7-011.md) | DONE | GPT-6 Luna (M7-011 worker) / ses_ebcc89792ffeb35v7g02KOC7m9 (reviewer) / Planner closure | M7-008, M7-009 | Implement Messages request translation, SSE stream framing, and usage extraction | Offline Messages translation, bounded SSE stream framing with native content_block_stop/final events, and non-additive usage extraction; reviewer PASS ses_ebcc89792ffeb35v7g02KOC7m9 | `go test -race -v -count=1 ./internal/connector/claudecode -run '^Test(Translate\|Execute\|SSE).*'`; [M7-011 verification](tasks/M7-011.md#verification) |
| [M7-012](tasks/M7-012.md) | DONE | GPT-6 Luna (M7-012 worker) / ses_ebcb15da3ffeZGXVJV57j37HFj (reviewer) / Planner closure | M7-011 | Prove early frame delivery, IO cancellation, backpressure, and terminal accounting | Offline streaming guarantees proven: early delivery, real IO close/cancellation, on-demand backpressure, exact terminal accounting; reviewer PASS ses_ebcb15da3ffeZGXVJV57j37HFj | `go test -race -v -count=1 ./internal/connector/claudecode -run '^Test(StreamingGuarantees\|Cancellation\|Backpressure).*'`; [M7-012 verification](tasks/M7-012.md#verification) |
| [M7-013](tasks/M7-013.md) | DRAFT | — | M7-010, M7-012 | Audit offline connector conformance and non-routable gateway composition candidate | — | Conformance test suite; `go test -race -v -count=1 ./cmd/gateway -run '^TestClaudeCodeComposition.*'` |
| [M7-014](tasks/M7-014.md) | DONE | GPT-6 Luna (M7-014 worker) / ses_ebceed5c5ffe5C77WaCB5mLFpF (reviewer) / Planner closure | M7-005, M7-008 | Record accountable user resolution of M7-005 NO-GO for offline implementation | DEC-018 records accountable authorization for only offline synthetic implementation/testing of registered M7-009–013 candidates; reviewer PASS ses_ebceed5c5ffe5C77WaCB5mLFpF | `git diff --check`, relative link audit; [M7-014 verification](tasks/M7-014.md#verification) |
| [M7-015](tasks/M7-015.md) | DONE | GPT-6 Luna (M7-015 worker) / ses_ebb06a177ffek3ALJf4wWMfZcL (reviewer) / Planner closure | M7-007, M7-014 | Record ADR and synchronize specs for manual pasted callback URL flow | DEC-019 ADR and CONTRACT/M2/M3/M6/M7 synchronized for manual pasted callback URL flow; offline scope extends only to M7-016/010/013; reviewer PASS ses_ebb06a177ffek3ALJf4wWMfZcL | `git diff --check`, relative link audit; [M7-015 verification](tasks/M7-015.md#verification) |
| [M7-016](tasks/M7-016.md) | DONE | GPT-6 Luna (M7-016 worker) / ses_ebaea83faffeZy3QjUmxz14uhu (reviewer) / Planner closure | M7-015 | Implement generic AuthRequest.CallbackInput, coordinator/admin stdin, and IPC plumbing | Generic callback input, coordinator validation and logger redaction, bounded admin stdin, and IPC propagation implemented; reviewer PASS ses_ebaea83faffeZy3QjUmxz14uhu | `go test -race -v -count=1 ./internal/core -run '^TestAuth.*'`; [M7-016 verification](tasks/M7-016.md#verification) |

All M7 research tasks (M7-001 through M7-006), offline specification candidate [M7-007](tasks/M7-007.md), synthetic fixtures [M7-008](tasks/M7-008.md), and accountable resolution [M7-014](tasks/M7-014.md) are DONE. M7-007 and M7-008 predate explicit authorization; DEC-018 resolves this honestly without backdating authority. In [M7-007](tasks/M7-007.md), OAuth PKCE candidate mechanics are documented; the original v1 limitations on browser user actions and callback input are resolved at the specification level by DEC-019. In [M7-008](tasks/M7-008.md), versioned synthetic OAuth/Messages fixtures have a SHA-256 manifest and integrity/content regression test; native content_block_stop is allowed in M7-BINDING; synthetic success is not provider proof. [M7-014](tasks/M7-014.md) records accountable user authorization for offline synthetic candidate work; user direction on 2026-10-16 adopting the 9Router manual callback flow explicitly authorizes extending this offline synthetic scope to the minimal generic runtime/admin/IPC plumbing in [M7-016](tasks/M7-016.md) alongside [M7-010](tasks/M7-010.md) and [M7-013](tasks/M7-013.md). M7-009, [M7-010](tasks/M7-010.md), M7-011, M7-012, [M7-015](tasks/M7-015.md), and [M7-016](tasks/M7-016.md) are DONE; [M7-013](tasks/M7-013.md) remains DRAFT. Live calls, login, real credential handling/persistence, traffic capture, binary reverse engineering, capability promotion beyond `Unknown`, and production routing remain strictly prohibited absent separate authorization and review.
