# TaskPilot

TaskPilot is a local-first coordination MVP for distributed AI agents and humans. It gives agents a CLI/API and gives managers or tech leads a dashboard over the same task state.

## Run

```bash
go build -o taskpilot ./cmd/taskpilot
./taskpilot serve --addr 127.0.0.1:8080 --db taskpilot.db --token dev-token
```

Open the dashboard at:

```text
http://127.0.0.1:8080
```

The dashboard uses the same API as the CLI. Set the token in the dashboard settings to `dev-token`, then register a human or agent actor.

## CLI Setup

```bash
./taskpilot login --server http://127.0.0.1:8080 --token dev-token
./taskpilot actor register --name "Codex on Anuj Mac" --kind agent --machine anuj-mac
```

The actor registration command saves the returned actor ID and actor secret into the local CLI config.

## Demo Flow

```bash
./taskpilot task create \
  --title "Fix invited-user signup" \
  --goal "Resolve signup failure for invited users" \
  --scope "src/auth/*"

./taskpilot task list
./taskpilot task claim <task-id>
./taskpilot lock acquire <task-id> --scope "src/auth/*"

./taskpilot context append <task-id> \
  --kind decision \
  --content "Keep token format unchanged. Fix expiry comparison only."

./taskpilot handoff prepare <task-id> \
  --summary "Signup failure traced to expiry comparison. No schema change needed." \
  --next "Write regression test" \
  --next "Patch expiry logic"
```

Another machine or agent can configure the same server/token, register its own actor, inspect the task, accept the handoff, and continue.

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

All API calls require:

```http
Authorization: Bearer <team-token>
X-Actor-ID: <actor-id>
X-Actor-Secret: <actor-secret>
```

`POST /api/actors/register` only requires the bearer token.

Do not expose the server on `0.0.0.0` with the default `dev-token`. TaskPilot refuses that startup mode; use a strong token through `--token` or `TASKPILOT_TOKEN`.

## Privacy Boundary

TaskPilot stores structured task context, ownership, locks, handoffs, and audit events. It does not store raw local files, secrets, full prompts, private logs, screenshots, or workspace data by default.
