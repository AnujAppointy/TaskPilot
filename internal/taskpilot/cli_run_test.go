package taskpilot

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseRunContextLine(t *testing.T) {
	tests := []struct {
		line    string
		kind    string
		content string
		ok      bool
	}{
		{`decision: Keep token format unchanged`, "decision", "Keep token format unchanged", true},
		{`progress: Still adding tests`, "note", "Still adding tests", true},
		{`finding: Expiry check fails after invite lookup`, "summary", "Finding: Expiry check fails after invite lookup", true},
		{`rationale: DB schema already has enough state`, "note", "Rationale: DB schema already has enough state", true},
		{`rejected: Adding a new invite token table would duplicate state`, "decision", "Rejected approach: Adding a new invite token table would duplicate state", true},
		{`files: src/auth/invite.go`, "output_ref", "src/auth/invite.go", true},
		{`verification: go test ./src/auth passed`, "note", "Verification: go test ./src/auth passed", true},
		{`next: Add used-token regression test`, "next", "Add used-token regression test", true},
		{`{"kind":"risk","content":"Expiry logic has timezone edge cases"}`, "risk", "Expiry logic has timezone edge cases", true},
		{`plain note`, "note", "plain note", true},
		{`   `, "", "", false},
	}
	for _, tt := range tests {
		got, ok := parseRunContextLine(tt.line)
		if ok != tt.ok {
			t.Fatalf("parseRunContextLine(%q) ok=%v want %v", tt.line, ok, tt.ok)
		}
		if !ok {
			continue
		}
		if got.Kind != tt.kind || got.Content != tt.content {
			t.Fatalf("parseRunContextLine(%q)=%+v want kind=%s content=%s", tt.line, got, tt.kind, tt.content)
		}
	}
}

func TestReadNewRunContextEntriesSurvivesRewrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.log")
	seen := map[string]bool{}
	if err := os.WriteFile(path, []byte("summary: Created planning.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := readNewRunContextEntries(path, seen)
	if len(first) != 1 || first[0].Kind != "summary" || first[0].Content != "Created planning.md" {
		t.Fatalf("first import = %+v", first)
	}
	if err := os.WriteFile(path, []byte("summary: Created planning.md\nsummary: Added technology section\nfiles: planning.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := readNewRunContextEntries(path, seen)
	if len(second) != 2 {
		t.Fatalf("second import should only include new complete lines, got %+v", second)
	}
	if second[0].Content != "Added technology section" || second[1].Kind != "output_ref" || second[1].Content != "planning.md" {
		t.Fatalf("unexpected second import: %+v", second)
	}
	third := readNewRunContextEntries(path, seen)
	if len(third) != 0 {
		t.Fatalf("third import should dedupe already imported lines, got %+v", third)
	}
}

func TestRunSyncIntervalCapsLongProgressInterval(t *testing.T) {
	if got := runSyncInterval(5 * time.Minute); got != 2*time.Second {
		t.Fatalf("runSyncInterval long duration = %v", got)
	}
	if got := runSyncInterval(500 * time.Millisecond); got != 500*time.Millisecond {
		t.Fatalf("runSyncInterval short duration = %v", got)
	}
}

func TestTaskPilotLiveSectionIsReplacedNotAppended(t *testing.T) {
	first := upsertTaskPilotLiveSection("# Repo\n", "first context")
	second := upsertTaskPilotLiveSection(first, "second context")
	if strings.Count(second, liveContextStart) != 1 || strings.Count(second, liveContextEnd) != 1 {
		t.Fatalf("expected exactly one managed section, got:\n%s", second)
	}
	if strings.Contains(second, "first context") || !strings.Contains(second, "second context") {
		t.Fatalf("managed section was not replaced correctly:\n%s", second)
	}
}

func TestMergeSessionStartHookJSONBytesAppendsAndPreservesExistingConfig(t *testing.T) {
	original := []byte(`{
  "theme": "dark",
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "echo existing"
          }
        ]
      }
    ]
  }
}`)
	updated, changed, err := mergeSessionStartHookJSONBytes(original, "hooks.SessionStart", SessionStartHook{Type: "command", Command: ".taskpilot/hooks/claude-session-start.sh"})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatalf("expected hook append to change config")
	}
	text := string(updated)
	if !strings.Contains(text, `"theme": "dark"`) || !strings.Contains(text, "echo existing") || !strings.Contains(text, ".taskpilot/hooks/claude-session-start.sh") {
		t.Fatalf("updated config lost existing content or hook:\n%s", text)
	}
}

func TestMergeSessionStartHookJSONBytesIsIdempotentByCommand(t *testing.T) {
	original := []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":".taskpilot/hooks/codex-session-start.sh"}]}]}}`)
	updated, changed, err := mergeSessionStartHookJSONBytes(original, "hooks.SessionStart", SessionStartHook{Type: "command", Command: ".taskpilot/hooks/codex-session-start.sh"})
	if err != nil {
		t.Fatal(err)
	}
	if changed || string(updated) != string(original) {
		t.Fatalf("already registered hook should be no-op, changed=%v updated=%s", changed, updated)
	}
}

func TestMergeSessionStartHookJSONBytesCreatesMissingConfig(t *testing.T) {
	updated, changed, err := mergeSessionStartHookJSONBytes([]byte(`{}`), "hooks.SessionStart", SessionStartHook{Type: "command", Command: ".taskpilot/hooks/gemini-session-start.sh"})
	if err != nil {
		t.Fatal(err)
	}
	if !changed || !strings.Contains(string(updated), ".taskpilot/hooks/gemini-session-start.sh") {
		t.Fatalf("expected missing config to be created, changed=%v updated=%s", changed, updated)
	}
}

func TestMergeSessionStartHookJSONBytesRejectsMalformedJSON(t *testing.T) {
	if _, _, err := mergeSessionStartHookJSONBytes([]byte(`{"hooks":`), "hooks.SessionStart", SessionStartHook{Type: "command", Command: "x"}); err == nil {
		t.Fatalf("expected malformed JSON error")
	}
}

func TestMergeSessionStartHookJSONCreatesBackupAndSkipsNoopWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"existing"}]}]}}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := mergeSessionStartHookJSON(path, "hooks.SessionStart", SessionStartHook{Type: "command", Command: ".taskpilot/hooks/claude-session-start.sh"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || first.BackupPath == "" {
		t.Fatalf("expected changed write with backup, got %+v", first)
	}
	if _, err := os.Stat(first.BackupPath); err != nil {
		t.Fatalf("expected backup file: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	second, err := mergeSessionStartHookJSON(path, "hooks.SessionStart", SessionStartHook{Type: "command", Command: ".taskpilot/hooks/claude-session-start.sh"}, false)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed {
		t.Fatalf("expected second merge to be no-op, got %+v", second)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("no-op merge should not rewrite file")
	}
}

func TestBranchMatchesTaskUsesMeaningfulWords(t *testing.T) {
	task := Task{Title: "Fix Stripe retry bug", Goal: "Resolve duplicate retry scheduling"}
	if !branchMatchesTask("fix-stripe-retry", task) {
		t.Fatalf("expected branch to match task title")
	}
	if branchMatchesTask("docs-readme", task) {
		t.Fatalf("unrelated branch should not match task")
	}
}

func TestGitChangedFileSnapshotIgnoresTaskPilotMetadata(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	runGitTestCommand(t, root, "init")
	runGitTestCommand(t, root, "config", "user.email", "taskpilot@example.com")
	runGitTestCommand(t, root, "config", "user.name", "TaskPilot")
	if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, root, "add", "app.go")
	runGitTestCommand(t, root, "commit", "-m", "init")
	if err := os.MkdirAll(filepath.Join(root, ".taskpilot"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".taskpilot", "repo.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n// changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := gitChangedFileSnapshotIn(root)
	if _, ok := got[".taskpilot/repo.json"]; ok {
		t.Fatalf("TaskPilot metadata should not be tracked as product work: %+v", got)
	}
	if _, ok := got["app.go"]; !ok {
		t.Fatalf("expected product file to be detected: %+v", got)
	}
}

func TestLoadRepoConfigOverridesCommittedGitRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	runGitTestCommand(t, root, "init")
	if err := os.MkdirAll(filepath.Join(root, ".taskpilot"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := repoEnableConfig{Version: 1, GitRoot: "/Users/appointy/Desktop/testing", HookCommand: `taskpilot context render --repo "/Users/appointy/Desktop/testing" --format markdown`}
	data, _ := json.Marshal(stale)
	if err := os.WriteFile(filepath.Join(root, ".taskpilot", "repo.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadRepoConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitRoot != root {
		t.Fatalf("expected local git root %q, got %q", root, cfg.GitRoot)
	}
	if strings.Contains(cfg.HookCommand, "/Users/appointy") {
		t.Fatalf("hook command should be portable, got %q", cfg.HookCommand)
	}
}

func TestWriteHookScriptsAreRepoRelative(t *testing.T) {
	root := t.TempDir()
	cfg := repoEnableConfig{GitRoot: root}
	if err := writeHookScripts(cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".taskpilot", "hooks", "codex-session-start.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), root) {
		t.Fatalf("hook script should not hard-code local root:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(root, ".taskpilot", "hooks", "codex-session-start.cmd")); err != nil {
		t.Fatalf("expected Windows hook script too: %v", err)
	}
}

func runGitTestCommand(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func TestInjectAgentStartupPromptCombinesHumanPrompt(t *testing.T) {
	prompt := agentLaunchPrompt("task_1", `C:\Temp\taskpilot-prompt.txt`)
	got := injectAgentStartupPrompt([]string{"codex", "add planning section"}, prompt)
	if len(got) != 2 {
		t.Fatalf("expected two args, got %+v", got)
	}
	if !strings.Contains(got[1], `C:\Temp\taskpilot-prompt.txt`) || !strings.Contains(got[1], "Human prompt for this work unit:") || !strings.Contains(got[1], "add planning section") {
		t.Fatalf("prompt was not combined correctly: %q", got[1])
	}
	if strings.Contains(got[1], "\n") {
		t.Fatalf("launch prompt should stay single-line for Windows argv safety: %q", got[1])
	}
}

func TestInjectAgentStartupPromptSupportsClaude(t *testing.T) {
	prompt := agentLaunchPrompt("task_1", "/tmp/taskpilot-prompt.txt")
	got := injectAgentStartupPrompt([]string{"claude", "review README"}, prompt)
	if len(got) != 2 {
		t.Fatalf("expected two args, got %+v", got)
	}
	if !strings.Contains(got[1], "/tmp/taskpilot-prompt.txt") || !strings.Contains(got[1], "review README") {
		t.Fatalf("claude prompt was not combined correctly: %+v", got)
	}
}

func TestInjectAgentStartupPromptSupportsPiAndOpenCode(t *testing.T) {
	prompt := agentLaunchPrompt("task_1", "/tmp/taskpilot-prompt.txt")
	for _, agent := range []string{"pi", "opencode"} {
		got := injectAgentStartupPrompt([]string{agent, "review README"}, prompt)
		if len(got) != 2 {
			t.Fatalf("%s: expected two args, got %+v", agent, got)
		}
		if !strings.Contains(got[1], "/tmp/taskpilot-prompt.txt") || !strings.Contains(got[1], "review README") {
			t.Fatalf("%s: prompt was not combined correctly: %+v", agent, got)
		}
	}
}

func TestInjectAgentStartupPromptPreservesModelFlagAndCombinesLastPrompt(t *testing.T) {
	prompt := agentLaunchPrompt("task_1", "/tmp/taskpilot-prompt.txt")
	got := injectAgentStartupPrompt([]string{"codex", "--model", "gpt-5", "review README"}, prompt)
	if len(got) != 4 {
		t.Fatalf("expected four args, got %+v", got)
	}
	if got[2] != "gpt-5" {
		t.Fatalf("model flag value should be preserved, got %+v", got)
	}
	if !strings.Contains(got[3], "/tmp/taskpilot-prompt.txt") || !strings.Contains(got[3], "review README") {
		t.Fatalf("last prompt arg was not combined: %+v", got)
	}
}

func TestInjectAgentStartupPromptAppendsWhenOnlyFlagsExist(t *testing.T) {
	prompt := agentLaunchPrompt("task_1", "/tmp/taskpilot-prompt.txt")
	got := injectAgentStartupPrompt([]string{"codex", "--model", "gpt-5"}, prompt)
	if len(got) != 4 || got[1] != "--model" || got[2] != "gpt-5" || got[3] != prompt {
		t.Fatalf("expected prompt appended after flag-only command, got %+v", got)
	}
}

func TestAgentLaunchPromptPointsToFullPromptFile(t *testing.T) {
	got := agentLaunchPrompt("task_123", `C:\Users\hp\AppData\Local\Temp\taskpilot-prompt.txt`)
	for _, want := range []string{"task_123", `C:\Users\hp\AppData\Local\Temp\taskpilot-prompt.txt`, "read the full TaskPilot instructions", "Do not infer"} {
		if !strings.Contains(got, want) {
			t.Fatalf("launch prompt missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("launch prompt should be single-line, got %q", got)
	}
}

func TestRelatedTaskCandidatesIncludeSameProjectPriorContextFallback(t *testing.T) {
	now := time.Date(2026, 6, 4, 6, 0, 0, 0, time.UTC)
	current := TaskDetail{Task: Task{ID: "task_current", ProjectID: "project_1", Title: "game.md", Goal: "Create game overview", UpdatedAt: now}}
	tasks := []Task{
		{
			ID:         "task_prior",
			ProjectID:  "project_1",
			Title:      "planning.md",
			Goal:       "Plan the same game",
			Status:     "completed",
			UpdatedAt:  now.Add(-1 * time.Hour),
			SearchText: "Created planning.md with game rules and implementation notes.",
		},
	}

	got := relatedTaskCandidates(current, tasks, now)
	if len(got) != 1 {
		t.Fatalf("expected same-project prior context fallback, got %+v", got)
	}
	if got[0].Task.ID != "task_prior" {
		t.Fatalf("expected task_prior, got %+v", got[0])
	}
	reasons := strings.Join(got[0].Reasons, "\n")
	for _, want := range []string{"same-project prior context", "has recorded task context"} {
		if !strings.Contains(reasons, want) {
			t.Fatalf("fallback reasons missing %q: %+v", want, got[0].Reasons)
		}
	}
}

func TestRelatedTaskCandidatesIgnoreSameProjectTasksWithoutMemorySignals(t *testing.T) {
	now := time.Date(2026, 6, 4, 6, 0, 0, 0, time.UTC)
	current := TaskDetail{Task: Task{ID: "task_current", ProjectID: "project_1", Title: "game.md", Goal: "Create game overview", UpdatedAt: now}}
	tasks := []Task{
		{
			ID:        "task_empty",
			ProjectID: "project_1",
			Title:     "unstarted idea",
			Goal:      "No recorded work yet",
			Status:    "ready",
			UpdatedAt: now.Add(-1 * time.Hour),
		},
	}

	if got := relatedTaskCandidates(current, tasks, now); len(got) != 0 {
		t.Fatalf("expected empty same-project task without memory to be ignored, got %+v", got)
	}
}

func TestSummarizeRelatedTaskIncludesLatestHandoffMarkdown(t *testing.T) {
	now := time.Date(2026, 6, 4, 6, 0, 0, 0, time.UTC)
	detail := TaskDetail{
		Task: Task{ID: "task_prior", ProjectID: "project_1", Title: "game.md", Goal: "Create game overview", Status: "completed", UpdatedAt: now},
		HandoffPacket: &HandoffPacket{
			ID: "packet_1",
			Packet: HandoffPacketContent{
				HandoffMessage: "game.md is complete and ready for related tasks.",
			},
			Markdown: "# Old Handoff\n\nOlder packet markdown.",
		},
		HandoffCheckpoints: []HandoffCheckpoint{
			{ID: "checkpoint_1", Sequence: 1, Markdown: "# Task Handoff\n\n## Completed Work\n- Created game.md"},
			{ID: "checkpoint_2", Sequence: 2, Markdown: "# Task Handoff\n\n## Completed Work\n- Added colors section"},
		},
	}

	got := summarizeRelatedTask(detail, nil, []string{"same-project prior context"})
	if got.HandoffSummary != "game.md is complete and ready for related tasks." {
		t.Fatalf("expected handoff packet message as summary, got %q", got.HandoffSummary)
	}
	if got.HandoffSource != "handoff_checkpoint:checkpoint_2" {
		t.Fatalf("expected latest checkpoint source, got %q", got.HandoffSource)
	}
	if !strings.Contains(got.HandoffMarkdown, "Added colors section") || strings.Contains(got.HandoffMarkdown, "Created game.md") {
		t.Fatalf("expected latest checkpoint markdown only, got %q", got.HandoffMarkdown)
	}
}

func TestParseHandoffMarkdownAcceptsLenientAgentDraft(t *testing.T) {
	markdown := `# Completed Work
- Created D:\SnakeGame\planning.md.

# Important Decisions
- No material decision made; work followed existing requirements.

# Current State
- planning.md is present.

# Remaining Work
- No remaining work unless revisions are requested.

# Suggested Next Steps
- Review planning.md if more detail is needed.

# Verification
- Read back planning.md.

# Files/Artifacts
- D:\SnakeGame\planning.md

# Handoff Message
- planning.md has been created and verified.
`
	content, err := parseHandoffMarkdownStrict(markdown, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(content.CompletedWork, "\n"), "planning.md") {
		t.Fatalf("expected completed work from lenient draft, got %+v", content.CompletedWork)
	}
	if !strings.Contains(strings.Join(content.ImplementationNotes, "\n"), "Read back planning.md") {
		t.Fatalf("expected verification alias under implementation notes, got %+v", content.ImplementationNotes)
	}
	if !strings.Contains(strings.Join(content.FilesComponentsAffected, "\n"), `D:\SnakeGame\planning.md`) {
		t.Fatalf("expected files alias under affected files, got %+v", content.FilesComponentsAffected)
	}
}

func TestAPIErrorMessageIncludesMarkdownValidationDetails(t *testing.T) {
	got := apiErrorMessage(APIError{
		Message: "markdown validation failed",
		Errors: []MarkdownValidationError{
			{Line: 1, Message: "missing top-level heading '# Task Handoff'"},
			{Section: "Objective", Message: "required section is empty"},
		},
	})
	for _, want := range []string{"markdown validation failed", "line 1", "Objective", "required section is empty"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected API error message to include %q, got %q", want, got)
		}
	}
}

func TestTouchedFilesSummary(t *testing.T) {
	before := map[string]gitFileState{"auth/old.go": {Status: "M", ModTime: 1, Size: 10}, "planning.md": {Status: "M", ModTime: 1, Size: 20}}
	after := map[string]gitFileState{"auth/old.go": {Status: "M", ModTime: 1, Size: 10}, "auth/new.go": {Status: "??", ModTime: 2, Size: 10}, "planning.md": {Status: "M", ModTime: 3, Size: 25}}
	summary, warning, changed := touchedFilesSummary(before, after)
	for _, want := range []string{"Files changed during this run:", "- auth/new.go"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
	if !strings.Contains(summary, "- planning.md") {
		t.Fatalf("summary should include pre-existing dirty file modified during run:\n%s", summary)
	}
	for _, want := range []string{"Pre-existing dirty worktree files", "- auth/old.go"} {
		if !strings.Contains(warning, want) {
			t.Fatalf("warning missing %q:\n%s", want, warning)
		}
	}
	if len(changed) != 2 {
		t.Fatalf("expected two changed files, got %+v", changed)
	}
}

func TestWorkspaceFileSnapshotDetectsNonGitFileChanges(t *testing.T) {
	dir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWD) }()
	before := workspaceFileSnapshot()
	if err := os.WriteFile(filepath.Join(dir, "PLANNING.md"), []byte("snake plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after := workspaceFileSnapshot()
	summary, _, changed := touchedFilesSummary(before, after)
	if !strings.Contains(summary, "PLANNING.md") || len(changed) != 1 || changed[0] != "PLANNING.md" {
		t.Fatalf("expected non-git workspace snapshot to detect PLANNING.md, summary=%q changed=%+v", summary, changed)
	}
}

func TestAgentHandoffTemplateRequiresRealAgentEdits(t *testing.T) {
	detail := TaskDetail{Task: Task{ID: "task_1", Goal: "Create PLANNING.md", Status: "in_progress"}}
	packet := HandoffPacket{Packet: HandoffPacketContent{TaskObjective: "Create PLANNING.md", CurrentStatus: "in_progress"}}
	markdown := agentHandoffTemplate("task_1", detail, packet)
	content, err := parseHandoffMarkdownStrict(markdown, false)
	if err != nil {
		t.Fatal(err)
	}
	errs := validateHandoffQuality(content)
	if len(errs) == 0 {
		t.Fatalf("expected placeholder handoff template to require real agent edits:\n%s", markdown)
	}
	if strings.Contains(markdown, "TaskPilot") || strings.Contains(markdown, "Be concise by default") || strings.Contains(markdown, "Preserve important long context") {
		t.Fatal("handoff template should not include TaskPilot mechanics or writing rules")
	}
	if !strings.Contains(agentStartupPrompt("task_1", "task.json", "related.json", "context.log", "handoff.md"), "Preserve important long context") || !strings.Contains(agentInstructions("task_1"), "Be concise by default") {
		t.Fatal("startup instructions should include handoff writing quality rules")
	}
	if !strings.Contains(agentStartupPrompt("task_1", "task.json", "related.json", "context.log", "handoff.md"), "handoff checkpoint") || !strings.Contains(agentInstructions("task_1"), "taskpilot handoff checkpoint") {
		t.Fatal("startup instructions should tell the agent to maintain the handoff file")
	}
}

func TestRunHandoffAttentionWarnsForPlaceholderDraft(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handoff.md")
	markdown := agentHandoffTemplate("task_1", TaskDetail{Task: Task{ID: "task_1", Goal: "Create PLANNING.md", Status: "in_progress"}}, HandoffPacket{})
	if err := os.WriteFile(path, []byte(markdown), 0o600); err != nil {
		t.Fatal(err)
	}
	tracker := &runHandoffTracker{}
	tracker.record(true, nil)
	if !warnIfRunHandoffNeedsAttention("task_1", path, tracker) {
		t.Fatal("expected placeholder handoff draft to need attention")
	}
}

func TestRunHandoffAttentionAcceptsValidCheckpointedDraft(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handoff.md")
	markdown := `# Task Handoff

## Objective
Create PLANNING.md

## Current Status
in_progress

## Current State
Planning document has been written and reviewed.

## Completed Work
- Created PLANNING.md with gameplay phases and validation checklist.

## Important Decisions
- No material decision made; work followed existing requirements.

## Remaining Work
- No known work remains for this documentation task.

## Suggested Next Steps
- Review PLANNING.md and create a separate implementation task if approved.

## Handoff Message
Planning document is ready for review.
`
	if err := os.WriteFile(path, []byte(markdown), 0o600); err != nil {
		t.Fatal(err)
	}
	tracker := &runHandoffTracker{}
	tracker.record(true, nil)
	if warnIfRunHandoffNeedsAttention("task_1", path, tracker) {
		t.Fatal("valid checkpointed handoff draft should not need attention")
	}
}

func TestParseHandoffAcceptsTaskPilotHandoffHeading(t *testing.T) {
	markdown := `# TaskPilot Handoff

## Current State
Planning doc is updated.

## Completed Work
- Added technology section.

## Important Decisions
- No material decision made; work followed existing requirements.

## Remaining Work
- None for this task.

## Suggested Next Steps
- Start a separate implementation task if needed.

## Handoff Message
Ready for the next agent.
`
	content, err := parseHandoffMarkdownStrict(markdown, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(content.CompletedWork) != 1 || !strings.Contains(content.CompletedWork[0], "technology") {
		t.Fatalf("unexpected parsed handoff content: %+v", content)
	}
}

func TestParseHandoffKeepsIndentedBulletsWithParentItem(t *testing.T) {
	markdown := `# Task Handoff

## Completed Work
- Created planning.md with:
  - game logic section
  - technology section

## Important Decisions
- No material decision made; work followed existing requirements.

## Current State
Planning doc is complete.

## Remaining Work
- None for this task.

## Suggested Next Steps
- Start implementation separately if needed.

## Handoff Message
Ready for the next agent.
`
	content, err := parseHandoffMarkdownStrict(markdown, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(content.CompletedWork) != 1 {
		t.Fatalf("nested bullets should stay with the parent work item, got %+v", content.CompletedWork)
	}
	if !strings.Contains(content.CompletedWork[0], "technology section") {
		t.Fatalf("nested bullet detail was lost: %+v", content.CompletedWork)
	}
}

func TestMCPInitializeAndToolsList(t *testing.T) {
	initResult, err := handleMCPRequest(mcpRequest{Method: "initialize"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(initResult)
	if !strings.Contains(string(raw), "taskpilot") {
		t.Fatalf("initialize result missing server info: %s", raw)
	}
	toolsResult, err := handleMCPRequest(mcpRequest{Method: "tools/list"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(toolsResult)
	for _, want := range []string{
		"create_project",
		"list_projects",
		"create_repository",
		"list_repositories",
		"create_workspace",
		"list_workspaces",
		"list_actors",
		"search_tasks",
		"list_my_tasks",
		"list_blocked_tasks",
		"list_active_locks",
		"list_conflicts",
		"read_task",
		"read_task_memory",
		"task_context_bundle",
		"ask_taskpilot",
		"create_task",
		"create_subtask",
		"add_dependency",
		"remove_dependency",
		"update_task",
		"append_task_fields",
		"update_task_status",
		"delete_task",
		"claim_task",
		"heartbeat_task",
		"release_task",
		"start_task_session",
		"finish_task_session",
		"append_context",
		"add_decision",
		"add_comment",
		"add_artifact",
		"add_git_ref",
		"create_context_snapshot",
		"acquire_lock",
		"check_scope_conflicts",
		"release_lock",
		"renew_lock",
		"override_lock",
		"prepare_handoff",
		"generate_handoff_packet",
		"read_latest_handoff",
		"publish_handoff",
		"accept_handoff",
		"reject_handoff",
		"list_handoffs",
		"checkpoint_handoff",
		"list_task_events",
		"list_recent_events",
		"find_related_tasks",
		"summarize_task",
		"summarize_project",
		"find_decisions",
		"find_blockers",
		"find_outputs",
		"complete_task",
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("tools/list missing %s: %s", want, raw)
		}
	}
}

func TestMCPTaskInputAcceptsStructuredTaskFields(t *testing.T) {
	in, err := mcpTaskInput(map[string]any{
		"title":               "Fix invited-user signup",
		"goal":                "Invited users can complete signup",
		"type":                "debugging",
		"priority":            "high",
		"project_id":          "project_1",
		"scope":               []any{"src/auth/*", "src/invites/*"},
		"requirements":        []any{"Keep old invite links working"},
		"completion_criteria": []any{"Regression tests pass"},
		"risks":               []any{"Timezone edge cases"},
		"blockers":            []any{"Need sample expired invite"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if in.Title != "Fix invited-user signup" || in.ProjectID != "project_1" || in.Type != "debugging" || in.Priority != "high" {
		t.Fatalf("unexpected task input basics: %+v", in)
	}
	if len(in.Scope) != 2 || len(in.Requirements) != 1 || len(in.CompletionCriteria) != 1 || len(in.Risks) != 1 || len(in.Blockers) != 1 {
		t.Fatalf("structured task fields were not preserved: %+v", in)
	}
}

func TestMCPFilterTasksMatchesQueryAndExcludesCompletedByDefault(t *testing.T) {
	tasks := []Task{
		{ID: "task_1", Title: "Fix invited-user signup", Goal: "Patch invite expiry comparison", Status: "ready", Priority: "high", Scope: []string{"src/auth/*"}, SearchText: "Decision: keep token format unchanged"},
		{ID: "task_2", Title: "Completed invite cleanup", Goal: "Old work", Status: "completed", Priority: "low", SearchText: "invite expiry"},
		{ID: "task_3", Title: "Billing report", Goal: "Export CSV", Status: "ready", Priority: "medium"},
	}

	got := filterMCPTasks(tasks, mcpTaskFilter{Query: "invite expiry", Limit: 10})
	if len(got) != 1 || got[0].ID != "task_1" {
		t.Fatalf("expected only active invite task, got %+v", got)
	}

	got = filterMCPTasks(tasks, mcpTaskFilter{Query: "invite expiry", IncludeCompleted: true, Limit: 10})
	if len(got) != 2 {
		t.Fatalf("expected active and completed invite tasks, got %+v", got)
	}
}

func TestMCPEvidenceFromDetailFindsDecisionContextArtifactAndLock(t *testing.T) {
	now := time.Date(2026, 6, 10, 8, 0, 0, 0, time.UTC)
	detail := TaskDetail{
		Task: Task{ID: "task_1", Title: "Fix invited-user signup", Goal: "Keep existing invite links working", Status: "in_progress"},
		Context: []ContextEntry{
			{ID: "ctx_1", TaskID: "task_1", Kind: "summary", Content: "Found invite expiry comparison bug", CreatedAt: now},
		},
		Decisions: []DecisionRecord{
			{ID: "dec_1", TaskID: "task_1", Decision: "Keep invite token format unchanged", Reason: "Existing invite links must keep working", CreatedAt: now},
		},
		Artifacts: []Artifact{
			{ID: "art_1", TaskID: "task_1", Kind: "pr", Title: "Invite signup fix", URI: "https://example.test/pr/1"},
		},
		Locks: []Lock{
			{ID: "lock_1", TaskID: "task_1", Scope: "src/auth/invite.go", ScopeType: "file", Status: "active", OwnerID: "actor_1"},
		},
	}

	evidence := mcpEvidenceFromDetail(detail, "invite", 10)
	raw, _ := json.Marshal(evidence)
	for _, want := range []string{"decision", "context", "artifact", "lock"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("expected evidence to include %s: %s", want, raw)
		}
	}
}
