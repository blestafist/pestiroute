#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
binary=${PESTIROUTE_BINARY:-"$root/bin/gateway"}
if [[ ! -x "$binary" ]]; then
  echo "verify-release: build binary first with scripts/release-build.sh" >&2
  exit 1
fi
if [[ $(id -u) -eq 0 ]]; then
  echo "verify-release: run as an unprivileged account" >&2
  exit 1
fi
for tool in curl python3 sqlite3; do
  command -v "$tool" >/dev/null || { echo "verify-release: missing $tool" >&2; exit 1; }
done

umask 077
ulimit -t 120
ulimit -v 2097152
ulimit -n 256
work=$(mktemp -d "${TMPDIR:-/tmp}/pestiroute-release.XXXXXX")
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

DB="$work/state/gateway.db" KEY="$work/config/master.key"
mkdir -m 0700 "$work/state" "$work/config"
python3 - "$KEY" <<'PY'
import os, sys
fd = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "wb") as f: f.write(os.urandom(32))
PY

read -r upstream_port gateway_port < <(python3 - <<'PY'
import socket
ports=[]
for _ in range(2):
    s=socket.socket(); s.bind(("127.0.0.1", 0)); ports.append(s.getsockname()[1]); s.close()
print(*ports)
PY
)

"$binary" admin --db "$DB" --master-key "$KEY" migrate >/dev/null
"$binary" admin --db "$DB" --master-key "$KEY" account create --id release-account --connector release-upstream >/dev/null
printf '%s\n' release-fake-credential | "$binary" admin --db "$DB" --master-key "$KEY" credential create --account release-account --id PESTIROUTE_RELEASE_UPSTREAM >/dev/null
"$binary" admin --db "$DB" --master-key "$KEY" policy create --id release-policy --models release-model --connectors release-upstream --rpm 100 --tpm 100000 >/dev/null
"$binary" admin --db "$DB" --master-key "$KEY" key create --policy release-policy >"$work/key-output"
key_secret=$(sed -n 's/.*secret=\([^ ]*\).*/\1/p' "$work/key-output")
key_id=$(sed -n 's/.* id=\([^ ]*\).*/\1/p' "$work/key-output")
[[ -n "$key_secret" && -n "$key_id" ]]
chmod 0600 "$work/key-output"
"$binary" admin --db "$DB" --master-key "$KEY" key create --policy release-policy >"$work/revoked-output"
revoked_secret=$(sed -n 's/.*secret=\([^ ]*\).*/\1/p' "$work/revoked-output")
revoked_id=$(sed -n 's/.* id=\([^ ]*\).*/\1/p' "$work/revoked-output")
[[ -n "$revoked_secret" && -n "$revoked_id" ]]
chmod 0600 "$work/revoked-output"
"$binary" admin --db "$DB" --master-key "$KEY" key revoke "$revoked_id" >/dev/null

cat >"$work/fake.py" <<'PY'
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json, os, sys, time
root, port = sys.argv[1], int(sys.argv[2])
class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        with open(os.path.join(root, "upstream-count"), "a") as f: f.write("request\n")
        assert self.path == "/v1/responses"
        assert self.headers.get("Authorization") == "Bearer release-fake-credential"
        request = json.loads(body)
        assert request["model"] == "release-model"
        label = request["input"]
        with open(os.path.join(root, "request-" + label + ".json"), "wb") as f: f.write(body)
        if request.get("stream"):
            self.send_response(200); self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache"); self.send_header("Connection", "close"); self.end_headers()
            chunks = [b'event: response.created\ndata: {"type":"response.created","response":{"id":"release-response","status":"in_progress"},"opaque":{"v":1}}\n\n',
                      b'event: response.in_progress\ndata: {"type":"response.in_progress","response":{"id":"release-response","status":"in_progress"}}\n\n',
                      b'event: response.output_item.added\ndata: {"type":"response.output_item.added","item":{"id":"release-item","type":"message","role":"assistant","status":"in_progress","content":[]}}\n\n',
                      b'event: response.content_part.added\ndata: {"type":"response.content_part.added","item_id":"release-item","part":{"type":"output_text","text":""}}\n\n',
                      b'event: response.output_text.delta\ndata: {"type":"response.output_text.delta","item_id":"release-item","delta":"release"}\n\n',
                      b'event: response.output_text.done\ndata: {"type":"response.output_text.done","item_id":"release-item","text":"release"}\n\n',
                      b'event: response.content_part.done\ndata: {"type":"response.content_part.done","item_id":"release-item","part":{"type":"output_text","text":"release"}}\n\n',
                      b'event: response.output_item.done\ndata: {"type":"response.output_item.done","item":{"id":"release-item","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"release"}]}}\n\n',
                      b'event: response.completed\ndata: {"type":"response.completed","response":{"id":"release-response","status":"completed","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}\n\n']
            if label == "stream-case":
                with open(os.path.join(root, "expected-stream.sse"), "wb") as f: f.write(b"".join(chunks))
            cancel = label == "cancel-case"
            try:
                for chunk in chunks:
                    self.wfile.write(chunk); self.wfile.flush()
                    if cancel: time.sleep(0.3)
                if cancel:
                    while True:
                        self.wfile.write(b": keepalive\n\n"); self.wfile.flush(); time.sleep(0.1)
            except (BrokenPipeError, ConnectionResetError, OSError):
                if cancel:
                    with open(os.path.join(root, "upstream-cancelled"), "a") as f: f.write("cancelled\n")
        else:
            data = b'{"id":"release-response","status":"completed","opaque":{"keep":true},"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}'
            with open(os.path.join(root, "expected-json-response"), "wb") as f: f.write(data)
            self.send_response(200); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data))); self.end_headers(); self.wfile.write(data)
    def log_message(self, *_): pass
ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
PY
python3 -u "$work/fake.py" "$work" "$upstream_port" >"$work/fake.log" 2>&1 & fake_pid=$!
python3 - "$upstream_port" <<'PY'
import socket, sys, time
deadline=time.monotonic()+10
while time.monotonic()<deadline:
    try:
        with socket.create_connection(("127.0.0.1",int(sys.argv[1])),timeout=.1): break
    except OSError: time.sleep(.05)
else: raise SystemExit("verify-release: fake upstream did not start")
PY

cat >"$work/gateway.yaml" <<YAML
version: 1
server: {listen: "127.0.0.1:$gateway_port", max_request_bytes: 1048576, shutdown_timeout: 5s}
storage: {driver: sqlite, path: "$DB"}
secrets: {master_key_file: "$KEY"}
connectors:
  - id: release-upstream
    kind: connector
    implementation: pestiroute.responses.local
    protocols: [openai.responses.v1]
    settings: {base_url: "http://127.0.0.1:$upstream_port/v1/responses", runtime_profile: ollama-v0.40.3, auth_mode: bearer, credential_id: PESTIROUTE_RELEASE_UPSTREAM, max_request_body_bytes: 1048576, max_request_header_bytes: 16384, connect_timeout: 1s, tls_handshake_timeout: 1s, response_header_timeout: 3s, stream_idle_timeout: 5s, model: release-model, account_id: release-account, capabilities: {llm.streaming: supported}}
routes:
  - id: release-route
    protocol: openai.responses.v1
    mode: native
    model: release-model
    adapter: pestiroute.responses.native
    policy: local
    budget: {unknown_estimate: reserve, conservative_tokens: 100}
    targets: [{connector: release-upstream, account: release-account}]
policies: {local: release-policy}
YAML
chmod 0600 "$work/gateway.yaml"

start_gateway() {
  "$binary" -config-format yaml -config "$work/gateway.yaml" >"$work/gateway.log" 2>&1 &
  gateway_pid=$!
  for _ in $(seq 1 100); do
    if curl -fsS "http://127.0.0.1:$gateway_port/readyz" >/dev/null 2>&1; then return; fi
    if ! kill -0 "$gateway_pid" 2>/dev/null; then cat "$work/gateway.log" >&2; return 1; fi
    sleep .1
  done
  cat "$work/gateway.log" >&2; echo "verify-release: gateway did not become ready" >&2; return 1
}
request() {
  curl -fsS "http://127.0.0.1:$gateway_port/v1/responses" -H "Authorization: Bearer $1" \
    -H 'Content-Type: application/json' --data-binary "$2"
}
snapshot_usage() {
  sqlite3 -noheader "$1" "SELECT a.request_id || '|' || u.attempt_id || '|' || coalesce(CAST(u.input_tokens AS TEXT),'NULL') || '|' || coalesce(CAST(u.output_tokens AS TEXT),'NULL') || '|' || coalesce(CAST(u.reasoning_tokens AS TEXT),'NULL') || '|' || coalesce(CAST(u.cached_tokens AS TEXT),'NULL') || '|' || u.source || '|' || u.completeness || '|' || u.recorded_at FROM usage_records u JOIN attempts a ON a.id=u.attempt_id ORDER BY a.request_id,u.attempt_id;" >"$2"
}
assert_one_new_usage() {
  python3 - "$1" "$2" <<'PY'
from collections import Counter
import sys
old = Counter(open(sys.argv[1]).read().splitlines())
new = Counter(open(sys.argv[2]).read().splitlines())
added = list((new - old).elements())
if len(added) != 1 or sum(new.values()) != sum(old.values()) + 1:
    raise SystemExit(f"verify-release: expected exactly one new usage identity; old={sum(old.values())} new={sum(new.values())}")
fields = added[0].split("|")
if (len(fields) != 9 or not fields[0] or not fields[1] or fields[2:6] != ["2", "1", "NULL", "NULL"]
        or fields[6:8] != ["provider", "complete"] or int(fields[8]) <= 0):
    raise SystemExit(f"verify-release: new usage record differs from expected provider usage: {added[0]}")
PY
}
start_gateway
[[ $(id -u) != 0 && -f "$DB.gateway.lock" && $(stat -c '%a' "$DB.gateway.lock") == 600 ]]

# Native JSON request/response bytes, including unknown extension fields, pass unchanged.
json_body='{"model":"release-model","input":"opaque-check","unknown_extension":{"x":[1,true]}}'
request "$key_secret" "$json_body" >"$work/json-response"
printf '%s' "$json_body" >"$work/expected-request.json"
cmp "$work/expected-request.json" "$work/request-opaque-check.json"
cmp "$work/expected-json-response" "$work/json-response"

# Verify live incremental SSE bytes (including unknown data), not only completion.
sse_body='{"model":"release-model","input":"stream-case","stream":true,"opaque":{"keep":2}}'
curl -fsS "http://127.0.0.1:$gateway_port/v1/responses" -H "Authorization: Bearer $key_secret" \
  -H 'Content-Type: application/json' -H 'Accept: text/event-stream' --data-binary "$sse_body" >"$work/stream.sse"
printf '%s' "$sse_body" >"$work/expected-stream-request.json"
cmp "$work/expected-stream-request.json" "$work/request-stream-case.json"
cmp "$work/expected-stream.sse" "$work/stream.sse"

# Repeatedly close after the first event; upstream cancellation and a stable FD
# count prove the path releases sockets instead of accumulating them.
fd_before=$(python3 - "$gateway_pid" <<'PY'
import os, sys
print(len(os.listdir(f"/proc/{sys.argv[1]}/fd")))
PY
)
for expected_cancels in 1 2 3; do
  python3 - "$gateway_port" "$key_secret" <<'PY'
import http.client, sys
c=http.client.HTTPConnection("127.0.0.1",int(sys.argv[1]),timeout=10)
c.request("POST","/v1/responses",body=b'{"model":"release-model","input":"cancel-case","stream":true}',
           headers={"Authorization":"Bearer "+sys.argv[2],"Content-Type":"application/json","Accept":"text/event-stream"})
r=c.getresponse(); assert r.status==200, r.status
first = (b'event: response.created\ndata: {"type":"response.created","response":{"id":"release-response","status":"in_progress"},"opaque":{"v":1}}\n\n')
assert r.readline() + r.readline() + r.readline() == first, "first opaque SSE event was not delivered intact before cancellation"
c.close()
PY
  python3 - "$work/upstream-cancelled" "$expected_cancels" <<'PY'
import os, sys, time
deadline=time.monotonic()+10
while time.monotonic()<deadline:
    try:
        if len(open(sys.argv[1]).readlines()) >= int(sys.argv[2]): break
    except FileNotFoundError: pass
    time.sleep(.05)
else: raise SystemExit("verify-release: client disconnect did not cancel upstream")
PY
done
sleep 0.2
fd_after=$(python3 - "$gateway_pid" <<'PY'
import os, sys
print(len(os.listdir(f"/proc/{sys.argv[1]}/fd")))
PY
)
(( fd_after <= fd_before + 1 )) || { echo "verify-release: gateway descriptors grew across cancellations ($fd_before->$fd_after)" >&2; exit 1; }

# Unknown and revoked keys are rejected before any upstream dispatch.
before=$(wc -l <"$work/upstream-count")
for pair in "missing-key:401" "$revoked_secret:401|403"; do
  token=${pair%:*}; expected=${pair##*:}
  status=$(curl -sS -o "$work/denied.json" -w '%{http_code}' "http://127.0.0.1:$gateway_port/v1/responses" \
    -H "Authorization: Bearer $token" -H 'Content-Type: application/json' --data-binary '{"model":"release-model","input":"denied"}')
  [[ "|$expected|" == *"|$status|"* ]]
done
[[ $(wc -l <"$work/upstream-count") == "$before" ]]

# Usage is durable before and after clean SIGTERM/restart on the same DB/key.
snapshot_usage "$DB" "$work/usage-before-restart.tsv"
[[ -s "$work/usage-before-restart.tsv" ]]
kill -TERM "$gateway_pid"; wait "$gateway_pid"; gateway_pid=
start_gateway
snapshot_usage "$DB" "$work/usage-after-restart.tsv"
cmp "$work/usage-before-restart.tsv" "$work/usage-after-restart.tsv"
restart_body='{"model":"release-model","input":"after-restart","stream":true}'
curl -fsS "http://127.0.0.1:$gateway_port/v1/responses" -H "Authorization: Bearer $key_secret" \
  -H 'Content-Type: application/json' -H 'Accept: text/event-stream' --data-binary "$restart_body" >"$work/restart-response.sse"
printf '%s' "$restart_body" >"$work/expected-restart-request.json"
cmp "$work/expected-restart-request.json" "$work/request-after-restart.json"
cmp "$work/expected-stream.sse" "$work/restart-response.sse"
snapshot_usage "$DB" "$work/usage-after-new-request.tsv"
assert_one_new_usage "$work/usage-after-restart.tsv" "$work/usage-after-new-request.tsv"

# Online SQLite backup while gateway is serving; validate and run from a fresh restored path.
BACKUP="$work/online-backup.db" RESTORED="$work/restored.db"
sqlite3 "$DB" ".backup '$BACKUP'"
[[ $(sqlite3 "$BACKUP" 'PRAGMA integrity_check;') == ok ]]
[[ -z $(sqlite3 "$BACKUP" 'PRAGMA foreign_key_check;') ]]
snapshot_usage "$DB" "$work/usage-live-before-backup.tsv"
snapshot_usage "$BACKUP" "$work/usage-backup.tsv"
cmp "$work/usage-live-before-backup.tsv" "$work/usage-backup.tsv"
sqlite3 "$BACKUP" ".backup '$RESTORED'"
[[ "$RESTORED" != "$DB" && $(sqlite3 "$RESTORED" 'PRAGMA integrity_check;') == ok ]]
[[ -z $(sqlite3 "$RESTORED" 'PRAGMA foreign_key_check;') ]]
snapshot_usage "$RESTORED" "$work/usage-restored-before-request.tsv"
cmp "$work/usage-live-before-backup.tsv" "$work/usage-restored-before-request.tsv"

kill -TERM "$gateway_pid"; wait "$gateway_pid"; gateway_pid=
sed "s|$DB|$RESTORED|" "$work/gateway.yaml" >"$work/restored.yaml"
"$binary" -config-format yaml -config "$work/restored.yaml" >"$work/restored.log" 2>&1 &
gateway_pid=$!
for _ in $(seq 1 100); do
  if curl -fsS "http://127.0.0.1:$gateway_port/readyz" >/dev/null 2>&1; then break; fi
  if ! kill -0 "$gateway_pid" 2>/dev/null; then cat "$work/restored.log" >&2; exit 1; fi
  sleep .1
done
restored_body='{"model":"release-model","input":"restored","stream":true}'
curl -fsS "http://127.0.0.1:$gateway_port/v1/responses" -H "Authorization: Bearer $key_secret" \
  -H 'Content-Type: application/json' -H 'Accept: text/event-stream' --data-binary "$restored_body" >"$work/restored-response.sse"
printf '%s' "$restored_body" >"$work/expected-restored-request.json"
cmp "$work/expected-restored-request.json" "$work/request-restored.json"
cmp "$work/expected-stream.sse" "$work/restored-response.sse"
snapshot_usage "$RESTORED" "$work/usage-restored-after-request.tsv"
assert_one_new_usage "$work/usage-restored-before-request.tsv" "$work/usage-restored-after-request.tsv"
kill -TERM "$gateway_pid"; wait "$gateway_pid"; gateway_pid=

# Clean shutdown releases the lock and listener; confirm the process is reaped and port reusable.
python3 - "$gateway_port" <<'PY'
import socket, sys
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
s.bind(("127.0.0.1",int(sys.argv[1]))); s.close()
PY
if grep -E 'release-fake-credential|secret=' "$work"/*.log; then
  echo "verify-release: secret material found in logs" >&2; exit 1
fi
echo "verify-release: PASS (uid=$(id -u), fd-limit=256, JSON/SSE opaque bytes, cancellation, 401/403 fail-closed, restart accounting, online backup/restore, SIGTERM and listener/lock cleanup)"
