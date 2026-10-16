# Current State

## Verified baseline

`main` includes M6 merge [PR #14](https://github.com/blestafist/pestiroute/pull/14), merge commit `1391227`, following M5.2 baseline `bea67fe`. M0–M4 deterministic implementation gates are accepted; protected native, Anthropic translation, Codex and local Responses paths are implemented. Core remains provider-agnostic under [CONTRACT](CONTRACT.md) and [M2 binding](M2-BINDING.md), with opaque bytes, scoped credentials/transport, SQLite accounting and delivery-safe fallback.

M5.1 and M5.2 handoffs are **conditionally accepted**, not fully DONE: [DEC-012/013](../project/DECISIONS.md) preserve unwaived live gates. Exact feature/evidence ceilings belong in [M4](M4-COMPATIBILITY.md), [Codex](M5.1-COMPATIBILITY.md) and [local-runtime](M5.2-COMPATIBILITY.md) matrices. Ollama and llama.cpp scoped live text gates completed; vLLM live evidence remains blocked. Tools/reasoning/parallel or compute cancellation are not promoted beyond their recorded scope. Historical completion/check evidence lives in [archived rows](archive/TASKS-DONE.md) and the stable task cards.

## M7 merged; next: independent M8 and M9 lanes

[PR #15](https://github.com/blestafist/pestiroute/pull/15) merged to main as
`1a76494113b20561cf5b04cf66c4ba0487e762c0`. Source `44f5a29` passed both
[PR CI](https://github.com/blestafist/pestiroute/actions/runs/38951339646) and
[branch CI](https://github.com/blestafist/pestiroute/actions/runs/38951332753):
external admission regressions, both M6 CLI procedures, format/vet/unit/race/build.
Review fixed Responses image URLs, malformed tool arguments, late SSE content,
and fixture whitespace handling. All 16 M7 tasks are in the
[completed registry](archive/TASKS-DONE.md#m7--claude-code-subscription-connector-research).

The Claude connector is still an offline candidate: public Execute fails closed,
gateway composition rejects it, capabilities remain Unknown, and no live support
is claimed. DEC-018/019 and M7-005's unwaived live gates remain in force.
M6 is complete; its external child runtime, acceptance and operational boundaries
remain in [M6-HANDOFF](M6-HANDOFF.md) and [LOCAL-M6](LOCAL-M6.md).

### M8 lane

[Claude Code execution/integration](M8-CLAUDE-INTEGRATION.md), branch `dev-m8`.
Start [M8-001](tasks/M8-001.md) (READY): settle the integration binding and scope
decisions. M8-002/003 are DRAFT; live/provider work and enabling production routes
are not authorized by this planning handoff.

### M9 lane

[Chat Completions client compatibility](M9-CHAT-COMPLETIONS.md), branch `dev-m9`.
Start [M9-001](tasks/M9-001.md) (READY): define the native adapter/Connector binding.
M9-002/003 are DRAFT. The endpoint is not implemented yet; this lane does not
wait for Claude provider access.

Use [separate worktrees and milestone filters](PARALLEL.md): `/next 10 M8` and
`/next 10 M9`. Keep lane-specific TASKS/CURRENT updates and shared gateway/spec
ownership explicit; a branch's completion is not evidence of integration on main.

## Open gates carried forward

| Task | Status and unblock condition |
| --- | --- |
| [M4-036](tasks/M4-036.md) | BLOCKED: official Anthropic credentials and its separate authorized procedure/review |
| [M4-041](tasks/M4-041.md) | BLOCKED: compatible endpoint live tool-cycle evidence remains unmet after the recorded failed probe; no implicit retry |
| [M5.1-049](tasks/M5.1-049.md) | BLOCKED: natural refresh threshold 2026-10-15 21:04:55.103 UTC plus current preflight, explicit authority and independent review; real refresh unverified |
| [M5.1-051](tasks/M5.1-051.md) | DRAFT/unrun: own READY assignment, explicit authority and independent review for encrypted-reasoning continuation |
| [M5.2-025](tasks/M5.2-025.md) | BLOCKED: provisioned compatible GPU/runtime/model and separate operator authority; DEC-013 at-most-3B ceiling remains, historical 8B candidate is not authorization |

None is an M6 dependency. Preserve the isolated operator credential store and prior finite request budgets as ceilings, not permission. Credentials and private payloads stay out of repository evidence.

## Checks and operations

Pinned Go 1.27.1: `./scripts/check.sh` performs formatting, vet, unit/race tests and offline build. Note: uncached whole-tree test sweeps (`go test ./...`) require `GOFLAGS=-p=1` to prevent cross-package snapshot interference from concurrent child socket directories in `/tmp` (`GOFLAGS=-p=1 ./scripts/check.sh`). Hosted M7 CI passed this exact command. This cloud host lacks `/proc/<pid>/task/<tid>/children`, so existing process-inspection tests and M6 CLI smoke cannot fully pass locally; use hosted Linux CI for that gate. Parallel worktrees need distinct TMPDIR values as well as package serialization.

[Implementation index](README.md) groups stable specs, bindings, procedures, compatibility matrices and evidence. [LOCAL-M3](LOCAL-M3.md), [LOCAL-M4](LOCAL-M4.md), [LOCAL-M5.1](LOCAL-M5.1.md) and [LOCAL-M5.2](LOCAL-M5.2.md) retain existing operations; protected schema v6 migration requires a stopped gateway, consistent SQLite backup and matching external master key. Do not infer new IPC commands or startup fields from the roadmap.
