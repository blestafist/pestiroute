# M13 — Deployment and Recovery

## Outcome

Reproducible single-node operation with a documented upgrade/rollback and tested
SQLite recovery procedure. Branch `dev-m13-operations`; first M13-001. The current
repository has no Dockerfile/release packaging; a source build is not a verified
server installation. This lane can start without provider accounts or WebUI.

## Sequence

1. Audit existing startup, shutdown, process lock, schema/key lifecycle and
   deployment artifacts. Bind supported Linux/amd64 operation and choose a
   minimal systemd/container delivery procedure; record exact build/runtime
   prerequisites and missing checks.
2. Add reproducible Go build/start packaging, an unprivileged single-node
   container recipe and a systemd example with explicit config/data/key paths.
   Do not introduce Kubernetes, orchestration services, remote databases or
   runtime toolchain installation. Keep generated outputs out of Git.
3. Prove a consistent SQLite backup/restore plus external master-key handling,
   wrong/missing-key failure and stopped-upgrade recovery. Never copy an active
   WAL database as if it were a consistent backup. Include process/socket
   cleanup and safe downgrade constraints after schema changes.
4. Run a local release acceptance procedure covering protected JSON/SSE,
   cancellation, denied/revoked keys, accounting across restart and backup
   restore; use a fake upstream and finite resources. Publish operator commands,
   tested platforms and exact failure outcomes. UI packaging receives a later
   integration check after M12-006 is on main; the Go deployment slice does not
   wait for it.

## Ownership

Own deployment/build artifacts, `scripts/release-*`, `M13-*` and task cards.
M13 is the named owner for CI changes to add packaging checks; preserve the
pinned toolchain, external/M6 and full Go gates. Frontend CI comes from M12
through a serial handoff, not competing workflow edits. M11 owns startup schema
and management-listener changes; do not modify those concurrently. Deployment
examples may document the accepted settings once they exist, never invent flags.

## Completion

A fresh checkout builds, starts as unprivileged user, passes fake-client checks,
shuts down cleanly, and restores a backup with historical usage and encrypted
credentials intact. Existing binary-only operation needs no Node/model/GPU.
Provider live gates remain independent release limitations, explicitly recorded.
Do not claim a Docker/systemd test that was only syntax-checked; missing local
container facilities require a real hosted/container check before that claim.
