# M13 Single-Node Deployment and Recovery Binding

This binding records the current protected gateway behavior and sets acceptance
bounds for M13 packaging/recovery work. It is not evidence that a packaged
service has been built or tested. The supported initial target is one
Linux/amd64 host and one protected gateway process.

## Support boundary and prerequisites

- Linux/amd64 is the only deployment target evidenced by the SQLite file-backed
  validation (Go 1.27.1, `CGO_ENABLED=0`). This does not yet establish a
  complete packaged-service test. Other OS/architectures and network filesystems
  are not supported claims.
- Build from the pinned Go toolchain in `go.mod`; a deployed binary does not
  require Go, Node, a model runtime, or a GPU. Upstream HTTPS use requires the
  host/container CA certificates. A container recipe is a downstream artifact,
  not an existing supported installation.
- Run as a dedicated, non-root service account. Root is not required. Give that
  account read access to the protected YAML and write access only to its private
  state directory. Use a local filesystem with working SQLite locking and
  durable sync semantics; do not place the live database on a network share.
- M13's supported deployment is protected version-1 YAML
  (`-config-format yaml -config PATH`) for SQLite-backed keys, policies,
  credentials and durable accounting. It requires `storage.driver: sqlite`,
  `storage.path`, and `secrets.master_key_file` as well as valid server,
  connector, route and policy settings.
- Be precise about JSON: M2 `components`/`routes` JSON accepts the top-level
  `database_path` and `master_key_file` fields and current code can wire
  SQLite-backed virtual-key authentication, credential access and accounting
  for it. That mode does not run protected YAML's full credential-reference
  validation, startup ledger/auth-session recovery, or database process-lock
  acquisition. It is therefore not an accepted M13 deployment, despite those
  persistence features. The legacy/single-target and M2 compatibility modes
  remain numeric-loopback modes; do not present them as equivalent protected
  startup.
- The implemented gateway flags are `-config`, `-config-format`, `-listen`, and
  `-shutdown-timeout`; protected YAML supplies the protected listener and
  shutdown timeout. Do not add deployment instructions that imply a new
  daemonization, pidfile, backup, or health-check flag. `/readyz` and `/healthz`
  are HTTP endpoints, not CLI options.

## Paths, ownership and secrets

Provision explicit absolute paths in the deployment configuration, for example
`storage.path: /var/lib/pestiroute/gateway.db` and
`secrets.master_key_file: /etc/pestiroute/master.key`. These are illustrative
paths, not defaults or extra configuration keys. Keep the DB, its SQLite
`-wal`/`-shm` sidecars and lock sidecar in service-private directories. Set the
state directory to mode `0700`, database and generated sidecars to `0600`, and
make them service-account-owned. Use a restrictive umask (`077`) when
provisioning. The gateway code does not itself enforce DB/sidecar mode bits, so
packaging must verify them rather than claim automatic permission hardening.

The master key is provisioned outside the DB/config tree as an owned regular
file, exactly 32 bytes, mode `0600`; symlinks and broader permissions are
rejected. Never generate a replacement key on startup or put it in the DB,
YAML, environment, logs, or backup. Retain the matching external key securely
for the full lifetime of its encrypted records. Losing it makes those records
unavailable; key rotation is not implemented. Restrict config and backup access
as they can reveal deployment metadata or persistent account/key/usage data.

Admin commands use explicit `gateway admin --db PATH --master-key PATH ...`
arguments and run as the same account with access to those paths. CLI mutations
can run alongside the gateway through SQLite's cross-process writer
coordination; migration and restore are stopped-service operations. Treat
one-time virtual-key output and credential input as secret material; do not
capture them in service logs or ordinary deployment logs.

## SQLite ownership, migration and recovery

The protected gateway acquires an exclusive nonblocking `flock` on
`<absolute-database-path>.gateway.lock` before opening its DB. This sidecar is
opened with no-follow semantics and mode `0600`; SQLite itself remains
responsible for CLI/gateway transaction coordination. This is an application
lock, not `PRAGMA locking_mode=EXCLUSIVE`. A second gateway for the same
resolved DB path fails closed. On clean close the descriptor is unlocked and
closed; the `.gateway.lock` pathname may remain. Do not remove that file while
the gateway might be running: the lock is on the opened inode, and unlinking it
could let another process lock a replacement inode.

SQLite runs in WAL mode with `synchronous=FULL`, foreign keys enabled, and a
500 ms busy timeout. Preserve the DB together with its WAL state as one
consistent snapshot. Never copy only the main DB file while a writer is active,
and never treat a raw copy of the DB, `-wal`, and `-shm` files as a backup.
Preferred acceptance is SQLite's backup API (the existing local procedure uses
`sqlite3` `.backup`) and validation of the produced standalone backup. The
backup API can provide a consistent snapshot while the DB is open; a file copy
is permitted only after the gateway and all DB connections are stopped cleanly.
No backup CLI subcommand is currently implemented.

Restore to a separate, service-owned DB path, never over a live DB. Validate
`PRAGMA integrity_check` returns `ok` and `PRAGMA foreign_key_check` returns no
rows, then use the matching external master key. Confirm current schema status,
historical usage, and decryptability of an encrypted credential through the
admin interface or protected request path before promoting the restored copy.
Keep an untouched backup and original DB until those checks pass.

For a schema upgrade: stop the gateway, take and verify a consistent backup,
retain the key, run `gateway admin --db PATH --master-key PATH migrate`, then
start the new binary and verify readiness and persisted state. Protected
startup requires the current schema version; it does not auto-migrate. Current
schema is v6. Migrations are forward-only; an older binary refuses a newer
schema, and downgrade after migration is not guaranteed. Recovery means restore
the pre-upgrade backup and matching key to a separate DB and run the compatible
older binary against that copy. Never promise in-place downgrade or key
rotation.

## Shutdown and failure behavior

The process handles `SIGINT` and `SIGTERM`. Shutdown withdraws readiness and
stops new inference admission, then calls HTTP server graceful shutdown with
the configured positive `shutdown_timeout`. If draining exceeds that bound,
remaining HTTP connections are forcibly closed. Component/transport and DB
cleanup then runs; the DB closes before the process lock is released. A clean
zero exit is evidence that these close steps returned without error. A
forced-closed active request may be incomplete; startup accounting recovery
marks uncertain dispatched work interrupted/conservative and never replays it.

The TCP listener is closed by graceful or forced server shutdown. The gateway
does not remove the lock sidecar file, and there is no separate pidfile or
socket-path cleanup guarantee. On startup failure, it fails closed and releases
resources acquired so far. If the process is killed or the host loses power,
the OS releases its flock when the process exits; SQLite WAL recovery and the
protected startup recovery transaction run before the next listener is served.
This is not a claim of zero data loss beyond SQLite's configured durability or
of recovery from damaged media, lost keys, or arbitrary filesystem failure.

## Downstream acceptance bounds

### M13-002 — packaged startup

- From a fresh checkout, build the pinned Linux/amd64 binary offline using
  repository build checks; run it as a non-root service account with only the
  explicit config, state and key paths mounted/provisioned.
- Verify startup, protected `/readyz`, one fake-upstream request, SIGTERM clean
  exit, subsequent restart, exclusive second-process rejection, and release of
  the lock after shutdown. Assert effective DB/WAL/key directory ownership and
  modes; prove no root, Go runtime, Node, model/GPU, or remote orchestration is
  required. Record container/systemd checks only if actually executed.

### M13-003 — backup, restore and upgrade

- With deterministic fake credentials/upstream, preserve a consistent backup,
  restore to a distinct DB path, pass SQLite integrity and foreign-key checks,
  retain usage/history and decrypt the encrypted credential with the original
  external key, then serve a protected request from the restored copy.
- Prove a live raw DB-file copy is not accepted as a procedure; exercise the
  SQLite backup API or stop all DB users before file copy. Check absent/wrong
  keys fail closed. For upgrade, stop, back up, migrate, start and verify state;
  prove older-binary behavior against the upgraded schema and recovery using
  the untouched pre-upgrade copy. Do not claim in-place downgrade or rotation.

### M13-004 — offline release acceptance

- Start from a fresh checkout and packaged unprivileged runtime; run the fake
  upstream end-to-end release matrix for protected JSON/SSE, cancellation,
  denied/revoked keys, accounting across restart, and backup/restore.
- Record exact Linux/amd64/toolchain/runtime, commands, finite resource bounds,
  shutdown and restore outcomes, and any checks unavailable locally. Keep
  provider/live gates separate; fake-upstream acceptance is not provider
  compatibility evidence. No UI packaging is required by this Go release
  check.

## Source audit

This binding reflects `cmd/gateway/main.go`, `cmd/gateway/yaml_config.go`,
`cmd/gateway/protected_startup.go`, `cmd/gateway/admin.go`,
`internal/storage/sqlite/process_lock.go`, `internal/storage/sqlite/sqlite.go`,
`docs/implementation/CONFIGURATION.md`, `M3-STORAGE.md`, and `LOCAL-M3.md`.
Those sources own exact flags, schema fields, key checks, lock behavior,
shutdown, and SQLite settings; this document adds no runtime switches or
configuration fields.

Related specifications: [M13 operations brief](M13-OPERATIONS.md),
[configuration](CONFIGURATION.md), [storage/recovery](M3-STORAGE.md),
[local protected procedure](LOCAL-M3.md), and [task registry](TASKS.md).
