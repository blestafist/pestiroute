# M4-001 Plain-Text Binding

This is the accepted first-slice binding for the [M4 translation Connector](M4-TRANSLATION.md#outcome-and-boundaries). It refines protocol-specific behavior only; the v1 opaque request/response, Connector ownership, streaming, and lifecycle rules remain those in [CONTRACT](CONTRACT.md#execution-request) and [DEC-003](../project/DECISIONS.md#dec-003--why-connectors-own-provider-translation). This records source behavior, not tested compatibility or entitlement.

## Exact baseline

| Leg | Binding |
| --- | --- |
| Client | OpenCode V2 CLI `2.0.6`, provider `@opencode/ai/providers/openai/responses`, HTTP transport, public OpenAI Responses API `POST /v1/responses`; protocol `openai.responses.v1`. Its model field is accepted only as the configured Responses-side route identity; no name parsing or feature heuristic. |
| Backend | Direct Anthropic public Messages API, `POST https://api.anthropic.com/v1/messages`, `anthropic-version: 2023-06-01`, `stream: true`, API-key authentication from the selected runtime account. The configured backend model is exactly `claude-opus-5-5`; connector configuration explicitly maps the admitted client model to it. No client-controlled model rewrite or account fallback. |
| Fixture source | Anthropic official [Messages create](https://platform.claude.com/docs/en/api/messages/create) and [streaming](https://platform.claude.com/docs/en/build-with-claude/streaming) documentation, inspected 2026-10-03. The published stream example explicitly uses `claude-opus-5-5` and shows its `message_start`, text block/deltas/stop, `message_delta`, and `message_stop`; M4-002 snapshots that synthetic source example and records hashes. |

The Responses HTTP API used here has no separately pinned API-version header. The client baseline is source-derived from the pinned [OpenCode v2.0.6 source tag](https://github.com/anomalyco/opencode/tree/b084acc55ea2cdb50e9c2ec49a8d9ab3608d43ad), specifically its [Responses provider tests](https://github.com/anomalyco/opencode/blob/b084acc55ea2cdb50e9c2ec49a8d9ab3608d43ad/packages/ai/test/provider/openai-responses.test.ts) and provider selection; this does not assert M4 runtime compatibility. There is no third-party SDK or copied implementation in this baseline. Synthetic fixture bytes are derived from official docs, not live captures; M4-002 owns exact fixture hashes and attribution.

Pinned plain-text request shape, reduced from the test `prepares OpenAI Responses target` (model shown there is a test route ID, not an M4 backend model claim):

```json
{
  "model": "gpt-4.1-mini",
  "input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"Say hello."}]}],
  "instructions": "You are concise.",
  "store": false,
  "include": ["reasoning.encrypted_content"],
  "stream": true,
  "max_output_tokens": 20,
  "temperature": 0
}
```

The same pinned test encodes assistant history as a typed `message` with `role:"assistant"`, `status:"completed"`, and `output_text` parts; its chronology test encodes a developer update as `role:"developer"` with string content between user and assistant turns. That late developer form is observed but deliberately rejected by this Messages profile because Anthropic has no ordered developer-history role. The accepted prefix-only developer rule below avoids silently moving it.

## First-slice request policy

* Require JSON `stream: true`. Missing, false, or non-boolean values are rejected locally before upstream dispatch; do not silently force streaming or synthesize a completed non-stream response.
* Accept `input` as a non-empty string or an ordered array of typed message objects. Each object has `type: "message"`, `role`, and `content`; only `user`, `assistant`, or `developer` roles are admitted. User content is a non-empty string or one or more `{type:"input_text", text:<non-empty string>}` parts; developer content is a non-empty string or `input_text` parts. Assistant content is one or more `{type:"output_text", text:<non-empty string>}` parts; optional `annotations` and `logprobs` are tolerated only when empty arrays. Assistant `status` is optional but, when supplied, must equal `"completed"`. Preserve content and message order. Reject other item/part fields, all non-text parts, function/tool items and results, and any unlisted role. A plain input string is one user message.
* Optional top-level `instructions` must be a non-empty string. A `developer` message is representable only in the initial contiguous developer prefix before any user/assistant message; map instructions and that prefix, in order, to Anthropic's ordered top-level `system` text blocks. Reject a developer message after conversational input begins; do not hoist it and alter chronology. Require at least one `user` or `assistant` message after any developer prefix: a developer-only input would map to `messages: []` and is rejected locally. A `system` role in input history is always rejected; only top-level `instructions` is supported as the initial system text. Anthropic Messages has no developer/system role in its conversation history, so it cannot faithfully encode late updates.
* The client `model` is required and must match the explicitly configured Responses-side route model exactly. The Connector sends the configured backend model `claude-opus-5-5`; it does not infer a backend model or rewrite from a model-name prefix.
* `max_output_tokens` omitted means backend `max_tokens: 4096`. Explicit values must be positive integers no greater than 4096 and are sent unchanged. Reject `max_tokens` (legacy spelling) and unsupported generation controls rather than dropping them. If `temperature` is omitted, omit the backend parameter and use Anthropic's documented default; if present, it must be a JSON number in the inclusive range `[0,1]` and is sent unchanged. This is an explicit direct parameter mapping, not a claim of identical sampling outcomes. Reject `top_p`, penalties, stop sequences and other unsupported generation controls. Admission reserves against the effective output limit (default 4096 or the explicit lower bound); estimation may remain unknown, never fabricated as zero.
* `store` may be omitted or supplied as exactly `false`; both mean no Responses server-side state is requested, and it is not forwarded (true is rejected). `include` may be omitted or supplied as a one-element array containing `reasoning.encrypted_content`; it requests an optional response field but does not enable reasoning. This plain-text profile emits no reasoning; if reasoning input or provider reasoning content occurs, reject/fail rather than flattening or discarding it. No reasoning support is claimed.
* The complete top-level allowlist is `model`, `input`, `instructions`, `stream`, `max_output_tokens`, `temperature`, `store`, and `include`. Reject any other supplied field, including `previous_response_id`, `conversation`, `background`, structured output, tools, and unknown extensions. No implicit server-side state, session, or output format is promised. Tool and reasoning policy remains for its later decision cards.

## Stream and lifecycle binding

The Connector translates provider events into the admitted Responses protocol, retaining separate response, output-item, and (later tool) call identities. The first valid Anthropic `message_start` supplies the backend message ID/model/initial usage. Create one client response ID and one text output-item ID for this execution; IDs are connector-generated and distinct, never aliases for one another. Emit `response.created`, then `response.in_progress`, `response.output_item.added`, and `response.content_part.added` before the first text delta. Each `text_delta` is forwarded promptly as `response.output_text.delta` in order; it is not held for the final body.

Success requires a valid text-block closure, a `message_delta` with a recognized normal stop (`end_turn` or `stop_sequence`), and `message_stop`. Emit `response.output_text.done`, `response.content_part.done`, `response.output_item.done`, and exactly one `response.completed` with final snapshots, then orderly EOF. `tool_use` stop ends one model response, not a tool execution; it is outside this text profile. `max_tokens` is truncation: emit final snapshots and one `response.incomplete` with `incomplete_details.reason: "max_output_tokens"`, and settle the attempt incomplete/failed, never completed. Provider error, malformed ordering, unknown semantic content, missing stop reason, missing `message_stop`, and premature EOF are failures, not success.

Before response `Head` reaches the Adapter, a conclusive local validation or upstream HTTP rejection may be returned as a pre-commit gateway error. The Adapter commits when it starts client headers for the stream. After that point, any already-forwarded text is irrevocable: the Connector may emit only the protocol-permitted terminal failure event if it can do so, otherwise close; it must not substitute an HTTP response, replay, retry, or fallback. Core finalizes the attempt exactly once under the v1 contract; no provider SSE bytes pass through.

Finite reconstruction ceilings for this profile: at most 64 provider content blocks, 1 MiB per SSE line/event, and 1 MiB cumulative retained text. Deltas still flow as received; the text ceiling exists only for required final snapshots. Crossing any ceiling terminates the attempt as incomplete/error and releases upstream resources. Reads remain coupled to downstream backpressure; cancellation, early close, deadline, and shutdown cancel the provider request. These are binding limits, not provider limits.

## Paper walkthroughs

1. **Text request:** Adapter validates the exact configured client model, `stream:true`, text input, and budget. Connector maps input and instructions, uses scoped account credentials, and streams to Anthropic. Local validation owns rejection before dispatch. The Adapter commits on `Head`; connector `message_start` opens the Responses lifecycle; text deltas flow immediately. Only both successful stop markers permit completed snapshots and `Complete(succeeded)`.
2. **Early delta:** after client headers/Head have committed, the first text delta is written before upstream completion. If a later error occurs, already-delivered text is not rolled back; Connector reports terminal failure if representable, otherwise closes, and Core finalizes once. No replacement response or retry is allowed.
3. **Successful stop:** after text block stop, normal `message_delta` stop reason and `message_stop`, Connector emits final snapshots and one completed terminal event and then `Complete(succeeded)`/EOF. A terminal event is never emitted twice.
4. **Truncation/error:** `max_tokens` stop yields `response.incomplete`, not completed. Provider SSE error, malformed sequence, absent required stop, premature EOF, or retained-state ceiling yields failed/incomplete; before Head, classify/reject pre-commit; after commit, only an in-band terminal failure or close. HTTP 200 alone is not success and ambiguous delivery is not safe to replay.
5. **Unsupported input:** image, tool, reasoning, a `system` history item, a developer message after any user/assistant turn, developer-only input, non-text content, non-streaming request, unsupported stateful/structured-output fields, or invalid output budget is rejected locally before provider call. An initial developer prefix followed by at least one user/assistant message is accepted and mapped in order with top-level instructions. No translation may silently discard meaning. Any unrecognized semantic provider content fails conservatively; only harmless notifications explicitly documented by later parser work may be ignored.

Later cards must use this binding as the baseline. Tools and reasoning remain unresolved and are not part of the text support claim. No live API entitlement, provider behavior, account availability, or PestiRoute compatibility is claimed.
