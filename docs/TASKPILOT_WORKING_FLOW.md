# TaskPilot Working Flow

This guide explains TaskPilot as a set of small flows. Each diagram is intentionally split by responsibility so a new human or agent can understand the system without reading the code first.

## One-Minute Picture

TaskPilot is the shared coordination layer between humans, agents, and the repository. The dashboard and CLI both talk to the same server, and the server is the source of truth for tasks, owners, locks, memory, handoffs, and audit events.

```mermaid
flowchart TD
  Human["Human lead<br/>uses dashboard"] --> Server["TaskPilot server<br/>coordination brain"]
  Agent["Agent or developer<br/>uses taskpilot CLI"] --> Server
  Runner["taskpilot run<br/>wraps Codex, Gemini, or another agent"] --> Server

  Server --> Task["Task record<br/>goal, scope, owner, status"]
  Server --> Lock["Locks<br/>files, artifacts, semantic areas"]
  Server --> Memory["Shared memory<br/>context, decisions, comments"]
  Server --> Handoff["Handoffs<br/>continue work safely"]
  Server --> Timeline["Audit timeline<br/>what changed and when"]

  Task --> DB[("SQLite or Postgres<br/>single source of truth")]
  Lock --> DB
  Memory --> DB
  Handoff --> DB
  Timeline --> DB
```

Read it as:

1. Humans and agents do not coordinate through private notes.
2. They coordinate through the TaskPilot server.
3. The server records every important ownership, work, memory, and handoff change.
4. The database can be SQLite for local use or Postgres for shared team use.

## Main Actors

```mermaid
flowchart LR
  Lead["Human lead<br/>creates tasks, reviews status,<br/>resolves conflicts"] --> Dashboard["Dashboard UI"]
  Dev["Developer<br/>uses CLI directly"] --> CLI["taskpilot CLI"]
  Codex["Codex"] --> Run["taskpilot run"]
  Gemini["Gemini"] --> Run

  Dashboard --> API["HTTP API"]
  CLI --> API
  Run --> API
  API --> Server["TaskPilot server"]
  Server --> Store["Store layer"]
  Store --> DB[("SQLite or Postgres")]
```

Important idea:

- The dashboard is for visibility and human control.
- The CLI is for task operations from a terminal.
- `taskpilot run` is the agent wrapper that adds claims, locks, heartbeats, context files, and handoff files around the real agent command.
- All normal coordination commands go through the server API.

## Task Lifecycle

Every piece of work starts as a task. A task moves through clear states so everyone can see whether it is free, owned, active, blocked, ready for handoff, under review, or finished.

```mermaid
flowchart TD
  Create["Create task<br/>title, goal, type, priority, scope"] --> Ready["ready<br/>available to pick up"]
  Ready --> Claim["Claim task<br/>actor becomes owner"]
  Claim --> Claimed["claimed<br/>owned but not actively running"]
  Claimed --> Start["Start work session<br/>manual work or taskpilot run"]
  Start --> Progress["in_progress<br/>heartbeats expected"]

  Progress --> Notes["Append memory<br/>findings, decisions, risks,<br/>artifacts, git refs"]
  Notes --> Progress

  Progress --> Blocked["blocked<br/>needs help or external input"]
  Blocked --> Progress

  Progress --> HandoffReady["handoff_ready<br/>another actor can continue"]
  HandoffReady --> Accept["Accept handoff<br/>new actor takes ownership"]
  Accept --> Claimed

  Progress --> Review["in_review<br/>work is ready to check"]
  Review --> Complete["completed<br/>explicit completion action"]

  Claimed --> Release["Release task<br/>owner steps away"]
  Release --> Ready

  Ready --> Cancel["cancelled<br/>work no longer needed"]
  Claimed --> Cancel
  Progress --> Cancel
```

Status meaning:

- `ready`: nobody owns it yet.
- `claimed`: someone owns it, but there is no active run session.
- `in_progress`: work is active and should send heartbeats.
- `blocked`: work cannot continue without help.
- `handoff_ready`: current owner prepared continuation details.
- `in_review`: implementation is ready for review.
- `completed`: task is done only after an explicit complete action.
- `cancelled`: task is intentionally stopped.

## Normal Manual Task Flow

This is the simplest path when a person or agent uses direct CLI commands.

```mermaid
flowchart TD
  Show["1. Inspect task<br/>taskpilot task show TASK_ID"] --> Claim["2. Claim ownership<br/>taskpilot task claim TASK_ID"]
  Claim --> Lock["3. Lock touched scope<br/>taskpilot lock acquire TASK_ID --scope src/auth/*"]
  Lock --> ConflictCheck{"Any active<br/>overlapping lock?"}

  ConflictCheck -- "Yes" --> Conflict["TaskPilot records conflict<br/>and explains current owner"]
  Conflict --> Resolve["Resolve conflict<br/>continue owner, transfer,<br/>split scope, pause, duplicate,<br/>or escalate"]

  ConflictCheck -- "No" --> Work["4. Do work in repo"]
  Work --> Memory["5. Save durable memory<br/>context, decisions, comments,<br/>artifacts, git refs"]
  Memory --> MoreWork{"More work<br/>needed?"}
  MoreWork -- "Yes" --> Work
  MoreWork -- "No" --> FinishChoice{"Finish now<br/>or hand off?"}

  FinishChoice -- "Finish" --> Complete["6A. Complete explicitly<br/>taskpilot task complete TASK_ID"]
  FinishChoice -- "Hand off" --> Handoff["6B. Prepare handoff<br/>summary + next steps"]
```

This flow prevents two people from silently editing the same area and keeps decisions attached to the task instead of buried in chat.

## `taskpilot run` Agent Flow

`taskpilot run` is the most important automation path. It wraps a child agent command with coordination steps before, during, and after the agent works.

```mermaid
flowchart TD
  Command["Human starts<br/>taskpilot run TASK_ID -- codex \"prompt\""] --> LoadConfig["CLI loads config<br/>server URL, actor ID, secret"]
  LoadConfig --> Fetch["Fetch task detail<br/>from server"]
  Fetch --> ClaimCheck{"Task already<br/>claimed by this actor?"}

  ClaimCheck -- "No" --> Claim["Claim task<br/>if available or allowed"]
  ClaimCheck -- "Yes" --> LockScopes["Use existing ownership"]
  Claim --> LockScopes["Acquire task and scope locks"]

  LockScopes --> LockConflict{"Lock conflict?"}
  LockConflict -- "Yes" --> Stop["Stop before unsafe work<br/>show conflict details"]
  LockConflict -- "No" --> Session["Start task session<br/>status becomes in_progress"]

  Session --> Files["Create local temp files<br/>task context, related context,<br/>run notes, handoff draft,<br/>agent prompt"]
  Files --> Env["Set environment variables<br/>TASKPILOT_TASK_ID,<br/>TASKPILOT_SESSION_ID,<br/>TASKPILOT_HANDOFF_FILE,<br/>and more"]
  Env --> Launch["Launch child agent<br/>Codex, Gemini, or command"]

  Launch --> Active["Agent works in repo"]
  Active --> Heartbeat["CLI sends heartbeats<br/>and renews active locks"]
  Active --> Notes["Agent writes incremental notes<br/>to run context file"]
  Active --> HandoffDraft["Agent updates handoff draft<br/>after meaningful work units"]
  HandoffDraft --> Checkpoint["taskpilot handoff checkpoint<br/>saves continuation memory"]

  Heartbeat --> Active
  Notes --> Active
  Checkpoint --> Active

  Active --> Exit["Child command exits"]
  Exit --> Import["CLI imports notes and checkpoints<br/>back to server"]
  Import --> FinishSession["Finish task session"]
  FinishSession --> Claimed["Task returns to claimed<br/>unless explicitly completed"]
```

Key point:

`taskpilot run` may create local temporary files for the child agent, but those files are not the long-term source of truth. The server database remains the shared record.

## Locks And Conflict Flow

Locks make parallel work safer. A lock can cover a file, directory, artifact, task, or semantic area. TaskPilot checks for overlapping active locks before granting a new one.

```mermaid
flowchart TD
  Request["Actor requests lock<br/>scope + scope type"] --> Stale["Server marks expired locks stale"]
  Stale --> Compare["Compare requested scope<br/>with active locks"]
  Compare --> Overlap{"Overlap with<br/>another actor?"}

  Overlap -- "No" --> Grant["Grant lock<br/>status active, TTL set"]
  Grant --> Renew["Renew during heartbeat<br/>extends expiration"]
  Renew --> Release["Release when work ends<br/>or owner steps away"]

  Overlap -- "Yes" --> Record["Record conflict<br/>lock_overlap"]
  Record --> Explain["Return conflict message<br/>current owner and scope"]
  Explain --> Decision{"How should team<br/>resolve it?"}

  Decision -- "Continue current owner" --> Current["Keep owner and note reason"]
  Decision -- "Transfer" --> Transfer["Move ownership and locks<br/>to target actor"]
  Decision -- "Split scope" --> Split["Document narrower scopes"]
  Decision -- "Pause" --> Pause["Mark task blocked"]
  Decision -- "Duplicate" --> Duplicate["Mark duplicate path"]
  Decision -- "Escalate" --> Escalate["Ask human to decide"]
```

Why this matters:

- Agents can work in parallel without guessing who owns a file or area.
- Stale locks can be detected when heartbeats stop.
- Conflict decisions become durable context, not tribal memory.

## Memory Flow

TaskPilot treats context as part of the task, not as disposable chat history.

```mermaid
flowchart LR
  Work["During work"] --> Context["Context entries<br/>summary, finding, risk,<br/>blocker, output reference"]
  Work --> Decision["Decision records<br/>choice, reason, impact,<br/>alternatives"]
  Work --> Comment["Comments<br/>human or agent discussion"]
  Work --> Artifact["Artifacts<br/>PRs, reports, logs,<br/>links to outputs"]
  Work --> Git["Git refs<br/>branch, commit, PR,<br/>changed files"]

  Context --> Detail["Task detail view"]
  Decision --> Detail
  Comment --> Detail
  Artifact --> Detail
  Git --> Detail
  Detail --> Handoff["Handoff packet"]
  Detail --> NextAgent["Next agent prompt context"]
```

This is how the next actor understands:

- what was tried
- what was decided
- what is risky
- what files changed
- what output proves progress
- what still needs attention

## Handoff Flow

Handoff is the continuation path when work should move from one actor to another.

```mermaid
flowchart TD
  Current["Current actor has partial progress"] --> Draft["Update handoff draft<br/>completed work, decisions,<br/>current state, remaining work,<br/>risks, next steps"]
  Draft --> Checkpoint["Checkpoint handoff<br/>during active run"]
  Checkpoint --> Prepare["Prepare or publish handoff<br/>task becomes handoff_ready"]
  Prepare --> Notify["Next actor sees handoff<br/>from dashboard or CLI"]
  Notify --> Review["Next actor reviews packet<br/>and task memory"]
  Review --> AcceptReject{"Accept handoff?"}

  AcceptReject -- "Accept" --> Transfer["Task ownership transfers<br/>locks transfer or refresh"]
  Transfer --> Continue["Next actor continues from context"]

  AcceptReject -- "Reject" --> Back["Handoff marked rejected<br/>current owner or lead must adjust"]
```

A good handoff should answer:

- What is already done?
- What important decisions were made?
- What is the current state?
- What remains?
- Which files or components were touched?
- What risks or blockers exist?
- What should the next actor do first?

## Completion Flow

Completion is intentionally separate from `taskpilot run` finishing. A child agent exiting does not mean the task is complete.

```mermaid
flowchart TD
  Exit["Agent or manual work stops"] --> Claimed["Task remains claimed<br/>owner still responsible"]
  Claimed --> Verify["Owner verifies completion criteria<br/>tests, review, output references"]
  Verify --> Criteria{"Criteria satisfied?"}

  Criteria -- "No" --> Continue["Continue work,<br/>block, or prepare handoff"]
  Criteria -- "Yes" --> Summary["Write completion summary"]
  Summary --> Complete["Explicit complete action<br/>taskpilot task complete TASK_ID"]
  Complete --> Done["Task status completed<br/>timeline records final event"]
```

This prevents accidental completion just because an agent process exited successfully.

## Storage Flow

TaskPilot uses one active storage backend per server process.

```mermaid
flowchart TD
  Start["Start TaskPilot server"] --> Config["Read TASKPILOT_DB_URL<br/>or --db"]
  Config --> Choice{"DB value starts with<br/>postgres:// or postgresql://?"}
  Choice -- "Yes" --> Postgres["Use Postgres<br/>shared/team deployment"]
  Choice -- "No" --> SQLite["Use SQLite file<br/>local/dev deployment"]

  Postgres --> Store["Same store methods<br/>business logic stays shared"]
  SQLite --> Store
  Store --> API["Server API"]
  API --> Dashboard["Dashboard"]
  API --> CLI["CLI and taskpilot run"]
```

What this means in plain language:

- The CLI normally does not open the database directly.
- The dashboard does not have a separate database.
- SQLite and Postgres are not both active for the same live server.
- The server chooses one backend at startup, then all normal work goes through that server.

## Complete Working Flow

This is the whole working model as a compact end-to-end flow.

```mermaid
flowchart TD
  Idea["Need coordinated work"] --> Create["Create task<br/>goal, scope, criteria"]
  Create --> Pick["Actor picks task<br/>dashboard or CLI"]
  Pick --> Claim["Claim ownership"]
  Claim --> Lock["Acquire locks"]
  Lock --> Safe{"Safe to work?"}

  Safe -- "No" --> Conflict["Conflict recorded"]
  Conflict --> Resolve["Resolve or escalate"]
  Resolve --> Pick

  Safe -- "Yes" --> RunMode{"How is work done?"}
  RunMode -- "Manual CLI" --> Manual["Work manually<br/>append context as needed"]
  RunMode -- "Agent wrapper" --> Wrapped["taskpilot run<br/>inject context, heartbeat,<br/>handoff draft, checkpoints"]

  Manual --> Memory["Save memory<br/>findings, decisions, artifacts,<br/>git refs, comments"]
  Wrapped --> Memory
  Memory --> Progress{"Done enough<br/>to finish?"}

  Progress -- "No, continue" --> More["Keep working<br/>heartbeats renew locks"]
  More --> Memory

  Progress -- "No, transfer" --> Handoff["Prepare handoff<br/>next steps and current state"]
  Handoff --> Next["Next actor accepts<br/>ownership and locks transfer"]
  Next --> Memory

  Progress -- "Blocked" --> Blocked["Mark blocked<br/>record blocker"]
  Blocked --> Resolve

  Progress -- "Yes" --> Verify["Verify completion criteria"]
  Verify --> Complete["Explicitly complete task"]
  Complete --> Audit["Audit timeline keeps record<br/>for future humans and agents"]
```

The shortest mental model:

```text
Task -> Claim -> Lock -> Work -> Remember -> Handoff or Complete
```

TaskPilot adds the safety rails around that simple loop:

- ownership so everyone knows who is responsible
- locks so parallel work does not collide silently
- heartbeats so stale work can be detected
- shared memory so context survives between agents
- handoffs so the next actor can continue without starting over
- audit events so the team can reconstruct what happened
