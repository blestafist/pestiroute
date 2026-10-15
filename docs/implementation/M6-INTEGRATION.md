# Minimal external Connector integration

M6 runs a trusted Linux child behind the existing `core.Connector` interface.
The [standalone example](../../examples/external-connector/main.go) is a separate
composition root wrapping the real native Responses implementation. It does
not duplicate transport, routing, credential storage or accounting.

## Build and verify

Use a clean checkout of the accepted `dev-m6` revision, Linux, Go 1.27.1 and
Python 3. Fetch pinned dependencies once with `go mod download`, then:

```sh
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off \
  go build -o /tmp/pestiroute-example-child ./examples/external-connector
python3 scripts/smoke-m6-offline.py --child-package ./examples/external-connector
```

The smoke builds the example independently and launches it through the actual
protected gateway/supervisor, then checks JSON/SSE, scoped credentials/HTTP,
crash recovery without active replay, settlement and cleanup. See
[LOCAL-M6](LOCAL-M6.md) for exact assertions and limits. Do not start the binary
directly: the host must first create and inject its paired private sockets.

The Go helper and Core types live under `internal/`; Go intentionally prevents
importing them from an unrelated module. Copy this example **inside this module**
or maintain an in-tree extension. There is no published Go SDK or stable public
Go import surface. A separately implemented child can use the checked-in
[protobuf schema](../../internal/ipc/proto/connector.proto), but must implement
the entire [wire/process binding](M6-BINDING.md), including flow control and
reverse services; a unary Execute stub is not interoperable.

## Versions and operations

| Layer | Required value / ownership |
| --- | --- |
| IPC protocol | `pestiroute.connector.v1` |
| IPC version | Major 1, minor at least 1 (epoch broker/control lane required) |
| Connector API | Exact advertised host pair 1.0; IPC minor is independent |
| Native protocol | `openai.responses.v1`, native mode |
| Runtime pins | gRPC-Go 1.75.1, protobuf 1.36.6 |
| Generation pins | protoc 33.0, protoc-gen-go 1.36.6, protoc-gen-go-grpc 1.5.1; regeneration command in binding |

The host verifies the launched peer and Handshake/Descriptor before Init.
Implement `Descriptor`, `Init`, `Health`, `Capabilities`, idempotent `Close`,
`Execute`, `Models`, `EstimateUsage`, and `Authenticate` through `core.Connector`;
unsupported operations return explicit supported/known flags. Do not turn an
unknown capability into supported simply because it crosses a process boundary.

| Go child helper operation | Services and supported scope |
| --- | --- |
| Execute | Invocation-scoped credential lookup, streaming HTTP and sanitized logging; Head → Body* → Complete → EOF |
| Authenticate | Invocation-scoped credential/HTTP/log services and opaque state/credential results; native Responses explicitly returns unsupported, synthetic fixtures verify richer auth |
| Models / EstimateUsage | Typed result/body bridging is present; the current generic child invokes these methods with empty InvocationServices. Pure/local implementations work; service-dependent model lookup or estimation needs additional scoped child wiring before it can be claimed |
| Lifecycle / capability queries | No invocation services; Descriptor available before Init; fixed context/deadline and close behavior |

The native example's Models/EstimateUsage/Authenticate behavior is inherited
from the existing implementation; it is not evidence of provider inference or
live authentication. The gateway currently has no production Models route.

## Host configuration and deployment boundary

Under a protected native Responses connector, set:

```yaml
process:
  executable: /absolute/path/to/pestiroute-example-child
  args: []
  working_directory: /absolute/existing/directory
  environment_allowlist: []
```

This opt-in path currently validates `pestiroute.responses.native`; the example
keeps that Descriptor. A different Connector identity also needs explicit
composition/config registration; arbitrary plugin discovery is out of scope.
Arguments are literal and the host does not search PATH. Only
`PESTIROUTE_CONNECTOR_SOCKET` and `PESTIROUTE_HOST_SERVICES_SOCKET` enter the child
environment. Keep credentials, gateway config, SQLite paths/master keys and
auth persistence on the host. Services expire with the host-issued invocation
and process epoch. The child must not retain them or select another account.

The helper implements fragmentation, 64 KiB body chunks, the 84 KiB wire cap,
four-call admission, and host-owned aggregate credits (4 MiB/64 frames per
invocation, 16 MiB/256 frames per process). Incremental response/callback bodies
and the separate urgent cancel/close lane are required. A lost transport leaves
delivery uncertain; never infer safe replay from a missing Head. Restart serves
new work only. Follow the binding for finite shutdown, group reaping and owned
socket removal.

This is crash isolation for a trusted same-user executable, not a sandbox for
hostile code. There is no remote hosting, hot reload, download marketplace,
external Adapter ABI or cross-platform evidence in M6.
