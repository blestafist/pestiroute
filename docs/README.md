# Development Documentation

**Universal AI Gateway Runtime** — a self-hosted gateway with OpenAI-compatible API and pluggable AI backends. Core manages request execution; connectors understand backend protocols.

[PLAN.md](../PLAN.md) describes the vision and architectural principles. This folder translates them into an implementation plan: which components are needed, how they interact, and what criteria define a completed stage.

The documentation was prepared before implementation began. The specified packages, commands, configuration, and interfaces represent a **proposed design**, not a description of a working application. Core constraints are inherited from PLAN; decisions about libraries and contract details can be refined based on early prototype results.

## Document Map

| Document | What it contains |
| --- | --- |
| [Stack](stack.md) | Go, HTTP, storage, observability, build tools and future IPC |
| [Architecture](architecture.md) | Package boundaries, request path, passthrough, streaming and routing |
| [Connector contract](connector-contract.md) | Execution envelope, stream events, capabilities, errors and runtime services |
| [Data and configuration](configuration.md) | Storage models, credentials, virtual keys, limits and YAML examples |
| [Milestones](milestones.md) | Implementation order, stage deliverables and acceptance criteria |
| [Compatibility testing](testing.md) | Conformance suite, fixtures, streaming scenarios and comparison with direct connection |
| [References](references.md) | Primary sources and review order for existing implementations |
| [Decisions and open questions](decisions.md) | Fixed constraints, working proposals and unresolved questions |

## How to Use

Before the first implementation, it's sufficient to read architecture, stack, and M0–M2 from milestones. During connector development, its contract and conformance suite become the primary documents. Changes affecting Core boundaries, passthrough semantics, or external API are first reflected in decisions, then in the corresponding documents.

We work in small vertical slices: a working path from client to test backend is more important than a large set of interfaces without execution. Each milestone has a verifiable result; moving to the next stage must not break the baseline of the previous one.

## Glossary

**Northbound** — the API visible to the gateway client. **Upstream** — the backend contacted by the connector. **Connector** — protocol implementation; **instance** — its configured instance with endpoint and parameters; **account** — a separate upstream identity with credential reference. **Virtual key** — the gateway client's key, not the provider's key. **Attempt** — a single execution attempt on a specific connector instance and account pair.

**Passthrough** means preserving request and response body bytes in native mode; transport headers may change. **Translation** means explicit protocol transformation inside the connector. These modes have different compatibility guarantees and must be distinguishable in diagnostics.
