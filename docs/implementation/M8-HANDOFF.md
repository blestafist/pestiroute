# M8 offline integration handoff

## Disposition

The user accepted integration of the offline M8 slice on 2026-10-17 and deferred
live verification because no Claude account is available. [DEC-020](../project/DECISIONS.md#dec-020--accept-m8-offline-integration-and-defer-live-enablement)
records this decision. M8-001 through M8-007 are implemented; M8 as a live
provider milestone is conditionally accepted, not fully DONE.

## Implemented and bounded

| Area | Implemented evidence | Release ceiling |
| --- | --- | --- |
| Public Execute | Synthetic Responses-to-Messages requests, incremental SSE, cancellation, bounded parsing and error handling | Only explicitly marked offline transport; fixed `.invalid` Messages endpoint and `synthetic-beta` |
| Agent history | Linked function calls/results and synthetic multi-round/parallel history | No verified live tools, reasoning signatures, or session continuation |
| OAuth | PKCE/manual callback, refresh, encrypted SQLite credentials, restart tests | Fixed synthetic auth endpoints; default admin exchange transport absent |
| Gateway | Real component initialization, scoped `oauth` lookup, configurable credential reference, fixture-only protected routing/accounting | YAML rejects Claude; ordinary runtime transport cannot execute; capabilities remain Unknown |

Review reconciled the M8 and M9 gateway imports, retained native Chat and its
idle-timeout/isolation fixes, and fixed response-body cleanup when a transport
returns both a response and an error. The regression covers cancellation too;
ambiguous failures remain non-replayable. No Core contract or provider branch
was added.

## Deferred work

[M8-008](tasks/M8-008.md) owns current provider binding and bounded live evidence.
It is BLOCKED on an available account and reviewed operator procedure. The
current synthetic endpoint cannot be made live by supplying credentials alone:
a real endpoint/auth/beta binding and controlled validation harness must first
be reviewed. [M8-009](tasks/M8-009.md) owns the later production enablement
and compatibility/runbook decision after the required evidence passes.
Neither blocks unrelated lanes. Do not delete these gates or relabel synthetic
coverage as provider compatibility. M7's historical findings remain intact.

## Verification

Targeted gate: `GOFLAGS=-p=1 go test -race -count=1 ./internal/connector/claudecode ./cmd/gateway ./internal/conformance -run 'Claude|PublicExecute|ExecuteTool|ExecuteRejects'`.
Full integration gate: `GOFLAGS=-p=1 ./scripts/check.sh` and both M6 offline
smoke procedures in hosted CI, using pinned Go 1.27.1. The PR/CI result is the
merge gate; older task-card results describe their original branch revisions.
No real provider calls were made for this integration.
