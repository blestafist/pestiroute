#!/bin/sh
set -eu
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/pestiroute-auth-launcher.XXXXXX")
trap 'rm -rf -- "$WORK"' EXIT HUP INT TERM
mkdir "$WORK/root"
: >"$WORK/root/gateway.db"
: >"$WORK/root/master.key"
cat >"$WORK/stub" <<'STUB'
#!/bin/sh
{
  printf 'secret=%s\n' "${PESTIROUTE_ANTIGRAVITY_CLIENT_SECRET-}"
  printf 'client_id=%s\n' "${PESTIROUTE_ANTIGRAVITY_CLIENT_ID-}"
  printf 'transport=%s\n' "${PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT-}"
  printf 'arg=<%s>\n' "$@"
} >"$CAPTURE"
printf 'called\n' >>"$CALLS"
STUB
chmod +x "$WORK/stub"

export TMPDIR=$WORK
export PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT=$WORK/root
export GATEWAY_BIN=$WORK/stub
export CAPTURE=$WORK/capture
export CALLS=$WORK/calls
export PESTIROUTE_ANTIGRAVITY_CLIENT_SECRET=synthetic-secret
export PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT=1
unset PESTIROUTE_ANTIGRAVITY_CLIENT_ID

"$SCRIPT_DIR/antigravity-auth.sh" start
grep -Fx 'secret=synthetic-secret' "$CAPTURE" >/dev/null
grep -Fx 'transport=1' "$CAPTURE" >/dev/null
grep -Fx "arg=<$WORK/root/gateway.db>" "$CAPTURE" >/dev/null
grep -Fx 'arg=<auth>' "$CAPTURE" >/dev/null
grep -Fx 'arg=<start>' "$CAPTURE" >/dev/null
! grep -F 'synthetic-secret' "$CAPTURE" | grep -F 'arg=' >/dev/null

"$SCRIPT_DIR/antigravity-auth.sh" continue safe-session
grep -Fx 'secret=synthetic-secret' "$CAPTURE" >/dev/null
grep -Fx 'transport=1' "$CAPTURE" >/dev/null
grep -Fx 'arg=<continue>' "$CAPTURE" >/dev/null
grep -Fx 'arg=<safe-session>' "$CAPTURE" >/dev/null
grep -Fx 'arg=<--callback-stdin>' "$CAPTURE" >/dev/null
grep -Fx "arg=<$WORK/root/gateway.db>" "$CAPTURE" >/dev/null
grep -Fx "arg=<$WORK/root/master.key>" "$CAPTURE" >/dev/null
! grep -F 'synthetic-secret' "$CAPTURE" | grep -F 'arg=' >/dev/null

if env -u PESTIROUTE_ANTIGRAVITY_CLIENT_SECRET "$SCRIPT_DIR/antigravity-auth.sh" continue safe-session >/dev/null 2>&1; then
  echo 'launcher unexpectedly accepted default client without its secret' >&2
  exit 1
fi
env -u PESTIROUTE_ANTIGRAVITY_CLIENT_SECRET PESTIROUTE_ANTIGRAVITY_CLIENT_ID=synthetic-public-client \
  "$SCRIPT_DIR/antigravity-auth.sh" continue public-session
grep -Fx 'secret=' "$CAPTURE" >/dev/null
grep -Fx 'client_id=synthetic-public-client' "$CAPTURE" >/dev/null
grep -Fx 'arg=<public-session>' "$CAPTURE" >/dev/null
if PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT= "$SCRIPT_DIR/antigravity-auth.sh" continue safe-session >/dev/null 2>&1; then
  echo 'launcher unexpectedly accepted disabled real transport' >&2
  exit 1
fi
before=$(wc -l <"$CALLS")
PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT=$WORK/missing "$SCRIPT_DIR/antigravity-auth.sh" start >/dev/null 2>&1 && {
  echo 'launcher unexpectedly accepted missing operator root' >&2
  exit 1
}
after=$(wc -l <"$CALLS")
[ "$before" -eq "$after" ] || { echo 'missing-root refusal invoked the auth command' >&2; exit 1; }
echo 'synthetic launcher checks passed'
