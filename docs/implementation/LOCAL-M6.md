# Local M6 external-process operations

Run from a clean checkout on Linux with Go 1.27.1 and Python 3. Download the
pinned Go modules once with `go mod download`; subsequent commands disable
module/network resolution. No provider, model, GPU or operator credential is
required. The procedure binds only numeric loopback addresses and private Unix
sockets, creates a temporary SQLite database/master key and synthetic secrets,
and removes its files on exit.

```sh
python3 scripts/smoke-m6-offline.py
```

The command builds `cmd/gateway` and `cmd/responses-connector-child` separately,
provisions a protected account, encrypted credential, policy and virtual key
through the real admin CLI, writes strict YAML with an explicit `process`
tuple, then launches the gateway executable. It verifies:

- Startup `/readyz`, a real child PID/process group, and an environment containing
  only the two host-owned socket paths.
- Byte-preserved JSON requests/responses and delivery of the first SSE event
  while the loopback upstream holds its completion gate.
- SIGKILL of a child after the upstream receives an active request. That request
  must fail; the gateway remains alive and `/healthz` responds. A replacement
  child serves a new request; exactly four upstream deliveries prove the crashed
  request was not replayed.
- SQLite attempts settle without held reservations. SIGTERM stops the gateway
  within 38 seconds; every observed child group and owned IPC directory disappears.
  Captured logs contain neither the virtual key nor the synthetic upstream secret.

Expected final output starts with `PASS: build/start`. Any failed assertion or
deadline exits nonzero. The script makes bounded **new** requests on HTTP 503 or pre-dispatch HTTP 400 `unsupported_target`
while waiting for recovery; it never retries the crashed invocation. Gateway
`/readyz` currently reports the startup/accounting flag, not aggregate live
connector health. Dispatch separately checks the connector's Health; a ready
gateway probe alone is not evidence that a replacement connector is admitted.

## Launch configuration

The generated YAML is an executable reference. For retained deployments, use the
same `connectors[].process` fields: absolute `executable`, literal `args`, absolute
existing `working_directory`, and empty `environment_allowlist`. Omit `process`
to keep the in-process implementation. Protected startup currently permits the
native Responses implementation in this external path; it is not arbitrary
executable discovery. See [integration](M6-INTEGRATION.md) and the normative
[binding](M6-BINDING.md#ownership-topology-versions).

The native YAML profile still requires `settings.credential_env` to name a
nonempty variable in the **gateway** environment. In protected mode the same
name selects the credential ID stored encrypted in SQLite; do not confuse it
with a child environment variable. The procedure uses a synthetic value and
creates that matching stored credential. No parent variables are inherited by
the child. Persistent backups must retain the matching external master key;
follow [M3 backup/restore](LOCAL-M3.md), not a live WAL/main-file copy.

## Failure and shutdown limits

The host owns socket names, invocation authority, credential persistence and
restart policy. Startup/handshake and Init each have their binding deadlines.
Shutdown stops admission, drains for at most 30 seconds, then cancels/revokes and
terminates the owned process group within the additional 5-second cleanup
window. Unexpected death permits at most three restarts in 60 seconds with
100 ms–2 s backoff; recovery serves future work only. A remaining process group
is a cleanup failure. The 38-second smoke wait includes gateway scheduling
margin; it does not change these bounds.

For targeted fault, backpressure and version checks beyond the CLI procedure:

```sh
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOFLAGS=-p=1 \
  go test -race -count=1 ./internal/connector/external \
  -run '^TestM6(Handshake|Backpressure|Supervisor|Shutdown)'
```

This evidence is Linux-only crash containment for trusted same-user binaries,
not an OS sandbox or a portability claim. Older live-provider gates remain in
[CURRENT](CURRENT.md#open-gates-carried-forward) and are outside this procedure.
