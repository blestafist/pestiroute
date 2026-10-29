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

[M10 brief](M10-ANTIGRAVITY.md). Branch `dev-m10-antigravity`; exact filter `/next 10 M10`. Only the initial audit/design is READY.

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [M10-001](tasks/M10-001.md) | READY | — | — | Pin current Antigravity source and authentication evidence | — | [Verification](tasks/M10-001.md#verification) |
| [M10-002](tasks/M10-002.md) | DRAFT | — | M10-001 | Bind one Antigravity execution slice | — | [Verification](tasks/M10-002.md#verification) |
| [M10-003](tasks/M10-003.md) | DRAFT | — | M10-002 | Prepare provenance-labelled fixtures and slice decision | — | [Verification](tasks/M10-003.md#verification) |
| [M10-004](tasks/M10-004.md) | DRAFT | — | M10-003 | Implement the minimal offline Antigravity vertical slice | — | [Verification](tasks/M10-004.md#verification) |
| [M10-005](tasks/M10-005.md) | DRAFT | — | M10-004 | Publish Antigravity offline handoff and next auth/tools scope | — | [Verification](tasks/M10-005.md#verification) |

## M11 — Administrative HTTP API

[M11 brief](M11-ADMIN-API.md). Branch `dev-m11-admin-api`; exact filter `/next 10 M11`. Administrative HTTP binding frozen in M11-001; all seven tasks M11-001 through M11-007 verified and DONE. Milestone scope complete.

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [M11-001](tasks/M11-001.md) | DONE | GPT-6 Luna (M11-001 worker) | — | Freeze the administrative HTTP binding | Administrative HTTP binding and OpenAPI 3.1.0 specification frozen; verified parity against existing CLI/store and YAML syntax | [Verification](tasks/M11-001.md#verification) |
| [M11-002](tasks/M11-002.md) | DONE | GPT-6 Luna (M11-002 worker) | M11-001 | Add opt-in authenticated admin listener | Opt-in authenticated admin listener implemented with RFC 6750 token validation, Host/Origin protection, TLS enforcement on non-loopback binds, and concurrent graceful shutdown; full pinned gate passed | `GOFLAGS=-p=1 ./scripts/check.sh`; [evidence](tasks/M11-002.md#completion-evidence) |
| [M11-003](tasks/M11-003.md) | DONE | GPT-6 Luna (M11-003 worker) | M11-002 | Expose redacted status topology and ledger views | Legacy account redaction, shared M2 database, sub-millisecond bounds, and data-backed usage reads verified; independent reviewer full pinned gate PASS | [Verification](tasks/M11-003.md#verification) |
| [M11-004](tasks/M11-004.md) | DONE | GPT-6 Luna (M11-004 worker) / ses_eb417d9ebffeXh81TJrhEweaKs (reviewer) | M11-003 | Expose account and write-only credential management | Account lifecycle and write-only credential operations verified; AES-256-GCM envelope encryption, CAS checks, transient buffer zeroing, and DEC-007 disablement verified; reviewer PASS ses_eb417d9ebffeXh81TJrhEweaKs | [Verification](tasks/M11-004.md#verification) |
| [M11-005](tasks/M11-005.md) | DONE | GPT-6 Luna (M11-005 worker) / ses_eb3e29284ffeAoQYH3WjFpp5Iv (reviewer) | M11-004 | Expose virtual-key and policy administration | Policy revisions and virtual key CRUD/reassign verified; CAS conflict and disable-only enforced; one-time secret emission, permanent revocation, and immediate inference admission verified; reviewer PASS ses_eb3e29284ffeAoQYH3WjFpp5Iv | `GOFLAGS=-p=1 ./scripts/check.sh`; [evidence](tasks/M11-005.md#completion-evidence) |
| [M11-006](tasks/M11-006.md) | DONE | GPT-6 Luna (M11-006 worker) / ses_eb395492affeBp1FWFG6MzUTrI (reviewer) | M11-004 | Bridge the existing auth coordinator to admin HTTP | Generic authenticated lifecycle HTTP wired to composed Core coordinator; sessions/continue/refresh, replay rejection, disabled/quarantined handling, and persistent DB startup verified; reviewer PASS ses_eb395492affeBp1FWFG6MzUTrI | `GOFLAGS=-p=1 ./scripts/check.sh`; [evidence](tasks/M11-006.md#completion-evidence) |
| [M11-007](tasks/M11-007.md) | DONE | GPT-6 Luna (M11-007 worker) / ses_eb3428f01ffe1CEnrnNvZHDZtR (reviewer) | M11-005, M11-006 | Verify and publish the admin API integration handoff | OpenAPI/HTTP parity verified, regression tests added for account defaults, policy revision checks, and unconfigured static mount; M11-HANDOFF.md published; reviewer PASS ses_eb3428f01ffe1CEnrnNvZHDZtR | `GOFLAGS=-p=1 ./scripts/check.sh`; [evidence](tasks/M11-007.md#completion-evidence) |

## M12 — WebUI

[M12 brief](M12-WEBUI.md). Branch `dev-m12-webui`; exact filter `/next 10 M12`. M12-001 through M12-008 and M12-011 are DONE; M12-009–010 complete operator UX. Execute 009 → 010.

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [M12-001](tasks/M12-001.md) | DONE | GPT-6 Luna (M12-001 worker) | — | Design complete operator workflows and UI states | Operator UX, 6-screen state matrix, M11 API inventory and embed fallback specified in M12-UX.md | [Verification](tasks/M12-001.md#completion-evidence) |
| [M12-002](tasks/M12-002.md) | DONE | GPT-6 Luna (M12-002 worker) / ses_eb521fa12ffeqQz4SFBTIhg8EM (reviewer) | M12-001 | Build the WebUI shell with a fixture adapter | React 19/TS/Vite shell in web/, 6 route stubs, fixture adapter with state selector and demo badge | [Verification](tasks/M12-002.md#completion-evidence) |
| [M12-003](tasks/M12-003.md) | DONE | GPT-6 Luna (M12-003 worker) / ses_eb506708fffeDFDsgJDFBQhMXE (reviewer) | M12-002 | Implement account and key management workflows on fixtures | Fixture account/key workflows, write-only credentials, one-time raw secret modal, named revoke confirmation, CAS conflict handling, rendered tri-state badges | [Verification](tasks/M12-003.md#completion-evidence) |
| [M12-004](tasks/M12-004.md) | DONE | GPT-6 Luna (M12-004 worker) / ses_eb4f075e1ffeSd4KXF3FkqjLFT (reviewer) | M12-003 | Implement overview usage routing and diagnostics screens | Overview readiness/counts, filterable/paginated usage ledger with attempt/fallback details, routing topology, settings/diagnostics, and view cache clearing | [Verification](tasks/M12-004.md#completion-evidence) |
| [M12-005](tasks/M12-005.md) | DONE | GPT-6 Luna (M12-005 worker) / reviewer PASS | M12-003, M12-004, M11-007 | Integrate the accepted admin API and authentication | Live AdminApi client and in-memory auth integrated across 6 operator screens; 401 unauthenticated transitions, 409 CAS input preservation, 403/404/413/500/503 alert rendering, advertised OAuth workflows, and non-persistence verified; reviewer PASS | `npm --prefix web test`; [evidence](tasks/M12-005.md#completion-evidence) |
| [M12-006](tasks/M12-006.md) | DONE | GPT-6 Luna (M12-006 worker) / reviewer PASS / Planner closure | M12-005 | Package embedded UI assets and verify complete operator E2E | Embedded UI, standard MIME serving, live modal accessibility, SQLite gateway and packaged Chromium E2E verified; Go sweeps pass | `TMPDIR=/tmp/m12-6/t GOFLAGS=-p=1 python3 scripts/e2e-m12-webui.py`; [evidence](tasks/M12-006.md#completion-evidence) |
| [M12-007](tasks/M12-007.md) | DONE | GPT-6 Luna (M12-007 worker) / reviewer PASS / Planner closure | M12-006 | Establish operator design foundation, theme tokens, and shell layout | Design foundation tokens, theme-aware technical shell, accessible unpersisted login card, contrast/responsive checks and 47 WebUI tests verified | `npm --prefix web run build && npm --prefix web test`; [evidence](tasks/M12-007.md#completion-evidence) |
| [M12-008](tasks/M12-008.md) | DONE | GPT-6 Luna (M12-008 worker) / reviewer PASS / Planner closure | M12-007 | Redesign core operator screens: Overview, Connections, Keys & Policies | Redesigned Overview, Connections, Keys & Policies in live and fixture modes; live write-only credentials, CAS 409 preservation, one-time secret modal, named revoke confirmation, known-zero distinction, and plain View Usage links verified; reviewer PASS | `npm --prefix web run build && npm --prefix web test`; [evidence](tasks/M12-008.md#completion-evidence) |
| [M12-009](tasks/M12-009.md) | DRAFT | — | M12-011 | Redesign data and diagnostics screens: Usage, Routing, Settings | — | `npm --prefix web run build && npm --prefix web test`; [card](tasks/M12-009.md) |
| [M12-010](tasks/M12-010.md) | DRAFT | — | M12-009 | Visual polish, responsive ergonomics, accessibility audit, and E2E regression | — | `npm --prefix web run build && npm --prefix web test`; [card](tasks/M12-010.md) |
| [M12-011](tasks/M12-011.md) | DONE | GPT-6 Luna (M12-011 worker) / reviewer PASS / Planner closure | M12-008 | Complete real ledger filtering, server pagination and attempt details | Real ledger pagination (limit/offset/total), account/key/date filtering, summary alignment, joined attempt details with nullable usage, out-of-order suppression, out-of-range recovery, and session invalidation verified with 59 WebUI tests; reviewer PASS | `npm --prefix web run build && npm --prefix web test`; [evidence](tasks/M12-011.md#completion-evidence) |

## M13 — Deployment and Recovery

[M13 brief](M13-OPERATIONS.md). Branch `dev-m13-operations`; exact filter `/next 10 M13`. All M13 tasks (M13-001 through M13-004) are DONE.

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [M13-001](tasks/M13-001.md) | DONE | GPT-6 Luna (M13-001 worker) / ses_eb52fd9c4ffe1EiU8LO7uiQktm (reviewer) / Planner closure | — | Bind single-node deployment and recovery acceptance | Audited and bound Linux/amd64 prerequisites, unprivileged execution, lock sidecar, WAL backup safety, and shutdown recovery in M13-BINDING.md; reviewer PASS ses_eb52fd9c4ffe1EiU8LO7uiQktm | [Verification](tasks/M13-001.md#verification) |
| [M13-002](tasks/M13-002.md) | DONE | GPT-6 Luna (M13-002 worker) / ses_eb51a5672ffe3MEvN6OrFdVaDV (reviewer) / Planner closure | M13-001 | Package reproducible single-node startup | Packaged reproducible static Linux/amd64 build, unprivileged Dockerfile, systemd service unit, deployment guide, and packaging smoke verification with live WAL/SHM600 and Docker CLI smoke; reviewer PASS ses_eb51a5672ffe3MEvN6OrFdVaDV | [Verification](tasks/M13-002.md#verification) |
| [M13-003](tasks/M13-003.md) | DONE | GPT-6 Luna (M13-003 worker) / ses_eb4cfa82fffenIQ2VutUNjSF4p (reviewer) / Planner closure | M13-001, M13-002 | Prove consistent backup restore and upgrade recovery | Verified online SQLite backup, distinct-path restore, checked integrity/FK validation, preserved account/key/credential/usage state, fail-closed key handling, stopped v5→v6 migration, and untouched-backup rollback recovery; reviewer PASS ses_eb4cfa82fffenIQ2VutUNjSF4p | [Verification](tasks/M13-003.md#verification) |
| [M13-004](tasks/M13-004.md) | DONE | GPT-6 Luna (M13-004 worker) / ses_eb4a14701ffejxyXJdynVUxFi5 (reviewer) / Planner closure | M13-002, M13-003 | Run packaged offline release acceptance and publish operations handoff | Byte-exact payloads and stable usage history verified across restart/restore; local M6 gates and full checks pass; hosted baseline 39047526958 verified read-only; reviewer PASS ses_eb4a14701ffejxyXJdynVUxFi5 | [Verification](tasks/M13-004.md#verification) |
