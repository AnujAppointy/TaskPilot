package taskpilot

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
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
	packets, err := s.ListHandoffs(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(packets) != 1 || packets[0].Packet != nil {
		t.Fatalf("expected simple handoff to create one visible handoff without a generated packet, got %+v", packets)
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

func TestUserSessionAndAPIKeyAuth(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testActor(t, s, "Agent A")
	u, err := s.CreateUser(ctx, "ADMIN@example.com", "Admin", "strong-password", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "admin@example.com" || u.Role != "admin" {
		t.Fatalf("unexpected user normalization: %+v", u)
	}
	if _, err := s.AuthenticateUser(ctx, u.Email, "wrong-password"); err == nil || errorCode(err) != "unauthorized" {
		t.Fatalf("expected bad password to fail, got %v", err)
	}
	if _, err := s.AuthenticateUser(ctx, u.Email, "strong-password"); err != nil {
		t.Fatalf("expected login to work: %v", err)
	}
	session, err := s.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.VerifySession(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != "user" || p.Role != "admin" || p.UserID != u.ID {
		t.Fatalf("unexpected session principal: %+v", p)
	}
	if err := s.RevokeSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifySession(ctx, session); err == nil || errorCode(err) != "unauthorized" {
		t.Fatalf("expected revoked session to fail, got %v", err)
	}
	if _, err := s.CreateAPIKey(ctx, "bad key", "missing-actor", "agent", nil, u.ID); err == nil || errorCode(err) != "validation" {
		t.Fatalf("expected missing actor validation, got %v", err)
	}
	key, err := s.CreateAPIKey(ctx, "agent key", a.ID, "agent", []string{"task:read", "context:write"}, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if key.Secret == "" {
		t.Fatal("expected one-time API key secret in create response")
	}
	p, err = s.VerifyAPIKey(ctx, key.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != "api_key" || p.ActorID != a.ID || !hasScope(p.Scopes, "context:write") || hasScope(p.Scopes, "lock:write") {
		t.Fatalf("unexpected api key principal/scopes: %+v", p)
	}
	events, err := s.ListEvents(ctx, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		raw := js(e.Payload)
		if contains(raw, key.Secret) || contains(raw, "strong-password") {
			t.Fatal("secret leaked into audit event payload")
		}
	}
}

func TestStatusTransitionValidation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testActor(t, s, "Agent A")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Task", Goal: "Goal"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteTask(ctx, a.ID, task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTask(ctx, a.ID, task.ID, TaskInput{Status: "blocked"}, "bad reopen"); err == nil || errorCode(err) != "validation" {
		t.Fatalf("expected invalid completed -> blocked transition, got %v", err)
	}
	if _, err := s.UpdateTask(ctx, a.ID, task.ID, TaskInput{Status: "ready"}, "reopen"); err != nil {
		t.Fatalf("expected completed -> ready reopen to work: %v", err)
	}
}

func TestStatsUsesEmptyDatabase(t *testing.T) {
	s := testStore(t)
	stats, err := s.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Tasks != 0 || stats.Actors != 0 || stats.ActiveLocks != 0 {
		t.Fatalf("unexpected empty stats: %+v", stats)
	}
}

func TestProjectsRepositoriesWorkspacesAndTaskFiltering(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testActor(t, s, "Agent A")
	projects, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) == 0 || projects[0].ID != "project_default" {
		t.Fatalf("expected default project, got %+v", projects)
	}
	project, err := s.CreateProject(ctx, a.ID, "Backend", "Backend coordination")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.CreateRepository(ctx, a.ID, project.ID, "api", "/repo/api", "main")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := s.CreateWorkspace(ctx, a.ID, project.ID, a.ID, "Anuj Mac", "anuj-mac", "local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, a.ID, TaskInput{ProjectID: project.ID, RepoID: repo.ID, WorkspaceID: workspace.ID, Title: "Task", Goal: "Goal"})
	if err != nil {
		t.Fatal(err)
	}
	if task.ProjectID != project.ID || task.RepoID != repo.ID || task.WorkspaceID != workspace.ID {
		t.Fatalf("task missing project/repo/workspace metadata: %+v", task)
	}
	filtered, err := s.ListTasks(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].ID != task.ID {
		t.Fatalf("expected filtered task, got %+v", filtered)
	}
	defaultTasks, err := s.ListTasks(ctx, "project_default")
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultTasks) != 0 {
		t.Fatalf("expected no default tasks, got %+v", defaultTasks)
	}
}

func TestSubtasksAndDependenciesBlockCompletion(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testActor(t, s, "Agent A")
	parent, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Parent", Goal: "Coordinate the work"})
	if err != nil {
		t.Fatal(err)
	}
	subtask, err := s.CreateTask(ctx, a.ID, TaskInput{ParentTaskID: parent.ID, Title: "Subtask", Goal: "Finish child work"})
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Blocker", Goal: "Complete this first"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Main", Goal: "Depends on blocker"})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := s.AddTaskDependency(ctx, a.ID, task.ID, blocker.ID)
	if err != nil {
		t.Fatal(err)
	}
	parentDetail, err := s.TaskDetail(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(parentDetail.Subtasks) != 1 || parentDetail.Subtasks[0].ID != subtask.ID {
		t.Fatalf("expected parent detail to include subtask, got %+v", parentDetail.Subtasks)
	}
	taskDetail, err := s.TaskDetail(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(taskDetail.Dependencies) != 1 || taskDetail.Dependencies[0].ID != dep.ID || taskDetail.Dependencies[0].DependsOnTask == nil {
		t.Fatalf("expected task detail dependency with expanded task, got %+v", taskDetail.Dependencies)
	}
	blockerDetail, err := s.TaskDetail(ctx, blocker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(blockerDetail.Dependents) != 1 || blockerDetail.Dependents[0].Task == nil || blockerDetail.Dependents[0].Task.ID != task.ID {
		t.Fatalf("expected blocker detail to include dependent task, got %+v", blockerDetail.Dependents)
	}
	if _, err := s.CompleteTask(ctx, a.ID, parent.ID, "done"); err == nil || errorCode(err) != "conflict" {
		t.Fatalf("expected incomplete subtask to block completion, got %v", err)
	}
	if _, err := s.CompleteTask(ctx, a.ID, task.ID, "done"); err == nil || errorCode(err) != "conflict" {
		t.Fatalf("expected open dependency to block completion, got %v", err)
	}
	if _, err := s.CompleteTask(ctx, a.ID, subtask.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteTask(ctx, a.ID, parent.ID, "done"); err != nil {
		t.Fatalf("expected parent completion after subtask completion: %v", err)
	}
	if _, err := s.CompleteTask(ctx, a.ID, blocker.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteTask(ctx, a.ID, task.ID, "done"); err != nil {
		t.Fatalf("expected task completion after dependency completion: %v", err)
	}
}

func TestTaskDependencyDuplicateRemovalAndCycleValidation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testActor(t, s, "Agent A")
	first, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "First", Goal: "First goal"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Second", Goal: "Second goal"})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := s.AddTaskDependency(ctx, a.ID, first.ID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTaskDependency(ctx, a.ID, first.ID, second.ID); err == nil || errorCode(err) != "conflict" {
		t.Fatalf("expected duplicate dependency conflict, got %v", err)
	}
	if _, err := s.AddTaskDependency(ctx, a.ID, second.ID, first.ID); err == nil || errorCode(err) != "validation" {
		t.Fatalf("expected dependency cycle validation, got %v", err)
	}
	if err := s.RemoveTaskDependency(ctx, a.ID, dep.ID); err != nil {
		t.Fatal(err)
	}
	deps, err := s.ListTaskDependencies(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Fatalf("expected dependency removal, got %+v", deps)
	}
	if _, err := s.AddTaskDependency(ctx, a.ID, second.ID, first.ID); err != nil {
		t.Fatalf("expected reverse dependency after removal to succeed: %v", err)
	}
}

func TestDecisionRecordsAndCommentsAreTaskDetailEntities(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testActor(t, s, "Agent A")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Decision Task", Goal: "Capture rationale"})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := s.AddDecision(ctx, a.ID, task.ID, "Keep token format unchanged", []string{"Rotate all tokens", "Add schema column"}, "Existing invite URLs must keep working", "Patch only expiry validation")
	if err != nil {
		t.Fatal(err)
	}
	comment, err := s.AddComment(ctx, a.ID, task.ID, "Please review the expiry edge cases before merge.")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDecision(ctx, a.ID, task.ID, "", nil, "", ""); err == nil || errorCode(err) != "validation" {
		t.Fatalf("expected empty decision validation, got %v", err)
	}
	if _, err := s.AddComment(ctx, a.ID, task.ID, ""); err == nil || errorCode(err) != "validation" {
		t.Fatalf("expected empty comment validation, got %v", err)
	}
	detail, err := s.TaskDetail(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Decisions) != 1 || detail.Decisions[0].ID != decision.ID || len(detail.Decisions[0].Alternatives) != 2 {
		t.Fatalf("expected decision in task detail, got %+v", detail.Decisions)
	}
	if len(detail.Comments) != 1 || detail.Comments[0].ID != comment.ID || detail.Comments[0].Body != comment.Body {
		t.Fatalf("expected comment in task detail, got %+v", detail.Comments)
	}
	events, err := s.ListEvents(ctx, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundDecision := false
	foundComment := false
	for _, e := range events {
		if e.EventType == "decision.recorded" {
			foundDecision = true
		}
		if e.EventType == "comment.added" {
			foundComment = true
		}
	}
	if !foundDecision || !foundComment {
		t.Fatalf("expected decision/comment audit events, got %+v", events)
	}
}

func TestConflictResolutionWorkflow(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testActor(t, s, "Agent A")
	b := testActor(t, s, "Agent B")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Conflict Task", Goal: "Coordinate collision"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(ctx, a.ID, task.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(ctx, b.ID, task.ID, "", false); err == nil || errorCode(err) != "conflict" {
		t.Fatalf("expected claim conflict, got %v", err)
	}
	conflicts, err := s.ListConflicts(ctx, "open")
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].ConflictType != "ownership" {
		t.Fatalf("expected open ownership conflict, got %+v", conflicts)
	}
	if _, err := s.ResolveConflict(ctx, a.ID, conflicts[0].ID, "transfer_ownership", "Agent B should continue.", b.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.OwnerID != b.ID || updated.Status != "claimed" {
		t.Fatalf("expected task transferred to Agent B, got %+v", updated)
	}
	resolved, err := s.ListConflicts(ctx, "resolved")
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].Resolution != "transfer_ownership" || resolved[0].ResolutionNote == "" {
		t.Fatalf("expected resolved transfer conflict, got %+v", resolved)
	}
}

func TestLockConflictResolutionCanPauseWork(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testActor(t, s, "Agent A")
	b := testActor(t, s, "Agent B")
	first, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "First", Goal: "Own broad scope"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateTask(ctx, b.ID, TaskInput{Title: "Second", Goal: "Collides on file"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcquireLock(ctx, a.ID, first.ID, "src/auth/*", "file_glob"); err != nil {
		t.Fatal(err)
	}
	if _, conflicts, err := s.AcquireLock(ctx, b.ID, second.ID, "src/auth/login.go", "file_glob"); err == nil || len(conflicts) != 1 {
		t.Fatalf("expected lock conflict, conflicts=%+v err=%v", conflicts, err)
	}
	open, err := s.ListConflicts(ctx, "open")
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].ConflictType != "lock_overlap" || open[0].OtherTask == nil || open[0].OtherTask.ID != first.ID {
		t.Fatalf("expected expanded lock conflict, got %+v", open)
	}
	if _, err := s.ResolveConflict(ctx, a.ID, open[0].ID, "pause_secondary_work", "Wait for auth owner to finish.", ""); err != nil {
		t.Fatal(err)
	}
	paused, err := s.GetTask(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != "blocked" {
		t.Fatalf("expected colliding task to be blocked, got %+v", paused)
	}
}

func TestArtifactReferencesAndGitMetadata(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testActor(t, s, "Agent A")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Artifact Task", Goal: "Track outputs"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := s.AddArtifact(ctx, a.ID, task.ID, "pr", "Signup fix PR", "https://github.com/acme/app/pull/42", "Reviewable code change", map[string]any{"provider": "github"})
	if err != nil {
		t.Fatal(err)
	}
	gitRef, err := s.AddGitRef(ctx, a.ID, task.ID, "feature/signup-fix", "abc1234", "https://github.com/acme/app/pull/42", []string{"src/auth/login.go", "src/auth/token.go"}, "Ready for review")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddArtifact(ctx, a.ID, task.ID, "raw_file", "Bad", "/tmp/secret.log", "", nil); err == nil || errorCode(err) != "validation" {
		t.Fatalf("expected invalid artifact kind validation, got %v", err)
	}
	if _, err := s.AddGitRef(ctx, a.ID, task.ID, "", "", "", nil, "empty"); err == nil || errorCode(err) != "validation" {
		t.Fatalf("expected empty git metadata validation, got %v", err)
	}
	detail, err := s.TaskDetail(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Artifacts) != 1 || detail.Artifacts[0].ID != artifact.ID || detail.Artifacts[0].Metadata["provider"] != "github" {
		t.Fatalf("expected artifact in detail, got %+v", detail.Artifacts)
	}
	if len(detail.GitRefs) != 1 || detail.GitRefs[0].ID != gitRef.ID || len(detail.GitRefs[0].ChangedFiles) != 2 {
		t.Fatalf("expected git metadata in detail, got %+v", detail.GitRefs)
	}
	events, err := s.ListEvents(ctx, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundArtifact := false
	foundGit := false
	for _, e := range events {
		if e.EventType == "artifact.referenced" {
			foundArtifact = true
		}
		if e.EventType == "git.metadata_attached" {
			foundGit = true
		}
	}
	if !foundArtifact || !foundGit {
		t.Fatalf("expected artifact/git audit events, got %+v", events)
	}
}

func TestContextSnapshotsAndHandoffPacket(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, err := s.RegisterActor(ctx, "Agent A", "agent", "mac")
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Memory Task", Goal: "Keep reliable handoff memory", Scope: []string{"README.md"}, Requirements: []string{"Document architecture"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "summary", "Added architecture overview"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "decision", "Keep TaskPilot vendor-neutral"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "risk", "Build target may be stale"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDecision(ctx, a.ID, task.ID, "Use context files for injection", nil, "Agents may not reach localhost", "Reliable handoff across runtimes"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.CreateContextSnapshot(ctx, a.ID, task.ID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Markdown == "" || len(snapshot.Summary.KeyDecisions) == 0 || len(snapshot.SourceContextIDs) != 3 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	packet, err := s.GenerateHandoffPacket(ctx, a.ID, task.ID, "", "ready")
	if err != nil {
		t.Fatal(err)
	}
	if packet.Markdown == "" || packet.Packet.TaskObjective != "Keep reliable handoff memory" || len(packet.Packet.ImportantDecisions) == 0 {
		t.Fatalf("unexpected handoff packet: %+v", packet)
	}
	updated, err := s.UpdateHandoffPacketMarkdown(ctx, a.ID, packet.ID, "# Task Handoff\n\n## Objective\nEdited objective\n\n## Current Status\nready\n\n## Current State\nImplementation is ready for continuation.\n\n## Completed Work\n- Added architecture overview\n\n## Important Decisions\n- Keep TaskPilot vendor-neutral\n\n## Remaining Work\n- Review final wording\n\n## Suggested Next Steps\n- Continue from edited packet\n\n## Handoff Message\nContinue from the edited packet.\n")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Packet.TaskObjective != "Edited objective" || len(updated.Packet.SuggestedNextSteps) != 1 {
		t.Fatalf("expected markdown edit to update JSON, got %+v", updated.Packet)
	}
	detail, err := s.TaskDetail(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.LatestSnapshot == nil || detail.HandoffPacket == nil {
		t.Fatalf("expected task detail memory fields, got snapshot=%v packet=%v", detail.LatestSnapshot, detail.HandoffPacket)
	}
}

func TestMarkdownValidationAndPublishHandoff(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := testActor(t, s, "Agent A")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Handoff Task", Goal: "Prepare clean handoff"})
	if err != nil {
		t.Fatal(err)
	}
	packet, err := s.GenerateHandoffPacket(ctx, a.ID, task.ID, "", "draft")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateHandoffPacketMarkdown(ctx, a.ID, packet.ID, "# Wrong\n\n## Objective\nBad\n"); err == nil {
		t.Fatal("expected markdown heading validation error")
	}
	edited, err := s.UpdateHandoffPacketMarkdown(ctx, a.ID, packet.ID, "# Task Handoff\n\n## Objective\nEdited objective\n\n## Current Status\nclaimed\n\n## Current State\nThe task is claimed and ready for handoff.\n\n## Completed Work\n- Prepared the clean handoff draft\n\n## Important Decisions\n- No material decision made; work followed existing requirements.\n\n## Remaining Work\n- Continue implementation safely\n\n## Suggested Next Steps\n- Continue safely\n\n## Handoff Message\nReady for the next agent.\n")
	if err != nil {
		t.Fatal(err)
	}
	if edited.Packet.TaskObjective != "Edited objective" || edited.Version != packet.Version+1 {
		t.Fatalf("expected markdown edit to sync JSON and increment version, got %+v", edited)
	}
	published, err := s.PublishHandoffPacket(ctx, a.ID, edited.ID)
	if err != nil {
		t.Fatal(err)
	}
	if published.Status != "published" || published.HandoffID == "" {
		t.Fatalf("expected published packet with linked handoff, got %+v", published)
	}
	updated, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "handoff_ready" {
		t.Fatalf("expected handoff_ready task, got %+v", updated)
	}
}

func TestNextContextFeedsSnapshotsAndHandoffSteps(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := testActor(t, s, "Agent A")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Next Context", Goal: "Preserve next steps"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "next", "Add invited-user regression test"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.CreateContextSnapshot(ctx, a.ID, task.ID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Summary.NextRecommendedActions) != 1 || snapshot.Summary.NextRecommendedActions[0] != "Add invited-user regression test" {
		t.Fatalf("expected next context in snapshot actions, got %+v", snapshot.Summary.NextRecommendedActions)
	}
	packet, err := s.GenerateHandoffPacket(ctx, a.ID, task.ID, "", "draft")
	if err != nil {
		t.Fatal(err)
	}
	if len(packet.Packet.SuggestedNextSteps) != 1 || packet.Packet.SuggestedNextSteps[0] != "Add invited-user regression test" {
		t.Fatalf("expected next context in handoff suggested steps, got %+v", packet.Packet.SuggestedNextSteps)
	}
}

func TestHandoffPacketSeparatesTimelineFromCurrentNextSteps(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := testActor(t, s, "Agent A")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Multi Handoff", Goal: "Continue across agents"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "summary", "taskpilot run started agent command: gemini"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "summary", "Completed initial README review"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "decision", "Keep the architecture overview vendor-neutral"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareHandoff(ctx, a.ID, task.ID, "", "First handoff after README review", []string{"Old next step should stay historical"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "summary", "Completed execution plan outline"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "next", "Current context next step"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "blocker", "taskpilot run command failed: exit status 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "output_ref", "Touched files detected by git status after taskpilot run:\nAlready changed before or still changed after run:\n- README.md\n- cli/root.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareHandoff(ctx, a.ID, task.ID, "", "Second handoff after execution plan", []string{"Current next step only"}); err != nil {
		t.Fatal(err)
	}
	packet, err := s.GenerateHandoffPacket(ctx, a.ID, task.ID, "", "draft")
	if err != nil {
		t.Fatal(err)
	}
	if len(packet.Packet.HandoffTimeline) != 2 {
		t.Fatalf("expected two handoff timeline entries, got %+v", packet.Packet.HandoffTimeline)
	}
	timeline := strings.Join(packet.Packet.HandoffTimeline, "\n")
	if !strings.Contains(timeline, "Handoff 1") || !strings.Contains(timeline, "First handoff") || !strings.Contains(timeline, "Handoff 2") || !strings.Contains(timeline, "Second handoff") {
		t.Fatalf("timeline missing handoff chronology:\n%s", timeline)
	}
	if contains(strings.Join(packet.Packet.CompletedWork, "\n"), "taskpilot run started agent command") {
		t.Fatalf("expected noisy run-start context filtered from completed work: %+v", packet.Packet.CompletedWork)
	}
	if !contains(strings.Join(packet.Packet.SuggestedNextSteps, "\n"), "Current next step only") {
		t.Fatalf("expected latest handoff next step in current suggestions, got %+v", packet.Packet.SuggestedNextSteps)
	}
	if contains(strings.Join(packet.Packet.SuggestedNextSteps, "\n"), "Old next step should stay historical") {
		t.Fatalf("old handoff next step should not remain current, got %+v", packet.Packet.SuggestedNextSteps)
	}
	if contains(strings.Join(packet.Packet.FilesComponentsAffected, "\n"), "README.md") || contains(strings.Join(packet.Packet.FilesComponentsAffected, "\n"), "cli/root.go") {
		t.Fatalf("pre-existing dirty files should not be treated as affected files, got %+v", packet.Packet.FilesComponentsAffected)
	}
	if len(packet.Packet.FailedSessions) != 1 || !contains(packet.Packet.FailedSessions[0], "exit status 1") {
		t.Fatalf("expected failed run context in failed sessions, got %+v", packet.Packet.FailedSessions)
	}
}

func TestPublishHandoffToleratesWrappedFilesAndEmptyNextSteps(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := testActor(t, s, "Agent A")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Execution doc", Goal: "Prepare execution plan"})
	if err != nil {
		t.Fatal(err)
	}
	packet, err := s.GenerateHandoffPacket(ctx, a.ID, task.ID, "", "draft")
	if err != nil {
		t.Fatal(err)
	}
	markdown := `# Task Handoff

## Objective
Prepare execution plan

## Current Status
claimed

## Current State
The execution plan has been prepared and needs review.

## Completed Work
- Created EXECUTION_PLAN.md

## Important Decisions
- No material decision made; work followed existing requirements.

## Files / Components Affected
- Touched files detected by git status after taskpilot run:
Newly changed during run:
EXECUTION_PLAN.md
Already changed before or still changed after run:
README.md

## Suggested Next Steps
- None recorded.

## Remaining Work
- Review the execution plan.

## Handoff Message
Execution plan draft is ready for review.
`
	edited, err := s.UpdateHandoffPacketMarkdown(ctx, a.ID, packet.ID, markdown)
	if err != nil {
		t.Fatal(err)
	}
	if len(edited.Packet.FilesComponentsAffected) != 1 || !strings.Contains(edited.Packet.FilesComponentsAffected[0], "EXECUTION_PLAN.md") {
		t.Fatalf("expected wrapped file section to stay as one useful item, got %+v", edited.Packet.FilesComponentsAffected)
	}
	if _, err := s.PublishHandoffPacket(ctx, a.ID, edited.ID); err == nil {
		t.Fatal("expected publish to require useful next steps")
	}
	edited, err = s.UpdateHandoffPacketMarkdown(ctx, a.ID, packet.ID, strings.Replace(markdown, "- None recorded.", "- Verify the execution plan against implementation phases.", 1))
	if err != nil {
		t.Fatal(err)
	}
	published, err := s.PublishHandoffPacket(ctx, a.ID, edited.ID)
	if err != nil {
		t.Fatal(err)
	}
	if published.Status != "published" || published.HandoffID == "" {
		t.Fatalf("expected published packet, got %+v", published)
	}
	if len(published.Packet.SuggestedNextSteps) == 0 {
		t.Fatalf("expected fallback suggested next step, got %+v", published.Packet)
	}
}

func TestAgentAuthoredHandoffDraftValidationAndPublish(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := testActor(t, s, "Agent A")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Planning", Goal: "Create robust planning doc"})
	if err != nil {
		t.Fatal(err)
	}
	packet, err := s.GenerateHandoffPacket(ctx, a.ID, task.ID, "", "draft")
	if err != nil {
		t.Fatal(err)
	}
	incomplete := `# Task Handoff

## Objective
Create robust planning doc

## Current Status
in_progress

## Current State
Planning document was explored.
`
	draft, err := s.UpdateHandoffPacketMarkdownWithSource(ctx, a.ID, packet.ID, incomplete, "agent_authored")
	if err != nil {
		t.Fatal(err)
	}
	if draft.Source != "agent_authored" || len(draft.ValidationErrors) == 0 {
		t.Fatalf("expected incomplete agent-authored draft with validation errors, got %+v", draft)
	}
	if _, err := s.PublishHandoffPacket(ctx, a.ID, draft.ID); err == nil {
		t.Fatal("expected publish to fail for incomplete agent-authored handoff")
	}
	complete := `# Task Handoff

## Objective
Create robust planning doc

## Current Status
in_progress

## Current State
PLANNING.md now contains the implementation phases and needs review.

## Completed Work
- Created PLANNING.md with a brief but robust implementation plan.

## Important Decisions
- Kept the plan brief and phase-oriented so another agent can execute it quickly.

## Remaining Work
- Review wording and align it with final delivery phases.

## Suggested Next Steps
- Review PLANNING.md and mark the task ready for review if it matches the goal.

## Handoff Message
Planning draft is ready for the next agent to review and tighten.
`
	draft, err = s.UpdateHandoffPacketMarkdownWithSource(ctx, a.ID, packet.ID, complete, "agent_authored")
	if err != nil {
		t.Fatal(err)
	}
	if len(draft.ValidationErrors) != 0 {
		t.Fatalf("expected complete draft without validation errors, got %+v", draft.ValidationErrors)
	}
	published, err := s.PublishHandoffPacket(ctx, a.ID, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if published.Source != "agent_authored" || published.Status != "published" {
		t.Fatalf("expected published agent-authored handoff, got %+v", published)
	}
	if !strings.Contains(strings.Join(published.Packet.CompletedWork, "\n"), "PLANNING.md") {
		t.Fatalf("expected agent-authored completed work to survive publish, got %+v", published.Packet.CompletedWork)
	}
}

func TestAgentAuthoredPlaceholderHandoffMergesRunEvidence(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := testActor(t, s, "Agent A")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Snake planning", Goal: "make a planning.md for the snake game with some new logic"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "summary", "Updated task files: PLANNING.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "decision", "Use a phase-based snake game plan so implementation can proceed incrementally."); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "output_ref", "Files changed during this run:\n- PLANNING.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "next", "Review PLANNING.md and start the first implementation phase."); err != nil {
		t.Fatal(err)
	}
	packet, err := s.GenerateHandoffPacket(ctx, a.ID, task.ID, "", "draft")
	if err != nil {
		t.Fatal(err)
	}
	placeholder := `# Task Handoff

## Objective
make a planning.md for the snake game with some new logic

## Current Status
in_progress

## Current State
Task is in_progress; continue from the latest task memory and verify the current repository state.

## Completed Work
- Replace this with concrete work completed during this session.

## Important Decisions
- Replace this with decisions made and why, or write: No material decision made; work followed existing requirements.

## Remaining Work
- Replace this with remaining work, or state that no known work remains.

## Suggested Next Steps
- Continue from the latest task context and verify completion criteria.

## Handoff Message
Write a concise message for the next agent before stopping.
`
	draft, err := s.UpdateHandoffPacketMarkdownWithSource(ctx, a.ID, packet.ID, placeholder, "agent_authored")
	if err != nil {
		t.Fatal(err)
	}
	if contains(strings.Join(draft.Packet.CompletedWork, "\n"), "Replace this") {
		t.Fatalf("placeholder completed work should be replaced, got %+v", draft.Packet.CompletedWork)
	}
	if !contains(strings.Join(draft.Packet.CompletedWork, "\n"), "PLANNING.md") {
		t.Fatalf("expected completed work to include run evidence, got %+v", draft.Packet.CompletedWork)
	}
	if !contains(strings.Join(draft.Packet.ImportantDecisions, "\n"), "phase-based snake game plan") {
		t.Fatalf("expected decision evidence to replace placeholder, got %+v", draft.Packet.ImportantDecisions)
	}
	if !contains(strings.Join(draft.Packet.SuggestedNextSteps, "\n"), "Review PLANNING.md") {
		t.Fatalf("expected specific next step, got %+v", draft.Packet.SuggestedNextSteps)
	}
	if len(draft.ValidationErrors) != 0 {
		t.Fatalf("expected merged draft to be publishable, got %+v", draft.ValidationErrors)
	}
}

func TestHandoffCheckpointsPreserveHistoryAndLatestNextSteps(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := testActor(t, s, "Agent A")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Snake planning", Goal: "Plan snake game logic"})
	if err != nil {
		t.Fatal(err)
	}
	packet, err := s.GenerateHandoffPacket(ctx, a.ID, task.ID, "", "draft")
	if err != nil {
		t.Fatal(err)
	}
	first := `# Task Handoff

## Objective
Plan snake game logic

## Current Status
in_progress

## Current State
Created initial planning structure.

## Completed Work
- Created PLANNING.md outline.

## Important Decisions
- Use phased implementation so the next agent can build incrementally.

## Remaining Work
- Add gameplay rules.

## Suggested Next Steps
- Add gameplay rules section.

## Handoff Message
Initial planning outline is ready.
`
	if _, err := s.CreateHandoffCheckpoint(ctx, a.ID, task.ID, packet.ID, "session-1", first); err != nil {
		t.Fatal(err)
	}
	second := `# Task Handoff

## Objective
Plan snake game logic

## Current Status
in_progress

## Current State
Gameplay rules and scoring notes are now included.

## Completed Work
- Added gameplay rules and scoring notes.

## Important Decisions
- Keep power-ups optional for phase two to protect MVP scope.

## Remaining Work
- Review PLANNING.md for clarity.

## Suggested Next Steps
- Review PLANNING.md and start implementation.

## Handoff Message
Planning is ready for review.
`
	if _, err := s.CreateHandoffCheckpoint(ctx, a.ID, task.ID, packet.ID, "session-1", second); err != nil {
		t.Fatal(err)
	}
	latest, err := s.LatestHandoffPacket(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil {
		t.Fatal("expected latest handoff packet")
	}
	completed := strings.Join(latest.Packet.CompletedWork, "\n")
	if !contains(completed, "Created PLANNING.md outline") || !contains(completed, "Added gameplay rules") {
		t.Fatalf("expected completed work from both checkpoints, got %+v", latest.Packet.CompletedWork)
	}
	decisions := strings.Join(latest.Packet.ImportantDecisions, "\n")
	if !contains(decisions, "phased implementation") || !contains(decisions, "power-ups optional") {
		t.Fatalf("expected decisions from both checkpoints, got %+v", latest.Packet.ImportantDecisions)
	}
	next := strings.Join(latest.Packet.SuggestedNextSteps, "\n")
	if !contains(next, "start implementation") || contains(next, "Add gameplay rules section") {
		t.Fatalf("expected only latest next step as current, got %+v", latest.Packet.SuggestedNextSteps)
	}
	timeline := strings.Join(latest.Packet.HandoffTimeline, "\n")
	if !contains(timeline, "Checkpoint 1") || !contains(timeline, "Checkpoint 2") || !contains(timeline, "Add gameplay rules section") {
		t.Fatalf("expected chronological checkpoint timeline with old next step, got %s", timeline)
	}
	checkpoints, err := s.ListHandoffCheckpoints(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != 2 || checkpoints[0].Sequence != 1 || checkpoints[1].Sequence != 2 {
		t.Fatalf("expected two sequenced checkpoints, got %+v", checkpoints)
	}
}

func TestCheckpointFallbackDoesNotPolluteAgentHandoffWithContextFragments(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := testActor(t, s, "Agent A")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Snake planning", Goal: "Plan a snake game"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "note", "0`."); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "note", "d a game implementation task separately; this task is complete as a documentation-only deliverable."); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "note", "Verification: Confirmed the workspace was empty before creating the document."); err != nil {
		t.Fatal(err)
	}
	packet, err := s.GenerateHandoffPacket(ctx, a.ID, task.ID, "", "draft")
	if err != nil {
		t.Fatal(err)
	}
	markdown := `# TaskPilot Handoff

## Current State
Planning doc is complete.

## Completed Work
- Created planning.md with:
  - technology section
  - validation checklist

## Important Decisions
- No material decision made; work followed existing requirements.

## Remaining Work
- None for this task.

## Suggested Next Steps
- Start implementation separately if needed.

## Handoff Message
Planning is ready for the next agent.
`
	if _, err := s.CreateHandoffCheckpoint(ctx, a.ID, task.ID, packet.ID, "session-1", markdown); err != nil {
		t.Fatal(err)
	}
	latest, err := s.LatestHandoffPacket(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(latest.Packet.ImplementationNotes, "\n")
	if contains(notes, "0`.") || contains(notes, "d a game") {
		t.Fatalf("fragmented context notes should not pollute checkpoint handoff: %+v", latest.Packet.ImplementationNotes)
	}
	completed := strings.Join(latest.Packet.CompletedWork, "\n")
	if !contains(completed, "technology section") || len(latest.Packet.CompletedWork) != 1 {
		t.Fatalf("nested completed-work bullets should be preserved under the parent item, got %+v", latest.Packet.CompletedWork)
	}
}

func TestDuplicateHandoffCheckpointReturnsExistingCheckpoint(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := testActor(t, s, "Agent A")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Snake planning", Goal: "Plan a snake game"})
	if err != nil {
		t.Fatal(err)
	}
	packet, err := s.GenerateHandoffPacket(ctx, a.ID, task.ID, "", "draft")
	if err != nil {
		t.Fatal(err)
	}
	markdown := `# Task Handoff

## Current State
Planning doc is complete.

## Completed Work
- Created planning.md.

## Important Decisions
- No material decision made; work followed existing requirements.

## Remaining Work
- None for this task.

## Suggested Next Steps
- Start implementation separately if needed.

## Handoff Message
Planning is ready.
`
	first, err := s.CreateHandoffCheckpoint(ctx, a.ID, task.ID, packet.ID, "session-1", markdown)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateHandoffCheckpoint(ctx, a.ID, task.ID, packet.ID, "session-1", markdown)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Sequence != first.Sequence {
		t.Fatalf("duplicate checkpoint should return existing row, first=%+v second=%+v", first, second)
	}
	checkpoints, err := s.ListHandoffCheckpoints(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != 1 {
		t.Fatalf("expected one checkpoint after duplicate submit, got %+v", checkpoints)
	}
}

func TestTaskSessionLifecycleReturnsToClaimed(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testActor(t, s, "Agent A")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Long Session", Goal: "Run agent", Scope: []string{"README.md"}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.StartTaskSession(ctx, a.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	started, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if started.Status != "in_progress" {
		t.Fatalf("expected in_progress while session runs, got %+v", started)
	}
	finished, err := s.FinishTaskSession(ctx, a.ID, task.ID, session.ID, "success", "agent exited")
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != "claimed" || finished.OwnerID != a.ID {
		t.Fatalf("expected session exit to return to claimed owner, got %+v", finished)
	}
	locks, err := s.ListLocks(ctx, task.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) == 0 || locks[0].OwnerID != a.ID || locks[0].Status != "active" {
		t.Fatalf("expected owned lock to remain active, got %+v", locks)
	}
}

func TestCompletedTasksAreFilteredFromOpenConflicts(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testActor(t, s, "Agent A")
	b := testActor(t, s, "Agent B")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Conflict Done", Goal: "Finish conflict"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(ctx, a.ID, task.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(ctx, b.ID, task.ID, "", false); err == nil {
		t.Fatal("expected claim conflict")
	}
	if _, err := s.CompleteTask(ctx, a.ID, task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	open, err := s.ListConflicts(ctx, "open")
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("expected completed task conflicts hidden from open list, got %+v", open)
	}
}

func TestStaleClaimDetails(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testActor(t, s, "Agent A")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Stale Task", Goal: "Show stale details"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(ctx, a.ID, task.ID, "", false); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-DefaultClaimTTL * 2)
	_, err = s.exec(ctx, `UPDATE tasks SET claim_expires_at=?, last_heartbeat_at=? WHERE id=?`, ts(past), ts(past), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := s.ListStaleClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].Task.ID != task.ID || stale[0].Owner == nil || stale[0].Reason == "" || len(stale[0].SuggestedActions) == 0 {
		t.Fatalf("expected detailed stale claim, got %+v", stale)
	}
}

func TestPostgresSQLRewrite(t *testing.T) {
	s := &Store{dialect: "postgres"}
	got := s.sql(`INSERT OR IGNORE INTO projects (id,name) VALUES (?,?)`)
	want := `INSERT INTO projects (id,name) VALUES ($1,$2) ON CONFLICT DO NOTHING`
	if got != want {
		t.Fatalf("postgres insert rewrite:\n got %s\nwant %s", got, want)
	}
	got = s.sql(`CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY AUTOINCREMENT, payload_json TEXT NOT NULL)`)
	if !strings.Contains(got, "id BIGSERIAL PRIMARY KEY") {
		t.Fatalf("expected BIGSERIAL rewrite, got %s", got)
	}
	got = s.sql(`SELECT '?' AS literal, id FROM tasks WHERE id=? AND title=?`)
	if got != `SELECT '?' AS literal, id FROM tasks WHERE id=$1 AND title=$2` {
		t.Fatalf("placeholder rewrite should skip quoted question marks, got %s", got)
	}
	got = s.sql(`ALTER TABLE tasks ADD COLUMN repo_id TEXT`)
	if got != `ALTER TABLE tasks ADD COLUMN IF NOT EXISTS repo_id TEXT` {
		t.Fatalf("alter add column rewrite should be idempotent, got %s", got)
	}
}

func TestPostgresStoreIntegration(t *testing.T) {
	dsn := os.Getenv("TASKPILOT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TASKPILOT_TEST_POSTGRES_DSN to run Postgres store integration test")
	}
	ctx := context.Background()
	s, err := OpenStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	a := testActor(t, s, "Postgres Agent")
	task, err := s.CreateTask(ctx, a.ID, TaskInput{Title: "Postgres Task", Goal: "Verify store workflow"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(ctx, a.ID, task.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcquireLock(ctx, a.ID, task.ID, "src/postgres/*", "file_glob"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendContext(ctx, a.ID, task.ID, "decision", "Postgres store path works"); err != nil {
		t.Fatal(err)
	}
	detail, err := s.TaskDetail(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.ID != task.ID || len(detail.Context) != 1 || len(detail.Locks) != 1 {
		t.Fatalf("expected postgres task detail with context and lock, got %+v", detail)
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
