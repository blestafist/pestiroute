#!/bin/sh
set -eu

if [ "${1:-}" != --inside ]; then
  command -v unshare >/dev/null
  exec unshare --user --map-root-user --net "$0" --inside
fi
shift

command -v ip >/dev/null
command -v python3 >/dev/null
command -v curl >/dev/null
command -v env >/dev/null
[ -n "${OPENCODE_BIN:-}" ] && [ -x "$OPENCODE_BIN" ] || {
  echo 'Set OPENCODE_BIN to the extracted OpenCode 2.0.6 binary.' >&2
  exit 2
}
version=$("$OPENCODE_BIN" --version)
[ "$version" = 'opencode v2.0.6' ] || {
  echo "Expected opencode v2.0.6, got: $version" >&2
  exit 2
}
echo "Pinned client: $version"

# Fail closed: only loopback exists in this fresh network namespace; no routes.
ip link set lo up
interfaces=$(ip -o link show | wc -l)
routes=$(ip route show)
[ "$interfaces" -eq 1 ] && [ -z "$routes" ] || {
  echo 'Network namespace is not isolated to loopback; refusing dry run.' >&2
  ip -o link show >&2
  ip route show >&2
  exit 2
}
echo 'Egress guard: fresh user/network namespace; loopback only; no routes.'

base=/tmp/opencode
if ! mkdir -p "$base" 2>/dev/null || [ ! -w "$base" ]; then
  base=${TMPDIR:-/tmp}
  mkdir -p "$base"
fi
dir=$(mktemp -d "$base/m1-smoke.XXXXXX")
fake_pid=
gateway_pid=
cleanup() {
  for pid in "$gateway_pid" "$fake_pid"; do [ -z "$pid" ] || kill "$pid" 2>/dev/null || :; done
  for pid in "$gateway_pid" "$fake_pid"; do [ -z "$pid" ] || wait "$pid" 2>/dev/null || :; done
  rm -rf "$dir"
}
trap cleanup EXIT HUP INT TERM

cat >"$dir/fake.py" <<'PY'
from http.server import BaseHTTPRequestHandler, HTTPServer
import json

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        size = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(size))
        auth = self.headers.get("Authorization")
        valid = (self.path == "/v1/responses"
                 and isinstance(body.get("stream"), bool)
                 and auth in ("Bearer synthetic-upstream-only", "Bearer synthetic-client-only"))
        with open("$CAPTURE", "a") as capture:
            capture.write(json.dumps({"method": "POST", "path": self.path,
                "model": body.get("model"), "stream": body.get("stream"),
                "accept": self.headers.get("Accept"),
                "authorization_present": bool(auth),
                "credential_scope": "gateway-upstream" if auth == "Bearer synthetic-upstream-only" else "cli-client" if auth == "Bearer synthetic-client-only" else "unexpected",
                "response_content_type": "text/event-stream" if body["stream"] else "application/json",
                "valid": valid}) + "\n")
        event = {"type": "response.completed", "response": {
            "id": "resp_fake", "object": "response", "status": "completed",
            "model": body["model"], "output": [{"id": "msg_fake",
                "type": "message", "role": "assistant", "status": "completed",
                "content": [{"type": "output_text", "text": "FAKE_OK",
                             "annotations": []}]}]}}
        message = event["response"]["output"][0]
        if body["stream"]:
            events = [
                ("response.created", {"type": "response.created", "response": {"id": "resp_fake", "status": "in_progress"}}),
                ("response.output_item.added", {"type": "response.output_item.added", "output_index": 0, "item": {"id": "msg_fake", "type": "message", "role": "assistant", "status": "in_progress", "content": []}}),
                ("response.content_part.added", {"type": "response.content_part.added", "item_id": "msg_fake", "output_index": 0, "content_index": 0, "part": {"type": "output_text", "text": "", "annotations": []}}),
                ("response.output_text.delta", {"type": "response.output_text.delta", "item_id": "msg_fake", "output_index": 0, "content_index": 0, "delta": "FAKE_OK"}),
                ("response.output_text.done", {"type": "response.output_text.done", "item_id": "msg_fake", "output_index": 0, "content_index": 0, "text": "FAKE_OK"}),
                ("response.content_part.done", {"type": "response.content_part.done", "item_id": "msg_fake", "output_index": 0, "content_index": 0, "part": message["content"][0]}),
                ("response.output_item.done", {"type": "response.output_item.done", "output_index": 0, "item": message}),
                ("response.completed", event),
            ]
            response = b"".join(b"event: " + name.encode() + b"\ndata: " + json.dumps(value).encode() + b"\n\n" for name, value in events)
        else:
            response = json.dumps(event["response"]).encode()
        self.send_response(200 if valid else 400)
        self.send_header("Content-Type", "text/event-stream" if body["stream"] else "application/json")
        self.send_header("Content-Length", str(len(response)))
        self.end_headers()
        self.wfile.write(response)

server = HTTPServer(("127.0.0.1", 0), Handler)
print(server.server_port, flush=True)
server.serve_forever()
PY
sed -i "s|\$CAPTURE|$dir/capture.json|" "$dir/fake.py"
python3 -u "$dir/fake.py" >"$dir/fake.log" 2>&1 & fake_pid=$!
for attempt in $(seq 1 50); do
  fake_port=$(head -n 1 "$dir/fake.log" 2>/dev/null || :)
  [ -n "$fake_port" ] && break
  kill -0 "$fake_pid" 2>/dev/null || { cat "$dir/fake.log" >&2; exit 1; }
  sleep .1
done
[ -n "${fake_port:-}" ] || { cat "$dir/fake.log" >&2; exit 1; }

cat >"$dir/gateway.json" <<JSON
{"listen":"127.0.0.1:8080","shutdown_timeout":"5s","upstream_endpoint":"http://127.0.0.1:$fake_port/v1/responses","upstream_credential_env":"PESTIROUTE_M1_FAKE_KEY","max_request_body_bytes":1048576,"max_request_header_bytes":16384,"connect_timeout":"2s","tls_handshake_timeout":"2s","response_header_timeout":"5s","stream_idle_timeout":"30s"}
JSON
PESTIROUTE_M1_FAKE_KEY=synthetic-upstream-only GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build -o "$dir/gateway" ./cmd/gateway
PESTIROUTE_M1_FAKE_KEY=synthetic-upstream-only "$dir/gateway" -config "$dir/gateway.json" >"$dir/gateway.log" 2>&1 & gateway_pid=$!
for attempt in $(seq 1 50); do
  curl -fsS http://127.0.0.1:8080/readyz >/dev/null 2>&1 && break
  kill -0 "$gateway_pid" 2>/dev/null || { cat "$dir/gateway.log" >&2; exit 1; }
  sleep .1
done
curl -fsS http://127.0.0.1:8080/readyz >/dev/null || { cat "$dir/gateway.log" >&2; exit 1; }
curl -fsS http://127.0.0.1:8080/v1/responses \
  -H 'Content-Type: application/json' -H 'Authorization: Bearer synthetic-client-only' \
  --data-binary '{"model":"gpt-5.4-mini","stream":false,"input":"local fake gateway probe"}' >/dev/null

mkdir -p "$dir/home" "$dir/xdg-config" "$dir/xdg-data" "$dir/project"
cat >"$dir/project/opencode.jsonc" <<JSON
{
  "model": "openai/gpt-5.4-mini",
  "agents": {
    "smoke": {
      "description": "No-tool local transport probe",
      "mode": "primary",
      "permissions": [{"action": "*", "resource": "*", "effect": "deny"}]
    }
  },
  "providers": {
    "openai": {
      "package": "@opencode/ai/providers/openai/responses",
      "name": "OpenAI Responses",
      "models": {
        "gpt-5.4-mini": {
          "name": "GPT-5.4 mini",
          "modelID": "gpt-5.4-mini"
        }
      },
      "settings": {
        "baseURL": "http://127.0.0.1:$fake_port/v1",
        "transport": "http"
      }
    }
  }
}
JSON
printf 'ALPHA-LOCAL-ONLY\n' >"$dir/project/alpha.txt"
printf 'BETA-LOCAL-ONLY\n' >"$dir/project/beta.txt"
(cd "$dir/project" && env -i \
  PATH="$PATH" HOME="$dir/home" XDG_CONFIG_HOME="$dir/xdg-config" \
  XDG_DATA_HOME="$dir/xdg-data" TMPDIR="$dir" TERM=dumb \
  OPENCODE_CONFIG="$dir/project/opencode.jsonc" \
  OPENCODE_DISABLE_AUTOUPDATE=1 OPENAI_API_KEY=synthetic-client-only \
  "$OPENCODE_BIN" run --standalone --agent smoke --title local-smoke --model openai/gpt-5.4-mini \
  'Reply with a short acknowledgement.' >"$dir/client.out" 2>"$dir/client.err") || {
    echo 'Pinned CLI local probe failed; sanitized diagnostics follow:' >&2
    sed -E 's/(Bearer )[A-Za-z0-9._-]+/\1[REDACTED]/g' "$dir/client.err" >&2
    [ ! -f "$dir/capture.json" ] || cat "$dir/capture.json" >&2
    exit 1
  }
grep -q 'FAKE_OK' "$dir/client.out" || {
  echo 'CLI did not return fake completion; local stdout follows:' >&2
  cat "$dir/client.out" >&2
  sed -E 's/(Bearer )[A-Za-z0-9._-]+/\1[REDACTED]/g' "$dir/client.err" >&2
  cat "$dir/capture.json" >&2
  exit 1
}
python3 - "$dir/capture.json" <<'PY'
import json, sys
captures = [json.loads(line) for line in open(sys.argv[1])]
assert len(captures) >= 2, captures
for capture in captures:
    assert capture["method"] == "POST", capture
    assert capture["path"] == "/v1/responses", capture
    assert capture["authorization_present"] is True, capture
    assert capture["valid"] is True, capture
assert {c["credential_scope"] for c in captures} == {"gateway-upstream", "cli-client"}, captures
cli = [c for c in captures if c["credential_scope"] == "cli-client"]
assert any(c["model"] == "gpt-5.4-mini" and c["stream"] is True
           and c["response_content_type"] == "text/event-stream" for c in cli), cli
assert all(c["stream"] is True for c in cli), cli
gateway = next(c for c in captures if c["credential_scope"] == "gateway-upstream")
assert gateway["stream"] is False, gateway
print(json.dumps(captures, sort_keys=True))
PY
if grep -Eq 'synthetic-(upstream-only|client-only)' "$dir/gateway.log" "$dir/client.err"; then
  echo 'synthetic credential leaked to diagnostics' >&2
  exit 1
fi
echo 'OpenCode 2.0.6 loopback HTTP/SSE probe passed; no provider egress possible.'
