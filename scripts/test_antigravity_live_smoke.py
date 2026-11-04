"""Offline full-runner fixture; no operator store or provider is accessed."""
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import time
import unittest

CANARY = "CARRIER_SECRET_CANARY_never_record"
ANSWER = 'event: response.output_text.delta\ndata: {"delta":"PRIVATE"}\n\n'
COMPLETE = 'event: response.completed\ndata: {}\n\n'

def carrier(value):
    import json
    return 'event: response.output_item.done\ndata: ' + json.dumps({"item": {"type": "reasoning", "encrypted_content": value}}) + '\n\n'


class RunnerTest(unittest.TestCase):
    def test_usage_diagnostic_extraction_is_enum_only(self):
        from antigravity_smoke_response import stream_diagnostics

        prefix = 'msg="Antigravity stream failure" phase=precommit category=precommit_invalid_usage '
        extended_fields = (
            "prompt_token_count", "candidates_token_count", "total_token_count",
            "candidates_tokens_details", "cache_tokens_details",
            "tool_use_prompt_tokens_details", "prompt_tokens_details",
            "thoughts_token_count", "cached_content_token_count",
            "tool_use_prompt_token_count", "service_tier", "other",
        )
        for field in extended_fields:
            with self.subTest(field=field):
                self.assertEqual(
                    stream_diagnostics(prefix + f'subkind=unknown_field known_field={field} raw=PRIVATE-CANARY tokens=907'),
                    [f"phase=precommit category=precommit_invalid_usage subkind=unknown_field known_field={field}"],
                )
        self.assertEqual(
            stream_diagnostics(prefix + 'subkind=invalid_type raw=PRIVATE-CANARY'),
            ["phase=precommit category=precommit_invalid_usage subkind=invalid_type"],
        )
        for malformed in (
            prefix + 'subkind=unknown_field known_field=PRIVATE-CANARY',
            prefix + 'subkind=PRIVATE-CANARY',
            prefix + 'subkind=invalid_type known_field=other',
            prefix + 'subkind=unknown_field known_field=other known_field=other',
        ):
            with self.subTest(malformed=malformed):
                self.assertEqual(stream_diagnostics(malformed), [])

    def test_production_error_shapes_and_corrected_request(self):
        for tc in (
            (400, "application/json", '{"error":{"message":"PRIVATE","type":"unsupported_feature","code":"unsupported_feature"}}', "fixed_code=unsupported_feature", 1),
            (503, "application/json", '{"error":{"code":"missing_or_unlisted_code"}}', "reason=missing_or_unlisted_code", 1, "gemini-3.8-flash-medium"),
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
            (200, "text/event-stream", ANSWER + COMPLETE, "nonempty=true terminal=true completed=true", 0, "gemini-3.8-flash-medium"),
            (200, "text/event-stream", ANSWER + COMPLETE, "nonempty=true terminal=true completed=true", 0, "gemini-pro-agent"),
            (503, "application/json", '{"error":{"code":"missing_or_unlisted_code"}}', "fixed_code=none reason=missing_or_unlisted_code", 1, "gemini-pro-agent"),
            (-1, "text/plain", "", "unexpected M10-051 budget baseline or refresh attestation", 1, "gemini-pro-agent", 44),
            (-1, "text/plain", "", "unexpected M10-051 budget baseline or refresh attestation", 1, "gemini-pro-agent", 47),
            (-1, "text/plain", "", "unexpected M10-051 budget baseline or refresh attestation", 1, "gemini-pro-agent", 46),
            (200, "text/event-stream", ANSWER + COMPLETE, "nonempty=true terminal=true completed=true", 0, "gemini-pro-agent", 46, "true"),
            (200, "text/event-stream", ANSWER + COMPLETE, "nonempty=true terminal=true completed=true", 0, "claude-sonnet-4-6", 47),
            (200, "text/event-stream", ANSWER + COMPLETE, "nonempty=true terminal=true completed=true", 0, "claude-opus-4-6-thinking", 48),
            (503, "application/json", '{"error":{"code":"missing_or_unlisted_code"}}', "status=503", 1, "gpt-oss-120b-medium", 49),
            (0, "text/plain", "", "model=claude-sonnet-4-6 runner_exit=1 budget_before=47 budget_after=48 dispatches=1", 1, "claude-sonnet-4-6", 47, None, True),
            (0, "text/plain", "", "model=gpt-oss-120b-medium runner_exit=1 budget_before=49 budget_after=50 dispatches=1", 1, "claude-sonnet-4-6", 47, None, True, "all-negative"),
            (-2, "text/plain", "", "insufficient authorized ceiling", 2, "claude-sonnet-4-6", 47, None, True, "cap"),
            (0, "text/plain", "", "post-model budget invariant failed model=claude-sonnet-4-6", 2, "claude-sonnet-4-6", 47, None, True, "delta2"),
        ):
            model = tc[5] if len(tc) > 5 else "gemini-2.5-flash"
            status, media, body, expected, returncode = tc[:5]
            initial_consumed = tc[6] if len(tc) > 6 else 42 if model == "gemini-3.8-flash-medium" else 45 if model == "gemini-pro-agent" else 8
            refresh_attestation = tc[7] if len(tc) > 7 else None
            batch = tc[8] if len(tc) > 8 else False
            cap_rejected = len(tc) > 9 and tc[9] == "cap"
            delta_overrun = len(tc) > 9 and tc[9] == "delta2"
            baseline_rejected = status == -1 or cap_rejected
            with self.subTest(media=media), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                (root / "runner-temp").mkdir()
                (root / "master.key").write_text("synthetic fixture only")
                db = sqlite3.connect(root / "gateway.db")
                db.execute("CREATE TABLE operator_request_budgets (account_id TEXT, ceiling INT, consumed INT)")
                db.execute("INSERT INTO operator_request_budgets VALUES ('m10-antigravity-authorized-account',?,?)", (50 if cap_rejected else 1000, initial_consumed))
                db.commit()
                db.close()
                gateway = root / "gateway"
                gateway.write_text(r'''#!/usr/bin/env python3
import http.server, json, os, re, sqlite3, sys
if sys.argv[1] == 'admin':
    args = sys.argv
    if 'account' in args: print('id=antigravity-local connector=antigravity enabled=true fixture')
    elif 'policy' in args:
        model = os.environ['FIXTURE_MODEL']
        models = '["gemini-3.8-flash-medium"]' if model == 'gemini-3.8-flash-medium' else '["gemini-pro-agent"]' if model == 'gemini-pro-agent' else '["claude-sonnet-4-6","claude-opus-4-6-thinking","gpt-oss-120b-medium"]' if model in ('claude-sonnet-4-6','claude-opus-4-6-thinking','gpt-oss-120b-medium') else '["gemini-2.5-pro","gemini-2.5-flash"]'
        if 'get' in args and 'policy' in args:
            print(f'id=m10-053-live-smoke revision=1 enabled=true models={models} connectors=["antigravity"] rpm=10 tpm=10000')
        elif 'update' in args and 'policy' in args:
            pass
        else:
            print(f'enabled=true revision=1 models={models} connectors=["antigravity"] rpm=10 tpm=10000')
    elif 'create' in args: print('secret=SYNTHETIC_TEST_KEY id=fixture-key')
    elif 'revoke' in args: pass
    else: sys.exit(2)
    sys.exit(0)
config = open(sys.argv[sys.argv.index('--config')+1]).read()
model = re.findall(r'model: "([^"]+)"', config)[0]
port = int(re.search(r'127.0.0.1:(\d+)', config)[1])
assert model in config
assert config.count(f'model: "{model}"') == 2
class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'
    def log_message(self, *args): pass
    def do_GET(self): self.send_error(404)
    def do_POST(self):
        with open(os.environ['FIXTURE_POSTS'], 'a') as f: f.write('post\n')
        if model in ('gemini-3.8-flash-medium', 'gemini-pro-agent', 'claude-sonnet-4-6', 'claude-opus-4-6-thinking', 'gpt-oss-120b-medium'):
            db = sqlite3.connect(os.path.join(os.environ['PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT'], 'gateway.db'))
            db.execute("UPDATE operator_request_budgets SET consumed=consumed+? WHERE account_id='m10-antigravity-authorized-account'", (int(os.environ.get('FIXTURE_BUDGET_DELTA', '1')),))
            db.commit()
            db.close()
        obj = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        assert set(obj) == {'model','stream','input','include'}
        assert obj['include'] == ['reasoning.encrypted_content']
        assert obj['model'] == model and obj['stream'] is True
        assert obj['input'] == 'Write one short sentence about a sailboat crossing a calm blue lake.'
        status = json.loads(os.environ.get('FIXTURE_STATUSES', '{}')).get(model, os.environ['FIXTURE_STATUS'])
        if model == 'gemini-3.8-flash-medium' and status != '200':
            print('level=DEBUG msg="Antigravity stream failure" phase=precommit category=precommit_invalid_usage subkind=unknown_field known_field=service_tier tokens=907 raw_field=PRIVATE-CANARY private=PRIVATE', flush=True)
        else:
            print('level=DEBUG msg="Antigravity stream failure" phase=precommit category=precommit_unsupported_reasoning kind=signature_only signature_type=string private=PRIVATE', flush=True)
        print('level=DEBUG msg="Antigravity inference diagnostic" status=503 branch=http_error upstream_status=other transport_error_class=none private=PRIVATE', flush=True)
        data = (os.environ['FIXTURE_BODY_SUCCESS'] if status == '200' and model == 'claude-opus-4-6-thinking' else os.environ['FIXTURE_BODY']).encode()
        with open(os.environ['FIXTURE_MODEL_POSTS'], 'a') as f: f.write(model + '\n')
        if status == '0':
            self.connection.sendall(data + b'\r\n')
            return
        self.send_response(int(status))
        self.send_header('Content-Type', os.environ['FIXTURE_MEDIA'])
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)
http.server.HTTPServer(('127.0.0.1',port),Handler).serve_forever()
''')
                gateway.chmod(0o700)
                env = dict(os.environ, PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT=tmp,
                           PESTIROUTE_ANTIGRAVITY_PROJECT="antigravity-local",
                           PESTIROUTE_ANTIGRAVITY_MODEL=model, FIXTURE_MODEL=model,
                              GATEWAY_BIN=str(gateway), FIXTURE_BODY=body, FIXTURE_MEDIA=media, FIXTURE_STATUS=str(status))
                env.pop("PESTIROUTE_ANTIGRAVITY_NATURAL_REFRESH_PERFORMED", None)
                if refresh_attestation is not None:
                    env["PESTIROUTE_ANTIGRAVITY_NATURAL_REFRESH_PERFORMED"] = refresh_attestation
                env.update(TMPDIR=str(root / "runner-temp"), FIXTURE_POSTS=str(root / "posts"), FIXTURE_MODEL_POSTS=str(root / "model-posts"), FIXTURE_BODY_SUCCESS=ANSWER + COMPLETE, PYTHONDONTWRITEBYTECODE="1")
                if model in ("claude-sonnet-4-6", "claude-opus-4-6-thinking", "gpt-oss-120b-medium"):
                    env["PESTIROUTE_ANTIGRAVITY_M053_EXPECTED_START"] = str(initial_consumed)
                if batch:
                    fixture_statuses = '{"claude-sonnet-4-6":"503","claude-opus-4-6-thinking":"200","gpt-oss-120b-medium":"503"}' if len(tc) < 10 else '{"claude-sonnet-4-6":"503","claude-opus-4-6-thinking":"503","gpt-oss-120b-medium":"503"}'
                    if len(tc) > 9 and tc[9] == "delta2":
                        env["FIXTURE_BUDGET_DELTA"] = "2"
                    env.update(
                        PESTIROUTE_ANTIGRAVITY_M053_BATCH="true",
                        PESTIROUTE_ANTIGRAVITY_M053_BASELINE="47",
                        PESTIROUTE_ANTIGRAVITY_M053_RUNWAY_SECONDS="420",
                        PESTIROUTE_ANTIGRAVITY_M053_PREFLIGHT_UNIX=str(int(time.time())),
                        FIXTURE_STATUSES=fixture_statuses,
                        FIXTURE_BODY_SUCCESS=ANSWER + COMPLETE,
                    )
                result = subprocess.run(["bash", str(Path(__file__).with_name("antigravity-live-smoke.sh"))], env=env, capture_output=True, text=True, timeout=20)
                self.assertEqual(result.returncode, returncode)
                self.assertIn(expected, result.stdout + result.stderr)
                if not cap_rejected:
                    self.assertIn("ephemeral_key_revoked=true", result.stdout)
                else:
                    self.assertNotIn("post\n", result.stdout)
                if baseline_rejected:
                    self.assertNotIn("gateway_stopped=true", result.stdout)
                elif model == "gemini-3.8-flash-medium" and status != 200:
                    self.assertIn("gateway stream phase=precommit category=precommit_invalid_usage subkind=unknown_field known_field=service_tier", result.stdout)
                    self.assertNotIn("raw_field", result.stdout)
                else:
                    self.assertIn("gateway stream phase=precommit category=precommit_unsupported_reasoning kind=signature_only signature_type=string", result.stdout)
                if not baseline_rejected:
                    self.assertIn("gateway inference status=503 branch=http_error upstream_status=other transport_error_class=none", result.stdout)
                self.assertNotIn("PRIVATE", result.stdout + result.stderr)
                self.assertNotIn("SYNTHETIC_TEST_KEY", result.stdout + result.stderr)
                self.assertNotIn(CANARY, result.stdout + result.stderr)
                self.assertNotIn("Traceback", result.stdout + result.stderr)
                expected_posts = 0 if baseline_rejected else 1 if batch and delta_overrun else 3 if batch else 2 if model == "gemini-3.8-flash-medium" and status == 200 else 1
                if expected_posts:
                    self.assertEqual((root / "posts").read_text(), "post\n" * expected_posts)
                else:
                    self.assertFalse((root / "posts").exists())
                db = sqlite3.connect(root / "gateway.db")
                consumed = db.execute("SELECT consumed FROM operator_request_budgets").fetchone()[0]
                db.close()
                expected_delta = 2 if batch and delta_overrun else 1
                expected_consumed = initial_consumed + (expected_posts * expected_delta if model in ("gemini-3.8-flash-medium", "gemini-pro-agent", "claude-sonnet-4-6", "claude-opus-4-6-thinking", "gpt-oss-120b-medium") else 0)
                self.assertEqual(consumed, expected_consumed)
                if batch and not cap_rejected:
                    if delta_overrun:
                        self.assertEqual((root / "model-posts").read_text().splitlines(), ["claude-sonnet-4-6"])
                        self.assertEqual(consumed, 49)
                    else:
                        self.assertEqual((root / "model-posts").read_text().splitlines(), ["claude-sonnet-4-6", "claude-opus-4-6-thinking", "gpt-oss-120b-medium"])
                        self.assertEqual(consumed, 50)
                    if not delta_overrun and len(tc) < 10:
                        self.assertIn("model=claude-opus-4-6-thinking runner_exit=0 budget_before=48 budget_after=49 dispatches=1", result.stdout)
                    elif not delta_overrun:
                        self.assertIn("model=claude-opus-4-6-thinking runner_exit=1 budget_before=48 budget_after=49 dispatches=1", result.stdout)
                    if not delta_overrun:
                        self.assertIn("model=gpt-oss-120b-medium runner_exit=1 budget_before=49 budget_after=50 dispatches=1", result.stdout)
                    self.assertIn("m10_053_policy_disabled=true", result.stdout)
                self.assertEqual(list((root / "runner-temp").iterdir()), [])
                self.assertNotIn("bytes=", result.stdout)
                if not baseline_rejected:
                    self.assertIn("gateway_stopped=true", result.stdout)
                if not cap_rejected:
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
        for phase in ("request", "getresponse", "iterate", "read", "timeout", "close"):
            class Response:
                status = 400 if phase == "read" else 200
                def readline(self):
                    if phase == "close": return b""
                    raise RuntimeError(CANARY)
                def read(self, *args):
                    raise RuntimeError(CANARY)
            class Socket:
                def settimeout(self, _timeout):
                    if phase == "timeout": raise RuntimeError(CANARY)
            class Connection:
                requests = 0
                closed = False
                sock = Socket()
                def connect(self): pass
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
            self.assertEqual(conn.requests, 0 if phase == "timeout" else 1)
            self.assertTrue(conn.closed)

    def test_m053_freshness_and_refresh_attestation_gates(self):
        runner = Path(__file__).with_name("antigravity-live-smoke.sh")
        for name, baseline, consumed, runway, age, refresh, accepted in (
            ("low runway", "47", 47, "419", 0, "false", False),
            ("stale preflight", "47", 47, "420", 61, "false", False),
            ("48 without refresh attestation", "48", 48, "420", 0, "false", False),
            ("refresh attested at wrong baseline", "47", 47, "420", 0, "true", False),
            ("48 with refresh attestation", "48", 48, "420", 0, "true", True),
        ):
            with self.subTest(name=name), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                temp = root / "temp"
                temp.mkdir()
                (root / "master.key").write_text("fixture only")
                db = sqlite3.connect(root / "gateway.db")
                db.execute("CREATE TABLE operator_request_budgets (account_id TEXT, ceiling INT, consumed INT)")
                db.execute("INSERT INTO operator_request_budgets VALUES ('m10-antigravity-authorized-account',1000,?)", (consumed,))
                db.commit()
                db.close()
                calls = root / "admin-calls"
                gateway = root / "gateway"
                gateway.write_text("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FIXTURE_ADMIN_CALLS\"\nexit 1\n")
                gateway.chmod(0o700)
                env = dict(os.environ,
                    PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT=str(root),
                    PESTIROUTE_ANTIGRAVITY_PROJECT="antigravity-local",
                    PESTIROUTE_ANTIGRAVITY_M053_BATCH="true",
                    PESTIROUTE_ANTIGRAVITY_M053_BASELINE=baseline,
                    PESTIROUTE_ANTIGRAVITY_M053_RUNWAY_SECONDS=runway,
                    PESTIROUTE_ANTIGRAVITY_M053_PREFLIGHT_UNIX=str(int(time.time()) - age),
                    PESTIROUTE_ANTIGRAVITY_NATURAL_REFRESH_PERFORMED=refresh,
                    GATEWAY_BIN=str(gateway), FIXTURE_ADMIN_CALLS=str(calls),
                    TMPDIR=str(temp), PYTHONDONTWRITEBYTECODE="1")
                result = subprocess.run(["bash", str(runner)], env=env, capture_output=True, text=True, timeout=10)
                if not accepted:
                    self.assertEqual(result.returncode, 2)
                    self.assertFalse(calls.exists())
                else:
                    self.assertNotIn("invalid M10-053 budget baseline", result.stdout + result.stderr)
                    self.assertEqual(result.returncode, 1)  # Stub rejects account metadata before gateway startup.
                    call_text = calls.read_text()
                    self.assertEqual(call_text.count("account get antigravity-local"), 3)
                    self.assertNotIn("/v1/responses", call_text)

    def test_cancellation_client_aborts_after_first_text_delta(self):
        import contextlib
        import http.client
        import io
        import sys
        from unittest.mock import patch
        runner = Path(__file__).with_name("antigravity-live-smoke.sh").read_text()
        self.assertIn("gemini-3.8-flash-medium) POLICY_ID=m10-046-live-smoke", runner)
        self.assertIn("REQUESTS=2; EXPECTED_DELTA=0; MAX_CONSUMED=45", runner)
        self.assertIn("$budget_start != 42 && $budget_start != 43", runner)
        self.assertIn("REQUEST_DEADLINE_SECONDS = 90", runner)
        program = runner.split('cat >"$WORK/request.py" <<\'PY\'\n', 1)[1].split('\nPY\n', 1)[0]
        class Response:
            status = 200
            closed = False
            lines = [b'event: response.output_text.delta\n', b'data: {"delta":"synthetic"}\n', b'\n', b'event: response.completed\n', b'data: {}\n\n']
            def readline(self): return self.lines.pop(0) if self.lines else b""
            def close(self): self.closed = True
        class Socket:
            def settimeout(self, _timeout): pass
        response = Response()
        class Connection:
            closed = False
            sock = Socket()
            def connect(self): pass
            def request(self, *args): pass
            def getresponse(self): return response
            def close(self): self.closed = True
        conn = Connection()
        output = io.StringIO()
        with patch.object(http.client, "HTTPConnection", return_value=conn), patch.object(sys, "argv", ["request", "1", "gemini-3.8-flash-medium", "cancel"]), patch.object(sys, "stdin", io.StringIO("SYNTHETIC_TEST_KEY\n")), contextlib.redirect_stdout(output):
            with self.assertRaises(SystemExit) as exit:
                exec(compile(program, "<offline-cancellation-runner>", "exec"), {})
        self.assertEqual(exit.exception.code, 0)
        self.assertIn("client_abort=true first_text_delta=true terminal=false", output.getvalue())
        self.assertTrue(response.closed)
        self.assertTrue(conn.closed)
        self.assertNotIn("SYNTHETIC_TEST_KEY", output.getvalue())

if __name__ == "__main__":
    unittest.main()
