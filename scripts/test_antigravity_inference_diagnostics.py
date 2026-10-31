import unittest

from antigravity_smoke_response import inference_diagnostics


class InferenceDiagnosticTests(unittest.TestCase):
    def test_extracts_only_allowlisted_fields(self):
        log = ('time=1 level=DEBUG msg="Antigravity inference diagnostic" status=503 '
               'branch=http_error upstream_status=RESOURCE_EXHAUSTED '
               'transport_error_class=none secret="DO-NOT-PRINT"\n'
               'level=DEBUG msg="Antigravity inference diagnostic" status=999 branch="leak" '
               'upstream_status="private" transport_error_class="none"\n')
        self.assertEqual(inference_diagnostics(log), [
            "status=503 branch=http_error upstream_status=RESOURCE_EXHAUSTED transport_error_class=none"
        ])


if __name__ == "__main__":
    unittest.main()
