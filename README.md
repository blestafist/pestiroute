# PestiRoute

PestiRoute is a planned self-hosted AI protocol gateway. The current server exposes only health and readiness probes; it does not proxy inference requests.

## Build

Install Go 1.27.1. From a clean checkout, build all packages without downloading dependencies or writing a binary into the repository:

```sh
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build -o /dev/null ./...
```

The server has no application dependencies. Plain `go build ./...` writes a `gateway` binary to the current directory; `-o /dev/null` avoids that artifact.

## Run

```sh
GOTOOLCHAIN=local go run ./cmd/gateway -listen 127.0.0.1:8080 -shutdown-timeout 5s
curl -f http://127.0.0.1:8080/healthz
curl -f http://127.0.0.1:8080/readyz
```

Both probes return `200 OK` while serving. Press Ctrl-C (SIGINT) or send SIGTERM to stop; shutdown waits up to the configured timeout for active requests and closes the listener. The default listener is `127.0.0.1:8080` and default shutdown timeout is `5s`. Alternatively pass `-config path/to/config.json` containing a JSON object such as `{"listen":"127.0.0.1:8080","shutdown_timeout":"5s"}`; explicit flags override file values. Invalid file paths, syntax, addresses and timeouts fail before serving with a diagnostic on stderr and a non-zero exit. The M3 YAML topology schema is not yet supported.
