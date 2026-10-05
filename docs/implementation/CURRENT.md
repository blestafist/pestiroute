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

## Immediate focus: M5.1-032 closed

M4 PR [#11](https://github.com/blestafist/pestiroute/pull/11) is merged through
merge commit `6002cadd226a11495cf659c7b0e7a635d5e41ba8`; head
`a030ef0949711b7172f1e93947772cd59febc0b7` had successful hosted CI run 47.
M4 has 43 DONE cards and two retained BLOCKED live gates: official Anthropic
[M4-036](tasks/M4-036.md), mandatory before production under
[DEC-008/D18](../project/DECISIONS.md#dec-008--accepted-narrow-deferral-of-official-anthropic-verification),
and compatible-endpoint tools [M4-041](tasks/M4-041.md). The compatible harness
and final review fixes are committed; [M4-045](tasks/M4-045.md) retains evidence.

The new branch is `dev-m5.1`, based on that merge. The
[Codex plan](TASKS.md#m51--codex-subscription-connector) has 43 small cards:
[M5.1-001](tasks/M5.1-001.md), [M5.1-002](tasks/M5.1-002.md),
[M5.1-003](tasks/M5.1-003.md), [M5.1-004](tasks/M5.1-004.md),
[M5.1-005](tasks/M5.1-005.md), [M5.1-006](tasks/M5.1-006.md),
[M5.1-007](tasks/M5.1-007.md), [M5.1-008](tasks/M5.1-008.md),
[M5.1-009](tasks/M5.1-009.md), [M5.1-010](tasks/M5.1-010.md), [M5.1-011](tasks/M5.1-011.md), [M5.1-012](tasks/M5.1-012.md), [M5.1-013](tasks/M5.1-013.md), [M5.1-014](tasks/M5.1-014.md), [M5.1-015](tasks/M5.1-015.md), [M5.1-016](tasks/M5.1-016.md), [M5.1-017](tasks/M5.1-017.md), [M5.1-018](tasks/M5.1-018.md), [M5.1-019](tasks/M5.1-019.md), [M5.1-020](tasks/M5.1-020.md), [M5.1-021](tasks/M5.1-021.md), [M5.1-022](tasks/M5.1-022.md), [M5.1-023](tasks/M5.1-023.md), [M5.1-024](tasks/M5.1-024.md), [M5.1-025](tasks/M5.1-025.md), [M5.1-026](tasks/M5.1-026.md), [M5.1-027](tasks/M5.1-027.md), [M5.1-028](tasks/M5.1-028.md), [M5.1-029](tasks/M5.1-029.md), [M5.1-030](tasks/M5.1-030.md), [M5.1-031](tasks/M5.1-031.md), and [M5.1-032](tasks/M5.1-032.md) are DONE,
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
and assembled two-account authorization credential and cache-header isolation proving spoofed client header stripping, virtual-key privacy, independent credential isolation across accounts, zero-send denials on out-of-policy/revoked/disabled requests, safe cache affinity without cross-account resource reuse, and secret redaction in responses and logs (M5.1-032).
All 17 tasks (M5.1-001..032) are DONE and verified on local deterministic fixtures; 11 dependent cards remain DRAFT pending sequential promotion. Production Codex reports llm.tools, llm.tools.parallel and llm.reasoning Unknown: these capabilities are not usable in production. No live authentication, inference, or tool support is claimed.
The supplied [research](../references/CODEX_CONNECTOR_RESEARCH.md) is checked
in with trailing EOF whitespace normalized; [M5.1-CODEX](M5.1-CODEX.md) maps it to inspected runtime seams.

The scoped candidate is ChatGPT subscription OAuth device login plus Responses
HTTP/SSE, ordinary tools, full-history encrypted reasoning and existing
protected accounting. Auth user presentation is bound by DEC-009; M5.1-003
specifies credential expiry/bootstrap and proactive refresh. Core remains
provider-neutral. No live authentication or inference is claimed.
Claude Code, Gemini CLI, ACP, WebSockets and Lite implementation stay outside
this plan. No live auth/inference was performed in planning. M5.1-040/041 own
the separate live evidence gates; the M4 deferral does not apply to them.

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
  live OAuth and IPC remain M4/M5/M6. Codex is planned, not implemented.
- Historical live evidence remains OpenCode 2.0.6 / OpenAI Responses /
  `gpt-5.4-mini`, with parallel tools unknown. M3 adds no live inference claim.
  Remote provider compute cancellation remains unverified.
- [References](REFERENCES.md#migration-from-9router) retains source maps; upstream
  provenance and code-reuse licenses must be resolved before copying source.
  Other unresolved design choices remain in [DECISIONS](../project/DECISIONS.md).
