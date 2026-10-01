# Current State

## Verified Baseline

- FND-001 Go module scaffold, FND-002 probe-only server, FND-003 controllable local fake upstream fixture, and FND-004 shared local/CI workflow are verified locally. M1-001 end-to-end loopback POST /v1/responses fixed JSON passthrough is verified. Hosted CI has not run.
- FND-005 records the historical M1 compatibility baseline selection in [References](REFERENCES.md#m1-compatibility-baseline); the current smoke model is `gpt-5.4-mini` (synchronized locally under M1-028; real inference remains an M1 smoke gate).
- [CONTRACT.md](CONTRACT.md) is the accepted v1 semantic boundary for Protocol Adapters, Core Runtime, and Connectors, with ADR change control. [M1-BINDING.md](M1-BINDING.md) specifies the minimal Go execution signatures, startup JSON subset, capability checks, and bounded streaming observer. Language/IPC bindings are not implemented; M3 configuration schema remains draft.
- Documentation is organized by project intent and implementation; the root [AGENTS.md](../../AGENTS.md) defines the agent entry path.

## Immediate Focus

M1-024 direct real-client tool baseline is DONE: single-tool continuation captured; two parallel calls and matching client results observed, but direct backend continuation remains not demonstrated. M1-029 and M1-030 are DONE with scoped `llm.tools`/`llm.reasoning` support for the exact target. M1-025 gateway tool smoke is DONE: single tool continuation (1,977 tokens across 2 rounds) and two parallel calls/matching continuation (2,238 tokens) verified after offline observer test; `llm.tools.parallel` remains unknown as OpenCode omitted `parallel_tool_calls`; five single attempts deviation recorded. Total quota of 5 initial smoke tasks complete. User authorized finishing M1-026, M1-003, and M1-027 plus prerequisite M1-031. M1-031 is DONE (fixed JSON idle timeout and upstream cancellation implemented, dispatch cancellation test race synchronized, race and offline checks verified). M1-026, M1-003, and M1-027 remain DRAFT; M1-026 preparation awaits next planner session. Unrelated `.opencode/agents/git-worker.md` edit preserved unstaged.

M1 has [31 bounded cards](TASKS.md#m1--transparent-responses-proxy-planning-candidates); assign one READY card per worker session, never the milestone. Real-provider smoke remains a separate gate. Task statuses and dependencies live only in TASKS.

## Open Inputs

- The selected OpenAI public Responses / OpenCode V2 baseline requires M1 access and live verification; account access/credit and actual parallel/cancellation behavior are unknown. Local scaffold work does not need provider credentials.
- Local 9Router/9Gateway revisions and source maps are recorded in [References](REFERENCES.md#migration-from-9router). Reproducible repository provenance and code-reuse licenses remain unresolved before code migration; live compatibility is unverified. These do not block M0.

Other architectural questions remain in [DECISIONS.md](../project/DECISIONS.md#questions-before-implementation). The `/next n` OpenCode command describes a bounded agent workflow; it does not provide a persistent scheduler or change task readiness rules. [Tooling adoption](TOOLING.md) records conditional future additions.

## Available Checks

`git diff --check` is available for patch whitespace. Documentation verification also checks local link targets/anchors, fenced blocks, and task registry consistency; no persistent checker command has been added.

With local Go 1.27.1, `./scripts/check.sh` passes: whitespace, `gofmt -l cmd internal`, vet, tests, race tests, and offline artifact-free build. In an isolated HEAD export, offline build and direct binary startup passed: `/healthz` and `/readyz` returned 200; SIGTERM exited 0 in <3s; malformed addresses and timeouts returned non-zero with stderr. M1-022 additionally verifies isolated-source build and fake-target POST using synthetic credentials; `go test -count=1 -v -run 'Test.*Baseline' ./cmd/gateway/...` records local TTFB/heap/goroutine observations (see task evidence; no budgets). M1-023 verifies exact OpenCode 2.0.6 plural-provider loopback HTTP/SSE probe via `OPENCODE_BIN=<2.0.6 binary> ./scripts/smoke-m1-dry-run.sh` inside an isolated loopback-only user/network namespace. `GOTOOLCHAIN=local go run ./cmd/gateway -listen invalid:address` also failed clearly. `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race ./internal/testutil/fakeupstream/...` passes byte capture, gated SSE, drops and cancellation. CI workflow invokes the same script on pinned Go 1.27.1; hosted execution and actionlint remain unrun. `gofmt -l .` reports an unrelated pre-existing file under `.refs/9Router/`.

## Update Rule

Replace this snapshot when actual capabilities, immediate focus, blockers, or verified commands change. Keep task completion evidence in its card and status in TASKS; do not append a running work log here.
