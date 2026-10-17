# Single-node Linux deployment

This package targets one Linux/amd64 host with a local filesystem supporting
SQLite locks and durable sync. Build from the pinned Go toolchain; the deployed
static binary does not need Go, Node, or a model/GPU runtime. TLS upstreams need
the system CA bundle. The service accepts only protected version-1 YAML; the
example in `config/gateway.yaml.example` is illustrative and uses `.invalid`
endpoints, so replace its model, connector, account, route, and policy values
with provisioned values.

## Build and local smoke check

```sh
./scripts/release-build.sh
TMPDIR=/path/to/private-temp ./scripts/verify-packaging.sh
```

Run the verification script as an unprivileged account. It provisions a private
temporary SQLite database and master key, starts a deterministic fake upstream,
checks readiness and a protected request, rejects a second process on the same
database lock, then proves clean SIGTERM exit and lock reacquisition. It requires
`curl` and `python3`; it does not contact a real provider.

## Filesystem and secret provisioning

Create `/etc/pestiroute` readable only to the service account and administrators;
keep YAML and credential environment files mode `0600`. Provision the exact
32-byte master key as an owned regular file at
`/etc/pestiroute/master.key`, mode `0600`; never put it in YAML, the database,
the image, or a backup. Do not regenerate it on startup. Give the service account
read access to its key and config and no write access to the config tree.

Use `/var/lib/pestiroute` owned by the service account, mode `0700`. Configure
`storage.path` there. The database, SQLite WAL/SHM sidecars, and
`<database>.gateway.lock` must be service-owned and mode `0600`; verify their
effective modes on the target filesystem. Keep the database and its sidecars on
local durable storage, never a network share. The gateway creates files under a
restrictive umask, but does not itself enforce DB/sidecar mode bits.

Provision and migrate the database using the same account and external key
before starting the service, for example `pestiroute admin --db
/var/lib/pestiroute/gateway.db --master-key /etc/pestiroute/master.key migrate`.
Create the configured accounts, credentials, policies and virtual keys with
the `gateway admin` commands. Supply connector credential environment values
through a protected systemd environment file (the service unit reads
`/etc/pestiroute/environment`); never put credentials in YAML or ordinary logs.

## systemd

Install the binary as `/usr/local/bin/pestiroute`, install the example as
`/etc/pestiroute/gateway.yaml`, and provision the key, database and environment
file as above. Install `systemd/pestiroute.service` under
`/etc/systemd/system/`, then run `systemctl daemon-reload` and
`systemctl enable --now pestiroute`. `StateDirectory` requests mode `0700`; the
unit runs as `pestiroute`, uses `ProtectSystem=strict`, `ProtectHome=yes`,
`UMask=0077`, and sends SIGTERM with a 30-second stop limit. Check
`systemctl status pestiroute` and `curl http://127.0.0.1:8080/readyz`.

## Container

Build for the supported architecture with `docker build --platform linux/amd64 -t pestiroute:local .`. Mount protected configuration
read-only at `/etc/pestiroute`, persistent state at `/var/lib/pestiroute`, and
pass the configured connector credential as a secret-backed environment value.
The image runs as UID/GID 10001, includes system CA certificates, and contains no
configuration, key, credential, or database. Provision/migrate the database
separately with the same binary and compatible mounted key before serving.

## Shutdown and recovery limits

SIGTERM withdraws readiness and drains requests within the configured
`shutdown_timeout`; systemd terminates the service cgroup after 30 seconds.
The lock is released on clean close or process exit; its sidecar pathname may
remain and must not be removed while a gateway could be running. Do not copy a
live WAL database as a backup. Use SQLite's backup API or stop every DB user and
copy only after clean shutdown. See [the protected local operations procedure](../docs/implementation/LOCAL-M3.md)
and [M13 binding](../docs/implementation/M13-BINDING.md) for recovery limits.
