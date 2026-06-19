package taskpilot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSendOrQueueHandoffCheckpointQueuesRetriableFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TASKPILOT_CONFIG", filepath.Join(dir, "config.json"))
	if err := saveConfig(Config{Server: "http://127.0.0.1:1", ActorID: "actor_1", ActorSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	starts := 0
	restore := stubHandoffSyncDaemonStarter(func() (bool, error) {
		starts++
		return true, nil
	})
	defer restore()

	if _, _, err := sendOrQueueHandoffCheckpoint("task_1", "packet_1", "session_1", validOutboxHandoffMarkdown()); err != nil {
		t.Fatalf("sendOrQueueHandoffCheckpoint should queue retriable failures, got %v", err)
	}
	if starts != 1 {
		t.Fatalf("expected retriable queue to start background sync once, got %d", starts)
	}

	paths, err := queuedHandoffCheckpointPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected one queued checkpoint, got %d paths=%v", len(paths), paths)
	}
	queued, err := readQueuedHandoffCheckpoint(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if queued.TaskID != "task_1" || queued.PacketID != "packet_1" || queued.SessionID != "session_1" {
		t.Fatalf("queued checkpoint has wrong identity: %+v", queued)
	}
	if !strings.Contains(queued.LastError, "cannot reach TaskPilot server") {
		t.Fatalf("queued checkpoint should preserve delivery error, got %q", queued.LastError)
	}
}

func TestFlushQueuedRepoCheckpointsSendsAndDeletesOutboxItem(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TASKPILOT_CONFIG", filepath.Join(dir, "config.json"))
	root := filepath.Join(dir, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, root, "init")
	runGitTestCommand(t, root, "config", "user.email", "taskpilot@example.com")
	runGitTestCommand(t, root, "config", "user.name", "TaskPilot")
	if err := os.WriteFile(filepath.Join(root, "tech.md"), []byte("# Tech\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, root, "add", "tech.md")
	runGitTestCommand(t, root, "commit", "-m", "init")
	if err := os.WriteFile(filepath.Join(root, "tech.md"), []byte("# Tech\n\n## Reason\nUse Canvas.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveRepoConfig(repoEnableConfig{Version: 1, GitRoot: root, ProjectID: "project_1", RepoID: "repo_1", WorkspaceID: "workspace_1", RepoName: "repo"}); err != nil {
		t.Fatal(err)
	}

	contextPosted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/tasks":
			_ = json.NewEncoder(w).Encode([]Task{})
		case r.Method == "POST" && r.URL.Path == "/api/tasks":
			_ = json.NewEncoder(w).Encode(Task{ID: "task_1", ProjectID: "project_1", RepoID: "repo_1", WorkspaceID: "workspace_1", Title: "Update tech.md", Goal: "Coordinate tech.md", Status: "in_progress"})
		case r.Method == "GET" && r.URL.Path == "/api/tasks/task_1":
			_ = json.NewEncoder(w).Encode(TaskDetail{Task: Task{ID: "task_1", ProjectID: "project_1", RepoID: "repo_1", WorkspaceID: "workspace_1", Title: "Update tech.md", Goal: "Coordinate tech.md", Status: "in_progress"}})
		case r.Method == "POST" && r.URL.Path == "/api/tasks/task_1/context":
			contextPosted = true
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if key, _ := body["memory_key"].(string); key == "" || strings.ContainsRune(key, 0) {
				t.Fatalf("expected safe memory key, got %#v", body["memory_key"])
			}
			_ = json.NewEncoder(w).Encode(ContextEntry{ID: "ctx_1", TaskID: "task_1", Kind: "summary"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	if err := saveConfig(Config{Server: server.URL, ActorID: "actor_1", ActorSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	if _, err := queueRepoCheckpoint(root, "agent-hook", "manual", errors.New("previous network failure")); err != nil {
		t.Fatal(err)
	}
	flushed, failed, err := flushQueuedRepoCheckpoints()
	if err != nil {
		t.Fatal(err)
	}
	if flushed != 1 || failed != 0 || !contextPosted {
		t.Fatalf("expected queued repo checkpoint to flush, flushed=%d failed=%d posted=%v", flushed, failed, contextPosted)
	}
	paths, err := queuedRepoCheckpointPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("expected repo checkpoint outbox to be empty, got %v", paths)
	}
}

func TestFlushQueuedRepoSemanticMemorySendsAgentAuthoredContext(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TASKPILOT_CONFIG", filepath.Join(dir, "config.json"))
	root := filepath.Join(dir, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, root, "init")
	runGitTestCommand(t, root, "config", "user.email", "taskpilot@example.com")
	runGitTestCommand(t, root, "config", "user.name", "TaskPilot")
	if err := os.WriteFile(filepath.Join(root, "tech.md"), []byte("# Tech\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, root, "add", "tech.md")
	runGitTestCommand(t, root, "commit", "-m", "init")
	if err := os.WriteFile(filepath.Join(root, "tech.md"), []byte("# Tech\n\n## Reason\nUse Canvas.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveRepoConfig(repoEnableConfig{Version: 1, GitRoot: root, ProjectID: "project_1", RepoID: "repo_1", WorkspaceID: "workspace_1", RepoName: "repo"}); err != nil {
		t.Fatal(err)
	}

	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/tasks":
			_ = json.NewEncoder(w).Encode([]Task{})
		case r.Method == "POST" && r.URL.Path == "/api/tasks":
			_ = json.NewEncoder(w).Encode(Task{ID: "task_1", ProjectID: "project_1", RepoID: "repo_1", WorkspaceID: "workspace_1", Title: "Update tech.md", Goal: "Coordinate tech.md", Status: "in_progress"})
		case r.Method == "POST" && r.URL.Path == "/api/tasks/task_1/context":
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(ContextEntry{ID: "ctx_1", TaskID: "task_1", Kind: "summary"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	if err := saveConfig(Config{Server: server.URL, ActorID: "actor_1", ActorSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	if _, err := queueRepoSemanticMemory(root, "Added two reason bullets to tech.md", "document stack rationale", "read tech.md after edit", "none", []string{"tech.md"}, "working", "mcp", errors.New("previous network failure")); err != nil {
		t.Fatal(err)
	}
	flushed, failed, err := flushQueuedRepoSemanticMemories()
	if err != nil {
		t.Fatal(err)
	}
	if flushed != 1 || failed != 0 {
		t.Fatalf("expected queued semantic memory to flush, flushed=%d failed=%d", flushed, failed)
	}
	if gotBody["confidence"] != "agent_authored" || gotBody["reason"] != "semantic_memory" || gotBody["source"] != "mcp" {
		t.Fatalf("expected agent-authored semantic body, got %+v", gotBody)
	}
	if content, _ := gotBody["content"].(string); !strings.Contains(content, "Added two reason bullets") || !strings.Contains(content, "Verification: read tech.md") {
		t.Fatalf("semantic content not preserved: %+v", gotBody)
	}
}

func TestRepoSemanticMemoryOutboxFallsBackToRepoLocalAndFlushes(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("TASKPILOT_CONFIG", filepath.Join(home, "config.json"))
	root := filepath.Join(dir, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, root, "init")
	runGitTestCommand(t, root, "config", "user.email", "taskpilot@example.com")
	runGitTestCommand(t, root, "config", "user.name", "TaskPilot")
	if err := os.WriteFile(filepath.Join(root, "tech.md"), []byte("# Tech\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, root, "add", "tech.md")
	runGitTestCommand(t, root, "commit", "-m", "init")
	if err := os.WriteFile(filepath.Join(root, "tech.md"), []byte("# Tech\n\n## Reason\nUse Canvas.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveRepoConfig(repoEnableConfig{Version: 1, GitRoot: root, ProjectID: "project_1", RepoID: "repo_1", WorkspaceID: "workspace_1", RepoName: "repo"}); err != nil {
		t.Fatal(err)
	}

	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/tasks":
			_ = json.NewEncoder(w).Encode([]Task{})
		case r.Method == "POST" && r.URL.Path == "/api/tasks":
			_ = json.NewEncoder(w).Encode(Task{ID: "task_1", ProjectID: "project_1", RepoID: "repo_1", WorkspaceID: "workspace_1", Title: "Update tech.md", Goal: "Coordinate tech.md", Status: "in_progress"})
		case r.Method == "POST" && r.URL.Path == "/api/tasks/task_1/context":
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(ContextEntry{ID: "ctx_1", TaskID: "task_1", Kind: "summary"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	if err := saveConfig(Config{Server: server.URL, ActorID: "actor_1", ActorSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	blockGlobalRepoOutbox(t)

	queued, err := queueRepoSemanticMemory(root, "Added one reason bullet to tech.md", "document stack rationale", "read tech.md after edit", "none", []string{"tech.md"}, "working", "agent-hook", errors.New("previous network failure"))
	if err != nil {
		t.Fatal(err)
	}
	localDir := filepath.Join(queued.RepoPath, ".taskpilot", "outbox", "repo-semantic-memory")
	if filepath.Dir(queued.QueuePath) != localDir {
		t.Fatalf("expected repo-local semantic outbox path under %s, got %s", localDir, queued.QueuePath)
	}
	paths, err := queuedRepoSemanticMemoryPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != queued.QueuePath {
		t.Fatalf("expected repo-local queued semantic memory path, got %v want %s", paths, queued.QueuePath)
	}

	flushed, failed, err := flushQueuedRepoSemanticMemories(root)
	if err != nil {
		t.Fatal(err)
	}
	if flushed != 1 || failed != 0 {
		t.Fatalf("expected repo-local semantic memory to flush, flushed=%d failed=%d", flushed, failed)
	}
	if gotBody["confidence"] != "agent_authored" || gotBody["reason"] != "semantic_memory" || gotBody["source"] != "agent-hook" {
		t.Fatalf("expected agent-authored semantic body, got %+v", gotBody)
	}
	paths, err = queuedRepoSemanticMemoryPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("expected repo-local semantic outbox to be empty after flush, got %v", paths)
	}
}

func TestRepoCheckpointOutboxFallsBackToRepoLocal(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("TASKPILOT_CONFIG", filepath.Join(home, "config.json"))
	root := filepath.Join(dir, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, root, "init")
	if err := saveConfig(Config{Server: "http://127.0.0.1:1", ActorID: "actor_1", ActorSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	blockGlobalRepoOutbox(t)

	queued, err := queueRepoCheckpoint(root, "agent-hook", "manual", errors.New("previous network failure"))
	if err != nil {
		t.Fatal(err)
	}
	localDir := filepath.Join(queued.RepoPath, ".taskpilot", "outbox", "repo-checkpoints")
	if filepath.Dir(queued.QueuePath) != localDir {
		t.Fatalf("expected repo-local checkpoint outbox path under %s, got %s", localDir, queued.QueuePath)
	}
	paths, err := queuedRepoCheckpointPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != queued.QueuePath {
		t.Fatalf("expected repo-local queued checkpoint path, got %v want %s", paths, queued.QueuePath)
	}
}

func TestAPIRequestOutboxFallsBackToRepoLocalAndFlushes(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("TASKPILOT_CONFIG", filepath.Join(home, "config.json"))
	root := filepath.Join(dir, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveRepoConfig(repoEnableConfig{Version: 1, GitRoot: root, ProjectID: "project_1", RepoID: "repo_1", WorkspaceID: "workspace_1", RepoName: "repo"}); err != nil {
		t.Fatal(err)
	}
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldwd) }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/tasks" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("X-Actor-ID") != "actor_1" || r.Header.Get("X-Actor-Secret") != "secret" {
			t.Fatalf("actor headers not preserved")
		}
		_ = json.NewEncoder(w).Encode([]Task{{ID: "task_1", Title: "proxied task"}})
	}))
	defer server.Close()
	if err := saveConfig(Config{Server: server.URL, ActorID: "actor_1", ActorSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	blockGlobalRepoOutbox(t)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	queued, responsePath, err := queueAPIRequest(cfg, "GET", "/api/tasks", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	localSuffix := filepath.Join(".taskpilot", "outbox", "api-requests")
	if !strings.HasSuffix(filepath.Dir(queued.QueuePath), localSuffix) {
		t.Fatalf("expected repo-local API request path ending in %s, got %s", localSuffix, queued.QueuePath)
	}
	flushed, failed, err := flushQueuedAPIRequests(root)
	if err != nil {
		t.Fatal(err)
	}
	if flushed != 1 || failed != 0 {
		t.Fatalf("expected API request to flush, flushed=%d failed=%d", flushed, failed)
	}
	response, err := readQueuedAPIResponse(responsePath)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != "ok" || !strings.Contains(string(response.Body), "proxied task") {
		t.Fatalf("unexpected API proxy response: %+v body=%s", response, string(response.Body))
	}
}

func TestRecordRepoSemanticMemoryQueuesWithoutAPIProxyWait(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("TASKPILOT_CONFIG", filepath.Join(home, "config.json"))
	root := filepath.Join(dir, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, root, "init")
	runGitTestCommand(t, root, "config", "user.email", "taskpilot@example.com")
	runGitTestCommand(t, root, "config", "user.name", "TaskPilot")
	if err := os.WriteFile(filepath.Join(root, "controls.md"), []byte("# Controls\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, root, "add", "controls.md")
	runGitTestCommand(t, root, "commit", "-m", "init")
	if err := os.WriteFile(filepath.Join(root, "controls.md"), []byte("# Controls\n\n- Use arrow keys.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveRepoConfig(repoEnableConfig{Version: 1, GitRoot: root, ProjectID: "project_1", RepoID: "repo_1", WorkspaceID: "workspace_1", RepoName: "repo"}); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(Config{Server: "http://127.0.0.1:1", ActorID: "actor_1", ActorSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	blockGlobalRepoOutbox(t)

	result, err := recordRepoSemanticMemory(root, "Added controls guidance.", "Document keyboard input.", "Read controls.md after editing.", "None.", []string{"controls.md"}, "working", "agent-hook", true)
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "queued" {
		t.Fatalf("expected semantic memory to queue when server is unreachable, got %+v", result)
	}
	paths, err := queuedRepoSemanticMemoryPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected one repo semantic memory item, got %v", paths)
	}
	apiPaths, err := queuedAPIRequestPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(apiPaths) != 0 {
		t.Fatalf("semantic memory should not wait through API proxy queue, got API requests %v", apiPaths)
	}
}

func TestTaskPilotHTTPTimeoutIsRetriable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TASKPILOT_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("TASKPILOT_HTTP_TIMEOUT", "20ms")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_ = json.NewEncoder(w).Encode([]Task{})
	}))
	defer server.Close()
	if err := saveConfig(Config{Server: server.URL, ActorID: "actor_1", ActorSecret: "secret"}); err != nil {
		t.Fatal(err)
	}

	var tasks []Task
	err := request("GET", "/api/tasks", nil, &tasks)
	if !isRetriableRequestError(err) {
		t.Fatalf("expected timeout to be retriable, got %v", err)
	}
}

func TestFlushQueuedHandoffCheckpointsSendsAndDeletesOutboxItem(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TASKPILOT_CONFIG", filepath.Join(dir, "config.json"))
	var gotPath string
	var gotActor string
	var gotSecret string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotActor = r.Header.Get("X-Actor-ID")
		gotSecret = r.Header.Get("X-Actor-Secret")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(HandoffCheckpoint{ID: "checkpoint_1", TaskID: "task_1"})
	}))
	defer server.Close()
	if err := saveConfig(Config{Server: server.URL, ActorID: "actor_1", ActorSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	if _, err := queueHandoffCheckpoint("task_1", "packet_1", "session_1", validOutboxHandoffMarkdown(), errors.New("previous network failure")); err != nil {
		t.Fatal(err)
	}

	flushed, failed, err := flushQueuedHandoffCheckpoints()
	if err != nil {
		t.Fatal(err)
	}
	if flushed != 1 || failed != 0 {
		t.Fatalf("flush result flushed=%d failed=%d", flushed, failed)
	}
	if gotPath != "/api/tasks/task_1/handoff-checkpoints" {
		t.Fatalf("unexpected request path %q", gotPath)
	}
	if gotActor != "actor_1" || gotSecret != "secret" {
		t.Fatalf("actor headers not preserved: actor=%q secret=%q", gotActor, gotSecret)
	}
	if gotBody["packet_id"] != "packet_1" || gotBody["session_id"] != "session_1" || gotBody["markdown"] != validOutboxHandoffMarkdown() {
		t.Fatalf("unexpected request body: %+v", gotBody)
	}
	paths, err := queuedHandoffCheckpointPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("expected flushed queue to be empty, got %v", paths)
	}
}

func TestHandoffOutboxWatchRetriesUntilCheckpointUploads(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TASKPILOT_CONFIG", filepath.Join(dir, "config.json"))
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(APIError{Error: "unavailable", Message: "server warming up"})
			return
		}
		_ = json.NewEncoder(w).Encode(HandoffCheckpoint{ID: "checkpoint_1", TaskID: "task_1"})
	}))
	defer server.Close()
	if err := saveConfig(Config{Server: server.URL, ActorID: "actor_1", ActorSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	if _, err := queueHandoffCheckpoint("task_1", "packet_1", "session_1", validOutboxHandoffMarkdown(), errors.New("previous network failure")); err != nil {
		t.Fatal(err)
	}

	result, err := runHandoffOutboxSync(context.Background(), handoffSyncOptions{Watch: true, Interval: time.Millisecond, MaxDuration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if result.Flushed != 1 || result.Failed != 1 || result.Skipped {
		t.Fatalf("unexpected sync result: %+v", result)
	}
	paths, err := queuedHandoffCheckpointPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("expected watch sync to clear outbox, got %v", paths)
	}
	if attempts != 2 {
		t.Fatalf("expected failed attempt plus retry, got %d", attempts)
	}
}

func TestHandoffOutboxWatchSkipsWhenWorkerLockIsFresh(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TASKPILOT_CONFIG", filepath.Join(dir, "config.json"))
	if err := saveConfig(Config{Server: "http://127.0.0.1:1", ActorID: "actor_1", ActorSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	acquired, err := acquireHandoffSyncLock(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("expected test to acquire fresh sync lock")
	}
	defer releaseHandoffSyncLock()

	result, err := runHandoffOutboxSync(context.Background(), handoffSyncOptions{Watch: true, Interval: time.Millisecond, MaxDuration: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Skipped {
		t.Fatalf("expected watch sync to skip with fresh lock, got %+v", result)
	}
}

func TestSendOrQueueHandoffCheckpointDoesNotQueueValidationError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TASKPILOT_CONFIG", filepath.Join(dir, "config.json"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(APIError{
			Error:   "validation",
			Message: "markdown validation failed",
			Errors:  []MarkdownValidationError{{Section: "Completed Work", Line: 4, Message: "add at least one concrete bullet"}},
		})
	}))
	defer server.Close()
	if err := saveConfig(Config{Server: server.URL, ActorID: "actor_1", ActorSecret: "secret"}); err != nil {
		t.Fatal(err)
	}

	_, _, err := sendOrQueueHandoffCheckpoint("task_1", "packet_1", "session_1", "# Task Handoff\n")
	if err == nil || !strings.Contains(err.Error(), "Completed Work line 4") {
		t.Fatalf("expected structured validation error, got %v", err)
	}
	paths, pathErr := queuedHandoffCheckpointPaths()
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if len(paths) != 0 {
		t.Fatalf("validation errors should not be queued, got %v", paths)
	}
}

func stubHandoffSyncDaemonStarter(starter func() (bool, error)) func() {
	previous := handoffSyncDaemonStarter
	handoffSyncDaemonStarter = starter
	return func() {
		handoffSyncDaemonStarter = previous
	}
}

func validOutboxHandoffMarkdown() string {
	return `# Task Handoff

## Objective
Create a planning document.

## Current Status
in_progress

## Current State
The planning document is ready for review.

## Completed Work
- Created planning.md with objective, core features, risks, and definition of done.

## Important Decisions
- Put planning.md in the repository root because there was no existing docs folder.

## Remaining Work
- No known work remains for this documentation task.

## Suggested Next Steps
- Review planning.md if wording changes are needed.

## Handoff Message
planning.md is complete and verified.
`
}

func blockGlobalRepoOutbox(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(taskpilotHomeDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskpilotHomeDir(), "outbox"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
}
