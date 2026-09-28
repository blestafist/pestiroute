# Compatibility Testing

## What Tests Prove

The primary object of verification is the observable behavior of the gateway. For native mode, byte preservation and transport lifecycle are verified; for translation, the semantics of the external protocol within declared capabilities are tested. Matching generated text between two real requests is not a valid criterion: the model may respond non-deterministically.

## Testing Levels

Unit tests are useful for routing eligibility, retry decisions, auth state transitions, and limits reconciliation. Integration tests use fake HTTP upstream and real server/client connections: cancellation and streaming cannot be convincingly verified by merely calling a handler with a recorder.

Conformance harness runs the same scenarios against different connectors and transports. Real upstream smoke tests verify the actual backend and specific client version separately from regular CI. They do not replace deterministic fixtures.

| Area | Required Scenario | Verified Result |
| --- | --- | --- |
| Passthrough | JSON with unknown nested fields and non-standard whitespace | Body matches byte-for-byte |
| Streaming | Multiple chunks with pause | First chunk available before response completes |
| Parsing | UTF-8, JSON and SSE delimiters split across chunks | No loss or duplication of bytes/events |
| Tools | One call and tool result in next request | Call ID preserves relationship between rounds |
| Parallel tools | Multiple interleaved tool argument streams | IDs, indices and order of each item are correct |
| Multi-round | Multiple tool rounds | Context and identifiers remain consistent |
| Tool choice | Auto, forced tool and tools disabled | Declared modes are honored or explicitly rejected |
| Reasoning | Reasoning events and associated counters | Declared semantics are not lost |
| Usage | Exact, estimated, missing and partial | Unknown does not become zero, no double counting |
| Early errors | Auth failure, 429, unavailable | Correct status and retry disposition |
| Late errors | Disconnect after Head/first chunk | No fallback and no new HTTP response |
| Cancellation | Client stops reading | Upstream and internal resources released |
| Backpressure | Slow client and long stream | Queues and memory remain bounded |
| Limits | Concurrent admission and repeated finalize | No limit bypass and no duplicate accounting |
| Runtime | Crash, malformed frames, version mismatch | Core continues serving other requests |

## Capability-aware Suite

Basic checks for lifecycle, errors, and cancellation are mandatory for all implementations. Feature checks run according to manifest. `Unsupported` is verified by negative scenario with expected error; `unknown` is displayed as unverified capability and does not count as successful pass.

Translation has a separate feature matrix: images, hosted tools, reasoning variants, background execution, and session resume are not considered supported merely because plain text passed the test. Unknown fields are guaranteed to be preserved in native mode; for translation, documentation specifies which fields can be transferred and which require explicit rejection.

## Fixtures and Traces

Fixtures are grouped by connector, upstream protocol, and the version of the investigated client/API. Alongside them are stored scenario description, source, and expected invariants. Credentials, cookies, personal prompts, and account identifiers are removed from recordings; after sanitization, fixtures must remain valid for the tested protocol.

For dynamic IDs and timestamps, structural matching is used: the values themselves may differ, but references between tool call and tool result must match. Native byte-equality is verified on the same recording, without such normalization.

## Direct versus Gateway

One scenario is run directly to the backend and through the gateway with the same model, settings, and client version. The comparison covers permissible event sequence, item completeness, tool ID relationships, number of rounds, and accounting semantics. Errors and unsupported features are also included in the comparison.

Smoke test results are recorded with date, connector version, upstream/client version, and scope. They confirm the verified combination, not lifetime compatibility with all provider models.

## Definition of Done

A change is ready when the target scenario is reproducible, corresponding conformance checks pass, and capabilities and documentation are aligned with behavior. Performance baseline is recorded for native path: latency overhead, time to first byte, memory per active stream, and absence of leaks after cancellation. Numerical budgets are established after M1 on specified hardware and load, not invented beforehand.
