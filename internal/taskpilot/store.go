package taskpilot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

type Store struct {
	db      *sql.DB
	dialect string
}

type StoreStats struct {
	Tasks       int `json:"tasks"`
	Actors      int `json:"actors"`
	ActiveLocks int `json:"active_locks"`
	Handoffs    int `json:"handoffs"`
	Events      int `json:"events"`
}

func OpenStore(path string) (*Store, error) {
	dialect := dbDialect(path)
	if dialect == "sqlite" && path != "" && path != ":memory:" {
		if err := ensureDir(filepath.Dir(path)); err != nil {
			return nil, err
		}
	}
	driver := "sqlite"
	if dialect == "postgres" {
		driver = "pgx"
	}
	db, err := sql.Open(driver, path)
	if err != nil {
		return nil, err
	}
	if dialect == "postgres" {
		db.SetMaxOpenConns(20)
		db.SetMaxIdleConns(10)
	} else {
		db.SetMaxOpenConns(5)
		db.SetMaxIdleConns(5)
	}
	s := &Store{db: db, dialect: dialect}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func dbDialect(path string) string {
	if strings.HasPrefix(path, "postgres://") || strings.HasPrefix(path, "postgresql://") {
		return "postgres"
	}
	return "sqlite"
}

func (s *Store) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if s.dialect == "postgres" && strings.HasPrefix(strings.TrimSpace(strings.ToUpper(query)), "PRAGMA ") {
		return noopResult(0), nil
	}
	return s.db.ExecContext(ctx, s.sql(query), args...)
}

func (s *Store) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.sql(query), args...)
}

func (s *Store) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.sql(query), args...)
}

func (s *Store) sql(query string) string {
	if s.dialect != "postgres" {
		return query
	}
	q := strings.ReplaceAll(query, "id INTEGER PRIMARY KEY AUTOINCREMENT", "id BIGSERIAL PRIMARY KEY")
	q = rewriteInsertOrIgnore(q)
	q = rewriteAlterAddColumn(q)
	return postgresPlaceholders(q)
}

var insertOrIgnoreRE = regexp.MustCompile(`(?is)^\s*INSERT\s+OR\s+IGNORE\s+INTO\s+(.+)$`)

func rewriteInsertOrIgnore(query string) string {
	m := insertOrIgnoreRE.FindStringSubmatch(query)
	if len(m) != 2 {
		return query
	}
	return "INSERT INTO " + m[1] + " ON CONFLICT DO NOTHING"
}

func rewriteAlterAddColumn(query string) string {
	upper := strings.ToUpper(query)
	if strings.Contains(upper, " ADD COLUMN ") && !strings.Contains(upper, " ADD COLUMN IF NOT EXISTS ") {
		return strings.Replace(query, " ADD COLUMN ", " ADD COLUMN IF NOT EXISTS ", 1)
	}
	return query
}

func postgresPlaceholders(query string) string {
	var b strings.Builder
	n := 1
	inSingle := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if ch == '\'' {
			b.WriteByte(ch)
			if inSingle && i+1 < len(query) && query[i+1] == '\'' {
				i++
				b.WriteByte(query[i])
				continue
			}
			inSingle = !inSingle
			continue
		}
		if ch == '?' && !inSingle {
			b.WriteString(fmt.Sprintf("$%d", n))
			n++
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

type noopResult int64

func (r noopResult) LastInsertId() (int64, error) { return 0, nil }
func (r noopResult) RowsAffected() (int64, error) { return int64(r), nil }

func (s *Store) Stats(ctx context.Context) (StoreStats, error) {
	var out StoreStats
	counts := []struct {
		query string
		dest  *int
	}{
		{`SELECT COUNT(*) FROM tasks`, &out.Tasks},
		{`SELECT COUNT(*) FROM actors`, &out.Actors},
		{`SELECT COUNT(*) FROM locks WHERE released_at IS NULL AND status IN ('active','stale')`, &out.ActiveLocks},
		{`SELECT COUNT(*) FROM handoffs`, &out.Handoffs},
		{`SELECT COUNT(*) FROM events`, &out.Events},
	}
	for _, c := range counts {
		var err error
		if strings.Contains(c.query, "?") {
			err = s.queryRow(ctx, c.query, ts(time.Now().UTC())).Scan(c.dest)
		} else {
			err = s.queryRow(ctx, c.query).Scan(c.dest)
		}
		if err != nil {
			return StoreStats{}, err
		}
	}
	return out, nil
}

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`PRAGMA journal_mode=WAL`,
		`CREATE TABLE IF NOT EXISTS actors (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, kind TEXT NOT NULL, machine_name TEXT,
			created_at TEXT NOT NULL, last_seen_at TEXT, actor_secret_hash TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY, email TEXT NOT NULL UNIQUE, name TEXT NOT NULL, password_hash TEXT NOT NULL,
			role TEXT NOT NULL, active INTEGER NOT NULL, created_at TEXT NOT NULL, last_seen_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY, user_id TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE,
			created_at TEXT NOT NULL, expires_at TEXT NOT NULL, revoked_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, actor_id TEXT NOT NULL, role TEXT NOT NULL,
			scopes TEXT NOT NULL, key_hash TEXT NOT NULL UNIQUE, prefix TEXT NOT NULL,
			created_by TEXT NOT NULL, created_at TEXT NOT NULL, revoked_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS projects (
			id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, description TEXT NOT NULL,
			created_by TEXT NOT NULL, created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS repositories (
			id TEXT PRIMARY KEY, project_id TEXT NOT NULL, name TEXT NOT NULL, path TEXT NOT NULL,
			default_branch TEXT NOT NULL, created_by TEXT NOT NULL, created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS workspaces (
			id TEXT PRIMARY KEY, project_id TEXT NOT NULL, actor_id TEXT, name TEXT NOT NULL,
			machine_name TEXT, kind TEXT NOT NULL, created_by TEXT NOT NULL,
			created_at TEXT NOT NULL, last_seen_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY, project_id TEXT NOT NULL DEFAULT 'project_default', repo_id TEXT, workspace_id TEXT,
			parent_task_id TEXT, title TEXT NOT NULL, goal TEXT NOT NULL, type TEXT NOT NULL,
			status TEXT NOT NULL, priority TEXT NOT NULL, owner_id TEXT, created_by TEXT NOT NULL,
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL, claim_expires_at TEXT,
			last_heartbeat_at TEXT, privacy_level TEXT NOT NULL, scope_json TEXT NOT NULL,
			requirements_json TEXT NOT NULL, completion_criteria_json TEXT NOT NULL,
			risks_json TEXT NOT NULL, blockers_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS context_entries (
			id TEXT PRIMARY KEY, task_id TEXT NOT NULL, author_id TEXT NOT NULL, kind TEXT NOT NULL,
			content TEXT NOT NULL, created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS decision_records (
			id TEXT PRIMARY KEY, task_id TEXT NOT NULL, author_id TEXT NOT NULL,
			decision TEXT NOT NULL, alternatives_json TEXT NOT NULL, reason TEXT NOT NULL,
			impact TEXT NOT NULL, created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS comments (
			id TEXT PRIMARY KEY, task_id TEXT NOT NULL, author_id TEXT NOT NULL,
			body TEXT NOT NULL, created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS artifacts (
			id TEXT PRIMARY KEY, task_id TEXT NOT NULL, author_id TEXT NOT NULL,
			kind TEXT NOT NULL, title TEXT NOT NULL, uri TEXT NOT NULL,
			description TEXT NOT NULL, metadata_json TEXT NOT NULL, created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS git_refs (
			id TEXT PRIMARY KEY, task_id TEXT NOT NULL, author_id TEXT NOT NULL,
			branch TEXT, commit_sha TEXT, pr_url TEXT, changed_files_json TEXT NOT NULL,
			note TEXT NOT NULL, created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS locks (
			id TEXT PRIMARY KEY, task_id TEXT NOT NULL, owner_id TEXT NOT NULL, scope TEXT NOT NULL,
			scope_type TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active',
			expires_at TEXT NOT NULL, last_heartbeat_at TEXT NOT NULL,
			created_at TEXT NOT NULL, released_at TEXT, released_by TEXT, release_reason TEXT,
			overridden_at TEXT, overridden_by TEXT, override_reason TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS conflicts (
			id TEXT PRIMARY KEY, task_id TEXT, actor_id TEXT, conflict_type TEXT NOT NULL,
			status TEXT NOT NULL, scope TEXT, scope_type TEXT, current_owner_id TEXT,
			other_actor_id TEXT, other_task_id TEXT, lock_id TEXT, conflicting_lock_id TEXT,
			resolution TEXT, resolution_note TEXT, created_at TEXT NOT NULL,
			resolved_at TEXT, resolved_by TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS handoffs (
			id TEXT PRIMARY KEY, task_id TEXT NOT NULL, from_actor_id TEXT NOT NULL, to_actor_id TEXT,
			status TEXT NOT NULL, resume_summary TEXT NOT NULL, next_steps_json TEXT NOT NULL,
			created_at TEXT NOT NULL, accepted_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS context_snapshots (
			id TEXT PRIMARY KEY, task_id TEXT NOT NULL, author_id TEXT NOT NULL,
			source TEXT NOT NULL, snapshot_type TEXT NOT NULL, status_at_time TEXT NOT NULL,
			summary_json TEXT NOT NULL, markdown_cache TEXT NOT NULL,
			source_context_ids_json TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS handoff_packets (
			id TEXT PRIMARY KEY, task_id TEXT NOT NULL, handoff_id TEXT, generated_by TEXT NOT NULL,
			status TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 1, packet_json TEXT NOT NULL, markdown_cache TEXT NOT NULL,
			source_snapshot_ids_json TEXT NOT NULL, source_context_ids_json TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT 'generated_fallback', validation_errors_json TEXT NOT NULL DEFAULT '[]',
			supporting_evidence_json TEXT NOT NULL DEFAULT '[]',
			edited_by TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS task_sessions (
			id TEXT PRIMARY KEY, task_id TEXT NOT NULL, actor_id TEXT NOT NULL,
			started_at TEXT NOT NULL, ended_at TEXT, exit_status TEXT, finish_reason TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS task_dependencies (
			id TEXT PRIMARY KEY, task_id TEXT NOT NULL, depends_on_id TEXT NOT NULL,
			created_by TEXT NOT NULL, created_at TEXT NOT NULL,
			UNIQUE(task_id, depends_on_id)
		)`,
		`CREATE TABLE IF NOT EXISTS events (
			id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT, actor_id TEXT NOT NULL,
			event_type TEXT NOT NULL, payload_json TEXT NOT NULL, created_at TEXT NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.exec(ctx, stmt); err != nil {
			return err
		}
	}
	_, _ = s.exec(ctx, `ALTER TABLE actors ADD COLUMN actor_secret_hash TEXT`)
	_, _ = s.exec(ctx, `ALTER TABLE tasks ADD COLUMN project_id TEXT NOT NULL DEFAULT 'project_default'`)
	_, _ = s.exec(ctx, `ALTER TABLE tasks ADD COLUMN repo_id TEXT`)
	_, _ = s.exec(ctx, `ALTER TABLE tasks ADD COLUMN workspace_id TEXT`)
	_, _ = s.exec(ctx, `ALTER TABLE tasks ADD COLUMN parent_task_id TEXT`)
	_, _ = s.exec(ctx, `ALTER TABLE locks ADD COLUMN status TEXT NOT NULL DEFAULT 'active'`)
	_, _ = s.exec(ctx, `ALTER TABLE locks ADD COLUMN last_heartbeat_at TEXT`)
	_, _ = s.exec(ctx, `ALTER TABLE locks ADD COLUMN released_by TEXT`)
	_, _ = s.exec(ctx, `ALTER TABLE locks ADD COLUMN release_reason TEXT`)
	_, _ = s.exec(ctx, `ALTER TABLE locks ADD COLUMN overridden_at TEXT`)
	_, _ = s.exec(ctx, `ALTER TABLE locks ADD COLUMN overridden_by TEXT`)
	_, _ = s.exec(ctx, `ALTER TABLE locks ADD COLUMN override_reason TEXT`)
	_, _ = s.exec(ctx, `UPDATE locks SET status='active' WHERE status='' OR status IS NULL`)
	_, _ = s.exec(ctx, `UPDATE locks SET last_heartbeat_at=created_at WHERE last_heartbeat_at='' OR last_heartbeat_at IS NULL`)
	_, _ = s.exec(ctx, `UPDATE locks SET status='released' WHERE released_at IS NOT NULL AND status='active'`)
	_, _ = s.exec(ctx, `ALTER TABLE handoff_packets ADD COLUMN version INTEGER NOT NULL DEFAULT 1`)
	_, _ = s.exec(ctx, `ALTER TABLE handoff_packets ADD COLUMN source TEXT NOT NULL DEFAULT 'generated_fallback'`)
	_, _ = s.exec(ctx, `ALTER TABLE handoff_packets ADD COLUMN validation_errors_json TEXT NOT NULL DEFAULT '[]'`)
	_, _ = s.exec(ctx, `ALTER TABLE handoff_packets ADD COLUMN supporting_evidence_json TEXT NOT NULL DEFAULT '[]'`)
	_, _ = s.exec(ctx, `UPDATE handoff_packets SET status='published' WHERE status='ready' AND handoff_id IS NOT NULL AND handoff_id<>''`)
	_, _ = s.exec(ctx, `UPDATE handoff_packets SET status='draft' WHERE status='ready' AND (handoff_id IS NULL OR handoff_id='')`)
	if err := s.ensureDefaultProject(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureDefaultProject(ctx context.Context) error {
	now := time.Now().UTC()
	_, err := s.exec(ctx, `INSERT OR IGNORE INTO projects (id,name,description,created_by,created_at) VALUES (?,?,?,?,?)`,
		"project_default", "Default", "Default project for existing TaskPilot tasks.", "system", ts(now))
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, `UPDATE tasks SET project_id='project_default' WHERE project_id='' OR project_id IS NULL`)
	return err
}

func (s *Store) CreateProject(ctx context.Context, actorID, name, description string) (Project, error) {
	if strings.TrimSpace(name) == "" {
		return Project{}, userErr("validation", "project name is required")
	}
	now := time.Now().UTC()
	p := Project{ID: newID("project"), Name: strings.TrimSpace(name), Description: strings.TrimSpace(description), CreatedBy: actorID, CreatedAt: now}
	_, err := s.exec(ctx, `INSERT INTO projects (id,name,description,created_by,created_at) VALUES (?,?,?,?,?)`,
		p.ID, p.Name, p.Description, p.CreatedBy, ts(p.CreatedAt))
	if err != nil {
		return Project{}, err
	}
	return p, s.addEvent(ctx, "", actorID, "project.created", p)
}

func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.query(ctx, `SELECT id,name,description,created_by,created_at FROM projects ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var p Project
		var created string
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.CreatedBy, &created); err != nil {
			return nil, err
		}
		p.CreatedAt = parseTS(created)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) projectExists(ctx context.Context, id string) (bool, error) {
	if id == "" {
		id = "project_default"
	}
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM projects WHERE id=?`, id).Scan(&n)
	return n > 0, err
}

func (s *Store) CreateRepository(ctx context.Context, actorID, projectID, name, path, branch string) (Repository, error) {
	if projectID == "" {
		projectID = "project_default"
	}
	if strings.TrimSpace(name) == "" {
		return Repository{}, userErr("validation", "repository name is required")
	}
	ok, err := s.projectExists(ctx, projectID)
	if err != nil {
		return Repository{}, err
	}
	if !ok {
		return Repository{}, userErr("validation", "project_id does not exist")
	}
	if branch == "" {
		branch = "main"
	}
	now := time.Now().UTC()
	r := Repository{ID: newID("repo"), ProjectID: projectID, Name: strings.TrimSpace(name), Path: strings.TrimSpace(path), DefaultBranch: branch, CreatedBy: actorID, CreatedAt: now}
	_, err = s.exec(ctx, `INSERT INTO repositories (id,project_id,name,path,default_branch,created_by,created_at) VALUES (?,?,?,?,?,?,?)`,
		r.ID, r.ProjectID, r.Name, r.Path, r.DefaultBranch, r.CreatedBy, ts(r.CreatedAt))
	if err != nil {
		return Repository{}, err
	}
	return r, s.addEvent(ctx, "", actorID, "repo.created", r)
}

func (s *Store) ListRepositories(ctx context.Context, projectID string) ([]Repository, error) {
	query := `SELECT id,project_id,name,path,default_branch,created_by,created_at FROM repositories`
	args := []any{}
	if projectID != "" {
		query += ` WHERE project_id=?`
		args = append(args, projectID)
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Repository{}
	for rows.Next() {
		var r Repository
		var created string
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Name, &r.Path, &r.DefaultBranch, &r.CreatedBy, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTS(created)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) CreateWorkspace(ctx context.Context, actorID, projectID, ownerActorID, name, machine, kind string) (Workspace, error) {
	if projectID == "" {
		projectID = "project_default"
	}
	if strings.TrimSpace(name) == "" {
		return Workspace{}, userErr("validation", "workspace name is required")
	}
	if kind == "" {
		kind = "local"
	}
	if !oneOf(kind, "local", "agent", "ci", "other") {
		return Workspace{}, userErr("validation", "workspace kind must be local, agent, ci, or other")
	}
	ok, err := s.projectExists(ctx, projectID)
	if err != nil {
		return Workspace{}, err
	}
	if !ok {
		return Workspace{}, userErr("validation", "project_id does not exist")
	}
	now := time.Now().UTC()
	w := Workspace{ID: newID("workspace"), ProjectID: projectID, ActorID: ownerActorID, Name: strings.TrimSpace(name), MachineName: machine, Kind: kind, CreatedBy: actorID, CreatedAt: now, LastSeenAt: &now}
	_, err = s.exec(ctx, `INSERT INTO workspaces (id,project_id,actor_id,name,machine_name,kind,created_by,created_at,last_seen_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		w.ID, w.ProjectID, w.ActorID, w.Name, w.MachineName, w.Kind, w.CreatedBy, ts(w.CreatedAt), tsPtr(w.LastSeenAt))
	if err != nil {
		return Workspace{}, err
	}
	return w, s.addEvent(ctx, "", actorID, "workspace.created", w)
}

func (s *Store) ListWorkspaces(ctx context.Context, projectID string) ([]Workspace, error) {
	query := `SELECT id,project_id,actor_id,name,machine_name,kind,created_by,created_at,last_seen_at FROM workspaces`
	args := []any{}
	if projectID != "" {
		query += ` WHERE project_id=?`
		args = append(args, projectID)
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Workspace{}
	for rows.Next() {
		var w Workspace
		var actor, machine, last sql.NullString
		var created string
		if err := rows.Scan(&w.ID, &w.ProjectID, &actor, &w.Name, &machine, &w.Kind, &w.CreatedBy, &created, &last); err != nil {
			return nil, err
		}
		w.ActorID = actor.String
		w.MachineName = machine.String
		w.CreatedAt = parseTS(created)
		if last.Valid {
			t := parseTS(last.String)
			w.LastSeenAt = &t
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) RegisterActor(ctx context.Context, name, kind, machine string) (Actor, error) {
	if strings.TrimSpace(name) == "" {
		return Actor{}, userErr("validation", "actor name is required")
	}
	if kind == "" {
		kind = "agent"
	}
	if kind != "human" && kind != "agent" {
		return Actor{}, userErr("validation", "actor kind must be human or agent")
	}
	now := time.Now().UTC()
	secret := newSecret()
	a := Actor{ID: newID("actor"), Name: name, Kind: kind, MachineName: machine, Secret: secret, CreatedAt: now, LastSeenAt: &now}
	_, err := s.exec(ctx, `INSERT INTO actors (id,name,kind,machine_name,created_at,last_seen_at,actor_secret_hash) VALUES (?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.Kind, a.MachineName, ts(a.CreatedAt), tsPtr(a.LastSeenAt), secretHash(secret))
	if err != nil {
		return Actor{}, err
	}
	eventActor := a
	eventActor.Secret = ""
	return a, s.addEvent(ctx, "", a.ID, "actor.registered", eventActor)
}

func (s *Store) VerifyActorSecret(ctx context.Context, actorID, secret string) (bool, error) {
	if actorID == "" || secret == "" {
		return false, nil
	}
	var hash sql.NullString
	err := s.queryRow(ctx, `SELECT actor_secret_hash FROM actors WHERE id=?`, actorID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return hash.Valid && hash.String == secretHash(secret), nil
}

func (s *Store) TouchActor(ctx context.Context, actorID string) {
	if actorID == "" {
		return
	}
	now := time.Now().UTC()
	_, _ = s.exec(ctx, `UPDATE actors SET last_seen_at=? WHERE id=?`, ts(now), actorID)
}

func (s *Store) ListActors(ctx context.Context) ([]Actor, error) {
	rows, err := s.query(ctx, `SELECT id,name,kind,machine_name,created_at,last_seen_at FROM actors ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Actor{}
	for rows.Next() {
		a, err := scanActor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetActor(ctx context.Context, id string) (*Actor, error) {
	row := s.queryRow(ctx, `SELECT id,name,kind,machine_name,created_at,last_seen_at FROM actors WHERE id=?`, id)
	a, err := scanActor(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

type TaskInput struct {
	ProjectID          string   `json:"project_id"`
	RepoID             string   `json:"repo_id"`
	WorkspaceID        string   `json:"workspace_id"`
	ParentTaskID       string   `json:"parent_task_id"`
	Title              string   `json:"title"`
	Goal               string   `json:"goal"`
	Type               string   `json:"type"`
	Status             string   `json:"status"`
	Priority           string   `json:"priority"`
	Scope              []string `json:"scope"`
	Requirements       []string `json:"requirements"`
	CompletionCriteria []string `json:"completion_criteria"`
	Risks              []string `json:"risks"`
	Blockers           []string `json:"blockers"`
	PrivacyLevel       string   `json:"privacy_level"`
}

func (s *Store) CreateTask(ctx context.Context, actorID string, in TaskInput) (Task, error) {
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Goal) == "" {
		return Task{}, userErr("validation", "title and goal are required")
	}
	if in.Type == "" {
		in.Type = "implementation"
	}
	if in.Status == "" {
		in.Status = "ready"
	}
	if in.Priority == "" {
		in.Priority = "normal"
	}
	if in.PrivacyLevel == "" {
		in.PrivacyLevel = "sanitized_context"
	}
	if in.ProjectID == "" {
		in.ProjectID = "project_default"
	}
	ok, err := s.projectExists(ctx, in.ProjectID)
	if err != nil {
		return Task{}, err
	}
	if !ok {
		return Task{}, userErr("validation", "project_id does not exist")
	}
	if in.ParentTaskID != "" {
		parent, err := s.GetTask(ctx, in.ParentTaskID)
		if err != nil {
			return Task{}, err
		}
		if parent.ID == "" {
			return Task{}, userErr("validation", "parent task does not exist")
		}
		if parent.ProjectID != in.ProjectID {
			return Task{}, userErr("validation", "parent task must be in the same project")
		}
	}
	if err := validateTaskEnums(in.Type, in.Status, in.Priority, in.PrivacyLevel); err != nil {
		return Task{}, err
	}
	now := time.Now().UTC()
	t := Task{
		ID: newID("task"), ProjectID: in.ProjectID, RepoID: in.RepoID, WorkspaceID: in.WorkspaceID,
		ParentTaskID: in.ParentTaskID, Title: in.Title, Goal: in.Goal, Type: in.Type, Status: in.Status,
		Priority: in.Priority, CreatedBy: actorID, CreatedAt: now, UpdatedAt: now,
		PrivacyLevel: in.PrivacyLevel, Scope: in.Scope, Requirements: in.Requirements,
		CompletionCriteria: in.CompletionCriteria, Risks: in.Risks, Blockers: in.Blockers,
	}
	_, err = s.exec(ctx, `INSERT INTO tasks
		(id,project_id,repo_id,workspace_id,parent_task_id,title,goal,type,status,priority,owner_id,created_by,created_at,updated_at,claim_expires_at,last_heartbeat_at,privacy_level,scope_json,requirements_json,completion_criteria_json,risks_json,blockers_json)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.ProjectID, t.RepoID, t.WorkspaceID, t.ParentTaskID, t.Title, t.Goal, t.Type, t.Status, t.Priority, t.OwnerID, t.CreatedBy, ts(t.CreatedAt), ts(t.UpdatedAt), nil, nil,
		t.PrivacyLevel, js(t.Scope), js(t.Requirements), js(t.CompletionCriteria), js(t.Risks), js(t.Blockers))
	if err != nil {
		return Task{}, err
	}
	return t, s.addEvent(ctx, t.ID, actorID, "task.created", t)
}

func (s *Store) ListTasks(ctx context.Context, projectID string) ([]Task, error) {
	return s.listTasks(ctx, projectID, "")
}

func (s *Store) ListSubtasks(ctx context.Context, parentTaskID string) ([]Task, error) {
	return s.listTasks(ctx, "", parentTaskID)
}

func (s *Store) listTasks(ctx context.Context, projectID, parentTaskID string) ([]Task, error) {
	query := `SELECT id,project_id,repo_id,workspace_id,parent_task_id,title,goal,type,status,priority,owner_id,created_by,created_at,updated_at,claim_expires_at,last_heartbeat_at,privacy_level,scope_json,requirements_json,completion_criteria_json,risks_json,blockers_json FROM tasks`
	args := []any{}
	clauses := []string{}
	if projectID != "" {
		clauses = append(clauses, `project_id=?`)
		args = append(args, projectID)
	}
	if parentTaskID != "" {
		clauses = append(clauses, `parent_task_id=?`)
		args = append(args, parentTaskID)
	}
	if len(clauses) > 0 {
		query += ` WHERE ` + strings.Join(clauses, ` AND `)
	}
	query += ` ORDER BY updated_at DESC`
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		t.ActiveLockCount = s.countActiveLocks(ctx, t.ID)
		t.LatestHandoffStatus = s.latestHandoffStatus(ctx, t.ID)
		t.PotentialConflictCount = s.countLockConflicts(ctx, t.ID)
		t.SubtaskCount = s.countSubtasks(ctx, t.ID)
		t.OpenDependencyCount = s.countOpenDependencies(ctx, t.ID)
		t.BlockedByCount = s.countDependents(ctx, t.ID)
		t.SearchText = s.taskSearchText(ctx, t.ID)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	row := s.queryRow(ctx, `SELECT id,project_id,repo_id,workspace_id,parent_task_id,title,goal,type,status,priority,owner_id,created_by,created_at,updated_at,claim_expires_at,last_heartbeat_at,privacy_level,scope_json,requirements_json,completion_criteria_json,risks_json,blockers_json FROM tasks WHERE id=?`, id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, userErr("not_found", "task not found")
	}
	return t, err
}

func (s *Store) TaskDetail(ctx context.Context, id string) (TaskDetail, error) {
	t, err := s.GetTask(ctx, id)
	if err != nil {
		return TaskDetail{}, err
	}
	var owner *Actor
	if t.OwnerID != "" {
		owner, _ = s.GetActor(ctx, t.OwnerID)
	}
	c, _ := s.ListContext(ctx, id)
	decisions, _ := s.ListDecisions(ctx, id)
	comments, _ := s.ListComments(ctx, id)
	artifacts, _ := s.ListArtifacts(ctx, id)
	gitRefs, _ := s.ListGitRefs(ctx, id)
	l, _ := s.ListLocks(ctx, id, false)
	h, _ := s.ListHandoffs(ctx, id)
	snapshots, _ := s.ListContextSnapshots(ctx, id)
	packet, _ := s.LatestHandoffPacket(ctx, id)
	e, _ := s.ListEvents(ctx, 0, id)
	subtasks, _ := s.ListSubtasks(ctx, id)
	dependencies, _ := s.ListTaskDependencies(ctx, id)
	dependents, _ := s.ListTaskDependents(ctx, id)
	var parent *Task
	if t.ParentTaskID != "" {
		if p, err := s.GetTask(ctx, t.ParentTaskID); err == nil {
			parent = &p
		}
	}
	var latestSnapshot *ContextSnapshot
	if len(snapshots) > 0 {
		latestSnapshot = &snapshots[len(snapshots)-1]
	}
	return TaskDetail{Task: t, Owner: owner, Parent: parent, Subtasks: subtasks, Dependencies: dependencies, Dependents: dependents, Context: c, Decisions: decisions, Comments: comments, Artifacts: artifacts, GitRefs: gitRefs, Locks: l, Handoffs: h, Snapshots: snapshots, LatestSnapshot: latestSnapshot, HandoffPacket: packet, Events: e}, nil
}

func (s *Store) UpdateTask(ctx context.Context, actorID, id string, in TaskInput, reason string) (Task, error) {
	t, err := s.GetTask(ctx, id)
	if err != nil {
		return Task{}, err
	}
	oldStatus := t.Status
	if in.Title != "" {
		t.Title = in.Title
	}
	if in.Goal != "" {
		t.Goal = in.Goal
	}
	if in.Type != "" {
		t.Type = in.Type
	}
	if in.Status != "" {
		if in.Status == "completed" && reason != "completed" {
			return Task{}, userErr("validation", "completed status requires the complete action")
		}
		if err := validateStatusTransition(t.Status, in.Status); err != nil {
			return Task{}, err
		}
		t.Status = in.Status
	}
	if in.Priority != "" {
		t.Priority = in.Priority
	}
	if in.ProjectID != "" {
		ok, err := s.projectExists(ctx, in.ProjectID)
		if err != nil {
			return Task{}, err
		}
		if !ok {
			return Task{}, userErr("validation", "project_id does not exist")
		}
		t.ProjectID = in.ProjectID
	}
	if in.RepoID != "" {
		t.RepoID = in.RepoID
	}
	if in.WorkspaceID != "" {
		t.WorkspaceID = in.WorkspaceID
	}
	if in.ParentTaskID != "" {
		if in.ParentTaskID == t.ID {
			return Task{}, userErr("validation", "task cannot be its own parent")
		}
		parent, err := s.GetTask(ctx, in.ParentTaskID)
		if err != nil {
			return Task{}, err
		}
		if parent.ProjectID != t.ProjectID {
			return Task{}, userErr("validation", "parent task must be in the same project")
		}
		t.ParentTaskID = in.ParentTaskID
	}
	if in.PrivacyLevel != "" {
		t.PrivacyLevel = in.PrivacyLevel
	}
	if in.Scope != nil {
		t.Scope = in.Scope
	}
	if in.Requirements != nil {
		t.Requirements = in.Requirements
	}
	if in.CompletionCriteria != nil {
		t.CompletionCriteria = in.CompletionCriteria
	}
	if in.Risks != nil {
		t.Risks = in.Risks
	}
	if in.Blockers != nil {
		t.Blockers = in.Blockers
	}
	if err := validateTaskEnums(t.Type, t.Status, t.Priority, t.PrivacyLevel); err != nil {
		return Task{}, err
	}
	t.UpdatedAt = time.Now().UTC()
	_, err = s.exec(ctx, `UPDATE tasks SET project_id=?,repo_id=?,workspace_id=?,parent_task_id=?,title=?,goal=?,type=?,status=?,priority=?,updated_at=?,privacy_level=?,scope_json=?,requirements_json=?,completion_criteria_json=?,risks_json=?,blockers_json=? WHERE id=?`,
		t.ProjectID, t.RepoID, t.WorkspaceID, t.ParentTaskID, t.Title, t.Goal, t.Type, t.Status, t.Priority, ts(t.UpdatedAt), t.PrivacyLevel, js(t.Scope), js(t.Requirements), js(t.CompletionCriteria), js(t.Risks), js(t.Blockers), t.ID)
	if err != nil {
		return Task{}, err
	}
	if oldStatus != t.Status {
		_ = s.addEvent(ctx, t.ID, actorID, "task.status_changed", map[string]any{"from": oldStatus, "to": t.Status, "reason": reason})
	}
	return t, s.addEvent(ctx, t.ID, actorID, "task.updated", map[string]any{"task": t, "reason": reason})
}

func (s *Store) ClaimTask(ctx context.Context, actorID, id, reason string, force bool) (Task, error) {
	t, err := s.GetTask(ctx, id)
	if err != nil {
		return Task{}, err
	}
	now := time.Now().UTC()
	if t.OwnerID != "" && t.OwnerID != actorID && t.ClaimExpiresAt != nil && t.ClaimExpiresAt.After(now) && !force {
		_, _ = s.addConflict(ctx, Conflict{TaskID: t.ID, ActorID: actorID, ConflictType: "ownership", Scope: "task ownership", CurrentOwnerID: t.OwnerID, OtherActorID: actorID})
		return Task{}, userErr("conflict", "task is actively owned; force reassignment requires reason")
	}
	if force && t.OwnerID != "" && t.OwnerID != actorID && strings.TrimSpace(reason) == "" {
		return Task{}, userErr("validation", "reason is required to force reassign an active task")
	}
	if force && t.OwnerID != "" && t.OwnerID != actorID {
		_, _ = s.exec(ctx, `UPDATE locks SET owner_id=?, status='active', last_heartbeat_at=?, expires_at=? WHERE task_id=? AND owner_id=? AND released_at IS NULL AND status IN ('active','stale')`,
			actorID, ts(now), ts(now.Add(DefaultLockTTL)), t.ID, t.OwnerID)
	}
	if err := s.ensureTaskLockable(ctx, actorID, t); err != nil {
		return Task{}, err
	}
	exp := now.Add(DefaultClaimTTL)
	t.OwnerID = actorID
	t.Status = "claimed"
	t.UpdatedAt = now
	t.LastHeartbeatAt = &now
	t.ClaimExpiresAt = &exp
	_, err = s.exec(ctx, `UPDATE tasks SET owner_id=?,status=?,updated_at=?,last_heartbeat_at=?,claim_expires_at=? WHERE id=?`,
		t.OwnerID, t.Status, ts(t.UpdatedAt), tsPtr(t.LastHeartbeatAt), tsPtr(t.ClaimExpiresAt), t.ID)
	if err != nil {
		return Task{}, err
	}
	etype := "task.claimed"
	if force {
		etype = "task.reassigned"
	}
	if err := s.ensureTaskLocks(ctx, actorID, t); err != nil {
		return Task{}, err
	}
	return t, s.addEvent(ctx, t.ID, actorID, etype, map[string]any{"owner_id": actorID, "reason": reason})
}

func (s *Store) ReleaseTask(ctx context.Context, actorID, id string) (Task, error) {
	t, err := s.GetTask(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if t.OwnerID != "" && t.OwnerID != actorID {
		return Task{}, userErr("forbidden", "only the current owner can release this task")
	}
	now := time.Now().UTC()
	t.OwnerID = ""
	t.Status = "ready"
	t.UpdatedAt = now
	t.ClaimExpiresAt = nil
	t.LastHeartbeatAt = nil
	_, err = s.exec(ctx, `UPDATE tasks SET owner_id='',status=?,updated_at=?,last_heartbeat_at=NULL,claim_expires_at=NULL WHERE id=?`, t.Status, ts(t.UpdatedAt), t.ID)
	if err != nil {
		return Task{}, err
	}
	_, _ = s.exec(ctx, `UPDATE locks SET status='released', released_at=?, released_by=?, release_reason=? WHERE task_id=? AND owner_id=? AND released_at IS NULL AND status IN ('active','stale')`,
		ts(now), actorID, "task released", t.ID, actorID)
	_ = s.addEvent(ctx, t.ID, actorID, "claim.released_by_user", map[string]any{"task_id": t.ID})
	return t, s.addEvent(ctx, t.ID, actorID, "task.released", nil)
}

func (s *Store) HeartbeatTask(ctx context.Context, actorID, id string) (Task, error) {
	t, err := s.GetTask(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if t.OwnerID != actorID {
		return Task{}, userErr("forbidden", "only the owner can heartbeat this task")
	}
	now := time.Now().UTC()
	exp := now.Add(DefaultClaimTTL)
	t.LastHeartbeatAt = &now
	t.ClaimExpiresAt = &exp
	t.UpdatedAt = now
	_, err = s.exec(ctx, `UPDATE tasks SET last_heartbeat_at=?,claim_expires_at=?,updated_at=? WHERE id=?`, ts(now), ts(exp), ts(now), t.ID)
	if err != nil {
		return Task{}, err
	}
	if err := s.renewTaskLocks(ctx, actorID, t.ID, now); err != nil {
		return Task{}, err
	}
	return t, s.addEvent(ctx, t.ID, actorID, "task.heartbeat", map[string]any{"claim_expires_at": exp})
}

func (s *Store) StartTaskSession(ctx context.Context, actorID, taskID string) (TaskSession, error) {
	t, err := s.GetTask(ctx, taskID)
	if err != nil {
		return TaskSession{}, err
	}
	if t.OwnerID == "" || t.OwnerID != actorID {
		if _, err := s.ClaimTask(ctx, actorID, taskID, "session start", false); err != nil {
			return TaskSession{}, err
		}
	}
	if err := s.ensureTaskLockable(ctx, actorID, t); err != nil {
		return TaskSession{}, err
	}
	if err := s.ensureTaskLocks(ctx, actorID, t); err != nil {
		return TaskSession{}, err
	}
	if _, err := s.UpdateTask(ctx, actorID, taskID, TaskInput{Status: "in_progress"}, "session started"); err != nil {
		return TaskSession{}, err
	}
	now := time.Now().UTC()
	session := TaskSession{ID: newID("session"), TaskID: taskID, ActorID: actorID, StartedAt: now}
	_, err = s.exec(ctx, `INSERT INTO task_sessions (id,task_id,actor_id,started_at) VALUES (?,?,?,?)`,
		session.ID, session.TaskID, session.ActorID, ts(session.StartedAt))
	if err != nil {
		return TaskSession{}, err
	}
	return session, s.addEvent(ctx, taskID, actorID, "task.session_started", session)
}

func (s *Store) FinishTaskSession(ctx context.Context, actorID, taskID, sessionID, exitStatus, reason string) (Task, error) {
	now := time.Now().UTC()
	if sessionID != "" {
		_, _ = s.exec(ctx, `UPDATE task_sessions SET ended_at=?,exit_status=?,finish_reason=? WHERE id=?`,
			ts(now), exitStatus, reason, sessionID)
	}
	t, err := s.GetTask(ctx, taskID)
	if err != nil {
		return Task{}, err
	}
	if t.OwnerID != actorID {
		return Task{}, userErr("forbidden", "only the owner can finish this task session")
	}
	nextStatus := "claimed"
	if t.Status == "completed" || t.Status == "blocked" || t.Status == "handoff_ready" || t.Status == "in_review" || t.Status == "cancelled" {
		nextStatus = t.Status
	}
	out, err := s.UpdateTask(ctx, actorID, taskID, TaskInput{Status: nextStatus}, reason)
	if err != nil {
		return Task{}, err
	}
	eventType := "task.session_finished"
	if exitStatus != "" && exitStatus != "success" {
		eventType = "task.session_failed"
	}
	return out, s.addEvent(ctx, taskID, actorID, eventType, map[string]any{"session_id": sessionID, "exit_status": exitStatus, "reason": reason})
}

func (s *Store) CompleteTask(ctx context.Context, actorID, id, summary string) (Task, error) {
	if n := s.countIncompleteSubtasks(ctx, id); n > 0 {
		return Task{}, userErr("conflict", "cannot complete task while subtasks are still open")
	}
	if n := s.countOpenDependencies(ctx, id); n > 0 {
		return Task{}, userErr("conflict", "cannot complete task while dependencies are still open")
	}
	if summary != "" {
		if _, err := s.AppendContext(ctx, actorID, id, "summary", summary); err != nil {
			return Task{}, err
		}
	}
	t, err := s.UpdateTask(ctx, actorID, id, TaskInput{Status: "completed"}, "completed")
	if err != nil {
		return Task{}, err
	}
	return t, s.addEvent(ctx, id, actorID, "task.completed", map[string]any{"summary": summary})
}

func (s *Store) AddTaskDependency(ctx context.Context, actorID, taskID, dependsOnID string) (TaskDependency, error) {
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(dependsOnID) == "" {
		return TaskDependency{}, userErr("validation", "task_id and depends_on_id are required")
	}
	if taskID == dependsOnID {
		return TaskDependency{}, userErr("validation", "task cannot depend on itself")
	}
	task, err := s.GetTask(ctx, taskID)
	if err != nil {
		return TaskDependency{}, err
	}
	dependsOn, err := s.GetTask(ctx, dependsOnID)
	if err != nil {
		return TaskDependency{}, err
	}
	if task.ProjectID != dependsOn.ProjectID {
		return TaskDependency{}, userErr("validation", "dependency must be in the same project")
	}
	if createsDependencyCycle(ctx, s, taskID, dependsOnID) {
		return TaskDependency{}, userErr("validation", "dependency would create a cycle")
	}
	now := time.Now().UTC()
	dep := TaskDependency{ID: newID("dep"), TaskID: taskID, DependsOnID: dependsOnID, CreatedBy: actorID, CreatedAt: now}
	_, err = s.exec(ctx, `INSERT INTO task_dependencies (id,task_id,depends_on_id,created_by,created_at) VALUES (?,?,?,?,?)`,
		dep.ID, dep.TaskID, dep.DependsOnID, dep.CreatedBy, ts(dep.CreatedAt))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return TaskDependency{}, userErr("conflict", "dependency already exists")
		}
		return TaskDependency{}, err
	}
	return dep, s.addEvent(ctx, taskID, actorID, "task.dependency_added", dep)
}

func createsDependencyCycle(ctx context.Context, s *Store, taskID, dependsOnID string) bool {
	stack := []string{dependsOnID}
	seen := map[string]bool{}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if current == taskID {
			return true
		}
		if seen[current] {
			continue
		}
		seen[current] = true
		rows, err := s.query(ctx, `SELECT depends_on_id FROM task_dependencies WHERE task_id=?`, current)
		if err != nil {
			continue
		}
		for rows.Next() {
			var next string
			if rows.Scan(&next) == nil {
				stack = append(stack, next)
			}
		}
		rows.Close()
	}
	return false
}

func (s *Store) RemoveTaskDependency(ctx context.Context, actorID, dependencyID string) error {
	var taskID string
	err := s.queryRow(ctx, `SELECT task_id FROM task_dependencies WHERE id=?`, dependencyID).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return userErr("not_found", "dependency not found")
	}
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, `DELETE FROM task_dependencies WHERE id=?`, dependencyID)
	if err != nil {
		return err
	}
	return s.addEvent(ctx, taskID, actorID, "task.dependency_removed", map[string]any{"id": dependencyID})
}

func (s *Store) ListTaskDependencies(ctx context.Context, taskID string) ([]TaskDependency, error) {
	return s.listTaskDependencyRows(ctx, `WHERE task_id=?`, taskID)
}

func (s *Store) ListTaskDependents(ctx context.Context, taskID string) ([]TaskDependency, error) {
	return s.listTaskDependencyRows(ctx, `WHERE depends_on_id=?`, taskID)
}

func (s *Store) listTaskDependencyRows(ctx context.Context, where, id string) ([]TaskDependency, error) {
	rows, err := s.query(ctx, `SELECT id,task_id,depends_on_id,created_by,created_at FROM task_dependencies `+where+` ORDER BY created_at DESC`, id)
	if err != nil {
		return nil, err
	}
	out := []TaskDependency{}
	for rows.Next() {
		var dep TaskDependency
		var created string
		if err := rows.Scan(&dep.ID, &dep.TaskID, &dep.DependsOnID, &dep.CreatedBy, &created); err != nil {
			_ = rows.Close()
			return nil, err
		}
		dep.CreatedAt = parseTS(created)
		out = append(out, dep)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		if t, err := s.GetTask(ctx, out[i].TaskID); err == nil {
			out[i].Task = &t
		}
		if t, err := s.GetTask(ctx, out[i].DependsOnID); err == nil {
			out[i].DependsOnTask = &t
		}
	}
	return out, nil
}

func (s *Store) AppendContext(ctx context.Context, actorID, taskID, kind, content string) (ContextEntry, error) {
	if content == "" {
		return ContextEntry{}, userErr("validation", "context content is required")
	}
	if kind == "" {
		kind = "note"
	}
	if !oneOf(kind, "summary", "decision", "note", "risk", "blocker", "output_ref", "next") {
		return ContextEntry{}, userErr("validation", "invalid context kind")
	}
	if _, err := s.GetTask(ctx, taskID); err != nil {
		return ContextEntry{}, err
	}
	now := time.Now().UTC()
	c := ContextEntry{ID: newID("ctx"), TaskID: taskID, AuthorID: actorID, Kind: kind, Content: content, CreatedAt: now}
	_, err := s.exec(ctx, `INSERT INTO context_entries (id,task_id,author_id,kind,content,created_at) VALUES (?,?,?,?,?,?)`,
		c.ID, c.TaskID, c.AuthorID, c.Kind, c.Content, ts(c.CreatedAt))
	if err != nil {
		return ContextEntry{}, err
	}
	return c, s.addEvent(ctx, taskID, actorID, "context.appended", c)
}

func (s *Store) ListContext(ctx context.Context, taskID string) ([]ContextEntry, error) {
	rows, err := s.query(ctx, `SELECT id,task_id,author_id,kind,content,created_at FROM context_entries WHERE task_id=? ORDER BY created_at ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ContextEntry{}
	for rows.Next() {
		var c ContextEntry
		var created string
		if err := rows.Scan(&c.ID, &c.TaskID, &c.AuthorID, &c.Kind, &c.Content, &created); err != nil {
			return nil, err
		}
		c.CreatedAt = parseTS(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) CreateContextSnapshot(ctx context.Context, actorID, taskID, snapshotType string) (ContextSnapshot, error) {
	if snapshotType == "" {
		snapshotType = "periodic"
	}
	if !oneOf(snapshotType, "periodic", "milestone", "manual", "final") {
		return ContextSnapshot{}, userErr("validation", "invalid snapshot type")
	}
	detail, err := s.TaskDetail(ctx, taskID)
	if err != nil {
		return ContextSnapshot{}, err
	}
	since := time.Time{}
	if last, err := s.latestSnapshotCreatedAt(ctx, taskID); err == nil {
		since = last
	}
	entries := []ContextEntry{}
	for _, entry := range detail.Context {
		if entry.CreatedAt.After(since) || snapshotType == "manual" || snapshotType == "final" {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 && snapshotType == "periodic" {
		return ContextSnapshot{}, userErr("validation", "no new context available for snapshot")
	}
	content, sourceContextIDs := buildSnapshotContent(detail, entries)
	markdown := renderSnapshotMarkdown(content)
	now := time.Now().UTC()
	out := ContextSnapshot{
		ID:               newID("snapshot"),
		TaskID:           taskID,
		AuthorID:         actorID,
		Source:           "system",
		SnapshotType:     snapshotType,
		StatusAtTime:     detail.Task.Status,
		Summary:          content,
		Markdown:         markdown,
		SourceContextIDs: sourceContextIDs,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	_, err = s.exec(ctx, `INSERT INTO context_snapshots (id,task_id,author_id,source,snapshot_type,status_at_time,summary_json,markdown_cache,source_context_ids_json,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		out.ID, out.TaskID, out.AuthorID, out.Source, out.SnapshotType, out.StatusAtTime, js(out.Summary), out.Markdown, js(out.SourceContextIDs), ts(out.CreatedAt), ts(out.UpdatedAt))
	if err != nil {
		return ContextSnapshot{}, err
	}
	return out, s.addEvent(ctx, taskID, actorID, "context.snapshot_created", out)
}

func (s *Store) ListContextSnapshots(ctx context.Context, taskID string) ([]ContextSnapshot, error) {
	rows, err := s.query(ctx, `SELECT id,task_id,author_id,source,snapshot_type,status_at_time,summary_json,markdown_cache,source_context_ids_json,created_at,updated_at FROM context_snapshots WHERE task_id=? ORDER BY created_at ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ContextSnapshot{}
	for rows.Next() {
		snapshot, err := scanContextSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, snapshot)
	}
	return out, rows.Err()
}

func (s *Store) UpdateContextSnapshotMarkdown(ctx context.Context, actorID, snapshotID, markdown string) (ContextSnapshot, error) {
	snapshot, err := s.getContextSnapshot(ctx, snapshotID)
	if err != nil {
		return ContextSnapshot{}, err
	}
	summary, err := parseSnapshotMarkdownStrict(markdown)
	if err != nil {
		return ContextSnapshot{}, err
	}
	snapshot.Summary = summary
	snapshot.Markdown = renderSnapshotMarkdown(snapshot.Summary)
	snapshot.UpdatedAt = time.Now().UTC()
	_, err = s.exec(ctx, `UPDATE context_snapshots SET summary_json=?,markdown_cache=?,updated_at=? WHERE id=?`, js(snapshot.Summary), snapshot.Markdown, ts(snapshot.UpdatedAt), snapshot.ID)
	if err != nil {
		return ContextSnapshot{}, err
	}
	return snapshot, s.addEvent(ctx, snapshot.TaskID, actorID, "context.snapshot_edited", map[string]any{"id": snapshot.ID})
}

func (s *Store) GenerateHandoffPacket(ctx context.Context, actorID, taskID, handoffID, status string) (HandoffPacket, error) {
	if status == "" {
		status = "draft"
	}
	if status == "ready" {
		status = "published"
	}
	if !oneOf(status, "draft", "published", "accepted", "rejected", "superseded") {
		return HandoffPacket{}, userErr("validation", "invalid handoff packet status")
	}
	detail, err := s.TaskDetail(ctx, taskID)
	if err != nil {
		return HandoffPacket{}, err
	}
	snapshots := detail.Snapshots
	if len(snapshots) == 0 {
		if snapshot, err := s.CreateContextSnapshot(ctx, actorID, taskID, "final"); err == nil {
			snapshots = []ContextSnapshot{snapshot}
		}
	}
	packetContent, sourceSnapshotIDs, sourceContextIDs := buildHandoffPacketContent(detail, snapshots)
	markdown := renderHandoffMarkdown(packetContent)
	now := time.Now().UTC()
	out := HandoffPacket{
		ID:                 newID("packet"),
		TaskID:             taskID,
		HandoffID:          handoffID,
		GeneratedBy:        actorID,
		Status:             status,
		Version:            1,
		Source:             "generated_fallback",
		ValidationErrors:   validateHandoffQuality(packetContent),
		SupportingEvidence: handoffSupportingEvidence(packetContent),
		Packet:             packetContent,
		Markdown:           markdown,
		SourceSnapshotIDs:  sourceSnapshotIDs,
		SourceContextIDs:   sourceContextIDs,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	_, _ = s.exec(ctx, `UPDATE handoff_packets SET status='superseded' WHERE task_id=? AND status IN ('draft','published')`, taskID)
	_, err = s.exec(ctx, `INSERT INTO handoff_packets (id,task_id,handoff_id,generated_by,status,version,packet_json,markdown_cache,source_snapshot_ids_json,source_context_ids_json,source,validation_errors_json,supporting_evidence_json,edited_by,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		out.ID, out.TaskID, nullableString(out.HandoffID), out.GeneratedBy, out.Status, out.Version, js(out.Packet), out.Markdown, js(out.SourceSnapshotIDs), js(out.SourceContextIDs), out.Source, js(out.ValidationErrors), js(out.SupportingEvidence), nil, ts(out.CreatedAt), ts(out.UpdatedAt))
	if err != nil {
		return HandoffPacket{}, err
	}
	return out, s.addEvent(ctx, taskID, actorID, "handoff.packet_generated", out)
}

func (s *Store) LatestHandoffPacket(ctx context.Context, taskID string) (*HandoffPacket, error) {
	row := s.queryRow(ctx, `SELECT id,task_id,handoff_id,generated_by,status,version,packet_json,markdown_cache,source_snapshot_ids_json,source_context_ids_json,source,validation_errors_json,supporting_evidence_json,edited_by,created_at,updated_at FROM handoff_packets WHERE task_id=? ORDER BY updated_at DESC LIMIT 1`, taskID)
	packet, err := scanHandoffPacket(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &packet, nil
}

func (s *Store) UpdateHandoffPacketMarkdown(ctx context.Context, actorID, packetID, markdown string) (HandoffPacket, error) {
	return s.UpdateHandoffPacketMarkdownWithSource(ctx, actorID, packetID, markdown, "manually_edited")
}

func (s *Store) UpdateHandoffPacketMarkdownWithSource(ctx context.Context, actorID, packetID, markdown, source string) (HandoffPacket, error) {
	packet, err := s.getHandoffPacket(ctx, packetID)
	if err != nil {
		return HandoffPacket{}, err
	}
	if !oneOf(source, "agent_authored", "generated_fallback", "manually_edited") {
		source = "manually_edited"
	}
	content, err := parseHandoffMarkdownStrict(markdown, false)
	if err != nil {
		return HandoffPacket{}, err
	}
	packet.Packet = content
	packet.Source = source
	packet.ValidationErrors = validateHandoffQuality(content)
	if len(packet.SupportingEvidence) == 0 {
		packet.SupportingEvidence = handoffSupportingEvidence(content)
	}
	packet.Markdown = renderHandoffMarkdown(packet.Packet)
	packet.EditedBy = actorID
	packet.Version++
	packet.UpdatedAt = time.Now().UTC()
	_, err = s.exec(ctx, `UPDATE handoff_packets SET packet_json=?,markdown_cache=?,source=?,validation_errors_json=?,supporting_evidence_json=?,edited_by=?,version=?,updated_at=? WHERE id=?`, js(packet.Packet), packet.Markdown, packet.Source, js(packet.ValidationErrors), js(packet.SupportingEvidence), actorID, packet.Version, ts(packet.UpdatedAt), packet.ID)
	if err != nil {
		return HandoffPacket{}, err
	}
	return packet, s.addEvent(ctx, packet.TaskID, actorID, "handoff.packet_edited", map[string]any{"id": packet.ID})
}

func (s *Store) PublishHandoffPacket(ctx context.Context, actorID, packetID string) (HandoffPacket, error) {
	packet, err := s.getHandoffPacket(ctx, packetID)
	if err != nil {
		return HandoffPacket{}, err
	}
	content, err := parseHandoffMarkdownStrict(packet.Markdown, true)
	if err != nil {
		return HandoffPacket{}, err
	}
	packet.Packet = content
	packet.ValidationErrors = validateHandoffQuality(content)
	if len(packet.ValidationErrors) > 0 {
		return HandoffPacket{}, markdownValidationErrors(packet.ValidationErrors)
	}
	if packet.HandoffID == "" {
		summary := strings.TrimSpace(packet.Packet.HandoffMessage)
		if summary == "" {
			summary = strings.TrimSpace(packet.Packet.TaskObjective)
		}
		if summary == "" {
			summary = "Handoff prepared from TaskPilot memory."
		}
		h := Handoff{ID: newID("handoff"), TaskID: packet.TaskID, FromActorID: actorID, Status: "prepared", ResumeSummary: summary, NextSteps: packet.Packet.SuggestedNextSteps, CreatedAt: time.Now().UTC()}
		_, err := s.exec(ctx, `INSERT INTO handoffs (id,task_id,from_actor_id,to_actor_id,status,resume_summary,next_steps_json,created_at,accepted_at) VALUES (?,?,?,?,?,?,?,?,NULL)`,
			h.ID, h.TaskID, h.FromActorID, h.ToActorID, h.Status, h.ResumeSummary, js(h.NextSteps), ts(h.CreatedAt))
		if err != nil {
			return HandoffPacket{}, err
		}
		packet.HandoffID = h.ID
	}
	packet.Status = "published"
	packet.Version++
	packet.Markdown = renderHandoffMarkdown(packet.Packet)
	packet.UpdatedAt = time.Now().UTC()
	_, err = s.exec(ctx, `UPDATE handoff_packets SET handoff_id=?,status='published',version=?,packet_json=?,markdown_cache=?,source=?,validation_errors_json=?,supporting_evidence_json=?,edited_by=?,updated_at=? WHERE id=?`,
		nullableString(packet.HandoffID), packet.Version, js(packet.Packet), packet.Markdown, packet.Source, js(packet.ValidationErrors), js(packet.SupportingEvidence), actorID, ts(packet.UpdatedAt), packet.ID)
	if err != nil {
		return HandoffPacket{}, err
	}
	if _, err := s.UpdateTask(ctx, actorID, packet.TaskID, TaskInput{Status: "handoff_ready"}, "handoff packet published"); err != nil {
		return HandoffPacket{}, err
	}
	return packet, s.addEvent(ctx, packet.TaskID, actorID, "handoff.packet_published", packet)
}

func (s *Store) latestSnapshotCreatedAt(ctx context.Context, taskID string) (time.Time, error) {
	var created string
	if err := s.queryRow(ctx, `SELECT created_at FROM context_snapshots WHERE task_id=? ORDER BY created_at DESC LIMIT 1`, taskID).Scan(&created); err != nil {
		return time.Time{}, err
	}
	return parseTS(created), nil
}

func (s *Store) AddDecision(ctx context.Context, actorID, taskID, decision string, alternatives []string, reason, impact string) (DecisionRecord, error) {
	decision = strings.TrimSpace(decision)
	if decision == "" {
		return DecisionRecord{}, userErr("validation", "decision is required")
	}
	if _, err := s.GetTask(ctx, taskID); err != nil {
		return DecisionRecord{}, err
	}
	alternatives = cleanStrings(alternatives)
	now := time.Now().UTC()
	d := DecisionRecord{ID: newID("dec"), TaskID: taskID, AuthorID: actorID, Decision: decision, Alternatives: alternatives, Reason: strings.TrimSpace(reason), Impact: strings.TrimSpace(impact), CreatedAt: now}
	_, err := s.exec(ctx, `INSERT INTO decision_records (id,task_id,author_id,decision,alternatives_json,reason,impact,created_at) VALUES (?,?,?,?,?,?,?,?)`,
		d.ID, d.TaskID, d.AuthorID, d.Decision, js(d.Alternatives), d.Reason, d.Impact, ts(d.CreatedAt))
	if err != nil {
		return DecisionRecord{}, err
	}
	return d, s.addEvent(ctx, taskID, actorID, "decision.recorded", d)
}

func (s *Store) ListDecisions(ctx context.Context, taskID string) ([]DecisionRecord, error) {
	rows, err := s.query(ctx, `SELECT id,task_id,author_id,decision,alternatives_json,reason,impact,created_at FROM decision_records WHERE task_id=? ORDER BY created_at ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DecisionRecord{}
	for rows.Next() {
		var d DecisionRecord
		var alternatives, created string
		if err := rows.Scan(&d.ID, &d.TaskID, &d.AuthorID, &d.Decision, &alternatives, &d.Reason, &d.Impact, &created); err != nil {
			return nil, err
		}
		fromJS(alternatives, &d.Alternatives)
		d.CreatedAt = parseTS(created)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) AddComment(ctx context.Context, actorID, taskID, body string) (Comment, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return Comment{}, userErr("validation", "comment body is required")
	}
	if _, err := s.GetTask(ctx, taskID); err != nil {
		return Comment{}, err
	}
	now := time.Now().UTC()
	c := Comment{ID: newID("cmt"), TaskID: taskID, AuthorID: actorID, Body: body, CreatedAt: now}
	_, err := s.exec(ctx, `INSERT INTO comments (id,task_id,author_id,body,created_at) VALUES (?,?,?,?,?)`,
		c.ID, c.TaskID, c.AuthorID, c.Body, ts(c.CreatedAt))
	if err != nil {
		return Comment{}, err
	}
	return c, s.addEvent(ctx, taskID, actorID, "comment.added", c)
}

func (s *Store) ListComments(ctx context.Context, taskID string) ([]Comment, error) {
	rows, err := s.query(ctx, `SELECT id,task_id,author_id,body,created_at FROM comments WHERE task_id=? ORDER BY created_at ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Comment{}
	for rows.Next() {
		var c Comment
		var created string
		if err := rows.Scan(&c.ID, &c.TaskID, &c.AuthorID, &c.Body, &created); err != nil {
			return nil, err
		}
		c.CreatedAt = parseTS(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) AddArtifact(ctx context.Context, actorID, taskID, kind, title, uri, description string, metadata map[string]any) (Artifact, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "other"
	}
	if !oneOf(kind, "pr", "log", "branch", "doc", "screenshot", "output", "other") {
		return Artifact{}, userErr("validation", "invalid artifact kind")
	}
	title = strings.TrimSpace(title)
	uri = strings.TrimSpace(uri)
	if title == "" {
		return Artifact{}, userErr("validation", "artifact title is required")
	}
	if uri == "" {
		return Artifact{}, userErr("validation", "artifact uri is required")
	}
	if _, err := s.GetTask(ctx, taskID); err != nil {
		return Artifact{}, err
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	now := time.Now().UTC()
	a := Artifact{ID: newID("artifact"), TaskID: taskID, AuthorID: actorID, Kind: kind, Title: title, URI: uri, Description: strings.TrimSpace(description), Metadata: metadata, CreatedAt: now}
	_, err := s.exec(ctx, `INSERT INTO artifacts (id,task_id,author_id,kind,title,uri,description,metadata_json,created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		a.ID, a.TaskID, a.AuthorID, a.Kind, a.Title, a.URI, a.Description, js(a.Metadata), ts(a.CreatedAt))
	if err != nil {
		return Artifact{}, err
	}
	return a, s.addEvent(ctx, taskID, actorID, "artifact.referenced", a)
}

func (s *Store) ListArtifacts(ctx context.Context, taskID string) ([]Artifact, error) {
	rows, err := s.query(ctx, `SELECT id,task_id,author_id,kind,title,uri,description,metadata_json,created_at FROM artifacts WHERE task_id=? ORDER BY created_at ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Artifact{}
	for rows.Next() {
		var a Artifact
		var metadata, created string
		if err := rows.Scan(&a.ID, &a.TaskID, &a.AuthorID, &a.Kind, &a.Title, &a.URI, &a.Description, &metadata, &created); err != nil {
			return nil, err
		}
		fromJS(metadata, &a.Metadata)
		if a.Metadata == nil {
			a.Metadata = map[string]any{}
		}
		a.CreatedAt = parseTS(created)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) AddGitRef(ctx context.Context, actorID, taskID, branch, commitSHA, prURL string, changedFiles []string, note string) (GitRef, error) {
	branch = strings.TrimSpace(branch)
	commitSHA = strings.TrimSpace(commitSHA)
	prURL = strings.TrimSpace(prURL)
	changedFiles = cleanStrings(changedFiles)
	if branch == "" && commitSHA == "" && prURL == "" && len(changedFiles) == 0 {
		return GitRef{}, userErr("validation", "at least one git metadata field is required")
	}
	if _, err := s.GetTask(ctx, taskID); err != nil {
		return GitRef{}, err
	}
	now := time.Now().UTC()
	g := GitRef{ID: newID("git"), TaskID: taskID, AuthorID: actorID, Branch: branch, CommitSHA: commitSHA, PRURL: prURL, ChangedFiles: changedFiles, Note: strings.TrimSpace(note), CreatedAt: now}
	_, err := s.exec(ctx, `INSERT INTO git_refs (id,task_id,author_id,branch,commit_sha,pr_url,changed_files_json,note,created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		g.ID, g.TaskID, g.AuthorID, nullableString(g.Branch), nullableString(g.CommitSHA), nullableString(g.PRURL), js(g.ChangedFiles), g.Note, ts(g.CreatedAt))
	if err != nil {
		return GitRef{}, err
	}
	return g, s.addEvent(ctx, taskID, actorID, "git.metadata_attached", g)
}

func (s *Store) ListGitRefs(ctx context.Context, taskID string) ([]GitRef, error) {
	rows, err := s.query(ctx, `SELECT id,task_id,author_id,branch,commit_sha,pr_url,changed_files_json,note,created_at FROM git_refs WHERE task_id=? ORDER BY created_at ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GitRef{}
	for rows.Next() {
		var g GitRef
		var branch, commit, pr sql.NullString
		var changed, created string
		if err := rows.Scan(&g.ID, &g.TaskID, &g.AuthorID, &branch, &commit, &pr, &changed, &g.Note, &created); err != nil {
			return nil, err
		}
		g.Branch = branch.String
		g.CommitSHA = commit.String
		g.PRURL = pr.String
		fromJS(changed, &g.ChangedFiles)
		g.CreatedAt = parseTS(created)
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) addConflict(ctx context.Context, in Conflict) (Conflict, error) {
	if in.ConflictType == "" {
		in.ConflictType = "unknown"
	}
	now := time.Now().UTC()
	c := Conflict{
		ID:                newID("conflict"),
		TaskID:            in.TaskID,
		ActorID:           in.ActorID,
		ConflictType:      in.ConflictType,
		Status:            "open",
		Scope:             strings.TrimSpace(in.Scope),
		ScopeType:         in.ScopeType,
		CurrentOwnerID:    in.CurrentOwnerID,
		OtherActorID:      in.OtherActorID,
		OtherTaskID:       in.OtherTaskID,
		LockID:            in.LockID,
		ConflictingLockID: in.ConflictingLockID,
		CreatedAt:         now,
	}
	_, err := s.exec(ctx, `INSERT INTO conflicts (id,task_id,actor_id,conflict_type,status,scope,scope_type,current_owner_id,other_actor_id,other_task_id,lock_id,conflicting_lock_id,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.TaskID, c.ActorID, c.ConflictType, c.Status, c.Scope, c.ScopeType, c.CurrentOwnerID, c.OtherActorID, c.OtherTaskID, c.LockID, c.ConflictingLockID, ts(c.CreatedAt))
	if err != nil {
		return Conflict{}, err
	}
	return c, s.addEvent(ctx, c.TaskID, c.ActorID, "conflict.detected", c)
}

func (s *Store) ListConflicts(ctx context.Context, status string) ([]Conflict, error) {
	q := `SELECT id,task_id,actor_id,conflict_type,status,scope,scope_type,current_owner_id,other_actor_id,other_task_id,lock_id,conflicting_lock_id,resolution,resolution_note,created_at,resolved_at,resolved_by FROM conflicts`
	args := []any{}
	if status != "" {
		q += ` WHERE status=?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := []Conflict{}
	for rows.Next() {
		c, err := scanConflict(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].TaskID != "" {
			if t, err := s.GetTask(ctx, out[i].TaskID); err == nil {
				out[i].Task = &t
			}
		}
		if out[i].OtherTaskID != "" {
			if t, err := s.GetTask(ctx, out[i].OtherTaskID); err == nil {
				out[i].OtherTask = &t
			}
		}
	}
	if status == "open" {
		filtered := out[:0]
		for _, c := range out {
			if c.Task != nil && !isActiveConflictTaskStatus(c.Task.Status) {
				continue
			}
			if c.OtherTask != nil && !isActiveConflictTaskStatus(c.OtherTask.Status) {
				continue
			}
			filtered = append(filtered, c)
		}
		out = filtered
	}
	return out, nil
}

func (s *Store) ListStaleClaims(ctx context.Context) ([]StaleClaim, error) {
	now := time.Now().UTC()
	tasks, err := s.ListTasks(ctx, "")
	if err != nil {
		return nil, err
	}
	out := []StaleClaim{}
	for _, task := range tasks {
		if task.OwnerID == "" || task.ClaimExpiresAt == nil || task.ClaimExpiresAt.After(now) || !isActiveConflictTaskStatus(task.Status) {
			continue
		}
		var owner *Actor
		if a, err := s.GetActor(ctx, task.OwnerID); err == nil {
			owner = a
		}
		last := task.LastHeartbeatAt
		if last == nil {
			last = &task.UpdatedAt
		}
		age := now.Sub(*last).Round(time.Second)
		claim := task.UpdatedAt
		stale := StaleClaim{
			Task:             task,
			Owner:            owner,
			ClaimedAt:        &claim,
			LastActivityAt:   last,
			ClaimExpiresAt:   task.ClaimExpiresAt,
			StaleThreshold:   DefaultClaimTTL.String(),
			Reason:           fmt.Sprintf("Claim expired because last activity was %s ago; threshold is %s.", age, DefaultClaimTTL),
			SuggestedActions: []string{"wait", "contact_owner", "release_claim", "reassign"},
		}
		out = append(out, stale)
	}
	return out, nil
}

func (s *Store) ResolveConflict(ctx context.Context, actorID, conflictID, resolution, note, targetActorID string) (Conflict, error) {
	resolution = strings.TrimSpace(resolution)
	note = strings.TrimSpace(note)
	if !oneOf(resolution, "continue_current_owner", "transfer_ownership", "split_scope", "pause_secondary_work", "mark_duplicate", "escalate_to_human") {
		return Conflict{}, userErr("validation", "invalid conflict resolution")
	}
	if note == "" {
		return Conflict{}, userErr("validation", "resolution note is required")
	}
	c, err := s.getConflict(ctx, conflictID)
	if err != nil {
		return Conflict{}, err
	}
	if c.Status == "resolved" {
		return Conflict{}, userErr("conflict", "conflict is already resolved")
	}
	if err := s.applyConflictResolution(ctx, actorID, c, resolution, note, targetActorID); err != nil {
		return Conflict{}, err
	}
	now := time.Now().UTC()
	_, err = s.exec(ctx, `UPDATE conflicts SET status='resolved', resolution=?, resolution_note=?, resolved_at=?, resolved_by=? WHERE id=?`,
		resolution, note, ts(now), actorID, conflictID)
	if err != nil {
		return Conflict{}, err
	}
	c.Status = "resolved"
	c.Resolution = resolution
	c.ResolutionNote = note
	c.ResolvedAt = &now
	c.ResolvedBy = actorID
	return c, s.addEvent(ctx, c.TaskID, actorID, "conflict.resolved", c)
}

func (s *Store) applyConflictResolution(ctx context.Context, actorID string, c Conflict, resolution, note, targetActorID string) error {
	taskID := c.TaskID
	switch resolution {
	case "continue_current_owner":
		if taskID != "" {
			_, _ = s.AppendContext(ctx, actorID, taskID, "summary", "Conflict resolved: continuing current owner. "+note)
		}
	case "transfer_ownership":
		target := strings.TrimSpace(targetActorID)
		if target == "" {
			target = c.OtherActorID
		}
		if target == "" {
			return userErr("validation", "target_actor_id is required for transfer_ownership")
		}
		if taskID != "" {
			_, _ = s.exec(ctx, `UPDATE locks SET owner_id=?, status='active', last_heartbeat_at=?, expires_at=? WHERE task_id=? AND released_at IS NULL AND status IN ('active','stale')`,
				target, ts(time.Now().UTC()), ts(time.Now().UTC().Add(DefaultLockTTL)), taskID)
			if _, err := s.ClaimTask(ctx, target, taskID, "conflict resolution by "+actorID+": "+note, true); err != nil {
				return err
			}
		}
	case "split_scope":
		if taskID != "" {
			_, _ = s.AppendContext(ctx, actorID, taskID, "decision", "Conflict resolved by splitting scope: "+note)
		}
	case "pause_secondary_work":
		if taskID != "" {
			_, _ = s.AppendContext(ctx, actorID, taskID, "blocker", "Paused by conflict resolution: "+note)
			if _, err := s.UpdateTask(ctx, actorID, taskID, TaskInput{Status: "blocked"}, "conflict resolution: pause secondary work"); err != nil {
				return err
			}
		}
	case "mark_duplicate":
		if taskID != "" {
			_, _ = s.AppendContext(ctx, actorID, taskID, "summary", "Marked duplicate by conflict resolution: "+note)
			if _, err := s.UpdateTask(ctx, actorID, taskID, TaskInput{Status: "cancelled"}, "conflict resolution: duplicate"); err != nil {
				return err
			}
		}
	case "escalate_to_human":
		if taskID != "" {
			_, _ = s.AppendContext(ctx, actorID, taskID, "blocker", "Escalated for human decision: "+note)
			if _, err := s.UpdateTask(ctx, actorID, taskID, TaskInput{Status: "blocked"}, "conflict resolution: escalate to human"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) getConflict(ctx context.Context, id string) (Conflict, error) {
	row := s.queryRow(ctx, `SELECT id,task_id,actor_id,conflict_type,status,scope,scope_type,current_owner_id,other_actor_id,other_task_id,lock_id,conflicting_lock_id,resolution,resolution_note,created_at,resolved_at,resolved_by FROM conflicts WHERE id=?`, id)
	c, err := scanConflict(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Conflict{}, userErr("not_found", "conflict not found")
	}
	return c, err
}

func (s *Store) AcquireLock(ctx context.Context, actorID, taskID, scope, scopeType string) (Lock, []Lock, error) {
	if strings.TrimSpace(scope) == "" {
		return Lock{}, nil, userErr("validation", "lock scope is required")
	}
	if scopeType == "" {
		scopeType = "file_glob"
	}
	if !oneOf(scopeType, "file_glob", "semantic_area", "artifact") {
		return Lock{}, nil, userErr("validation", "invalid lock scope type")
	}
	if _, err := s.GetTask(ctx, taskID); err != nil {
		return Lock{}, nil, err
	}
	_ = s.markStaleLocks(ctx, time.Now().UTC())
	conflicts, err := s.FindLockConflicts(ctx, actorID, scope, scopeType)
	if err != nil {
		return Lock{}, nil, err
	}
	if len(conflicts) > 0 {
		for _, lockConflict := range conflicts {
			_, _ = s.addConflict(ctx, Conflict{
				TaskID:            taskID,
				ActorID:           actorID,
				ConflictType:      "lock_overlap",
				Scope:             scope,
				ScopeType:         scopeType,
				CurrentOwnerID:    lockConflict.OwnerID,
				OtherActorID:      actorID,
				OtherTaskID:       lockConflict.TaskID,
				ConflictingLockID: lockConflict.ID,
			})
		}
		return Lock{}, conflicts, userErr("conflict", "active overlapping lock exists")
	}
	now := time.Now().UTC()
	l := Lock{ID: newID("lock"), TaskID: taskID, OwnerID: actorID, Scope: scope, ScopeType: scopeType, Status: "active", ExpiresAt: now.Add(DefaultLockTTL), LastHeartbeatAt: now, CreatedAt: now}
	_, err = s.exec(ctx, `INSERT INTO locks (id,task_id,owner_id,scope,scope_type,status,expires_at,last_heartbeat_at,created_at,released_at) VALUES (?,?,?,?,?,?,?,?,?,NULL)`,
		l.ID, l.TaskID, l.OwnerID, l.Scope, l.ScopeType, l.Status, ts(l.ExpiresAt), ts(l.LastHeartbeatAt), ts(l.CreatedAt))
	if err != nil {
		return Lock{}, nil, err
	}
	return l, nil, s.addEvent(ctx, taskID, actorID, "lock.acquired", l)
}

func (s *Store) ListLocks(ctx context.Context, taskID string, activeOnly bool) ([]Lock, error) {
	_ = s.markStaleLocks(ctx, time.Now().UTC())
	q := `SELECT id,task_id,owner_id,scope,scope_type,status,expires_at,last_heartbeat_at,created_at,released_at,released_by,release_reason,overridden_at,overridden_by,override_reason FROM locks`
	args := []any{}
	clauses := []string{}
	if taskID != "" {
		clauses = append(clauses, "task_id=?")
		args = append(args, taskID)
	}
	if activeOnly {
		clauses = append(clauses, "released_at IS NULL AND status IN ('active','stale')")
	}
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	q += " ORDER BY created_at DESC"
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Lock{}
	for rows.Next() {
		l, err := scanLock(rows)
		if err != nil {
			return nil, err
		}
		s.hydrateLockDisplay(ctx, &l)
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) FindLockConflicts(ctx context.Context, actorID, scope, scopeType string) ([]Lock, error) {
	locks, err := s.ListLocks(ctx, "", true)
	if err != nil {
		return nil, err
	}
	var conflicts []Lock
	for _, l := range locks {
		if l.OwnerID == actorID {
			continue
		}
		if scopesOverlap(scopeType, scope, l.ScopeType, l.Scope) {
			s.hydrateLockDisplay(ctx, &l)
			conflicts = append(conflicts, l)
		}
	}
	return conflicts, nil
}

func (s *Store) ReleaseLock(ctx context.Context, actorID, lockID string) (Lock, error) {
	return s.ReleaseLockWithReason(ctx, actorID, lockID, "")
}

func (s *Store) ReleaseLockWithReason(ctx context.Context, actorID, lockID, reason string) (Lock, error) {
	l, err := s.getLock(ctx, lockID)
	if err != nil {
		return Lock{}, err
	}
	if l.OwnerID != actorID {
		return Lock{}, userErr("forbidden", "only the lock owner can release this lock")
	}
	now := time.Now().UTC()
	l.ReleasedAt = &now
	l.ReleasedBy = actorID
	l.ReleaseReason = strings.TrimSpace(reason)
	l.Status = "released"
	_, err = s.exec(ctx, `UPDATE locks SET released_at=?, released_by=?, release_reason=?, status='released' WHERE id=?`, ts(now), actorID, l.ReleaseReason, lockID)
	if err != nil {
		return Lock{}, err
	}
	return l, s.addEvent(ctx, l.TaskID, actorID, "lock.released", l)
}

func (s *Store) RenewLock(ctx context.Context, actorID, lockID string) (Lock, error) {
	l, err := s.getLock(ctx, lockID)
	if err != nil {
		return Lock{}, err
	}
	if l.OwnerID != actorID {
		return Lock{}, userErr("forbidden", "only the lock owner can renew this lock")
	}
	now := time.Now().UTC()
	l.ExpiresAt = now.Add(DefaultLockTTL)
	l.LastHeartbeatAt = now
	l.Status = "active"
	_, err = s.exec(ctx, `UPDATE locks SET expires_at=?, last_heartbeat_at=?, status='active' WHERE id=?`, ts(l.ExpiresAt), ts(l.LastHeartbeatAt), lockID)
	if err != nil {
		return Lock{}, err
	}
	return l, s.addEvent(ctx, l.TaskID, actorID, "lock.renewed", l)
}

func (s *Store) OverrideLock(ctx context.Context, actorID, lockID, reason string) (Lock, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Lock{}, userErr("validation", "override reason is required")
	}
	l, err := s.getLock(ctx, lockID)
	if err != nil {
		return Lock{}, err
	}
	now := time.Now().UTC()
	l.Status = "overridden"
	l.OverriddenAt = &now
	l.OverriddenBy = actorID
	l.OverrideReason = reason
	_, err = s.exec(ctx, `UPDATE locks SET status='overridden', overridden_at=?, overridden_by=?, override_reason=? WHERE id=?`,
		ts(now), actorID, reason, lockID)
	if err != nil {
		return Lock{}, err
	}
	return l, s.addEvent(ctx, l.TaskID, actorID, "lock.overridden", l)
}

func (s *Store) ensureTaskLockable(ctx context.Context, actorID string, task Task) error {
	for _, scope := range taskLockScopes(task) {
		conflicts, err := s.FindLockConflicts(ctx, actorID, scope.scope, scope.scopeType)
		if err != nil {
			return err
		}
		if len(conflicts) > 0 {
			for _, lockConflict := range conflicts {
				_, _ = s.addConflict(ctx, Conflict{
					TaskID:            task.ID,
					ActorID:           actorID,
					ConflictType:      "lock_overlap",
					Scope:             scope.scope,
					ScopeType:         scope.scopeType,
					CurrentOwnerID:    lockConflict.OwnerID,
					OtherActorID:      actorID,
					OtherTaskID:       lockConflict.TaskID,
					ConflictingLockID: lockConflict.ID,
				})
			}
			return userErr("conflict", lockConflictMessage(conflicts[0]))
		}
	}
	return nil
}

func (s *Store) ensureTaskLocks(ctx context.Context, actorID string, task Task) error {
	for _, scope := range taskLockScopes(task) {
		if err := s.ensureOwnedLock(ctx, actorID, task.ID, scope.scope, scope.scopeType); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ensureOwnedLock(ctx context.Context, actorID, taskID, scope, scopeType string) error {
	locks, err := s.ListLocks(ctx, taskID, true)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, l := range locks {
		if l.OwnerID == actorID && l.Scope == scope && l.ScopeType == scopeType {
			_, err := s.exec(ctx, `UPDATE locks SET status='active', expires_at=?, last_heartbeat_at=? WHERE id=?`,
				ts(now.Add(DefaultLockTTL)), ts(now), l.ID)
			return err
		}
	}
	_, _, err = s.AcquireLock(ctx, actorID, taskID, scope, scopeType)
	return err
}

func (s *Store) renewTaskLocks(ctx context.Context, actorID, taskID string, now time.Time) error {
	_, err := s.exec(ctx, `UPDATE locks SET status='active', expires_at=?, last_heartbeat_at=? WHERE task_id=? AND owner_id=? AND released_at IS NULL AND status IN ('active','stale')`,
		ts(now.Add(DefaultLockTTL)), ts(now), taskID, actorID)
	return err
}

func (s *Store) markStaleLocks(ctx context.Context, now time.Time) error {
	_, err := s.exec(ctx, `UPDATE locks SET status='stale' WHERE released_at IS NULL AND status='active' AND expires_at<=?`, ts(now))
	return err
}

func taskLockScopes(task Task) []struct {
	scope     string
	scopeType string
} {
	if len(task.Scope) == 0 {
		return []struct {
			scope     string
			scopeType string
		}{{scope: "task:" + task.ID, scopeType: "semantic_area"}}
	}
	out := make([]struct {
		scope     string
		scopeType string
	}, 0, len(task.Scope))
	for _, scope := range task.Scope {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		out = append(out, struct {
			scope     string
			scopeType string
		}{scope: scope, scopeType: "file_glob"})
	}
	if len(out) == 0 {
		out = append(out, struct {
			scope     string
			scopeType string
		}{scope: "task:" + task.ID, scopeType: "semantic_area"})
	}
	return out
}

func lockConflictMessage(lock Lock) string {
	owner := lock.OwnerName
	if owner == "" {
		owner = lock.OwnerID
	}
	return fmt.Sprintf("This task/scope is currently being worked on by %s. Please wait, or coordinate with the owner to release the lock.", owner)
}

func (s *Store) hydrateLockDisplay(ctx context.Context, l *Lock) {
	if l == nil {
		return
	}
	if actor, err := s.GetActor(ctx, l.OwnerID); err == nil {
		l.OwnerName = actor.Name
	}
	if task, err := s.GetTask(ctx, l.TaskID); err == nil {
		l.TaskTitle = task.Title
	}
	l.Message = lockConflictMessage(*l)
}

func (s *Store) PrepareHandoff(ctx context.Context, actorID, taskID, toActorID, summary string, next []string) (Handoff, error) {
	if summary == "" {
		return Handoff{}, userErr("validation", "handoff summary is required")
	}
	if _, err := s.GetTask(ctx, taskID); err != nil {
		return Handoff{}, err
	}
	now := time.Now().UTC()
	h := Handoff{ID: newID("handoff"), TaskID: taskID, FromActorID: actorID, ToActorID: toActorID, Status: "prepared", ResumeSummary: summary, NextSteps: next, CreatedAt: now}
	_, err := s.exec(ctx, `INSERT INTO handoffs (id,task_id,from_actor_id,to_actor_id,status,resume_summary,next_steps_json,created_at,accepted_at) VALUES (?,?,?,?,?,?,?,?,NULL)`,
		h.ID, h.TaskID, h.FromActorID, h.ToActorID, h.Status, h.ResumeSummary, js(h.NextSteps), ts(h.CreatedAt))
	if err != nil {
		return Handoff{}, err
	}
	if _, err := s.UpdateTask(ctx, actorID, taskID, TaskInput{Status: "handoff_ready"}, "handoff prepared"); err != nil {
		return Handoff{}, err
	}
	return h, s.addEvent(ctx, taskID, actorID, "handoff.prepared", h)
}

func (s *Store) AcceptHandoff(ctx context.Context, actorID, handoffID string) (Handoff, error) {
	h, err := s.getHandoff(ctx, handoffID)
	if err != nil {
		return Handoff{}, err
	}
	if h.ToActorID != "" && h.ToActorID != actorID {
		return Handoff{}, userErr("forbidden", "handoff is targeted to another actor")
	}
	now := time.Now().UTC()
	h.Status = "accepted"
	h.AcceptedAt = &now
	_, err = s.exec(ctx, `UPDATE handoffs SET status=?,accepted_at=? WHERE id=?`, h.Status, ts(now), h.ID)
	if err != nil {
		return Handoff{}, err
	}
	if _, err := s.exec(ctx, `UPDATE locks SET owner_id=?, status='active', last_heartbeat_at=?, expires_at=? WHERE task_id=? AND owner_id=? AND released_at IS NULL AND status IN ('active','stale')`,
		actorID, ts(now), ts(now.Add(DefaultLockTTL)), h.TaskID, h.FromActorID); err != nil {
		return Handoff{}, err
	}
	if _, err := s.ClaimTask(ctx, actorID, h.TaskID, "handoff accepted", true); err != nil {
		return Handoff{}, err
	}
	_, _ = s.exec(ctx, `UPDATE handoff_packets SET status='accepted' WHERE handoff_id=?`, h.ID)
	_ = s.addEvent(ctx, h.TaskID, actorID, "lock.transferred", map[string]any{"from_actor_id": h.FromActorID, "to_actor_id": actorID})
	return h, s.addEvent(ctx, h.TaskID, actorID, "handoff.accepted", h)
}

func (s *Store) RejectHandoff(ctx context.Context, actorID, handoffID string) (Handoff, error) {
	h, err := s.getHandoff(ctx, handoffID)
	if err != nil {
		return Handoff{}, err
	}
	h.Status = "rejected"
	_, err = s.exec(ctx, `UPDATE handoffs SET status=? WHERE id=?`, h.Status, h.ID)
	if err != nil {
		return Handoff{}, err
	}
	_, _ = s.exec(ctx, `UPDATE handoff_packets SET status='rejected' WHERE handoff_id=?`, h.ID)
	return h, s.addEvent(ctx, h.TaskID, actorID, "handoff.rejected", h)
}

func (s *Store) ListHandoffs(ctx context.Context, taskID string) ([]Handoff, error) {
	q := `SELECT id,task_id,from_actor_id,to_actor_id,status,resume_summary,next_steps_json,created_at,accepted_at FROM handoffs`
	args := []any{}
	if taskID != "" {
		q += ` WHERE task_id=?`
		args = append(args, taskID)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Handoff{}
	for rows.Next() {
		h, err := scanHandoff(rows)
		if err != nil {
			return nil, err
		}
		if t, err := s.GetTask(ctx, h.TaskID); err == nil {
			h.Task = &t
		}
		if p, err := s.LatestHandoffPacket(ctx, h.TaskID); err == nil {
			h.Packet = p
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) ListEvents(ctx context.Context, since int64, taskID string) ([]Event, error) {
	q := `SELECT id,task_id,actor_id,event_type,payload_json,created_at FROM events WHERE id>?`
	args := []any{since}
	if taskID != "" {
		q += ` AND task_id=?`
		args = append(args, taskID)
	}
	q += ` ORDER BY id ASC LIMIT 500`
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var taskID, payload, created string
		if err := rows.Scan(&e.ID, &taskID, &e.ActorID, &e.EventType, &payload, &created); err != nil {
			return nil, err
		}
		e.TaskID = taskID
		_ = json.Unmarshal([]byte(payload), &e.Payload)
		e.CreatedAt = parseTS(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) addEvent(ctx context.Context, taskID, actorID, typ string, payload any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	b, _ := json.Marshal(payload)
	_, err := s.exec(ctx, `INSERT INTO events (task_id,actor_id,event_type,payload_json,created_at) VALUES (?,?,?,?,?)`,
		taskID, actorID, typ, string(b), ts(time.Now().UTC()))
	return err
}

func (s *Store) getLock(ctx context.Context, id string) (Lock, error) {
	row := s.queryRow(ctx, `SELECT id,task_id,owner_id,scope,scope_type,status,expires_at,last_heartbeat_at,created_at,released_at,released_by,release_reason,overridden_at,overridden_by,override_reason FROM locks WHERE id=?`, id)
	l, err := scanLock(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Lock{}, userErr("not_found", "lock not found")
	}
	return l, err
}

func (s *Store) getHandoff(ctx context.Context, id string) (Handoff, error) {
	row := s.queryRow(ctx, `SELECT id,task_id,from_actor_id,to_actor_id,status,resume_summary,next_steps_json,created_at,accepted_at FROM handoffs WHERE id=?`, id)
	h, err := scanHandoff(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Handoff{}, userErr("not_found", "handoff not found")
	}
	return h, err
}

func (s *Store) getContextSnapshot(ctx context.Context, id string) (ContextSnapshot, error) {
	row := s.queryRow(ctx, `SELECT id,task_id,author_id,source,snapshot_type,status_at_time,summary_json,markdown_cache,source_context_ids_json,created_at,updated_at FROM context_snapshots WHERE id=?`, id)
	snapshot, err := scanContextSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ContextSnapshot{}, userErr("not_found", "context snapshot not found")
	}
	return snapshot, err
}

func (s *Store) getHandoffPacket(ctx context.Context, id string) (HandoffPacket, error) {
	row := s.queryRow(ctx, `SELECT id,task_id,handoff_id,generated_by,status,version,packet_json,markdown_cache,source_snapshot_ids_json,source_context_ids_json,source,validation_errors_json,supporting_evidence_json,edited_by,created_at,updated_at FROM handoff_packets WHERE id=?`, id)
	packet, err := scanHandoffPacket(row)
	if errors.Is(err, sql.ErrNoRows) {
		return HandoffPacket{}, userErr("not_found", "handoff packet not found")
	}
	return packet, err
}

func (s *Store) countActiveLocks(ctx context.Context, taskID string) int {
	var n int
	_ = s.queryRow(ctx, `SELECT COUNT(*) FROM locks WHERE task_id=? AND released_at IS NULL AND status IN ('active','stale')`, taskID).Scan(&n)
	return n
}

func (s *Store) latestHandoffStatus(ctx context.Context, taskID string) string {
	var status string
	_ = s.queryRow(ctx, `SELECT status FROM handoffs WHERE task_id=? ORDER BY created_at DESC LIMIT 1`, taskID).Scan(&status)
	return status
}

func (s *Store) countLockConflicts(ctx context.Context, taskID string) int {
	locks, _ := s.ListLocks(ctx, taskID, true)
	all, _ := s.ListLocks(ctx, "", true)
	n := 0
	for _, a := range locks {
		for _, b := range all {
			if a.ID != b.ID && a.OwnerID != b.OwnerID && scopesOverlap(a.ScopeType, a.Scope, b.ScopeType, b.Scope) {
				n++
			}
		}
	}
	return n
}

func (s *Store) countSubtasks(ctx context.Context, taskID string) int {
	var n int
	_ = s.queryRow(ctx, `SELECT COUNT(*) FROM tasks WHERE parent_task_id=?`, taskID).Scan(&n)
	return n
}

func (s *Store) countIncompleteSubtasks(ctx context.Context, taskID string) int {
	var n int
	_ = s.queryRow(ctx, `SELECT COUNT(*) FROM tasks WHERE parent_task_id=? AND status NOT IN ('completed','cancelled')`, taskID).Scan(&n)
	return n
}

func (s *Store) countOpenDependencies(ctx context.Context, taskID string) int {
	var n int
	_ = s.queryRow(ctx, `SELECT COUNT(*) FROM task_dependencies d JOIN tasks t ON t.id=d.depends_on_id WHERE d.task_id=? AND t.status NOT IN ('completed','cancelled')`, taskID).Scan(&n)
	return n
}

func (s *Store) countDependents(ctx context.Context, taskID string) int {
	var n int
	_ = s.queryRow(ctx, `SELECT COUNT(*) FROM task_dependencies WHERE depends_on_id=?`, taskID).Scan(&n)
	return n
}

func (s *Store) taskSearchText(ctx context.Context, taskID string) string {
	parts := []string{}
	rows, err := s.query(ctx, `SELECT content FROM context_entries WHERE task_id=? ORDER BY created_at DESC LIMIT 20`, taskID)
	if err == nil {
		for rows.Next() {
			var content string
			if rows.Scan(&content) == nil {
				parts = append(parts, content)
			}
		}
		_ = rows.Close()
	}
	rows, err = s.query(ctx, `SELECT decision,reason,impact FROM decision_records WHERE task_id=? ORDER BY created_at DESC LIMIT 20`, taskID)
	if err == nil {
		for rows.Next() {
			var decision, reason, impact string
			if rows.Scan(&decision, &reason, &impact) == nil {
				parts = append(parts, decision, reason, impact)
			}
		}
		_ = rows.Close()
	}
	return strings.Join(parts, " ")
}

type scanner interface{ Scan(dest ...any) error }

func scanActor(row scanner) (Actor, error) {
	var a Actor
	var created, last sql.NullString
	if err := row.Scan(&a.ID, &a.Name, &a.Kind, &a.MachineName, &created, &last); err != nil {
		return Actor{}, err
	}
	a.CreatedAt = parseTS(created.String)
	if last.Valid {
		t := parseTS(last.String)
		a.LastSeenAt = &t
	}
	return a, nil
}

func scanTask(row scanner) (Task, error) {
	var t Task
	var repo, workspace, parent, owner, claim, heartbeat sql.NullString
	var created, updated, scope, req, criteria, risks, blockers string
	if err := row.Scan(&t.ID, &t.ProjectID, &repo, &workspace, &parent, &t.Title, &t.Goal, &t.Type, &t.Status, &t.Priority, &owner, &t.CreatedBy, &created, &updated, &claim, &heartbeat, &t.PrivacyLevel, &scope, &req, &criteria, &risks, &blockers); err != nil {
		return Task{}, err
	}
	if t.ProjectID == "" {
		t.ProjectID = "project_default"
	}
	t.RepoID = repo.String
	t.WorkspaceID = workspace.String
	t.ParentTaskID = parent.String
	t.OwnerID = owner.String
	t.CreatedAt = parseTS(created)
	t.UpdatedAt = parseTS(updated)
	if claim.Valid {
		v := parseTS(claim.String)
		t.ClaimExpiresAt = &v
	}
	if heartbeat.Valid {
		v := parseTS(heartbeat.String)
		t.LastHeartbeatAt = &v
	}
	fromJS(scope, &t.Scope)
	fromJS(req, &t.Requirements)
	fromJS(criteria, &t.CompletionCriteria)
	fromJS(risks, &t.Risks)
	fromJS(blockers, &t.Blockers)
	return t, nil
}

func scanLock(row scanner) (Lock, error) {
	var l Lock
	var exp, heartbeat, created string
	var released, releasedBy, releaseReason, overriddenAt, overriddenBy, overrideReason sql.NullString
	if err := row.Scan(&l.ID, &l.TaskID, &l.OwnerID, &l.Scope, &l.ScopeType, &l.Status, &exp, &heartbeat, &created, &released, &releasedBy, &releaseReason, &overriddenAt, &overriddenBy, &overrideReason); err != nil {
		return Lock{}, err
	}
	if l.Status == "" {
		l.Status = "active"
	}
	l.ExpiresAt = parseTS(exp)
	l.LastHeartbeatAt = parseTS(heartbeat)
	l.CreatedAt = parseTS(created)
	if released.Valid {
		v := parseTS(released.String)
		l.ReleasedAt = &v
	}
	l.ReleasedBy = releasedBy.String
	l.ReleaseReason = releaseReason.String
	if overriddenAt.Valid {
		v := parseTS(overriddenAt.String)
		l.OverriddenAt = &v
	}
	l.OverriddenBy = overriddenBy.String
	l.OverrideReason = overrideReason.String
	return l, nil
}

func scanConflict(row scanner) (Conflict, error) {
	var c Conflict
	var taskID, actorID, scope, scopeType, currentOwner, otherActor, otherTask, lockID, conflictingLock, resolution, note, resolvedAt, resolvedBy sql.NullString
	var created string
	if err := row.Scan(&c.ID, &taskID, &actorID, &c.ConflictType, &c.Status, &scope, &scopeType, &currentOwner, &otherActor, &otherTask, &lockID, &conflictingLock, &resolution, &note, &created, &resolvedAt, &resolvedBy); err != nil {
		return Conflict{}, err
	}
	c.TaskID = taskID.String
	c.ActorID = actorID.String
	c.Scope = scope.String
	c.ScopeType = scopeType.String
	c.CurrentOwnerID = currentOwner.String
	c.OtherActorID = otherActor.String
	c.OtherTaskID = otherTask.String
	c.LockID = lockID.String
	c.ConflictingLockID = conflictingLock.String
	c.Resolution = resolution.String
	c.ResolutionNote = note.String
	c.CreatedAt = parseTS(created)
	if resolvedAt.Valid {
		v := parseTS(resolvedAt.String)
		c.ResolvedAt = &v
	}
	c.ResolvedBy = resolvedBy.String
	return c, nil
}

func scanHandoff(row scanner) (Handoff, error) {
	var h Handoff
	var next, created string
	var to, accepted sql.NullString
	if err := row.Scan(&h.ID, &h.TaskID, &h.FromActorID, &to, &h.Status, &h.ResumeSummary, &next, &created, &accepted); err != nil {
		return Handoff{}, err
	}
	h.ToActorID = to.String
	fromJS(next, &h.NextSteps)
	h.CreatedAt = parseTS(created)
	if accepted.Valid {
		v := parseTS(accepted.String)
		h.AcceptedAt = &v
	}
	return h, nil
}

func scanContextSnapshot(row scanner) (ContextSnapshot, error) {
	var s ContextSnapshot
	var summary, sourceIDs, created, updated string
	if err := row.Scan(&s.ID, &s.TaskID, &s.AuthorID, &s.Source, &s.SnapshotType, &s.StatusAtTime, &summary, &s.Markdown, &sourceIDs, &created, &updated); err != nil {
		return ContextSnapshot{}, err
	}
	fromJS(summary, &s.Summary)
	fromJS(sourceIDs, &s.SourceContextIDs)
	s.CreatedAt = parseTS(created)
	s.UpdatedAt = parseTS(updated)
	return s, nil
}

func scanHandoffPacket(row scanner) (HandoffPacket, error) {
	var p HandoffPacket
	var handoffID, editedBy sql.NullString
	var packet, snapshotIDs, contextIDs, validationErrors, supportingEvidence, created, updated string
	if err := row.Scan(&p.ID, &p.TaskID, &handoffID, &p.GeneratedBy, &p.Status, &p.Version, &packet, &p.Markdown, &snapshotIDs, &contextIDs, &p.Source, &validationErrors, &supportingEvidence, &editedBy, &created, &updated); err != nil {
		return HandoffPacket{}, err
	}
	if p.Version == 0 {
		p.Version = 1
	}
	if p.Source == "" {
		p.Source = "generated_fallback"
	}
	p.HandoffID = handoffID.String
	p.EditedBy = editedBy.String
	fromJS(packet, &p.Packet)
	fromJS(snapshotIDs, &p.SourceSnapshotIDs)
	fromJS(contextIDs, &p.SourceContextIDs)
	fromJS(validationErrors, &p.ValidationErrors)
	fromJS(supportingEvidence, &p.SupportingEvidence)
	p.CreatedAt = parseTS(created)
	p.UpdatedAt = parseTS(updated)
	return p, nil
}

func cleanStrings(in []string) []string {
	out := []string{}
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func nullableString(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

func validateTaskEnums(typ, status, priority, privacy string) error {
	if !oneOf(typ, "planning", "research", "implementation", "review", "debugging", "documentation", "other") {
		return userErr("validation", "invalid task type")
	}
	if !oneOf(status, "ready", "claimed", "in_progress", "blocked", "handoff_ready", "in_review", "completed", "cancelled") {
		return userErr("validation", "invalid task status")
	}
	if !oneOf(priority, "low", "normal", "high", "urgent") {
		return userErr("validation", "invalid priority")
	}
	if !oneOf(privacy, "metadata_only", "sanitized_context") {
		return userErr("validation", "invalid privacy level")
	}
	return nil
}

func validateStatusTransition(from, to string) error {
	if from == to || from == "" {
		return nil
	}
	if !oneOf(to, "ready", "claimed", "in_progress", "blocked", "handoff_ready", "in_review", "completed", "cancelled") {
		return userErr("validation", "invalid task status")
	}
	if from == "completed" && to != "in_review" && to != "ready" {
		return userErr("validation", "completed tasks can only be reopened to ready or in_review")
	}
	if from == "cancelled" && to != "ready" {
		return userErr("validation", "cancelled tasks can only be reopened to ready")
	}
	return nil
}

func isActiveConflictTaskStatus(status string) bool {
	return status == "ready" || status == "claimed" || status == "in_progress"
}

func scopesOverlap(at, a, bt, b string) bool {
	if at == "semantic_area" || bt == "semantic_area" {
		return at == bt && strings.EqualFold(a, b)
	}
	aa := cleanScope(a)
	bb := cleanScope(b)
	return aa == bb || strings.HasPrefix(aa, bb) || strings.HasPrefix(bb, aa)
}

func cleanScope(v string) string {
	v = strings.TrimSpace(strings.TrimSuffix(v, "*"))
	v = strings.TrimSuffix(v, "/")
	return v
}

func js(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func fromJS(s string, out any) { _ = json.Unmarshal([]byte(s), out) }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func tsPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

type publicError struct{ code, msg string }

func (e publicError) Error() string  { return e.msg }
func userErr(code, msg string) error { return publicError{code: code, msg: msg} }
func errorCode(err error) string {
	var pe publicError
	if errors.As(err, &pe) {
		return pe.code
	}
	return "internal"
}

func oneOf(v string, opts ...string) bool {
	for _, opt := range opts {
		if v == opt {
			return true
		}
	}
	return false
}

func newID(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UTC().UnixNano())
}

func newSecret() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return newID("secret")
	}
	return hex.EncodeToString(b[:])
}

func secretHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
