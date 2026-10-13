# Disposable gRPC spike

This package is test-only and is not a production transport binding. The schema
and generated stubs are pinned to `protoc` 33.0, `protoc-gen-go` v1.36.6,
`protoc-gen-go-grpc` v1.5.1, gRPC-Go v1.75.1, and protobuf v1.36.6.

Regenerate from the repository root (install the two Go plugins at the exact
versions above and put them on `PATH`; use protoc 33.0):

```sh
protoc -I internal/conformance/grpc_spike \
  --go_out=internal/conformance/grpc_spike --go_opt=paths=source_relative \
  --go-grpc_out=internal/conformance/grpc_spike --go-grpc_opt=paths=source_relative \
  internal/conformance/grpc_spike/m6.proto
```
