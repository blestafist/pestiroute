#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
binary=${PESTIROUTE_BINARY:-"$root/bin/gateway"}
if [[ ! -x "$binary" ]]; then
  echo "verify-recovery: build binary first with scripts/release-build.sh" >&2
  exit 1
fi
if [[ $(id -u) -eq 0 ]]; then
  echo "verify-recovery: run as an unprivileged account" >&2
  exit 1
fi
for tool in curl python3 sqlite3 git tar go sha256sum; do
  command -v "$tool" >/dev/null || { echo "verify-recovery: missing $tool" >&2; exit 1; }
done

umask 077
work=$(mktemp -d "${TMPDIR:-/tmp}/pestiroute-recovery.XXXXXX")
gateway_pid= fake_pid=
cleanup() {
  for pid in "$gateway_pid" "$fake_pid"; do
    [[ -z "$pid" ]] || kill "$pid" 2>/dev/null || true
  done
  for pid in "$gateway_pid" "$fake_pid"; do
    [[ -z "$pid" ]] || wait "$pid" 2>/dev/null || true
  done
  rm -rf "$work"
}
trap cleanup EXIT HUP INT TERM

DB="$work/state/gateway.db" KEY="$work/config/master.key"
mkdir -m 0700 "$work/state" "$work/config"
python3 - "$KEY" "$work/malformed.key" "$work/wrong.key" <<'PY'
import os, sys
for path, data in ((sys.argv[1], os.urandom(32)), (sys.argv[2], os.urandom(31)), (sys.argv[3], b'W' * 32)):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as f:
        f.write(data)
PY

read -r upstream_port gateway_port < <(python3 - <<'PY'
import socket
ports=[]
for _ in range(2):
    s=socket.socket(); s.bind(("127.0.0.1", 0)); ports.append(s.getsockname()[1]); s.close()
print(*ports)
PY
)

legacy_commit=f0ed045 # Latest complete protected runtime before schema migration 6.
legacy_source="$work/legacy-source"
legacy_binary="$work/gateway-v5"
mkdir -m 0700 "$legacy_source"
git -C "$root" archive "$legacy_commit" | tar -x -C "$legacy_source"
(cd "$legacy_source" && GOTOOLCHAIN=go1.27.1 CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -buildvcs=false -ldflags='-buildid=' -o "$legacy_binary" ./cmd/gateway)

# Create real v5 state with a binary that supports only schema v5.
"$legacy_binary" admin --db "$DB" --master-key "$KEY" migrate >/dev/null
[[ $(sqlite3 "$DB" 'SELECT max(version) FROM schema_migrations;') == 5 ]]
"$legacy_binary" admin --db "$DB" --master-key "$KEY" account create --id recovery-account --connector recovery-upstream >/dev/null
printf '%s\n' recovery-fake-credential | "$legacy_binary" admin --db "$DB" --master-key "$KEY" credential create --account recovery-account --id PESTIROUTE_RECOVERY_UPSTREAM >/dev/null
"$legacy_binary" admin --db "$DB" --master-key "$KEY" policy create --id recovery-policy --models recovery-model --connectors recovery-upstream --rpm 20 --tpm 10000 >/dev/null
"$legacy_binary" admin --db "$DB" --master-key "$KEY" key create --policy recovery-policy >"$work/key-output"
key_secret=$(sed -n 's/.*secret=\([^ ]*\).*/\1/p' "$work/key-output")
key_id=$(sed -n 's/.* id=\([^ ]*\).*/\1/p' "$work/key-output")
[[ -n "$key_secret" ]]
[[ -n "$key_id" ]]
chmod 0600 "$work/key-output"
"$legacy_binary" admin --db "$DB" --master-key "$KEY" key create --policy recovery-policy >"$work/revoked-key-output"
revoked_secret=$(sed -n 's/.*secret=\([^ ]*\).*/\1/p' "$work/revoked-key-output")
revoked_id=$(sed -n 's/.* id=\([^ ]*\).*/\1/p' "$work/revoked-key-output")
[[ -n "$revoked_secret" && -n "$revoked_id" ]]
chmod 0600 "$work/revoked-key-output"
"$legacy_binary" admin --db "$DB" --master-key "$KEY" key revoke "$revoked_id" >/dev/null
V5_BACKUP="$work/pre-upgrade-v5.db"

cat >"$work/fake.py" <<'PY'
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json, sys
class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        with open(sys.argv[2], "a", encoding="utf-8") as marker: marker.write("request\n")
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        assert self.path == "/v1/responses"
        assert self.headers.get("Authorization") == "Bearer recovery-fake-credential"
        assert json.loads(body)["model"] == "recovery-model"
        data = b'{"id":"recovery-fake-response","status":"completed","usage":{"input_tokens":2,"output_tokens":3}}'
        self.send_response(200); self.send_header("Content-Length", str(len(data)))
        self.end_headers(); self.wfile.write(data)
    def log_message(self, *_): pass
ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
PY
: >"$work/upstream-requests"
python3 -u "$work/fake.py" "$upstream_port" "$work/upstream-requests" >"$work/fake.log" 2>&1 & fake_pid=$!
python3 - "$upstream_port" <<'PY'
import socket, sys, time
deadline = time.monotonic() + 10
while time.monotonic() < deadline:
    try:
        with socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=0.1): break
    except OSError: time.sleep(0.05)
else: raise SystemExit("verify-recovery: fake upstream did not start")
PY

write_config() {
  local path=$1 db=$2 key=$3
  cat >"$path" <<YAML
version: 1
server: {listen: "127.0.0.1:$gateway_port", max_request_bytes: 1048576, shutdown_timeout: 5s}
storage: {driver: sqlite, path: "$db"}
secrets: {master_key_file: "$key"}
connectors:
  - id: recovery-upstream
    kind: connector
    implementation: pestiroute.responses.native
    protocols: [openai.responses.v1]
    settings: {base_url: "http://127.0.0.1:$upstream_port/v1/responses", upstream_protocol: openai.responses.v1, mode: native, credential_env: PESTIROUTE_RECOVERY_UPSTREAM, max_request_body_bytes: 1048576, max_request_header_bytes: 16384, connect_timeout: 1s, tls_handshake_timeout: 1s, response_header_timeout: 3s, stream_idle_timeout: 5s}
routes:
  - id: recovery-route
    protocol: openai.responses.v1
    mode: native
    model: recovery-model
    adapter: pestiroute.responses.native
    policy: local
    budget: {unknown_estimate: reserve, conservative_tokens: 100}
    targets: [{connector: recovery-upstream, account: recovery-account}]
policies: {local: recovery-policy}
YAML
  chmod 0600 "$path"
}
start_gateway_with() {
  local executable=$1 config=$2 log=$3
  PESTIROUTE_RECOVERY_UPSTREAM=recovery-fake-credential "$executable" -config-format yaml -config "$config" >"$log" 2>&1 &
  gateway_pid=$!
  for _ in $(seq 1 100); do
    if curl -fsS "http://127.0.0.1:$gateway_port/readyz" >/dev/null 2>&1; then return; fi
    if ! kill -0 "$gateway_pid" 2>/dev/null; then cat "$log" >&2; return 1; fi
    sleep 0.1
  done
  cat "$log" >&2
  echo "verify-recovery: gateway did not become ready" >&2
  return 1
}
start_gateway() { start_gateway_with "$binary" "$1" "$2"; }
check_foreign_keys() {
  local db=$1 violations
  if ! violations=$(sqlite3 "$db" 'PRAGMA foreign_key_check;'); then
    return 1
  fi
  [[ -z "$violations" ]]
}
request() {
  local token=$1 label=$2 status
  status=$(curl -sS -o "$work/$label.json" -w '%{http_code}' "http://127.0.0.1:$gateway_port/v1/responses" \
    -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    --data-binary '{"model":"recovery-model","input":"recovery-check"}')
  [[ "$status" == 200 ]] && grep -q recovery-fake-response "$work/$label.json"
}
restore_database() {
  local source=$1 destination=$2 expected_schema=$3 stage schema
  # Stage and validate a fresh file, then hard-link only if the destination is
  # still absent. Never write or rename over an operator's existing database.
  [[ ! -e "$destination" ]] || return 1
  stage="$destination.restore.$$.${RANDOM}"
  if ! sqlite3 "$source" ".backup '$stage'" >"$work/restore-cli.log" 2>&1; then
    rm -f "$stage" "$stage-wal" "$stage-shm"
    return 1
  fi
  if [[ $(sqlite3 "$stage" 'PRAGMA integrity_check;' 2>/dev/null) != ok ]] ||
     ! check_foreign_keys "$stage"; then
    rm -f "$stage" "$stage-wal" "$stage-shm"
    return 1
  fi
  if ! schema=$(sqlite3 "$stage" "SELECT printf('%d:%d:%d', count(*), min(version), max(version)) FROM schema_migrations;" 2>/dev/null) ||
     [[ "$schema" != "$expected_schema:1:$expected_schema" ]]; then
    rm -f "$stage" "$stage-wal" "$stage-shm"
    return 1
  fi
  chmod 0600 "$stage"
  if ! ln "$stage" "$destination"; then
    rm -f "$stage" "$stage-wal" "$stage-shm"
    return 1
  fi
  rm -f "$stage" "$stage-wal" "$stage-shm"
}

CONFIG="$work/gateway.yaml"
write_config "$CONFIG" "$DB" "$KEY"
# Exercise the actual pre-upgrade v5 runtime before backup: serving a protected
# request records historical usage in the v5 database, then the process stops.
start_gateway_with "$legacy_binary" "$CONFIG" "$work/v5-before-upgrade.log"
request "$key_secret" v5-before-upgrade
[[ $(sqlite3 "$DB" 'SELECT count(*) FROM usage_records;') -ge 1 ]]
[[ $(sqlite3 "$DB" 'SELECT max(version) FROM schema_migrations;') == 5 ]]
kill -TERM "$gateway_pid"; wait "$gateway_pid"; gateway_pid=
sqlite3 "$DB" ".backup '$V5_BACKUP'"
[[ $(sqlite3 "$V5_BACKUP" 'PRAGMA integrity_check;') == ok ]]
check_foreign_keys "$V5_BACKUP"
[[ $(sqlite3 "$V5_BACKUP" 'SELECT max(version) FROM schema_migrations;') == 5 ]]
[[ $(sqlite3 "$V5_BACKUP" "SELECT count(*) FROM accounts WHERE id='recovery-account';") == 1 ]]
[[ $(sqlite3 "$V5_BACKUP" "SELECT count(*) FROM credentials WHERE account_id='recovery-account';") == 1 ]]
[[ $(sqlite3 "$V5_BACKUP" "SELECT count(*) FROM key_policies WHERE id='recovery-policy';") == 1 ]]
[[ $(sqlite3 "$V5_BACKUP" 'SELECT count(*) FROM virtual_keys;') == 2 ]]
[[ $(sqlite3 "$V5_BACKUP" 'SELECT count(*) FROM virtual_keys WHERE revoked=1;') == 1 ]]
V5_USAGE=$(sqlite3 "$V5_BACKUP" 'SELECT count(*) FROM usage_records;')
[[ "$V5_USAGE" -ge 1 ]]
# The current packaged binary performs the stopped v5→v6 migration.
"$binary" admin --db "$DB" --master-key "$KEY" migrate >/dev/null
[[ $(sqlite3 "$DB" 'SELECT max(version) FROM schema_migrations;') == 6 ]]
[[ $(sqlite3 "$DB" 'SELECT count(*) FROM usage_records;') == "$V5_USAGE" ]]
UNMIGRATED="$work/unmigrated-v5.db"
restore_database "$V5_BACKUP" "$UNMIGRATED" 5
UNMIGRATED_CONFIG="$work/unmigrated-v5.yaml"
write_config "$UNMIGRATED_CONFIG" "$UNMIGRATED" "$KEY"
if PESTIROUTE_RECOVERY_UPSTREAM=recovery-fake-credential "$binary" -config-format yaml -config "$UNMIGRATED_CONFIG" >"$work/unmigrated.log" 2>&1; then
  echo "verify-recovery: current binary accepted unmigrated v5 schema" >&2; exit 1
fi
grep -q 'runtime database is not fully migrated' "$work/unmigrated.log"
start_gateway "$CONFIG" "$work/gateway.log"
request "$key_secret" before-backup
[[ $(sqlite3 "$DB" 'SELECT count(*) FROM usage_records;') -gt "$V5_USAGE" ]]
# The gateway is still open (and WAL mode is active): .backup invokes SQLite's
# online backup API, including committed content not necessarily checkpointed.
[[ $(sqlite3 "$DB" 'PRAGMA journal_mode;') == wal ]]
BACKUP="$work/online-v6.db"
sqlite3 "$DB" ".backup '$BACKUP'"
[[ $(sqlite3 "$BACKUP" 'PRAGMA integrity_check;') == ok ]]
check_foreign_keys "$BACKUP"
[[ $(sqlite3 "$BACKUP" 'SELECT max(version) FROM schema_migrations;') == 6 ]]
[[ $(sqlite3 "$BACKUP" "SELECT count(*) FROM accounts WHERE id='recovery-account';") == 1 ]]
[[ $(sqlite3 "$BACKUP" "SELECT count(*) FROM credentials WHERE account_id='recovery-account';") == 1 ]]
[[ $(sqlite3 "$BACKUP" "SELECT count(*) FROM key_policies WHERE id='recovery-policy';") -ge 1 ]]
[[ $(sqlite3 "$BACKUP" 'SELECT count(*) FROM virtual_keys;') == 2 ]]
[[ $(sqlite3 "$BACKUP" 'SELECT count(*) FROM virtual_keys WHERE revoked=1;') == 1 ]]
[[ $(sqlite3 "$BACKUP" 'SELECT count(*) FROM usage_records;') -ge 1 ]]

# Distinct-path restore; admin checks preserve metadata, and the protected
# request below proves the original key decrypts the encrypted credential.
RESTORED="$work/restored.db"
restore_database "$BACKUP" "$RESTORED" 6
[[ "$RESTORED" != "$DB" && "$RESTORED" != "$BACKUP" ]]
[[ $(sqlite3 "$RESTORED" 'SELECT count(*) FROM usage_records;') == $(sqlite3 "$DB" 'SELECT count(*) FROM usage_records;') ]]
# Failed restore, corrupt source, and repeated restore attempts must never
# overwrite a valid destination or leave a partial file at a fresh destination.
RESTORED_HASH=$(sha256sum "$RESTORED" | awk '{print $1}')
CORRUPT_BACKUP="$work/corrupt.db"
printf 'not a sqlite database\n' >"$CORRUPT_BACKUP"
if restore_database "$CORRUPT_BACKUP" "$RESTORED" 6; then
  echo "verify-recovery: corrupt restore unexpectedly replaced existing DB" >&2; exit 1
fi
if restore_database "$BACKUP" "$RESTORED" 6; then
  echo "verify-recovery: repeated restore unexpectedly replaced existing DB" >&2; exit 1
fi
CORRUPT_RESTORE="$work/corrupt-restore.db"
if restore_database "$CORRUPT_BACKUP" "$CORRUPT_RESTORE" 6; then
  echo "verify-recovery: corrupt backup unexpectedly restored" >&2; exit 1
fi
[[ ! -e "$CORRUPT_RESTORE" ]]
# A malformed FK declaration can pass integrity_check while foreign_key_check
# itself errors without emitting rows. It must not be mistaken for an empty,
# valid result or promoted as a restore.
MALFORMED_FK="$work/malformed-fk.db"
sqlite3 "$MALFORMED_FK" <<'SQL'
PRAGMA foreign_keys=OFF;
CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
INSERT INTO schema_migrations VALUES (1, '2026-10-17T00:00:00Z');
CREATE TABLE parent(id TEXT);
CREATE TABLE child(parent_id TEXT REFERENCES parent(id));
INSERT INTO child VALUES ('orphan');
SQL
[[ $(sqlite3 "$MALFORMED_FK" 'PRAGMA integrity_check;') == ok ]]
if malformed_fk_output=$(sqlite3 "$MALFORMED_FK" 'PRAGMA foreign_key_check;' 2>"$work/malformed-fk-check.log"); then
  echo "verify-recovery: malformed foreign-key check unexpectedly succeeded" >&2; exit 1
fi
[[ -z "$malformed_fk_output" ]]
MALFORMED_FK_RESTORE="$work/malformed-fk-restore.db"
if restore_database "$MALFORMED_FK" "$MALFORMED_FK_RESTORE" 1; then
  echo "verify-recovery: malformed foreign-key schema unexpectedly restored" >&2; exit 1
fi
[[ ! -e "$MALFORMED_FK_RESTORE" ]]
[[ $(sha256sum "$RESTORED" | awk '{print $1}') == "$RESTORED_HASH" ]]
"$binary" admin --db "$RESTORED" --master-key "$KEY" status >/dev/null
"$binary" admin --db "$RESTORED" --master-key "$KEY" account get recovery-account >/dev/null
"$binary" admin --db "$RESTORED" --master-key "$KEY" policy get recovery-policy >/dev/null
"$binary" admin --db "$RESTORED" --master-key "$KEY" key list >/dev/null
"$binary" admin --db "$RESTORED" --master-key "$KEY" usage summary >/dev/null
kill -TERM "$gateway_pid"; wait "$gateway_pid"; gateway_pid=
RESTORED_CONFIG="$work/restored.yaml"
write_config "$RESTORED_CONFIG" "$RESTORED" "$KEY"
start_gateway "$RESTORED_CONFIG" "$work/restored.log"
request "$key_secret" restored
requests_before=$(wc -l <"$work/upstream-requests")
revoked_status=$(curl -sS -o "$work/revoked-key-response.json" -w '%{http_code}' \
  "http://127.0.0.1:$gateway_port/v1/responses" \
  -H "Authorization: Bearer $revoked_secret" -H 'Content-Type: application/json' \
  --data-binary '{"model":"recovery-model","input":"revoked-key-check"}')
[[ "$revoked_status" != 200 ]]
grep -q '"error"' "$work/revoked-key-response.json"
[[ $(wc -l <"$work/upstream-requests") == "$requests_before" ]]

# Missing keys fail file validation; wrong valid keys fail when encrypted credentials are used.
for candidate in "$work/absent.key" "$work/malformed.key"; do
  label=$(basename "$candidate")
  if "$binary" admin --db "$RESTORED" --master-key "$candidate" status >"$work/$label.admin" 2>&1; then
    echo "verify-recovery: admin unexpectedly accepted $label" >&2; exit 1
  fi
  grep -q 'admin: invalid master key file' "$work/$label.admin"
  ! grep -q 'recovery-fake-credential' "$work/$label.admin"
  bad_config="$work/$label.yaml"
  write_config "$bad_config" "$RESTORED" "$candidate"
  if PESTIROUTE_RECOVERY_UPSTREAM=recovery-fake-credential "$binary" -config-format yaml -config "$bad_config" >"$work/$label.gateway" 2>&1; then
    echo "verify-recovery: gateway unexpectedly accepted $label" >&2; exit 1
  fi
  ! grep -q 'recovery-fake-credential' "$work/$label.gateway"
done
grep -q 'master key' "$work/absent.key.gateway"
grep -q 'master key' "$work/malformed.key.gateway"

# A syntactically valid but incorrect 32-byte key passes startup's file checks;
# protected credential access must still fail closed without reaching upstream.
kill -TERM "$gateway_pid"; wait "$gateway_pid"; gateway_pid=
WRONG_CONFIG="$work/wrong-key.yaml"
write_config "$WRONG_CONFIG" "$RESTORED" "$work/wrong.key"
start_gateway "$WRONG_CONFIG" "$work/wrong-key.log"
requests_before=$(wc -l <"$work/upstream-requests")
wrong_status=$(curl -sS -o "$work/wrong-key-response.json" -w '%{http_code}' \
  "http://127.0.0.1:$gateway_port/v1/responses" \
  -H "Authorization: Bearer $key_secret" -H 'Content-Type: application/json' \
  --data-binary '{"model":"recovery-model","input":"recovery-check"}')
[[ "$wrong_status" != 200 ]]
grep -q '"error"' "$work/wrong-key-response.json"
! grep -q 'recovery-fake-credential\|wrong-key-material' "$work/wrong-key-response.json"
[[ $(wc -l <"$work/upstream-requests") == "$requests_before" ]]
kill -TERM "$gateway_pid"; wait "$gateway_pid"; gateway_pid=

# The historical v5 binary must reject the actual migrated v6 database.
if "$legacy_binary" admin --db "$DB" --master-key "$KEY" status >"$work/legacy-newer-status.log" 2>&1; then
  echo "verify-recovery: v5 binary accepted actual v6 database" >&2; exit 1
fi
grep -q 'unsupported or unreadable schema' "$work/legacy-newer-status.log"

# Roll back by restoring the untouched, genuine v5 backup to a fresh path and
# proving the compatible historical binary can read its preserved state.
ROLLBACK="$work/rollback.db"
restore_database "$V5_BACKUP" "$ROLLBACK" 5
[[ $(sqlite3 "$ROLLBACK" 'PRAGMA integrity_check;') == ok ]]
check_foreign_keys "$ROLLBACK"
[[ $(sqlite3 "$ROLLBACK" 'SELECT count(*) FROM usage_records;') == "$V5_USAGE" ]]
"$legacy_binary" admin --db "$ROLLBACK" --master-key "$KEY" status | grep -q 'schema version 5'
"$legacy_binary" admin --db "$ROLLBACK" --master-key "$KEY" account get recovery-account >/dev/null
"$legacy_binary" admin --db "$ROLLBACK" --master-key "$KEY" policy get recovery-policy >/dev/null
"$legacy_binary" admin --db "$ROLLBACK" --master-key "$KEY" key get "$revoked_id" | grep -q 'revoked=true'
ROLLBACK_CONFIG="$work/rollback-v5.yaml"
write_config "$ROLLBACK_CONFIG" "$ROLLBACK" "$KEY"
start_gateway_with "$legacy_binary" "$ROLLBACK_CONFIG" "$work/rollback-v5.log"
curl -fsS "http://127.0.0.1:$gateway_port/readyz" >/dev/null
request "$key_secret" rollback-v5
[[ $(sqlite3 "$ROLLBACK" 'SELECT count(*) FROM usage_records;') -gt "$V5_USAGE" ]]
kill -TERM "$gateway_pid"; wait "$gateway_pid"; gateway_pid=

if grep -E 'recovery-fake-credential|wrong-key-material' "$work"/*.log; then
  echo "verify-recovery: credential material found in logs" >&2; exit 1
fi
echo "verify-recovery: PASS (online v6 backup and fresh safe restore, no-overwrite/corrupt restore failures, v5 migration and post-upgrade state, restored key revocation, wrong-key delivery denial, historical-v5 rejection and rollback)"
