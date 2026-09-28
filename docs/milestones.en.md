# Milestones

## Overview

Work progresses from transparent request path to managed runtime. M1–M6 correspond to the PLAN phases; M0 adds a short preparation. Estimates below are guidelines for a single developer familiar with Go, not calendar commitments. Uncertainty is especially high for subscription protocols and translation.

| Stage | Outcome | Estimate |
| --- | --- | --- |
| M0 | Project scaffold and a fake upstream | 2–3 working days |
| M1 | Native Responses proxy | 4–7 days |
| M2 | Working Connector API boundary | 4–7 days |
| M3 | Accounts, virtual keys, usage and limits | 8–12 days |
| M4 | Anthropic translation and compatibility matrix | 8–15 days |
| M5 | Codex / Claude Code and local backends | Separate spike and estimate per protocol |
| M6 | External connector process runtime | 8–15 days after IPC spike |

M3 completes a minimally viable native version for standalone use. M4 validates the core architectural assumption about translation. Support for external third-party plugins appears after M6.

## M0 · Project Foundation

Create Go module, entrypoint, configuration loader, and minimal health/readiness endpoints. Add a fake upstream capable of returning JSON and managed SSE streams, delaying chunks, breaking connections, and observing cancellation. Lock down the toolchain and basic CI.

**Done when:** binary starts from clean checkout with a documented command; invalid configuration stops startup with a clear error; local test passes without external credentials. Health means the process is live; readiness means it's ready to accept requests with loaded configuration.

## M1 · Transparent Responses Proxy

Implement `POST /v1/responses` with one explicitly configured target, bounded request body, native forwarding, and cancellation. Place backend-specific code in a separate package from day one, even before the final Connector API. Use existing upstream model ID without alias rewrite for smoke tests.

**Done when:** request body bytes, including unknown fields, reach the fake upstream unchanged; response bytes are preserved; first chunk reaches client before upstream response completes; tool IDs and event order are unchanged. Client disconnect closes upstream request; late upstream failure does not trigger second response. One documented smoke test is executed with a real client using tools and parallel tool calls.

## M2 · Connector API and Conformance Baseline

Extract execution envelope, registry, descriptor, stream frames, typed errors, and runtime services. Move M1 connector behind a common interface. Add model/capability eligibility, explicit routing, and attempt recording. Implement a test connector with predictable failures so routing is not only verified by a successful HTTP proxy.

**Done when:** Core does not import concrete connectors and protocol parsers; conformance harness runs against native and fake implementations; M1 native regression suite stays green. An unknown capability does not satisfy a mandatory requirement, and a malformed stream sequence is recorded as a runtime error. The scoped contract is concrete enough for implementation without hidden global state.

## M3 · Access, Accounts and Accounting

Add SQLite schema/migrations, secret storage, accounts, and a CLI for managing virtual keys, key policies, and usage records. Implement reservations, reconciliation, recovery after restart, and bounded fallback only for safe failures. OAuth runtime interfaces are prepared here; real provider flows are validated in M5.

**Done when:** a revoked key does not reach upstream; concurrent requests do not bypass the admission limit; usage is not duplicated on repeated completion; interrupted attempts recover correctly. Credentials survive restart in encrypted storage; the master key is not stored in the database. Fallback respects key restrictions and does not execute after commit or ambiguous delivery.

## M4 · First Translation Connector

Implement Anthropic API connector as explicit Responses ↔ Messages transformation. First verify plain text and streaming, then tools, parallel tool calls, multiple tool rounds, tool choice, usage, and supported reasoning behavior. Record supported/unsupported/unknown status and transformation constraints for each feature.

**Done when:** conformance suite confirms claimed capabilities; tool call IDs and tool results are correctly linked across rounds; streaming events have a proper lifecycle. Unsupported fields with significant semantics are rejected with a clear error, not silently lost. No Anthropic model or error code branches appear in Core.

## M5 · Subscriptions and Local Models

For Codex and Claude Code, start with a short research spike: pin client sources/version, auth flow, request format, streaming, and minimal trace. After that, implement each connector separately, including refresh and account affinity. Gemini CLI follows the same process by priority.

Ollama/vLLM are first verified through a common compatible connector. If a backend only provides Chat Completions, either a declared Responses translator or a separate connector is needed; URL-style match alone does not mean ready support.

ACP is considered separately: it is a protocol for communicating with an agent process, not equivalent to a subscription backend API. The spike must identify a concrete agent, transport, and owner of the agent/tool loop before including ACP in the implementation scope.

**Done for each connector when:** fresh authorization, expiry, and concurrent refresh are verified; connector does not persist its own secrets; versioned fixtures and a capability matrix are added; the direct-versus-gateway smoke test is reproducible. Unknown stateful capabilities are explicitly marked. Each connector can be released independently of others.

## M6 · External Plugin Runtime

Compare stdio framing and gRPC over Unix socket on one scenario with a long stream and cancellation. After selection, implement handshake, process supervision, scoped host services, bounded queues, shutdown, and restart backoff. Pass the same native connector through the process boundary.

**Done when:** a crashed connector does not terminate the gateway; a slow consumer does not cause unbounded memory growth; an incompatible contract version is rejected before execution; cancellation stops work and releases process resources. Conformance results for in-process and external implementations match. Already-started generation cannot be automatically restarted after a crash.

## First Task After Documentation

Start with M0 and one M1 integration test: a fake upstream accepts JSON with an unknown field and returns two SSE chunks with a controlled pause. The test simultaneously verifies payload preservation, immediate delivery of the first chunk, and cancellation. This creates a useful baseline before full auth, storage, and plugin SDK appear.
