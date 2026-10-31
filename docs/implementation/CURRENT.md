# Current State

## Verified baseline

`main` includes M6 merge [PR #14](https://github.com/blestafist/pestiroute/pull/14), merge commit `1391227`, following M5.2 baseline `bea67fe`. M0–M4 deterministic implementation gates are accepted; protected native, Anthropic translation, Codex and local Responses paths are implemented. Core remains provider-agnostic under [CONTRACT](CONTRACT.md) and [M2 binding](M2-BINDING.md), with opaque bytes, scoped credentials/transport, SQLite accounting and delivery-safe fallback.

M5.1 and M5.2 handoffs are **conditionally accepted**, not fully DONE: [DEC-012/013](../project/DECISIONS.md) preserve unwaived live gates. Exact feature/evidence ceilings belong in [M4](M4-COMPATIBILITY.md), [Codex](M5.1-COMPATIBILITY.md) and [local-runtime](M5.2-COMPATIBILITY.md) matrices. Ollama and llama.cpp scoped live text gates completed; vLLM live evidence remains blocked. Tools/reasoning/parallel or compute cancellation are not promoted beyond their recorded scope. Historical completion/check evidence lives in [archived rows](archive/TASKS-DONE.md) and the stable task cards.

## Integrated offline baseline

M8 offline integration merged through [PR #19](https://github.com/blestafist/pestiroute/pull/19)
as `6891ed6`; [hosted integration CI](https://github.com/blestafist/pestiroute/actions/runs/39046595588)
passed for source `0e88570` (external/M6, format, vet, unit, race and build).

M7 research and M9 native Chat Completions (including M9-007 stream idle timeout
and mixed-protocol isolation) are on main. M8-001–007 are accepted for offline
integration under [DEC-020](../project/DECISIONS.md#dec-020--accept-m8-offline-integration-and-defer-live-enablement).
See [M8-HANDOFF](M8-HANDOFF.md): synthetic Execute, OAuth/manual callback,
encrypted credential lifecycle, tool history and protected fixture composition
are implemented. Claude production routing remains disabled; YAML is rejected,
endpoints are `.invalid`, default exchange transport is absent and capabilities
remain Unknown. M8 is not fully live-verified.

M8-008 is BLOCKED because no Claude account is available; M8-009 is DRAFT pending
provider evidence and a reviewed enablement decision. These gates are deferred,
not waived. Native Responses, native Chat, Anthropic translation, Codex,
local-runtime profiles and external processes retain their individual evidence
ceilings and existing tests. M9 branch reconciliation is merged in PR #18;
the earlier CURRENT claim that it awaited review was stale.

## Next independent lanes

| Milestone | Branch | First READY / active task | Scope / dependency |
| --- | --- | --- | --- |
| [M10](M10-ANTIGRAVITY.md) | `dev-m10-antigravity` | M10-028 BLOCKED | M10-029–038 DONE. M10-028 BLOCKED (budget 23/1000; precommit_invalid_content). M10-038 closed DONE offline. Zero live calls. |
| [M11](M11-ADMIN-API.md) | `dev-m11-admin-api` | M11-001 | Management contract → protected API; owns shared gateway composition |
| [M12](M12-WEBUI.md) | `dev-m12-webui` | M12-001 | UX and fixture UI now; real integration waits for M11-007 on main |
| [M13](M13-OPERATIONS.md) | `dev-m13-operations` | M13-001 | Packaging and backup/restore; no provider or UI prerequisite for initial slice |

Each has a dedicated worktree and `/next 10 M10` (or M11/M12/M13) filter.
See [PARALLEL](PARALLEL.md) before starting; later cards are DRAFT until their
dependencies and concrete checks are settled. Plans/branches are not implemented
features. All new branches start from the same accepted planning baseline.

## Open gates carried forward

| Task | Status and unblock condition |
| --- | --- |
| [M4-036](tasks/M4-036.md) | BLOCKED: official Anthropic credentials and its separate authorized procedure/review |
| [M4-041](tasks/M4-041.md) | BLOCKED: compatible endpoint live tool-cycle evidence remains unmet after the recorded failed probe; no implicit retry |
| [M5.1-049](tasks/M5.1-049.md) | BLOCKED: authorized explicit refresh passed Oct 17 (token rotation, revision 2→3, restart decryption); persistent-store natural refresh window 2026-10-27 11:24:32.933–11:25:32.933 UTC still needs current preflight, explicit authority and independent review; automatic trigger unverified |
| [M5.1-051](tasks/M5.1-051.md) | DRAFT/unrun: own READY assignment, explicit authority and independent review for encrypted-reasoning continuation |
| [M5.2-025](tasks/M5.2-025.md) | BLOCKED: provisioned compatible GPU/runtime/model and separate operator authority; DEC-013 at-most-3B ceiling remains, historical 8B candidate is not authorization |

M8-008 is also BLOCKED as described above. Antigravity live browser OAuth was completed for this operator. M10-028 sole authorized post-M10-037 Flash turn used supplied verified gateway-76f3587 at base 76f3587: HTTP 503, zero SSE/reasoning/text deltas, nonempty=false terminal=false completed=false; exact allowlisted diagnostic phase=precommit category=precommit_invalid_content. Upstream status/root cause unavailable. Zero-HTTP probe passed unexpired; no refresh. Budget 22→23/1000. Ephemeral key revoked, gateway stopped, temporary files removed; persistent store preserved. Returned BLOCKED; no retry or further live authorization. Unsigned public summaries remain supported under DEC-028; signed parts remain blocked. See M10-028 evidence for bounded follow-up needs.

## Checks and operations

Pinned Go 1.27.1: `./scripts/check.sh` performs formatting, vet, unit/race tests and offline build. Note: uncached whole-tree test sweeps (`go test ./...`) require `GOFLAGS=-p=1` to prevent cross-package snapshot interference from concurrent child socket directories in `/tmp` (`GOFLAGS=-p=1 ./scripts/check.sh`). Hosted M7 CI passed this exact command. This cloud host lacks `/proc/<pid>/task/<tid>/children`, so existing process-inspection tests and M6 CLI smoke cannot fully pass locally; use hosted Linux CI for that gate. Parallel worktrees need distinct TMPDIR values as well as package serialization.

M10-027 and M10-029–038 closed DONE. M10-038 accepted unsigned empty text parts as no-op and added fixed invalid_content subkinds under DEC-029; scoped Go race **786 passed** (779 isolated), Python **11 passed** (2 isolated). M10-028 remains BLOCKED; budget unchanged at 23/1000. Agents must not inspect/display/log private database contents, master keys, tokens, or payloads. Tools, multiturn history, and cancellation follow initial smoke readiness.

[Implementation index](README.md) groups stable specs, bindings, procedures, compatibility matrices and evidence. [LOCAL-M3](LOCAL-M3.md), [LOCAL-M4](LOCAL-M4.md), [LOCAL-M5.1](LOCAL-M5.1.md) and [LOCAL-M5.2](LOCAL-M5.2.md) retain existing operations; protected schema migrations require a stopped gateway, consistent SQLite backup and matching external master key. Do not infer new IPC commands or startup fields from the roadmap.
