# Local M2 topology check

This demonstrates the implemented local M2 JSON startup path only. It uses Go 1.27.1, Python 3 and curl, synthetic environment credentials, and loopback listeners; it makes no external requests and is not real-provider compatibility evidence. Run from the repository root. The binary is built offline from this checkout into a temporary directory.

```sh
set -eu
dir=$(mktemp -d "${TMPDIR:-/tmp}/m2-local.XXXXXX")
fake_pid= gateway_pid=
cleanup() {
  for pid in "$gateway_pid" "$fake_pid"; do [ -z "$pid" ] || kill "$pid" 2>/dev/null || :; done
  for pid in "$gateway_pid" "$fake_pid"; do [ -z "$pid" ] || wait "$pid" 2>/dev/null || :; done
  rm -rf "$dir"
}
trap cleanup EXIT
cat >"$dir/fake.py" <<'PY'
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from threading import Thread
import json, os, pathlib, sys

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        assert self.path == "/v1/responses"
        assert self.headers.get("Authorization") == "Bearer " + self.server.key
        assert json.loads(body)["model"] == self.server.model
        payload = json.dumps({"id": self.server.marker, "status": "completed"}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)
    def log_message(self, *_): pass

servers = []
for model, marker, key in (("model-a", "fake-a", "synthetic-a"),
                           ("model-b", "fake-b", "synthetic-b"),
                           ("gpt-5.4-mini", "fake-legacy", "synthetic-legacy")):
    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    server.model, server.marker, server.key = model, marker, key
    Thread(target=server.serve_forever, daemon=True).start()
    servers.append(server)
pathlib.Path(sys.argv[1]).write_text("\n".join(str(s.server_port) for s in servers))
import signal
signal.pause()
PY
python3 -u "$dir/fake.py" "$dir/ports" >"$dir/fake.log" 2>&1 & fake_pid=$!
for attempt in $(seq 1 50); do [ -s "$dir/ports" ] && break; kill -0 "$fake_pid" 2>/dev/null || { cat "$dir/fake.log"; exit 1; }; sleep .1; done
[ -s "$dir/ports" ] || { cat "$dir/fake.log"; exit 1; }
port_a=$(sed -n '1p' "$dir/ports"); port_b=$(sed -n '2p' "$dir/ports"); port_legacy=$(sed -n '3p' "$dir/ports")
cat >"$dir/topology.json" <<JSON
{"listen":"127.0.0.1:0","shutdown_timeout":"1s","components":[{"id":"adapter","implementation":"pestiroute.responses.native","kind":"adapter"},{"id":"connector-a","implementation":"pestiroute.responses.native","kind":"connector","endpoint":"http://127.0.0.1:$port_a/v1/responses","credential_env":"PESTIROUTE_LOCAL_A","max_request_body_bytes":1048576,"max_request_header_bytes":16384,"connect_timeout":"2s","tls_handshake_timeout":"2s","response_header_timeout":"5s","stream_idle_timeout":"30s"},{"id":"connector-b","implementation":"pestiroute.responses.native","kind":"connector","endpoint":"http://127.0.0.1:$port_b/v1/responses","credential_env":"PESTIROUTE_LOCAL_B","max_request_body_bytes":1048576,"max_request_header_bytes":16384,"connect_timeout":"2s","tls_handshake_timeout":"2s","response_header_timeout":"5s","stream_idle_timeout":"30s"}],"routes":[{"protocol":"openai.responses.v1","mode":"native","model":"model-a","account":"account-a","adapter":"adapter","connector":"connector-a"},{"protocol":"openai.responses.v1","mode":"native","model":"model-b","account":"account-b","adapter":"adapter","connector":"connector-b"}]}
JSON
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build -o "$dir/gateway" ./cmd/gateway
PESTIROUTE_LOCAL_A=synthetic-a PESTIROUTE_LOCAL_B=synthetic-b "$dir/gateway" -config "$dir/topology.json" >"$dir/gateway.log" 2>&1 & gateway_pid=$!
for attempt in $(seq 1 50); do gateway_addr=$(sed -n 's/^gateway: listening on //p' "$dir/gateway.log" | head -n 1); [ -n "${gateway_addr:-}" ] && curl -fsS "http://$gateway_addr/readyz" >/dev/null 2>&1 && break; kill -0 "$gateway_pid" 2>/dev/null || { cat "$dir/gateway.log"; exit 1; }; sleep .1; done
[ -n "${gateway_addr:-}" ] || { cat "$dir/gateway.log"; exit 1; }
curl -fsS "http://$gateway_addr/healthz" >/dev/null
curl -fsS "http://$gateway_addr/readyz" >/dev/null
for model in model-a model-b; do curl -fsS "http://$gateway_addr/v1/responses" -H 'Content-Type: application/json' -H 'Authorization: Bearer synthetic-client-only' --data-binary "{\"model\":\"$model\"}"; echo; done
if grep -Eq 'synthetic-(a|b|client-only|legacy)' "$dir/gateway.log"; then echo 'synthetic credential leaked to gateway stderr' >&2; exit 1; fi
python3 - "$gateway_pid" <<'PY'
import os, signal, sys, time
pid = int(sys.argv[1]); start = time.monotonic(); os.kill(pid, signal.SIGTERM)
while time.monotonic() - start < 3:
    try:
        if os.waitpid(pid, os.WNOHANG)[0] == pid: break
    except ChildProcessError: break
    time.sleep(.02)
else: raise SystemExit("gateway did not shut down within 3s")
PY
gateway_pid=

# The old single-target fields remain accepted and normalize to the built-in
# Responses adapter/connector and one gpt-5.4-mini native route.
cat >"$dir/legacy.json" <<JSON
{"listen":"127.0.0.1:0","shutdown_timeout":"1s","upstream_endpoint":"http://127.0.0.1:$port_legacy/v1/responses","upstream_credential_env":"PESTIROUTE_LOCAL_LEGACY","max_request_body_bytes":1048576,"max_request_header_bytes":16384,"connect_timeout":"2s","tls_handshake_timeout":"2s","response_header_timeout":"5s","stream_idle_timeout":"30s"}
JSON
PESTIROUTE_LOCAL_LEGACY=synthetic-legacy "$dir/gateway" -config "$dir/legacy.json" >"$dir/legacy.log" 2>&1 & gateway_pid=$!
for attempt in $(seq 1 50); do legacy_addr=$(sed -n 's/^gateway: listening on //p' "$dir/legacy.log" | head -n 1); [ -n "${legacy_addr:-}" ] && curl -fsS "http://$legacy_addr/readyz" >/dev/null 2>&1 && break; kill -0 "$gateway_pid" 2>/dev/null || { cat "$dir/legacy.log"; exit 1; }; sleep .1; done
[ -n "${legacy_addr:-}" ] || { cat "$dir/legacy.log"; exit 1; }
curl -fsS "http://$legacy_addr/v1/responses" -H 'Content-Type: application/json' -H 'Authorization: Bearer synthetic-client-only' --data-binary '{"model":"gpt-5.4-mini"}'
if grep -Eq 'synthetic-(legacy|client-only)' "$dir/legacy.log"; then echo 'synthetic credential leaked to gateway stderr' >&2; exit 1; fi
python3 - "$gateway_pid" <<'PY'
import os, signal, sys, time
pid = int(sys.argv[1]); start = time.monotonic(); os.kill(pid, signal.SIGTERM)
while time.monotonic() - start < 3:
    try:
        if os.waitpid(pid, os.WNOHANG)[0] == pid: break
    except ChildProcessError: break
    time.sleep(.02)
else: raise SystemExit("legacy gateway did not shut down within 3s")
PY
gateway_pid=
```

Each fake target validates its expected model and synthetic bearer token and returns a distinct marker (`fake-a`, `fake-b`, or `fake-legacy`). The multi-route calls therefore prove dispatch to two separate connector instances; the legacy call proves the old fields normalize to the fixed `gpt-5.4-mini` route. Values are synthetic; do not substitute provider credentials. The example verifies absence of credentials in gateway stderr, not arbitrary operating-system process inspection.
