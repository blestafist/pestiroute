# PestiRoute

PestiRoute is a planned self-hosted AI protocol gateway. This repository currently contains only a Go scaffold, not a working server or inference gateway.

## Build

Install Go 1.27.1. From a clean checkout, build all packages without downloading dependencies or writing a binary into the repository:

```sh
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build -o /dev/null ./...
```

The scaffold has no application dependencies. The plain `go build ./...` check also succeeds, but Go writes a `gateway` binary to the current directory when the only package is a command; `-o /dev/null` avoids that artifact. Running `GOTOOLCHAIN=local go run ./cmd/gateway` prints `gateway: scaffold only; no configured service is running (startup planned in FND-002)` to stderr and exits with status 1. It does not start a listener.
