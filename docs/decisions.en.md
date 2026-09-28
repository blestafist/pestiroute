# Decisions and Open Questions

## Statuses

**Accepted** means a constraint from PLAN or direct clarification of its boundaries. **Proposed** is a working choice for the first implementation that needs validation through code. **Open** is a question with a concrete decision milestone. Changing an accepted constraint requires updating PLAN and related documents.

## Decision Registry

| ID | Status | Decision | Rationale |
| --- | --- | --- | --- |
| D01 | Accepted | OpenAI Responses is the primary external contract | Agent workflows without requiring custom LLM language |
| D02 | Accepted | Core contains no backend-specific code | Extension through connectors |
| D03 | Accepted | Payload is opaque; native body is preserved byte-for-byte | Compatibility with unknown fields and extensions |
| D04 | Accepted | Northbound adapter is separated from Core | API parsing is kept separate from routing and limits |
| D05 | Accepted | Translation and tokenizer belong to the connector | API differences are not normalized inside the core |
| D06 | Accepted | Third-party connectors execute out-of-process | Their failure does not bring down the gateway |
| D07 | Proposed | Go, `net/http`, single module | Minimal infrastructure for M0–M3 |
| D08 | Proposed | SQLite for single-node state | Self-hosted deployment without separate database |
| D09 | Proposed | `Head / Body / Complete` as stream contract | Transport semantics without LLM event model |
| D10 | Proposed | gRPC over Unix socket for IPC | Validate overhead and host-service lifecycle in M6 |
| D11 | Accepted | Model rewrite is not a native passthrough | Body modification must be explicit |
| D12 | Proposed | CLI for initial admin operations | Account/key management without UI milestone |

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
| Is a Responses → Chat translator needed in the first local connector? | M5 | Validate capabilities of specific Ollama/vLLM versions |
| What exactly does the ACP connector execute? | Before ACP implementation | Choose agent process, transport, and tool loop owner |
| How does an external connector call host runtime services? | M6 | Prototype scoped bidirectional lifecycle and cancellation |

## Format of Future ADRs

For decisions with multiple serious alternatives, create a short ADR: context, chosen option, rationale, consequences, and review criteria. There's no need to create a separate ADR for each library; it's more important to record decisions that change package boundaries, compatibility guarantees, or persistent data formats.

Until such decisions emerge, this registry remains the single entry point. It helps distinguish promised system properties from hypotheses that still need validation.
