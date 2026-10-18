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

PestiRoute uses the public client identifier published at
`https://antigravity.google/oauth/client-metadata.json` and redirect URI
`https://antigravity.google/oauth-callback` by default. A client ID is a public
identifier, not a secret; its publication does not establish third-party
authorization or account eligibility. OAuth endpoints, scopes and protocol
compatibility remain unverified. Do not copy plugin credentials. Local overrides
remain available; never place a client secret in YAML, command-line arguments,
shell history, logs, or chat.

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

Keep the printed verification URL and session handle in the local terminal; do
not paste them into chat or save terminal output to shared logs. The official
Antigravity remote-SSH instructions say to open that URL in a local browser,
complete sign-in, copy the displayed authorization code, and paste it into the
remote terminal. The default redirect URI is
`https://antigravity.google/oauth-callback`. Browser sign-in/token exchange is a
separate operator-authorized live action; the offline checks never perform it.

**Only when that live action is explicitly authorized**, opt in to outbound
HTTPS and continue with the session handle printed by `start`:

```sh
export PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT=1
"$WORK/gateway" admin --db "$DB" --master-key "$MASTER_KEY" auth continue --session SESSION_ID --callback-stdin
```

Paste the displayed code into this command's stdin and press Enter. A full
callback URL is also accepted when provided by an authorized flow. Never put the
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
