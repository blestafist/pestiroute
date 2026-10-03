# M3 Access, Admission, and Accounting Runtime Binding

This is a proposed concrete runtime binding for M3, not implemented behavior
and not a change to accepted v1 semantics. [CONTRACT.md](CONTRACT.md) remains
authoritative for opaque payloads, Connector support operations, usage reports,
and `Head / Body / Complete / EOF`. These runtime-owned Go-shaped signatures
are semantic proposals; SQL and SQLite types remain behind the composition-root
repositories described in [M3-STORAGE.md](M3-STORAGE.md). No protocol or storage
package is imported by Core.

## Trust and policy snapshot

The northbound adapter extracts the presented virtual key into a separate
authentication argument; it never adds the key or an authorization result to
`ExecutionRequest`, `RequestMetadata`, or `RawPayload`. After verification, Core
passes an immutable trusted principal containing internal key ID, policy ID,
and the observed key/policy revisions through private runtime context/arguments
to admission. Client-supplied IDs are not authority. The raw presented key is
discarded after verification and is never logged or persisted.

```go
type TrustedPrincipal struct {
    KeyID, PolicyID string
    KeyRevision, PolicyRevision int64
}

type KeyStore interface {
    Verify(context.Context, string) (TrustedPrincipal, error)
}

type PolicySnapshot struct {
    ID string
    Revision int64
    Enabled bool
    Models, Connectors []string // exact identifiers
    RPM, TPM int64
}

type PolicyStore interface {
    Snapshot(context.Context, TrustedPrincipal) (PolicySnapshot, error)
}

type RouteBudget struct {
    UnknownEstimate string // "reject" or "reserve"
    ConservativeTokens int64
}
```

For each request, successful key verification and policy snapshot form the
authorization snapshot. `RequestRecord` persists key ID/revision and policy
ID/revision; the policy revision references an immutable versioned policy row.
The atomic `Admit` transaction rechecks enabled/revoked key state, policy enabled
state, and both observed revisions. If either changed before the transaction's
serialization point, admission fails and the caller re-authenticates/reloads;
it does not retry with stale authority. A revocation
or policy edit committed after that point affects later admissions, not an
already admitted request or its in-flight fallback. Each fallback target still
has to satisfy the request's admitted policy snapshot and current route/account
eligibility. Key revocation is monotonic; historical request references remain.
This specifies the visibility boundary, not a change to the v1 Connector API.

## Admission and accounting operations

All types below are runtime-owned semantic values. A successful store call
means its short transaction committed and SQLite acknowledged it under the
configured durability policy. No transaction encloses estimation, Execute, or
stream consumption.

RPM or TPM exhaustion maps to the v1 `rate_limited` gateway error. Only a
distinct typed limit rejection may map to this category; stale authorization,
ledger identity conflicts, and storage failures must not be classified as a
limit rejection.

```go
type AccountingStore interface {
    Admit(context.Context, RequestRecord, AttemptRecord, ReservationRecord) error
    BeginAttempt(context.Context, AttemptRecord, ReservationRecord) error
    RecordDispatchIntent(context.Context, AttemptID) error
    FinalizeAttempt(context.Context, TerminalAttempt) error
    Recover(context.Context, time.Time) (RecoveryReport, error)
}
```

`RequestRecord` carries trusted key/policy IDs and revisions, exact client
protocol/model, and the admitted policy snapshot reference; it does not carry
`accepted_at`, which the repository assigns inside its serialized write
transaction. `AttemptRecord` carries the selected target and its route-budget
policy; `ReservationRecord` carries the known estimate or route-configured
conservative reserve. One transaction checks revisions and limits, then creates
exactly one request, its first attempt/reservation, and its accepted-request RPM
record. Failure has no partial request, charge, attempt, or reservation.
`BeginAttempt` creates a distinct ordinal and token reservation for fallback on
that same request; it does not count RPM again. It checks the persisted admitted
authorization snapshot and the candidate's exact target restrictions.
`RecordDispatchIntent`
must be durably acknowledged before `Connector.Execute`. `FinalizeAttempt`
atomically records terminal attempt outcome, nullable usage, and reservation
settlement keyed by attempt ID. Repeating the same terminal result is a no-op;
a conflicting result is an error and cannot overwrite the first settlement.
Only a terminal attempt marked final terminalizes the request. Recovery follows
the idempotent behavior in [M3-STORAGE.md](M3-STORAGE.md#lifecycle-and-recovery)
and never creates/replays attempts.

Before `Admit`, Core asks the selected Connector's existing
`EstimateUsage` support operation with `UsageQuery` and invocation-scoped
services. It does so outside SQL and passes its estimate/method into the
attempt/reservation records.
Known nonnegative input/output counts yield a reservation of their checked sum.
Unsupported, unknown, absent, negative, or overflowing estimates follow the
selected route's configured `UnknownEstimate` policy: reject or reserve its
fixed positive `ConservativeTokens` value. This budget belongs to route
configuration, not virtual-key policy; never silently reserve zero. An estimate
is a budget, not a hard upstream cap.

## Limit arithmetic

RPM is a rolling 60,000-millisecond window over accepted client requests, not
attempts. After the SQLite writer lock is acquired, `Admit` samples one UTC
Unix-millisecond `now`, sets `cutoff = now - 60_000`, and counts committed
accepted request rows for the same virtual key with `accepted_at > cutoff` and
`accepted_at <= now`. Admission permits a request only when `N < RPM`; the new
request then occupies the last slot. A zero limit denies all. Integer
subtraction, count increment, and rate conversion use checked arithmetic; an
overflow is a fail-closed storage/admission error. Boundaries are exact: a
request at the cutoff is outside the window, one millisecond later is inside.
The serialized transaction performs check, timestamp assignment, and insert
together, so concurrent contenders cannot both take the last slot; timestamps
are sampled only after writer-lock acquisition. Timestamp storage is UTC
Unix milliseconds as specified in M3-STORAGE; equal millisecond timestamps are
valid.

TPM is a rolling 60,000-millisecond budget per virtual key and counts only input
plus output. For admission, sum all held reservations regardless of their
request's age (an in-flight stream remains budgeted until settlement), plus
settled effective charges whose `reconciled_at` is in the current window. Add
the candidate reservation using checked arithmetic and admit only if the sum is
within the policy TPM limit. Settlement atomically replaces that attempt's hold
with its effective charge and samples `reconciled_at` after acquiring the
SQLite writer lock. This keeps a long-running request's actual/overage charge
visible for the subsequent window instead of dropping it based on old
`accepted_at`. Released, proven
undispatched attempts contribute zero. RPM alone is based on `accepted_at`.
Every dispatched fallback attempt consumes tokens independently: prior settled
attempt charges remain in the TPM total and the next attempt reserves additional
budget. If two attempts consume 80 and 120 tokens, respectively, the request
contributes 200 tokens in the applicable window, not the maximum of 120. Sums
are checked for int64 overflow; overflow denies admission.

For reservation `E` and terminal billable usage `A`, define `A` only when both
provider-reported input and output counts are present and nonnegative, using
checked addition. Exact complete provider usage sets effective charge to `A`,
even when below `E`. Partial or unknown usage is conservative: effective charge
is `max(E, known provider input/output lower bound)`, where the lower bound is
the checked sum of whichever nonnegative counters are present (a missing
counter contributes no known lower-bound tokens, not a claim of zero usage).
Unknown usage with no known counters retains `E`. Charge every dispatched
attempt separately in its durable record and sum settled per-attempt effective
charges in the TPM window; do not collapse fallback attempts to a maximum. An
actual overage above `E` is recorded and reduces later availability; do not
truncate it to the estimate.
An attempt proven not dispatched before durable intent releases its reservation
and has zero effective charge. Once intent is acknowledged, delivery may have
occurred: cancellation, ambiguous failure, partial stream, or restart without
complete usage retains the conservative effective charge and never triggers
replay. A terminal Complete is not durable until `FinalizeAttempt` acknowledges
the transaction. `reasoning_tokens` and `cached_tokens` are preserved as detail
and are never added again to input + output.

## Failure and state walkthroughs

**Concurrent last RPM slot and timestamp order:** call A begins before B but is
blocked waiting for SQLite's writer lock. B acquires the lock first, samples
`accepted_at` inside the transaction, and commits; A acquires the lock next,
samples its timestamp then. With a stable wall clock, the lock-first B sample
precedes A's sample; neither timestamp is captured before lock acquisition.
With `r-1` prior rows, B takes the last slot and A observes `N=r` and is
rejected with no rows or partial TPM reservation. Accepted timestamps follow
serialized admission order; no pre-lock timestamp or skew exception is involved.

**Second resource fails:** a request has room under RPM but its estimate exceeds
remaining TPM. The one admission transaction rejects; the RPM row, request,
attempt, and reservation all roll back. It cannot consume an RPM slot without
the complete admission.

**Fallback with two attempts:** request R is admitted once, adding one RPM row
and attempt A's reservation. A safe, uncommitted, policy-eligible failure is
terminally settled at 80 tokens. `BeginAttempt` creates B and its additional
reservation under R; it adds no RPM row and does not execute until B's dispatch
intent is acknowledged. If B settles at 120 tokens, TPM charges 80 + 120 in the
applicable windows because both attempts consumed upstream budget. A
committed/unsafe/unknown failure cannot use this path.

**Duplicate finalization:** two concurrent terminal publications for the same
attempt race on its unique attempt key. The first committed terminal result and
settlement wins; a byte-for-byte semantically identical duplicate succeeds as
an idempotent no-op, while a conflicting outcome/usage is reported and changes
nothing. No duplicate usage or reservation adjustment is created.

**Unknown estimate:** with the selected route configured `reject`, unsupported
or unknown estimate causes pre-admission rejection and no accounting rows. With
route policy `reserve`, its configured positive conservative amount is reserved
atomically; absent/invalid route value fails closed. Unknown terminal usage
keeps at least that amount, never zero.

**Long stream and late overage:** request R reserves 100 tokens at acceptance
time T and remains dispatched beyond T+60 seconds. Its held 100 remains in the
TPM admission sum despite the RPM row aging out. On settlement at T+65 seconds
with actual usage 150, the transaction replaces that hold with charge 150 and
sets `reconciled_at` to T+65. Later admissions count 150 in the settlement-time
window; the overage is not lost because `accepted_at` is old, and the hold is not
double-counted with the settled usage.

**Partial stream completion:** after intent, Head and some Body bytes may be
committed. If a stream ends before valid Complete followed by EOF, record an
incomplete attempt; use supplied known input/output as a lower bound and retain
at least the estimate when usage is incomplete/unknown. Finalize durably once.
No response replacement, fallback, or replay occurs after Head commitment.

**Terminal write failure:** before client commit, failure to acknowledge durable
terminal settlement returns a sanitized gateway error and no success claim;
the request is not redispatched. After commit, close/cancel the stream where
possible, withdraw readiness and fail subsequent admission closed. Do not
report durable success or replay. On restart, a still-intent, nonterminal
attempt is recovered conservatively; if the terminal transaction committed but
its acknowledgement was lost, its terminal row wins and recovery leaves it
unchanged. This is the SQLite acknowledgement boundary in
[M3-STORAGE.md](M3-STORAGE.md#lifecycle-and-recovery), not a replacement for
the v1 stream lifecycle.

## Implementation boundary

The current `Dispatcher.Finalize func(AttemptResult)` and optional
`AttemptObservationSink.TryRecord` are not durable acknowledgements. Future M3
dispatch binding must propagate `FinalizeAttempt` errors through lifecycle
handling instead of treating the callback or telemetry ring as persistence.
Terminal SQL acknowledgement must not be moved ahead of validated Complete and
EOF, nor may it change the accepted stream frames or Connector signatures.
`internal/core` continues to own runtime interfaces and policy decisions;
SQLite repositories implement them at the composition boundary.
