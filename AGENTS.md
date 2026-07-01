# TaskPilot Agent Rules

This repository uses TaskPilot for human-agent coordination.

TaskPilot is the system of record for real repository work. Do not treat task
creation as optional just because the user did not provide a task ID.

When the user gives you a TaskPilot task ID:

1. Run `taskpilot task show <task-id> --json` before starting.
2. Claim the task before editing.
3. Acquire locks for files, artifacts, or semantic areas you will touch.
4. Send heartbeat while actively working, or use `taskpilot run <task-id> -- <agent-command>`.
5. Append sanitized findings, decisions, risks, blockers, and output references.
   When launched through `taskpilot run`, write sanitized entries to `$TASKPILOT_RUN_CONTEXT_FILE`:
   - `decision: Keep token format unchanged`
   - `blocker: Missing reproduction data`
   - `{"kind":"summary","content":"Added regression coverage"}`
6. Do not upload raw local files, secrets, prompts, logs, screenshots, or customer data unless explicitly approved.
7. Prepare a handoff if stopping before completion.
8. Mark complete only when the task completion criteria are satisfied.

When the user starts work without a TaskPilot task ID:

1. Inspect TaskPilot context before editing: active session task, likely repo
   task, existing open tasks, recent semantic memory, changed files, locks, and
   relationships.
2. If there is a matching active or open task for the same objective, reuse it,
   claim it, acquire needed locks, and record all later memory against it.
3. If there is no matching task and the request is real repository work, create
   the owning task before or immediately after the first safe inspection step.
4. "Real repository work" includes implementation, bug fixing, refactoring,
   tests, documentation that decides or changes project direction, planning
   artifacts, technical decisions, architecture notes, dependency choices,
   schema/API changes, release/deployment work, and any multi-step investigation
   whose result should be remembered.
5. Tiny mechanical requests may skip task creation only when they are clearly
   disposable and do not create a decision, product direction, or future
   coordination need. Even then, record semantic memory only if it can be tied
   to a suitable existing task or clearly queued as lightweight context.
6. If TaskPilot is unavailable, unauthorized, or not enabled, say so, continue
   only as far as the user's request safely allows, and record the failed
   TaskPilot action in your final response or handoff.

# Task Intelligence Rules

Before creating a TaskPilot task or recording repo memory:

1. Inspect the active task context, likely current repo task, related tasks,
   recent memory, changed files, and existing relationships.
2. Prefer an existing task when the objective, active session, semantic memory,
   or changed files match the current work.
3. Create a subtask for a smaller piece of an existing objective.
4. Create a new task only for a distinct outcome, and connect it to related
   work with `parent_of`, `subtask_of`, `related_to`, `depends_on`, `blocks`,
   `continues`, `duplicates`, or `supersedes` when relevant.
5. Treat the first meaningful work in a newly enabled repo as a distinct
   outcome that needs an initial owning task unless a matching task already
   exists.
6. Use one primary task for one logical outcome. Keep follow-up prompts in the
   same session on that task when they deepen the same outcome, but create a new
   related task when the user switches to a different deliverable, decision,
   bug, feature, or investigation.
7. A prompt that creates or updates a technical decision, planning document,
   architecture direction, API contract, schema, dependency choice, or release
   choice must have an owning task. Do not record only semantic memory for that
   work.
8. Record semantic memory against the task that owns the work, not a fresh
   file-based task and not an unassigned repo-level note when a task should
   exist.
9. Use outcome-based task names such as `Define Snake game technical decisions`
   or `Fix semantic memory routing for active repo tasks`; avoid names like
   `Update controls.md`, `Update technicaldecisions.md`, or `Inferred work on 6
   changed files`.
10. Include the intended outcome, reasoning, verification, files, task
    selection or creation rationale, and remaining work when recording semantic
    memory.
11. Improve inferred task titles, goals, scope, and relationships when better
    context becomes available.
12. If a task decision is ambiguous, choose the safest auditable action: reuse
    high-confidence matches, create a related task for a distinct moderate-
    confidence outcome, and document why.

Session boundary examples:

- Same task: "create planning.md for Snake" followed by "make an initial brief
  of technicaldecisions.md from planning.md"; both belong to the same game
  planning outcome unless the repo already has a separate technical-decisions
  task.
- New related task: after planning the Snake game, the user asks to implement
  gameplay, fix deployment, change authentication, or choose a production
  persistence architecture.
- Subtask: the user asks for a smaller bounded part of a known parent outcome,
  such as "add keyboard input handling" under an implementation task.
- No new task: the user asks a one-line question, requests a read-only status
  check, or asks for a disposable local scratch file with no future coordination
  value.
