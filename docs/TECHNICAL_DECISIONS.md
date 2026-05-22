# TaskPilot Technical Decision Document

## Purpose

This document explains the main technology choices behind TaskPilot and why they fit the product goal: coordinating humans and AI agents working across different machines without making every agent depend on one vendor, IDE, or runtime.

TaskPilot is designed as a task-centric coordination layer. The system stores shared task state, ownership, locks, handoffs, decisions, context, artifacts, and audit events so agents such as Codex and Gemini can continue each other's work with less human translation.

## High-Level Architecture

```text
Codex / Gemini / Other Agent
        |
        v
TaskPilot CLI / taskpilot run
        |
        v
TaskPilot REST API
        |
        v
Go Coordination Server
        |
        v
SQLite or Postgres Database
        ^
        |
React Dashboard
```

The CLI and dashboard are two interfaces over the same backend. Agents mostly use the CLI and `taskpilot run`; humans mostly use the dashboard.

## Technology Decisions

### 1. Go For Backend And CLI

**Decision:** Use Go for the TaskPilot server and CLI.

**Why:**

- Go produces a single portable binary, which is useful for internal tools.
- The same language can power both the server and CLI.
- Go is strong for long-running daemons, HTTP APIs, background workers, and filesystem/process integration.
- The standard library is enough for much of the MVP: HTTP server, JSON, process execution, signals, file handling, and concurrency.
- It is easier to distribute to Mac, Windows, and Linux developer machines than a large runtime-heavy tool.

**Value to TaskPilot:**

- Developers can install one `taskpilot` binary.
- Agents can call a simple CLI from any repo.
- `taskpilot run` can wrap child agent processes, inject context, send heartbeats, and collect structured output.

**Trade-off:**

- Go is not the fastest language for building complex UI-heavy logic, so the dashboard is handled separately in frontend code.

### 2. React Dashboard

**Decision:** Use a React-based dashboard served by the Go server.

**Why:**

- Managers and tech leads need visibility without terminal commands.
- React is familiar and good for interactive task boards, filters, detail panels, forms, and real-time updates.
- Serving the dashboard from the Go server keeps deployment simple for the MVP.

**Value to TaskPilot:**

- Humans can see task status, context, owners, locks, handoffs, conflicts, decisions, and artifacts.
- Leads can resolve coordination issues without using CLI commands.
- Dashboard actions call the same API as the CLI, so UI and CLI stay consistent.

**Trade-off:**

- A richer frontend introduces UI state management concerns. For example, polling must not reset form inputs while a user is typing.

### 3. REST API As The Shared Interface

**Decision:** Use REST JSON APIs as the main interface between CLI, dashboard, and server.

**Why:**

- REST is simple, debuggable, and language-agnostic.
- Any agent provider or tool can call HTTP without needing a vendor-specific SDK.
- The dashboard and CLI can share the same backend behavior.
- JSON fits structured task context well.

**Value to TaskPilot:**

- Codex, Gemini, scripts, dashboard, and future integrations can all use the same task system.
- It keeps TaskPilot interoperable across agent providers.

**Trade-off:**

- REST alone does not provide live updates. The MVP uses polling, with a path toward Server-Sent Events for better real-time behavior.

### 4. SQLite For Local Development

**Decision:** Use SQLite as the default local database.

**Why:**

- SQLite requires no separate database server.
- It is easy to run on a developer laptop.
- It works well for local demos, MVP validation, and small internal deployments.
- It keeps setup lightweight while the product shape is still evolving.

**Value to TaskPilot:**

- New users can start quickly.
- The system can be tested without Docker or cloud infrastructure.
- Local-first development remains simple.

**Trade-off:**

- SQLite is not ideal for larger multi-user production workloads with heavy concurrency.

### 5. Postgres For Production

**Decision:** Support Postgres for production-style deployments.

**Why:**

- Postgres is better for concurrent team usage.
- It has mature backup, restore, monitoring, and operations support.
- It is the safer long-term choice for shared team infrastructure.

**Value to TaskPilot:**

- Teams can move from a local MVP to a shared internal server without changing the task model.
- The same APIs and dashboard work over a more production-ready database.

**Trade-off:**

- Postgres adds operational complexity compared with SQLite, so SQLite remains the default local mode.

### 6. `taskpilot run` As The Agent Wrapper

**Decision:** Make `taskpilot run <task-id> -- <agent-command>` the primary agent workflow.

**Why:**

- Relying on every agent to remember manual CLI commands is brittle.
- Some agent runtimes may not be able to reach localhost or read the same shell config as the parent terminal.
- The wrapper can fetch task context from the server before launching the agent.
- The wrapper can inject context files, collect agent updates, send heartbeats, and mark final status.

**Value to TaskPilot:**

- Agents no longer need humans to paste task context manually.
- Codex on Mac can continue from Gemini on Windows using related task context.
- The parent wrapper handles server communication, so the child agent can work even if direct CLI/server access is limited.

**Trade-off:**

- The wrapper must understand common agent command behavior. For known commands such as `codex` and `gemini`, TaskPilot injects a startup prompt directly.

### 7. Context Files For Agent Injection

**Decision:** Inject task context through temporary local files:

```text
TASKPILOT_TASK_CONTEXT_FILE
TASKPILOT_RELATED_CONTEXT_FILE
TASKPILOT_RUN_CONTEXT_FILE
```

**Why:**

- Environment variables are too small and easy for agents to ignore.
- Large JSON context is easier to inspect as a file.
- Some child agents cannot reliably call the TaskPilot server directly.
- Files make the workflow provider-neutral.

**Value to TaskPilot:**

- Current task context is available before the agent starts work.
- Relevant parent or prior task context is available without fetching the whole database.
- Agents can write structured progress back while they work.

**Trade-off:**

- Temporary files are cleaned up after the run, so debugging generated context requires either logs or a future `--keep-context` option.

### 8. Selective Related Context Instead Of Full Memory

**Decision:** Fetch only relevant related task context, not every task.

**Why:**

- Pulling all task history would create noise.
- It could expose unrelated context.
- It would overload the agent with unnecessary information.
- Most continuation work only needs the current task, linked tasks, and prior work with overlapping scope.

**Current selection signals:**

- Direct parent/subtask/dependency relation.
- Same project.
- Same repository.
- Overlapping scope, such as `README.md`.
- Recent updates.
- Completed prior work.

**Value to TaskPilot:**

- Agents receive useful continuity without drowning in irrelevant task history.
- Human trust boundaries are preserved because unrelated work is not injected by default.

**Trade-off:**

- Relevance scoring is heuristic. Over time it may need stronger semantic matching, explicit task links, or user-controlled context policies.

### 9. Structured Task Context

**Decision:** Store structured context entries rather than unstructured transcripts.

Supported context kinds include:

```text
summary
decision
note
risk
blocker
output_ref
```

**Why:**

- Agents need compact continuation context, not full chat logs.
- Humans need quick status understanding.
- Different context kinds support different dashboard sections and future automation.

**Value to TaskPilot:**

- Handoffs become clearer.
- Future agents can quickly understand what was found, decided, blocked, or produced.
- Dashboard stays readable.

**Trade-off:**

- Agents must write useful structured updates. `taskpilot run` helps by giving explicit accepted formats.

### 10. Locks And Conflict Detection

**Decision:** Add task ownership, locks, and conflict detection as first-class coordination concepts.

**Why:**

- Shared memory alone does not prevent duplicate work.
- Agents need to know when someone else owns a task or file scope.
- Leads need to see collisions before they become git conflicts.

**Value to TaskPilot:**

- One active owner per task.
- File or semantic scopes can be locked.
- The dashboard can explain why conflicts exist and who is involved.

**Trade-off:**

- MVP lock overlap detection is intentionally simple. More advanced file-level and git-aware conflict detection can be added later.

### 11. Dashboard Polling Now, SSE Later

**Decision:** Use polling for MVP dashboard updates, while keeping the system ready for Server-Sent Events.

**Why:**

- Polling is simpler and reliable enough for MVP validation.
- The event model already gives a natural path to SSE.
- Real-time behavior can be improved incrementally.

**Value to TaskPilot:**

- Dashboard can reflect CLI and agent updates without manual refresh.
- The implementation remains simple while product behavior is validated.

**Trade-off:**

- Polling is less efficient than SSE and requires careful UI handling so forms do not reset while users type.

### 12. Team Token, Users, And API Keys

**Decision:** Keep legacy team-token login for development, and add user/API-key authentication for more realistic internal usage.

**Why:**

- Team token keeps local demos simple.
- Human users and agent API keys are needed for accountability.
- Different actors need different permissions and audit trails.

**Value to TaskPilot:**

- Dashboard users can log in.
- Agents can authenticate with API keys.
- Events and context entries can be tied to a human or agent identity.

**Trade-off:**

- This is not yet a full enterprise identity system. SSO/OIDC can come later if needed.

## Why This Stack Fits The Main Problem

The main problem is not only storing memory. It is coordinating distributed human-agent work across machines.

TaskPilot needs to support:

- Shared task state.
- Agent-to-agent handoff.
- Human dashboard visibility.
- Ownership and locks.
- Conflict detection.
- Structured context transfer.
- Local-first operation.
- A future path to shared infrastructure.

The chosen stack supports that path:

```text
Go binary
  -> easy local installation and agent wrapping

REST JSON API
  -> interoperable with any agent/tool

React dashboard
  -> human visibility and governance

SQLite
  -> easy local-first start

Postgres
  -> production shared-server scale

taskpilot run
  -> automatic agent coordination

context files
  -> reliable provider-neutral context injection
```

## Alternatives Considered

### Browser-only app

Rejected because agents need a CLI/API wrapper and filesystem/process integration.

### SaaS-first cloud service

Deferred because the initial requirement is local-first and internal-team friendly.

### Agent-provider-specific SDK

Rejected because TaskPilot must work across Codex, Gemini, and future agents.

### Git-only coordination

Rejected because git can show code changes, but not ownership, handoff rationale, decisions, blockers, or live task status.

### Slack/Chat-based coordination

Rejected as the canonical system because chat is not structured enough for reliable agent continuation.

## Current Known Trade-Offs

- Related context selection is useful but heuristic.
- Dashboard polling works, but SSE would provide cleaner real-time updates.
- Temporary context files are removed after the run.
- Some advanced security features are intentionally not prioritized yet because this is currently an internal tool.
- Artifact sharing is reference-first; raw artifact upload needs stricter approval and audit before broad use.

## Future Technical Direction

Recommended next technical improvements:

1. Add `taskpilot run --keep-context` for debugging injected context files.
2. Add Server-Sent Events for live dashboard updates.
3. Improve related-context relevance with explicit task links and semantic tags.
4. Add stronger git integration for branch, PR, and changed-file tracking.
5. Add production deployment hardening around backups, metrics, and operational docs.
6. Add richer MCP support so agents can coordinate through tools instead of shell commands.

