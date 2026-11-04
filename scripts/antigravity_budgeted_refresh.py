"""One explicit admin refresh; private configuration/output never reaches logs."""
import os
from pathlib import Path
import re
import sqlite3
import subprocess


REASONS = {
    "unauthenticated", "auth_refresh_failed", "upstream_error", "rate_limited",
    "invalid_request", "persistence_failed", "revision_mismatch", "cancelled",
    "deadline_exceeded", "unsupported_feature", "unavailable", "auth_unavailable",
}


def refresh(root, binary):
    root = Path(root)
    db = sqlite3.connect("file:" + str(root / "gateway.db") + "?mode=ro", uri=True)
    try:
        db.execute("PRAGMA query_only=ON")
        accounts = db.execute("SELECT id FROM accounts WHERE id='antigravity-local' AND connector='antigravity' AND enabled=1").fetchall()
        before = db.execute("SELECT ceiling, consumed FROM operator_request_budgets WHERE account_id='m10-antigravity-authorized-account'").fetchone()
        if len(accounts) != 1 or before is None or not 0 <= before[1] < before[0] <= 1000:
            raise ValueError()
        # Same protected plain-text runtime configuration used by the operator launcher.
        secret = (root / "client-secret").read_text().rstrip("\n")
        if not secret or "\n" in secret or "\x00" in secret:
            raise ValueError()
        env = dict(os.environ, PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT="1",
                   PESTIROUTE_ANTIGRAVITY_CLIENT_SECRET=secret)
        print(f"refresh budget_before ceiling={before[0]} consumed={before[1]}", flush=True)
        result = subprocess.run([str(binary), "admin", "--db", str(root / "gateway.db"),
                                 "--master-key", str(root / "master.key"), "auth", "--debug",
                                 "refresh", "--account", accounts[0][0]],
                                env=env, capture_output=True, text=True, timeout=120)
        del secret, env
        after = db.execute("SELECT ceiling, consumed FROM operator_request_budgets WHERE account_id='m10-antigravity-authorized-account'").fetchone()
        print(f"refresh budget_after ceiling={after[0]} consumed={after[1]}")
        success = result.returncode == 0 and re.fullmatch(r"authentication refreshed revision=\d+\s*", result.stdout) is not None
        if success and after == (before[0], before[1] + 1):
            print("refresh success=true reason=refreshed")
            return 0
        # No arbitrary raw stdout, stderr, or provider error is forwarded.
        match = re.search(r"authentication diagnostic: reason=([a-z_]+)(?: HTTP status ([1-5][0-9]{2}))?(?:\n|$)", result.stderr)
        reason = match[1] if match and match[1] in REASONS else "refresh_failed_unmapped"
        status = match[2] if match and match[2] else "unavailable"
        print(f"refresh success=false reason={reason} http_status={status}")
        return 1
    finally:
        db.close()


if __name__ == "__main__":
    try:
        code = refresh(os.environ["PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT"], os.environ["GATEWAY_BIN"])
    except Exception:
        print("refresh success=false reason=local_refresh_execution_failed")
        code = 1
    raise SystemExit(code)
