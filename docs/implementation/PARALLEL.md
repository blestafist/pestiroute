# Parallel OpenCode Development

One writer per worktree and one exact milestone filter per OpenCode session.
Separate directories/indexes/HEADs isolate file writes; they do not resolve
conflicting designs or make another lane's DONE dependency integrated.

## Active lane map

| Filter | Branch | First READY | Exclusive source ownership |
| --- | --- | --- | --- |
| M10 | dev-m10-antigravity | M10-001 | `internal/connector/antigravity/**`, dedicated fixtures and M10 docs/cards |
| M11 | dev-m11-admin-api | M11-001 | Management Go packages, `docs/api/**`, shared gateway/admin composition and config |
| M12 | dev-m12-webui | M12-001 | `web/**`, UI embedding package and dedicated browser tests |
| M13 | dev-m13-operations | M13-001 | Deployment/release/backup artifacts and packaging CI sequencing |

Every lane owns only its registry rows/cards and CURRENT section. Core contracts,
SQLite schema, shared docs and test helpers need a named owner and serial review
window before changes. M10 requests M11's gateway window for connector wiring.
M12 consumes M11's static mount seam instead of concurrently editing startup.
M13 coordinates packaging CI with M12; preserve existing full Go/M6 checks.

M8's offline slice is integrated; M8-008/009 retain live/enablement gates and are
not a parallel implementation queue until ready. M9's implementation is on main.
Do not reuse or reset these branches as new lanes.

## Setup from the published planning baseline

The remote lane branches are already created; do not create them again from an
older main. From a clean main checkout, inspect `git worktree list` first:

```sh
git fetch origin
git worktree add --track -b dev-m10-antigravity ../pestiroute-m10 origin/dev-m10-antigravity
git worktree add --track -b dev-m11-admin-api ../pestiroute-m11 origin/dev-m11-admin-api
git worktree add --track -b dev-m12-webui ../pestiroute-m12 origin/dev-m12-webui
git worktree add --track -b dev-m13-operations ../pestiroute-m13 origin/dev-m13-operations
```

If a local branch or path exists, inspect and reuse its correct worktree; never
reset/delete/force-add it or switch a branch beneath an active session. Confirm
upstream is the matching lane, never origin/main. In each worktree independently:

```sh
export TMPDIR="$(mktemp -d /tmp/pestiroute-lane.XXXXXX)"
export GOFLAGS=-p=1
opencode
```

Then run exactly `/next 10 M10`, `/next 10 M11`, `/next 10 M12` or `/next 10 M13`
in its matching worktree. Each process gets a distinct TMPDIR; do not share the
example directory between terminals. N is a maximum accepted task count; do not
invent tasks to fill it. A lane can prepare its next DRAFT card once dependencies
and check readiness are proved, and stops when no eligible work remains.

## Real independence and dependency handoffs

M10 research, M11 binding/API, M12 UX/mock screens and M13 packaging can start
now. M12-005 needs M11-007 integrated onto main and then merged into the UI lane;
M11 being DONE on another branch is insufficient. After integration, recheck the
API contract against fixtures/client. Antigravity cannot silently expand the
shared contract or block UI on Google access. Provider account/hardware/live
gates stay in their existing tasks and do not block offline lanes.

## Review, pushes and integration

Retain the repository's Planner → Worker → Reviewer workflow and explicit PASS
rule. Freeze a task during review; commit only its accepted scope. At batch end,
push the matching established upstream once; no per-task pushes and no lane push
to main. The named mapping above also authorizes first matching publication if an
upstream is missing, after verifying origin and outgoing task scope. Do not
force-push or publish another lane's work. No provider calls are setup checks.

Each lane integrates through its own reviewed PR and actual green revision CI.
Merging is separately authorized; integrate one lane at a time, reconcile shared
files and rerun affected checks. This planning assignment creates branches but
does not authorize future unattended merges or real-provider enablement. No
background scheduler is started by `/next`; these are foreground sessions.
