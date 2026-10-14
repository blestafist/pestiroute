# Current State

## Verified baseline

`main` includes M5.2 merge [PR #13](https://github.com/blestafist/pestiroute/pull/13), commit `bea67fe`, following source `1d9634f`. M0–M4 deterministic implementation gates are accepted; protected native, Anthropic translation, Codex and local Responses paths are implemented. Core remains provider-agnostic under [CONTRACT](CONTRACT.md) and [M2 binding](M2-BINDING.md), with opaque bytes, scoped credentials/transport, SQLite accounting and delivery-safe fallback.

M5.1 and M5.2 handoffs are **conditionally accepted**, not fully DONE: [DEC-012/013](../project/DECISIONS.md) preserve unwaived live gates. Exact feature/evidence ceilings belong in [M4](M4-COMPATIBILITY.md), [Codex](M5.1-COMPATIBILITY.md) and [local-runtime](M5.2-COMPATIBILITY.md) matrices. Ollama and llama.cpp scoped live text gates completed; vLLM live evidence remains blocked. Tools/reasoning/parallel or compute cancellation are not promoted beyond their recorded scope. Historical completion/check evidence lives in [archived rows](archive/TASKS-DONE.md) and the stable task cards.

## Immediate handoff: M6 implementation

New branch `dev-m6` starts at that merged main revision. [M6-EXTERNAL](M6-EXTERNAL.md) scopes external Connector processes and [DEC-014](../project/DECISIONS.md#dec-014--select-grpc-over-unix-socket-for-external-connector-ipc) selects gRPC over Unix socket. In ongoing /next batch, 6/19 tasks are verified DONE (`59c40ca`, `05fbc6c`, `7f7389e`, `a19bad9`, `dc77253` for M6-015, and [M6-016](tasks/M6-016.md) complete and verified, ready for commit verification); up to 13 tasks remain under original /next19 cap without backlog expansion. Next candidate selection paused until M6-016 commit verified; candidates 017–032 remain DRAFT with no further dispatch or READY prep.

Use one card per Worker/Reviewer session and one verified task per commit through existing `/next N`. The M6 plan is not authorization to execute older live gates. No new MCP/plugin is required; transport libraries/generators are selected and pinned through the spikes. Existing server configuration does not establish machine-local connections.

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

Pinned Go 1.27.1: `./scripts/check.sh` performs formatting, vet, unit/race tests and offline build. M6-004 ran it successfully on the execution checkout; see its card for focused race checks and raw transport measurements.

[Implementation index](README.md) groups stable specs, bindings, procedures, compatibility matrices and evidence. [LOCAL-M3](LOCAL-M3.md), [LOCAL-M4](LOCAL-M4.md), [LOCAL-M5.1](LOCAL-M5.1.md) and [LOCAL-M5.2](LOCAL-M5.2.md) retain existing operations; protected schema v6 migration requires a stopped gateway, consistent SQLite backup and matching external master key. Do not infer new IPC commands or startup fields from the roadmap.
