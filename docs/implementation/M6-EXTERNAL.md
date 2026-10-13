# M6 — External Connector Process Runtime

## Outcome and scope

Implement [M6](ROADMAP.md#m6--external-plugin-runtime): the existing native Responses Connector can run in a child process behind the same `core.Connector` semantics. Crashes stay isolated, payloads remain opaque, streaming is incremental and bounded, cancellation releases resources, and in-process/external conformance agrees. This plan does not select a transport. [D10](../project/DECISIONS.md) and [STACK](STACK.md) still propose gRPC over Unix socket; only measured M6-003/004 evidence and the M6-005 ADR can accept or replace it.

Start from merged main `bea67fe` (PR #13), not the old M5.2 branch. Conditional acceptance under DEC-012/013 carries forward without converting incomplete live gates into DONE. M6 uses child binaries and deterministic loopback fake upstreams; no provider inference, GPU, model download or OAuth refresh is required.

## Existing seams to reuse

| Owner | Existing seam | M6 responsibility |
| --- | --- | --- |
| Core contract | `internal/core/component.go`, `connector.go`, `execution.go` | Preserve descriptors, exact API-pair matching, support operations, invocation services and Head/Body/Complete/EOF |
| Core lifecycle | `dispatch.go`, `retry.go`, `accounting.go`, `persistent_services.go` | Keep routing, retry, credential ownership and once-only settlement here; bridge infrastructure only |
| Native backend | `internal/connector/responses/component.go`, `transport.go` | Reuse the actual Connector in a child composition root, including its invocation-scoped HTTPDoer; no second native implementation |
| Composition | `cmd/gateway`, `internal/core/registry.go` | Construct external proxies/supervisors outside Core; explicit opt-in instance wiring |
| Verification | `internal/conformance`, `internal/testutil/fakeupstream`, `internal/testutil/scripted` | Parameterize equivalent assertions for in-process and real child-process fixtures |

Package paths, executable/config keys, wire schemas and libraries beyond these existing seams are decisions for M6-005/006. Do not make Core import an IPC transport library or concrete Connector. Linux/Unix sockets are the initial comparison environment, not a portability claim. Other operating systems remain unverified until explicitly tested.

## Transport selection gate

M6-002 defines one scenario, then M6-003 and M6-004 execute it separately using disposable prototypes. Compare framed stdio and gRPC over Unix socket on exactly the same payload, concurrency, long-stream size, stalls, host-service callbacks, cancellation and finite deadline. Capture first-byte latency, throughput, peak/bounded memory, queue limits, cancellation latency, CPU, cleanup and implementation/dependency cost; record repetitions and machine/toolchain versions. Select numeric budgets before measurement; do not tune one candidate's workload to favor it.

The scenario must exercise reverse credential lookup and runtime-controlled HTTP request/response streaming, including a callback while output is stalled. A stream-only ping benchmark cannot establish the host-service lifecycle. Raw payload bytes must survive arbitrary splits, binary values and unknown fields; nullable usage stays distinct from zero. Unknown required controls and incompatible versions reject before execution. A child dying after delivery leaves replay unsafe/unknown even before Head.

Spike code is isolated from production startup. Use no real credentials or external endpoints. Pin any candidate library/code generator and retain reproducible generated-code commands; before spike readiness verify dependencies are obtainable in the development environment. Missing gRPC tooling blocks that spike, not a fabricated result or automatic stdio selection. M6-005 records motivation, alternatives, evidence, compatibility and migration requirements in DECISIONS, synchronizing STACK. Any semantic change also updates CONTRACT under its ADR rule; an IPC envelope must not create provider-aware Core types.

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
| 012–016 | Credentials, streaming HTTP, logging, Models/EstimateUsage and synthetic Authenticate parity |
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
