package taskpilot

import (
	"context"
	"os"
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
