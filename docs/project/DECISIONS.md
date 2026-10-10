# Decisions and Open Questions

## Statuses

**Accepted** means a project constraint or direct clarification of its boundaries. **Proposed** is a working choice for the first implementation that needs validation through code. **Open** is a question with a concrete decision milestone. A change to an accepted constraint must be recorded here and reflected in the affected project/specification documents; a task card cannot silently override it.

## Decision Registry

| ID | Status | Decision | Rationale |
| --- | --- | --- | --- |
| D01 | Accepted | OpenAI Responses is the primary external contract | Enables agent workflows without requiring custom LLM-specific language |
| D02 | Accepted | Core contains no backend-specific code | Enables extension through connectors |
| D03 | Accepted | Payload is opaque; native body is preserved byte-for-byte | Ensures compatibility with unknown fields and extensions |
| D04 | Accepted | Northbound adapter is separated from Core | Keeps API parsing separate from routing and limits |
| D05 | Accepted | Provider-specific translation and tokenizer belong to the connector | Prevents API differences from being normalized inside the core |
| D06 | Accepted | Third-party connectors execute out-of-process | Ensures their failure does not bring down the gateway |
| D07 | Proposed | Go, `net/http`, single module | Provides minimal infrastructure for M0–M3 |
| D08 | Proposed | SQLite for single-node state | Enables self-hosted deployment without separate database |
| D09 | Accepted | `Head / Body / Complete` as the v1 transport stream | DEC-004 fixes lifecycle semantics without an LLM event model |
| D10 | Proposed | gRPC over Unix socket for IPC | Will validate overhead and host-service lifecycle in M6 |
| D11 | Accepted | Model rewrite is not a native passthrough | Requires body modification to be explicit |
| D12 | Proposed | CLI for initial admin operations | Enables account/key management without UI milestone |
| D13 | Accepted | Client Protocol Adapters own client request parsing, protocol validation, envelope creation, and client response formatting | Supports multiple northbound protocols without protocol structures or conversions in Core; provider-specific translation remains in connectors |
| D14 | Accepted | Responses is primary for agents; Chat Completions is a compatibility protocol; both are first-class northbound interfaces | Adds client compatibility through adapters while preserving the opaque transport envelope and connector model |
| D15 | Accepted | CONTRACT.md owns the v1 Adapter–Core Runtime–Connector boundary; semantic changes require ADR | Prevents per-provider API growth and undocumented compatibility breaks; see DEC-004 |
| D16 | Proposed | M3 protected startup loads its master key from a permission-checked external file | Keeps key material out of SQLite and configuration/environment surfaces; see DEC-005 |
| D18 | Accepted | Defer official Anthropic verification as an M4 completion prerequisite only after offline gates pass; retain it before production release | Human governance accepted the narrow deferral on 2026-10-04 and authorized the documentation batch; separates offline implementation quality from official-provider entitlement; see DEC-008 |

## Contract ADRs

These accepted ADRs explain the existing constraints and establish the internal contract baseline. `Dxx` rows remain the compact registry; `DEC-xxx` identifies a rationale record, not a renumbering of that registry.

### DEC-001 — Why Core Has No LLM Abstractions

- **Status:** Accepted; elaborates D02 and D03.
- **Context:** Client and backend protocols evolve independently in tools, reasoning, multimodal content, and extensions.
- **Decision:** Core executes opaque payloads with transport/routing metadata. It must not define universal messages, tools, reasoning objects, or response chunks.
- **Alternative rejected:** Normalize every request into a shared LLM schema. This loses unknown semantics and forces Core/API changes whenever protocols evolve.
- **Consequences:** Protocol Adapters and Connectors own semantic validation and transformation. Capabilities describe eligibility, not a normalized payload language. Native passthrough is the compatibility baseline.
- **Verification:** Unknown fields survive byte-for-byte; Core has no protocol-parser dependencies; new backend support does not add provider branches.

### DEC-002 — Why Responses Is a Protocol, Not the Internal Model

- **Status:** Accepted; elaborates D01, D04, D13, and D14.
- **Context:** Responses is the primary agent-oriented client protocol, but other client formats must be independently supportable.
- **Decision:** Responses is one versioned protocol at the Protocol Adapter boundary. The internal envelope carries a protocol identifier and opaque bytes rather than using Responses structures as a shared representation.
- **Alternative rejected:** Convert all client requests to Responses before execution. That makes one external protocol an internal dependency and constrains future formats.
- **Consequences:** Chat Completions remains a first-class compatibility interface. Each Connector declares accepted protocols; adding an adapter does not automatically make every backend compatible.
- **Verification:** The internal contract defines no Responses-specific fields; routing matches declared protocol and capabilities.

### DEC-003 — Why Connectors Own Provider Translation

- **Status:** Accepted; elaborates D05 and D13.
- **Context:** Mapping provider fields, streaming events, tokenization, and accounting depends on backend-specific behavior and capabilities.
- **Decision:** Connectors privately translate the admitted client protocol to/from their Backend Provider. Protocol Adapters decode client requests and encode client transport; Core only orchestrates. The public Connector API exposes execution, not named protocol-conversion methods.
- **Alternative rejected:** A translation hub in Core or a shared provider-neutral message model. Either leaks provider assumptions into runtime infrastructure.
- **Consequences:** Connectors may share private protocol helpers, while their public contract stays stable. Adapters preserve already-compatible bytes rather than translating twice. Unsupported semantic transformations are explicit failures.
- **Verification:** Translation tests cover declared semantics and negative cases; native bytes and tool relationships are preserved where promised; no provider conversion logic appears in Core.

### DEC-004 — Stable Internal Execution Contract v1

- **Status:** Accepted; establishes D09 and D15 before implementation.
- **Context:** Earlier documents used draft Go signatures, short capability keys, and conceptual streaming roles. Leaving them as competing contracts would create incompatible first Connectors.
- **Decision:** [CONTRACT.md](../implementation/CONTRACT.md) owns the language-neutral v1 boundary: `ExecutionRequest`/`RawPayload`, a transport `ExecutionResponse`, component lifecycle, adapter and Connector operations, namespaced three-valued capabilities, and common error metadata. `Head / Body / Complete` remains the frame set; HEADERS/DATA/CONTROL/USAGE/ERROR/END are mapped roles. Existing auth, model-discovery, and usage-estimation responsibilities are retained.
- **Alternatives rejected:** Freeze Go/IPC syntax before validation; introduce six underspecified frame variants; use only a retryable boolean for replay; or keep multiple capability spellings.
- **Compatibility:** This supersedes documentation drafts, not released plugins. Existing short capability keys must be replaced in examples; future semantic changes require an ADR and explicit versioning policy. Protocol-specific payload additions alone do not change the internal contract.
- **Verification:** M2 conformance must cover frame lifecycle, opacity, capability eligibility, scoped services, cancellation, and retry safety; M6 must preserve the same behavior across IPC. Concrete bindings may refine representation, not silently change semantics.

### DEC-005 — External Master-Key File for Protected M3 Startup

- **Status:** Accepted and verified in M3; does not change the accepted v1 execution contract.
- **Context:** M3 encrypts stored credentials and authentication temporary state. The earlier stack candidate allowed either a separate file or environment variable, but a key must not share storage/configuration or be silently replaced by a development fallback.
- **Decision:** Protected startup reads a 32-byte key from an operator-provisioned external regular file owned by the service account with mode `0600`; reject symlinks, broader permissions, missing/wrong-length keys, and do not fall back to environment variables or M2 JSON. Ciphertext carries format/key versions and uses record-bound authenticated data. Rotation tooling is not included in M3.
- **Alternatives:** Environment variable (more likely to leak through process diagnostics/inherited environments); key in YAML/SQLite (same compromise domain as protected data); silently generated key (would make persisted data undecryptable after restart).
- **Consequences:** Deployment must provision and retain the matching external key version and filesystem permissions before protected startup. Losing the key makes corresponding ciphertext unavailable; M3 does not claim rotation or recovery from key loss. Development compatibility startup remains distinct and explicit.
- **Compatibility/migration:** Applies only to new protected M3 startup; existing M2 JSON is unchanged. No v1 API/frame semantics change. Data/key format changes require versioned envelopes and a migration plan.
- **Verification:** M3 startup tests must cover valid permissions/length, missing or invalid key, symlink/permission rejection, encrypted credential and temporary-state restart round trips, and absence of key/plaintext in SQLite and logs.

### DEC-006 — Separate Request Closure from Attempt Settlement

- **Status:** Accepted and verified in M3-046; runtime-owned binding only.
- **Context:** A failed attempt must settle before fallback is evaluated. Closing the request during that settlement blocks fallback; leaving it admitted until restart makes normal usage queries incorrect.
- **Decision:** `FinalizeAttempt` persists attempt outcome, usage and reservation settlement. Dispatch calls the separate idempotent `FinishRequest` only after execution ends, before exposing Complete or returning a terminal error. Closure rejects active attempts/holds and uses the last persisted attempt outcome. Startup recovery closes the gap if the process dies between settlement and closure.
- **Alternatives:** Couple settlement to the runtime's retry decision with a final-attempt flag; close requests only during restart. Separate closure keeps the repository independent of retry policy and normal requests terminal while the process runs.
- **Compatibility:** No Connector, frame, payload or v1 semantic change; one new Core-owned accounting operation, no ledger schema rewrite. Existing settled rows/charges remain authoritative.
- **Verification:** Success is terminal before restart; failed attempts remain eligible for bounded fallback; concurrent duplicate closure is idempotent; failed closure suppresses Complete and withdraws protected readiness.

### DEC-007 — Durably Claim Interactive Auth Continuations

- **Status:** Accepted and verified in M3-046; runtime-owned binding only.
- **Context:** Per-account in-memory locks cannot exclude a second CLI process or prevent replay after a process dies during an opaque provider continuation. Credential CAS prevents stale writes but cannot undo a repeated provider call.
- **Decision:** Commit an exclusive `auth_session_invocations` claim before Authenticate. Check enabled account, expiry, credential revision and the encrypted-state nonce as an opaque version token. Advance/finish resolves the claim atomically with state persistence; startup consumes/erases sessions with leftover claims. A stale reader cannot claim an already-advanced state even if credential revision is unchanged. No SQL transaction spans provider work.
- **Alternatives:** In-memory locking alone; consume every session before the call and lose legitimate multi-step continuation; infer replay safety from generic provider errors. The durable claim preserves acknowledged next steps and fails closed on uncertain calls.
- **Compatibility/migration:** Additive schema v6 leaves migrations v1–v5 unchanged. Stop the gateway, back up consistently and run `admin migrate`; protected startup refuses older schemas, older binaries refuse v6. Existing unclaimed, unexpired sessions survive. No public Connector or v1 semantic change.
- **Verification:** Independent database handles have one claim winner; stale readers cannot claim advanced state; persistence/restart cannot revive an ambiguous continuation; account disable invalidates state and rejects pending credential replacement.

### DEC-008 — Accepted Narrow Deferral of Official Anthropic Verification

- **Status:** Accepted by human governance authority on 2026-10-04. The authority explicitly accepted the proposed offline M4 closure path and authorized the full documentation batch autonomously.
- **Context:** M4 local translation and protected-operation checks are distinct from official-provider evidence. M4-040 verified only plain-text streaming on a compatible endpoint using a test-only model override. M4-041 stopped after one HTTP 200 response ended in turn 1 `tool_use`, classified `other` / `unexpected_tool_name`; tool identity, usage and further behavior remain unknown. This is not evidence of official Anthropic behavior or proof that the compatible endpoint's tools are unsupported. M4-036 official evidence is unavailable and remains BLOCKED; M4-037 and M4-038 remain DRAFT.
- **Decision:** M4 may be completed through offline acceptance, deterministic conformance, and required negative/error cases without completing official live verification. Official Anthropic verification remains mandatory before production release. The dependency and roadmap revisions implementing this accepted decision are recorded separately in `ROADMAP.md`, `TASKS.md`, and the downstream cards; this ADR alone does not declare M4 complete. M4-036 remains BLOCKED until official evidence is captured, and M4-041 remains BLOCKED with its compatible-endpoint tool-cycle acceptance unmet. Neither status is waived or passed by this decision.
- **Alternatives rejected:** Keep the original official gate as an M4 prerequisite (safe, but blocks all M4 closure absent access); substitute a compatible endpoint or infer official support from offline tests (invalid evidence); silently mark M4-036 complete or promote M4-037/M4-038 (unauthorized gate waiver).
- **Non-negotiable verification obligations:** Offline M4 acceptance, deterministic conformance, and all required negative/error cases remain mandatory, with no skips standing in for required scenarios. Before production release, official live verification remains mandatory. It must capture sanitized, versioned, semantically comparable direct Messages and translated gateway evidence for text and streaming; tool declaration/use and linked results across multiple rounds; parallel calls; tool choice; usage; the supported reasoning policy; and cancellation, explicitly separating local transport cancellation from remote-compute behavior. Record exact client, model, API/profile, source provenance, event order/timing and actual usage availability. Unrun behavior stays unknown; failures remain blockers and require evidence-based review, not retries beyond authorization or inferred support. The compatible endpoint/model override is test-only and grants no custom-model or Anthropic entitlement claim.
- **Compatibility / impact:** No v1 contract, API, production code, capability semantics, or Core boundary changes; no capability relaxation or M5 scope expansion. Offline milestone quality evidence is assessed independently of official-provider entitlement. Official support remains unverified and production release remains gated until the required live evidence is captured and reviewed. No production support claim follows from offline completion.
- **Acceptance condition:** Satisfied by explicit human governance acceptance on 2026-10-04, including the reduced claim boundary, offline requirements, mandatory pre-production live obligation, and authorization to perform the bounded documentation synchronization autonomously.
- **Verification:** On any later accepted path, audit offline conformance and negative cases separately from the exact official live evidence matrix; verify provenance, privacy, event/usage/cancellation scope, and every public support claim against evidence. This ADR itself requires documentation/link and registry/dependency checks only.

### DEC-009 — Separate Safe User Actions from Opaque Auth State

- **Status:** Accepted for the M5.1 Codex device-auth binding under the
  authorized M5.1 roadmap and M5.1-002 scope.
- **Context:** `AuthResult.State` is opaque Connector continuation state encrypted
  by Core. Reusing it for display data or printing it can disclose device
  identifiers, PKCE material, or tokens, while the current result has no generic
  way to present a verification URL/code to the operator.
- **Decision:** Add optional `AuthResult.UserAction *AuthUserAction` with
  `VerificationURI`, `UserCode`, and `PollInterval`. This is a presentation-only
  projection, separate from opaque `State` and credential values. The Connector
  extracts only the safe action from provider response; the URI is absolute
  HTTPS without embedded credentials, and the value never contains a
  device-auth identifier, PKCE material, token, or raw response.
  Core projects it to transient `AuthSession.UserAction` for the admin
  presentation seam and does not persist it as continuation state. Admin output
  may display the URI, code, poll interval, opaque runtime session handle, and
  runtime expiry, but not `State` or credentials.
- **Validation and discovery:** Core validates before exposing: URI is 1–2048
  printable ASCII bytes with no spaces/control bytes, absolute HTTPS with a
  host and no userinfo; code is 1–256 printable ASCII bytes with no controls;
  poll interval is positive. `UserAction` is valid only with
  `Supported=true, NextAction=continue`. Nil presence is explicit, self-
  describing discovery; a nil action on continue is valid for legacy opaque
  continuations that do not require operator interaction. Any Connector flow
  that does require user interaction MUST return a valid action. Connector
  auth-flow conformance tests enforce this obligation; Core cannot infer a
  missing action from opaque state, and no new capability is introduced.
- **Lifecycle and security:** Start calls Authenticate once. A continuing result
  creates the existing encrypted opaque session with fixed runtime expiry.
  Each explicit continuation makes at most one poll; a pending result can return
  a new user action and provider interval, which the caller observes before its
  next poll. There is no runtime polling loop and no provider response extends
  expiry. Caller cancellation follows M3-AUTH: before invocation it consumes
  the session; after invocation begins it consumes the durably claimed
  continuation conservatively. DEC-007 claim ownership, revision checks,
  acknowledged credential persistence, and no-SQL-during-provider-call rules
  are unchanged. An action on a completed, unsupported, or refresh result is
  invalid. For any malformed/inconsistent action result, discard all result
  fields before exposure/persistence; Start creates no session, Continue
  consumes its already-claimed session/claim, and an invoked Refresh result is
  quarantined. If claim consumption cannot commit, expose no result and rely on
  DEC-007 restart recovery to consume the ambiguous claim. Expired or stale
  sessions are rejected before provider work.
- **Alternatives:** Encode the action in opaque state, expose provider-specific
  structures, or have Connectors print directly. Each couples presentation to
  secrets/provider format or bypasses runtime ownership; rejected.
- **Compatibility / impact:** Additive optional AuthResult field and one generic
  AuthUserAction value; Core exposes the result transiently as
  `AuthSession.UserAction`. Presence is self-describing (`nil` means absent), so
  older producers may omit it and older consumers may ignore it. A nil action
  remains valid for legacy continue flows that do not require user interaction;
  interactive Connector flows are contractually required to return one and
  conformance-tested. No new
  operation, provider branch, database column,
  encrypted-state format, or session-lifetime change. Existing Connectors that
  omit the field remain valid for auth flows not requiring user presentation;
  implementations requiring operator action must supply it. CONTRACT and
  M3-AUTH are synchronized. This is an additive v1 semantic extension under the
  existing Connector API version; old consumers ignore the optional value and
  old producers yield nil. No protocol-adapter or inference contract changes.
- **Verification:** Paper-walk start, pending poll, success, expiry/timeout,
  cancellation, and competing/stale continuation; audit that user output never
  contains opaque state or credentials. Verify synchronized specifications and
  existing offline checks before dependent implementation proceeds.

### DEC-010 — Carry Credential Expiry with Auth Results

- **Status:** Accepted under M5.1-003 planner arbitration.
- **Context:** The v1 `AuthResult` returns named candidate credential bytes but no expiry. The M3 SQLite credential row already has an expiry column and revision CAS, while the current composition adapter clears expiry on replacement. A Codex OAuth access token must have trusted absolute expiry persisted atomically with its encrypted access/refresh/account-identity bundle. Inferring expiry from provider JWTs or parsing the provider bundle in Core/runtime would violate Connector ownership.
- **Decision:** Add optional absolute `CredentialExpiresAt` to `AuthResult`, produced by the Connector from a successful trusted exchange response. Core generically rejects an invalid supplied time and carries it as runtime metadata; profile conformance requires it where expiry is mandatory. The `AuthCredentials` runtime snapshot carries expiry. `ReplaceAuthCredentials`, `ResolveRefresh`, and `FinishAuthSession` atomically CAS-replace encrypted candidate bytes and expiry in the same transaction that advances revision and resolves auth state. Existing profiles may omit it and retain no-expiry behavior. For `Authenticate` with `AuthRequest.Action=refresh`, that action selects auth-scoped credential resolution, which may return the selected account's expired encrypted bundle only to refresh; ordinary inference access remains expiry-gated. Freshness margin ownership/policy belongs to the selected Connector profile, passed to generic coordination without a provider branch; waiters skip only when the latest snapshot expires strictly beyond now plus that margin. A refresh CAS mismatch after exchange discards candidates, quarantines as `ambiguous_result`, and never retries or reuses the possibly rotated token.
- **Alternatives:** Parse JWT/provider bundle in Core or storage (rejected: provider semantics outside Connector); encode expiry as a special named credential and parse it generically (rejected: weakly typed, accidental persistence/merge hazards); infer expiry from local token timing (rejected: not authoritative).
- **Compatibility / impact:** Additive optional field; existing producers may omit it for profiles not requiring expiry and retain current no-expiry behavior. A producer/profile relying on expiry requires a consumer that persists the field; an older consumer that ignores it is not compatible with that profile. This extends v1 auth-result and scoped credential-access semantics and is synchronized in CONTRACT.md, M3-AUTH.md, and M5.1-BINDING.md. Existing `credentials.expires_at` stores expiry; no schema change is needed. No provider-specific Core logic is permitted.
- **Acceptance condition:** Satisfied by planner arbitration assigning M5.1-003 to resolve this semantic gap and explicitly authorizing DEC-010 acceptance and specification synchronization.
- **Verification:** M5.1-003 paper-walked bundle opacity, trusted expiry provenance, nil compatibility, atomic CAS/expiry behavior, and stale/ambiguous refresh handling; synchronized CONTRACT, M3-AUTH, and M5.1-BINDING. Local offline checks require no live provider call.

### DEC-011 — Opt-in Codex Responses Lite Text Profile

- **Status:** Accepted, amended by M5.1-047 after direct custom-tool evidence
  and protected offline gateway verification. Scope remains profile-, model-,
  and account-specific; this is not global Codex CLI compatibility.
- **Context:** The pinned official Codex source declares `gpt-6-luna` as using
  Responses Lite, while the existing standard Codex profile remains the default.
  The authorized direct Luna Lite capture `luna-7-completion.json` records HTTP
  200, completed stream drained to EOF, one reasoning item, and 27 reasoning
  tokens with `effort: high` and `context: all_turns`; reviewer PASS is recorded
  by `ses_eeff1a4c6ffex6A3uqMCHiJXCp`. This is scoped direct evidence only, not
  gateway-pair or full M5.1-043 evidence. The earlier plain request omitting
  reasoning was rejected with field `reasoning.context`; that rejects the
  omission, not the model. M5.1-044 directly verified one function tool
  emission and a fresh two-request function roundtrip (HTTP 200/200); its
  explicit parallel request was rejected (HTTP 400). The direct capture
  observed one emitted call only; it did not test or impose an output-call
  count ceiling. M5.1-047 directly completed one harmless custom-tool
  roundtrip in two HTTP 200 requests, with completed terminals and usage
  observed. This is limited to the observed custom Lark shape; it does not
  establish live gateway behavior, other grammars, parallel support, or general
  entitlement.
- **Decision:** Specify one opt-in connector profile, with the exact literal
  `codex-responses-http-sse-lite-v1`, selected only through the existing
  `componentConfig.Profile` setting. It is restricted to configured model
  `gpt-6-luna` and the `plain_text` scenario. The Connector adds exactly
  `x-openai-internal-codex-responses-lite: true` from trusted profile
  configuration; caller headers cannot select or override it. It uses scoped
  credentials and native Responses payload bytes. For this opt-in profile only,
  the Connector sends honest identity metadata `originator: pestiroute` and
  `User-Agent: PestiRoute` (no official-client or version impersonation), plus
  one fresh random UUID per attempt reused as `session-id`, `thread-id`, and
  `x-client-request-id`. These values are ephemeral request correlation only;
  they do not create persistent conversations or a Core session API. Suppress
  caller-supplied Lite, originator, user-agent, and correlation-header values,
  including case variants; Lite selection comes from trusted configuration and
  account authentication remains solely scoped-credential-derived. Never log
  the UUID or raw header values. This matches the known successful direct
  identity shape as a compatibility recipe, not a proven provider requirement;
  the later paired-runner mismatch does not establish that missing headers
  caused its unknown 400. No Adapter/Core API growth,
  automatic Lite conversion, or caller-controlled profile/header. The caller
  forms the Lite-shaped body, including its `additional_tools` prefix and
  `reasoning.context: all_turns`; the Connector preserves it byte-for-byte.
  Non-native modes and all other profile/model combinations reject Lite
  selection. The
  Lite profile may declare `llm.reasoning: Supported` only after verified
  direct evidence and offline profile-scoped passthrough, ordering, and usage
  tests. The captured direct proof used `effort: high`; the official catalog's
  `medium` default is not required and must not be substituted into that proof.
  A matched gateway pair is additionally required before publishing a working
  support claim for the route.
  `llm.tools: Supported` is permitted only for exact `openai.responses.v1` /
  `native` / `gpt-6-luna` / configured account scope, and covers client-owned
  function tools in their established Lite forms (direct definitions or the
  official `functions` namespace) plus custom tools only in that namespace
  inside a Lite `additional_tools` developer input item. The admitted custom
  envelope is `type:"custom"`
  with name, description, and
  `format:{type:"grammar",syntax:"lark",definition:<non-empty string>}`;
  function `parameters` are not accepted on custom definitions. The Connector
  checks this envelope but does not parse/compile the grammar, execute tools, or
  alter native body bytes. Custom definitions require `tool_choice:"auto"`;
  top-level custom definitions, forced choices with custom definitions,
  unproven grammar forms/syntaxes, hosted tools, and other namespaces fail
  before send. Custom call items, input deltas, linked outputs, and encrypted
  reasoning history remain opaque and are forwarded without a tool-item count
  ceiling. With `parallel_tool_calls:false`, emitted function/custom call items
  remain client-owned; this does not establish provider-side parallel support.
  A true `parallel_tool_calls` requires `llm.tools.parallel`, which remains
  `Unknown` in every Codex scope and is rejected before send. Standard-profile
  `llm.tools` and `llm.reasoning` remain `Unknown`. No
  capability gate may be bypassed. The
  existing standard literal `codex-responses-http-sse-v1` and its default
  behavior remain unchanged.
- **Alternatives rejected:** Silently selecting Lite from a model name (mixes
  profile selection with routing); changing the standard profile (breaks its
  established default); normalizing arbitrary Responses bodies into a universal
  Lite shape (violates native opacity); and inferring production support from
  direct-only evidence as proof of live gateway/provider behavior (insufficient).
- **Compatibility / impact:** Additive, explicit configuration opt-in only.
  Existing standard-profile configurations and behavior are unchanged. No
  credential format/storage, public API, or Core contract changes. The
  profile-scoped declaration follows the existing exact-scope capability model.
  The opt-in profile literal is additive; the existing standard literal/default
  and its upstream header set are unchanged. Protected fake-TLS tests establish
  local gateway preservation and zero-send rejection only; no live gateway tool
  round was run, and this does not close M5.1-043.
- **Bounds and limitations:** The scoped smoke flow uses a 30-second deadline,
  1 MiB response-byte ceiling, and 256 UTF-8 output-text-byte client observation
  cap. These bound local observation only; they do not guarantee an upstream
  generation/billing ceiling or server-side cap.
- **Verification:** Independently review source-pinned profile behavior and
  synchronized M5.1 binding/configuration/smoke text. Before implementation
  claims, verify exact header isolation, byte-preserving native request behavior,
  rejection of unsupported required semantics before send, and bounded
  incremental streaming. The direct custom artifact is limited to the exact
  model/profile and observed Lark custom shape. Offline protected-gateway
  evidence is separate and is not live gateway/provider evidence. Neither
  establishes other grammars, hosted/parallel tools, general entitlement, or
  M5.1-043 completion.

### DEC-012 — Conditional M5.1 Codex Implementation Acceptance with Deferred Live Gates

- **Status:** Accepted by explicit user approval on 2026-10-10 for conditional
  acceptance of M5.1 implementation/audit evidence while two live gates remain
  open. This is not M5.1 milestone completion or live-gate passage.
- **Context:** M5.1-043's audit is ready to proceed with its dependencies DONE.
  M5.1-049's read-only preflight verified the selected gateway credential is
  still fresh; its production one-minute freshness margin does not make refresh
  due until 2026-10-15 21:04:55.103 UTC. Forcing expiry or refreshing early is
  prohibited. Existing implementation and deterministic tests cover refresh
  rotation, coalescing, cancellation, persistence, quarantine/restart recovery,
  affinity, streaming and settlement, while actual provider refresh remains
  unobserved.
- **Decision:** Conditionally accept the implementation and deterministic-test
  evidence audited by M5.1-043 without representing M5.1 as DONE or passing its
  live gates. Retain (1) live OAuth refresh as M5.1-049, BLOCKED until its
  natural eligibility time, and (2) scoped two-turn encrypted-reasoning replay
  as M5.1-051, DRAFT and unrun. Both are separately tracked follow-ups; neither
  is waived or authorized for execution by this decision. M5.1-049 requires the
  due time, fresh read-only preflight, explicit authorization, and independent
  review PASS of the then-current diff. M5.1-051 requires its own READY status,
  assignment, explicit live authorization and independent review. These scoped
  gates do not require or claim comprehensive live coverage of all
  reasoning/history forms. Do not claim all M5.1 live acceptance or broader
  Codex compatibility.
- **Alternatives rejected:** Force expiration, revoke/rotate credentials, or
  invoke refresh before eligibility; or represent the milestone as DONE while
  either live gate remains open.
- **Compatibility / impact:** Documentation/governance exception only. No v1
  contract, implementation behavior, capability declaration, or follow-up
  acceptance criteria change. This is not the M4 DEC-008 deferral and grants no
  provider call authorization.
- **Verification:** Audit task evidence and its exact claim boundaries; verify
  M5.1-049 stays BLOCKED with a consistent natural due time, M5.1-051 stays
  DRAFT and unrun, both are reflected as open in M5.1-043/current compatibility
  claims, and links/dependencies remain valid. No live request is part of this
  decision.

## Questions Before Implementation

| Question | When to Decide | How to Validate |
| --- | --- | --- |
| Which upstream and client provide the first baseline? | M0 | Choose a Responses-native endpoint and reproducible tool scenario |
| What is the reproducible provenance and code-reuse license of the inspected sources? | Before code migration | Resolve repository identity and licenses for the local revisions in the [source maps](../implementation/REFERENCES.md#migration-from-9router) |
| Which Responses features are included in the first public compatibility claim? | M1 | Publish endpoint/feature matrix, including unsupported stateful operations |
| How do public model IDs map to upstream IDs? | M2 | Native identity mapping; rewrite only as explicit connector mode |
| How to extract requirements without full payload parsing? | M2 | Route policy plus minimal northbound extractor; unknown fields preserved |
| What does output budget look like with unknown usage? | M3 | Choose rejection/conservative policy and validate concurrency |
| How is account affinity preserved for response IDs? | Before multi-account stateful routing | Validate scope IDs and lifetime without rewriting native response |
| Which reasoning capabilities can be transferred without losing semantics? | M4 | Real fixtures and explicit negative cases |
| Is a Responses → Chat translator needed in the first local connector? | M5.2 | Validate capabilities of specific Ollama/vLLM versions |
| What exactly does the ACP connector execute? | Before ACP implementation | Choose agent process, transport, and tool loop owner |
| How does an external connector call host runtime services? | M6 | Prototype scoped bidirectional lifecycle and cancellation |

## Format of Future ADRs

Every semantic internal-contract change requires a short ADR here: context, decision, alternatives, consequences, compatibility/migration impact, and verification criteria. Update CONTRACT and affected specifications with the decision. Other substantial architectural tradeoffs use the same format; routine editorial corrections do not need an ADR.

This registry and its compact ADRs remain the single decision entry point. They distinguish accepted constraints from implementation choices that still need validation.
