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
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func OpenStore(path string) (*Store, error) {
	if path != "" {
		if err := ensureDir(filepath.Dir(path)); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(5)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`PRAGMA journal_mode=WAL`,
		`CREATE TABLE IF NOT EXISTS actors (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, kind TEXT NOT NULL, machine_name TEXT,
			created_at TEXT NOT NULL, last_seen_at TEXT, actor_secret_hash TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY, title TEXT NOT NULL, goal TEXT NOT NULL, type TEXT NOT NULL,
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
		`CREATE TABLE IF NOT EXISTS locks (
			id TEXT PRIMARY KEY, task_id TEXT NOT NULL, owner_id TEXT NOT NULL, scope TEXT NOT NULL,
			scope_type TEXT NOT NULL, expires_at TEXT NOT NULL, created_at TEXT NOT NULL, released_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS handoffs (
			id TEXT PRIMARY KEY, task_id TEXT NOT NULL, from_actor_id TEXT NOT NULL, to_actor_id TEXT,
			status TEXT NOT NULL, resume_summary TEXT NOT NULL, next_steps_json TEXT NOT NULL,
			created_at TEXT NOT NULL, accepted_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS events (
			id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT, actor_id TEXT NOT NULL,
			event_type TEXT NOT NULL, payload_json TEXT NOT NULL, created_at TEXT NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	_, _ = s.db.ExecContext(ctx, `ALTER TABLE actors ADD COLUMN actor_secret_hash TEXT`)
	return nil
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO actors (id,name,kind,machine_name,created_at,last_seen_at,actor_secret_hash) VALUES (?,?,?,?,?,?,?)`,
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
	err := s.db.QueryRowContext(ctx, `SELECT actor_secret_hash FROM actors WHERE id=?`, actorID).Scan(&hash)
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
	_, _ = s.db.ExecContext(ctx, `UPDATE actors SET last_seen_at=? WHERE id=?`, ts(now), actorID)
}

func (s *Store) ListActors(ctx context.Context) ([]Actor, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,kind,machine_name,created_at,last_seen_at FROM actors ORDER BY created_at DESC`)
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
	row := s.db.QueryRowContext(ctx, `SELECT id,name,kind,machine_name,created_at,last_seen_at FROM actors WHERE id=?`, id)
	a, err := scanActor(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

type TaskInput struct {
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
	if err := validateTaskEnums(in.Type, in.Status, in.Priority, in.PrivacyLevel); err != nil {
		return Task{}, err
	}
	now := time.Now().UTC()
	t := Task{
		ID: newID("task"), Title: in.Title, Goal: in.Goal, Type: in.Type, Status: in.Status,
		Priority: in.Priority, CreatedBy: actorID, CreatedAt: now, UpdatedAt: now,
		PrivacyLevel: in.PrivacyLevel, Scope: in.Scope, Requirements: in.Requirements,
		CompletionCriteria: in.CompletionCriteria, Risks: in.Risks, Blockers: in.Blockers,
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tasks
		(id,title,goal,type,status,priority,owner_id,created_by,created_at,updated_at,claim_expires_at,last_heartbeat_at,privacy_level,scope_json,requirements_json,completion_criteria_json,risks_json,blockers_json)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Title, t.Goal, t.Type, t.Status, t.Priority, t.OwnerID, t.CreatedBy, ts(t.CreatedAt), ts(t.UpdatedAt), nil, nil,
		t.PrivacyLevel, js(t.Scope), js(t.Requirements), js(t.CompletionCriteria), js(t.Risks), js(t.Blockers))
	if err != nil {
		return Task{}, err
	}
	return t, s.addEvent(ctx, t.ID, actorID, "task.created", t)
}

func (s *Store) ListTasks(ctx context.Context) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,title,goal,type,status,priority,owner_id,created_by,created_at,updated_at,claim_expires_at,last_heartbeat_at,privacy_level,scope_json,requirements_json,completion_criteria_json,risks_json,blockers_json FROM tasks ORDER BY updated_at DESC`)
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
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,title,goal,type,status,priority,owner_id,created_by,created_at,updated_at,claim_expires_at,last_heartbeat_at,privacy_level,scope_json,requirements_json,completion_criteria_json,risks_json,blockers_json FROM tasks WHERE id=?`, id)
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
	l, _ := s.ListLocks(ctx, id, false)
	h, _ := s.ListHandoffs(ctx, id)
	e, _ := s.ListEvents(ctx, 0, id)
	return TaskDetail{Task: t, Owner: owner, Context: c, Locks: l, Handoffs: h, Events: e}, nil
}

func (s *Store) UpdateTask(ctx context.Context, actorID, id string, in TaskInput, reason string) (Task, error) {
	t, err := s.GetTask(ctx, id)
	if err != nil {
		return Task{}, err
	}
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
		t.Status = in.Status
	}
	if in.Priority != "" {
		t.Priority = in.Priority
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
	_, err = s.db.ExecContext(ctx, `UPDATE tasks SET title=?,goal=?,type=?,status=?,priority=?,updated_at=?,privacy_level=?,scope_json=?,requirements_json=?,completion_criteria_json=?,risks_json=?,blockers_json=? WHERE id=?`,
		t.Title, t.Goal, t.Type, t.Status, t.Priority, ts(t.UpdatedAt), t.PrivacyLevel, js(t.Scope), js(t.Requirements), js(t.CompletionCriteria), js(t.Risks), js(t.Blockers), t.ID)
	if err != nil {
		return Task{}, err
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
		_ = s.addEvent(ctx, t.ID, actorID, "conflict.detected", map[string]any{"reason": "active_owner", "owner_id": t.OwnerID})
		return Task{}, userErr("conflict", "task is actively owned; force reassignment requires reason")
	}
	if force && t.OwnerID != "" && t.OwnerID != actorID && strings.TrimSpace(reason) == "" {
		return Task{}, userErr("validation", "reason is required to force reassign an active task")
	}
	exp := now.Add(DefaultClaimTTL)
	t.OwnerID = actorID
	t.Status = "claimed"
	t.UpdatedAt = now
	t.LastHeartbeatAt = &now
	t.ClaimExpiresAt = &exp
	_, err = s.db.ExecContext(ctx, `UPDATE tasks SET owner_id=?,status=?,updated_at=?,last_heartbeat_at=?,claim_expires_at=? WHERE id=?`,
		t.OwnerID, t.Status, ts(t.UpdatedAt), tsPtr(t.LastHeartbeatAt), tsPtr(t.ClaimExpiresAt), t.ID)
	if err != nil {
		return Task{}, err
	}
	etype := "task.claimed"
	if force {
		etype = "task.reassigned"
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
	_, err = s.db.ExecContext(ctx, `UPDATE tasks SET owner_id='',status=?,updated_at=?,last_heartbeat_at=NULL,claim_expires_at=NULL WHERE id=?`, t.Status, ts(t.UpdatedAt), t.ID)
	if err != nil {
		return Task{}, err
	}
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
	_, err = s.db.ExecContext(ctx, `UPDATE tasks SET last_heartbeat_at=?,claim_expires_at=?,updated_at=? WHERE id=?`, ts(now), ts(exp), ts(now), t.ID)
	if err != nil {
		return Task{}, err
	}
	return t, s.addEvent(ctx, t.ID, actorID, "task.heartbeat", map[string]any{"claim_expires_at": exp})
}

func (s *Store) CompleteTask(ctx context.Context, actorID, id, summary string) (Task, error) {
	if summary != "" {
		if _, err := s.AppendContext(ctx, actorID, id, "summary", summary); err != nil {
			return Task{}, err
		}
	}
	return s.UpdateTask(ctx, actorID, id, TaskInput{Status: "completed"}, "completed")
}

func (s *Store) AppendContext(ctx context.Context, actorID, taskID, kind, content string) (ContextEntry, error) {
	if content == "" {
		return ContextEntry{}, userErr("validation", "context content is required")
	}
	if kind == "" {
		kind = "note"
	}
	if !oneOf(kind, "summary", "decision", "note", "risk", "blocker", "output_ref") {
		return ContextEntry{}, userErr("validation", "invalid context kind")
	}
	if _, err := s.GetTask(ctx, taskID); err != nil {
		return ContextEntry{}, err
	}
	now := time.Now().UTC()
	c := ContextEntry{ID: newID("ctx"), TaskID: taskID, AuthorID: actorID, Kind: kind, Content: content, CreatedAt: now}
	_, err := s.db.ExecContext(ctx, `INSERT INTO context_entries (id,task_id,author_id,kind,content,created_at) VALUES (?,?,?,?,?,?)`,
		c.ID, c.TaskID, c.AuthorID, c.Kind, c.Content, ts(c.CreatedAt))
	if err != nil {
		return ContextEntry{}, err
	}
	return c, s.addEvent(ctx, taskID, actorID, "context.appended", c)
}

func (s *Store) ListContext(ctx context.Context, taskID string) ([]ContextEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,task_id,author_id,kind,content,created_at FROM context_entries WHERE task_id=? ORDER BY created_at ASC`, taskID)
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
	conflicts, err := s.FindLockConflicts(ctx, actorID, scope, scopeType)
	if err != nil {
		return Lock{}, nil, err
	}
	if len(conflicts) > 0 {
		_ = s.addEvent(ctx, taskID, actorID, "conflict.detected", map[string]any{"scope": scope, "conflicts": conflicts})
		return Lock{}, conflicts, userErr("conflict", "active overlapping lock exists")
	}
	now := time.Now().UTC()
	l := Lock{ID: newID("lock"), TaskID: taskID, OwnerID: actorID, Scope: scope, ScopeType: scopeType, ExpiresAt: now.Add(DefaultLockTTL), CreatedAt: now}
	_, err = s.db.ExecContext(ctx, `INSERT INTO locks (id,task_id,owner_id,scope,scope_type,expires_at,created_at,released_at) VALUES (?,?,?,?,?,?,?,NULL)`,
		l.ID, l.TaskID, l.OwnerID, l.Scope, l.ScopeType, ts(l.ExpiresAt), ts(l.CreatedAt))
	if err != nil {
		return Lock{}, nil, err
	}
	return l, nil, s.addEvent(ctx, taskID, actorID, "lock.acquired", l)
}

func (s *Store) ListLocks(ctx context.Context, taskID string, activeOnly bool) ([]Lock, error) {
	q := `SELECT id,task_id,owner_id,scope,scope_type,expires_at,created_at,released_at FROM locks`
	args := []any{}
	clauses := []string{}
	if taskID != "" {
		clauses = append(clauses, "task_id=?")
		args = append(args, taskID)
	}
	if activeOnly {
		clauses = append(clauses, "released_at IS NULL AND expires_at>?")
		args = append(args, ts(time.Now().UTC()))
	}
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	q += " ORDER BY created_at DESC"
	rows, err := s.db.QueryContext(ctx, q, args...)
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
			conflicts = append(conflicts, l)
		}
	}
	return conflicts, nil
}

func (s *Store) ReleaseLock(ctx context.Context, actorID, lockID string) (Lock, error) {
	l, err := s.getLock(ctx, lockID)
	if err != nil {
		return Lock{}, err
	}
	if l.OwnerID != actorID {
		return Lock{}, userErr("forbidden", "only the lock owner can release this lock")
	}
	now := time.Now().UTC()
	l.ReleasedAt = &now
	_, err = s.db.ExecContext(ctx, `UPDATE locks SET released_at=? WHERE id=?`, ts(now), lockID)
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
	l.ExpiresAt = time.Now().UTC().Add(DefaultLockTTL)
	_, err = s.db.ExecContext(ctx, `UPDATE locks SET expires_at=? WHERE id=?`, ts(l.ExpiresAt), lockID)
	if err != nil {
		return Lock{}, err
	}
	return l, s.addEvent(ctx, l.TaskID, actorID, "lock.renewed", l)
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO handoffs (id,task_id,from_actor_id,to_actor_id,status,resume_summary,next_steps_json,created_at,accepted_at) VALUES (?,?,?,?,?,?,?,?,NULL)`,
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
	_, err = s.db.ExecContext(ctx, `UPDATE handoffs SET status=?,accepted_at=? WHERE id=?`, h.Status, ts(now), h.ID)
	if err != nil {
		return Handoff{}, err
	}
	if _, err := s.ClaimTask(ctx, actorID, h.TaskID, "handoff accepted", true); err != nil {
		return Handoff{}, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE locks SET owner_id=? WHERE task_id=? AND owner_id=? AND released_at IS NULL AND expires_at>?`,
		actorID, h.TaskID, h.FromActorID, ts(time.Now().UTC())); err != nil {
		return Handoff{}, err
	}
	_ = s.addEvent(ctx, h.TaskID, actorID, "lock.transferred", map[string]any{"from_actor_id": h.FromActorID, "to_actor_id": actorID})
	return h, s.addEvent(ctx, h.TaskID, actorID, "handoff.accepted", h)
}

func (s *Store) RejectHandoff(ctx context.Context, actorID, handoffID string) (Handoff, error) {
	h, err := s.getHandoff(ctx, handoffID)
	if err != nil {
		return Handoff{}, err
	}
	h.Status = "rejected"
	_, err = s.db.ExecContext(ctx, `UPDATE handoffs SET status=? WHERE id=?`, h.Status, h.ID)
	if err != nil {
		return Handoff{}, err
	}
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
	rows, err := s.db.QueryContext(ctx, q, args...)
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
	rows, err := s.db.QueryContext(ctx, q, args...)
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO events (task_id,actor_id,event_type,payload_json,created_at) VALUES (?,?,?,?,?)`,
		taskID, actorID, typ, string(b), ts(time.Now().UTC()))
	return err
}

func (s *Store) getLock(ctx context.Context, id string) (Lock, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,task_id,owner_id,scope,scope_type,expires_at,created_at,released_at FROM locks WHERE id=?`, id)
	l, err := scanLock(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Lock{}, userErr("not_found", "lock not found")
	}
	return l, err
}

func (s *Store) getHandoff(ctx context.Context, id string) (Handoff, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,task_id,from_actor_id,to_actor_id,status,resume_summary,next_steps_json,created_at,accepted_at FROM handoffs WHERE id=?`, id)
	h, err := scanHandoff(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Handoff{}, userErr("not_found", "handoff not found")
	}
	return h, err
}

func (s *Store) countActiveLocks(ctx context.Context, taskID string) int {
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM locks WHERE task_id=? AND released_at IS NULL AND expires_at>?`, taskID, ts(time.Now().UTC())).Scan(&n)
	return n
}

func (s *Store) latestHandoffStatus(ctx context.Context, taskID string) string {
	var status string
	_ = s.db.QueryRowContext(ctx, `SELECT status FROM handoffs WHERE task_id=? ORDER BY created_at DESC LIMIT 1`, taskID).Scan(&status)
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
	var owner, claim, heartbeat sql.NullString
	var created, updated, scope, req, criteria, risks, blockers string
	if err := row.Scan(&t.ID, &t.Title, &t.Goal, &t.Type, &t.Status, &t.Priority, &owner, &t.CreatedBy, &created, &updated, &claim, &heartbeat, &t.PrivacyLevel, &scope, &req, &criteria, &risks, &blockers); err != nil {
		return Task{}, err
	}
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
	var exp, created string
	var released sql.NullString
	if err := row.Scan(&l.ID, &l.TaskID, &l.OwnerID, &l.Scope, &l.ScopeType, &exp, &created, &released); err != nil {
		return Lock{}, err
	}
	l.ExpiresAt = parseTS(exp)
	l.CreatedAt = parseTS(created)
	if released.Valid {
		v := parseTS(released.String)
		l.ReleasedAt = &v
	}
	return l, nil
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
