#!/usr/bin/env python3
"""Exercise protected M6 operations using real binaries and loopback fixtures."""

import argparse
import concurrent.futures
import http.client
import http.server
import json
import os
from pathlib import Path
import re
import signal
import socket
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time


def eventually(check, timeout=10):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = check()
        if value:
            return value
        time.sleep(0.025)
    raise RuntimeError("deadline expired waiting for fixture condition")


def children(pid):
    found = set()
    for path in Path(f"/proc/{pid}/task").glob("*/children"):
        try:
            found.update(int(p) for p in path.read_text().split())
        except FileNotFoundError:
            pass  # A runtime thread exited during enumeration.
    return sorted(found)


def process_gone(pid):
    try:
        os.killpg(pid, 0)
        return False
    except ProcessLookupError:
        return True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--child-package", default="./cmd/responses-connector-child")
    args = parser.parse_args()
    if sys.platform != "linux":
        parser.error("the M6 process binding requires Linux")
    root = Path(__file__).resolve().parents[1]
    build_env = dict(os.environ, GOTOOLCHAIN="local", GOPROXY="off", GOSUMDB="off")
    if subprocess.check_output(["go", "env", "GOVERSION"], env=build_env).strip() != b"go1.27.1":
        parser.error("Go 1.27.1 is required")

    release_stream, crash_entered, release_crash = (threading.Event() for _ in range(3))
    seen = []
    first = b'event: response.created\ndata: {"type":"response.created","future":true}\n\n'
    last = b'event: response.completed\ndata: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":4}}}\n\n'
    reply = b'{ "id":"offline", "status":"completed", "future":[1,  2],"usage":{"input_tokens":3,"output_tokens":4}}'

    class Upstream(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            body = self.rfile.read(int(self.headers["Content-Length"]))
            seen.append(body)
            if self.path != "/v1/responses" or self.headers.get("Authorization") != "Bearer synthetic-m6":
                self.send_error(400)
                return
            value = json.loads(body)
            if value.get("input") == "crash":
                crash_entered.set()
                release_crash.wait(15)
                return
            self.send_response(200)
            if value.get("stream"):
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                self.wfile.write(first)
                self.wfile.flush()
                if release_stream.wait(15):
                    self.wfile.write(last)
                    self.wfile.flush()
            else:
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(reply)))
                self.end_headers()
                self.wfile.write(reply)

        def log_message(self, *_):
            pass

    upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
    upstream.daemon_threads = True
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    gateway = None
    secret = ""
    groups, socket_dirs = set(), set()
    temporary = tempfile.TemporaryDirectory(prefix="m6-offline-")
    try:
        tmp = temporary.name
        directory = Path(tmp)
        binary, child = directory / "gateway", directory / "child"
        for package, target in [("./cmd/gateway", binary), (args.child_package, child)]:
            subprocess.run(["go", "build", "-o", str(target), package], cwd=root, env=build_env, check=True)
        key, db = directory / "master.key", directory / "gateway.db"
        key.write_bytes(os.urandom(32))
        key.chmod(0o600)

        def admin(*command, data=None):
            return subprocess.check_output([str(binary), "admin", "--db", str(db), "--master-key", str(key), *command], input=data)

        admin("migrate")
        admin("account", "create", "--id", "account-a", "--connector", "upstream")
        admin("credential", "create", "--account", "account-a", "--id", "M6_SYNTHETIC_CREDENTIAL", data=b"synthetic-m6\n")
        admin("policy", "create", "--id", "m6-policy", "--models", "gpt-5.4-mini", "--connectors", "upstream", "--rpm", "30", "--tpm", "30000")
        secret = re.search(rb"secret=([^\s]+)", admin("key", "create", "--policy", "m6-policy")).group(1).decode()
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", 0))
            port = probe.getsockname()[1]
        config = directory / "gateway.yaml"
        config.write_text(f'''version: 1
server: {{listen: "127.0.0.1:{port}", max_request_bytes: 1048576, shutdown_timeout: 2s}}
storage: {{driver: sqlite, path: "{db}"}}
secrets: {{master_key_file: "{key}"}}
connectors:
  - id: upstream
    kind: connector
    implementation: pestiroute.responses.native
    protocols: [openai.responses.v1]
    settings:
      base_url: "http://127.0.0.1:{upstream.server_port}/v1/responses"
      upstream_protocol: openai.responses.v1
      mode: native
      credential_env: M6_SYNTHETIC_CREDENTIAL
      max_request_body_bytes: 1048576
      max_request_header_bytes: 8192
      connect_timeout: 1s
      tls_handshake_timeout: 1s
      response_header_timeout: 10s
      stream_idle_timeout: 10s
    process:
      executable: "{child}"
      args: []
      working_directory: "{directory}"
      environment_allowlist: []
routes:
  - id: route-a
    protocol: openai.responses.v1
    mode: native
    model: gpt-5.4-mini
    adapter: pestiroute.responses.native
    policy: standard
    budget: {{unknown_estimate: reserve, conservative_tokens: 100}}
    targets: [{{connector: upstream, account: account-a}}]
policies: {{standard: m6-policy}}
''')
        with (directory / "gateway.log").open("wb") as log:
            gateway = subprocess.Popen([str(binary), "-config-format", "yaml", "-config", str(config)], stdout=log, stderr=log, env=dict(os.environ, M6_SYNTHETIC_CREDENTIAL="synthetic-m6"))

            def probe(path):
                if gateway.poll() is not None:
                    raise RuntimeError("gateway exited before readiness")
                conn = http.client.HTTPConnection("127.0.0.1", port, timeout=1)
                try:
                    conn.request("GET", path)
                    response = conn.getresponse()
                    response.read()
                    return response.status == 200
                except OSError:
                    return False
                finally:
                    conn.close()

            def remember_child(excluding=0):
                pids = [pid for pid in children(gateway.pid) if pid != excluding]
                if len(pids) != 1:
                    return False
                pid = pids[0]
                try:
                    if Path(f"/proc/{pid}/exe").resolve() != child:
                        return False
                except FileNotFoundError:
                    return False
                if os.getpgid(pid) != pid:
                    raise RuntimeError("child is not a process-group leader")
                env = dict(item.split(b"=", 1) for item in Path(f"/proc/{pid}/environ").read_bytes().split(b"\0") if item)
                assert set(env) == {b"PESTIROUTE_CONNECTOR_SOCKET", b"PESTIROUTE_HOST_SERVICES_SOCKET"}
                socket_dirs.add(Path(os.fsdecode(env[b"PESTIROUTE_CONNECTOR_SOCKET"])).parent)
                groups.add(pid)
                return pid

            def post(body):
                conn = http.client.HTTPConnection("127.0.0.1", port, timeout=15)
                try:
                    conn.request("POST", "/v1/responses", body, {"Authorization": "Bearer " + secret, "Content-Type": "application/json"})
                    response = conn.getresponse()
                    return response.status, response.read()
                finally:
                    conn.close()

            eventually(lambda: probe("/readyz"), 35)
            original = eventually(remember_child)
            request = b'{"model":"gpt-5.4-mini","input":"offline","future":[1,  2]}'
            assert post(request) == (200, reply)
            assert seen == [request], "request bytes changed"
            conn = http.client.HTTPConnection("127.0.0.1", port, timeout=15)
            try:
                conn.request("POST", "/v1/responses", b'{"model":"gpt-5.4-mini","stream":true,"input":"offline"}', {"Authorization": "Bearer " + secret, "Content-Type": "application/json"})
                response = conn.getresponse()
                assert response.status == 200
                assert response.read(len(first)) == first, "first SSE bytes not delivered before completion"
                assert not release_stream.is_set()
                release_stream.set()
                assert response.read() == last
            finally:
                release_stream.set()
                conn.close()
            with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
                result = pool.submit(post, b'{"model":"gpt-5.4-mini","input":"crash"}')
                assert crash_entered.wait(10), "request did not reach upstream"
                os.kill(original, signal.SIGKILL)
                status, _ = result.result(timeout=15)
                assert status >= 400, "crashed invocation falsely succeeded"
            release_crash.set()
            assert gateway.poll() is None and probe("/healthz"), "gateway died with child"
            replacement = eventually(lambda: remember_child(original), 35)
            eventually(lambda: probe("/readyz"), 35)
            assert replacement != original
            # /readyz reflects gateway startup; dispatch admission also checks
            # connector Health. Each unavailable probe below is NEW work.
            def recovered():
                status, body = post(request)
                if status == 503:
                    return False
                if status == 400 and json.loads(body).get("error", {}).get("code") == "unsupported_target":
                    return False  # Route selection rejected this NEW request before dispatch.
                assert (status, body) == (200, reply), f"new work failed after recovery: status={status} body={body!r}"
                return True
            eventually(recovered, 35)
            assert len(seen) == 4, "active request replayed"
            gateway.send_signal(signal.SIGTERM)
            assert gateway.wait(timeout=38) == 0
            for pid in groups:
                eventually(lambda pid=pid: process_gone(pid), 5)
            assert all(not path.exists() for path in socket_dirs), "owned IPC directory leaked"
            with sqlite3.connect(db) as database:
                assert database.execute("SELECT COUNT(*) FROM requests").fetchone()[0] >= 4
                assert database.execute("SELECT COUNT(*) FROM attempts").fetchone()[0] >= 4
                assert database.execute("SELECT COUNT(*) FROM reservations WHERE state='held'").fetchone()[0] == 0
            logs = (directory / "gateway.log").read_bytes()
            assert secret.encode() not in logs and b"synthetic-m6" not in logs
        print("PASS: build/start, readiness, opaque JSON, early SSE, active crash without replay, new-work recovery, SQLite settlement, process/socket cleanup")
    except Exception:
        log_path = Path(temporary.name) / "gateway.log"
        if log_path.exists():
            diagnostic = log_path.read_text(errors="replace")[-8000:]
            if secret:
                diagnostic = diagnostic.replace(secret, "[virtual-key-redacted]")
            diagnostic = diagnostic.replace("synthetic-m6", "[synthetic-secret-redacted]")
            print("gateway diagnostics:\n" + diagnostic, file=sys.stderr)
        raise
    finally:
        release_crash.set()
        release_stream.set()
        if gateway is not None and gateway.poll() is None:
            gateway.terminate()
            try:
                gateway.wait(timeout=38)
            except subprocess.TimeoutExpired:
                gateway.kill()
                gateway.wait()
        for pid in groups:
            if not process_gone(pid):
                os.killpg(pid, signal.SIGKILL)
        upstream.shutdown()
        upstream.server_close()
        temporary.cleanup()


if __name__ == "__main__":
    main()
