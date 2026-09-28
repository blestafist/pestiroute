# PestiRoute — Agent Guide

PestiRoute is a self-hosted AI protocol gateway: `Client → Protocol Adapter → Core Runtime → Connector → Backend Provider`. Work in small, verifiable vertical slices. Documentation is in English; respond to the user in their language.

## Start Here

1. Read [CURRENT](docs/implementation/CURRENT.md) for actual repository state and immediate focus; inspect the working tree before editing.
2. Read the assigned row in [TASKS](docs/implementation/TASKS.md) and its task card. If no task was assigned, use CURRENT to identify the next planning or implementation action.
3. Read only the card's linked specification sections and the code you will change. Use targeted search instead of loading all docs.
4. Verify dependencies and acceptance criteria before implementation. A DRAFT row is a planning candidate, not an executable assignment.

For direct user requests without a task card, act on the requested scope; do not create tracking bureaucracy for a small fix or documentation edit. Record new bounded implementation work in TASKS when it needs a planned handoff.

## Read by Task

| Task area | Read as needed |
| --- | --- |
| Product scope or MVP | [Project](docs/project/README.md) |
| Boundaries, request path, retries | [Architecture](docs/project/ARCHITECTURE.md) |
| New capability or API extension | [Extensibility](docs/project/EXTENSIBILITY.md), [Decisions](docs/project/DECISIONS.md) |
| Scaffold, dependencies, build | [Stack](docs/implementation/STACK.md), relevant [Roadmap](docs/implementation/ROADMAP.md) stage |
| Connector or execution runtime | [Contract](docs/implementation/CONTRACT.md), relevant [Testing](docs/implementation/TESTING.md) scenarios |
| Storage, credentials, limits | [Configuration](docs/implementation/CONFIGURATION.md), contract auth/usage sections |
| Provider research or migration | [References](docs/implementation/REFERENCES.md), relevant contract and testing sections |

The complete document index and glossary are in [docs/README.md](docs/README.md).

## Architectural Guardrails

- **Protocol Adapters** parse/validate client formats, create execution envelopes, and format output. Responses is primary for agents; Chat Completions is a first-class compatibility interface.
- **Core Runtime** owns routing, access policies, limits, accounting, secrets, and execution lifecycle. It must not parse OpenAI/Anthropic structures, SSE, tool calls, or reasoning, or branch on provider names.
- **Connectors** own backend communication, provider authentication, translation, streaming, tokenization, and usage extraction. They use scoped runtime credentials and never own secret storage or implicit account fallback.
- Keep payloads opaque. Do not introduce universal `Message[]`, `Tool[]`, `Reasoning{}`, `UniversalResponse`, or provider-specific execution methods.
- Native mode preserves body bytes and unknown fields. Model replacement is explicit Connector translation, not routing. Adapter formatting must not repeat already-completed connector translation.
- Preserve incremental streaming, ordering, bounded backpressure, cancellation, and exactly-once attempt finalization. No complete-response buffering. Usage/control metadata is separate from user-facing bytes.
- Fallback requires an uncommitted response and confirmed safe retry. Never replay an ambiguous delivery automatically; no fallback after commit.
- Only `supported` satisfies a required capability; `unknown` does not. Evaluate protocol/model/account scope, not provider-name assumptions.
- Core and routing do not import concrete Connectors or Protocol Adapters. Wire implementations at the composition root. Built-ins initially run in-process; external third-party Connectors follow the later process boundary.
- Future extension concepts are not immediate deliverables. Implement reload, sessions, async, new adapters, or IPC only when explicitly scoped.

## Sources of Truth

| Fact | Owner |
| --- | --- |
| Product intent and MVP boundary | `docs/project/README.md` |
| Runtime behavior and architectural boundaries | `docs/project/ARCHITECTURE.md` |
| Evolution constraints | `docs/project/EXTENSIBILITY.md` |
| Decision status and unresolved tradeoffs | `docs/project/DECISIONS.md` |
| Milestone outcomes | `docs/implementation/ROADMAP.md` |
| Actual state and immediate handoff | `docs/implementation/CURRENT.md` |
| Task status, owner, dependencies, result, primary check | `docs/implementation/TASKS.md` |
| One task's scope, acceptance and verification evidence | `docs/implementation/tasks/<ID>.md` |
| Technical specification | Relevant implementation document |

Accepted constraints bind implementation. Proposed interfaces and libraries are drafts to validate, not claims of working code. Task cards narrow scope; they cannot silently override architecture. Resolve a relevant conflict in DECISIONS and the owning specification before encoding a new contract. Do not duplicate full contracts or roadmap text into task cards.

## Planning and Execution

- Use the registry rules in TASKS and the single task-card template. Each task delivers one bounded, independently verifiable result with explicit dependencies.
- Planner creates cards just ahead of execution. Keep distant work at roadmap level until evidence supports decomposition; do not create placeholder cards for every future feature.
- Before starting a registered task, confirm READY status and completed dependencies, then set ACTIVE and an identifiable owner. Preserve unrelated edits and avoid expanding into adjacent tasks.
- Prefer a working client-to-fake-backend slice over speculative frameworks. Add directories and abstractions when a scoped feature needs them.
- If blocked, record the concrete reason and what unblocks it in the card, set BLOCKED, and summarize only the immediate impact in CURRENT.
- On completion, record actual result and primary check in TASKS, concise verification evidence in the card, and update CURRENT if focus, commands, blockers, or capabilities changed. Mark DONE only when acceptance is verified.

## Verification and Handoff

- Use commands verified in CURRENT and checks in the task card. Do not present proposed STACK commands as available before the scaffold exists.
- For Go work, use the relevant formatting, build, vet, tests, and race checks defined by the repository. Test observable behavior: opaque byte preservation, real streaming delivery, cancellation, retry safety, and accounting where affected.
- Regular checks use local deterministic fixtures. Real-provider smoke tests are separate and record client/provider versions; never include credentials or private payloads in fixtures/logs.
- For documentation changes, check relative links/anchors, terminology, task dependencies, and `git diff --check`. Keep docs concise and replace duplicate material with links.
- Report changed behavior, checks actually run and their results, and any remaining blocker. Do not claim unrun checks passed or draft functionality exists.
- Keep CURRENT a short snapshot, not a session diary. Put durable rationale in DECISIONS, task evidence in the card, and history in Git.
