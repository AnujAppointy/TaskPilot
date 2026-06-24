package taskpilot

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestActorSecretAuthOnRoutes(t *testing.T) {
	store := testStore(t)
	actor := testActor(t, store, "Scoped Agent")
	server := NewServer(store, "dev-token")

	getTasks := actorReq(t, server, "GET", "/api/tasks", nil, actor.ID, actor.Secret)
	if getTasks.Code != http.StatusOK {
		t.Fatalf("actor task read status=%d body=%s", getTasks.Code, getTasks.Body.String())
	}
	createTask := actorReq(t, server, "POST", "/api/tasks", map[string]any{"title": "Task", "goal": "Goal"}, actor.ID, actor.Secret)
	if createTask.Code != http.StatusOK {
		t.Fatalf("actor task write status=%d body=%s", createTask.Code, createTask.Body.String())
	}
	bad := actorReq(t, server, "GET", "/api/tasks", nil, actor.ID, "wrong-secret")
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("bad actor secret should be unauthorized, status=%d body=%s", bad.Code, bad.Body.String())
	}
}

func TestActorSessionAuthAttributesContextAndLocks(t *testing.T) {
	store := testStore(t)
	actor := testActor(t, store, "Session Agent")
	server := NewServer(store, "dev-token")

	activate := req(t, server, "POST", "/api/actor-sessions/activate", ActorSessionStartInput{ActorID: actor.ID, ActorSecret: actor.Secret, TerminalID: "terminal-a", AgentProvider: "codex"})
	if activate.Code != http.StatusOK {
		t.Fatalf("activate status=%d body=%s", activate.Code, activate.Body.String())
	}
	var activation ActorSessionActivation
	if err := json.Unmarshal(activate.Body.Bytes(), &activation); err != nil {
		t.Fatal(err)
	}
	createTask := sessionReq(t, server, "POST", "/api/tasks", map[string]any{"title": "Task", "goal": "Goal"}, actor.ID, activation.Session.ID, activation.SessionToken)
	if createTask.Code != http.StatusOK {
		t.Fatalf("create task status=%d body=%s", createTask.Code, createTask.Body.String())
	}
	var task Task
	if err := json.Unmarshal(createTask.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	context := sessionReq(t, server, "POST", "/api/tasks/"+task.ID+"/context", map[string]any{"kind": "summary", "content": "session scoped memory", "source": "mcp"}, actor.ID, activation.Session.ID, activation.SessionToken)
	if context.Code != http.StatusOK {
		t.Fatalf("context status=%d body=%s", context.Code, context.Body.String())
	}
	var entry ContextEntry
	if err := json.Unmarshal(context.Body.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.ActorSessionID != activation.Session.ID {
		t.Fatalf("expected context actor session %q, got %+v", activation.Session.ID, entry)
	}
	lockResp := sessionReq(t, server, "POST", "/api/tasks/"+task.ID+"/locks", map[string]any{"scope": "src/*", "scope_type": "file_glob"}, actor.ID, activation.Session.ID, activation.SessionToken)
	if lockResp.Code != http.StatusOK {
		t.Fatalf("lock status=%d body=%s", lockResp.Code, lockResp.Body.String())
	}
	var lock Lock
	if err := json.Unmarshal(lockResp.Body.Bytes(), &lock); err != nil {
		t.Fatal(err)
	}
	if lock.ActorSessionID != activation.Session.ID {
		t.Fatalf("expected lock actor session %q, got %+v", activation.Session.ID, lock)
	}
}

func TestSessionLoginPasswordAndUserRoutes(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	user, err := store.CreateUser(ctx, "dev@example.com", "Developer", "strong-password", "")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, "dev-token")

	login := req(t, server, "POST", "/api/auth/login", map[string]any{"email": "dev@example.com", "password": "strong-password"})
	if login.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	me := reqWithCookie(t, server, "GET", "/api/me", nil, cookie)
	if me.Code != http.StatusOK {
		t.Fatalf("me status=%d body=%s", me.Code, me.Body.String())
	}
	createTask := reqWithCookie(t, server, "POST", "/api/tasks", map[string]any{"title": "Session Task", "goal": "Check session write"}, cookie)
	if createTask.Code != http.StatusOK {
		t.Fatalf("session user create task status=%d body=%s", createTask.Code, createTask.Body.String())
	}
	change := reqWithCookie(t, server, "POST", "/api/me/password", map[string]any{"current_password": "strong-password", "new_password": "new-strong-password"}, cookie)
	if change.Code != http.StatusOK {
		t.Fatalf("change password status=%d body=%s", change.Code, change.Body.String())
	}
	if _, err := store.AuthenticateUser(ctx, user.Email, "strong-password"); err == nil {
		t.Fatal("old password should no longer authenticate")
	}
	if _, err := store.AuthenticateUser(ctx, user.Email, "new-strong-password"); err != nil {
		t.Fatalf("new password should authenticate: %v", err)
	}
}

func TestSessionHandoffAcceptUsesDefaultActorOwner(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	user, err := store.CreateUser(ctx, "dev@example.com", "Developer", "strong-password", "")
	if err != nil {
		t.Fatal(err)
	}
	from := testActor(t, store, "Original Agent")
	task, err := store.CreateTask(ctx, from.ID, TaskInput{Title: "Plan", Goal: "Plan the work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimTask(ctx, from.ID, task.ID, "", false); err != nil {
		t.Fatal(err)
	}
	handoff, err := store.PrepareHandoff(ctx, from.ID, task.ID, "", "Ready for next actor", []string{"Continue planning"})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, "dev-token")
	login := req(t, server, "POST", "/api/auth/login", map[string]any{"email": user.Email, "password": "strong-password"})
	if login.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	accept := reqWithCookie(t, server, "POST", "/api/handoffs/"+handoff.ID+"/accept", map[string]any{}, cookie)
	if accept.Code != http.StatusOK {
		t.Fatalf("accept status=%d body=%s", accept.Code, accept.Body.String())
	}
	detail := reqWithCookie(t, server, "GET", "/api/tasks/"+task.ID, nil, cookie)
	if detail.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	var out TaskDetail
	if err := json.Unmarshal(detail.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Task.OwnerID == user.ID {
		t.Fatalf("dashboard handoff accept should not store user id as owner: %+v", out.Task)
	}
	if out.Owner == nil || out.Owner.CreatedByUserID != user.ID {
		t.Fatalf("expected owner to resolve to user's default actor, got owner=%+v task=%+v", out.Owner, out.Task)
	}
}

func TestProtectedRoutesRequireAuth(t *testing.T) {
	store := testStore(t)
	server := NewServer(store, "dev-token")
	routes := []struct {
		method string
		path   string
		body   any
	}{
		{"GET", "/api/me", nil},
		{"POST", "/api/me/password", map[string]any{}},
		{"GET", "/api/projects", nil},
		{"POST", "/api/projects", map[string]any{}},
		{"GET", "/api/repositories", nil},
		{"POST", "/api/repositories", map[string]any{}},
		{"GET", "/api/workspaces", nil},
		{"POST", "/api/workspaces", map[string]any{}},
		{"POST", "/api/actors/register", map[string]any{}},
		{"GET", "/api/actors", nil},
		{"POST", "/api/tasks", map[string]any{}},
		{"GET", "/api/tasks", nil},
		{"GET", "/api/tasks/task_missing", nil},
		{"PATCH", "/api/tasks/task_missing", map[string]any{}},
		{"POST", "/api/tasks/task_missing/subtasks", map[string]any{}},
		{"POST", "/api/tasks/task_missing/dependencies", map[string]any{}},
		{"POST", "/api/tasks/task_missing/claim", map[string]any{}},
		{"POST", "/api/tasks/task_missing/release", map[string]any{}},
		{"POST", "/api/tasks/task_missing/heartbeat", map[string]any{}},
		{"POST", "/api/tasks/task_missing/complete", map[string]any{}},
		{"POST", "/api/tasks/task_missing/context", map[string]any{}},
		{"GET", "/api/tasks/task_missing/context", nil},
		{"POST", "/api/tasks/task_missing/decisions", map[string]any{}},
		{"GET", "/api/tasks/task_missing/decisions", nil},
		{"POST", "/api/tasks/task_missing/comments", map[string]any{}},
		{"GET", "/api/tasks/task_missing/comments", nil},
		{"POST", "/api/tasks/task_missing/artifacts", map[string]any{}},
		{"GET", "/api/tasks/task_missing/artifacts", nil},
		{"POST", "/api/tasks/task_missing/git", map[string]any{}},
		{"GET", "/api/tasks/task_missing/git", nil},
		{"POST", "/api/tasks/task_missing/locks", map[string]any{}},
		{"GET", "/api/tasks/task_missing/locks", nil},
		{"POST", "/api/locks/lock_missing/release", map[string]any{}},
		{"POST", "/api/locks/lock_missing/renew", map[string]any{}},
		{"POST", "/api/tasks/task_missing/handoff", map[string]any{}},
		{"POST", "/api/handoffs/handoff_missing/accept", map[string]any{}},
		{"POST", "/api/handoffs/handoff_missing/reject", map[string]any{}},
		{"DELETE", "/api/dependencies/dep_missing", nil},
		{"GET", "/api/conflicts", nil},
		{"POST", "/api/conflicts/conflict_missing/resolve", map[string]any{}},
		{"GET", "/api/handoffs", nil},
		{"GET", "/api/events", nil},
		{"GET", "/api/events/stream", nil},
	}
	for _, route := range routes {
		rec := req(t, server, route.method, route.path, route.body)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s should require auth, status=%d body=%s", route.method, route.path, rec.Code, rec.Body.String())
		}
	}
}

func TestEventsStreamSendsExistingEvents(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	actor := testActor(t, store, "Stream Agent")
	if _, err := store.CreateTask(ctx, actor.ID, TaskInput{Title: "Stream Task", Goal: "Check stream"}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, "dev-token")
	reqCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r := httptest.NewRequestWithContext(reqCtx, "GET", "/api/events/stream?since=0", nil)
	r.Header.Set("X-Actor-ID", actor.ID)
	r.Header.Set("X-Actor-Secret", actor.Secret)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, r)
	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream status=%d body=%s", res.StatusCode, rec.Body.String())
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("expected event-stream content type, got %q", ct)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "event: taskpilot.event") || !strings.Contains(string(body), "task.created") {
		t.Fatalf("expected stream event, got:\n%s", string(body))
	}
}

func actorReq(t *testing.T, h http.Handler, method, path string, body any, actorID, actorSecret string) *httptest.ResponseRecorder {
	t.Helper()
	var rbody *bytes.Reader
	if body == nil {
		rbody = bytes.NewReader(nil)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rbody = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, rbody)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Actor-ID", actorID)
	r.Header.Set("X-Actor-Secret", actorSecret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func sessionReq(t *testing.T, h http.Handler, method, path string, body any, actorID, sessionID, sessionToken string) *httptest.ResponseRecorder {
	t.Helper()
	var rbody *bytes.Reader
	if body == nil {
		rbody = bytes.NewReader(nil)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rbody = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, rbody)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Actor-ID", actorID)
	r.Header.Set("X-Actor-Session-ID", sessionID)
	r.Header.Set("X-Actor-Session-Token", sessionToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func req(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rbody *bytes.Reader
	if body == nil {
		rbody = bytes.NewReader(nil)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rbody = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, rbody)
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func reqWithCookie(t *testing.T, h http.Handler, method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var rbody *bytes.Reader
	if body == nil {
		rbody = bytes.NewReader(nil)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rbody = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, rbody)
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}
