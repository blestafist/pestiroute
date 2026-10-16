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

All 16 M7 research/offline candidate tasks are DONE and retained in the
[completed registry](archive/TASKS-DONE.md#m7--claude-code-subscription-connector-research).
[PR #15](https://github.com/blestafist/pestiroute/pull/15) merged as `1a76494`;
[hosted PR CI](https://github.com/blestafist/pestiroute/actions/runs/38951339646)
passed for source `44f5a29`. Public Execute still fails closed, gateway routing
is disabled, and capabilities remain Unknown. M8 owns the next integration plan;
M7's live prerequisites remain unwaived.

## M8 — Claude Code Execution and Gateway Integration

[Milestone plan](M8-CLAUDE-INTEGRATION.md). Lane: `dev-m8`; select with `/next N M8`
in its own worktree. Only the design audit is initially executable. Required
scope/contract decisions precede implementation; live work needs its own later
card and authority. Later phases remain in the plan until decomposition is justified.

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [M8-001](tasks/M8-001.md) | DONE | GPT-6 Luna (M8-001 worker) / ses_eb97adc7cffeJKggMCppVKni1a (reviewer) / Planner closure | M7-013 | Bind scoped Claude execution, lifecycle, and enablement decisions | Offline execution binding and ADR proposal completed in M8-BINDING.md; downstream tasks remain DRAFT pending offline scope authority and transport/beta resolution; reviewer PASS ses_eb97adc7cffeJKggMCppVKni1a | `TMPDIR=/tmp/pestiroute-m8.mIzAVt/opencode GOFLAGS=-p=1 go test -race -count=1 ./internal/connector/claudecode` && `git diff --check`; [evidence](tasks/M8-001.md#completion-evidence) |
| [M8-002](tasks/M8-002.md) | DONE | GPT-6 Luna (M8-002 worker) / ses_eb9647671ffeMgsiLCXh1bsNzO (reviewer) / Planner closure | M8-001 | Connect public Execute to a reviewed synthetic Messages transport | Connected public Execute to package-private offline fake transport; fixed .invalid endpoint and synthetic-beta header; pre-dispatch validation, bounded error drain, body closure, and credential extraction verified; descriptor exposes authenticate and execute; reviewer PASS ses_eb9647671ffeMgsiLCXh1bsNzO | `TMPDIR=/tmp/pestiroute-m8.mIzAVt/opencode GOFLAGS=-p=1 go test -race -count=1 ./internal/connector/claudecode` && `git diff --check`; [evidence](tasks/M8-002.md#completion-evidence) |
| [M8-003](tasks/M8-003.md) | DRAFT | — | M8-002 | Prove public execution lifecycle, bounds, and accounting | — | [Lifecycle verification](tasks/M8-003.md#verification) |

## M9 — Chat Completions Client Compatibility

[Milestone plan](M9-CHAT-COMPLETIONS.md). Lane: `dev-m9`; select with `/next N M9`
in its own worktree. Independent of M8's Claude/live gates. Only the binding audit
is initially READY; new package and gateway verification commands must be settled
before implementation readiness. See [parallel ownership](PARALLEL.md).

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [M9-001](tasks/M9-001.md) | READY | — | — | Bind native Chat adapter/Connector, framing, capabilities, and protected routing | — | [Binding verification](tasks/M9-001.md#verification) |
| [M9-002](tasks/M9-002.md) | DRAFT | — | M9-001 | Implement native Chat adapter and fake-backend transport slice | — | [Slice verification](tasks/M9-002.md#verification) |
| [M9-003](tasks/M9-003.md) | DRAFT | — | M9-002 | Expose protected Chat endpoint with explicit protocol routing and accounting | — | [Gateway verification](tasks/M9-003.md#verification) |
