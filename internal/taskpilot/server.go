package taskpilot

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed static/*
var staticFS embed.FS

type Server struct {
	store   *Store
	token   string
	mux     *http.ServeMux
	metrics serverMetrics
}

func NewServer(store *Store, token string) *Server {
	if token == "" {
		token = "dev-token"
	}
	s := &Server{store: store, token: token, mux: http.NewServeMux(), metrics: serverMetrics{Started: time.Now().UTC().Format(time.RFC3339)}}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.middleware(s.mux).ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.Ping(r.Context()); err != nil {
			writeErr(w, http.StatusServiceUnavailable, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	s.mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		stats, _ := s.store.Stats(r.Context())
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(metricsText(serverMetrics{
			Requests: atomic.LoadUint64(&s.metrics.Requests),
			Errors:   atomic.LoadUint64(&s.metrics.Errors),
			Started:  s.metrics.Started,
		}, stats)))
	})
	s.mux.Handle("POST /api/auth/login", http.HandlerFunc(s.handleLogin))
	s.mux.Handle("POST /api/auth/logout", s.auth(http.HandlerFunc(s.handleLogout)))
	s.mux.Handle("GET /api/me", s.auth(http.HandlerFunc(s.handleMe)))
	s.mux.Handle("POST /api/me/password", s.requireScope("task:read", s.handleChangeOwnPassword))
	s.mux.Handle("GET /api/users", s.requireScope("admin", s.handleUsers))
	s.mux.Handle("POST /api/users", s.requireScope("admin", s.handleCreateUser))
	s.mux.Handle("PATCH /api/users/{id}", s.requireScope("admin", s.handleUpdateUser))
	s.mux.Handle("POST /api/users/{id}/password", s.requireScope("admin", s.handleResetUserPassword))
	s.mux.Handle("GET /api/api-keys", s.requireScope("admin", s.handleAPIKeys))
	s.mux.Handle("POST /api/api-keys", s.requireScope("admin", s.handleCreateAPIKey))
	s.mux.Handle("DELETE /api/api-keys/{id}", s.requireScope("admin", s.handleRevokeAPIKey))
	s.mux.Handle("GET /api/projects", s.requireScope("task:read", s.handleProjects))
	s.mux.Handle("POST /api/projects", s.requireScope("task:write", s.handleCreateProject))
	s.mux.Handle("GET /api/repositories", s.requireScope("task:read", s.handleRepositories))
	s.mux.Handle("POST /api/repositories", s.requireScope("task:write", s.handleCreateRepository))
	s.mux.Handle("GET /api/workspaces", s.requireScope("task:read", s.handleWorkspaces))
	s.mux.Handle("POST /api/workspaces", s.requireScope("task:write", s.handleCreateWorkspace))
	s.mux.Handle("POST /api/actors/register", s.auth(http.HandlerFunc(s.handleRegisterActor)))
	s.mux.Handle("GET /api/actors", s.requireScope("task:read", s.handleActors))
	s.mux.Handle("POST /api/tasks", s.requireScope("task:write", s.handleCreateTask))
	s.mux.Handle("GET /api/tasks", s.requireScope("task:read", s.handleTasks))
	s.mux.Handle("GET /api/tasks/{id}", s.requireScope("task:read", s.handleTaskDetail))
	s.mux.Handle("PATCH /api/tasks/{id}", s.requireScope("task:write", s.handleUpdateTask))
	s.mux.Handle("POST /api/tasks/{id}/subtasks", s.requireScope("task:write", s.handleCreateSubtask))
	s.mux.Handle("POST /api/tasks/{id}/dependencies", s.requireScope("task:write", s.handleAddDependency))
	s.mux.Handle("POST /api/tasks/{id}/claim", s.requireScope("task:write", s.handleClaimTask))
	s.mux.Handle("POST /api/tasks/{id}/release", s.requireScope("task:write", s.handleReleaseTask))
	s.mux.Handle("POST /api/tasks/{id}/heartbeat", s.requireScope("task:write", s.handleHeartbeatTask))
	s.mux.Handle("POST /api/tasks/{id}/sessions/start", s.requireScope("task:write", s.handleStartTaskSession))
	s.mux.Handle("POST /api/tasks/{id}/sessions/finish", s.requireScope("task:write", s.handleFinishTaskSession))
	s.mux.Handle("POST /api/tasks/{id}/complete", s.requireScope("task:write", s.handleCompleteTask))
	s.mux.Handle("POST /api/tasks/{id}/context", s.requireScope("context:write", s.handleAppendContext))
	s.mux.Handle("GET /api/tasks/{id}/context", s.requireScope("task:read", s.handleContext))
	s.mux.Handle("POST /api/tasks/{id}/snapshots", s.requireScope("context:write", s.handleCreateSnapshot))
	s.mux.Handle("GET /api/tasks/{id}/snapshots", s.requireScope("task:read", s.handleSnapshots))
	s.mux.Handle("PATCH /api/snapshots/{id}", s.requireScope("context:write", s.handleUpdateSnapshot))
	s.mux.Handle("POST /api/tasks/{id}/handoff-packet/generate", s.requireScope("handoff:write", s.handleGenerateHandoffPacket))
	s.mux.Handle("GET /api/tasks/{id}/handoff-packet", s.requireScope("task:read", s.handleLatestHandoffPacket))
	s.mux.Handle("PATCH /api/handoff-packets/{id}", s.requireScope("handoff:write", s.handleUpdateHandoffPacket))
	s.mux.Handle("POST /api/handoff-packets/{id}/publish", s.requireScope("handoff:write", s.handlePublishHandoffPacket))
	s.mux.Handle("POST /api/tasks/{id}/decisions", s.requireScope("context:write", s.handleAddDecision))
	s.mux.Handle("GET /api/tasks/{id}/decisions", s.requireScope("task:read", s.handleDecisions))
	s.mux.Handle("POST /api/tasks/{id}/comments", s.requireScope("context:write", s.handleAddComment))
	s.mux.Handle("GET /api/tasks/{id}/comments", s.requireScope("task:read", s.handleComments))
	s.mux.Handle("POST /api/tasks/{id}/artifacts", s.requireScope("context:write", s.handleAddArtifact))
	s.mux.Handle("GET /api/tasks/{id}/artifacts", s.requireScope("task:read", s.handleArtifacts))
	s.mux.Handle("POST /api/tasks/{id}/git", s.requireScope("context:write", s.handleAddGitRef))
	s.mux.Handle("GET /api/tasks/{id}/git", s.requireScope("task:read", s.handleGitRefs))
	s.mux.Handle("POST /api/tasks/{id}/locks", s.requireScope("lock:write", s.handleAcquireLock))
	s.mux.Handle("GET /api/tasks/{id}/locks", s.requireScope("task:read", s.handleLocks))
	s.mux.Handle("POST /api/locks/{id}/release", s.requireScope("lock:write", s.handleReleaseLock))
	s.mux.Handle("POST /api/locks/{id}/renew", s.requireScope("lock:write", s.handleRenewLock))
	s.mux.Handle("POST /api/locks/{id}/override", s.requireScope("lock:write", s.handleOverrideLock))
	s.mux.Handle("POST /api/tasks/{id}/handoff", s.requireScope("handoff:write", s.handlePrepareHandoff))
	s.mux.Handle("POST /api/handoffs/{id}/accept", s.requireScope("handoff:write", s.handleAcceptHandoff))
	s.mux.Handle("POST /api/handoffs/{id}/reject", s.requireScope("handoff:write", s.handleRejectHandoff))
	s.mux.Handle("DELETE /api/dependencies/{id}", s.requireScope("task:write", s.handleRemoveDependency))
	s.mux.Handle("GET /api/conflicts", s.requireScope("task:read", s.handleConflicts))
	s.mux.Handle("GET /api/conflicts/stale-claims", s.requireScope("task:read", s.handleStaleClaims))
	s.mux.Handle("POST /api/conflicts/{id}/resolve", s.requireScope("task:write", s.handleResolveConflict))
	s.mux.Handle("GET /api/handoffs", s.requireScope("task:read", s.handleHandoffs))
	s.mux.Handle("GET /api/events", s.requireScope("task:read", s.handleEvents))
	s.mux.Handle("GET /api/events/stream", s.requireScope("task:read", s.handleEventsStream))
	s.mux.Handle("/", s.dashboard())
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := s.authPrincipal(r); ok {
			ctx := context.WithValue(r.Context(), actorKey{}, p.ActorID)
			ctx = context.WithValue(ctx, principalKey{}, p)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got != s.token {
			writeErr(w, http.StatusUnauthorized, userErr("unauthorized", "invalid or missing team token"))
			return
		}
		actorID := r.Header.Get("X-Actor-ID")
		if actorID == "" && r.URL.Path != "/api/actors/register" {
			writeErr(w, http.StatusUnauthorized, userErr("unauthorized", "missing X-Actor-ID"))
			return
		}
		if actorID != "" {
			ok, err := s.store.VerifyActorSecret(r.Context(), actorID, r.Header.Get("X-Actor-Secret"))
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			if !ok {
				writeErr(w, http.StatusUnauthorized, userErr("unauthorized", "invalid actor credentials"))
				return
			}
			s.store.TouchActor(r.Context(), actorID)
		}
		p := Principal{ID: actorID, Kind: "legacy_actor", Role: "agent", ActorID: actorID, Scopes: []string{"admin"}}
		ctx := context.WithValue(r.Context(), actorKey{}, actorID)
		ctx = context.WithValue(ctx, principalKey{}, p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type actorKey struct{}
type principalKey struct{}

func actorID(r *http.Request) string {
	v, _ := r.Context().Value(actorKey{}).(string)
	return v
}

func principal(r *http.Request) Principal {
	v, _ := r.Context().Value(principalKey{}).(Principal)
	return v
}

func (s *Server) authPrincipal(r *http.Request) (Principal, bool) {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "ApiKey ") {
		p, err := s.store.VerifyAPIKey(r.Context(), strings.TrimPrefix(auth, "ApiKey "))
		return p, err == nil
	}
	if cookie, err := r.Cookie("taskpilot_session"); err == nil {
		p, err := s.store.VerifySession(r.Context(), cookie.Value)
		return p, err == nil
	}
	return Principal{}, false
}

func (s *Server) requireScope(required string, next http.HandlerFunc) http.Handler {
	return s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principal(r)
		if p.Kind == "api_key" {
			need := "read"
			if required == "admin" {
				need = "admin"
			} else if required != "task:read" {
				need = "write"
			}
			if !hasScope(p.Scopes, required) || !roleAllows(p.Role, need) {
				writeErr(w, http.StatusForbidden, userErr("forbidden", "api key role or scope does not allow this action"))
				return
			}
		}
		if p.Kind == "user" {
			need := "read"
			if required == "admin" {
				need = "admin"
			} else if required != "task:read" {
				need = "write"
			}
			if !roleAllows(p.Role, need) {
				writeErr(w, http.StatusForbidden, userErr("forbidden", "role does not allow this action"))
				return
			}
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *Server) dashboard() http.Handler {
	sub, _ := fs.Sub(staticFS, "static")
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	u, err := s.store.AuthenticateUser(r.Context(), in.Email, in.Password)
	if err != nil {
		writeResult(w, nil, err)
		return
	}
	token, err := s.store.CreateSession(r.Context(), u.ID)
	if err != nil {
		writeResult(w, nil, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "taskpilot_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
	})
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("taskpilot_session"); err == nil {
		_ = s.store.RevokeSession(r.Context(), cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "taskpilot_session", Value: "", Path: "/", HttpOnly: true, MaxAge: -1, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, principal(r))
}

func (s *Server) handleChangeOwnPassword(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p.Kind != "user" || p.UserID == "" {
		writeErr(w, http.StatusForbidden, userErr("forbidden", "only human sessions can change passwords"))
		return
	}
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decode(w, r, &in) {
		return
	}
	err := s.store.ChangeUserPassword(r.Context(), actorID(r), p.UserID, in.CurrentPassword, in.NewPassword, true)
	writeResult(w, map[string]string{"status": "ok"}, err)
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ListUsers(r.Context())
	writeResult(w, out, err)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !decode(w, r, &in) {
		return
	}
	u, err := s.store.CreateUser(r.Context(), in.Email, in.Name, in.Password, in.Role)
	if err == nil {
		err = s.store.addEvent(r.Context(), "", actorID(r), "user.invited", map[string]any{"id": u.ID, "email": u.Email, "role": u.Role})
	}
	writeResult(w, u, err)
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name   string `json:"name"`
		Role   string `json:"role"`
		Active *bool  `json:"active"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.store.UpdateUser(r.Context(), actorID(r), r.PathValue("id"), in.Name, in.Role, in.Active)
	writeResult(w, out, err)
}

func (s *Server) handleResetUserPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		NewPassword string `json:"new_password"`
	}
	if !decode(w, r, &in) {
		return
	}
	err := s.store.ChangeUserPassword(r.Context(), actorID(r), r.PathValue("id"), "", in.NewPassword, false)
	writeResult(w, map[string]string{"status": "ok"}, err)
}

func (s *Server) handleAPIKeys(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ListAPIKeys(r.Context())
	writeResult(w, out, err)
}

func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    string   `json:"name"`
		ActorID string   `json:"actor_id"`
		Role    string   `json:"role"`
		Scopes  []string `json:"scopes"`
	}
	if !decode(w, r, &in) {
		return
	}
	key, err := s.store.CreateAPIKey(r.Context(), in.Name, in.ActorID, in.Role, in.Scopes, actorID(r))
	writeResult(w, key, err)
}

func (s *Server) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	err := s.store.RevokeAPIKey(r.Context(), actorID(r), r.PathValue("id"))
	writeResult(w, map[string]string{"status": "ok"}, err)
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ListProjects(r.Context())
	writeResult(w, out, err)
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.store.CreateProject(r.Context(), actorID(r), in.Name, in.Description)
	writeResult(w, out, err)
}

func (s *Server) handleRepositories(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ListRepositories(r.Context(), r.URL.Query().Get("project_id"))
	writeResult(w, out, err)
}

func (s *Server) handleCreateRepository(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProjectID     string `json:"project_id"`
		Name          string `json:"name"`
		Path          string `json:"path"`
		DefaultBranch string `json:"default_branch"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.store.CreateRepository(r.Context(), actorID(r), in.ProjectID, in.Name, in.Path, in.DefaultBranch)
	writeResult(w, out, err)
}

func (s *Server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ListWorkspaces(r.Context(), r.URL.Query().Get("project_id"))
	writeResult(w, out, err)
}

func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProjectID   string `json:"project_id"`
		ActorID     string `json:"actor_id"`
		Name        string `json:"name"`
		MachineName string `json:"machine_name"`
		Kind        string `json:"kind"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.store.CreateWorkspace(r.Context(), actorID(r), in.ProjectID, in.ActorID, in.Name, in.MachineName, in.Kind)
	writeResult(w, out, err)
}

func (s *Server) handleRegisterActor(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		Kind        string `json:"kind"`
		MachineName string `json:"machine_name"`
	}
	if !decode(w, r, &in) {
		return
	}
	a, err := s.store.RegisterActor(r.Context(), in.Name, in.Kind, in.MachineName)
	writeResult(w, a, err)
}

func (s *Server) handleActors(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ListActors(r.Context())
	writeResult(w, out, err)
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var in TaskInput
	if !decode(w, r, &in) {
		return
	}
	t, err := s.store.CreateTask(r.Context(), actorID(r), in)
	writeResult(w, t, err)
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ListTasks(r.Context(), r.URL.Query().Get("project_id"))
	writeResult(w, out, err)
}

func (s *Server) handleTaskDetail(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.TaskDetail(r.Context(), r.PathValue("id"))
	writeResult(w, out, err)
}

func (s *Server) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TaskInput
		Reason string `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.store.UpdateTask(r.Context(), actorID(r), r.PathValue("id"), in.TaskInput, in.Reason)
	writeResult(w, out, err)
}

func (s *Server) handleCreateSubtask(w http.ResponseWriter, r *http.Request) {
	parent, err := s.store.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeResult(w, nil, err)
		return
	}
	var in TaskInput
	if !decode(w, r, &in) {
		return
	}
	in.ParentTaskID = parent.ID
	if in.ProjectID == "" {
		in.ProjectID = parent.ProjectID
	}
	if in.RepoID == "" {
		in.RepoID = parent.RepoID
	}
	if in.WorkspaceID == "" {
		in.WorkspaceID = parent.WorkspaceID
	}
	out, err := s.store.CreateTask(r.Context(), actorID(r), in)
	writeResult(w, out, err)
}

func (s *Server) handleAddDependency(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DependsOnID string `json:"depends_on_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.store.AddTaskDependency(r.Context(), actorID(r), r.PathValue("id"), in.DependsOnID)
	writeResult(w, out, err)
}

func (s *Server) handleRemoveDependency(w http.ResponseWriter, r *http.Request) {
	err := s.store.RemoveTaskDependency(r.Context(), actorID(r), r.PathValue("id"))
	writeResult(w, map[string]string{"status": "ok"}, err)
}

func (s *Server) handleClaimTask(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
		Force  bool   `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	out, err := s.store.ClaimTask(r.Context(), actorID(r), r.PathValue("id"), in.Reason, in.Force)
	writeResult(w, out, err)
}

func (s *Server) handleReleaseTask(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ReleaseTask(r.Context(), actorID(r), r.PathValue("id"))
	writeResult(w, out, err)
}

func (s *Server) handleHeartbeatTask(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.HeartbeatTask(r.Context(), actorID(r), r.PathValue("id"))
	writeResult(w, out, err)
}

func (s *Server) handleStartTaskSession(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.StartTaskSession(r.Context(), actorID(r), r.PathValue("id"))
	writeResult(w, out, err)
}

func (s *Server) handleFinishTaskSession(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SessionID    string `json:"session_id"`
		ExitStatus   string `json:"exit_status"`
		FinishReason string `json:"finish_reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.store.FinishTaskSession(r.Context(), actorID(r), r.PathValue("id"), in.SessionID, in.ExitStatus, in.FinishReason)
	writeResult(w, out, err)
}

func (s *Server) handleCompleteTask(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Summary string `json:"summary"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	out, err := s.store.CompleteTask(r.Context(), actorID(r), r.PathValue("id"), in.Summary)
	writeResult(w, out, err)
}

func (s *Server) handleAppendContext(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind    string `json:"kind"`
		Content string `json:"content"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.store.AppendContext(r.Context(), actorID(r), r.PathValue("id"), in.Kind, in.Content)
	writeResult(w, out, err)
}

func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ListContext(r.Context(), r.PathValue("id"))
	writeResult(w, out, err)
}

func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SnapshotType string `json:"snapshot_type"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.store.CreateContextSnapshot(r.Context(), actorID(r), r.PathValue("id"), in.SnapshotType)
	writeResult(w, out, err)
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ListContextSnapshots(r.Context(), r.PathValue("id"))
	writeResult(w, out, err)
}

func (s *Server) handleUpdateSnapshot(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Markdown string `json:"markdown"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.store.UpdateContextSnapshotMarkdown(r.Context(), actorID(r), r.PathValue("id"), in.Markdown)
	writeResult(w, out, err)
}

func (s *Server) handleGenerateHandoffPacket(w http.ResponseWriter, r *http.Request) {
	var in struct {
		HandoffID string `json:"handoff_id"`
		Status    string `json:"status"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.store.GenerateHandoffPacket(r.Context(), actorID(r), r.PathValue("id"), in.HandoffID, in.Status)
	writeResult(w, out, err)
}

func (s *Server) handleLatestHandoffPacket(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.LatestHandoffPacket(r.Context(), r.PathValue("id"))
	writeResult(w, out, err)
}

func (s *Server) handleUpdateHandoffPacket(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Markdown string `json:"markdown"`
		Source   string `json:"source"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Source == "" {
		in.Source = "manually_edited"
	}
	out, err := s.store.UpdateHandoffPacketMarkdownWithSource(r.Context(), actorID(r), r.PathValue("id"), in.Markdown, in.Source)
	writeResult(w, out, err)
}

func (s *Server) handlePublishHandoffPacket(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.PublishHandoffPacket(r.Context(), actorID(r), r.PathValue("id"))
	writeResult(w, out, err)
}

func (s *Server) handleAddDecision(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Decision     string   `json:"decision"`
		Alternatives []string `json:"alternatives"`
		Reason       string   `json:"reason"`
		Impact       string   `json:"impact"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.store.AddDecision(r.Context(), actorID(r), r.PathValue("id"), in.Decision, in.Alternatives, in.Reason, in.Impact)
	writeResult(w, out, err)
}

func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ListDecisions(r.Context(), r.PathValue("id"))
	writeResult(w, out, err)
}

func (s *Server) handleAddComment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Body string `json:"body"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.store.AddComment(r.Context(), actorID(r), r.PathValue("id"), in.Body)
	writeResult(w, out, err)
}

func (s *Server) handleComments(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ListComments(r.Context(), r.PathValue("id"))
	writeResult(w, out, err)
}

func (s *Server) handleAddArtifact(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind        string         `json:"kind"`
		Title       string         `json:"title"`
		URI         string         `json:"uri"`
		Description string         `json:"description"`
		Metadata    map[string]any `json:"metadata"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.store.AddArtifact(r.Context(), actorID(r), r.PathValue("id"), in.Kind, in.Title, in.URI, in.Description, in.Metadata)
	writeResult(w, out, err)
}

func (s *Server) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ListArtifacts(r.Context(), r.PathValue("id"))
	writeResult(w, out, err)
}

func (s *Server) handleAddGitRef(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Branch       string   `json:"branch"`
		CommitSHA    string   `json:"commit_sha"`
		PRURL        string   `json:"pr_url"`
		ChangedFiles []string `json:"changed_files"`
		Note         string   `json:"note"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.store.AddGitRef(r.Context(), actorID(r), r.PathValue("id"), in.Branch, in.CommitSHA, in.PRURL, in.ChangedFiles, in.Note)
	writeResult(w, out, err)
}

func (s *Server) handleGitRefs(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ListGitRefs(r.Context(), r.PathValue("id"))
	writeResult(w, out, err)
}

func (s *Server) handleAcquireLock(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Scope     string `json:"scope"`
		ScopeType string `json:"scope_type"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, conflicts, err := s.store.AcquireLock(r.Context(), actorID(r), r.PathValue("id"), in.Scope, in.ScopeType)
	if err != nil && len(conflicts) > 0 {
		msg := err.Error()
		if conflicts[0].Message != "" {
			msg = conflicts[0].Message
		}
		writeJSON(w, http.StatusConflict, map[string]any{"error": "conflict", "message": msg, "conflicts": conflicts})
		return
	}
	writeResult(w, out, err)
}

func (s *Server) handleLocks(w http.ResponseWriter, r *http.Request) {
	active := r.URL.Query().Get("active") == "true"
	out, err := s.store.ListLocks(r.Context(), r.PathValue("id"), active)
	writeResult(w, out, err)
}

func (s *Server) handleReleaseLock(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	out, err := s.store.ReleaseLockWithReason(r.Context(), actorID(r), r.PathValue("id"), in.Reason)
	writeResult(w, out, err)
}

func (s *Server) handleOverrideLock(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.store.OverrideLock(r.Context(), actorID(r), r.PathValue("id"), in.Reason)
	writeResult(w, out, err)
}

func (s *Server) handleConflicts(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	out, err := s.store.ListConflicts(r.Context(), status)
	writeResult(w, out, err)
}

func (s *Server) handleStaleClaims(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ListStaleClaims(r.Context())
	writeResult(w, out, err)
}

func (s *Server) handleResolveConflict(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Resolution    string `json:"resolution"`
		Note          string `json:"note"`
		TargetActorID string `json:"target_actor_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.store.ResolveConflict(r.Context(), actorID(r), r.PathValue("id"), in.Resolution, in.Note, in.TargetActorID)
	writeResult(w, out, err)
}

func (s *Server) handleRenewLock(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.RenewLock(r.Context(), actorID(r), r.PathValue("id"))
	writeResult(w, out, err)
}

func (s *Server) handlePrepareHandoff(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ToActorID string   `json:"to_actor_id"`
		Summary   string   `json:"summary"`
		NextSteps []string `json:"next_steps"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.store.PrepareHandoff(r.Context(), actorID(r), r.PathValue("id"), in.ToActorID, in.Summary, in.NextSteps)
	writeResult(w, out, err)
}

func (s *Server) handleAcceptHandoff(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.AcceptHandoff(r.Context(), actorID(r), r.PathValue("id"))
	writeResult(w, out, err)
}

func (s *Server) handleRejectHandoff(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.RejectHandoff(r.Context(), actorID(r), r.PathValue("id"))
	writeResult(w, out, err)
}

func (s *Server) handleHandoffs(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ListHandoffs(r.Context(), "")
	writeResult(w, out, err)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	out, err := s.store.ListEvents(r.Context(), since, "")
	writeResult(w, out, err)
}

func (s *Server) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, ": taskpilot stream connected\n\n")
	flusher.Flush()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	send := func() bool {
		events, err := s.store.ListEvents(r.Context(), since, "")
		if err != nil {
			_, _ = fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
			flusher.Flush()
			return false
		}
		for _, event := range events {
			data, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "id: %d\nevent: taskpilot.event\ndata: %s\n\n", event.ID, data)
			if event.ID > since {
				since = event.ID
			}
		}
		flusher.Flush()
		return true
	}
	_ = send()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !send() {
				return
			}
		case <-heartbeat.C:
			_, _ = fmt.Fprintf(w, ": heartbeat %s\n\n", time.Now().UTC().Format(time.RFC3339))
			flusher.Flush()
		}
	}
}

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		writeErr(w, http.StatusBadRequest, userErr("bad_request", "invalid JSON body"))
		return false
	}
	return true
}

func writeResult(w http.ResponseWriter, out any, err error) {
	if err != nil {
		var mdErrs markdownValidationErrors
		if errors.As(err, &mdErrs) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "validation", "message": "markdown validation failed", "errors": []MarkdownValidationError(mdErrs)})
			return
		}
		status := http.StatusInternalServerError
		switch errorCode(err) {
		case "validation", "bad_request":
			status = http.StatusBadRequest
		case "unauthorized":
			status = http.StatusUnauthorized
		case "forbidden":
			status = http.StatusForbidden
		case "not_found":
			status = http.StatusNotFound
		case "conflict":
			status = http.StatusConflict
		}
		writeErr(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	var pe publicError
	code := "internal"
	msg := "internal server error"
	if errors.As(err, &pe) {
		code = pe.code
		msg = pe.msg
	} else if err != nil {
		msg = err.Error()
	}
	writeJSON(w, status, APIError{Error: code, Message: msg})
}

func writeJSON(w http.ResponseWriter, status int, out any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(out)
}

func ListenAndServe(addr, dbPath, token string) error {
	cfg := LoadServerConfig(addr, dbPath, token, false)
	return ListenAndServeConfig(cfg)
}

func ListenAndServeConfig(cfg ServerConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	store, err := openStoreWithRetry(cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           NewServer(store, cfg.Token),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()
	fmt.Printf("TaskPilot server listening on %s\n", cfg.EffectiveBaseURL())
	fmt.Printf("Team token: %s\n", cfg.Token)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case sig := <-stop:
		fmt.Printf("TaskPilot shutting down after %s\n", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(ctx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func openStoreWithRetry(cfg ServerConfig) (*Store, error) {
	attempts := 1
	if cfg.Production || dbDialect(cfg.DBPath) == "postgres" {
		attempts = 20
	}
	var last error
	for i := 1; i <= attempts; i++ {
		store, err := OpenStore(cfg.DBPath)
		if err == nil {
			return store, nil
		}
		last = err
		if i < attempts {
			fmt.Printf("Waiting for database (%d/%d): %v\n", i, attempts, err)
			time.Sleep(2 * time.Second)
		}
	}
	return nil, last
}
