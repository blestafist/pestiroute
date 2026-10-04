# Codex OAuth fixture provenance

All payloads in this directory are synthetic schema/control-flow fixtures, not
provider captures, private payloads, or evidence of live compatibility. Shapes
are informed by the pinned [OpenCode v2 source](https://github.com/anomalyco/opencode/tree/0bef0eab678e94a389930a2a1365115fd5a35c69)
and the OAuth 2.0 Device Authorization Grant ([RFC 8628](https://www.rfc-editor.org/rfc/rfc8628)); the
provider-specific authorization-code exchange is described in the repository's
[Codex research](../../../../../docs/references/CODEX_CONNECTOR_RESEARCH.md#33-headless-device-flow).
OpenCode source uses an authorization-code/verifier exchange after polling;
these fixtures do not claim that the provider implements the RFC's direct
device-code token exchange. The safe presentation and expiry cases align with
[DEC-009](../../../../../docs/project/DECISIONS.md#dec-009--separate-safe-user-actions-from-opaque-auth-state)
and [DEC-010](../../../../../docs/project/DECISIONS.md#dec-010--carry-credential-expiry-with-auth-results),
respectively.

Identifiers, codes, verifiers, and tokens are conspicuously synthetic strings
and have no usable secrets. User codes and URIs are examples only. No credentials,
account identifiers, cookies, personal prompts, or captured response bodies are
included. Poll 403/404 files represent pending status cases; terminal errors are
short bounded examples. These fixtures establish no live provider behavior.

SHA-256 (verified against each fixture by `TestAuthFixtureIntegrityAndMalformedInput`):

| File | SHA-256 |
| --- | --- |
| `exchange-no-refresh.json` | `0b81a3bf0276ecdea942e6f5607d96b08e8b1d5a844a493ba67ddb49d8d2c949` |
| `exchange-success.json` | `8e3e77b84f5f1d0c574d39935190dea9ee26dc14a56dd479eea7a7c5e50be885` |
| `exchange-terminal-error.json` | `fe576038c424efb067a0d91748ccd73e1cc928ab388b64c5268ac70f90150290` |
| `malformed.json` | `2239632801b3b5fbb2f7f06b851117d1f96fe9524e7b591aa6bed273926e8d04` |
| `poll-authorized.json` | `19bdb702e5dd6b25067582a9cc70bfd41b22b849d4c0e1ccd45af8553fb2ad8a` |
| `poll-pending-403.json` | `9d2fd6aada807d2ded9ae041ee48cf34659216f69d857a837083a0e54ef0c59c` |
| `poll-pending-404.json` | `28882aad83a0e251bdc6bb73469a858b36606084854061a00af4ebc2272e0a10` |
| `poll-pending.json` | `e06c3b573ba0ec54f47413ce860f0afefc5b89e6e4c21b79a1e027f3b7326d16` |
| `poll-slow-down.json` | `90264f57ea5e1cdded54f2ec1605e224aed8f33a444d5af96218d0cd49437128` |
| `refresh-no-rotation.json` | `4b8a0a701c8ab167f1fe42c662a2d46a33af3936a897d00671183447cbf4af07` |
| `refresh-success.json` | `9fdf96c3ef67a12b07a9964ab53d3d9f3cf9f8ef4baec344a052e0b6c69fdafc` |
| `refresh-terminal-error.json` | `fe576038c424efb067a0d91748ccd73e1cc928ab388b64c5268ac70f90150290` |
| `start-invalid-interval.json` | `e82318556fda8ef0c36c3c417f5a357343bcd5798be32094dfd2b6a0d9453b62` |
| `start-string-interval.json` | `8a00dee88d0662c410e14d4e03fec05a015f2daf2c86929dc12ab99d7d22ab25` |
| `start-success.json` | `63bf284cd76ac21fa8c2c977577a3379fec3809dde9308723a33faf3d3f5f0c8` |
