# Local M10 Antigravity operator preparation

This procedure prepares a **disposable local fixture** for the current CLI and
documents a separate, operator-authorized live smoke. It does not establish
Google OAuth/client approval, project eligibility, provider compatibility, or
tool support. The Antigravity endpoint and OAuth defaults are candidates, not a
supported Google API contract. Automated checks and this offline setup make no
provider calls.

## Offline setup: isolated database, key, account, and credential placeholder

Run from the repository root in Bash with Go and Python 3. This creates a temporary
database and a 32-byte master key readable only by the current user. This fixture
is offline-only and disposable; do not use it for browser auth or live inference,
and do not point these commands at a real operator database.

```sh
set -eu
umask 077
WORK=$(mktemp -d "${TMPDIR:-/tmp}/pestiroute-m10.XXXXXX")
DB=$WORK/gateway.db
MASTER_KEY=$WORK/master.key
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go build -o "$WORK/gateway" ./cmd/gateway

python3 - "$MASTER_KEY" <<'PY'
import os, sys
fd = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "wb") as key:
    key.write(os.urandom(32))
PY
test "$(wc -c <"$MASTER_KEY")" -eq 32
test "$(stat -c '%a' "$MASTER_KEY")" = 600

"$WORK/gateway" admin --db "$DB" --master-key "$MASTER_KEY" migrate
"$WORK/gateway" admin --db "$DB" --master-key "$MASTER_KEY" status
"$WORK/gateway" admin --db "$DB" --master-key "$MASTER_KEY" account create --id antigravity-local --connector antigravity
printf '%s\n' 'pending-oauth-bootstrap' |
  "$WORK/gateway" admin --db "$DB" --master-key "$MASTER_KEY" credential create --account antigravity-local --id oauth
```

The current binary migrates a new database through schema version 6; check the
`status` output. For an existing database, stop the gateway and take a
consistent SQLite backup before migrating. Keep the matching external master
key; never replace it with a newly generated key for an existing database.
The commands above initialize only their newly created disposable directory.
An operator-store rebuild or schema upgrade is a separate, explicit manual
operation with a verified backup; never overwrite an existing database or
credential row or regenerate its master key as part of this procedure.

The auth CLI currently requires **exactly one credential row** for an account
and binds it under the name `oauth`. `authCredentialRefs` enforces this before
authentication, so create the encrypted placeholder first; auth continuation
replaces that row. The placeholder is not a provider token; do not attempt
inference until OAuth continuation replaces it.
Future administration tooling may create the account and its initial row as one
operator-consented action; no such combined command exists today.

## OAuth client and remote-SSH code handling

PestiRoute defaults to the public OAuth client identity, token endpoint and
scopes observed in pinned 9Router v0.5.104 source; it does not bundle a client
secret. The known emulated client requires a configured client secret before
token exchange or refresh, and those operations fail closed when it is absent.
The client metadata URL is not used as `client_id`. Authorization sends
`prompt=consent` and retains PKCE S256. With no configured redirect URI, each
`auth start` briefly binds loopback port 0 to obtain a dynamic candidate port,
releases that socket, and records `http://localhost:<port>/callback` in the
opaque continuation. A configured redirect override is also recorded per
session. The exact recorded URI is used for callback validation and token
exchange even if connector configuration later changes.

PestiRoute does **not** keep a callback listener bound. Releasing the port does
not reserve or establish ownership of it: another local process may receive,
observe, or interfere with the browser callback. PKCE S256 keeps the verifier in
protected continuation state and prevents an interceptor lacking that verifier
from redeeming the code; it does not provide callback confidentiality, prevent
denial of service, or establish that the provider accepts/registers dynamic
ports. The dynamic URI is an offline candidate based on pinned 9Router shape,
not a working-client or provider-registration claim. These are unofficial
observations, not proof of Google authorization, account eligibility or
protocol compatibility. Treat any locally configured client secret as a secret.
Never copy user/plugin credentials.
Local endpoint, client ID/secret, redirect and scope overrides remain available.
Custom client IDs remain configurable without a secret, allowing explicit
public-client configurations. `auth start` itself remains offline and emits no
network request. Live authorization remains blocked pending separate provider
registration evidence and operator authorization.

The emulated client secret is runtime configuration, never a repository default
or a user/plugin token. Provide `PESTIROUTE_ANTIGRAVITY_CLIENT_SECRET` through the
operator's protected secret manager/runtime environment; do not put its value in
repository files, shell commands, scripts, arguments, or logs. The standalone
launcher preserves this inherited variable and requires it for the default
emulated client. If an explicitly reviewed public client is configured through
`PESTIROUTE_ANTIGRAVITY_CLIENT_ID`, the secret may be absent; token exchange still
uses the connector's own client validation and fails closed when required.

## Standalone auth launcher

Build a standalone binary offline, then choose an existing persistent operator
directory outside `/tmp` that contains `gateway.db` and its matching `master.key`.
The launcher does not create, migrate, overwrite, or clean up that directory; it
is not an initializer. Do not point it at an existing store unless its use is
explicitly authorized. The account must already be `antigravity-local` with the
required `oauth` credential row. The offline fixture above is deliberately not
the persistent live-auth store.

```sh
export PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT=/absolute/path/to/persistent/operator-data
export GATEWAY_BIN="$WORK/gateway"
# Supply PESTIROUTE_ANTIGRAVITY_CLIENT_SECRET from protected runtime config.
scripts/antigravity-auth.sh start
```

Run `start` in a private local terminal without redirection, shell tracing, or
shared terminal capture: its child prints the authorization URL and session
handle there. It does not log or store the URL itself. Use the session handle
only as a command argument; callback codes and URLs must never be arguments.
The launcher has no implicit onboarding, retry, database setup, or credential
creation behavior.

Optional local overrides; absent an override, `auth start` records a new dynamic
loopback redirect URI in that session's opaque continuation:

```sh
# Set a client-ID override only when your reviewed local configuration requires it:
# export PESTIROUTE_ANTIGRAVITY_CLIENT_ID='your-public-client-id'
```

Optional overrides (candidate defaults are not official protocol facts):

```text
PESTIROUTE_ANTIGRAVITY_CLIENT_SECRET
PESTIROUTE_ANTIGRAVITY_AUTH_ENDPOINT
PESTIROUTE_ANTIGRAVITY_TOKEN_ENDPOINT
PESTIROUTE_ANTIGRAVITY_REDIRECT_URI
PESTIROUTE_ANTIGRAVITY_SCOPES       # whitespace-separated
PESTIROUTE_ANTIGRAVITY_ENDPOINT      # inference HTTPS origin
```

With the variables set and the disposable DB/credential above, start is local
only and makes no network request:

```sh
"$WORK/gateway" admin --db "$DB" --master-key "$MASTER_KEY" auth start --account antigravity-local
```

The following describes the unverified candidate only; do not perform it without
separate operator authorization. Keep the printed verification URL and session
handle in the local terminal; do not paste them into chat or save terminal
output to shared logs. Open the URL in a browser. After sign-in, copy the
complete redirected callback URL, including code and state, from the browser's
address bar and paste it into `--callback-stdin` below. Because no callback
listener runs, the browser may show a connection failure. Do not replace the
recorded URI with a different URI or use the official web callback. The
connector's raw-code DEC-022 mode is also supported, but the complete URL is the
primary remote-browser path because it preserves and validates state. Browser
callback validation accepts only the explicit optional metadata allowlist
(`scope`, `authuser`, `prompt`, `hd`, and optional `iss`) under DEC-023/024, with
at most seven distinct query keys total. A present `iss` must decode once to the
exact trusted value `https://accounts.google.com`; absence remains compatible.
Values are not displayed, logged, or persisted, and their presence is not
guaranteed by this procedure.
sign-in/token exchange remains a separate operator-authorized live action;
offline checks never perform it. Provider acceptance of dynamic loopback ports
is unverified.

An SSH `-L` tunnel alone cannot capture this callback: there is no PestiRoute
listener on the forwarded server port. PestiRoute does not implement a listener
or make an SSH-forwarding claim. Forwarding can apply only in a separately
designed setup where a real process is already bound to the matching callback
address and port; it is not an alternative to manual paste here.

Only after separate live authorization, opt in to outbound HTTPS and continue
with the session handle printed by `start`:

```sh
# Only after separate operator authorization; keep protected client-secret
# configuration in the process environment, not in this command.
export PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT=1
scripts/antigravity-auth.sh continue SESSION_ID
```

Paste the complete loopback callback URL into this command's stdin and press
Enter. The script invokes the standalone binary with `--callback-stdin`; it
does not echo or log the pasted value. Never put the code/URL in command
arguments, shell history, files, logs, or chat. Continuation exchanges the one-time
code and performs one candidate `loadCodeAssist` project lookup over HTTPS. Both
requests require the explicit real-transport opt-in, which enables outbound auth
and inference HTTPS; it is not a test flag. Do not run continuation during
offline checks or task closure. Discovery metadata does not override the
explicit Execute project, and no onboarding or fallback occurs.

## Protected gateway configuration and single-turn smoke

After separately authorized OAuth succeeds, the `oauth` row contains the
encrypted credential. Point the gateway at the same persistent database and
matching key used by the launcher; keep only the YAML and binary in the temporary
working directory:

```sh
DB=$PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT/gateway.db
MASTER_KEY=$PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT/master.key
cat >"$WORK/gateway.yaml" <<YAML
version: 1
server: {listen: "127.0.0.1:8080", max_request_bytes: 1048576, shutdown_timeout: 5s}
storage: {driver: sqlite, path: "$DB"}
secrets: {master_key_file: "$MASTER_KEY"}
connectors:
  - id: antigravity
    kind: connector
    implementation: pestiroute.antigravity.v1internal
    protocols: [openai.responses.v1]
    settings: {project: my-project, model: gemini-2.5-pro, account_id: antigravity-local}
routes:
  - id: antigravity-route
    protocol: openai.responses.v1
    mode: translation
    model: gemini-2.5-pro
    adapter: pestiroute.responses.native
    policy: local
    budget: {unknown_estimate: reserve, conservative_tokens: 4096}
    targets: [{connector: antigravity, account: antigravity-local}]
policies: {local: antigravity-local-policy}
YAML
"$WORK/gateway" admin --db "$DB" --master-key "$MASTER_KEY" policy create --id antigravity-local-policy --models gemini-2.5-pro --connectors antigravity --rpm 10 --tpm 10000
```

`project` must be the operator's project identifier accepted by the current
configuration validator; its provider association/eligibility is not verified.
The model name above is a smoke candidate, not a promise of availability.
Optional YAML `endpoint` overrides the candidate inference HTTPS origin; the
environment variable `PESTIROUTE_ANTIGRAVITY_ENDPOINT` also overrides it.

The following is a **live provider request**, not an offline test. Run it only
after separate operator authorization, successful manual auth, and confirmation
of the account/project and finite request budget. Start the local gateway with
the real-transport opt-in still set. Keep it running in this terminal and note
the non-secret temporary directory path printed by `printf`. The gateway config
uses the persistent operator database/key set above; only the binary and YAML
remain temporary:

```sh
printf 'WORK=%s\n' "$WORK"
export PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT=1
"$WORK/gateway" --config "$WORK/gateway.yaml" --config-format yaml
```

In a second local terminal, set `WORK` to that printed path and set the same
persistent operator root used in the first terminal, then issue a virtual key
against that store. Keep its one-time secret out of history/logs/chat. The CLI
prints the secret, so capture it only in a local shell variable:

```sh
WORK=/absolute/path/printed/above
export PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT=/absolute/path/to/persistent/operator-data
DB=$PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT/gateway.db
MASTER_KEY=$PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT/master.key
export PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT=1
KEY_OUTPUT=$("$WORK/gateway" admin --db "$DB" --master-key "$MASTER_KEY" key create --policy antigravity-local-policy)
VKEY=$(printf '%s\n' "$KEY_OUTPUT" | sed -n 's/^secret=\([^ ]*\).*/\1/p')
unset KEY_OUTPUT
test -n "$VKEY"
printf 'header = "Authorization: Bearer %s"\n' "$VKEY" |
  curl --config - -sS -N --fail-with-body http://127.0.0.1:8080/v1/responses \
  -H 'Content-Type: application/json' \
  --data-binary '{"model":"gemini-2.5-pro","stream":true,"store":false,"input":"Reply with a brief greeting."}'
unset VKEY
```

The virtual-key header is supplied to curl through its stdin config, not an
expanded command-line argument. Do not enable shell tracing (`set -x`) or curl
verbose/trace output for this command; keep the response and local terminal
private as well.

Stop the gateway with Ctrl-C, then remove the disposable fixture directory from
the first terminal with `rm -rf -- "$WORK"`. Do not remove a real data
directory.

This smoke exercises only one text turn over streaming Responses. A separately
authorized tool-loop request has this Responses shape; this is an offline
candidate example, not a recommendation to send it to a provider. No currently
allowlisted model is established here as non-thinking, and no provider/model
tool-loop behavior is verified. Only consider a real request after separate
evidence establishes that the selected model does not enforce signatures; the
client must execute returned calls and send results/history in a subsequent
request (there is no server-side tool execution):

```json
{"model":"gemini-2.5-pro","stream":true,"store":false,"input":"Look up the current status.","tools":[{"type":"function","name":"lookup","description":"Look up a status","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}],"tool_choice":"auto"}
```

Offline candidate translation accepts causal multi-turn Responses function-call
history, including paired `function_call_output` items and parallel call
coalescence; the client remains responsible for executing tools. The synthetic
tests do not establish provider acceptance. Capability evidence for
`llm.streaming`, `auth.oauth`, and `usage.exact` remain `Unknown`, so this is not
a general provider-support claim. Text-only multi-turn requests do not require
signature continuation, but fullworking/tool loops on signature-enforcing
Thinking models are blocked: no Responses carrier or scoped signature
verification is implemented. Reasoning/signature-bearing submitted history is
rejected before dispatch; a signature first seen upstream is rejected before
frame delivery, after dispatch. The connector does not detect/block models by
thinking/non-thinking status: it accepts only the `gemini-2.5-pro` and
`gemini-2.5-flash` model identifiers, which is a separate allowlist. Never bypass
that guard. No browser auth
or live inference was run to prepare this document. Offline connector and gateway
tests use deterministic synthetic transports and are not live-provider evidence.
