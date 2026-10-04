#!/bin/sh
set -eu

cd "$(dirname "$0")/.."
umask 077
keyfile=/tmp/opencode/anthropic-api-key
urlfile=/tmp/opencode/anthropic-base-url
artifacts=$(mktemp -d /tmp/opencode/m4-040-live.XXXXXX)
success=0
cleanup() {
	rm -f -- "$keyfile" "$urlfile"
	if [ "$success" -ne 1 ]; then
		rm -rf -- "$artifacts"
	fi
	if { [ "$success" -ne 1 ] && { [ -e "$artifacts" ] || [ -L "$artifacts" ]; }; } || [ -e "$keyfile" ] || [ -L "$keyfile" ] || [ -e "$urlfile" ] || [ -L "$urlfile" ]; then
		echo 'CLEANUP_FAIL' >&2
		return 1
	fi
	echo 'CLEANUP_PASS'
	if [ "$success" -eq 1 ]; then
		echo "SANITIZED_ARTIFACTS=$artifacts"
	fi
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

PESTIROUTE_M4_COMPAT_LIVE=1 \
PESTIROUTE_SMOKE_M4_ARTIFACT_DIR="$artifacts" \
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off \
	go test -count=1 -timeout=75s ./cmd/gateway -run '^TestM4CompatibleLiveProbe$'
python3 scripts/smoke-m4-artifacts.py "$artifacts/direct.json" "$artifacts/gateway.json"
success=1
echo 'Compatible endpoint probe and artifact validation passed.'
