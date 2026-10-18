# Local M10 Antigravity operator preparation

This procedure prepares a **disposable local fixture** for the current CLI and
documents a separate, operator-authorized live smoke. It does not establish
Google OAuth/client approval, project eligibility, provider compatibility, or
tool support. The Antigravity endpoint and OAuth defaults are candidates, not a
supported Google API contract. Automated checks and this offline setup make no
provider calls.

## Offline setup: isolated database, key, account, and credential placeholder

Run from the repository root in Bash with Go and Python 3. This creates a temporary
database and a 32-byte master key readable only by the current user. All files
are disposable; do not point these commands at a real operator database.

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
The client metadata URL is not used as `client_id`. Authorization also sends
`prompt=consent` and retains PKCE S256. The default redirect is
`http://localhost:8080/callback`, a bounded fixed-port candidate matching the
loopback shape only. Pinned 9Router starts a listener and chooses a dynamic port;
PestiRoute starts no callback listener, and Google acceptance of this fixed URI
is unverified. This is not a working-client or source-identical claim. These are
unofficial candidate observations, not proof of Google authorization, account
eligibility or protocol compatibility. Treat any locally configured client
secret as a secret. Never copy user/plugin credentials.
Local endpoint, client ID/secret, redirect and scope overrides remain available.
Custom client IDs remain configurable without a secret, allowing explicit
public-client configurations.

**Live authorization is blocked.** No callback listener is running, and an
unrelated local process may already own port 8080 and receive the callback URL
and authorization code. Do not sign in, authorize, or run `auth continue` for
this candidate until the planner-approved callback design and safe callback
handling are available. `auth start` itself remains offline and emits no network
request.

Optional local overrides only (the client ID and redirect URI already default):

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

The following describes the unverified candidate only; do not perform it while
the live authorization blocker above remains. Keep the printed verification URL and session handle in the local terminal; do
not paste them into chat or save terminal output to shared logs. This candidate
uses a loopback redirect, unlike the official remote-SSH UX. If separately
authorized to try it, open the URL in a local browser and after sign-in copy the
complete redirected `http://localhost:8080/callback?...` URL from the address
bar, including both code and state. PestiRoute has no listener; if nothing else
owns the port the browser may show a connection failure, but another local
process could receive the callback. Do not attempt this live while the blocker
above remains. Do not substitute the official web callback or paste only the
code for this flow. Browser sign-in/token exchange remains a separate
operator-authorized live action; offline checks never perform it. A fixed-port
candidate and pasted URL have not been verified with Google.

After the callback blocker is cleared and a separate live action is explicitly
authorized, opt in to outbound HTTPS and continue with the session handle printed
by `start`:

```sh
export PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT=1
"$WORK/gateway" admin --db "$DB" --master-key "$MASTER_KEY" auth continue --session SESSION_ID --callback-stdin
```

Paste the complete loopback callback URL into this command's stdin and press
Enter. Never put the
code/URL in command arguments, shell history, files, logs, or chat;
`--callback-stdin` does not echo it. Continuation exchanges the one-time code and
performs one candidate `loadCodeAssist` project lookup over HTTPS. Both requests
require the explicit real-transport opt-in, which enables outbound auth and
inference HTTPS; it is not a test flag. Do not run continuation during offline
checks or task closure. Discovery metadata does not override the explicit Execute
project, and no onboarding or fallback occurs.

## Protected gateway configuration and single-turn smoke

After separately authorized OAuth succeeds, the `oauth` row contains the
encrypted credential. Create `$WORK/gateway.yaml` with the disposable paths and
operator-selected project name:

```sh
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
the non-secret temporary directory path printed by `printf`:

```sh
printf 'WORK=%s\n' "$WORK"
export PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT=1
"$WORK/gateway" --config "$WORK/gateway.yaml" --config-format yaml
```

In a second local terminal, set `WORK` to that printed path, then issue a
virtual key and keep its one-time secret out of history/logs/chat. The CLI
prints the secret, so capture it only in a local shell variable:

```sh
WORK=/absolute/path/printed/above
DB=$WORK/gateway.db
MASTER_KEY=$WORK/master.key
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

This smoke exercises only one text turn over streaming Responses. It does not
verify broad client compatibility or provider support. Multi-round tool calling,
tool-result/history continuation, and thought-signature handling remain
unsupported/blocked; do not include tools in this smoke. No browser auth or live
inference was run to prepare this document. Offline connector and gateway tests
use deterministic synthetic transports and are not live-provider evidence.
