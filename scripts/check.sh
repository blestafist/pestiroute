#!/usr/bin/env bash
set -euo pipefail

export GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off

version=$(go env GOVERSION)
case "$version" in
  go1.27.1|go1.27.1-*) ;;
  *) echo "check: Go 1.27.1 required (found $version)" >&2; exit 1 ;;
esac

echo 'check: patch whitespace' >&2
git diff --check >&2

echo 'check: formatting (cmd, internal)' >&2
unformatted=$(gofmt -l cmd internal)
if [ -n "$unformatted" ]; then
  printf 'check: gofmt required for:\n%s\n' "$unformatted" >&2
  exit 1
fi

echo 'check: vet' >&2
go vet ./...

echo 'check: unit tests' >&2
go test ./...

echo 'check: race tests' >&2
go test -race ./...

echo 'check: offline build' >&2
go build -o /dev/null ./...
