# Anthropic fixture provenance

These are synthetic documentation fixtures, not provider captures or evidence
of live compatibility. The backend profile is `claude-opus-5-5`, Anthropic
Messages API version `2023-06-01`; the client request baseline is OpenCode v2.0.6
at commit `b084acc55ea2cdb50e9c2ec49a8d9ab3608d43ad`. Request shapes come from
the pinned [Responses provider tests](https://github.com/anomalyco/opencode/blob/b084acc55ea2cdb50e9c2ec49a8d9ab3608d43ad/packages/ai/test/provider/openai-responses.test.ts).
The positive request retains `store:false`,
`include:[reasoning.encrypted_content]`, and `temperature:0`; rejected request
shapes reflect the accepted [M4-001 binding](../../../../docs/implementation/M4-BINDING.md#first-slice-request-policy).

Anthropic event names and payload shapes in the stream files are synthetic
snapshots based on the official [Messages streaming documentation](https://platform.claude.com/docs/en/build-with-claude/streaming),
inspected 2026-10-03. Failure payloads and IDs/text are synthetic. No SDK or
implementation source was copied. No credentials, account identifiers, private
prompts, or live response payloads are included.

SHA-256 (checked by `TestFixtureIntegrityAndMalformedInput`):

| File | SHA-256 |
| --- | --- |
| `http-error.json` | `15461cdf7d18362d2c31fcb7863999548c08bd0682898f2604e5dcb75d229b0f` |
| `malformed.json` | `e921bf2934d2d0c149fbdba4e6292d4c4abdb400b46370482bac6e10044b476a` |
| `request-developer-only.json` | `3633802e9fb1c535dcf017dc20919687e8d505d17235a14bf9b5d27959784690` |
| `request-late-developer.json` | `12da7d7eaaa8cdb81d599b55fb5c07f92a6bb86270f036cd622f00e0abad6ba4` |
| `request-positive.json` | `b95f7ba46ac4cbea9edd2d77d609a2017edd6a147beebe4c8c695f894f4a6ca0` |
| `request-system-history.json` | `b196feb3741a61e07929b358f4491beb1f6316c0d9e94dcf0c99d617579088d2` |
| `stream-error.sse` | `35837c5537d85715b2a00b4d021bd58bbc77c2eb616aedc216d607bc74286f26` |
| `stream-max-tokens.sse` | `4576b3ba65bff19fa109d28601d5684a28f4079d133d3d045ac74b85f4c9bb3a` |
| `stream-normal.sse` | `89a8bf56a7f10b795c33ad01840083554c8bc4c50aa6b24412c873d550ec5284` |
| `stream-premature-eof.sse` | `34a13b99657c51e9fb0a36bb9da840432cde47f8368125782abbf82b89a3f1bb` |
