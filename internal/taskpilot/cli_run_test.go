package taskpilot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if !strings.Contains(agentStartupPrompt("task_1", "task.json", "related.json", "context.log", "handoff.md"), "handoff checkpoint") || !strings.Contains(agentInstructions("task_1"), "taskpilot handoff checkpoint") {
		t.Fatal("startup instructions should tell the agent to maintain the handoff file")
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
