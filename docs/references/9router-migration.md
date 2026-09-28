# Source

Repository: local `.refs/9Router` (`9router-go`; its own `AGENTS.md` identifies `decolua/9router` as a separate upstream, not inspected here).
Revision/commit: `14bb4f241350076984ad9a2ec639dd6cf7ae9de0`.
Date: 2026-09-28 (research date; source commit date 2026-09-28).

Source citations below are relative to `.refs/9Router/`; this untracked local checkout is needed to follow them. License for this checkout: **UNKNOWN** (no license file found; a README badge is not a grant). No live provider interoperability was tested.

# Purpose

Extract provider wire-protocol and OAuth knowledge for future PestiRoute Connector planning, **not** the source gateway's routing or architecture. See [PestiRoute's connector boundary](../implementation/CONTRACT.md#connector-contract).

# Existing capabilities

## Research status

**KNOWN** — the cited provider code, four OAuth flows, request builders, streaming paths and usage extraction have been mapped. Coverage of every registered backend and uninspected edge cases is not implied.

## Migration status

**READY** — the documented Codex, Claude Code, Kiro AI and Antigravity protocol/auth knowledge can inform Connector planning. **NEEDS_VALIDATION** — current endpoints, client versions, edge cases and code-reuse rights before implementation or compatibility claims. Old routing and universal models are **NOT_APPLICABLE** to migration.

## PestiRoute implementation status

**NOT_STARTED** — this document describes the source, not implemented PestiRoute Connectors.

## Live verification status

**UNKNOWN** — no live provider/client interoperability was tested at this revision.

## Architecture map

| Concern | Evidence in 9Router | Finding |
| --- | --- | --- |
| Backend inventory | `internal/proxy/executor/init.go:5-86`; `internal/providers/providers.go:505-537,783-787`; `internal/proxy/gemini.go:19-101` | Registered executors include generic OpenAI-style backends (e.g. OpenAI, DeepSeek, Groq, OpenRouter, Ollama) and specialized Codex, Claude, Kiro and OpenCode; Antigravity and Gemini CLI also have provider configurations and separate Gemini forwarding paths. Registration/catalog entry is **not** evidence of live support or feature parity. |
| Client protocols | `internal/handlers/router.go:49-84`; `internal/handlers/chat/chat.go:179-205`; `internal/handlers/chat/responses.go:60-108` | Exposes Chat Completions, Anthropic Messages, Responses, models, embeddings and media endpoints; also an Ollama chat handler. Messages and Responses may be bridged through Chat depending on backend; native Responses can pass through. Endpoint presence does not establish full protocol compatibility. |
| Auth entry and refresh | `internal/handlers/oauth/pkce.go:48-82,113-176,288-325`; `internal/handlers/oauth/device.go:293-337`; `internal/handlers/oauth/antigravity.go:64-103`; `internal/proxy/oauth/background.go:16-33,124-208` | OAuth handlers initiate/exchange credentials; provider refreshers and background worker renew and save connection data. This persistence placement is a **source observation**, not a target design. |
| Builders and model handling | `internal/proxy/executor/transform.go:99-130,168-219`; `internal/translator/kiro.go:48-130`; `internal/proxy/gemini.go:19-101`; `internal/providers/registry_models.go:15-87`; `internal/translator/antigravity.go:375-384` | Codex builds Responses payloads; Kiro builds conversation-state requests; Antigravity wraps Gemini requests and rewrites model IDs. Catalog model sets are snapshots, not verified live availability. |
| Streaming and usage | `internal/proxy/executor/stream.go:36-105,406-450,479-648,747-939`; `internal/proxy/executor/claude_messages.go:48-173`; `internal/translator/responses_usage.go:5-41`; `internal/translator/gemini.go:500-522` | Codex/Claude SSE handling, Kiro AWS binary EventStream parsing, terminal Responses usage and Gemini usage metadata. Kiro usage in this path is estimated from output length, not exact provider usage. |

## Required OAuth flows

| Backend | Source implementation | Notable constraint / gap |
| --- | --- | --- |
| Codex | Authorization-code PKCE S256 via `auth.openai.com`; offline scope and Codex authorize parameters (`internal/handlers/oauth/pkce.go:61-82,113-176`). A loopback callback listener on the **fixed** `http://localhost:1455/auth/callback` keeps verifier/state and exchanges the code (`internal/handlers/oauth/codex_proxy.go:19-86,228-287,334-405`). Exchange is form-encoded; token expiry, refresh token and ID-token claims are stored (`internal/handlers/oauth/pkce.go:288-325,364-466`). Generic form refresh config includes Codex (`internal/providers/oauth.go:51-55,115-143`); account ID becomes upstream `chatgpt-account-id` (`internal/proxy/grokcli.go:56-72`). | Fixed callback conflicts with arbitrary dashboard redirects; token storage and callback/session lifetime must be redesigned around PestiRoute Core. Current upstream acceptance **UNKNOWN**. |
| Claude Code | PKCE S256 at `claude.ai/oauth/authorize`, with `code=true` and inference/profile scopes (`internal/handlers/oauth/pkce.go:48-60,113-176`). Token exchange sends JSON including state and verifier (`internal/handlers/oauth/pkce.go:288-309`); JSON refresh retains old refresh token when omitted (`internal/proxy/oauth/claude.go:12-76`). Claude OAuth/CLI-specific headers are configured in `internal/providers/providers.go:505-517`. | Do not confuse this login with ordinary Anthropic API-key auth. Current OAuth/client compatibility **UNKNOWN**. |
| Kiro AI | AWS SSO OIDC client registration and device authorization in a selected region, then polling; session includes client ID/secret and region (`internal/handlers/oauth/device.go:293-337,484-531,717-749`). Separately, Google/GitHub social PKCE uses Kiro desktop auth `/login` and `kiro://kiro.kiroAgent/authenticate-success`, with JSON token exchange (`internal/handlers/oauth/oauth.go:113-246`). AWS regional OIDC and social `/refreshToken` refresh paths differ (`internal/proxy/oauth/kiro.go:25-163`). | Refresh-token/CLI imports exist, but external-IdP refresh is explicitly not ported (`internal/handlers/oauth/kiro_import.go:20-30`; `internal/proxy/oauth/kiro.go:25-30`). Device and social are distinct flows; current acceptance **UNKNOWN**. |
| Google Antigravity | Google authorization-code with offline access, Cloud Platform and Cloud Code-related scopes; defaults to `http://localhost:8080/callback`, with redirect override (`internal/handlers/oauth/antigravity.go:18-29,56-103`). Shared callback page relays code to UI (`internal/handlers/oauth/callback.go:60-99,133-147`); server exchanges form-encoded code/client credentials and persists token/expiry, retaining prior refresh token if omitted (`internal/handlers/oauth/antigravity.go:106-155,182-284`). Google form refresh config is in `internal/providers/oauth.go:38-45,115-143`; project discovery/onboarding in `internal/handlers/chat/antigravity_project.go:104-193`. | **No PKCE or device flow is present in this inspected Antigravity authorize/exchange path** (`internal/handlers/oauth/antigravity.go:64-148`). Do not assume state/session validation or current upstream approval without verification. |

Provider quirks worth isolating: Codex Responses-lite and `/compact` shapes (`internal/proxy/executor/codex_responses_lite.go:14-22,58-129`; `internal/proxy/executor/transform.go:168-219`); Claude event-to-event Messages streaming and usage (`internal/proxy/executor/claude_messages.go:48-173`); Kiro `conversationState`, tool placement on last user turn and binary EventStream (`internal/translator/kiro.go:14-24,48-130`; `internal/providers/eventstream.go:9-29`); Antigravity `v1internal` envelope, project discovery and thought signatures (`internal/proxy/gemini.go:19-101`; `internal/translator/antigravity.go:603-652`). Focused tests exist for Codex callback/SSE, Kiro streaming and Responses usage (`internal/handlers/oauth/codex_proxy_test.go`, `internal/proxy/executor/kiro_test.go`, `internal/proxy/executor/stream_test.go`, `internal/translator/responses_usage_test.go`); **not run** in this research.

# Reusable knowledge

**REUSE as research input:** upstream auth endpoints, scopes and grant encodings; provider-specific headers, request envelopes and model ID rules; incremental stream/event parsing and usage-source distinctions; regression fixtures and quirks. Adapt only after validating against the selected client/provider version and PestiRoute's scoped Connector contract. The original source's provider-specific implementation location is not a target location.

# Not migrated

**DO NOT MIGRATE:** handler-owned combos, account fallback and model routing (`internal/handlers/chat/responses.go:91-123`); global executor dispatch (`internal/proxy/executor/init.go:5-86`); shared universal translation types and usage state (`internal/translator/types.go:10-46`; `internal/translator/usage.go:10-27`); provider logic in Core or client adapters. Do not copy error-path payload logging: `internal/proxy/gemini.go:117-135` writes request bodies to `/tmp` on upstream errors. Do not copy raw token persistence or callback ownership unchanged.

# Target mapping

| PestiRoute layer | Mapping, not an implementation claim |
| --- | --- |
| Protocol Adapter | Decode/encode the chosen client protocol (Responses, Chat Completions, Messages only if later scoped); preserve raw bytes in native mode. Never own backend OAuth or provider request translation. |
| Core Runtime | Own account selection, secret persistence, auth-session/PKCE state, refresh serialization, admission and exactly-once accounting. Never parse provider SSE, usage bodies or model internals. |
| Connector | Own provider OAuth steps, upstream auth headers, request translation, model metadata, incremental parsing and usage normalization; return output compatible with the admitted client protocol. |

# Unknowns

- License and provenance/redistribution rights of this checkout (and of any upstream-derived code); do not copy code until resolved.
- Current provider endpoints, client versions, OAuth policies/scopes and callback eligibility; no live traces or direct-vs-gateway compatibility checks were performed.
- Which registered providers/models/features actually pass current Responses, Chat and Messages client tests; retry/disconnect safety and exact usage per model/account.
- Antigravity PKCE outside the inspected path: **UNKNOWN**; inspected path has none. Kiro external-IdP refresh is not implemented in the cited refresher.
