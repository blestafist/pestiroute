import contextlib
import io
from pathlib import Path
import sqlite3
import tempfile
import unittest
from unittest.mock import patch
from subprocess import CompletedProcess

from antigravity_budgeted_refresh import refresh


class RefreshTest(unittest.TestCase):
    def test_capture_budget_and_secret_boundary(self):
        for success in (True, False):
            with self.subTest(success=success), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                (root / "client-secret").write_text("PRIVATE_CONFIG\n")
                db = sqlite3.connect(root / "gateway.db")
                db.executescript("CREATE TABLE accounts(id TEXT, connector TEXT, enabled INT); INSERT INTO accounts VALUES('antigravity-local','antigravity',1); CREATE TABLE operator_request_budgets(account_id TEXT,ceiling INT,consumed INT); INSERT INTO operator_request_budgets VALUES('m10-antigravity-authorized-account',1000,9);")
                db.close()

                def run(argv, **kw):
                    self.assertNotIn("PRIVATE_CONFIG", repr(argv))
                    self.assertEqual(kw["env"]["PESTIROUTE_ANTIGRAVITY_CLIENT_SECRET"], "PRIVATE_CONFIG")
                    self.assertEqual(argv[-4:], ["--debug", "refresh", "--account", "antigravity-local"])
                    self.assertTrue(kw["capture_output"])
                    db = sqlite3.connect(root / "gateway.db")
                    db.execute("UPDATE operator_request_budgets SET consumed=10")
                    db.commit()
                    db.close()
                    return CompletedProcess(argv, 0 if success else 1,
                                            "authentication refreshed revision=3\n" if success else "PRIVATE_OUTPUT",
                                            "PRIVATE_ERROR\nauthentication diagnostic: reason=unauthenticated HTTP status 401\n")

                out = io.StringIO()
                with patch("antigravity_budgeted_refresh.subprocess.run", run), contextlib.redirect_stdout(out):
                    rc = refresh(root, "/fixture/gateway")
                self.assertEqual(rc, 0 if success else 1)
                self.assertNotIn("PRIVATE", out.getvalue())
                self.assertIn("consumed=10", out.getvalue())
                self.assertIn("reason=refreshed" if success else "reason=unauthenticated http_status=401", out.getvalue())


if __name__ == "__main__":
    unittest.main()
