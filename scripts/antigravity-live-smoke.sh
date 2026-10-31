#!/usr/bin/env bash
# One authorized, bounded real-transport smoke. Never traces or prints secrets or response text.
set -euo pipefail
umask 077
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

: "${PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT:?set the authorized persistent operator root}"
: "${PESTIROUTE_ANTIGRAVITY_PROJECT:?set the authorized project identifier}"
: "${PESTIROUTE_ANTIGRAVITY_MODEL:?set gemini-2.5-pro or gemini-2.5-flash}"
: "${GATEWAY_BIN:?set the built gateway binary path}"
case "$PESTIROUTE_ANTIGRAVITY_MODEL" in gemini-2.5-pro|gemini-2.5-flash) ;; *) echo 'unsupported model selection' >&2; exit 2 ;; esac
[[ $PESTIROUTE_ANTIGRAVITY_PROJECT =~ ^[A-Za-z0-9._:-]+$ ]] || { echo 'invalid project identifier' >&2; exit 2; }
DB=$PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT/gateway.db
MASTER_KEY=$PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT/master.key
test -r "$DB" && test -r "$MASTER_KEY" && test -x "$GATEWAY_BIN"
POLICY_ID=m10-028-live-smoke
export PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT=1
WORK=$(mktemp -d "${TMPDIR:-/tmp}/pestiroute-m10-live.XXXXXX")
chmod 700 "$WORK"
gateway_pid=
key_id=
cleanup() {
  rc=$?
  trap - EXIT
  if [[ -n $key_id ]]; then
    if "$GATEWAY_BIN" admin --db "$DB" --master-key "$MASTER_KEY" key revoke "$key_id" >/dev/null 2>&1; then
      echo 'ephemeral_key_revoked=true'
    else
      echo 'ephemeral_key_revoked=false' >&2
      rc=1
    fi
  fi
  if [[ -n ${gateway_pid:-} ]]; then kill "$gateway_pid" 2>/dev/null || true; wait "$gateway_pid" 2>/dev/null || true; echo 'gateway_stopped=true'; fi
  rm -rf -- "$WORK"
  test ! -e "$WORK" && echo 'temporary_runner_files_removed=true'
  exit "$rc"
}
trap cleanup EXIT INT TERM

# Verify only safe account metadata; never enumerate or display credentials.
ACCOUNT_META=$("$GATEWAY_BIN" admin --db "$DB" --master-key "$MASTER_KEY" account get antigravity-local 2>/dev/null) || { echo 'expected account metadata unavailable' >&2; exit 1; }
[[ $ACCOUNT_META == 'id=antigravity-local connector=antigravity enabled=true '* ]] || { echo 'expected enabled Antigravity account not found' >&2; exit 1; }
unset ACCOUNT_META

# Reuse only this exact gateway-local policy, otherwise create it with the task's bounded limits.
if POLICY_META=$("$GATEWAY_BIN" admin --db "$DB" --master-key "$MASTER_KEY" policy get "$POLICY_ID" 2>/dev/null); then
  [[ $POLICY_META == *'enabled=true'*'models=["gemini-2.5-pro","gemini-2.5-flash"]'*'connectors=["antigravity"]'*'rpm=10 tpm=10000'* ]] || { echo 'existing smoke policy metadata mismatch' >&2; exit 1; }
else
  "$GATEWAY_BIN" admin --db "$DB" --master-key "$MASTER_KEY" policy create --id "$POLICY_ID" --models gemini-2.5-pro,gemini-2.5-flash --connectors antigravity --rpm 10 --tpm 10000 >/dev/null 2>&1 || { echo 'smoke policy creation failed' >&2; exit 1; }
fi
unset POLICY_META

# Capture the one-time key-create output only in this process; never forward it to logs or argv.
KEY_OUT=$("$GATEWAY_BIN" admin --db "$DB" --master-key "$MASTER_KEY" key create --policy "$POLICY_ID" 2>/dev/null) || { echo 'ephemeral key creation failed' >&2; exit 1; }
IFS=' ' read -r -a key_fields <<<"$KEY_OUT"
for field in "${key_fields[@]}"; do
  case $field in secret=*) key_secret=${field#secret=} ;; id=*) key_id=${field#id=} ;; esac
done
unset KEY_OUT key_fields field
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
    settings: {project: "$PESTIROUTE_ANTIGRAVITY_PROJECT", model: "$PESTIROUTE_ANTIGRAVITY_MODEL", account_id: antigravity-local}
routes:
  - id: antigravity-live-smoke
    protocol: openai.responses.v1
    mode: translation
    model: "$PESTIROUTE_ANTIGRAVITY_MODEL"
    adapter: pestiroute.responses.native
    policy: antigravity-live-smoke-policy
    budget: {unknown_estimate: reserve, conservative_tokens: 4096}
    targets: [{connector: antigravity, account: antigravity-local}]
policies: {antigravity-live-smoke-policy: "$POLICY_ID"}
YAML

budget() {
  python3 - "$DB" "${1:-0}" <<'PY'
import sqlite3, sys
db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
db.execute("PRAGMA query_only=ON")
row = db.execute("SELECT ceiling, consumed FROM operator_request_budgets WHERE account_id='m10-antigravity-authorized-account'").fetchone()
db.close()
needed = int(sys.argv[2])
if row is None or not (0 <= row[1] <= row[0] <= 1000) or row[0] - row[1] < needed: raise SystemExit("budget preflight failed")
print(f"budget ceiling={row[0]} consumed={row[1]}")
PY
}
budget 2
"$GATEWAY_BIN" --debug --config "$WORK/gateway.yaml" --config-format yaml >"$WORK/gateway.log" 2>&1 &
gateway_pid=$!
python3 - "$PORT" "$gateway_pid" "$WORK/gateway.log" <<'PY'
import glob, os, sys, time, urllib.error, urllib.request
port, pid, logfile = sys.argv[1], int(sys.argv[2]), sys.argv[3]
def startup_failure():
    try: text = open(logfile, encoding="utf-8", errors="replace").read()
    except Exception: text = ""
    patterns = [("invalid runtime master key", "runtime_master_key_invalid"), ("database lock", "database_lock_failed"), ("must be at least 4096", "route_budget_too_low"), ("invalid yaml", "yaml_configuration_invalid"), ("listen", "listener_start_failed"), ("account", "account_setup_failed"), ("credential", "credential_setup_failed"), ("config", "gateway_configuration_invalid")]
    code = next((code for needle, code in patterns if needle in text.lower()), "gateway_startup_failed")
    print(f"gateway readiness=failed startup_code={code}")
    raise SystemExit(1)
def owns_listener():
    wanted = f"{int(port):04X}"
    inodes = {link[8:-1] for fd in glob.glob(f"/proc/{pid}/fd/*")
              if (link := os.readlink(fd)).startswith("socket:[")}
    with open("/proc/net/tcp", encoding="ascii") as table:
        return any((fields := row.split())[1].endswith(":" + wanted) and fields[3] == "0A" and fields[9] in inodes
                   for row in table.readlines()[1:])
for _ in range(100):
    proc = f"/proc/{pid}/stat"
    if not os.path.exists(proc): startup_failure()
    with open(proc, encoding="ascii") as f: state = f.read().rsplit(")", 1)[1].strip().split()[0]
    if state == "Z": startup_failure()
    status = None
    try:
        response = urllib.request.urlopen(f"http://127.0.0.1:{port}/", timeout=.2)
        status = response.status
        response.close()
    except urllib.error.HTTPError as e:
        status = e.code
    except Exception: pass
    if status == 404 and owns_listener(): print("gateway readiness=owned-loopback-404"); break
    time.sleep(.1)
else: raise SystemExit("gateway readiness failed")
PY

cat >"$WORK/request.py" <<'PY'
import http.client, json, sys
from antigravity_smoke_response import diagnostic

def request(conn, model):
    key = sys.stdin.readline().rstrip("\n")
    if not key or sys.stdin.readline(): return 2
    body = json.dumps({"model": model, "stream": True, "include": ["reasoning.encrypted_content"], "input": "Write one short sentence about a sailboat crossing a calm blue lake."})
    conn.request("POST", "/v1/responses", body, {"Authorization": "Bearer " + key, "Content-Type": "application/json"})
    key = ""
    resp = conn.getresponse()
    count, terminal, completed, nonempty = 0, False, False, False
    carrier_count, carrier_strings = 0, True
    event = "other"
    error_diagnostic = "fixed_code=none reason=success_stream"
    event_counts = {"response.created": 0, "response.output_item.added": 0, "response.content_part.added": 0, "response.output_text.delta": 0, "response.output_text.done": 0, "response.output_item.done": 0, "response.completed": 0, "response.failed": 0, "response.incomplete": 0, "other": 0}
    event_counts.update({name: 0 for name in ("response.reasoning_summary_part.added", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_summary_part.done")})
    if resp.status == 200:
        for raw in resp:
            line = raw.decode("utf-8", "ignore").strip()
            if line.startswith("event: "):
                event = line[7:] if line[7:] in event_counts else "other"
                count += 1
                event_counts[event] += 1
                if event in {"response.completed", "response.failed", "response.incomplete"}: terminal = True
                if event == "response.completed": completed = True
            elif line.startswith("data: ") and event in {"response.output_text.delta", "response.output_item.done"}:
                try:
                    value = json.loads(line[6:])
                    if not isinstance(value, dict): continue
                    if event == "response.output_text.delta":
                        delta = value.get("delta")
                        nonempty = nonempty or (isinstance(delta, str) and bool(delta.strip()))
                    else:
                        item = value.get("item")
                        # Count only finalized reasoning items, not repeated completed-response copies.
                        if isinstance(item, dict) and item.get("type") == "reasoning" and "encrypted_content" in item:
                            carrier_count += 1
                            carrier_strings = carrier_strings and isinstance(item["encrypted_content"], str)
                except (ValueError, TypeError):
                    pass
    else:
        error_diagnostic = " ".join(diagnostic(resp.read(65536), resp.getheader("Content-Type", "")).split()[:2])
    print(f"request status={resp.status} sse_events={count} nonempty={str(nonempty).lower()} terminal={str(terminal).lower()} completed={str(completed).lower()} event_counts={','.join(f'{key}:{value}' for key, value in event_counts.items() if value)} {error_diagnostic} reasoning_encrypted_content_count={carrier_count} has_reasoning_encrypted_content={str(carrier_count > 0).lower()} reasoning_encrypted_content_all_strings={str(carrier_count > 0 and carrier_strings).lower()}")
    return int(resp.status != 200 or not nonempty or not terminal or not completed)

conn = None
try:
    conn = http.client.HTTPConnection("127.0.0.1", int(sys.argv[1]), timeout=90)
    rc = request(conn, sys.argv[2])
except Exception:
    # Exception messages can contain arbitrary status/header/body bytes. Never print them.
    print("request reason=transport_or_parse_failed")
    rc = 1
finally:
    if conn is not None:
        try:
            conn.close()
        except Exception:
            print("request reason=transport_or_parse_failed")
            rc = 1
raise SystemExit(rc)
PY
if printf '%s\n' "$key_secret" | PYTHONPATH="$SCRIPT_DIR" python3 "$WORK/request.py" "$PORT" "$PESTIROUTE_ANTIGRAVITY_MODEL"; then
  request_rc=0
else
  request_rc=$?
fi
unset key_secret
budget
PYTHONPATH="$SCRIPT_DIR" python3 - "$WORK/gateway.log" <<'PY'
import sys
from antigravity_smoke_response import inference_diagnostics, stream_diagnostics
try:
    with open(sys.argv[1], encoding="utf-8", errors="replace") as log:
        text = log.read(1 << 20)
        diagnostics = inference_diagnostics(text)
        streams = stream_diagnostics(text)
except OSError:
    diagnostics = []
    streams = []
for diagnostic in diagnostics:
    print("gateway inference " + diagnostic)
if not diagnostics:
    print("gateway inference reason=diagnostic_unavailable")
for diagnostic in streams:
    print("gateway stream " + diagnostic)
if not streams:
    print("gateway stream reason=diagnostic_unavailable")
PY
exit "$request_rc"
