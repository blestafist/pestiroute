#!/usr/bin/env bash
# Bounded live wrapper; implementation and tests use only local fake HTTP.
set -euo pipefail
umask 077
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
: "${PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT:?set separately authorized operator root}"
: "${PESTIROUTE_ANTIGRAVITY_PROJECT:?set separately authorized project}"
: "${GATEWAY_BIN:?set offline-built gateway binary}"
[[ ${PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT:-} == 1 ]] || { echo 'explicit real-transport opt-in required' >&2; exit 2; }
[[ $PESTIROUTE_ANTIGRAVITY_PROJECT =~ ^[A-Za-z0-9._:-]+$ ]] || { echo 'invalid project identifier' >&2; exit 2; }
test -x "$GATEWAY_BIN" || { echo 'gateway binary unavailable' >&2; exit 2; }
DB=$PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT/gateway.db
MASTER_KEY=$PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT/master.key
test -r "$DB" && test -r "$MASTER_KEY" || { echo 'operator store unavailable' >&2; exit 2; }
export PESTIROUTE_ANTIGRAVITY_MODEL=gemini-2.5-flash
POLICY_ID=m10-043-live-tool-smoke
WORK=$(mktemp -d "${TMPDIR:-/tmp}/pestiroute-m10-tool.XXXXXX")
chmod 700 "$WORK"
gateway_pid=
key_id=
report_gateway_diagnostics() {
  if [[ ! -f $WORK/gateway.log ]]; then
    echo 'gateway_diagnostics=unavailable'
    return 0
  fi
  PYTHONPATH="$SCRIPT_DIR" python3 - "$WORK/gateway.log" <<'PY' || true
import sys
from antigravity_smoke_response import inference_diagnostics, stream_diagnostics
try:
    with open(sys.argv[1], encoding="utf-8", errors="replace") as stream:
        text = stream.read(1 << 20)
    inference = inference_diagnostics(text)
    failures = stream_diagnostics(text)
except Exception:
    inference, failures = [], []
for record in inference:
    print("gateway_inference " + record)
for record in failures:
    print("gateway_stream " + record)
if not inference and not failures:
    print("gateway_diagnostics=unavailable")
PY
}
cleanup() {
  rc=$?
  trap - EXIT INT TERM
  if [[ $rc -ne 0 ]]; then report_gateway_diagnostics; fi
  if [[ -n $key_id ]]; then
    if "$GATEWAY_BIN" admin --db "$DB" --master-key "$MASTER_KEY" key revoke "$key_id" >/dev/null 2>&1; then
      echo 'ephemeral_key_revoked=true'
    else
      echo 'ephemeral_key_revoked=false' >&2
      rc=1
    fi
  fi
  if [[ -n $gateway_pid ]]; then
    kill "$gateway_pid" 2>/dev/null || true
    wait "$gateway_pid" 2>/dev/null || true
    echo 'gateway_stopped=true'
  fi
  rm -rf -- "$WORK"
  test ! -e "$WORK" && echo 'temporary_runner_files_removed=true'
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Read only the durable aggregate budget. Never query credentials or payload tables.
budget_state=$(python3 - "$DB" <<'PY'
import sqlite3, sys
db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
db.execute("PRAGMA query_only=ON")
row = db.execute("SELECT ceiling,consumed FROM operator_request_budgets WHERE account_id='m10-antigravity-authorized-account'").fetchone()
db.close()
if row is None or type(row[0]) is not int or type(row[1]) is not int: raise SystemExit(1)
if not (0 <= row[1] <= row[0] <= 1000) or row[0] - row[1] < 2: raise SystemExit(1)
print(row[0], row[1])
PY
) || { echo 'request budget preflight failed' >&2; exit 1; }
read -r budget_ceiling before <<<"$budget_state"
unset budget_state

if policy_meta=$("$GATEWAY_BIN" admin --db "$DB" --master-key "$MASTER_KEY" policy get "$POLICY_ID" 2>/dev/null); then
  [[ $policy_meta == *'enabled=true'*'models=["gemini-2.5-flash"]'*'connectors=["antigravity"]'*'rpm=10 tpm=10000'* ]] || { echo 'existing smoke policy mismatch' >&2; exit 1; }
  unset policy_meta
else
  "$GATEWAY_BIN" admin --db "$DB" --master-key "$MASTER_KEY" policy create --id "$POLICY_ID" --models gemini-2.5-flash --connectors antigravity --rpm 10 --tpm 10000 >/dev/null 2>&1 || { echo 'smoke policy setup failed' >&2; exit 1; }
fi
key_output=$("$GATEWAY_BIN" admin --db "$DB" --master-key "$MASTER_KEY" key create --policy "$POLICY_ID" 2>/dev/null) || { echo 'ephemeral key creation failed' >&2; exit 1; }
for field in $key_output; do
  case $field in secret=*) key_secret=${field#secret=} ;; id=*) key_id=${field#id=} ;; esac
done
unset key_output field
if [[ -z ${key_secret:-} || -z $key_id ]]; then echo 'ephemeral key metadata unavailable' >&2; exit 1; fi
PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
cat >"$WORK/gateway.yaml" <<YAML
version: 1
server: {listen: "127.0.0.1:$PORT", max_request_bytes: 1048576, shutdown_timeout: 5s}
storage: {driver: sqlite, path: "$DB"}
secrets: {master_key_file: "$MASTER_KEY"}
connectors:
  - id: antigravity
    kind: connector
    implementation: pestiroute.antigravity.v1internal
    protocols: [openai.responses.v1]
    settings: {project: "$PESTIROUTE_ANTIGRAVITY_PROJECT", model: gemini-2.5-flash, account_id: antigravity-local}
routes:
  - id: antigravity-tool-smoke
    protocol: openai.responses.v1
    mode: translation
    model: gemini-2.5-flash
    adapter: pestiroute.responses.native
    policy: antigravity-tool-smoke-policy
    budget: {unknown_estimate: reserve, conservative_tokens: 4096}
    targets: [{connector: antigravity, account: antigravity-local}]
policies: {antigravity-tool-smoke-policy: "$POLICY_ID"}
YAML

"$GATEWAY_BIN" --debug --config "$WORK/gateway.yaml" --config-format yaml >"$WORK/gateway.log" 2>&1 &
gateway_pid=$!
python3 - "$PORT" "$gateway_pid" <<'PY'
import os, sys, time, urllib.error, urllib.request
port, pid = sys.argv[1], int(sys.argv[2])
for _ in range(100):
    if not os.path.exists(f"/proc/{pid}/stat"): raise SystemExit(1)
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/", timeout=.2): pass
    except urllib.error.HTTPError as error:
        if error.code == 404: break
    except Exception: pass
    time.sleep(.1)
else: raise SystemExit(1)
PY

if runner_output=$(printf '%s\n' "$key_secret" | python3 "$SCRIPT_DIR/antigravity_live_tool_smoke.py" "$PORT"); then
  runner_rc=0
else
  runner_rc=$?
fi
unset key_secret
if [[ -n ${runner_output:-} ]]; then printf '%s\n' "$runner_output"; fi
after=$(python3 - "$DB" "$before" "$budget_ceiling" "$runner_rc" <<'PY'
import sqlite3, sys
db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
db.execute("PRAGMA query_only=ON")
row = db.execute("SELECT ceiling,consumed FROM operator_request_budgets WHERE account_id='m10-antigravity-authorized-account'").fetchone()
db.close()
start = int(sys.argv[2])
ceiling = int(sys.argv[3])
runner_rc = int(sys.argv[4])
if row is None or type(row[0]) is not int or type(row[1]) is not int or row[0] != ceiling: raise SystemExit(1)
delta = None if row is None else row[1] - start
if runner_rc == 0:
    if delta != 2: raise SystemExit(1)
elif delta < 0 or delta > 2:
    raise SystemExit(1)
if not (0 <= start <= row[1] <= row[0] <= 1000): raise SystemExit(1)
print(row[1])
PY
) || { echo 'request budget ceiling check failed' >&2; exit 1; }
[[ $runner_rc -ne 0 || $runner_output == *'dispatches=2 '* ]] || { echo 'inference dispatch count mismatch' >&2; exit 1; }
printf 'budget_before=%s budget_after=%s budget_ceiling=%s max_inference_delta=2\n' "$before" "$after" "$budget_ceiling"
exit "$runner_rc"
