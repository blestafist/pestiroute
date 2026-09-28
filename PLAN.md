# Universal AI Gateway Runtime

> The practical development plan, stack, contracts, and readiness criteria are collected in [docs/README.md](docs/README.md).

> The [Extensibility Model](docs/EXTENSIBILITY.md) defines how Protocol Adapters and Connectors evolve through lifecycle management, capabilities, opaque execution envelopes, and explicit API compatibility. Core Runtime remains provider-agnostic; extensions must preserve native passthrough and must not introduce a universal LLM abstraction.

## Project Vision

We are building a lightweight self-hosted AI protocol gateway that provides an OpenAI-compatible API and connects to any AI backends through an extensible connector system. This is not just an LLM API gateway: it must work with regular API providers, existing AI subscriptions, local models, and official AI client protocols, including those recovered through reverse engineering.

The gateway centralizes routing between backends, credential management, accounts, limits, and usage tracking. The core remains minimal: it manages infrastructure while all backend-specific logic resides in connectors.

The primary architectural constraint is to avoid creating an internal "universal LLM language." The external contract already exists: OpenAI Responses API. Each connector independently decides how to deliver such a request to its backend and return a compatible response.

## 1. Boundary Between Core and Connectors

Core handles API serving, client authentication, virtual keys, routing, limits, usage, connector lifecycle, and plugin runtime. Core knows nothing about specific providers: it should contain no OpenAI, Anthropic, or Gemini code, no provider OAuth flows, no request/response formats, no tokenization rules, and no backend-specific streaming formats.

The OpenAI-compatible API is the gateway's external contract, not justification for introducing provider logic into the infrastructure core. External API processing must remain independent of any particular upstream's structure.

```text
                         Clients
                            |
                  Client API Protocol
                            |
                  Client Protocol Adapter
                            |
                  Internal Execution Envelope
                            |
                 +----------v-----------+
                 |         CORE         |
                 | API Server / Auth    |
                 | Routing / Limits     |
                 | Usage / Runtime      |
                 +----------+-----------+
                            |
                    Connector Interface
                            |
                             +-- API Connector → OpenAI / Anthropic / Gemini APIs
                             +-- OpenAI-Compatible Connector → OpenRouter / vLLM OpenAI endpoint
                             +-- Agent Protocol Connector → Claude Code / Codex / Gemini CLI / ACP
                             +-- Local Runtime Connector → Ollama native API / llama.cpp / vLLM native endpoints
```

## 2. External API

The primary endpoint is `POST /v1/responses`. It must be compatible with OpenAI Responses API, Codex clients, OpenCode, and other agent frameworks. Responses was chosen as the primary contract because it supports reasoning, tool calls, parallel tools, streaming events, and multi-step agent workflows.

Additionally, `POST /v1/chat/completions` can be supported through its own Client Protocol Adapter. Chat Completions is a compatibility protocol and Responses is the primary agent-oriented protocol, but both are first-class northbound interfaces. Anthropic Messages and potentially Gemini-compatible APIs and other protocols can be added through the same architectural layer.

Client Protocol Adapters parse and validate client-protocol requests, produce the minimal internal execution envelope, and convert connector output streams into the client protocol format. Core remains unaware of OpenAI and Anthropic request structures, Responses/Chat Completions conversion, tool call formats, and reasoning formats. The envelope carries a protocol identifier, raw payload, and routing metadata; responses carry stream frames, raw bytes where possible, and lifecycle events, not a universal LLM model. Backend communication, provider authentication, provider-specific translation, streaming, and usage remain connector responsibilities. See [Client Protocol Adapters](docs/architecture.md#client-protocol-adapters) for boundaries and example paths.

## 3. What is a Connector

A connector is an isolated implementation for interacting with one backend or a family of compatible backends. It receives a request in a declared protocol and may internally use direct API calls, protocol emulation, or local process communication. Core does not care which approach is used.

Proposed implementation structure:

```text
connectors/
  openai-api/
  anthropic-api/
  gemini-api/
  openai-compatible/
  claude-code/
  codex/
  gemini-cli/
  ollama/
  vllm/
```

### Connector Types

1. **API Connector** — direct communication with official provider APIs, such as OpenAI API, Anthropic API, and Gemini API. It handles upstream authentication, request formation, response parsing, streaming, and usage extraction. For example: `Core → OpenAI API Connector → OpenAI API`.
2. **OpenAI-Compatible Connector** — connects existing backends exposing OpenAI-compatible APIs, such as OpenRouter, vLLM, Ollama's OpenAI endpoint, LM Studio, Together, and Groq. Connecting a compatible service should require minimal configuration without writing new code.
3. **Agent Protocol Connector** — exposes existing AI client/subscription protocols through the gateway, such as Claude Code, Codex, Gemini CLI, and ACP-based agents.
4. **Local Runtime Connector** — connects local inference runtimes, such as Ollama's native API, llama.cpp, and vLLM native endpoints.

A connector may internally use direct API calls, protocol emulation, or local process communication; Core does not care which approach is used.

```yaml
connector: openai-compatible
base_url: https://backend.example/v1
api_key: <credential-reference>
models:
  - model-a
  - model-b
```

### Agent Protocol Connector

An Agent Protocol Connector reproduces how an official AI client communicates with its backend. It does not necessarily run an agent: the Claude Code Protocol Connector, for example, does not run Claude Code itself. It reproduces the client/backend communication contract. It may use reverse-engineered protocols, existing authentication flows, hidden parameters, and provider-specific interaction patterns. ACP-based connectors may instead communicate with an agent process, as scoped for that protocol. These details remain inside the connector.

```text
OpenAI Responses API
        |
        v
Claude Code Protocol Connector
        |
        v
Anthropic backend
```

### Reverse-Engineered Connectors

Reverse-engineered connectors are first-class citizens. The gateway may support backends where official API access is unavailable, subscription access exists, and official clients already implement authentication and transport. The connector owns protocol implementation, authentication behavior, request generation, streaming handling, and provider-specific quirks. Core remains unchanged.

### Local Runtime Connectors

Local runtime connectors enable working with Ollama, vLLM, and llama.cpp. If a runtime already provides a suitable OpenAI-compatible API, it can be connected through the generic compatible connector; a separate implementation is only needed for specific protocols or behaviors.

## 4. Connector API

The connector interface should be small and infrastructural. Methods like `ChatCompletion()`, `Responses()`, `AnthropicMessages()`, or `GeminiGenerate()` must not be added: such forms leak provider-specific models into Core.

Preliminary Go contract:

```go
type Connector interface {
    Describe(ctx context.Context) Descriptor
    Authenticate(ctx context.Context, request AuthRequest, runtime Runtime) AuthResult
    EstimateUsage(ctx context.Context, request Request, runtime Runtime) UsageEstimate
    Execute(ctx context.Context, request Request, runtime Runtime) Stream
    Models(ctx context.Context, runtime Runtime) []Model
    Health(ctx context.Context, runtime Runtime) HealthStatus
}
```

`Describe` reports connector capabilities, `Authenticate` performs the connector's authentication portion, `EstimateUsage` estimates usage before execution, and `Execute` returns a result stream. `Models` provides available models, `Health` reports connector status. Specific types and error handling mechanics are clarified during implementation without extending the interface with provider-specific methods.

## 5. Request Model and Native Passthrough

A request is passed as an opaque payload with protocol indication and a minimal set of infrastructure metadata. There is no need for universal `Messages[]`, `Tools[]`, `Reasoning{}`, or `Images{}`: providers evolve independently, and such abstraction would quickly become limiting.

```go
type Request struct {
    Protocol string
    Headers  map[string][]string
    Body     []byte

    Metadata struct {
        Model        string
        Streaming    bool
        Requirements []Capability
    }
}
```

For example, `Protocol` contains `openai.responses/v1`, and `Body` contains the original JSON. Metadata is used for routing and execution management but does not replace the payload with an internal LLM model.

Native passthrough is a mandatory requirement. If a connector natively supports the incoming protocol, the request body flows through `Client → Protocol Adapter → Core → Connector → Provider` unchanged. Core must not parse, reassemble, normalize the payload, or remove unknown fields. Extracting necessary routing metadata at the external API boundary must not turn into request body reconstruction inside Core.

This preserves tool calls, parallel tool calls, reasoning, and provider extensions, including fields the gateway does not yet recognize.

## 6. Translation

If a backend does not support the incoming protocol, transformation is performed exclusively within the connector. For example, the Anthropic connector translates OpenAI Responses into Anthropic Messages and returns a result compatible with the external API.

Only the connector knows field mapping rules, tool transformations, streaming events, errors, and usage. Core does not participate in semantic translation between APIs and does not maintain an intermediate universal response or request model.

## 7. Capabilities

Each connector declares its capabilities. The description should distinguish between confirmed support, lack of support, and unknown status.

```yaml
capabilities:
  streaming: true
  tools: true
  parallel_tools: true
  reasoning: true
  images: false
  session_resume: false
  exact_usage: true
```

Routing considers request requirements: if `tools`, `parallel_tools`, and `reasoning` are needed, only connectors supporting all required capabilities can be selected. In MVP, these requirements may be explicitly set in metadata and route configuration; automatic capability matching is developed later.

## 8. Authentication and Credentials

Core owns secret storage, encryption, credential references, and account management. It also provides common OAuth infrastructure: callback server, PKCE, and refresh scheduling. However, provider OAuth endpoints, scopes, token exchange, and refresh implementation belong to the connector.

The authentication flow looks like `User → Core Auth Runtime → Connector Authenticate() → Provider → Credential Store`. The connector executes the protocol-specific portion through the runtime but does not store secrets itself. This separation enables centralized credential management without adding provider-specific knowledge to Core.

## 9. Usage and Token Counting

Tokenization belongs to the connector, since the same request might consume, for example, 12 thousand tokens with OpenAI and 13 thousand with Anthropic. Core should not select a tokenizer or interpret provider-specific counting rules.

Before execution, Core calls `connector.EstimateUsage()` and uses the estimate to check limits. After execution, the connector extracts actual usage from the backend response and passes it to Core Usage Storage. The report includes `input_tokens`, `output_tokens`, `reasoning_tokens`, and `cached_tokens`; the `exact_usage` capability indicates whether precise tracking is available.

## 10. Virtual Keys

The gateway provides its own virtual API keys. They can be created, revoked, enabled, and disabled; each key supports restrictions by models and connectors, RPM and TPM limits, and usage tracking. Clients receive only virtual keys and never access provider credentials.

## 11. Routing

In MVP, routing is built on explicit model mapping, connector and account selection, fallback, and limit checking. For each request, the gateway must determine a suitable route and verify the connector has the required capabilities.

Later, selection by latency and cost can be added, along with automatic capability matching. These mechanisms should not change the responsibility boundary: Core selects the executor, the connector understands the backend.

## 12. Streaming

Streaming is mandatory from the first working version. The stream flows through the chain `Provider Stream → Connector → Core → Client` without buffering the complete response. The gateway must support SSE, cancellation propagation from client to upstream, tool streaming, reasoning streaming, and usage events.

Provider streaming format is parsed by the connector. Core passes the result to the client and manages the request lifecycle without interpreting the backend's internal protocol.

## 13. Plugin Runtime

A third-party connector should not be able to crash Core with its failure. Therefore, Go native plugins are not used; the target model is a separate connector process communicating with Core through a versioned IPC protocol.

Possible transports include stdio, Unix socket, or gRPC/Connect. The final choice must provide streaming, cancellation, health checks, versioning, and process isolation. External plugin runtime is a separate phase, but the connector boundary is designed with this execution model in mind from the start.

## 14. Connector Manifest

Each connector provides a manifest with identifier, type, version, accepted protocols, capabilities, and available authentication methods.

```yaml
id: claude-code
type: agent-protocol
version: "1.0"
protocols:
  accepts:
    - openai.responses/v1
capabilities:
  tools: true
  streaming: true
  parallel_tools: unknown
auth:
  - oauth
```

The manifest allows Core to discover and use connectors without knowing their internal implementation.

## 15. Testing and Conformance

Connectors need a common conformance suite. It verifies basic requests, streaming, tool calls, parallel tool calls, multiple sequential tool rounds, reasoning, tool choice, usage, errors, cancellation, and preservation of unknown fields. Capability checks align with the manifest; unsupported capabilities should not appear as successfully supported.

A key regression test compares direct `OpenCode → Provider` connection with `OpenCode → Gateway → Provider` connection. Behavior must remain equivalent, especially for parallel tool calls, tool identifiers, and event ordering. Native passthrough is separately verified to ensure request body preservation without modifications.

## 16. First Connectors

The first connector needed is a generic OpenAI-compatible one: it covers most compatible services. OpenAI API and Codex provide reference implementations for direct API and client protocol respectively. Anthropic API is needed to verify the translation architecture, Claude Code for subscription-backed usage, and Ollama/vLLM for local models.

These implementations should use the same infrastructure contract without requiring provider-specific exceptions in Core.

## 17. Migration from 9Router

**9Router migration principle: provider code is migrated, not architecture.** Reuse provider adapters, OAuth flows, request builders, stream parsers, usage extraction, model handling, and protocol knowledge inside corresponding connectors.

Do not migrate old routing, old core abstractions, or old internal LLM models. Every migrated connector must pass direct-vs-gateway compatibility tests as well as the conformance suite: working code in 9Router alone does not confirm compatibility with the new gateway.

## 18. MVP Implementation Order

### Phase 1 — Transparent Proxy

Build a minimal working path `OpenCode → Gateway → OpenAI Responses`. The main goal is to confirm transparent request passing, streaming, tools, and parallel tools. This version establishes the behavior baseline for subsequent changes.

### Phase 2 — Connector API

Formalize a minimal Connector API and move the OpenAI implementation into a connector. Verify that the infrastructure core no longer depends on upstream structure and maintains native passthrough.

### Phase 3 — Access and Usage Management

Add credentials, accounts, virtual keys, usage, and limits. Connect pre-execution usage estimation with limit checking and actual usage storage after execution.

### Phase 4 — Anthropic Connector

Add the Anthropic API connector and implement translation from OpenAI Responses. This phase verifies that all request, response, and streaming transformations remain within the connector.

### Phase 5 — Agent Protocol Connectors

Add connectors for Claude Code, Codex, Gemini CLI, and ACP. Research each client protocol before implementing its connector separately, using common runtime infrastructure for authentication. Local runtime connectors are tracked separately in M5.2.

### Phase 6 — External Plugin Runtime

Move connector execution into separate processes. Implement IPC, versioning, health checks, streaming, cancellation, and isolation while preserving the already verified API behavior.

## 19. What is Not in Project Scope

Do not start with UI, building a marketplace, billing, Kubernetes infrastructure, or distributed clusters. The gateway should also not become an agent framework, prompt management system, or universal LLM abstraction. The priority is a lightweight runtime with a reliable external contract and extensible connectors.

## Final Architectural Rule

**Core manages infrastructure. Connector understands backend. These responsibilities must not be mixed.**

OpenAI Responses remains the external contract. All specifics of APIs, subscriptions, client protocols, and local runtimes are isolated inside connectors, not turned into a new universal language inside the gateway.
