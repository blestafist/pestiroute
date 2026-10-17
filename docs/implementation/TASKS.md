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
| [M5.1-049](tasks/M5.1-049.md) | BLOCKED | GPT-6.1 Sol (operator recovery) | M5.1-040, M5.1-048 | Verify live gateway OAuth token refresh and atomic credential rotation | Authorized explicit refresh passed Oct 17: token rotation, revision 2→3, restart decryption; natural trigger still unrun, new window Oct 27 11:24:32.933–11:25:32.933 UTC; no inference | [Refresh evidence](tasks/M5.1-049.md#completion-evidence); separate explicit/natural procedures in card |
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
in its own worktree. M8-001–007 are complete; the offline slice is accepted under
DEC-020. M8-008/009 retain the deferred live and enablement work. See [handoff](M8-HANDOFF.md).

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [M8-001](tasks/M8-001.md) | DONE | GPT-6 Luna (M8-001 worker) / ses_eb97adc7cffeJKggMCppVKni1a (reviewer) / Planner closure | M7-013 | Bind scoped Claude execution, lifecycle, and enablement decisions | Offline execution binding and ADR proposal completed in M8-BINDING.md; downstream tasks remain DRAFT pending offline scope authority and transport/beta resolution; reviewer PASS ses_eb97adc7cffeJKggMCppVKni1a | `TMPDIR=/tmp/pestiroute-m8.mIzAVt/opencode GOFLAGS=-p=1 go test -race -count=1 ./internal/connector/claudecode` && `git diff --check`; [evidence](tasks/M8-001.md#completion-evidence) |
| [M8-002](tasks/M8-002.md) | DONE | GPT-6 Luna (M8-002 worker) / ses_eb9647671ffeMgsiLCXh1bsNzO (reviewer) / Planner closure | M8-001 | Connect public Execute to a reviewed synthetic Messages transport | Connected public Execute to package-private offline fake transport; fixed .invalid endpoint and synthetic-beta header; pre-dispatch validation, bounded error drain, body closure, and credential extraction verified; descriptor exposes authenticate and execute; reviewer PASS ses_eb9647671ffeMgsiLCXh1bsNzO | `TMPDIR=/tmp/pestiroute-m8.mIzAVt/opencode GOFLAGS=-p=1 go test -race -count=1 ./internal/connector/claudecode` && `git diff --check`; [evidence](tasks/M8-002.md#completion-evidence) |
| [M8-003](tasks/M8-003.md) | DONE | GPT-6 Luna (M8-003 worker) / ses_eb9555c2affe8Q07iZneYsSL2j (reviewer) / Planner closure | M8-002 | Prove public execution lifecycle, bounds, and accounting | Verified incremental delivery with bounded backpressure, blocked IO cancellation, frame sequencing, single finalization, bounded over-limit event handling, and zero redispatch; reviewer PASS ses_eb9555c2affe8Q07iZneYsSL2j | `TMPDIR=/tmp/pestiroute-m8.mIzAVt/opencode GOFLAGS=-p=1 go test -race -count=1 ./internal/connector/claudecode ./internal/conformance -run 'Claude'` && `git diff --check`; [evidence](tasks/M8-003.md#completion-evidence) |
| [M8-004](tasks/M8-004.md) | DONE | GPT-6 Luna (M8-004 worker) / ses_eb945874effepQ5h3Av5h0AYMp (reviewer) / Planner closure | M8-003 | Map multi-round function-call and result history in synthetic Claude execution | Multi-round function_call and function_call_output mapped to tool_use and tool_result blocks; turn grouping, chronology guards, and pre-dispatch rejection of unsupported reasoning/state verified; zero dispatch for invalid history; reviewer PASS ses_eb945874effepQ5h3Av5h0AYMp | `TMPDIR=/tmp/pestiroute-m8.mIzAVt/opencode GOFLAGS=-p=1 go test -race -count=1 ./internal/connector/claudecode -run 'ToolHistory|Execute.*Tool'` && `git diff --check`; [evidence](tasks/M8-004.md#completion-evidence) |
| [M8-005](tasks/M8-005.md) | DONE | GPT-6 Luna (M8-005 worker) / ses_eb6ae757fffertu2NF0cqup3m8 (reviewer) / Planner closure | M8-004 | Bind Claude Code admin OAuth callback flow and encrypted SQLite credential lifecycle | Claude Code account registered in admin auth CLI; start/callback/refresh and AES-256-GCM persistence verified against local TLS .invalid transport; default fail-closed without transport; reviewer PASS ses_eb6ae757fffertu2NF0cqup3m8 | `TMPDIR=/tmp/pestiroute-m8.mIzAVt/opencode GOFLAGS=-p=1 go test -race -count=1 ./cmd/gateway -run '^TestAdminAuthClaudeCode'` && `git diff --check`; [evidence](tasks/M8-005.md#completion-evidence) |
| [M8-006](tasks/M8-006.md) | DONE | GPT-6 Luna (M8-006 worker) / ses_eb683a91effe7dyXnEUwYGWOYs (reviewer) / Planner closure | M8-005 | Verify protected gateway Responses admission and route eligibility under offline fake fixtures | Real composition in main.go/protected_startup initializes Claude Code and resolves oauth credentials; admission split proves unwrapped Unknown capability rejected by route eligibility while fixture-scoped wrapper executes end-to-end streaming and accounting; uncomposed default and YAML fail-closed preserved; reviewer PASS ses_eb683a91effe7dyXnEUwYGWOYs | `TMPDIR=/tmp/pestiroute-m8.mIzAVt/opencode GOFLAGS=-p=1 go test -race -count=1 ./cmd/gateway -run '^TestClaudeCode'` && `git diff --check`; [evidence](tasks/M8-006.md#completion-evidence) |
| [M8-007](tasks/M8-007.md) | DONE | GPT-6 Luna (M8-007 worker) / ses_eb65ba94affevvkE4alNG15Gfd (reviewer) / Planner closure | M8-006 | Support configurable protected credential_id in Claude Code connector settings | Configurable protected credential reference, oauth default, and whitespace rejection verified end-to-end with encrypted SQLite fixture; YAML remains unsupported; reviewer PASS ses_eb65ba94affevvkE4alNG15Gfd | `TMPDIR=/tmp/pestiroute-m8.mIzAVt/opencode GOFLAGS=-p=1 go test -race -count=1 ./cmd/gateway -run '^TestClaudeCode'` && `git diff --check`; [evidence](tasks/M8-007.md#completion-evidence) |
| [M8-008](tasks/M8-008.md) | BLOCKED | — | M8-007 | Establish current provider binding and bounded live evidence | Deferred: no Claude account; synthetic endpoints are not a live implementation | [Live prerequisites](tasks/M8-008.md#verification) |
| [M8-009](tasks/M8-009.md) | DRAFT | — | M8-008 | Decide scoped production enablement and publish release matrix/runbook | — | [Release verification](tasks/M8-009.md#verification) |

## M9 — Chat Completions Client Compatibility

[Milestone plan](M9-CHAT-COMPLETIONS.md). Lane: `dev-m9`; select with `/next N M9`
in its own worktree. Independent of M8's Claude/live gates. Only the binding audit
is initially READY; new package and gateway verification commands must be settled
before implementation readiness. See [parallel ownership](PARALLEL.md).

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [M9-001](tasks/M9-001.md) | DONE | GPT-6 Luna (M9-001 worker) | — | Bind native Chat adapter/Connector, framing, capabilities, and protected routing | [Native Chat Completions binding](M9-BINDING.md) | [Binding verification](tasks/M9-001.md#verification) |
| [M9-002](tasks/M9-002.md) | DONE | GPT-6 Luna (M9-002 worker) | M9-001 | Implement native Chat adapter and fake-backend transport slice | Native Chat adapter, connector and conformance slice; opaque payloads, streaming, cancellation, usage mapping | [Slice verification](tasks/M9-002.md#verification) |
| [M9-003](tasks/M9-003.md) | DONE | GPT-6 Luna (M9-003 worker) | M9-002 | Expose protected Chat endpoint with explicit protocol routing and accounting | Protected Chat endpoint, YAML/topology validation, pre-dispatch denials, token accounting settlement | [Gateway verification](tasks/M9-003.md#verification) |
| [M9-004](tasks/M9-004.md) | DONE | GPT-6 Luna (M9-004 worker) | M9-003 | Verify Chat Completions agent workflows, tool calling, and structured formats | Multi-round tools, streaming deltas, parallel gating, choices, structured formats | [Agent workflow verification](tasks/M9-004.md#completion-evidence) |
| [M9-005](tasks/M9-005.md) | DONE | GPT-6 Luna (M9-005 worker) | M9-004, M9-006 | Publish Chat Completions compatibility matrix, offline smoke procedure, and milestone handoff | Compatibility matrix, full CLI JSON/SSE/tools smoke procedure, and milestone handoff verified | [Handoff verification](tasks/M9-005.md#completion-evidence) |
| [M9-006](tasks/M9-006.md) | DONE | OpenCode worker / reviewer ses_eb6b182e3ffe2KSmxDhUre2lbo | M9-004 | Add explicit YAML capability declarations for native Chat connector | YAML capabilities mapping, tri-state validation, protected startup propagation, and gating verified | [Gateway capability verification](tasks/M9-006.md#completion-evidence) |
| [M9-007](tasks/M9-007.md) | DONE | GPT-6 Luna (M9-007 worker) / reviewer ses_eb660fd21ffeXVr5EyzjOW0D38 | M9-005, M9-006 | Enforce Chat stream idle timeout, test mixed protocol isolation, and clarify opaque routing | Chat stream idle timeout enforced on SSE/JSON, stall/reset tests verified, mixed protocol isolation tested, opaque routing clarified | [Verification](tasks/M9-007.md#verification) |

## M10 — Google Antigravity Connector

[M10 brief](M10-ANTIGRAVITY.md). Branch `dev-m10-antigravity`; exact filter `/next 10 M10`. M10-001–006 DONE (M10-007 DRAFT).

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [M10-001](tasks/M10-001.md) | DONE | GPT-6 Luna (M10-001 worker) | — | Pin current Antigravity source and authentication evidence | Dated official and pinned MIT plugin research; unsupported wire facts explicit | [git diff --check](tasks/M10-001.md#completion-evidence) |
| [M10-002](tasks/M10-002.md) | DONE | GPT-6 Luna (M10-002 worker) | M10-001 | Bind one Antigravity execution slice in M10-BINDING.md | Normative text/stream candidate binding, error/retry matrix, 403 unauthenticated mapping; no contract ADR needed | [git diff --check](tasks/M10-002.md#completion-evidence) |
| [M10-003](tasks/M10-003.md) | DONE | GPT-6 Luna (M10-003 worker) / ses_eb51c4692ffehzwZvfUZXc64n7 (reviewer) / Planner closure | M10-002 | Prepare provenance-labelled fixtures and slice decision | Synthetic fixture corpus, manifest integrity test, and fake-only GO decision in M10-SLICE-DECISION.md; 403 unauthenticated mapping preserved | `TMPDIR=/tmp/pestiroute-lane.hGmeZS/opencode GOFLAGS=-p=1 go test -race -count=1 ./internal/connector/antigravity/...` && `git diff --check`; [evidence](tasks/M10-003.md#completion-evidence) |
| [M10-004](tasks/M10-004.md) | DONE | GPT-6 Luna (M10-004 worker) / ses_eb50db93cffeLuOxLXZleGQ9RU (reviewer) / Planner closure | M10-003 | Implement the minimal offline Antigravity vertical slice | Offline Antigravity text/stream slice verified; fake transport, credentials, incremental SSE, usage & error mapping pass | `TMPDIR=/tmp/pestiroute-lane.hGmeZS/antigravity GOFLAGS=-p=1 go test -race -count=3 ./internal/connector/antigravity/...` && `git diff --check`; [evidence](tasks/M10-004.md#completion-evidence) |
| [M10-005](tasks/M10-005.md) | DONE | GPT-6 Luna (M10-005 worker) / ses_eb4ff2737ffeXzROhQfbTcVyMl (reviewer) / Planner closure | M10-004 | Publish Antigravity offline handoff and next auth/tools scope | Offline matrix/handoff published; fake-only scope, capability Unknowns, event omissions, and DRAFT research follow-ups verified | `TMPDIR=/tmp/pestiroute-lane.hGmeZS/opencode GOFLAGS=-p=1 go test -race -count=1 ./internal/connector/antigravity/...` && `git diff --check`; [evidence](tasks/M10-005.md#completion-evidence) |
| [M10-006](tasks/M10-006.md) | DONE | GPT-6 Luna (M10-006 worker) / ses_eb4f0317affeF68uEGHOSnlleP (reviewer) / Planner closure | M10-005 | Research and bind Antigravity tool calling, history, and thought-signature constraints | Tool candidate binding specified; signature-dependent replay BLOCKED; synthetic multi-round cases defined | `TMPDIR=/tmp/pestiroute-lane.hGmeZS/opencode GOFLAGS=-p=1 go test -race -count=1 ./internal/connector/antigravity/...` && `git diff --check`; [evidence](tasks/M10-006.md#completion-evidence) |
| [M10-007](tasks/M10-007.md) | DRAFT | — | M10-005 | Research and bind Antigravity OAuth refresh lifecycle and Authenticate state machine | — | `TMPDIR=/tmp/pestiroute-lane.hGmeZS/opencode GOFLAGS=-p=1 go test -race -count=1 ./internal/connector/antigravity/...` && `git diff --check`; [Verification](tasks/M10-007.md#verification) |

## M11 — Administrative HTTP API

[M11 brief](M11-ADMIN-API.md). Branch `dev-m11-admin-api`; exact filter `/next 10 M11`. Only the initial audit/design is READY.

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [M11-001](tasks/M11-001.md) | READY | — | — | Freeze the administrative HTTP binding | — | [Verification](tasks/M11-001.md#verification) |
| [M11-002](tasks/M11-002.md) | DRAFT | — | M11-001 | Add opt-in authenticated admin listener | — | [Verification](tasks/M11-002.md#verification) |
| [M11-003](tasks/M11-003.md) | DRAFT | — | M11-002 | Expose redacted status topology and ledger views | — | [Verification](tasks/M11-003.md#verification) |
| [M11-004](tasks/M11-004.md) | DRAFT | — | M11-003 | Expose account and write-only credential management | — | [Verification](tasks/M11-004.md#verification) |
| [M11-005](tasks/M11-005.md) | DRAFT | — | M11-004 | Expose virtual-key and policy administration | — | [Verification](tasks/M11-005.md#verification) |
| [M11-006](tasks/M11-006.md) | DRAFT | — | M11-004 | Bridge the existing auth coordinator to admin HTTP | — | [Verification](tasks/M11-006.md#verification) |
| [M11-007](tasks/M11-007.md) | DRAFT | — | M11-005, M11-006 | Verify and publish the admin API integration handoff | — | [Verification](tasks/M11-007.md#verification) |

## M12 — WebUI

[M12 brief](M12-WEBUI.md). Branch `dev-m12-webui`; exact filter `/next 10 M12`. Only the initial audit/design is READY.

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [M12-001](tasks/M12-001.md) | READY | — | — | Design complete operator workflows and UI states | — | [Verification](tasks/M12-001.md#verification) |
| [M12-002](tasks/M12-002.md) | DRAFT | — | M12-001 | Build the WebUI shell with a fixture adapter | — | [Verification](tasks/M12-002.md#verification) |
| [M12-003](tasks/M12-003.md) | DRAFT | — | M12-002 | Implement account and key management workflows on fixtures | — | [Verification](tasks/M12-003.md#verification) |
| [M12-004](tasks/M12-004.md) | DRAFT | — | M12-002 | Implement overview usage routing and diagnostics screens | — | [Verification](tasks/M12-004.md#verification) |
| [M12-005](tasks/M12-005.md) | DRAFT | — | M12-003, M12-004, M11-007 | Integrate the accepted admin API and authentication | — | [Verification](tasks/M12-005.md#verification) |
| [M12-006](tasks/M12-006.md) | DRAFT | — | M12-005 | Package embedded UI assets and verify complete operator E2E | — | [Verification](tasks/M12-006.md#verification) |

## M13 — Deployment and Recovery

[M13 brief](M13-OPERATIONS.md). Branch `dev-m13-operations`; exact filter `/next 10 M13`. Only the initial audit/design is READY.

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [M13-001](tasks/M13-001.md) | READY | — | — | Bind single-node deployment and recovery acceptance | — | [Verification](tasks/M13-001.md#verification) |
| [M13-002](tasks/M13-002.md) | DRAFT | — | M13-001 | Package reproducible single-node startup | — | [Verification](tasks/M13-002.md#verification) |
| [M13-003](tasks/M13-003.md) | DRAFT | — | M13-001 | Prove consistent backup restore and upgrade recovery | — | [Verification](tasks/M13-003.md#verification) |
| [M13-004](tasks/M13-004.md) | DRAFT | — | M13-002, M13-003 | Run packaged offline release acceptance and publish operations handoff | — | [Verification](tasks/M13-004.md#verification) |
