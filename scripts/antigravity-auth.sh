#!/bin/sh
set -eu
set +x

usage() {
  echo 'usage: PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT=/persistent/path GATEWAY_BIN=/path/to/gateway scripts/antigravity-auth.sh start|continue SESSION_ID' >&2
  exit 2
}

[ "$#" -ge 1 ] || usage
action=$1
shift
root=${PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT:?set PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT to an existing persistent operator directory}
gateway=${GATEWAY_BIN:?set GATEWAY_BIN to the standalone gateway binary}
[ -d "$root" ] || { echo 'operator root must already exist' >&2; exit 2; }
[ -f "$root/gateway.db" ] || { echo 'operator database not found' >&2; exit 2; }
[ -f "$root/master.key" ] || { echo 'matching operator master key not found' >&2; exit 2; }
[ -x "$gateway" ] || { echo 'gateway binary must be executable' >&2; exit 2; }

case "$action" in
  start)
    [ "$#" -eq 0 ] || usage
    exec "$gateway" admin --db "$root/gateway.db" --master-key "$root/master.key" auth start --account antigravity-local
    ;;
  continue)
    [ "$#" -eq 1 ] || usage
    if [ -z "${PESTIROUTE_ANTIGRAVITY_CLIENT_SECRET:-}" ] && [ -z "${PESTIROUTE_ANTIGRAVITY_CLIENT_ID:-}" ]; then
      echo 'runtime client secret is required for the default emulated client; set a reviewed public-client ID override when applicable' >&2
      exit 2
    fi
    [ "${PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT:-}" = 1 ] || { echo 'set PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT=1 only after separate operator authorization' >&2; exit 2; }
    exec "$gateway" admin --db "$root/gateway.db" --master-key "$root/master.key" auth continue --session "$1" --callback-stdin
    ;;
  *) usage ;;
esac
