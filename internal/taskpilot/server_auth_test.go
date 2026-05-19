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

func TestAPIKeyScopeEnforcementOnRoutes(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	actor := testActor(t, store, "Scoped Agent")
	viewer, err := store.CreateAPIKey(ctx, "viewer", actor.ID, "viewer", []string{"task:read"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := store.CreateAPIKey(ctx, "admin", actor.ID, "admin", []string{"admin"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, "dev-token")

	getTasks := authReq(t, server, "GET", "/api/tasks", nil, viewer.Secret)
	if getTasks.Code != http.StatusOK {
		t.Fatalf("viewer task read status=%d body=%s", getTasks.Code, getTasks.Body.String())
	}
	createTask := authReq(t, server, "POST", "/api/tasks", map[string]any{"title": "Task", "goal": "Goal"}, viewer.Secret)
	if createTask.Code != http.StatusForbidden {
		t.Fatalf("viewer task write should be forbidden, status=%d body=%s", createTask.Code, createTask.Body.String())
	}
	createUser := authReq(t, server, "POST", "/api/users", map[string]any{
		"email": "new@example.com", "name": "New User", "password": "strong-password", "role": "developer",
	}, admin.Secret)
	if createUser.Code != http.StatusOK {
		t.Fatalf("admin create user status=%d body=%s", createUser.Code, createUser.Body.String())
	}
}

func TestSessionLoginPasswordAndAdminKeyRoutes(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	actor := testActor(t, store, "Agent")
	adminUser, err := store.CreateUser(ctx, "admin@example.com", "Admin", "strong-password", "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateUser(ctx, "viewer@example.com", "Viewer", "strong-password", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, "dev-token")

	login := req(t, server, "POST", "/api/auth/login", map[string]any{"email": "admin@example.com", "password": "strong-password"})
	if login.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	me := reqWithCookie(t, server, "GET", "/api/me", nil, cookie)
	if me.Code != http.StatusOK {
		t.Fatalf("me status=%d body=%s", me.Code, me.Body.String())
	}
	createKey := reqWithCookie(t, server, "POST", "/api/api-keys", map[string]any{
		"name": "agent key", "actor_id": actor.ID, "role": "agent", "scopes": []string{"task:read"},
	}, cookie)
	if createKey.Code != http.StatusOK {
		t.Fatalf("create key status=%d body=%s", createKey.Code, createKey.Body.String())
	}
	var key APIKey
	if err := json.Unmarshal(createKey.Body.Bytes(), &key); err != nil {
		t.Fatal(err)
	}
	if key.Secret == "" {
		t.Fatal("expected one-time raw api key on create")
	}
	listKeys := reqWithCookie(t, server, "GET", "/api/api-keys", nil, cookie)
	if listKeys.Code != http.StatusOK {
		t.Fatalf("list keys status=%d body=%s", listKeys.Code, listKeys.Body.String())
	}
	if strings.Contains(listKeys.Body.String(), key.Secret) {
		t.Fatal("list api keys leaked raw secret")
	}
	revoke := reqWithCookie(t, server, "DELETE", "/api/api-keys/"+key.ID, nil, cookie)
	if revoke.Code != http.StatusOK {
		t.Fatalf("revoke key status=%d body=%s", revoke.Code, revoke.Body.String())
	}
	if _, err := store.VerifyAPIKey(ctx, key.Secret); err == nil {
		t.Fatal("expected revoked api key to stop authenticating")
	}
	change := reqWithCookie(t, server, "POST", "/api/me/password", map[string]any{"current_password": "strong-password", "new_password": "new-strong-password"}, cookie)
	if change.Code != http.StatusOK {
		t.Fatalf("change password status=%d body=%s", change.Code, change.Body.String())
	}
	if _, err := store.AuthenticateUser(ctx, adminUser.Email, "strong-password"); err == nil {
		t.Fatal("old password should no longer authenticate")
	}
	if _, err := store.AuthenticateUser(ctx, adminUser.Email, "new-strong-password"); err != nil {
		t.Fatalf("new password should authenticate: %v", err)
	}
}

func TestViewerSessionCannotMutateAdminRoutes(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	_, err := store.CreateUser(ctx, "viewer@example.com", "Viewer", "strong-password", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, "dev-token")
	login := req(t, server, "POST", "/api/auth/login", map[string]any{"email": "viewer@example.com", "password": "strong-password"})
	if login.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	users := reqWithCookie(t, server, "GET", "/api/users", nil, cookie)
	if users.Code != http.StatusForbidden {
		t.Fatalf("viewer users route should be forbidden, status=%d body=%s", users.Code, users.Body.String())
	}
	taskCreate := reqWithCookie(t, server, "POST", "/api/tasks", map[string]any{"title": "Task", "goal": "Goal"}, cookie)
	if taskCreate.Code != http.StatusForbidden {
		t.Fatalf("viewer task create should be forbidden, status=%d body=%s", taskCreate.Code, taskCreate.Body.String())
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
		{"GET", "/api/users", nil},
		{"POST", "/api/users", map[string]any{}},
		{"PATCH", "/api/users/user_missing", map[string]any{}},
		{"POST", "/api/users/user_missing/password", map[string]any{}},
		{"GET", "/api/api-keys", nil},
		{"POST", "/api/api-keys", map[string]any{}},
		{"DELETE", "/api/api-keys/key_missing", nil},
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
	key, err := store.CreateAPIKey(ctx, "stream", actor.ID, "viewer", []string{"task:read"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTask(ctx, actor.ID, TaskInput{Title: "Stream Task", Goal: "Check stream"}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(store, "dev-token")
	reqCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r := httptest.NewRequestWithContext(reqCtx, "GET", "/api/events/stream?since=0", nil)
	r.Header.Set("Authorization", "ApiKey "+key.Secret)
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

func authReq(t *testing.T, h http.Handler, method, path string, body any, apiKey string) *httptest.ResponseRecorder {
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
	r.Header.Set("Authorization", "ApiKey "+apiKey)
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
