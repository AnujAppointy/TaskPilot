# TaskPilot Agent Rules

This repository uses TaskPilot for human-agent coordination.

When the user gives you a TaskPilot task ID:

1. Run `taskpilot task show <task-id> --json` before starting.
2. Claim the task before editing.
3. Acquire locks for files, artifacts, or semantic areas you will touch.
4. Send heartbeat while actively working, or use `taskpilot run <task-id> -- <agent-command>`.
5. Append sanitized findings, decisions, risks, blockers, and output references.
6. Do not upload raw local files, secrets, prompts, logs, screenshots, or customer data unless explicitly approved.
7. Prepare a handoff if stopping before completion.
8. Mark complete only when the task completion criteria are satisfied.

# Task Intelligence Rules

Before creating a TaskPilot task or recording repo memory:

1. Inspect the active task context, likely current repo task, related tasks, recent memory, changed files, and existing relationships.
2. Prefer an existing task when the objective, active session, semantic memory, or changed files match the current work.
3. Create a subtask for a smaller piece of an existing objective.
4. Create a new task only for a distinct outcome, and connect it to related work with `parent_of`, `subtask_of`, `related_to`, `depends_on`, `blocks`, `continues`, `duplicates`, or `supersedes` when relevant.
5. Record semantic memory against the task that owns the work, not a fresh file-based task.
6. Use outcome-based task names such as `Fix semantic memory routing for active repo tasks`; avoid names like `Update controls.md` or `Inferred work on changed files`.
7. Include the intended outcome, reasoning, verification, files, and remaining work when recording semantic memory.
8. Improve inferred task titles, goals, scope, and relationships when better context becomes available.

# Codebase Exploration Rules

This repository is indexed using Codebase Memory MCP.

Before broad repository exploration:

1. Use the `codebase-memory` MCP to locate relevant modules, symbols, callers, callees, routes, and dependency paths.
2. Use graph results to narrow down which files need to be inspected.
3. Read the actual source files before editing them.
4. Treat source code, tests, and runtime behaviour as authoritative.
5. Use Codebase Memory for impact analysis before modifying important functions.
6. After structural changes, confirm that new or modified symbols are visible in the graph.
7. Run relevant tests after all changes.

Use Codebase Memory especially for:

- understanding unfamiliar architecture;
- tracing feature flows;
- finding callers and dependencies;
- identifying affected modules;
- checking the blast radius of a change;
- locating related routes and tests.

Do not use it as a replacement for:

- reading exact implementation details;
- running tests;
- checking runtime errors;
- reviewing Git changes.
