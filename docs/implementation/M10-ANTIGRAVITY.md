# M10 — Google Antigravity Connector

## Outcome

An evidence-scoped Antigravity subscription backend behind the existing Connector
contract, starting with research and a deterministic vertical slice. Branch
`dev-m10-antigravity`; first task M10-001. No M8 live dependency.

## Sequence and gates

1. Pin current official auth/product documentation and inspect reproducible
   source references. Identify the personal subscription backend separately
   from Gemini API-key, enterprise/ADC, Gemini CLI, SDK and agent-process paths.
   Pin source revisions and licenses before reuse; unavailable source does not
   prohibit an independently written synthetic implementation, but remains an
   explicit evidence limit.
2. Bind account/project discovery or onboarding, OAuth/refresh, model identity,
   request envelope, streaming, usage and errors. Distinguish observed wire
   evidence from third-party assumptions. Map Responses directly inside this
   Connector; never add universal messages or provider logic to Core.
3. Record a GO/NO-GO for one minimal slice, explicit unsupported fields and
   Unknowns. Add sanitized fixtures with provenance. No invented client IDs,
   scopes, endpoints or beta/identity headers presented as verified protocol.
4. Implement only the reviewed fake-backend text/stream slice; prove first
   event before completion, cancellation, bounds, usage and replay safety.
5. Publish the offline matrix and prepare the next bounded auth/tools/history
   cards from the findings. Real account validation and production enablement
   remain separate later gates. Do not expand a mock into a production claim.

Later outcomes, intentionally not pre-decomposed: connector OAuth using the
existing coordinator/encrypted storage; project/account binding; tool rounds
and thought-signature handling; scoped model/quota metadata; protected gateway
composition; versioned direct-versus-gateway evidence and operator runbook.
No fallback to a paid API, account rotation, or quota bypass is implicit.

## State and identity constraints

Thought/reasoning signatures and tool identifiers remain connector-owned. If
replay requires state that the opaque contract cannot preserve, propose an ADR
and compatibility impact before implementation; do not silently omit it.
Project discovery/onboarding side effects and retry rules must be documented;
no hidden provisioning or infinite retry. Runtime chooses the account and owns
secrets, refresh coordination, admission and accounting. Connector owns provider
wire formats, token exchange, usage extraction and model validation.

## Ownership

Own `internal/connector/antigravity/**`, dedicated conformance fixtures/tests,
`docs/implementation/M10-*`, `tasks/M10-*` and the M10 registry/CURRENT section.
Research can read `docs/references/9router-migration.md`; copying old Core/router
architecture is excluded. M11 owns composition/shared admin files: request a
bounded sequential integration window after the standalone slice is reviewed.
M10 does not edit frontend, SQLite schema, CI or Core contracts unilaterally.

## Verification and completion

Initial checks are reproducible document/source audits. Later tasks must add
executable commands once their package exists. Deterministic conformance is
mandatory before gateway wiring; full hosted CI covers integration. No live
calls are authorized by this plan. Production support requires separately
recorded applicable auth/refresh, streaming/tools/state, cancellation and usage
evidence. If blocked, deliver the research finding and next unblock condition.

## Starting references

Checked 2026-10-17: [official authentication](https://antigravity.google/docs/install?tab=cli)
and [enterprise distinctions](https://antigravity.google/docs/enterprise).
They document product/auth surfaces, not a supported PestiRoute backend wire
contract. The [local migration map](../references/9router-migration.md) records
an older inspected CloudCode/v1internal path and unresolved provenance/license
limits. Revalidate it against pinned current sources; do not assume continuity.
