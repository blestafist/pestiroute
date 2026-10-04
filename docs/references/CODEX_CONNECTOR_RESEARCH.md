# Codex connector research for PestiRoute

Research date: 2026-10-02. Status: source-backed research and implementation proposal, not an accepted ADR or an implemented connector.

## 1. Recommendation and evidence boundary

Implement a dedicated **Agent Protocol Connector** that talks directly to the ChatGPT Codex backend. Start with streamed OpenAI Responses requests over HTTP/SSE, OAuth device login, runtime-owned credentials and refresh, and connector-local adaptation of the Responses dialect. No Codex CLI process, OpenCode process, tool execution loop, universal message model, or new provider-specific Core API is necessary for that slice.

OpenCode v2 is a useful protocol reference, but its application architecture is not the architecture to transplant. It constructs model requests from its own conversation and tool abstractions, manages session history, and executes tools. PestiRoute receives an already-formed client request and forwards or explicitly adapts it.

**The central compatibility requirements are more than an endpoint and token:** subscription OAuth; the selected ChatGPT account header; correct Responses input and tool items; `store:false`; streamed output; preservation of encrypted reasoning and item identities across turns; backend-compatible generation options; and reliable classification of terminal events and ambiguous delivery.

Evidence labels used here:

- **Observed in source:** directly read at a pinned revision.
- **Proposal:** how that behavior should fit PestiRoute.
- **Unverified:** requires a deterministic test or an authorized real-provider smoke test.

No real account login, inference, quota probe, or upstream smoke test was performed. Existing tests were inspected, not executed. The supplied PestiRoute documents are the normative project input; the live PestiRoute implementation and CURRENT/TASKS were not audited in this research.

### Pinned sources

| Source | Revision | Role |
| --- | --- | --- |
| OpenCode, branch `v2` | `0bef0eab678e94a389930a2a1365115fd5a35c69` | Main research target; actual v2 implementation |
| OpenCode, branch `dev` | `c42ae0d56b6f86f8df39d451d6d2cfe6414b3928` | Comparison only; contains the older fetch-intercepting Codex plugin |
| Official OpenAI Codex, branch `main` | `c5d242fa7907bff1b7a7e26e95febc548c0a6963` | Independent check of backend request, auth, usage, and model-specific behavior |

Do not substitute `dev/packages/opencode/src/plugin/openai/codex.ts` for the v2 implementation. In the inspected v2 tree, the important paths are `packages/core/src/plugin/provider/openai.ts` and `packages/ai/src/protocols/*`. The older `packages/llm` paths are not the v2 paths. References below use immutable commit links.

## 2. Code navigation map

| Read order | File and symbol | What it establishes |
| --- | --- | --- |
| 1 | [V2-OAUTH], `OpenAIPlugin`, `browser`, `headless`, `refresh` | OAuth exchanges, backend selection, subscription model filtering, request hooks |
| 2 | [V2-INTEGRATION], `connection.resolve` | Credential resolution, proactive expiry check, calling provider refresh, persistence |
| 3 | [V2-RESOLVER], `resolveCatalogModel`, `nativeCredentialSettings` | How an OAuth access token reaches the model's bearer authentication |
| 4 | [V2-REQUEST], `SessionModelRequest.prepare` | System/history/tools, cache affinity, request hooks, transport binding |
| 5 | [V2-PROVIDER], `configure`, `model`; [V2-DEFAULTS] | Responses selection and provider defaults |
| 6 | [V2-RESPONSES], `fromRequest`, `protocol`, `route` | OpenAI-specific schema, namespaces, effort updates, HTTP and WebSocket transport |
| 7 | [V2-OPENRESPONSES], `lowerConversation`, `lowerMessages`, `lowerGeneration`, `step`, `mapUsage` | Actual wire fields, input replay, tools, reasoning, event lifecycle and usage |
| 8 | [V2-HTTP], `jsonRequestParts`, `httpJson` | Endpoint construction, overlays, auth, HTTP streaming timeouts |
| 9 | [V2-CHANNEL], `message`, `driver`, `transport` | Responses WebSocket messages and event validation |
| 10 | [V2-CONTINUATION], `incremental`, `driver` | Full versus incremental requests, previous response IDs and explicit rejections |
| 11 | [V2-SESSIONTRANSPORT], `start`, `bind`, `affinity` | Socket ownership, serialization, delivery states, fallback and checkpoint commitment |
| 12 | [CX-REQUEST], `ResponsesApiRequest`; [CX-CLIENT], `build_responses_request` | Official-client dialect and Responses Lite differences |

The v2 path can be followed in four stages:

1. `OpenAIPlugin` registers ChatGPT OAuth methods and changes the selected OpenAI provider to the Codex base URL when its active credential is a matching ChatGPT OAuth credential.
2. The integration runtime resolves that credential and refreshes it when needed. The model resolver supplies its access token as the provider's `apiKey` setting; the provider authentication implementation turns that setting into a bearer header.
3. Session request preparation creates a canonical LLM request, runs the request hooks, and the Responses protocol lowers it into backend JSON.
4. The selected transport sends HTTP/SSE or a Responses WebSocket exchange. Protocol parsing turns events into OpenCode's own LLM events, which its runner uses for history and tools.

For PestiRoute, stage 3 is much smaller: the incoming payload is already Responses JSON. Stage 4 should return client-compatible bytes plus separate control/usage metadata, rather than OpenCode's semantic LLM events.

## 3. OAuth in OpenCode v2

### 3.1 Provider constants

Observed in [V2-OAUTH], lines 15–24:

| Value | Exact inspected value |
| --- | --- |
| OAuth issuer | `https://auth.openai.com` |
| OAuth client ID | `app_EMoamEEZ73f0CkXaXp7hrann` |
| Codex base URL | `https://chatgpt.com/backend-api/codex` |
| Browser callback ports | `1455`, fallback `1457` |
| Integration method IDs | `chatgpt-browser`, `chatgpt-headless` |
| Device polling safety margin | 3,000 ms |

This client ID is present in public client code. It is not a client secret. Its continued suitability for a separate PestiRoute integration is a live compatibility question, not guaranteed by the source snapshot. Keep auth behavior behind a versioned connector profile.

### 3.2 Browser authorization-code flow with PKCE

`browser(app)` in [V2-OAUTH], lines 52–109:

1. Generate a PKCE verifier and S256 challenge.
2. Generate a random state.
3. Start a local HTTP callback server.
4. Return the authorization URL and an asynchronous callback operation.
5. Validate the callback code and state.
6. Exchange the code for tokens.
7. Construct a credential; the integration layer owns persistence.

The authorization URL is assembled by `authorizeURL`, lines 401–414:

```text
GET https://auth.openai.com/oauth/authorize
    ?response_type=code
    &client_id=app_EMoamEEZ73f0CkXaXp7hrann
    &redirect_uri=http://localhost:<port>/auth/callback
    &scope=openid profile email offline_access
    &code_challenge=<S256 challenge>
    &code_challenge_method=S256
    &id_token_add_organizations=true
    &codex_cli_simplified_flow=true
    &state=<random state>
    &originator=opencode
```

The exchange in `exchange`, lines 336–347, is:

```http
POST /oauth/token
Host: auth.openai.com
Content-Type: application/x-www-form-urlencoded

grant_type=authorization_code&code=<code>&redirect_uri=<exact callback URI>&client_id=<client ID>&code_verifier=<verifier>
```

The callback server binds to `localhost`. On an occupied port, v2 attempts to cancel an older listener, retries port 1455, and then tries 1457. This is desktop-client convenience, not a gateway requirement. PestiRoute should not automatically cancel an unrelated process on a local port.

For a remote self-hosted gateway, the browser's `localhost` is the user's machine, not the gateway host. A loopback browser flow needs a local CLI or an explicitly designed callback handoff. Device login is therefore the better initial gateway flow.

PKCE generation in OpenCode uses 43 randomly selected allowed characters and an SHA-256 challenge. A Go implementation can use cryptographic random bytes encoded as base64url; exact reproduction of its modulo-based character selection is unnecessary. State and verifier belong to PestiRoute's auth-session runtime, not connector-global variables.

### 3.3 Headless device flow

`headless(app)`, lines 171–230, performs a provider-specific flow:

**Start**

```http
POST /api/accounts/deviceauth/usercode
Host: auth.openai.com
Content-Type: application/json

{"client_id":"app_EMoamEEZ73f0CkXaXp7hrann"}
```

Expected source-level response fields:

```json
{
  "device_auth_id": "<opaque>",
  "user_code": "<display to user>",
  "interval": "5"
}
```

The user visits `https://auth.openai.com/codex/device` and enters the code.

**Poll**

```http
POST /api/accounts/deviceauth/token
Host: auth.openai.com
Content-Type: application/json

{"device_auth_id":"<opaque>","user_code":"<displayed code>"}
```

In the inspected implementation:

- A successful poll returns `authorization_code` and `code_verifier`.
- HTTP 403 and 404 are treated as pending during polling.
- Other unsuccessful statuses fail the operation.
- The interval is parsed as a string, defaults to 5 seconds, is clamped to at least 1 second, and receives an extra 3-second safety margin.
- The returned authorization code is exchanged at `/oauth/token`, using `redirect_uri=https://auth.openai.com/deviceauth/callback`.

This is not a drop-in RFC 8628 `device_code` grant exchange. The provider first returns an authorization code and verifier, then the client performs an authorization-code exchange.

The plugin's polling loop itself is open-ended. The v2 integration layer gives OAuth attempts a default 10-minute lifetime ([V2-INTEGRATION], `attemptLifetime` and attempt lifecycle). The official Codex device implementation explicitly uses a 15-minute maximum wait ([CX-DEVICE], `poll_for_token`). PestiRoute should impose a runtime-owned deadline and cancellation; do not copy an infinite loop.

Do not generalize the polling treatment of 403/404 to inference endpoints: there it may mean permission denial or missing model.

### 3.4 Credential content and account identity

`credential`, lines 378–387, stores:

```text
type      = oauth
methodID  = chatgpt-browser | chatgpt-headless
refresh   = tokens.refresh_token
access    = tokens.access_token
expires   = now + (expires_in or 3600) seconds
metadata.accountID = extracted account, when present
```

`extractAccountID` and `claim`, lines 416–429, first inspect the ID token, then the access token. Within a parsed JWT payload, precedence is:

1. `chatgpt_account_id`;
2. `https://api.openai.com/auth.chatgpt_account_id`;
3. `organizations[0].id`.

The JWT payload is decoded here, not cryptographically verified by these helper functions. Treat the result as metadata obtained from the trusted token exchange, not as an authorization proof accepted from arbitrary client input. A gateway's selected account must never be chosen by an inbound account header.

The organization fallback is an observed client heuristic. Verify it for the accounts PestiRoute actually supports; do not silently select a different organization when identity is ambiguous.

### 3.5 Refresh and persistence

`refresh`, lines 350–364:

```http
POST /oauth/token
Host: auth.openai.com
Content-Type: application/x-www-form-urlencoded

grant_type=refresh_token&refresh_token=<current refresh token>&client_id=<client ID>
```

V2's `Integration.connection.resolve`, lines 697–714:

1. Load the selected credential.
2. Resolve its registered refresh implementation.
3. If expiry is more than 5 minutes away, return it unchanged.
4. Otherwise call the provider's refresh implementation.
5. Persist the returned credential through the credential service.
6. Return the refreshed value.

The refresh helper preserves existing metadata when the new credential has none. The `loading` semaphore in `OpenAIPlugin` serializes credential-switch reload work; it is not evidence of account-scoped refresh singleflight. The inspected `connection.resolve` block does not show a refresh lock. Do not assume concurrent refresh safety merely because the application has a semaphore elsewhere.

**PestiRoute requirement:** serialize refresh per selected account, reload the credential after acquiring the lock, persist token rotation atomically, and return the persisted generation. The connector performs the provider exchange through `Authenticate(refresh)`; the runtime owns the lock, session state, storage and lifetime.

A refresh response missing a rotated refresh token should not cause accidental credential loss. Validate actual provider behavior and define an explicit retain-versus-reject policy. OpenCode's source types assume `refresh_token` is present.

If exchange succeeds but storage fails, report persistence failure distinctly. A caller must not receive a success result that falsely claims the rotated credential survived restart.

## 4. Backend endpoint and headers

### 4.1 How v2 selects the subscription backend

`OpenAIPlugin.load`, lines 239–249, accepts the active OpenAI OAuth credential only when its method ID matches the ChatGPT browser or headless method.

The provider transform, lines 256–271, sets:

- The default transport to `websocket`, unless a provider transport setting already exists.
- The Codex base URL when a ChatGPT credential is active.
- `originator: opencode`.
- `x-codex-beta-features: remote_compaction_v2`.
- `chatgpt-account-id` from credential metadata, when available.

The session `model.request` hook, lines 300–315, also redirects an OpenAI API origin to the Codex base URL and sets `session-id` using session affinity. A custom proxy origin is preserved by that hook. [V2-OAUTH-TEST] explicitly checks custom-provider and proxy isolation.

The Responses endpoint appends `/responses` to the base URL. Thus the normal HTTP request is:

```text
POST https://chatgpt.com/backend-api/codex/responses
```

The official Codex provider configuration independently defines the same Codex base URL ([CX-PROVIDER]).

### 4.2 Where bearer auth is added

`ModelResolver.nativeCredentialSettings`, lines 308–318, supplies an OAuth access token as `apiKey` for the native OpenAI provider.

`OpenAI.configure` selects bearer auth through `AuthOptions.bearer` ([V2-PROVIDER], [V2-AUTH]). `HttpTransport.jsonRequestParts` applies route auth after constructing the URL, body, and headers ([V2-HTTP], lines 56–72). The wire header is:

```text
Authorization: Bearer <OAuth access token>
```

The field name `apiKey` in OpenCode's provider setting does **not** mean a platform API key is used for this flow. It holds the OAuth bearer token.

### 4.3 Header ownership for PestiRoute

| Header | Evidence and proposed treatment |
| --- | --- |
| `Authorization` | Build from scoped runtime credentials; never forward the client's virtual key |
| `ChatGPT-Account-Id` | Use the runtime-selected account metadata; never trust an inbound value |
| `Content-Type: application/json` | Generate for the backend JSON request |
| `Accept: text/event-stream` | Official HTTP Responses client sets it ([CX-HTTP]); appropriate for the SSE slice |
| `originator` | V2 uses `opencode`; whether another identity is accepted needs a smoke test |
| `User-Agent` | V2 request preparation supplies its application user agent; use a documented, versioned PestiRoute profile |
| `session-id` | V2 uses it for cache routing; accept only a bounded, permitted affinity value or generate a stable connector-local mapping |
| `OpenAI-Beta: responses_websockets=2026-02-06` | WebSocket protocol negotiation; not required for the initial HTTP path |
| `x-codex-beta-features: remote_compaction_v2` | V2 advertises a feature it implements; do not advertise native compaction in an initial connector that does not implement it |
| `x-openai-internal-codex-residency` | Present in the older `dev` plugin, derived from token claims; not present in the inspected v2 OpenAI plugin. Treat as a separate account-specific compatibility investigation |
| Hop-by-hop, original length, arbitrary account/project/auth headers | Filter under the gateway transport policy; recalculate body length after transformation |

Do not copy every OpenCode telemetry header into the upstream. An exact compatibility profile can preserve justified client identity headers, but gateway authorization and credential selection remain independent of those values.

### 4.4 Cache affinity is not response-resource affinity

[V2-AFFINITY] uses:

```ts
session.parentID ?? session.fork?.sessionID ?? session.id
```

The v2 request builder uses that affinity for headers and `prompt_cache_key` (and strips a specific `ses_` prefix for its cache key). This helps keep cache prefixes consistent across related work.

PestiRoute should not invent a new cache key for every attempt or derive it from the gateway's attempt ID. Preserve a client-provided valid key or establish a documented mapping scoped to account and connector. Stable instructions and tools matter too; a matching key does not compensate for changing prompt prefixes.

A cache key does not make `previous_response_id` portable across accounts, providers, or deployments.


## 5. Wire request construction

### 5.1 Actual v2 lowering

[V2-REQUEST] builds a request with system text, chronological messages, tools, tool choice, generation options, provider options, and a prompt cache key. Its OpenAI hook removes the default output limit from context and compaction requests.

[V2-RESPONSES], `fromRequest`, delegates conversation and generation fields to [V2-OPENRESPONSES]:

```ts
// OpenResponses.lowerConversation
const instructions = ProviderShared.joinText(request.system)
return {
  model: request.model.id,
  input: yield* lowerMessages(request, adapter),
  ...(instructions ? { instructions } : {}),
}
```

Generation lowering includes `stream:true`, optional generation controls, `store`, `prompt_cache_key`, `include`, `reasoning`, `text`, `service_tier`, `parallel_tool_calls` and other Responses options. V2's route defaults include:

```ts
defaults: {
  providerOptions: {
    store: false,
    include: ["reasoning.encrypted_content"],
  },
}
```

The provider facade supplies model-family defaults such as medium effort and automatic reasoning summary for eligible GPT-5 models ([V2-DEFAULTS]). These are application defaults, not evidence that every Codex model accepts the same values.

### 5.2 Field-by-field implications

| Wire field | Observed behavior | PestiRoute proposal |
| --- | --- | --- |
| `model` | Selected backend model ID | Route by original model; rewrite aliases only in explicit connector translation mode |
| `input` | Chronological Responses items | Preserve items, extensions, IDs and order; no shared message conversion |
| `instructions` | Initial system content becomes a top-level string | Preserve supplied instructions; if adapting system items, use a documented order-preserving rule |
| `stream` | V2 sends true | Initial slice accepts explicit streamed requests; reject unsupported non-streaming semantics locally |
| `store` | Defaults to false; official Codex also sends false | Default omission to false in translation mode; reject explicit true if unsupported |
| `include` | Includes encrypted reasoning by default | Preserve requested inclusions and optionally add encrypted reasoning under the declared profile |
| `reasoning.effort` | Model options and variants | Preserve explicit values; validate by model profile, not a global hardcoded enum |
| `reasoning.summary` | Defaults depend on provider/model settings | Preserve supported explicit values; omit unsupported optional defaults |
| `text.verbosity` | Optional provider option | Capability/model dependent |
| `parallel_tool_calls` | Explicit provider option wins; otherwise derived from tool choice's disable-parallel flag; absent if unspecified | Never force false globally; preserve the client choice and verify the selected backend |
| `tools` | Plain functions or supported native tool entries/namespaces | First slice can restrict claims to ordinary function tools; preserve supported namespaces only after fixtures |
| `tool_choice` | auto, none, required, named function and allowed-tools forms | Support only fixture-verified forms; do not confuse a backend error with account unavailability |
| `max_output_tokens` | Plugin removes the app's default from primary/compaction | Backend compatibility policy must be explicit; never silently remove a client's meaningful bound |
| `temperature`, `top_p`, penalties | Protocol can encode them; backend acceptance is a different question | Reject unsupported explicit settings; do not infer acceptance from the encoder |
| `previous_response_id`, `conversation` | Stateful/continuation behavior exists in later transport code | Reject in the first stateless connector; preserve support only with account/deployment affinity |
| `background` | WebSocket message construction strips it | First gateway slice rejects requested background execution instead of silently downgrading it |
| `context_management`, `compaction_trigger` | V2 implements native compaction paths | Separate extension, not initial advertised support |

PestiRoute accepts raw Responses JSON, not OpenCode's `providerOptions` settings. For example, `reasoningEffort` is an OpenCode setting; `reasoning:{"effort":...}` is the wire representation. Do not add both to the upstream request.

### 5.3 Why removing max_output_tokens is not a harmless generic proxy change

[V2-OAUTH], lines 316–322, says the ChatGPT backend rejects a requested output limit and removes the application default:

```ts
const omitOutputLimit = (evt: SessionRequest) =>
  Effect.sync(() => {
    delete evt.options.maxTokens
  })
```

OpenCode controls that default and can choose not to send it. PestiRoute may receive an intentional output limit from another client. Silently dropping it changes semantics and can affect accounting.

Initial proposal:

- If the client omits the field, omit it upstream.
- If the selected backend/profile cannot honor an explicit limit, return `unsupported_feature` before delivery.
- An optional operator-enabled rewrite policy may explicitly permit omission, but it must be documented as a behavior change and must not masquerade as native mode.

The source comment is evidence of the client's current compatibility policy; a live smoke test is still needed to establish exactly which models reject the field today.

### 5.4 Unknown fields and execution modes

Use two deliberately different modes:

**Native dialect pass-through:** a request already satisfies the exact validated Codex dialect; body bytes remain unchanged. Auth and transport headers can change. Native mode cannot insert `store:false`, remove generation fields, or replace the model.

**Explicit Responses-to-Codex adaptation:** the connector creates a new backend body while keeping the admitted request immutable. A connector-private `map[string]json.RawMessage` or targeted JSON editing can preserve unknown field values without inventing a universal message representation. Byte-for-byte preservation is not claimed for transformed JSON.

Classify fields as known-compatible, known-unsupported-with-significant-semantics, or unverified. Preserve optional extensions when the profile permits that; reject required unknown semantics. A strict Go struct containing only a familiar subset is unsuitable because re-encoding it drops extensions.

Also define how duplicate JSON keys are handled. A plain map decoder accepts last-wins behavior; the adapter and connector must not interpret the same payload differently. Reject ambiguous keys or use a single established boundary rule.

### 5.5 Synthetic first-turn request

This is an illustrative request shape assembled from source, not a captured successful Codex call. `<verified-model>` must be replaced with a model authorized for the selected account.

```json
{
  "model": "<verified-model>",
  "instructions": "Use the available tools when needed.",
  "input": [
    {
      "type": "message",
      "role": "user",
      "content": [
        {"type": "input_text", "text": "Read the project configuration."}
      ]
    }
  ],
  "tools": [
    {
      "type": "function",
      "name": "read_file",
      "description": "Read a file from the client workspace.",
      "parameters": {
        "type": "object",
        "properties": {"path": {"type": "string"}},
        "required": ["path"],
        "additionalProperties": false
      },
      "strict": false
    }
  ],
  "tool_choice": "auto",
  "parallel_tool_calls": true,
  "reasoning": {"effort": "medium", "summary": "auto"},
  "include": ["reasoning.encrypted_content"],
  "prompt_cache_key": "example-project-session",
  "store": false,
  "stream": true
}
```

The gateway forwards the tool call to the client. It does not open a file or invoke an MCP server.

## 6. Tools, history, and encrypted reasoning

### 6.1 Three identities that must remain distinct

A function tool has a definition name. A generated function-call output item has an item ID. A call/result pair has a `call_id`. These are different identifiers.

Example output item:

```json
{
  "type": "function_call",
  "id": "fc_example",
  "call_id": "call_example",
  "name": "read_file",
  "arguments": "{\"path\":\"go.mod\"}"
}
```

Example client result:

```json
{
  "type": "function_call_output",
  "call_id": "call_example",
  "output": "module example.org/pestiroute"
}
```

[V2-OPENRESPONSES], `lowerToolCall` and tool-result lowering, preserve the call identity and encode results as `function_call_output`. The gateway must not replace `call_id` with `fc_example`, regenerate call IDs, collapse two parallel calls, or reorder their events.

`arguments` on the wire is a JSON string, not an object. Streaming argument deltas can split anywhere; an incomplete delta is not executable arguments.

### 6.2 Tool schemas and strictness

`OpenResponses.lowerTool`, lines 445–457, sets `strict:false` because OpenCode's common tool definition does not express Responses strict-schema policy. This permits flexible client/MCP schemas.

For PestiRoute, preserving an already-valid client's `strict:true` matters. Do not turn every client tool into `strict:false` simply because OpenCode chooses that default for its own tool abstraction.

[V2-RESPONSES] also supports native tool namespaces and flattens deeper nested namespace levels into leaf names. That is app-side tool lowering. A gateway receiving Responses-native namespaces should preserve their existing wire structure if its Codex profile supports them; it should not blindly repeat OpenCode's lowering.

Hosted tools, image generation, shell/custom tools, and namespaces are separate compatibility claims. Function tools working does not prove all these features work.

### 6.3 Initial instructions and chronological updates

V2 puts initial system content in top-level `instructions`. Later system messages in chronological history are lowered to developer messages at their actual positions ([V2-OPENRESPONSES], lines 592–619).

This distinction prevents a late instruction from being moved to the beginning of the conversation. A gateway must not collect every system/developer message from `input` into a new top-level instructions string. For arbitrary incoming Responses input, the minimal strategy is to preserve the client's existing arrangement, with only a precisely scoped compatibility transform when required.

Do not insert OpenCode's own coding system prompt. PestiRoute must preserve the client's agent behavior.

### 6.4 Why encrypted reasoning matters

`store:false` means a follow-up cannot generally assume the backend stores its prior response items as retrievable objects. The client sends the needed history again.

A Responses reasoning item can contain:

```json
{
  "type": "reasoning",
  "id": "rs_example",
  "summary": [{"type": "summary_text", "text": "I will inspect the configuration."}],
  "encrypted_content": "<opaque provider ciphertext>"
}
```

The readable summary is not the hidden reasoning state. V2 requests `reasoning.encrypted_content`, attaches the returned state to its internal reasoning metadata, and reconstructs reasoning items on the next turn. Multiple summary fragments belonging to one reasoning item are combined ([V2-OPENRESPONSES], `lowerReasoning`, `lowerMessages`, and reasoning event handling).

PestiRoute should preserve the entire already-formed reasoning item and its ciphertext. It must not decrypt it, turn it into ordinary assistant text, strip it, log it by default, or regenerate it from the summary. Cross-provider conversion cannot safely fabricate the hidden state.

There is a branch difference: the older dev native protocol drops summary-only reasoning replay when `store:false` and no encrypted state is available. The inspected v2 lowerer does not show that same final blanket filter. Do not apply the older filter to arbitrary client payloads without a verified compatibility rule.

### 6.5 Stateless second turn

A follow-up after the example tool call can include the original input, the reasoning output item, the function call, and its result:

```json
[
  {
    "type": "message",
    "role": "user",
    "content": [{"type": "input_text", "text": "Read the project configuration."}]
  },
  {
    "type": "reasoning",
    "id": "rs_example",
    "summary": [],
    "encrypted_content": "<opaque provider ciphertext>"
  },
  {
    "type": "function_call",
    "id": "fc_example",
    "call_id": "call_example",
    "name": "read_file",
    "arguments": "{\"path\":\"go.mod\"}"
  },
  {
    "type": "function_call_output",
    "call_id": "call_example",
    "output": "module example.org/pestiroute"
  }
]
```

This is also a synthetic shape. The actual client may send additional message phase, item metadata, namespace, or other fields. Preserve them under the selected profile.

The first connector should use this full-history approach. It avoids introducing response-resource storage and socket continuation state before a basic subscription path works.

## 7. Streaming and usage

### 7.1 Events that matter

[V2-OPENRESPONSES], its stream schema and `step`, recognize the Responses lifecycle. The important families include:

| Event family | Meaning for gateway implementation |
| --- | --- |
| `response.created`, `response.in_progress` | Response lifecycle, not proof of completion |
| `response.output_item.added` | Start of message, reasoning or tool output item |
| `response.content_part.added`, `response.output_text.delta`, done variants | Text/content stream; bytes and order must survive |
| `response.function_call_arguments.delta`, `.done` | Incremental and final tool argument data |
| `response.output_item.done` | Authoritative completed output item, including tool/reasoning metadata |
| Reasoning summary part/text events | Summary fragments and eventual encrypted-state attachment |
| `response.completed` | Successful terminal response and final usage |
| `response.incomplete` | Terminal partial result with an incomplete reason; not full success |
| `response.failed`, `error` | Terminal failure, even when transport status is 200 |
| `codex.rate_limits`, provider notifications/keepalives | Provider control information; do not mistake them for response completion |

The inspected v2 terminal set contains completed, incomplete, failed and error. The older dev WebSocket bridge additionally recognizes `response.done`; this is not evidence of v2 normalizing it to `response.completed`. Support an alias only after a versioned fixture shows the required shape.

Unknown provider events should normally remain visible in compatible byte forwarding. Unknown terminal semantics require conservative completion behavior; do not fabricate success.

### 7.2 OpenCode parsing versus gateway parsing

OpenCode creates semantic text, reasoning and tool events because it is an agent client. It also settles pending tool calls at terminal completion when a compatible backend omits an `output_item.done` event ([V2-OPENRESPONSES], `onResponseFinish`; [V2-RESPONSES-TEST]).

PestiRoute should not reconstruct all of OpenCode's semantic event machinery for a same-protocol connector. Its HTTP/SSE path needs a smaller parser for:

- terminal response outcome;
- final usage;
- provider errors/retry hints;
- versioned provider diagnostics.

Forward original response bytes while observing the stream. Do not rebuild the user-facing SSE from a narrow event struct: that loses unknown fields, IDs, content variants, reasoning metadata and future events.

If a backend genuinely requires response normalization, make that a connector-owned translation with its own fixtures. Missing `output_item.done` settlement in an agent client's history layer is not automatically a gateway normalization requirement.

### 7.3 PestiRoute frames

For a successful HTTP response:

```text
Head(protocol = admitted Responses protocol, status = upstream status, safe headers)
Body(original upstream byte chunks, incrementally)
Complete(outcome = succeeded, usage = normalized final report)
EOF
```

These are the existing contract frames. Do not add `TextChunk`, `ToolCallChunk`, a Codex execution method, or decoded reasoning to Core.

If HTTP status is unsuccessful, classify the bounded upstream error before exposing Head so that Core can decide whether a safe fallback is permitted. Its preserved client-visible error body remains separate from the infrastructure error.

An error discovered inside a 200 SSE stream after Head is committed ends the same attempt. It cannot trigger replacement JSON, changed status, or another account.

EOF without a recognized valid terminal event is incomplete, even if text or a function call was received. A failed underlying transport may prevent delivery of Complete; Core's exactly-once finalizer covers that case.

### 7.4 Usage extraction

[V2-OPENRESPONSES], `mapUsage`, and the official SSE client [CX-SSE] treat counters as inclusive totals:

| PestiRoute field | Provider field |
| --- | --- |
| `input_tokens` | `response.usage.input_tokens` |
| `output_tokens` | `response.usage.output_tokens` |
| `cached_tokens` | `response.usage.input_tokens_details.cached_tokens` |
| `reasoning_tokens` | `response.usage.output_tokens_details.reasoning_tokens` |
| Source/completeness | Provider when actually reported; partial/unknown otherwise |

Do not add cached tokens to input tokens or reasoning tokens to output tokens. For example, input=100, cached=40, output=20 and reasoning=5 means 100 input and 20 output, not 140 and 25.

V2 and current Codex also understand cache-write details. The supplied PestiRoute contract has no separate cache-write field; keep that detail in permitted provider diagnostics instead of silently extending the public report.

If the terminal event is truncated or usage is absent, fields remain unknown. Subscription billing does not mean zero token usage. V2 sets subscription monetary cost to zero; this is a UI cost representation, not accounting evidence that execution used no tokens.

Exact usage capability should be claimed only for the tested protocol/model/account and normal completion case. Interrupted usage must retain the original admission estimate separately.

### 7.5 Quota is separate from token usage

[CX-QUOTA] parses Codex quota headers and `codex.rate_limits` events. Header families include used percentage, window minutes and reset-at values for primary and secondary windows, potentially for multiple metered limit IDs.

These are provider quota snapshots, not a tokenizer and not an exact per-request debit. They may be missing or stale. Parse provider semantics in the connector; Core can use generic classified throttling metadata and policy.

Do not add a live quota polling service to the first slice. If later required, its scheduling and API ownership need explicit scope; quota updates must not invent repeated Complete frames or new unversioned control operations.

### 7.6 SSE implementation details in Go

The implementation must handle network chunks that split JSON, UTF-8, line endings and SSE events. It must support multiline `data:` fields, comments, CRLF, events larger than the default Scanner token limit, and bounded event size.

Use a byte-oriented parser with explicit limits. Forwarding chunk boundaries need not equal event boundaries. Observe raw chunks before or during delivery so Complete cannot race past the parser's terminal state.

Never let a telemetry parser consume the response independently with an unbounded tee queue. One consumer or a tightly coupled read-observe-forward loop preserves backpressure. A slow client must slow upstream reads; cancellation must close the upstream body and unblock reads/writes.

Set connection, TLS, header, stream-idle and total deadlines separately. A short global HTTP client timeout will break long reasoning calls. V2's session WebSocket defaults use a 30-minute idle timeout, reflecting that reasoning can be silent for minutes; this is a reference value, not a mandatory gateway setting.

## 8. Errors and retry safety

### 8.1 Preserve provider diagnostics without delegating policy to them

V2 reads error code/message from top-level event fields, nested `error`, or `response.error` ([V2-OPENRESPONSES], `errorDetail`, `providerFailure`). It distinguishes failures such as context overflow instead of reducing every failure to a disconnected stream.

PestiRoute's connector translates provider errors to `GatewayError`. Core acts only on Category, Retryable, RetryDisposition and policy. It must not branch on Codex codes or parse provider bodies.

| Situation | Category proposal | Replay disposition |
| --- | --- | --- |
| Local unsupported request before delivery | unsupported_feature / invalid_request | safe delivery state, but non-retryable with unchanged request |
| Expired/unusable credential before delivery | unauthenticated | Non-retryable until credentials change |
| DNS/connect failure conclusively before body delivery | unavailable | safe, potentially retryable |
| Explicit provider rejection before execution | Code-specific category | safe only when that rejection's semantics are verified |
| HTTP 401/403 | unauthenticated / permission_denied, according to body/profile | No hidden inference retry; auth recovery and a new runtime attempt need explicit policy |
| HTTP 429 | rate_limited | Potentially transient; status alone does not prove safe replay |
| HTTP 5xx | unavailable / internal according to evidence | Do not assume safe because it is a 5xx |
| Connection loss after possible request delivery | unavailable / timeout | unknown or unsafe; no automatic replay |
| SSE terminal failure after commit | Provider classification | No fallback after commit |
| Cancellation | cancelled | No replay; provider outcome may remain uncertain |
| EOF before terminal | unavailable / internal according to cause | Incomplete attempt; unknown replay safety |

Do not mark a no-fallback validation error as a retryable failure just because no bytes were sent.

### 8.2 A gateway attempt differs from an agent-client retry

OpenCode's runner can retry a model step and reconstruct its canonical conversation. PestiRoute serves a client-visible HTTP exchange and records each backend attempt. It must not hide inference retries inside `Execute`.

Credential exchange/refresh is an authentication operation, not an inference retry. Resolve a fresh selected-account credential before Execute. If inference returns 401, do not refresh and resend inside the connector under the same attempt; report the classified failure for runtime policy.

The initial HTTP path should make one inference request per Execute. Go transport behavior also needs review: do not attach replay-enabling idempotency assumptions, `Idempotency-Key`, or custom retry middleware without upstream proof. Standard transport reconnection must not become an accidental application replay.

### 8.3 Pre-head classification does not require whole-response buffering

For a non-2xx response, read only a bounded error payload, classify it and preserve that bounded payload for client delivery. For a 2xx stream, emit Head and stream incrementally. Do not wait for the entire generation to determine whether to try another account.

A provider error as the first SSE event may already occur after HTTP commitment in the basic path. That is a conservative, valid outcome: report failure on the existing stream. Delaying Head to inspect the first provider event is a separately tested optimization, not necessary for initial correctness.


## 9. WebSocket continuation in v2

### 9.1 What is actually implemented

Unlike the older dev bridge, v2 has a stateful Responses continuation implementation.

[V2-RESPONSES] selects a channel-capable transport with the beta protocol header `responses_websockets=2026-02-06` and a requested socket rotation age of 55 minutes. [V2-OAUTH] defaults the OpenAI provider to WebSocket unless its transport setting is already specified. The session model request layer binds the transport only when its resolved policy selects WebSocket.

The URL is the HTTPS Responses URL converted to WSS. [V2-CHANNEL], `message`, removes HTTP-only `stream`, `stream_options` and `background`, then creates:

```json
{
  "type": "response.create",
  "model": "<verified-model>",
  "instructions": "<client instructions>",
  "input": ["<Responses items>"],
  "store": false
}
```

This is a schematic message; the other supported request fields remain present. A WebSocket JSON message is not an SSE event. The transport feeds its JSON frames into the same protocol machinery.

### 9.2 State, affinity, and serialization

[V2-SESSIONTRANSPORT] owns state per session:

- semaphore for one active exchange;
- physical connection and opening time;
- connection affinity;
- active exchange delivery state;
- last checkpoint and a staged pending checkpoint;
- failure count and a sticky HTTP fallback mode.

The connection affinity includes URL and a hash of sorted handshake headers. A refreshed bearer token therefore changes affinity and reopens the connection; tests explicitly cover this.

Concurrent calls in one v2 session are serialized, rather than borrowing the same socket concurrently. Other sessions can run concurrently. This differs from the older dev pool, which sends a busy session's concurrent request over HTTP.

For a gateway, the pool key must include connector instance, runtime-selected account and relevant backend identity. An arbitrary client session header is insufficient isolation. Reusing a socket across two accounts can leak state even if their client session strings happen to match.

### 9.3 How incremental requests are selected

[V2-CONTINUATION], `incremental`, compares:

1. All non-input request invariants, excluding the continuation control fields.
2. The next input prefix against prior request input plus prior authoritative response output.

Only if both match does it send the new suffix with `previous_response_id`. Otherwise it sends a full request.

Thus a normal tool round can send only:

```json
{
  "type": "response.create",
  "previous_response_id": "<prior response ID>",
  "input": [
    {
      "type": "function_call_output",
      "call_id": "call_example",
      "output": "<client result>"
    }
  ]
}
```

The actual incremental message also contains the request fields selected by the route's continuation shaping. Do not implement incremental mode by simply noticing that `previous_response_id` exists or by taking the last input item.

A completion may re-encrypt a reasoning item. V2's checkpoint builder deliberately prefers the matching `output_item.done` reasoning representation when comparing against what the client actually replayed. This is a concrete reason to preserve full reasoning items rather than summary text.

### 9.4 Checkpoint commitment

On completed response observation, the session transport stages a checkpoint. It promotes that checkpoint only when the outer execution calls `complete`. Incomplete response, rejection, interrupted consumption, rotation and dirty channel state invalidate continuation as appropriate.

Receiving a completed event is not enough to publish state that a downstream consumer did not finish consuming. PestiRoute needs equivalent consistency if it later owns continuation state.

A backend response ID generated during an interrupted attempt must not be silently attached to a future client call.

### 9.5 Delivery and recovery

V2 separates not-sent, ambiguous, provider-observed and rejected delivery states. Its transport:

- Can fall back to HTTP when connection setup fails before a request is sent.
- Does not immediately replay an ambiguous send failure.
- Records midstream losses and may select HTTP for future exchanges after five consecutive stream failures.
- Uses explicit rejected status for specific continuation recovery.
- Drops a socket after provider error frames; the source explains the Codex backend stops serving that connection reliably after an error.
- Rotates aged sockets and changes in auth/handshake affinity.

The continuation layer recognizes `previous_response_not_found` as retry-full and `websocket_connection_limit_reached` as rotate-and-retry-full. It also treats an unclassified invalid-request rejection of an incremental send as a full-context recovery candidate.

These are useful hypotheses for a later Codex profile, not a license to retry every invalid request. In PestiRoute, every inference replay is a new runtime-owned attempt; permitted recovery needs confirmed safe rejection and must remain pre-commit. Once Head is forwarded, even an explicit continuation rejection cannot become cross-account fallback in that exchange.

V2 handles close code 1009 as message-too-large and permits HTTP recovery when classified as rejected. PestiRoute should validate that rejection evidence and account for commit before reproducing it.

### 9.6 Backpressure gap

The v2 session transport creates `Queue.unbounded<string, AIError>()` for active frames. Its tests deliberately exercise a synchronous burst larger than any fixed capacity ([V2-TRANSPORT-TEST], “buffers a synchronous burst of frames larger than any fixed capacity”).

That may be an application choice, but PestiRoute explicitly requires bounded backpressure. Do not copy this queue. Merely changing it to a bounded queue while retaining a non-blocking callback can drop frames; reader flow control and bounded bytes must be designed together.

The low-level v2 WebSocket implementation also has a 16 MiB frame limit ([V2-WS]). The gateway should choose its own explicit limits from tested workloads, not copy this number without capacity analysis.

### 9.7 Initial scope decision

**Proposal:** HTTP/SSE first; no WebSocket dependency, continuation cache, or socket pool in the first connector slice. HTTP is an implemented route in OpenCode and the official client, so WebSocket is not intrinsically required to reproduce basic Codex inference.

After the HTTP path passes tool/reasoning/multi-turn tests, investigate WebSocket transport independently. A backend-only WebSocket implementation might still fit opaque contract frames, but persistent continuation ownership, affinity and recovery must be explicitly scoped. Client-facing bidirectional sessions require a separately specified contract extension.

## 10. Official Codex differences: Responses Lite

The official client's request builder is not identical to OpenCode v2. This matters especially when comparing tool behavior.

### 10.1 Source-level differences

[CX-CLIENT], `build_reasoning` and `build_responses_request`, uses model metadata `use_responses_lite`.

| Behavior | Standard request profile | Official Responses Lite profile |
| --- | --- | --- |
| Initial tools | Top-level tools field | `additional_tools` prefix item in input |
| Initial instructions | Top-level instructions string | Initial instruction content as a prefix item in input |
| `reasoning.context` | Omitted by the official builder | `all_turns` |
| `parallel_tool_calls` | Prompt's configured value | Forced false by the inspected official builder |
| Internal header | No Lite opt-in | `x-openai-internal-codex-responses-lite:true` |
| Provider/tool metadata | Profile dependent | Some additional client/internal metadata |

The relevant official expression is:

```rust
parallel_tool_calls: prompt.parallel_tool_calls && !model_info.use_responses_lite,
```

The bundled official model catalog [CX-MODELS] marks the inspected GPT-6 Sol/Luna entries as `use_responses_lite:true`. Some of those same entries have `supports_parallel_tool_calls:true`. That demonstrates why a model catalog capability flag alone does not tell you the resulting request: the selected request profile can override it.

The inspected OpenCode v2 OpenAI plugin and standard Responses lowerer do not contain this official-client Lite opt-in or the same prefix construction. Their normal path uses top-level instructions and tools.

### 10.2 What this does and does not explain

A difference in number of tool calls could result from:

- a gateway rewriting `parallel_tool_calls`;
- choosing a different dialect/profile;
- losing tool schemas or multi-turn reasoning state;
- a client's own tool scheduling and prompt;
- model behavior independent of transport.

The official Lite branch gives a specific code-level hypothesis worth testing. It does not prove the backend always requires Lite for these models, or that `parallel_tool_calls:true` guarantees several calls in every model turn.

For PestiRoute:

1. Start with the OpenCode-style Responses profile that the client already uses.
2. Record actual upstream request shape in redacted fixture/smoke evidence.
3. Add a separate Lite profile only if tested backend compatibility requires it.
4. Declare tool-parallel support per profile/model/account.
5. Never activate Lite because a model name happens to contain “Sol” or “Luna” in Core.

If a profile cannot satisfy a requested parallel-tool capability, routing must exclude it. The connector should not silently turn the requirement off.

### 10.3 Model availability

V2's model transform filters a pre-existing catalog using an allow set, deny set, reasoning-mode checks and a numeric GPT version heuristic. It zeroes cost and overrides context/input limits for enabled ChatGPT entries. It does not prove the selected account is entitled to every visible model.

The official model manager [CX-MODEL-MANAGER] has bundled metadata, cached/remote catalog policy, account identity considerations and model-listing refresh. That is a better reference for a later account-aware model discovery spike than copying a static version regex.

Initial PestiRoute proposal: explicitly configured, individually smoke-tested backend IDs with scoped metadata. Later discovery belongs to the connector's existing `Models` operation. Avoid a universal context-limit override; record where the number came from and for which endpoint/account/profile it was validated.

## 11. Native compaction and other features to scope separately

V2 supports more than basic streamed generation:

- `POST /responses/compact`, built by [V2-COMPACTION].
- In-band `compaction_trigger` and `context_management`, implemented through [V2-CHECKPOINT] and [V2-RESPONSES].
- Encrypted compaction items retained in history.
- Model effort updates through `configuration_update` for compatible models.
- Native tool namespaces and hosted image generation/tool outputs.

The plugin's `remote_compaction_v2` beta header is therefore tied to actual surrounding functionality.

Compaction checkpoints are backend provenance-bound. V2 verifies they return to the originating provider/API; request preparation also checks that a routing hook does not move an existing opaque context window to another deployment.

PestiRoute's current contract names future session capabilities but does not define general compaction jobs or Responses resource operations. Do not add hidden calls to `/compact` inside Execute, mutate client conversation history, or advertise background/resources/session support as a consequence of basic Responses support.

A raw compatible compaction item might eventually be relayed in a scoped profile, but its affinity and fallback restrictions must be defined first.

## 12. Fit to PestiRoute's contract

### 12.1 Ownership mapping

| Responsibility | Owner in PestiRoute |
| --- | --- |
| Client endpoint, bounded body, protocol decode and declared requirements | Responses Protocol Adapter |
| Virtual-key authorization and route/account eligibility | Core Runtime |
| Account selection, attempt identity, reservation and exactly-once accounting | Core Runtime |
| Secret encryption/persistence, auth-session storage and refresh serialization | Shared runtime services |
| OAuth URLs, scopes, exchange, refresh interpretation and account-claim extraction | Codex Connector |
| Codex endpoint, headers, model rewrite and payload dialect policy | Codex Connector |
| Upstream SSE/WebSocket knowledge, errors, token usage and quota interpretation | Codex Connector/private helpers |
| Client tool execution and conversation construction | The calling client |
| Concrete connector construction and registration | Composition root |

The supplied CONTRACT, ARCHITECTURE, EXTENSIBILITY and AGENTS already establish these boundaries. A Codex provider addition should not require provider-specific public APIs.

### 12.2 Existing operations

| Contract operation | Proposed initial responsibility |
| --- | --- |
| Descriptor | Stable connector ID, Agent Protocol category, supported contract version, Responses protocol and OAuth methods |
| Init | Validate fixed/allowed backend endpoint and profile configuration; set up scoped HTTP resources |
| Health | Local readiness/configuration status; no inference probe or entitlement claim |
| Capabilities | Scoped tested matrix; absent/unproven properties remain unknown |
| Models | Configured verified IDs first; later discovery without inference |
| EstimateUsage | Connector-owned estimate with explicit method/confidence; unknown is not zero |
| Authenticate | Device start/continue and refresh; browser flow after remote callback ownership is settled |
| Execute | One selected-account inference attempt, incremental bytes and exactly one orderly completion |
| Close | Cancel/drain connector work and release resources idempotently |

Do not claim this table describes existing Go signatures or available code. The supplied contract is semantic; implementation must inspect CURRENT and current Go bindings before editing.

### 12.3 Suggested private implementation files

These are proposed locations, to adapt to actual repository conventions:

| File | Bounded purpose |
| --- | --- |
| `connectors/codex/connector.go` | Descriptor/lifecycle and existing contract integration |
| `connectors/codex/oauth.go` | Provider exchange and refresh functions |
| `connectors/codex/device.go` | Device start/poll state-machine behavior |
| `connectors/codex/claims.go` | Bounded token metadata extraction |
| `connectors/codex/request.go` | Explicit JSON adaptation and request/header construction |
| `connectors/codex/sse.go` | Byte-preserving stream observation, terminal and usage extraction |
| `connectors/codex/errors.go` | Sanitized provider classification and retry disposition |
| `connectors/codex/capabilities.go` | Profile/model/account capability declarations |
| `testdata/codex/<profile-version>/` | Redacted deterministic provider fixtures |

Do not add all these files as empty scaffolding. Add them as the corresponding bounded work is implemented. Reuse existing HTTP lifecycle, filtering, cancellation and conformance helpers where their semantics match; keep Codex interpretation private.

### 12.4 Execute control flow

Conceptual pseudocode, not a claim about current language bindings:

```text
validate admitted protocol, selected mode and profile
build a new upstream request only if explicit adaptation is enabled
obtain the selected account's runtime-managed usable credential
build allowed upstream headers using that credential
send one HTTP request with the execution cancellation context

if rejected HTTP response:
    read a bounded error payload
    classify before exposing Head
    return classified error or preserved error stream under contract policy

emit Head for admitted Responses protocol
for each upstream byte chunk:
    observe chunk with bounded connector-local SSE parser
    forward the original bytes with backpressure
    stop appropriately on cancellation or terminal error

emit Complete with actual outcome and provider/partial/unknown usage
close upstream resources
```

Every failure path must release the body and producer resources. If the caller closes its stream, cancellation propagates without waiting for a complete response.

### 12.5 Initial capability proposal

These are desired declarations **after** conformance and smoke evidence, not current verified PestiRoute capabilities.

| Capability / feature | Initial scope |
| --- | --- |
| Responses streaming | Candidate supported after text/lifecycle tests |
| Function tools | Candidate supported after call/result and multiple-round tests |
| Parallel function tools | Unknown until profile/model/account tests; only supported can satisfy a requirement |
| Reasoning | Candidate supported after encrypted-state replay |
| Vision | Unknown until a tested source/model/input fixture and smoke |
| Audio / hosted tools / structured output | Unknown or explicitly unsupported according to actual scoped profile |
| OAuth | Candidate supported after exchange, refresh, cancellation and storage tests |
| Exact usage | Candidate supported for reported terminal counters; partial/unknown on interruption |
| Non-streamed Responses | Unsupported in the first slice |
| Previous-response/conversation resources | Unsupported in the first slice |
| Background execution / native compaction | Unsupported in the first slice |
| Northbound Chat Completions / Anthropic | Not accepted by the initial Codex connector; requires its own connector-local translator |

A minimal HTTP Codex connector can fit the existing v1 semantic contract. New auth UI actions, unavailable runtime service operations, or stateful session commands may expose implementation/specification gaps. Resolve a relevant gap through the required ADR process before changing contract semantics.

## 13. Verification and handoff

### 13.1 What to reuse from upstream tests

| Source tests | Observable behavior to carry into Go fixtures |
| --- | --- |
| [V2-OAUTH-TEST] | Browser/headless registration; subscription catalog filtering; API-key isolation; proxy isolation; omitted application output limits |
| [V2-INTEGRATION-TEST] | Expired credentials, OAuth completion/failure/cancellation, attempt expiry and persistence |
| [V2-RESPONSES-TEST] | Function schemas, parallel option precedence, namespaces, chronological developer updates, usage, reasoning, terminal recovery |
| [V2-TRANSPORT-TEST] | Auth changes rotate socket, no ambiguous-send fallback, checkpoint commitment, queue cancellation and cross-session isolation |
| [V2-WS-RECORDINGS] | Tool continuation on one socket, full context after reconnect, explicit continuation rejection |
| [DEV-CODEX-TEST] | Account-claim precedence, residency claims and concurrent refresh deduplication in the older plugin |
| [CX-SSE] and [CX-SSE-ERROR] | Terminal usage and provider-specific failure shapes |
| [CX-DEVICE] | Device polling deadline and provider-specific login responses |

The dev plugin's successful singleflight test does not establish that the v2 integration resolver has the same locking behavior. Use that test's goal as a PestiRoute runtime requirement.

Recorded OpenAI Responses cassettes are protocol evidence, not automatically subscription-backend evidence. For example, the inspected dev “native-openai-oauth-tool-loop” cassette records `api.openai.com/v1/responses` URLs before the fetch rewrite. The v2 WebSocket recordings likewise must be read with their actual recorded endpoint/model. Labels such as “OAuth” do not substitute for a captured ChatGPT request.

### 13.2 Deterministic acceptance scenarios

The first release should have tests for the following behaviors, using local fake upstreams and redacted fixtures:

1. Device start, pending poll, success, failure, deadline and cancellation.
2. PKCE/state validation if browser flow is included.
3. Account-claim precedence, absent identity and malformed token data.
4. Refresh before expiry with 20 concurrent callers causes one provider exchange per account and returns the same persisted credential generation.
5. Different accounts refresh independently and cannot obtain one another's credentials.
6. Rotated-token persistence survives restart; storage failure never claims durable success.
7. Client virtual key and inbound account/auth headers never reach the upstream.
8. Native mode preserves body bytes and extensions; rewrite mode keeps the admitted request immutable.
9. A meaningful unsupported field such as `store:true` or an unsupported explicit output limit fails before upstream delivery.
10. First SSE bytes reach the client before the upstream finishes; response size does not create whole-response buffering.
11. SSE framing survives every byte split, CRLF, multiline data, Unicode and large bounded events.
12. Tool names, output-item IDs, call IDs, arguments and result links survive at least two tool rounds.
13. Two tool calls in one response remain distinct and ordered, including interleaved argument deltas.
14. Encrypted reasoning and message phase/unknown fields survive replay.
15. Usage extraction handles inclusive totals, zero versus missing, cached/reasoning subsets and interrupted attempts.
16. A 200 response with `response.failed` is recorded as failed; `response.incomplete` is not promoted to success.
17. EOF without terminal is incomplete, not free, and is not retried automatically.
18. Cancellation/stream close stops upstream work and finalizes accounting once.
19. A conclusively not-sent failure may allow one runtime-policy fallback; ambiguous delivery and post-commit failure never do.
20. Repeated completion or malformed frame ordering cannot duplicate usage/release or fabricate success.

WebSocket/compaction scenarios belong to later scoped work, not this initial test gate.

### 13.3 Real-provider comparison

After deterministic checks, run a separately authorized, bounded direct-versus-gateway smoke with the user's own selected account:

- Pin client version, connector profile, backend model and source revisions.
- Use the same instructions, tool definitions, history, options and account.
- Compare a small text stream, a function-tool round, two independent tool calls, and a follow-up containing encrypted reasoning.
- Verify headers/body shape through redacted local traces, time to first byte, event order and final usage.
- If a model emits one call despite permitting parallel calls, distinguish request/capability preservation from the model's choice. A single model response is not a deterministic proof of parallel capability.
- Test refresh separately; do not invalidate a working credential merely to manufacture an expiry failure.
- Keep access/refresh tokens and private prompts out of fixture commits.

No smoke has been run for this research. Consequently, backend entitlement, exact optional field acceptance and complete feature coverage remain unverified.

### 13.4 Bounded implementation candidates

These are research handoff candidates, not READY task cards or edits to TASKS:

| Order | One independently verifiable result | Dependency |
| --- | --- | --- |
| 1 | Freeze profile/request/SSE/auth fixture set and scoped compatibility matrix | This research plus source/smoke decisions |
| 2 | Map existing Go contract/runtime services and identify concrete gaps | Current repository inspection |
| 3 | Device authorization exchange behind Authenticate, with deadline/cancellation | 1–2 |
| 4 | Runtime account-scoped refresh and rotated-token persistence tests | 2–3 and existing M3 auth runtime |
| 5 | Selected-account request/header builder and explicit JSON policy | 1–2 |
| 6 | One streamed HTTP Execute with cancellation and cleanup | 4–5 |
| 7 | Byte-preserving SSE observer and terminal/usage classification | 6 |
| 8 | Tools, reasoning and parallel-call conformance fixtures | 7 |
| 9 | Retry-safety and interrupted accounting integration | 6–8 and existing runtime policy |
| 10 | Direct-versus-gateway smoke, verified capability declarations and documentation | 3–9 |
| Later | Browser login, model discovery, WebSocket continuation, native compaction, other northbound translation | Separate spikes and evidence |

This fits M5.1 in the supplied roadmap. It does not move Codex into M3 or reopen M4 architecture by itself. Plans should reference this research rather than duplicate it into every card.

## 14. Open questions and decisions

Before claiming production compatibility, resolve:

- Which exact models and account types are in the initial support scope?
- Does the selected backend currently accept the standard OpenCode-style dialect for each model, and when is Lite actually required?
- Which explicit generation controls are accepted, rejected or semantically transformed?
- What client identity/header profile is required, including constrained-account residency?
- Must account identity be present, and is organization fallback acceptable for supported accounts?
- Can refresh omit a refresh token, and how are revoked/rotated refresh-token errors classified?
- Does the current PestiRoute auth runtime expose every action needed for device login without changing the v1 contract?
- How is cache affinity admitted, scoped and kept stable?
- Which upstream rejection codes prove that inference was not started?
- What are body, error-payload, SSE-event and future WebSocket byte limits?
- Which model/tokenizer estimate is available before provider usage arrives?
- Does the initial client require non-streaming, namespaces, vision or hosted tools?
- How are opaque reasoning/compaction state and affinity protected against cross-account fallback?
- For later WebSocket recovery, where is the continuation checkpoint owned and when is it committed?

The recommended first-slice decisions are HTTP/SSE, Responses-only, function tools, full client-supplied history, runtime-owned OAuth/refresh, explicit request adaptation, conservative replay safety and byte-preserving output. These are proposals requiring normal project review; this document does not override accepted decisions.

## 15. Reuse and licensing

OpenCode's inspected repository is MIT licensed ([V2-LICENSE]). Official Codex is Apache-2.0 licensed ([CX-LICENSE]). If source is copied or translated into implementation, preserve the applicable copyright/license/notice obligations and mark its origin and pinned revision in the repository.

Prefer porting small provider-specific behavior and observable fixtures, with their attribution. Do not transplant OpenCode's Effect runtime, plugin system, canonical LLM schema, session database, agent runner or tool execution machinery into PestiRoute.

For the first HTTP/SSE implementation, the project's existing Go standard-library HTTP/JSON/crypto stack is sufficient in principle. A WebSocket library becomes relevant only when that later slice is scoped; an MCP server is not needed for the connector to call Codex.

## 16. Sources

All code links are pinned to the inspected commits. The project basis is the supplied AGENTS.md, ARCHITECTURE.md, DECISIONS.md, README.md, EXTENSIBILITY.md, CONTRACT.md, REFERENCES.md, ROADMAP.md and STACK.md. Code snippets and synthetic examples above are research illustrations, not captured successful provider traffic.

[V2-OAUTH]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/core/src/plugin/provider/openai.ts
[V2-INTEGRATION]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/core/src/integration.ts
[V2-RESOLVER]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/core/src/model-resolver.ts
[V2-REQUEST]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/core/src/session/model-request.ts
[V2-PROVIDER]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/ai/src/providers/openai.ts
[V2-DEFAULTS]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/ai/src/providers/openai-options.ts
[V2-RESPONSES]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/ai/src/protocols/openai-responses.ts
[V2-OPENRESPONSES]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/ai/src/protocols/open-responses.ts
[V2-HTTP]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/ai/src/route/transport/http.ts
[V2-AUTH]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/ai/src/route/auth-options.ts
[V2-CHANNEL]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/ai/src/protocols/open-responses-channel.ts
[V2-CONTINUATION]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/ai/src/protocols/open-responses-continuation.ts
[V2-SESSIONTRANSPORT]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/core/src/session/model-transport.ts
[V2-WS]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/ai/src/route/transport/websocket.ts
[V2-AFFINITY]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/core/src/session/affinity.ts
[V2-COMPACTION]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/ai/src/protocols/utils/responses-compaction.ts
[V2-CHECKPOINT]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/ai/src/protocols/utils/responses-checkpoint.ts
[V2-OAUTH-TEST]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/core/test/plugin/provider-openai.test.ts
[V2-INTEGRATION-TEST]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/core/test/integration.test.ts
[V2-RESPONSES-TEST]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/ai/test/provider/openai-responses.test.ts
[V2-TRANSPORT-TEST]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/core/test/session-model-transport.test.ts
[V2-WS-RECORDINGS]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/packages/ai/test/provider/openai-responses-websocket.recorded.test.ts
[V2-LICENSE]: https://github.com/anomalyco/opencode/blob/0bef0eab678e94a389930a2a1365115fd5a35c69/LICENSE
[DEV-CODEX]: https://github.com/anomalyco/opencode/blob/c42ae0d56b6f86f8df39d451d6d2cfe6414b3928/packages/opencode/src/plugin/openai/codex.ts
[DEV-CODEX-TEST]: https://github.com/anomalyco/opencode/blob/c42ae0d56b6f86f8df39d451d6d2cfe6414b3928/packages/opencode/test/plugin/codex.test.ts
[DEV-ROLLOUT]: https://github.com/anomalyco/opencode/blob/c42ae0d56b6f86f8df39d451d6d2cfe6414b3928/packages/opencode/src/plugin/openai/README.md
[CX-REQUEST]: https://github.com/openai/codex/blob/c5d242fa7907bff1b7a7e26e95febc548c0a6963/codex-rs/codex-api/src/common.rs
[CX-CLIENT]: https://github.com/openai/codex/blob/c5d242fa7907bff1b7a7e26e95febc548c0a6963/codex-rs/core/src/client.rs
[CX-DEVICE]: https://github.com/openai/codex/blob/c5d242fa7907bff1b7a7e26e95febc548c0a6963/codex-rs/login/src/device_code_auth.rs
[CX-PROVIDER]: https://github.com/openai/codex/blob/c5d242fa7907bff1b7a7e26e95febc548c0a6963/codex-rs/model-provider-info/src/lib.rs
[CX-HTTP]: https://github.com/openai/codex/blob/c5d242fa7907bff1b7a7e26e95febc548c0a6963/codex-rs/codex-api/src/endpoint/responses.rs
[CX-SSE]: https://github.com/openai/codex/blob/c5d242fa7907bff1b7a7e26e95febc548c0a6963/codex-rs/codex-api/src/sse/responses.rs
[CX-SSE-ERROR]: https://github.com/openai/codex/blob/c5d242fa7907bff1b7a7e26e95febc548c0a6963/codex-rs/codex-api/src/sse/responses_error.rs
[CX-QUOTA]: https://github.com/openai/codex/blob/c5d242fa7907bff1b7a7e26e95febc548c0a6963/codex-rs/codex-api/src/rate_limits.rs
[CX-MODELS]: https://github.com/openai/codex/blob/c5d242fa7907bff1b7a7e26e95febc548c0a6963/codex-rs/models-manager/models.json
[CX-MODEL-MANAGER]: https://github.com/openai/codex/blob/c5d242fa7907bff1b7a7e26e95febc548c0a6963/codex-rs/models-manager/src/manager.rs
[CX-LICENSE]: https://github.com/openai/codex/blob/c5d242fa7907bff1b7a7e26e95febc548c0a6963/LICENSE
