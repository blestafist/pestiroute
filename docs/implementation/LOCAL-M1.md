# Local M1 proxy check

This verifies the native path locally only. It is not real-client or live-provider compatibility evidence. Requires Go 1.27.1, Python 3 and curl; it uses loopback listeners and synthetic credentials only.

From the repository root, create a temporary workspace and fake `POST /v1/responses` target:

```sh
set -eu
dir=$(mktemp -d /tmp/opencode/m1-local.XXXXXX)
fake_pid= gateway_pid=
cleanup() {
  for pid in "$gateway_pid" "$fake_pid"; do [ -z "$pid" ] || kill "$pid" 2>/dev/null || :; done
  for pid in "$gateway_pid" "$fake_pid"; do [ -z "$pid" ] || wait "$pid" 2>/dev/null || :; done
  rm -rf "$dir"
}
trap cleanup EXIT
cat >"$dir/fake.py" <<'PY'
from http.server import BaseHTTPRequestHandler, HTTPServer

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        assert self.path == "/v1/responses"
        assert self.headers.get("Authorization") == "Bearer synthetic-only"
        assert b'"unknown"' in body
        payload = b'{"id":"local-fake","status":"completed"}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

server = HTTPServer(("127.0.0.1", 0), Handler)
print(server.server_port, flush=True)
server.serve_forever()
PY
python3 -u "$dir/fake.py" >"$dir/fake.log" 2>&1 & fake_pid=$!
for attempt in $(seq 1 50); do
  fake_port=$(head -n 1 "$dir/fake.log" 2>/dev/null || :)
  [ -n "$fake_port" ] && curl -fsS "http://127.0.0.1:$fake_port/healthz" >/dev/null && break
  kill -0 "$fake_pid" 2>/dev/null || { cat "$dir/fake.log"; exit 1; }
  sleep .1
done
[ -n "${fake_port:-}" ] && curl -fsS "http://127.0.0.1:$fake_port/healthz" >/dev/null || { cat "$dir/fake.log"; exit 1; }
cat >"$dir/gateway.json" <<JSON
{"listen":"127.0.0.1:0","shutdown_timeout":"5s","upstream_endpoint":"http://127.0.0.1:$fake_port/v1/responses","upstream_credential_env":"PESTIROUTE_LOCAL_FAKE_KEY","max_request_body_bytes":1048576,"max_request_header_bytes":16384,"connect_timeout":"2s","tls_handshake_timeout":"2s","response_header_timeout":"5s","stream_idle_timeout":"30s"}
JSON
PESTIROUTE_LOCAL_FAKE_KEY=synthetic-only GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build -o "$dir/gateway" ./cmd/gateway
PESTIROUTE_LOCAL_FAKE_KEY=synthetic-only "$dir/gateway" -config "$dir/gateway.json" >"$dir/gateway.log" 2>&1 & gateway_pid=$!
for attempt in $(seq 1 50); do
  gateway_addr=$(sed -n 's/^gateway: listening on //p' "$dir/gateway.log" | head -n 1)
  [ -n "$gateway_addr" ] && curl -fsS "http://$gateway_addr/readyz" >/dev/null && break
  kill -0 "$gateway_pid" 2>/dev/null || { cat "$dir/gateway.log"; cat "$dir/fake.log"; exit 1; }
  sleep .1
done
[ -n "${gateway_addr:-}" ] && curl -fsS "http://$gateway_addr/readyz" >/dev/null || { cat "$dir/gateway.log"; cat "$dir/fake.log"; exit 1; }
curl -fsS "http://$gateway_addr/healthz"
curl -fsS "http://$gateway_addr/readyz"
curl -fsS "http://$gateway_addr/v1/responses" -H 'Content-Type: application/json' -H 'Authorization: Bearer synthetic-client-only' --data-binary '{"model":"gpt-5.4-mini","unknown":{"preserve":true}}'
if grep -Eq 'synthetic-(only|client-only)' "$dir/gateway.log"; then echo 'synthetic credential leaked to gateway stderr' >&2; cat "$dir/gateway.log" >&2; exit 1; fi
```

The required inference fields are `upstream_endpoint`, `upstream_credential_env`, positive body/header byte limits, and positive Go duration values for connect, TLS handshake, response-header and stream-idle timeouts. Inference listeners must bind numeric loopback (`127.0.0.1` or `::1`). The fake accepts only the synthetic selected bearer token and returns fixed JSON. Neither token is a provider credential; don't put real secrets in config, shell history, or logs.

For local regressions and the observational native baseline run:

```sh
./scripts/check.sh
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -v ./cmd/gateway/...
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -count=1 -v -run 'Test.*Baseline' ./cmd/gateway/...
```

`TestNativeBaseline` warms both paths five times using the same `http.Client` transport, then gathers 25 alternating single-request samples per path and reports min/median/p95/max for TTFB and complete response. Run it several times with `-count=1` to expose local variance. It separately snapshots signed whole-process heap deltas for 4 and 8 concurrently active direct and gateway SSE streams; the divided number is a process-level per-stream estimate, not gateway-attributed memory. It confirms every client goroutine exits and each fake upstream reports cancellation before resource teardown. This is a small local sample, not a capacity test or budget. Compare only runs with the same Go version, host and load; fake results say nothing about external client/provider behavior.
