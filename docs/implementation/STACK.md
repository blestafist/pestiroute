# Technology Stack

## Core Choices

The proposed stack is designed for a single self-hosted process with simple deployment and predictable streaming. The primary language is **Go**: it's well-suited for HTTP proxying, concurrent requests, cancellation via `context.Context`, and building compact binaries. The supported version is pinned in `go.mod` and CI during project setup, avoiding reliance on a floating `latest`.

| Area | Proposed Solution | Rationale |
| --- | --- | --- |
| HTTP server and client | `net/http`, `http.Transport` | Managed connections, cancellation, and streaming without a large framework |
| API routing | `http.ServeMux` | Standard library is sufficient for the initial set of endpoints |
| JSON | `encoding/json` at API boundaries and within connectors | Core passes raw bytes without re-serializing requests |
| Configuration | YAML via `go.yaml.in/yaml/v3` | Human-readable configuration with explicit schema and unknown key validation |
| Storage | SQLite via `database/sql` and `modernc.org/sqlite` | Local storage without a separate database service and without CGO |
| SQL | Explicit queries and versioned SQL migrations | Small schema, transparent transactions; no need for ORM yet |
| Logging | `log/slog` | Structured events without an additional logging framework |
| Metrics | Prometheus client | Counters, gauges, and latency histograms for operational monitoring |
| Testing | `testing`, `httptest`, Go fuzzing, and race detector | Most protocol validation runs locally |
| Packaging | Go binary and container | Convenient deployment on servers or local machines |
| External connectors | Tentatively gRPC + Protobuf over Unix socket | Versioning, streaming, and cancellation through typed transport |

These are initial preferences. The SQLite driver will be validated with a small spike on migrations, concurrent writes, and supported platforms. The IPC choice is finalized only after comparing alternatives on a real streaming scenario in M6.

## HTTP and Streaming

The [M1 native binding and startup note](M1-BINDING.md) fixes the initial single-target JSON subset and component handoffs; the broader stack choices below are not an M1 implementation checklist.

Each connector instance has a reusable HTTP client with connection pooling. Timeouts are separated into connection establishment, TLS handshake, response header reception, and idle time for active streams. A single short global `Client.Timeout` is unsuitable for long agent responses.

Transport and SDK defaults must preserve the [one-attempt and replay rules](CONTRACT.md#connector-contract). Inference clients must disable automatic redirect following and SDK retries; audit `http.Transport` replay behavior, including replayable bodies and idempotency headers, so one Execute cannot silently resend inference. A redirect or transient transport error is classified by the Connector under the existing retry rules, not automatically followed. Scoped credentials must not travel to an unselected redirect target.

Native forwarding must not automatically decompress or recompress bodies. Configure compression behavior explicitly (including Go transport's automatic gzip handling), and keep content encoding and length headers consistent with the bytes actually forwarded. Header filtering excludes gateway credentials and hop-by-hop headers, including fields named by `Connection`; blindly copying headers is not passthrough.

On the server side, constraints are set for request body size, header read time, and graceful shutdown. The full body of incoming JSON can be read into a bounded buffer since the contract uses `[]byte`. Response buffering is prohibited for the outgoing stream: responses are forwarded to the client as they arrive.

The SSE parser is located in the connector or its protocol helper. It must correctly handle UTF-8 and JSON splitting across network chunks, multiline events, and events exceeding the standard `bufio.Scanner` limit. Core works with bytes and does not parse SSE.

## Storage and Secrets

SQLite stores accounts, encrypted credentials, virtual keys, usage data, and the migration journal. WAL mode, busy timeout, and short transactions reduce contention; network calls are never made inside SQL transactions. For a single-process setup, a serialized writer is acceptable if load tests confirm sufficient throughput.

Credentials are encrypted using standard library primitives, such as AES-GCM with a unique nonce and AAD that binds the ciphertext to the credential ID and format version. The master key comes from a separate file or environment variable and is not stored in the same database. Virtual keys are generated from cryptographically random bytes, displayed upon creation, and stored only as a digest with a separate public identifier.

## Observability

Each request and attempt is assigned an ID. Logs should include route, connector instance, account ID, execution mode, status, latency, time to first byte, fallback reason, and final usage. Prompt bodies, responses, and secrets are not logged by default.

Metrics track request count, active streams, errors, execution time, first-byte latency, limit rejections, and connector failures. Request IDs, arbitrary model names, and account IDs are not used as unbounded metric labels. Tracing via OpenTelemetry can be added once a stable request lifecycle emerges.

## Build and Workflow

The project starts with a single Go module and one main binary `gateway`. Minimal CI runs `gofmt`, `go vet ./...`, `go test ./...`, `go test -race ./...`, and build checks. Protocol fixtures do not require network credentials. Real upstream smoke tests run separately and have explicitly limited request volumes.

The container runs as an unprivileged user, receives configuration and persistent data directory via mounts, and includes CA certificates for upstream TLS. SQLite backups are performed using consistent snapshot/backup methods; simple file copying of an active WAL database is not a proper procedure.
