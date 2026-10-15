# Implementation Index

Start with [CURRENT](CURRENT.md), then one selected row in [TASKS](TASKS.md) and its task card. Read only linked sections. Stable specification and task-card paths are intentionally retained so existing agent handoffs/evidence remain usable.

| Group | Documents |
| --- | --- |
| Active planning | [M6 external runtime](M6-EXTERNAL.md), [ROADMAP](ROADMAP.md), [TASKS](TASKS.md), [card template](tasks/TEMPLATE.md) |
| Shared specifications | [CONTRACT](CONTRACT.md), [CONFIGURATION](CONFIGURATION.md), [TESTING](TESTING.md), [STACK](STACK.md), [REFERENCES](REFERENCES.md), [TOOLING](TOOLING.md) |
| Native/component bindings | [M1](M1-BINDING.md), [M2](M2-BINDING.md) |
| Protected runtime/storage | [M3 runtime](M3-RUNTIME.md), [storage](M3-STORAGE.md), [auth](M3-AUTH.md), [config](M3-CONFIG.md) |
| Connector implementation briefs | [M4 translation](M4-TRANSLATION.md), [M4 binding](M4-BINDING.md), [M5.1 Codex](M5.1-CODEX.md), [M5.1 binding](M5.1-BINDING.md), [M5.2 local](M5.2-LOCAL.md) |
| Evidence-scoped support | [M4 matrix](M4-COMPATIBILITY.md), [Codex matrix](M5.1-COMPATIBILITY.md), [local-runtime matrix](M5.2-COMPATIBILITY.md) |
| Offline operations | [M1](LOCAL-M1.md), [M2](LOCAL-M2.md), [M3](LOCAL-M3.md), [M4](LOCAL-M4.md), [M5.1](LOCAL-M5.1.md), [M5.2](LOCAL-M5.2.md), [M6](LOCAL-M6.md) |
| Separate live procedures | [M1](SMOKE-M1.md), [M4](SMOKE-M4.md), [M5.1](SMOKE-M5.1.md), [M5.2](SMOKE-M5.2.md); authority/readiness comes from each live task |
| External Connector integration | [M6 wire binding](M6-BINDING.md), [minimal guide/example](M6-INTEGRATION.md), [acceptance/handoff](M6-HANDOFF.md) |
| History | [Completed task registry](archive/TASKS-DONE.md); detailed completion evidence remains in `tasks/<ID>.md`, sanitized artifacts in `evidence/`; Git owns session history |

TASKS and its archive have disjoint rows: pending work is active, historical DONE work is archived. An archived row is still a registered dependency. Do not load the entire archive for each worker session. Keep IDs/card paths stable and never discard blocked live gates to clean up documentation.
