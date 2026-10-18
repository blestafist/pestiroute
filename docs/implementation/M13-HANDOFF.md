# M13 Deployment and Recovery Handoff

## Offline release acceptance

The packaged release acceptance was run on Linux/amd64 (`x86_64`, Linux kernel
7.2.10) as unprivileged UID 1000 with the pinned Go 1.27.1 toolchain. The static
gateway needs no Go runtime after build. No GPU, provider account, or external
network access was used; the deterministic fake upstream and gateway bind only
to loopback. Prerequisites are Go 1.27.1 for build/checks, Bash, Python 3,
`curl`, and `sqlite3`.

The release process applies finite per-process limits: 120 CPU seconds, 2 GiB
virtual address space, and 256 file descriptors. Protected request size is
bounded at 1 MiB; connector connect, response-header, and stream-idle timeouts
are 1, 3, and 5 seconds respectively. These are acceptance-test bounds, not
production capacity recommendations.

Run from the repository root as a non-root user:

```sh
./scripts/release-build.sh
TMPDIR=/path/to/private-temp ./scripts/verify-packaging.sh
TMPDIR=/path/to/private-temp ./scripts/verify-recovery.sh
TMPDIR=/path/to/private-temp ./scripts/verify-release.sh
```

`verify-release.sh` compared the complete protected JSON request/response bytes
and SSE request/response bytes, including unknown opaque fields, against the
fake-upstream fixtures. A separate client read the first complete SSE event
byte-for-byte before disconnect; three repetitions were observed canceling the
upstream. Unknown-key HTTP 401 and revoked-key rejection (401 or 403) dispatched
no upstream requests.

Usage snapshots include request ID, attempt ID, token counters, source,
completeness, and recorded timestamp. The historical snapshot matched exactly
across same-DB/key SIGTERM restart, online SQLite `.backup`, and distinct-path
restore. The post-restart request added exactly one provider-usage record with
input/output 2/1; the restored authenticated request using the matching external
key and restored encrypted credential added exactly one equivalent new record.
Backup and restore integrity/FK checks passed. Clean shutdown allowed listener
rebind and lock reacquisition. The script enforces fd/CPU/address-space bounds
and checks lock mode; `/proc/<pid>/fd` counts across three cancelled streams
showed no accumulation beyond one transient descriptor.

## Operations and recovery

For packaging, key/config/state permissions, SIGTERM behavior, and deployment
prerequisites see [`deploy/README.md`](../../deploy/README.md). The SQLite
online-backup and staged, distinct-path restore commands are in its Backup and
restore section; the local offline procedure is
[`LOCAL-M3`](LOCAL-M3.md). Never copy a live WAL database as a raw file set or
restore over a live/existing database. Retain the matching external master key
and an untouched backup until restored authenticated traffic and historical
usage are verified. The gateway releases its database lock on clean shutdown;
the `.gateway.lock` pathname can remain and must not be removed while the
gateway might be running.

The release script verifies listener cleanup and lock reuse through restart and
successful restored-database startup. M13 packaging checks exercise effective
protected file modes; these outcomes do not imply that every systemd/container
deployment was installed and tested as part of this offline release run.

## Release limitations

- Linux/amd64 single-node local-filesystem deployment only; no GPU, real
  provider, network filesystem, or live-provider compatibility gate is tested.
- UI packaging remains deferred until M12-006 is integrated on main.
- The independent provider/live gates M4-036, M4-041, M5.1-049, M5.2-025, and
  M8-008 remain deferred or blocked as recorded in CURRENT and their task cards.
  Fake-upstream release acceptance does not satisfy them.
- The release script uses deterministic local fixture traffic and does not
  establish production CPU/memory capacity, provider service limits, or
  interruption recovery beyond the checks reported above.

## Verification evidence

On Oct 17, 2026, pinned Go 1.27.1 release, packaging, recovery, targeted
regression, full repository, and the M6 external race plus both offline process
smoke checks passed locally on Linux/amd64. `/proc/<pid>/task/<tid>/children`
inspection worked in both smoke gates; the older CURRENT host-limitation note
does not describe this run. Exact commands/results are in
[`M13-004 Completion Evidence`](tasks/M13-004.md#completion-evidence).

Read-only inspection of hosted CI run [39047526958](https://github.com/blestafist/pestiroute/actions/runs/39047526958)
confirmed `success` for source `5738253cbf902fb0d8ab5eba9a1c859e5faf39be`; this
is the lane baseline, not the current M13 revision. Current-revision hosted CI
has not run: per [parallel workflow](PARALLEL.md#review-pushes-and-integration),
it follows the authorized batch-end PR push. No push, PR, or CI trigger was
performed for this handoff.
