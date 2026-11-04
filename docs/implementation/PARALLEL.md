# Parallel OpenCode Development

One writer per worktree and one exact milestone filter per OpenCode session.
Separate directories/indexes/HEADs isolate file writes; they do not resolve
conflicting designs or make another lane's DONE dependency integrated.

## Active lane map

| Filter | Branch | Next work | Exclusive source ownership |
| --- | --- | --- | --- |
| M10 | dev-m10-antigravity | Paused for this integration; existing worker owns progress | Antigravity connector, fixtures and M10 docs |
| M12 | dev-m12-webui | M12-007, 008, 011, 009, 010 in dependency order | `web/**`, UI embedding, browser tests and M12 docs |

M11 Admin API and M13 Operations are integrated. Do not relaunch their completed
queues. M12 owns the serial shared-file window for `.github/workflows/webui.yml`,
`scripts/release-build.sh`, `cmd/gateway/main.go` UI wiring and its own task/docs
updates during this follow-up integration. Core contracts and SQLite remain
outside scope. Antigravity is explicitly excluded and does not block WebUI.

M8 offline and M9 implementation are already on main; provider live gates remain
in their existing cards. Do not reset or reuse their branches.

## Updating existing worktrees

In the existing idle M12 worktree, inspect status and worktrees, then fetch and
merge the accepted main baseline without rewriting branch history:

```sh
git status --short
git worktree list
git fetch origin
git merge --ff-only origin/dev-m12-webui
git merge origin/main
```

Stop to preserve local edits or resolve actual divergence; never reset an active
worker. These commands assume the current branch is `dev-m12-webui`.

If a local branch or path exists, inspect and reuse its correct worktree; never
reset/delete/force-add it or switch a branch beneath an active session. Confirm
upstream is the matching lane, never origin/main. In each worktree independently:

```sh
export TMPDIR="$(mktemp -d /tmp/pestiroute-lane.XXXXXX)"
export GOFLAGS=-p=1
opencode
```

Then run `/next 10 M12` in that worktree. N is a maximum accepted count;
execute only eligible tasks in dependency order. M12-008/011/009/010 remain DRAFT
until their prerequisites are accepted and the planner checks their cards.
Do not run two sessions editing the shared `App.tsx` simultaneously.

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
