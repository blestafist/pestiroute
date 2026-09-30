# M1 OpenCode smoke procedure

This procedure separates a local fake-target dry run from the later paid/live
compatibility runs (M1-024..M1-026). The dry run makes no external network
requests and uses synthetic credentials only. It is not client/provider
compatibility evidence.

## Exact client

Use OpenCode V2 CLI **2.0.6** only. The V2 installation page publishes versioned
archives at `https://opencode.ai/files/bin/2.0.6/`; for Linux x64:

```sh
mkdir -p /tmp/opencode/m1-client
curl -fL https://opencode.ai/files/bin/2.0.6/opencode-linux-x64.tar.gz -o /tmp/opencode/m1-client/client.tar.gz
tar -xzf /tmp/opencode/m1-client/client.tar.gz -C /tmp/opencode/m1-client
/tmp/opencode/m1-client/opencode --version
```

Require the exact output `opencode v2.0.6` before continuing (use the matching
versioned archive for another platform). The Linux x64 archive was downloaded,
extracted, and its version command passed locally on 2026-09-30. Its checksum
was not verified. Never substitute a globally installed or newer client; set
`OPENCODE_BIN` to the actual extracted binary path if it differs from the
example below. V2 config syntax/provider details are recorded in [the selected
baseline](REFERENCES.md#m1-compatibility-baseline).

## Disposable project and settings

Create an empty temporary directory with exactly these harmless files:

```text
smoke/
  alpha.txt       # alpha first line: ALPHA-LOCAL-ONLY
  beta.txt        # beta first line: BETA-LOCAL-ONLY
  opencode.jsonc
```

Use fresh sessions and the same files, prompt, agent/tool permissions, model,
and settings for each direct/gateway leg. The request scenario is: first ask
the agent to read `alpha.txt` and report its first line (observe a tool call,
tool result, and continuation); then ask it to read both files using separate
calls requested in parallel, without waiting for one result before requesting
the other. Record if parallel calls did not occur; do not retry more than once
per leg. The limit is two manually initiated client runs per scenario per leg,
not a cap on HTTP attempts the client itself may make; record observed attempts
separately. The local fake returns successful responses and does not test 5xx
retry behavior.

Configure the `providers` (plural) map, explicit OpenAI Responses runtime,
snapshot, and HTTP transport in the disposable `opencode.jsonc`:

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "model": "openai/gpt-4.1-mini-2025-04-14",
  "providers": {
    "openai": {
      "package": "@opencode/ai/providers/openai/responses",
      "name": "OpenAI Responses",
      "models": {
        "gpt-4.1-mini-2025-04-14": {
          "name": "GPT-4.1 mini snapshot",
          "modelID": "gpt-4.1-mini-2025-04-14"
        }
      },
      "settings": {
        "transport": "http"
      }
    }
  }
}
```

Use the direct OpenAI default endpoint for the first leg. For the gateway leg,
change only the provider's `settings.baseURL` to
`http://127.0.0.1:8080/v1`; retain model ID and `transport: "http"`. Confirm the
actual request URL is `/v1/responses`, the body uses model
`gpt-4.1-mini-2025-04-14`, and streaming is enabled before calling either leg's
evidence real. In the local CLI probe, the observed request was an HTTP
`POST /v1/responses` with `stream:true` and an SSE response; the probe does not
establish through an A/B test that the transport setting alone caused that
selection. If the model catalog cannot resolve this
exact ID, pin the explicit `modelID` above; never substitute another snapshot.

Keep the disposable project independent of saved OpenCode accounts: do not run
provider login/auth commands; use a clean `HOME`, `XDG_CONFIG_HOME`, and
`XDG_DATA_HOME` (empty temporary directories) and pass the leg's API key only in
the client process environment. The dry-run runs the exact CLI with those clean
directories and synthetic credentials against a loopback fake endpoint, then
separately sends a synthetic-client-key request through the local gateway. The
sanitized capture distinguishes the CLI's synthetic client bearer from the
gateway's selected synthetic upstream bearer by scope label only; it records
neither token value. This demonstrates clean-account process configuration,
not precedence behavior against an existing saved account. For a live gateway
leg, configure the gateway's upstream key through its own environment variable
as documented in [LOCAL-M1](LOCAL-M1.md); the OpenCode client-facing key and
gateway upstream key are separate. Never put either real key in JSON, shell
command arguments, logs, traces, or source control. Use an
authorized OpenAI Platform API key for a later live direct leg; this is not a
ChatGPT subscription login. Do not run a live leg without explicit API access,
billing authority, and credit.

## Fake-target dry run

Extract the versioned CLI archive above before running the local probe:

```sh
OPENCODE_BIN=/tmp/opencode/m1-client/opencode ./scripts/smoke-m1-dry-run.sh
```

The script fails closed unless Linux user/network namespaces are available. It
re-executes in a fresh network namespace with only loopback enabled, confirms
there is no other interface or route, and starts the fake and gateway there.
No proxy or `NO_PROXY` setting is relied on for egress prevention. It builds the
gateway offline, sends a fixed JSON request through it to check selected
upstream credential handling, then invokes exact OpenCode 2.0.6 in standalone
mode with clean account directories and a synthetic client key against the
loopback fake. The CLI invocation observes an HTTP `POST /v1/responses`,
snapshot model, `stream:true`, SSE response, and synthetic client bearer. This
verifies the configured combination; it does not isolate transport-setting
causality with an A/B test. The fake returns valid 200 JSON/SSE completions (not
5xx); this does not exercise retry behavior. Temporary capture contains only request metadata,
response content type, auth presence, and a credential-scope label; it excludes
credential values and prompt and is deleted with the temporary workspace.
Nothing is written to persistent evidence. This proves local CLI
configuration/request behavior only, not client/provider compatibility or
saved-account precedence outside the clean isolated environment.

## Live evidence capture (M1-024..M1-026 only)

For each leg record client version, date, endpoint class, exact model and
transport, sanitized request path/model/stream, first SSE event timestamp before
completion, event order, item IDs/output indices/call IDs, complete argument
streams, tool execution/result-to-call relationships, subsequent request
rounds, terminal event/error, and available per-round usage. For M1-026 also
record a longer stream interrupted after its first event, upstream cancellation
and cleanup, and no second response/retry. Compare structure and relationships,
not generated text or IDs across independent generations. Keep prompt/tool
settings identical between direct and gateway legs.

Only after an authorized live run, write sanitized `direct.json`,
`gateway.json`, and `README.md` beneath
`evidence/m1/openai-responses/gpt-4.1-mini-2025-04-14/opencode-2.0.6/`.
The README records scenario, sources, versions, scope and invariants. Exclude
credentials, Authorization/cookies, account identifiers, private prompts/file
contents, and raw provider payloads. Do not create placeholder or purported
live-evidence files before the smoke runs. Real-client tool/cancellation
behavior remains unknown until M1-024..M1-026 execute.
