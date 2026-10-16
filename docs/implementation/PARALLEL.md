# Parallel OpenCode Development

Run one OpenCode session per Git worktree and milestone. Separate branches in
the same directory do not isolate writers: switching branches beneath another
session changes its files and review target. Git worktrees have separate files,
indexes, and HEADs, while sharing repository objects and remotes.

## Start two lanes

After the planning/configuration changes are on main, run from a clean checkout:

```sh
git fetch origin
git worktree add -b dev-m8 ../pestiroute-m8 origin/main
git worktree add -b dev-m9 ../pestiroute-m9 origin/main
```

If a branch or path already exists, inspect `git worktree list` and its status;
reuse the correct lane rather than resetting, deleting, or force-adding it.
For an existing unmounted branch, use `git worktree add ../pestiroute-m8 dev-m8`
(likewise for M9). Worktree setup and first upstream establishment are explicit
Git operations, not implementation task commits.

In terminal A:

```sh
cd ../pestiroute-m8
export TMPDIR="$(mktemp -d /tmp/pestiroute-m8.XXXXXX)"
export GOFLAGS=-p=1
opencode
```

Then run `/next 10 M8`. In terminal B:

```sh
cd ../pestiroute-m9
export TMPDIR="$(mktemp -d /tmp/pestiroute-m9.XXXXXX)"
export GOFLAGS=-p=1
opencode
```

Then run `/next 10 M9`. Ten is the maximum completed task count, not permission
to invent extra cards or bypass dependencies. These are two foreground sessions;
the command does not start a background scheduler. The OpenCode CLI must already
be installed and configured with the existing models/MCP services. No new JSON
schema keys, plugins, or account credentials are needed for this workflow.

Do not execute untrusted provider calls as a setup check. Each lane's TMPDIR
keeps process/socket snapshots separate; Go package serialization alone does not
isolate simultaneous test runs in two worktrees. Retain a lane's temporary
directory until all its processes stop; cleanup is a separate operator action.

## Ownership and coordination

- `/next N M8` selects only `M8-*`; `/next N M9` selects only `M9-*`.
  Legacy `/next N` remains available for a single coordinated backlog session.
  In parallel operation always supply the milestone. A blocked lane stops or
  selects another eligible task in that lane; it cannot take the other backlog.
- Keep one implementation writer and one serial planner/Git sequence per
  worktree. A task under review is frozen until the reviewer returns a verdict.
  Continue the existing worker/reviewer session limits and explicit PASS rule.
- Change only the lane's TASKS rows, task cards, and CURRENT lane section.
  Do not archive/reorder another milestone, rewrite the shared baseline, or
  close another lane's tasks. Each branch's DONE status is local evidence until
  its commits are integrated; another lane must not assume it is on main.
- The M8/M9 briefs name source ownership. Shared gateway files, contracts,
  storage, CI, agent settings, and test helpers need an explicit single owner
  and sequencing plan before overlapping edits. Separate worktrees prevent
  file races; they do not make conflicting designs independent.
- Independent task cards may be prepared concurrently. A dependency from
  another branch is unmet until the necessary commit is integrated into the
  consuming lane and its evidence is rechecked. Do not mark it DONE by hearsay.

## Commits, pushes, and integration

One accepted task produces one scoped commit in its lane. Serial Git writes are
required within a worktree, not across independent worktrees. At batch end push
only the lane branch. For these named lanes, `/next N M8` or `/next N M9`
authorizes first publication with `git push -u origin dev-m8` or `dev-m9`
respectively after confirming the remote is this repository and outgoing commits
are the assigned lane's work. Other unknown destinations still require input.
Never push a lane to main, force-push, or change another worktree's branch.

Review each lane through its own PR against main. Merging remains a separately
authorized operation. Integrate one at a time; after the first merge, update the
second branch by an explicitly authorized merge, reconcile shared documentation
and gateway changes, and rerun affected checks/review. Do not automatically
cherry-pick or rebase another lane. Both lanes need green CI for their actual
review revisions. Live readiness or capability changes require their own gates.
