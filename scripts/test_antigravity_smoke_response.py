import json
import unittest

from antigravity_smoke_response import ALLOWED_CODES, STREAM_FAILURES, diagnostic, inference_diagnostics, stream_diagnostics


class DiagnosticsTest(unittest.TestCase):
    def test_stream_schema_and_inference_preserved(self):
        for phase, category in STREAM_FAILURES:
            for quoted in (False, True):
                p, c = (f'"{phase}"', f'"{category}"') if quoted else (phase, category)
                line = f'level=DEBUG msg="Antigravity stream failure" phase={p} category={c} private=PRIVATE'
                if category == "precommit_unsupported_reasoning":
                    self.assertEqual(stream_diagnostics(line), [])
                    detail = line + ' kind=both signature_type=other'
                    self.assertEqual(stream_diagnostics(detail), [
                        f"phase={phase} category={category} kind=both signature_type=other"
                    ])
                    for invalid in ('kind=unknown signature_type=other', 'kind=both signature_type=private', 'kind=both'):
                        self.assertEqual(stream_diagnostics(line + ' ' + invalid), [])
                elif category == "postcommit_error":
                    self.assertEqual(stream_diagnostics(line), [])
                    detail = line + ' cause=framing'
                    self.assertEqual(stream_diagnostics(detail), [
                        f"phase={phase} category={category} cause=framing"
                    ])
                    self.assertEqual(stream_diagnostics(detail + ' cause=eof'), [])
                    self.assertEqual(stream_diagnostics(line + ' cause=private'), [])
                elif category == "precommit_invalid_usage":
                    self.assertEqual(stream_diagnostics(line), [])
                    detail = line + ' subkind=unknown_field known_field=other'
                    self.assertEqual(stream_diagnostics(detail), [
                        f"phase={phase} category={category} subkind=unknown_field known_field=other"
                    ])
                else:
                    self.assertEqual(stream_diagnostics(line), [f"phase={phase} category={category}"])
        for phase, category in (("PRIVATE", "precommit_eof"), ("precommit", "PRIVATE"), ("postcommit", "precommit_eof"), ("precommit", "postcommit_error"), ("precommit", "precommit_eofPRIVATE")):
            self.assertEqual(stream_diagnostics(f'msg="Antigravity stream failure" phase={phase} category={category}'), [])
        self.assertEqual(stream_diagnostics('msg="PRIVATE" phase=precommit category=precommit_eof'), [])
        inference = 'msg="Antigravity inference diagnostic" status=503 branch=http_error upstream_status=other transport_error_class=none private=PRIVATE'
        self.assertEqual(inference_diagnostics(inference), ["status=503 branch=http_error upstream_status=other transport_error_class=none"])
        self.assertEqual(stream_diagnostics(inference), [])

    def test_all_fixed_codes_and_wire_shapes(self):
        for code in ALLOWED_CODES:
            error = {"message": "PRIVATE_DO_NOT_EMIT", "type": "unsupported_feature", "code": code}
            for obj in ({"error": error}, error, {"response": {"error": error}}):
                data = json.dumps(obj).encode()
                for body, media in ((data, "application/json"), (b"event: response.failed\r\ndata: " + data + b"\r\n\r\n", "text/event-stream; charset=utf-8")):
                    result = diagnostic(body, media)
                    self.assertIn("fixed_code=" + code + " ", result)
                    self.assertNotIn("PRIVATE", result)

    def test_missing_malformed_and_unlisted_are_sanitized(self):
        for body in (b"", b"PRIVATE", b"<html>PRIVATE</html>", b'{"error":{"message":"unsupported_feature PRIVATE"}}', b'{"error":{"code":"PRIVATE"}}', b'{"error":{"code":{}}}'):
            result = diagnostic(body, "PRIVATE/content-type")
            self.assertIn("fixed_code=none reason=", result)
            self.assertIn("content_type=other bytes=", result)
            self.assertNotIn("PRIVATE", result)


if __name__ == "__main__":
    unittest.main()
