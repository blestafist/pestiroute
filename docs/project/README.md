# PestiRoute — Project Scope

PestiRoute is a lightweight, self-hosted AI protocol gateway. Clients use a stable API while operators route requests across official API providers, subscription-backed client protocols, and local inference runtimes through replaceable Connectors.

The intended result is a small deployable runtime that centralizes routing, accounts, credentials, virtual keys, limits, and usage tracking. It should preserve agent workflows—tools, parallel tool calls, reasoning, and streaming—within each backend's declared capabilities.

## Product Boundaries

```text
Client → Protocol Adapter → Core Runtime → Connector → Backend Provider
```

- **Protocol Adapter:** how a client talks to PestiRoute; parses and validates the client protocol and formats output.
- **Core Runtime:** infrastructure, routing, policies, accounting, and execution lifecycle; payloads remain opaque.
- **Connector:** backend communication, authentication, translation, streaming, tokenization, and provider usage.

OpenAI Responses is the primary agent-oriented protocol. Chat Completions is a compatibility protocol; both are first-class northbound interfaces. Anthropic Messages and potentially Gemini-compatible formats fit the same adapter boundary. Architectural support does not imply that every endpoint is implemented in the first release.

The architecture explicitly excludes an internal universal LLM language: no shared `Message[]`, `Tool[]`, `Reasoning{}`, or `UniversalResponse`. Native mode preserves body bytes, including unknown fields; translation is explicit and capability-scoped.

## Backends We Want to Reach

| Connector family | Intended targets |
| --- | --- |
| API | OpenAI, Anthropic, Gemini official APIs |
| OpenAI-Compatible | OpenRouter, vLLM OpenAI endpoint, LM Studio, Together, Groq, Ollama OpenAI endpoint |
| Agent Protocol | Codex, Claude Code, Gemini CLI, ACP agents |
| Local Runtime | Ollama native API, vLLM native endpoints, llama.cpp |

An Agent Protocol Connector reproduces the client/backend protocol; it does not necessarily run the official client or an agent. ACP may communicate with an agent process. Reverse-engineered protocols are first-class connector implementations. Each target needs its own research and verified compatibility scope.

## MVP and Expansion

The standalone **native MVP ends at M3**:

- One working Responses-native path, starting with the generic OpenAI-Compatible Connector.
- Incremental streaming, cancellation, byte-preserving passthrough, and explicit routing.
- Connector contract and deterministic conformance baseline.
- Accounts, encrypted credentials, virtual keys, usage, limits, and safe bounded fallback.
- Single-process, single-node deployment with local administrative CLI and SQLite state.

M4 validates cross-provider translation through Anthropic. M5.1 adds independently researched agent-protocol connectors; M5.2 covers local runtimes. M6 adds the external connector process runtime. OpenAI API and Codex provide useful direct-API/client-protocol reference implementations. Stage deliverables and acceptance gates live only in [ROADMAP.md](../implementation/ROADMAP.md).

Additional northbound endpoints, session operations, configuration reload, async execution, and alternative secrets backends remain extension points until separately scoped. External third-party connectors follow the M6 process boundary; built-in first-party connectors initially run in-process.

## Success Criteria

- A real agent client, such as OpenCode, can complete a reproducible tool workflow through the gateway.
- Native direct-versus-gateway behavior preserves bytes, identifiers, event ordering, and cancellation semantics.
- New backend support is added through Connectors; client formats through Protocol Adapters, without provider branches in Core Runtime.
- Claimed capabilities have evidence; unsupported and unknown features are explicit.
- Operators can manage access and usage without exposing provider credentials to clients.

## Non-Goals

Initial scope excludes UI, marketplace, billing, Kubernetes infrastructure, distributed clusters, agent orchestration, prompt management, and a universal LLM abstraction. Session portability across providers and complete Responses resource APIs are not implied by basic inference support.

## Migration Principle

Reuse provider code and protocol knowledge from 9Router, not its routing, core architecture, or internal LLM models. Source revision and license must be recorded before migration. The research workflow and source registry live in [REFERENCES.md](../implementation/REFERENCES.md#migration-from-9router).

## Where to Go Next

- [ARCHITECTURE.md](ARCHITECTURE.md): component boundaries and request behavior.
- [EXTENSIBILITY.md](EXTENSIBILITY.md): extension constraints and compatibility.
- [DECISIONS.md](DECISIONS.md): accepted/proposed decisions and unresolved questions.
- [CURRENT.md](../implementation/CURRENT.md): actual repository state and immediate focus.
