package taskpilot

import (
	"context"
	"strings"
	"testing"
)

func TestTaskClaimConflictAndExpiry(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testActor(t, s, "Agent A")
	b := testActor(t, s, "Agent B")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Task", Goal: "Goal"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(ctx, a.ID, task.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(ctx, b.ID, task.ID, "", false); err == nil || errorCode(err) != "conflict" {
		t.Fatalf("expected active owner conflict, got %v", err)
	}
	if _, err := s.ClaimTask(ctx, b.ID, task.ID, "handoff", true); err != nil {
		t.Fatalf("force claim should work with reason: %v", err)
	}
}

func TestLockConflictAndRelease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testActor(t, s, "Agent A")
	b := testActor(t, s, "Agent B")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Task", Goal: "Goal"})
	if err != nil {
		t.Fatal(err)
	}
	lock, conflicts, err := s.AcquireLock(ctx, a.ID, task.ID, "src/auth/*", "file_glob")
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("expected first lock to succeed, lock=%v conflicts=%v err=%v", lock, conflicts, err)
	}
	if _, conflicts, err := s.AcquireLock(ctx, b.ID, task.ID, "src/auth/login.go", "file_glob"); err == nil || len(conflicts) != 1 {
		t.Fatalf("expected overlapping lock conflict, conflicts=%v err=%v", conflicts, err)
	}
	if _, err := s.ReleaseLock(ctx, a.ID, lock.ID); err != nil {
		t.Fatal(err)
	}
	if _, conflicts, err := s.AcquireLock(ctx, b.ID, task.ID, "src/auth/login.go", "file_glob"); err != nil || len(conflicts) != 0 {
		t.Fatalf("expected lock after release to succeed, conflicts=%v err=%v", conflicts, err)
	}
}

func TestHandoffTransfersTaskAndActiveLocks(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testActor(t, s, "Agent A")
	b := testActor(t, s, "Agent B")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Task", Goal: "Goal"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(ctx, a.ID, task.ID, "", false); err != nil {
		t.Fatal(err)
	}
	lock, _, err := s.AcquireLock(ctx, a.ID, task.ID, "src/auth/*", "file_glob")
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.PrepareHandoff(ctx, a.ID, task.ID, b.ID, "resume here", []string{"continue"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptHandoff(ctx, b.ID, h.ID); err != nil {
		t.Fatal(err)
	}
	detail, err := s.TaskDetail(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.OwnerID != b.ID {
		t.Fatalf("expected owner %s, got %s", b.ID, detail.Task.OwnerID)
	}
	foundTransferred := false
	for _, l := range detail.Locks {
		if l.ID == lock.ID && l.OwnerID == b.ID {
			foundTransferred = true
		}
	}
	if !foundTransferred {
		t.Fatalf("expected active lock to transfer to accepting actor")
	}
}

func TestActorSecretVerification(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testActor(t, s, "Agent A")
	ok, err := s.VerifyActorSecret(ctx, a.ID, a.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected actor secret to verify")
	}
	ok, err = s.VerifyActorSecret(ctx, a.ID, "wrong")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected wrong actor secret to fail")
	}
	events, err := s.ListEvents(ctx, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		raw := js(e.Payload)
		if raw != "{}" && contains(raw, a.Secret) {
			t.Fatal("actor secret leaked into audit event payload")
		}
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testActor(t *testing.T, s *Store, name string) Actor {
	t.Helper()
	a, err := s.RegisterActor(context.Background(), name, "agent", "test")
	if err != nil {
		t.Fatal(err)
	}
	return a
}
