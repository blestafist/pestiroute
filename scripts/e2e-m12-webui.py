#!/usr/bin/env python3
"""Exercise the packaged gateway's operator and inference lifecycle offline."""

import argparse
import http.server
import json
import os
import pathlib
import secrets
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


class FakeUpstream(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if self.path != "/v1/responses" or body.get("model") != "m12-model":
            self.send_error(400)
            return
        if self.headers.get("Authorization") != "Bearer synthetic-upstream-secret":
            self.send_error(401)
            return
        payload = json.dumps({"id": "m12-fake-response", "status": "completed", "usage": {"input_tokens": 3, "output_tokens": 2}}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_):
        pass


def main(serve=False):
    root = pathlib.Path(__file__).resolve().parents[1]
    with tempfile.TemporaryDirectory(prefix="m12-e2e-", dir=os.environ.get("TMPDIR")) as temp:
        directory = pathlib.Path(temp)
        binary, database, master_key = directory / "gateway", directory / "gateway.db", directory / "master.key"
        operator_token = secrets.token_urlsafe(32)
        upstream = http.server.ThreadingHTTPServer(("127.0.0.1", free_port()), FakeUpstream)
        threading.Thread(target=upstream.serve_forever, daemon=True).start()
        client_port, admin_port = free_port(), free_port()
        gateway = None
        try:
            master_key.write_bytes(secrets.token_bytes(32))
            master_key.chmod(0o600)
            gateway_binary = os.environ.get("M12_E2E_GATEWAY") if serve else binary
            if serve and (not gateway_binary or not pathlib.Path(gateway_binary).is_file()):
                raise RuntimeError("--serve requires M12_E2E_GATEWAY to name a packaged gateway binary")
            if not serve:
                subprocess.run(["go", "build", "-o", binary, "./cmd/gateway"], cwd=root, check=True)
            subprocess.run([gateway_binary, "admin", "--db", database, "--master-key", master_key, "migrate"], cwd=root, check=True, stdout=subprocess.DEVNULL)
            subprocess.run([gateway_binary, "admin", "--db", database, "--master-key", master_key, "account", "create", "--id", "m12-runtime", "--connector", "m12-connector"], cwd=root, check=True, stdout=subprocess.DEVNULL)
            subprocess.run([gateway_binary, "admin", "--db", database, "--master-key", master_key, "credential", "create", "--account", "m12-runtime", "--id", "M12_RUNTIME_CREDENTIAL"], cwd=root, input="synthetic-upstream-secret\n", text=True, check=True, stdout=subprocess.DEVNULL)
            subprocess.run([gateway_binary, "admin", "--db", database, "--master-key", master_key, "policy", "create", "--id", "m12-route-policy", "--models", "m12-model", "--connectors", "m12-connector", "--rpm", "20", "--tpm", "10000"], cwd=root, check=True, stdout=subprocess.DEVNULL)
            config_path = directory / "gateway.json"
            config_path.write_text(f'''version: 1
server: {{listen: "127.0.0.1:{client_port}", max_request_bytes: 1048576, shutdown_timeout: 2s}}
storage: {{driver: sqlite, path: "{database}"}}
secrets: {{master_key_file: "{master_key}"}}
connectors:
  - id: m12-connector
    kind: connector
    implementation: pestiroute.responses.native
    protocols: [openai.responses.v1]
    settings: {{base_url: "http://127.0.0.1:{upstream.server_port}/v1/responses", upstream_protocol: openai.responses.v1, mode: native, credential_env: M12_RUNTIME_CREDENTIAL, max_request_body_bytes: 1048576, max_request_header_bytes: 16384, connect_timeout: 1s, tls_handshake_timeout: 1s, response_header_timeout: 2s, stream_idle_timeout: 3s}}
routes:
  - id: m12-route
    protocol: openai.responses.v1
    mode: native
    model: m12-model
    adapter: pestiroute.responses.native
    policy: runtime
    budget: {{unknown_estimate: reserve, conservative_tokens: 100}}
    targets: [{{connector: m12-connector, account: m12-runtime}}]
policies: {{runtime: m12-route-policy}}
admin: {{listen: "127.0.0.1:{admin_port}", token_env: M12_E2E_OPERATOR, read_timeout: 3s, write_timeout: 3s, idle_timeout: 3s}}
''')
            gateway = subprocess.Popen([gateway_binary, "-config-format", "yaml", "-config", config_path], cwd=root,
                                       env={**os.environ, "M12_E2E_OPERATOR": operator_token, "M12_RUNTIME_CREDENTIAL": "synthetic-upstream-secret"},
                                       stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
            admin = f"http://127.0.0.1:{admin_port}"
            client = f"http://127.0.0.1:{client_port}"

            def call(method, path, payload=None, auth=True, expected=200):
                headers = {"Host": f"127.0.0.1:{admin_port}"}
                if auth:
                    headers["Authorization"] = f"Bearer {operator_token}"
                data = None if payload is None else json.dumps(payload).encode()
                if data is not None:
                    headers["Content-Type"] = "application/json"
                request = urllib.request.Request(admin + path, data=data, headers=headers, method=method)
                try:
                    response = urllib.request.urlopen(request, timeout=2)
                except urllib.error.HTTPError as error:
                    response = error
                body = response.read()
                if response.status != expected:
                    raise AssertionError(f"{method} {path}: HTTP {response.status}, expected {expected}")
                return json.loads(body) if body else {}

            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                if gateway.poll() is not None:
                    raise RuntimeError(gateway.stderr.read().decode())
                try:
                    call("GET", "/admin/v1/status", auth=False, expected=401)
                    break
                except (OSError, urllib.error.URLError):
                    time.sleep(0.05)
            else:
                raise TimeoutError("admin listener did not start")
            status = call("GET", "/admin/v1/status")
            assert status["routes"][0]["model"] == "m12-model"
            with urllib.request.urlopen(client + "/readyz", timeout=2) as readiness:
                assert readiness.status == 200 and readiness.read().strip() == b"ok"
            if serve:
                print(json.dumps({"admin": admin, "client": client, "operator_token": operator_token}), flush=True)
                sys.stdin.readline()
                return
            call("POST", "/admin/v1/accounts", {"id": "m12-operator-account", "connector": "m12-connector"}, expected=201)
            credential = call("POST", "/admin/v1/accounts/m12-operator-account/credentials", {"id": "m12-operator-credential", "secret": "write-only-synthetic-credential"}, expected=201)
            assert "secret" not in credential
            call("POST", "/admin/v1/policies", {"id": "m12-policy", "models": ["m12-model"], "connectors": ["m12-connector"], "rpm": 10, "tpm": 1000}, expected=201)
            issued = call("POST", "/admin/v1/keys", {"policy_id": "m12-policy", "policy_revision": 1}, expected=201)
            virtual_key = issued["secret"]
            assert virtual_key
            infer = urllib.request.Request(client + "/v1/responses", data=b'{"model":"m12-model","input":"synthetic"}',
                                           headers={"Authorization": f"Bearer {virtual_key}", "Content-Type": "application/json"})
            try:
                with urllib.request.urlopen(infer, timeout=3) as response:
                    inference_result = response.read()
                    assert response.status == 200 and b"m12-fake-response" in inference_result
            except urllib.error.HTTPError as error:
                raise AssertionError(f"inference returned HTTP {error.code}: {error.read().decode()}") from None
            call("POST", "/admin/v1/policies", {"id": "m12-deny-policy", "models": ["m12-model"], "connectors": ["not-m12-connector"], "rpm": 10, "tpm": 1000}, expected=201)
            denied_key = call("POST", "/admin/v1/keys", {"policy_id": "m12-deny-policy", "policy_revision": 1}, expected=201)["secret"]
            denied = urllib.request.Request(client + "/v1/responses", data=b'{"model":"m12-model","input":"must not dispatch"}',
                                            headers={"Authorization": f"Bearer {denied_key}", "Content-Type": "application/json"})
            try:
                urllib.request.urlopen(denied, timeout=3)
                raise AssertionError("restricted key unexpectedly admitted a disallowed model")
            except urllib.error.HTTPError as error:
                assert error.code == 403
            ledger = call("GET", "/admin/v1/usage/requests")
            attempts = call("GET", "/admin/v1/usage/attempts")
            assert ledger["total"] == 1 and len(ledger["items"][0]["attempts"]) == 1
            assert attempts["total"] == 1
            call("DELETE", "/admin/v1/keys/" + issued["key"]["id"])
            rejected = urllib.request.Request(client + "/v1/responses", data=b'{"model":"m12-model","input":"after revoke"}',
                                              headers={"Authorization": f"Bearer {virtual_key}", "Content-Type": "application/json"})
            try:
                urllib.request.urlopen(rejected, timeout=3)
                raise AssertionError("revoked key inference unexpectedly succeeded")
            except urllib.error.HTTPError as error:
                assert error.code == 401
            print("PASS: real gateway + SQLite admin lifecycle, restricted fake-upstream inference, ledger, revocation")
        finally:
            if gateway is not None:
                gateway.terminate()
                try:
                    gateway.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    gateway.kill()
                    gateway.wait()
            upstream.shutdown()
            upstream.server_close()


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--serve", action="store_true", help="serve the packaged gateway fixture for browser tests")
    main(parser.parse_args().serve)
