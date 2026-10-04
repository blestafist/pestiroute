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

## Immediate focus: M4 offline gate verified; PR handoff to M5.1 research

M4 implements explicit Responses ↔ Anthropic Messages translation under the
accepted [DEC-008/D18](../project/DECISIONS.md#dec-008--accepted-narrow-deferral-of-official-anthropic-verification)
offline milestone gate. [M4-038](tasks/M4-038.md) audited the earlier committed
baseline; [M4-045](tasks/M4-045.md) reviews the assembled branch and fixes mixed
text/tool output, tool argument validation, truncation, SSE metadata and missing
function results. Core remains protocol-neutral and native bodies remain opaque. The registry has
45 M4 cards: 43 DONE, 2 BLOCKED, and no pending offline implementation.

The compatible-endpoint harness is committed in `9035e18`, including all five
files previously left in the local worktree. Its deterministic TLS/HTTP2 tests
cover client-owned tool rounds, replay, cancellation and durable settlement.
Those tests do not satisfy either live gate:

- [M4-036](tasks/M4-036.md): official Anthropic capture remains BLOCKED and is
  mandatory before production release under DEC-008/D18.
- [M4-041](tasks/M4-041.md): compatible-endpoint tool cycles remain BLOCKED.
  The prior live batch stopped on its first direct request with HTTP 200,
  `unexpected_tool_name`, one dispatch and zero retries. Later rounds and
  cancellation were unrun. The task card preserves the historical report;
  its local temporary failure artifact is not included in the repository.

Production targets direct Anthropic with the pinned backend model. The
`cc/claude-sonnet-5-5` compatible model and endpoint override remain test-only.
Live parallel tools, reasoning, OAuth and remote compute cancellation have no
support claim. No live inference is part of the final branch review.

After the PR, the next planning boundary is M5.1 Codex translation research and
scoping; no M5 implementation is included. Reuse registry, scoped services,
authorization, admission, accounting and dispatch. Request translation, provider
SSE/errors and token normalization remain Connector-owned. Keep the existing
Context7, gopls, shell and Git tool configuration; see [Tooling](TOOLING.md#m4-tooling-gate).

## Available checks

[M4-045](tasks/M4-045.md) records the final review on `9035e18` plus the
review fixes. `./scripts/check.sh` passes formatting, vet, unit/race tests and
offline build. The uncached Anthropic/gateway/conformance race suites, ten
repeated new regressions, pure-Go tests/build, compatible TLS/HTTP2 harness,
artifact validator, shell syntax, and Markdown link/dependency audit also pass.
Hosted PR CI is reported separately in GitHub.

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
  live OAuth and IPC remain M4/M5/M6.
- Historical live evidence remains OpenCode 2.0.6 / OpenAI Responses /
  `gpt-5.4-mini`, with parallel tools unknown. M3 adds no live inference claim.
  Remote provider compute cancellation remains unverified.
- [References](REFERENCES.md#migration-from-9router) retains source maps; upstream
  provenance and code-reuse licenses must be resolved before copying source.
  Other unresolved design choices remain in [DECISIONS](../project/DECISIONS.md).
