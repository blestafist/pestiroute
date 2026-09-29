---
description: Complete up to N consecutive ready tasks with planning, implementation, review, and closure
agent: orchestrator
subagent: false
---

Execute `/next $1` for this PestiRoute checkout. Treat `$1` as the maximum number of **newly completed** registered tasks in this invocation. It must be one positive decimal integer; if it is missing, malformed, or has extra arguments, explain the usage (`/next 10`) and do no work. Do not treat it as a task ID, a time budget, or permission to work beyond the registered backlog.

Continue autonomously, one task at a time, until that many tasks are DONE, the user stops you, or no in-scope task can make progress. Do not end after a single task merely to ask whether to continue. This command starts a bounded session workflow; it does not schedule background execution after the session ends.

1. Read `docs/implementation/CURRENT.md`, the relevant `docs/implementation/TASKS.md` rows, and the working-tree status. Preserve unrelated changes. Resume an ACTIVE task only if its owner and work state can be reconciled; never silently take over another owner's work.
2. Ask the planner to select the next task whose dependencies are DONE. A DRAFT row is not executable: have the planner prepare its card and concrete check, then require READY before dispatch. If a decision or dependency prevents readiness, record the blocker and consider another independent in-scope candidate. Do not invent new milestones or expand task scope to fill the quota.
3. Assign exactly one READY task to the worker. Require it to claim ACTIVE ownership, implement the card, run applicable checks, and record evidence. Give the reviewer the actual task-specific diff and acceptance criteria for independent review. Require an explicit PASS on the current diff; treat REJECT, missing or ambiguous verdicts, and stale reviews as non-approval. Resolve findings through the worker and obtain focused re-review as needed.
4. Only after that PASS, ask the planner to verify every acceptance criterion and mark DONE with evidence. Count the task toward `$1` only after the registry says DONE. A BLOCKED or still ACTIVE task does not count. Refresh the handoff before choosing the next task; serialize shared checkout writes and task registry updates.
5. Repeat steps 2–4 without waiting for routine user confirmation. Stop when no eligible task can be made READY, a necessary decision is outside existing authority, verification cannot be completed, or the same problem remains unresolved after a bounded correction attempt. Do not loop or claim completion to reach the quota.

If any tasks were completed, require git-worker after the loop to commit only this batch's completed, verified task changes, including planner handoff updates. Verify the commit result before reporting delivery; if a clean scoped commit is impossible, report the blocker and leave unrelated changes untouched. Do not create an empty commit when no tasks were completed. Do not merge, deploy, push, or perform other external writes unless separately authorized.

Respect `AGENTS.md`, the agent permissions, the accepted contract, and the user's current scope. At the end, report completed task IDs, checks and evidence, the count versus `$1`, the commit result, and the concrete reason if you stopped early. Never claim work continues after the session stops.
