# Current State

## Verified baseline

M0–M4 implementation/offline gates are accepted. The normative boundary remains [CONTRACT](CONTRACT.md); [M2-BINDING](M2-BINDING.md) preserves opaque payloads, scoped services and checked Head/Body/Complete/EOF. Protected execution includes SQLite virtual-key/account/policy storage, external-master-key AES-GCM secrets, admission/reservations/usage, crash recovery, account isolation and bounded delivery-safe fallback. Core has no production imports of concrete Connectors or Adapters.

M4 PR [#11](https://github.com/blestafist/pestiroute/pull/11) merged through 6002cadd226a11495cf659c7b0e7a635d5e41ba8. [M4-036](tasks/M4-036.md) official Anthropic and [M4-041](tasks/M4-041.md) compatible-endpoint tools live gates remain BLOCKED; DEC-008's production constraint remains in force.

## Immediate focus: M5.2 baseline and source gates

M5.1 source e1aca937948a8bd225367cc95c19942b3771cb93 is conditionally accepted by [M5.1-043](tasks/M5.1-043.md) and [DEC-012](../project/DECISIONS.md#dec-012--conditional-m51-codex-implementation-acceptance-with-deferred-live-gates). Portable fixture fix 4e1235257c2e955bbbd060186b45a55445a6686c replaces the synthetic gateway fixture's hard-coded /tmp/opencode parent with testing temporary storage. [PR #12](https://github.com/blestafist/pestiroute/pull/12) merged through 9d640cf1968ee93e8187a38648dae3e06a28c2b1, preserving both parents and milestone history. Final head 55c50aa8835e7394c212de6fc6ae8f3fbd5979a2 also contains deterministic terminal-signal test synchronization and scoped SSE fixture whitespace attributes. [Hosted PR CI](https://github.com/blestafist/pestiroute/actions/runs/38089962350) and [push CI](https://github.com/blestafist/pestiroute/actions/runs/38089959147) passed full pinned Go 1.27.1 shared checks on that exact head. The planning branch dev-m5.2 preserves planning commit 0075bd0 and merges this main revision before implementation.

[M5.2-LOCAL](M5.2-LOCAL.md) defines the bounded local-runtime plan. [M5.2-001](tasks/M5.2-001.md) is READY; all other new cards remain DRAFT. Recheck clean-checkout baseline and current compatible code, then pin Ollama/vLLM/llama.cpp separately. Prefer compatible Responses reuse; 006 decides the smallest implementation path and rechecks 008–013 before execution. No M5.2 functionality or installed runtime/model/hardware is claimed by planning. Use one card per Worker/Reviewer session and existing /next N; preserve Context7/gopls config, with no new MCP/plugin required.

## Codex support and open live gates

The protected Codex Connector includes device OAuth, atomic credential/expiry persistence, serialized refresh/quarantine/recovery, HTTP/SSE transport, account isolation, client-owned history, conservative retry and durable accounting. [M5.1-COMPATIBILITY](M5.1-COMPATIBILITY.md) owns evidence-scoped support. Live proof is limited to the exercised Lite gpt-6-luna plain-text/function/custom-Lark forms, M5.1-040 device-auth/local decrypt and M5.1-050 official CLI 0.162.1 read-only comparison against the historical gateway baseline. It is not general entitlement, standard-profile tools, parallel-true support or comprehensive reasoning coverage.

[M5.1-049](tasks/M5.1-049.md) stays BLOCKED until the natural threshold 2026-10-15 21:04:55.103 UTC and its own current preflight, explicit authorization and independent review. Real OAuth refresh has not been verified. [M5.1-051](tasks/M5.1-051.md) stays DRAFT/unrun; live encrypted-reasoning continuation needs its own READY assignment/authorization/review. Both remain open and neither is an M5.2 research prerequisite. The previously recorded 177/200 remaining request budget is a ceiling, not authorization. Preserve the operator's isolated credential store; no credentials/raw payloads belong in repository evidence.

M5.1-043's local checks included operator working-tree helpers not present in clean checkout; its card preserves that limit. Claude Code, Gemini CLI, ACP, WebSockets and IPC remain future work. Lite implementation is present under DEC-011; do not repeat the obsolete original planning exclusion as current state.

## Checks and operations

Use pinned Go 1.27.1: ./scripts/check.sh performs formatting, vet, unit/race tests and offline build. Affected uncached race and pure-Go checks are listed in each card. M5.1-043 records the earlier local review results; the exact-head hosted CI and merge evidence above are separate. The terminal-signal race regression also passed 100 local repetitions and related cancellation/deadline cases passed 20 repetitions in an isolated Core checkout with Go 1.27.2; this is narrower than the full hosted repository check. For this documentation-only M5.2 plan, verify links/anchors, registry dependency acyclicity/statuses, diff whitespace and preservation of older live gates; no real backend call is part of planning.

[LOCAL-M3](LOCAL-M3.md), [LOCAL-M4](LOCAL-M4.md), and [LOCAL-M5.1](LOCAL-M5.1.md) retain existing protected operations; schema v6 must be migrated while stopped with a consistent SQLite backup and the matching external master key. Local runtime launch/model installation is separate from deterministic CI and remains unverified until M5.2-007 and the per-runtime smoke gates.
