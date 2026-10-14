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

[M6 plan](M6-EXTERNAL.md) owns the execution gates and scope. One READY card per Worker/Reviewer session. Only M6-001 is initially READY. Cards 002–006 are transport-independent planning/spike work; remaining rows are bounded DRAFT candidates without cards until M6-006 fixes the binding. Before promotion, create a card from [TEMPLATE](tasks/TEMPLATE.md), replace the descriptive check with exact commands and measured bounds, and reassess actual predecessors. A candidate row never authorizes its dependencies or production implementation of both transports. Retained live gates above are not M6 dependencies.

| ID | Status | Owner | Depends | Scope | Result | Check |
| --- | --- | --- | --- | --- | --- | --- |
| [M6-001](tasks/M6-001.md) | DONE | GPT-6 Luna (M6-001 worker) / ses_ec8fec907ffe8XdpMCiCkV3rSn (reviewer) / Planner closure | M5.2-028 | Audit the merged M5.2 baseline and M6 seams: Record exact revision, clean checks, contract-to-code map and retained open gates | Audited merge base bea67fe (PR #13) and seams; code mapping, Models host-dispatch gap, Authenticate scope and five retained open gates recorded; shared checks clean | `go version`, `./scripts/check.sh`, `git diff --check`; [evidence](tasks/M6-001.md#completion-evidence) |
| [M6-002](tasks/M6-002.md) | DONE | GPT-6 Luna (M6-002 worker) / ses_ec8e5dc4effehsM1NIe6GjNlbO (reviewer) / Planner closure | M6-001 | Define the common IPC comparison scenario: One bounded long-stream/cancellation/callback scenario and reproducible in-process control | Defined deterministic 16 MiB opaque control, stalled callbacks, joint cancellation release, and numeric budgets in M6-EXTERNAL; verified control and shared checks clean | `go test ./internal/conformance -run '^TestM6IPCScenario'`, `./scripts/check.sh`, `git diff --check`; [evidence](tasks/M6-002.md#completion-evidence) |
| [M6-003](tasks/M6-003.md) | DONE | OpenCode subagent (M6-003 worker) / ses_ec8ceeb0fffexzxj24bgdgwSgU (reviewer) / Planner closure | M6-002 | Run the framed-stdio IPC spike: Disposable child-process framing, long stream, cancellation and reverse services measured | Framed stdio child spike measured on M6-002 workload; 16 MiB stream, reverse credentials, stalled HTTP streaming, joint cancel release, queue/memory bounds, and fail-stop reaping verified; raw metrics recorded in card | `go test -race -count=1 ./internal/conformance/...`, `./scripts/check.sh`, `git diff --check`; [evidence](tasks/M6-003.md#completion-evidence) |
| [M6-004](tasks/M6-004.md) | DONE | GPT-6 Luna (M6-004 worker) / ses_ec889c4e4ffeKqCwo9eLnUK7ig (reviewer) / Planner closure | M6-002 | Run the gRPC over Unix socket IPC spike: Disposable pinned gRPC/Protobuf candidate on the same scenario and resource budget | gRPC over Unix socket spike measured on M6-002 workload; 16 MiB stream, reverse credentials, stalled HTTP streaming, joint cancel release, queue/memory/84 KiB wire bounds, and failure/watchdog cleanup verified; raw metrics recorded in card | `go test -race -count=1 ./internal/conformance -run '^TestM6IPC.*GRPC'`, `./scripts/check.sh`, `git diff --check`; [evidence](tasks/M6-004.md#completion-evidence) |
| [M6-005](tasks/M6-005.md) | DONE | GPT-6 Luna (M6-005 worker) / ses_ec8830f4fffeQlb8knbZS6p7Uy (reviewer) / Planner closure | M6-003, M6-004 | Select IPC transport and record compatibility decision: Reviewed evidence-based ADR; update proposed D10 and stack; reject semantic drift | Selected gRPC over Unix socket in DEC-014 from matched M6-003/004 spike evidence; updated D10, STACK and M6-EXTERNAL consistently; no CONTRACT change | DEC-014 consistency audit, `./scripts/check.sh`, `git diff --check`; [evidence](tasks/M6-005.md#completion-evidence) |
| [M6-006](tasks/M6-006.md) | DONE | GPT-6 Luna (reassigned worker) / ses_ec86582d5ffeuK2gCzh9imKwZ5 (reviewer) / Planner closure | M6-005 | Specify the selected wire and process binding: Concrete v1 mapping, reverse services, limits, cancellation and failure matrix | Uniform typed RecordFragment framing, incremental opaque bodies, reverse services, limits and zero-ambiguous-replay failure matrix specified in M6-BINDING; cards 007–011 refined; reviewer PASS | Documentation audit, `./scripts/check.sh`, `git diff --check`; [evidence](tasks/M6-006.md#completion-evidence) |
| [M6-007](tasks/M6-007.md) | DONE | GPT-6 Luna (M6-007 worker) / ses_ec8527388ffeKtlgPlGWDHHpCY (reviewer) / Planner closure | M6-006 | Implement uniform bounded fragmentation/reconstruction for every typed protobuf record; keep opaque Execute/EstimateUsage/HTTP bodies incrementally chunked under existing limits | Bounded Protobuf schemas, uniform RecordFragment codec and incremental body chunking implemented; Go field parity, envelope wire limits, adversarial fragment rejection and typed body bypass rejection verified | `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -count=1 ./internal/ipc/... -run 'Test(M6Codec.*|M6GoValueConvertersParity|M6CapabilityResultCollectionParity|M6HTTPMetadataAndBodyCarriers|M6RequestBodyChunking|M6EstimateUsageBodyChunking|M6PayloadCarriersOverFrameBoundary|M6CollectionResultsChunking|M6ControlRecord.*|M6ControlFragmentRejectsMalformed)$'`, `./scripts/check.sh`, `git diff --check`; [evidence](tasks/M6-007.md#completion-evidence) |
| [M6-008](tasks/M6-008.md) | DONE | GPT-6 Luna (M6-008 worker) / ses_ec83d8e83ffeDs8z3XOS6SwJ6S (reviewer) / Planner closure | M6-006 | Implement exact child launch/environment, paired private sockets, process-group shutdown/reaping and bounded diagnostics | Linux child supervisor implemented; 0700 paired-socket dir, SO_PEERCRED check, process group management, 64 KiB diagnostics, fixed 30 s deadline, 2 s TERM / 2 s KILL / 5 s group cleanup verified; reviewer PASS | `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -count=1 ./internal/connector/external/... -run '^TestM6Process(Launch|RejectsForeignSocket|StartupDeadline|BoundsDiagnostics|ReapsGroup|CleansOwnedSocket)$'`, `./scripts/check.sh`, `git diff --check`; [evidence](tasks/M6-008.md#completion-evidence) |
| [M6-009](tasks/M6-009.md) | DONE | GPT-6 Luna (M6-009 worker) / ses_ec82ac764ffegYEyCuo7V4rjVV (reviewer) / Planner closure | M6-007, M6-008 | Verify peer and negotiate IPC/API versions before Init or Execute | Generated gRPC Handshake/Descriptor streaming client/server, 84 KiB transport caps, supervisor failed-readiness abort/reap, and exact API-version pair enforcement verified with launched helper child; reviewer PASS | `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -count=1 ./internal/connector/external/... -run '^TestM6(Handshake|Process)'`, `./scripts/check.sh`, `git diff --check`; [evidence](tasks/M6-009.md#completion-evidence) |
| [M6-010](tasks/M6-010.md) | DONE | GPT-6 Luna (M6-010 worker) / ses_ec8135487ffeX6giwnL9Z42wxm (reviewer) / Planner closure | M6-009 | Bridge Descriptor, Init, Health, Capabilities and Close with partial-init cleanup | Lifecycle-only Component proxy implemented; cloned Descriptor, opaque config fragmentation, partial-init teardown/reap, allowlisted Health diagnostics, concurrent context-aware Close, and Capabilities fragmentation verified; reviewer PASS | `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -count=1 ./internal/connector/external/... -run '^TestM6Lifecycle'`; [evidence](tasks/M6-010.md#completion-evidence) |
| [M6-011](tasks/M6-011.md) | DONE | GPT-6 Luna (M6-011 worker) / ses_ec679a6ccffehYHuRLnvKNM5al (reviewer) / Planner closure | M6-010 | Correlate bounded concurrent calls by runtime invocation/epoch; reject stale and cross-call traffic | Host-owned epoch-bound invocation registry; random IDs, account isolation, callback correlation, bounded admission and nonblocking revocation verified; reviewer PASS | `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -count=1 ./internal/connector/external/... -run '^TestM6Invocation'`; [evidence](tasks/M6-011.md#completion-evidence) |
| [M6-012](tasks/M6-012.md) | DONE | GPT-6 Luna (M6-012 worker) / ses_ec66284e8ffe07JVuzEgF51jYy (reviewer) / Planner closure | M6-011 | Bridge selected credential access: Named credential access bound to invocation/account and expiry; revocation on close | Host-served fragmented CredentialGet enforces 84 KiB gRPC frames, bounded out-of-order replay detection, 2 s timeout and 8/32 callback limits; reviewer PASS | `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -count=1 ./internal/connector/external/... -run '^TestM6Credential'`; [evidence](tasks/M6-012.md#completion-evidence) |
| [M6-013](tasks/M6-013.md) | DONE | OpenCode subagent (M6-013 worker) / ses_ec6486ddfffeh2ebCIjDXK3wct (reviewer) / Planner closure | M6-011 | Bridge scoped HTTP request and response streams: Runtime-controlled transport including incremental bodies, header bounds and cancellation | Reverse bidirectional HTTPDo stream implemented; incremental <=64 KiB chunks, header validation/hop filtering, prompt early-stop HTTPBodyComplete, truncated-body rejection, and terminal read cancellation verified; reviewer PASS | `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -count=1 ./internal/connector/external/... -run '^TestM6HTTP'`; [evidence](tasks/M6-013.md#completion-evidence) |
| [M6-014](tasks/M6-014.md) | DONE | GPT-6 Luna (M6-014 fresh worker) / ses_ec6083e08ffePQF18RC7zs9wTp (reviewer) / Planner closure | M6-012 | Bridge bounded sanitized logging: Invocation logging and child diagnostics bounded/redacted without raw payload logging | Client-streaming HostServices.Log implemented; 8 KiB records, prohibited key filtering, secret redaction, 256-queue best-effort drop, and isolated handler cancellation verified; reviewer PASS | `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -count=1 ./internal/connector/external/... -run '^TestM6Log'`; [evidence](tasks/M6-014.md#completion-evidence) |
| M6-015 | DRAFT | — | M6-011 | Bridge model enumeration and usage estimates: Preserve scope, supported/known flags and nullable counters across IPC | — | Scoped and unknown/nil result parity tests |
| M6-016 | DRAFT | — | M6-012, M6-014, M6-015 | Bridge authentication results and continuations: Opaque auth state/candidates and optional expiry/user action; Core retains CAS/persistence | — | Synthetic auth/expiry/invalid-action/storage-failure parity |
| M6-017 | DRAFT | — | M6-012, M6-013 | Bridge one fixed Execute response: One attempt through Head/Body/Complete/EOF with pre-head failures and exact opaque bytes | — | Deterministic fixed-response and pre-head failure tests |
| M6-018 | DRAFT | — | M6-017 | Bridge incremental Execute output: Ordered partial frames delivered before producer completion without response aggregation | — | Gated split-byte, early-delivery and terminal-frame tests |
| M6-019 | DRAFT | — | M6-018 | Implement per-invocation cancellation and deadlines: Cancel Execute/callbacks before and after Head; idempotent stream close and bounded acknowledgement | — | Gated cancellation/stalled callback/deadline tests |
| M6-020 | DRAFT | — | M6-019 | Enforce aggregate backpressure and control progress: Numeric per-call/process limits including HTTP service traffic; cancel can progress under saturation | — | Slow-reader/maximum-concurrency bounds and stall-cancel tests |
| M6-021 | DRAFT | — | M6-019 | Classify IPC loss and malformed stream failures: Unexpected EOF, bad frames and child crash produce truthful outcome and no ambiguous replay | — | Before-send/after-send/post-Head fault-injection tests |
| M6-022 | DRAFT | — | M6-021 | Supervise recovery for new work only: Bounded restart backoff/budget, readiness withdrawal and new epoch; never replay active work | — | Crash-loop, re-handshake and attempt-count tests |
| M6-023 | DRAFT | — | M6-020, M6-022 | Implement drain and process-tree shutdown: Stop admission, finite drain, cancel, terminate/kill, reap and remove owned IPC resources | — | Real child and descendant shutdown/deadline tests |
| M6-024 | DRAFT | — | M6-016, M6-018, M6-020, M6-023 | Wrap the existing native Responses Connector: Child composition reuses Responses implementation via host services without provider branches in Core | — | In-process/external same-native-connector fixture comparison |
| M6-025 | DRAFT | — | M6-024 | Compose external instances in protected startup: Strict opt-in process config/registry wiring with existing virtual-key/account/policy rules | — | Invalid config, unavailable child and legacy-startup tests |
| M6-026 | DRAFT | — | M6-025 | Verify protected JSON and SSE end to end: Real client → protected gateway → child → loopback upstream byte and early-delivery proof | — | Protected real-socket text/tool-byte roundtrip tests |
| M6-027 | DRAFT | — | M6-021, M6-026 | Verify crash accounting and delivery-safe fallback: Exactly-once settlement, unknown usage and no replay after uncertain delivery/commit | — | SQLite reservation/attempt/fallback fault-injection tests |
| M6-028 | DRAFT | — | M6-026, M6-027 | Verify concurrent external account isolation: Parallel accounts/instances cannot exchange credentials, frames or callback authority | — | Concurrent account-isolation and stale-process race tests |
| M6-029 | DRAFT | — | M6-015, M6-016, M6-020, M6-023, M6-027, M6-028 | Run shared conformance against external and in-process native: Same semantic assertions for both paths and scripted malformed/crash fixtures | — | Shared conformance plus affected uncached race suites |
| M6-030 | DRAFT | — | M6-029 | Execute offline external-process operations procedure: Clean-checkout child build/start, readiness, crash recovery and shutdown documented and executed | — | Reproducible LOCAL-M6 procedure without credentials/models |
| M6-031 | DRAFT | — | M6-029 | Publish the minimal external connector integration guide: Wire version, build/run example and supported service/operation scope documented | — | Independent disposable connector interoperability check |
| M6-032 | DRAFT | — | M6-030, M6-031 | Audit M6 acceptance and prepare handoff: Roadmap gates mapped to exact evidence and limitations; old live gates preserved | — | Shared checks, exact-head CI, architecture and registry audit |
