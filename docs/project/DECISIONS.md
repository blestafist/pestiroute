# Decisions and Open Questions

## Statuses

**Accepted** means a project constraint or direct clarification of its boundaries. **Proposed** is a working choice for the first implementation that needs validation through code. **Open** is a question with a concrete decision milestone. A change to an accepted constraint must be recorded here and reflected in the affected project/specification documents; a task card cannot silently override it.

## Decision Registry

| ID | Status | Decision | Rationale |
| --- | --- | --- | --- |
| D01 | Accepted | OpenAI Responses is the primary external contract | Enables agent workflows without requiring custom LLM-specific language |
| D02 | Accepted | Core contains no backend-specific code | Enables extension through connectors |
| D03 | Accepted | Payload is opaque; native body is preserved byte-for-byte | Ensures compatibility with unknown fields and extensions |
| D04 | Accepted | Northbound adapter is separated from Core | Keeps API parsing separate from routing and limits |
| D05 | Accepted | Provider-specific translation and tokenizer belong to the connector | Prevents API differences from being normalized inside the core |
| D06 | Accepted | Third-party connectors execute out-of-process | Ensures their failure does not bring down the gateway |
| D07 | Proposed | Go, `net/http`, single module | Provides minimal infrastructure for M0–M3 |
| D08 | Proposed | SQLite for single-node state | Enables self-hosted deployment without separate database |
| D09 | Accepted | `Head / Body / Complete` as the v1 transport stream | DEC-004 fixes lifecycle semantics without an LLM event model |
| D10 | Proposed | gRPC over Unix socket for IPC | Will validate overhead and host-service lifecycle in M6 |
| D11 | Accepted | Model rewrite is not a native passthrough | Requires body modification to be explicit |
| D12 | Proposed | CLI for initial admin operations | Enables account/key management without UI milestone |
| D13 | Accepted | Client Protocol Adapters own client request parsing, protocol validation, envelope creation, and client response formatting | Supports multiple northbound protocols without protocol structures or conversions in Core; provider-specific translation remains in connectors |
| D14 | Accepted | Responses is primary for agents; Chat Completions is a compatibility protocol; both are first-class northbound interfaces | Adds client compatibility through adapters while preserving the opaque transport envelope and connector model |
| D15 | Accepted | CONTRACT.md owns the v1 Adapter–Core Runtime–Connector boundary; semantic changes require ADR | Prevents per-provider API growth and undocumented compatibility breaks; see DEC-004 |

## Contract ADRs

These accepted ADRs explain the existing constraints and establish the internal contract baseline. `Dxx` rows remain the compact registry; `DEC-xxx` identifies a rationale record, not a renumbering of that registry.

### DEC-001 — Why Core Has No LLM Abstractions

- **Status:** Accepted; elaborates D02 and D03.
- **Context:** Client and backend protocols evolve independently in tools, reasoning, multimodal content, and extensions.
- **Decision:** Core executes opaque payloads with transport/routing metadata. It must not define universal messages, tools, reasoning objects, or response chunks.
- **Alternative rejected:** Normalize every request into a shared LLM schema. This loses unknown semantics and forces Core/API changes whenever protocols evolve.
- **Consequences:** Protocol Adapters and Connectors own semantic validation and transformation. Capabilities describe eligibility, not a normalized payload language. Native passthrough is the compatibility baseline.
- **Verification:** Unknown fields survive byte-for-byte; Core has no protocol-parser dependencies; new backend support does not add provider branches.

### DEC-002 — Why Responses Is a Protocol, Not the Internal Model

- **Status:** Accepted; elaborates D01, D04, D13, and D14.
- **Context:** Responses is the primary agent-oriented client protocol, but other client formats must be independently supportable.
- **Decision:** Responses is one versioned protocol at the Protocol Adapter boundary. The internal envelope carries a protocol identifier and opaque bytes rather than using Responses structures as a shared representation.
- **Alternative rejected:** Convert all client requests to Responses before execution. That makes one external protocol an internal dependency and constrains future formats.
- **Consequences:** Chat Completions remains a first-class compatibility interface. Each Connector declares accepted protocols; adding an adapter does not automatically make every backend compatible.
- **Verification:** The internal contract defines no Responses-specific fields; routing matches declared protocol and capabilities.

### DEC-003 — Why Connectors Own Provider Translation

- **Status:** Accepted; elaborates D05 and D13.
- **Context:** Mapping provider fields, streaming events, tokenization, and accounting depends on backend-specific behavior and capabilities.
- **Decision:** Connectors privately translate the admitted client protocol to/from their Backend Provider. Protocol Adapters decode client requests and encode client transport; Core only orchestrates. The public Connector API exposes execution, not named protocol-conversion methods.
- **Alternative rejected:** A translation hub in Core or a shared provider-neutral message model. Either leaks provider assumptions into runtime infrastructure.
- **Consequences:** Connectors may share private protocol helpers, while their public contract stays stable. Adapters preserve already-compatible bytes rather than translating twice. Unsupported semantic transformations are explicit failures.
- **Verification:** Translation tests cover declared semantics and negative cases; native bytes and tool relationships are preserved where promised; no provider conversion logic appears in Core.

### DEC-004 — Stable Internal Execution Contract v1

- **Status:** Accepted; establishes D09 and D15 before implementation.
- **Context:** Earlier documents used draft Go signatures, short capability keys, and conceptual streaming roles. Leaving them as competing contracts would create incompatible first Connectors.
- **Decision:** [CONTRACT.md](../implementation/CONTRACT.md) owns the language-neutral v1 boundary: `ExecutionRequest`/`RawPayload`, a transport `ExecutionResponse`, component lifecycle, adapter and Connector operations, namespaced three-valued capabilities, and common error metadata. `Head / Body / Complete` remains the frame set; HEADERS/DATA/CONTROL/USAGE/ERROR/END are mapped roles. Existing auth, model-discovery, and usage-estimation responsibilities are retained.
- **Alternatives rejected:** Freeze Go/IPC syntax before validation; introduce six underspecified frame variants; use only a retryable boolean for replay; or keep multiple capability spellings.
- **Compatibility:** This supersedes documentation drafts, not released plugins. Existing short capability keys must be replaced in examples; future semantic changes require an ADR and explicit versioning policy. Protocol-specific payload additions alone do not change the internal contract.
- **Verification:** M2 conformance must cover frame lifecycle, opacity, capability eligibility, scoped services, cancellation, and retry safety; M6 must preserve the same behavior across IPC. Concrete bindings may refine representation, not silently change semantics.

## Questions Before Implementation

| Question | When to Decide | How to Validate |
| --- | --- | --- |
| Which upstream and client provide the first baseline? | M0 | Choose a Responses-native endpoint and reproducible tool scenario |
| Where are the required 9Router sources located? | Before migration | Record repository, commit, license, and adapter list |
| Which Responses features are included in the first public compatibility claim? | M1 | Publish endpoint/feature matrix, including unsupported stateful operations |
| How do public model IDs map to upstream IDs? | M2 | Native identity mapping; rewrite only as explicit connector mode |
| How to extract requirements without full payload parsing? | M2 | Route policy plus minimal northbound extractor; unknown fields preserved |
| What does output budget look like with unknown usage? | M3 | Choose rejection/conservative policy and validate concurrency |
| How is account affinity preserved for response IDs? | Before multi-account stateful routing | Validate scope IDs and lifetime without rewriting native response |
| Which reasoning capabilities can be transferred without losing semantics? | M4 | Real fixtures and explicit negative cases |
| Is a Responses → Chat translator needed in the first local connector? | M5.2 | Validate capabilities of specific Ollama/vLLM versions |
| What exactly does the ACP connector execute? | Before ACP implementation | Choose agent process, transport, and tool loop owner |
| How does an external connector call host runtime services? | M6 | Prototype scoped bidirectional lifecycle and cancellation |

## Format of Future ADRs

Every semantic internal-contract change requires a short ADR here: context, decision, alternatives, consequences, compatibility/migration impact, and verification criteria. Update CONTRACT and affected specifications with the decision. Other substantial architectural tradeoffs use the same format; routine editorial corrections do not need an ADR.

This registry and its compact ADRs remain the single decision entry point. They distinguish accepted constraints from implementation choices that still need validation.
