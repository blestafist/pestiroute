# Source

Repository: local `.refs/9Router` and `.refs/9Gateway` (research sources only; see [9Router map](9router-migration.md) and [9Gateway map](9gateway-migration.md)).
Revision/commit: 9Router `14bb4f241350076984ad9a2ec639dd6cf7ae9de0`; 9Gateway `704d456b8da0434ce408d8f4873201f17c5b6f58`.
Date: 2026-09-28 (research date; source commit dates: 9Router 2026-09-28, 9Gateway 2026-09-19).

# Purpose

Provide a source-evidence matrix for the future Planner, not a PestiRoute support declaration. **Source implementation ≠ verified live compatibility ≠ implemented PestiRoute capability.** See [scoped capabilities](../implementation/CONTRACT.md#capability-model): only confirmed `supported` satisfies a required capability; otherwise `unknown`.

# Existing capabilities

The four independent fields below apply to the **documented source behavior**, not to every backend/model/protocol combination: **Research status** (`KNOWN` / `UNKNOWN`) records whether the cited behavior was studied; **Migration status** (`READY` / `NEEDS_VALIDATION` / `NOT_APPLICABLE`) records whether that knowledge can inform planning; **PestiRoute implementation status** (`NOT_STARTED` / `IN_PROGRESS` / `DONE`) records implementation progress; **Live verification status** (`VERIFIED` / `UNKNOWN`) records real client/provider testing. `READY` is readiness of *knowledge for planning*, not permission to copy code or a claim of deployment compatibility.

| Source/backend or feature | Client-facing evidence | Auth evidence | Stream / usage evidence | Research status | Migration status | PestiRoute implementation status | Live verification status |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 9Router Codex | Responses handler/bridge; Codex Responses builder (`internal/handlers/chat/responses.go:60-108`; `internal/proxy/executor/transform.go:99-130`) | PKCE + fixed loopback callback; generic form token refresh (`internal/handlers/oauth/pkce.go:61-82`; `internal/handlers/oauth/codex_proxy.go:19-86`; `internal/providers/oauth.go:51-55,115-143`) | Codex SSE and terminal Responses usage (`internal/proxy/executor/stream.go:479-648`; `internal/translator/responses_usage.go:5-41`) | KNOWN | READY | NOT_STARTED | UNKNOWN |
| 9Router Claude Code | Messages handler; translation for non-native routes (`internal/handlers/chat/chat.go:179-205`) | Claude PKCE/JSON exchange and refresh (`internal/handlers/oauth/pkce.go:48-60,288-309`; `internal/proxy/oauth/claude.go:12-76`) | Claude event parsing/usage (`internal/proxy/executor/claude_messages.go:48-173`) | KNOWN | READY | NOT_STARTED | UNKNOWN |
| 9Router Kiro AI | Chat-facing request to `conversationState` (`internal/translator/kiro.go:48-130`) | AWS device + social PKCE, separate refresh modes (`internal/handlers/oauth/device.go:293-337`; `internal/handlers/oauth/oauth.go:113-246`; `internal/proxy/oauth/kiro.go:25-163`) | Binary AWS EventStream; output-length usage estimate, not exact usage (`internal/providers/eventstream.go:9-29`; `internal/proxy/executor/stream.go:747-939`) | KNOWN | READY | NOT_STARTED | UNKNOWN |
| 9Router Google Antigravity | Gemini-envelope translation to `/v1internal` (`internal/proxy/gemini.go:19-101`) | Google auth code/offline, form exchange/refresh; no PKCE in inspected flow (`internal/handlers/oauth/antigravity.go:64-155`; `internal/providers/oauth.go:115-143`) | Gemini usage metadata including cached/reasoning (`internal/translator/gemini.go:500-522`) | KNOWN | READY | NOT_STARTED | UNKNOWN |
| 9Router other registered backends: registry/endpoints | Chat, Messages, Responses, models and media handlers exist (`internal/handlers/router.go:49-84`); registry includes generic and specialized executors (`internal/proxy/executor/init.go:5-86`) | Per-backend auth coverage not established by registration | Per-protocol/model/account feature parity not established by registration | KNOWN | READY | NOT_STARTED | UNKNOWN |
| 9Router other backends: individual flow parity | Per-backend/client combinations not mapped in this research | **UNKNOWN** | **UNKNOWN** | UNKNOWN | NEEDS_VALIDATION | NOT_STARTED | UNKNOWN |
| 9Gateway client access + limits | `/v1/*` bearer-key proxy; per-key policies (`internal/httpserver/authentication.go:34-64`; `internal/auth/policy.go:71-111`) | Pepper-HMAC virtual keys (`internal/auth/key.go:14-26,80-101`) | Request/token windows, admission leases (`internal/limiter/request.go:58-131`; `internal/limiter/lease.go:140-152`) | KNOWN | READY | NOT_STARTED | UNKNOWN |
| 9Gateway accounting + lifecycle | SQLite request history and startup/shutdown (`internal/storage/storage.go:124-172`; `cmd/gateway/main.go:115-256,386-478`) | Single upstream bearer replacement is source coupling, not a Core auth design (`internal/httpserver/server.go:819-837`) | Bounded async response observation; usage may be unknown (`internal/httpserver/usage_observation.go:333-445,468-496`) | KNOWN | READY | NOT_STARTED | UNKNOWN |
| Old gateway-to-9router coupling | Single upstream deployment (`README.md:3-12`) | Bearer replacement (`internal/httpserver/server.go:819-837`) | Gateway-side response parsing (`internal/httpserver/usage_observation.go:333-445`) | KNOWN | NOT_APPLICABLE | NOT_STARTED | UNKNOWN |

Paths in the matrix are relative to the named untracked `.refs/` checkout; no live smoke checks or source tests were run. Client protocol exposure does **not** imply that a particular backend supports each protocol or feature.

# Reusable knowledge

**REUSE:** use the 9Router per-backend wire/auth/stream/usage map to define future Connector-scoped fixtures; use 9Gateway key, limit, settlement and lifecycle behavior as Core design input. Treat unknown usage as unknown and estimated charges as estimates, not `usage.exact`.

# Not migrated

**DO NOT MIGRATE:** 9Router universal translators, fallback architecture and provider dispatch in Core; 9Gateway-to-9router coupling, gateway-side provider usage parsing and Chat response aggregation. No entry in this matrix authorizes implementation or asserts PestiRoute capability.

# Target mapping

| Protocol Adapter | Core Runtime | Connector |
| --- | --- | --- |
| Client formats and transport framing | Keys, scoped auth sessions/secrets, policy, selection, admission, usage reconciliation | Provider OAuth, request/wire translation, stream parsing, models and usage extraction |

# Unknowns

- Exact client and provider versions, current live OAuth eligibility and direct-vs-gateway test traces for all four named backends.
- For each protocol/model/account: supported streaming, tools, reasoning, vision, cached tokens and exact usage; do not infer support from provider registration.
- Source licenses and copy permissions; crash/restart and concurrent-account behavior under PestiRoute's contract.
