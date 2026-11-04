import unittest

from antigravity_smoke_response import stream_diagnostics


class M039StreamDiagnosticsTests(unittest.TestCase):
    def test_postcommit_cause_allowlist_and_safe_details(self):
        logs = '\n'.join((
            'msg="Antigravity stream failure" phase=postcommit category=postcommit_error cause=eof',
            'msg="Antigravity stream failure" phase=postcommit category=postcommit_error cause=unsupported_reasoning kind=both signature_type=null',
            'msg="Antigravity stream failure" phase=postcommit category=postcommit_error cause=invalid_content subkind=invalid_utf8_text',
            'msg="Antigravity stream failure" phase=postcommit category=postcommit_error cause=PRIVATE-CANARY',
            'msg="Antigravity stream failure" phase=postcommit category=postcommit_error cause=eof cause=framing',
            'msg="Antigravity stream failure" phase=postcommit category=postcommit_error cause=unsupported_reasoning kind=PRIVATE-CANARY signature_type=null',
            'msg="Antigravity stream failure" phase=postcommit category=postcommit_error cause=invalid_content subkind=private_value',
            'msg="Antigravity stream failure" phase=postcommit category=postcommit_error cause=eof subkind=invalid_utf8_text subkind=done_with_candidates',
        ))
        self.assertEqual(stream_diagnostics(logs), [
            'phase=postcommit category=postcommit_error cause=eof',
            'phase=postcommit category=postcommit_error cause=unsupported_reasoning kind=both signature_type=null',
            'phase=postcommit category=postcommit_error cause=invalid_content subkind=invalid_utf8_text',
        ])
        self.assertNotIn('PRIVATE-CANARY', ''.join(stream_diagnostics(logs)))


if __name__ == "__main__":
    unittest.main()
