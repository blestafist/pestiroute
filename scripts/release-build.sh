#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out=${1:-"$root/bin/gateway"}
mkdir -p "$(dirname "$out")"

tmp=$(mktemp "${TMPDIR:-/tmp}/pestiroute-build.XXXXXX")
trap 'rm -f "$tmp"' EXIT

cd "$root"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTOOLCHAIN=go1.27.1 \
  go build -trimpath -buildvcs=false -ldflags='-buildid=' -o "$tmp" ./cmd/gateway
install -m 0755 "$tmp" "$out"
