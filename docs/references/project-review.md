# Pre-Implementation Review — 2026-09-28

## Scope and Result

Reviewed all 18 tracked Markdown documents and `.gitignore`: product scope, architecture, accepted v1 contract and ADRs, extensibility, configuration, stack, roadmap, task registry/template, testing, current state, and source research. The working tree was clean at review start. No application source, Go module, executable tests, or CI exists yet. This is a design/documentation review, not runtime verification or a new audit of the external `.refs/` projects.

The architecture is internally viable as a starting point: opaque payloads, Connector-owned translation, scoped credentials, conservative replay, and single-owner attempt finalization are consistently required by the normative contract. The fixes below remove implementation traps; they do not change `CONTRACT.md` or accepted ADR semantics.

## Findings Fixed

| Priority | Finding and consequence | Correction |
| --- | --- | --- |
| High | M1 referred to a future final Connector API and M2 to extracting the execution contract. This could lead to a disposable proxy with incompatible lifecycle/commit semantics. | [Roadmap](../implementation/ROADMAP.md#m1--transparent-responses-proxy) now requires v1 semantics in the first slice; M2 completes bindings and conformance. |
| High | The proposed HTTP stack omitted hidden transport/SDK replay and redirects. A single Execute could resend inference or contact an unselected destination without Core retry authorization. | [HTTP guidance](../implementation/STACK.md#http-and-streaming) explicitly requires disabling redirects/SDK retries and auditing transport replay; deterministic cases added to [Testing](../implementation/TESTING.md#testing-levels). |
| High | Default compression and blind header forwarding could violate native byte preservation or leak gateway credentials. | HTTP guidance and verification now explicitly cover content encoding/length, gateway credentials, and Connection-nominated hop-by-hop headers. |
| Medium | Architecture wording assigned key validation to the adapter and suggested output conversion there, inviting duplicated authorization or translation. | [Adapter responsibilities](../project/ARCHITECTURE.md#client-protocol-adapters) and [request lifecycle](../project/ARCHITECTURE.md#request-lifecycle) now clearly retain Core-owned key verification and Connector-owned translation. |
| Medium | “Client stops reading” was a cancellation test, although a stalled reader may only trigger backpressure. | Tests distinguish explicit disconnect/cancellation from bounded slow-reader behavior. |
| Medium | CURRENT/REFERENCES claimed source research was absent, despite committed source maps; one Markdown link depended on an ignored checkout. | Entry documents link existing research, preserve unresolved provenance/license/live-verification status, and no longer depend on an ignored file link. |

## Gates Still Requiring Decisions or Evidence

- **Before FND-001 becomes READY:** select module path and pinned Go version, prepare its card and exact checks. The existing DRAFT registry is accurate; no implementation task is executable yet.
- **Before the M1 compatibility claim:** choose a real Responses-native backend and client/version; define the supported POST/streaming surface and stateful restrictions. Native forwarding does not establish resource API coverage or cross-account affinity.
- **Before binding/conformance acceptance:** specify concrete cancellation/close behavior, bounded queues and deadlines, then prove Head handoff prevents replay, discarded attempts release resources, and every termination finalizes once. These are verification obligations under v1, not reasons to redesign it.
- **Before M3 accounting acceptance:** choose concrete reservation/window and unknown-output budgeting policy, and verify crash recovery and concurrent reconciliation. Estimates are not hard consumption bounds; the existing configuration draft must not be presented as a proven limiter.
- **Before source code migration:** resolve reproducible repository provenance and licenses; the source maps are research evidence only. Provider/client compatibility and external source test results remain unverified.

These gates belong to their existing milestones and task preparation. No new implementation scope or contract extension is approved by this review.

## Verification

- Read every tracked project document; checked terminology, ownership boundaries, milestone ordering and task dependencies against v1.
- Local Python checks passed: 19 Markdown files (including this report), 75 relative links/anchors, fenced-block closure, and the four registered task dependency references/cycle absence.
- `git diff --check` passed for patch whitespace.
- No build, Go tests, race tests, provider smoke tests or external source tests were run: this repository has no implementation to exercise. Added test scenarios are requirements, not passing results.

## Follow-up: Critical Review of the M0 Candidates

Reviewed each candidate after recording the high-level M0 backlog. All six remain planning inputs; no task card or executable acceptance claim was introduced.

| Candidate | Challenge | Review result |
| --- | --- | --- |
| FND-001 | Does a scaffold force a speculative framework or unsettled toolchain? | Keep only module identity, pinned Go, entrypoint and reproducible build. Planner resolves choices before READY; no Connector SDK or directory framework is needed here. |
| FND-002 | Does startup drag M3 configuration/storage into M0 or falsely claim inference readiness? | Keep a minimal startup configuration subset. Clarified that readiness concerns the implemented service, not provider health or inference eligibility. |
| FND-003 | Can the fixture actually support the first byte-preservation/streaming regression? | Added request capture; clarified deterministic synchronization and observable cleanup. Fixture behavior is verified in M0; the gateway integration remains M1. |
| FND-004 | Must CI wait for every feature, and is scaffold-only green CI enough? | Its only prerequisite is the scaffold, allowing checks early. Explicitly require rerunning those checks on the assembled M0 in FND-006. |
| FND-005 | Does baseline research imply proven compatibility or block local work on credentials? | Clarified concrete intended backend/model/client selection, evidence and prerequisites. Live execution belongs to M1; missing access is recorded without blocking independent local tasks. |
| FND-006 | Is this a second implementation task or merely an unchecked milestone label? | Keep one outcome: evidence-backed M0 closure and M1 planning handoff. Strengthened its check to cover assembled clean-checkout startup, invalid configuration and local/CI verification without provider credentials. Defects return to their owning tasks. |

The dependency graph has no cycle: scaffold precedes local implementation/check setup; baseline research is independent; the final handoff joins completed prerequisites. No accepted contract, milestone deliverable, or existing task identity changed. Planner retains concrete design and task-card preparation.

Follow-up verification passed: 19 Markdown files, 78 local links/anchors, fenced blocks, six unique DRAFT task rows with valid acyclic dependencies, and no prematurely created task cards. `git diff --check` passed. Implementation checks remain unavailable.
