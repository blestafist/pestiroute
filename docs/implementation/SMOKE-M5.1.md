# M5.1 Codex live smoke procedure

This is the bounded live-smoke procedure. M5.1-040/041 record its executed
auth/inference evidence and limits; see the [compatibility matrix](M5.1-COMPATIBILITY.md).
This procedure uses the selected subscription Codex endpoint, never the public
OpenAI API or a compatible proxy. It is not evidence of broader capability or
account entitlement.

Normative context: [candidate profile and dialect policy](M5.1-BINDING.md#candidate-profile-codex-responses-http-sse-v1),
[live acceptance gate](M5.1-CODEX.md#local-versus-live-acceptance),
[direct-versus-gateway invariants](TESTING.md#direct-versus-gateway), and the
[bounded artifact validator](../../scripts/smoke-m5.1-artifacts.py).

## Fixed selection

- Client: Python `http.client`, CPython **3.14.7**. Pin this exact client for
  both legs; if unavailable, stop and revise/review this procedure rather than
  silently substituting a client. Record `python3 --version` and the gateway
  source revision (`git rev-parse HEAD`) in sanitized run notes.
- Request profile: opt-in Lite HTTP/SSE Responses profile
  `codex-responses-http-sse-lite-v1` (`openai.responses.v1`); no WebSocket,
  hosted tools, or compatible endpoint. The gateway route uses this exact
  profile, native mode, and configured model `gpt-6-luna`. The existing standard
  `codex-responses-http-sse-v1` profile remains the default and is not changed.
- Model: exact `gpt-6-luna`; do not substitute another catalog result.
- Account: one operator-selected, eligible ChatGPT subscription account. Record
  only a local alias (for example `selected-A`), never its ID, email, token, or
  private account metadata. Direct and gateway must use this same account.
- Direct inference URL: `https://chatgpt.com/backend-api/codex/responses`.
  Gateway inference URL: the configured local listener at `/v1/responses`,
  routed to that same Codex profile/account. Auth legs use the configured Codex
  device-auth flow through the official auth endpoint, not inference.
- Lite flag: the gateway Connector injects exactly
  `x-openai-internal-codex-responses-lite: true` from trusted profile config;
  the direct harness sends the same fixed flag for parity. Caller values,
  including case variants, cannot select or override it. For Lite only, both
  legs use honest `originator: pestiroute` and `User-Agent: PestiRoute` values,
  not an official client identity or version. Generate a fresh random UUID per
  attempt and place the same UUID in `session-id`, `thread-id`, and
  `x-client-request-id`. These are ephemeral request-correlation labels, not
  persistent sessions or a Core session capability. Suppress caller-supplied
  identity/correlation values including case variants; never log their values.
  Direct upstream header names also include `Authorization: Bearer <selected-account OAuth
  access token>` and `ChatGPT-Account-Id: <selected account ID>`. The gateway
  client sends only `Authorization: Bearer <gateway virtual key>`; the gateway
  constructs the two selected-account upstream headers from its scoped
  credential. Never copy upstream credentials into the gateway client request
  or record either value.
  These additional identity headers align the direct and gateway legs to the
  known successful direct recipe, but their necessity is unproven. The paired
  runner's omission does not establish the cause of its HTTP 400; provider reason
  remains unknown. Standard-profile headers stay unchanged. Record only actual
  harness/runtime versions; never infer or fabricate Codex-client/provider
  versions from these headers.

Before any later live authorization, confirm the operator has authorized the
selected account and bounded requests, the subscription can incur the expected
usage, the candidate model is available, and network/client access is permitted.
Ensure account credentials are already valid and recovery access is available.
No production claim or feature admission follows from this procedure.

## Bounds and non-negotiable stops

Run one fresh, disposable client session per scenario, at most two client
requests per leg, one attempt per request, and a 30-second wall-clock deadline
per request. Cancel the client and stop the leg on deadline, 1 MiB received
response bytes, or the client-side output-text safety limit. Enforce
the byte ceiling in the CPython reader by counting every response-body byte
before parsing; read no more than the remaining allowance plus one sentinel
byte. At the first byte over the cap or deadline, close the response and
connection, record the bound hit in run notes, and stop that scenario. Do not
reconnect or replay the request. Use `stream:true` and `store:false`; the
subscription backend rejects `max_output_tokens`, so omit it from every wire
request. The artifact's `max_output_tokens` metadata records only the client's
output-text safety setting, not a request field or backend-enforced limit. The
client conservatively cancels after at most 256 UTF-8 output-text bytes (or a
smaller explicitly selected text limit); this byte-count ceiling is the safe
upper-bound estimator for observed output tokens, not a bound on provider
generation or billing. The task's maximum configured client-side target is
4096; do not claim that the backend honored it. The independent time and full
response-byte bounds remain in force. Do not add dialect-specific fallback
fields. A 4xx (including rejection of the profile, model, or any
option), redirect, timeout, unexpected status, malformed SSE, or bound hit ends
that scenario: no second attempt, payload weakening, Lite switch, API-base
change, retrying SDK, or alternate/account fallback. Never auto-retry an
ambiguous delivery or a committed stream.

Stop the whole gate on account mismatch, credential/auth ambiguity, unexpected
dispatch, possible paid/unbounded usage, token/private-data exposure, any
request not matching the selected profile, or inability to enforce the stated
client-side deadline/byte bound. Record the concrete reason as BLOCKED; do not
call a compatible API endpoint a substitute. If a model returns one call when
parallel calls were permitted, record what happened: it neither proves nor
disproves parallel support.

## Separate auth leg

M5.1-040 performs this only after its own authorization and readiness check.
Normally capture direct and gateway device-start/continue results for the same
selected account, at most one start and one continuation per leg, without
recording device/user codes or tokens. The operator-approved M5.1-040 exception
defers the duplicate independent direct browser login: retain only the truthful
single gateway record and explicitly mark the comparison deferred; never create
a synthetic direct record. Identify CLI completion as operator-reported and
read-only offline decrypt/restart verification as locally measured. Verify
authorization scope, selected account binding, persistence acknowledgement,
expiry metadata, and usability after restart using sanitized boolean/status
observations only.
Refresh is a separate selected-account operation: do not expire/revoke/delete a
valid credential or manufacture a refresh failure. Exercise refresh only when
the selected account's normal proactive-refresh condition is met and the
credential is still valid; verify successful persistence/expiry and retain the
rotated credential. Otherwise mark live refresh unrun and cite synthetic local
refresh/concurrency/recovery tests separately. Stop on account mismatch,
unexpected auth redirect, ambiguous continuation, failed persistence, or any
need to expose opaque auth state. Validate a regular redacted pair with
`scripts/smoke-m5.1-artifacts.py --auth`; validate the approved single-leg
exception with `scripts/smoke-m5.1-artifacts.py --auth <path> --gateway-only`.
No secret-bearing output or raw auth transcript is retained.

## Paired inference leg

For each scenario, use the same client version/model/options and identical
synthetic instructions/tool definitions. Histories are equivalent, not
byte-identical where generated IDs or encrypted bytes differ: preserve each
leg's returned IDs and state within that leg; never splice sessions. Use
harmless synthetic text only. Do not compare generated prose across independent
model runs. The client returns only synthetic tool results; the gateway never
executes tools. Record no prompts, arguments, headers, or raw bodies.

This DEC-011 smoke scope is the matched `plain_text` request only. It does not
run tool or parallel-tool scenarios under Lite; keep `llm.tools` and
`llm.tools.parallel` `Unknown` for both profiles and reject requests requiring
them before dispatch. The Lite body retains `reasoning.context: all_turns`;
the direct proof used `reasoning.effort: high`. The narrowly scoped Lite native
reasoning capability is supported by the direct completion and matched pair;
standard-profile reasoning remains `Unknown`. Direct POST path is
`/backend-api/codex/responses`; gateway client POST path is `/v1/responses`.
Both legs use the same native Responses body, without Lite conversion.
`max_output_tokens` is deliberately absent:

The caller forms the Lite-shaped body, including its `additional_tools` input
prefix and any instruction items in `input` where applicable; the Connector
does not synthesize, move, or strip these fields. The old preflight that omitted
reasoning was rejected with field `reasoning.context`; this does not show the
model is unsupported. The sanitized direct capture
`/tmp/opencode/pestiroute-live-evidence/luna-7-completion.json` records HTTP 200,
completed through EOF, one reasoning item, and 27 reasoning tokens, with effort
`high` and context `all_turns` (reviewer PASS `ses_eeff1a4c6ffex6A3uqMCHiJXCp`).
The official catalog default is medium, but high is the observed proof setting;
do not substitute a different effort into that evidence. The matched pair in
M5.1-041 additionally verifies one direct and one gateway plain-text response.

```json
{"model":"gpt-6-luna","stream":true,"store":false,"instructions":"","reasoning":{"effort":"high","context":"all_turns"},"include":["reasoning.encrypted_content"],"input":[{"type":"additional_tools","id":"at_<caller-formed-uuid>","role":"developer","tools":[]},{"type":"message","role":"user","content":[{"type":"input_text","text":"What is 137 × 293? Reply with only the number."}]}],"tool_choice":"auto","parallel_tool_calls":false}
```

This pair body includes the selected reasoning fields and caller-formed
`additional_tools` UUID prefix; it also explicitly sets automatic tool choice
and disables parallel tool calls. It has no top-level tools. Choose the
caller-owned prefix value once and send the exact same constant request body in
both legs; it is distinct from each leg's fresh header-correlation UUID. The
Connector must forward the caller's body byte-for-byte. For each leg, share that
attempt's one UUID across its three correlation headers, but generate separate
UUIDs for the separate direct and gateway attempts. Do not bypass capability
gates. M5.1-041 records the direct completion, matched gateway pair, and offline
scoped passthrough/event-order/usage evidence supporting `llm.reasoning:
Supported` only for the exact native Lite model and account scope. This does not
promote tools/parallel, standard reasoning, or other Lite options, and does not
close M5.1-043.

The artifact validator retains the existing `subscription_codex` profile and
accepts its existing `gpt-5.4-mini` artifacts plus the DEC-011 `gpt-6-luna`
plain-text pair without changing the strict artifact field schema. Luna Lite is
limited to `plain_text`; other scenarios are rejected. This schema support and
offline implementation do not constitute paired live evidence or a working-route
claim. Do not fabricate or relabel artifacts.

For tool scenarios use this harmless tool definition and the exact synthetic
instructions below; parallel preservation adds the explicit
`"parallel_tool_calls":true` option, never a Lite-specific field:

```json
{"type":"function","name":"smoke_lookup","description":"Return a fixed synthetic marker.","parameters":{"type":"object","properties":{"key":{"type":"string","enum":["alpha","beta"]}},"required":["key"],"additionalProperties":false},"strict":true}
```

- Function round: `Call smoke_lookup once with key alpha.`
- Parallel-preservation round: `Call smoke_lookup with key alpha and also with key beta; make both independent calls.`
- Encrypted-reasoning follow-up: `Continue.`

Represent user and function history as these concrete Responses input items;
replace placeholders only with IDs/arguments returned by that same leg:

```json
{"type":"message","role":"user","content":[{"type":"input_text","text":"Call smoke_lookup once with key alpha."}]}
{"type":"function_call","id":"<returned item id>","call_id":"<returned call id>","name":"smoke_lookup","arguments":"<returned arguments>"}
{"type":"function_call_output","call_id":"<matching returned call id>","output":"{\"marker\":\"synthetic-alpha\"}"}
```

For the reasoning follow-up, include the complete chronological prior input and
the returned `reasoning` item unchanged, including `id`, `summary`, and opaque
`encrypted_content`, then the following user item:

```json
{"type":"reasoning","id":"<returned item id>","summary":[{"type":"summary_text","text":"<returned summary text>"}],"encrypted_content":"<opaque returned value>"}
{"type":"message","role":"user","content":[{"type":"input_text","text":"Continue."}]}
```

Request `include:["reasoning.encrypted_content"]` on the initial and follow-up
requests. Keep ciphertext in ephemeral memory only; never print or persist it.

Return only `{"marker":"synthetic-alpha"}` or
`{"marker":"synthetic-beta"}` in the corresponding
`function_call_output`, preserving the response's own `call_id` and chronological
history. Do not add a second call if the model emits one; the paired artifacts
describe observed calls, not intended calls.

1. **Text (only DEC-011 Lite scenario):** `Reply with the word OK.` Compare status, first-event delivery before
   completion, ordered SSE event categories, terminal outcome, usage presence,
   and actual dispatch count.
2. **Function round (out of this Lite scope):** production `llm.tools` is `Unknown`; expect gateway HTTP
   400 `unsupported_capability` before dispatch and zero upstream dispatches. Record
   sanitized error/status plus trusted zero-dispatch evidence. A separately
   authorized direct probe can use the instruction and history shapes above;
   compare call/result identity and order there only. Do not claim a successful
   paired gateway round or enable production tools.
3. **Parallel preservation (out of this Lite scope):** production `llm.tools` and
   `llm.tools.parallel` are `Unknown`; expect gateway HTTP 400
   `unsupported_capability` before dispatch and zero upstream dispatches. A
   direct probe, if separately authorized,
   explicitly sends `parallel_tool_calls:true` and compares any returned
   interleaved IDs/deltas/results. Rejection is not permission to drop the
   field. A single emitted call is not evidence of parallel support.
4. **Encrypted reasoning follow-up (not covered by the completed pair):** the
    scoped Lite `llm.reasoning` capability does not establish multi-round
    encrypted-history continuation. Any separately authorized probe must use
    the exact `include` and history item shapes above, retain ciphertext only in
    ephemeral memory, and record only sanitized outcomes. If unrun, keep this
    scenario unverified; never fabricate a follow-up. No ciphertext is logged or
    captured. Standard-profile reasoning remains `Unknown` and its production
    zero-send rejection is covered by the local regression.

The following standard-profile reasoning shape is outside this DEC-011 Lite
smoke scope; standard-profile reasoning remains `Unknown`, and no such request
is sent as part of the Luna pair.

The reasoning-only gate must actually request the capability: include the
top-level `"reasoning":{"effort":"medium"}` object on **both** initial and
follow-up requests, along with `include:["reasoning.encrypted_content"]`. The
initial shape is:

```json
{"model":"gpt-5.4-mini","stream":true,"store":false,"reasoning":{"effort":"medium"},"include":["reasoning.encrypted_content"],"instructions":"Initial instruction.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Begin."}]}]}
```

The follow-up repeats the same top-level reasoning/include fields and
instructions; its `input` is the exact prior chronological input plus the
returned encrypted `reasoning` item and this user message:

```json
{"type":"message","role":"user","content":[{"type":"input_text","text":"Continue."}]}
```

The existing local `TestCodexEncryptedReasoningAndHistoryReplay` verifies
production-connector HTTP 400 `unsupported_capability` and zero upstream sends
for the initial reasoning request. A separate synthetic test-only connector
exercises two history-replay rounds; this does not establish that production
accepted this exact follow-up envelope and is not live compatibility evidence.

This procedure does not create a smoke-only capability admission. Existing
M5.1-029/M5.1-030 local fixtures are test-only synthetic capability declarations,
not a production route or an admission mode for live smoke. Do not add a
production bypass or claim new capability support.

For the successful text pair compare redacted request field presence/options,
status, permissible event order, early delivery, terminal outcome, usage
semantics, and dispatch/retry counts. For direct-only tool/reasoning probes,
compare only the observed history/identity preservation described above; do not
compare generated prose, dynamic IDs across runs, timestamps, or ciphertext.
For production gateway feature-gate cases compare expected status/error code
and zero dispatch. Record failures and unrun scenarios explicitly; unknown
remains unknown.

Production feature-gate evidence is distinct from compatibility evidence:
retain only sanitized client status/error code and trusted local upstream
dispatch count zero. Do not create successful gateway artifacts or set a
production capability `Supported` from a direct probe or harness-only admission.

## Missing-access disposition

If selected-account access, operator authorization, network access, or the
fixed client/profile input is missing, do not create a synthetic live artifact.
For auth, update `docs/implementation/tasks/M5.1-040.md`; for inference, update
`docs/implementation/tasks/M5.1-041.md`: set its TASKS row and card status to
`BLOCKED`, and replace `## Open Questions / Blocker` with these exact fields:

```text
Reason: <specific missing access/input; no secrets or account identifiers>
Unblock condition: <specific authorization/access/input required>
Evidence: no live request made; no live artifact created
Attempted scope: <auth flow or inference scenario not run>
```

Update `docs/implementation/CURRENT.md` only if immediate focus changes; state
the blocked card and unblock condition. Keep local fake evidence in M5.1-039's
Completion Evidence. Never fabricate a `subscription_codex` artifact to satisfy
the validator.

## Local-only dry-run and artifact checks

This card's executable check is offline and never uses the fixed live URLs:

```sh
set -eu
umask 077
tmp=$(mktemp -d /tmp/opencode/m5.1-039.XXXXXX)
trap 'rm -rf "$tmp"' EXIT
python3 -B scripts/smoke-m5.1-artifacts.py --self-test --output-dir "$tmp"
python3 -B scripts/smoke-m5.1-artifacts.py "$tmp/direct.json" "$tmp/gateway.json"
python3 -B scripts/smoke-m5.1-artifacts.py --auth "$tmp/auth.json"
```

The fake harness proves only local schema/redaction, paired observations, and
loopback stream behavior. It does not execute live auth, model calls, encrypted
reasoning, or establish account entitlement. Future live artifacts must use
`endpoint_profile: subscription_codex`, the exact client/version and model,
scenario fixture IDs, and the validator's strict allowlist. Keep only the
validator-approved status, bounds, event categories, identity links and
redacted reasoning-presence marker; exclude all credentials, identifiers,
headers, prompts, arguments, ciphertext and raw payloads. Keep live and fake
evidence distinct.
