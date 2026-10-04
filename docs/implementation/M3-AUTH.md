# M3 Provider-Neutral Authentication Runtime Binding

This note fixes the M3 runtime ownership convention for the existing v1
Connector `Authenticate` operation. It adds no Connector fields or semantic
operation; [CONTRACT.md](CONTRACT.md) remains authoritative. Provider endpoint,
scope, exchange, and response formats stay inside the Connector. M3 proves the
runtime with scripted Connectors, not live OAuth flows.

## Binding the existing operation

The binding is `AuthRequest{AccountID, Action, State}` and
`AuthResult{Supported, State, NextAction, UserAction, Credentials}`. `UserAction`
is an optional `*AuthUserAction{VerificationURI, UserCode, PollInterval}` safe
presentation projection; it is distinct from opaque `State`. Apply these runtime
conventions:

| Field | Runtime convention |
| --- | --- |
| `AuthRequest.AccountID` | Runtime-selected account ID, set by the caller and never accepted from client state. |
| `AuthRequest.Action` | Exactly `start`, `continue`, or `refresh`. Other values fail before Connector invocation. `refresh` is runtime/internal only. |
| `AuthRequest.State` | For `start` and `refresh`, empty. For `continue`, the Connector's opaque state bytes loaded from the runtime's protected session row. Never client-supplied or a database handle. |
| `AuthResult.Supported` | `false` means do not accept returned state/credentials. If the method was invoked, it is not evidence that no network exchange occurred; retain/quarantine any refresh marker and invalidate the interactive continuation conservatively. |
| `AuthResult.State` | Opaque Connector state bytes encoded in the existing string field; persist as bytes without parsing. Empty means no continuation state. |
| `AuthResult.NextAction` | Empty means the flow ended; otherwise exactly `continue`. The runtime creates/retains a session only for `continue`. It does not infer provider-specific actions. |
| `AuthResult.UserAction` | Optional safe presentation projection, valid only when `Supported=true` and `NextAction=continue`. Core validates before exposing: URI is 1–2048 printable ASCII bytes without spaces/control bytes, absolute HTTPS with host and no userinfo; code is 1–256 printable ASCII bytes without controls; interval is positive. It never contains device-auth identifiers, PKCE material, tokens, or raw response/state. Core returns it transiently with the auth session and never persists it. |
| `AuthResult.Credentials` | Candidate account credentials returned by the Connector. Runtime validates and atomically persists them; they are not success until that write is acknowledged. |

The runtime assigns an unguessable session ID and binds the row to one account,
the selected Connector instance, a fixed expiry, and the credential revision
observed when the flow began. Session ID is the caller's handle, not Connector
state. Start creates the protected row only when the Connector supports the
operation and returns a continuing action; a finished flow needs no session.
Continue resolves the runtime session, rejects expired/missing/consumed sessions,
loads only its account-bound opaque state, and checks the stored credential
revision both before the Connector call and atomically when persisting the next
state or consuming the session. Before invocation, it commits an exclusive claim
conditioned on the loaded encrypted-state nonce and current enabled account.
A stale reader cannot claim state advanced by another process. A mismatch
discards returned state and fails closed. Expiry is runtime-owned, fixed at start,
and checked before every
continuation; an extension requires a future explicit policy, not a provider
response.

Start and Continue return the optional `UserAction` on the transient
`AuthSession` result, separately from its runtime-owned session handle and
expiry. It is not an `AuthSession` persistence field. A pending poll is one
explicit `continue` invocation, never a coordinator polling loop. The caller observes
`PollInterval` and waits at least that long before issuing another
continuation;
the interval does not change the fixed session expiry. A completed result has no
continuation session, and candidate credentials are successful only after the
existing acknowledged atomic write. Presentation data is transient and must not
be logged or included in credential/state storage. The Connector is responsible
for ensuring its verification URI is absolute HTTPS without embedded credentials
and the URI/code are safe to display without device-auth identifiers or other
secrets.

`UserAction` is valid only with `Supported=true` and `NextAction=continue`; an
action on a completed, unsupported, or refresh result is invalid. A nil action
on `continue` remains valid for legacy opaque continuations with no operator
interaction. Any Connector flow requiring user interaction MUST provide a valid
action; the Connector's auth-flow conformance tests enforce this because Core
cannot infer a missing projection from opaque state. Presence is self-describing
and requires no separate capability. Core rejects malformed or inconsistent
actions before returning any result field: Start creates no session or
credentials; Continue discards the result and consumes its already-claimed
session/claim; Refresh discards it and quarantines the invoked refresh marker.
If consuming a claimed continuation cannot be committed, do not expose any
result and retain DEC-007 restart recovery's consume-on-ambiguous-claim behavior.

Refresh loads the selected account's current credential snapshot and revision,
then supplies it only through that account's invocation-scoped
`InvocationServices.Credentials`. No database, repository, credential handle
for another account, or decrypted credential map is passed as `State`.
`Credentials` in the result replaces the selected account's credential set as
one atomic revision-checked write; it is not merged implicitly. The runtime
returns persisted success only after acknowledgement. Start/continue session
data and any PKCE verifier/challenge material are opaque, encrypted at rest,
excluded from logs, and cleared on completion, cancellation, expiry, or
account/Connector invalidation. Cancellation before a Connector call consumes
the session. Once a call has begun, cancellation may have consumed provider
state: invalidate the continuation and retain only the non-secret uncertain
marker until explicitly resolved. Cleanup is best-effort if storage is
unavailable and expiry is the restart-safe backstop for ordinary sessions.

## Ownership and concurrency

| Transition | Durable state and required behavior |
| --- | --- |
| Unsupported, known before call | If descriptor/preflight establishes unsupported before `Authenticate` is invoked, do not create a session/refresh marker or write credentials. `Supported:false` returned by an invoked method is not proof of no exchange. |
| Start | Call once. Validate any `UserAction` before exposing output or persisting data. Persist encrypted opaque state and fixed expiry only for a supported result with `NextAction=continue`; invalid action combinations create no session and discard all candidates. |
| Continue | Require an active, unexpired session, enabled account, exact credential revision and loaded state version. Commit one durable claim before calling; competing/stale claims fail before invocation. Validate any `UserAction` before exposing output. Successful advance/finish resolves the claim atomically with the new state or consumed session; invalid results discard all fields and consume the claimed session. |
| Ordinary session expiry | Reject before `Authenticate`; consume/expire and delete encrypted state. Never revive it from client input. |
| Refresh preflight proves no call | Clear the durable refresh marker; there is no provider exchange to recover. This is limited to runtime facts proving invocation never began, not a Connector error/result. |
| Refresh succeeds with replacement credentials | Atomically compare expected credential revision, replace the selected account credentials/revision, and clear the marker. Acknowledged commit is the only persisted-success response. |
| Refresh returns incomplete credentials or continuation state/action | Do not claim refresh success: require non-empty named replacement values and no continuation state/action or `UserAction`. Discard returned values and quarantine the marker; invocation may already have consumed a token. |
| Refresh ambiguous / cancelled after call begins / generic error / `Supported:false` returned | Treat exchange outcome as uncertain. Retain a quarantined marker, fail closed for automatic refresh, and never infer safe delivery/no exchange from a generic error or unsupported result. |
| Same-account concurrent refresh | Serialize by account. A waiter reloads credential revision and re-evaluates whether refresh is still needed; if the prior refresh advanced revision and credentials are valid, skip its Connector call. Otherwise it uses a fresh snapshot/revision and its own durable marker. Transactions end before network work. |
| Different-account refresh | Independent account locks and snapshots; one account's wait/failure cannot block or disclose another account's credentials. |
| External credential update / stale revision | CAS compares the expected revision atomically. On mismatch discard candidates and do not overwrite or silently rerun. A refresh waiter re-evaluates against the latest revision as above. |
| Explicit successful reauthentication | Persist replacement credentials and resolve that account's quarantined refresh markers in the same transaction. A failed/stale write leaves markers quarantined. |

No SQL transaction spans a Connector call. Per-account serialization is runtime
coordination; the credential CAS is the cross-process stale-write guard. A
durable row in `auth_sessions` is created with `kind=refresh`,
`state_envelope=NULL`, the selected account/Connector, expected credential
revision, and `lifecycle=refresh_in_progress` before network exchange. It has a
runtime operation ID and is not a user continuation session. A successful
credential replacement clears it atomically. A persisted `uncertain` row has a
non-secret quarantine reason (`ambiguous_result`, `cancelled_after_call`,
`persistence_failed`, or `restart_in_progress`), expected/current revision, and
timestamps only; never store provider diagnostics, token values, or raw errors.
Quarantined rows are exempt from ordinary expiry/cleanup and remain until an
explicit successful reauthentication resolves them atomically. Restart converts
every leftover `refresh_in_progress` row to `uncertain` and never retries it.
Restart also consumes sessions with leftover interactive invocation claims,
erases their encrypted state and removes the claims; it never repeats an
ambiguous continuation. Unclaimed, unexpired interactive sessions survive.
Expired ordinary sessions are consumed and their encrypted state removed;
account/Connector invalidation consumes ordinary session state but converts any
in-progress refresh to `uncertain`, preserving the quarantine evidence.

## Exchange succeeded, persistence failed

If the Connector may have exchanged/rotated credentials but the atomic credential
write fails, return an authentication persistence failure, never persisted
success. Keep the old credential revision unchanged and retain/quarantine the
prewritten `auth_sessions` marker; if the write error leaves commit outcome
unknown, reload durable state before deciding, and fail closed while uncertain.
Never use returned candidates after an unacknowledged write or blindly repeat
the potentially consumed refresh token. If the database itself is unavailable,
restart recovery converts the durable in-progress marker to `uncertain`; the
account's automatic refresh remains blocked until explicit successful
reauthentication atomically replaces credentials and resolves the marker. This
does not promise recovery of a token that was exchanged but could not be stored.

Ordinary session rows use the encrypted opaque-state envelope and account-bound
AAD from [M3-STORAGE.md](M3-STORAGE.md#state-and-schema); a refresh marker is the
same table with no Connector state envelope. Credential replacements and marker
resolution use one revision-checked atomic transaction. These are runtime-owned
conventions over existing operation fields, not new provider-facing fields.
`internal/core.AuthCoordinator`, `internal/storage/sqlite.AuthSessions` and the
composition adapter implement these rules. The current adapter requires one
pre-provisioned credential per account; multi-credential/enrollment flows and
live provider OAuth remain outside this validated M3 binding. See
[DEC-007](../project/DECISIONS.md#dec-007--durably-claim-interactive-auth-continuations).
