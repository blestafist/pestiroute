#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
with_ui=false
if [[ ${1:-} == --with-ui ]]; then
  with_ui=true
  shift
fi
if (($# > 1)); then
  echo "usage: $0 [--with-ui] [output-path]" >&2
  exit 2
fi
out=${1:-"$root/bin/gateway"}
mkdir -p "$(dirname "$out")"

tmp=$(mktemp "${TMPDIR:-/tmp}/pestiroute-build.XXXXXX")
bundle="$root/internal/ui/assets"
backup=
assets_backed_up=false
cleanup() {
  if $assets_backed_up; then
    rm -rf "$bundle"
    mv "$backup/assets" "$bundle"
  fi
  [[ -z "$backup" ]] || rm -rf "$backup"
  rm -f "$tmp"
}
trap cleanup EXIT

cd "$root"
if $with_ui; then
  [[ $(node --version) == v22.16.0 && $(npm --version) == 10.9.2 ]] || {
    echo "UI packaging requires Node.js v22.16.0 and npm 10.9.2" >&2
    exit 1
  }
  npm --prefix web ci
  npm --prefix web run build
  backup=$(mktemp -d "${TMPDIR:-/tmp}/pestiroute-assets.XXXXXX")
  cp -R "$bundle" "$backup/assets"
  assets_backed_up=true
  rm -rf "$bundle"
  mkdir -p "$bundle"
  cp -R web/dist/. "$bundle/"
  test -s "$bundle/index.html"
fi
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTOOLCHAIN=go1.27.1 \
  go build -trimpath -buildvcs=false -ldflags='-buildid=' -o "$tmp" ./cmd/gateway
install -m 0755 "$tmp" "$out"
