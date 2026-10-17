# M8 — Claude Code Execution and Gateway Integration

## Current disposition

M8-001–007 are implemented and accepted for offline integration under
[DEC-020](../project/DECISIONS.md#dec-020--accept-m8-offline-integration-and-defer-live-enablement).
[M8-HANDOFF](M8-HANDOFF.md) owns the current implementation ceiling.
M8-008 is BLOCKED on an account/current live binding; M8-009 is DRAFT pending
that evidence. This milestone is not fully live-verified. The sequence below
preserves the original milestone goals, not a claim that its starting state
still describes the current implementation.

## Goal and starting point

Turn the M7 candidate into an evidence-scoped, explicitly enabled Responses
backend. M7 merged in [PR #15](https://github.com/blestafist/pestiroute/pull/15)
as `1a76494113b20561cf5b04cf66c4ba0487e762c0`, with passing
[PR CI](https://github.com/blestafist/pestiroute/actions/runs/38951339646).
Its public Execute currently fails closed; translation, streaming, and OAuth
mechanics have synthetic coverage only. Gateway composition rejects it.

This is a plan, not a claim of provider compatibility or authorization for live
calls. [M7-BINDING](M7-BINDING.md), [DEC-018/019](../project/DECISIONS.md),
and the unwaived [M7-005 gates](tasks/M7-005.md) remain the baseline.
Planning does not silently expand those decisions' implementation authority.

## Delivery sequence

| Phase | Bounded outcome | Gate / evidence |
| --- | --- | --- |
| 1. Integration design | Audit current code; define scoped transport, credential lookup, endpoint validation, execution lifecycle, and the exact enabling decision | M8-001 produces the binding and any ADR proposal; resolve required decisions before implementation becomes READY |
| 2. Synthetic execution | Connect request translation and stream parsing through scoped HTTP services using fake credentials and local/injected transports | M8-002: no ambient HTTP client, account fallback, hidden retry, or production registration; unsupported inputs fail before dispatch |
| 3. Lifecycle proof | Exercise the public execution path under incremental delivery, cancellation, slow readers, malformed streams, and accounting | M8-003: one Head and terminal outcome, bounded memory, real IO interruption, conservative retry disposition |
| 4. Agent semantics | Add multi-round function-call/result history, parallel calls, and explicitly supported reasoning behavior | Create bounded cards only after the execution design is settled; preserve call IDs/order; reject unsupported state/signatures rather than inventing replay semantics |
| 5. Protected integration | Prove account binding, encrypted credential lifecycle, admin callback flow, route eligibility, and admission against local fixtures | Explicitly gated composition; fake endpoint configuration cannot enable arbitrary provider traffic; regression tests retain default rejection |
| 6. Authorized live evidence | Establish current provider/client/source versions, entitlement/registration, a reviewed credential procedure, request budget, and direct-versus-gateway comparison | Separate READY live card and explicit operator authority; text/stream/tools/refresh/cancellation evidence are individually scoped; no secret evidence committed |
| 7. Release decision | Publish a compatibility matrix and operator runbook; enable only the verified protocol/model/account capabilities | Reviewed GO decision and all relevant live gates; otherwise report the blocker and keep production routing disabled |

The first three cards are registered in [TASKS](TASKS.md#m8--claude-code-execution-and-gateway-integration).
Later phases become cards just before execution; they are not implied assignments.
Live prerequisites can be recorded while offline design progresses, but failure
to obtain them cannot be converted into a support claim.

## Architectural limits

- Reuse `core.Connector`, scoped credentials/transport, and existing accounting;
  do not put Claude authentication, SSE, tool, or model logic into Core.
- Preserve Responses as the client protocol. This lane does not implement the
  Chat Completions adapter or convert through a universal message structure.
- No official Claude Code subprocess/agent loop, browser automation, external
  plugin runtime expansion, or automatic provider replay.
- Synthetic success never promotes a capability or proves live entitlement.
  Unknown model/account scope stays ineligible for required capabilities.

## Parallel ownership

Use branch `dev-m8` in its own worktree. Own
`internal/connector/claudecode/**`, Claude-specific conformance cases,
`docs/implementation/M8-*`, `tasks/M8-*`, and this lane's TASKS/CURRENT sections.
Coordinate edits to gateway composition, auth plumbing, shared specs, CI, and
test helpers with M9 before dispatch; stage those integrations separately.
M9 has no dependency on M8's provider/live gates. See [parallel workflow](PARALLEL.md).

## Acceptance and verification

M8 is complete only when its reviewed release scope has public execution and
protected gateway evidence, a compatibility matrix distinguishing synthetic
and live results, documented enable/disable behavior, and passing relevant
unit/race/conformance/CLI/hosted checks. A live blocker leaves that portion open;
finishing the offline phases alone does not complete this milestone.

Baseline commands available today (Go 1.27.1):

```sh
GOFLAGS=-p=1 go test -race -count=1 ./internal/connector/claudecode ./internal/conformance ./cmd/gateway
GOFLAGS=-p=1 ./scripts/check.sh
git diff --check
```

Use a worktree-specific TMPDIR. Tests needing Linux process inspection must run
on a host with `/proc/<pid>/task/<tid>/children`; hosted CI passed for the merged
baseline. New phase-specific commands belong in their cards once implemented.
