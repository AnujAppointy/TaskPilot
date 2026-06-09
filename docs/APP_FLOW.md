# TaskPilot App Flow

## 1. System Entry Points

TaskPilot has three main user-facing entry points:

- Dashboard served from the Go server at `/`.
- CLI command `taskpilot`.
- Agent wrapper command `taskpilot run <task-id> -- <agent-command>`.

All normal coordination flows converge on the same HTTP API and store.

```mermaid
flowchart LR
  Browser["Dashboard"] --> API["TaskPilot HTTP API"]
  CLI["taskpilot CLI"] --> API
  Run["taskpilot run"] --> API
  Run --> Agent["Child agent process"]
  API --> Store["Store layer"]
  Store --> DB[("SQLite or Postgres")]
```

## 2. Dashboard Startup Flow

```mermaid
flowchart TD
  Load["Browser loads /"] --> Static["Server returns embedded index.html, app.js, styles.css"]
  Static --> Me["Dashboard calls GET /api/me"]
  Me --> Auth{"Session valid?"}
  Auth -- "No" --> Login["Show signup/login screen"]
  Auth -- "Yes" --> Refresh["Load tasks, actors, projects, repos, workspaces, handoffs, conflicts, events"]
  Refresh --> Board["Render selected tab"]
  Board --> Stream["Open event stream /api/events/stream"]
  Stream --> Update["Refresh UI on events or scheduled polling"]
```

The dashboard keeps UI state in memory and selected actor/project settings in `localStorage` where possible. Rendering is delayed while a form field is focused so background refreshes do not interrupt typing.

## 3. Authentication Flow

### Signup

```mermaid
sequenceDiagram
  participant UI as Dashboard
  participant API as Server
  participant Store as Store

  UI->>API: POST /api/auth/signup
  API->>Store: CreateUser(email, password)
  Store-->>API: user
  API->>Store: EnsureDefaultActorForUser(user)
  Store-->>API: default actor
  API->>Store: CreateSession(user_id)
  API-->>UI: Set taskpilot_session cookie + user + actor
```

### Login

```mermaid
sequenceDiagram
  participant UI as Dashboard
  participant API as Server
  participant Store as Store

  UI->>API: POST /api/auth/login
  API->>Store: AuthenticateUser(email, password)
  API->>Store: EnsureDefaultActorForUser(user)
  API->>Store: CreateSession(user_id)
  API-->>UI: Set taskpilot_session cookie
```

### CLI actor authentication

CLI requests send:

- `X-Actor-ID`
- `X-Actor-Secret`

The server verifies the actor secret hash, touches `last_seen_at`, and treats the actor as a legacy actor principal for the request.

## 4. Dashboard Navigation Flow

The dashboard has these primary tabs:

- `board`
- `detail`
- `projects`
- `conflicts`
- `actors`
- `handoffs`
- `settings`

```mermaid
flowchart TD
  Board["Board"] --> Detail["Task detail"]
  Board --> NewTask["New task form"]
  Detail --> Memory["Task memory sections"]
  Detail --> Actions["Claim, release, status, complete, delete"]
  Projects["Projects"] --> Repos["Repositories"]
  Projects --> Workspaces["Workspaces"]
  Conflicts["Conflicts"] --> Resolve["Resolve conflict"]
  Actors["Actors"] --> Setup["Generate CLI setup command"]
  Handoffs["Handoffs"] --> AcceptReject["Accept or reject"]
  Settings["Settings"] --> Identity["Current user and actor setup"]
```

## 5. Task Board Flow

```mermaid
flowchart TD
  Refresh["Load /api/tasks"] --> Filters["Apply search, owner, status, repo, priority, blocked, stale filters"]
  Filters --> Columns["Group by lifecycle status"]
  Columns --> Card["Render task card"]
  Card --> Select["User opens task"]
  Select --> DetailFetch["GET /api/tasks/{id}"]
  DetailFetch --> Detail["Render task detail"]
```

The board shows derived operational signals such as active lock count, handoff status, conflict count, subtask count, dependency count, and stale/blocked indicators.

## 6. Task Creation Flow

```mermaid
sequenceDiagram
  participant User
  participant UI
  participant API
  participant Store

  User->>UI: Fill task form
  UI->>API: POST /api/tasks
  API->>Store: CreateTask(actor_id, TaskInput)
  Store->>Store: Validate title, goal, type, status, priority, project
  Store->>Store: Insert task
  Store->>Store: Add task.created event
  Store-->>API: Task
  API-->>UI: Task JSON
  UI->>API: GET /api/tasks/{id}
  UI-->>User: Detail view
```

Default task values:

- project defaults to `project_default`
- type defaults to `implementation`
- status defaults to `ready`
- priority defaults to `medium`
- privacy level defaults to `internal`

## 7. Manual Work Flow

```mermaid
flowchart TD
  Show["Inspect task detail"] --> Claim["POST /api/tasks/{id}/claim"]
  Claim --> Lock["POST /api/tasks/{id}/locks"]
  Lock --> Work["Work in repository"]
  Work --> Context["Append context, decisions, comments"]
  Work --> Output["Attach artifacts and git refs"]
  Context --> FinishChoice{"Done?"}
  Output --> FinishChoice
  FinishChoice -- "No" --> Handoff["Prepare handoff or mark blocked"]
  FinishChoice -- "Yes" --> Complete["POST /api/tasks/{id}/complete"]
```

Manual work is expected to follow the same shared memory discipline as agent work: claim first, lock touched scope, and write findings/decisions as durable context.

## 8. `taskpilot run` Flow

```mermaid
flowchart TD
  Command["taskpilot run TASK_ID -- agent prompt"] --> Config["Load local config"]
  Config --> Flush["Flush queued handoff checkpoints"]
  Flush --> Fetch["GET /api/tasks/{id}"]
  Fetch --> Claim{"Owned by current actor?"}
  Claim -- "No" --> ClaimAPI["POST /api/tasks/{id}/claim"]
  Claim -- "Yes" --> Locks
  ClaimAPI --> Locks["Acquire locks for task scope"]
  Locks --> Session["POST /api/tasks/{id}/sessions/start"]
  Session --> Files["Create temp context and handoff files"]
  Files --> Packet["Generate draft handoff packet"]
  Packet --> Prompt["Create startup prompt file"]
  Prompt --> Inject["Inject pointer into known agent prompt"]
  Inject --> Launch["Run child process"]
  Launch --> Active["Agent works"]
  Active --> Heartbeat["Heartbeat loop"]
  Active --> Progress["Progress import loop"]
  Active --> Checkpoint["Handoff checkpoint loop"]
  Heartbeat --> Active
  Progress --> Active
  Checkpoint --> Active
  Active --> Exit["Child exits"]
  Exit --> Import["Import final notes and touched file summary"]
  Import --> Finish["POST /api/tasks/{id}/sessions/finish"]
  Finish --> Complete{"--complete passed?"}
  Complete -- "Yes" --> CompleteAPI["POST /api/tasks/{id}/complete"]
  Complete -- "No" --> Claimed["Task remains claimed"]
```

The child process receives environment variables including:

- `TASKPILOT_TASK_ID`
- `TASKPILOT_SERVER`
- `TASKPILOT_ACTOR_ID`
- `TASKPILOT_SESSION_ID`
- `TASKPILOT_HANDOFF_PACKET_ID`
- `TASKPILOT_PROJECT_ID`
- `TASKPILOT_REPO_ID`
- `TASKPILOT_WORKSPACE_ID`
- `TASKPILOT_TASK_CONTEXT_FILE`
- `TASKPILOT_RELATED_CONTEXT_FILE`
- `TASKPILOT_RUN_CONTEXT_FILE`
- `TASKPILOT_HANDOFF_FILE`
- `TASKPILOT_AGENT_PROMPT_FILE`
- `TASKPILOT_AGENT_INSTRUCTIONS`

## 9. Task Session Flow

```mermaid
stateDiagram-v2
  [*] --> ready
  ready --> claimed: claim
  claimed --> in_progress: start session
  in_progress --> claimed: finish session
  in_progress --> completed: explicit complete
  claimed --> ready: release
  in_progress --> blocked: status update
  in_progress --> handoff_ready: prepare/publish handoff
  handoff_ready --> claimed: accept handoff
  in_progress --> in_review: send to review
  ready --> cancelled: cancel
  claimed --> cancelled: cancel
```

Important behavior: `FinishTaskSession` returns `in_progress` tasks to `claimed`. This prevents normal child-process exit from implying completion.

## 10. Lock and Conflict Flow

```mermaid
flowchart TD
  Request["Acquire lock"] --> Stale["Mark expired locks stale"]
  Stale --> Validate["Ensure task is lockable by actor"]
  Validate --> Find["Find overlapping active locks"]
  Find --> Conflict{"Other owner overlaps?"}
  Conflict -- "No" --> Insert["Insert active lock"]
  Insert --> Event["lock.acquired event"]
  Conflict -- "Yes" --> Record["Insert open conflict"]
  Record --> Error["Return lock_conflict with owner/scope message"]
```

Conflict resolution can:

- keep current owner
- transfer ownership
- split scope
- pause work
- mark duplicate
- escalate

## 11. Handoff Flow

```mermaid
flowchart TD
  Draft["Draft handoff file or generated packet"] --> Checkpoint["Create checkpoint"]
  Checkpoint --> Edit["Edit packet markdown"]
  Edit --> Publish["Publish handoff packet"]
  Publish --> Ready["Task status handoff_ready"]
  Ready --> Next["Next actor reviews"]
  Next --> Choice{"Accept?"}
  Choice -- "Yes" --> Accept["Accept handoff and transfer ownership"]
  Choice -- "No" --> Reject["Reject handoff"]
```

`taskpilot run` creates and updates the handoff draft during active work. If checkpoint upload is temporarily unavailable, the CLI writes a local queued checkpoint and later flushes it.

## 12. Event Flow

```mermaid
flowchart LR
  Mutation["Store mutation"] --> Event["events row"]
  Event --> API["GET /api/events"]
  Event --> SSE["GET /api/events/stream"]
  API --> Dashboard["Dashboard refresh"]
  SSE --> Dashboard
```

Events provide the task timeline and a lightweight live-update trigger for the dashboard.

## 13. Operational Flow

### Local SQLite

```bash
taskpilot serve --addr 127.0.0.1:8080 --db taskpilot.db
```

The server opens a SQLite file, runs migrations, serves the dashboard, and handles all API requests.

### Shared Postgres

```bash
docker compose up -d --build
```

When `TASKPILOT_DB_URL` starts with `postgres://` or `postgresql://`, the store uses the Postgres driver and rewrites SQLite-oriented SQL where needed.

