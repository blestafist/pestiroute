# Codex Responses fixture manifest

All payloads below are **synthetic**, generated locally for deterministic tests; none were captured from a provider or client. The source profile is pinned OpenCode v2 `0bef0eab678e94a389930a2a1365115fd5a35c69` and OpenAI Codex `c5d242fa7907bff1b7a7e26e95febc548c0a6963`; see [M5.1 binding](../../../../../docs/implementation/M5.1-BINDING.md#pinned-source-and-runnable-client). The runnable client reference is OpenCode V2 CLI 2.0.6, not a claim of verified Codex compatibility.

No fixture contains usable credentials, account identifiers, personal prompts, or provider payloads. Ciphertext and identifiers are unmistakably synthetic. These fixtures specify shape and test invariants only, not live provider behavior. The `store:true` and `stream:false` requests record negative field shapes; their presence is not proof of production rejection.

## Intended stream outcomes

| Fixture | Intended outcome |
| --- | --- |
| `stream-normal-text.sse` | completed with inclusive usage |
| `stream-tools.sse` | completed tool-call arguments |
| `stream-interleaved.sse` | completed interleaved items/calls |
| `stream-reasoning.sse` | completed; opaque reasoning ciphertext |
| `stream-incomplete.sse` | incomplete (`max_output_tokens`) |
| `stream-failed.sse` | failed |
| `stream-error.sse` | failed error event |
| `stream-premature-eof.sse` | incomplete: connection ends before terminal event |
| `stream-crlf-multiline.sse` | completed; CRLF/multiline/Unicode |
| `stream-usage-zero-missing.sse` | completed with the entire usage object omitted |
| `stream-boundary-keepalive.sse` | completed after keepalive/rate_limits and near-ceiling event |

## SHA-256

| File | SHA-256 |
| --- | --- |
| `request-positive.json` | `a099e48b6dd305ba3f0c84b43f5009993c2b5332d3772857fb72c8e94dd725b0` |
| `request-reasoning-item.json` | `07ad0ea90d6de53fea976b587c432a0186b0ea3fa0294f5d576026ae0423f045` |
| `request-store-true.json` | `50072a746f8cf919ab672bb6e18aaeb2f8c446653f47a2324eee1944fa751470` |
| `request-stream-false.json` | `a05d037f99f97f6c4a90881e1aee1df07d4a5fbaf684c5bd699c2cd0cc19e397` |
| `request-tools-history.json` | `250d5e8d8cbfb2a9f7fb2a07b875e284b802a910e823c56c57f7c1534f17a0ac` |
| `request-unknown-fields.json` | `de171866d5c69665efa5b40d2164abe26ee57a97bf53f106be0c978038872be6` |
| `stream-boundary-keepalive.sse` | `36d283e0cfa0a8154c523c563e57c6259b802a4f77d57875e6d6d0743f9d0d6e` |
| `stream-crlf-multiline.sse` | `e8150d4de518fc73c01676c685a268401034b9994b24583db0bfc9ec4a2c3328` |
| `stream-error.sse` | `b051ece0fd98dd5ca819c3c7df0781aa5ed0c5c1df30975ffaa02bf6ff7722c4` |
| `stream-failed.sse` | `9e9d5f3c5743b1e72cff07a12d46f9af3643aa1333ddeb181cd17492967a0069` |
| `stream-incomplete.sse` | `0388d5405fedfaa831848f2612ce5658762d8f1e1f5b954fb9fb79eb0a8cdff1` |
| `stream-interleaved.sse` | `aab16d7f3cbfa7253dc3b40a96ae278fa228c2dea8942448ded75412f1e1fa4d` |
| `stream-normal-text.sse` | `7ce1f45ebbde4f20ce39b5420a1c0aae1d8f86018d0705be62b8302877328da2` |
| `stream-premature-eof.sse` | `26c2865b4782d78a3e58552834854fd4490320f3663ed98f9f0c3d43672081c8` |
| `stream-reasoning.sse` | `ba52cf66afa78a7bf3bbd692f6b7351dc5b3cc520899867fde8758a2f77f80b8` |
| `stream-tools.sse` | `ae58072b2099b8b986520181a673eecc928681a0ed5fc25c2e1e1144cc38d1d4` |
| `stream-usage-zero-missing.sse` | `5843fd7bda4d0555158d88f9d31f62971c90012962170aed55dd126efb523162` |
