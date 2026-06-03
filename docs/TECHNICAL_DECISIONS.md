# TaskPilot Technical Decisions

This document explains what TaskPilot is built with and why. The goal is to keep the system easy to run inside a company while still being useful for real multi-agent software work.

## Product Goal

TaskPilot coordinates humans and AI agents around structured tasks.

It is not only "shared memory." It is a work hook around agent sessions:

```text
taskpilot run <task-id> -- codex "prompt"
```

That wrapper lets TaskPilot claim work, inject task context, send heartbeats, create locks, collect progress, validate handoff memory, and keep the dashboard updated.

## Current Architecture

```mermaid
flowchart TD
  Browser["Dashboard<br/>human workflow"] --> API["REST API"]
  CLI["TaskPilot CLI"] --> API
  Run["taskpilot run"] --> API
  Run --> Agent["Child agent<br/>Codex / Gemini"]
  Agent --> Files["Run context file<br/>Handoff file"]
  Files --> Run
  API --> Server["Go coordination server"]
  Server --> Store[("SQLite dev<br/>Postgres shared server")]
  Server --> Audit["Events / audit log"]
  Server --> Locks["Locks + conflicts"]
  Server --> Memory["Task memory + decisions + handoffs"]
```

## Main Technical Choices

### Go for backend and CLI

**Decision:** Build the server and CLI in Go.

**Why:**

- One portable binary works well for internal tools.
- Go is good for HTTP servers, CLI tools, subprocess management, signals, filesystem access, and concurrency.
- The same binary can serve the API, dashboard, migrations, backup, admin commands, and agent wrapper.
- Distribution is simpler across Mac, Windows, and Linux.

**Value:**

Agents can run one command from any repo:

```bash
taskpilot run <task-id> -- codex "prompt"
```

**Trade-off:**

Go is not ideal for complex frontend UI, so the dashboard uses browser JavaScript served by the Go server.

### REST JSON API

**Decision:** Use REST JSON as the shared interface for CLI and dashboard.

**Why:**

- Easy to debug with `curl`.
- Works with any language or agent runtime.
- Keeps Codex, Gemini, dashboard, and scripts on the same behavior path.

**Value:**

Dashboard actions and CLI actions mutate the same task model and write the same audit events.

**Trade-off:**

REST needs polling or SSE for live UI updates. The current dashboard is polling-oriented, with the backend event model ready for SSE later.

### SQLite for local development

**Decision:** Keep SQLite as the default local database.

**Why:**

- Zero database setup.
- Easy demos and local testing.
- Simple backup as a file.

**Value:**

A developer can run TaskPilot quickly on one laptop.

**Trade-off:**

SQLite is not the best shared-team database under heavier concurrency.

### Postgres for shared server use

**Decision:** Support Postgres through `TASKPILOT_DB_URL`.

**Why:**

- Better concurrency for multiple laptops and agents.
- Better operational support for backup, restore, monitoring, and Docker deployment.
- Fits the shared-server model.

**Value:**

The team can use one TaskPilot server from Mac and Windows machines.

**Trade-off:**

Postgres adds deployment complexity, so SQLite remains useful for local development.

### Dashboard served by the Go server

**Decision:** Serve the dashboard from the TaskPilot server.

**Why:**

- One server is easier for teams to run.
- The dashboard naturally shares the same auth and API.
- No separate frontend hosting is needed for the MVP/internal tool.

**Value:**

Open:

```text
http://<taskpilot-server>:8080
```

and the team sees the same state as the CLI.

**Trade-off:**

The UI must carefully avoid polling bugs that reset input fields while the user types.

## Agent Wrapper Design

### `taskpilot run`

**Decision:** Make `taskpilot run` the normal way to start agents.

```bash
taskpilot run <task-id> -- codex "your prompt"
taskpilot run <task-id> -- gemini "your prompt"
```

**Why:**

Manual coordination is brittle. Agents may forget to claim tasks, acquire locks, write context, or prepare handoffs.

`taskpilot run` handles the outer workflow:

1. Read current task from server.
2. Claim if available.
3. Start a task session.
4. Move status to `in_progress`.
5. Acquire locks.
6. Start heartbeat.
7. Create injected context files.
8. Inject startup prompt into known agents.
9. Collect run context.
10. Validate handoff file.
11. Return task to `claimed` on exit unless explicitly completed.

**Important lifecycle decision:**

Successful agent exit does not mean task completion.

```text
ready -> claimed -> in_progress -> claimed
```

Completion is deliberate:

```bash
taskpilot task complete <task-id> --summary "Done and verified."
```

**Value:**

An interrupted session does not accidentally mark work complete.

### Prompt injection

**Decision:** For known agents like Codex and Gemini, TaskPilot combines the TaskPilot startup prompt with the human work-unit prompt.

Example command:

```bash
taskpilot run task_123 -- codex "Add a technology section to PLANNING.md"
```

The injected prompt includes:

```text
Work on the current TaskPilot task.
...
Human prompt for this work unit:
Add a technology section to PLANNING.md
```

TaskPilot also prints the injected prompt file path:

```text
TaskPilot: injected task context into codex prompt. Full injected prompt: /tmp/taskpilot-...-prompt-....txt
```

**Why:**

Agents should not guess from repo-local databases or stale chat memory. The server task is authoritative.

**Trade-off:**

The child agent still needs to follow instructions. TaskPilot now validates the handoff file at exit and warns if the agent did not produce useful memory.

### Context files

**Decision:** Use temp files for injected context and agent output.

TaskPilot creates:

```text
TASKPILOT_TASK_CONTEXT_FILE
TASKPILOT_RELATED_CONTEXT_FILE
TASKPILOT_RUN_CONTEXT_FILE
TASKPILOT_HANDOFF_FILE
TASKPILOT_AGENT_PROMPT_FILE
```

**Why files instead of only env vars:**

- JSON context can be larger than comfortable environment variables.
- Files are provider-neutral.
- Agents can inspect them easily.
- The parent wrapper can still communicate with the server even if the child agent cannot.

**Value:**

Codex on Mac and Gemini on Windows can receive the same task truth from the server.

**Trade-off:**

Temp files are normally cleaned up. If the handoff is weak or invalid, TaskPilot keeps the handoff file on disk for repair.

## Handoff Memory Design

### Agent-authored handoff file

**Decision:** Prefer an agent-authored `TASKPILOT_HANDOFF_FILE` over rule-based handoff inference.

**Why:**

Rule-based generation can miss important reasoning. The working agent knows what it did, why it did it, and what remains.

The required handoff sections are:

- Completed Work
- Important Decisions
- Current State
- Remaining Work
- Suggested Next Steps
- Handoff Message

If no meaningful decision was made, the agent must explicitly write:

```text
No material decision made; work followed existing requirements.
```

**Value:**

The next agent gets useful continuation memory instead of generic `None recorded` sections.

### Work-unit checkpoints

**Decision:** Replace time-based handoff syncing with explicit checkpoints.

Command:

```bash
taskpilot handoff checkpoint <task-id> --file "$TASKPILOT_HANDOFF_FILE"
```

**Why:**

Time-based sync can capture half-written or noisy handoff content. A checkpoint represents a completed prompt response or meaningful unit of work.

**How checkpoints are merged:**

- Completed work accumulates.
- Important decisions accumulate.
- Current state comes from the latest checkpoint.
- Suggested next steps come from the latest checkpoint only.
- Older next steps stay in the checkpoint timeline.

```mermaid
sequenceDiagram
  participant Agent as Agent
  participant File as TASKPILOT_HANDOFF_FILE
  participant CLI as taskpilot checkpoint
  participant API as TaskPilot API
  participant DB as Database

  Agent->>File: update after work unit 1
  Agent->>CLI: handoff checkpoint
  CLI->>API: POST /handoff-checkpoints
  API->>DB: save checkpoint 1
  Agent->>File: update after work unit 2
  Agent->>CLI: handoff checkpoint
  CLI->>API: POST /handoff-checkpoints
  API->>DB: save checkpoint 2
  API->>DB: rebuild latest handoff draft
```

**Value:**

The handoff timeline is chronological and action-oriented.

### Handoff validation

**Decision:** Validate handoff quality before trusting it.

TaskPilot warns if:

- Completed work is empty or placeholder text.
- Important decisions are missing.
- Remaining work is missing.
- Handoff message is missing.
- No checkpoint reached the server.

Example:

```text
TaskPilot handoff needs attention before another agent can continue reliably:
  - Completed Work: completed work is required
  - Important Decisions: important decisions are required
```

**Value:**

Weak handoffs do not silently become the source of truth.

**Trade-off:**

This cannot force a model to write perfect content, but it makes failure visible and repairable.

## Task Lifecycle

```mermaid
stateDiagram-v2
  [*] --> ready
  ready --> claimed: claim
  claimed --> in_progress: taskpilot run starts
  in_progress --> claimed: run exits
  claimed --> blocked: manual
  in_progress --> blocked: manual
  claimed --> handoff_ready: publish handoff
  in_progress --> handoff_ready: manual
  handoff_ready --> claimed: accept handoff
  claimed --> in_review: manual
  in_review --> completed: manual complete
  claimed --> completed: explicit complete
  ready --> cancelled: manual
  claimed --> cancelled: manual
```

**Decision:** Completion is manual-only.

**Why:**

An agent session may stop because of interruption, failure, or partial work. That should not mark the task completed.

## Locking And Conflict Design

**Decision:** Track ownership and locks separately.

Ownership says who owns the task.

Locks say what the owner is touching.

Lock fields include:

- Task ID.
- Scope.
- Scope type.
- Owner ID and name.
- Created time.
- Last heartbeat.
- Expiry.
- Status: active, stale, released, overridden.
- Release or override reason.

**Why:**

A task owner may touch one or more areas. Another task may overlap that scope.

**Conflict behavior:**

- Active tasks are checked for ownership and lock conflicts.
- Completed and cancelled tasks are hidden from open conflicts.
- Stale claims include reason, threshold, owner, last activity, and suggested actions.

**Value:**

Leads can resolve collisions before they become code conflicts.

## Markdown And JSON Sync

**Decision:** Store structured JSON as source of truth, but let the UI display and edit Markdown.

**Why:**

Developers read Markdown more easily than raw JSON. The backend still needs structured fields for validation, filtering, and handoff generation.

**Flow:**

```text
JSON -> render Markdown -> user edits Markdown -> strict parser -> JSON -> normalized Markdown
```

**Validation rules:**

- Required top-level heading.
- Known sections.
- Duplicate section errors.
- Required sections for publish.
- List sections use `- item`.
- Unknown sections must be under `Extra Sections`.

**Value:**

The UI is readable, while the backend remains structured.

## Related Context Selection

**Decision:** Inject selected related context, not every task.

TaskPilot selects related context from:

- Parent task.
- Subtasks.
- Dependencies.
- Same project.
- Same repo.
- Overlapping scope.
- Recent relevant work.

**Why:**

Injecting all task history would add noise and may expose unrelated work.

**Value:**

The agent receives useful continuity without drowning in irrelevant context.

**Trade-off:**

Selection is heuristic. Explicit links and better relevance scoring can improve it later.

## Authentication Choices

**Decision:** Support both simple internal token auth and stronger user/API-key auth.

Current options:

- Team token for internal development.
- Actor ID and actor secret for legacy CLI identity.
- API keys for agents.
- Email/password sessions for humans.

**Why:**

The project is currently an internal tool, so the primary focus is coordination behavior. But actor identity and auditability still matter.

**Value:**

Teams can start simple and move toward stronger identity when needed.

## Deployment Choices

### Local development

```bash
taskpilot serve --addr 127.0.0.1:8080 --db taskpilot.db
```

### Docker shared server

```bash
docker compose up --build
```

### Production-like environment

Important env vars:

```text
TASKPILOT_ENV
TASKPILOT_HTTP_ADDR
TASKPILOT_DB_URL
TASKPILOT_SECRET_KEY
TASKPILOT_BASE_URL
TASKPILOT_ARTIFACT_DIR
TASKPILOT_HEARTBEAT_INTERVAL
```

Postgres example:

```text
TASKPILOT_DB_URL=postgres://taskpilot:password@localhost:5432/taskpilot?sslmode=disable
```

## Why These Choices Fit The Main Problem

TaskPilot needs to coordinate agents across machines, not just store notes.

The stack supports that:

```text
Go binary
  easy install and subprocess wrapping

REST API
  shared path for CLI, dashboard, agents, scripts

Dashboard
  human visibility and governance

SQLite
  simple local development

Postgres
  shared team server

taskpilot run
  automatic ownership, prompt injection, heartbeat, handoff validation

Handoff checkpoints
  transfer-ready memory across long-running sessions
```

## Alternatives Considered

### Chat-only coordination

Rejected as the main source of truth because chat is unstructured and easy to lose.

### Git-only coordination

Rejected because git tracks code changes, not ownership, decisions, handoffs, blockers, or task state.

### SaaS-first architecture

Deferred because the current goal is internal self-hosted coordination.

### Agent-provider-specific SDK

Rejected because TaskPilot should work with Codex, Gemini, and future agents.

### Rule-only handoff generation

Rejected as the primary handoff source because it produced weak handoffs. It remains useful as fallback evidence.

## Known Trade-Offs

- Agents can still ignore instructions; TaskPilot now warns when handoff quality is weak.
- Checkpoints are agent-declared, not automatically detected from hidden model internals.
- Related context selection is useful but heuristic.
- Polling works for the dashboard, but SSE would be cleaner.
- The current internal-security posture is practical, not enterprise-grade.
- Raw artifact uploads are intentionally not the default.

## Recommended Next Technical Improvements

1. Add `taskpilot run --keep-context` for easier debugging of injected files.
2. Add Server-Sent Events for live dashboard updates.
3. Add an MCP checkpoint tool so agents can save handoffs without shell commands.
4. Improve related-context scoring with explicit task links and semantic tags.
5. Add a release/version command so teammates can compare installed binaries quickly.
6. Add more dashboard tests around handoff edit and publish flows.
7. Add backup/restore docs for Postgres deployment.
