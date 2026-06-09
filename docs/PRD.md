# TaskPilot Product Requirements Document

## 1. Product Summary

TaskPilot is a coordination system for humans and AI coding agents working on the same software projects. It provides a shared server, CLI, and dashboard for managing task ownership, file or semantic locks, durable task memory, agent run sessions, handoffs, conflicts, artifacts, git references, and audit events.

The core product promise is:

```text
TaskPilot makes multi-agent software work observable, resumable, and safer to coordinate.
```

TaskPilot is not only a task board. It is a work wrapper around agent sessions through:

```bash
taskpilot run <task-id> -- <agent-command> [args...]
```

That wrapper claims the task, starts a session, acquires locks, injects context into the child agent, sends heartbeats, imports progress notes, checkpoints handoff memory, and leaves the task in an explicit lifecycle state.

## 2. Target Users

### Human lead

Creates tasks, assigns or reviews work, monitors progress, resolves conflicts, inspects handoffs, and checks audit history from the dashboard.

### Developer

Uses the CLI to claim tasks, acquire locks, append context, attach artifacts, link git metadata, and complete work.

### AI coding agent

Runs under `taskpilot run`, receives task context and handoff instructions, writes progress and handoff notes, and updates the shared task record through the CLI/API.

### Team operator

Runs TaskPilot locally with SQLite or as a shared server with Postgres and Docker.

## 3. Problems

Teams using multiple humans and AI agents often lose track of:

- who owns a task right now
- which files or areas are being changed
- what decisions were already made
- whether an agent is still active or stale
- what changed during an agent session
- how another actor should resume partially completed work
- which conflicts require human resolution

Without a shared coordination layer, agents can duplicate work, overwrite each other, forget context, or mark work complete prematurely.

## 4. Goals

- Provide one shared source of truth for task state across dashboard, CLI, and agent runs.
- Make ownership explicit through task claims and owner fields.
- Reduce unsafe parallel edits through locks and conflict records.
- Preserve task memory through context, decisions, comments, artifacts, git refs, snapshots, and handoff packets.
- Support resumable agent workflows through injected context files and checkpointed handoff files.
- Keep completion explicit; successful agent exit should not automatically mean the task is done.
- Support local development with SQLite and shared team deployment with Postgres.
- Keep the product usable from any repository by installing `taskpilot` on PATH.

## 5. Non-Goals

- TaskPilot does not replace GitHub, Git, or CI.
- TaskPilot does not execute code review itself.
- TaskPilot does not store raw local files or logs as a default workflow.
- TaskPilot does not currently implement granular role-based authorization; scope checks are wired but permissive in the current server.
- TaskPilot does not currently provide a separate frontend service; the dashboard is embedded in the Go server.

## 6. Current Feature Set

### Authentication and identity

- Email/password signup and login for dashboard users.
- HTTP-only `taskpilot_session` cookie for user sessions.
- Actor identities for agents, developers, and legacy CLI usage.
- Actor secrets stored as hashes.
- User-owned actors can be created, updated, deleted, and reset from the dashboard/API.

### Project organization

- Projects group task work.
- Repositories attach project-specific repo metadata such as name, path, and default branch.
- Workspaces identify machines or working contexts, optionally tied to actors.
- A default project is automatically created for backward compatibility.

### Task management

Tasks include:

- title, goal, type, status, priority
- project, repository, workspace, and parent task links
- owner and claim expiration
- scope, requirements, completion criteria, risks, blockers
- derived counts for locks, conflicts, subtasks, dependencies, and handoffs

Supported task lifecycle states:

- `ready`
- `claimed`
- `in_progress`
- `blocked`
- `handoff_ready`
- `in_review`
- `completed`
- `cancelled`

### Subtasks and dependencies

- A task can have subtasks.
- A task can depend on another task.
- The detail view shows blocked-by and blocking relationships.

### Ownership and sessions

- Actors claim tasks before work.
- Claims have a default TTL of 15 minutes.
- Active sessions move tasks to `in_progress`.
- Finishing a session returns the task to `claimed` unless explicit completion is requested.
- Heartbeats refresh task and lock freshness.

### Locks and conflicts

- Locks cover scopes such as files, globs, semantic areas, artifacts, or tasks.
- Locks have a default TTL of 30 minutes.
- Lock acquisition checks overlapping active locks.
- Overlap creates an open `lock_overlap` conflict.
- Stale locks and stale claims are discoverable.
- Conflicts can be resolved by continuing current ownership, transferring, splitting scope, pausing, duplicating, or escalating.

### Durable memory

TaskPilot stores:

- context entries
- decision records
- comments
- artifacts
- git refs
- context snapshots
- handoff packets
- handoff checkpoints
- audit events

### Handoff workflow

- Current actor prepares a handoff with summary and next steps.
- Handoff packets can be generated, edited, checkpointed, published, accepted, or rejected.
- Packets include structured sections such as completed work, current state, decisions, affected files, risks, blockers, remaining work, and suggested next steps.
- Markdown validation errors are stored with packets/checkpoints.

### Agent run wrapper

`taskpilot run`:

1. Loads CLI config.
2. Flushes queued handoff checkpoints.
3. Fetches task detail.
4. Claims the task if needed.
5. Acquires locks for task scope.
6. Starts a task session.
7. Creates local temp files for task context, related context, run context, handoff draft, and startup prompt.
8. Injects a startup prompt into known agent commands unless disabled.
9. Runs the child command with TaskPilot environment variables.
10. Sends heartbeats and imports progress while the child runs.
11. Checkpoints handoff changes.
12. Imports final context and touched file summaries.
13. Finishes the session.
14. Completes only when `--complete` is explicitly passed.

### Offline checkpoint outbox

If handoff checkpoint upload fails due to a retriable network/server issue, the CLI queues the checkpoint locally and retries on later runs or through:

```bash
taskpilot handoff sync --watch
```

### Dashboard

The embedded dashboard supports:

- authentication
- task board and filters
- task creation and detail editing
- memory, decisions, comments, artifacts, git refs, locks, handoffs, and timeline
- projects, repositories, and workspaces
- conflict and stale claim visibility
- actor management and CLI setup commands
- settings and selected project persistence
- event stream refresh with polling fallback behavior

### Operations

- `taskpilot serve` starts the HTTP server.
- `taskpilot migrate status|up` opens the configured store and runs migrations.
- `taskpilot backup create` copies a SQLite database file.
- Health endpoints: `/healthz`, `/readyz`.
- Metrics endpoint: `/metrics`.

## 7. Key User Flows

### Human creates work

1. Human logs into dashboard.
2. Human creates or selects project/repo/workspace.
3. Human creates a task with goal, priority, scope, requirements, and completion criteria.
4. Human watches the task move across lifecycle states.

### Agent performs work

1. Agent or human starts `taskpilot run`.
2. TaskPilot claims and locks work.
3. Agent reads injected task context.
4. Agent works in the repository.
5. TaskPilot records heartbeats, context, checkpoints, and events.
6. Agent exits.
7. Task returns to `claimed` unless explicitly completed.

### Handoff between agents

1. Current actor updates handoff draft.
2. Actor checkpoints or publishes handoff packet.
3. Task enters `handoff_ready`.
4. Next actor accepts handoff.
5. Ownership transfers and the next actor continues from shared memory.

### Conflict resolution

1. Actor requests a lock.
2. Server detects overlap with another active lock.
3. Server records an open conflict.
4. Human or actor resolves with a recorded resolution.
5. Task, lock, or owner state changes according to the resolution.

## 8. Functional Requirements

- The system must expose all normal coordination operations through the HTTP API.
- The CLI and dashboard must use the same API and shared data model.
- Task creation must support project, repo, workspace, parent task, scope, requirements, completion criteria, risks, and blockers.
- Claiming must prevent non-force takeover of an actively owned task.
- Heartbeats must refresh task claims and active locks.
- `taskpilot run` must not complete a task unless `--complete` is used.
- Lock acquisition must detect active overlapping locks owned by other actors.
- Handoff packets must be editable and publishable.
- Handoff checkpoints must support offline queueing and later sync.
- Events must be recorded for meaningful mutations.
- SQLite and Postgres must share the same store-level business behavior.

## 9. Non-Functional Requirements

- The product should run as a single Go binary.
- Local setup should work without external services.
- Shared team setup should support Postgres.
- CLI commands should be scriptable and JSON-friendly where applicable.
- Agent run behavior should tolerate child process failure and preserve useful handoff context.
- Production mode should require a sufficiently strong secret key.
- Secrets must not be returned in normal API responses except one-time generated actor secret values.

## 10. Current Risks and Gaps

- `requireScope` currently authenticates but does not enforce per-scope authorization.
- Migrations are inline and adaptive rather than versioned migration files.
- There are no declared foreign-key constraints in table creation.
- The dashboard is a plain embedded JavaScript app, so frontend complexity can grow quickly.
- Search is represented as derived `search_text` on tasks, but there is no dedicated full-text index.
- Some docs mention older API-key/admin command concepts that are not active in the current visible command set.

## 11. Success Metrics

- Agents start work through `taskpilot run` instead of unwrapped shell commands.
- Most active tasks have an owner, recent heartbeat, and appropriate locks.
- Partial work has useful handoff checkpoints before a session ends.
- Conflicts are visible and resolved through TaskPilot rather than private messages.
- A new actor can resume a task from TaskPilot memory without needing prior chat context.

