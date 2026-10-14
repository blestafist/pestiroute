# M6 gRPC/Unix-socket Go binding

This binds the accepted [v1 contract](CONTRACT.md) and [Go binding](M2-BINDING.md) to DEC-014's gRPC transport. It adds no Core API or execution semantics. Implementation proceeds only through the downstream task gates. Initial platform evidence is Linux only; this is not an OS sandbox.

## Ownership, topology, versions

`internal/core` continues to own contract values and interfaces and imports no IPC packages. `internal/ipc/proto/connector.proto` owns the schema; generated bindings and codecs live in `internal/ipc/gen` (`go_package = "github.com/blestafist/pestiroute/internal/ipc/gen;ipcgen"`). `internal/connector/external` owns the host proxy/client and process supervisor; a dedicated external-connector command/composition root owns child service registration and implementation wiring. Gateway composition constructs proxies.

Pin the measured candidates: gRPC-Go v1.75.1, protobuf v1.36.6, protoc 33.0, protoc-gen-go v1.36.6, and protoc-gen-go-grpc v1.5.1. Go module dependencies belong only to IPC/child packages, never Core. Check in generated code. From repository root, regenerate with pinned tools on `PATH`:

```sh
protoc -I internal/ipc/proto \
  --go_out=internal/ipc/gen --go_opt=paths=source_relative \
  --go-grpc_out=internal/ipc/gen --go-grpc_opt=paths=source_relative \
  internal/ipc/proto/connector.proto
```

Launch configuration fields are exactly:

| Field | Value / validation |
| --- | --- |
| `Executable` | Required absolute executable path; no PATH lookup. |
| `Args` | Literal `[]string`; no shell expansion or implicit arguments. |
| `WorkingDirectory` | Required absolute existing directory. |
| `EnvironmentAllowlist` | Must be empty; no inherited variable names are permitted. |

The host creates a private per-process directory (0700), binds `host-services.sock` before launch, and passes both endpoint paths to the child. The child binds `connector.sock`; the host dials it. Paths are non-secret. The launched environment consists only of these injected values:

| Name | Value / owner |
| --- | --- |
| `PESTIROUTE_CONNECTOR_SOCKET` | Absolute owned `connector.sock` path; child binds, host dials. |
| `PESTIROUTE_HOST_SERVICES_SOCKET` | Absolute owned `host-services.sock` path; host binds, child dials. |

The host verifies each peer against the expected process identity, unlinks only its owned sockets, and removes the private directory after shutdown. No credentials, auth state, DB path/key, or gateway config are passed in argv/environment or `ComponentConfig.Data`; that field carries only component-owned non-secret configuration. Child stdout/stderr are diagnostics only, captured with 64 KiB total retention; gRPC owns both socket protocols. Initial process supervision is Linux-only, same-user, and not a sandbox.

The child is started as a new process-group leader (`Setpgid=true`, PGID=child PID; no new session). Descendants inherit that group. Startup/connect/handshake share a 30 s deadline; timeout prevents readiness and immediately begins the SIGTERM/SIGKILL/reap sequence below, without a drain. Shutdown stops admission and invokes idempotent Close/drain with a 30 s deadline. Successful Close stops the child server and the child exits; the supervisor allows 2 s for orderly exit. On Close timeout it first revokes callbacks/cancels invocations and sends SIGTERM immediately; if a successfully closed child has not exited after 2 s, send SIGTERM then. In either case, send SIGKILL to the owned group 2 s after SIGTERM if the group remains; the total process-exit/termination/reap window after Close completion/timeout is at most 5 s. The child must reap its own descendants during orderly Close/SIGTERM. The gateway calls `Wait` exactly once for its direct child and checks that the group disappears by the deadline. Killed orphan descendants are reparented and reaped by the OS init process; the gateway does not claim their wait statuses. A remaining group at the hard deadline is a cleanup failure, not a clean restart. Socket/directory cleanup follows termination/reap. A child that deliberately escapes its group is outside the same-user crash-containment guarantee.

If the direct child exits unexpectedly, withdraw readiness, revoke callbacks and cancel its active invocations immediately, then apply the same SIGTERM/SIGKILL/reap window to any surviving group members. Do not admit new work or restart until group/socket cleanup succeeds; never replay active invocations.

Host connects and verifies peer identity before any operation; handshake returns only the fixed IPC protocol ID `pestiroute.connector.v1` and IPC major/minor pair as a typed record using the same bounded fragment envelope (reserved bootstrap call ID under the host-issued process epoch). Reject wrong peer, protocol ID, or unsupported IPC major. Then request the complete Descriptor using its server-streaming RPC and reject absence of the host's exact Connector API pair before Init. IPC minor does not imply API compatibility; unknown required operations/control values reject, optional declared fields may be ignored. Component API pair, IPC version, protocol identifier and implementation version are independent. Descriptor is available pre-Init. Init failure still invokes idempotent Close.

## Wire value mapping

Each Go method remains one logical call. RPC direction selects how the common fragment envelope is transported; it never defines a different Go method or per-fragment invocation:

| Operation | gRPC direction | Logical record flow |
| --- | --- | --- |
| Handshake | Server-streaming | Host sends fixed bootstrap request; child returns protocol ID and IPC version as one fragmented typed record. |
| `Descriptor` | Server-streaming | Child returns one complete fragmented Descriptor before Init; no services. |
| `Init` | Client-streaming | Host streams one ComponentConfig record; child invokes Init once after complete validation; status is gRPC status only. |
| `Health` | Server-streaming | Child returns one complete fragmented Health record. |
| `Capabilities` | Bidirectional | Host sends one CapabilityScope record; child returns one CapabilityResult record. |
| `Close` | Unary | No typed input/result record; status is gRPC status only. |
| `Models` | Bidirectional | Host sends one ModelQuery record; child returns one ModelsResult record. |
| `EstimateUsage` | Bidirectional | Host sends one UsageQuery record plus separate body chunks; child calls once and returns one EstimateResult record. |
| `Authenticate` | Bidirectional | One AuthRequest record in; one AuthResult record out. |
| `Execute` | Bidirectional | One ExecutionRequest and AttemptScope control record plus separate request body chunks in; Head/Complete typed records (or one pre-Head GatewayError record) and Body chunks out under the existing stream lifecycle. |
| `CredentialGet` | Bidirectional | Child sends one named-lookup record; host returns the exact credential bytes in one logical result record. |
| `HTTPDo` | Bidirectional | Child sends request metadata record and body chunks; host returns response metadata record and body chunks. |
| `Log` | Client-streaming | Child sends typed log records; host acknowledges by gRPC status only. |

Every logical typed protobuf request/result/control record uses the same fragmentation/reassembly rules below, including `ExecutionRequest` metadata, `CapabilityScope`, `ModelQuery`, `HeadFrame`, `GatewayError`, and `EstimateResult`. A method is invoked once only after every expected request record and any separate request-body stream is complete and valid; a result is exposed only after every result record validates. Init/Close/Log use fixed bounded gRPC status codes/text, not serialized Go errors. These wire stream shapes do not change Core method signatures or semantics. The protobuf typed `StreamFrame` carries only Head/Complete controls; its Core `Body` payload is never serialized there and must use the dedicated incremental `BodyChunk` carrier. Execute's opaque request body and response Body frames remain separate incremental body chunks, not fragments of an aggregate ExecutionResponse; response body is delivered incrementally without whole-response buffering. The stream still enforces Head/Body*/Complete/EOF, and transport loss does not imply retry safety. Operation wrappers are context/deadline bound and do not retain invocation-scoped services.

## Uniform typed-record fragmentation

Every complete logical typed protobuf record is serialized as its existing protobuf message and carried over the operation's stream as ordered `RecordFragment` envelopes; this is not limited to collections, large bytes fields, or specially identified Go fields. It includes fixed lifecycle/control records such as ExecuteStart/InputComplete and HTTP body-end markers: their small size is not a bypass around the common typed-record framing. Only the opaque `BodyChunk` transport frame is an exception, because it is already independently bounded and incrementally delivered. Each envelope carries process epoch, RPC/call identity, record ID, expected record type/operation, uint64 total encoded length, uint64 byte offset, uint64 sequence, final flag, and fragment bytes. Fragments for one record are contiguous from offset/sequence zero; every fragment repeats the same identity/type/total, the next offset equals prior offset plus fragment length, and exactly the last fragment ends at total length. One empty record is represented by a zero-length final fragment. Interleaving records in one direction, duplicate/skipped/reordered fragments, overflow, type/call mismatch, early end, extra bytes, or stream termination before final reject the entire logical call. Check all uint64 offset/length arithmetic and Go slice representability; do not preallocate from the declared total. Decode and validate each complete protobuf and all operation-specific records before invoking the Go method or exposing its result; never expose a partially decoded control record. Map order remains non-semantic; repeated-field and byte order remain exact. Unknown required fields/discriminants reject under the version rules above.

Each fragment's payload is at most 64 KiB and envelope/encoding overhead at most 4 KiB, within the existing 84 KiB encoded gRPC-frame ceiling. The sender emits one logical record at a time per stream direction and does not enqueue the entire record. The receiver allows at most one incomplete record assembler per stream direction, consumes fragments in sequence, and applies backpressure before reading/sending more. This assembly is only the inherent complete logical typed value required by the existing Go API; there is no new aggregate-size admission cap. Peak assembly/codec memory therefore scales with live logical records and is not claimed to fit the transport-queue budget; only one current record is serialized/reassembled per direction, and temporary wire bytes are released as soon as decode completes. The number of assemblers is bounded by concurrent RPC/invocation/callback limits, but bytes per logical record are not numerically bounded by Core; exhaustion of host/child memory is a process failure, not something IPC can reject without introducing a semantic aggregate limit. A future numeric metadata bound or incremental Core value API requires an accepted contract change/ADR; M6 adds neither.

Record-fragment queue occupancy is bounded independently of the logical record's total length: it counts with body chunks toward the existing 64 frames / 4 MiB application bytes per invocation (at most 5.25 MiB encoded) and 256 frames / 16 MiB application bytes / 21 MiB encoded per process. The separate urgent cancel/close control lane remains reserved and capped at 64 messages / 256 KiB, so saturated record/body queues cannot block cancellation. Queue limits bound outstanding transport data, not the logical typed value already being assembled. No typed record is split into repeated Core method calls. Opaque Execute/HTTP body chunks retain their existing incremental treatment and are never collected into a complete user response.

| Go value | Wire representation / invariant |
| --- | --- |
| `Descriptor` / `APIVersion` | All fields; versions are repeated `{uint32 major, uint32 minor}` constrained to Go `uint16`, nonzero major, unique sorted exact pairs. Kind and enum strings validated against known contract values. |
| `Component`, `Connector`, `InstanceID` | Interfaces are not serialized. Host binds configured InstanceID to its child/proxy; child returns implementation Descriptor.ID separately. Operations are the explicit RPCs below; children cannot choose or impersonate runtime instance IDs. |
| `ComponentConfig`, `Health` | Config bytes remain opaque. Health state preserves ready/unavailable/unknown; its already-bounded/sanitized diagnostic uses the common record fragments when larger than one frame, without truncation. |
| `ComponentConfig.Data`, `AuthRequest.State`, auth state/credential bytes, other typed byte fields | Exact bytes are protobuf-encoded as part of their complete logical typed record, then uniformly fragmented; never text/base64/JSON-transcoded. Preserve empty values; the Go contract does not assign nil-vs-empty semantics to `[]byte` fields. `ExecutionRequest.Payload.Body`, HTTP bodies and Execute response bodies are the explicit incremental-body exceptions below. |
| `ExecutionRequest` | ID, model, capability identifiers, metadata and payload protocol/content type are one typed record and may span fragments. Runtime-owned ID/scope are host-issued; child cannot select account/instance. Payload body uses a separate incremental opaque-body stream. |
| `AttemptScope` | Runtime-issued attempt ID, selected account ID and mode; not copied from client metadata. |
| `RequestMetadata` | Every header key/value, ordered value slice, flag, and scalar is in the fragmented typed record; `Streaming` has explicit presence. `AffinityKnown`, `SessionBound`, and `IngressHeaderBytes` are preserved. `Extensions` maps namespaced extension IDs to opaque protobuf `bytes` slots; the generic binding never parses or coerces values. |
| `Capability`, `CapabilityState`, `HealthState`, `FrameType`, `Outcome`, `ErrorCategory`, `RetryDisposition`, `UsageSource`, `UsageCompleteness` | Stable enum/string values validated against `core` constants; unknown required discriminants reject. Empty retry disposition decodes as unknown, never safe. |
| `HeadFrame` | Complete fragmented control record: protocol/content type, optional HTTP status using presence, all repeated header values, optional GatewayError using message presence. Reconstruct/validate the full Head before commit. |
| `CompleteFrame` | Complete fragmented control record: outcome enum, optional UsageReport and optional GatewayError; succeeded/error consistency as CONTRACT. |
| `GatewayError` | All fields/text fragmented as part of its containing record, including optional RetryAfter as presence plus signed nanoseconds; empty retry disposition decodes as unknown, never safe. Provider/OriginalError remain diagnostics, not client output. |
| `UsageReport` | Each `*int64` counter uses presence; nil differs from zero. Source/completeness are validated enums; unknown is retained. |
| `CapabilityScope`, `CapabilityResult` | Scope strings and complete result map are fragmented typed records. Map entries preserve explicit supported/unsupported/unknown; omission is unknown. |
| `ModelQuery`, `ModelInfo`, `ModelsResult` | Query strings and complete result/list are fragmented typed records. Map entries preserve explicit supported/unsupported/unknown; missing is unknown. Optional `Available` preserves nil versus false. `Supported` remains separate from empty result. |
| `UsageQuery` | Query/scope fields, runtime-owned `MaxBodyBytes` selected route limit, plus RawPayload protocol/content type are fragmented as one typed record; `Payload.Body` uses its separate streamed attachment and is reassembled only within that conveyed limit. Account remains host-selected and validated against invocation authority. See [DEC-015](../project/DECISIONS.md#dec-015--convey-the-admitted-route-body-bound-to-usage-estimation). |
| `EstimateResult` | `Supported`, `Known`, optional UsageReport, Method; nil usage is not zero. Result is streamed in bounded records/fragments and reconstructed atomically; no method-string aggregate cap is added. |
| `AuthRequest` / `AuthResult` | All fields including action, opaque state, candidate credential map, optional user action and optional absolute expiry timestamp. State is carried as bytes and converted to/from the Go string without text coercion. Expiry is UTC instant with protobuf timestamp validity; no inference from credential bytes. `PollInterval` preserves duration nanoseconds. |
| `ExecutionResponse`, `Stream`, `InvocationServices`, `HTTPDoer`, `CredentialAccess`, `slog.Logger` | Not values on the wire. ExecutionResponse is represented by the Execute stream; invocation services are reverse RPCs. |
| Errors and Go interfaces | Go `error`, `Stream`, `CredentialAccess`, `HTTPDoer`, logger, contexts, and interface handles are never serialized. Structured GatewayError fields use the common fragmented typed record. Init/Close transport status uses a gRPC status code and fixed bounded status text only; interfaces become scoped RPCs. |

Each outer fragment envelope is validated before append (frame size, identity/type, sequence, offset/length arithmetic, and end marker). Only after full-record reassembly is the inner typed protobuf decoded and validated (enum, required presence, field semantics, and duplicate/unknown required controls); decoding never silently normalizes contract distinctions. These checks do not impose an aggregate-record limit. Map order is not semantic; repeated values retain order. A slot's bytes are produced/consumed only by that extension's separately declared and negotiated codec; generic code never claims arbitrary Go `any` values are serializable. A codec absent from the negotiated declaration is not invoked.

## Reverse services and authority

Every reverse call carries an unguessable invocation ID plus process epoch and monotonically unique call ID. Host validates all three against a live invocation; it obtains selected account and instance from its own invocation record, never from child-supplied account fields. Calls from stale epochs, completed/cancelled invocations, unknown calls, or mismatched account authority fail closed. Close/cancel revokes authority before acknowledgement; late results are discarded. Callback deadlines are bounded by the parent invocation deadline and service-specific limits. At most 8 outstanding callbacks per invocation and 32 per process; cancellation/control uses a separate reserved control path and progresses while data queues are full.

* Credential service accepts only a named credential lookup and returns bytes for the selected account, or a sanitized denial. Each lookup has a 2 s deadline bounded by the parent invocation; it is invocation-scoped, never exposes storage, and is revoked on cancellation/end/Close. Child never persists the result.
* HTTP service is the runtime-controlled equivalent of `HTTPDoer.Do`: carry method, URL, host override, headers, content length and request body; response carries status code, headers, content length and response body, incrementally. Body data is chunked, never aggregated by IPC; each chunk is bounded to 64 KiB. Header aggregate is at most 16 KiB per request/response. The host owns request context/deadline, DNS/dial/TLS, redirect policy, transfer framing, hop-by-hop filtering, connection reuse, response-body closure, and response transport metadata; child cannot set `RequestURI`, TLS state, remote address, or replay body. No automatic retry; delivery becomes ambiguous when request body delivery begins. Callback cancellation closes request/response bodies and releases both sides.
* Logging service carries bounded structured level/message/attributes to the runtime logger (8 KiB maximum per record), with sensitive values redacted and payload/body/credential fields prohibited. It is best effort; queue at most 256 records process-wide and drop excess rather than block execution. Child diagnostics are separately bounded.

## Payload-bearing operation audit

The uniform `RecordFragment` envelope carries every typed protobuf value, including unbounded strings/maps/slices and nested control/error records. Its fixed per-fragment and outstanding-queue bounds do not set a limit on encoded logical-record length. The application method is called once per complete request record; only a fully reconstructed, validated result record is returned.

| Go payload field / operation | Wire carrier and bound |
| --- | --- |
| `ExecutionRequest` and `AttemptScope` / `Execute` | Complete typed control records and ExecuteStart/InputComplete use `RecordFragment`; `Payload.Body` is excluded and travels as ordered `BodyChunk*`, reassembled exactly under the selected route's effective admitted-body limit before one Execute call. |
| `UsageQuery` control fields / `EstimateUsage` | Complete typed query record uses `RecordFragment`, including runtime-owned `MaxBodyBytes`; `Payload.Body` is a separate ordered body-chunk attachment reassembled under that effective admitted-body limit. EstimateUsage runs once. |
| `EstimateResult`, including `Method` / `EstimateUsage` | Complete result is one or more bounded `RecordFragment`s in the same bidirectional call; validate/reconstruct atomically before returning. No aggregate method-string limit. |
| Descriptor, `CapabilityScope`/`CapabilityResult`, `ModelQuery`/`ModelsResult` | Every complete request/result, all nested fields and collection entries use `RecordFragment`; preserve order/map/presence semantics and expose no partial value. No aggregate size limit absent from Core. |
| `ComponentConfig.Data`, `AuthRequest`/`AuthResult`, credential values, `HeadFrame`, `CompleteFrame`, `GatewayError` | Complete typed records use the same fragments, preserving every field and opaque bytes; no hand-maintained per-field exception list or aggregate value cap. |
| `HTTPDo` request/response metadata | Typed method/URL/headers/length/status/control records use `RecordFragment`; existing 16 KiB aggregate HTTP-header semantic limit still applies. |
| `ExecutionRequest.Payload.Body`, `HTTPDo` bodies, Execute `BodyFrame.Data` | Dedicated opaque body chunks, each at most 64 KiB; deliver response/callback bodies incrementally and never collect the whole user-facing response. |
| `CredentialAccess.Get` bytes / `CredentialGet` | The complete typed credential result is carried by common record fragments and reconstructed as the existing returned `[]byte`; invocation identity and callback limits apply. |
| `Log` records | The typed log record uses `RecordFragment`; the existing 8 KiB per-record and 256-record process queue bounds remain in force. |

Request-body aggregate limits are never inferred from frame size: use the selected route's effective pre-existing admitted limit, not an M6-specific value. A 1 MiB body is a boundary fixture only, not a universal ceiling. Other existing semantic limits remain authoritative (including HTTP headers, health diagnostic, logging and configuration-loader limits); do not truncate or cap other logical records. Record serialization/reassembly is the only aggregate materialization required to honor a typed Go method value. It is not counted as queued transport data and has no newly invented admission maximum. The current M6-007 codec audit must cover every request/result record and each separate body carrier, not only Execute.

## Limits and lifecycle

The measured M6-002 ceilings are fixed: maximum encoded gRPC wire frame 84 KiB including the five-byte gRPC prefix (gRPC serialized-message limit 84 KiB minus five bytes); body chunk at most 64 KiB + at most 16 KiB aggregate HTTP headers + at most 4 KiB envelope/framing. Each decoded gRPC message is checked against the wire-frame ceiling before conversion. The shared bounded data queue holds at most 64 frames / 4 MiB application bytes per invocation (5.25 MiB encoded) and 256 frames / 16 MiB application bytes / 21 MiB encoded process-wide, with at most four concurrent invocations. Opaque body chunks and typed-record fragments both consume these same queue credits, so metadata traffic cannot bypass backpressure or inflate the stated queue ceiling. These limits cover queued transport fragments only, not the inherent logical typed record currently being reassembled; no logical aggregate-size cap is inferred. Do not reserve/allocate from declared total length before fragments arrive. The urgent cancel/close lane is separate, reserved, and capped at 64 messages / 256 KiB process-wide. Limits include HTTP callback traffic; producers block under data backpressure while cancel/close proceeds separately.

`RawPayload.Body` is not one gRPC message. Host sends non-empty chunks of at most 64 KiB in stream order; empty body is `ExecuteStart` followed by `InputComplete` and no chunks. Child appends chunks to a bounded reassembly buffer and calls Execute only after `InputComplete`, preserving every byte and exact order. The aggregate is bounded by the existing selected route's effective admitted-body limit (ingress/route/Connector minimum); a lower route limit applies, and M6 adds no payload limit. A 1 MiB body is a boundary fixture, not a universal maximum. Before append, check `chunkLen <= routeLimit-currentLen` without overflow; reject cumulative excess before child Execute. The request bound is distinct from the 64 KiB per-frame and 84 KiB encoded-wire limits. Response Body frames remain incremental and independently bounded; no whole-response buffering.

Other hard bounds: 30 s socket connect/handshake; 30 s Init; 2 s Health/Capabilities/Descriptor calls; every operation also obeys caller context; 64 KiB total retained diagnostics; 30 s graceful drain then cancel; 5 s terminate-to-kill/reap; at most 3 restarts in 60 s, exponential backoff 100 ms–2 s. Restart creates a fresh epoch and serves only new work; it never retries an active operation. Startup/handshake failure prevents readiness. A hung callback, saturated queue or expired invocation cancels that invocation; process-wide limit breach marks child unhealthy and supervisor terminates it after cancelling/revoking its invocations.

| Observed delivery state | Result / retry rule |
| --- | --- |
| Failure proven before Execute request is sent | Unavailable, safe-to-retry only if Core's existing eligibility/policy/deadline rules also permit; no child execution occurred. |
| Request send began, acknowledgement absent, or transport loss before Head | Delivery unknown; mark attempt incomplete/unsafe or unknown; never replay. |
| Head received but not handed to Adapter | Core applies existing pre-commit classification; only explicit retryable + safe + policy-eligible result may retry. IPC loss itself is not such a classification. |
| Head handed to Adapter (committed), then crash/EOF/error | Incomplete attempt, no fallback/replay; close stream and finalize once using known usage. |
| Valid Complete then orderly EOF | Preserve outcome/error/nullable usage, finalize once. Complete without EOF, duplicate terminal/control, malformed frame, or RPC failure is incomplete/contract failure, never success. |
| Cancel/deadline/Close | Revoke callbacks, interrupt RPC and streams, release queues; bounded acknowledgement is not proof provider work did not occur and grants no retry safety. |
| Incomplete/malformed typed record | Reject the entire logical record/call; do not invoke a malformed request or expose a partial result. If Execute delivery already began, preserve unknown/unsafe delivery classification; never infer replay safety from fragment failure. |
| Init partial failure / drain expiry | Close idempotently; stop admission, drain to deadline, cancel remaining calls, terminate/kill/reap child and remove owned socket. |

The binding does not claim portability, malicious-child containment, or transport-level proof of upstream delivery. Core's existing commit, retry, accounting and cancellation rules remain authoritative.

## Extension-slot compatibility rule

The Go contract's `RequestMetadata.Extensions` remains `map[string]any`; it does not promise a universal serialization for arbitrary values. Per [CONTRACT](CONTRACT.md#execution-request), extension meaning and interpretation belong to an explicit declaration, and undeclared required extensions fail closed. The binding therefore uses a protobuf `map<string, bytes>` only as opaque transport slots. Each future extension must separately declare/negotiate its namespaced key and its own byte codec; the generic IPC layer does not choose JSON, protobuf `Any`, stringification, or type erasure.

There are no declared extension codecs in M6 v1. Therefore a v1 request with a non-empty `Extensions` map has only undeclared entries and is rejected before child execution; an empty map round-trips as empty. This is contract-preserving fail-closed behavior, not a Core type/API change. No production producer or consumer currently uses this field (`internal/core/execution.go:37`); future use requires the declaration and codec before enabling a slot. This planner decision resolves the prior ambiguity without asserting arbitrary Go `any` values are universally serializable.

## Downstream gates

Prepared cards: [M6-007](tasks/M6-007.md) through [M6-011](tasks/M6-011.md). M6-007 has advanced through READY and is ACTIVE in worker verification; M6-008 through M6-011 remain DRAFT until their own predecessors and checks are accepted. Each card fixes its scope/check; later operations retain their own M6 gates.
