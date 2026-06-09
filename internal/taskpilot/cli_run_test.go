package taskpilot

import (
	"encoding/json"
	"os"
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
	for _, want := range []string{"read_task", "claim_task", "heartbeat_task", "append_context", "complete_task"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("tools/list missing %s: %s", want, raw)
		}
	}
}
