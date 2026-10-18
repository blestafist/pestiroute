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

## M11–M13 integration

This revision reconciles M11 Admin API, the completed M12 fixture UI, and M13
Operations against main baseline `5738253`. Integration is serial: [M11 PR #20](https://github.com/blestafist/pestiroute/pull/20),
then [M13 PR #21](https://github.com/blestafist/pestiroute/pull/21), then the M12
fixture integration. Each requires green CI before its merge. M10 is still in
its own active lane and is not included here.

| Milestone | Source branch / revision | Result / next step |
| --- | --- | --- |
| [M11](M11-ADMIN-API.md) | `dev-m11-admin-api` / `886a81d` | M11-001–007 DONE; isolated management API, [OpenAPI](../api/admin.openapi.yaml) and [handoff](M11-HANDOFF.md) |
| [M12](M12-WEBUI.md) | `dev-m12-webui` / `771b206` | M12-001–006 DONE; complete operator WebUI, asset embedding, and packaged Chromium E2E; [handoff](M12-HANDOFF.md) |
| [M13](M13-OPERATIONS.md) | `dev-m13-operations` / `1b0cb6e` | M13-001–004 DONE; deployment, backup/restore and [handoff](M13-HANDOFF.md); UI packaging deferred |

M12-006 is verified and closed DONE with reviewer explicit PASS and Planner
closure. All M12 tasks (M12-001–M12-006) are complete and verified. The WebUI
defaults to the accepted M11 admin API client with memory-only authentication,
embeds built assets via the M11 static seam beneath `/admin/`, retains an
explicit HTTP 503 unavailable fallback for Go-only builds without Node, and
passes end-to-end operator workflow validation in headless Chromium with real
gateway and SQLite fixtures. Do not switch branches underneath workers. See
[PARALLEL](PARALLEL.md).

The integration review resolved the CURRENT table conflict and fixed five task
card links. Dedicated WebUI CI now runs lockfile installation, typechecking,
fixture tests and the production asset build. Operations CI runs the packaged
build, permissions/startup, backup/restore and protected offline release scripts
against the integrated gateway. These deterministic gates do not satisfy any
provider live gate or the unfinished M12-005/006 browser acceptance.

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
socket probes. Source task cards record their original worker/reviewer runs;
these are not fresh integration checks. This integration host has no Go
executable or installed WebUI dependencies, so build, unit/race, browser and
packaging runtime checks have not been rerun. Whitespace, shell syntax and
changed Markdown file links were checked locally. The CI, Operations and WebUI workflows must be green before accepting this
integration on main; source task-card checks are not fresh integration evidence.

[Implementation index](README.md) groups stable specs, bindings, procedures, compatibility matrices and evidence. [LOCAL-M3](LOCAL-M3.md), [LOCAL-M4](LOCAL-M4.md), [LOCAL-M5.1](LOCAL-M5.1.md) and [LOCAL-M5.2](LOCAL-M5.2.md) retain existing operations; protected schema v6 migration requires a stopped gateway, consistent SQLite backup and matching external master key. Do not infer new IPC commands or startup fields from the roadmap.
