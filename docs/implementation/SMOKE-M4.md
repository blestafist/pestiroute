# M4 direct-versus-gateway smoke procedure

This is a preparation and offline-verification procedure, not permission to make
provider calls. **Do not run either leg against a public endpoint for M4-035.**
Live capture, credentials, external account access, paid inference, and any
external writes are unauthorized here and reserved for M4-036 with its explicit
approval. The offline checks use synthetic fixtures and loopback-only fakes;
they establish neither provider entitlement nor live compatibility.

## Pinned comparison profile

| Leg | Client/request | Provider/API | Model |
| --- | --- | --- | --- |
| Direct | Reproducible `curl` HTTP harness; record `curl --version` and exact harness revision. Send Messages JSON/SSE without a gateway. OpenCode CLI is not used on this leg because its baseline is Responses. | Anthropic Messages `POST /v1/messages`, API root `https://api.anthropic.com`, `anthropic-version: 2023-06-01` | `claude-opus-5-5` |
| Gateway | OpenCode V2 CLI `2.0.6` using `@opencode/ai/providers/openai/responses`, HTTP transport, Responses `POST /v1/responses`; alternatively use the same reproducible curl harness for a protocol-level comparison and record that substitution. | PestiRoute protected translation route, which calls Anthropic Messages as above | Client route model exactly as configured in protected YAML; backend mapping `claude-opus-5-5` |

The gateway client model is a route identity, not the Anthropic model name; do
not rewrite it in the request. The client has no separately pinned Responses
API-version header. The scope is streaming, full-history, client-owned tool
execution; the gateway does not execute tools. See the [accepted binding](M4-BINDING.md#exact-baseline)
and [compatibility matrix](M4-COMPATIBILITY.md#profile). OpenCode 2.0.6 is a
reproducibility baseline, not established live compatibility. Record exact
binary version, OS/architecture, gateway source revision, Connector build/source
revision, curl version where used, Anthropic API version, and model identifier
with any later M4-036 evidence.

## Matched scenarios

Use a fresh client session per scenario and the same harmless prompt, tool
schemas, tool results, output bound, and temperature on both legs. Use
`max_output_tokens: 256` (direct Messages `max_tokens: 256`) and `temperature: 0`
for positive calls. The common tool schemas are:

```json
[
  {"name":"weather","description":"Get fixture weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}},
  {"name":"clock","description":"Get fixture UTC time","parameters":{"type":"object","properties":{"zone":{"type":"string"}},"required":["zone"],"additionalProperties":false}}
]
```

Send these as flat Responses function declarations to the gateway and as
Messages `tools` with `input_schema` on the direct leg. Use these exact harmless
prompts: `Reply exactly: smoke-ok.`; `Get weather for Paris, then report done.`;
`Request weather for Paris and clock for UTC before returning either result.`;
and `Get weather for Paris, then get the UTC time, one tool round at a time.`
For continuations insert client-owned results verbatim into full history:
`{"temperature_c":18,"conditions":"clear"}` and `{"time":"12:00Z"}`.
The two-round scenario returns the weather result, allows a clock call in the
next model response, returns the clock result, and then allows final text. Record
the fixture ID and compare structure, not generated text or IDs.

For the gateway leg use the matching Responses representation of the same
Messages intent; record the request fixture ID, not private prompt text. Tool
schemas must be ordinary flat Responses function tools accepted by the profile.
For tool rounds, the client supplies the complete ordered call/result history
back to the next request. Compare event/turn structure, completion status,
tool-call/result IDs and relationships, and usage availability—not generated
text or independently generated IDs.

1. **Plain text:** one user prompt, incremental streaming, normal completion.
2. **One tool and return:** request one named function; record its call ID,
   return a synthetic tool result in the next full-history request, and record
   the completed continuation.
3. **Two calls in one turn:** request two declared functions and return both
   results. Record whether calls are concurrent or sequential. Do not claim
   parallel support: `llm.tools.parallel` is unknown and a request requiring
   parallel execution must be rejected before provider dispatch.
4. **Two rounds:** complete two tool-call/result/continuation rounds (at least
   three model requests); preserve call/result linkage and chronology each
   round. Gateway tool execution remains client-side.
5. **Reasoning rejection:** submit a request containing Responses `reasoning`
   controls. Record the client-visible rejection and prove the local fake's
   Anthropic request count did not increase. This is a negative case, not a
   direct-provider comparison; do not send it to Anthropic.

The pinned profile requires `stream:true`, defaults `max_output_tokens` to
4096, and rejects values above 4096. Use an explicit cap no greater than 4096
for every generation. Set an HTTP timeout of at most 30 seconds for each
request/attempt. Allow at most two manually initiated complete client runs per
scenario per leg, with automatic SDK/client retries disabled; tool continuation
requests within a run are distinct expected requests, not retries. Record run
count, each continuation request count, timeout, and any observed retry. Stop
after the cap; never retry an ambiguous delivery.

## Offline dry run (required before any separate live gate)

Run from the repository root with Go 1.27.1 and the locally cached dependencies.
These deterministic tests drive the Anthropic connector and the assembled
protected gateway against local fake Messages servers; fixtures use synthetic
credentials. They include streamed text/tool lifecycle, multiple client-owned
rounds, response/request bounds, and reasoning rejection with an unchanged fake
upstream count. They make no provider calls and create no live evidence:

```sh
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -v -count=1 ./internal/connector/anthropic/... -run 'Test(GenerationControls|.*Smoke.*|Execute.*)'
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -v -count=1 ./cmd/gateway -run '^TestTranslationClientOwnedToolRounds$'
./scripts/check.sh
```

The gateway integration test's fake is the local target for the translated
leg; connector tests use local fakes for Messages wire behavior. The tests assert
synthetic auth at the fake boundary rather than saving credentials in traces.
For operational protected-startup coverage, the offline [LOCAL-M4 procedure](LOCAL-M4.md)
uses disposable credentials and loopback only; its translation requests reject
locally, so it is not evidence of successful live translation.

The file-based validator and fixture mode use Python's standard library. Fixture
mode starts a loopback-only Messages fake, sends it a direct Messages tool request
with a synthetic `x-api-key`, then runs the protected PestiRoute Responses
integration test against its loopback fake Messages backend. That test emits a
sanitized gateway-leg artifact from the actual protected request/response. Both
artifacts are read from disk and paired by the validator; deliberately malformed
over-cap and credential-leak files must be rejected. The caller owns the private
disposable directory:

```sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
python3 scripts/smoke-m4-artifacts.py --self-test --output-dir "$tmp"
python3 scripts/smoke-m4-artifacts.py "$tmp/direct.json" "$tmp/gateway.json"
```

The gateway artifact is from the protected Responses-to-Messages integration,
not from a fake Responses endpoint. Its client is labeled as the Go test HTTP
client; the direct local leg is `Python http.client`. Neither is live-client
compatibility evidence. For any later M4-036 capture, record the actual direct
curl version or exact OpenCode 2.0.6 / curl version. The integration test also
proves tool rounds and pre-dispatch reasoning rejection. The validator reads
artifacts and rejects unknown fields, raw
headers/bodies, invalid scenario/client/API labels, mismatched scenario/fixture
IDs, timeouts over 30 seconds, output tokens over 4096, over two client
runs/attempts, and credential/header leakage. It permits only ordinal tool-link
labels. The deliberate 4097-token and secret-bearing fixtures must fail. These prove offline
capture structure and stated limits, not provider behavior. Also exclude cookies,
account IDs, and private prompt/results. Offline artifacts are temporary and
deleted; do not check in dry-run output.

## Capture format and redaction

If M4-036 is separately authorized, store one sanitized JSON file per leg and a
README beneath a versioned path, e.g.
`evidence/m4/anthropic-messages/claude-opus-5-5/opencode-2.0.6/`. The direct
trace must be labeled `direct_messages`; the gateway trace must be labeled
`gateway_responses_to_messages`. Each JSON document contains only:

```json
{
  "schema_version": 1,
  "leg": "direct_messages",
  "captured_at_utc": "YYYY-MM-DDTHH:MM:SSZ",
  "source_revision": "<git revision>",
  "client": {"name": "curl", "version": "<exact version>"},
  "gateway": null,
  "provider": {"api": "anthropic-messages", "api_version": "2023-06-01"},
  "scenario": "plain_text",
  "limits": {"timeout_seconds": 30, "max_output_tokens": 4096, "client_runs": 1},
  "fixture_id": "<matched-safe-fixture-id>",
  "backend_model": "claude-opus-5-5",
  "requests": [{"run": 1, "attempt": 1, "timeout_seconds": 30, "max_output_tokens": 4096, "status": 200, "stream": true, "api_path": "/v1/messages"}],
  "outcome": "completed",
  "observations": {"terminal": "message_stop", "tool_links": [], "usage": "unknown", "target_dispatches": 1}
}
```

The `limits` object contains exactly `timeout_seconds`, `max_output_tokens`,
and `client_runs`; request records contain exactly the keys shown above. For a
reasoning negative, record a rejected gateway outcome with zero upstream
dispatches. Never store auth-header values or raw payloads.

For a gateway document, set `leg` to `gateway_responses_to_messages`, record
the exact OpenCode (or curl) version and PestiRoute revision in `gateway`, and
describe Responses terminal observations. Never include request/response bodies,
prompt/tool text, call arguments/results, IDs, headers, raw events, or account
identifiers. Represent IDs only as within-document ordinal links such as
`call_1` / `result_for_call_1`. Replace any captured `x-api-key` value with
`<ANTHROPIC_KEY_REDACTED>` and any client `Authorization: Bearer` value with
`<CLIENT_KEY_REDACTED>`; safest is to omit the header entirely and record only
`credential_present: true`. Apply redaction before writing files or logs, then
scan the final artifacts and temporary logs for both known test secrets and
`x-api-key`, `Authorization`, `Bearer`, cookies, account IDs, and prompt fixture
sentinels. Do not use real credentials in this validation. A matching text
response alone is not a comparison pass.

README records scope, date, exact versions/revisions, scenario outcomes, count
of requests/runs, cap compliance, redaction scan result, and limitations. Keep
direct and gateway traces separate. Missing access/credentials or external
account entitlement are preconditions for M4-036, never reasons to substitute
synthetic data as live evidence. Do not create placeholder live artifacts.

## Disposable-workspace cleanup and live gate

Any subsequently authorized live capture must be separately approved under
M4-036, including accounts, billing authority, spend ceiling, and exact run
budget. No live call, live credential, paid request, external write, or evidence
publication is authorized by this M4-035 procedure. M4-036 must enforce the
per-request timeout/token cap and two-run cap above, define a total spend ceiling
before execution, and stop when any cap is reached; if pricing or spend cannot
be bounded, do not run.

For offline work use only synthetic credentials, loopback fakes, and a private
temporary directory (`umask 077`). Trap exit/signals to stop child processes and
remove the workspace; verify no child process remains and no capture/log files
persist. Never put credentials in command arguments, config, shell history,
fixtures, logs, or evidence. The [LOCAL-M4 operations](LOCAL-M4.md) document
provides the tested protected configuration and cleanup pattern.
