import unittest

from antigravity_smoke_response import stream_diagnostics


class M038StreamDiagnosticTests(unittest.TestCase):
    def test_invalid_content_subkinds_are_allowlisted(self):
        for subkind in ("done_with_candidates", "text_with_function_call", "invalid_utf8_text"):
            with self.subTest(subkind=subkind):
                log = f'msg="Antigravity stream failure" phase=precommit category=precommit_invalid_content subkind={subkind}'
                self.assertEqual(
                    stream_diagnostics(log),
                    [f"phase=precommit category=precommit_invalid_content subkind={subkind}"],
                )

    def test_unknown_or_duplicate_subkind_is_not_exposed(self):
        unknown = 'msg="Antigravity stream failure" phase=precommit category=precommit_invalid_content subkind=private_value'
        self.assertEqual(stream_diagnostics(unknown), ["phase=precommit category=precommit_invalid_content"])
        duplicate = 'msg="Antigravity stream failure" phase=precommit category=precommit_invalid_content subkind=done_with_candidates subkind=invalid_utf8_text'
        self.assertEqual(stream_diagnostics(duplicate), [])


if __name__ == "__main__":
    unittest.main()
