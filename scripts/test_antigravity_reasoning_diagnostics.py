import unittest

from antigravity_smoke_response import stream_diagnostics


class ReasoningDiagnosticsTests(unittest.TestCase):
    prefix = 'level=DEBUG msg="Antigravity stream failure" phase=precommit category=precommit_unsupported_reasoning'

    def test_all_fixed_kind_signature_combinations(self):
        for kind in ("thought_only", "signature_only", "both"):
            for signature_type in ("absent", "null", "string", "other"):
                with self.subTest(kind=kind, signature_type=signature_type):
                    line = f"{self.prefix} kind={kind} signature_type={signature_type} private=CANARY-DO-NOT-EMIT"
                    self.assertEqual(stream_diagnostics(line), [
                        f"phase=precommit category=precommit_unsupported_reasoning kind={kind} signature_type={signature_type}"
                    ])
                    quoted = f'{self.prefix} kind="{kind}" signature_type="{signature_type}"'
                    self.assertEqual(len(stream_diagnostics(quoted)), 1)
                    self.assertNotIn("CANARY", " ".join(stream_diagnostics(line)))

    def test_missing_required_fields_rejected(self):
        for suffix in ("", " kind=both", " signature_type=other"):
            with self.subTest(suffix=suffix):
                self.assertEqual(stream_diagnostics(self.prefix + suffix), [])

    def test_malformed_duplicate_and_unlisted_values_rejected(self):
        for suffix in (
            " kind=unknown signature_type=other",
            " kind=bothx signature_type=other",
            " kind=both signature_type=private",
            " kind=both signature_type=otherx",
            " kind=both signature_type=other kind=thought_only",
            " kind=both signature_type=other signature_type=private",
            ' kind="both signature_type=other',
        ):
            with self.subTest(suffix=suffix):
                self.assertEqual(stream_diagnostics(self.prefix + suffix + " CANARY-PRIVATE"), [])


if __name__ == "__main__":
    unittest.main()
