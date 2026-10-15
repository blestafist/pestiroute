# Current State

## Verified baseline

`main` includes M6 merge [PR #14](https://github.com/blestafist/pestiroute/pull/14), merge commit `1391227`, following M5.2 baseline `bea67fe`. M0–M4 deterministic implementation gates are accepted; protected native, Anthropic translation, Codex and local Responses paths are implemented. Core remains provider-agnostic under [CONTRACT](CONTRACT.md) and [M2 binding](M2-BINDING.md), with opaque bytes, scoped credentials/transport, SQLite accounting and delivery-safe fallback.

M5.1 and M5.2 handoffs are **conditionally accepted**, not fully DONE: [DEC-012/013](../project/DECISIONS.md) preserve unwaived live gates. Exact feature/evidence ceilings belong in [M4](M4-COMPATIBILITY.md), [Codex](M5.1-COMPATIBILITY.md) and [local-runtime](M5.2-COMPATIBILITY.md) matrices. Ollama and llama.cpp scoped live text gates completed; vLLM live evidence remains blocked. Tools/reasoning/parallel or compute cancellation are not promoted beyond their recorded scope. Historical completion/check evidence lives in [archived rows](archive/TASKS-DONE.md) and the stable task cards.

## M6 complete; M7 research complete (evidence-bound NO-GO)

`dev-m6` completed the external Connector runtime and was merged by [PR #14](https://github.com/blestafist/pestiroute/pull/14) as `1391227`. All 33 tasks (001–032 plus audit follow-up 033) are verified DONE in the [archive](archive/TASKS-DONE.md#m6--external-plugin-runtime). Source `05be332a5b5a1cc84d198175e17500c2930a562c` passed [exact-head hosted CI](https://github.com/blestafist/pestiroute/actions/runs/38712179217/attempts/2) and [PR CI](https://github.com/blestafist/pestiroute/actions/runs/38712189193): focused uncached race tests, both clean-checkout CLI procedures and serialized shared checks.

The native Responses Connector runs in a supervised Linux child behind the same Core interface. Four concurrent operations, invocation/account/epoch isolation, bounded incremental IPC/reverse HTTP, cancellation, finite drain/reaping, safe accounting and recovery for new work are verified. Final audit fixed serialized admission and stale readiness before watcher scheduling. See [acceptance and limits](M6-HANDOFF.md), [operations](LOCAL-M6.md) and [minimal integration/example](M6-INTEGRATION.md). Core has no concrete connector/adapter or IPC imports. This is trusted same-user crash isolation, not an OS sandbox; generic Models/EstimateUsage child helpers currently receive no host services. Gateway `/readyz` is not aggregate connector health.

No M6 blocker remains. M7 Claude Code subscription Connector research baseline tasks ([M7-001](tasks/M7-001.md) through [M7-005](tasks/M7-005.md)) and public source research ([M7-006](tasks/M7-006.md)) are DONE with reviewer PASS `ses_ebf419acaffeis1nHF16zPuJEY`. Authoritative `decolua/9router` commit `87a74e1bda3677193c6f71a7e2100c90311f0689` (MIT) and companion `luqman-v1/9router-go` commit `14bb4f241350076984ad9a2ec639dd6cf7ae9de0` (license unknown) technical paths are mapped in [CLAUDE_CODE_CONNECTOR_RESEARCH.md](../references/CLAUDE_CODE_CONNECTOR_RESEARCH.md). No provider/login calls, credential usage, binary reverse engineering, or runtime implementation was performed. The M7-005 NO-GO disposition stands on current evidence; technical source facts, live interoperability, and authorization remain distinct. No further work is prepared or authorized.

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

Pinned Go 1.27.1: `./scripts/check.sh` performs formatting, vet, unit/race tests and offline build. Note: uncached whole-tree test sweeps (`go test ./...`) require `GOFLAGS=-p=1` to prevent cross-package snapshot interference from concurrent child socket directories in `/tmp` (`GOFLAGS=-p=1 ./scripts/check.sh`). Hosted M6 closure runs this exact command; local Unix-socket tests are blocked by the execution sandbox, while local build/vet and pure admission regression passed.

M7 research started with [M7-001](tasks/M7-001.md) (DONE). [Implementation index](README.md) groups stable specs, bindings, procedures, compatibility matrices and evidence. [LOCAL-M3](LOCAL-M3.md), [LOCAL-M4](LOCAL-M4.md), [LOCAL-M5.1](LOCAL-M5.1.md) and [LOCAL-M5.2](LOCAL-M5.2.md) retain existing operations; protected schema v6 migration requires a stopped gateway, consistent SQLite backup and matching external master key. Do not infer new IPC commands or startup fields from the roadmap.
