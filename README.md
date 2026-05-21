# TaskPilot

TaskPilot is a local-first coordination MVP for distributed AI agents and humans. It gives agents a CLI/API and gives managers or tech leads a dashboard over the same task state.

New to TaskPilot? Start with the team-friendly onboarding guide:

```text
docs/ONBOARDING.md
```

## Run

```bash
go build -o taskpilot ./cmd/taskpilot
./taskpilot serve --addr 127.0.0.1:8080 --db taskpilot.db --token dev-token
```

Optional env-file setup:

```bash
cp .env.example .env
set -a
source .env
set +a
./taskpilot serve
```

Open the dashboard at:

```text
http://127.0.0.1:8080
```

The dashboard uses the same API as the CLI. Set the token in the dashboard settings to `dev-token`, then register a human or agent actor.

Production-like self-hosted run:

```bash
TASKPILOT_TOKEN="$(openssl rand -hex 24)" \
TASKPILOT_SECRET_KEY="$(openssl rand -hex 32)" \
TASKPILOT_DB_URL="postgres://taskpilot:password@localhost:5432/taskpilot?sslmode=disable" \
./taskpilot serve --addr 0.0.0.0:8080 --production
```

SQLite remains the default for local development. Production can use Postgres by setting `TASKPILOT_DB_URL` to a `postgres://` or `postgresql://` connection string.

Docker:

```bash
docker compose up --build
```

## CLI Setup

Legacy development setup:

```bash
./taskpilot login --server http://127.0.0.1:8080 --token dev-token
./taskpilot actor register --name "Codex on Anuj Mac" --kind agent --machine anuj-mac
```

The actor registration command saves the returned actor ID and actor secret into the local CLI config.

## Projects, Repositories, And Workspaces

Use projects to keep team work separated, repositories to tell agents where code lives, and workspaces to identify the machine or runtime doing the work.

```bash
./taskpilot project create --name "Appointy Backend" --description "Backend agent coordination"
./taskpilot repo create --project <project-id> --name appointy-api --path /path/to/repo --branch main
./taskpilot workspace create --project <project-id> --name "Anuj Mac" --actor <actor-id> --machine anuj-mac
./taskpilot task create --project <project-id> --repo <repo-id> --workspace <workspace-id> --title "Fix signup bug" --goal "Resolve invited-user signup failure"
./taskpilot task list --project <project-id>
```

Existing databases automatically get a `Default` project so older tasks keep working.

Production-style local bootstrap:

```bash
./taskpilot admin create-user \
  --email admin@example.com \
  --name "Admin" \
  --password "change-this-password" \
  --role admin

./taskpilot admin create-actor \
  --name "Codex on Anuj Mac" \
  --kind agent \
  --machine anuj-mac

./taskpilot admin create-api-key \
  --name "Codex agent key" \
  --actor <actor-id> \
  --scope task:read \
  --scope task:write \
  --scope lock:write \
  --scope context:write \
  --scope handoff:write

./taskpilot login --server http://127.0.0.1:8080 --api-key <tpk_...>
```

The raw API key is shown only once when it is created. Store it in the local TaskPilot config with `login --api-key` or `api-key set`.

## Demo Flow

```bash
./taskpilot task create \
  --title "Fix invited-user signup" \
  --goal "Resolve signup failure for invited users" \
  --scope "src/auth/*"

./taskpilot task list
./taskpilot task claim <task-id>
./taskpilot lock acquire <task-id> --scope "src/auth/*"

./taskpilot task subtask <task-id> \
  --title "Write invited-user regression test" \
  --goal "Capture the failing expiry case before patching"

./taskpilot task depend <task-id> --on <blocking-task-id>

./taskpilot context append <task-id> \
  --kind decision \
  --content "Keep token format unchanged. Fix expiry comparison only."

./taskpilot handoff prepare <task-id> \
  --summary "Signup failure traced to expiry comparison. No schema change needed." \
  --next "Write regression test" \
  --next "Patch expiry logic"
```

Another machine or agent can configure the same server/token, register its own actor, inspect the task, accept the handoff, and continue.

## Subtasks and Dependencies

Use subtasks to split a larger task into owned child work. A parent task cannot be completed while any child task is still open.

Use dependencies when one task must wait for another task to finish first. A dependent task cannot be completed until every blocking task is completed or cancelled.

The dashboard exposes the same model:

- Task cards show subtask and blocker counts.
- Task detail shows parent, subtasks, "Blocked By", and "Blocking" sections.
- Humans can create subtasks, add blockers, and remove dependencies from the dashboard.

Every subtask and dependency change writes an audit event, so handoffs and status reviews keep their timeline.

## Decisions and Comments

Use decision records for durable rationale that future agents should preserve:

```bash
./taskpilot decision add <task-id> \
  --decision "Keep token format unchanged" \
  --alternative "Rotate all tokens" \
  --reason "Existing invite links must keep working" \
  --impact "Patch only expiry validation"
```

Use comments for human discussion, review notes, and questions:

```bash
./taskpilot comment add <task-id> --body "Please review expiry edge cases before merge."
```

Task detail and the dashboard show decisions and comments separately from generic agent context.

## Conflict Resolution

TaskPilot records ownership and lock collisions as open conflicts. The dashboard Conflicts view lets a lead choose a resolution with a required note:

- continue current owner
- transfer ownership
- split scope
- pause secondary work
- mark duplicate
- escalate to human

Resolution actions write audit events. Outcomes that change workflow also update task state, for example pausing secondary work marks the task blocked and adds blocker context.

## Artifacts and Git Metadata

TaskPilot stores artifact references by default, not raw local files. Use artifacts for PRs, logs, branches, docs, screenshots, and output links:

```bash
./taskpilot artifact add <task-id> \
  --kind pr \
  --title "Signup fix PR" \
  --uri "https://github.com/acme/app/pull/42" \
  --description "Reviewable implementation for invited-user signup"
```

Attach git metadata so managers can see where implementation work lives:

```bash
./taskpilot git link-branch <task-id>
./taskpilot git attach-pr <task-id> "https://github.com/acme/app/pull/42"
./taskpilot git attach <task-id> --branch feature/signup-fix --commit abc1234 --file src/auth/login.go
```

Task detail and the dashboard show artifact references and git metadata separately from context and comments.

## Agent Wrapper

Use `taskpilot run` when you want TaskPilot to claim the task, heartbeat while the child agent runs, pass task context through environment variables, import sanitized progress, and complete or block the task based on the child process exit status.

```bash
./taskpilot run <task-id> -- codex
./taskpilot run <task-id> --progress-interval 2m --no-complete -- codex
```

The child process receives:

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

`taskpilot run` appends sanitized run context automatically when the agent starts, periodically while it runs, succeeds, or fails. The child agent can add richer context by writing lines to `TASKPILOT_RUN_CONTEXT_FILE`:

```text
decision: Keep token format unchanged
blocker: Invite reproduction data is missing
{"kind":"summary","content":"Added invited-user regression coverage"}
```

After the command exits, TaskPilot imports those entries, records touched files from `git status`, and prepares a handoff automatically if the command fails.

Initialize project instructions:

```bash
./taskpilot agent init
```

Expose TaskPilot to MCP-capable agents:

```bash
./taskpilot mcp serve
```

The MCP server provides tools for `read_task`, `claim_task`, `heartbeat_task`, `append_context`, and `complete_task` using the same CLI config as other TaskPilot commands.

The dashboard task board also supports search and filters for project, owner, status, repo, priority, blocked state, and stale claims. Search covers task title, goal, context entries, and decision records.

## Operations

```bash
./taskpilot migrate status
./taskpilot migrate up
./taskpilot backup create --out taskpilot-backup.db
./taskpilot backup restore --in taskpilot-backup.db
```

Health and observability:

```text
GET /healthz
GET /readyz
GET /metrics
```

## Agent Operating Instructions

When working on a TaskPilot task:

1. Read the task before starting.
2. Claim the task before editing.
3. Acquire locks for files, artifacts, or semantic areas you will touch.
4. Send heartbeat while actively working.
5. Append sanitized findings, decisions, risks, and next steps.
6. Never upload raw local files unless explicitly approved outside the MVP.
7. Prepare a handoff if stopping before completion.
8. Mark task complete only when completion criteria are satisfied.

## API Auth

TaskPilot now supports production-style auth while keeping the legacy team token flow for local development.

Agent/API-key auth:

```http
Authorization: ApiKey <tpk_...>
```

Human dashboard session auth:

```http
POST /api/auth/login
Cookie: taskpilot_session=<session>
```

Legacy development auth:

```http
Authorization: Bearer <team-token>
X-Actor-ID: <actor-id>
X-Actor-Secret: <actor-secret>
```

`POST /api/actors/register` only requires the bearer token.

Do not expose the server on `0.0.0.0` with the default `dev-token`. TaskPilot refuses that startup mode; use a strong token through `--token` or `TASKPILOT_TOKEN`.

Production mode also requires `TASKPILOT_SECRET_KEY` with at least 32 characters.

## Privacy Boundary

TaskPilot stores structured task context, ownership, locks, handoffs, and audit events. It does not store raw local files, secrets, full prompts, private logs, screenshots, or workspace data by default.
