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
| D10 | Accepted | gRPC over Unix socket for external Connector IPC | DEC-014 selects the bidirectional typed transport after matched M6 spikes; production binding remains for M6-006 |
| D11 | Accepted | Model rewrite is not a native passthrough | Requires body modification to be explicit |
| D12 | Proposed | CLI for initial admin operations | Enables account/key management without UI milestone |
| D13 | Accepted | Client Protocol Adapters own client request parsing, protocol validation, envelope creation, and client response formatting | Supports multiple northbound protocols without protocol structures or conversions in Core; provider-specific translation remains in connectors |
| D14 | Accepted | Responses is primary for agents; Chat Completions is a compatibility protocol; both are first-class northbound interfaces | Adds client compatibility through adapters while preserving the opaque transport envelope and connector model |
| D15 | Accepted | CONTRACT.md owns the v1 Adapter–Core Runtime–Connector boundary; semantic changes require ADR | Prevents per-provider API growth and undocumented compatibility breaks; see DEC-004 |
| D16 | Proposed | M3 protected startup loads its master key from a permission-checked external file | Keeps key material out of SQLite and configuration/environment surfaces; see DEC-005 |
| D18 | Accepted | Defer official Anthropic verification as an M4 completion prerequisite only after offline gates pass; retain it before production release | Human governance accepted the narrow deferral on 2026-10-04 and authorized the documentation batch; separates offline implementation quality from official-provider entitlement; see DEC-008 |
| D19 | Accepted | External Connector IPC uses a host-owned epoch credit broker and a dedicated priority control lane | Enforces aggregate queue ceilings across independent data sockets; see DEC-017 |
| D20 | Accepted | Original M7 offline synthetic candidate authorization for M7-009–013 | Accountable user resolution preserved M7-005 evidence and live gates; see DEC-018 |
| D21 | Accepted | Manual OAuth callback contract and narrowly bounded offline scope extension | Adds transient callback input; authorizes only M7-016, M7-010, and M7-013 offline work; see DEC-019 |

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

### DEC-013 — Conditional M5.2 Handoff with Deferred vLLM Live Gate

- **Status:** Accepted by explicit user approval on 2026-10-13 for a conditional
  M5.2 handoff based on the completed offline slice and reviewed, exact-scope
  Ollama and llama.cpp live evidence. This does not mark M5.2 DONE.
- **Context:** M5.2-024 verified paired direct/gateway behavior on official
  Ollama v0.40.3 with `llama3.2:1b`; M5.2-026 verified it on official
  llama.cpp v0.6.0 with a 1.2358B Q8_0 Llama-3.2-1B-Instruct model. M5.2-025
  remains BLOCKED because the required vLLM/GPU environment is unavailable and
  execution is not authorized. The existing at-most-3B model bound remains in
  force for future vLLM work; the card's Qwen3-8B candidate is not authorization
  to exceed it.
- **Decision:** Accept the verified offline M5.2 implementation and the narrowly
  evidenced Ollama and llama.cpp observations for handoff. Retain M5.2-025 as
  BLOCKED; do not claim vLLM evidence, all-runtime acceptance, or M5.2 DONE.
  Before any vLLM run, resolve the candidate against the at-most-3B bound and
  obtain separately provisioned prerequisites, a READY/assigned gate, and
  explicit operator authorization. No capability is promoted by this decision;
  unexercised model/runtime features remain **Unknown**. M4 live gates and both
  DEC-012 M5.1 follow-ups remain open and unchanged.
- **Alternatives rejected:** Mark M5.2 DONE while vLLM is unverified; waive the
  vLLM gate or model-size bound; or infer capability support from offline
  fixtures or the two other runtimes' observations.
- **Compatibility / impact:** Documentation and milestone-governance decision
  only. No contract, runtime behavior, capability declaration, task acceptance
  criterion, or live-execution authorization changes.
- **Verification:** Reconcile M5.2-024/025/026 status and evidence, keep all
  capability declarations unchanged, verify the at-most-3B vLLM limit and
  authorization boundary, retain older live gates, and check links/statuses.
  No live request is part of this decision.

### DEC-014 — Select gRPC over Unix Socket for External Connector IPC

- **Status:** Accepted as the technical transport selection under M6-005; the
  production wire/process binding is not yet specified or implemented.
- **Context:** M6-002 fixed one 16 MiB opaque streaming workload, callback
  behavior, concurrency, cancellation and queue/frame budgets. M6-003 and
  M6-004 ran disposable framed-stdio and gRPC/Unix-socket child prototypes on
  that workload, with ten measured repetitions after warmup on the same Linux
  x86_64 host (Go 1.27.1, 11th Gen Intel Core i5-1135G7, 8 logical CPUs,
  32,458,596 KiB RAM). Full raw measurements, commands, limitations and
  reviewer records remain in the [spike cards](../implementation/tasks/M6-003.md#completion-evidence)
  and [M6-004](../implementation/tasks/M6-004.md#completion-evidence).
- **Decision:** Use gRPC bidirectional streaming over a private Unix-domain
  socket for the first external Connector runtime. It naturally supplies
  typed, generated messages and duplex RPC needed for invocation-scoped reverse
  credential/HTTP/logging services while one execution stream is active. Keep
  framing, protobuf/gRPC code and dependencies outside `internal/core`; Core
  continues to speak only the existing language-neutral v1 component contract.
  M6-006 defines the actual binding and version handshake before any production
  implementation.
- **Evidence and tradeoff:** Both prototypes passed opaque-byte parity, early
  delivery, reverse credential and HTTP callback parity, joint cancellation,
  bounded queues, saturation and child cleanup within the common limits. The
  measured stdio median was 11.069 ms first Body, 49.679 ms completion and
  0.716 ms reap, at 315.05–324.57 MiB/s; gRPC medians were 19.902 ms first
  Body, 284.234 ms completion, 2.053 ms reap and 56.53 MiB/s. Peak observed
  retained queue was 82,970 B/2 frames for stdio and 82,986 B/2 frames for
  gRPC; both remained well below the shared bounds. gRPC's additional latency,
  CPU, dependencies and generator/tooling are real costs. Select it because
  generated typed duplex RPC and transport-managed cancellation/versionable
  messages reduce the bespoke protocol machinery and semantic drift risk at
  this boundary; the observed 16 MiB completion time remains within the
  scenario's 10-second repetition deadline. These are one-host prototype
  measurements, not production SLO or cross-platform evidence.
- **Alternative rejected:** Framed stdio is simpler to launch and materially
  faster/lighter in this measurement, but requires a project-owned framing,
  multiplexing, reverse-call, control-progress and compatibility protocol on
  stdin/stdout. Retain the spike and its raw evidence as a frozen comparison;
  do not maintain two production transports or infer stdio production support.
- **Compatibility and migration:** No CONTRACT, API, `StreamFrame`, retry,
  capability, credential authority, or lifecycle semantic change. Keep
  component API major/minor, IPC wire/schema version, protocol identifier, and
  implementation version distinct; M6-006 must define explicit incompatibility
  rejection before Init/Execute and additive evolution rules. Initial support
  is Linux with Unix-domain sockets only; other OSes and socket security
  behavior remain unverified. Migration is limited to the disposable spikes:
  downstream production work starts from the M6-006 binding, not either spike
  implementation. Pin runtime/module and protoc/plugin versions, commit
  reproducibly generated code with its exact regeneration command, and keep
  code-generation tools/dependencies out of Core; production pins and supported
  Go/tooling matrix are fixed with the binding/build task.
- **Verification:** Reproduce the card's stdio, gRPC and common-control race
  commands; compare all shared workload/budget rows and failure limits; review
  this record against CONTRACT/M2-BINDING and synchronized STACK/M6-EXTERNAL.
  M6-005 reviewer must accept the selection and M6-006 scope before binding
  work is promoted. No real provider or live service call is relevant.

### DEC-015 — Convey the Admitted Route Body Bound to Usage Estimation

- **Status:** Accepted by user authorization on 2026-10-14.
- **Context:** External `EstimateUsage` receives opaque request bytes separately
  from its fragmented typed query. The child needs the runtime's already
  admitted body ceiling to reassemble those bytes without inventing an IPC
  limit or trusting a child-selected value.
- **Decision:** Add runtime-owned `MaxBodyBytes int64` to `core.UsageQuery`.
  Core populates it from the selected route's effective admitted body limit;
  the protobuf binding carries it as additive field 6. The host and child both
  enforce the bound before passing the complete opaque body to the Connector.
  This changes no payload semantics, accounting policy, or provider behavior.
- **Alternative rejected:** Infer a maximum from the frame size or use a fixed
  IPC cap; either would reject bodies Core admitted or make transport ceilings
  masquerade as semantic limits.
- **Compatibility/migration:** This is an approved internal contract extension
  and protobuf wire addition. Existing components and operation behavior remain
  valid; the new external RPC is available only when both host and child use
  this binding. Protobuf field 6 is additive and does not change the IPC major.
- **Verification:** Test exact-boundary body preservation and over-bound
  rejection at host/child assembly; verify Core forwards the selected route
  value and protobuf field 6 round-trips. No live provider call is relevant.

### DEC-016 — Convey the Admitted Route Body Bound to Execute

- **Status:** Accepted by user authorization on 2026-10-14.
- **Context:** Execute sends opaque request-body chunks separately from its
  typed start record. The child must reassemble only within Core's selected
  route admission bound; the transport chunk/frame bounds are not that semantic
  limit.
- **Decision:** Add runtime-owned `MaxBodyBytes int64` to `core.AttemptScope`.
  `Dispatcher.executeAttempt` populates it from the selected route's effective
  body cap and normalizes absent or nonpositive caps to `math.MaxInt64` (the
  unbounded sentinel). The external Execute binding carries it as additive
  `ExecuteStart.max_body_bytes` field 3. Host and child reject request bodies
  over the conveyed value before invoking the child Connector. This does not
  change payload bytes, selection, retries, accounting, or provider behavior.
- **Alternative rejected:** Infer the aggregate cap from transport frame size
  or invent a fixed IPC payload limit; either would conflict with Core route
  admission.
- **Compatibility/migration:** This approved internal contract and protobuf
  extension requires host and child to use the updated binding together.
  `ExecuteStart` field 3 is additive and does not change the IPC major. No
  production child composition is added by this decision; that remains M6-024.
- **Verification:** Test route-bound and unbounded `AttemptScope` population,
  exact-boundary transport, and over-bound rejection at both host and child
  assembly. No live provider call is relevant.

### DEC-017 — Enforce Aggregate IPC Backpressure with a Host-Owned Epoch Broker

- **Status:** Accepted by the M6-020 planner resolution; IPC-only semantic extension, not a Core API change.
- **Context:** Execute and reverse HTTP use independent gRPC connections. HTTP/2 flow-control windows are peer-configurable and cannot enforce shared per-invocation/process limits across both sockets; an independent real-transport probe sent 257 x 64 KiB frames without receiver `Recv()` despite an 84 KiB server initial window.
- **Decision:** The host owns an epoch-bound `CreditBroker` with 64 frames / 4 MiB per invocation and 256 frames / 16 MiB per process. Every streamed data envelope (Execute and HTTPDo, either direction; BodyChunk or RecordFragment) carries a monotonic sequence. A producer must reserve one frame and its application bytes before sending; host-issued reservations are authoritative and the receiver returns credit only after delivery/assembly consumption. Identity binds epoch, invocation, operation, call, direction and sequence. Reject stale, replayed, missing, oversized or unreserved data. Use separate control RPC connections for broker Reserve/Ack and prioritized Cancel/Close, with a combined 64-message / 256 KiB cap and bounded/coalesced ACKs. Cancellation revokes reservations without waiting for data credits. gRPC windows alone are not an enforcement mechanism.
- **Alternatives rejected:** Tune HTTP/2 initial windows (peer-controlled, connection-local); maintain independent per-socket/per-stream budgets (cannot enforce aggregate cross-socket limits); buffer complete responses (breaks incremental delivery); or change Core/provider APIs (unnecessary).
- **Compatibility / impact:** Add `FlowControl` RPCs and frame sequence fields to the generated IPC schema; require IPC minor >=1 before Init/Execute. IPC major remains 1 because the addition is minor-version gated; old children are rejected rather than used without broker semantics. No Core contract, provider API, or native child composition change; child helper integration is defined here, production root remains M6-024.
- **Verification:** Real gRPC tests must prove cross-socket shared limits with large peer windows, tiny-frame count limits, reserve-before-send, partial-read ACK release, stale/replay rejection, control progress under saturation and reservation cleanup on cancellation. Synchronize this ADR with M6-BINDING and M6-EXTERNAL; regenerate protobuf bindings using pinned tools.

### DEC-018 — Authorize Bounded Offline M7 Candidate Work

- **Status:** Accepted by the accountable user direction in this task assignment on 2026-10-16; limited to the scope below.
- **Context:** M7-005 recorded an evidence-bound NO-GO for implementation on the available evidence. It found no established source/reuse authorization or backend protocol, kept all assessed capabilities unknown, and required separately accountable authorization before reconsideration. M7-007's offline binding and M7-008's synthetic fixtures were completed before this explicit authorization; their prior completion is historical work, not evidence that authorization existed at the time.
- **Decision:** Preserve M7-005's factual NO-GO findings and its unmet live/evidence gates. The user authorizes offline-only synthetic implementation and testing of the already registered bounded candidates M7-009 through M7-013, with no M7 milestone expansion. Each candidate retains its own task scope, dependencies, readiness and review. This is not legal advice, a determination of rights, permission to use Claude services, or a finding of live feasibility.
- **Prohibitions:** No live provider/network calls, login, real credential handling or persistence, traffic capture, binary reverse engineering, or legal determination. Capabilities remain `Unknown` (including OAuth); synthetic results cannot promote them. The connector remains non-routable and default-gated; no production composition or support claim is authorized. Separate live authorization, evidence and review gates remain mandatory before any live work or routing decision.
- **Alternatives rejected:** Treat offline synthetic tests as provider evidence, reinterpret M7-005 as technically impossible/permanently closed, or infer prior authorization for M7-007/M7-008. Each would overstate evidence or authority.
- **Compatibility / impact:** Documentation/governance only. No v1 contract, runtime, connector, capability, routing, milestone, or production behavior changes. This bounded authorization resolves the M7-005 accountable-authority revisit condition only for the listed offline candidates.
- **Verification:** Audit synchronized ROADMAP, TASKS, CURRENT and this ADR; confirm historical sequencing is explicit, live/credential/routability/capability prohibitions are consistent, and relative links/anchors and `git diff --check` pass. No runtime or live-provider check is authorized or required.

### DEC-019 — Accept Manual Pasted OAuth Callback Input

- **Status:** Accepted by the accountable user direction on 2026-10-16. M7-015 itself is documentation-only; this decision also grants the narrowly bounded offline implementation authorization below.
- **Context:** The existing `AuthUserAction` requires a device code and positive poll interval, while a browser authorization-code/PKCE flow has neither. Its redirect returns a code and state, but v1 `AuthRequest.State` is reserved for runtime-loaded opaque continuation bytes. The inspected 9Router flow demonstrates a manual pasted-callback interaction; it is a design reference, not evidence of provider compatibility or authority.
- **Decision:** Permit browser-redirect `AuthUserAction` with an HTTPS `VerificationURI`, empty `UserCode`, and zero `PollInterval`; device-code actions retain non-empty code and positive interval. Add `CallbackInput string` to `AuthRequest` and the runtime continuation entry point. It is supplied separately by the client only for `continue`, is 1–4096 printable ASCII bytes, is bounded and never logged, persisted, or treated as opaque state. Empty input remains valid for other continuation flows; non-empty input on `start`/`refresh` is rejected. The Connector alone parses callback input. The configured redirect URI must be absolute, have a non-empty authority, and contain no query, fragment, or userinfo. Parse pasted callback input strictly and compare its raw scheme, raw authority, and escaped path to that configured URI byte-for-byte, without normalization; reject userinfo, fragment, malformed authority/escaping, or any mismatch. Strictly decode its query and require exactly one non-empty case-sensitive `code` and exactly one non-empty case-sensitive `state`; reject duplicate keys, malformed encoding, OAuth error parameters, and every unexpected parameter. Compare the state value after exactly one strict query-decoding pass, byte-for-byte with expected state in protected continuation state before exchanging; client input never supplies or replaces expected state. Use the exact configured redirect URI in the token exchange. Invalid callbacks fail closed without reflecting or logging input, code, or provider error details.
- **Replay and disclosure:** Reject invalid action/bounds before claiming a continuation. After a validly bounded continuation is claimed, the existing durable one-invocation claim (DEC-007) is the one-time-use guard: each invocation consumes/advances its claim exactly once, including Connector-rejected callback and ambiguous outcomes; never retry or replay an attempted exchange. Reject stale/consumed sessions before Connector work. Keep verifier, expected state, credentials/tokens and raw callback out of presentation and logs. The authorization URL necessarily carries the public PKCE challenge and protocol state required by the browser flow; these appear only as protocol parameters in `VerificationURI`, never as separate presentation values. Never expose the verifier or credentials in `UserAction`, and do not put callback data in presentation fields. This narrowly clarifies DEC-009's presentation rule for browser redirects; device-code presentation rules do not change.
- **Bounded authorization:** The user's 2026-10-16 direction adopting the manual callback flow extends offline-only synthetic work to exactly M7-016 (minimal generic Core/coordinator/admin/IPC plumbing), M7-010 (offline connector OAuth/PKCE state machine), and M7-013 (offline conformance and non-routable composition audit), within their individual cards. This is a new bounded authorization beyond DEC-018's original registered M7-009–013 scope; DEC-018's historical scope and findings remain unchanged. M7-015 remains a documentation-only task. No other task or milestone work is authorized by this extension.
- **Prohibitions:** No live/provider calls or external network traffic, login, real credentials or their handling/persistence, capability promotion beyond `Unknown`, routability/production routes, or production support claims. Use only synthetic fixtures and controllable offline fakes; local loopback fake-server tests do not authorize external traffic. The connector remains non-routable/default-gated. No live authorization or entitlement is implied.
- **Alternatives rejected:** Overload opaque `AuthRequest.State`, add a provider-specific callback endpoint, or expose code/state in `AuthUserAction`; each conflates client input, provider format, or protected continuation data. Continue to require a device code for all auth actions would make browser redirect flows unrepresentable.
- **Compatibility / impact:** Additive optional `AuthRequest.CallbackInput` and additive runtime continuation argument; no new operation, stored field, schema migration, provider branch, or change to opaque state. Existing callers pass empty input and existing device-code flows retain their current action semantics. Browser-flow callers must send the callback only with explicit continuation. The v1 contract and M2/M6 bindings are amended at specification level; generic Go Core/coordinator/admin and protobuf/IPC implementation is authorized only in M7-016 and remains pending. Existing v1 implementations cannot receive callback input or perform this flow until upgraded. No auth capability is promoted. DEC-018 remains unchanged and controls its original candidate scope; this decision's bounded extension carries the same offline-only boundary for the three named cards.
- **Verification:** Paper-walk valid callback, wrong/missing/duplicate state/code, OAuth error, unexpected parameters, malformed percent escapes, raw redirect mismatch (including case/escaping differences), oversized/non-ASCII input, replay/stale claim, cancellation and ambiguous exchange. Audit that only the runtime owns opaque session state, the Connector validates exact redirect/state before exchange, all attempted continuations are single-use, and credentials/verifier/raw callback are neither presented nor logged. Synchronize CONTRACT, M2-BINDING, M3-AUTH, M6-BINDING, M6-HANDOFF and M7-BINDING; run documentation/link checks for M7-015. M7-016/010/013 perform only the offline deterministic checks in their own cards; no live or provider check is authorized.

### DEC-020 — Accept M8 Offline Integration and Defer Live Enablement

- **Status:** Accepted by the user's 2026-10-17 instruction to integrate M8 without an available Claude account.
- **Decision:** Integrate M8-001–007's synthetic execution, auth, and protected fixture composition after integration review and deterministic CI. Accept the offline slice conditionally; do not declare the full live milestone DONE. This explicitly accepts the implemented offline extension beyond DEC-018/019 without rewriting their historical authority or M7's findings.
- **Boundary:** Keep fixed `.invalid` endpoints, synthetic beta, absent default admin exchange transport, YAML rejection, ordinary Execute transport rejection, and Unknown provider capabilities. No real credentials, provider traffic, production route enabling, or support promotion is part of this integration.
- **Follow-up:** M8-008 remains BLOCKED on an account plus a reviewed current provider binding and finite live procedure. M8-009 owns subsequent reviewed production enablement. Deferred live gates do not block independent milestones.
- **Compatibility:** No Core API or schema change; existing Responses/Chat and Codex paths remain supported at their recorded evidence ceilings.
- **Verification:** Reconcile against current main; test public Execute, OAuth, protected composition and non-enablement, plus whole-tree hosted format/vet/unit/race/build and M6 regressions. See [handoff](../implementation/M8-HANDOFF.md).

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
