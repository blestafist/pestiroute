#!/bin/sh
set -eu
set +x

usage() {
  echo 'usage: PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT=/persistent/path GATEWAY_BIN=/path/to/gateway scripts/antigravity-auth.sh [--debug] start | continue SESSION_ID (PESTIROUTE_ANTIGRAVITY_ALLOW_ONBOARD=1 opts in)' >&2
  exit 2
}

[ "$#" -ge 1 ] || usage
debug_flag=
onboard_flag=
if [ "${PESTIROUTE_ANTIGRAVITY_ALLOW_ONBOARD:-}" = 1 ]; then
  onboard_flag=--allow-onboard
fi
if [ "${1:-}" = --debug ]; then
  debug_flag=--debug
  shift
fi
action=${1:-}
[ -n "$action" ] || usage
shift
if [ "${PESTIROUTE_AUTH_DEBUG:-}" = 1 ]; then
  debug_flag=--debug
fi
root=${PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT:?set PESTIROUTE_ANTIGRAVITY_OPERATOR_ROOT to an existing persistent operator directory}
gateway=${GATEWAY_BIN:?set GATEWAY_BIN to the standalone gateway binary}
[ -d "$root" ] || { echo 'operator root must already exist' >&2; exit 2; }
[ -f "$root/gateway.db" ] || { echo 'operator database not found' >&2; exit 2; }
[ -f "$root/master.key" ] || { echo 'matching operator master key not found' >&2; exit 2; }
[ -x "$gateway" ] || { echo 'gateway binary must be executable' >&2; exit 2; }

case "$action" in
  start)
    [ "$#" -eq 0 ] || usage
    exec "$gateway" admin --db "$root/gateway.db" --master-key "$root/master.key" auth ${debug_flag:+'--debug'} start --account antigravity-local
    ;;
  continue)
    [ "$#" -eq 1 ] || usage
    if [ -z "${PESTIROUTE_ANTIGRAVITY_CLIENT_SECRET:-}" ] && [ -z "${PESTIROUTE_ANTIGRAVITY_CLIENT_ID:-}" ]; then
      echo 'runtime client secret is required for the default emulated client; set a reviewed public-client ID override when applicable' >&2
      exit 2
    fi
    [ "${PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT:-}" = 1 ] || { echo 'set PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT=1 only after separate operator authorization' >&2; exit 2; }
    if [ -t 0 ]; then
      printf 'Paste the OAuth callback URL or authorization code: ' >&2
      tty_settings=
      tty_echo_disabled=1
      restore_tty() {
        if [ "$tty_echo_disabled" -eq 1 ] && [ -n "$tty_settings" ]; then
          stty "$tty_settings" <&0 2>/dev/null || :
          tty_echo_disabled=0
        fi
      }
      trap 'restore_tty' 0
      trap 'exit 129' HUP
      trap 'exit 130' INT
      trap 'exit 143' TERM
      tty_settings=$(stty -g <&0) || { tty_echo_disabled=0; echo 'cannot read terminal settings' >&2; exit 2; }
      stty -echo <&0 2>/dev/null || { echo 'cannot disable terminal echo' >&2; exit 2; }
      if ! IFS= read -r callback <&0; then
        restore_tty
        echo >&2
        echo 'callback input is required' >&2
        exit 2
      fi
      restore_tty
      echo >&2
      case "$callback" in
        *[![:space:]]*) ;;
        *) echo 'callback input is required' >&2; exit 2 ;;
      esac
      printf '%s\n' "$callback" | "$gateway" admin --db "$root/gateway.db" --master-key "$root/master.key" auth ${debug_flag:+'--debug'} continue --session "$1" --callback-stdin ${onboard_flag:+'--allow-onboard'}
    else
      "$gateway" admin --db "$root/gateway.db" --master-key "$root/master.key" auth ${debug_flag:+'--debug'} continue --session "$1" --callback-stdin ${onboard_flag:+'--allow-onboard'}
    fi
    ;;
  *) usage ;;
esac
