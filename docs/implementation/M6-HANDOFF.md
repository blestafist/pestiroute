# M6 acceptance and handoff

## Delivery

M6 is accepted on `dev-m6`: all 33 tasks (001–032 plus final-audit follow-up 033) are DONE in the [archive](archive/TASKS-DONE.md#m6--external-plugin-runtime). The final audit found serialized production admission and stale supervisor readiness during watcher scheduling. Their correction admits four operations, preserves once-only Init, drains every slot on Close, and rejects failed generations synchronously.

The native Responses implementation runs in a supervised Linux child through
the existing Core contract. Runtime-owned scoped credentials, HTTP transport,
logging, accounting and retry policy remain on the host. IPC uses versioned
gRPC/Unix sockets, opaque incremental bodies and host-owned aggregate credits.

## Acceptance map

All test paths below are under `internal/connector/external` unless specified.
The hosted workflow executes the focused new regressions uncached with `-race
-v -count=1`, both clean-checkout CLI procedures, then serialized shared
format/vet/unit/race/build checks. Local compile/vet and the deterministic
dead-generation race regression pass; local Unix-socket runtime tests are
blocked by the execution sandbox and are not reported as passing.

| Roadmap / plan gate | Executable evidence |
| --- | --- |
| Reviewed transport choice and wire/API compatibility | DEC-014 and M6-003/004 measurements; `TestM6Handshake*` rejects bad peers, protocol/major/minor and missing exact API pair before Init |
| Crash isolation; recovery for new work, no active replay | `TestM6SupervisorRestartsNewWorkOnly`, `TestM6SupervisorRejectsDeadGenerationBeforeWatcher`, `cmd/gateway: TestExternalCrashAccounting`, both CLI procedures |
| Incremental, opaque JSON/SSE and early delivery | `TestM6ResponsesChildOperationsAndFixedParity`, `TestM6ResponsesChildSSEParity`, `cmd/gateway: TestM6ProtectedExternalE2E`, both CLI procedures |
| Bounded aggregate queues and measured resident memory with stalled consumers | `TestM6ResponsesChildAggregateStalledConsumers`: four actual child streams, enlarged HTTP/2 window, 256-frame saturation, sustained per-call/process bounds and parent/child RSS samples; `TestM6Backpressure*` covers shared Execute/reverse-HTTP credits and urgent controls |
| Cancellation/deadlines release callbacks and authority | `TestM6ResponsesChildCancelBeforeHeadReleasesCallback`, `TestM6ResponsesChildSSEEarlyDeliveryAndCancellation`, `TestM6FaultCallerDeadlineBeforeAndAfterHead`, aggregate fixture cancellation |
| Finite drain/shutdown, no owned child/socket leaks | `TestM6Shutdown*` (including four streams, fifth deadline, last-stream drain), `cmd/gateway: TestProtectedStartupExternalSIGTERMReapsUncooperativeChild`, both CLI procedures |
| In-process/external native contract equivalence | `internal/conformance: TestConformance*` external-native fixture matrix for opacity, incremental delivery, lifecycle, failed Init, scoped services, backpressure and cancellation |
| Exactly-once accounting/fallback and account/epoch isolation | `cmd/gateway: TestM6ProtectedExternalFallback`, `TestExternalCrashAccounting`, `TestM6ProtectedExternalAccountIsolation`; operational SQLite settlement assertions |
| Offline operations and minimal interoperable example | [LOCAL-M6](LOCAL-M6.md), [integration guide](M6-INTEGRATION.md), independently built [example](../../examples/external-connector/main.go) |

## Verification record

Source `fcb9f390268c42536690f370dc6ec6c742c967bb` passed [exact-head push CI](https://github.com/blestafist/pestiroute/actions/runs/38710537528) on Linux/Go 1.27.1: focused uncached race regressions, production CLI operations, separately built example interoperability, and serialized shared format/vet/unit/race/build checks. The independent reviewer inspected source, architecture boundaries and hosted results. Local Markdown targets/anchors, registry uniqueness/dependency DAG and whitespace checks passed.

The exact-head focused race run retains raw aggregate measurements: 256 outstanding frames / 1,921,024 application bytes through the one-second stall; parent RSS 53,018,624 baseline / 111,677,440 peak bytes; child RSS 49,528,832 / 86,290,432 bytes. Both stay within the preselected 128 MiB RSS growth allowance per process (separate from the strict 16 MiB queue ceiling); four callbacks/credits released in 1.00 ms.

The PR workflow checks a synthetic merge revision; the linked push run checks the exact source branch head. This final evidence-only closure commit is checked again by hosted CI, whose current status belongs to the PR/commit checks rather than a circular self-reference here.

## Compatibility and remaining boundaries

Linux only, trusted same-user processes. Process supervision contains crashes;
it is not an OS sandbox. The protected external configuration currently accepts
the native Responses implementation. Go composition helpers remain internal to
this module; the example is not a published SDK. Models/EstimateUsage typed
bridges exist, but the generic child supplies no InvocationServices to those
two methods. Authenticate's richer state/credential behavior has synthetic
evidence; native Responses explicitly returns unsupported. IPC does not promote
provider/model capability claims. Gateway `/readyz` is a startup/accounting flag;
dispatch separately observes connector Health.

Core remains independent of concrete connectors, adapters and gRPC/Protobuf.
Runtime body-bound additions are covered by DEC-015/016 and synchronized with
CONTRACT; DEC-017 owns aggregate IPC credits without a provider-specific Core
API. No remote hosting, discovery, hot reload, external Adapters or active
generation replay is introduced.

DEC-019's additive `AuthRequest.CallbackInput` is a later contract/specification
change, not part of the shipped M6 Go/protobuf implementation. Generic Core,
coordinator/admin and IPC field plumbing is implemented by M7-016 in the current
repository; do not infer it from M6 completion evidence. Offline Connector OAuth
PKCE is implemented by M7-010; it remains uncomposed and does not imply live
interoperability, production credential handling, or routing.

The older M4-036, M4-041, M5.1-049, M5.1-051 and M5.2-025 live gates retain their
existing statuses, authority requirements and evidence ceilings in
[CURRENT](CURRENT.md#open-gates-carried-forward). None is an M6 prerequisite and
none was executed or waived during M6 closure. M5.1-049's natural refresh time
remains 2026-10-15 21:04:55.103 UTC, followed by its recorded preflight/authority/
review requirements. M5.2-025 retains DEC-013's at-most-3B ceiling.
