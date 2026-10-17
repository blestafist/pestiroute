# Local M9 Chat Completions smoke

Run from the repository root with pinned Go 1.27.1, Python 3, and curl. This
isolated protected-gateway procedure uses synthetic credentials and
a loopback-only fake Chat backend. It makes no external requests and is not
real-provider or third-party-client compatibility evidence. The private
synthetic key material is removed on exit. Ports 19091 and 19092 must be free.

```sh
set -eu
umask 077
dir=$(mktemp -d "${TMPDIR:-/tmp}/m9-local.XXXXXX")
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
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go build -o "$dir/gateway" ./cmd/gateway
"$dir/gateway" admin --db "$DB" --master-key "$KEY" migrate
"$dir/gateway" admin --db "$DB" --master-key "$KEY" account create --id local-chat-account --connector local-chat
printf '%s\n' synthetic-chat-credential | "$dir/gateway" admin --db "$DB" --master-key "$KEY" credential create --account local-chat-account --id PESTIROUTE_LOCAL_CHAT
"$dir/gateway" admin --db "$DB" --master-key "$KEY" policy create --id local-chat-policy --models local-chat-model --connectors local-chat --rpm 20 --tpm 10000
"$dir/gateway" admin --db "$DB" --master-key "$KEY" key create --policy local-chat-policy >"$dir/key-output"
chmod 600 "$dir/key-output"
KEY_SECRET=$(sed -n 's/.*secret=\([^ ]*\).*/\1/p' "$dir/key-output")
test -n "$KEY_SECRET"

cat >"$dir/fake.py" <<'PY'
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json, os
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        assert self.path == "/v1/chat/completions"
        assert self.headers.get("Authorization") == "Bearer synthetic-chat-credential"
        request = json.loads(body)
        assert request["model"] == "local-chat-model"
        with open(os.environ["FAKE_CALLS"], "a") as calls: calls.write(request.get("case", "json") + "\n")
        if request.get("stream") is True:
            chunks = [
                b'data: {"id":"chatcmpl-sse","choices":[{"index":0,"delta":{"content":"offline"},"finish_reason":null}]}\n\n',
                b'data: {"id":"chatcmpl-sse","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2}}\n\n',
                b'data: [DONE]\n\n',
            ]
            self.send_response(200); self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache"); self.end_headers()
            for chunk in chunks:
                self.wfile.write(chunk); self.wfile.flush()
            return
        if "tools" in request:
            data = b'{"id":"chatcmpl-tool","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_local","type":"function","function":{"name":"local_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}'
        else:
            data = b'{"id":"chatcmpl-json","choices":[{"message":{"role":"assistant","content":"offline"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}'
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data))); self.end_headers(); self.wfile.write(data)
    def log_message(self, *_): pass
ThreadingHTTPServer(("127.0.0.1", 19091), H).serve_forever()
PY
FAKE_CALLS=$dir/fake.calls python3 -u "$dir/fake.py" >"$dir/fake.log" 2>&1 & fake_pid=$!
cat >"$dir/gateway.yaml" <<YAML
version: 1
server: {listen: "127.0.0.1:19092", max_request_bytes: 1048576, shutdown_timeout: 2s}
storage: {driver: sqlite, path: "$DB"}
secrets: {master_key_file: "$KEY"}
connectors:
  - id: local-chat
    kind: connector
    implementation: pestiroute.chat-completions.native
    protocols: [openai.chat-completions.v1]
    settings:
      base_url: http://127.0.0.1:19091/v1/chat/completions
      upstream_protocol: openai.chat-completions.v1
      mode: native
      credential_env: PESTIROUTE_LOCAL_CHAT
      max_request_body_bytes: 1048576
      max_request_header_bytes: 16384
      connect_timeout: 1s
      tls_handshake_timeout: 1s
      response_header_timeout: 3s
      stream_idle_timeout: 5s
      capabilities:
        llm.streaming: supported
        llm.tools: supported
        llm.tools.parallel: supported
        llm.structured_output: supported
routes:
  - id: local-chat-route
    protocol: openai.chat-completions.v1
    mode: native
    model: local-chat-model
    adapter: pestiroute.chat-completions.native
    policy: local-chat-policy
    budget: {unknown_estimate: reserve, conservative_tokens: 100}
    targets: [{connector: local-chat, account: local-chat-account}]
policies: {local-chat-policy: local-chat-policy}
YAML
PESTIROUTE_LOCAL_CHAT=synthetic-chat-credential "$dir/gateway" -config-format yaml -config "$dir/gateway.yaml" >"$dir/gateway.log" 2>&1 & gateway_pid=$!
ready() { i=0; until curl -fsS http://127.0.0.1:19092/readyz >/dev/null 2>&1; do i=$((i+1)); [ "$i" -lt 60 ] || { cat "$dir/gateway.log" >&2; return 1; }; sleep 0.1; done; }
expect_status() {
  expected=$1; shift
  status=$(curl -sS -o "$dir/error.json" -w '%{http_code}' "$@")
  test "$status" = "$expected" || { echo "expected HTTP $expected, got $status" >&2; cat "$dir/error.json" >&2; return 1; }
}
ready

# Native JSON.
curl -fsS http://127.0.0.1:19092/v1/chat/completions -H "Authorization: Bearer $KEY_SECRET" -H 'Content-Type: application/json' --data-binary '{"model":"local-chat-model","messages":[{"role":"user","content":"ping"}]}' >"$dir/json.out"
grep -q 'chatcmpl-json' "$dir/json.out"

# Incremental native SSE, usage-only chunk, and required [DONE].
curl -fsSN http://127.0.0.1:19092/v1/chat/completions -H "Authorization: Bearer $KEY_SECRET" -H 'Content-Type: application/json' --data-binary '{"model":"local-chat-model","messages":[{"role":"user","content":"ping"}],"stream":true,"stream_options":{"include_usage":true}}' >"$dir/sse.out"
grep -q 'chatcmpl-sse' "$dir/sse.out"
grep -q '"prompt_tokens":4' "$dir/sse.out"
test "$(grep -Fc 'data: [DONE]' "$dir/sse.out")" -eq 1

# Native function tool-call response is forwarded from the fake backend.
curl -fsS http://127.0.0.1:19092/v1/chat/completions -H "Authorization: Bearer $KEY_SECRET" -H 'Content-Type: application/json' --data-binary '{"model":"local-chat-model","messages":[{"role":"user","content":"call tool"}],"tools":[{"type":"function","function":{"name":"local_tool","parameters":{"type":"object"}}}]}' >"$dir/tool.out"
grep -q 'call_local' "$dir/tool.out"
grep -q 'tool_calls' "$dir/tool.out"

# Undeclared reasoning is rejected before dispatch.
expect_status 400 http://127.0.0.1:19092/v1/chat/completions -H "Authorization: Bearer $KEY_SECRET" -H 'Content-Type: application/json' --data-binary '{"model":"local-chat-model","messages":[{"role":"user","content":"reason"}],"reasoning_effort":"low"}'
grep -q 'unsupported_capability' "$dir/error.json"
test "$(wc -l <"$dir/fake.calls")" -eq 3

# SQLite records three successful, exactly-once settlements and input/output usage.
"$dir/gateway" admin --db "$DB" --master-key "$KEY" usage requests --json >"$dir/usage.json"
python3 - "$dir/usage.json" <<'PY'
import json, sys
requests = json.load(open(sys.argv[1]))
assert len(requests) == 3, len(requests)
attempts = [a for r in requests for a in r["attempts"]]
assert len(attempts) == 3, len(attempts)
for a in attempts:
    assert a["state"] == "succeeded" and a["reservation"]["state"] == "settled", a
    assert a["usage"]["input_tokens"] > 0 and a["usage"]["output_tokens"] > 0, a
assert sorted((a["usage"]["input_tokens"], a["usage"]["output_tokens"]) for a in attempts) == [(3, 2), (4, 2), (5, 3)]
print("SQLite settlement PASS: 3 succeeded attempts, 3 settled reservations, mapped token usage")
PY
"$dir/gateway" admin --db "$DB" --master-key "$KEY" usage summary
kill -TERM "$gateway_pid"; wait "$gateway_pid"; gateway_pid=
if grep -E 'synthetic-chat-credential|secret=' "$dir"/gateway.log "$dir"/fake.log; then exit 1; fi
echo 'LOCAL-M9 CLI PASS (loopback fake backend; no real provider/client evidence)'
```

The script exercises protected startup, explicit scoped YAML capability
declarations, JSON/SSE/tool-call responses, a pre-dispatch denial for undeclared
reasoning, SQLite usage settlement, and clean shutdown. It proves only local
gateway behavior against the included deterministic fake; it does not establish
external-client interoperability, provider capability, provider usage
exactness, or live compatibility. No real client or provider versions apply.
