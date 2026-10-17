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

## Backup and restore

Keep backups private: they contain accounts, virtual-key digests, usage history,
and encrypted credentials. They do not contain the external master key. Retain
that exact key separately; losing it makes encrypted credentials unavailable.
Do not copy only the main database file while the gateway is running, and do not
copy a live DB/WAL/SHM set as a substitute for a consistent snapshot. With the
SQLite CLI installed, `.backup` uses SQLite's online backup API and is safe
while the gateway is serving:

Provision the backup directory as root, then switch to the service account:

```sh
install -d -o pestiroute -g pestiroute -m 0700 /var/lib/pestiroute/backups
```

Run SQLite commands as `pestiroute`, which owns the DB and master key. Preserve
the resulting `BACKUP` path for a later restore or upgrade rollback:

```sh
set -Eeuo pipefail
umask 077
DB=/var/lib/pestiroute/gateway.db
BACKUP_DIR=/var/lib/pestiroute/backups
BACKUP_ATTEMPT=$(mktemp -d "$BACKUP_DIR/online.XXXXXX")
BACKUP="$BACKUP_ATTEMPT/gateway.db"
test ! -e "$BACKUP"
cleanup() { rm -rf "$BACKUP_ATTEMPT"; }
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
sqlite3 "$DB" ".backup '$BACKUP'"
test "$(sqlite3 "$BACKUP" 'PRAGMA integrity_check;')" = ok
if ! BACKUP_FOREIGN_KEYS=$(sqlite3 "$BACKUP" 'PRAGMA foreign_key_check;'); then
  echo "foreign-key validation command failed" >&2
  exit 1
fi
test -z "$BACKUP_FOREIGN_KEYS"
BACKUP_SCHEMA=$(sqlite3 "$BACKUP" 'SELECT max(version) FROM schema_migrations;')
test "$BACKUP_SCHEMA" -ge 1
trap - EXIT HUP INT TERM
```

On success, retain the uniquely created attempt directory and note its schema
version. Any failed backup or integrity/reference check aborts the operation and
cleans up the incomplete attempt; never reuse or overwrite an earlier backup.

The backup directory is inside the service-owned state tree on protected local
storage; access to the backup also grants access to durable account, key, and
usage metadata. Run SQLite/admin operations as the service account that owns
the DB and master key. If SQLite CLI is unavailable, stop the gateway cleanly,
ensure every other DB connection is closed, then copy the database and run
equivalent integrity/reference checks. Never treat a raw active-WAL file copy
as a standalone backup. For upgrade rollback, retain this pre-upgrade snapshot
untouched, record its `SELECT max(version) FROM schema_migrations;` result, and
do not substitute a post-upgrade snapshot for it.

Restore to a fresh, service-owned path inside `/var/lib/pestiroute`, never over
the live DB or an existing restore. Stage and validate first; then use a
same-directory hard link as an atomic no-overwrite promotion. If the backup is
corrupt or the destination already exists, leave the destination untouched and
choose a new path rather than repeating a restore over it. Provision the
directory as root:

```sh
install -d -o pestiroute -g pestiroute -m 0700 /var/lib/pestiroute/restore
```

Then perform restore and admin validation as the service account:

```sh
set -Eeuo pipefail
umask 077
RESTORE_DIR=/var/lib/pestiroute/restore
RESTORE="$RESTORE_DIR/gateway-$(date -u +%Y%m%dT%H%M%SZ).db"
STAGE="$RESTORE.stage"
BACKUP=/var/lib/pestiroute/backups/online.ID/gateway.db # selected retained snapshot
test ! -e "$RESTORE" && test ! -e "$STAGE"
BACKUP_SCHEMA=$(sqlite3 "$BACKUP" 'SELECT max(version) FROM schema_migrations;')
test "$BACKUP_SCHEMA" -ge 1
cleanup() { rm -f "$STAGE" "$STAGE-wal" "$STAGE-shm"; }
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
sqlite3 "$BACKUP" ".backup '$STAGE'"
test "$(sqlite3 "$STAGE" 'PRAGMA integrity_check;')" = ok
if ! STAGE_FOREIGN_KEYS=$(sqlite3 "$STAGE" 'PRAGMA foreign_key_check;'); then
  echo "foreign-key validation command failed" >&2
  exit 1
fi
test -z "$STAGE_FOREIGN_KEYS"
STAGE_SCHEMA=$(sqlite3 "$STAGE" "SELECT printf('%d:%d:%d', count(*), min(version), max(version)) FROM schema_migrations;")
test "$STAGE_SCHEMA" = "$BACKUP_SCHEMA:1:$BACKUP_SCHEMA"
chmod 0600 "$STAGE"
ln "$STAGE" "$RESTORE"   # atomic no-overwrite; fails if RESTORE exists
rm -f "$STAGE" "$STAGE-wal" "$STAGE-shm"
trap - EXIT HUP INT TERM
pestiroute admin --db "$RESTORE" --master-key /etc/pestiroute/master.key status
pestiroute admin --db "$RESTORE" --master-key /etc/pestiroute/master.key account list
pestiroute admin --db "$RESTORE" --master-key /etc/pestiroute/master.key usage summary
```

Point a protected YAML copy at the restored path, use the retained original
master key, and verify `/readyz` plus an authenticated request through the
restored virtual key before promotion. Keep the source DB and untouched backup
until validation completes. Missing or malformed key files are rejected by
startup and admin commands. A syntactically valid but incorrect 32-byte key is
not authenticated against SQLite at startup: startup may become ready, but a
request that needs an encrypted credential must fail closed and must not reach
the upstream. `admin status` only reports schema and does not decrypt credentials,
so a successful status is not proof that the key matches. Verify credential
decryptability through an authenticated fake-upstream request (or another path
that actually reads the encrypted record) before promoting a restore. Do not
bypass errors by generating a replacement key. Do not put key or credential
contents in logs or backup files.

If backup creation or restore validation fails, do not promote the stage. Remove
only that incomplete stage after stopping any process that might have opened it;
leave existing databases and backups untouched. Repeated restore to an existing
destination must fail rather than overwrite it. Verify revoked keys remain
denied after restore as part of application-level acceptance.

## Stopped schema upgrade and rollback

Schema migrations are forward-only; protected startup requires the schema version
supported by its binary and does not auto-migrate. Stop the gateway, make and
validate a consistent backup (retaining its schema version and key), and retain
the external master key before upgrade. For a rollback snapshot that includes
history, first run a successful request through the compatible pre-upgrade
gateway and confirm usage is recorded. Then stop it, take the untouched backup,
and migrate with the target binary while stopped. Start the target and verify
readiness and persisted accounts,
policies, keys, credentials and usage state. Run systemd commands as root and DB
commands as the service account:

```sh
# Run this command as root.
systemctl stop pestiroute
```

As `pestiroute`, create and validate a unique pre-upgrade snapshot. Each attempt
uses a fresh directory; no existing backup can be overwritten:

```sh
set -Eeuo pipefail
umask 077
DB=/var/lib/pestiroute/gateway.db
BACKUP_DIR=/var/lib/pestiroute/backups
BACKUP_ATTEMPT=$(mktemp -d "$BACKUP_DIR/preupgrade.XXXXXX")
BACKUP="$BACKUP_ATTEMPT/gateway.db"
test ! -e "$BACKUP"
cleanup() { if [[ ${BACKUP_READY:-0} != 1 ]]; then rm -rf "$BACKUP_ATTEMPT"; fi; }
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
sqlite3 "$DB" ".backup '$BACKUP'"
test "$(sqlite3 "$BACKUP" 'PRAGMA integrity_check;')" = ok
if ! BACKUP_FOREIGN_KEYS=$(sqlite3 "$BACKUP" 'PRAGMA foreign_key_check;'); then
  echo "foreign-key validation command failed" >&2
  exit 1
fi
test -z "$BACKUP_FOREIGN_KEYS"
BACKUP_SCHEMA=$(sqlite3 "$BACKUP" 'SELECT max(version) FROM schema_migrations;')
test "$BACKUP_SCHEMA" -ge 1
BACKUP_READY=1
trap - EXIT HUP INT TERM
pestiroute admin --db "$DB" --master-key /etc/pestiroute/master.key migrate
```

Return to root to start and verify the service:

```sh
systemctl start pestiroute
curl -fsS http://127.0.0.1:8080/readyz
```

Run persisted-state checks as `pestiroute`:

```sh
pestiroute admin --db "$DB" --master-key /etc/pestiroute/master.key account list
pestiroute admin --db "$DB" --master-key /etc/pestiroute/master.key policy list
pestiroute admin --db "$DB" --master-key /etc/pestiroute/master.key key list
pestiroute admin --db "$DB" --master-key /etc/pestiroute/master.key usage summary
```

If upgrade validation fails, stop the service. Do not run an older binary
against a newer schema and do not attempt an in-place downgrade. It must reject
the actual newer-schema DB. Using the service account, restore the untouched
pre-upgrade backup with the staged, validated, no-overwrite procedure above to a
separate clean DB path. Confirm its schema version is supported by the older
binary, run that binary's admin status and data checks against the restored
copy, then point its protected YAML at that path and start it with the same
external master key. Verify readiness, serve a credential-backed request, and
confirm the pre-upgrade usage remains present. Preserve the upgraded DB and source backup for diagnosis;
promote neither copy until readiness and required application checks succeed.
This restores the pre-upgrade database state; it does not roll back the external
key or recover data written after the backup.

Run the deterministic local recovery acceptance as an unprivileged user after
building the release binary:

```sh
./scripts/release-build.sh
TMPDIR=/path/to/private-temp ./scripts/verify-recovery.sh
```

It requires Go 1.27.1, Git, tar, `sqlite3`, `curl`, `python3`, and `sha256sum`;
it builds the schema-v5 compatibility binary from historical commit `f0ed045` in
its private temporary directory. It exercises online `.backup`, safe fresh-path
restore and no-overwrite failure behavior, key failures, revoked-key persistence,
stopped v5→v6 migration, rejection of the actual v6 DB by the v5 binary, and
restoration of the untouched v5 backup for that binary. It uses only a local fake
upstream and makes no live-provider claim.
