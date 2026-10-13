# Local M5.2 protected operations

Offline, synthetic end-to-end procedure for all three protected local Responses
profiles. Run from the repository root with Go 1.27.1, Python 3, `curl`, and
`sqlite3`. It uses only temporary synthetic credentials and loopback fake
backends; no runtime, model, external endpoint, or live backend is contacted.
The generated master key, virtual key, and temporary database are private and
deleted on exit. Keep the master key separate from any retained database backup.

These procedures prove protected synthetic routing, persistence, and backup
mechanics only; they do not accept any runtime release claim or promote model
capabilities. The independent live gates [M5.2-024](tasks/M5.2-024.md),
[M5.2-025](tasks/M5.2-025.md), and [M5.2-026](tasks/M5.2-026.md) remain
BLOCKED on their recorded runtime/model/hardware prerequisites and operator
authorization. See [M5.2-COMPATIBILITY](M5.2-COMPATIBILITY.md) for the audited
Unknown capability matrix.

## Provision, coexist, restart, and restore

Run the complete block in a clean shell. It provisions distinct no-auth Ollama
and bearer-authenticated vLLM/llama.cpp targets, with one protected route per
profile. Every fake checks the expected path, model, and auth mode, then returns
a non-empty synthetic Responses JSON body. Each virtual-key-authenticated
request uses unknown usage and the configured conservative reservation. The
gateway is stopped before SQLite's online `.backup` command; the restored
database uses the same external master key and is dispatched successfully.

```sh
set -eu
umask 077
dir=$(mktemp -d "${TMPDIR:-/tmp}/m52-local.XXXXXX")
pids=
cleanup() {
  for pid in $pids; do kill "$pid" 2>/dev/null || :; done
  for pid in $pids; do wait "$pid" 2>/dev/null || :; done
  rm -rf "$dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
DB=$dir/gateway.db KEY=$dir/master.key

python3 - "$KEY" <<'PY'
import os, sys
fd = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "wb") as f: f.write(os.urandom(32))
PY
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go build -o "$dir/gateway" ./cmd/gateway

# Choose free loopback ports; fake servers bind only to 127.0.0.1.
python3 - "$dir/ports" <<'PY'
import socket, sys
ports=[]
for _ in range(4):
    s=socket.socket(); s.bind(("127.0.0.1",0)); ports.append(s.getsockname()[1]); s.close()
open(sys.argv[1],"w").write(" ".join(map(str,ports))+"\n")
PY
read GATEWAY_PORT OLLAMA_PORT VLLM_PORT LLAMA_PORT <"$dir/ports"

cat >"$dir/fakes.py" <<'PY'
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json, sys, threading
ports=list(map(int,sys.argv[1:]))
profiles=[("ollama","synthetic-ollama","",ports[0]),
          ("vllm","synthetic-vllm","Bearer synthetic-vllm-key",ports[1]),
          ("llama.cpp","synthetic-llama","Bearer synthetic-llama-key",ports[2])]
class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        n=int(self.headers.get("Content-Length","0")); body=json.loads(self.rfile.read(n))
        name,model,auth,_=self.server.profile
        assert self.path=="/v1/responses" and body.get("model")==model
        assert self.headers.get("Authorization","")==auth
        data=json.dumps({"id":"synthetic-"+name,"status":"completed","output_text":"synthetic response from "+name}).encode()
        self.send_response(200); self.send_header("Content-Type","application/json")
        self.send_header("Content-Length",str(len(data))); self.end_headers(); self.wfile.write(data)
    def log_message(self,*_): pass
servers=[]
for p in profiles:
    s=ThreadingHTTPServer(("127.0.0.1",p[3]),Handler); s.profile=p; servers.append(s)
for s in servers: threading.Thread(target=s.serve_forever,daemon=True).start()
threading.Event().wait()
PY
python3 -u "$dir/fakes.py" "$OLLAMA_PORT" "$VLLM_PORT" "$LLAMA_PORT" >"$dir/fakes.log" 2>&1 & pids="$pids $!"
python3 - "$dir/ports" <<'PY'
import socket,sys,time
ports=list(map(int,open(sys.argv[1]).read().split()))
for port in ports[1:]:
    for _ in range(100):
        try:
            with socket.create_connection(("127.0.0.1",port),.1): break
        except OSError: time.sleep(.05)
    else: raise SystemExit("fake backend failed readiness")
PY

"$dir/gateway" admin --db "$DB" --master-key "$KEY" migrate >"$dir/migrate.out"
for profile in ollama vllm llama; do
  "$dir/gateway" admin --db "$DB" --master-key "$KEY" account create --id "$profile-account" --connector "$profile" >"$dir/$profile-account.out"
done
printf '%s\n' synthetic-vllm-key | "$dir/gateway" admin --db "$DB" --master-key "$KEY" credential create --account vllm-account --id vllm-secret >"$dir/vllm-credential.out"
printf '%s\n' synthetic-llama-key | "$dir/gateway" admin --db "$DB" --master-key "$KEY" credential create --account llama-account --id llama-secret >"$dir/llama-credential.out"
"$dir/gateway" admin --db "$DB" --master-key "$KEY" policy create --id local-policy --models synthetic-ollama,synthetic-vllm,synthetic-llama --connectors ollama,vllm,llama --rpm 30 --tpm 30000 >"$dir/policy.out"
"$dir/gateway" admin --db "$DB" --master-key "$KEY" key create --policy local-policy >"$dir/key.out"
chmod 600 "$dir/key.out"
VKEY=$(sed -n 's/.*secret=\([^ ]*\).*/\1/p' "$dir/key.out")
test -n "$VKEY"

cat >"$dir/gateway.yaml" <<YAML
version: 1
server: {listen: "127.0.0.1:$GATEWAY_PORT", max_request_bytes: 1048576, shutdown_timeout: 2s}
storage: {driver: sqlite, path: "$DB"}
secrets: {master_key_file: "$KEY"}
connectors:
  - {id: ollama, kind: connector, implementation: pestiroute.responses.local, protocols: [openai.responses.v1], settings: {base_url: "http://127.0.0.1:$OLLAMA_PORT/v1/responses", runtime_profile: ollama-v0.40.3, auth_mode: none, model: synthetic-ollama, account_id: ollama-account, max_request_body_bytes: 1048576, max_request_header_bytes: 8192, connect_timeout: 1s, tls_handshake_timeout: 1s, response_header_timeout: 3s, stream_idle_timeout: 5s}}
  - {id: vllm, kind: connector, implementation: pestiroute.responses.local, protocols: [openai.responses.v1], settings: {base_url: "http://127.0.0.1:$VLLM_PORT/v1/responses", runtime_profile: vllm-v0.31.0, auth_mode: bearer, credential_id: vllm-secret, model: synthetic-vllm, account_id: vllm-account, max_request_body_bytes: 1048576, max_request_header_bytes: 8192, connect_timeout: 1s, tls_handshake_timeout: 1s, response_header_timeout: 3s, stream_idle_timeout: 5s}}
  - {id: llama, kind: connector, implementation: pestiroute.responses.local, protocols: [openai.responses.v1], settings: {base_url: "http://127.0.0.1:$LLAMA_PORT/v1/responses", runtime_profile: llamacpp-v0.6.0, auth_mode: bearer, credential_id: llama-secret, model: synthetic-llama, account_id: llama-account, max_request_body_bytes: 1048576, max_request_header_bytes: 8192, connect_timeout: 1s, tls_handshake_timeout: 1s, response_header_timeout: 3s, stream_idle_timeout: 5s}}
routes:
  - {id: ollama-route, protocol: openai.responses.v1, mode: native, model: synthetic-ollama, adapter: pestiroute.responses.native, policy: local, budget: {unknown_estimate: reserve, conservative_tokens: 100}, targets: [{connector: ollama, account: ollama-account}]}
  - {id: vllm-route, protocol: openai.responses.v1, mode: native, model: synthetic-vllm, adapter: pestiroute.responses.native, policy: local, budget: {unknown_estimate: reserve, conservative_tokens: 100}, targets: [{connector: vllm, account: vllm-account}]}
  - {id: llama-route, protocol: openai.responses.v1, mode: native, model: synthetic-llama, adapter: pestiroute.responses.native, policy: local, budget: {unknown_estimate: reserve, conservative_tokens: 100}, targets: [{connector: llama, account: llama-account}]}
policies: {local: local-policy}
YAML

start_gateway() {
  "$dir/gateway" -config-format yaml -config "$1" >"$dir/gateway.log" 2>&1 & gateway_pid=$!; pids="$pids $gateway_pid"
  python3 - "$GATEWAY_PORT" <<'PY'
import socket,sys,time
for _ in range(100):
    try:
        with socket.create_connection(("127.0.0.1",int(sys.argv[1])),.1): break
    except OSError: time.sleep(.1)
else: raise SystemExit("gateway failed readiness")
PY
  curl -fsS "http://127.0.0.1:$GATEWAY_PORT/readyz" >/dev/null
}
stop_gateway() {
  old=$gateway_pid; kill -TERM "$old"; wait "$old" || { rc=$?; test "$rc" -eq 143; }
  pids=$(printf '%s' "$pids" | sed "s/ $old//"); gateway_pid=
}
request_profile() {
  model=$1
  curl -fsS "http://127.0.0.1:$GATEWAY_PORT/v1/responses" -H "Authorization: Bearer $VKEY" -H 'Content-Type: application/json' --data-binary "{\"model\":\"$model\",\"input\":\"synthetic offline request\"}"
}
assert_unknown_reservations() {
  db=$1 expected=$2
  count=$(sqlite3 "$db" "SELECT COUNT(*) FROM requests q JOIN attempts a ON a.request_id=q.id JOIN usage_records u ON u.attempt_id=a.id JOIN reservations r ON r.attempt_id=a.id WHERE q.policy_id='local-policy' AND a.estimate_method='conservative' AND a.estimate_tokens=100 AND r.estimated_tokens=100 AND r.state='conservative' AND r.effective_charge=100 AND r.actual_tokens IS NULL AND u.source='unknown' AND u.completeness='unknown' AND u.input_tokens IS NULL AND u.output_tokens IS NULL;")
  test "$count" -eq "$expected"
  test "$(sqlite3 "$db" "SELECT COUNT(DISTINCT q.model) FROM requests q JOIN attempts a ON a.request_id=q.id JOIN reservations r ON r.attempt_id=a.id WHERE q.policy_id='local-policy' AND a.estimate_method='conservative' AND a.estimate_tokens=100 AND r.estimated_tokens=100 AND r.state='conservative' AND r.effective_charge=100;")" -eq 3
}
start_gateway "$dir/gateway.yaml"
for model in synthetic-ollama synthetic-vllm synthetic-llama; do
  request_profile "$model" >"$dir/$model.response"
  grep -q 'synthetic response from' "$dir/$model.response"
done
"$dir/gateway" admin --db "$DB" --master-key "$KEY" usage summary --json >"$dir/usage.json"
"$dir/gateway" admin --db "$DB" --master-key "$KEY" usage requests --json >"$dir/requests.json"
test "$(sqlite3 "$DB" "SELECT COUNT(*) FROM requests WHERE state='succeeded';")" -eq 3
stop_gateway
start_gateway "$dir/gateway.yaml"
request_profile synthetic-vllm >"$dir/restarted.response"
grep -q 'synthetic response from' "$dir/restarted.response"
test "$(sqlite3 "$DB" "SELECT COUNT(*) FROM requests WHERE state='succeeded';")" -eq 4
assert_unknown_reservations "$DB" 4
stop_gateway

# Back up the populated database only after a clean gateway shutdown. Reopen the
# copy with the original external master key and prove all three routes survive.
sqlite3 "$DB" ".backup '$dir/backup.db'"
cp "$dir/backup.db" "$dir/restored.db"
test "$(sqlite3 "$dir/restored.db" 'PRAGMA integrity_check;')" = ok
test -z "$(sqlite3 "$dir/restored.db" 'PRAGMA foreign_key_check;')"
sed "s|$DB|$dir/restored.db|g" "$dir/gateway.yaml" >"$dir/restored.yaml"
start_gateway "$dir/restored.yaml"
for model in synthetic-ollama synthetic-vllm synthetic-llama; do
  request_profile "$model" >"$dir/$model-restored.response"
  grep -q 'synthetic response from' "$dir/$model-restored.response"
done
stop_gateway
test "$(sqlite3 "$dir/restored.db" "SELECT COUNT(*) FROM requests WHERE state='succeeded';")" -eq 7
assert_unknown_reservations "$dir/restored.db" 7

# Scan every captured CLI/log/response artifact (including usage.json and
# requests.json) for the exact virtual key and both exact upstream keys. The
# one-time key output is the sole intentional virtual-key exception; still scan
# that file for upstream credentials.
for artifact in "$dir"/*.out "$dir"/*.log "$dir"/*.json "$dir"/*.response; do
  [ -f "$artifact" ] || continue
  if [ "$artifact" = "$dir/key.out" ]; then
    ! grep -F -e synthetic-vllm-key -e synthetic-llama-key "$artifact"
  else
    ! grep -F -e "$VKEY" -e synthetic-vllm-key -e synthetic-llama-key "$artifact"
  fi
done
```

SQLite `.backup` is used after graceful shutdown (no live WAL/main-file copy).
The virtual key output is intentionally private and mode 0600; the external
master key is retained unchanged for restore. Synthetic usage is unknown to the
gateway, so configured conservative reservations apply. These checks prove
protected local routing, persistence, and backup mechanics only; they are not
runtime/model inference or compatibility evidence.
