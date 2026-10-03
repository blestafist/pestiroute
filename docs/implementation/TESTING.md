# Compatibility Testing

## What Tests Prove

Tests primarily verify the observable behavior of the gateway. For native mode, byte preservation and transport lifecycle are verified; for translation, the semantics of the external protocol within declared capabilities are tested. Matching generated text between two real requests is not a valid criterion: the model may respond non-deterministically.

## Testing Levels

Unit tests are useful for routing eligibility, retry decisions, auth state transitions, and limit reconciliation. Integration tests use a fake HTTP upstream and real server/client connections: cancellation and streaming cannot be convincingly verified by merely calling a handler with a recorder.

The conformance harness runs the same scenarios against different connectors and transports. Real upstream smoke tests verify the actual backend and specific client versions separately from regular CI. They do not replace deterministic fixtures.

| Area | Required Scenario | Verified Result |
| --- | --- | --- |
| Passthrough | JSON with unknown nested fields and non-standard whitespace | Body matches byte-for-byte |
| Streaming | Multiple chunks with pause | First chunk available before response completes |
| Parsing | UTF-8, JSON, and SSE delimiters split across chunks | No loss or duplication of bytes/events |
| Tools | One call and tool result in next request | Call ID preserves the relationship between rounds |
| Parallel tools | Multiple interleaved tool argument streams | IDs, indices, and order of each item are correct |
| Multi-round | Multiple tool rounds | Context and identifiers remain consistent |
| Tool choice | Auto, forced tool, and tools disabled | Declared modes are honored or explicitly rejected |
| Reasoning | Reasoning events and associated counters | Declared semantics are not lost |
| Usage | Exact, estimated, missing, and partial | Unknown values are not converted to zero; no double counting |
| Early errors | Auth failure, 429, unavailable | Correct status and retry disposition |
| Late errors | Disconnect after Head/first chunk | No fallback and no new HTTP response |
| Cancellation | Client disconnects or cancels the request | Upstream and internal resources are released |
| Backpressure | Slow client and long stream | Queues and memory remain bounded |
| Limits | Concurrent admission and repeated finalize | No limit bypass and no duplicate accounting |
| Runtime | Crash, malformed frames, version mismatch | The core continues serving other requests |
| Contract lifecycle | Duplicate Head/Complete, Body before Head or after Complete, unknown frame, EOF without Complete | Invalid sequences fail the attempt; accounting finalizes once |
| Component lifecycle | Failed Init, repeated Close, shutdown with active work | No execution before readiness; partial resources released; drain/cancel respects deadline |
| Adapter boundary | Decode failure, pre-stream error, Encode write failure | Common error metadata; no backend call on invalid input; write failure cancels producer |
| Retry metadata | Retryable error with unsafe/unknown disposition, or safe error after commit | No replay; a status code or transient flag alone cannot trigger fallback |
| Capability scope | Missing capability, parent-only support, incompatible protocol/model/account | Required capability must match exactly and be confirmed in the applicable scope |
| HTTP replay | Inference redirect, SDK retry configuration, dropped reused connection with replayable body/idempotency headers | One Execute does not silently replay inference; redirect destination receives no request or credentials |
| Native HTTP bytes | Encoded response body, explicit/absent Accept-Encoding, transport length changes | Body bytes remain unchanged; content encoding/length describe the forwarded bytes |
| Header isolation | Gateway Authorization and Connection-nominated headers in requests/responses | Gateway credentials never reach upstream; hop-by-hop fields are removed; only selected account credentials are used |

A client that merely stops reading exercises backpressure, not necessarily cancellation: the server may not observe a disconnect. Cancellation tests explicitly close/cancel the client connection or request and assert producer cleanup; stalled-reader tests verify bounded buffering and configured deadlines separately.

## Capability-Aware Suite

Basic checks for lifecycle, errors, and cancellation are mandatory for all implementations. Feature checks run according to the manifest. `Unsupported` is verified by a negative scenario with the expected error; `unknown` is displayed as an unverified capability and does not count as a successful pass.

Translation has a separate feature matrix: images, hosted tools, reasoning variants, background execution, and session resume are not considered supported merely because plain text passed the test. Unknown fields are guaranteed to be preserved in native mode; for translation, documentation specifies which fields can be transferred and which require explicit rejection.

## Local checks

Run [LOCAL-M3](LOCAL-M3.md) for the offline protected startup/admin/accounting
procedure and synthetic SQLite backup/restore. It never contacts a provider.
The deterministic fallback, admission, restart-recovery, and crash cases are
also covered by gateway Go tests; see the task card [M3-045](tasks/M3-045.md)
for the direct proof commands.

### M2 local checks

Run `./scripts/check.sh` for repository formatting, vet, unit/race suites and offline build. Focused M2 checks include `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -count=15 ./internal/conformance/...` for shared native/scripted conformance and `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -count=1 ./cmd/gateway/...` for strict M2 topology configuration, multi-route dispatch, legacy normalization, and gateway regressions. The [LOCAL-M2 procedure](LOCAL-M2.md) separately runs a real local gateway against deterministic Python loopback fake targets; it does not contact a provider or verify live compatibility.

## Fixtures and Traces

Fixtures are grouped by connector, upstream protocol, and the version of the client/API under investigation. Stored alongside them are the scenario description, source, and expected invariants. Credentials, cookies, personal prompts, and account identifiers are removed from recordings; after sanitization, fixtures must remain valid for the tested protocol.

For dynamic IDs and timestamps, structural matching is used: the values themselves may differ, but references between tool calls and tool results must match. Native byte-equality is verified on the same recording without such normalization.

## Direct versus Gateway

One scenario is run both directly to the backend and through the gateway with the same model, settings, and client version. The comparison covers the permissible event sequence, item completeness, tool ID relationships, number of rounds, and accounting semantics. Errors and unsupported features are also included in the comparison.

Smoke test results are recorded with the date, connector version, upstream/client version, and scope. They confirm the verified combination, not lifetime compatibility with all provider models.

## Definition of Done

A change is ready when the target scenario is reproducible, corresponding conformance checks pass, and capabilities and documentation are aligned with behavior. A performance baseline is recorded for the native path: latency overhead, time to first byte, memory per active stream, and absence of leaks after cancellation. Numerical budgets are established after M1 on specified hardware and load, not invented beforehand.
