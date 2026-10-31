"""Offline full-runner fixture; no operator store or provider is accessed."""
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import unittest

CANARY = "CARRIER_SECRET_CANARY_never_record"
ANSWER = 'event: response.output_text.delta\ndata: {"delta":"PRIVATE"}\n\n'
COMPLETE = 'event: response.completed\ndata: {}\n\n'

def carrier(value):
    import json
    return 'event: response.output_item.done\ndata: ' + json.dumps({"item": {"type": "reasoning", "encrypted_content": value}}) + '\n\n'


class RunnerTest(unittest.TestCase):
    def test_production_error_shapes_and_corrected_request(self):
        for status, media, body, expected, returncode in (
            (400, "application/json", '{"error":{"message":"PRIVATE","type":"unsupported_feature","code":"unsupported_feature"}}', "fixed_code=unsupported_feature", 1),
            (400, "text/event-stream", 'event: response.failed\ndata: {"response":{"error":{"code":"unsupported_feature","message":"PRIVATE"}}}\n\n', "fixed_code=unsupported_feature", 1),
            (400, "text/plain", "PRIVATE", "reason=non_json_or_malformed", 1),
            (200, "text/event-stream", 'event: response.reasoning_summary_text.delta\ndata: {"delta":"PRIVATE"}\n\nevent: response.completed\ndata: {}\n\n', "nonempty=false terminal=true completed=true", 1),
            (200, "text/event-stream", 'event: response.output_text.delta\ndata: {"delta":"PRIVATE"}\n\nevent: response.completed\ndata: {}\n\n', "nonempty=true terminal=true completed=true", 0),
            (200, "text/event-stream", 'event: response.output_text.delta\ndata: {"delta":"PRIVATE"}\n\n', "nonempty=true terminal=false completed=false", 1),
            (200, "text/event-stream", ANSWER + carrier(CANARY) + COMPLETE, "reasoning_encrypted_content_count=1 has_reasoning_encrypted_content=true reasoning_encrypted_content_all_strings=true", 0),
            (200, "text/event-stream", ANSWER + carrier(CANARY) + 'event: response.completed\ndata: {"response":{"output":[{"type":"reasoning","encrypted_content":"' + CANARY + '"}]}}\n\n', "reasoning_encrypted_content_count=1 has_reasoning_encrypted_content=true", 0),
            (200, "text/event-stream", carrier(CANARY) + COMPLETE, "nonempty=false terminal=true completed=true", 1),
            (200, "text/event-stream", ANSWER + carrier(CANARY), "nonempty=true terminal=false completed=false", 1),
            (200, "text/event-stream", ANSWER + carrier(CANARY) + 'event: response.failed\ndata: {"error":"' + CANARY + '"}\n\n', "nonempty=true terminal=true completed=false", 1),
            (200, "text/event-stream", ANSWER + carrier(CANARY) + 'event: response.incomplete\ndata: {"error":"' + CANARY + '"}\n\n', "nonempty=true terminal=true completed=false", 1),
            (200, "text/event-stream", ANSWER + carrier(CANARY) + carrier(None) + COMPLETE, "reasoning_encrypted_content_count=2 has_reasoning_encrypted_content=true reasoning_encrypted_content_all_strings=false", 0),
            (200, "text/event-stream", ANSWER + 'event: response.output_item.done\ndata: {"item":' + CANARY + '\n\n' + COMPLETE, "reasoning_encrypted_content_count=0 has_reasoning_encrypted_content=false", 0),
            (400, "application/json", '{"error":{"message":"' + CANARY + '","code":"invalid_request"}}', "fixed_code=invalid_request", 1),
            (400, "text/plain", CANARY, "reason=non_json_or_malformed", 1),
            (0, "text/plain", CANARY, "reason=transport_or_parse_failed", 1),
        ):
            with self.subTest(media=media), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                (root / "runner-temp").mkdir()
                (root / "master.key").write_text("synthetic fixture only")
                db = sqlite3.connect(root / "gateway.db")
                db.execute("CREATE TABLE operator_request_budgets (account_id TEXT, ceiling INT, consumed INT)")
                db.execute("INSERT INTO operator_request_budgets VALUES ('m10-antigravity-authorized-account',1000,8)")
                db.commit()
                db.close()
                gateway = root / "gateway"
                gateway.write_text(r'''#!/usr/bin/env python3
import http.server, json, os, re, sys
if sys.argv[1] == 'admin':
    args = sys.argv
    if 'account' in args: print('id=antigravity-local connector=antigravity enabled=true fixture')
    elif 'policy' in args: print('enabled=true models=["gemini-2.5-pro","gemini-2.5-flash"] connectors=["antigravity"] rpm=10 tpm=10000')
    elif 'create' in args: print('secret=SYNTHETIC_TEST_KEY id=fixture-key')
    elif 'revoke' in args: pass
    else: sys.exit(2)
    sys.exit(0)
config = open(sys.argv[sys.argv.index('--config')+1]).read()
port = int(re.search(r'127.0.0.1:(\d+)', config)[1])
assert 'model: "gemini-2.5-flash"' in config
assert config.count('model: "gemini-2.5-flash"') == 2
class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_GET(self): self.send_error(404)
    def do_POST(self):
        with open(os.environ['FIXTURE_POSTS'], 'a') as f: f.write('post\n')
        obj = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        assert set(obj) == {'model','stream','input','include'}
        assert obj['include'] == ['reasoning.encrypted_content']
        assert obj['model'] == 'gemini-2.5-flash' and obj['stream'] is True
        assert obj['input'] == 'Write one short sentence about a sailboat crossing a calm blue lake.'
        print('level=DEBUG msg="Antigravity stream failure" phase=precommit category=precommit_unsupported_reasoning kind=signature_only signature_type=string private=PRIVATE', flush=True)
        print('level=DEBUG msg="Antigravity inference diagnostic" status=503 branch=http_error upstream_status=other transport_error_class=none private=PRIVATE', flush=True)
        data = os.environ['FIXTURE_BODY'].encode()
        if os.environ['FIXTURE_STATUS'] == '0':
            self.connection.sendall(data + b'\r\n')
            return
        self.send_response(int(os.environ['FIXTURE_STATUS']))
        self.send_header('Content-Type', os.environ['FIXTURE_MEDIA'])
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)
http.server.HTTPServer(('127.0.0.1',port),Handler).serve_forever()
''')
                gateway.chmod(0o700)
                env = dict(os.environ, PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT=tmp,
                           PESTIROUTE_ANTIGRAVITY_PROJECT="antigravity-local",
                           PESTIROUTE_ANTIGRAVITY_MODEL="gemini-2.5-flash",
                             GATEWAY_BIN=str(gateway), FIXTURE_BODY=body, FIXTURE_MEDIA=media, FIXTURE_STATUS=str(status))
                env.update(TMPDIR=str(root / "runner-temp"), FIXTURE_POSTS=str(root / "posts"), PYTHONDONTWRITEBYTECODE="1")
                result = subprocess.run(["bash", str(Path(__file__).with_name("antigravity-live-smoke.sh"))], env=env, capture_output=True, text=True, timeout=20)
                self.assertEqual(result.returncode, returncode)
                self.assertIn(expected, result.stdout)
                self.assertIn("ephemeral_key_revoked=true", result.stdout)
                self.assertIn("gateway stream phase=precommit category=precommit_unsupported_reasoning kind=signature_only signature_type=string", result.stdout)
                self.assertIn("gateway inference status=503 branch=http_error upstream_status=other transport_error_class=none", result.stdout)
                self.assertNotIn("PRIVATE", result.stdout + result.stderr)
                self.assertNotIn("SYNTHETIC_TEST_KEY", result.stdout + result.stderr)
                self.assertNotIn(CANARY, result.stdout + result.stderr)
                self.assertNotIn("Traceback", result.stdout + result.stderr)
                self.assertEqual((root / "posts").read_text(), "post\n")
                self.assertEqual(list((root / "runner-temp").iterdir()), [])
                self.assertNotIn("bytes=", result.stdout)
                self.assertIn("gateway_stopped=true", result.stdout)
                self.assertIn("temporary_runner_files_removed=true", result.stdout)

    def test_request_exception_paths_never_emit_private_exception(self):
        # Inject failures at each I/O boundary, including close, without any network.
        import contextlib
        import http.client
        import io
        import sys
        from unittest.mock import patch
        runner = Path(__file__).with_name("antigravity-live-smoke.sh").read_text()
        program = runner.split('cat >"$WORK/request.py" <<\'PY\'\n', 1)[1].split('\nPY\n', 1)[0]
        for phase in ("request", "getresponse", "iterate", "read", "close"):
            class Response:
                status = 400 if phase == "read" else 200
                def __iter__(self):
                    if phase == "close": return iter(())
                    raise RuntimeError(CANARY)
                def read(self, *args):
                    raise RuntimeError(CANARY)
            class Connection:
                requests = 0
                closed = False
                def request(self, *args):
                    self.requests += 1
                    if phase == "request": raise RuntimeError(CANARY)
                def getresponse(self):
                    if phase == "getresponse": raise RuntimeError(CANARY)
                    return Response()
                def close(self):
                    self.closed = True
                    if phase == "close": raise RuntimeError(CANARY)
            conn = Connection()
            output = io.StringIO()
            with self.subTest(phase=phase), patch.object(http.client, "HTTPConnection", return_value=conn), patch.object(sys, "argv", ["request", "1", "gemini-2.5-flash"]), patch.object(sys, "stdin", io.StringIO("SYNTHETIC_TEST_KEY\n")), contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
                with self.assertRaises(SystemExit) as exit:
                    exec(compile(program, "<offline-runner>", "exec"), {})
                self.assertEqual(exit.exception.code, 1)
            self.assertIn("request reason=transport_or_parse_failed\n", output.getvalue())
            self.assertNotIn(CANARY, output.getvalue())
            self.assertNotIn("Traceback", output.getvalue())
            self.assertEqual(conn.requests, 1)
            self.assertTrue(conn.closed)


if __name__ == "__main__":
    unittest.main()
