# Local M5.1 Codex operations

Offline, synthetic end-to-end procedure. Requires Go 1.27.1, Python 3,
OpenSSL, `curl`, and `sqlite3`. The built gateway CLI performs actual admin auth
start/continue/refresh through an explicit loopback HTTPS CONNECT proxy whose
TLS certificate is trusted only by this shell. It accepts only the synthetic
`auth.openai.com` and `chatgpt.com` names and never forwards traffic. A
test-binary-only composition helper runs the protected gateway against the same
YAML and SQLite database; it injects the same proxy client because the
production Codex transport intentionally pins its provider endpoint and disables
environment proxies. No production endpoint/configuration override is added.

All accounts, credentials, virtual keys, tokens, requests and files are
disposable. No live account, credential, provider connection, or external write
is used. The synthetic CA, proxy, native and Anthropic test endpoints bind only
to loopback. Choose free ports 19093–19095; the CONNECT proxy selects its own.

## Provision, authenticate, stream, recover, disable, restart, and restore

Run the block from the repository root. The trap terminates and waits for every
child before deleting the private temporary directory. Auth command output and
the generated virtual key remain in mode-0600 files; the script checks that
synthetic tokens and opaque device state do not appear in captured output/logs.

```sh
set -eu
umask 077
dir=$(mktemp -d "${TMPDIR:-/tmp}/m51-local.XXXXXX")
pids=
cleanup() {
  for pid in $pids; do kill "$pid" 2>/dev/null || :; done
  for pid in $pids; do wait "$pid" 2>/dev/null || :; done
  rm -rf "$dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
DB=$dir/gateway.db KEY=$dir/master.key
PORT=19093 NATIVE_PORT=19094 ANTHROPIC_PORT=19095

python3 - "$KEY" <<'PY'
import os, sys
fd = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "wb") as f: f.write(os.urandom(32))
PY
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$dir/fake.key" -out "$dir/fake-ca.pem" -days 1 -subj '/CN=PestiRoute synthetic local fixture' -addext 'subjectAltName=DNS:auth.openai.com,DNS:chatgpt.com' >/dev/null 2>&1
chmod 600 "$dir/fake.key" "$dir/fake-ca.pem"
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go build -o "$dir/gateway" ./cmd/gateway
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -c -o "$dir/gateway-test" ./cmd/gateway

cat >"$dir/fake.py" <<'PY'
import base64, http.server, json, os, socketserver, ssl, sys, threading

root, cert, key, port_file = sys.argv[1:]
events = os.path.join(root, "events")
failure = os.path.join(root, "refresh-fail")
lock = threading.Lock()
def event(name):
    with lock, open(events, "a") as f: f.write(name + "\n")
def jwt():
    payload = base64.urlsafe_b64encode(json.dumps({"chatgpt_account_id":"local-codex"}).encode()).decode().rstrip("=")
    return "e30." + payload + ".c2ln"
TOKEN = jwt()
def answer(name, status, body, content_type="application/json"):
    event(name)
    return status, content_type, body

class ProxyHandler(socketserver.BaseRequestHandler):
    def handle(self):
        raw = self.request.makefile("rb")
        line = raw.readline().decode("ascii", "replace").strip()
        headers = {}
        while True:
            h = raw.readline()
            if h in (b"\r\n", b"\n", b""): break
            k, _, v = h.decode("latin1").partition(":")
            headers[k.lower()] = v.strip()
        host = line.split(" ")[1].split(":")[0] if line.startswith("CONNECT ") else ""
        if host not in ("auth.openai.com", "chatgpt.com"):
            return
        self.request.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(cert, key)
        try: conn = context.wrap_socket(self.request, server_side=True)
        except (OSError, ssl.SSLError): return
        stream = conn.makefile("rwb", buffering=0)
        try:
            request = stream.readline().decode("ascii", "replace").strip()
            if not request: return
            method, path, _ = request.split(" ", 2)
            hdr = {}
            while True:
                h = stream.readline()
                if h in (b"\r\n", b"\n", b""): break
                k, _, v = h.decode("latin1").partition(":")
                hdr[k.lower()] = v.strip()
            body = stream.read(int(hdr.get("content-length", "0")))
            if host == "auth.openai.com" and path == "/api/accounts/deviceauth/usercode":
                status, kind, data = answer("device_start", 200, json.dumps({"device_code":"SYNTH_DEVICE_PRIVATE", "device_auth_id":"SYNTH_DEVICE_AUTH_PRIVATE", "user_code":"SAFE-LOCAL-CODE", "verification_uri":"https://auth.openai.com/codex/device", "interval":1, "expires_in":300}).encode())
            elif host == "auth.openai.com" and path == "/api/accounts/deviceauth/token":
                status, kind, data = answer("device_poll", 200, b'{"authorization_code":"SYNTH_CODE_PRIVATE","code_verifier":"SYNTH_VERIFIER_PRIVATE"}')
            elif host == "auth.openai.com" and path == "/oauth/token":
                from urllib.parse import parse_qs
                grant = parse_qs(body.decode()).get("grant_type", [""])[0]
                if grant == "authorization_code":
                    status, kind, data = answer("code_exchange", 200, json.dumps({"access_token":TOKEN, "refresh_token":"SYNTH_REFRESH_PRIVATE", "id_token":TOKEN, "expires_in":3600, "token_type":"Bearer"}).encode())
                elif grant == "refresh_token" and os.path.exists(failure):
                    status, kind, data = answer("refresh_failed", 500, b'{"error":"synthetic_failure"}')
                elif grant == "refresh_token":
                    status, kind, data = answer("refresh", 200, json.dumps({"access_token":TOKEN, "refresh_token":"SYNTH_REFRESH_PRIVATE_2", "id_token":TOKEN, "expires_in":30, "token_type":"Bearer"}).encode())
                else:
                    status, kind, data = answer("unexpected_auth", 400, b'{}')
            elif host == "chatgpt.com" and path == "/backend-api/codex/responses":
                req = json.loads(body)
                if hdr.get("authorization") != "Bearer " + TOKEN or hdr.get("chatgpt-account-id") != "local-codex" or req.get("model") != "gpt-5.4-mini":
                    status, kind, data = answer("bad_codex_request", 403, b'{}')
                else:
                    status, kind, data = answer("codex_response", 200, b'event: response.completed\ndata: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":2}}}\n\n', "text/event-stream")
            else:
                status, kind, data = answer("unexpected_request", 404, b'{}')
            conn.sendall(("HTTP/1.1 %d %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n" % (status, "OK" if status == 200 else "Synthetic", kind, len(data))).encode("ascii") + data)
        except (OSError, ValueError, KeyError, json.JSONDecodeError):
            return
        finally:
            try: conn.close()
            except OSError: pass

class Threaded(socketserver.ThreadingMixIn, socketserver.TCPServer):
    allow_reuse_address = True
    daemon_threads = True
server = Threaded(("127.0.0.1", 0), ProxyHandler)
with open(port_file, "w") as f: f.write(str(server.server_address[1]))
server.serve_forever()
PY

cat >"$dir/local-backends.py" <<'PY'
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json, sys
class Native(BaseHTTPRequestHandler):
    def do_POST(self):
        n=int(self.headers.get("Content-Length","0")); body=json.loads(self.rfile.read(n))
        assert self.path=="/v1/responses" and self.headers.get("Authorization")=="Bearer synthetic-native-key"
        data=b'{"id":"local-native-response","status":"completed"}'
        self.send_response(200); self.send_header("Content-Length",str(len(data))); self.end_headers(); self.wfile.write(data)
    def log_message(self,*_): pass
class Anthropic(BaseHTTPRequestHandler):
    def do_POST(self):
        n=int(self.headers.get("Content-Length","0")); self.rfile.read(n)
        assert self.path=="/v1/messages" and self.headers.get("X-Api-Key")=="synthetic-anthropic-key"
        data=(b'event: message_start\ndata: {"type":"message_start","message":{"usage":{"input_tokens":1}}}\n\n'
              b'event: content_block_start\ndata: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}\n\n'
              b'event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"local-anthropic"}}\n\n'
              b'event: content_block_stop\ndata: {"type":"content_block_stop","index":0}\n\n'
              b'event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}\n\n'
              b'event: message_stop\ndata: {"type":"message_stop"}\n\n')
        self.send_response(200); self.send_header("Content-Type","text/event-stream"); self.send_header("Content-Length",str(len(data))); self.end_headers(); self.wfile.write(data)
    def log_message(self,*_): pass
which,port=sys.argv[1],int(sys.argv[2])
ThreadingHTTPServer(("127.0.0.1",port),Native if which=="native" else Anthropic).serve_forever()
PY

python3 -u "$dir/fake.py" "$dir" "$dir/fake-ca.pem" "$dir/fake.key" "$dir/proxy.port" >"$dir/proxy.log" 2>&1 & pids="$pids $!"
python3 -u "$dir/local-backends.py" native "$NATIVE_PORT" >"$dir/native.log" 2>&1 & pids="$pids $!"
python3 -u "$dir/local-backends.py" anthropic "$ANTHROPIC_PORT" >"$dir/anthropic.log" 2>&1 & pids="$pids $!"
python3 - "$dir/proxy.port" <<'PY'
import os,socket,sys,time
for _ in range(100):
    try:
        port=int(open(sys.argv[1]).read()); s=socket.create_connection(("127.0.0.1",port),.1); s.close(); break
    except (OSError,ValueError,FileNotFoundError): time.sleep(.05)
else: raise SystemExit("synthetic loopback proxy did not start")
PY
PROXY_PORT=$(cat "$dir/proxy.port")
export HTTPS_PROXY="http://127.0.0.1:$PROXY_PORT" https_proxy="http://127.0.0.1:$PROXY_PORT"
export HTTP_PROXY="http://127.0.0.1:$PROXY_PORT" http_proxy="http://127.0.0.1:$PROXY_PORT"
export SSL_CERT_FILE="$dir/fake-ca.pem" NO_PROXY=localhost,127.0.0.1 no_proxy=localhost,127.0.0.1
unset ALL_PROXY all_proxy FTP_PROXY ftp_proxy || :

"$dir/gateway" admin --db "$DB" --master-key "$KEY" migrate
"$dir/gateway" admin --db "$DB" --master-key "$KEY" account create --id local-codex --connector codex
"$dir/gateway" admin --db "$DB" --master-key "$KEY" account create --id local-native --connector local-native
"$dir/gateway" admin --db "$DB" --master-key "$KEY" account create --id local-anthropic --connector local-anthropic
printf '%s\n' '{"placeholder":true}' | "$dir/gateway" admin --db "$DB" --master-key "$KEY" credential create --account local-codex --id oauth >"$dir/credential.out"
printf '%s\n' synthetic-native-key | "$dir/gateway" admin --db "$DB" --master-key "$KEY" credential create --account local-native --id PESTIROUTE_LOCAL_NATIVE >"$dir/native-credential.out"
printf '%s\n' synthetic-anthropic-key | "$dir/gateway" admin --db "$DB" --master-key "$KEY" credential create --account local-anthropic --id local-anthropic-key >"$dir/anthropic-credential.out"
"$dir/gateway" admin --db "$DB" --master-key "$KEY" policy create --id local-policy --models gpt-5.4-mini,local-native-model,local-translation-model --connectors codex,local-native,local-anthropic --rpm 50 --tpm 100000
"$dir/gateway" admin --db "$DB" --master-key "$KEY" key create --policy local-policy >"$dir/key.out"
chmod 600 "$dir/key.out"
VKEY=$(sed -n 's/.*secret=\([^ ]*\).*/\1/p' "$dir/key.out")
test -n "$VKEY"
cat >"$dir/gateway.yaml" <<YAML
version: 1
server: {listen: "127.0.0.1:$PORT", max_request_bytes: 1048576, shutdown_timeout: 2s}
storage: {driver: sqlite, path: "$DB"}
secrets: {master_key_file: "$KEY"}
connectors:
  - id: codex
    kind: connector
    implementation: pestiroute.codex.responses
    protocols: [openai.responses.v1]
    settings: {profile: codex-responses-http-sse-v1, model: gpt-5.4-mini, account_id: local-codex}
  - id: local-native
    kind: connector
    implementation: pestiroute.responses.native
    protocols: [openai.responses.v1]
    settings: {base_url: "http://127.0.0.1:$NATIVE_PORT/v1/responses", upstream_protocol: openai.responses.v1, mode: native, credential_env: PESTIROUTE_LOCAL_NATIVE, max_request_body_bytes: 1048576, max_request_header_bytes: 16384, connect_timeout: 1s, tls_handshake_timeout: 1s, response_header_timeout: 3s, stream_idle_timeout: 5s}
  - id: local-anthropic
    kind: connector
    implementation: pestiroute.anthropic.messages
    protocols: [openai.responses.v1]
    settings: {model: local-translation-model, account_id: local-anthropic, credential_id: local-anthropic-key}
routes:
  - id: codex-route
    protocol: openai.responses.v1
    mode: native
    model: gpt-5.4-mini
    adapter: pestiroute.responses.native
    policy: local
    budget: {unknown_estimate: reserve, conservative_tokens: 4096}
    targets: [{connector: codex, account: local-codex}]
  - id: native-route
    protocol: openai.responses.v1
    mode: native
    model: local-native-model
    adapter: pestiroute.responses.native
    policy: local
    budget: {unknown_estimate: reserve, conservative_tokens: 100}
    targets: [{connector: local-native, account: local-native}]
  - id: anthropic-route
    protocol: openai.responses.v1
    mode: translation
    model: local-translation-model
    adapter: pestiroute.responses.native
    policy: local
    budget: {unknown_estimate: reserve, conservative_tokens: 4096}
    targets: [{connector: local-anthropic, account: local-anthropic}]
policies: {local: local-policy}
YAML

start_gateway() {
  PESTIROUTE_M51_LOCAL_DAEMON=1 PESTIROUTE_M51_CONFIG="$1" PESTIROUTE_M51_DB="$2" PESTIROUTE_M51_KEY="$KEY" PESTIROUTE_M51_ANTHROPIC_URL="http://127.0.0.1:$ANTHROPIC_PORT/v1/messages" PESTIROUTE_LOCAL_NATIVE=synthetic-native-key "$dir/gateway-test" -test.run '^TestM51LocalDaemon$' >"$dir/gateway.log" 2>&1 & gateway_pid=$!; pids="$pids $gateway_pid"
  python3 - "$PORT" <<'PY'
import socket,sys,time
for _ in range(100):
    try:
        s=socket.create_connection(("127.0.0.1",int(sys.argv[1])),.1); s.close(); break
    except OSError: time.sleep(.1)
else: raise SystemExit("local gateway did not start")
PY
  curl -fsS "http://127.0.0.1:$PORT/readyz" >/dev/null
}
stop_gateway() {
  old_pid=$gateway_pid
  kill -TERM "$old_pid"; wait "$old_pid" || { status=$?; [ "$status" -eq 143 ]; }
  pids=$(printf '%s' "$pids" | sed "s/ $old_pid//")
  gateway_pid=
}
codex_request() {
  curl -sS -N -o "$dir/codex.out" -w '%{http_code}' "http://127.0.0.1:$PORT/v1/responses" -H "Authorization: Bearer $VKEY" -H 'Content-Type: application/json' --data-binary '{"model":"gpt-5.4-mini","stream":true,"store":false,"input":"synthetic local"}'
}
reauth() {
  "$dir/gateway" admin --db "$DB" --master-key "$KEY" auth start --account local-codex >"$dir/auth-start.out" 2>"$dir/auth-start.err"
  SESSION=$(sed -n 's/.*session=\([^ ]*\).*/\1/p' "$dir/auth-start.out")
  test -n "$SESSION"
  "$dir/gateway" admin --db "$DB" --master-key "$KEY" auth continue --session "$SESSION" >"$dir/auth-poll.out" 2>"$dir/auth-poll.err"
  "$dir/gateway" admin --db "$DB" --master-key "$KEY" auth continue --session "$SESSION" >"$dir/auth-exchange.out" 2>"$dir/auth-exchange.err"
  grep -q 'authentication complete' "$dir/auth-exchange.out"
}
"$dir/gateway" admin --db "$DB" --master-key "$KEY" auth start --account local-codex >"$dir/auth-start.out" 2>"$dir/auth-start.err"
SESSION=$(sed -n 's/.*session=\([^ ]*\).*/\1/p' "$dir/auth-start.out")
test -n "$SESSION"
"$dir/gateway" admin --db "$DB" --master-key "$KEY" auth continue --session "$SESSION" >"$dir/auth-poll.out" 2>"$dir/auth-poll.err"
"$dir/gateway" admin --db "$DB" --master-key "$KEY" auth continue --session "$SESSION" >"$dir/auth-exchange.out" 2>"$dir/auth-exchange.err"
grep -q 'authentication complete' "$dir/auth-exchange.out"
"$dir/gateway" admin --db "$DB" --master-key "$KEY" auth refresh --account local-codex >"$dir/auth-refresh.out" 2>"$dir/auth-refresh.err"
grep -Eq 'revision=[3-9][0-9]*' "$dir/auth-refresh.out"
test "$(sqlite3 "$DB" "SELECT revision FROM credentials WHERE id='oauth';")" -ge 3
test "$(sqlite3 "$DB" "SELECT length(ciphertext) FROM credentials WHERE id='oauth';")" -gt 40

start_gateway "$dir/gateway.yaml" "$DB"
status=$(codex_request)
test "$status" = 200
grep -q 'response.completed' "$dir/codex.out"
test "$(grep -c '^refresh$' "$dir/events")" -ge 2
test "$(grep -c '^codex_response$' "$dir/events")" -eq 1

# Failed freshness refresh quarantines the account. Restart retains the denial;
# explicit interactive reauthentication clears quarantine before the next send.
touch "$dir/refresh-fail"
status=$(codex_request)
test "$status" != 200
stop_gateway
start_gateway "$dir/gateway.yaml" "$DB"
refreshes=$(grep -c '^refresh_failed$' "$dir/events")
status=$(codex_request)
test "$status" != 200
test "$(grep -c '^refresh_failed$' "$dir/events")" -eq "$refreshes"
stop_gateway
rm "$dir/refresh-fail"
reauth
start_gateway "$dir/gateway.yaml" "$DB"
status=$(codex_request)
test "$status" = 200
grep -q 'response.completed' "$dir/codex.out"

# Actual sibling routes use their own synthetic local accounts and credentials.
curl -fsS "http://127.0.0.1:$PORT/v1/responses" -H "Authorization: Bearer $VKEY" -H 'Content-Type: application/json' --data-binary '{"model":"local-native-model","input":"synthetic"}' | grep -q local-native-response
curl -fsS "http://127.0.0.1:$PORT/v1/responses" -H "Authorization: Bearer $VKEY" -H 'Content-Type: application/json' --data-binary '{"model":"local-translation-model","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic"}]}],"max_output_tokens":32}' | grep -q local-anthropic

# Disable must reject before dispatch while the daemon is live. Re-enable and
# reauthenticate without changing the running listener.
"$dir/gateway" admin --db "$DB" --master-key "$KEY" account disable local-codex >/dev/null
codex_before=$(grep -c '^codex_response$' "$dir/events")
status=$(codex_request)
test "$status" != 200
test "$(grep -c '^codex_response$' "$dir/events")" -eq "$codex_before"
"$dir/gateway" admin --db "$DB" --master-key "$KEY" account enable local-codex >/dev/null
reauth
test "$(codex_request)" = 200
stop_gateway

"$dir/gateway" admin --db "$DB" --master-key "$KEY" usage summary --json >"$dir/usage.json"
"$dir/gateway" admin --db "$DB" --master-key "$KEY" usage requests --json >"$dir/requests.json"
grep -q 'gpt-5.4-mini' "$dir/requests.json"
test "$(sqlite3 "$DB" "SELECT COUNT(*) FROM requests WHERE state='succeeded' AND model='gpt-5.4-mini';")" -ge 2
sqlite3 "$DB" ".backup '$dir/backup.db'"
cp "$dir/backup.db" "$dir/restored.db"
test "$(sqlite3 "$dir/restored.db" 'PRAGMA integrity_check;')" = ok
test -z "$(sqlite3 "$dir/restored.db" 'PRAGMA foreign_key_check;')"
sed "s|$DB|$dir/restored.db|g" "$dir/gateway.yaml" >"$dir/restored.yaml"
start_gateway "$dir/restored.yaml" "$dir/restored.db"
test "$(codex_request)" = 200
grep -q 'response.completed' "$dir/codex.out"
"$dir/gateway" admin --db "$dir/restored.db" --master-key "$KEY" usage summary --json >"$dir/restored-usage.json"
test "$(sqlite3 "$dir/restored.db" "SELECT COUNT(*) FROM requests WHERE state='succeeded' AND model='gpt-5.4-mini';")" -ge 3
stop_gateway

if grep -E 'SYNTH_(DEVICE|DEVICE_AUTH|CODE|VERIFIER|ACCESS|REFRESH)_PRIVATE|secret=' "$dir"/auth-*.out "$dir"/auth-*.err "$dir"/gateway.log "$dir"/proxy.log; then exit 1; fi
```

The shell executes the real compiled `gateway admin` binary for all three auth
commands. Its only authentication/network peer is the loopback TLS proxy; the
proxy's allowlist has no upstream forwarding path. It encrypts the resulting
OAuth bundle in the real disposable SQLite database. Captured auth output is
checked for tokens and private device/code state; the database query reveals
only encrypted byte length and revision.

`cmd/gateway/m51_local_daemon_test.go` is compiled only into the test binary.
It loads the same YAML and DB and serves a real protected HTTP listener; the
factory substitutes a proxy-enabled client only for the Codex test Connector.
There is no production endpoint override. The local TLS peer checks Codex bearer
and account headers, answers streaming Responses requests, rotates credentials,
and can fail refresh deterministically. The script proves proactive refresh,
streamed completion, durable usage, failure quarantine across restart,
interactive-auth recovery, and disabled-account zero-dispatch denial. Native and
Anthropic routes are dispatched to independent loopback fake backends.

All gateway instances are stopped before backup. The restored YAML points at a
new DB path and the same external master key; the restored daemon must stream a
successful Codex response, and restored usage records must include both prior
requests and the restored dispatch. Never copy a live WAL database or retain
the synthetic master/virtual keys. Local results are not live auth, entitlement,
model availability or provider compatibility evidence; see
[M5.1 compatibility limits](M5.1-COMPATIBILITY.md).
