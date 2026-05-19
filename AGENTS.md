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
