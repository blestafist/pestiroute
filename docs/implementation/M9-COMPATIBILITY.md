# M9 Chat Completions Compatibility

This matrix records the gateway's admitted native subset and deterministic
local evidence. It is not a claim that an arbitrary OpenAI-compatible client,
provider, model, or account supports these features. No real provider/client
versioned smoke was run for M9; provider-specific support remains unverified.

## Profile

| Item | Binding |
| --- | --- |
| Client protocol / mode | `openai.chat-completions.v1` / `native` |
| Endpoint | `POST /v1/chat/completions` |
| Adapter and Connector | `pestiroute.chat-completions.native` (separate component-kind instances) |
| Backend | Native Chat Completions JSON/SSE; original request body forwarded unchanged |
| Scope | Exact protocol, native mode, configured model, and selected account; no provider-name inference or Responses fallback |
| Local baseline | Go gateway tests and loopback fake backend; no external client library version applies |

The API binding is deliberately narrower than every public Chat field. See the
[accepted field and framing contract](M9-BINDING.md#decode-admitted-shapes-and-capability-requirements).
Recognized request fields are validated and capability-gated there; other
fields remain opaque pass-through rather than proof of backend acceptance.

## Scoped CONTRACT capability matrix

Only a `supported` declaration at the exact `(protocol, mode, model, account)`
scope satisfies a request. The Connector returns its configured declarations
only at that scope; absent declarations are `unknown`. Protected YAML accepts
optional explicit `capabilities` entries with values `supported`,
`unsupported`, or `unknown`; omission remains `unknown`. Rows marked “local
supported” describe only the synthetic local fixture configuration/proof, not
an unconditional default or provider guarantee.

| CONTRACT capability | Local result / declaration | Evidence and ceiling |
| --- | --- | --- |
| `llm.streaming` | `supported` explicitly declared for the synthetic CLI fixture; undeclared remains `unknown` | `LOCAL-M9` proves protected YAML startup and successful fake SSE with usage and `[DONE]`; `TestChatProtectedGateway_SSE` proves early delivery. No provider claim. |
| `llm.tools` | `supported` explicitly declared for the synthetic CLI fixture; undeclared remains `unknown` | `LOCAL-M9` proves fake tool-call JSON pass-through; `TestChatAgentWorkflow_MultiRoundTool` and `TestChatAgentWorkflow_ToolChoice` exercise workflows. This proves gateway pass-through, not tool execution by gateway or upstream-model support. |
| `llm.tools.parallel` | `supported` explicitly declared for the synthetic CLI fixture; undeclared remains `unknown` | `TestChatAgentWorkflow_ParallelToolCalls` proves supported and absent/unknown pre-dispatch behavior. Does not claim provider parallel execution semantics. |
| `llm.structured_output` | `supported` explicitly declared for the synthetic CLI fixture; undeclared remains `unknown` | `TestChatAgentWorkflow_ResponseFormat` checks `text`, `json_object`, and `json_schema` forwarding and gates JSON modes before dispatch. Schema enforcement by a backend is not proven. |
| `llm.reasoning` | `unknown` (omitted from synthetic YAML); no support claim | `LOCAL-M9` proves a `reasoning_effort` request is rejected before fake-backend dispatch. |
| `llm.vision` | `unknown` unless explicitly configured; no support claim | Image inputs are validated and require this capability; no M9 provider/model proof. |
| `llm.audio` | `unknown` unless explicitly configured; no support claim | Audio representations are validated and require this capability; no M9 provider/model proof. |
| `auth.oauth` | `unknown`; no support claim | Native Connector uses existing scoped bearer credentials; OAuth support is not implemented or implied. |
| `usage.exact` | `unknown`; no support claim | Valid Chat usage fields are mapped; missing/invalid values stay unknown. Local fixtures prove extraction/settlement only for supplied counters, not universal exactness. |

## Admitted subset and fail-closed behavior

The adapter admits the request/message/tool/format shapes documented in the
[binding](M9-BINDING.md#decode-admitted-shapes-and-capability-requirements),
including non-empty model/messages, native function tools and history, the
declared response formats, and JSON/SSE responses. It retains original payload
bytes. `stream:true`, tool/function calls, parallel tool calls, structured
output, reasoning, vision, and audio each require their corresponding scoped
capability as specified by that binding. Missing and `unknown` do not pass.
YAML can explicitly declare capability states at the configured Connector scope;
it does not infer support from protocol, endpoint, model, or provider name. See
the [YAML binding](M9-BINDING.md#protected-yaml-and-admin-behavior).

Malformed known fields, duplicate JSON keys, unsupported/custom tool
representations, file parts, `web_search_options`, unknown tool-choice forms,
and unsupported response formats reject before upstream dispatch. The full
field-specific rules and HTTP/lifecycle error handling remain in the binding;
this summary does not introduce another contract.

## Evidence boundary and handoff

Deterministic evidence is in M9-002 through M9-004 task cards and gateway
`TestChat*`, Chat adapter/Connector package race tests, and conformance tests
(the filtered `-run TestChat` command selects four tests in conformance; the
adapter and Connector packages have no matching test names). The runnable
[LOCAL-M9](LOCAL-M9.md) procedure provisions synthetic SQLite state, exercises
JSON, SSE, tools, and a pre-dispatch denial against a loopback fake, and checks
accounting and shutdown. This is offline compatibility evidence only. No
OpenAI or third-party client version was used, and no real-provider request,
credential, endpoint, or live capability is claimed. Any real-provider/client
smoke requires its own scope, version record, authority, and task.

M9 phase 5 hands off the compatibility subset and reproducible local procedure;
it does not authorize cross-protocol translation, live gates, or production
claims beyond recorded evidence.
