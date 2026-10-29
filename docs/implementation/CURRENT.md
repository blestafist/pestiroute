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

## Operator console integration and next work

M11 Admin API, M13 Operations and the fixture UI are merged through PRs
[#20](https://github.com/blestafist/pestiroute/pull/20),
[#21](https://github.com/blestafist/pestiroute/pull/21) and
[#22](https://github.com/blestafist/pestiroute/pull/22); accepted main baseline
`831f2d4`. Their integration CI, Operations and WebUI runs passed on `87b5694`.

The follow-up integration includes `dev-m12-webui` through `c573113`:
M12-005 real Admin API/authentication and M12-006 embedding/browser workflow.
Source evidence is in [M12-HANDOFF](M12-HANDOFF.md) and task cards; integration
acceptance requires green CI, Operations and WebUI on the new PR revision.
The WebUI workflow now also builds the embedded binary and runs Chromium
against the real local gateway/SQLite fixture. Packaging uses Node 24, matching CI.

The live console is a functional baseline, not the full UX outcome: Usage still
loads only its first 50 rows and lacks the fixture view's filtering/pagination.
M12-007 design foundation, theme tokens and shell layout are verified DONE. Next
execute M12-008 management workflows, M12-011 live ledger controls, M12-009
data/diagnostics presentation, then M12-010 visual and browser acceptance.
M12-011 is a bounded functional task, not cosmetic work.
See [TASKS](TASKS.md#m12--webui) and [parallel ownership](PARALLEL.md).

Antigravity (`dev-m10-antigravity`) remains in progress and excluded from this
integration by operator direction. Do not merge, restart or claim completion of
that lane; its branch-local task registry is authoritative for its newer work.
M11/M13 need no separate new worker session for their already merged scope.

## Open gates carried forward

| Task | Status and unblock condition |
| --- | --- |
| [M4-036](tasks/M4-036.md) | BLOCKED: official Anthropic credentials and its separate authorized procedure/review |
| [M4-041](tasks/M4-041.md) | BLOCKED: compatible endpoint live tool-cycle evidence remains unmet after the recorded failed probe; no implicit retry |
| [M5.1-049](tasks/M5.1-049.md) | BLOCKED: authorized explicit refresh passed Oct 17 (token rotation, revision 2→3, restart decryption); persistent-store natural refresh window 2026-10-27 11:24:32.933–11:25:32.933 UTC still needs current preflight, explicit authority and independent review; automatic trigger unverified |
| [M5.1-051](tasks/M5.1-051.md) | DRAFT/unrun: own READY assignment, explicit authority and independent review for encrypted-reasoning continuation |
| [M5.2-025](tasks/M5.2-025.md) | BLOCKED: provisioned compatible GPU/runtime/model and separate operator authority; DEC-013 at-most-3B ceiling remains, historical 8B candidate is not authorization |

M8-008 is also BLOCKED as described above. None is a dependency of independent new lanes. Preserve the isolated operator credential store and prior finite request budgets as ceilings, not permission. Credentials and private payloads stay out of repository evidence.

## Checks and operations

Pinned Go 1.27.1: `GOFLAGS=-p=1 ./scripts/check.sh` performs formatting,
vet, unit/race tests and offline build. Use a short unique TMPDIR for process
socket probes. UI checks use Node 24, `npm --prefix web ci`, typecheck,
`typecheck:e2e`, tests and build. Full UI release and browser commands are in
[M12-HANDOFF](M12-HANDOFF.md). No provider access is needed.

Source task evidence is historical; current integration evidence must come from
its CI revision. The planning host has no Go executable; no local Go or packaged
browser run is claimed. New verification is recorded in the integration PR.

[Implementation index](README.md) groups stable specs, bindings, procedures, compatibility matrices and evidence. [LOCAL-M3](LOCAL-M3.md), [LOCAL-M4](LOCAL-M4.md), [LOCAL-M5.1](LOCAL-M5.1.md) and [LOCAL-M5.2](LOCAL-M5.2.md) retain existing operations; protected schema v6 migration requires a stopped gateway, consistent SQLite backup and matching external master key. Do not infer new IPC commands or startup fields from the roadmap.
