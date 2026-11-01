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
| [M10](M10-ANTIGRAVITY.md) | `dev-m10-antigravity` | M10-001–048 all DONE | Exact 3.8 Flash streaming & cancellation verified; 3.1 Pro wire ID research ready (budget 44/1000) |
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

M8-008 is also BLOCKED as described above. Antigravity live browser OAuth was completed for this operator. M10-028 Phase 2 used verified gateway-586f4fb after fresh reviewer PASS ses_e6c57169bffe4UHa5d6vJweG8F: exactly one Flash stream returned HTTP 200, 7 SSE events, 0 reasoning deltas, 2 ordinary text deltas, nonempty=true terminal=true completed=true. Explicit encrypted-content include yielded zero carriers; live encrypted continuity remains unverified. Zero-HTTP probe found expired credentials; one legitimate refresh plus inference consumed 26→28/1000. Cleanup confirmed key revoked, gateway stopped and temporary files removed. M10-028 closed DONE following reviewer PASS ses_e6c51cf9affeO8MJn1yhJ6WQPV on the full 10-file artifact review target. No further live request authorized by this task. See M10-028 evidence.

## Checks and operations

Pinned Go 1.27.1: `./scripts/check.sh` performs formatting, vet, unit/race tests and offline build. Note: uncached whole-tree test sweeps (`go test ./...`) require `GOFLAGS=-p=1` to prevent cross-package snapshot interference from concurrent child socket directories in `/tmp` (`GOFLAGS=-p=1 ./scripts/check.sh`). Hosted M7 CI passed this exact command. This cloud host lacks `/proc/<pid>/task/<tid>/children`, so existing process-inspection tests and M6 CLI smoke cannot fully pass locally; use hosted Linux CI for that gate. Parallel worktrees need distinct TMPDIR values as well as package serialization.

M10-001–048 closed DONE. M10-046 closed DONE following reviewer PASS `ses_e67d848deffedx3YyQoZc2CTdP`: clean `gateway-3e72d9d8` verified; fresh zero-HTTP preflight passed all flags; exact Flash-medium completion succeeded (HTTP 200, 7 SSE events, 2 text deltas, terminal and completed true); success-gated cancellation returned HTTP 200 with `client_abort=true first_text_delta=true terminal=false`. Budget 42→44/1000 (0 refreshes, ceiling 1000 unchanged); cleanup confirmed key revoked, gateway stopped, temporary files removed. Proves local client abort only; upstream closure verified by offline connector tests; remote compute cancellation is not claimed. Note on 3.1 Pro: pinned 9router registry maps 3.1 Pro high → `gemini-pro-agent` and low → `gemini-3.1-pro-low`; prior projection bare/high/medium/low false does not exclude actual wire IDs; next bounded discovery will evaluate these two fixed wire IDs (note only, no new card until commit). M10-047 closed DONE following reviewer PASS `ses_e67fd2e29ffeN6vC4aL11G0r6a`: offline usage rejection diagnostics across all parseUsage/parseUsageMetadata branches (fixed subkind and allowlisted known_field enums, deterministic priority, strict rejection and Core accounting preserved, zero token/payload leaks; 260 connector race tests pass, vet/build clean, budget 40/1000 unchanged, zero live calls). M10-048 closed DONE following reviewer PASS `ses_e67e54a2effe1rc3RIgdmHl153`: validated camelCase public UsageMetadata supplemental fields (int32 counts, details enums, serviceTier) accepted under DEC-033 while DEC-030 is preserved; trafficType and snake_case rejected; Core accounting unchanged; 287 connector race tests pass, vet/build clean, budget 42/1000 unchanged, zero live calls. M10-042 offline carrier/replay evidence retains its own scope; M10-028 verifies only one live Flash text stream. M10-044 enabled scoped llm.tools for Responses on gemini-2.5-pro/flash. M10-043 live two-turn tool calling closed DONE. M10-045 closed DONE following reviewer PASS `ses_e683e6c37ffeppLbfpGGqtZsjc`: exact metadata probe verified `gemini-3.8-flash-{high,medium,low}` are available (`true`), while bare `gemini-3.8-flash` and all probed `gemini-3.1-pro` variants are unavailable (`false`, unknown_count=10). Bounded recovery consumed budget 36→38/1000 (one natural refresh 36→37, one metadata dispatch 37→38; ceiling 1000 unchanged). Connector offline translation admits exact 3.8 Flash variants; unadmitted IDs reject HTTP 400; no silent aliasing; tools and reasoning continuity remain Unknown on 3.8. Connector/gateway race 822 passed, vet/build clean. Agents must not inspect/display/log private database contents, master keys, tokens, or payloads; see [M10-045 evidence](tasks/M10-045.md#completion-evidence).

[Implementation index](README.md) groups stable specs, bindings, procedures, compatibility matrices and evidence. [LOCAL-M3](LOCAL-M3.md), [LOCAL-M4](LOCAL-M4.md), [LOCAL-M5.1](LOCAL-M5.1.md) and [LOCAL-M5.2](LOCAL-M5.2.md) retain existing operations; protected schema migrations require a stopped gateway, consistent SQLite backup and matching external master key. Do not infer new IPC commands or startup fields from the roadmap.
