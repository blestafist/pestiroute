# Documentation Map

Start implementation from [AGENTS.md](../AGENTS.md), then [CURRENT.md](implementation/CURRENT.md) and the selected row in [TASKS.md](implementation/TASKS.md). Read linked sections for that task, not the entire documentation tree.

## Project — What and Why

| Document | Owns |
| --- | --- |
| [Project scope](project/README.md) | Product goal, target backends, MVP boundary, success criteria, non-goals |
| [Architecture](project/ARCHITECTURE.md) | Component boundaries, request path, passthrough, streaming, retry and stateful behavior |
| [Extensibility](project/EXTENSIBILITY.md) | Lifecycle, capability and API evolution; optional future features |
| [Decisions](project/DECISIONS.md) | Accepted constraints, proposed choices, open questions |

## Implementation — How and What Next

| Document | Owns |
| --- | --- |
| [Current](implementation/CURRENT.md) | Verified state, immediate focus, blockers, available commands |
| [Tasks](implementation/TASKS.md) | Task status, owner, dependencies, scope, result, primary check; [bounded M2 plan](implementation/TASKS.md#m2--connector-api-and-conformance-baseline) |
| [Task template](implementation/tasks/TEMPLATE.md) | Planner's format for bounded task cards in `implementation/tasks/` |
| [Roadmap](implementation/ROADMAP.md) | Milestones and acceptance gates; not a task checklist |
| [Stack](implementation/STACK.md) | Toolchain, libraries, build and operational conventions |
| [Contract](implementation/CONTRACT.md) | Normative v1 Adapter–Core Runtime–Connector boundary, frames, capabilities, errors and lifecycle |
| [Configuration](implementation/CONFIGURATION.md) | Target configuration schema, persistent state, access and accounting |
| [Testing](implementation/TESTING.md) | Behavioral verification, conformance, fixtures, smoke tests |
| [References](implementation/REFERENCES.md) | Protocol sources and connector/migration research workflow |
| [Development tooling](implementation/TOOLING.md) | When to add OpenCode MCPs, plugins, commands, and local tools |

Project documents define intent; CONTRACT defines accepted internal semantics. Concrete bindings, configuration, and stack choices remain drafts until verified by code. Consult CURRENT for actual support. Keep each fact in its owning document and link rather than copying it into other files.

The [2026-09-28 pre-implementation review](references/project-review.md) records reviewed scope, corrected risks, and remaining verification gates; it does not replace the owning specifications.

## Glossary

**Northbound** — client-facing API. **Upstream / Backend Provider** — backend contacted by a Connector. **Protocol Adapter** — client-protocol boundary. **Core Runtime** — provider-agnostic execution infrastructure. **Connector** — backend implementation. **Instance** — configured Connector with endpoint and parameters. **Account** — upstream identity with a credential reference. **Virtual key** — gateway client's key. **Attempt** — one execution on a Connector instance/account pair.

**Native passthrough** preserves request and response body bytes; transport headers may change. **Translation** is explicit provider-protocol transformation in a Connector; Protocol Adapters own client-facing formatting.
