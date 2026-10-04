# Local M4 protected operations

Offline procedure for the M4 native Responses and Anthropic Messages translation
topology. Run from the repository root with Go 1.27.1, Python 3, curl, and the
SQLite CLI. Uses synthetic secrets and a loopback fake Responses backend only;
the translation requests below are rejected locally and never contact Anthropic.
No live credentials or external calls are needed. The generated master key and
virtual keys are private; retain the master key separately from database backups.

## Provision, exercise, restart, and restore

```sh
set -eu
umask 077
dir=$(mktemp -d "${TMPDIR:-/tmp}/m4-local.XXXXXX")
gateway_pid= fake_pid=
cleanup() {
  for pid in "$gateway_pid" "$fake_pid"; do [ -z "$pid" ] || kill "$pid" 2>/dev/null || :; done
  for pid in "$gateway_pid" "$fake_pid"; do [ -z "$pid" ] || wait "$pid" 2>/dev/null || :; done
  rm -rf "$dir"
}
trap cleanup EXIT HUP INT TERM
DB=$dir/gateway.db KEY=$dir/master.key
python3 - "$KEY" <<'PY'
import os, sys
fd = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "wb") as f: f.write(os.urandom(32))
PY
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build -o "$dir/gateway" ./cmd/gateway
"$dir/gateway" admin --db "$DB" --master-key "$KEY" migrate
"$dir/gateway" admin --db "$DB" --master-key "$KEY" account create --id native-account --connector local-native
"$dir/gateway" admin --db "$DB" --master-key "$KEY" account create --id anthropic-account --connector local-anthropic
printf '%s\n' synthetic-native-credential | "$dir/gateway" admin --db "$DB" --master-key "$KEY" credential create --account native-account --id PESTIROUTE_LOCAL_NATIVE
printf '%s\n' synthetic-anthropic-credential | "$dir/gateway" admin --db "$DB" --master-key "$KEY" credential create --account anthropic-account --id local-anthropic-credential
"$dir/gateway" admin --db "$DB" --master-key "$KEY" policy create --id native-policy --models local-native-model --connectors local-native --rpm 1 --tpm 10000
"$dir/gateway" admin --db "$DB" --master-key "$KEY" policy create --id translation-policy --models local-translation-model --connectors local-anthropic --rpm 10 --tpm 10000
"$dir/gateway" admin --db "$DB" --master-key "$KEY" key create --policy native-policy >"$dir/native-key"
"$dir/gateway" admin --db "$DB" --master-key "$KEY" key create --policy translation-policy >"$dir/translation-key"
chmod 600 "$dir/native-key" "$dir/translation-key"
NATIVE_KEY=$(sed -n 's/.*secret=\([^ ]*\).*/\1/p' "$dir/native-key")
TRANSLATION_KEY=$(sed -n 's/.*secret=\([^ ]*\).*/\1/p' "$dir/translation-key")
test -n "$NATIVE_KEY" && test -n "$TRANSLATION_KEY"

cat >"$dir/fake.py" <<'PY'
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        assert self.path == "/v1/responses"
        assert self.headers.get("Authorization") == "Bearer synthetic-native-credential"
        assert json.loads(body)["model"] == "local-native-model"
        data = b'{"id":"local-response","status":"completed"}'
        self.send_response(200); self.send_header("Content-Length", str(len(data)))
        self.end_headers(); self.wfile.write(data)
    def log_message(self, *_): pass
ThreadingHTTPServer(("127.0.0.1", 19091), H).serve_forever()
PY
python3 -u "$dir/fake.py" >"$dir/fake.log" 2>&1 & fake_pid=$!
python3 - <<'PY'
import socket, time
for _ in range(50):
    try:
        with socket.create_connection(("127.0.0.1", 19091), timeout=0.1): break
    except OSError:
        time.sleep(0.1)
else:
    raise SystemExit("loopback fake did not start")
PY
cat >"$dir/gateway.yaml" <<YAML
version: 1
server: {listen: "127.0.0.1:19092", max_request_bytes: 1048576, shutdown_timeout: 1s}
storage: {driver: sqlite, path: "$DB"}
secrets: {master_key_file: "$KEY"}
connectors:
  - id: local-native
    kind: connector
    implementation: pestiroute.responses.native
    protocols: [openai.responses.v1]
    settings: {base_url: http://127.0.0.1:19091/v1/responses, upstream_protocol: openai.responses.v1, mode: native, credential_env: PESTIROUTE_LOCAL_NATIVE, max_request_body_bytes: 1048576, max_request_header_bytes: 16384, connect_timeout: 1s, tls_handshake_timeout: 1s, response_header_timeout: 3s, stream_idle_timeout: 5s}
  - id: local-anthropic
    kind: connector
    implementation: pestiroute.anthropic.messages
    protocols: [openai.responses.v1]
    settings: {model: local-translation-model, account_id: anthropic-account, credential_id: local-anthropic-credential}
routes:
  - id: local-native-route
    protocol: openai.responses.v1
    mode: native
    model: local-native-model
    adapter: pestiroute.responses.native
    policy: native
    budget: {unknown_estimate: reserve, conservative_tokens: 100}
    targets: [{connector: local-native, account: native-account}]
  - id: local-translation-route
    protocol: openai.responses.v1
    mode: translation
    model: local-translation-model
    adapter: pestiroute.responses.native
    policy: translation
    budget: {unknown_estimate: reserve, conservative_tokens: 4096}
    targets: [{connector: local-anthropic, account: anthropic-account}]
policies: {native: native-policy, translation: translation-policy}
YAML
PESTIROUTE_LOCAL_NATIVE=synthetic-native-credential "$dir/gateway" -config-format yaml -config "$dir/gateway.yaml" >"$dir/gateway.log" 2>&1 & gateway_pid=$!
ready() { i=0; until curl -fsS http://127.0.0.1:19092/readyz >/dev/null 2>&1; do i=$((i+1)); [ "$i" -lt 50 ] || return 1; sleep 0.1; done; }
expect_status() {
  expected=$1; shift
  status=$(curl -sS -o "$dir/rejected.json" -w '%{http_code}' "$@")
  test "$status" = "$expected" || { echo "expected HTTP $expected, got $status" >&2; cat "$dir/rejected.json" >&2; return 1; }
}
expect_error_code() {
  expected_code=$1; shift
  expect_status 400 "$@"
  grep -q "\"code\":\"$expected_code\"" "$dir/rejected.json"
}
ready

# Native route dispatches once to the local fake; the second accepted-key
# request hits its RPM=1 limit before dispatch.
curl -fsS http://127.0.0.1:19092/v1/responses -H "Authorization: Bearer $NATIVE_KEY" -H 'Content-Type: application/json' --data-binary '{"model":"local-native-model","input":"synthetic"}' | grep -q local-response
expect_status 429 http://127.0.0.1:19092/v1/responses -H "Authorization: Bearer $NATIVE_KEY" -H 'Content-Type: application/json' --data-binary '{"model":"local-native-model","input":"rate limited"}'

# Both translation failures are local HTTP 400 pre-dispatch gates: the first
# violates the streaming-only profile; the second requires undeclared structured
# output capability. Neither makes a network call to Anthropic.
expect_error_code invalid_request http://127.0.0.1:19092/v1/responses -H "Authorization: Bearer $TRANSLATION_KEY" -H 'Content-Type: application/json' --data-binary '{"model":"local-translation-model","input":"synthetic","stream":false}'
expect_error_code unsupported_capability http://127.0.0.1:19092/v1/responses -H "Authorization: Bearer $TRANSLATION_KEY" -H 'Content-Type: application/json' --data-binary '{"model":"local-translation-model","input":"synthetic","stream":true,"text":{"format":{"type":"json_object"}}}'
"$dir/gateway" admin --db "$DB" --master-key "$KEY" usage summary
"$dir/gateway" admin --db "$DB" --master-key "$KEY" usage requests

kill -TERM "$gateway_pid"; wait "$gateway_pid"; gateway_pid=
PESTIROUTE_LOCAL_NATIVE=synthetic-native-credential "$dir/gateway" -config-format yaml -config "$dir/gateway.yaml" >"$dir/restart.log" 2>&1 & gateway_pid=$!
ready
kill -TERM "$gateway_pid"; wait "$gateway_pid"; gateway_pid=

# Make a SQLite-consistent backup after clean shutdown and restore it offline.
sqlite3 "$DB" ".backup '$dir/backup.db'"
cp "$dir/backup.db" "$dir/restored.db"
test "$(sqlite3 "$dir/restored.db" 'PRAGMA integrity_check;')" = ok
test -z "$(sqlite3 "$dir/restored.db" 'PRAGMA foreign_key_check;')"
RESTORED=$dir/restored.db
"$dir/gateway" admin --db "$RESTORED" --master-key "$KEY" status
"$dir/gateway" admin --db "$RESTORED" --master-key "$KEY" account get anthropic-account
"$dir/gateway" admin --db "$RESTORED" --master-key "$KEY" policy get translation-policy
"$dir/gateway" admin --db "$RESTORED" --master-key "$KEY" usage summary
"$dir/gateway" admin --db "$RESTORED" --master-key "$KEY" usage requests
NATIVE_KEY_ID=$(sed -n 's/.* id=\([^ ]*\).*/\1/p' "$dir/native-key")
"$dir/gateway" admin --db "$RESTORED" --master-key "$KEY" policy update native-policy --expected-revision 1 --rpm 10
"$dir/gateway" admin --db "$RESTORED" --master-key "$KEY" key update-policy "$NATIVE_KEY_ID" --policy native-policy --policy-revision 2 --expected-revision 1
sed "s|$DB|$RESTORED|" "$dir/gateway.yaml" >"$dir/restored.yaml"
PESTIROUTE_LOCAL_NATIVE=synthetic-native-credential "$dir/gateway" -config-format yaml -config "$dir/restored.yaml" >"$dir/restored.log" 2>&1 & gateway_pid=$!
ready
curl -fsS http://127.0.0.1:19092/v1/responses -H "Authorization: Bearer $NATIVE_KEY" -H 'Content-Type: application/json' --data-binary '{"model":"local-native-model","input":"restored"}' | grep -q local-response
kill -TERM "$gateway_pid"; wait "$gateway_pid"; gateway_pid=
"$dir/gateway" admin --db "$RESTORED" --master-key "$KEY" usage summary
if grep -E 'synthetic-(native|anthropic)-credential|secret=' "$dir"/gateway*.log "$dir"/restart.log "$dir"/fake.log; then exit 1; fi
```

The CLI stores both connector credentials encrypted in SQLite; YAML contains
only the Anthropic `credential_id`, never the credential value. The native
Connector's `credential_env` names its selected SQLite credential record and is
also required in the process environment by protected configuration validation.
Key output and the 32-byte master key are mode
0600 under the temporary directory, which is removed on exit. Ports 19091 and
19092 must be free. The translation profile uses
`pestiroute.anthropic.messages`, `mode: translation`, and reserves at least 4096
tokens when the estimate is unknown. `stream` must be literal `true`; structured
output is undeclared and fails capability admission. See the exact [M4
profile](M4-COMPATIBILITY.md#profile) and [protected YAML
connector settings](M3-CONFIG.md#connector-entries).

The fake endpoint verifies one native dispatch; native RPM=1 rejects the next
request with 429. Usage queries show persistent request/attempt/reservation data;
restart checks readiness against the same schema-v6 database. The backup retains
encrypted credentials for both connectors, virtual-key digests, request history
and usage charges. The restored native dispatch using the retained key and
external master key proves the native credential can be decrypted after restore;
the Anthropic encrypted record remains stored without sending it to the provider.
Never restore over a live database or copy the main DB file while a WAL writer is
active. Only local deterministic behavior is claimed;
real Anthropic entitlement and compatibility remain [unknown](M4-COMPATIBILITY.md).

## Focused offline regression checks

The shell procedure above demonstrates protected startup and local rejection.
Run the fixture suites for translation-specific lifecycle, capability and
request-policy behavior, plus the common connector conformance contract:

```sh
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -v -count=1 ./cmd/gateway -run 'Test(Protected.*Translation|Composition|ConfiguredRoutes|YAML|AnthropicSettings).*'
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -v -count=1 ./internal/conformance/...
./scripts/check.sh
```

No command in this procedure makes a live-provider request.
