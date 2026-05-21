package taskpilot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	Server      string `json:"server"`
	Token       string `json:"token"`
	APIKey      string `json:"api_key,omitempty"`
	ActorID     string `json:"actor_id"`
	ActorSecret string `json:"actor_secret"`
}

func Run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		addr := fs.String("addr", "127.0.0.1:8080", "listen address")
		db := fs.String("db", "taskpilot.db", "SQLite database path")
		token := fs.String("token", getenv("TASKPILOT_TOKEN", "dev-token"), "team token")
		production := fs.Bool("production", false, "enforce production safety checks")
		_ = fs.Parse(args[1:])
		return ListenAndServeConfig(LoadServerConfig(*addr, *db, *token, *production))
	case "login":
		return runLogin(args[1:])
	case "run":
		return runAgentCommand(args[1:])
	case "agent":
		return runAgent(args[1:])
	case "mcp":
		return runMCP(args[1:])
	case "project":
		return runProject(args[1:])
	case "repo":
		return runRepo(args[1:])
	case "workspace":
		return runWorkspace(args[1:])
	case "api-key":
		return runAPIKey(args[1:])
	case "git":
		return runGit(args[1:])
	case "artifact":
		return runArtifact(args[1:])
	case "migrate":
		return runMigrate(args[1:])
	case "admin":
		return runAdmin(args[1:])
	case "backup":
		return runBackup(args[1:])
	case "config":
		return runConfig(args[1:])
	case "actor":
		return runActor(args[1:])
	case "task":
		return runTask(args[1:])
	case "context":
		return runContext(args[1:])
	case "decision":
		return runDecision(args[1:])
	case "comment":
		return runComment(args[1:])
	case "lock":
		return runLock(args[1:])
	case "handoff":
		return runHandoff(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Print(`TaskPilot

Server:
  taskpilot serve --addr 0.0.0.0:8080 --db taskpilot.db --token dev-token

Config:
  taskpilot login --server http://127.0.0.1:8080 --token dev-token
  taskpilot login --server http://127.0.0.1:8080 --api-key tpk_...
  taskpilot config set-server http://127.0.0.1:8080
  taskpilot config set-token dev-token
  taskpilot config set-api-key tpk_...
  taskpilot config set-actor actor_... <actor-secret>

Production auth bootstrap:
  taskpilot admin create-user --email admin@example.com --name Admin --password "change-me-strong" --role admin
  taskpilot admin create-actor --name "Codex Agent" --kind agent --machine anuj-mac
 taskpilot admin create-api-key --name "Lead key" --actor <actor-id> --role admin --scope admin
  taskpilot project create --name "Appointy Backend"
  taskpilot repo create --project <project-id> --name appointy-api --path /path/to/repo
  taskpilot workspace create --project <project-id> --name "Anuj Mac" --actor <actor-id>

Agent CLI:
 taskpilot actor register --name "Codex on Anuj Mac" --kind agent --machine anuj-mac
  taskpilot task create --title "Fix signup bug" --goal "Resolve invited-user signup failure" --scope "src/auth/*" --project <project-id>
  taskpilot task list
 taskpilot task show <task-id>
  taskpilot task subtask <task-id> --title "Write tests" --goal "Add regression coverage"
  taskpilot task depend <task-id> --on <dependency-task-id>
  taskpilot task claim <task-id>
  taskpilot lock acquire <task-id> --scope "src/auth/*"
  taskpilot context append <task-id> --kind decision --content "Keep token format unchanged"
  taskpilot decision add <task-id> --decision "Keep token format unchanged" --reason "Existing links depend on it"
  taskpilot comment add <task-id> --body "Please review edge cases before merge"
  taskpilot artifact add <task-id> --kind pr --title "Signup fix PR" --uri https://github.com/org/repo/pull/42
  taskpilot git link-branch <task-id>
  taskpilot git attach-pr <task-id> https://github.com/org/repo/pull/42
  taskpilot handoff prepare <task-id> --summary "Ready for next agent" --next "Write test" --next "Patch logic"

Automation:
  taskpilot run <task-id> -- <agent-command> [args...]
  taskpilot agent init
  taskpilot mcp serve
 taskpilot migrate status
  taskpilot backup create --out taskpilot-backup.db
`)
}

func runScaffold(domain string, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: taskpilot %s <command>", domain)
	}
	return fmt.Errorf("%s %s is scaffolded for the production milestone and is not active in this binary yet", domain, args[1])
}

func runAgentCommand(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("usage: taskpilot run <task-id> [--progress-interval 5s] [--no-complete] [--handoff-on-failure] [--handoff-to actor-id] [--summary text] -- <agent-command> [args...]")
	}
	taskID := args[0]
	sep := -1
	for i, arg := range args {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || sep == len(args)-1 {
		return fmt.Errorf("usage: taskpilot run <task-id> [options] -- <agent-command> [args...]")
	}
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	progressEvery := fs.Duration("progress-interval", progressInterval(), "sync run context to the server while the child command runs")
	noComplete := fs.Bool("no-complete", false, "leave task in progress instead of completing after successful command")
	handoffOnFailure := fs.Bool("handoff-on-failure", true, "prepare a handoff packet if the child command fails")
	handoffTo := fs.String("handoff-to", "", "target actor for failure handoff")
	summaryFlag := fs.String("summary", "", "completion summary override")
	noPromptInject := fs.Bool("no-prompt-inject", false, "do not pass TaskPilot startup prompt to known agent commands")
	_ = fs.Parse(args[1:sep])
	commandArgs := args[sep+1:]
	if len(commandArgs) == 0 {
		return fmt.Errorf("usage: taskpilot run <task-id> [options] -- <agent-command> [args...]")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if cfg.Server == "" {
		cfg.Server = "http://127.0.0.1:8080"
	}
	var detail TaskDetail
	if err := request("GET", "/api/tasks/"+taskID, nil, &detail); err != nil {
		return err
	}
	beforeFiles := gitChangedFiles()
	contextPath, cleanup, err := createRunContextFile(taskID)
	if err != nil {
		return err
	}
	defer cleanup()
	var contextOffset int64
	_ = appendRunContext(taskID, "summary", "taskpilot run started agent command: "+strings.Join(commandArgs, " "))
	if detail.Task.OwnerID == "" || detail.Task.OwnerID != cfg.ActorID {
		var claimed Task
		if err := request("POST", "/api/tasks/"+taskID+"/claim", map[string]any{"reason": "taskpilot run"}, &claimed); err != nil {
			return err
		}
	}
	for _, scope := range detail.Task.Scope {
		var lock Lock
		_ = request("POST", "/api/tasks/"+taskID+"/locks", map[string]any{"scope": scope, "scope_type": "file_glob"}, &lock)
	}
	if err := request("GET", "/api/tasks/"+taskID, nil, &detail); err != nil {
		return err
	}
	taskContextPath, relatedContextPath, contextCleanup, err := createAgentContextFiles(taskID, detail)
	if err != nil {
		return err
	}
	defer contextCleanup()
	startupPrompt := agentStartupPrompt(taskID, taskContextPath, relatedContextPath, contextPath)
	promptPath, promptCleanup, err := createTextTemp("taskpilot-"+taskID+"-prompt-*.txt", startupPrompt)
	if err != nil {
		return err
	}
	defer promptCleanup()
	if !*noPromptInject {
		commandArgs = injectAgentStartupPrompt(commandArgs, startupPrompt)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	done := make(chan struct{})
	var contextMu sync.Mutex
	go heartbeatLoop(ctx, taskID, done)
	go progressLoop(ctx, taskID, contextPath, &contextOffset, *progressEvery, done, &contextMu)
	cmd := exec.CommandContext(ctx, commandArgs[0], commandArgs[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(),
		"TASKPILOT_TASK_ID="+taskID,
		"TASKPILOT_SERVER="+cfg.Server,
		"TASKPILOT_ACTOR_ID="+cfg.ActorID,
		"TASKPILOT_PROJECT_ID="+detail.Task.ProjectID,
		"TASKPILOT_REPO_ID="+detail.Task.RepoID,
		"TASKPILOT_WORKSPACE_ID="+detail.Task.WorkspaceID,
		"TASKPILOT_RUN_CONTEXT_FILE="+contextPath,
		"TASKPILOT_TASK_CONTEXT_FILE="+taskContextPath,
		"TASKPILOT_RELATED_CONTEXT_FILE="+relatedContextPath,
		"TASKPILOT_AGENT_PROMPT_FILE="+promptPath,
		"TASKPILOT_AGENT_INSTRUCTIONS="+agentInstructions(taskID),
	)
	err = cmd.Run()
	close(done)
	contextMu.Lock()
	imported := importRunContextSince(taskID, contextPath, &contextOffset)
	contextMu.Unlock()
	changed := touchedFilesSummary(beforeFiles, gitChangedFiles())
	if changed != "" {
		_ = appendRunContext(taskID, "output_ref", changed)
	}
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "agent command exited with error: %v\n", err)
		_ = appendRunContext(taskID, "blocker", "taskpilot run command failed: "+err.Error())
		if *handoffOnFailure {
			_, _ = prepareRunHandoff(taskID, *handoffTo, err.Error(), changed, imported)
		}
		var status Task
		_ = request("PATCH", "/api/tasks/"+taskID, map[string]any{"status": "blocked", "reason": "agent command failed"}, &status)
		_, _ = fmt.Fprintf(os.Stderr, "Task marked blocked. Add context or prepare handoff with taskpilot context/handoff commands.\n")
		return err
	}
	summary := strings.TrimSpace(*summaryFlag)
	if summary == "" {
		summary = strings.TrimSpace(os.Getenv("TASKPILOT_RUN_SUMMARY"))
	}
	if summary == "" {
		summary = "Agent command completed successfully through taskpilot run."
	}
	if *noComplete {
		_ = appendRunContext(taskID, "summary", summary)
		var inProgress Task
		_ = request("PATCH", "/api/tasks/"+taskID, map[string]any{"status": "in_progress", "reason": "taskpilot run finished with --no-complete"}, &inProgress)
		return print(inProgress, false)
	}
	var completed Task
	if err := request("POST", "/api/tasks/"+taskID+"/complete", map[string]any{"summary": summary}, &completed); err != nil {
		return err
	}
	return print(completed, false)
}

func appendRunContext(taskID, kind, content string) error {
	var out ContextEntry
	return request("POST", "/api/tasks/"+taskID+"/context", map[string]any{"kind": kind, "content": content}, &out)
}

func prepareRunHandoff(taskID, to, errText, changed string, imported int) (Handoff, error) {
	summary := "Agent command failed during taskpilot run: " + errText
	next := []string{"Review blocker context and command failure.", "Resume from latest task context."}
	if changed != "" {
		next = append(next, "Inspect touched files listed in the latest output_ref context.")
	}
	if imported > 0 {
		next = append(next, "Review imported run context entries before continuing.")
	}
	var out Handoff
	err := request("POST", "/api/tasks/"+taskID+"/handoff", map[string]any{"to_actor_id": to, "summary": summary, "next_steps": next}, &out)
	return out, err
}

func heartbeatLoop(ctx context.Context, taskID string, done <-chan struct{}) {
	ticker := time.NewTicker(heartbeatInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			var out Task
			_ = request("POST", "/api/tasks/"+taskID+"/heartbeat", map[string]any{}, &out)
		}
	}
}

func progressLoop(ctx context.Context, taskID, contextPath string, contextOffset *int64, interval time.Duration, done <-chan struct{}, mu *sync.Mutex) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			mu.Lock()
			importRunContextSince(taskID, contextPath, contextOffset)
			mu.Unlock()
		}
	}
}

func createRunContextFile(taskID string) (string, func(), error) {
	f, err := os.CreateTemp("", "taskpilot-"+taskID+"-context-*.log")
	if err != nil {
		return "", nil, err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		return "", nil, err
	}
	return path, func() { _ = os.Remove(path) }, nil
}

type agentTaskContextFile struct {
	GeneratedAt time.Time       `json:"generated_at"`
	Usage       string          `json:"usage"`
	CurrentTask agentTaskDetail `json:"current_task"`
}

type agentRelatedContextFile struct {
	GeneratedAt   time.Time             `json:"generated_at"`
	Usage         string                `json:"usage"`
	SelectionRule string                `json:"selection_rule"`
	RelatedTasks  []agentRelatedContext `json:"related_tasks"`
}

type agentTaskDetail struct {
	Task         Task             `json:"task"`
	Owner        *Actor           `json:"owner,omitempty"`
	Parent       *Task            `json:"parent,omitempty"`
	Subtasks     []Task           `json:"subtasks,omitempty"`
	Dependencies []TaskDependency `json:"dependencies,omitempty"`
	Dependents   []TaskDependency `json:"dependents,omitempty"`
	Context      []ContextEntry   `json:"context,omitempty"`
	Decisions    []DecisionRecord `json:"decisions,omitempty"`
	Comments     []Comment        `json:"comments,omitempty"`
	Artifacts    []Artifact       `json:"artifacts,omitempty"`
	GitRefs      []GitRef         `json:"git_refs,omitempty"`
	Locks        []Lock           `json:"locks,omitempty"`
	Handoffs     []Handoff        `json:"handoffs,omitempty"`
}

type agentRelatedContext struct {
	ID               string           `json:"id"`
	Title            string           `json:"title"`
	Goal             string           `json:"goal,omitempty"`
	Type             string           `json:"type"`
	Status           string           `json:"status"`
	Priority         string           `json:"priority"`
	OwnerID          string           `json:"owner_id,omitempty"`
	UpdatedAt        time.Time        `json:"updated_at"`
	Scope            []string         `json:"scope,omitempty"`
	Relation         []string         `json:"relation,omitempty"`
	RelevanceReasons []string         `json:"relevance_reasons,omitempty"`
	Summaries        []string         `json:"summaries,omitempty"`
	Decisions        []DecisionRecord `json:"decisions,omitempty"`
	Risks            []string         `json:"risks,omitempty"`
	Blockers         []string         `json:"blockers,omitempty"`
	Outputs          []string         `json:"outputs,omitempty"`
	Artifacts        []Artifact       `json:"artifacts,omitempty"`
	GitRefs          []GitRef         `json:"git_refs,omitempty"`
	HandoffSummary   string           `json:"handoff_summary,omitempty"`
}

func createAgentContextFiles(taskID string, detail TaskDetail) (string, string, func(), error) {
	taskSnapshot := agentTaskContextFile{
		GeneratedAt: time.Now().UTC(),
		Usage:       "Read this first. It is the authoritative TaskPilot snapshot for the current task. Prefer it over live CLI calls from inside sandboxed agents.",
		CurrentTask: compactAgentTaskDetail(detail),
	}
	relatedSnapshot := agentRelatedContextFile{
		GeneratedAt:   time.Now().UTC(),
		Usage:         "Use this as prior work context. These tasks were selected because they are linked or relevant to the current task; unrelated tasks are intentionally omitted.",
		SelectionRule: "Includes directly linked tasks plus up to five same-project tasks with overlapping scope/repo/parent signals. Related tasks contain summaries, decisions, risks, blockers, outputs, artifacts, and git refs, not full event history.",
		RelatedTasks:  collectRelatedAgentContexts(detail),
	}
	taskPath, taskCleanup, err := createJSONTemp("taskpilot-"+taskID+"-task-*.json", taskSnapshot)
	if err != nil {
		return "", "", nil, err
	}
	relatedPath, relatedCleanup, err := createJSONTemp("taskpilot-"+taskID+"-related-*.json", relatedSnapshot)
	if err != nil {
		taskCleanup()
		return "", "", nil, err
	}
	return taskPath, relatedPath, func() {
		taskCleanup()
		relatedCleanup()
	}, nil
}

func createJSONTemp(pattern string, v any) (string, func(), error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", nil, err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", nil, err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", nil, err
	}
	return path, func() { _ = os.Remove(path) }, nil
}

func createTextTemp(pattern, content string) (string, func(), error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", nil, err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", nil, err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", nil, err
	}
	return path, func() { _ = os.Remove(path) }, nil
}

func injectAgentStartupPrompt(commandArgs []string, prompt string) []string {
	if len(commandArgs) == 0 {
		return commandArgs
	}
	name := strings.ToLower(filepath.Base(commandArgs[0]))
	switch name {
	case "codex", "gemini":
		if len(commandArgs) == 1 {
			return append(commandArgs, prompt)
		}
	}
	return commandArgs
}

func agentStartupPrompt(taskID, taskContextPath, relatedContextPath, runContextPath string) string {
	return `Work on the current TaskPilot task.

You are launched by taskpilot run. Do not infer the task from repo-local files or stale local databases.

Before doing any repository analysis or edits:
1. Read ` + taskContextPath + ` for the authoritative current task snapshot.
2. Read ` + relatedContextPath + ` for selected related/prior task context from the TaskPilot server.
3. Treat TASKPILOT_TASK_ID=` + taskID + ` as the only current task.
4. Ignore repo-local .taskpilot-data.db, old peer/daemon state, and guessed commands like ./bin/taskpilot task current unless the current task context explicitly asks for them.

While working:
- Follow the current task goal, scope, locks, blockers, decisions, and handoff state from the context files.
- Use related context only when it is relevant to the current task. Do not pull unrelated task history into the answer.
- If live taskpilot CLI/server access fails from inside the agent runtime, continue from the injected context files.
- Write useful updates immediately to ` + runContextPath + `.

Accepted update lines for ` + runContextPath + `:
- summary: what you found or completed
- decision: decision made and why
- risk: risk or caveat
- blocker: what blocks progress
- output_ref: changed files, PRs, docs, or other references
- next: next step for another agent or human

Do not upload or write secrets, raw private logs, customer data, private prompts, or raw local files into TaskPilot context.`
}

func compactAgentTaskDetail(detail TaskDetail) agentTaskDetail {
	return agentTaskDetail{
		Task:         detail.Task,
		Owner:        detail.Owner,
		Parent:       detail.Parent,
		Subtasks:     detail.Subtasks,
		Dependencies: detail.Dependencies,
		Dependents:   detail.Dependents,
		Context:      compactContextEntries(detail.Context, 40),
		Decisions:    limitDecisions(detail.Decisions, 20),
		Comments:     limitComments(detail.Comments, 20),
		Artifacts:    limitArtifacts(detail.Artifacts, 20),
		GitRefs:      limitGitRefs(detail.GitRefs, 20),
		Locks:        detail.Locks,
		Handoffs:     detail.Handoffs,
	}
}

type relatedCandidate struct {
	Task      Task
	Score     int
	Reasons   []string
	Relations []string
}

func collectRelatedAgentContexts(current TaskDetail) []agentRelatedContext {
	var tasks []Task
	path := "/api/tasks"
	if current.Task.ProjectID != "" {
		path += "?project_id=" + current.Task.ProjectID
	}
	if err := request("GET", path, nil, &tasks); err != nil {
		return nil
	}
	linked := linkedTaskRelations(current)
	candidates := []relatedCandidate{}
	for _, task := range tasks {
		if task.ID == current.Task.ID {
			continue
		}
		score, reasons := relatedTaskScore(current.Task, task)
		relations := linked[task.ID]
		if len(relations) > 0 {
			score += 100
			reasons = append(reasons, "directly linked to current task")
		}
		if score < 50 {
			continue
		}
		candidates = append(candidates, relatedCandidate{Task: task, Score: score, Reasons: uniqueStrings(reasons), Relations: uniqueStrings(relations)})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score == candidates[j].Score {
			return candidates[i].Task.UpdatedAt.After(candidates[j].Task.UpdatedAt)
		}
		return candidates[i].Score > candidates[j].Score
	})
	if len(candidates) > 5 {
		candidates = candidates[:5]
	}
	out := []agentRelatedContext{}
	for _, candidate := range candidates {
		var detail TaskDetail
		if err := request("GET", "/api/tasks/"+candidate.Task.ID, nil, &detail); err != nil {
			continue
		}
		out = append(out, summarizeRelatedTask(detail, candidate.Relations, candidate.Reasons))
	}
	return out
}

func linkedTaskRelations(detail TaskDetail) map[string][]string {
	out := map[string][]string{}
	if detail.Parent != nil {
		out[detail.Parent.ID] = append(out[detail.Parent.ID], "parent")
	}
	for _, task := range detail.Subtasks {
		out[task.ID] = append(out[task.ID], "subtask")
	}
	for _, dep := range detail.Dependencies {
		if dep.DependsOnID != "" {
			out[dep.DependsOnID] = append(out[dep.DependsOnID], "blocked_by")
		}
	}
	for _, dep := range detail.Dependents {
		if dep.TaskID != "" {
			out[dep.TaskID] = append(out[dep.TaskID], "blocking")
		}
	}
	return out
}

func relatedTaskScore(current, candidate Task) (int, []string) {
	score := 0
	reasons := []string{}
	if current.ProjectID != "" && candidate.ProjectID == current.ProjectID {
		score += 5
	}
	if current.RepoID != "" && candidate.RepoID == current.RepoID {
		score += 15
		reasons = append(reasons, "same repository")
	}
	if current.ParentTaskID != "" && candidate.ParentTaskID == current.ParentTaskID {
		score += 20
		reasons = append(reasons, "same parent task")
	}
	if taskScopesOverlap(current.Scope, candidate.Scope) {
		score += 70
		reasons = append(reasons, "overlapping scope")
	}
	if candidate.Status == "completed" {
		score += 10
		reasons = append(reasons, "completed prior work")
	}
	if time.Since(candidate.UpdatedAt) <= 14*24*time.Hour {
		score += 10
		reasons = append(reasons, "recently updated")
	}
	return score, reasons
}

func taskScopesOverlap(a, b []string) bool {
	for _, left := range a {
		for _, right := range b {
			if scopeOverlaps(left, right) {
				return true
			}
		}
	}
	return false
}

func scopeOverlaps(a, b string) bool {
	a = normalizeScope(a)
	b = normalizeScope(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	ap := scopePrefix(a)
	bp := scopePrefix(b)
	return ap != "" && bp != "" && (strings.HasPrefix(ap, bp) || strings.HasPrefix(bp, ap))
}

func normalizeScope(scope string) string {
	scope = strings.TrimSpace(strings.ReplaceAll(scope, "\\", "/"))
	scope = strings.TrimPrefix(scope, "./")
	return scope
}

func scopePrefix(scope string) string {
	scope = normalizeScope(scope)
	scope = strings.TrimSuffix(scope, "*")
	scope = strings.TrimSuffix(scope, "/")
	if scope == "" {
		return ""
	}
	if strings.ContainsAny(scope, "*?[") {
		return strings.TrimRight(scope[:strings.IndexAny(scope, "*?[")], "/")
	}
	return scope
}

func summarizeRelatedTask(detail TaskDetail, relations, reasons []string) agentRelatedContext {
	summaries := []string{}
	risks := []string{}
	blockers := []string{}
	outputs := []string{}
	for _, entry := range compactContextEntries(detail.Context, 30) {
		switch entry.Kind {
		case "summary":
			summaries = append(summaries, entry.Content)
		case "decision":
			summaries = append(summaries, "Decision note: "+entry.Content)
		case "risk":
			risks = append(risks, entry.Content)
		case "blocker":
			blockers = append(blockers, entry.Content)
		case "output_ref":
			outputs = append(outputs, entry.Content)
		}
	}
	handoffSummary := ""
	if len(detail.Handoffs) > 0 {
		handoffSummary = detail.Handoffs[len(detail.Handoffs)-1].ResumeSummary
	}
	return agentRelatedContext{
		ID:               detail.Task.ID,
		Title:            detail.Task.Title,
		Goal:             detail.Task.Goal,
		Type:             detail.Task.Type,
		Status:           detail.Task.Status,
		Priority:         detail.Task.Priority,
		OwnerID:          detail.Task.OwnerID,
		UpdatedAt:        detail.Task.UpdatedAt,
		Scope:            detail.Task.Scope,
		Relation:         relations,
		RelevanceReasons: reasons,
		Summaries:        limitStrings(uniqueStrings(summaries), 8),
		Decisions:        limitDecisions(detail.Decisions, 8),
		Risks:            limitStrings(uniqueStrings(append(risks, detail.Task.Risks...)), 8),
		Blockers:         limitStrings(uniqueStrings(append(blockers, detail.Task.Blockers...)), 8),
		Outputs:          limitStrings(uniqueStrings(outputs), 8),
		Artifacts:        limitArtifacts(detail.Artifacts, 8),
		GitRefs:          limitGitRefs(detail.GitRefs, 8),
		HandoffSummary:   handoffSummary,
	}
}

func compactContextEntries(entries []ContextEntry, max int) []ContextEntry {
	out := []ContextEntry{}
	seen := map[string]bool{}
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if isNoisyRunContext(entry.Content) {
			continue
		}
		key := entry.Kind + "\x00" + strings.TrimSpace(entry.Content)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, entry)
		if len(out) >= max {
			break
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func isNoisyRunContext(content string) bool {
	return strings.Contains(strings.ToLower(content), "taskpilot run is still active; heartbeat renewed")
}

func uniqueStrings(values []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func limitStrings(values []string, max int) []string {
	if len(values) <= max {
		return values
	}
	return values[:max]
}

func limitDecisions(values []DecisionRecord, max int) []DecisionRecord {
	if len(values) <= max {
		return values
	}
	return values[len(values)-max:]
}

func limitComments(values []Comment, max int) []Comment {
	if len(values) <= max {
		return values
	}
	return values[len(values)-max:]
}

func limitArtifacts(values []Artifact, max int) []Artifact {
	if len(values) <= max {
		return values
	}
	return values[len(values)-max:]
}

func limitGitRefs(values []GitRef, max int) []GitRef {
	if len(values) <= max {
		return values
	}
	return values[len(values)-max:]
}

func importRunContextSince(taskID, path string, offset *int64) int {
	entries, next := readRunContextFileSince(path, *offset)
	*offset = next
	imported := 0
	for _, entry := range entries {
		if appendRunContext(taskID, entry.Kind, entry.Content) == nil {
			imported++
		}
	}
	return imported
}

type runContextEntry struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

func readRunContextFileSince(path string, offset int64) ([]runContextEntry, int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, offset
		}
	}
	out := []runContextEntry{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if entry, ok := parseRunContextLine(scanner.Text()); ok {
			out = append(out, entry)
		}
	}
	next, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		next = offset
	}
	return out, next
}

func parseRunContextLine(line string) (runContextEntry, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return runContextEntry{}, false
	}
	var entry runContextEntry
	if strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &entry) == nil {
		entry.Kind = normalizeContextKind(entry.Kind)
		entry.Content = strings.TrimSpace(entry.Content)
		return entry, entry.Content != ""
	}
	parts := strings.SplitN(line, ":", 2)
	if len(parts) == 2 {
		entry.Kind = normalizeContextKind(parts[0])
		entry.Content = strings.TrimSpace(parts[1])
		return entry, entry.Content != ""
	}
	return runContextEntry{Kind: "note", Content: line}, true
}

func normalizeContextKind(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case "summary", "decision", "note", "risk", "blocker", "output_ref":
		return kind
	case "output", "artifact", "file", "files":
		return "output_ref"
	case "progress":
		return "note"
	default:
		return "note"
	}
}

func gitChangedFiles() map[string]bool {
	out := map[string]bool{}
	cmd := exec.Command("git", "status", "--porcelain")
	data, err := cmd.Output()
	if err != nil {
		return out
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if strings.Contains(path, " -> ") {
			parts := strings.Split(path, " -> ")
			path = parts[len(parts)-1]
		}
		if path != "" {
			out[path] = true
		}
	}
	return out
}

func gitChangedFileList() []string {
	files := make([]string, 0, len(gitChangedFiles()))
	for path := range gitChangedFiles() {
		files = append(files, path)
	}
	sort.Strings(files)
	return files
}

func currentGitBranch() string {
	out, err := exec.Command("git", "branch", "--show-current").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func currentGitCommit() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func touchedFilesSummary(before, after map[string]bool) string {
	if len(after) == 0 {
		return ""
	}
	newOrChanged := []string{}
	existing := []string{}
	for path := range after {
		if before[path] {
			existing = append(existing, path)
		} else {
			newOrChanged = append(newOrChanged, path)
		}
	}
	sort.Strings(newOrChanged)
	sort.Strings(existing)
	if len(newOrChanged) == 0 && len(existing) == 0 {
		return ""
	}
	lines := []string{"Touched files detected by git status after taskpilot run:"}
	if len(newOrChanged) > 0 {
		lines = append(lines, "Newly changed during run:")
		for _, path := range newOrChanged {
			lines = append(lines, "- "+path)
		}
	}
	if len(existing) > 0 {
		lines = append(lines, "Already changed before or still changed after run:")
		for _, path := range existing {
			lines = append(lines, "- "+path)
		}
	}
	return strings.Join(lines, "\n")
}

func runAgent(args []string) error {
	if len(args) == 0 || args[0] != "init" {
		return fmt.Errorf("usage: taskpilot agent init")
	}
	path := "AGENTS.md"
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	return os.WriteFile(path, []byte(agentRulesFile()), 0644)
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *mcpError `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func runMCP(args []string) error {
	if len(args) < 1 || args[0] != "serve" {
		return fmt.Errorf("usage: taskpilot mcp serve")
	}
	scanner := bufio.NewScanner(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	defer writer.Flush()
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req mcpRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			writeMCP(writer, mcpResponse{JSONRPC: "2.0", Error: &mcpError{Code: -32700, Message: "parse error"}})
			continue
		}
		if req.ID == nil {
			continue
		}
		result, err := handleMCPRequest(req)
		resp := mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
		if err != nil {
			resp.Result = nil
			resp.Error = &mcpError{Code: -32000, Message: err.Error()}
		}
		writeMCP(writer, resp)
	}
	return scanner.Err()
}

func writeMCP(w *bufio.Writer, resp mcpResponse) {
	b, _ := json.Marshal(resp)
	_, _ = w.Write(b)
	_, _ = w.WriteString("\n")
	_ = w.Flush()
}

func handleMCPRequest(req mcpRequest) (any, error) {
	switch req.Method {
	case "initialize":
		return map[string]any{"protocolVersion": "2024-11-05", "serverInfo": map[string]any{"name": "taskpilot", "version": "0.1.0"}, "capabilities": map[string]any{"tools": map[string]any{}}}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools()}, nil
	case "tools/call":
		var in struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &in); err != nil {
			return nil, err
		}
		return callMCPTool(in.Name, in.Arguments)
	default:
		return nil, fmt.Errorf("unsupported MCP method %s", req.Method)
	}
}

func mcpTools() []map[string]any {
	return []map[string]any{
		mcpTool("read_task", "Read full TaskPilot task detail.", map[string]any{"task_id": mcpString("Task ID")}, []string{"task_id"}),
		mcpTool("claim_task", "Claim a TaskPilot task.", map[string]any{"task_id": mcpString("Task ID"), "force": map[string]any{"type": "boolean"}, "reason": mcpString("Reason for force claim")}, []string{"task_id"}),
		mcpTool("heartbeat_task", "Renew active task ownership heartbeat.", map[string]any{"task_id": mcpString("Task ID")}, []string{"task_id"}),
		mcpTool("append_context", "Append sanitized task context.", map[string]any{"task_id": mcpString("Task ID"), "kind": mcpString("summary, decision, note, risk, blocker, output_ref"), "content": mcpString("Sanitized context content")}, []string{"task_id", "content"}),
		mcpTool("complete_task", "Complete a task with a summary.", map[string]any{"task_id": mcpString("Task ID"), "summary": mcpString("Completion summary")}, []string{"task_id"}),
	}
}

func mcpString(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func mcpTool(name, description string, properties map[string]any, required []string) map[string]any {
	return map[string]any{"name": name, "description": description, "inputSchema": map[string]any{"type": "object", "properties": properties, "required": required}}
}

func callMCPTool(name string, args map[string]any) (any, error) {
	switch name {
	case "read_task":
		var out TaskDetail
		if err := request("GET", "/api/tasks/"+mcpArg(args, "task_id"), nil, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "claim_task":
		var out Task
		body := map[string]any{"force": mcpBoolArg(args, "force"), "reason": mcpArg(args, "reason")}
		if err := request("POST", "/api/tasks/"+mcpArg(args, "task_id")+"/claim", body, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "heartbeat_task":
		var out Task
		if err := request("POST", "/api/tasks/"+mcpArg(args, "task_id")+"/heartbeat", map[string]any{}, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "append_context":
		var out ContextEntry
		kind := mcpArg(args, "kind")
		if kind == "" {
			kind = "note"
		}
		body := map[string]any{"kind": kind, "content": mcpArg(args, "content")}
		if err := request("POST", "/api/tasks/"+mcpArg(args, "task_id")+"/context", body, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "complete_task":
		var out Task
		if err := request("POST", "/api/tasks/"+mcpArg(args, "task_id")+"/complete", map[string]any{"summary": mcpArg(args, "summary")}, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	default:
		return nil, fmt.Errorf("unknown MCP tool %s", name)
	}
}

func mcpArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func mcpBoolArg(args map[string]any, key string) bool {
	if args == nil {
		return false
	}
	if v, ok := args[key].(bool); ok {
		return v
	}
	return false
}

func mcpToolResult(v any) map[string]any {
	b, _ := json.MarshalIndent(v, "", "  ")
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}}
}

func agentInstructions(taskID string) string {
	return `You are working inside TaskPilot coordination.

TaskPilot is the shared task memory for humans and agents across machines. Treat it as the source of truth for task status, ownership, decisions, handoffs, and coordination.

Current task:
- TASKPILOT_TASK_ID=` + taskID + `
- Use TASKPILOT_SERVER when calling TaskPilot.
- Use TASKPILOT_ACTOR_ID as your agent identity.
- Read TASKPILOT_TASK_CONTEXT_FILE for the current task snapshot.
- Read TASKPILOT_RELATED_CONTEXT_FILE for selected prior/linked work context.
- Write task progress to TASKPILOT_RUN_CONTEXT_FILE.

Required workflow:
1. Read TASKPILOT_TASK_CONTEXT_FILE before making assumptions.
2. Read TASKPILOT_RELATED_CONTEXT_FILE for linked tasks and relevant prior work, especially tasks with overlapping scope.
3. Respect the task goal, scope, status, owner, locks, decisions, blockers, and handoff state.
4. Work only inside the task scope unless the user explicitly expands it.
5. Do not duplicate work already owned by another actor.
6. If you discover overlap, blockers, stale context, or unsafe ambiguity, record it as task context.
7. Share sanitized context only. Do not write secrets, raw local files, private prompts, customer data, credentials, or long logs.
8. Preserve decisions made by previous agents unless new evidence clearly invalidates them.
9. Before stopping, leave enough context for another agent to continue without asking a human to re-explain.

Write useful updates to TASKPILOT_RUN_CONTEXT_FILE as soon as each meaningful unit of work finishes. Do not wait until the whole session ends.

Accepted context formats:
- summary: Found that expiry validation fails after invite token lookup
- decision: Keep token format unchanged
- risk: Changing DB schema may break existing invite links
- blocker: Need reproduction data for expired invite token
- output_ref: Touched src/auth/token.go and src/auth/token_test.go
- next: Add regression coverage for invited-user signup
- {"kind":"decision","content":"Patch expiry comparison only"}

Recommended update timing:
- After reading important task context, write a short summary if it changes your plan.
- After finding a root cause, write a summary.
- After making a decision, write a decision with the reason.
- After discovering a risk or blocker, write it immediately.
- After changing files or creating outputs, write an output_ref.
- Before handing off or stopping, write next steps.

When possible, use the TaskPilot CLI directly:
- taskpilot task show ` + taskID + ` --json
- taskpilot context append ` + taskID + ` --kind decision --content "..."
- taskpilot decision add ` + taskID + ` --decision "..." --reason "..." --impact "..."
- taskpilot handoff prepare ` + taskID + ` --summary "..." --next "..."

If the taskpilot command is not available on PATH, or the agent runtime cannot reach the TaskPilot server, continue from TASKPILOT_TASK_CONTEXT_FILE and TASKPILOT_RELATED_CONTEXT_FILE, then write updates to TASKPILOT_RUN_CONTEXT_FILE so TaskPilot can import your context.

Completion rule:
- Mark work complete only when the task goal and completion criteria are satisfied.
- If work cannot be completed, record blocker/risk/next steps and leave the task ready for handoff.`
}

func agentRulesFile() string {
	return `# TaskPilot Agent Rules

This repository uses TaskPilot for human-agent coordination.

When the user gives you a TaskPilot task ID:

1. Run ` + "`taskpilot task show <task-id> --json`" + ` before starting.
2. Claim the task before editing.
3. Acquire locks for files, artifacts, or semantic areas you will touch.
4. Send heartbeat while actively working, or use ` + "`taskpilot run <task-id> -- <agent-command>`" + `.
5. Append sanitized findings, decisions, risks, blockers, and output references.
   When launched through ` + "`taskpilot run`" + `, write sanitized entries to ` + "`$TASKPILOT_RUN_CONTEXT_FILE`" + `:
   - ` + "`decision: Keep token format unchanged`" + `
   - ` + "`blocker: Missing reproduction data`" + `
   - ` + "`{\"kind\":\"summary\",\"content\":\"Added regression coverage\"}`" + `
6. Do not upload raw local files, secrets, prompts, logs, screenshots, or customer data unless explicitly approved.
7. Prepare a handoff if stopping before completion.
8. Mark complete only when the task completion criteria are satisfied.
`
}

func runMigrate(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: taskpilot migrate up|status")
	}
	cfg := LoadServerConfig("", "taskpilot.db", "", false)
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()
	switch args[0] {
	case "up":
		fmt.Println("migrations applied")
	case "status":
		stats, err := store.Stats(context.Background())
		if err != nil {
			return err
		}
		return print(stats, true)
	default:
		return fmt.Errorf("unknown migrate command %q", args[0])
	}
	return nil
}

func runAdmin(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: taskpilot admin create-user|create-actor|reset-password|create-api-key")
	}
	switch args[0] {
	case "create-user":
		fs := flag.NewFlagSet("admin create-user", flag.ExitOnError)
		email := fs.String("email", "", "user email")
		name := fs.String("name", "", "display name")
		password := fs.String("password", "", "password")
		role := fs.String("role", "developer", "admin, maintainer, developer, or viewer")
		db := fs.String("db", firstNonEmpty(os.Getenv("TASKPILOT_DB_URL"), "taskpilot.db"), "SQLite database path")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		store, err := OpenStore(*db)
		if err != nil {
			return err
		}
		defer store.Close()
		out, err := store.CreateUser(context.Background(), *email, *name, *password, *role)
		if err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "create-actor":
		fs := flag.NewFlagSet("admin create-actor", flag.ExitOnError)
		name := fs.String("name", "", "actor name")
		kind := fs.String("kind", "agent", "human or agent")
		machine := fs.String("machine", "", "machine name")
		db := fs.String("db", firstNonEmpty(os.Getenv("TASKPILOT_DB_URL"), "taskpilot.db"), "SQLite database path")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		store, err := OpenStore(*db)
		if err != nil {
			return err
		}
		defer store.Close()
		out, err := store.RegisterActor(context.Background(), *name, *kind, *machine)
		if err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "create-api-key":
		fs := flag.NewFlagSet("admin create-api-key", flag.ExitOnError)
		name := fs.String("name", "", "key name")
		actor := fs.String("actor", "", "actor id")
		role := fs.String("role", "agent", "admin, maintainer, developer, agent, or viewer")
		scopes := multiFlag{}
		fs.Var(&scopes, "scope", "scope; repeat for multiple")
		db := fs.String("db", firstNonEmpty(os.Getenv("TASKPILOT_DB_URL"), "taskpilot.db"), "SQLite database path")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		store, err := OpenStore(*db)
		if err != nil {
			return err
		}
		defer store.Close()
		out, err := store.CreateAPIKey(context.Background(), *name, *actor, *role, []string(scopes), "local-admin")
		if err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "reset-password":
		fs := flag.NewFlagSet("admin reset-password", flag.ExitOnError)
		userID := fs.String("user", "", "user id")
		password := fs.String("password", "", "new password")
		db := fs.String("db", firstNonEmpty(os.Getenv("TASKPILOT_DB_URL"), "taskpilot.db"), "SQLite database path")
		_ = fs.Parse(args[1:])
		if *userID == "" {
			return fmt.Errorf("--user is required")
		}
		store, err := OpenStore(*db)
		if err != nil {
			return err
		}
		defer store.Close()
		if err := store.ChangeUserPassword(context.Background(), "local-admin", *userID, "", *password, false); err != nil {
			return err
		}
		fmt.Println("password reset")
		return nil
	default:
		return fmt.Errorf("unknown admin command %q", args[0])
	}
}

func runAPIKey(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: taskpilot api-key set|create|list|revoke")
	}
	switch args[0] {
	case "set":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot api-key set <api-key>")
		}
		cfg, _ := loadConfig()
		cfg.APIKey = args[1]
		return saveConfig(cfg)
	case "create":
		fs := flag.NewFlagSet("api-key create", flag.ExitOnError)
		name := fs.String("name", "", "key name")
		actor := fs.String("actor", "", "actor id")
		role := fs.String("role", "agent", "admin, maintainer, developer, agent, or viewer")
		scopes := multiFlag{}
		fs.Var(&scopes, "scope", "scope; repeat for multiple")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		var out APIKey
		body := map[string]any{"name": *name, "actor_id": *actor, "role": *role, "scopes": []string(scopes)}
		if err := request("POST", "/api/api-keys", body, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "list":
		var out []APIKey
		if err := request("GET", "/api/api-keys", nil, &out); err != nil {
			return err
		}
		return print(out, has(args, "--json"))
	case "revoke":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot api-key revoke <key-id>")
		}
		if err := request("DELETE", "/api/api-keys/"+args[1], nil, nil); err != nil {
			return err
		}
		fmt.Println("api key revoked")
		return nil
	default:
		return fmt.Errorf("unknown api-key command %q", args[0])
	}
}

func runProject(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: taskpilot project create|list")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("project create", flag.ExitOnError)
		name := fs.String("name", "", "project name")
		description := fs.String("description", "", "description")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		var out Project
		if err := request("POST", "/api/projects", map[string]any{"name": *name, "description": *description}, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "list":
		var out []Project
		if err := request("GET", "/api/projects", nil, &out); err != nil {
			return err
		}
		return print(out, has(args, "--json"))
	default:
		return fmt.Errorf("unknown project command %q", args[0])
	}
}

func runRepo(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: taskpilot repo create|list")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("repo create", flag.ExitOnError)
		project := fs.String("project", "", "project id")
		name := fs.String("name", "", "repository name")
		path := fs.String("path", "", "local path or remote url")
		branch := fs.String("branch", "main", "default branch")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		var out Repository
		if err := request("POST", "/api/repositories", map[string]any{"project_id": *project, "name": *name, "path": *path, "default_branch": *branch}, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "list":
		fs := flag.NewFlagSet("repo list", flag.ExitOnError)
		project := fs.String("project", "", "project id")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		path := "/api/repositories"
		if *project != "" {
			path += "?project_id=" + *project
		}
		var out []Repository
		if err := request("GET", path, nil, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	default:
		return fmt.Errorf("unknown repo command %q", args[0])
	}
}

func runWorkspace(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: taskpilot workspace create|list")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("workspace create", flag.ExitOnError)
		project := fs.String("project", "", "project id")
		actor := fs.String("actor", "", "actor id")
		name := fs.String("name", "", "workspace name")
		machine := fs.String("machine", "", "machine name")
		kind := fs.String("kind", "local", "local, agent, ci, or other")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		var out Workspace
		if err := request("POST", "/api/workspaces", map[string]any{"project_id": *project, "actor_id": *actor, "name": *name, "machine_name": *machine, "kind": *kind}, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "list":
		fs := flag.NewFlagSet("workspace list", flag.ExitOnError)
		project := fs.String("project", "", "project id")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		path := "/api/workspaces"
		if *project != "" {
			path += "?project_id=" + *project
		}
		var out []Workspace
		if err := request("GET", path, nil, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	default:
		return fmt.Errorf("unknown workspace command %q", args[0])
	}
}

func runBackup(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: taskpilot backup create|restore")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("backup create", flag.ExitOnError)
		out := fs.String("out", "taskpilot-backup.db", "backup path")
		db := fs.String("db", firstNonEmpty(os.Getenv("TASKPILOT_DB_URL"), "taskpilot.db"), "SQLite database path")
		_ = fs.Parse(args[1:])
		return copyFile(*db, *out)
	case "restore":
		fs := flag.NewFlagSet("backup restore", flag.ExitOnError)
		in := fs.String("in", "", "backup path")
		db := fs.String("db", firstNonEmpty(os.Getenv("TASKPILOT_DB_URL"), "taskpilot.db"), "SQLite database path")
		_ = fs.Parse(args[1:])
		if *in == "" {
			return fmt.Errorf("--in is required")
		}
		return copyFile(*in, *db)
	default:
		return fmt.Errorf("unknown backup command %q", args[0])
	}
}

func runLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	server := fs.String("server", "http://127.0.0.1:8080", "server URL")
	token := fs.String("token", "dev-token", "team token")
	apiKey := fs.String("api-key", "", "production API key")
	_ = fs.Parse(args)
	cfg, _ := loadConfig()
	cfg.Server = strings.TrimRight(*server, "/")
	cfg.Token = *token
	cfg.APIKey = *apiKey
	return saveConfig(cfg)
}

func runConfig(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: taskpilot config set-server|set-token|set-api-key <value> OR taskpilot config set-actor <actor-id> <actor-secret>")
	}
	cfg, _ := loadConfig()
	switch args[0] {
	case "set-server":
		cfg.Server = strings.TrimRight(args[1], "/")
	case "set-token":
		cfg.Token = args[1]
	case "set-api-key":
		cfg.APIKey = args[1]
	case "set-actor":
		if len(args) < 3 {
			return fmt.Errorf("usage: taskpilot config set-actor <actor-id> <actor-secret>")
		}
		cfg.ActorID = args[1]
		cfg.ActorSecret = args[2]
	default:
		return fmt.Errorf("unknown config command %q", args[0])
	}
	return saveConfig(cfg)
}

func runActor(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: taskpilot actor register|list")
	}
	switch args[0] {
	case "register":
		fs := flag.NewFlagSet("actor register", flag.ExitOnError)
		name := fs.String("name", "", "actor name")
		kind := fs.String("kind", "agent", "human or agent")
		machine := fs.String("machine", "", "machine name")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		var out Actor
		if err := requestNoActor("POST", "/api/actors/register", map[string]any{"name": *name, "kind": *kind, "machine_name": *machine}, &out); err != nil {
			return err
		}
		cfg, _ := loadConfig()
		cfg.ActorID = out.ID
		cfg.ActorSecret = out.Secret
		_ = saveConfig(cfg)
		return print(out, *jsonOut)
	case "list":
		var out []Actor
		if err := request("GET", "/api/actors", nil, &out); err != nil {
			return err
		}
		return print(out, has(args, "--json"))
	default:
		return fmt.Errorf("unknown actor command %q", args[0])
	}
}

func runTask(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: taskpilot task create|list|show|subtask|depend|undepend|claim|release|heartbeat|status|complete")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("task create", flag.ExitOnError)
		title := fs.String("title", "", "title")
		goal := fs.String("goal", "", "goal")
		typ := fs.String("type", "implementation", "task type")
		priority := fs.String("priority", "normal", "priority")
		scope := fs.String("scope", "", "comma-separated scope")
		project := fs.String("project", "", "project id")
		repo := fs.String("repo", "", "repository id")
		workspace := fs.String("workspace", "", "workspace id")
		parent := fs.String("parent", "", "parent task id")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		var out Task
		body := TaskInput{ProjectID: *project, RepoID: *repo, WorkspaceID: *workspace, ParentTaskID: *parent, Title: *title, Goal: *goal, Type: *typ, Priority: *priority, Scope: splitCSV(*scope)}
		if err := request("POST", "/api/tasks", body, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "list":
		fs := flag.NewFlagSet("task list", flag.ExitOnError)
		project := fs.String("project", "", "project id")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		path := "/api/tasks"
		if *project != "" {
			path += "?project_id=" + *project
		}
		var out []Task
		if err := request("GET", path, nil, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "show":
		id, jsonOut, err := idAndJSON(args[1:])
		if err != nil {
			return err
		}
		var out TaskDetail
		if err := request("GET", "/api/tasks/"+id, nil, &out); err != nil {
			return err
		}
		return print(out, jsonOut)
	case "subtask":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot task subtask <parent-task-id> --title text --goal text")
		}
		fs := flag.NewFlagSet("task subtask", flag.ExitOnError)
		title := fs.String("title", "", "title")
		goal := fs.String("goal", "", "goal")
		typ := fs.String("type", "implementation", "task type")
		priority := fs.String("priority", "normal", "priority")
		scope := fs.String("scope", "", "comma-separated scope")
		jsonOut := fs.Bool("json", false, "print JSON")
		id := args[1]
		_ = fs.Parse(args[2:])
		var out Task
		body := TaskInput{Title: *title, Goal: *goal, Type: *typ, Priority: *priority, Scope: splitCSV(*scope)}
		if err := request("POST", "/api/tasks/"+id+"/subtasks", body, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "depend":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot task depend <task-id> --on <dependency-task-id>")
		}
		fs := flag.NewFlagSet("task depend", flag.ExitOnError)
		on := fs.String("on", "", "dependency task id")
		jsonOut := fs.Bool("json", false, "print JSON")
		id := args[1]
		_ = fs.Parse(args[2:])
		var out TaskDependency
		if err := request("POST", "/api/tasks/"+id+"/dependencies", map[string]any{"depends_on_id": *on}, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "undepend":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot task undepend <dependency-id>")
		}
		if err := request("DELETE", "/api/dependencies/"+args[1], nil, nil); err != nil {
			return err
		}
		fmt.Println("dependency removed")
		return nil
	case "claim":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot task claim <task-id> [--force] [--reason text]")
		}
		fs := flag.NewFlagSet("task claim", flag.ExitOnError)
		force := fs.Bool("force", false, "force reassignment")
		reason := fs.String("reason", "", "reason")
		jsonOut := fs.Bool("json", false, "print JSON")
		id := args[1]
		_ = fs.Parse(args[2:])
		var out Task
		if err := request("POST", "/api/tasks/"+id+"/claim", map[string]any{"force": *force, "reason": *reason}, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "release", "heartbeat":
		id, jsonOut, err := idAndJSON(args[1:])
		if err != nil {
			return err
		}
		var out Task
		if err := request("POST", "/api/tasks/"+id+"/"+args[0], map[string]any{}, &out); err != nil {
			return err
		}
		return print(out, jsonOut)
	case "status":
		if len(args) < 3 {
			return fmt.Errorf("usage: taskpilot task status <task-id> <status>")
		}
		var out Task
		if err := request("PATCH", "/api/tasks/"+args[1], map[string]any{"status": args[2]}, &out); err != nil {
			return err
		}
		return print(out, has(args, "--json"))
	case "complete":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot task complete <task-id> --summary text")
		}
		fs := flag.NewFlagSet("task complete", flag.ExitOnError)
		summary := fs.String("summary", "", "completion summary")
		jsonOut := fs.Bool("json", false, "print JSON")
		id := args[1]
		_ = fs.Parse(args[2:])
		var out Task
		if err := request("POST", "/api/tasks/"+id+"/complete", map[string]any{"summary": *summary}, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	default:
		return fmt.Errorf("unknown task command %q", args[0])
	}
}

func runContext(args []string) error {
	if len(args) < 1 || args[0] != "append" || len(args) < 2 {
		return fmt.Errorf("usage: taskpilot context append <task-id> --kind decision --content text")
	}
	fs := flag.NewFlagSet("context append", flag.ExitOnError)
	kind := fs.String("kind", "note", "context kind")
	content := fs.String("content", "", "content")
	jsonOut := fs.Bool("json", false, "print JSON")
	id := args[1]
	_ = fs.Parse(args[2:])
	var out ContextEntry
	if err := request("POST", "/api/tasks/"+id+"/context", map[string]any{"kind": *kind, "content": *content}, &out); err != nil {
		return err
	}
	return print(out, *jsonOut)
}

func runDecision(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: taskpilot decision add|list")
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot decision add <task-id> --decision text [--alternative text] [--reason text] [--impact text]")
		}
		fs := flag.NewFlagSet("decision add", flag.ExitOnError)
		decision := fs.String("decision", "", "decision text")
		reason := fs.String("reason", "", "why this decision was made")
		impact := fs.String("impact", "", "expected impact")
		jsonOut := fs.Bool("json", false, "print JSON")
		alternatives := multiFlag{}
		fs.Var(&alternatives, "alternative", "alternative considered; can be repeated")
		id := args[1]
		_ = fs.Parse(args[2:])
		var out DecisionRecord
		if err := request("POST", "/api/tasks/"+id+"/decisions", map[string]any{"decision": *decision, "alternatives": []string(alternatives), "reason": *reason, "impact": *impact}, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "list":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot decision list <task-id> [--json]")
		}
		id, jsonOut, err := idAndJSON(args[1:])
		if err != nil {
			return err
		}
		var out []DecisionRecord
		if err := request("GET", "/api/tasks/"+id+"/decisions", nil, &out); err != nil {
			return err
		}
		return print(out, jsonOut)
	default:
		return fmt.Errorf("unknown decision command %q", args[0])
	}
}

func runComment(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: taskpilot comment add|list")
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot comment add <task-id> --body text")
		}
		fs := flag.NewFlagSet("comment add", flag.ExitOnError)
		body := fs.String("body", "", "comment body")
		jsonOut := fs.Bool("json", false, "print JSON")
		id := args[1]
		_ = fs.Parse(args[2:])
		var out Comment
		if err := request("POST", "/api/tasks/"+id+"/comments", map[string]any{"body": *body}, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "list":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot comment list <task-id> [--json]")
		}
		id, jsonOut, err := idAndJSON(args[1:])
		if err != nil {
			return err
		}
		var out []Comment
		if err := request("GET", "/api/tasks/"+id+"/comments", nil, &out); err != nil {
			return err
		}
		return print(out, jsonOut)
	default:
		return fmt.Errorf("unknown comment command %q", args[0])
	}
}

func runArtifact(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: taskpilot artifact add|list")
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot artifact add <task-id> --kind pr --title text --uri ref")
		}
		fs := flag.NewFlagSet("artifact add", flag.ExitOnError)
		kind := fs.String("kind", "other", "artifact kind: pr, log, branch, doc, screenshot, output, other")
		title := fs.String("title", "", "artifact title")
		uri := fs.String("uri", "", "artifact reference URI/path")
		description := fs.String("description", "", "description")
		jsonOut := fs.Bool("json", false, "print JSON")
		id := args[1]
		_ = fs.Parse(args[2:])
		var out Artifact
		if err := request("POST", "/api/tasks/"+id+"/artifacts", map[string]any{"kind": *kind, "title": *title, "uri": *uri, "description": *description}, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "list":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot artifact list <task-id> [--json]")
		}
		id, jsonOut, err := idAndJSON(args[1:])
		if err != nil {
			return err
		}
		var out []Artifact
		if err := request("GET", "/api/tasks/"+id+"/artifacts", nil, &out); err != nil {
			return err
		}
		return print(out, jsonOut)
	default:
		return fmt.Errorf("unknown artifact command %q", args[0])
	}
}

func runGit(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: taskpilot git link-branch|attach-pr|attach")
	}
	switch args[0] {
	case "link-branch":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot git link-branch <task-id> [--branch name] [--commit sha] [--changed] [--note text]")
		}
		fs := flag.NewFlagSet("git link-branch", flag.ExitOnError)
		branch := fs.String("branch", currentGitBranch(), "branch name")
		commit := fs.String("commit", currentGitCommit(), "commit sha")
		includeChanged := fs.Bool("changed", true, "attach current changed files")
		note := fs.String("note", "", "note")
		jsonOut := fs.Bool("json", false, "print JSON")
		id := args[1]
		_ = fs.Parse(args[2:])
		changed := []string{}
		if *includeChanged {
			changed = gitChangedFileList()
		}
		var out GitRef
		if err := request("POST", "/api/tasks/"+id+"/git", map[string]any{"branch": *branch, "commit_sha": *commit, "changed_files": changed, "note": *note}, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "attach-pr":
		if len(args) < 3 {
			return fmt.Errorf("usage: taskpilot git attach-pr <task-id> <url> [--branch name] [--commit sha] [--changed] [--note text]")
		}
		fs := flag.NewFlagSet("git attach-pr", flag.ExitOnError)
		branch := fs.String("branch", currentGitBranch(), "branch name")
		commit := fs.String("commit", currentGitCommit(), "commit sha")
		includeChanged := fs.Bool("changed", true, "attach current changed files")
		note := fs.String("note", "", "note")
		jsonOut := fs.Bool("json", false, "print JSON")
		id := args[1]
		prURL := args[2]
		_ = fs.Parse(args[3:])
		changed := []string{}
		if *includeChanged {
			changed = gitChangedFileList()
		}
		var out GitRef
		if err := request("POST", "/api/tasks/"+id+"/git", map[string]any{"branch": *branch, "commit_sha": *commit, "pr_url": prURL, "changed_files": changed, "note": *note}, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "attach":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot git attach <task-id> [--branch name] [--commit sha] [--pr url] [--file path] [--note text]")
		}
		fs := flag.NewFlagSet("git attach", flag.ExitOnError)
		branch := fs.String("branch", "", "branch name")
		commit := fs.String("commit", "", "commit sha")
		prURL := fs.String("pr", "", "pull request URL")
		note := fs.String("note", "", "note")
		jsonOut := fs.Bool("json", false, "print JSON")
		files := multiFlag{}
		fs.Var(&files, "file", "changed file; can be repeated")
		id := args[1]
		_ = fs.Parse(args[2:])
		var out GitRef
		if err := request("POST", "/api/tasks/"+id+"/git", map[string]any{"branch": *branch, "commit_sha": *commit, "pr_url": *prURL, "changed_files": []string(files), "note": *note}, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	default:
		return fmt.Errorf("unknown git command %q", args[0])
	}
}

func runLock(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: taskpilot lock acquire|release|renew")
	}
	switch args[0] {
	case "acquire":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot lock acquire <task-id> --scope src/auth/*")
		}
		fs := flag.NewFlagSet("lock acquire", flag.ExitOnError)
		scope := fs.String("scope", "", "scope")
		scopeType := fs.String("type", "file_glob", "file_glob, semantic_area, or artifact")
		jsonOut := fs.Bool("json", false, "print JSON")
		id := args[1]
		_ = fs.Parse(args[2:])
		var out Lock
		if err := request("POST", "/api/tasks/"+id+"/locks", map[string]any{"scope": *scope, "scope_type": *scopeType}, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "release", "renew":
		id, jsonOut, err := idAndJSON(args[1:])
		if err != nil {
			return err
		}
		var out Lock
		if err := request("POST", "/api/locks/"+id+"/"+args[0], map[string]any{}, &out); err != nil {
			return err
		}
		return print(out, jsonOut)
	default:
		return fmt.Errorf("unknown lock command %q", args[0])
	}
}

func runHandoff(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: taskpilot handoff prepare|accept|reject")
	}
	switch args[0] {
	case "prepare":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot handoff prepare <task-id> --summary text --next step")
		}
		fs := flag.NewFlagSet("handoff prepare", flag.ExitOnError)
		to := fs.String("to", "", "target actor id")
		summary := fs.String("summary", "", "resume summary")
		next := multiFlag{}
		fs.Var(&next, "next", "next step")
		jsonOut := fs.Bool("json", false, "print JSON")
		id := args[1]
		_ = fs.Parse(args[2:])
		var out Handoff
		if err := request("POST", "/api/tasks/"+id+"/handoff", map[string]any{"to_actor_id": *to, "summary": *summary, "next_steps": []string(next)}, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "accept", "reject":
		id, jsonOut, err := idAndJSON(args[1:])
		if err != nil {
			return err
		}
		var out Handoff
		if err := request("POST", "/api/handoffs/"+id+"/"+args[0], map[string]any{}, &out); err != nil {
			return err
		}
		return print(out, jsonOut)
	default:
		return fmt.Errorf("unknown handoff command %q", args[0])
	}
}

func request(method, path string, body any, out any) error {
	return doRequest(method, path, body, out, true)
}

func requestNoActor(method, path string, body any, out any) error {
	return doRequest(method, path, body, out, false)
}

func doRequest(method, path string, body any, out any, includeActor bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if cfg.Server == "" {
		cfg.Server = "http://127.0.0.1:8080"
	}
	if cfg.Token == "" {
		cfg.Token = "dev-token"
	}
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(cfg.Server, "/")+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "ApiKey "+cfg.APIKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	if includeActor && cfg.ActorID != "" {
		req.Header.Set("X-Actor-ID", cfg.ActorID)
	}
	if includeActor && cfg.ActorSecret != "" {
		req.Header.Set("X-Actor-Secret", cfg.ActorSecret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) || strings.Contains(err.Error(), "connect: connection refused") || strings.Contains(err.Error(), "operation not permitted") {
			return fmt.Errorf("cannot reach TaskPilot server at %s; start it with `taskpilot serve --addr 127.0.0.1:8080 --token <token>` and check `taskpilot config set-server`", cfg.Server)
		}
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var ae APIError
		if json.Unmarshal(data, &ae) == nil && ae.Message != "" {
			return fmt.Errorf("%s: %s", ae.Error, ae.Message)
		}
		return fmt.Errorf("request failed: %s", resp.Status)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func print(v any, jsonOut bool) error {
	if jsonOut {
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	switch x := v.(type) {
	case Task:
		fmt.Printf("%s\t%s\t%s\tproject=%s\towner=%s\n", x.ID, x.Status, x.Title, x.ProjectID, x.OwnerID)
	case []Task:
		for _, t := range x {
			fmt.Printf("%s\t%s\t%s\tproject=%s\towner=%s\tlocks=%d\n", t.ID, t.Status, t.Title, t.ProjectID, t.OwnerID, t.ActiveLockCount)
		}
	case Actor:
		fmt.Printf("%s\t%s\t%s\n", x.ID, x.Kind, x.Name)
	case []Actor:
		for _, a := range x {
			fmt.Printf("%s\t%s\t%s\t%s\n", a.ID, a.Kind, a.Name, a.MachineName)
		}
	case Project:
		fmt.Printf("%s\t%s\t%s\n", x.ID, x.Name, x.Description)
	case []Project:
		for _, p := range x {
			fmt.Printf("%s\t%s\t%s\n", p.ID, p.Name, p.Description)
		}
	case Repository:
		fmt.Printf("%s\t%s\tproject=%s\tbranch=%s\t%s\n", x.ID, x.Name, x.ProjectID, x.DefaultBranch, x.Path)
	case []Repository:
		for _, r := range x {
			fmt.Printf("%s\t%s\tproject=%s\tbranch=%s\t%s\n", r.ID, r.Name, r.ProjectID, r.DefaultBranch, r.Path)
		}
	case Workspace:
		fmt.Printf("%s\t%s\tproject=%s\tactor=%s\t%s\n", x.ID, x.Name, x.ProjectID, x.ActorID, x.MachineName)
	case []Workspace:
		for _, w := range x {
			fmt.Printf("%s\t%s\tproject=%s\tactor=%s\t%s\n", w.ID, w.Name, w.ProjectID, w.ActorID, w.MachineName)
		}
	case TaskDependency:
		fmt.Printf("%s\ttask=%s\tdepends_on=%s\n", x.ID, x.TaskID, x.DependsOnID)
	case []TaskDependency:
		for _, d := range x {
			fmt.Printf("%s\ttask=%s\tdepends_on=%s\n", d.ID, d.TaskID, d.DependsOnID)
		}
	case DecisionRecord:
		fmt.Printf("%s\t%s\nReason: %s\nImpact: %s\n", x.ID, x.Decision, x.Reason, x.Impact)
	case []DecisionRecord:
		for _, d := range x {
			fmt.Printf("%s\t%s\treason=%s\n", d.ID, d.Decision, d.Reason)
		}
	case Comment:
		fmt.Printf("%s\t%s\t%s\n", x.ID, x.AuthorID, x.Body)
	case []Comment:
		for _, c := range x {
			fmt.Printf("%s\t%s\t%s\n", c.ID, c.AuthorID, c.Body)
		}
	case Artifact:
		fmt.Printf("%s\t%s\t%s\t%s\n", x.ID, x.Kind, x.Title, x.URI)
	case []Artifact:
		for _, a := range x {
			fmt.Printf("%s\t%s\t%s\t%s\n", a.ID, a.Kind, a.Title, a.URI)
		}
	case GitRef:
		fmt.Printf("%s\tbranch=%s\tcommit=%s\tpr=%s\tfiles=%s\n", x.ID, x.Branch, x.CommitSHA, x.PRURL, strings.Join(x.ChangedFiles, ","))
	case []GitRef:
		for _, g := range x {
			fmt.Printf("%s\tbranch=%s\tcommit=%s\tpr=%s\tfiles=%s\n", g.ID, g.Branch, g.CommitSHA, g.PRURL, strings.Join(g.ChangedFiles, ","))
		}
	case User:
		fmt.Printf("%s\t%s\t%s\t%s\tactive=%t\n", x.ID, x.Email, x.Name, x.Role, x.Active)
	case []User:
		for _, u := range x {
			fmt.Printf("%s\t%s\t%s\t%s\tactive=%t\n", u.ID, u.Email, u.Name, u.Role, u.Active)
		}
	case APIKey:
		secret := ""
		if x.Secret != "" {
			secret = "\tapi_key=" + x.Secret
		}
		fmt.Printf("%s\t%s\tactor=%s\trole=%s\tscopes=%s%s\n", x.ID, x.Name, x.ActorID, x.Role, strings.Join(x.Scopes, ","), secret)
	case []APIKey:
		for _, k := range x {
			revoked := ""
			if k.RevokedAt != nil {
				revoked = "\trevoked=true"
			}
			fmt.Printf("%s\t%s\tactor=%s\trole=%s\tscopes=%s\tprefix=%s%s\n", k.ID, k.Name, k.ActorID, k.Role, strings.Join(k.Scopes, ","), k.Prefix, revoked)
		}
	case TaskDetail:
		fmt.Printf("%s\t%s\t%s\nProject: %s\nRepo: %s\nWorkspace: %s\nParent: %s\nGoal: %s\nOwner: %s\nScope: %s\n", x.Task.ID, x.Task.Status, x.Task.Title, x.Task.ProjectID, x.Task.RepoID, x.Task.WorkspaceID, x.Task.ParentTaskID, x.Task.Goal, x.Task.OwnerID, strings.Join(x.Task.Scope, ", "))
		if len(x.Subtasks) > 0 {
			fmt.Println("\nSubtasks:")
			for _, t := range x.Subtasks {
				fmt.Printf("- %s %s: %s\n", t.ID, t.Status, t.Title)
			}
		}
		if len(x.Dependencies) > 0 {
			fmt.Println("\nBlocked by:")
			for _, d := range x.Dependencies {
				name := d.DependsOnID
				if d.DependsOnTask != nil {
					name = d.DependsOnTask.Title
				}
				fmt.Printf("- %s %s\n", d.ID, name)
			}
		}
		if len(x.Decisions) > 0 {
			fmt.Println("\nDecisions:")
			for _, d := range x.Decisions {
				fmt.Printf("- %s: %s\n", d.ID, d.Decision)
				if d.Reason != "" {
					fmt.Printf("  reason: %s\n", d.Reason)
				}
			}
		}
		if len(x.Comments) > 0 {
			fmt.Println("\nComments:")
			for _, c := range x.Comments {
				fmt.Printf("- %s: %s\n", c.AuthorID, c.Body)
			}
		}
		if len(x.Artifacts) > 0 {
			fmt.Println("\nArtifacts:")
			for _, a := range x.Artifacts {
				fmt.Printf("- %s %s: %s (%s)\n", a.ID, a.Kind, a.Title, a.URI)
			}
		}
		if len(x.GitRefs) > 0 {
			fmt.Println("\nGit:")
			for _, g := range x.GitRefs {
				fmt.Printf("- %s branch=%s commit=%s pr=%s files=%s\n", g.ID, g.Branch, g.CommitSHA, g.PRURL, strings.Join(g.ChangedFiles, ","))
			}
		}
		if len(x.Context) > 0 {
			fmt.Println("\nContext:")
			for _, c := range x.Context {
				fmt.Printf("- %s: %s\n", c.Kind, c.Content)
			}
		}
		if len(x.Handoffs) > 0 {
			fmt.Println("\nHandoffs:")
			for _, h := range x.Handoffs {
				fmt.Printf("- %s %s: %s\n", h.ID, h.Status, h.ResumeSummary)
			}
		}
	default:
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(b))
	}
	return nil
}

func loadConfig() (Config, error) {
	var cfg Config
	b, err := os.ReadFile(configPath())
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	return cfg, json.Unmarshal(b, &cfg)
}

func saveConfig(cfg Config) error {
	if err := ensureDir(filepath.Dir(configPath())); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	path := configPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func configPath() string {
	if v := os.Getenv("TASKPILOT_CONFIG"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".taskpilot", "config.json")
}

func ensureDir(path string) error {
	if path == "." || path == "" {
		return nil
	}
	return os.MkdirAll(path, 0755)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := ensureDir(filepath.Dir(dst)); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func getenv(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func splitCSV(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func has(args []string, needle string) bool {
	for _, arg := range args {
		if arg == needle {
			return true
		}
	}
	return false
}

func idAndJSON(args []string) (string, bool, error) {
	if len(args) < 1 {
		return "", false, fmt.Errorf("missing id")
	}
	return args[0], has(args, "--json"), nil
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}
