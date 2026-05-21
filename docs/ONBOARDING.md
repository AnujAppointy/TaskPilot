# TaskPilot Onboarding Guide

TaskPilot is a coordination system for teams that use AI agents across different machines. It helps humans and agents work from the same task context instead of passing status, decisions, and handoffs manually through chat.

The simplest way to understand TaskPilot is:

```text
TaskPilot is the shared work memory for humans, Codex, Gemini, and other agents.
```

Agents can work from the CLI. Managers and tech leads can use the dashboard. Both interfaces read and write the same task state.

## Why TaskPilot Exists

Without TaskPilot, distributed agent work usually breaks in predictable ways:

- Two agents start the same work without knowing about each other.
- A developer stops an agent session and the next person does not know what was decided.
- Context stays inside one terminal session and never reaches the rest of the team.
- Managers need to ask people for status because the CLI is invisible.
- Handoffs depend on humans summarizing everything correctly.
- Parallel work creates file or scope conflicts late, usually at git merge time.

TaskPilot solves this by making the task the shared coordination unit. Every task has a goal, owner, status, scope, context, decisions, locks, handoffs, artifacts, and timeline.

## What TaskPilot Can Do

TaskPilot currently supports:

- Shared task board for humans and agents.
- CLI for agent workflows.
- Dashboard for managers, tech leads, and reviewers.
- Actor registration for humans and agents.
- Task ownership and claim protection.
- Heartbeats for active agent sessions.
- Locks for files, semantic areas, and artifacts.
- Conflict detection for overlapping work.
- Context entries for summaries, notes, decisions, risks, blockers, and output references.
- First-class decision records.
- Human comments separate from agent context.
- Handoffs between agents or humans.
- Subtasks and task dependencies.
- Artifact references for PRs, logs, docs, outputs, and screenshots.
- Git metadata for branches, commits, PR URLs, and changed files.
- Postgres-backed shared server for team use.
- `taskpilot run` wrapper so agents can coordinate automatically while they work.

## Mental Model

TaskPilot has three main parts:

```text
Shared TaskPilot Server
  Stores task state, context, events, locks, conflicts, handoffs, and actors.

CLI
  Used by agents and power users.
  Example: taskpilot run <task-id> -- codex

Dashboard
  Used by managers, tech leads, reviewers, and humans.
  Shows the same task state as the CLI.
```

The CLI and dashboard are not separate systems. They are two views over the same shared coordination layer.

If an agent updates a task from CLI, the dashboard changes. If a lead resolves a conflict in the dashboard, CLI users see the updated state.

## Core Concepts

### Task

A task is the canonical unit of work.

It carries:

- Title and goal.
- Type such as planning, research, implementation, review, debugging, documentation, or other.
- Status such as ready, claimed, in progress, blocked, handoff ready, in review, completed, or cancelled.
- Priority.
- Owner.
- Scope.
- Context.
- Decisions.
- Locks.
- Handoffs.
- Artifacts.
- Git metadata.
- Event timeline.

Value:

The task lets another human or agent understand what is happening without asking for a manual explanation.

### Actor

An actor is a human or agent identity.

Examples:

```text
Codex CLI - Mac
Gemini CLI - Windows
Anuj - Tech Lead
```

Value:

TaskPilot knows who owns work, who added context, who accepted a handoff, and which machine or agent is active.

### Ownership

Only one active owner should work on a task at a time.

When an agent runs:

```bash
taskpilot run <task-id> -- codex
```

TaskPilot claims the task for that agent.

Value:

This prevents two agents from unknowingly doing the same task.

### Heartbeat

While an agent is running through `taskpilot run`, TaskPilot sends heartbeats.

Value:

The dashboard can show that the agent is still active. If heartbeats stop, the task can be treated as stale.

### Lock

A lock describes the scope an agent plans to touch.

Examples:

```text
src/auth/*
billing/refunds
release-notes.md
```

Value:

TaskPilot can warn when two agents are working on overlapping areas before the conflict becomes a git merge problem.

### Context

Context is structured information attached to a task.

Examples:

```text
summary: Signup failure happens after expiry validation.
decision: Keep token format unchanged.
risk: Do not change DB schema during this fix.
blocker: Need production invite token sample.
output_ref: PR https://github.com/org/repo/pull/42
```

Value:

The next agent can continue from the previous agent's reasoning without needing a human to translate.

### Decision

Decisions are durable rationale.

Example:

```text
Decision: Keep token format unchanged.
Reason: Existing invite links must continue working.
Impact: Patch only expiry validation.
Alternatives: Rotate tokens, add DB schema column.
```

Value:

Future agents know not only what was chosen, but why.

### Comment

Comments are human discussion and review notes.

Value:

Human discussion stays separate from agent-generated task context.

### Handoff

A handoff is a prepared continuation packet.

It includes:

- Previous owner.
- Next owner if known.
- Resume summary.
- Next steps.

Value:

One agent can stop and another agent can continue without a human rewriting the story.

### Conflict

A conflict means TaskPilot detected competing work.

Common cases:

- Two agents tried to own the same task.
- Two tasks have overlapping locks.
- One task should wait for another task.

Value:

Tech leads can resolve collisions early from the dashboard.

### Artifact

Artifacts are references to work outputs.

Examples:

- PR URL.
- Log link.
- Branch name.
- Design doc.
- Test output.
- Screenshot reference.

TaskPilot stores references by default, not raw local files.

Value:

Handoffs become richer without uploading private local data.

### Git Metadata

Git metadata links implementation work to the task.

Examples:

- Branch.
- Commit SHA.
- PR URL.
- Changed files.

Value:

Managers can see where code work lives without asking the agent or developer.

## Dashboard: What Humans Use

Open the dashboard:

```text
http://<taskpilot-server>:8080
```

For a local Docker setup on the same Mac:

```text
http://127.0.0.1:8080
```

For another laptop on the same network:

```text
http://<mac-lan-ip>:8080
```

### Dashboard Login

For the current Docker setup, use the development token:

```text
change-this-team-token-before-use
```

For local SQLite development, the default token is:

```text
dev-token
```

### Task Board

The task board groups tasks by status:

- Ready
- Claimed
- In progress
- Blocked
- Handoff ready
- In review
- Completed

Use it to answer:

- What is active?
- Who owns what?
- What is blocked?
- Which tasks have conflicts?
- Which tasks are stale?

### Task Detail

Task detail shows:

- Goal.
- Status.
- Owner.
- Project.
- Repository.
- Workspace.
- Scope.
- Subtasks.
- Dependencies.
- Decisions.
- Comments.
- Artifacts.
- Git metadata.
- Context.
- Locks.
- Handoffs.
- Timeline.

Use it to understand the full state of one task.

### Conflicts

The Conflicts page shows:

- Why a conflict happened.
- Which tasks are involved.
- Which actors are involved.
- The overlapping scope.
- Resolution options.

Resolution options include:

- Continue current owner.
- Transfer ownership.
- Split scope.
- Pause secondary work.
- Mark duplicate.
- Escalate to human.

### People / Agents

Shows registered humans and agents.

Use it to check:

- Which agents exist.
- Which machine they belong to.
- Which actor id to use for handoffs.

### Handoffs

Shows prepared handoffs and lets the receiving actor accept them.

Use it when one agent stops and another agent needs to continue.

### Projects

Projects group work by product, team, or codebase.

Repositories and workspaces can also be attached so agents know where work belongs.

### Settings

Settings handles connection identity.

For normal internal testing:

- Use the team token.
- Let the dashboard manage its own actor identity.

Do not manually edit actor credentials unless you are debugging.

## CLI: What Agents Use

The CLI should be available as:

```bash
taskpilot
```

from any repo.

Recommended install pattern:

Mac/Linux:

```bash
mkdir -p ~/.local/bin
ln -sf "/path/to/taskpilot" ~/.local/bin/taskpilot
export PATH="$HOME/.local/bin:$PATH"
```

Windows:

```text
C:\Tools\taskpilot\taskpilot.exe
```

Add this folder to Windows Path:

```text
C:\Tools\taskpilot
```

After setup:

```bash
taskpilot
taskpilot task list
```

should work from any repo.

### CLI Login

Mac server itself:

```bash
taskpilot login --server http://127.0.0.1:8080 --token change-this-team-token-before-use
```

Another laptop on the same network:

```bash
taskpilot login --server http://<mac-lan-ip>:8080 --token change-this-team-token-before-use
```

### Register Agent

Codex on Mac:

```bash
taskpilot actor register --name "Codex CLI - Mac" --kind agent --machine macbook
```

Gemini on Windows:

```powershell
taskpilot actor register --name "Gemini CLI - Windows" --kind agent --machine windows-laptop
```

### Create Task

```bash
taskpilot task create \
  --title "Fix invited-user signup expiry" \
  --goal "Find and fix expiry validation failure for invited users" \
  --type debugging \
  --priority high \
  --scope "src/auth/*"
```

### List Tasks

```bash
taskpilot task list
```

### Show Task

```bash
taskpilot task show <task-id>
```

### Claim Task

```bash
taskpilot task claim <task-id>
```

### Acquire Lock

```bash
taskpilot lock acquire <task-id> --scope "src/auth/*"
```

### Add Context

```bash
taskpilot context append <task-id> \
  --kind decision \
  --content "Keep token format unchanged"
```

### Record Decision

```bash
taskpilot decision add <task-id> \
  --decision "Keep token format unchanged" \
  --reason "Existing invite links depend on it" \
  --impact "Only patch expiry validation"
```

### Add Comment

```bash
taskpilot comment add <task-id> --body "Please review edge cases before merge."
```

### Prepare Handoff

```bash
taskpilot handoff prepare <task-id> \
  --to <actor-id> \
  --summary "Root cause traced to expiry comparison. Token format should stay unchanged." \
  --next "Add failing regression test" \
  --next "Patch expiry comparison"
```

### Accept Handoff

```bash
taskpilot handoff accept <handoff-id>
```

### Add Artifact

```bash
taskpilot artifact add <task-id> \
  --kind pr \
  --title "Signup fix PR" \
  --uri "https://github.com/org/repo/pull/42"
```

### Attach Git Metadata

```bash
taskpilot git link-branch <task-id>
taskpilot git attach-pr <task-id> "https://github.com/org/repo/pull/42"
```

## Agent Automation With `taskpilot run`

The most important command is:

```bash
taskpilot run <task-id> -- <agent-command>
```

Examples:

```bash
taskpilot run <task-id> -- codex
taskpilot run <task-id> -- gemini
```

With this wrapper, TaskPilot automatically:

- Reads the task.
- Claims the task if available.
- Acquires locks based on task scope.
- Starts heartbeat.
- Passes task environment variables to the child agent.
- Provides a context file for the agent to write summaries and decisions.
- Imports context while the agent is running.
- Captures touched files after the command exits.
- Completes, blocks, or leaves the task in progress depending on the run outcome.

The child agent receives:

```text
TASKPILOT_TASK_ID
TASKPILOT_SERVER
TASKPILOT_ACTOR_ID
TASKPILOT_PROJECT_ID
TASKPILOT_REPO_ID
TASKPILOT_WORKSPACE_ID
TASKPILOT_RUN_CONTEXT_FILE
TASKPILOT_AGENT_INSTRUCTIONS
```

Agents should write useful context to:

```text
TASKPILOT_RUN_CONTEXT_FILE
```

Example lines:

```text
summary: Signup failure happens after expiry validation.
decision: Keep token format unchanged.
risk: Do not change DB schema.
blocker: Need production invite sample.
next: Add failing regression test.
```

TaskPilot imports these entries into the task context.

## Two-Agent Scenario: Codex On Mac, Gemini On Windows

This scenario tests the real reason TaskPilot exists.

Goal:

```text
Codex investigates the bug on Mac.
Gemini continues from Codex's decisions on Windows.
Both work in the same git repo without losing context.
```

### Setup

Both laptops clone the same repo.

Mac:

```bash
cd /path/to/repo
taskpilot login --server http://127.0.0.1:8080 --token change-this-team-token-before-use
taskpilot actor register --name "Codex CLI - Mac" --kind agent --machine macbook
```

Windows:

```powershell
cd C:\path\to\repo
taskpilot login --server http://<mac-lan-ip>:8080 --token change-this-team-token-before-use
taskpilot actor register --name "Gemini CLI - Windows" --kind agent --machine windows-laptop
```

### Create Investigation Task

On Mac:

```bash
taskpilot task create \
  --title "Investigate invited-user signup expiry bug" \
  --goal "Find why invited users fail signup after expiry validation." \
  --type debugging \
  --priority high \
  --scope "src/auth/*" \
  --json
```

Save the returned id:

```text
TASK_A=<task-id>
```

### Codex Starts Work

On Mac:

```bash
taskpilot run TASK_A --no-complete --progress-interval 30s -- codex
```

Prompt Codex:

```text
You are working under TaskPilot task TASK_A.

Focus only on src/auth/*.
Find the root cause of invited-user signup expiry failure.
Do not change token format unless the task context explicitly says to.
Write findings, decisions, risks, blockers, and next steps to TASKPILOT_RUN_CONTEXT_FILE.
```

Codex should write context like:

```text
summary: Failure happens after invite token lookup during expiry comparison.
decision: Keep token format unchanged.
risk: Changing DB schema would break existing invite links.
next: Add regression test for expired invited-user signup.
```

Dashboard now shows:

- Task is owned by Codex CLI - Mac.
- Status is claimed or in progress.
- Lock on `src/auth/*`.
- Context from Codex.
- Timeline of activity.

### Gemini Continues From Codex Context

Find Gemini actor id:

```bash
taskpilot actor list
```

Prepare handoff:

```bash
taskpilot handoff prepare TASK_A \
  --to <gemini-actor-id> \
  --summary "Codex traced the failure to expiry comparison after invite token lookup. Token format must stay unchanged." \
  --next "Add failing regression test for expired invited-user signup" \
  --next "Patch expiry comparison only if the test confirms the issue"
```

On Windows:

```powershell
taskpilot handoff accept <handoff-id>
taskpilot run TASK_A --progress-interval 30s -- gemini
```

Prompt Gemini:

```text
Continue TaskPilot task TASK_A from the accepted handoff.

Use Codex's recorded decision:
- Keep token format unchanged.
- Do not change DB schema.
- Add regression coverage first.
- Patch only expiry comparison.

Write final findings and implementation summary to TASKPILOT_RUN_CONTEXT_FILE.
```

Gemini can now continue without asking a human:

```text
What did Codex find?
What did we decide?
What should I avoid changing?
What should I do next?
```

That information is already in TaskPilot.

### Expected Dashboard Result

The task detail should show:

- Codex's summary.
- Codex's decision.
- Handoff from Codex to Gemini.
- Gemini accepting the handoff.
- Gemini's final summary.
- Git metadata or artifacts if attached.
- Completed status when done.

## Conflict Scenario

Create two tasks with the same scope:

```bash
taskpilot task create \
  --title "Patch auth expiry logic" \
  --goal "Patch expiry comparison" \
  --scope "src/auth/*"

taskpilot task create \
  --title "Refactor auth invite flow" \
  --goal "Clean up invite signup path" \
  --scope "src/auth/*"
```

Start Codex on one task:

```bash
taskpilot run <task-1> --no-complete -- codex
```

Start Gemini on the other task:

```bash
taskpilot run <task-2> --no-complete -- gemini
```

TaskPilot should detect overlapping scope.

The dashboard Conflicts page should explain:

- Which tasks overlap.
- Which actors are involved.
- Which scope overlaps.
- What resolution options exist.

The lead can choose:

- Continue current owner.
- Transfer ownership.
- Split scope.
- Pause secondary work.
- Mark duplicate.
- Escalate to human.

## Recommended Team Workflow

For every agent task:

1. Create or select a TaskPilot task.
2. Make sure the task has a clear goal and scope.
3. Run the agent through `taskpilot run`.
4. Let the dashboard show ownership and status.
5. Ask the agent to write decisions and summaries to `TASKPILOT_RUN_CONTEXT_FILE`.
6. Use handoff when another agent or teammate should continue.
7. Attach PR/artifact/git metadata when work is reviewable.
8. Complete the task only when the completion criteria are satisfied.

## What To Tell Agents

Use this prompt when launching Codex, Gemini, or another CLI agent:

```text
You are working inside TaskPilot.

Read TASKPILOT_AGENT_INSTRUCTIONS.
Use TASKPILOT_TASK_ID as the current task.
Focus only on the task scope.
Write useful summaries, decisions, risks, blockers, and next steps to TASKPILOT_RUN_CONTEXT_FILE.
Do not upload raw files or secrets.
If you stop before completing the task, leave a handoff summary.
```

## What TaskPilot Does Not Try To Do

TaskPilot does not replace git, GitHub, Jira, Linear, Slack, or human review.

It coordinates agent work around shared task context.

It does not store raw local files by default.

It does not automatically understand every private thought inside an agent unless the agent writes useful context to the run context file or uses the TaskPilot CLI/API.

## Troubleshooting

### `taskpilot` command not found

TaskPilot is not on PATH.

Install it globally:

```bash
mkdir -p ~/.local/bin
ln -sf "/path/to/taskpilot" ~/.local/bin/taskpilot
export PATH="$HOME/.local/bin:$PATH"
```

Windows:

```text
Put taskpilot.exe in C:\Tools\taskpilot
Add C:\Tools\taskpilot to Path
```

### Dashboard token rejected

Docker default token:

```text
change-this-team-token-before-use
```

Local SQLite default token:

```text
dev-token
```

If browser state is stale:

```js
localStorage.clear()
location.reload()
```

### Windows cannot reach Mac server

Check from Windows:

```powershell
curl http://<mac-lan-ip>:8080/healthz
```

If it fails:

- Confirm both laptops are on the same network.
- Confirm Docker is running on Mac.
- Confirm TaskPilot exposes port `8080`.
- Check macOS firewall.

### Context does not appear while agent is running

Make sure the agent writes to:

```text
TASKPILOT_RUN_CONTEXT_FILE
```

Example:

```text
decision: Keep token format unchanged.
summary: Added failing regression test.
```

TaskPilot imports these lines while the run is active and again when the run exits.

## Success Criteria For A Team

TaskPilot is working well when:

- A lead can see active agent work without opening terminals.
- Two agents do not unknowingly own the same task.
- Overlapping file or scope work is visible early.
- Handoffs carry enough context for another agent to continue.
- Decisions survive beyond one local terminal session.
- PRs, branches, and outputs are linked back to the task.
- Humans can resolve conflicts from the dashboard.
- Agents can use `taskpilot` from any repo without absolute paths.

