package taskpilot

import (
	"encoding/json"
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
	before := map[string]bool{"auth/old.go": true}
	after := map[string]bool{"auth/old.go": true, "auth/new.go": true}
	summary := touchedFilesSummary(before, after)
	for _, want := range []string{"Touched files detected", "Newly changed during run:", "- auth/new.go", "Already changed before", "- auth/old.go"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
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
