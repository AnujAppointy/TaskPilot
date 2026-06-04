package taskpilot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
