# Current State

## Verified baseline

M0–M3 are complete. [M3-046](tasks/M3-046.md) records the final gate audit,
review fixes and M4 handoff on source `d36a91a7e0869c286bed27e6eb525af183ad029d`.
The closure commit updates documentation only. Local verification used pinned
Go 1.27.1 on Linux/amd64; hosted PR CI is tracked separately in GitHub.

[CONTRACT.md](CONTRACT.md) remains the accepted v1 semantic boundary.
[M2-BINDING.md](M2-BINDING.md) retains managed components, scoped invocation
services, explicit native identity routes and checked Head/Body/Complete/EOF.
Core has no production imports of concrete Adapters, Connectors or parsers.

Protected version-1 YAML composes SQLite-backed virtual keys/policies/accounts,
AES-256-GCM credentials with an external permission-checked master key, atomic
RPM/TPM admission, per-attempt reservations/usage, durable dispatch intent,
request closure and conservative crash recovery. Persistence failures withdraw
readiness and block new protected requests across routes. Opt-in ordered fallback
requires explicit safe delivery before client commit, fresh candidate eligibility
and bounded attempts/deadline; it admits RPM once and charges each attempt.
Stateful/unknown-affinity requests require a configured singleton target.

Schema v6 adds durable claims for interactive auth continuation to the v5 auth
sessions. Protected startup recovers refresh markers to uncertain and consumes
ambiguous claimed continuations before listen. Account disable atomically
invalidates auth state. Scripted start/continue/refresh, revision CAS and
cross-process claims are verified; live provider auth remains M5.1.

## Immediate focus: M5.1-043 DRAFT; 23/200 upstream requests used, 177 remain

M4 PR [#11](https://github.com/blestafist/pestiroute/pull/11) is merged through
merge commit `6002cadd226a11495cf659c7b0e7a635d5e41ba8`; head
`a030ef0949711b7172f1e93947772cd59febc0b7` had successful hosted CI run 47.
M4 has 43 DONE cards and two retained BLOCKED live gates: official Anthropic
[M4-036](tasks/M4-036.md), mandatory before production under
[DEC-008/D18](../project/DECISIONS.md#dec-008--accepted-narrow-deferral-of-official-anthropic-verification),
and compatible-endpoint tools [M4-041](tasks/M4-041.md). The compatible harness
and final review fixes are committed; [M4-045](tasks/M4-045.md) retains evidence.

The new branch is `dev-m5.1`, based on that merge. The
[Codex plan](TASKS.md#m51--codex-subscription-connector) has 49 small cards:
[M5.1-001](tasks/M5.1-001.md) through [M5.1-042](tasks/M5.1-042.md), and [M5.1-044](tasks/M5.1-044.md) through [M5.1-048](tasks/M5.1-048.md) are DONE,
resolving the source/profile baseline, safe device-login presentation/continuation,
atomic OAuth credential/freshness binding, request dialect/affinity/retry policy under accepted
[DEC-009](../project/DECISIONS.md#dec-009--separate-safe-user-actions-from-opaque-auth-state)
and [DEC-010](../project/DECISIONS.md#dec-010--carry-credential-expiry-with-auth-results),
versioned synthetic OAuth test fixtures, versioned Responses stream/replay fixtures,
the scoped Codex component lifecycle and descriptor with conservative Unknown capabilities,
bounded token metadata / credential bundle codec with strict secret sanitation,
one device-authorization start exchange with fail-closed direct RoundTrip redirect prevention,
bounded device-authorization continuation polling with fixed-expiry preservation and authorized transition,
authorized authorization code exchange for credentials with DEC-010 expiry,
candidate-only selected-account token refresh exchange with token rotation/retention and DEC-010 expiry,
atomic OAuth credential and expiry persistence with single-revision CAS across Core and SQLite,
transient Core `AuthSession.UserAction` validation and safe admin CLI presentation,
default admin auth wiring for configured Codex accounts with redirect-refusing RoundTripper transport and account-scoped credentials,
generic credential freshness resolution before inference dispatch with locked margin rechecks and refresh-scoped credential access while preserving interactive reauthentication from quarantine,
concurrent refresh coalescing, cancellation safety, account isolation, independent-handle marker contention, and revision CAS quarantine under locked SQLite coordination,
auth uncertainty persistence failure, restart recovery with in-flight refresh quarantine to uncertain, claimed continuation consumption, and explicit reauthentication recovery,
Connector-private standard Responses profile request validation with native/translation stream/store invariants, 1 MiB body bounds, recursive duplicate JSON rejection, unsupported feature/continuation fail-closed checks, and byte-preserving fixture admission,
Connector-private request adaptation with byte-preserving native output, immutable inputs, injected stream/store/reasoning defaults, and raw unknown/tool/history fidelity,
trusted selected-account HTTP request header construction with OAuth credential/account binding, strict hop-by-hop/client-auth filtering, Connection nomination suppression, and fail-closed ASCII validation,
and invocation-scoped managed HTTP transport hardening with non-replayable POST, redirect/compression refusal, phase deadlines, 16 KiB header and 64 MiB response bounds, incremental <= 4 KiB Body chunks, cancellation/idle cleanup, and provisional completion,
and Connector-private inline SSE framing observation with exact-byte preservation, arbitrary split and CRLF/multiline tolerance, 1 MiB event and 4 KiB scalar limits, fail-closed malformed handling, and non-2xx rejection bypass,
and pre-head HTTP rejection classification with bounded error body forwarding and non-retryable status mapping, and Connector-private SSE terminal event observation mapping completed, failed, error, incomplete, truncated, and trailing-data cases to contract outcomes with verbatim byte preservation,
and Connector-private inclusive token usage extraction into normative UsageReport without subset double counting, zero versus absent preservation, invalid counter fallback to unknown, partial accounting on stream failure/interruption, exactly-once completion, and verbatim SSE body preservation,
and honest unknown usage estimation with reject versus conservative reservation enforcement, invalid/overflow budget pre-inference rejection, and zero provider sends, and protected Codex route composition with exact-scope streaming capability, proactive credential freshness before dispatch, usage accounting, invalid budget rejection, revoked/disabled fail-closed denial, and coexisting native and Anthropic routes (M5.1-027),
and protected real-socket Codex text streaming with early delta delivery before gated upstream completion, exact-byte SSE forwarding without whole-response buffering, truthful completed/incomplete/failed/error/EOF terminal outcomes, and exactly-once ledger and accounting settlement (M5.1-028),
and two client-owned function-tool rounds with exact ordered history passthrough, distinct item/call IDs, linked function results, named and standard string tool-choice validation, unknown message/call extension preservation, and fail-closed zero-send rejection of unsupported resource history, with production Codex capabilities truthfully retaining llm.tools Unknown (M5.1-029),
and explicit boolean parallel_tool_calls preservation and interleaved SSE deltas with distinct item and call IDs, ordered argument streaming, and linked tool results in subsequent rounds, with production Codex capabilities truthfully retaining llm.tools and llm.tools.parallel as Unknown (not usable in production; requests requiring them are rejected without provider sends) (M5.1-030),
and chronological full-history item replay with verbatim encrypted reasoning ciphertext, summary fragments, item IDs, message phases, opaque extensions, and initial instructions across rounds, with fail-closed zero-send rejection of unsupported previous responses, resources, and server compaction, while production Codex capabilities truthfully retain llm.reasoning as Unknown (M5.1-031),
and assembled two-account authorization credential and cache-header isolation proving spoofed client header stripping, virtual-key privacy, independent credential isolation across accounts, zero-send denials on out-of-policy/revoked/disabled requests, safe cache affinity without cross-account resource reuse, and secret redaction in responses and logs (M5.1-032),
and real client-socket disconnect upstream cancellation without wait, 48 MiB stalled-reader bounded transport backpressure with ordered intact resumption, graceful stream drain and forced drain deadline expiry cancellation, zero post-commit fallback, and unobservable remote-compute cancellation preserved as unknown (M5.1-033),
and delivery-safe retry and opaque replay affinity boundaries proving unknown-affinity classification for historical Responses reasoning with encrypted_content, pre-admission zero-send fail-closed rejection on multi-candidate routes, singleton target execution without candidate cycling, advancement only on explicit safe no-send, and zero fallback on HTTP 401/403/429/500, ambiguous connection drops, committed SSE failures, and client cancellation (M5.1-034),
and assembled real-socket Codex durable settlement proving exactly-once terminal accounting across success, incomplete, failed, error, and EOF streams, conservative reservations, crash-intent restart reconciliation without duplicate charges or free work, auth/storage failure fail-closed readiness withdrawal, and duplicate Complete frame idempotency (M5.1-035),
and offline Codex conformance and scoped capability matrix audit proving participation in shared opacity, incremental, cancellation, backpressure, and terminal fixtures, published M5.1-COMPATIBILITY.md separating local deterministic evidence from unverified live entitlement, truthful capability declarations with llm.streaming Supported and unproven capabilities Unknown, and honest Models scope enforcement (M5.1-036),
and synthetic local Codex operations guide and test daemon proving real CLI TLS loopback auth start/continue/refresh without secret leakage, same-DB YAML protected streaming, proactive refresh, quarantine restart and interactive recovery, account disablement, native/Anthropic coexistence, and populated WAL-safe SQLite backup/restore integrity (M5.1-037),
and bounded smoke capture and privacy validation proving paired direct/gateway inference and auth schema enforcement, strict secret/token/prompt redaction, operational bounds, and offline loopback self-testing across all scenarios without external provider calls (M5.1-038),
and exact live auth and paired inference procedure with explicit CPython client/profile/model selection, harmless function/parallel/reasoning probes, zero-dispatch rejection expectations, and bounded auth/refresh dry-runs with zero external calls (M5.1-039).
M5.1-040–042 are DONE; see their cards for the auth and paired plain-text evidence. M5.1-044's direct single-tool probe and roundtrip are recorded in [its card](tasks/M5.1-044.md); 184 authorized Lite requests remained after M5.1-047's two-request probe. M5.1-045–047 are DONE: native Lite `gpt-6-luna` function-kind and custom Lark grammar tool support, multi-call forwarding/replay, and offline protected fake-TLS roundtrips are verified; standard-profile tools and explicit parallel requests remain fail-closed Unknown with zero-send gates. [M5.1-047](tasks/M5.1-047.md) is DONE (direct two-request roundtrip passed HTTP 200/200; scoped native Lite custom Lark grammar in functions namespace admitted; offline mixed-history, incremental/cancellation, and zero-send coverage verified; reviewer PASS). Its card contains no gateway-custom live claim; M5.1-048 now records that separate evidence. M5.1-043 remains DRAFT.
M5.1-048 is DONE. Both native Lite (`gpt-6-luna`) protected gateway single-function (v4) and custom Lark grammar (v5) tool roundtrips are empirically verified: both completed HTTP 200/200, reported usage (324/26 and 391/24), snapshot/delta and normalized marker equality, 2 target dispatches, 2 network dials, and zero retries. Proof is durably mirrored in `docs/implementation/evidence/M5.1-048/` with zero secrets, tokens, or raw prompts. The objective for live gateway tools is complete.
M5.1-048 is DONE. Both native Lite (`gpt-6-luna`) protected gateway single-function (v4) and custom Lark grammar (v5) tool roundtrips are empirically verified: both completed HTTP 200/200, reported usage (324/26 and 391/24), snapshot/delta and normalized marker equality, 2 target dispatches, 2 network dials, and zero retries. Proof is durably mirrored in `docs/implementation/evidence/M5.1-048/` with zero secrets, tokens, or raw prompts. The objective for live gateway tools is complete; stop here. M5.1-043 formal milestone closure is not started; refresh is not forced, and independent direct login remains deferred. Cumulative live budget is 23/200 used (177 remain). Isolated gateway credential store `/tmp/opencode/pestiroute-auth.pQ8lCE` is preserved; zero raw credentials or parallel refreshes permitted.
The supplied [research](../references/CODEX_CONNECTOR_RESEARCH.md) is checked
in with trailing EOF whitespace normalized; [M5.1-CODEX](M5.1-CODEX.md) maps it to inspected runtime seams.

The scoped candidate is ChatGPT subscription OAuth device login plus Responses
HTTP/SSE, ordinary tools, full-history encrypted reasoning and existing
protected accounting. Auth user presentation is bound by DEC-009; M5.1-003
specifies credential expiry/bootstrap and proactive refresh. Core remains
provider-neutral. M5.1-040 records one operator-reported device login and locally
measured read-only persistence/decryption; duplicate direct login comparison is
operator-deferred, and refresh was not due/run. M5.1-041 separately verifies one
matched Lite plain-text direct/gateway inference pair; it does not establish
standard-profile live success or general account entitlement.
Claude Code, Gemini CLI, ACP, WebSockets and Lite implementation stay outside
this plan. No live auth/inference was performed in planning. The M4 deferral
does not apply to M5.1-040/041.

Keep the checked-in Context7/gopls configuration, shell/Git and Go checks;
[M5.1 tooling](TOOLING.md#m51-tooling-gate) requires no new MCP/plugin. This
inspection does not verify the developer's running MCP connections. Use one
card per worker/reviewer session and the existing `/next N` workflow.

## Available checks

[M4-045](tasks/M4-045.md) records the final review on `9035e18` plus the
review fixes. `./scripts/check.sh` passes formatting, vet, unit/race tests and
offline build. The uncached Anthropic/gateway/conformance race suites, ten
repeated new regressions, pure-Go tests/build, compatible TLS/HTTP2 harness,
artifact validator, shell syntax, and Markdown link/dependency audit also pass.
Hosted M4 PR CI run 47 passed on the exact merged PR head. Planning verification
checked 205 registry rows, acyclic dependencies, all 43 new template cards,
732 relative links/anchors across 69 documents, retained M4 blockers, and a
content-preserving research copy; `git diff --check` passed. No Go code changed or
runtime suite was rerun for this documentation-only plan.

[LOCAL-M4](LOCAL-M4.md) passes offline dual native/translation protected provisioning,
native loopback dispatch, local translation validation/rejections, rate limiting,
usage inspection, restart, and WAL-safe backup/restore integrity.
[LOCAL-M3](LOCAL-M3.md) passes protected provisioning, inference, limits, usage,
restart and consistent backup/restore. For an existing older M3 database: stop
the gateway, make a consistent backup and run `admin migrate` to v6 before
protected startup. Retain the matching external master key. Legacy/M2 JSON
remain loopback development compatibility modes; see [LOCAL-M2](LOCAL-M2.md).

## Evidence limits and open inputs

- Built-in Connectors include native Responses and protected Anthropic Messages
  translation. A translated instance has one account/model scope; protected YAML
  selects its SQLite credential explicitly with `credential_id`, while native
  settings retain `credential_env`. Inference uses scoped SQLite credentials.
- Native transport failures retain unknown delivery; safe fallback is proven
  with explicit deterministic fixtures, not a broader real-provider retry claim.
  Remaining translation gates/live compatibility, stateful cross-target affinity,
  broader live OAuth verification and IPC remain M4/M5/M6. M5.1 has only the
  narrowly scoped auth and Lite evidence summarized above; M5.1-043 review remains.
- Historical M1 live evidence remains OpenCode 2.0.6 / public OpenAI Responses /
  `gpt-5.4-mini`, with parallel tools unknown; it is distinct from ChatGPT
  subscription Codex. Remote provider compute cancellation remains unverified.
- [References](REFERENCES.md#migration-from-9router) retains source maps; upstream
  provenance and code-reuse licenses must be resolved before copying source.
  Other unresolved design choices remain in [DECISIONS](../project/DECISIONS.md).
