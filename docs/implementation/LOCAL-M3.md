# Local M3 protected operations

Offline procedure with synthetic credentials and loopback only; not provider
compatibility evidence. Run from repository root with Go 1.27.1, Python 3 and
curl and `sqlite3`. The key-creation output is private because its
secret is shown once. Do not substitute live credentials.

## Provision and exercise protected startup

```sh
set -eu
umask 077
dir=$(mktemp -d "${TMPDIR:-/tmp}/m3-local.XXXXXX")
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
"$dir/gateway" admin --db "$DB" --master-key "$KEY" account create --id local-account --connector local-upstream
printf '%s\n' synthetic-connector-credential | "$dir/gateway" admin --db "$DB" --master-key "$KEY" credential create --account local-account --id PESTIROUTE_LOCAL_CONNECTOR
"$dir/gateway" admin --db "$DB" --master-key "$KEY" policy create --id local-policy --models synthetic-model --connectors local-upstream --rpm 1 --tpm 10000
"$dir/gateway" admin --db "$DB" --master-key "$KEY" key create --policy local-policy >"$dir/key-output"
chmod 600 "$dir/key-output"
KEY_SECRET=$(sed -n 's/.*secret=\([^ ]*\).*/\1/p' "$dir/key-output")
test -n "$KEY_SECRET"
cat >"$dir/fake.py" <<'PY'
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        assert self.path == "/v1/responses"
        assert self.headers.get("Authorization") == "Bearer synthetic-connector-credential"
        assert json.loads(body)["model"] == "synthetic-model"
        data = b'{"id":"synthetic-response","status":"completed"}'
        self.send_response(200); self.send_header("Content-Length", str(len(data)))
        self.end_headers(); self.wfile.write(data)
    def log_message(self, *_): pass
ThreadingHTTPServer(("127.0.0.1", 19091), H).serve_forever()
PY
python3 -u "$dir/fake.py" >"$dir/fake.log" 2>&1 & fake_pid=$!
cat >"$dir/gateway.yaml" <<YAML
version: 1
server: {listen: "127.0.0.1:19092", max_request_bytes: 1048576, shutdown_timeout: 1s}
storage: {driver: sqlite, path: "$DB"}
secrets: {master_key_file: "$KEY"}
connectors:
  - id: local-upstream
    kind: connector
    implementation: pestiroute.responses.native
    protocols: [openai.responses.v1]
    settings: {base_url: http://127.0.0.1:19091/v1/responses, upstream_protocol: openai.responses.v1, mode: native, credential_env: PESTIROUTE_LOCAL_CONNECTOR, max_request_body_bytes: 1048576, max_request_header_bytes: 16384, connect_timeout: 1s, tls_handshake_timeout: 1s, response_header_timeout: 3s, stream_idle_timeout: 5s}
routes:
  - id: synthetic-route
    protocol: openai.responses.v1
    mode: native
    model: synthetic-model
    adapter: pestiroute.responses.native
    policy: local
    budget: {unknown_estimate: reserve, conservative_tokens: 100}
    targets: [{connector: local-upstream, account: local-account}]
policies: {local: local-policy}
YAML
PESTIROUTE_LOCAL_CONNECTOR=synthetic-connector-credential "$dir/gateway" -config-format yaml -config "$dir/gateway.yaml" >"$dir/gateway.log" 2>&1 & gateway_pid=$!
sleep 1
curl -fsS http://127.0.0.1:19092/readyz >/dev/null
curl -fsS http://127.0.0.1:19092/v1/responses -H "Authorization: Bearer $KEY_SECRET" -H 'Content-Type: application/json' --data-binary '{"model":"synthetic-model","input":"synthetic"}' | grep -q synthetic-response
status=$(curl -sS -o "$dir/rejected.json" -w '%{http_code}' http://127.0.0.1:19092/v1/responses -H "Authorization: Bearer $KEY_SECRET" -H 'Content-Type: application/json' --data-binary '{"model":"synthetic-model","input":"synthetic"}')
test "$status" = 429
"$dir/gateway" admin --db "$DB" --master-key "$KEY" usage summary
"$dir/gateway" admin --db "$DB" --master-key "$KEY" usage requests
kill -TERM "$gateway_pid"; wait "$gateway_pid"; gateway_pid=
PESTIROUTE_LOCAL_CONNECTOR=synthetic-connector-credential "$dir/gateway" -config-format yaml -config "$dir/gateway.yaml" >"$dir/restart.log" 2>&1 & gateway_pid=$!
sleep 1
curl -fsS http://127.0.0.1:19092/readyz >/dev/null
kill -TERM "$gateway_pid"; wait "$gateway_pid"; gateway_pid=

# Consistent backup, offline restore, integrity/reference checks and proof that
# the retained master key still decrypts the restored connector credential.
sqlite3 "$DB" ".backup '$dir/backup.db'"
cp "$dir/backup.db" "$dir/restored.db"
test "$(sqlite3 "$dir/restored.db" 'PRAGMA integrity_check;')" = ok
test -z "$(sqlite3 "$dir/restored.db" 'PRAGMA foreign_key_check;')"
RESTORED=$dir/restored.db
"$dir/gateway" admin --db "$RESTORED" --master-key "$KEY" status
"$dir/gateway" admin --db "$RESTORED" --master-key "$KEY" account get local-account
"$dir/gateway" admin --db "$RESTORED" --master-key "$KEY" usage summary
"$dir/gateway" admin --db "$RESTORED" --master-key "$KEY" policy update local-policy --expected-revision 1 --rpm 100
KEY_ID=$(sed -n 's/.* id=\([^ ]*\).*/\1/p' "$dir/key-output")
"$dir/gateway" admin --db "$RESTORED" --master-key "$KEY" key update-policy "$KEY_ID" --policy local-policy --policy-revision 2 --expected-revision 1
sed "s|$DB|$RESTORED|" "$dir/gateway.yaml" >"$dir/restored.yaml"
PESTIROUTE_LOCAL_CONNECTOR=synthetic-connector-credential "$dir/gateway" -config-format yaml -config "$dir/restored.yaml" >"$dir/restored.log" 2>&1 & gateway_pid=$!
sleep 1
curl -fsS http://127.0.0.1:19092/v1/responses -H "Authorization: Bearer $KEY_SECRET" -H 'Content-Type: application/json' --data-binary '{"model":"synthetic-model","input":"restored"}' | grep -q synthetic-response
kill -TERM "$gateway_pid"; wait "$gateway_pid"; gateway_pid=
if grep -E 'synthetic-connector-credential|secret=' "$dir"/gateway*.log "$dir"/restored.log "$dir/fake.log"; then exit 1; fi
```

The new temporary DB makes RPM=1 rejection deterministic. Ports 19091/19092
must be free. TPM rejection is proven by controlled-estimate tests; this
procedure does not assume a native Connector estimate. Key/master-key files
and the temporary tree are private. Graceful restart proves persisted state,
not interrupted-attempt recovery.

## Safe fallback and crash recovery

The deterministic Core/SQLite fixture injects an explicitly safe uncommitted
failure; arbitrary provider HTTP status is not equivalent. These checks run
offline and cover fallback accounting, true child-process crash/recovery with
no replay, admission and affinity restrictions:

```sh
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -count=10 ./cmd/gateway/... -run 'TestFallbackSQLiteDispatcher'
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -count=1 ./cmd/gateway/... -run 'Test(AbruptRestart|RestartAccounting)'
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -count=1 ./cmd/gateway/... -run 'Test(StatefulAffinityFallbackBoundaries|AccessRevocation|AdmissionConcurrentLimit|LivePolicyUpdate)'
```

The local HTTP procedure has one target and does not claim to demonstrate
fallback. Its fake upstream returns a successful native primary response; the
second request is rejected by RPM before dispatch. It does not exercise a
native upstream error. Native HTTP transport/status errors have unknown retry
disposition, so they fail closed rather than being treated as safe. Use the
deterministic scripted-connector test above as the bounded safe-fallback
evidence; it injects the explicit safe, uncommitted failure and verifies the
second candidate and per-attempt accounting without claiming HTTP status-code
behavior.

## WAL-safe backup and restore

Stop all gateway processes cleanly. The runnable procedure above requires the
SQLite CLI so it can execute a consistent backup plus integrity and foreign-key
checks. Prefer SQLite's consistent backup method; never copy the main DB file
while a WAL writer is active:

```sh
sqlite3 "$DB" ".backup '$dir/backup.db'"
```

If no SQLite CLI is available, after clean shutdown and with no open DB
connections, an offline file copy is allowed, but integrity/reference checks
must be run through a SQLite API/driver instead:

```sh
cp "$DB" "$dir/backup.db"
cp "$dir/backup.db" "$dir/restored.db"
sqlite3 "$dir/restored.db" 'PRAGMA integrity_check; PRAGMA foreign_key_check;'
```

Expect `ok` and no foreign-key rows. Point a copy of YAML at `restored.db`,
retain the same external master key, then check `admin status`, `account get
local-account`, `policy get local-policy`, and `usage summary`. This preserves
the encrypted credential, key digest and usage history/charges; loss of the
external key makes credentials unusable. Never restore over a live database.
The optional CLI documents [`.backup`](https://www.sqlite.org/backup.html)
and [PRAGMA](https://www.sqlite.org/pragma.html); driver coverage is in
`go test -race ./internal/storage/sqlite/...`.

## Upgrading an existing M3 database

This binary requires schema v6. Stop the gateway, make a consistent backup as
above, retain the external master key, and run `gateway admin --db PATH migrate`
before protected startup. The additive migration preserves v5 credentials and
unclaimed auth sessions; startup consumes ambiguous claimed continuations and
quarantines interrupted refreshes without replay. An older binary refuses the
newer schema. Downgrade and master-key rotation are outside M3.
