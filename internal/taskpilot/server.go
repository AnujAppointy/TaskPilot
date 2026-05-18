package taskpilot

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
)

//go:embed static/*
var staticFS embed.FS

type Server struct {
	store *Store
	token string
	mux   *http.ServeMux
}

func NewServer(store *Store, token string) *Server {
	if token == "" {
		token = "dev-token"
	}
	s := &Server{store: store, token: token, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.Handle("POST /api/actors/register", s.auth(http.HandlerFunc(s.handleRegisterActor)))
	s.mux.Handle("GET /api/actors", s.auth(http.HandlerFunc(s.handleActors)))
	s.mux.Handle("POST /api/tasks", s.auth(http.HandlerFunc(s.handleCreateTask)))
	s.mux.Handle("GET /api/tasks", s.auth(http.HandlerFunc(s.handleTasks)))
	s.mux.Handle("GET /api/tasks/{id}", s.auth(http.HandlerFunc(s.handleTaskDetail)))
	s.mux.Handle("PATCH /api/tasks/{id}", s.auth(http.HandlerFunc(s.handleUpdateTask)))
	s.mux.Handle("POST /api/tasks/{id}/claim", s.auth(http.HandlerFunc(s.handleClaimTask)))
	s.mux.Handle("POST /api/tasks/{id}/release", s.auth(http.HandlerFunc(s.handleReleaseTask)))
	s.mux.Handle("POST /api/tasks/{id}/heartbeat", s.auth(http.HandlerFunc(s.handleHeartbeatTask)))
	s.mux.Handle("POST /api/tasks/{id}/complete", s.auth(http.HandlerFunc(s.handleCompleteTask)))
	s.mux.Handle("POST /api/tasks/{id}/context", s.auth(http.HandlerFunc(s.handleAppendContext)))
	s.mux.Handle("GET /api/tasks/{id}/context", s.auth(http.HandlerFunc(s.handleContext)))
	s.mux.Handle("POST /api/tasks/{id}/locks", s.auth(http.HandlerFunc(s.handleAcquireLock)))
	s.mux.Handle("GET /api/tasks/{id}/locks", s.auth(http.HandlerFunc(s.handleLocks)))
	s.mux.Handle("POST /api/locks/{id}/release", s.auth(http.HandlerFunc(s.handleReleaseLock)))
	s.mux.Handle("POST /api/locks/{id}/renew", s.auth(http.HandlerFunc(s.handleRenewLock)))
	s.mux.Handle("POST /api/tasks/{id}/handoff", s.auth(http.HandlerFunc(s.handlePrepareHandoff)))
	s.mux.Handle("POST /api/handoffs/{id}/accept", s.auth(http.HandlerFunc(s.handleAcceptHandoff)))
	s.mux.Handle("POST /api/handoffs/{id}/reject", s.auth(http.HandlerFunc(s.handleRejectHandoff)))
	s.mux.Handle("GET /api/handoffs", s.auth(http.HandlerFunc(s.handleHandoffs)))
	s.mux.Handle("GET /api/events", s.auth(http.HandlerFunc(s.handleEvents)))
	s.mux.Handle("/", s.dashboard())
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, actorID)))
	})
}

type actorKey struct{}

func actorID(r *http.Request) string {
	v, _ := r.Context().Value(actorKey{}).(string)
	return v
}

func (s *Server) dashboard() http.Handler {
	sub, _ := fs.Sub(staticFS, "static")
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
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
	out, err := s.store.ListTasks(r.Context())
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
		writeJSON(w, http.StatusConflict, map[string]any{"error": "conflict", "message": err.Error(), "conflicts": conflicts})
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
	out, err := s.store.ReleaseLock(r.Context(), actorID(r), r.PathValue("id"))
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

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		writeErr(w, http.StatusBadRequest, userErr("bad_request", "invalid JSON body"))
		return false
	}
	return true
}

func writeResult(w http.ResponseWriter, out any, err error) {
	if err != nil {
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
	if token == "" || token == "dev-token" {
		host, _, _ := net.SplitHostPort(addr)
		if host != "" && host != "127.0.0.1" && host != "localhost" && host != "::1" {
			return userErr("validation", "refusing to expose server with default token; set TASKPILOT_TOKEN or pass --token with a strong value")
		}
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	fmt.Printf("TaskPilot server listening on http://%s\n", addr)
	fmt.Printf("Team token: %s\n", token)
	return http.ListenAndServe(addr, NewServer(store, token))
}
