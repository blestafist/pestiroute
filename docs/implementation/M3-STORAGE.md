# M3 Storage and Crash Consistency

This note fixes the proposed M3 persistence boundary before implementation. It
does not claim that SQLite, protected startup, durable accounting, or recovery
is implemented. The accepted v1 semantics remain in [CONTRACT.md](CONTRACT.md);
the existing M2 observation callback is not a persistence API.

## Ownership and database operation

One gateway process owns a database path at a time. Startup takes an exclusive
process lock for that path before opening/serving; a second gateway fails
closed. The local administrative CLI may use the same database concurrently.
Use SQLite WAL mode, `synchronous=FULL`, a bounded busy timeout, foreign keys,
and short transactions. The database must reside on a local filesystem with
SQLite locking and durable sync semantics. Document that copying only the main
database file while WAL is active is not a backup; use SQLite's online backup
API or a quiesced, checkpointed backup. Shutdown stops admission, drains active
requests for the configured bounded grace, then cancels remaining work and
closes storage; it must not wait indefinitely for a writer.

The M3-002 driver spike pins pure-Go `modernc.org/sqlite` v1.60.1. The initial
`internal/storage/sqlite.Open` seam configures WAL, `synchronous=FULL`,
`foreign_keys=ON`, and `busy_timeout=500` on every pooled connection through the
driver DSN. Temporary file-backed tests passed on Linux/amd64 with Go 1.27.1
and `CGO_ENABLED=0`; other deployment platforms are not verified. A context
cancelled while SQLite is inside its busy handler returns after the bounded
500 ms wait, rather than interrupting that wait immediately.

Gateway-originated writes go through one runtime-owned writer with a bounded
queue (or direct serialized calls); queue saturation returns an error rather
than growing memory without bound. The local CLI uses the same runtime-owned
repository/mutation rules, with its own serialized write calls; SQLite's
cross-process writer lock serializes CLI transactions against the gateway.
Configure the documented busy timeout on both processes; lock contention past
that bound returns a storage error, never an unbounded wait or retry loop. CLI
mutations use short transactions and cannot write attempt/reservation state
directly. A successful write call means its transaction committed and SQLite
acknowledged it under the configured durability policy. SQL,
migrations, and SQLite types stay outside production `internal/core`. No
transaction spans connector/network work. Storage errors are returned to the
caller and never converted into a successful accounting result.

The narrow runtime-owned repository boundary is:

```go
type AccountingStore interface {
    Admit(context.Context, RequestRecord, AttemptRecord, ReservationRecord) error
    BeginAttempt(context.Context, AttemptRecord, ReservationRecord) error
    RecordDispatchIntent(context.Context, AttemptID) error
    FinalizeAttempt(context.Context, TerminalRecord) error
    Recover(context.Context, time.Time) (RecoveryReport, error)
}
```

These are semantic signatures, not existing package declarations. Initial
`Admit` is one atomic transaction: it checks the RPM window while holding
SQLite's write lock, then creates the request, its first attempt (ordinal 1),
that attempt's reservation, and the accepted-request RPM record (the request's
`accepted_at`; RPM is derived from accepted request rows). It does not increment
RPM again on retries. A retry uses
`BeginAttempt` to create the next distinct attempt and its reservation for the
same still-open request; uniqueness of `(request_id, ordinal)` and
`reservations.attempt_id` prevents duplicate attempts/holds. It neither creates
a second request nor takes another RPM charge. A failed admission rolls back
all four effects. `RecordDispatchIntent` is an acknowledged durable transition
required before `Connector.Execute`;
`FinalizeAttempt` atomically stores terminal attempt outcome, nullable usage,
and reservation reconciliation, idempotently keyed by attempt ID; its terminal
record says whether this is the final attempt so intermediate fallback attempts
do not terminalize the request. `Recover` atomically closes stale in-flight
attempts, conservatively settles them, and terminalizes their still-open request
as interrupted (or failed with `not_dispatched` reason if no intent was written).
In the same recovery transaction, after stale attempts have been handled, it
also terminalizes any `admitted` request with no nonterminal attempt, using the
last attempt's persisted outcome; this covers a crash after a non-final attempt
was finalized but before its retry was durably begun. Atomic `Admit` guarantees
an admitted request has an initial attempt. Recovery never creates an attempt
or retries work.
The runtime owns interface/types and propagates errors through dispatch to a
client-safe gateway error. The storage implementation owns SQL transactions,
uniqueness, and constraints. No storage implementation is injected into Core;
the composition root adapts it to future Core-owned execution hooks without
changing Core's standard-library-only imports. Such hooks must preserve the
existing `AttemptResult` semantics and make terminal acknowledgement explicit;
the current void `Finalize` and lossy `TryRecord` cannot satisfy this boundary.

## State and schema

Use opaque random IDs (not payload-derived identifiers), UTC Unix-millisecond
timestamps, and explicit foreign keys. Keep historical rows when an account,
key, or policy is disabled/deleted: use restrictive or `SET NULL` references,
never cascading deletion into requests, attempts, usage, or reservations.
Store neither request/response payloads nor plaintext secrets/master key.

| Table | Key, relations, uniqueness and persisted facts |
| --- | --- |
| `schema_migrations` | `version` primary key; applied UTC timestamp; each migration is atomic and recorded in the same transaction as its DDL/data changes. |
| `accounts` | `id` primary key; connector instance reference, enabled/health state, created/updated UTC timestamps. Account identity is stable; disabling is not deletion. |
| `credentials` | `id` primary key; `account_id` foreign key; encrypted envelope, format/key version, expiry, monotonically increasing `revision`, updated UTC timestamp; unique `(account_id, id)` for scoped references. Revision-checked replacement is atomic. |
| `virtual_keys` | Internal `id` primary key and unique public identifier; unique keyed digest; policy reference, enabled/revoked state, created/revoked UTC timestamps. Store only a cryptographic digest, never the presented key. CLI create/revoke is an atomic repository mutation; revocation is monotonic and historical request references are retained. |
| `key_policies` | `id` primary key; versioned policy values and UTC timestamps. Model/connector restrictions are exact values; policy edits do not rewrite historical rows. |
| `auth_sessions` | `id` primary key; account foreign key, connector-owned flow/state labels, expiry/creation UTC timestamps, encrypted temporary state and format/key version. Expired sessions are invalid; temporary secrets are encrypted just like credentials. |
| `requests` | `id` primary key; virtual-key reference retained with `SET NULL` on key purge (prefer revocation); indexed accepted UTC timestamp (the one RPM count), exact requested protocol/model/route identity, state `admitted` or terminal outcome, and finish timestamp. No payload/body fields. |
| `attempts` | `id` primary key; request foreign key, ordinal unique per request, selected account reference retained with `SET NULL`, connector/route identifiers, state, commit flag, safe error category/retry disposition, dispatch/finish UTC timestamps. Initial admission creates ordinal 1 and its reservation atomically; retries use higher ordinals. No provider diagnostics or raw errors. |
| `usage_records` | `attempt_id` primary key and foreign key; nullable input/output/reasoning/cached token counts, source and completeness, recorded UTC timestamp. Unknown counts stay SQL `NULL`; detail counters are not summed again. One terminal usage row per attempt. |
| `reservations` | `attempt_id` primary key and foreign key; estimated reserved tokens, nullable actual billable total, lifecycle state, reconciliation timestamp. Unique attempt association makes settlement idempotent. Every attempt has exactly one reservation; request-level RPM admission is recorded once per accepted request, separately from per-attempt token reservations. |

CLI credential replacement uses compare-and-swap on `(credential_id,
expected_revision)`: encrypt the replacement, then commit the new envelope and
`revision + 1` in one short transaction, failing on a stale revision. The
invocation runtime reads/decrypts the selected revision before Execute and
passes an immutable invocation-scoped snapshot; an already-running call is not
mutated by a CLI update, while the next invocation sees the committed revision.
Virtual-key revocation is likewise atomic and monotonic; each new admission
must check the persisted enabled/revoked state, not a stale CLI/runtime cache.
Master-key material is immutable for this M3 scope: CLI has no command to
replace the external key file or re-encrypt data. Key rotation needs a separate
versioned migration/operational design.

Credentials and auth temporary state use an authenticated encrypted envelope:
algorithm/format version, key version, nonce, ciphertext, and authentication
tag. AES-GCM is the initial candidate. AAD binds table/purpose, stable record
ID, account ID where applicable, and format version. Generate a fresh nonce
with `crypto/rand` for every encryption. Master-key version selects the
decryption key; rotation tooling is not in M3, so deployments must retain the
externally managed key version for the lifetime of data encrypted with it.
Never put keys in SQLite, config YAML, logs, diagnostics, or backups.

Protected startup loads the 32-byte master key from an operator-provisioned
file outside the database/config tree; require a regular file owned by the
service account, mode `0600` (reject broader permissions and symlinks), and
read exactly 32 bytes. Do not silently fall back to environment variables or
development JSON when unavailable. File permissions and external provisioning
are the boundary; M3 does not implement key rotation or a secret manager.

Initial migration order: (1) migration journal and accounts, (2) credentials
and auth sessions, (3) policies and virtual keys, (4) requests and attempts,
(5) usage and reservations, then indexes/constraints and any additive
compatibility migrations. Each version is transactional and immutable after
release; later changes add migrations, never edit an applied migration.

## Lifecycle and recovery

Request states: `admitted → terminal`; an initial attempt and held reservation
are created atomically with `admitted`. Attempts: `reserved → intent →
terminal`. Terminal attempt states are `succeeded`, `failed`, `cancelled`, and
`interrupted`; a reservation proven undispatched terminalizes as `failed` with
a `not_dispatched` reason. Reservations progress `held → settled`,
`held → released` for an attempt proven never dispatched, or
`held → conservative` on an uncertain dispatch/recovery. A known estimate is
held before execution. Unknown estimate handling is an explicit policy
(`reject` or an operator-configured conservative value), never zero. `Admit`
acquires the SQLite writer lock before checking the key's accepted-request
window and inserting the request; that indexed request row is the sole RPM
charge. A retry creates only a distinct attempt/reservation and does not
increment RPM again.

| Boundary/crash | Durable state and required behavior |
| --- | --- |
| Before admission transaction commits | No accepted request, RPM charge, attempt, reservation, or upstream call. Admission transaction failure rolls back all state and rejects before Execute. |
| After initial admission/reservation, before intent | The request (one RPM count), ordinal-1 attempt and its reservation committed atomically; no dispatch intent exists. Recovery releases the reservation, marks the attempt failed with `not_dispatched` reason, and terminalizes the request as failed. No upstream call occurred. |
| After dispatch intent, before/during Execute or stream | Intent is durable, so upstream delivery may have occurred. Recovery marks the attempt `interrupted`, retains its estimate as a conservative charge, records usage as unknown (`NULL` counts), and never replays it automatically. This includes a crash during client streaming after Head/Body commitment. |
| Upstream completed, before terminal SQL commit | The upstream outcome may be successful but is not durable. Recovery treats it as interrupted with conservative reservation and unknown usage. It must not infer success or retry. |
| Terminal write fails while process is live | Do not acknowledge durable success/accounting. Before client commit, return a sanitized gateway failure and do not dispatch/retry. After stream commit, close/cancel the response where possible, stop new admission/withdraw readiness, preserve no-success claim, and report persistence failure internally without secrets/payload. Do not replay; the restart recovery path handles the uncertain intent. |
| Crash after non-final attempt, before retry | The last attempt and its usage/reservation are already terminal/settled, but the request remains admitted and has no active attempt. Recovery terminalizes only the request using that last attempt's outcome; it creates no attempt, retry, or accounting entry. |
| Repeated recovery/restart | One transaction changes each stale nonterminal attempt to interrupted (or failed with `not_dispatched` reason), reconciles/releases its reservation once by attempt ID, then terminalizes its still-open request. It also terminalizes admitted requests whose attempts are all terminal using the last attempt outcome. Terminal attempts/requests and existing settlements are unchanged; repeated runs are idempotent. |

### Required traces

**Successful known usage:** commit admission and attempt reservation; commit
dispatch intent; invoke Execute outside SQL; consume Complete and validate EOF;
commit succeeded attempt, usage counters/source/completeness, and reservation
settlement together; only then acknowledge terminal persistence. If usage is
provider-reported, store its nullable values without adding cached/reasoning
details to input/output totals.

**Unknown usage:** commit admission and configured conservative estimate;
commit intent; Execute; Complete reports unknown/no counts; commit terminal
outcome with unknown source/completeness and `NULL` counts. Keep the estimate
charged conservatively (do not settle to zero). Missing usage is not zero and
does not justify release of consumed budget.

**Failed terminal write:** after a dispatched attempt, the connector completes
but the terminal transaction fails. The caller receives a storage failure,
not a durable success claim; no second Execute occurs. If the process remains
alive, readiness is withdrawn and new admissions fail closed. On restart, the
still-nonterminal intent is recovered conservatively as interrupted with
unknown usage. If the transaction actually committed but its acknowledgement
was lost, its terminal row wins and recovery leaves it untouched.

**Two restart cycles:** cycle one finds intent attempt A plus held reservation
and converts it in one transaction to interrupted/conservative/unknown and
terminalizes A's request; it leaves terminal attempt B, its request, and its
settled usage unchanged. The process restarts and handles a new request C
normally. Cycle two runs recovery again: A remains interrupted and charged
once with its request terminal, B remains terminal, and C is recovered only if
it also has a durable intent and no terminal row, then its request is
terminalized too. All settlement keys are unique by attempt ID. Neither cycle
invokes Execute for recovered work.

**Crash between a failed attempt and retry:** attempt A is finalized as a
non-final failure with its existing usage/reservation settlement; the request
remains admitted while live execution would next evaluate retry eligibility.
The process crashes before `BeginAttempt`. Recovery finds no nonterminal
attempt and terminalizes only the request with A's persisted failure outcome.
It does not create attempt B, re-evaluate or replay the request, or add another
RPM/token accounting entry, even if A recorded safe retry disposition. That
disposition never makes an unsafe or committed attempt retryable; live retry
eligibility remains unchanged, while restart recovery categorically does not
continue retries without a durably begun attempt.

## Cross-document boundary

[CONFIGURATION.md state separation and entities](CONFIGURATION.md#state-separation)
describe the topology/state split; [usage and reconciliation](CONFIGURATION.md#limits-and-reconciliation)
and [the v1 usage contract](CONTRACT.md#usage-and-scoped-runtime-services)
own existing semantics. [STACK.md storage and secrets](STACK.md#storage-and-secrets)
links this concrete design. A semantic v1 change requires the
[DECISIONS.md ADR process](../project/DECISIONS.md#format-of-future-adrs),
including [DEC-005's master-key decision](../project/DECISIONS.md#dec-005--external-master-key-file-for-protected-m3-startup);
this note chooses M3 persistence behavior without changing the v1 payload or
stream contract.
