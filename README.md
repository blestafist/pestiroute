# PestiRoute

PestiRoute is a self-hosted AI protocol gateway. The current server exposes health/readiness probes and a native `POST /v1/responses` proxy when inference is configured.

## Build

Install Go 1.27.1. From a clean checkout, build all packages without downloading dependencies or writing a binary into the repository:

```sh
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build -o /dev/null ./...
```

Fetch the pinned modules once with `go mod download`. To produce a standalone executable, use `go build -o /tmp/pestiroute-gateway ./cmd/gateway`.

## Verify

With Go 1.27.1, run `GOFLAGS=-p=1 ./scripts/check.sh` from the repository root. CI runs the same script for formatting, vet, tests, race detection, and an offline artifact-free build. Package serialization avoids interference between child-process cleanup snapshots. Linux CI also runs real external-process operations and example interoperability; checks need no provider credentials.

## Run

```sh
GOTOOLCHAIN=local go run ./cmd/gateway -listen 127.0.0.1:8080 -shutdown-timeout 5s
curl -f http://127.0.0.1:8080/healthz
curl -f http://127.0.0.1:8080/readyz
```

Both probes return `200 OK` while serving. Press Ctrl-C (SIGINT) or send SIGTERM to stop; shutdown waits up to the configured timeout for active requests and closes the listener. The default listener is `127.0.0.1:8080` and default shutdown timeout is `5s`. Alternatively pass `-config path/to/config.json` containing a JSON object such as `{"listen":"127.0.0.1:8080","shutdown_timeout":"5s"}`; explicit flags override file values. Invalid file paths, syntax, addresses and timeouts fail before serving with a diagnostic on stderr and a non-zero exit. Protected YAML startup is available through `-config-format yaml -config /absolute/path/to/gateway.yaml`; provision SQLite accounts, credentials, policy and a virtual key first as described in the operations guides.

To run the configured native proxy against a local fake target with synthetic credentials, follow [the local M1 procedure](docs/implementation/LOCAL-M1.md). This is local verification, not live-provider or real-client compatibility evidence.

## Protected operations and external connectors

[CURRENT](docs/implementation/CURRENT.md) owns the actual implementation state and retained live-provider gates. Protected routing includes virtual keys, account policies, encrypted SQLite credentials and usage accounting. Existing in-process connector support and evidence ceilings remain in the linked compatibility matrices.

On Linux, [LOCAL-M6](docs/implementation/LOCAL-M6.md) builds and starts a protected gateway with the native Responses connector in a supervised external process, using only a synthetic loopback upstream. It verifies opaque JSON, incremental SSE, crash isolation without active-request replay, new-work recovery and clean shutdown. The [integration guide](docs/implementation/M6-INTEGRATION.md) contains a minimal standalone child example and its exact wire/API/platform boundaries.
