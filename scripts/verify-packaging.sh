#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
binary=${PESTIROUTE_BINARY:-"$root/bin/gateway"}
if [[ ! -x "$binary" ]]; then
  echo "verify-packaging: build binary first with scripts/release-build.sh" >&2
  exit 1
fi
if [[ $(id -u) -eq 0 ]]; then
  echo "verify-packaging: run as an unprivileged account" >&2
  exit 1
fi
for tool in curl python3; do
  command -v "$tool" >/dev/null || { echo "verify-packaging: missing $tool" >&2; exit 1; }
done

umask 077
work=$(mktemp -d "${TMPDIR:-/tmp}/pestiroute-package.XXXXXX")
gateway_pid= fake_pid=
cleanup() {
  for pid in "$gateway_pid" "$fake_pid"; do
    [[ -z "$pid" ]] || kill "$pid" 2>/dev/null || true
  done
  for pid in "$gateway_pid" "$fake_pid"; do
    [[ -z "$pid" ]] || wait "$pid" 2>/dev/null || true
  done
  rm -rf "$work"
}
trap cleanup EXIT HUP INT TERM

DB="$work/state/gateway.db"
KEY="$work/config/master.key"
mkdir -m 0700 "$work/state" "$work/config"
python3 - "$KEY" <<'PY'
import os, sys
fd = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "wb") as f:
    f.write(os.urandom(32))
PY
chmod 0600 "$KEY"

read -r upstream_port gateway_port < <(python3 - <<'PY'
import socket
ports=[]
for _ in range(2):
    s=socket.socket(); s.bind(("127.0.0.1", 0)); ports.append(s.getsockname()[1]); s.close()
print(*ports)
PY
)

"$binary" admin --db "$DB" --master-key "$KEY" migrate >/dev/null
"$binary" admin --db "$DB" --master-key "$KEY" account create --id package-account --connector package-upstream >/dev/null
printf '%s\n' package-fake-credential | "$binary" admin --db "$DB" --master-key "$KEY" credential create --account package-account --id PESTIROUTE_PACKAGE_UPSTREAM >/dev/null
"$binary" admin --db "$DB" --master-key "$KEY" policy create --id package-policy --models package-model --connectors package-upstream --rpm 10 --tpm 10000 >/dev/null
"$binary" admin --db "$DB" --master-key "$KEY" key create --policy package-policy >"$work/key-output"
key_secret=$(sed -n 's/.*secret=\([^ ]*\).*/\1/p' "$work/key-output")
[[ -n "$key_secret" ]]
chmod 0600 "$work/key-output"

cat >"$work/fake.py" <<'PY'
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json, sys
class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        assert self.path == "/v1/responses"
        assert self.headers.get("Authorization") == "Bearer package-fake-credential"
        assert json.loads(body)["model"] == "package-model"
        data = b'{"id":"package-fake-response","status":"completed"}'
        self.send_response(200); self.send_header("Content-Length", str(len(data)))
        self.end_headers(); self.wfile.write(data)
    def log_message(self, *_): pass
ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
PY
python3 -u "$work/fake.py" "$upstream_port" >"$work/fake.log" 2>&1 & fake_pid=$!
python3 - "$upstream_port" <<'PY'
import socket, sys, time
deadline = time.monotonic() + 10
while time.monotonic() < deadline:
    try:
        with socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=0.1):
            break
    except OSError:
        time.sleep(0.05)
else:
    raise SystemExit("verify-packaging: fake upstream did not start")
PY

cat >"$work/gateway.yaml" <<YAML
version: 1
server: {listen: "127.0.0.1:$gateway_port", max_request_bytes: 1048576, shutdown_timeout: 5s}
storage: {driver: sqlite, path: "$DB"}
secrets: {master_key_file: "$KEY"}
connectors:
  - id: package-upstream
    kind: connector
    implementation: pestiroute.responses.native
    protocols: [openai.responses.v1]
    settings: {base_url: "http://127.0.0.1:$upstream_port/v1/responses", upstream_protocol: openai.responses.v1, mode: native, credential_env: PESTIROUTE_PACKAGE_UPSTREAM, max_request_body_bytes: 1048576, max_request_header_bytes: 16384, connect_timeout: 1s, tls_handshake_timeout: 1s, response_header_timeout: 3s, stream_idle_timeout: 5s}
routes:
  - id: package-route
    protocol: openai.responses.v1
    mode: native
    model: package-model
    adapter: pestiroute.responses.native
    policy: local
    budget: {unknown_estimate: reserve, conservative_tokens: 100}
    targets: [{connector: package-upstream, account: package-account}]
policies: {local: package-policy}
YAML
chmod 0600 "$work/gateway.yaml"

start_gateway() {
  PESTIROUTE_PACKAGE_UPSTREAM=package-fake-credential "$binary" -config-format yaml -config "$work/gateway.yaml" >"$work/gateway.log" 2>&1 &
  gateway_pid=$!
  for _ in $(seq 1 100); do
    if curl -fsS "http://127.0.0.1:$gateway_port/readyz" >/dev/null 2>&1; then return; fi
    if ! kill -0 "$gateway_pid" 2>/dev/null; then cat "$work/gateway.log" >&2; return 1; fi
    sleep 0.1
  done
  cat "$work/gateway.log" >&2
  echo "verify-packaging: gateway did not become ready" >&2
  return 1
}

start_gateway
lock="$DB.gateway.lock"
[[ -f "$lock" && $(stat -c '%a' "$lock") == 600 ]]
status=$(curl -sS -o "$work/response.json" -w '%{http_code}' \
  "http://127.0.0.1:$gateway_port/v1/responses" \
  -H "Authorization: Bearer $key_secret" -H 'Content-Type: application/json' \
  --data-binary '{"model":"package-model","input":"package-smoke"}')
[[ "$status" == 200 ]]
grep -q package-fake-response "$work/response.json"
for sidecar in "$DB-wal" "$DB-shm"; do
  [[ -f "$sidecar" && $(stat -c '%a' "$sidecar") == 600 ]]
done

if PESTIROUTE_PACKAGE_UPSTREAM=package-fake-credential "$binary" -config-format yaml -config "$work/gateway.yaml" >"$work/second.log" 2>&1; then
  echo "verify-packaging: second gateway unexpectedly acquired the database lock" >&2
  exit 1
fi
grep -q 'cannot acquire gateway database lock' "$work/second.log"

kill -TERM "$gateway_pid"
wait "$gateway_pid"
gateway_pid=
[[ -d "$work/state" && $(stat -c '%a' "$work/state") == 700 ]]
[[ $(stat -c '%a' "$KEY") == 600 && $(stat -c '%a' "$DB") == 600 ]]

# Reopening the same DB proves SIGTERM released the flock; a clean exit proves shutdown succeeded.
start_gateway
kill -TERM "$gateway_pid"
wait "$gateway_pid"
gateway_pid=
echo "verify-packaging: PASS (static binary, uid=$(id -u), readyz, fake request, exclusive/released lock, protected modes, SIGTERM exit 0)"
