# TaskPilot Server, Postgres, and SQLite Flow

This document explains how the current TaskPilot implementation works at the server and database level, with special focus on:

- what runs on the server
- when SQLite is used
- when Postgres is used
- how the CLI, dashboard, server, and database connect
- which commands go through the server and which open the database directly

## Short Version

TaskPilot does not currently run SQLite and Postgres at the same time for the same live server.

Instead, the current architecture is:

1. One TaskPilot server process starts.
2. That server opens exactly one storage backend.
3. The storage backend is chosen from `TASKPILOT_DB_URL` or `--db`.
4. If the value looks like a Postgres URL, TaskPilot uses Postgres.
5. Otherwise, TaskPilot uses SQLite.

So the real runtime choice is:

- local/dev setup: usually one server backed by a local SQLite file
- shared/team setup: usually one server backed by Postgres

The CLI and dashboard both talk to that same server over HTTP for normal task work.

## Main Architecture

```mermaid
flowchart LR
  CLI["taskpilot CLI"] --> API["HTTP API"]
  UI["Dashboard UI"] --> API
  API --> Server["Go TaskPilot server"]
  Server --> Store["Store abstraction"]
  Store --> DB1[("SQLite file")]
  Store --> DB2[("Postgres")]
```

Important detail:

- `Store` is one abstraction layer in Go
- the same business logic is used for both database backends
- the selected backend is decided at startup

## What Runs On The Server

The TaskPilot server is the main coordination layer. It is responsible for:

- serving the dashboard HTML/JS/CSS
- exposing the HTTP API under `/api/...`
- authenticating users, API keys, and legacy actor credentials
- creating tasks, locks, handoffs, snapshots, comments, artifacts, and sessions
- updating task status and heartbeats
- reading and writing all shared coordination data

This is wired in [`internal/taskpilot/server.go`](/Users/appointy/Documents/New project 6/internal/taskpilot/server.go) and started by the `serve` command in [`internal/taskpilot/cli.go`](/Users/appointy/Documents/New project 6/internal/taskpilot/cli.go).

## How Backend Selection Works

The server config loads the DB setting here:

- [`internal/taskpilot/config.go`](/Users/appointy/Documents/New project 6/internal/taskpilot/config.go)

The effective rule is:

- `TASKPILOT_DB_URL` wins if set
- otherwise `--db` is used
- otherwise the default is `taskpilot.db`

The dialect check is in [`internal/taskpilot/store.go`](/Users/appointy/Documents/New project 6/internal/taskpilot/store.go):

- if the DB string starts with `postgres://` or `postgresql://`, dialect is `postgres`
- everything else is treated as `sqlite`

That means:

- `taskpilot.db` -> SQLite
- `/some/path/taskpilot.db` -> SQLite
- `:memory:` -> SQLite
- `postgres://taskpilot:password@host:5432/taskpilot?sslmode=disable` -> Postgres

## When SQLite Is Triggered

SQLite is triggered when the DB value is a file path instead of a Postgres URL.

Typical cases:

- local development
- running `taskpilot serve --db taskpilot.db`
- using the default `.env` value `TASKPILOT_DB_URL=taskpilot.db`
- quick single-machine usage without a separate database server

In this mode:

- the Go server opens a local SQLite file
- migrations run automatically on startup
- dashboard and CLI still use HTTP to talk to the server
- all shared task state lives inside that SQLite file

Local example from the repo:

```bash
./bin/taskpilot serve --addr 127.0.0.1:8080 --db taskpilot.db --token dev-token
```

## When Postgres Is Triggered

Postgres is triggered when the DB value starts with `postgres://` or `postgresql://`.

Typical cases:

- Docker/shared server deployment
- team setup where multiple machines connect to one central TaskPilot server
- production-like deployment

In this mode:

- the Go server opens Postgres through the `pgx` driver
- the same store methods are reused
- SQL is lightly rewritten where SQLite and Postgres differ
- the server retries DB startup more aggressively because Postgres may still be booting

Docker in this repo uses that setup:

- Postgres container
- TaskPilot server container
- `TASKPILOT_DB_URL=postgres://...`

See:

- [`docker-compose.yml`](/Users/appointy/Documents/New project 6/docker-compose.yml)

## How SQLite And Postgres Are Abstracted In Code

The abstraction sits in [`internal/taskpilot/store.go`](/Users/appointy/Documents/New project 6/internal/taskpilot/store.go).

Key behaviors:

- `OpenStore(...)` decides the driver: `sqlite` or `pgx`
- the `Store` keeps a `dialect` field
- `migrate(...)` runs at startup for both backends
- `sql(...)` rewrites some SQL only when dialect is Postgres

Examples of Postgres-specific adaptation:

- SQLite `?` placeholders are rewritten to Postgres `$1`, `$2`, ...
- `INSERT OR IGNORE` is rewritten to `ON CONFLICT DO NOTHING`
- `ADD COLUMN` is rewritten to `ADD COLUMN IF NOT EXISTS`
- SQLite `PRAGMA` statements are ignored for Postgres

So the application code mostly stays backend-neutral, and the store layer handles the differences.

## How The Normal Request Flow Works

For day-to-day work, the flow is:

1. CLI or dashboard sends an HTTP request to the TaskPilot server.
2. The server validates auth.
3. The server calls store methods.
4. The store reads/writes the configured database.
5. The server returns JSON to the CLI or dashboard.

Examples:

- `taskpilot task list`
- `taskpilot task show <id>`
- `taskpilot task claim <id>`
- `taskpilot lock acquire <id> --scope "src/auth/*"`
- dashboard loading `/api/tasks`

These do not open SQLite directly from the user machine. They call the server.

## How The CLI Connects

The CLI uses a saved config with values such as:

- server URL
- team token or API key
- actor ID
- actor secret

Normal CLI commands use HTTP requests built in [`internal/taskpilot/cli.go`](/Users/appointy/Documents/New project 6/internal/taskpilot/cli.go).

The important point is:

- the CLI is usually not the database client
- the CLI is usually an API client

So if your server is backed by Postgres, the CLI still just talks HTTP to the server.
If your server is backed by SQLite, the CLI still just talks HTTP to the server.

The DB choice is mainly a server-side concern for normal task operations.

## How The Dashboard Connects

The dashboard is served by the same Go server and the frontend JavaScript calls the same `/api/...` endpoints.

So the dashboard path is:

1. browser loads TaskPilot UI from the Go server
2. frontend JS calls TaskPilot API
3. server reads/writes the database through the store

There is no separate dashboard database.

## `taskpilot run` Flow

`taskpilot run` is important because it feels local, but it still works through the server.

Flow:

1. CLI reads its configured server URL.
2. CLI fetches task detail from the server.
3. CLI claims task through the server if needed.
4. CLI acquires locks through the server.
5. CLI starts a task session through the server.
6. CLI writes temporary local files for injected task context and handoff draft.
7. CLI launches the child agent command.
8. While the agent runs, CLI sends heartbeats and context updates back to the server.
9. Server persists those updates in the configured database.

So `taskpilot run` has both:

- local temporary files for agent context
- remote shared persistence through the server

Those temporary files are not the main system of record. The server database is.

## Commands That Go Through The Server

Most user-facing coordination commands go through the server, including:

- `login` only saves local CLI config
- `actor register`
- `task create|list|show|claim|status|complete`
- `context append`
- `decision add|list`
- `comment add|list`
- `artifact add|list`
- `git link-branch|attach-pr|attach`
- `lock acquire|release|renew`
- `handoff prepare|checkpoint|accept|reject`
- `project create|list`
- `repo create|list`
- `workspace create|list`
- `api-key create|list|revoke`
- `run`

These commands depend on a reachable TaskPilot server.

## Commands That Open The Database Directly

A smaller set of commands bypass the HTTP server and call `OpenStore(...)` directly:

- `taskpilot serve`
- `taskpilot migrate up|status`
- `taskpilot admin create-user`
- `taskpilot admin create-actor`
- `taskpilot admin create-api-key`
- `taskpilot admin reset-password`
- `taskpilot backup create|restore`

This is an important distinction:

- normal coordination commands are remote API calls
- server/bootstrap/maintenance commands can open the database directly

## What “Local SQLite” Really Means In Current TaskPilot

In the current codebase, “local SQLite” means:

- the TaskPilot server process is using a local `.db` file
- not that every CLI command has its own local offline database
- not that SQLite and Postgres are both active for the same request path

So the current architecture is not:

- CLI writes SQLite locally
- server later syncs that to Postgres

Instead, it is:

- one active shared database backend per server process
- SQLite for simple local mode
- Postgres for shared server mode

## Connection Scenarios

### Scenario 1: Local development with SQLite

```mermaid
flowchart LR
  CLI["taskpilot CLI on same machine"] --> Server["TaskPilot server"]
  Browser["Browser dashboard"] --> Server
  Server --> SQLite[("taskpilot.db")]
```

Triggered by:

- `--db taskpilot.db`
- or `TASKPILOT_DB_URL=taskpilot.db`

### Scenario 2: Shared deployment with Postgres

```mermaid
flowchart LR
  CLI1["CLI on machine A"] --> Server["Shared TaskPilot server"]
  CLI2["CLI on machine B"] --> Server
  Browser["Dashboard browser"] --> Server
  Server --> Postgres[("Postgres")]
```

Triggered by:

- `TASKPILOT_DB_URL=postgres://...`

## Startup Behavior Differences

There are a few server-level differences when Postgres is selected:

- Postgres uses more open/idle connections than SQLite
- server startup retries more times for Postgres or production mode
- SQLite creates the parent directory for the DB file if needed
- SQLite enables WAL mode with `PRAGMA journal_mode=WAL`

These behaviors are implemented in [`internal/taskpilot/store.go`](/Users/appointy/Documents/New project 6/internal/taskpilot/store.go) and [`internal/taskpilot/server.go`](/Users/appointy/Documents/New project 6/internal/taskpilot/server.go).

## Practical Rule Of Thumb

Use SQLite when:

- you want the fastest local setup
- one machine is enough
- you are developing or testing TaskPilot itself

Use Postgres when:

- multiple machines or users need a central shared coordination server
- you want a more production-style deployment
- you are running through Docker or a hosted server

## One Important Limitation To Know

The current implementation does not show a built-in live sync layer between SQLite and Postgres.

That means:

- TaskPilot is not currently running SQLite as a local cache and Postgres as the authoritative upstream at the same time
- whichever backend the server starts with becomes the active source of truth for that server

Also, the current `backup create|restore` implementation is file-copy based, which naturally fits SQLite much better than a Postgres DSN.

## Source References

- [`internal/taskpilot/config.go`](/Users/appointy/Documents/New project 6/internal/taskpilot/config.go)
- [`internal/taskpilot/store.go`](/Users/appointy/Documents/New project 6/internal/taskpilot/store.go)
- [`internal/taskpilot/server.go`](/Users/appointy/Documents/New project 6/internal/taskpilot/server.go)
- [`internal/taskpilot/cli.go`](/Users/appointy/Documents/New project 6/internal/taskpilot/cli.go)
- [`docker-compose.yml`](/Users/appointy/Documents/New project 6/docker-compose.yml)
- [`README.md`](/Users/appointy/Documents/New project 6/README.md)
