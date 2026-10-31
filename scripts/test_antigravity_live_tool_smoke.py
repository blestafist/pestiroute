"""Synthetic streamed gateway transcripts; no provider or operator files are used."""
import http.server
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import threading
import unittest

SCRIPT = Path(__file__).with_name("antigravity_live_tool_smoke.py")
CANARIES = ("PRIVATE_OUTPUT_CANARY", "OPAQUE_CIPHERTEXT_CANARY", "PRIVATE_CALL_ID_CANARY", "PRIVATE_ARGS_CANARY")


class FixtureServer(http.server.ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True


class ChunkedServer(http.server.ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def handle_error(self, *_args):
        pass


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        self.server.requests.append(body)
        turn = len(self.server.requests)
        spec = self.server.transcripts[turn - 1] if turn <= len(self.server.transcripts) else b""
        status, media, wire = spec if isinstance(spec, tuple) else (200, "text/event-stream", spec)
        self.send_response(status)
        self.send_header("Content-Type", media)
        self.end_headers()
        try:
            self.wfile.write(wire)
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass


class ChunkedHandler(Handler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        with self.server.lock:
            index = len(self.server.requests)
            self.server.requests.append(body)
            self.server.client_ports.append(self.client_address[1])
        output = ToolSmokeTest.first_output_fixture()
        if index % 2 == 0:
            wire = (event("response.output_text.delta", {"delta": "first harmless delta"})
                    + event("response.completed", {"response": {"status": "completed", "output": output}}))
        else:
            wire = (event("response.output_text.delta", {"delta": "second harmless delta"})
                    + event("response.completed", {"response": {
                        "status": "completed", "output": [{"id": "answer", "type": "message"}]}}))
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        # Split even within SSE lines: both events and the terminating chunk are real HTTP framing.
        for chunk in (wire[:11], wire[11:47], wire[47:]):
            if chunk:
                self.wfile.write(f"{len(chunk):X}\r\n".encode() + chunk + b"\r\n")
                self.wfile.flush()
        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()


def event(name, value):
    return (f"event: {name}\ndata: {json.dumps(value, separators=(',', ':'))}\n\n").encode()


def transcript(output, *, text=None, complete=True):
    wire = b""
    if text is not None:
        wire += event("response.output_text.delta", {"delta": text})
    if complete:
        wire += event("response.completed", {"response": {"status": "completed", "output": output}})
    return wire


class ToolSmokeTest(unittest.TestCase):
    def run_fixture(self, transcripts):
        server = FixtureServer(("127.0.0.1", 0), Handler)
        server.requests = []
        server.transcripts = transcripts
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            return subprocess.run([sys.executable, str(SCRIPT), str(server.server_address[1])],
                                  input="SYNTHETIC_KEY\n", text=True, capture_output=True, timeout=5), server.requests
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=2)

    def first_output(self):
        return self.first_output_fixture()

    @staticmethod
    def first_output_fixture():
        return [
            {"id": "reasoning-id", "type": "reasoning", "summary": [], "status": "completed",
             "encrypted_content": CANARIES[1]},
            {"id": "call-item", "type": "function_call", "call_id": CANARIES[2], "name": "add",
             "arguments": json.dumps({"a": 17, "b": 25})},
        ]

    def test_two_turn_exact_native_history_and_private_output(self):
        output = self.first_output()
        result, requests = self.run_fixture([
            transcript(output), transcript([{"id": "answer", "type": "message"}], text=CANARIES[0])])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(requests), 2)
        first, second = requests
        self.assertEqual(first["model"], "gemini-2.5-flash")
        self.assertNotIn("store", first)
        self.assertNotIn("store", second)
        self.assertEqual(first["include"], ["reasoning.encrypted_content"])
        self.assertEqual(first["tools"][0]["name"], "add")
        self.assertEqual(first["tool_choice"], {"type": "function", "name": "add"})
        self.assertEqual(second["input"], [
            {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "Add 17 and 25."}]},
            *output,
            {"type": "function_call_output", "call_id": CANARIES[2], "output": "42"},
        ])
        self.assertEqual(second["include"], first["include"])
        self.assertIn("dispatches=2", result.stdout)
        self.assertIn("turn=1 status=200 failure=none", result.stdout)
        self.assertIn("tool_name_matched=true args_valid=true", result.stdout)
        self.assertIn("carrier_present=true carrier_count=1 carrier_type=string", result.stdout)
        self.assertIn("turn=2 status=200 failure=none", result.stdout)
        self.assertIn("terminal=true completed=true nonempty=true", result.stdout)
        self.assertFalse(any(value in result.stdout + result.stderr for value in CANARIES))

    def test_chunked_completed_turns_close_and_reopen_connection_twenty_times(self):
        server = ChunkedServer(("127.0.0.1", 0), ChunkedHandler)
        server.requests, server.client_ports, server.lock = [], [], threading.Lock()
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            for _ in range(20):
                result = subprocess.run([sys.executable, str(SCRIPT), str(server.server_address[1])],
                                        input="SYNTHETIC_KEY\n", text=True, capture_output=True, timeout=5)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("turn=1 status=200 failure=none", result.stdout)
                self.assertIn("text_deltas=1", result.stdout)
                self.assertIn("turn=2 status=200 failure=none", result.stdout)
                self.assertIn("dispatches=2 replay_accepted=true", result.stdout)
                self.assertFalse(any(secret in result.stdout + result.stderr for secret in CANARIES))
            self.assertEqual(len(server.requests), 40)
            for index in range(0, len(server.client_ports), 2):
                self.assertNotEqual(server.client_ports[index], server.client_ports[index + 1])
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=2)

    def test_first_turn_failures_never_dispatch_second_request(self):
        call = self.first_output()[1]
        duplicate = [call, dict(call, id="second-call", call_id="another-private-id")]
        unexpected = [dict(call, name="external-action")]
        external = [{"id": "external", "type": "computer_call"}, call]
        out_of_bounds = [dict(call, arguments='{"a":1001,"b":1}')]
        malformed_args = [dict(call, arguments='{"a":17,"b":}')]
        for label, output, wire in (
            ("missing_call", [], transcript([], text="private")),
            ("duplicate_calls", duplicate, transcript(duplicate)),
            ("unexpected_tool", unexpected, transcript(unexpected)),
            ("external_action", external, transcript(external)),
            ("bounds", out_of_bounds, transcript(out_of_bounds)),
            ("malformed_arguments", malformed_args, transcript(malformed_args)),
            ("missing_completion", [call], transcript([call], complete=False)),
            ("malformed_json", [call], b"event: response.completed\ndata: {private-json\n\n"),
            ("closed_stream", [call], b"event: response.output_text.delta\ndata: {\"delta\":\"private\"}\n\n"),
        ):
            with self.subTest(label=label):
                result, requests = self.run_fixture([wire])
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(len(requests), 1)
                failure = "invalid_arguments" if label in {"bounds", "malformed_arguments"} else "no_completed" if label in {"missing_completion", "closed_stream"} else "invalid_stream" if label == "malformed_json" else "invalid_tool"
                self.assertIn(f"turn=1 status=200 failure={failure}", result.stdout)
                self.assertNotIn("private", result.stdout + result.stderr)
                self.assertNotIn(CANARIES[2], result.stdout + result.stderr)

    def test_second_turn_http_statuses_and_postcommit_failure_are_sanitized(self):
        error_cases = (
            (400, {"error": {"code": "invalid_request", "message": CANARIES[0]}},
             "status=400 failure=http_error fixed_code=invalid_request"),
            (503, {"error": {"code": "upstream_error", "message": CANARIES[0]}},
             "status=503 failure=http_error fixed_code=upstream_error"),
        )
        for status, body, summary in error_cases:
            wire = json.dumps(body).encode()
            result, requests = self.run_fixture([transcript(self.first_output()), (status, "application/json", wire)])
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(len(requests), 2)
            self.assertIn("turn=1 status=200 failure=none", result.stdout)
            self.assertIn("turn=2 " + summary, result.stdout)
            self.assertNotIn(CANARIES[0], result.stdout + result.stderr)

        failed_stream = (event("response.output_item.done", {"item": {"type": "reasoning", "encrypted_content": CANARIES[1]}})
                         + event("response.output_text.delta", {"delta": CANARIES[0]})
                         + event("response.failed", {"error": {"code": "upstream_error", "message": CANARIES[1]}}))
        result, requests = self.run_fixture([transcript(self.first_output()), (200, "text/event-stream", failed_stream)])
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(requests), 2)
        self.assertIn("turn=2 status=200 failure=stream_failed fixed_code=upstream_error", result.stdout)
        self.assertIn("text_deltas=1", result.stdout)
        self.assertIn("terminal=true completed=false", result.stdout)
        self.assertIn("carrier_present=true carrier_count=1 carrier_type=string", result.stdout)
        self.assertNotIn(CANARIES[0], result.stdout + result.stderr)
        self.assertNotIn(CANARIES[1], result.stdout + result.stderr)

    def test_carrier_telemetry_reports_only_type_counts(self):
        output = self.first_output() + [
            {"type": "reasoning", "encrypted_content": None},
            {"type": "reasoning", "encrypted_content": {"opaque": CANARIES[0]}},
        ]
        result, requests = self.run_fixture([
            transcript(output), transcript([{"id": "answer", "type": "message"}], text="done")])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(requests), 2)
        self.assertIn("carrier_present=true carrier_count=3 carrier_type=mixed", result.stdout)
        self.assertIn("carrier_string_count=1 carrier_null_count=1 carrier_other_count=1", result.stdout)
        self.assertNotIn(CANARIES[0], result.stdout + result.stderr)

    def test_second_turn_failure_and_request_timeout_are_sanitized(self):
        call = self.first_output()
        result, requests = self.run_fixture([transcript(call), transcript([], complete=False)])
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(requests), 2)
        self.assertIn("turn=2 status=200 failure=no_completed", result.stdout)
        self.assertFalse(any(value in result.stdout + result.stderr for value in CANARIES))
        result, requests = self.run_fixture([
            transcript(self.first_output()), transcript([{"id": "answer", "type": "message"}])])
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(requests), 2)
        self.assertIn("turn=2 status=200 failure=nonempty_required", result.stdout)

    def test_timeout_closes_response_and_connection_without_private_output(self):
        import contextlib
        import io
        from unittest.mock import patch

        class Response:
            status = 200
            closed = False
            def __iter__(self):
                raise TimeoutError(CANARIES[0])
            def close(self):
                self.closed = True
                raise RuntimeError(CANARIES[0])

        class Connection:
            closed = False
            response = Response()
            def __init__(self, *_args, **_kwargs): pass
            def request(self, *_args, **_kwargs): pass
            def getresponse(self): return self.response
            def close(self):
                self.closed = True
                raise RuntimeError(CANARIES[1])

        connection = Connection()
        output = io.StringIO()
        with patch("http.client.HTTPConnection", return_value=connection), \
                patch.object(sys, "argv", [str(SCRIPT), "1234"]), \
                patch.object(sys, "stdin", io.StringIO("SYNTHETIC_KEY\n")), \
                contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
            import importlib.util
            spec = importlib.util.spec_from_file_location("antigravity_live_tool_smoke", SCRIPT)
            module = importlib.util.module_from_spec(spec)
            sys.path.insert(0, str(SCRIPT.parent))
            spec.loader.exec_module(module)
            self.assertEqual(module.main(), 1)
        self.assertTrue(connection.closed)
        self.assertTrue(connection.response.closed)
        self.assertIn("turn=1 status=200 failure=transport_error", output.getvalue())
        self.assertFalse(any(secret in output.getvalue() for secret in CANARIES))

    def test_response_and_connection_close_errors_do_not_override_completed_turns(self):
        import contextlib
        import io
        from unittest.mock import patch

        output_items = self.first_output()
        response_lines = [event("response.completed", {"response": {
            "status": "completed", "output": output_items}}).splitlines(keepends=True)]
        second_lines = [(event("response.output_text.delta", {"delta": "ok"})
                         + event("response.completed", {"response": {
                             "status": "completed", "output": [{"type": "message"}]}})).splitlines(keepends=True)]

        class Response:
            status = 200
            def __init__(self, lines): self.lines, self.closed = lines, False
            def __iter__(self): return iter(self.lines)
            def close(self):
                self.closed = True
                raise RuntimeError(CANARIES[0])

        class Connection:
            def __init__(self, lines): self.response, self.closed = Response(lines), False
            def request(self, *_args, **_kwargs): pass
            def getresponse(self): return self.response
            def close(self):
                self.closed = True
                raise RuntimeError(CANARIES[1])

        connections = [Connection(response_lines[0]), Connection(second_lines[0])]
        output = io.StringIO()
        with patch("http.client.HTTPConnection", side_effect=connections), \
                patch.object(sys, "argv", [str(SCRIPT), "1234"]), \
                patch.object(sys, "stdin", io.StringIO("SYNTHETIC_KEY\n")), \
                contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
            import importlib.util
            spec = importlib.util.spec_from_file_location("antigravity_live_tool_smoke", SCRIPT)
            module = importlib.util.module_from_spec(spec)
            sys.path.insert(0, str(SCRIPT.parent))
            spec.loader.exec_module(module)
            self.assertEqual(module.main(), 0)
        self.assertIn("dispatches=2 replay_accepted=true", output.getvalue())
        self.assertTrue(all(connection.closed and connection.response.closed for connection in connections))
        self.assertFalse(any(secret in output.getvalue() for secret in CANARIES))

        incomplete = Connection(event("response.output_text.delta", {"delta": "private"}).splitlines(keepends=True))
        output = io.StringIO()
        with patch("http.client.HTTPConnection", return_value=incomplete), \
                patch.object(sys, "argv", [str(SCRIPT), "1234"]), \
                patch.object(sys, "stdin", io.StringIO("SYNTHETIC_KEY\n")), \
                contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
            self.assertEqual(module.main(), 1)
        self.assertIn("turn=1 status=200 failure=no_completed", output.getvalue())
        self.assertTrue(incomplete.closed and incomplete.response.closed)
        self.assertNotIn("private", output.getvalue())

    def test_shell_lifecycle_cleans_key_gateway_and_temp_files_on_success_and_failure(self):
        cases = tuple((budget, outcome) for budget in (28, 29, 43, 998)
                      for outcome in ("success", "first_failure", "pre_dispatch_failure"))
        for budget_start, outcome in cases:
            with self.subTest(budget_start=budget_start, outcome=outcome), tempfile.TemporaryDirectory() as root:
                path = Path(root)
                (path / "operator").mkdir()
                (path / "operator/gateway.db").touch()
                (path / "operator/master.key").write_text("synthetic only")
                db = sqlite3.connect(path / "operator/gateway.db")
                db.execute("""CREATE TABLE operator_request_budgets (
                    account_id TEXT PRIMARY KEY, ceiling INTEGER NOT NULL CHECK (ceiling >= 0),
                    consumed INTEGER NOT NULL DEFAULT 0 CHECK (consumed >= 0),
                    created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
                    CHECK (consumed <= ceiling))""")
                db.executemany("INSERT INTO operator_request_budgets VALUES(?,?,?,?,?)", (
                    ("m10-antigravity-authorized-account", 1000, budget_start, 1, 1),
                    ("unrelated-account", 17, 11, 1, 1)))
                db.commit()
                budget_row = db.execute("SELECT ceiling,consumed FROM operator_request_budgets WHERE account_id=?",
                                        ("m10-antigravity-authorized-account",)).fetchone()
                self.assertIs(type(budget_row[0]), int)
                self.assertIs(type(budget_row[1]), int)
                self.assertEqual(budget_row, (1000, budget_start))
                db.close()
                fake = path / "gateway"
                fake.write_text(r'''#!/usr/bin/env python3
import http.server, json, os, re, sqlite3, sys
if sys.argv[1] == 'admin':
    if 'policy' in sys.argv:
        if 'get' in sys.argv: raise SystemExit(1)
    elif 'create' in sys.argv: print('secret=SYNTHETIC_KEY id=fixture-key')
    elif 'revoke' in sys.argv: open(os.environ['REVOKED'], 'w').write('yes')
    raise SystemExit(0)
config = open(sys.argv[sys.argv.index('--config')+1]).read()
port = int(re.search(r'127.0.0.1:(\d+)', config)[1])
open(os.environ['GATEWAY_PID_FILE'],'w').write(str(os.getpid()))
first = {"response":{"status":"completed","output":[{"id":"call","type":"function_call","call_id":"PRIVATE_CALL_ID_CANARY","name":"add","arguments":"{\"a\":17,\"b\":25}"}]}}
second = {"response":{"status":"completed","output":[{"id":"message","type":"message"}]}}
class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_GET(self): self.send_error(404)
    def do_POST(self):
        count=int(open(os.environ['DISPATCHES']).read())+1 if os.path.exists(os.environ['DISPATCHES']) else 1
        open(os.environ['DISPATCHES'],'w').write(str(count))
        self.rfile.read(int(self.headers['Content-Length']))
        print('level=DEBUG msg="Antigravity stream failure" phase=postcommit category=postcommit_error cause=eof private=PRIVATE_CALL_ID_CANARY', flush=True)
        print('level=DEBUG msg="Antigravity inference diagnostic" status=503 branch=http_error upstream_status=other transport_error_class=none private=PRIVATE_CALL_ID_CANARY', flush=True)
        if count == 1 and os.environ.get('OUTCOME') == 'pre_dispatch_failure':
            self.send_response(400); self.send_header('Content-Type','application/json'); self.end_headers()
            self.wfile.write(b'{"error":{"code":"invalid_request"}}'); return
        db=sqlite3.connect(os.environ['BUDGET_DB']); db.execute("UPDATE operator_request_budgets SET consumed=consumed+1 WHERE account_id='m10-antigravity-authorized-account'"); db.commit(); db.close()
        if count == 1 and os.environ.get('OUTCOME') == 'first_failure': wire=b'event: response.completed\ndata: {bad\n\n'
        elif count == 1: wire=b'event: response.completed\ndata: '+json.dumps(first).encode()+b'\n\n'
        else: wire=b'event: response.output_text.delta\ndata: {"delta":"ordinary"}\n\nevent: response.completed\ndata: '+json.dumps(second).encode()+b'\n\n'
        self.send_response(200); self.send_header('Content-Type','text/event-stream'); self.end_headers(); self.wfile.write(wire)
http.server.HTTPServer(('127.0.0.1',port),Handler).serve_forever()
''')
                fake.chmod(0o700)
                env = dict(os.environ, PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT=str(path / "operator"),
                           PESTIROUTE_ANTIGRAVITY_PROJECT="antigravity-local", PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT="1",
                           GATEWAY_BIN=str(fake), TMPDIR=str(path), BUDGET_DB=str(path / "operator/gateway.db"),
                           REVOKED=str(path / "revoked"), DISPATCHES=str(path / "dispatches"),
                           GATEWAY_PID_FILE=str(path / "gateway.pid"), PYTHONDONTWRITEBYTECODE="1")
                env["OUTCOME"] = outcome
                result = subprocess.run(["bash", str(Path(__file__).with_name("antigravity-live-tool-smoke.sh"))],
                                        env=env, capture_output=True, text=True, timeout=15)
                failed = outcome != "success"
                self.assertEqual(result.returncode == 0, not failed, result.stdout + result.stderr)
                self.assertTrue((path / "revoked").exists())
                expected_dispatches = "2" if outcome == "success" else "1"
                self.assertEqual((path / "dispatches").read_text(), expected_dispatches)
                check = sqlite3.connect(path / "operator/gateway.db")
                target = check.execute("SELECT ceiling,consumed FROM operator_request_budgets WHERE account_id=?",
                                       ("m10-antigravity-authorized-account",)).fetchone()
                unrelated = check.execute("SELECT ceiling,consumed FROM operator_request_budgets WHERE account_id=?",
                                          ("unrelated-account",)).fetchone()
                check.close()
                self.assertEqual(target, (1000, budget_start + {"success": 2, "first_failure": 1,
                                                                "pre_dispatch_failure": 0}[outcome]))
                self.assertEqual(unrelated, (17, 11))
                if outcome == "success":
                    self.assertIn(f"budget_before={budget_start} budget_after={budget_start + 2} budget_ceiling=1000", result.stdout)
                elif outcome == "pre_dispatch_failure":
                    self.assertIn(f"budget_before={budget_start} budget_after={budget_start}", result.stdout)
                    self.assertNotIn("request budget ceiling check failed", result.stdout + result.stderr)
                    self.assertIn("runner=failed dispatches_bounded=true", result.stdout)
                self.assertIn("ephemeral_key_revoked=true", result.stdout)
                self.assertIn("gateway_stopped=true", result.stdout)
                self.assertIn("temporary_runner_files_removed=true", result.stdout)
                if failed:
                    self.assertIn("gateway_stream phase=postcommit category=postcommit_error cause=eof", result.stdout)
                    self.assertIn("gateway_inference status=503 branch=http_error upstream_status=other transport_error_class=none", result.stdout)
                self.assertFalse(any(secret in result.stdout + result.stderr for secret in (*CANARIES, "SYNTHETIC_KEY")))
                with self.assertRaises(ProcessLookupError):
                    os.kill(int((path / "gateway.pid").read_text()), 0)
                remaining = [item.name for item in path.iterdir() if item.name not in {"operator", "gateway", "revoked", "dispatches", "gateway.pid"}]
                self.assertEqual(remaining, [])


if __name__ == "__main__":
    unittest.main()
