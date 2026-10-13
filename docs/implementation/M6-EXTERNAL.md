# M6 — External Connector Process Runtime

## Outcome and scope

Implement [M6](ROADMAP.md#m6--external-plugin-runtime): the existing native Responses Connector can run in a child process behind the same `core.Connector` semantics. Crashes stay isolated, payloads remain opaque, streaming is incremental and bounded, cancellation releases resources, and in-process/external conformance agrees. [DEC-014](../project/DECISIONS.md#dec-014--select-grpc-over-unix-socket-for-external-connector-ipc) selected gRPC over Unix socket from measured M6-003/004 evidence; M6-005 is DONE. Production wire/process binding is M6-006 scope.

Start from merged main `bea67fe` (PR #13), not the old M5.2 branch. Conditional acceptance under DEC-012/013 carries forward without converting incomplete live gates into DONE. M6 uses child binaries and deterministic loopback fake upstreams; no provider inference, GPU, model download or OAuth refresh is required.

## Existing seams to reuse

| Owner | Existing seam | M6 responsibility |
| --- | --- | --- |
| Core contract | `internal/core/component.go`, `connector.go`, `execution.go` | Preserve descriptors, exact API-pair matching, support operations, invocation services and Head/Body/Complete/EOF |
| Core lifecycle | `dispatch.go`, `retry.go`, `accounting.go`, `persistent_services.go` | Keep routing, retry, credential ownership and once-only settlement here; bridge infrastructure only |
| Native backend | `internal/connector/responses/component.go`, `transport.go` | Reuse the actual Connector in a child composition root, including its invocation-scoped HTTPDoer; no second native implementation |
| Composition | `cmd/gateway`, `internal/core/registry.go` | Construct external proxies/supervisors outside Core; explicit opt-in instance wiring |
| Verification | `internal/conformance`, `internal/testutil/fakeupstream`, `internal/testutil/scripted` | Parameterize equivalent assertions for in-process and real child-process fixtures |

### M6-001 baseline audit

The audited code baseline is merge commit `bea67fe027e9a34cdb07629a84fbbad021fb1584` (PR #13), parented by `9d640cf1968ee93e8187a38648dae3e06a28c2b1` and M5.2 source `1d9634f8adbad7048222a316b732332641fb6d4c`; the latter is an ancestor of the merge. The clean execution checkout was `dev-m6` at `c842b6c` (`docs(m6): plan external connector runtime and archive completed backlog`). This is carried-forward code, not the M6 checks run now; see [M6-001 execution evidence](tasks/M6-001.md#completion-evidence).

| Contract operation / service | Existing code ownership and observed seam |
| --- | --- |
| `Descriptor`, `Init`, `Health`, `Capabilities`, `Close` | `core.Component` in `internal/core/component.go`; descriptor/version validation and mutable declaration cloning live there. |
| `Execute`, `EstimateUsage`, `Authenticate` | `core.Connector` and typed support values in `internal/core/connector.go`; request/response bytes and lifecycle are in `internal/core/execution.go`. |
| `Models` | Declared on `core.Connector` in `internal/core/connector.go` and implemented by native, Anthropic, Codex and scripted connectors. The audit found no production Core dispatch or Adapter/gateway route calling it; direct calls are in conformance/tests. M6-015 owns the IPC bridge/parity decision for this operation. Do not assume a pre-existing host-side Models call path. |
| Execute and estimate dispatch | `internal/core/dispatch.go` selects the connector and passes scoped services; `internal/core/estimate.go` invokes estimate. `internal/core/retry.go` and accounting own retry/finalization, not connectors. |
| Auth operation | `internal/core/auth_coordinator.go` owns auth lifecycle/session coordination and invokes `Authenticate`; persistence remains runtime-owned. |
| Credential access, HTTP transport, logging | `core.InvocationServices` holds `CredentialAccess`, `HTTPDoer`, `*slog.Logger`; `internal/core/persistent_services.go` and `services.go` bind scoped implementations. Native Responses uses its own transport in `internal/connector/responses/transport.go`. These live interfaces are not wire values. |
| Conformance | `internal/conformance/conformance_test.go` runs shared contract assertions against native-loopback, Codex, Anthropic-translation and scripted in-process fixtures, including opacity, lifecycle, incremental delivery and cancellation. It has no child-process fixture or reverse-service callback under stalled output; M6-002 adds only the common streaming/cancellation scenario with credential lookup and HTTP callbacks. Synthetic `Authenticate` parity belongs to M6-016's authentication bridge, not M6-002. |

Open gates remain M4-036 (BLOCKED: official Anthropic credentials and separately authorized procedure/review), M4-041 (BLOCKED: live tool cycle unmet after one authorized compatible-endpoint call failed its allowlist; no retry), M5.1-049 (BLOCKED until its natural refresh due time `2026-10-15 21:04:55.103 UTC`, then current preflight, explicit authority and independent review), M5.1-051 (DRAFT/unrun; own READY assignment, explicit live authority and independent review required), and M5.2-025 (BLOCKED pending provisioned vLLM/GPU/runtime/model and operator authority; DEC-013's at-most-3B ceiling holds). DEC-012/013 do not waive these gates or authorize provider calls; none is an M6 dependency.

Package paths, executable/config keys, and production wire schema details beyond the selected gRPC transport are decisions for M6-006. Do not make Core import an IPC transport library or concrete Connector. Linux/Unix sockets are the initial evidence environment, not a portability claim. Other operating systems remain unverified until explicitly tested.

## Transport selection gate

M6-002 defines one scenario, then M6-003 and M6-004 execute it separately using disposable prototypes. Compare framed stdio and gRPC over Unix socket on exactly the same payload, concurrency, long-stream size, stalls, host-service callbacks, cancellation and finite deadline. Capture first-byte latency, throughput, peak/bounded memory, queue limits, cancellation latency, CPU, cleanup and implementation/dependency cost; record repetitions and machine/toolchain versions. Select numeric budgets before measurement; do not tune one candidate's workload to favor it.

The scenario must exercise reverse credential lookup and runtime-controlled HTTP request/response streaming, including a callback while output is stalled. A stream-only ping benchmark cannot establish the host-service lifecycle. Raw payload bytes must survive arbitrary splits, binary values and unknown fields; nullable usage stays distinct from zero. Unknown required controls and incompatible versions reject before execution. A child dying after delivery leaves replay unsafe/unknown even before Head.

### M6-002 common comparison worksheet

The in-process control is `TestM6IPCScenarioControl` in `internal/conformance/ipc_scenario_test.go`; cancellation release is `TestM6IPCScenarioCancellationReleasesCallback`. Both spikes must use the same selector/workload and add transport-specific tests without modifying the control fixture:

```sh
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -count=1 ./internal/conformance -run '^TestM6IPCScenario(Control|CancellationReleasesCallback)$'
```

Fixed workload: one warmup (not measured), 10 measured repetitions, max concurrency 1 (then a separate 4-call saturation/cancel run), 256 x 64 KiB application-body frames (16 MiB exact opaque output, including NUL/0xff bytes), 1 KiB synthetic request header, 64 KiB callback request and response bodies, one Execute-time scoped credential resolution plus one reverse credential lookup while stalled, and one loopback-only HTTP callback while the stream reader is stalled after its first 64 KiB body; then cancellation of a blocked callback and gated stream. No real secret/network. Control deadlines: 2 s gate/cancel acknowledgement, 10 s per repetition, 60 s process cleanup. Keep application and wire sizes distinct: maximum application body per frame is 64 KiB; maximum aggregate headers are 16 KiB; the IPC envelope/framing allowance is 4 KiB; therefore the maximum encoded wire frame is 84 KiB (64 + 16 + 4 KiB). Per-call queue is at most 64 frames / 4 MiB of application body (at most 5.25 MiB encoded using the envelope allowance); total queued application payload is at most 16 MiB per process, with at most 256 queued frames / 21 MiB encoded across four calls. Do not change these values for either transport; actual transport encoding must fit the 84 KiB frame ceiling. The fixture asserts exact application bytes, early delivery, callback body parity and joint callback/stream release on cancellation. Implementations must additionally prove their queue/memory bounds under the separate 4-call run.

Record for every repetition: first-Body latency from Execute start, total completion time/throughput, process CPU time, peak RSS, maximum queued bytes/frames, cancellation request-to-ack latency, child exit/reap and owned-socket cleanup. Use monotonic clock; capture Go/toolchain, OS/kernel, CPU, RAM, transport/library/generator revisions and exact commands. Report each raw repetition and median/p95 (no dropping outliers), warmup separately, plus bytes equality and pass/fail against the fixed budgets. The in-process run establishes workload/control behavior; only actual child-process measurements count as transport evidence.

Spike code is isolated from production startup. Use no real credentials or external endpoints. M6-003/004 retained pinned candidate libraries/code generators and reproducible generated-code commands; [DEC-014](../project/DECISIONS.md#dec-014--select-grpc-over-unix-socket-for-external-connector-ipc) records the selected transport, tradeoffs and compatibility policy, synchronized with STACK. M6-006 fixes production pins and binding. Any semantic change also updates CONTRACT under its ADR rule; an IPC envelope must not create provider-aware Core types.

## Binding gate

M6-006 must specify the complete mapping of every existing Component/Connector operation and value, separate API/IPC/component/protocol versions, nil/unknown distinctions, encoding bounds and forward-compatibility policy. Include synchronous Descriptor discovery before Init, support queries, opaque Authenticate state/candidate credentials/expiry/user action, cancellation and idempotent Close. A native Connector returning unsupported auth is not proof that Authenticate can be omitted from the binding; use synthetic auth fixtures.

`InvocationServices` contains live interfaces and cannot simply be serialized. Define invocation-scoped reverse calls for named credential access, HTTPDoer and logging. Runtime chooses account/instance, issues and expires authority, and owns credential persistence, auth-session claims/refresh serialization and CAS. Child-supplied account IDs cannot increase authority. Bound outstanding calls and in-flight bytes; cancellation/revocation must work when a body queue is full. Cover HTTP request/response headers, incremental bodies, non-replayable delivery, closure and transport-owned restrictions, not only credentials.

Define dispatch milestones and conservative failure classification: proven never-sent, delivered/ambiguous, committed, orderly Complete and unexpected EOF. Process restart is for future eligible work only. Do not infer retry safety from a missing acknowledgement or successful relaunch. Define process epoch/request correlation so late frames and callbacks cannot attach to a new process generation.

Choose explicit executable/argv, environment allowlist, working directory, process ownership, private IPC endpoint and peer binding, startup/handshake timeouts, max invocations, frame/header/body/queue/diagnostic limits, drain deadline and bounded restart budget/backoff. Do not pass secrets, database access, master key or auth state through command-line/config/environment. Child diagnostic channels are bounded and redacted; stdout is protocol-only if stdio wins. A child given credentials can access them during its invocation: process isolation contains crashes, it is not an OS sandbox or protection against a malicious same-user executable. Document this concrete deployment boundary without inventing a sandbox.

## Execution gates

| Candidates | Result and promotion boundary |
| --- | --- |
| 001–006 | Baseline → common comparison → two spikes → reviewed ADR → binding; six small cards prepared now |
| 007–011 | Codec, launch, handshake, lifecycle and invocation isolation; prepare cards only against accepted binding |
| 012–016 | Credentials, streaming HTTP, logging, Models/EstimateUsage and synthetic Authenticate parity (the authentication parity is specifically M6-016) |
| 017–020 | Fixed Execute → incremental output → cancellation/deadlines → aggregate backpressure/control progress |
| 021–023 | Crash/invalid-frame classification → bounded recovery for new work → drain/termination/reaping |
| 024–028 | Actual native Connector child → protected configuration → JSON/SSE integration → accounting/fallback → account isolation |
| 029–032 | Shared conformance → executed offline operations → minimal integration guide/example → final acceptance/handoff |

The [registry](TASKS.md#m6--external-plugin-runtime) owns exact dependencies and checks. These are outcome candidates, not frozen package designs. Promote dependent cards only after predecessor evidence and an executable verification procedure exist. Use the existing Planner → Worker → Reviewer cycle and one verified task per commit; stop at each card boundary. No task is marked DONE by this planning commit.

## Acceptance evidence

Final audit must map the ROADMAP gates to deterministic real-child tests: incompatible handshake rejected before Init/Execute, crash isolation with a healthy gateway probe/unaffected request, long-stream exact bytes and early delivery, measured aggregate memory/queue bounds under stalled consumers, cancellation/deadlines with callbacks released, finite shutdown with no zombies/owned socket leaks, in-process/external conformance equivalence, and zero active-request replay after crash/restart. Add protected SQLite settlement/reservation and account/epoch isolation evidence; a gateway health probe alone is insufficient.

M6-029 establishes exact uncached race commands; M6-030 writes and executes LOCAL-M6 from a clean checkout; M6-031 publishes a minimal interoperating connector example and its supported wire/API versions, not a general multi-language SDK framework or marketplace. M6-032 runs shared checks and exact-head hosted CI and audits Core dependency neutrality and all retained live gates. Missing evidence stays explicit.

No hot reload, remote plugin hosting, external Adapters, dynamic downloads/discovery, sessions/async/tool-loop ownership, new provider connectors, automatic replay, or comprehensive OS sandbox belongs in M6. Capability claims do not increase because a Connector crosses IPC.

## Tooling and retained gates

Keep Context7/gopls and existing shell/Git/Go tools. No new MCP/plugin is needed now. If gRPC wins, pinned runtime/code-generation dependencies and reproducibility are ordinary project tooling selected through the spike/ADR, not assumed installed. Machine-local connection state is not established by checked-in MCP configuration.

M4-036 and M4-041 remain BLOCKED. M5.1-049 remains BLOCKED until its natural refresh threshold (2026-10-15 21:04:55.103 UTC) plus its existing current preflight/authorization/review; M5.1-051 remains DRAFT and unrun. M5.2-025 remains BLOCKED, with the DEC-013 at-most-3B bound and separate operator authority; its historical 8B candidate is not authorization. None is an M6 prerequisite or implicitly authorized by this plan.
