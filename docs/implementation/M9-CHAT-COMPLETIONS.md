# M9 — Chat Completions Client Compatibility

## Goal and starting point

Implement `POST /v1/chat/completions` as a first-class northbound interface,
alongside Responses, under the existing
[adapter contract](CONTRACT.md#protocol-adapter-contract). The repository
currently implements only the Responses adapter. The
[project scope](../project/README.md) and
[architecture](../project/ARCHITECTURE.md#example-2-chat-completions-compatibility)
already allow this extension; they do not claim it works today.

Deliver a native Chat Completions path to a compatible backend first. A new
adapter alone cannot use a Responses-only Connector: declare the Chat protocol
on a compatible native Connector and route by exact protocol/model/account
scope. Do not convert Chat to Responses in Core or in the adapter.

## Delivery sequence

| Phase | Bounded outcome | Gate / evidence |
| --- | --- | --- |
| 1. Binding and source baseline | Define exact protocol/component IDs, admitted fields/capabilities, native connector scope, usage extraction, and HTTP behavior | M9-001: audit current adapter, connector, gateway, and contract; pin public protocol references; identify any required ADR before implementation |
| 2. Adapter and native transport slice | Decode Chat requests into opaque envelopes and stream compatible native output without mutation | M9-002: JSON and SSE local fixtures, unknown-field preservation, errors, `[DONE]`, usage chunks, cancellation, and bounded reads |
| 3. Protected gateway route | Compose the adapter and compatible Connector at the root; expose the endpoint with existing auth, policy, admission, accounting, and startup validation | M9-003: client-to-fake-backend JSON/SSE with a protected synthetic account and explicit routing |
| 4. Agent workflows | Prove function calls/results, parallel tool calls, tool choice, multi-round history, and supported response-format options | Create bounded cards after the binding/slice; preserve IDs and unknown bytes in native mode; required capabilities must be explicitly scoped |
| 5. Compatibility and handoff | Publish the supported subset, configuration examples, CLI smoke procedure, and versioned client evidence | Deterministic fake-backend smoke and regressions for Responses; any real-provider/client call has its own prerequisites and authority |

The first three cards are registered in [TASKS](TASKS.md#m9--chat-completions-client-compatibility).
No Claude integration or subscription live gate is an M9 dependency. Translation
to Responses-only or Anthropic-only backends is a later separately scoped
Connector feature, not an implied part of the native endpoint.

## Architectural limits

- Keep protocol parsing in the adapter and native backend/usage handling in
  the Connector. Core remains provider- and client-format-neutral.
- Reuse protected accounts, policies, limits, reservations, and exactly-once
  finalization. Do not add a second accounting path or universal message model.
- Preserve body bytes and unknown native fields, incremental delivery, bounded
  backpressure, and disconnect cancellation. Never retry after commit or on
  ambiguous delivery.
- Document the admitted client subset and any explicit rejection. Native byte
  preservation is not proof that every upstream model supports every feature.
- No UI, new provider OAuth, persistence schema migration, async resource API,
  automatic model rewrite, or Chat-to-Responses translation in this milestone.

## Parallel ownership

Use branch `dev-m9` in its own worktree. Own new
`internal/adapter/chatcompletions/**`, a protocol-specific native Connector
package if selected by M9-001, Chat-specific conformance/tests,
`docs/implementation/M9-*`, `tasks/M9-*`, and this lane's TASKS/CURRENT sections.
Preserve `internal/connector/claudecode/**`. Coordinate shared gateway/router,
specification, CI, and fixture-helper edits with M8 before starting them;
separate protocol-specific composition files where the current structure allows.
See [parallel workflow](PARALLEL.md).

## Acceptance and verification

The protected endpoint must serve byte-preserving native JSON and incremental
SSE, complete a deterministic multi-round tool workflow, apply key/policy/limit
checks before dispatch, settle usage once, and cancel upstream on disconnect.
Malformed chunks, missing terminal markers, upstream errors, and wrong-protocol
routes must fail according to the binding. Existing Responses native,
Anthropic, Codex, local-runtime, and external-child checks remain green.

Commands available today (Go 1.27.1):

```sh
GOFLAGS=-p=1 go test -race -count=1 ./internal/adapter/responses ./internal/conformance ./cmd/gateway
GOFLAGS=-p=1 ./scripts/check.sh
git diff --check
```

These are baseline regressions, not Chat support evidence. New Chat package and
CLI commands must be added to the cards when those deliverables exist. Keep
worktree-specific TMPDIR values for independent process-test runs.
