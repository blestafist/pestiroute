# M4 Anthropic Translation Planning Brief

This is a planning input for [M4](ROADMAP.md#m4--first-translation-connector),
not an accepted new contract or an implemented Connector. The roadmap order is
unchanged: Anthropic translation in M4, Codex and other agent protocols in M5.1.
The supplied Codex research informs that later milestone; its subscription auth,
Responses dialect and inclusive usage rules do not specify Anthropic Messages.

## Outcome and boundaries

Deliver an explicit `openai.responses.v1` to Anthropic Messages API Connector,
returning Responses-compatible output through the existing Adapter. Begin with
plain text and incremental streaming; add ordinary function tools, choice,
parallel calls, multiple rounds, normalized usage and the reasoning behavior
that can actually be represented. Publish a scoped compatibility matrix with
supported/unsupported/unknown claims and their limits.

Use the existing v1 `Connector`, `AttemptScope`, `InvocationServices` and
Head/Body/Complete/EOF. Translation, Messages JSON, SSE, provider errors and
token estimation stay private to the Connector. Core may enable the already
specified generic translation mode; it must not acquire Anthropic branches or
protocol structs. Runtime-owned stored credentials supply the selected account's
API key. The gateway forwards function calls/results and never executes tools.

The initial candidate transport is direct Anthropic HTTP/SSE with an API key,
one configured account/model per instance, and client-supplied full history.
M4-001 fixes the exact model, API version, client baseline and first-slice policy
from sources in the [M4-001 binding](M4-BINDING.md). No live compatibility has
been established by this plan; later task cards use that binding for the accepted
plain-text baseline and retain their own feature-specific decision gates.

Codex/Claude Code OAuth, Bedrock/Vertex, a northbound Messages or Chat adapter,
WebSocket, response resource storage, background jobs, sessions, compaction,
hosted tools, vision, audio, IPC and model discovery are outside this milestone's
first profile. A non-streaming client request needs an explicit supported policy
or a local rejection; do not silently force streaming. Keep unsupported work at
roadmap level rather than pre-create more implementation cards.

## Inspected implementation seams

Planning baseline: `dev-m4` at `d411798bbd79fa9bdb0155a6e00bbb418afc007a`
(merged M3, schema v6). These are observed restrictions, not missing v1 semantics:

| Seam | Current behavior | Bounded owner |
| --- | --- | --- |
| `internal/core/routes.go` | Route construction, selection and candidates accept only native mode | M4-003 enables generic translation mode |
| `internal/adapter/responses/component.go` | Client-format scope and capabilities were verified for the native path | M4-004 checks mode-independent format eligibility |
| `internal/connector/responses/component.go` | Native lifecycle, scope guards and conservative support operations | M4-005 adds a separate implementation |
| `cmd/gateway/yaml_config.go` | One native settings shape and implementation/mode allowlist | M4-014 adds a strict connector-owned translation shape |
| `cmd/gateway/protected_startup.go`, `main.go` | Native construction and HTTP service composition | M4-015 wires the new instance at the composition root |
| `internal/conformance/*_test.go` | Native/scripted fixtures, several native-mode assumptions | M4-033 exercises translation obligations without weakening native opacity |

Do not remove native `credential_env` validation globally or widen legacy JSON
by accident. A new stored-API-key profile can use its own non-secret settings;
M4-014/015 must preserve existing startup behavior and encrypted SQLite ownership.
Inspect current code again before changing it; package and test names below are
candidates until prerequisite work establishes them.

## Request policy decisions

M4-001 fixes only the plain-text streaming binding and records unresolved later
features. M4-007/008 implement its client-field policy. M4-017/018/019 extend it
for tools; M4-029 fixes reasoning representability separately. A field matrix
must distinguish omission, explicit defaults, invalid values, unsupported
semantics and verified mappings. Unknown fields in translation are not an excuse
to discard meaningful semantics; unknown native fields still survive unchanged.

Specify model identity/explicit rewrite, `stream`, output budget, `instructions`,
string/item `input`, roles, `store`, `previous_response_id`, `conversation`,
`background`, include requests, structured output and generation controls. Only
admit an instruction/history shape whose chronology and priority can be kept;
reject a late system/developer update if the selected Messages profile cannot
represent it. Never hoist it ahead of prior user/assistant messages silently.

Choose how an omitted output budget supplies required backend `max_tokens`, and
how the admission reservation matches the actual sent limit. Preserve an explicit
client bound or reject it. Do not silently drop strict-schema guarantees, force
parallelism off, inject agent instructions or infer model features from a name.

## Stream, completion and resource policy

Build a Connector-local Messages parser and Responses emitter. Preserve content
block order and separate response IDs, output item IDs and function call IDs.
Define created/in-progress, item/content starts, deltas, done snapshots and one
terminal outcome. `tool_use` finishes one model response while the client owns
the next tool round. A valid message stop and stop reason are required for success;
max-token stops, streaming errors, missing stop and malformed lifecycle cannot
be promoted to success from HTTP 200.

Use bounded event/line bytes, block count, argument bytes, retained text/tool
snapshots and total retained state. Responses done/terminal snapshots may require
retaining emitted content: fix a finite budget and exhaustion behavior in the
binding, stream deltas immediately, and never retain an unbounded response or
delay forwarding until completion. Backpressure remains coupled to upstream
reads; no background unbounded tee. Deadlines, Close, early abandonment and
shutdown release all resources.

Unknown harmless provider notifications may be ignored under a documented
policy; unknown semantic content cannot be translated by pretending it is text.
Provider SSE bytes are not Responses bytes and must not escape untranslated.
After Head commit, emit only a permitted Responses terminal failure or close;
no replacement HTTP response, hidden resend or fallback.

## Usage and retry decisions

Anthropic cache counters differ from Responses totals. M4-023 verifies the
selected profile's input/cache components, cumulative stream snapshots, inclusive
output and any reported thinking subset. Missing counters stay unknown, zeros
stay known, invalid/overflowing counters fail conservatively. Do not add repeated
message-delta totals together. Client-visible usage and Complete usage must agree;
cache-write details must not create a new public Core field without an ADR.

M4-009 reuses the M3 known/unknown estimate and configured-budget admission path.
Do not invent an exact local tokenizer or a zero estimate. A provider count-token
call is a separate future choice, not an automatic dependency for every request.
M4-028 verifies durable settlement, interrupted estimates and restart behavior.

M4-010/024 classify errors; M4-027 proves retry boundaries using the actual
Connector. Validation failures may be conclusively unsent but non-retryable.
HTTP 429/5xx alone does not prove replay safety. Ambiguous delivery and committed
failure never permit automatic replay. Signed/opaque provider state has no proven
cross-account portability; apply existing conservative affinity rules and let the
Adapter supply protocol metadata if an accepted reasoning policy requires it.

## Reasoning decision gate

M4-029 decides whether the selected Responses client can return/replay Anthropic
thinking, signature and redacted state without loss, false encryption claims or
new Core abstractions. Check request controls and thinking/tool-choice constraints
for the exact model/version. Never label a signature as OpenAI ciphertext merely
because both are opaque. Never flatten thinking into ordinary assistant text.

M4-030/031 implement and verify that decision. If a reversible representation
needs an extension, use the ADR/specification process before implementation and
split additional work if it cannot fit one card. Otherwise declare the scope
unsupported/unknown and reject affected input locally; do not claim full reasoning
support or make lossy replay a milestone success. The final matrix records the
actual result rather than treating these candidates as accepted support.

## Evidence and execution order

The [M4 task registry](TASKS.md#m4--first-translation-connector) alone owns task
states/dependencies. One READY card per worker session; dependencies are reading
context, not permission to implement a chain. Only M4-001 is initially executable.
The Planner refreshes dependent DRAFT cards from actual code/results and inserts
exact package/test commands before promotion. Do not load the whole milestone or
the full Codex research into every worker's context.

| Review gate | Evidence owners |
| --- | --- |
| First plain-text vertical slice | M4-001 through M4-016 |
| Function schema/choice, distinct calls, multiple rounds | M4-017 through M4-022 |
| Correct usage, errors, cancellation, limits and durable accounting | M4-023 through M4-028 |
| Honest reasoning behavior and scoped matrix | M4-029 through M4-032 |
| Common conformance, native regression and runnable operations | M4-033, M4-034 |
| Reproducible live procedure, capture and comparison | M4-035 through M4-037 |
| Final acceptance-to-evidence audit and M5.1 handoff | M4-038 |

Regular checks use synthetic local upstreams and no account credentials. The live
gate is separate, bounded and uses explicitly available account access; lack of
access blocks that capture, not local development. Record actual client/model/API
versions and source revisions. A single response choosing one tool is not proof
that parallel requests were altered. Missing live evidence remains unknown.

## Source selection

Official documentation inspected on 2026-10-03. These mutable links are research
entry points, not immutable fixtures or proof of live PestiRoute compatibility.
M4-001/002 must record exact profile values, provenance and fixture hashes; any
copied SDK/source fixture requires its pinned revision and license attribution.

| Source | What to verify |
| --- | --- |
| [Messages create](https://platform.claude.com/docs/en/api/messages/create) | Request fields, roles, tools and model constraints |
| [Streaming](https://platform.claude.com/docs/en/build-with-claude/streaming) | Block lifecycle, delta families, cumulative usage, error/ping events |
| [Define tools](https://platform.claude.com/docs/en/agents-and-tools/tool-use/define-tools) | Function schemas and tool metadata |
| [Handle calls](https://platform.claude.com/docs/en/agents-and-tools/tool-use/handle-tool-calls) | Call/result linkage and full-history replay |
| [Parallel calls](https://platform.claude.com/docs/en/agents-and-tools/tool-use/parallel-tool-use) | Choice/parallel controls and grouped tool results |
| [Prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching) | Input/cache accounting components |
| [Thinking](https://platform.claude.com/docs/en/build-with-claude/thinking) | Model-specific controls and opaque replay state |
| [Errors](https://platform.claude.com/docs/en/api/errors) | HTTP/SSE errors and request diagnostics |
| [Responses streaming](https://developers.openai.com/api/docs/guides/streaming-responses) | Client-facing event lifecycle |

No new MCP or framework is needed for this plan. See the [M4 tooling gate](TOOLING.md#m4-tooling-gate).
