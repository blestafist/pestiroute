# Technology Stack

## Core Choices

The proposed stack is designed for a single self-hosted process with simple deployment and predictable streaming. The primary language is **Go**: it's well-suited for HTTP proxying, concurrent requests, cancellation via `context.Context`, and building compact binaries. The supported version is pinned in `go.mod` and CI during project setup, avoiding reliance on a floating `latest`.

| Area | Proposed Solution | Rationale |
| --- | --- | --- |
| HTTP server and client | `net/http`, `http.Transport` | Managed connections, cancellation, and streaming without a large framework |
| API routing | `http.ServeMux` | Standard library is sufficient for the initial set of endpoints |
| JSON | `encoding/json` at API boundaries and within connectors | Core passes raw bytes without re-serializing requests |
| Configuration | YAML via `go.yaml.in/yaml/v3` | Human-readable configuration with explicit schema and unknown key validation |
| Storage | SQLite via `database/sql` and `modernc.org/sqlite` v1.60.1 | File-backed integration spike passed on Linux/amd64 with Go 1.27.1 and CGO disabled; startup configures WAL, `synchronous=FULL`, foreign keys, and a 500 ms busy timeout |
| SQL | Explicit queries and versioned SQL migrations | Small schema, transparent transactions; no need for ORM yet |
| Logging | `log/slog` | Structured events without an additional logging framework |
| Metrics | Prometheus client | Counters, gauges, and latency histograms for operational monitoring |
| Testing | `testing`, `httptest`, Go fuzzing, and race detector | Most protocol validation runs locally |
| Packaging | Go binary and container | Convenient deployment on servers or local machines |
| External connectors | Tentatively gRPC + Protobuf over Unix socket | Versioning, streaming, and cancellation through typed transport |

These are initial preferences. The SQLite driver will be validated with a small spike on migrations, concurrent writes, and supported platforms. The IPC choice is finalized only after comparing alternatives on a real streaming scenario in M6.

## HTTP and Streaming

The [M1 native binding and startup note](M1-BINDING.md) fixes the initial single-target JSON subset. [M2-BINDING.md](M2-BINDING.md) defines the contract-preserving component, scoped-service, route, and stream-validation binding; the broader stack choices below are not an implementation checklist.

The implemented gateway startup format is M2 strict JSON with `components` and `routes`; the [proposed YAML](CONFIGURATION.md#proposed-yaml) and SQLite choices remain future M3 design, not available startup features. The [local M2 procedure](LOCAL-M2.md) demonstrates offline build, loopback multi-route dispatch, and legacy single-target compatibility. M2 conformance and gateway regressions can be run offline with `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -count=15 ./internal/conformance/...` and `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -count=1 ./cmd/gateway/...`; `./scripts/check.sh` runs repository-wide checks.

Each connector instance has a reusable HTTP client with connection pooling. Timeouts are separated into connection establishment, TLS handshake, response header reception, and idle time for active streams. A single short global `Client.Timeout` is unsuitable for long agent responses.

Transport and SDK defaults must preserve the [one-attempt and replay rules](CONTRACT.md#connector-contract). Inference clients must disable automatic redirect following and SDK retries; audit `http.Transport` replay behavior, including replayable bodies and idempotency headers, so one Execute cannot silently resend inference. A redirect or transient transport error is classified by the Connector under the existing retry rules, not automatically followed. Scoped credentials must not travel to an unselected redirect target.

Native forwarding must not automatically decompress or recompress bodies. Configure compression behavior explicitly (including Go transport's automatic gzip handling), and keep content encoding and length headers consistent with the bytes actually forwarded. Header filtering excludes gateway credentials and hop-by-hop headers, including fields named by `Connection`; blindly copying headers is not passthrough.

On the server side, constraints are set for request body size, header read time, and graceful shutdown. The full body of incoming JSON can be read into a bounded buffer since the contract uses `[]byte`. Response buffering is prohibited for the outgoing stream: responses are forwarded to the client as they arrive.

The SSE parser is located in the connector or its protocol helper. It must correctly handle UTF-8 and JSON splitting across network chunks, multiline events, and events exceeding the standard `bufio.Scanner` limit. Core works with bytes and does not parse SSE.

## Storage and Secrets

SQLite stores accounts, encrypted credentials, virtual keys, usage data, and the migration journal. The pinned pure-Go driver is `modernc.org/sqlite` v1.60.1. `internal/storage/sqlite.Open` configures WAL, `synchronous=FULL`, foreign keys, and a 500 ms busy timeout via connection DSN so pooled connections receive connection-local settings. The file-backed spike passed on Linux/amd64, Go 1.27.1 with `CGO_ENABLED=0`; other deployment platforms remain unverified. A context cancelled during SQLite's busy-handler wait is observed after that bounded wait (up to 500 ms), not immediately. WAL mode, busy timeout, and short transactions reduce contention; network calls are never made inside SQL transactions. For a single-process setup, a serialized writer is acceptable if load tests confirm sufficient throughput.

Credentials are encrypted using standard library primitives, such as AES-GCM with a unique nonce and AAD that binds the ciphertext to the credential ID and format version. For the proposed M3 protected startup, the master key comes from a permission-checked external file and is not stored in the database; see the key policy below. Virtual keys are generated from cryptographically random bytes, displayed upon creation, and stored only as a digest with a separate public identifier.

The M3-specific schema, writer/repository boundary, acknowledged intent and terminal writes, conservative recovery, key-file policy, and crash traces are fixed in [M3-STORAGE.md](M3-STORAGE.md). That design narrows the initial master-key source to a protected external file; it does not add implementation support or change the M2 startup path.

## Observability

Each request and attempt is assigned an ID. Logs should include route, connector instance, account ID, execution mode, status, latency, time to first byte, fallback reason, and final usage. Prompt bodies, responses, and secrets are not logged by default.

Metrics track request count, active streams, errors, execution time, first-byte latency, limit rejections, and connector failures. Request IDs, arbitrary model names, and account IDs are not used as unbounded metric labels. Tracing via OpenTelemetry can be added once a stable request lifecycle emerges.

## Build and Workflow

The project starts with a single Go module and one main binary `gateway`. Minimal CI runs `gofmt`, `go vet ./...`, `go test ./...`, `go test -race ./...`, and build checks. Protocol fixtures do not require network credentials. Real upstream smoke tests run separately and have explicitly limited request volumes.

The container runs as an unprivileged user, receives configuration and persistent data directory via mounts, and includes CA certificates for upstream TLS. SQLite backups are performed using consistent snapshot/backup methods; simple file copying of an active WAL database is not a proper procedure.
