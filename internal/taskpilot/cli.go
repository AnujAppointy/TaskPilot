package taskpilot

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type Config struct {
	Server            string `json:"server"`
	Email             string `json:"email,omitempty"`
	ActorID           string `json:"actor_id"`
	ActorSecret       string `json:"actor_secret"`
	ActorSessionID    string `json:"actor_session_id,omitempty"`
	ActorSessionToken string `json:"actor_session_token,omitempty"`
	CurrentTaskID     string `json:"current_task_id,omitempty"`
	AgentProvider     string `json:"agent_provider,omitempty"`
}

type configDiagnostics struct {
	ConfigPath             string                   `json:"config_path"`
	Server                 string                   `json:"server"`
	Email                  string                   `json:"email,omitempty"`
	ActorID                string                   `json:"actor_id"`
	HasSecret              bool                     `json:"has_secret"`
	Auth                   string                   `json:"auth"`
	Sources                map[string]string        `json:"sources,omitempty"`
	Effective              bool                     `json:"effective"`
	EnvOverrideActive      bool                     `json:"env_override_active"`
	DeprecatedGlobalActor  bool                     `json:"deprecated_global_actor"`
	Global                 globalConfigDiagnostics  `json:"global"`
	CurrentTerminalSession sessionConfigDiagnostics `json:"current_terminal_session"`
}

type globalConfigDiagnostics struct {
	Server          string `json:"server"`
	Email           string `json:"email,omitempty"`
	LegacyActorID   string `json:"legacy_actor_id,omitempty"`
	HasLegacySecret bool   `json:"has_legacy_secret"`
}

type sessionConfigDiagnostics struct {
	Active        bool   `json:"active"`
	Scope         string `json:"scope,omitempty"`
	ActorID       string `json:"actor_id,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Status        string `json:"status,omitempty"`
	CurrentTaskID string `json:"current_task_id,omitempty"`
	SessionFile   string `json:"session_file,omitempty"`
	Source        string `json:"source,omitempty"`
	HasToken      bool   `json:"has_token,omitempty"`
}

type terminalActorSession struct {
	Server            string    `json:"server"`
	ActorID           string    `json:"actor_id"`
	ActorName         string    `json:"actor_name,omitempty"`
	ActorSessionID    string    `json:"actor_session_id"`
	ActorSessionToken string    `json:"actor_session_token"`
	CurrentTaskID     string    `json:"current_task_id,omitempty"`
	AgentProvider     string    `json:"agent_provider,omitempty"`
	RepositoryPath    string    `json:"repository_path,omitempty"`
	MachineID         string    `json:"machine_id,omitempty"`
	TerminalID        string    `json:"terminal_id,omitempty"`
	ProcessID         int       `json:"process_id,omitempty"`
	Status            string    `json:"status,omitempty"`
	StartedAt         time.Time `json:"started_at"`
	LastHeartbeatAt   time.Time `json:"last_heartbeat_at,omitempty"`
	SessionFile       string    `json:"session_file,omitempty"`
}

type retriableRequestError struct{ err error }

func (e retriableRequestError) Error() string { return e.err.Error() }
func (e retriableRequestError) Unwrap() error { return e.err }

type queuedHandoffCheckpoint struct {
	ID            string    `json:"id"`
	Server        string    `json:"server"`
	ActorID       string    `json:"actor_id"`
	TaskID        string    `json:"task_id"`
	PacketID      string    `json:"packet_id,omitempty"`
	SessionID     string    `json:"session_id,omitempty"`
	Markdown      string    `json:"markdown"`
	CreatedAt     time.Time `json:"created_at"`
	LastAttemptAt time.Time `json:"last_attempt_at,omitempty"`
	Attempts      int       `json:"attempts"`
	LastError     string    `json:"last_error,omitempty"`
}

type queuedRepoCheckpoint struct {
	ID            string    `json:"id"`
	Server        string    `json:"server"`
	ActorID       string    `json:"actor_id"`
	RepoPath      string    `json:"repo_path"`
	Source        string    `json:"source"`
	Reason        string    `json:"reason"`
	QueuePath     string    `json:"queue_path,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	LastAttemptAt time.Time `json:"last_attempt_at,omitempty"`
	Attempts      int       `json:"attempts"`
	LastError     string    `json:"last_error,omitempty"`
}

type queuedRepoSemanticMemory struct {
	ID            string    `json:"id"`
	Server        string    `json:"server"`
	ActorID       string    `json:"actor_id"`
	RepoPath      string    `json:"repo_path"`
	TaskID        string    `json:"task_id,omitempty"`
	CompletedWork string    `json:"completed_work"`
	Why           string    `json:"why,omitempty"`
	Verification  string    `json:"verification,omitempty"`
	RemainingWork string    `json:"remaining_work,omitempty"`
	Files         []string  `json:"files,omitempty"`
	Source        string    `json:"source,omitempty"`
	Stage         string    `json:"stage,omitempty"`
	QueuePath     string    `json:"queue_path,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	LastAttemptAt time.Time `json:"last_attempt_at,omitempty"`
	Attempts      int       `json:"attempts"`
	LastError     string    `json:"last_error,omitempty"`
}

type queuedAPIRequest struct {
	ID            string          `json:"id"`
	Server        string          `json:"server"`
	ActorID       string          `json:"actor_id"`
	Method        string          `json:"method"`
	Path          string          `json:"path"`
	Body          json.RawMessage `json:"body,omitempty"`
	IncludeActor  bool            `json:"include_actor"`
	CreatedAt     time.Time       `json:"created_at"`
	LastAttemptAt time.Time       `json:"last_attempt_at,omitempty"`
	Attempts      int             `json:"attempts"`
	LastError     string          `json:"last_error,omitempty"`
	QueuePath     string          `json:"queue_path,omitempty"`
}

type queuedAPIResponse struct {
	ID          string          `json:"id"`
	Status      string          `json:"status"`
	Body        json.RawMessage `json:"body,omitempty"`
	Error       string          `json:"error,omitempty"`
	CompletedAt time.Time       `json:"completed_at"`
}

type handoffSyncOptions struct {
	Watch       bool
	Interval    time.Duration
	MaxDuration time.Duration
}

type handoffSyncResult struct {
	Flushed int  `json:"flushed"`
	Failed  int  `json:"failed"`
	Skipped bool `json:"skipped"`
}

var handoffSyncDaemonStarter = startHandoffSyncDaemon

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
		production := fs.Bool("production", false, "enforce production safety checks")
		_ = fs.Parse(args[1:])
		return ListenAndServeConfig(LoadServerConfig(*addr, *db, "", *production))
	case "login":
		return runLogin(args[1:])
	case "enable":
		return runEnable(args[1:])
	case "daemon":
		return runDaemon(args[1:])
	case "status":
		return runStatus(args[1:])
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
	case "git":
		return runGit(args[1:])
	case "artifact":
		return runArtifact(args[1:])
	case "migrate":
		return runMigrate(args[1:])
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
  taskpilot serve --addr 0.0.0.0:8080 --db taskpilot.db

Config:
  taskpilot login --server http://127.0.0.1:8080 --email anuj@company.com
  taskpilot config show
  taskpilot config set-server http://127.0.0.1:8080
  taskpilot config set-email anuj@company.com
  taskpilot actor activate --secret <actor-secret>

Bootstrap:
  taskpilot enable
  taskpilot daemon install
  taskpilot daemon start
  taskpilot daemon doctor
  taskpilot status
  taskpilot project create --name "Appointy Backend"
  taskpilot repo create --project <project-id> --name appointy-api --path /path/to/repo
  taskpilot repo repair --repo . --dry-run
  taskpilot workspace create --project <project-id> --name "Anuj Mac" --actor <actor-id>

Agent CLI:
  taskpilot task create --title "Fix signup bug" --goal "Resolve invited-user signup failure" --scope "src/auth/*" --project <project-id>
  taskpilot task list
 taskpilot task show <task-id>
  taskpilot task subtask <task-id> --title "Write tests" --goal "Add regression coverage"
  taskpilot task depend <task-id> --on <dependency-task-id>
  taskpilot task claim <task-id>
  taskpilot lock acquire <task-id> --scope "src/auth/*"
  taskpilot context append <task-id> --kind decision --content "Keep response shape stable"
  taskpilot context checkpoint --repo . --source agent-hook --reason manual
  taskpilot decision add <task-id> --decision "Keep response shape stable" --reason "Existing clients depend on it"
  taskpilot comment add <task-id> --body "Please review edge cases before merge"
  taskpilot context render --repo . --format codex
  taskpilot artifact add <task-id> --kind pr --title "Signup fix PR" --uri https://github.com/org/repo/pull/42
  taskpilot git link-branch <task-id>
  taskpilot git attach-pr <task-id> https://github.com/org/repo/pull/42
  taskpilot handoff prepare <task-id> --summary "Ready for next agent" --next "Write test" --next "Patch logic"
  taskpilot handoff checkpoint <task-id> --file "$TASKPILOT_HANDOFF_FILE"
  taskpilot handoff sync --watch

Automation:
  taskpilot run <task-id> -- <agent-command> [args...]
  taskpilot agent init
  taskpilot agent configure all
  taskpilot agent doctor
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

type repoEnableConfig struct {
	Version       int       `json:"version"`
	GitRoot       string    `json:"git_root"`
	RemoteURL     string    `json:"remote_url,omitempty"`
	DefaultBranch string    `json:"default_branch,omitempty"`
	ProjectID     string    `json:"project_id"`
	RepoID        string    `json:"repo_id"`
	WorkspaceID   string    `json:"workspace_id"`
	RepoName      string    `json:"repo_name"`
	ContextFiles  []string  `json:"context_files"`
	HookCommand   string    `json:"hook_command"`
	MCPCommand    string    `json:"mcp_command"`
	EnabledAt     time.Time `json:"enabled_at"`
}

type daemonRegistry struct {
	Repos []string `json:"repos"`
}

type repoRuntimeState struct {
	TaskID        string
	SessionID     string
	LastSignature string
}

type repoActivity struct {
	Config       repoEnableConfig `json:"config"`
	Branch       string           `json:"branch"`
	Commit       string           `json:"commit"`
	ChangedFiles []string         `json:"changed_files"`
}

type repoTaskMatch struct {
	Task          Task                        `json:"task"`
	Score         int                         `json:"score"`
	Confidence    float64                     `json:"confidence"`
	Action        string                      `json:"action"`
	Reasons       []string                    `json:"reasons"`
	Evidence      []string                    `json:"evidence,omitempty"`
	Candidates    []TaskIntelligenceCandidate `json:"candidates,omitempty"`
	Relationship  string                      `json:"relationship,omitempty"`
	ProposedTitle string                      `json:"proposed_title,omitempty"`
	ProposedGoal  string                      `json:"proposed_goal,omitempty"`
	Renamed       bool                        `json:"renamed,omitempty"`
	PreviousTitle string                      `json:"previous_title,omitempty"`
	NewTitle      string                      `json:"new_title,omitempty"`
}

type repoWorkIntent struct {
	Objective    string
	Completed    string
	Why          string
	Verification string
	Remaining    string
	Files        []string
	Stage        string
	Source       string
	ActiveTaskID string
	SessionID    string
	Kind         string
}

type AgentDetection struct {
	Name        string `json:"name"`
	Installed   bool   `json:"installed"`
	Binary      string `json:"binary,omitempty"`
	ConfigPath  string `json:"config_path"`
	HookCommand string `json:"hook_command"`
	Supported   bool   `json:"supported"`
}

type AgentConfigureResult struct {
	Name           string `json:"name"`
	ConfigPath     string `json:"config_path"`
	HookCommand    string `json:"hook_command"`
	MCPConfigPath  string `json:"mcp_config_path,omitempty"`
	Status         string `json:"status"`
	Changed        bool   `json:"changed"`
	DryRun         bool   `json:"dry_run,omitempty"`
	BackupPath     string `json:"backup_path,omitempty"`
	Message        string `json:"message"`
	ManualFallback string `json:"manual_fallback,omitempty"`
}

type AgentHealth struct {
	Name             string `json:"name"`
	Installed        bool   `json:"installed"`
	ConfigPath       string `json:"config_path"`
	HookScriptPath   string `json:"hook_script_path"`
	HookScriptExists bool   `json:"hook_script_exists"`
	Registered       bool   `json:"registered"`
	MCPRegistered    bool   `json:"mcp_registered,omitempty"`
	MCPConfigPath    string `json:"mcp_config_path,omitempty"`
	Status           string `json:"status"`
	Message          string `json:"message,omitempty"`
}

type DaemonInstallInfo struct {
	Method       string    `json:"method"`
	ServiceName  string    `json:"service_name"`
	BinaryPath   string    `json:"binary_path"`
	Command      []string  `json:"command"`
	LogPath      string    `json:"log_path"`
	ErrorLogPath string    `json:"error_log_path,omitempty"`
	InstalledAt  time.Time `json:"installed_at"`
}

type DaemonInstallResult struct {
	Method        string `json:"method"`
	ServiceName   string `json:"service_name"`
	BinaryPath    string `json:"binary_path"`
	Status        string `json:"status"`
	Changed       bool   `json:"changed"`
	Started       bool   `json:"started"`
	ConfigPath    string `json:"config_path,omitempty"`
	Message       string `json:"message"`
	ManualCommand string `json:"manual_command,omitempty"`
	Error         string `json:"error,omitempty"`
}

type DaemonHealth struct {
	Method       string   `json:"method"`
	ServiceName  string   `json:"service_name"`
	BinaryPath   string   `json:"binary_path,omitempty"`
	Installed    bool     `json:"installed"`
	Running      bool     `json:"running"`
	Status       string   `json:"status"`
	ConfigPath   string   `json:"config_path,omitempty"`
	LogPath      string   `json:"log_path"`
	EnabledRepos []string `json:"enabled_repos,omitempty"`
	Message      string   `json:"message,omitempty"`
}

type AgentAdapter interface {
	Name() string
	Detect(repo repoEnableConfig) AgentDetection
	Configure(repo repoEnableConfig, dryRun bool) AgentConfigureResult
	Doctor(repo repoEnableConfig) AgentHealth
	ManualInstructions(repo repoEnableConfig) string
}

type jsonHookAdapter struct {
	name       string
	binary     string
	configRel  string
	hookRel    string
	format     string
	jsonPath   string
	finalPaths []string
	extraNote  string
	supported  bool
	entryLabel string
}

type opencodePluginAdapter struct {
	name      string
	binaries  []string
	configRel string
	extraNote string
}

type hermesShellHookAdapter struct {
	name      string
	binaries  []string
	extraNote string
}

type SessionStartHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

type knownAgentProfile struct {
	Name            string
	Binaries        []string
	PromptInjection bool
	NativeJSONHooks bool
	ConfigRel       string
	Format          string
	JSONPath        string
	FinalPaths      []string
	ExtraNote       string
	ManualSetupNote string
	OpenCodePlugin  bool
	HermesShellHook bool
}

var knownAgentProfiles = []knownAgentProfile{
	{Name: "claude", Binaries: []string{"claude"}, PromptInjection: true, NativeJSONHooks: true, ConfigRel: ".claude/settings.json", Format: "claude", JSONPath: "hooks.SessionStart", FinalPaths: []string{"hooks.SessionEnd"}, ExtraNote: "Claude Code may show its normal hook approval prompt the first time this repo opens."},
	{Name: "codex", Binaries: []string{"codex"}, PromptInjection: true, NativeJSONHooks: true, ConfigRel: ".codex/hooks.json", Format: "codex", JSONPath: "hooks.SessionStart", FinalPaths: []string{"hooks.SessionEnd"}, ExtraNote: "Codex may show its normal hook approval prompt the first time this repo opens."},
	{Name: "gemini", Binaries: []string{"gemini"}, PromptInjection: true, NativeJSONHooks: true, ConfigRel: ".gemini/settings.json", Format: "gemini", JSONPath: "hooks.SessionStart", FinalPaths: []string{"hooks.SessionEnd"}, ExtraNote: "Gemini hook output is limited to bounded TaskPilot context."},
	{Name: "hermes", Binaries: []string{"hermes", "hermes-agent"}, PromptInjection: true, ExtraNote: "Hermes shell hooks inject TaskPilot context on pre_llm_call and checkpoint on on_session_end.", HermesShellHook: true},
	{Name: "opencode", Binaries: []string{"opencode", "open-code"}, PromptInjection: true, ConfigRel: ".opencode/plugins/taskpilot.js", ExtraNote: "OpenCode loads project plugins from .opencode/plugins at startup.", OpenCodePlugin: true},
	{Name: "openclaude", Binaries: []string{"openclaude", "open-claude"}, PromptInjection: true, ManualSetupNote: "TaskPilot can wrap OpenClaude through `taskpilot run`; native session hooks require OpenClaude to expose a command hook setting."},
	{Name: "pi", Binaries: []string{"pi"}, PromptInjection: true, ManualSetupNote: "TaskPilot can wrap Pi through `taskpilot run`; native session hooks require Pi to expose a command hook setting."},
}

type jsonHookWriteResult struct {
	Changed    bool
	Created    bool
	BackupPath string
	Message    string
}

var taskIDPattern = regexp.MustCompile(`task_[A-Za-z0-9_-]+`)

func runEnable(args []string) error {
	fs := flag.NewFlagSet("enable", flag.ExitOnError)
	projectID := fs.String("project", "project_default", "TaskPilot project ID")
	repoName := fs.String("repo-name", "", "repository display name")
	workspaceID := fs.String("workspace", "", "TaskPilot workspace ID")
	liveFiles := fs.Bool("live-files", true, "maintain bounded TaskPilot sections in repo instruction files")
	noDaemonInstall := fs.Bool("no-daemon-install", false, "do not install/start the login daemon")
	jsonOut := fs.Bool("json", false, "print JSON")
	_ = fs.Parse(args)
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if cfg.Server == "" {
		cfg.Server = "http://127.0.0.1:8080"
		_ = saveConfig(cfg)
	}
	if cfg.ActorID == "" || (cfg.ActorSecret == "" && cfg.ActorSessionToken == "") {
		return fmt.Errorf("no TaskPilot actor session configured; run `taskpilot actor activate --secret <actor-secret>`")
	}
	root, err := gitRoot(".")
	if err != nil {
		return err
	}
	remote := gitRemote(root)
	branch := gitDefaultBranch(root)
	name := strings.TrimSpace(*repoName)
	if name == "" {
		name = filepath.Base(root)
	}
	repo, err := ensureRemoteRepository(*projectID, name, root, branch)
	if err != nil {
		return err
	}
	workspace := strings.TrimSpace(*workspaceID)
	if workspace == "" {
		created, err := ensureWorkspace(*projectID, cfg.ActorID)
		if err != nil {
			return err
		}
		workspace = created.ID
	}
	contextFiles := []string{}
	if *liveFiles {
		contextFiles = []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md"}
	}
	out := repoEnableConfig{
		Version:       1,
		GitRoot:       root,
		RemoteURL:     remote,
		DefaultBranch: branch,
		ProjectID:     *projectID,
		RepoID:        repo.ID,
		WorkspaceID:   workspace,
		RepoName:      name,
		ContextFiles:  contextFiles,
		HookCommand:   "taskpilot context render --repo . --format markdown",
		MCPCommand:    "taskpilot mcp serve",
		EnabledAt:     time.Now().UTC(),
	}
	if err := saveRepoConfig(out); err != nil {
		return err
	}
	if err := addEnabledRepo(root); err != nil {
		return err
	}
	if err := writeHookScripts(out); err != nil {
		return err
	}
	if len(out.ContextFiles) > 0 {
		rendered, renderErr := renderRepoContext(root, "markdown")
		if renderErr == nil {
			_ = updateLiveContextFiles(out, rendered)
		}
	}
	adapterResults := configureAgentAdapters(out, false, []string{"all"})
	var daemonResult *DaemonInstallResult
	if !*noDaemonInstall {
		result := installDaemonAutoStart()
		daemonResult = &result
	}
	if *jsonOut {
		return print(map[string]any{"repo": out, "agent_adapters": adapterResults, "daemon": daemonResult}, true)
	}
	fmt.Printf("TaskPilot enabled for Git repo %s\n", root)
	fmt.Printf("Project: %s\nRepository: %s\nWorkspace: %s\n", out.ProjectID, out.RepoID, out.WorkspaceID)
	fmt.Printf("Hook command: %s\n", out.HookCommand)
	for _, result := range adapterResults {
		if result.Message != "" {
			fmt.Printf("%s: %s\n", result.Name, result.Message)
		}
	}
	if daemonResult != nil {
		fmt.Printf("daemon: %s\n", daemonResult.Message)
	}
	return nil
}

func runDaemon(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: taskpilot daemon install|uninstall|doctor|start|run|stop|status")
	}
	switch args[0] {
	case "install":
		result := installDaemonAutoStart()
		return print(result, has(args, "--json"))
	case "uninstall":
		result := uninstallDaemonAutoStart()
		return print(result, has(args, "--json"))
	case "doctor":
		health := daemonDoctor()
		return print(health, has(args, "--json"))
	case "start":
		fs := flag.NewFlagSet("daemon start", flag.ExitOnError)
		foreground := fs.Bool("foreground", false, "run in the foreground")
		once := fs.Bool("once", false, "sync once and exit")
		interval := fs.Duration("interval", 10*time.Second, "poll interval")
		_ = fs.Parse(args[1:])
		if *foreground || *once {
			return runDaemonLoop(*interval, *once)
		}
		return startRepoDaemonProcess(*interval)
	case "run":
		fs := flag.NewFlagSet("daemon run", flag.ExitOnError)
		interval := fs.Duration("interval", 10*time.Second, "poll interval")
		_ = fs.Parse(args[1:])
		return runDaemonLoop(*interval, false)
	case "stop":
		if err := ensureDir(filepath.Dir(repoDaemonStopPath())); err != nil {
			return err
		}
		if err := os.WriteFile(repoDaemonStopPath(), []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o600); err != nil {
			return err
		}
		fmt.Println("TaskPilot daemon stop requested.")
		return nil
	case "status":
		return runStatus(args[1:])
	default:
		return fmt.Errorf("unknown daemon command %q", args[0])
	}
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	repoPath := fs.String("repo", ".", "repo path")
	jsonOut := fs.Bool("json", false, "print JSON")
	_ = fs.Parse(args)
	root, err := gitRoot(*repoPath)
	if err != nil {
		return err
	}
	activity, err := currentRepoActivity(root)
	if err != nil {
		return err
	}
	rendered, _ := renderRepoContext(root, "markdown")
	out := map[string]any{
		"repo":          activity.Config,
		"branch":        activity.Branch,
		"commit":        activity.Commit,
		"changed_files": activity.ChangedFiles,
		"context":       rendered,
	}
	if *jsonOut {
		return print(out, true)
	}
	fmt.Printf("TaskPilot repo: %s\n", root)
	fmt.Printf("Branch: %s\nCommit: %s\nChanged files: %d\n", activity.Branch, activity.Commit, len(activity.ChangedFiles))
	if rendered != "" {
		fmt.Println()
		fmt.Println(rendered)
	}
	return nil
}

func startRepoDaemonProcess(interval time.Duration) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := ensureDir(taskpilotHomeDir()); err != nil {
		return err
	}
	_ = os.Remove(repoDaemonStopPath())
	logFile, err := os.OpenFile(repoDaemonLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "daemon", "run", "--interval", interval.String())
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return err
	}
	_ = cmd.Process.Release()
	_ = logFile.Close()
	fmt.Printf("TaskPilot daemon started. Log: %s\n", repoDaemonLogPath())
	return nil
}

func runDaemonLoop(interval time.Duration, once bool) error {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	started := time.Now()
	state := map[string]*repoRuntimeState{}
	for {
		reg, err := loadDaemonRegistry()
		if err != nil {
			return err
		}
		_, _, _ = flushQueuedAPIRequests(reg.Repos...)
		_, _, _ = flushQueuedRepoSemanticMemories(reg.Repos...)
		_, _, _ = flushQueuedRepoCheckpoints(reg.Repos...)
		for _, repo := range reg.Repos {
			repo = strings.TrimSpace(repo)
			if repo == "" {
				continue
			}
			st := state[repo]
			if st == nil {
				st = &repoRuntimeState{}
				state[repo] = st
			}
			if err := syncRepoActivity(repo, st); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "TaskPilot daemon: %s: %v\n", repo, err)
			}
		}
		if once {
			return nil
		}
		if daemonStopRequested(started) {
			return nil
		}
		time.Sleep(interval)
	}
}

func syncRepoActivity(repoPath string, state *repoRuntimeState) error {
	activity, err := currentRepoActivity(repoPath)
	if err != nil {
		return err
	}
	rendered, err := renderRepoContext(activity.Config.GitRoot, "markdown")
	if err == nil {
		_ = updateLiveContextFiles(activity.Config, rendered)
	}
	if len(activity.ChangedFiles) == 0 {
		return nil
	}
	signature := repoActivitySignature(activity)
	task, _, err := ensureTaskForRepoActivityWithIntentWithProxy(activity, repoWorkIntent{Kind: "daemon", ActiveTaskID: state.TaskID, SessionID: state.SessionID, Source: "daemon"}, false)
	if err != nil {
		return err
	}
	if state.TaskID != task.ID || state.SessionID == "" {
		var session TaskSession
		if err := request("POST", "/api/tasks/"+url.PathEscape(task.ID)+"/sessions/start", map[string]any{}, &session); err == nil {
			state.TaskID = task.ID
			state.SessionID = session.ID
		}
	}
	var heartbeat Task
	_ = request("POST", "/api/tasks/"+url.PathEscape(task.ID)+"/heartbeat", map[string]any{}, &heartbeat)
	for _, file := range activity.ChangedFiles {
		var lock Lock
		_ = request("POST", "/api/tasks/"+url.PathEscape(task.ID)+"/locks", map[string]any{"scope": file, "scope_type": "file"}, &lock)
	}
	if signature != state.LastSignature {
		if _, err := checkpointRepoContext(activity.Config.GitRoot, "daemon", "file_change"); err != nil {
			return err
		}
		state.LastSignature = signature
	}
	return nil
}

func ensureTaskForRepoActivity(activity repoActivity) (Task, error) {
	return ensureTaskForRepoActivityWithProxy(activity, true)
}

func ensureTaskForRepoActivityWithProxy(activity repoActivity, allowProxy bool) (Task, error) {
	task, _, err := ensureTaskForRepoActivityWithIntentWithProxy(activity, repoWorkIntent{Kind: "repo_activity"}, allowProxy)
	return task, err
}

func ensureTaskForRepoActivityWithIntentWithProxy(activity repoActivity, intent repoWorkIntent, allowProxy bool) (Task, repoTaskMatch, error) {
	intent = normalizeRepoWorkIntent(activity, intent)
	match, err := resolveRepoTaskWithIntentWithProxy(activity, intent, allowProxy)
	if err != nil {
		return Task{}, repoTaskMatch{}, err
	}
	title, goal, evidence := repoIntentIdentity(activity, intent)
	match.ProposedTitle = title
	match.ProposedGoal = goal
	match.Evidence = appendUniqueStrings(match.Evidence, evidence...)
	if match.Task.ID != "" && match.Score >= 80 {
		mergedScope := appendUniqueStrings(filterProductRepoFiles(match.Task.Scope), repoIntentFiles(activity, intent)...)
		updates := map[string]any{}
		if !sameStringSet(mergedScope, match.Task.Scope) {
			updates["scope"] = mergedScope
		}
		if shouldEnrichTaskIdentity(match.Task, title, goal) {
			match.Renamed = true
			match.PreviousTitle = match.Task.Title
			match.NewTitle = title
			updates["title"] = title
			updates["goal"] = goal
		}
		if len(updates) > 0 {
			updates["reason"] = "TaskPilot intelligence reused existing task for matching repo intent"
			updates["intelligence_decision"] = repoTaskIntelligenceDecision("reuse", match)
			var updated Task
			_ = doRequestWithProxy("PATCH", "/api/tasks/"+url.PathEscape(match.Task.ID), updates, &updated, true, allowProxy)
			if updated.ID != "" {
				match.Task = updated
				return updated, match, nil
			}
		}
		return match.Task, match, nil
	}
	match.Action = "create"
	match.Score = maxInt(match.Score, 30)
	if match.Confidence == 0 {
		match.Confidence = 0.35
	}
	body := TaskInput{
		ProjectID:            activity.Config.ProjectID,
		RepoID:               activity.Config.RepoID,
		WorkspaceID:          activity.Config.WorkspaceID,
		Title:                title,
		Goal:                 goal,
		Type:                 "implementation",
		Priority:             "normal",
		Status:               "ready",
		Scope:                repoIntentFiles(activity, intent),
		PrivacyLevel:         "sanitized_context",
		Requirements:         []string{"Keep this inferred task identity outcome-based; refine title, goal, scope, and relationships as more semantic context arrives."},
		IntelligenceDecision: repoTaskIntelligenceDecision("create", match),
	}
	if match.Task.ID != "" && match.Score >= 55 {
		relationshipType := repoRelationshipForMatch(match)
		body.Relationships = []TaskRelationship{{
			TargetTaskID: match.Task.ID,
			Type:         relationshipType,
			Reason:       "TaskPilot found moderate overlap while creating a distinct repo task: " + strings.Join(match.Reasons, ", "),
			Confidence:   match.Confidence,
			Source:       "inference",
		}}
	}
	var created Task
	if err := doRequestWithProxy("POST", "/api/tasks", body, &created, true, allowProxy); err != nil {
		return Task{}, match, err
	}
	match.Task = created
	return created, match, nil
}

func resolveRepoTask(activity repoActivity) (repoTaskMatch, error) {
	return resolveRepoTaskWithProxy(activity, true)
}

func resolveRepoTaskWithProxy(activity repoActivity, allowProxy bool) (repoTaskMatch, error) {
	return resolveRepoTaskWithIntentWithProxy(activity, repoWorkIntent{Kind: "repo_activity"}, allowProxy)
}

func resolveRepoTaskWithIntentWithProxy(activity repoActivity, intent repoWorkIntent, allowProxy bool) (repoTaskMatch, error) {
	tasks, err := tasksForRepoWithProxy(activity.Config.RepoID, activity.Config.ProjectID, allowProxy)
	if err != nil {
		return repoTaskMatch{}, err
	}
	intent = normalizeRepoWorkIntent(activity, intent)
	title, goal, evidence := repoIntentIdentity(activity, intent)
	intentText := strings.Join([]string{intent.Objective, intent.Completed, intent.Why, title, goal}, " ")
	explicit := taskIDPattern.FindString(activity.Branch)
	best := repoTaskMatch{}
	now := time.Now().UTC()
	for _, task := range tasks {
		if task.Status == "cancelled" {
			continue
		}
		score, reasons := repoTaskIntentScore(task, activity, intent, intentText, explicit, now)
		if explicit != "" && task.ID == explicit {
			score += 1000
		}
		confidence := repoTaskMatchConfidence(score)
		candidate := TaskIntelligenceCandidate{TaskID: task.ID, Title: task.Title, Score: score, Confidence: confidence, Action: repoTaskCandidateAction(score), Reasons: reasons}
		best.Candidates = append(best.Candidates, candidate)
		if score > best.Score {
			best.Task = task
			best.Score = score
			best.Confidence = confidence
			best.Reasons = reasons
		}
	}
	best.Action = repoTaskCandidateAction(best.Score)
	best.ProposedTitle = title
	best.ProposedGoal = goal
	best.Evidence = evidence
	sort.SliceStable(best.Candidates, func(i, j int) bool {
		return best.Candidates[i].Score > best.Candidates[j].Score
	})
	if len(best.Candidates) > 8 {
		best.Candidates = best.Candidates[:8]
	}
	return best, nil
}

func tasksForProject(projectID string) ([]Task, error) {
	path := "/api/tasks"
	if projectID != "" {
		path += "?project_id=" + url.QueryEscape(projectID)
	}
	var tasks []Task
	err := request("GET", path, nil, &tasks)
	return tasks, err
}

func tasksForRepo(repoID, fallbackProjectID string) ([]Task, error) {
	return tasksForRepoWithProxy(repoID, fallbackProjectID, true)
}

func tasksForRepoWithProxy(repoID, fallbackProjectID string, allowProxy bool) ([]Task, error) {
	path := "/api/tasks"
	if repoID != "" {
		path += "?repo_id=" + url.QueryEscape(repoID)
	} else if fallbackProjectID != "" {
		path += "?project_id=" + url.QueryEscape(fallbackProjectID)
	}
	var tasks []Task
	err := doRequestWithProxy("GET", path, nil, &tasks, true, allowProxy)
	return tasks, err
}

func currentRepoActivity(repoPath string) (repoActivity, error) {
	root, err := gitRoot(repoPath)
	if err != nil {
		return repoActivity{}, err
	}
	cfg, err := loadRepoConfig(root)
	if err != nil {
		return repoActivity{}, err
	}
	return repoActivity{
		Config:       cfg,
		Branch:       gitBranch(root),
		Commit:       gitCommit(root),
		ChangedFiles: gitChangedFileListIn(root),
	}, nil
}

func inferredTaskTitle(activity repoActivity) string {
	title, _, _ := repoIntentIdentity(activity, repoWorkIntent{Kind: "repo_activity"})
	return title
}

func inferredTaskGoal(activity repoActivity) string {
	_, goal, _ := repoIntentIdentity(activity, repoWorkIntent{Kind: "repo_activity"})
	return goal
}

func normalizeRepoWorkIntent(activity repoActivity, intent repoWorkIntent) repoWorkIntent {
	intent.Objective = strings.TrimSpace(intent.Objective)
	intent.Completed = strings.TrimSpace(intent.Completed)
	intent.Why = strings.TrimSpace(intent.Why)
	intent.Verification = strings.TrimSpace(intent.Verification)
	intent.Remaining = strings.TrimSpace(intent.Remaining)
	intent.Stage = strings.TrimSpace(intent.Stage)
	intent.Source = strings.TrimSpace(intent.Source)
	intent.ActiveTaskID = strings.TrimSpace(intent.ActiveTaskID)
	intent.SessionID = strings.TrimSpace(intent.SessionID)
	intent.Kind = strings.TrimSpace(intent.Kind)
	if intent.Kind == "" {
		intent.Kind = "repo_activity"
	}
	if len(intent.Files) == 0 {
		intent.Files = activity.ChangedFiles
	}
	intent.Files = repoIntentFiles(activity, intent)
	if intent.Objective == "" && intent.Completed != "" {
		intent.Objective = intent.Completed
	}
	return intent
}

func repoIntentFiles(activity repoActivity, intent repoWorkIntent) []string {
	files := filterProductRepoFiles(intent.Files)
	if len(files) == 0 {
		files = filterProductRepoFiles(activity.ChangedFiles)
	}
	return uniqueStrings(files)
}

func repoIntentIdentity(activity repoActivity, intent repoWorkIntent) (string, string, []string) {
	intent = normalizeRepoWorkIntent(activity, intent)
	files := repoIntentFiles(activity, intent)
	evidence := []string{}
	if intent.Objective != "" {
		evidence = append(evidence, "objective: "+intent.Objective)
	}
	if intent.Completed != "" {
		evidence = append(evidence, "completed_work: "+intent.Completed)
	}
	if intent.Why != "" {
		evidence = append(evidence, "why: "+intent.Why)
	}
	if len(files) > 0 {
		evidence = append(evidence, "files: "+strings.Join(limitStrings(files, 8), ", "))
	}

	stats := map[string]repoDiffStat{}
	headings := map[string][]string{}
	if activity.Config.GitRoot != "" && len(files) > 0 {
		stats = repoDiffStats(activity.Config.GitRoot, files)
		headings = markdownHeadings(activity.Config.GitRoot, files)
		for file, hs := range headings {
			if len(hs) > 0 {
				evidence = append(evidence, file+" headings: "+strings.Join(limitStrings(hs, 4), ", "))
			}
		}
	}

	title := ""
	if intent.Objective != "" {
		title = outcomeTitleFromText(intent.Objective, "Improve")
	}
	if title == "" && intent.Completed != "" {
		title = outcomeTitleFromText(intent.Completed, "Improve")
	}
	if title == "" {
		title = outcomeTitleFromHeadings(files, stats, headings)
	}
	if title == "" && isMeaningfulBranch(activity.Branch) {
		title = outcomeTitleFromText(humanizeBranchName(activity.Branch), "Improve")
	}
	if title == "" {
		title = "Coordinate repository work"
	}

	goal := strings.TrimSpace(intent.Objective)
	if goal == "" && intent.Completed != "" {
		goal = intent.Completed
	}
	if len(files) > 0 {
		summary := metadataSemanticSummary(activity, files, stats, headings)
		if goal == "" {
			goal = summary
		} else {
			goal = strings.TrimSuffix(goal, ".") + ". " + summary
		}
	}
	if intent.Why != "" {
		goal = strings.TrimSuffix(goal, ".") + ". Why: " + intent.Why
	}
	if goal == "" {
		goal = fmt.Sprintf("Coordinate live work in %s around the current repository intent.", activity.Config.RepoName)
	}
	return title, goal, evidence
}

func outcomeTitleFromHeadings(files []string, stats map[string]repoDiffStat, headings map[string][]string) string {
	if len(files) == 1 {
		file := files[0]
		if hs := headings[file]; len(hs) > 0 {
			return outcomeVerbForFile(file, stats[file], strings.Join(hs, " ")) + " " + sentenceCasePhrase(hs[0])
		}
		if topic := fileTopic(file); topic != "" {
			return outcomeVerbForFile(file, stats[file], topic) + " " + topic
		}
	}
	topics := []string{}
	for _, file := range files {
		if hs := headings[file]; len(hs) > 0 {
			topics = append(topics, sentenceCasePhrase(hs[0]))
			continue
		}
		if topic := fileTopic(file); topic != "" {
			topics = append(topics, topic)
		}
	}
	topics = uniqueStrings(limitStrings(topics, 3))
	if len(topics) > 0 {
		return "Improve " + strings.Join(topics, " and ")
	}
	return ""
}

func outcomeVerbForFile(file string, stat repoDiffStat, text string) string {
	if strings.TrimSpace(stat.Status) == "??" || strings.Contains(stat.Status, "A") {
		return "Add"
	}
	lower := strings.ToLower(file + " " + text)
	if strings.EqualFold(filepath.Ext(file), ".md") {
		if strings.Contains(lower, "rule") || strings.Contains(lower, "control") || strings.Contains(lower, "policy") || strings.Contains(lower, "requirement") || strings.Contains(lower, "spec") || strings.Contains(lower, "plan") || strings.Contains(lower, "design") {
			return "Define"
		}
		return "Clarify"
	}
	if strings.Contains(lower, "test") {
		return "Test"
	}
	if strings.Contains(lower, "fix") || strings.Contains(lower, "bug") {
		return "Fix"
	}
	return "Improve"
}

func outcomeTitleFromText(text, fallbackVerb string) string {
	text = strings.TrimSpace(singleLine(redactSensitiveText(text)))
	text = strings.TrimPrefix(text, "Completed:")
	text = strings.TrimPrefix(text, "completed:")
	text = strings.TrimSpace(strings.Trim(text, "."))
	if text == "" {
		return ""
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	verb := normalizedOutcomeVerb(fields[0])
	if verb != "" {
		fields[0] = verb
	} else {
		if fallbackVerb == "" {
			fallbackVerb = "Improve"
		}
		fields = append([]string{fallbackVerb}, fields...)
	}
	fields = limitStrings(fields, 10)
	title := strings.Join(fields, " ")
	title = strings.Trim(title, " .,:;")
	return title
}

func normalizedOutcomeVerb(word string) string {
	switch strings.ToLower(strings.Trim(word, " .,:;")) {
	case "add", "added", "adds":
		return "Add"
	case "build", "built":
		return "Build"
	case "clarify", "clarified":
		return "Clarify"
	case "coordinate", "coordinated":
		return "Coordinate"
	case "create", "created":
		return "Create"
	case "define", "defined":
		return "Define"
	case "fix", "fixed":
		return "Fix"
	case "implement", "implemented":
		return "Implement"
	case "improve", "improved", "update", "updated", "change", "changed":
		return "Improve"
	case "refactor", "refactored":
		return "Refactor"
	case "route", "routed":
		return "Route"
	case "test", "tested":
		return "Test"
	}
	return ""
}

func sentenceCasePhrase(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	return strings.ToLower(value)
}

func fileTopic(file string) string {
	base := filepath.Base(file)
	ext := filepath.Ext(base)
	if ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	base = strings.ReplaceAll(base, "-", " ")
	base = strings.ReplaceAll(base, "_", " ")
	base = strings.Join(strings.Fields(base), " ")
	return strings.ToLower(base)
}

func repoTaskIntentScore(task Task, activity repoActivity, intent repoWorkIntent, intentText, explicit string, now time.Time) (int, []string) {
	score := 0
	reasons := []string{}
	if explicit != "" && task.ID == explicit {
		reasons = append(reasons, "branch names explicit task id")
	}
	if intent.ActiveTaskID != "" && task.ID == intent.ActiveTaskID {
		score += 260
		reasons = append(reasons, "active repo session task")
	}
	if task.RepoID != "" && task.RepoID == activity.Config.RepoID {
		score += 20
		reasons = append(reasons, "same repo")
	}
	if task.WorkspaceID != "" && task.WorkspaceID == activity.Config.WorkspaceID {
		score += 10
		reasons = append(reasons, "same workspace")
	}
	files := repoIntentFiles(activity, intent)
	taskScope := filterProductRepoFiles(task.Scope)
	if len(files) > 0 && sameStringSet(taskScope, files) {
		score += 90
		reasons = append(reasons, "same changed file set")
	} else if taskScopesOverlap(taskScope, files) {
		score += 70
		reasons = append(reasons, "overlapping changed files")
	}
	if branchMatchesTask(activity.Branch, task) {
		score += 35
		reasons = append(reasons, "branch matches task words")
	}
	shared := sharedRepoKeywordCount(intentText, task.Title+" "+task.Goal+" "+task.SearchText)
	if shared >= 4 {
		score += 80
		reasons = append(reasons, "strong semantic intent match")
	} else if shared == 3 {
		score += 60
		reasons = append(reasons, "semantic intent match")
	} else if shared == 2 {
		score += 40
		reasons = append(reasons, "partial semantic intent match")
	} else if shared == 1 {
		score += 20
		reasons = append(reasons, "weak semantic intent match")
	}
	if task.OwnerID != "" && task.LastHeartbeatAt != nil && now.Sub(*task.LastHeartbeatAt) <= DefaultClaimTTL {
		score += 25
		reasons = append(reasons, "active task heartbeat")
	}
	if task.Status == "completed" {
		if !task.UpdatedAt.IsZero() && now.Sub(task.UpdatedAt) <= 72*time.Hour {
			score += 15
			reasons = append(reasons, "recent completed task may be receiving follow-up")
		} else {
			score -= 25
			reasons = append(reasons, "completed task")
		}
	}
	if isGenericInferredTaskTitle(task.Title) {
		score -= 15
		reasons = append(reasons, "generic inferred title needs stronger evidence")
	}
	if score < 0 {
		score = 0
	}
	return score, reasons
}

func sharedRepoKeywordCount(a, b string) int {
	left := repoKeywords(a)
	right := repoKeywords(b)
	count := 0
	for word := range left {
		if right[word] {
			count++
		}
	}
	return count
}

func repoKeywords(text string) map[string]bool {
	out := map[string]bool{}
	for _, word := range regexp.MustCompile(`[a-z0-9]+`).FindAllString(strings.ToLower(text), -1) {
		if len(word) < 4 || repoIntentStopWord(word) {
			continue
		}
		out[word] = true
	}
	return out
}

func repoIntentStopWord(word string) bool {
	switch word {
	case "about", "active", "added", "around", "branch", "changed", "changes", "completed", "context", "current", "files", "implementation", "inferred", "line", "lines", "normal", "product", "recorded", "repository", "repo", "task", "taskpilot", "updated", "work", "working":
		return true
	}
	return false
}

func repoTaskMatchConfidence(score int) float64 {
	switch {
	case score >= 160:
		return 0.98
	case score >= 120:
		return 0.9
	case score >= 80:
		return 0.78
	case score >= 55:
		return 0.58
	case score > 0:
		return 0.35
	default:
		return 0.2
	}
}

func repoTaskCandidateAction(score int) string {
	switch {
	case score >= 80:
		return "reuse"
	case score >= 55:
		return "create_related"
	default:
		return "create_provisional"
	}
}

func repoRelationshipForMatch(match repoTaskMatch) string {
	reasons := strings.Join(match.Reasons, " ")
	if strings.Contains(reasons, "active repo session") || strings.Contains(reasons, "semantic intent") {
		return "continues"
	}
	if match.Score >= 75 {
		return "related_to"
	}
	return "related_to"
}

func repoTaskIntelligenceDecision(action string, match repoTaskMatch) *TaskIntelligenceDecision {
	reason := strings.Join(match.Reasons, "; ")
	if reason == "" {
		reason = "no existing task reached the reuse threshold"
	}
	return &TaskIntelligenceDecision{
		Decision:       "repo_task_selection",
		Action:         action,
		SelectedTaskID: match.Task.ID,
		Confidence:     match.Confidence,
		Reason:         reason,
		Evidence:       match.Evidence,
		Candidates:     match.Candidates,
		Renamed:        match.Renamed,
		PreviousTitle:  match.PreviousTitle,
		NewTitle:       match.NewTitle,
	}
}

func shouldEnrichTaskIdentity(task Task, proposedTitle, proposedGoal string) bool {
	if strings.TrimSpace(proposedTitle) == "" || isGenericInferredTaskTitle(proposedTitle) {
		return false
	}
	if !isGenericInferredTaskTitle(task.Title) {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(task.Title), strings.TrimSpace(proposedTitle)) || (strings.TrimSpace(task.Goal) != "" && strings.TrimSpace(proposedGoal) != "")
}

func isGenericInferredTaskTitle(title string) bool {
	lower := strings.ToLower(strings.TrimSpace(title))
	return lower == "" ||
		lower == "inferred repo work" ||
		strings.HasPrefix(lower, "update ") ||
		strings.HasPrefix(lower, "work on repo branch") ||
		strings.HasPrefix(lower, "repository modifications") ||
		strings.Contains(lower, "changed files") ||
		strings.Contains(lower, "inferred work")
}

func isMeaningfulBranch(branch string) bool {
	branch = strings.TrimSpace(branch)
	return branch != "" && branch != "main" && branch != "master" && branch != "HEAD"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func humanizeBranchName(branch string) string {
	branch = strings.TrimSpace(branch)
	branch = taskIDPattern.ReplaceAllString(branch, "")
	branch = strings.Trim(branch, "-_/ ")
	branch = strings.ReplaceAll(branch, "-", " ")
	branch = strings.ReplaceAll(branch, "_", " ")
	branch = strings.Join(strings.Fields(branch), " ")
	if branch == "" {
		return "repo branch"
	}
	return branch
}

func branchMatchesTask(branch string, task Task) bool {
	branch = strings.ToLower(strings.ReplaceAll(branch, "-", " "))
	words := strings.Fields(strings.ToLower(task.Title + " " + task.Goal))
	matches := 0
	for _, word := range words {
		word = strings.Trim(word, ".,:;()[]{}")
		if len(word) < 4 {
			continue
		}
		if strings.Contains(branch, word) {
			matches++
		}
		if matches >= 2 {
			return true
		}
	}
	return false
}

func repoActivitySignature(activity repoActivity) string {
	states := gitChangedFileSnapshotIn(activity.Config.GitRoot)
	lines := []string{activity.Branch, activity.Commit}
	files := filterProductRepoFiles(activity.ChangedFiles)
	sort.Strings(files)
	for _, file := range files {
		state := states[file]
		lines = append(lines, strings.Join([]string{
			file,
			state.Status,
			strconv.FormatInt(state.Size, 10),
			strconv.FormatInt(state.ModTime, 10),
			state.Hash,
		}, "\x00"))
	}
	return strings.Join(lines, "\n")
}

func ensureRemoteRepository(projectID, name, root, branch string) (Repository, error) {
	var repos []Repository
	path := "/api/repositories"
	if projectID != "" {
		path += "?project_id=" + url.QueryEscape(projectID)
	}
	if err := request("GET", path, nil, &repos); err == nil {
		cleanRoot := cleanPath(root)
		for _, repo := range repos {
			if cleanPath(repo.Path) == cleanRoot || strings.EqualFold(repo.Name, name) {
				return repo, nil
			}
		}
	}
	var out Repository
	err := request("POST", "/api/repositories", map[string]any{"project_id": projectID, "name": name, "path": root, "default_branch": branch}, &out)
	return out, err
}

func ensureWorkspace(projectID, actorID string) (Workspace, error) {
	host, _ := os.Hostname()
	if host == "" {
		host = "local-machine"
	}
	var workspaces []Workspace
	path := "/api/workspaces"
	if projectID != "" {
		path += "?project_id=" + url.QueryEscape(projectID)
	}
	if err := request("GET", path, nil, &workspaces); err == nil {
		for _, workspace := range workspaces {
			if workspace.ActorID == actorID && strings.EqualFold(workspace.MachineName, host) {
				return workspace, nil
			}
		}
	}
	var out Workspace
	err := request("POST", "/api/workspaces", map[string]any{"project_id": projectID, "actor_id": actorID, "name": host, "machine_name": host, "kind": "local"}, &out)
	return out, err
}

func gitRoot(path string) (string, error) {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		if root, fallbackErr := taskPilotRepoRoot(path); fallbackErr == nil {
			return root, nil
		}
		return "", fmt.Errorf("not inside a Git repo; run this from a real Git repository")
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("could not detect Git root")
	}
	return filepath.Clean(root), nil
}

func taskPilotRepoRoot(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err == nil && !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	for {
		if _, err := os.Stat(repoConfigPath(abs)); err == nil {
			return filepath.Clean(abs), nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			break
		}
		abs = parent
	}
	return "", fmt.Errorf("repo is not TaskPilot-enabled")
}

func gitRemote(root string) string {
	out, err := exec.Command("git", "-C", root, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitDefaultBranch(root string) string {
	for _, args := range [][]string{{"symbolic-ref", "--short", "refs/remotes/origin/HEAD"}, {"branch", "--show-current"}} {
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
		if err == nil {
			branch := strings.TrimSpace(string(out))
			branch = strings.TrimPrefix(branch, "origin/")
			if branch != "" {
				return branch
			}
		}
	}
	return "main"
}

func gitBranch(root string) string {
	out, err := exec.Command("git", "-C", root, "branch", "--show-current").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitCommit(root string) string {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitChangedFileListIn(root string) []string {
	files := []string{}
	for path := range gitChangedFileSnapshotIn(root) {
		files = append(files, path)
	}
	sort.Strings(files)
	return files
}

func gitChangedFileSnapshotIn(root string) map[string]gitFileState {
	out := map[string]gitFileState{}
	cmd := exec.Command("git", "-C", root, "status", "--porcelain")
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
		status := strings.TrimSpace(line[:2])
		path := strings.TrimSpace(line[3:])
		if strings.Contains(path, " -> ") {
			parts := strings.Split(path, " -> ")
			path = parts[len(parts)-1]
		}
		if path == "" {
			continue
		}
		if isTaskPilotManagedRepoFile(path) {
			continue
		}
		state := gitFileState{Status: status}
		if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); err == nil {
			state.ModTime = info.ModTime().UnixNano()
			state.Size = info.Size()
			if !info.IsDir() {
				state.Hash = localFileSHA256(filepath.Join(root, filepath.FromSlash(path)))
			}
		}
		out[filepath.ToSlash(path)] = state
	}
	return out
}

func localFileSHA256(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return ""
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func isTaskPilotManagedRepoFile(path string) bool {
	path = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(path)), "./")
	switch path {
	case "AGENTS.md", "CLAUDE.md", "GEMINI.md", ".taskpilot", ".claude", ".codex", ".gemini":
		return true
	}
	for _, prefix := range []string{".taskpilot/", ".claude/", ".codex/", ".gemini/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func filterProductRepoFiles(files []string) []string {
	out := []string{}
	for _, file := range files {
		if strings.TrimSpace(file) == "" || isTaskPilotManagedRepoFile(file) {
			continue
		}
		out = append(out, file)
	}
	return uniqueStrings(out)
}

func repoConfigPath(root string) string {
	return filepath.Join(root, ".taskpilot", "repo.json")
}

func loadRepoConfig(root string) (repoEnableConfig, error) {
	var cfg repoEnableConfig
	data, err := os.ReadFile(repoConfigPath(root))
	if err != nil {
		return cfg, fmt.Errorf("repo is not TaskPilot-enabled; run `taskpilot enable` from the Git repo root")
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	cfg.GitRoot = root
	cfg.HookCommand = "taskpilot context render --repo . --format markdown"
	return cfg, nil
}

func saveRepoConfig(cfg repoEnableConfig) error {
	if err := ensureDir(filepath.Dir(repoConfigPath(cfg.GitRoot))); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(repoConfigPath(cfg.GitRoot), data, 0o600)
}

func daemonRegistryPath() string {
	return filepath.Join(taskpilotHomeDir(), "enabled-repos.json")
}

func loadDaemonRegistry() (daemonRegistry, error) {
	var reg daemonRegistry
	data, err := os.ReadFile(daemonRegistryPath())
	if os.IsNotExist(err) {
		return reg, nil
	}
	if err != nil {
		return reg, err
	}
	return reg, json.Unmarshal(data, &reg)
}

func saveDaemonRegistry(reg daemonRegistry) error {
	if err := ensureDir(filepath.Dir(daemonRegistryPath())); err != nil {
		return err
	}
	reg.Repos = uniqueStrings(cleanStrings(reg.Repos))
	sort.Strings(reg.Repos)
	data, _ := json.MarshalIndent(reg, "", "  ")
	return os.WriteFile(daemonRegistryPath(), data, 0o600)
}

func addEnabledRepo(root string) error {
	reg, err := loadDaemonRegistry()
	if err != nil {
		return err
	}
	reg.Repos = appendUniqueStrings(reg.Repos, filepath.Clean(root))
	return saveDaemonRegistry(reg)
}

func repoDaemonStopPath() string {
	return filepath.Join(taskpilotHomeDir(), "repo-daemon.stop")
}

func repoDaemonLogPath() string {
	return filepath.Join(taskpilotHomeDir(), "repo-daemon.log")
}

func repoDaemonErrLogPath() string {
	return filepath.Join(taskpilotHomeDir(), "repo-daemon.err.log")
}

func daemonInstallInfoPath() string {
	return filepath.Join(taskpilotHomeDir(), "daemon-install.json")
}

func daemonStopRequested(started time.Time) bool {
	info, err := os.Stat(repoDaemonStopPath())
	return err == nil && info.ModTime().After(started)
}

func installDaemonAutoStart() DaemonInstallResult {
	binary, err := currentExecutablePath()
	if err != nil {
		return daemonInstallFailure("unknown", "", "", "could not resolve current taskpilot binary", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return installDaemonLaunchd(binary)
	case "windows":
		return installDaemonWindowsTask(binary)
	case "linux":
		return installDaemonSystemd(binary)
	default:
		return DaemonInstallResult{Method: runtime.GOOS, BinaryPath: binary, Status: "unsupported", Message: "daemon auto-start is not supported on this platform", ManualCommand: binary + " daemon start"}
	}
}

func uninstallDaemonAutoStart() DaemonInstallResult {
	switch runtime.GOOS {
	case "darwin":
		return uninstallDaemonLaunchd()
	case "windows":
		return uninstallDaemonWindowsTask()
	case "linux":
		return uninstallDaemonSystemd()
	default:
		return DaemonInstallResult{Method: runtime.GOOS, Status: "unsupported", Message: "daemon auto-start uninstall is not supported on this platform"}
	}
}

func daemonDoctor() DaemonHealth {
	reg, _ := loadDaemonRegistry()
	var health DaemonHealth
	switch runtime.GOOS {
	case "darwin":
		health = doctorDaemonLaunchd(reg.Repos)
	case "windows":
		health = doctorDaemonWindowsTask(reg.Repos)
	case "linux":
		health = doctorDaemonSystemd(reg.Repos)
	default:
		health = DaemonHealth{Method: runtime.GOOS, ServiceName: daemonServiceName(), Installed: false, Running: false, Status: "unsupported", LogPath: repoDaemonLogPath(), EnabledRepos: reg.Repos, Message: "daemon auto-start is not supported on this platform"}
	}
	if msg := gitRepoDoctorMessage(reg.Repos); msg != "" {
		if health.Message != "" {
			health.Message += "; " + msg
		} else {
			health.Message = msg
		}
	}
	return health
}

func gitRepoDoctorMessage(repos []string) string {
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		out, err := exec.Command("git", "-C", repo, "status", "--porcelain").CombinedOutput()
		if err == nil {
			continue
		}
		text := strings.ToLower(string(out) + " " + err.Error())
		if strings.Contains(text, "dubious ownership") || strings.Contains(text, "safe.directory") {
			return "Git safe.directory is blocking repo capture for " + repo + "; run `git config --global --add safe.directory " + repo + "`"
		}
	}
	return ""
}

func currentExecutablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(exe)
}

func daemonServiceName() string {
	if runtime.GOOS == "darwin" {
		return "com.taskpilot.daemon"
	}
	if runtime.GOOS == "windows" {
		return "TaskPilotDaemon"
	}
	return "taskpilot-daemon"
}

func saveDaemonInstallInfo(info DaemonInstallInfo) error {
	if err := ensureDir(filepath.Dir(daemonInstallInfoPath())); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(info, "", "  ")
	return os.WriteFile(daemonInstallInfoPath(), data, 0o600)
}

func loadDaemonInstallInfo() (DaemonInstallInfo, error) {
	var info DaemonInstallInfo
	data, err := os.ReadFile(daemonInstallInfoPath())
	if err != nil {
		return info, err
	}
	return info, json.Unmarshal(data, &info)
}

func daemonInstallFailure(method, service, binary, message string, err error) DaemonInstallResult {
	out := DaemonInstallResult{Method: method, ServiceName: service, BinaryPath: binary, Status: "error", Message: message}
	if err != nil {
		out.Error = err.Error()
		if binary != "" {
			out.ManualCommand = binary + " daemon start"
		}
	}
	return out
}

func installDaemonLaunchd(binary string) DaemonInstallResult {
	service := daemonServiceName()
	path := macLaunchAgentPath()
	content := renderMacLaunchAgentPlist(service, binary, repoDaemonLogPath(), repoDaemonErrLogPath())
	if err := atomicWriteFile(path, []byte(content), 0o644); err != nil {
		return daemonInstallFailure("launchd", service, binary, "could not write launch agent", err)
	}
	started := false
	if uid := currentUserID(); uid != "" {
		_ = exec.Command("launchctl", "bootout", "gui/"+uid, path).Run()
		if err := exec.Command("launchctl", "bootstrap", "gui/"+uid, path).Run(); err == nil {
			_ = exec.Command("launchctl", "kickstart", "-k", "gui/"+uid+"/"+service).Run()
			started = true
		} else if err := exec.Command("launchctl", "load", "-w", path).Run(); err == nil {
			started = true
		}
	}
	info := DaemonInstallInfo{Method: "launchd", ServiceName: service, BinaryPath: binary, Command: []string{binary, "daemon", "run"}, LogPath: repoDaemonLogPath(), ErrorLogPath: repoDaemonErrLogPath(), InstalledAt: time.Now().UTC()}
	_ = saveDaemonInstallInfo(info)
	return DaemonInstallResult{Method: "launchd", ServiceName: service, BinaryPath: binary, Status: "installed", Changed: true, Started: started, ConfigPath: path, Message: daemonInstallMessage(started)}
}

func uninstallDaemonLaunchd() DaemonInstallResult {
	service := daemonServiceName()
	path := macLaunchAgentPath()
	if uid := currentUserID(); uid != "" {
		_ = exec.Command("launchctl", "bootout", "gui/"+uid, path).Run()
		_ = exec.Command("launchctl", "remove", service).Run()
	}
	_ = os.Remove(path)
	_ = os.Remove(daemonInstallInfoPath())
	return DaemonInstallResult{Method: "launchd", ServiceName: service, Status: "uninstalled", ConfigPath: path, Message: "TaskPilot launch agent removed; enabled repo state was kept"}
}

func doctorDaemonLaunchd(repos []string) DaemonHealth {
	service := daemonServiceName()
	path := macLaunchAgentPath()
	_, statErr := os.Stat(path)
	running := false
	if uid := currentUserID(); uid != "" {
		if err := exec.Command("launchctl", "print", "gui/"+uid+"/"+service).Run(); err == nil {
			running = true
		}
	}
	info, _ := loadDaemonInstallInfo()
	status := "ok"
	msg := ""
	if statErr != nil {
		status = "not_installed"
		msg = "run `taskpilot daemon install`"
	} else if !running {
		status = "installed_not_running"
		msg = "run `taskpilot daemon install` or inspect launchctl logs"
	}
	return DaemonHealth{Method: "launchd", ServiceName: service, BinaryPath: info.BinaryPath, Installed: statErr == nil, Running: running, Status: status, ConfigPath: path, LogPath: repoDaemonLogPath(), EnabledRepos: repos, Message: msg}
}

func installDaemonWindowsTask(binary string) DaemonInstallResult {
	service := daemonServiceName()
	taskCommand := windowsScheduledTaskCommand(binary)
	create := exec.Command("schtasks", "/Create", "/TN", service, "/TR", taskCommand, "/SC", "ONLOGON", "/RL", "LIMITED", "/F")
	if out, err := create.CombinedOutput(); err != nil {
		return installDaemonWindowsStartupFallback(binary, strings.TrimSpace(string(out)), err)
	}
	started := exec.Command("schtasks", "/Run", "/TN", service).Run() == nil
	info := DaemonInstallInfo{Method: "scheduled_task", ServiceName: service, BinaryPath: binary, Command: []string{binary, "daemon", "run"}, LogPath: repoDaemonLogPath(), InstalledAt: time.Now().UTC()}
	_ = saveDaemonInstallInfo(info)
	return DaemonInstallResult{Method: "scheduled_task", ServiceName: service, BinaryPath: binary, Status: "installed", Changed: true, Started: started, Message: daemonInstallMessage(started)}
}

func installDaemonWindowsStartupFallback(binary, schtasksOutput string, schtasksErr error) DaemonInstallResult {
	service := daemonServiceName()
	path := windowsStartupDaemonPath()
	if path == "" {
		return daemonInstallFailure("windows_startup_folder", service, binary, "could not locate Windows Startup folder after scheduled task failed: "+schtasksOutput, schtasksErr)
	}
	content := renderWindowsStartupDaemonCmd(binary, repoDaemonLogPath(), repoDaemonErrLogPath())
	if err := atomicWriteFile(path, []byte(content), 0o644); err != nil {
		return daemonInstallFailure("windows_startup_folder", service, binary, "could not write Startup folder launcher after scheduled task failed: "+schtasksOutput, err)
	}
	started := startRepoDaemonProcess(10*time.Second) == nil
	info := DaemonInstallInfo{Method: "windows_startup_folder", ServiceName: service, BinaryPath: binary, Command: []string{binary, "daemon", "run"}, LogPath: repoDaemonLogPath(), ErrorLogPath: repoDaemonErrLogPath(), InstalledAt: time.Now().UTC()}
	_ = saveDaemonInstallInfo(info)
	msg := daemonInstallMessage(started) + "; used Startup folder fallback because Scheduled Task failed"
	if schtasksOutput != "" {
		msg += ": " + schtasksOutput
	}
	return DaemonInstallResult{Method: "windows_startup_folder", ServiceName: service, BinaryPath: binary, Status: "installed", Changed: true, Started: started, ConfigPath: path, Message: msg}
}

func uninstallDaemonWindowsTask() DaemonInstallResult {
	service := daemonServiceName()
	_ = exec.Command("schtasks", "/End", "/TN", service).Run()
	_ = exec.Command("schtasks", "/Delete", "/TN", service, "/F").Run()
	if path := windowsStartupDaemonPath(); path != "" {
		_ = os.Remove(path)
	}
	_ = os.Remove(daemonInstallInfoPath())
	return DaemonInstallResult{Method: "windows", ServiceName: service, Status: "uninstalled", Message: "TaskPilot auto-start entries removed; enabled repo state was kept"}
}

func doctorDaemonWindowsTask(repos []string) DaemonHealth {
	service := daemonServiceName()
	query := exec.Command("schtasks", "/Query", "/TN", service, "/FO", "LIST", "/V")
	out, err := query.CombinedOutput()
	installed := err == nil
	text := strings.ToLower(string(out))
	running := installed && strings.Contains(text, "status:") && strings.Contains(text, "running")
	method := "scheduled_task"
	configPath := ""
	if !installed {
		if path := windowsStartupDaemonPath(); path != "" {
			if _, statErr := os.Stat(path); statErr == nil {
				installed = true
				method = "windows_startup_folder"
				configPath = path
			}
		}
	}
	info, _ := loadDaemonInstallInfo()
	status := "ok"
	msg := ""
	if !installed {
		status = "not_installed"
		msg = "run `taskpilot daemon install`"
	} else if !running && method == "scheduled_task" {
		status = "installed_not_running"
		msg = "scheduled task is installed and will start at next login; run `taskpilot daemon install` to start it now"
	} else if method == "windows_startup_folder" {
		status = "installed"
		msg = "Startup folder launcher is installed; it will start TaskPilot at next login"
	}
	return DaemonHealth{Method: method, ServiceName: service, BinaryPath: info.BinaryPath, Installed: installed, Running: running, Status: status, ConfigPath: configPath, LogPath: repoDaemonLogPath(), EnabledRepos: repos, Message: msg}
}

func installDaemonSystemd(binary string) DaemonInstallResult {
	service := daemonServiceName()
	path := linuxSystemdUnitPath()
	content := renderLinuxSystemdUnit(service, binary)
	if err := atomicWriteFile(path, []byte(content), 0o644); err != nil {
		return daemonInstallFailure("systemd_user", service, binary, "could not write user systemd unit", err)
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return daemonInstallFailure("systemd_user", service, binary, "could not reload user systemd: "+strings.TrimSpace(string(out)), err)
	}
	started := exec.Command("systemctl", "--user", "enable", "--now", service+".service").Run() == nil
	info := DaemonInstallInfo{Method: "systemd_user", ServiceName: service, BinaryPath: binary, Command: []string{binary, "daemon", "run"}, LogPath: repoDaemonLogPath(), InstalledAt: time.Now().UTC()}
	_ = saveDaemonInstallInfo(info)
	return DaemonInstallResult{Method: "systemd_user", ServiceName: service, BinaryPath: binary, Status: "installed", Changed: true, Started: started, ConfigPath: path, Message: daemonInstallMessage(started)}
}

func uninstallDaemonSystemd() DaemonInstallResult {
	service := daemonServiceName()
	_ = exec.Command("systemctl", "--user", "disable", "--now", service+".service").Run()
	_ = os.Remove(linuxSystemdUnitPath())
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	_ = os.Remove(daemonInstallInfoPath())
	return DaemonInstallResult{Method: "systemd_user", ServiceName: service, Status: "uninstalled", ConfigPath: linuxSystemdUnitPath(), Message: "TaskPilot user service removed; enabled repo state was kept"}
}

func doctorDaemonSystemd(repos []string) DaemonHealth {
	service := daemonServiceName()
	_, statErr := os.Stat(linuxSystemdUnitPath())
	running := exec.Command("systemctl", "--user", "is-active", "--quiet", service+".service").Run() == nil
	info, _ := loadDaemonInstallInfo()
	status := "ok"
	msg := ""
	if statErr != nil {
		status = "not_installed"
		msg = "run `taskpilot daemon install`"
	} else if !running {
		status = "installed_not_running"
		msg = "run `systemctl --user start " + service + ".service`"
	}
	return DaemonHealth{Method: "systemd_user", ServiceName: service, BinaryPath: info.BinaryPath, Installed: statErr == nil, Running: running, Status: status, ConfigPath: linuxSystemdUnitPath(), LogPath: repoDaemonLogPath(), EnabledRepos: repos, Message: msg}
}

func daemonInstallMessage(started bool) string {
	if started {
		return "daemon installed and running; it will start automatically on login"
	}
	return "daemon installed for login auto-start, but could not be started immediately; run `taskpilot daemon doctor`"
}

func macLaunchAgentPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", daemonServiceName()+".plist")
}

func linuxSystemdUnitPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", daemonServiceName()+".service")
}

func currentUserID() string {
	out, err := exec.Command("id", "-u").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func renderMacLaunchAgentPlist(label, binary, logPath, errLogPath string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + xmlEscape(label) + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + xmlEscape(binary) + `</string>
    <string>daemon</string>
    <string>run</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>` + xmlEscape(logPath) + `</string>
  <key>StandardErrorPath</key>
  <string>` + xmlEscape(errLogPath) + `</string>
</dict>
</plist>
`
}

func renderLinuxSystemdUnit(service, binary string) string {
	return `[Unit]
Description=TaskPilot repo daemon
After=network-online.target

[Service]
Type=simple
ExecStart=` + systemdEscape(binary) + ` daemon run
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`
}

func windowsScheduledTaskCommand(binary string) string {
	return `"` + binary + `" daemon run`
}

func windowsStartupDaemonPath() string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		home, _ := os.UserHomeDir()
		if home == "" {
			return ""
		}
		appData = filepath.Join(home, "AppData", "Roaming")
	}
	return filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "TaskPilotDaemon.cmd")
}

func renderWindowsStartupDaemonCmd(binary, logPath, errLogPath string) string {
	return "@echo off\r\n" +
		"start \"TaskPilot Daemon\" /min \"" + binary + "\" daemon run >> \"" + logPath + "\" 2>> \"" + errLogPath + "\"\r\n"
}

func xmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return replacer.Replace(value)
}

func systemdEscape(value string) string {
	if !strings.ContainsAny(value, " \t\n\"'\\") {
		return value
	}
	return `"` + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`) + `"`
}

func writeHookScripts(cfg repoEnableConfig) error {
	dir := filepath.Join(cfg.GitRoot, ".taskpilot", "hooks")
	if err := ensureDir(dir); err != nil {
		return err
	}
	shScript := "#!/bin/sh\nSCRIPT_DIR=$(CDPATH= cd -- \"$(dirname -- \"$0\")\" && pwd)\nREPO_ROOT=$(CDPATH= cd -- \"$SCRIPT_DIR/../..\" && pwd)\nexec taskpilot context render --repo \"$REPO_ROOT\" --format markdown\n"
	cmdScript := "@echo off\r\nset \"SCRIPT_DIR=%~dp0\"\r\nfor %%I in (\"%SCRIPT_DIR%..\\..\") do set \"REPO_ROOT=%%~fI\"\r\ntaskpilot context render --repo \"%REPO_ROOT%\" --format markdown\r\n"
	if err := os.WriteFile(filepath.Join(dir, "session-start.sh"), []byte(shScript), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "session-start.cmd"), []byte(cmdScript), 0o755); err != nil {
		return err
	}
	for _, profile := range knownAgentProfiles {
		if err := os.WriteFile(filepath.Join(dir, profile.Name+"-session-start.sh"), []byte(shScript), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, profile.Name+"-session-start.cmd"), []byte(cmdScript), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func agentAdapters() []AgentAdapter {
	out := []AgentAdapter{}
	for _, profile := range knownAgentProfiles {
		if profile.NativeJSONHooks {
			out = append(out, jsonHookAdapter{name: profile.Name, binary: profile.Binaries[0], configRel: profile.ConfigRel, hookRel: ".taskpilot/hooks/" + profile.Name + "-session-start", format: profile.Format, jsonPath: profile.JSONPath, finalPaths: profile.FinalPaths, supported: true, extraNote: profile.ExtraNote})
			continue
		}
		if profile.OpenCodePlugin {
			out = append(out, opencodePluginAdapter{name: profile.Name, binaries: profile.Binaries, configRel: profile.ConfigRel, extraNote: profile.ExtraNote})
			continue
		}
		if profile.HermesShellHook {
			out = append(out, hermesShellHookAdapter{name: profile.Name, binaries: profile.Binaries, extraNote: profile.ExtraNote})
			continue
		}
		out = append(out, unsupportedAgentAdapter{name: profile.Name, manualNote: profile.ManualSetupNote})
	}
	return out
}

func configureAgentAdapters(repo repoEnableConfig, dryRun bool, names []string) []AgentConfigureResult {
	out := []AgentConfigureResult{}
	for _, adapter := range selectedAgentAdapters(names) {
		out = append(out, adapter.Configure(repo, dryRun))
	}
	return out
}

func doctorAgentAdapters(repo repoEnableConfig) []AgentHealth {
	out := []AgentHealth{}
	for _, adapter := range agentAdapters() {
		out = append(out, adapter.Doctor(repo))
	}
	return out
}

func selectedAgentAdapters(names []string) []AgentAdapter {
	all := agentAdapters()
	wanted := map[string]bool{}
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if name == "all" {
			return all
		}
		wanted[name] = true
	}
	out := []AgentAdapter{}
	for _, adapter := range all {
		if wanted[adapter.Name()] {
			out = append(out, adapter)
		}
	}
	for name := range wanted {
		found := false
		for _, adapter := range out {
			if adapter.Name() == name {
				found = true
				break
			}
		}
		if !found {
			out = append(out, unsupportedAgentAdapter{name: name})
		}
	}
	return out
}

func (a jsonHookAdapter) Name() string { return a.name }

func (a jsonHookAdapter) Detect(repo repoEnableConfig) AgentDetection {
	bin, err := exec.LookPath(a.binary)
	return AgentDetection{
		Name:        a.name,
		Installed:   err == nil,
		Binary:      bin,
		ConfigPath:  filepath.Join(repo.GitRoot, a.configRel),
		HookCommand: a.command(),
		Supported:   a.supported,
	}
}

func (a jsonHookAdapter) Configure(repo repoEnableConfig, dryRun bool) AgentConfigureResult {
	detection := a.Detect(repo)
	result := AgentConfigureResult{Name: a.name, ConfigPath: detection.ConfigPath, HookCommand: detection.HookCommand, DryRun: dryRun}
	if !a.supported {
		result.Status = "unsupported"
		result.Message = "agent adapter is not supported yet"
		result.ManualFallback = a.ManualInstructions(repo)
		return result
	}
	writeResult, err := mergeSessionStartHookJSON(detection.ConfigPath, a.jsonPath, SessionStartHook{Type: "command", Command: a.command()}, dryRun, a.legacyCommands()...)
	if err != nil {
		result.Status = "error"
		result.Message = err.Error()
		result.ManualFallback = a.ManualInstructions(repo)
		return result
	}
	finalChanged := false
	for _, path := range a.finalPaths {
		finalResult, err := mergeSessionStartHookJSON(detection.ConfigPath, path, SessionStartHook{Type: "command", Command: a.finalCommand()}, dryRun)
		if err != nil {
			result.Status = "error"
			result.Message = err.Error()
			result.ManualFallback = a.ManualInstructions(repo)
			return result
		}
		finalChanged = finalChanged || finalResult.Changed
		if result.BackupPath == "" {
			result.BackupPath = finalResult.BackupPath
		}
	}
	writeResult.Changed = writeResult.Changed || finalChanged
	mcpChanged := false
	if a.name == "codex" {
		mcpResult, err := ensureCodexTaskPilotMCPConfig(dryRun)
		result.MCPConfigPath = mcpResult.ConfigPath
		if err != nil {
			result.Status = "error"
			result.Message = err.Error()
			result.ManualFallback = a.ManualInstructions(repo)
			return result
		}
		mcpChanged = mcpResult.Changed
	}
	writeResult.Changed = writeResult.Changed || mcpChanged
	result.Changed = writeResult.Changed
	if result.BackupPath == "" {
		result.BackupPath = writeResult.BackupPath
	}
	if dryRun {
		result.Status = "dry_run"
		if writeResult.Changed {
			result.Message = fmt.Sprintf("would register %s session hooks in %s", a.name, a.configRel)
		} else {
			result.Message = fmt.Sprintf("%s session hooks already registered in %s", a.name, a.configRel)
		}
		return result
	}
	if writeResult.Changed {
		result.Status = "configured"
		result.Message = fmt.Sprintf("registered %s session hooks in %s", a.name, a.configRel)
		if a.name == "codex" {
			result.Message += "; registered TaskPilot MCP semantic-memory tools in Codex config"
		}
		if a.extraNote != "" {
			result.Message += "; " + a.extraNote
		}
		return result
	}
	result.Status = "already_configured"
	result.Message = fmt.Sprintf("%s session hooks already registered in %s", a.name, a.configRel)
	if a.name == "codex" {
		result.Message += "; TaskPilot MCP semantic-memory tools already registered in Codex config"
	}
	return result
}

func (a jsonHookAdapter) Doctor(repo repoEnableConfig) AgentHealth {
	detection := a.Detect(repo)
	registered := jsonConfigHasHookCommand(detection.ConfigPath, a.jsonPath, a.command())
	finalRegistered := len(a.finalPaths) == 0
	for _, path := range a.finalPaths {
		if jsonConfigHasHookCommand(detection.ConfigPath, path, a.finalCommand()) {
			finalRegistered = true
			break
		}
	}
	legacyRegistered := false
	for _, legacy := range a.legacyCommands() {
		if jsonConfigHasHookCommand(detection.ConfigPath, a.jsonPath, legacy) {
			legacyRegistered = true
			break
		}
	}
	status := "ok"
	msg := ""
	if !registered && legacyRegistered {
		status = "legacy_registered"
		msg = "run `taskpilot agent configure " + a.name + "` to upgrade to the cross-platform hook command"
	} else if !registered {
		status = "not_registered"
		msg = "run `taskpilot agent configure " + a.name + "`"
	} else if !finalRegistered {
		status = "partial"
		msg = "session-start hook is registered, but memory checkpoint hook is missing; run `taskpilot agent configure " + a.name + "`"
	} else if !detection.Installed {
		status = "agent_not_on_path"
		msg = "hook is registered, but the agent binary was not found on PATH"
	}
	health := AgentHealth{Name: a.name, Installed: detection.Installed, ConfigPath: detection.ConfigPath, HookScriptPath: "", HookScriptExists: true, Registered: registered, Status: status, Message: msg}
	if a.name == "codex" {
		mcpHealth := codexTaskPilotMCPHealth()
		health.MCPRegistered = mcpHealth.OK
		health.MCPConfigPath = mcpHealth.ConfigPath
		if !mcpHealth.OK {
			health.Status = "partial"
			if health.Message != "" {
				health.Message += "; "
			}
			health.Message += mcpHealth.Message
		}
	}
	return health
}

func (a jsonHookAdapter) ManualInstructions(repo repoEnableConfig) string {
	if a.name == "codex" {
		return fmt.Sprintf("Manual setup for %s: add a session-start command hook to %s that runs %s, a session-end hook that runs %s, and register Codex MCP server `taskpilot` with command `taskpilot mcp serve` including tool `record_repo_semantic_memory`.", a.name, filepath.Join(repo.GitRoot, a.configRel), a.command(), a.finalCommand())
	}
	return fmt.Sprintf("Manual setup for %s: add a session-start command hook to %s that runs %s, and a session-end hook that runs %s.", a.name, filepath.Join(repo.GitRoot, a.configRel), a.command(), a.finalCommand())
}

func (a opencodePluginAdapter) Name() string { return a.name }

func (a opencodePluginAdapter) Detect(repo repoEnableConfig) AgentDetection {
	bin := ""
	installed := false
	for _, binary := range a.binaries {
		if found, err := exec.LookPath(binary); err == nil {
			bin = found
			installed = true
			break
		}
	}
	return AgentDetection{
		Name:        a.name,
		Installed:   installed,
		Binary:      bin,
		ConfigPath:  filepath.Join(repo.GitRoot, a.configRel),
		HookCommand: "OpenCode project plugin",
		Supported:   true,
	}
}

func (a opencodePluginAdapter) Configure(repo repoEnableConfig, dryRun bool) AgentConfigureResult {
	detection := a.Detect(repo)
	result := AgentConfigureResult{Name: a.name, ConfigPath: detection.ConfigPath, HookCommand: detection.HookCommand, DryRun: dryRun}
	plugin := []byte(renderOpenCodeTaskPilotPlugin())
	existing, err := os.ReadFile(detection.ConfigPath)
	if os.IsNotExist(err) {
		existing = nil
	} else if err != nil {
		result.Status = "error"
		result.Message = err.Error()
		result.ManualFallback = a.ManualInstructions(repo)
		return result
	}
	changed := !bytes.Equal(bytes.TrimSpace(existing), bytes.TrimSpace(plugin))
	result.Changed = changed
	if dryRun {
		result.Status = "dry_run"
		if changed {
			result.Message = fmt.Sprintf("would register %s native OpenCode plugin in %s", a.name, a.configRel)
		} else {
			result.Message = fmt.Sprintf("%s native OpenCode plugin already registered in %s", a.name, a.configRel)
		}
		return result
	}
	if !changed {
		result.Status = "already_configured"
		result.Message = fmt.Sprintf("%s native OpenCode plugin already registered in %s", a.name, a.configRel)
		return result
	}
	if err := ensureDir(filepath.Dir(detection.ConfigPath)); err != nil {
		result.Status = "error"
		result.Message = err.Error()
		result.ManualFallback = a.ManualInstructions(repo)
		return result
	}
	if len(existing) > 0 {
		backup, err := ensureOneTimeBackup(detection.ConfigPath)
		if err != nil {
			result.Status = "error"
			result.Message = err.Error()
			result.ManualFallback = a.ManualInstructions(repo)
			return result
		}
		result.BackupPath = backup
	}
	if err := atomicWriteFile(detection.ConfigPath, plugin, 0o644); err != nil {
		result.Status = "error"
		result.Message = err.Error()
		result.ManualFallback = a.ManualInstructions(repo)
		return result
	}
	result.Status = "configured"
	result.Message = fmt.Sprintf("registered %s native OpenCode plugin in %s", a.name, a.configRel)
	if a.extraNote != "" {
		result.Message += "; " + a.extraNote
	}
	return result
}

func (a opencodePluginAdapter) Doctor(repo repoEnableConfig) AgentHealth {
	detection := a.Detect(repo)
	registered := openCodePluginRegistered(detection.ConfigPath)
	status := "ok"
	msg := ""
	if !registered {
		status = "not_registered"
		msg = "run `taskpilot agent configure " + a.name + "`"
	} else if !detection.Installed {
		status = "agent_not_on_path"
		msg = "native OpenCode plugin is registered, but the OpenCode binary was not found on PATH"
	}
	return AgentHealth{Name: a.name, Installed: detection.Installed, ConfigPath: detection.ConfigPath, HookScriptPath: detection.ConfigPath, HookScriptExists: registered, Registered: registered, Status: status, Message: msg}
}

func (a opencodePluginAdapter) ManualInstructions(repo repoEnableConfig) string {
	return fmt.Sprintf("Manual setup for %s: create %s with a project plugin that appends `taskpilot context render --repo <repo> --format markdown` output through `experimental.chat.system.transform` and checkpoints on `session.idle`.", a.name, filepath.Join(repo.GitRoot, a.configRel))
}

func openCodePluginRegistered(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	text := string(data)
	return strings.Contains(text, "TaskPilotOpenCodePlugin") &&
		strings.Contains(text, "experimental.chat.system.transform") &&
		strings.Contains(text, "session.idle") &&
		strings.Contains(text, "taskpilot context render") &&
		strings.Contains(text, "taskpilot context checkpoint")
}

func renderOpenCodeTaskPilotPlugin() string {
	return `// Managed by TaskPilot. Re-run ` + "`taskpilot agent configure opencode`" + ` to refresh.
export const TaskPilotOpenCodePlugin = async ({ $, directory, worktree, client }) => {
  const repo = worktree || directory || "."

  const renderContext = async () => {
    try {
      return await ` + "$" + "`taskpilot context render --repo ${repo} --format markdown`" + `.text()
    } catch (err) {
      const message = err && err.message ? err.message : String(err)
      return "## TaskPilot Live Repo Context\n\nTaskPilot context render failed: " + message
    }
  }

  const checkpoint = async (reason) => {
    try {
      await ` + "$" + "`taskpilot context checkpoint --repo ${repo} --source agent-hook --reason ${reason}`" + `.quiet()
    } catch (err) {
      if (client && client.app && client.app.log) {
        await client.app.log({
          body: {
            service: "taskpilot",
            level: "warn",
            message: "TaskPilot checkpoint failed",
            extra: { reason, error: err && err.message ? err.message : String(err) },
          },
        })
      }
    }
  }

  return {
    "experimental.chat.system.transform": async (_input, output) => {
      output.system.push(await renderContext())
    },
    event: async ({ event }) => {
      if (event.type === "session.idle") {
        await checkpoint("session_idle")
      }
    },
  }
}
`
}

func (a hermesShellHookAdapter) Name() string { return a.name }

func (a hermesShellHookAdapter) Detect(repo repoEnableConfig) AgentDetection {
	bin := ""
	installed := false
	for _, binary := range a.binaries {
		if found, err := exec.LookPath(binary); err == nil {
			bin = found
			installed = true
			break
		}
	}
	return AgentDetection{
		Name:        a.name,
		Installed:   installed,
		Binary:      bin,
		ConfigPath:  hermesConfigPath(),
		HookCommand: "Hermes shell hooks",
		Supported:   true,
	}
}

func (a hermesShellHookAdapter) Configure(repo repoEnableConfig, dryRun bool) AgentConfigureResult {
	detection := a.Detect(repo)
	result := AgentConfigureResult{Name: a.name, ConfigPath: detection.ConfigPath, HookCommand: detection.HookCommand, DryRun: dryRun}
	contextScript, checkpointScript := hermesTaskPilotHookPaths()
	contextBody := []byte(renderHermesTaskPilotContextHook())
	checkpointBody := []byte(renderHermesTaskPilotCheckpointHook())
	config, err := os.ReadFile(detection.ConfigPath)
	if os.IsNotExist(err) {
		config = nil
	} else if err != nil {
		result.Status = "error"
		result.Message = err.Error()
		result.ManualFallback = a.ManualInstructions(repo)
		return result
	}
	updated := string(config)
	changed := false
	var hookChanged bool
	updated, hookChanged = ensureHermesHookYAML(updated, "pre_llm_call", contextScript, 20)
	changed = changed || hookChanged
	updated, hookChanged = ensureHermesHookYAML(updated, "on_session_end", checkpointScript, 20)
	changed = changed || hookChanged
	if existing, err := os.ReadFile(contextScript); err != nil || !bytes.Equal(existing, contextBody) {
		changed = true
	}
	if existing, err := os.ReadFile(checkpointScript); err != nil || !bytes.Equal(existing, checkpointBody) {
		changed = true
	}
	result.Changed = changed
	if dryRun {
		result.Status = "dry_run"
		if changed {
			result.Message = fmt.Sprintf("would register %s native Hermes shell hooks in %s", a.name, detection.ConfigPath)
		} else {
			result.Message = fmt.Sprintf("%s native Hermes shell hooks already registered in %s", a.name, detection.ConfigPath)
		}
		return result
	}
	if !changed {
		result.Status = "already_configured"
		result.Message = fmt.Sprintf("%s native Hermes shell hooks already registered in %s", a.name, detection.ConfigPath)
		return result
	}
	for _, write := range []struct {
		path string
		body []byte
		mode os.FileMode
	}{
		{contextScript, contextBody, 0o755},
		{checkpointScript, checkpointBody, 0o755},
	} {
		if err := ensureDir(filepath.Dir(write.path)); err != nil {
			result.Status = "error"
			result.Message = err.Error()
			result.ManualFallback = a.ManualInstructions(repo)
			return result
		}
		if err := atomicWriteFile(write.path, write.body, write.mode); err != nil {
			result.Status = "error"
			result.Message = err.Error()
			result.ManualFallback = a.ManualInstructions(repo)
			return result
		}
	}
	if err := ensureDir(filepath.Dir(detection.ConfigPath)); err != nil {
		result.Status = "error"
		result.Message = err.Error()
		result.ManualFallback = a.ManualInstructions(repo)
		return result
	}
	if len(config) > 0 && !bytes.Equal(bytes.TrimSpace(config), bytes.TrimSpace([]byte(updated))) {
		backup, err := ensureOneTimeBackup(detection.ConfigPath)
		if err != nil {
			result.Status = "error"
			result.Message = err.Error()
			result.ManualFallback = a.ManualInstructions(repo)
			return result
		}
		result.BackupPath = backup
	}
	if err := atomicWriteFile(detection.ConfigPath, []byte(updated), 0o600); err != nil {
		result.Status = "error"
		result.Message = err.Error()
		result.ManualFallback = a.ManualInstructions(repo)
		return result
	}
	result.Status = "configured"
	result.Message = fmt.Sprintf("registered %s native Hermes shell hooks in %s", a.name, detection.ConfigPath)
	if a.extraNote != "" {
		result.Message += "; " + a.extraNote
	}
	return result
}

func (a hermesShellHookAdapter) Doctor(repo repoEnableConfig) AgentHealth {
	detection := a.Detect(repo)
	registered := hermesShellHooksRegistered()
	status := "ok"
	msg := ""
	if !registered {
		status = "not_registered"
		msg = "run `taskpilot agent configure " + a.name + "`"
	} else if !detection.Installed {
		status = "agent_not_on_path"
		msg = "native Hermes shell hooks are registered, but the Hermes binary was not found on PATH"
	}
	return AgentHealth{Name: a.name, Installed: detection.Installed, ConfigPath: detection.ConfigPath, HookScriptPath: filepath.Dir(hermesTaskPilotContextHookPath()), HookScriptExists: registered, Registered: registered, Status: status, Message: msg}
}

func (a hermesShellHookAdapter) ManualInstructions(repo repoEnableConfig) string {
	contextScript, checkpointScript := hermesTaskPilotHookPaths()
	return fmt.Sprintf("Manual setup for %s: create executable scripts %s and %s, then add them under hooks.pre_llm_call and hooks.on_session_end in %s.", a.name, contextScript, checkpointScript, hermesConfigPath())
}

func hermesConfigPath() string {
	home := strings.TrimSpace(os.Getenv("HERMES_HOME"))
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(userHome, ".hermes")
		}
	}
	if home == "" {
		home = ".hermes"
	}
	return filepath.Join(home, "config.yaml")
}

func hermesTaskPilotContextHookPath() string {
	contextScript, _ := hermesTaskPilotHookPaths()
	return contextScript
}

func hermesTaskPilotHookPaths() (string, string) {
	home := filepath.Dir(hermesConfigPath())
	dir := filepath.Join(home, "agent-hooks")
	return filepath.Join(dir, "taskpilot-context.py"), filepath.Join(dir, "taskpilot-checkpoint.py")
}

func hermesShellHooksRegistered() bool {
	contextScript, checkpointScript := hermesTaskPilotHookPaths()
	data, err := os.ReadFile(hermesConfigPath())
	if err != nil {
		return false
	}
	text := string(data)
	if !strings.Contains(text, strconv.Quote(contextScript)) || !strings.Contains(text, strconv.Quote(checkpointScript)) {
		return false
	}
	contextData, err := os.ReadFile(contextScript)
	if err != nil || !pythonHookContainsTaskPilotCommand(string(contextData), "render") {
		return false
	}
	checkpointData, err := os.ReadFile(checkpointScript)
	return err == nil && pythonHookContainsTaskPilotCommand(string(checkpointData), "checkpoint")
}

func pythonHookContainsTaskPilotCommand(text, subcommand string) bool {
	return strings.Contains(text, strconv.Quote("taskpilot")) &&
		strings.Contains(text, strconv.Quote("context")) &&
		strings.Contains(text, strconv.Quote(subcommand))
}

func ensureHermesHookYAML(text, event, command string, timeout int) (string, bool) {
	if strings.Contains(text, strconv.Quote(command)) || strings.Contains(text, command) {
		return text, false
	}
	entry := []string{
		"    - command: " + strconv.Quote(command),
		fmt.Sprintf("      timeout: %d", timeout),
	}
	lines := splitYAMLLines(text)
	hooksIdx := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "hooks:" {
			hooksIdx = i
			break
		}
	}
	if hooksIdx < 0 {
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, "hooks:", "  "+event+":")
		lines = append(lines, entry...)
		return strings.Join(lines, "\n") + "\n", true
	}
	hooksEnd := len(lines)
	for i := hooksIdx + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(lines[i], " ") && !strings.HasPrefix(lines[i], "\t") {
			hooksEnd = i
			break
		}
	}
	eventIdx := -1
	for i := hooksIdx + 1; i < hooksEnd; i++ {
		if strings.TrimSpace(lines[i]) == event+":" && strings.HasPrefix(lines[i], "  ") {
			eventIdx = i
			break
		}
	}
	if eventIdx < 0 {
		insert := append([]string{"  " + event + ":"}, entry...)
		lines = insertLines(lines, hooksEnd, insert)
		return strings.Join(lines, "\n") + "\n", true
	}
	eventEnd := hooksEnd
	for i := eventIdx + 1; i < hooksEnd; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(lines[i], "  ") && !strings.HasPrefix(lines[i], "    ") && strings.HasSuffix(trimmed, ":") {
			eventEnd = i
			break
		}
	}
	lines = insertLines(lines, eventEnd, entry)
	return strings.Join(lines, "\n") + "\n", true
}

func splitYAMLLines(text string) []string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func insertLines(lines []string, idx int, insert []string) []string {
	out := append([]string{}, lines[:idx]...)
	out = append(out, insert...)
	return append(out, lines[idx:]...)
}

func renderHermesTaskPilotContextHook() string {
	return `#!/usr/bin/env python3
import json
import os
import subprocess
import sys

try:
    payload = json.load(sys.stdin)
except Exception:
    payload = {}

repo = payload.get("cwd") or os.getcwd()
try:
    result = subprocess.run(
        ["taskpilot", "context", "render", "--repo", repo, "--format", "markdown"],
        capture_output=True,
        text=True,
        timeout=20,
    )
    if result.returncode == 0:
        context = result.stdout
    else:
        context = "## TaskPilot Live Repo Context\n\nTaskPilot context render failed:\n" + (result.stderr or result.stdout)
except Exception as exc:
    context = "## TaskPilot Live Repo Context\n\nTaskPilot context render failed: " + str(exc)

print(json.dumps({"context": context}))
`
}

func renderHermesTaskPilotCheckpointHook() string {
	return `#!/usr/bin/env python3
import json
import os
import subprocess
import sys

try:
    payload = json.load(sys.stdin)
except Exception:
    payload = {}

repo = payload.get("cwd") or os.getcwd()
try:
    subprocess.run(
        ["taskpilot", "context", "checkpoint", "--repo", repo, "--source", "agent-hook", "--reason", "session_end"],
        capture_output=True,
        text=True,
        timeout=20,
    )
except Exception:
    pass

print("{}")
`
}

type codexMCPConfigResult struct {
	ConfigPath string
	Changed    bool
}

type codexMCPHealthResult struct {
	ConfigPath string
	OK         bool
	Message    string
}

var probeCodexTaskPilotMCP = probeCodexTaskPilotMCPServer

var requiredCodexTaskPilotMCPTools = []string{
	"get_current_repo_context",
	"ensure_task_for_repo_session",
	"record_repo_session_context",
	"record_repo_semantic_memory",
	"add_task_relationship",
}

func ensureCodexTaskPilotMCPConfig(dryRun bool) (codexMCPConfigResult, error) {
	path := codexConfigPath()
	original, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		original = nil
	} else if err != nil {
		return codexMCPConfigResult{ConfigPath: path}, err
	}
	updated := string(original)
	command := codexTaskPilotCommand()
	changed := false
	updated, changed = ensureTomlTableKey(updated, "mcp_servers.taskpilot", "command", strconv.Quote(command))
	changedAny := changed
	updated, changed = ensureTomlTableKey(updated, "mcp_servers.taskpilot", "args", `["mcp", "serve"]`)
	changedAny = changedAny || changed
	updated, changed = ensureTomlTableKey(updated, "mcp_servers.taskpilot.env", "TASKPILOT_CONFIG", strconv.Quote(taskPilotConfigEnvPath()))
	changedAny = changedAny || changed
	for _, tool := range requiredCodexTaskPilotMCPTools {
		updated, changed = ensureTomlTableKey(updated, "mcp_servers.taskpilot.tools."+tool, "approval_mode", `"approve"`)
		changedAny = changedAny || changed
	}
	if !changedAny || dryRun {
		return codexMCPConfigResult{ConfigPath: path, Changed: changedAny}, nil
	}
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return codexMCPConfigResult{ConfigPath: path}, err
	}
	if len(original) > 0 {
		backup := path + ".taskpilot.bak"
		if _, err := os.Stat(backup); os.IsNotExist(err) {
			_ = os.WriteFile(backup, original, 0o600)
		}
	}
	return codexMCPConfigResult{ConfigPath: path, Changed: true}, atomicWriteFile(path, []byte(updated), 0o600)
}

func codexTaskPilotMCPHealth() codexMCPHealthResult {
	path := codexConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return codexMCPHealthResult{ConfigPath: path, OK: false, Message: "TaskPilot MCP is not configured for Codex; run `taskpilot agent configure codex`"}
	}
	text := string(data)
	if !tomlTableExists(text, "mcp_servers.taskpilot") {
		return codexMCPHealthResult{ConfigPath: path, OK: false, Message: "TaskPilot MCP server is missing from Codex config; run `taskpilot agent configure codex`"}
	}
	command, ok := tomlStringKey(text, "mcp_servers.taskpilot", "command")
	if !ok || strings.TrimSpace(command) == "" {
		return codexMCPHealthResult{ConfigPath: path, OK: false, Message: "TaskPilot MCP command is missing from Codex config; run `taskpilot agent configure codex`"}
	}
	expectedCommand := codexTaskPilotCommand()
	if filepath.Clean(command) != filepath.Clean(expectedCommand) {
		return codexMCPHealthResult{ConfigPath: path, OK: false, Message: "TaskPilot MCP command points to " + command + ", expected " + expectedCommand + "; run `taskpilot agent configure codex`"}
	}
	args, ok := tomlStringArrayKey(text, "mcp_servers.taskpilot", "args")
	if !ok || len(args) != 2 || args[0] != "mcp" || args[1] != "serve" {
		return codexMCPHealthResult{ConfigPath: path, OK: false, Message: "TaskPilot MCP args should be [\"mcp\", \"serve\"]; run `taskpilot agent configure codex`"}
	}
	configEnv, ok := tomlStringKey(text, "mcp_servers.taskpilot.env", "TASKPILOT_CONFIG")
	expectedConfig := taskPilotConfigEnvPath()
	if !ok || filepath.Clean(configEnv) != filepath.Clean(expectedConfig) {
		return codexMCPHealthResult{ConfigPath: path, OK: false, Message: "TaskPilot MCP TASKPILOT_CONFIG env is missing or stale; run `taskpilot agent configure codex`"}
	}
	missing := []string{}
	for _, tool := range requiredCodexTaskPilotMCPTools {
		if !tomlTableExists(text, "mcp_servers.taskpilot.tools."+tool) {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return codexMCPHealthResult{ConfigPath: path, OK: false, Message: "TaskPilot MCP semantic-memory tools missing from Codex config: " + strings.Join(missing, ", ") + "; run `taskpilot agent configure codex`"}
	}
	if err := probeCodexTaskPilotMCP(command, args, map[string]string{"TASKPILOT_CONFIG": configEnv}); err != nil {
		return codexMCPHealthResult{ConfigPath: path, OK: false, Message: "TaskPilot MCP subprocess probe failed: " + err.Error() + "; run `taskpilot agent configure codex`"}
	}
	return codexMCPHealthResult{ConfigPath: path, OK: true}
}

func probeCodexTaskPilotMCPServer(command string, args []string, env map[string]string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = os.Environ()
	for key, value := range env {
		if strings.TrimSpace(key) == "" {
			continue
		}
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("timed out running %s", command)
		}
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf("%v: %s", err, detail)
		}
		return err
	}
	scanner := bufio.NewScanner(strings.NewReader(stdout.String()))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var resp mcpResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			return fmt.Errorf("invalid JSON-RPC response: %v", err)
		}
		if resp.Error != nil {
			return errors.New(resp.Error.Message)
		}
		if mcpResponseHasTool(resp, "record_repo_semantic_memory") {
			return nil
		}
		return fmt.Errorf("record_repo_semantic_memory tool not listed")
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("no JSON-RPC response from MCP subprocess")
}

func mcpResponseHasTool(resp mcpResponse, name string) bool {
	result, ok := resp.Result.(map[string]any)
	if !ok {
		return false
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		return false
	}
	for _, tool := range tools {
		item, ok := tool.(map[string]any)
		if !ok {
			continue
		}
		if got, _ := item["name"].(string); got == name {
			return true
		}
	}
	return false
}

func codexConfigPath() string {
	home := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if home == "" {
		userHome, _ := os.UserHomeDir()
		home = filepath.Join(userHome, ".codex")
	}
	return filepath.Join(home, "config.toml")
}

func codexTaskPilotCommand() string {
	if path, err := exec.LookPath("taskpilot"); err == nil && strings.TrimSpace(path) != "" {
		if abs, absErr := filepath.Abs(path); absErr == nil {
			return abs
		}
		return path
	}
	if path, err := currentExecutablePath(); err == nil {
		return path
	}
	return "taskpilot"
}

func taskPilotConfigEnvPath() string {
	path := configPath()
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func tomlTableExists(text, table string) bool {
	_, _, ok := tomlTableRange(text, table)
	return ok
}

func tomlStringKey(text, table, key string) (string, bool) {
	raw, ok := tomlRawKey(text, table, key)
	if !ok {
		return "", false
	}
	value, err := strconv.Unquote(raw)
	if err != nil {
		return strings.Trim(raw, `"`), true
	}
	return value, true
}

func tomlStringArrayKey(text, table, key string) ([]string, bool) {
	raw, ok := tomlRawKey(text, table, key)
	if !ok {
		return nil, false
	}
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "[") || !strings.HasSuffix(raw, "]") {
		return nil, false
	}
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(raw, "["), "]"))
	if raw == "" {
		return []string{}, true
	}
	parts := strings.Split(raw, ",")
	out := []string{}
	for _, part := range parts {
		value, err := strconv.Unquote(strings.TrimSpace(part))
		if err != nil {
			return nil, false
		}
		out = append(out, value)
	}
	return out, true
}

func tomlRawKey(text, table, key string) (string, bool) {
	start, end, ok := tomlTableRange(text, table)
	if !ok {
		return "", false
	}
	block := text[start:end]
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		before, after, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(before) != key {
			continue
		}
		return strings.TrimSpace(after), true
	}
	return "", false
}

func ensureTomlTableKey(text, table, key, value string) (string, bool) {
	start, end, ok := tomlTableRange(text, table)
	if !ok {
		text = strings.TrimRight(text, "\n")
		if text != "" {
			text += "\n\n"
		}
		text += "[" + table + "]\n" + key + " = " + value + "\n"
		return text, true
	}
	block := text[start:end]
	lines := strings.SplitAfter(block, "\n")
	offset := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key+" ") || strings.HasPrefix(trimmed, key+"=") {
			newLine := key + " = " + value
			if strings.HasSuffix(line, "\n") {
				newLine += "\n"
			}
			if line == newLine {
				return text, false
			}
			return text[:start+offset] + newLine + text[start+offset+len(line):], true
		}
		offset += len(line)
	}
	insert := key + " = " + value + "\n"
	if !strings.HasSuffix(block, "\n") {
		insert = "\n" + insert
	}
	return text[:end] + insert + text[end:], true
}

func tomlTableRange(text, table string) (int, int, bool) {
	offset := 0
	start := -1
	for _, line := range splitLinesKeepingEnd(text) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			name := strings.Trim(trimmed, "[]")
			if start >= 0 {
				return start, offset, true
			}
			if name == table {
				start = offset
			}
		}
		offset += len(line)
	}
	if start >= 0 {
		return start, len(text), true
	}
	return 0, 0, false
}

func splitLinesKeepingEnd(text string) []string {
	if text == "" {
		return nil
	}
	out := []string{}
	for len(text) > 0 {
		idx := strings.IndexByte(text, '\n')
		if idx < 0 {
			out = append(out, text)
			break
		}
		out = append(out, text[:idx+1])
		text = text[idx+1:]
	}
	return out
}

func (a jsonHookAdapter) command() string {
	format := strings.TrimSpace(a.format)
	if format == "" {
		format = "markdown"
	}
	return "taskpilot context render --repo . --format " + format
}

func (a jsonHookAdapter) finalCommand() string {
	return "taskpilot context checkpoint --repo . --source agent-hook --reason session_end"
}

func (a jsonHookAdapter) legacyCommands() []string {
	base := strings.TrimSuffix(strings.TrimSuffix(a.hookRel, ".sh"), ".cmd")
	return []string{
		filepath.ToSlash(base + ".sh"),
		filepath.ToSlash(base + ".cmd"),
		filepath.ToSlash(base),
	}
}

type unsupportedAgentAdapter struct {
	name       string
	manualNote string
}

func (a unsupportedAgentAdapter) Name() string { return a.name }
func (a unsupportedAgentAdapter) Detect(repo repoEnableConfig) AgentDetection {
	return AgentDetection{Name: a.name, Supported: false}
}
func (a unsupportedAgentAdapter) Configure(repo repoEnableConfig, dryRun bool) AgentConfigureResult {
	return AgentConfigureResult{Name: a.name, Status: "unsupported", DryRun: dryRun, Message: "native session-hook adapter is not available", ManualFallback: a.ManualInstructions(repo)}
}
func (a unsupportedAgentAdapter) Doctor(repo repoEnableConfig) AgentHealth {
	return AgentHealth{Name: a.name, Status: "unsupported", Message: a.ManualInstructions(repo)}
}
func (a unsupportedAgentAdapter) ManualInstructions(repo repoEnableConfig) string {
	if strings.TrimSpace(a.manualNote) != "" {
		return a.manualNote
	}
	return "No automatic adapter exists for " + a.name + ". Configure your agent to run `.taskpilot/hooks/session-start.sh` on session start."
}

func mergeSessionStartHookJSON(path, hookPath string, hook SessionStartHook, dryRun bool, aliases ...string) (jsonHookWriteResult, error) {
	original, existed, err := readJSONConfigOrEmpty(path)
	if err != nil {
		return jsonHookWriteResult{}, err
	}
	updated, changed, err := mergeSessionStartHookJSONBytes(original, hookPath, hook, aliases...)
	if err != nil {
		return jsonHookWriteResult{}, err
	}
	result := jsonHookWriteResult{Changed: changed, Created: !existed}
	if !changed || dryRun {
		if !changed {
			result.Message = "already registered"
		}
		return result, nil
	}
	if existed {
		backupPath, err := ensureOneTimeBackup(path)
		if err != nil {
			return result, err
		}
		result.BackupPath = backupPath
	}
	if err := atomicWriteFile(path, updated, 0o644); err != nil {
		return result, err
	}
	return result, nil
}

func mergeSessionStartHookJSONBytes(original []byte, hookPath string, hook SessionStartHook, aliases ...string) ([]byte, bool, error) {
	if len(bytes.TrimSpace(original)) == 0 {
		original = []byte("{}")
	}
	if !gjson.ValidBytes(original) {
		return nil, false, fmt.Errorf("invalid JSON config")
	}
	entries := gjson.GetBytes(original, hookPath)
	if !entries.Exists() {
		entryBytes, _ := json.Marshal(map[string]any{"hooks": []SessionStartHook{hook}})
		out, err := sjson.SetRawBytes(original, hookPath, []byte("["+string(entryBytes)+"]"))
		return out, err == nil && !bytes.Equal(out, original), err
	}
	if !entries.IsArray() {
		return nil, false, fmt.Errorf("%s exists but is not an array", hookPath)
	}
	for i, entry := range entries.Array() {
		hooks := entry.Get("hooks")
		if !hooks.IsArray() {
			continue
		}
		for j, child := range hooks.Array() {
			if !commandMatchesHook(child.Get("command").String(), hook.Command, aliases) {
				continue
			}
			out := original
			var err error
			out, err = sjson.SetBytes(out, fmt.Sprintf("%s.%d.hooks.%d.type", hookPath, i, j), hook.Type)
			if err != nil {
				return nil, false, err
			}
			out, err = sjson.SetBytes(out, fmt.Sprintf("%s.%d.hooks.%d.command", hookPath, i, j), hook.Command)
			if err != nil {
				return nil, false, err
			}
			return out, !bytes.Equal(out, original), nil
		}
	}
	entryBytes, _ := json.Marshal(map[string]any{"hooks": []SessionStartHook{hook}})
	out, err := sjson.SetRawBytes(original, hookPath+".-1", entryBytes)
	return out, err == nil && !bytes.Equal(out, original), err
}

func commandMatchesHook(command, desired string, aliases []string) bool {
	if command == desired {
		return true
	}
	for _, alias := range aliases {
		if command == alias {
			return true
		}
	}
	return false
}

func jsonConfigHasHookCommand(path, hookPath, command string) bool {
	data, err := os.ReadFile(path)
	if err != nil || !gjson.ValidBytes(data) {
		return false
	}
	entries := gjson.GetBytes(data, hookPath)
	if !entries.IsArray() {
		return false
	}
	for _, entry := range entries.Array() {
		for _, hook := range entry.Get("hooks").Array() {
			if hook.Get("command").String() == command {
				return true
			}
		}
	}
	return false
}

func readJSONConfigOrEmpty(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return []byte("{}"), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func ensureOneTimeBackup(path string) (string, error) {
	backupPath := path + ".taskpilot.bak"
	if _, err := os.Stat(backupPath); err == nil {
		return backupPath, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := copyFile(path, backupPath); err != nil {
		return "", err
	}
	return backupPath, nil
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := ensureDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "'\"'\"'") + "'"
}

func cleanPath(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

type renderedRepoContext struct {
	Repo            repoEnableConfig `json:"repo"`
	Branch          string           `json:"branch"`
	Commit          string           `json:"commit"`
	ChangedFiles    []string         `json:"changed_files"`
	LikelyTask      *Task            `json:"likely_task,omitempty"`
	MatchScore      int              `json:"match_score,omitempty"`
	MatchConfidence float64          `json:"match_confidence,omitempty"`
	MatchAction     string           `json:"match_action,omitempty"`
	MatchReasons    []string         `json:"match_reasons,omitempty"`
	ActiveOverlaps  []Task           `json:"active_overlaps,omitempty"`
	ActiveLocks     []Lock           `json:"active_locks,omitempty"`
	RecentContext   []ContextEntry   `json:"recent_context,omitempty"`
	RecentDecisions []DecisionRecord `json:"recent_decisions,omitempty"`
	Warnings        []string         `json:"warnings,omitempty"`
}

func renderRepoContext(root, format string) (string, error) {
	activity, err := currentRepoActivity(root)
	if err != nil {
		return "", err
	}
	payload := renderedRepoContext{
		Repo:         activity.Config,
		Branch:       activity.Branch,
		Commit:       activity.Commit,
		ChangedFiles: activity.ChangedFiles,
	}
	if match, err := resolveRepoTask(activity); err == nil && match.Task.ID != "" {
		payload.LikelyTask = &match.Task
		payload.MatchScore = match.Score
		payload.MatchConfidence = match.Confidence
		payload.MatchAction = match.Action
		payload.MatchReasons = match.Reasons
	}
	tasks, err := tasksForRepo(activity.Config.RepoID, activity.Config.ProjectID)
	if err != nil {
		payload.Warnings = append(payload.Warnings, "Could not load tasks: "+err.Error())
	} else {
		payload.ActiveOverlaps = activeRepoOverlaps(tasks, activity)
	}
	if locks, warnings, err := mcpActiveLocks(map[string]any{"project_id": activity.Config.ProjectID, "limit": 50}); err == nil {
		payload.ActiveLocks = relevantLocks(locks, activity)
		payload.Warnings = append(payload.Warnings, warnings...)
	}
	seedTasks := append([]Task{}, payload.ActiveOverlaps...)
	if payload.LikelyTask != nil {
		seedTasks = append(seedTasks, *payload.LikelyTask)
	}
	payload.RecentContext, payload.RecentDecisions = recentRepoMemory(seedTasks)
	switch strings.ToLower(format) {
	case "json":
		data, _ := json.MarshalIndent(payload, "", "  ")
		return string(data), nil
	case "codex", "claude", "gemini", "markdown", "":
		return renderRepoContextMarkdown(payload), nil
	default:
		return "", fmt.Errorf("unknown context render format %q", format)
	}
}

func renderRepoContextMarkdown(ctx renderedRepoContext) string {
	lines := []string{
		"## TaskPilot Live Repo Context",
		"",
		"Use this shared context before planning or editing. Do not upload raw source files, prompts, logs, secrets, screenshots, or customer data.",
		"Before creating a task, inspect the likely task, active overlaps, recent memory, and relationships. Reuse the matching task when the objective is the same; create a subtask or related task only for a distinct outcome. Avoid generic file-based task names, and improve inferred task identity when semantic context becomes clearer.",
		"After meaningful work, record agent-authored memory before your final response and do not move on until TaskPilot returns `recorded` or `queued`. First use TaskPilot MCP `record_repo_semantic_memory` with task_id when known, completed work, why, files, verification, and remaining work. If MCP is unavailable, run `taskpilot context record-semantic --repo . --task-id task_... --completed-work \"...\" --why \"...\" --verification \"...\" --remaining-work \"...\" --files path1,path2`. A `queued` result is success: the daemon will flush it.",
		"",
		fmt.Sprintf("- Repo: %s", ctx.Repo.RepoName),
		fmt.Sprintf("- Branch: %s", fallbackText(ctx.Branch, "unknown")),
		fmt.Sprintf("- Commit: %s", fallbackText(ctx.Commit, "unknown")),
	}
	if len(ctx.ChangedFiles) > 0 {
		lines = append(lines, fmt.Sprintf("- Product files currently changed: %d", len(ctx.ChangedFiles)))
		for _, file := range limitStrings(ctx.ChangedFiles, 8) {
			lines = append(lines, "  - "+file)
		}
	} else {
		lines = append(lines, "- Product files currently changed: none")
	}
	if ctx.LikelyTask != nil {
		lines = append(lines, "", "### Likely Current Task")
		lines = append(lines, fmt.Sprintf("- %s %s: %s", ctx.LikelyTask.ID, ctx.LikelyTask.Status, ctx.LikelyTask.Title))
		if ctx.LikelyTask.OwnerID != "" {
			lines = append(lines, "- Owner: "+ctx.LikelyTask.OwnerID)
		}
		if len(ctx.MatchReasons) > 0 {
			lines = append(lines, "- Why this task: "+strings.Join(ctx.MatchReasons, ", "))
		}
		if ctx.MatchScore > 0 {
			lines = append(lines, fmt.Sprintf("- Match: action=%s score=%d confidence=%.2f", fallbackText(ctx.MatchAction, "unknown"), ctx.MatchScore, ctx.MatchConfidence))
		}
	}
	if len(ctx.ActiveOverlaps) > 0 {
		lines = append(lines, "", "### Active Or Overlapping Work")
		for _, task := range repoLimitTasks(ctx.ActiveOverlaps, 6) {
			owner := task.OwnerID
			if owner == "" {
				owner = "unowned"
			}
			lines = append(lines, fmt.Sprintf("- %s %s owner=%s: %s", task.ID, task.Status, owner, task.Title))
			scope := filterProductRepoFiles(task.Scope)
			if len(scope) > 0 {
				lines = append(lines, "  scope: "+strings.Join(limitStrings(scope, 5), ", "))
			}
		}
	}
	if len(ctx.ActiveLocks) > 0 {
		lines = append(lines, "", "### Active Locks")
		for _, lock := range repoLimitLocks(ctx.ActiveLocks, 8) {
			lines = append(lines, fmt.Sprintf("- %s owner=%s task=%s scope=%s", lock.Status, lock.OwnerID, lock.TaskID, lock.Scope))
		}
	}
	if len(ctx.RecentDecisions) > 0 || len(ctx.RecentContext) > 0 {
		lines = append(lines, "", "### Recent Useful Memory")
		for _, decision := range repoLimitDecisions(ctx.RecentDecisions, 4) {
			lines = append(lines, "- decision: "+decision.Decision)
		}
		for _, entry := range repoLimitContextEntries(ctx.RecentContext, 6) {
			lines = append(lines, fmt.Sprintf("- %s: %s", entry.Kind, singleLine(entry.Content)))
		}
	}
	if len(ctx.Warnings) > 0 {
		lines = append(lines, "", "### Warnings")
		for _, warning := range limitStrings(uniqueStrings(ctx.Warnings), 4) {
			lines = append(lines, "- "+warning)
		}
	}
	lines = append(lines, "", "Before changing files, respect active locks and coordinate if this context shows overlapping work.")
	return strings.Join(lines, "\n")
}

func activeRepoOverlaps(tasks []Task, activity repoActivity) []Task {
	out := []Task{}
	now := time.Now().UTC()
	for _, task := range tasks {
		if task.Status == "completed" || task.Status == "cancelled" {
			continue
		}
		if task.RepoID != "" && task.RepoID != activity.Config.RepoID {
			continue
		}
		active := task.OwnerID != "" && task.LastHeartbeatAt != nil && now.Sub(*task.LastHeartbeatAt) <= 2*DefaultClaimTTL
		overlap := len(activity.ChangedFiles) > 0 && taskScopesOverlap(task.Scope, activity.ChangedFiles)
		if active || overlap {
			out = append(out, task)
		}
	}
	return out
}

func relevantLocks(locks []Lock, activity repoActivity) []Lock {
	out := []Lock{}
	for _, lock := range locks {
		if lock.ReleasedAt != nil || (lock.Status != "active" && lock.Status != "stale") {
			continue
		}
		if len(activity.ChangedFiles) == 0 {
			out = append(out, lock)
			continue
		}
		for _, file := range activity.ChangedFiles {
			if scopeOverlaps(file, lock.Scope) {
				out = append(out, lock)
				break
			}
		}
	}
	return out
}

func recentRepoMemory(tasks []Task) ([]ContextEntry, []DecisionRecord) {
	seen := map[string]bool{}
	contextEntries := []ContextEntry{}
	decisions := []DecisionRecord{}
	for _, task := range tasks {
		if task.ID == "" || seen[task.ID] {
			continue
		}
		seen[task.ID] = true
		var detail TaskDetail
		if err := request("GET", "/api/tasks/"+url.PathEscape(task.ID), nil, &detail); err != nil {
			continue
		}
		contextEntries = append(contextEntries, compactContextEntries(detail.Context, 4)...)
		decisions = append(decisions, detail.Decisions...)
		if len(contextEntries) >= 10 && len(decisions) >= 6 {
			break
		}
	}
	sort.Slice(contextEntries, func(i, j int) bool {
		ri, rj := contextConfidenceRank(contextEntries[i]), contextConfidenceRank(contextEntries[j])
		if ri != rj {
			return ri < rj
		}
		return contextEntries[i].CreatedAt.After(contextEntries[j].CreatedAt)
	})
	sort.Slice(decisions, func(i, j int) bool { return decisions[i].CreatedAt.After(decisions[j].CreatedAt) })
	return contextEntries, decisions
}

func contextConfidenceRank(entry ContextEntry) int {
	if entry.Confidence == "agent_authored" && entry.Stage == "final" {
		return 0
	}
	switch entry.Confidence {
	case "agent_authored":
		return 1
	case "metadata_inferred":
		return 2
	case "file_checkpoint":
		return 3
	default:
		if oneOf(entry.Source, "mcp", "agent-hook", "taskpilot-run", "ui", "manual") {
			return 1
		}
		return 4
	}
}

func updateLiveContextFiles(cfg repoEnableConfig, rendered string) error {
	for _, name := range cfg.ContextFiles {
		name = strings.TrimSpace(name)
		if name == "" || filepath.IsAbs(name) || strings.Contains(filepath.Clean(name), "..") {
			continue
		}
		path := filepath.Join(cfg.GitRoot, name)
		base := liveContextBaseFile(name)
		if data, err := os.ReadFile(path); err == nil {
			base = string(data)
		}
		updated := upsertTaskPilotLiveSection(base, rendered)
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func liveContextBaseFile(name string) string {
	switch strings.ToUpper(filepath.Base(name)) {
	case "AGENTS.MD":
		return agentRulesFile()
	case "CLAUDE.MD":
		return "# Claude Repository Instructions\n\nThis repository uses TaskPilot for shared agent context.\n"
	case "GEMINI.MD":
		return "# Gemini Repository Instructions\n\nThis repository uses TaskPilot for shared agent context.\n"
	default:
		return "# Repository Instructions\n\nThis repository uses TaskPilot for shared agent context.\n"
	}
}

const (
	liveContextStart = "<!-- TASKPILOT:LIVE-CONTEXT:START -->"
	liveContextEnd   = "<!-- TASKPILOT:LIVE-CONTEXT:END -->"
)

func upsertTaskPilotLiveSection(content, rendered string) string {
	section := liveContextStart + "\n" + strings.TrimSpace(rendered) + "\n" + liveContextEnd
	start := strings.Index(content, liveContextStart)
	end := strings.Index(content, liveContextEnd)
	if start >= 0 && end >= start {
		end += len(liveContextEnd)
		return strings.TrimRight(content[:start], "\n") + "\n\n" + section + "\n" + strings.TrimLeft(content[end:], "\n")
	}
	return strings.TrimRight(content, "\n") + "\n\n" + section + "\n"
}

func fallbackText(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func singleLine(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 220 {
		value = value[:217] + "..."
	}
	return value
}

func repoLimitTasks(tasks []Task, max int) []Task {
	if max > 0 && len(tasks) > max {
		return tasks[:max]
	}
	return tasks
}

func repoLimitLocks(locks []Lock, max int) []Lock {
	if max > 0 && len(locks) > max {
		return locks[:max]
	}
	return locks
}

func repoLimitDecisions(decisions []DecisionRecord, max int) []DecisionRecord {
	if max > 0 && len(decisions) > max {
		return decisions[:max]
	}
	return decisions
}

func repoLimitContextEntries(entries []ContextEntry, max int) []ContextEntry {
	if max > 0 && len(entries) > max {
		return entries[:max]
	}
	return entries
}

func runAgentCommand(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("usage: taskpilot run <task-id> [--progress-interval 5s] [--complete] [--handoff-on-failure] [--handoff-to actor-id] [--summary text] -- <agent-command> [args...]")
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
	progressEvery := fs.Duration("progress-interval", progressInterval(), "sync lightweight run context to the server while the child command runs")
	noComplete := fs.Bool("no-complete", false, "deprecated: taskpilot run no longer completes automatically")
	completeOnSuccess := fs.Bool("complete", false, "explicitly complete the task when the child command succeeds")
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
	if err := ensureActorSessionForConfig(&cfg, true); err != nil {
		return err
	}
	_, _, _ = flushQueuedHandoffCheckpoints()
	var detail TaskDetail
	if err := request("GET", "/api/tasks/"+taskID, nil, &detail); err != nil {
		return err
	}
	beforeFiles := gitChangedFileSnapshot()
	contextPath, cleanup, err := createRunContextFile(taskID)
	if err != nil {
		return err
	}
	defer cleanup()
	importedContextLines := map[string]bool{}
	if detail.Task.OwnerID == "" || detail.Task.OwnerID != cfg.ActorID {
		var claimed Task
		if err := request("POST", "/api/tasks/"+taskID+"/claim", map[string]any{"reason": "taskpilot run"}, &claimed); err != nil {
			return taskRunOwnershipError(taskID, cfg, detail, err)
		}
	}
	for _, scope := range taskLockScopes(detail.Task) {
		var lock Lock
		_ = request("POST", "/api/tasks/"+taskID+"/locks", map[string]any{"scope": scope.scope, "scope_type": scope.scopeType}, &lock)
	}
	if err := request("GET", "/api/tasks/"+taskID, nil, &detail); err != nil {
		return err
	}
	var session TaskSession
	if err := request("POST", "/api/tasks/"+taskID+"/sessions/start", map[string]any{}, &session); err != nil {
		return taskRunOwnershipError(taskID, cfg, detail, err)
	}
	cfg.CurrentTaskID = taskID
	_ = updateTerminalActorSessionTask(cfg, taskID)
	_ = doRequestWithConfig(cfg, "POST", "/api/actor-sessions/current/heartbeat", ActorSessionHeartbeat{CurrentTaskID: taskID, Status: "active"}, nil, true)
	_ = appendRunContext(taskID, "summary", "taskpilot run started agent command: "+strings.Join(commandArgs, " "))
	taskContextPath, relatedContextPath, contextCleanup, err := createAgentContextFiles(taskID, detail)
	if err != nil {
		return err
	}
	defer contextCleanup()
	handoffPacket, err := createRunHandoffPacket(taskID, "", "draft")
	if err != nil {
		return err
	}
	handoffPath, handoffCleanup, err := createAgentHandoffFile(taskID, detail, handoffPacket)
	if err != nil {
		return err
	}
	preserveHandoffFile := false
	defer func() {
		if !preserveHandoffFile {
			handoffCleanup()
		}
	}()
	lastHandoffHash := fileHash(handoffPath)
	handoffTracker := &runHandoffTracker{}
	startupPrompt := agentStartupPrompt(taskID, taskContextPath, relatedContextPath, contextPath, handoffPath)
	promptPath, promptCleanup, err := createTextTemp("taskpilot-"+taskID+"-prompt-*.txt", startupPrompt)
	if err != nil {
		return err
	}
	defer promptCleanup()
	launchPrompt := agentLaunchPrompt(taskID, promptPath)
	injectedPrompt := false
	if !*noPromptInject {
		before := strings.Join(commandArgs, "\x00")
		commandArgs = injectAgentStartupPrompt(commandArgs, launchPrompt)
		injectedPrompt = strings.Join(commandArgs, "\x00") != before
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	done := make(chan struct{})
	var contextMu sync.Mutex
	go heartbeatLoop(ctx, taskID, done)
	go progressLoop(ctx, taskID, contextPath, importedContextLines, handoffPacket.ID, session.ID, handoffPath, &lastHandoffHash, *progressEvery, done, &contextMu, handoffTracker)
	cmd := exec.CommandContext(ctx, commandArgs[0], commandArgs[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(),
		"TASKPILOT_TASK_ID="+taskID,
		"TASKPILOT_SERVER="+cfg.Server,
		"TASKPILOT_ACTOR_ID="+cfg.ActorID,
		"TASKPILOT_ACTOR_SESSION_ID="+cfg.ActorSessionID,
		"TASKPILOT_ACTOR_SESSION_TOKEN="+cfg.ActorSessionToken,
		"TASKPILOT_SESSION_ID="+session.ID,
		"TASKPILOT_HANDOFF_PACKET_ID="+handoffPacket.ID,
		"TASKPILOT_PROJECT_ID="+detail.Task.ProjectID,
		"TASKPILOT_REPO_ID="+detail.Task.RepoID,
		"TASKPILOT_WORKSPACE_ID="+detail.Task.WorkspaceID,
		"TASKPILOT_RUN_CONTEXT_FILE="+contextPath,
		"TASKPILOT_HANDOFF_FILE="+handoffPath,
		"TASKPILOT_TASK_CONTEXT_FILE="+taskContextPath,
		"TASKPILOT_RELATED_CONTEXT_FILE="+relatedContextPath,
		"TASKPILOT_AGENT_PROMPT_FILE="+promptPath,
		"TASKPILOT_AGENT_INSTRUCTIONS="+agentInstructions(taskID),
	)
	if injectedPrompt {
		_, _ = fmt.Fprintf(os.Stderr, "TaskPilot: injected startup pointer into %s prompt. Full TaskPilot prompt file: %s\n", filepath.Base(commandArgs[0]), promptPath)
	}
	_, _ = fmt.Fprintf(os.Stderr, "TaskPilot: handoff draft file: %s\n", handoffPath)
	_, _ = fmt.Fprintf(os.Stderr, "TaskPilot: after each meaningful work unit, update the handoff draft and run: taskpilot handoff checkpoint %s --file %q\n", taskID, handoffPath)
	err = cmd.Run()
	close(done)
	contextMu.Lock()
	imported := importRunContext(taskID, contextPath, importedContextLines)
	handoffTracker.record(checkpointRunHandoffIfChanged(taskID, handoffPacket.ID, session.ID, handoffPath, &lastHandoffHash, true))
	contextMu.Unlock()
	changed, preExisting, changedFiles := touchedFilesSummary(beforeFiles, gitChangedFileSnapshot())
	if changed != "" {
		_ = appendRunContext(taskID, "output_ref", changed)
		_ = appendRunContext(taskID, "summary", "Updated task files: "+strings.Join(changedFiles, ", "))
	}
	if preExisting != "" {
		_ = appendRunContext(taskID, "risk", preExisting)
	}
	if changed != "" || preExisting != "" {
		handoffTracker.record(checkpointRunHandoffIfChanged(taskID, handoffPacket.ID, session.ID, handoffPath, &lastHandoffHash, true))
	}
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "agent command exited with error: %v\n", err)
		_ = appendRunContext(taskID, "blocker", "taskpilot run command failed: "+err.Error())
		_ = request("POST", "/api/tasks/"+taskID+"/sessions/finish", map[string]any{"session_id": session.ID, "exit_status": "failed", "finish_reason": err.Error()}, &Task{})
		if warnIfRunHandoffNeedsAttention(taskID, handoffPath, handoffTracker) {
			preserveHandoffFile = true
		}
		if *handoffOnFailure && *handoffTo != "" {
			_, _ = prepareRunHandoff(taskID, *handoffTo, err.Error(), changed, imported)
		}
		_, _ = fmt.Fprintf(os.Stderr, "Task returned to claimed state. Review context, mark blocked, or publish a handoff if needed.\n")
		return err
	}
	summary := strings.TrimSpace(*summaryFlag)
	if summary == "" {
		summary = strings.TrimSpace(os.Getenv("TASKPILOT_RUN_SUMMARY"))
	}
	if summary == "" {
		summary = "Agent command completed successfully through taskpilot run."
	}
	handoffTracker.record(checkpointRunHandoffIfChanged(taskID, handoffPacket.ID, session.ID, handoffPath, &lastHandoffHash, true))
	if warnIfRunHandoffNeedsAttention(taskID, handoffPath, handoffTracker) {
		preserveHandoffFile = true
	}
	if *completeOnSuccess && !*noComplete {
		_ = request("POST", "/api/tasks/"+taskID+"/sessions/finish", map[string]any{"session_id": session.ID, "exit_status": "success", "finish_reason": "agent command exited before explicit completion"}, &Task{})
		var completed Task
		if err := request("POST", "/api/tasks/"+taskID+"/complete", map[string]any{"summary": summary}, &completed); err != nil {
			return err
		}
		return print(completed, false)
	}
	_ = appendRunContext(taskID, "summary", summary)
	var claimed Task
	if err := request("POST", "/api/tasks/"+taskID+"/sessions/finish", map[string]any{"session_id": session.ID, "exit_status": "success", "finish_reason": "agent command exited"}, &claimed); err != nil {
		return err
	}
	return print(claimed, false)
}

func appendRunContext(taskID, kind, content string) error {
	return appendContextWithMeta(taskID, kind, content, "taskpilot-run", "", nil)
}

func appendContextWithMeta(taskID, kind, content, source, reason string, files []string) error {
	var out ContextEntry
	body := map[string]any{"kind": kind, "content": content}
	if strings.TrimSpace(source) != "" {
		body["source"] = source
	}
	if strings.TrimSpace(reason) != "" {
		body["reason"] = reason
	}
	if len(files) > 0 {
		body["files"] = files
	}
	return request("POST", "/api/tasks/"+url.PathEscape(taskID)+"/context", body, &out)
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
			_ = request("POST", "/api/actor-sessions/current/heartbeat", ActorSessionHeartbeat{CurrentTaskID: taskID, Status: "active"}, &ActorSession{})
		}
	}
}

func progressLoop(ctx context.Context, taskID, contextPath string, importedContextLines map[string]bool, handoffPacketID, sessionID, handoffPath string, lastHandoffHash *string, interval time.Duration, done <-chan struct{}, mu *sync.Mutex, tracker *runHandoffTracker) {
	if interval <= 0 {
		return
	}
	interval = runSyncInterval(interval)
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
			_ = importRunContext(taskID, contextPath, importedContextLines)
			tracker.record(checkpointRunHandoffIfChanged(taskID, handoffPacketID, sessionID, handoffPath, lastHandoffHash, false))
			mu.Unlock()
		}
	}
}

func runSyncInterval(configured time.Duration) time.Duration {
	if configured <= 0 {
		return configured
	}
	const maxLiveSyncDelay = 2 * time.Second
	if configured > maxLiveSyncDelay {
		return maxLiveSyncDelay
	}
	return configured
}

func generateRunSnapshot(taskID, snapshotType string) error {
	var out ContextSnapshot
	return request("POST", "/api/tasks/"+taskID+"/snapshots", map[string]any{"snapshot_type": snapshotType}, &out)
}

func generateRunHandoffPacket(taskID, handoffID, status string) error {
	_, err := createRunHandoffPacket(taskID, handoffID, status)
	return err
}

func createRunHandoffPacket(taskID, handoffID, status string) (HandoffPacket, error) {
	var out HandoffPacket
	err := request("POST", "/api/tasks/"+taskID+"/handoff-packet/generate", map[string]any{"handoff_id": handoffID, "status": status}, &out)
	return out, err
}

func checkpointRunHandoff(taskID, packetID, sessionID, handoffPath string) error {
	data, err := os.ReadFile(handoffPath)
	if err != nil {
		return err
	}
	_, _, err = sendOrQueueHandoffCheckpoint(taskID, packetID, sessionID, string(data))
	return err
}

func checkpointRunHandoffIfChanged(taskID, packetID, sessionID, handoffPath string, lastHash *string, force bool) (bool, error) {
	currentHash := fileHash(handoffPath)
	if currentHash == "" {
		return false, nil
	}
	if !force && lastHash != nil && *lastHash == currentHash {
		return false, nil
	}
	if err := checkpointRunHandoff(taskID, packetID, sessionID, handoffPath); err != nil {
		return true, err
	}
	if lastHash != nil {
		*lastHash = currentHash
	}
	return true, nil
}

func sendOrQueueHandoffCheckpoint(taskID, packetID, sessionID, markdown string) (HandoffCheckpoint, *queuedHandoffCheckpoint, error) {
	_, _, _ = flushQueuedHandoffCheckpoints()
	checkpoint, err := postHandoffCheckpointPayload(taskID, packetID, sessionID, markdown)
	if err != nil {
		if !isRetriableRequestError(err) {
			return HandoffCheckpoint{}, nil, err
		}
		queued, queueErr := queueHandoffCheckpoint(taskID, packetID, sessionID, markdown, err)
		if queueErr != nil {
			return HandoffCheckpoint{}, nil, fmt.Errorf("%w; additionally failed to queue checkpoint locally: %v", err, queueErr)
		}
		_, _ = fmt.Fprintf(os.Stderr, "TaskPilot: handoff checkpoint upload is queued locally as %s and will retry on the next run or `taskpilot handoff sync`.\n", queued.ID)
		started, daemonErr := handoffSyncDaemonStarter()
		if daemonErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "TaskPilot: could not start background handoff sync: %v. Run `taskpilot handoff sync` after connectivity returns.\n", daemonErr)
		} else if started {
			_, _ = fmt.Fprintln(os.Stderr, "TaskPilot: background handoff sync started.")
		}
		return HandoffCheckpoint{}, &queued, nil
	}
	return checkpoint, nil, nil
}

func postHandoffCheckpointPayload(taskID, packetID, sessionID, markdown string) (HandoffCheckpoint, error) {
	var out HandoffCheckpoint
	err := request("POST", "/api/tasks/"+taskID+"/handoff-checkpoints", map[string]any{"packet_id": packetID, "session_id": sessionID, "markdown": markdown}, &out)
	return out, err
}

func queueHandoffCheckpoint(taskID, packetID, sessionID, markdown string, cause error) (queuedHandoffCheckpoint, error) {
	cfg, err := loadConfig()
	if err != nil {
		return queuedHandoffCheckpoint{}, err
	}
	if cfg.Server == "" {
		cfg.Server = "http://127.0.0.1:8080"
	}
	now := time.Now().UTC()
	sum := sha256.Sum256([]byte(strings.Join([]string{cfg.Server, cfg.ActorID, taskID, packetID, sessionID, markdown}, "\x00")))
	id := fmt.Sprintf("local_checkpoint_%x", sum[:8])
	queued := queuedHandoffCheckpoint{
		ID:            id,
		Server:        cfg.Server,
		ActorID:       cfg.ActorID,
		TaskID:        taskID,
		PacketID:      packetID,
		SessionID:     sessionID,
		Markdown:      markdown,
		CreatedAt:     now,
		LastAttemptAt: now,
		Attempts:      1,
		LastError:     cause.Error(),
	}
	path := queuedHandoffCheckpointPath(id)
	if existing, err := readQueuedHandoffCheckpoint(path); err == nil {
		queued.CreatedAt = existing.CreatedAt
		queued.Attempts = existing.Attempts + 1
	}
	if err := writeQueuedHandoffCheckpoint(path, queued); err != nil {
		return queuedHandoffCheckpoint{}, err
	}
	return queued, nil
}

func flushQueuedHandoffCheckpoints() (int, int, error) {
	cfg, err := loadConfig()
	if err != nil {
		return 0, 0, err
	}
	if cfg.Server == "" {
		cfg.Server = "http://127.0.0.1:8080"
	}
	paths, err := queuedHandoffCheckpointPaths()
	if err != nil {
		return 0, 0, err
	}
	flushed := 0
	failed := 0
	for _, path := range paths {
		queued, err := readQueuedHandoffCheckpoint(path)
		if err != nil {
			failed++
			continue
		}
		if queued.Server != cfg.Server || queued.ActorID != cfg.ActorID {
			continue
		}
		_, err = postHandoffCheckpointPayload(queued.TaskID, queued.PacketID, queued.SessionID, queued.Markdown)
		if err == nil {
			_ = os.Remove(path)
			flushed++
			continue
		}
		failed++
		queued.Attempts++
		queued.LastAttemptAt = time.Now().UTC()
		queued.LastError = err.Error()
		_ = writeQueuedHandoffCheckpoint(path, queued)
		if !isRetriableRequestError(err) {
			continue
		}
	}
	return flushed, failed, nil
}

func runHandoffOutboxSync(ctx context.Context, opts handoffSyncOptions) (handoffSyncResult, error) {
	if opts.Interval <= 0 {
		opts.Interval = handoffSyncInterval()
	}
	if opts.MaxDuration <= 0 {
		opts.MaxDuration = handoffSyncMaxDuration()
	}
	deadline := time.Now().Add(opts.MaxDuration)
	if opts.Watch {
		acquired, err := acquireHandoffSyncLock(opts.MaxDuration)
		if err != nil {
			return handoffSyncResult{}, err
		}
		if !acquired {
			return handoffSyncResult{Skipped: true}, nil
		}
		defer releaseHandoffSyncLock()
	}
	result := handoffSyncResult{}
	for {
		flushed, failed, err := flushQueuedHandoffCheckpoints()
		if err != nil {
			return result, err
		}
		result.Flushed += flushed
		result.Failed += failed
		hasQueued, err := hasSyncableQueuedHandoffCheckpoints()
		if err != nil {
			return result, err
		}
		if !opts.Watch || !hasQueued {
			return result, nil
		}
		wait := opts.Interval
		if remaining := time.Until(deadline); remaining <= 0 {
			return result, nil
		} else if remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, nil
		case <-timer.C:
		}
	}
}

func startHandoffSyncDaemon() (bool, error) {
	fresh, err := handoffSyncLockIsFresh(handoffSyncMaxDuration())
	if err != nil {
		return false, err
	}
	if fresh {
		return false, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	if err := ensureDir(filepath.Dir(handoffSyncLogPath())); err != nil {
		return false, err
	}
	logFile, err := os.OpenFile(handoffSyncLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	cmd := exec.Command(exe, "handoff", "sync", "--watch", "--interval", handoffSyncInterval().String(), "--max-duration", handoffSyncMaxDuration().String())
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return false, err
	}
	_ = cmd.Process.Release()
	_ = logFile.Close()
	return true, nil
}

func hasSyncableQueuedHandoffCheckpoints() (bool, error) {
	cfg, err := loadConfig()
	if err != nil {
		return false, err
	}
	if cfg.Server == "" {
		cfg.Server = "http://127.0.0.1:8080"
	}
	paths, err := queuedHandoffCheckpointPaths()
	if err != nil {
		return false, err
	}
	for _, path := range paths {
		queued, err := readQueuedHandoffCheckpoint(path)
		if err != nil {
			continue
		}
		if queued.Server == cfg.Server && queued.ActorID == cfg.ActorID {
			return true, nil
		}
	}
	return false, nil
}

func acquireHandoffSyncLock(maxAge time.Duration) (bool, error) {
	if maxAge <= 0 {
		maxAge = handoffSyncMaxDuration()
	}
	if err := ensureDir(filepath.Dir(handoffSyncLockPath())); err != nil {
		return false, err
	}
	path := handoffSyncLockPath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = fmt.Fprintf(f, "%s\n", time.Now().UTC().Format(time.RFC3339Nano))
		return true, f.Close()
	}
	if !os.IsExist(err) {
		return false, err
	}
	fresh, err := handoffSyncLockIsFresh(maxAge)
	if err != nil {
		return false, err
	}
	if fresh {
		return false, nil
	}
	_ = os.Remove(path)
	return acquireHandoffSyncLock(maxAge)
}

func releaseHandoffSyncLock() {
	_ = os.Remove(handoffSyncLockPath())
}

func handoffSyncLockIsFresh(maxAge time.Duration) (bool, error) {
	if maxAge <= 0 {
		maxAge = handoffSyncMaxDuration()
	}
	info, err := os.Stat(handoffSyncLockPath())
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return time.Since(info.ModTime()) < maxAge+time.Minute, nil
}

func queuedHandoffCheckpointPaths() ([]string, error) {
	dir := handoffOutboxDir()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		paths = append(paths, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(paths)
	return paths, nil
}

func queuedHandoffCheckpointPath(id string) string {
	return filepath.Join(handoffOutboxDir(), id+".json")
}

func readQueuedHandoffCheckpoint(path string) (queuedHandoffCheckpoint, error) {
	var queued queuedHandoffCheckpoint
	data, err := os.ReadFile(path)
	if err != nil {
		return queued, err
	}
	return queued, json.Unmarshal(data, &queued)
}

func writeQueuedHandoffCheckpoint(path string, queued queuedHandoffCheckpoint) error {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(queued, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func handoffOutboxDir() string {
	return filepath.Join(taskpilotHomeDir(), "outbox", "handoff-checkpoints")
}

func handoffSyncLockPath() string {
	return filepath.Join(taskpilotHomeDir(), "outbox", "handoff-checkpoints.sync.lock")
}

func handoffSyncLogPath() string {
	return filepath.Join(taskpilotHomeDir(), "outbox", "handoff-sync.log")
}

func handoffSyncInterval() time.Duration {
	if v := os.Getenv("TASKPILOT_HANDOFF_SYNC_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 10 * time.Second
}

func handoffSyncMaxDuration() time.Duration {
	if v := os.Getenv("TASKPILOT_HANDOFF_SYNC_MAX_DURATION"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return time.Hour
}

func taskpilotHomeDir() string {
	if v := os.Getenv("TASKPILOT_CONFIG"); v != "" {
		return filepath.Dir(v)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".taskpilot")
}

func isRetriableRequestError(err error) bool {
	var retriable retriableRequestError
	return errors.As(err, &retriable)
}

type runHandoffTracker struct {
	mu         sync.Mutex
	attempts   int
	successes  int
	lastErrMsg string
}

func (t *runHandoffTracker) record(sent bool, err error) {
	if t == nil || !sent {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.attempts++
	if err != nil {
		t.lastErrMsg = err.Error()
		return
	}
	t.successes++
	t.lastErrMsg = ""
}

func (t *runHandoffTracker) snapshot() (attempts, successes int, lastErr string) {
	if t == nil {
		return 0, 0, ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.attempts, t.successes, t.lastErrMsg
}

func warnIfRunHandoffNeedsAttention(taskID, handoffPath string, tracker *runHandoffTracker) bool {
	reasons := []string{}
	data, err := os.ReadFile(handoffPath)
	if err != nil {
		reasons = append(reasons, "Could not read TASKPILOT_HANDOFF_FILE: "+err.Error())
	} else {
		content, err := parseHandoffMarkdownStrict(string(data), false)
		if err != nil {
			reasons = append(reasons, "Handoff Markdown could not be parsed: "+err.Error())
		} else {
			for _, validationErr := range validateHandoffQuality(content) {
				section := strings.TrimSpace(validationErr.Section)
				if section == "" {
					section = "Document"
				}
				reasons = append(reasons, section+": "+validationErr.Message)
			}
		}
	}
	attempts, successes, lastErr := tracker.snapshot()
	if successes == 0 {
		if attempts == 0 {
			reasons = append(reasons, "No handoff checkpoint was sent to the TaskPilot server.")
		} else {
			reasons = append(reasons, "No handoff checkpoint was saved by the TaskPilot server.")
		}
	}
	if lastErr != "" {
		reasons = append(reasons, "Last checkpoint error: "+lastErr)
	}
	reasons = uniqueStrings(cleanStrings(reasons))
	if len(reasons) == 0 {
		return false
	}
	_, _ = fmt.Fprintln(os.Stderr)
	_, _ = fmt.Fprintln(os.Stderr, "TaskPilot handoff needs attention before another agent can continue reliably:")
	for _, reason := range reasons {
		_, _ = fmt.Fprintf(os.Stderr, "  - %s\n", reason)
	}
	_, _ = fmt.Fprintf(os.Stderr, "Update the handoff draft and save a checkpoint with:\n  taskpilot handoff checkpoint %s --file %q\n", taskID, handoffPath)
	_, _ = fmt.Fprintf(os.Stderr, "TaskPilot kept the handoff file on disk for repair: %s\n\n", handoffPath)
	return true
}

func fileHash(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
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

func createAgentHandoffFile(taskID string, detail TaskDetail, packet HandoffPacket) (string, func(), error) {
	markdown := agentHandoffTemplate(taskID, detail, packet)
	return createTextTemp("taskpilot-"+taskID+"-handoff-*.md", markdown)
}

func agentHandoffTemplate(taskID string, detail TaskDetail, packet HandoffPacket) string {
	content := packet.Packet
	if content.TaskObjective == "" {
		content.TaskObjective = detail.Task.Goal
	}
	if content.CurrentStatus == "" {
		content.CurrentStatus = detail.Task.Status
	}
	if content.CurrentState == "" {
		content.CurrentState = "Describe the current state of the work after this session."
	}
	if content.HandoffMessage == "" {
		content.HandoffMessage = "Write a concise message for the next agent before stopping."
	}
	if len(content.CompletedWork) == 0 {
		content.CompletedWork = []string{"Replace this with concrete work completed during this session."}
	}
	if len(content.ImportantDecisions) == 0 {
		content.ImportantDecisions = []string{"Replace this with decisions made and why, or write: No material decision made; work followed existing requirements."}
	}
	if len(content.RemainingWork) == 0 {
		content.RemainingWork = []string{"Replace this with remaining work, or state that no known work remains."}
	}
	if len(content.SuggestedNextSteps) == 0 {
		content.SuggestedNextSteps = []string{"Replace this with the next concrete action for another agent or human."}
	}
	return renderHandoffMarkdown(content)
}

func handoffWritingRules() string {
	return `Handoff writing rules:
- Write for the next agent, not for a transcript archive.
- Be concise by default: keep bullets specific, outcome-focused, and non-repetitive.
- Include only durable insight: completed work, important decisions and reasons, current state, remaining work, risks, blockers, verification, files/artifacts, and assumptions.
- Do not paste raw command logs, raw prompts, screenshots, secrets, customer data, or long copied files.
- Do not record every small action. Merge routine steps into one useful bullet.
- Preserve important long context when shortening would remove meaning. If a complex decision, risk, failure, or verification needs detail, write the necessary detail clearly.
- Prefer exact references over prose dumps: task IDs, file paths, checkpoint IDs, commands run, test names, artifact links, and concise excerpts.
- Keep timeline entries short. Use them to explain meaningful state changes, not every command.
- Keep Remaining Work and Suggested Next Steps limited to work that is still actually pending.
- If nothing material changed, say that plainly instead of inflating the handoff.`
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
	Task                  Task                       `json:"task"`
	Owner                 *Actor                     `json:"owner,omitempty"`
	Parent                *Task                      `json:"parent,omitempty"`
	Subtasks              []Task                     `json:"subtasks,omitempty"`
	Dependencies          []TaskDependency           `json:"dependencies,omitempty"`
	Dependents            []TaskDependency           `json:"dependents,omitempty"`
	Relationships         []TaskRelationship         `json:"relationships,omitempty"`
	IncomingRelationships []TaskRelationship         `json:"incoming_relationships,omitempty"`
	IntelligenceDecisions []TaskIntelligenceDecision `json:"intelligence_decisions,omitempty"`
	Context               []ContextEntry             `json:"context,omitempty"`
	Decisions             []DecisionRecord           `json:"decisions,omitempty"`
	Comments              []Comment                  `json:"comments,omitempty"`
	Artifacts             []Artifact                 `json:"artifacts,omitempty"`
	GitRefs               []GitRef                   `json:"git_refs,omitempty"`
	Locks                 []Lock                     `json:"locks,omitempty"`
	Handoffs              []Handoff                  `json:"handoffs,omitempty"`
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
	HandoffMarkdown  string           `json:"handoff_markdown,omitempty"`
	HandoffSource    string           `json:"handoff_source,omitempty"`
}

const maxRelatedHandoffMarkdownChars = 24000

func createAgentContextFiles(taskID string, detail TaskDetail) (string, string, func(), error) {
	taskSnapshot := agentTaskContextFile{
		GeneratedAt: time.Now().UTC(),
		Usage:       "Read this first. It is the authoritative TaskPilot snapshot for the current task. Prefer it over live CLI calls from inside sandboxed agents.",
		CurrentTask: compactAgentTaskDetail(detail),
	}
	relatedSnapshot := agentRelatedContextFile{
		GeneratedAt:   time.Now().UTC(),
		Usage:         "Use this as prior work context. These tasks were selected because they are linked or relevant to the current task; unrelated tasks are intentionally omitted.",
		SelectionRule: "Includes directly linked tasks plus up to five same-project tasks with strong scope/repo/parent signals. If those signals are sparse, fills remaining slots with recent same-project tasks that have useful recorded memory. Related tasks include the latest handoff markdown when available, plus summaries, decisions, risks, blockers, outputs, artifacts, and git refs.",
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
	if isPromptInjectableAgent(commandArgs[0]) {
		if len(commandArgs) == 1 || isAgentResumeCommand(commandArgs) {
			return append(commandArgs, prompt)
		}
		return injectPromptIntoAgentArgs(commandArgs, prompt)
	}
	return commandArgs
}

func isPromptInjectableAgent(command string) bool {
	name := normalizedAgentCommandName(command)
	if name == "" {
		return false
	}
	for _, profile := range knownAgentProfiles {
		if !profile.PromptInjection {
			continue
		}
		for _, binary := range profile.Binaries {
			if name == strings.ToLower(binary) {
				return true
			}
		}
	}
	return false
}

func normalizedAgentCommandName(command string) string {
	name := strings.TrimSpace(command)
	if name == "" {
		return ""
	}
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.ToLower(filepath.Base(name))
	return strings.TrimSuffix(name, ".exe")
}

func agentLaunchPrompt(taskID, promptPath string) string {
	return "TaskPilot task " + taskID + ": before doing any repository analysis or edits, read the full TaskPilot instructions from " + promptPath + " and follow them exactly. If you cannot read that file, stop and report the TaskPilot context problem. Do not infer the task from repo files."
}

func injectPromptIntoAgentArgs(commandArgs []string, prompt string) []string {
	out := append([]string{}, commandArgs...)
	if idx := lastAgentPromptArgIndex(out); idx > 0 {
		out[idx] = combineTaskPilotAndUserPrompt(prompt, out[idx])
		return out
	}
	return append(out, prompt)
}

func lastAgentPromptArgIndex(commandArgs []string) int {
	valueFlags := map[string]bool{
		"-m": true, "--model": true,
		"-c": true, "--config": true,
		"-C": true, "--cd": true, "--cwd": true,
		"--profile": true, "--sandbox": true, "--approval-policy": true,
	}
	positional := []int{}
	for i := 1; i < len(commandArgs); i++ {
		arg := commandArgs[i]
		if arg == "--" {
			for j := i + 1; j < len(commandArgs); j++ {
				positional = append(positional, j)
			}
			break
		}
		if strings.HasPrefix(arg, "--") {
			if strings.Contains(arg, "=") {
				continue
			}
			if valueFlags[arg] && i+1 < len(commandArgs) {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			if valueFlags[arg] && i+1 < len(commandArgs) {
				i++
			}
			continue
		}
		positional = append(positional, i)
	}
	if len(positional) == 0 {
		return -1
	}
	return positional[len(positional)-1]
}

func combineTaskPilotAndUserPrompt(taskPilotPrompt, userPrompt string) string {
	userPrompt = strings.TrimSpace(userPrompt)
	if userPrompt == "" {
		return taskPilotPrompt
	}
	return taskPilotPrompt + " Human prompt for this work unit: " + userPrompt
}

func isAgentResumeCommand(commandArgs []string) bool {
	if len(commandArgs) < 2 {
		return false
	}
	for _, arg := range commandArgs[1:] {
		switch strings.ToLower(arg) {
		case "resume", "continue":
			return true
		}
	}
	return false
}

func agentStartupPrompt(taskID, taskContextPath, relatedContextPath, runContextPath, handoffPath string) string {
	return `Work on the current TaskPilot task.

You are launched by taskpilot run. Do not infer the task from repo-local files or stale local databases.

If this is a resumed agent session, treat this prompt as the fresh TaskPilot coordination context for the resumed work. The previous chat memory may help with conversation continuity, but TaskPilot task state is authoritative.

Before doing any repository analysis or edits:
1. Read ` + taskContextPath + ` for the authoritative current task snapshot.
2. Read ` + relatedContextPath + ` for selected related/prior task context from the TaskPilot server.
3. Treat TASKPILOT_TASK_ID=` + taskID + ` as the only current task.
4. Ignore repo-local .taskpilot-data.db, old peer/daemon state, and guessed commands like ./bin/taskpilot task current unless the current task context explicitly asks for them.

While working:
- Follow the current task goal, scope, locks, blockers, decisions, and handoff state from the context files.
- Use related context only when it is relevant to the current task. Do not pull unrelated task history into the answer.
- If live taskpilot CLI/server access fails from inside the agent runtime, continue from the injected context files.
- Write useful incremental updates immediately to ` + runContextPath + `.
- Keep the transfer-ready handoff draft updated in ` + handoffPath + `.

Write context that would let a different agent continue the work without reading this chat. Prefer short, specific entries over vague status updates.

Accepted update lines for ` + runContextPath + `:
- summary: concrete work completed, current state, or root cause found
- finding: important observation discovered during investigation
- decision: decision made plus the reason and tradeoff
- rationale: why the current approach is being used
- rejected: approach considered but not used, and why
- risk: risk, caveat, assumption, or possible regression
- blocker: what blocks progress and what is needed to unblock
- files: files, modules, APIs, commands, PRs, docs, or artifacts touched
- verification: tests, commands, checks, or manual validation performed
- next: specific next step another agent or human should take

Handoff checkpoint rules for ` + handoffPath + `:
- Treat this as the main memory another agent will read.
- After each completed prompt response or meaningful unit of work, update this file and run:
  taskpilot handoff checkpoint ` + taskID + ` --file "` + handoffPath + `"
- Do not erase earlier completed work or decisions from this file. Add the new work, then update Current State, Remaining Work, Suggested Next Steps, and Handoff Message.
- Completed Work must list what is actually done so the next agent does not repeat it.
- Important Decisions must list decisions and reasons. If no material decision was made, write exactly: No material decision made; work followed existing requirements.
- Current State must say where the task stands right now.
- Remaining Work and Suggested Next Steps must include only still-pending work.
- Handoff Message must be a concise message to the next agent.
- Do not leave placeholder text in required sections.

` + handoffWritingRules() + `

Useful examples:
- summary: Traced invite signup failure to expiry comparison after token lookup.
- finding: Token format is reused by existing invite links, so changing it would break old emails.
- decision: Patch expiry comparison only; keep token format unchanged to preserve compatibility.
- rationale: The failure is after validation, so DB schema changes are unnecessary.
- rejected: Rejected adding a new invite_tokens table because the existing token record has enough state.
- risk: Timezone handling may still be fragile around midnight UTC.
- blocker: Need a real expired invite sample before changing cleanup behavior.
- files: src/auth/invite.go, src/auth/invite_test.go
- verification: go test ./src/auth passed after adding invited-user regression coverage.
- next: Add one regression test for already-used invite tokens.

Do not upload or write secrets, raw private logs, customer data, private prompts, or raw local files into TaskPilot context.`
}

func compactAgentTaskDetail(detail TaskDetail) agentTaskDetail {
	return agentTaskDetail{
		Task:                  detail.Task,
		Owner:                 detail.Owner,
		Parent:                detail.Parent,
		Subtasks:              detail.Subtasks,
		Dependencies:          detail.Dependencies,
		Dependents:            detail.Dependents,
		Relationships:         detail.Relationships,
		IncomingRelationships: detail.IncomingRelationships,
		IntelligenceDecisions: limitTaskIntelligenceDecisions(detail.IntelligenceDecisions, 10),
		Context:               compactContextEntries(detail.Context, 40),
		Decisions:             limitDecisions(detail.Decisions, 20),
		Comments:              limitComments(detail.Comments, 20),
		Artifacts:             limitArtifacts(detail.Artifacts, 20),
		GitRefs:               limitGitRefs(detail.GitRefs, 20),
		Locks:                 detail.Locks,
		Handoffs:              detail.Handoffs,
	}
}

type relatedCandidate struct {
	Task      Task
	Score     int
	Reasons   []string
	Relations []string
}

const (
	maxRelatedAgentContexts   = 5
	strongRelatedTaskScore    = 50
	sameProjectFallbackWeight = 35
)

func collectRelatedAgentContexts(current TaskDetail) []agentRelatedContext {
	var tasks []Task
	path := "/api/tasks"
	if current.Task.ProjectID != "" {
		path += "?project_id=" + current.Task.ProjectID
	}
	if err := request("GET", path, nil, &tasks); err != nil {
		return nil
	}
	candidates := relatedTaskCandidates(current, tasks, time.Now())
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

func relatedTaskCandidates(current TaskDetail, tasks []Task, now time.Time) []relatedCandidate {
	linked := linkedTaskRelations(current)
	strong := []relatedCandidate{}
	fallback := []relatedCandidate{}
	for _, task := range tasks {
		if task.ID == current.Task.ID {
			continue
		}
		score, reasons := relatedTaskScoreAt(current.Task, task, now)
		relations := linked[task.ID]
		if len(relations) > 0 {
			score += 100
			reasons = append(reasons, "directly linked to current task")
		}
		candidate := relatedCandidate{Task: task, Score: score, Reasons: uniqueStrings(reasons), Relations: uniqueStrings(relations)}
		if score >= strongRelatedTaskScore {
			strong = append(strong, candidate)
			continue
		}
		if fallbackScore, fallbackReasons, ok := sameProjectFallbackScore(current.Task, task, score, now); ok {
			candidate.Score = fallbackScore
			candidate.Reasons = uniqueStrings(append(candidate.Reasons, fallbackReasons...))
			fallback = append(fallback, candidate)
		}
	}
	sortRelatedCandidates(strong)
	sortRelatedCandidates(fallback)
	candidates := append([]relatedCandidate{}, strong...)
	for _, candidate := range fallback {
		if len(candidates) >= maxRelatedAgentContexts {
			break
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) > maxRelatedAgentContexts {
		candidates = candidates[:maxRelatedAgentContexts]
	}
	return candidates
}

func sortRelatedCandidates(candidates []relatedCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score == candidates[j].Score {
			return candidates[i].Task.UpdatedAt.After(candidates[j].Task.UpdatedAt)
		}
		return candidates[i].Score > candidates[j].Score
	})
}

func sameProjectFallbackScore(current, candidate Task, baseScore int, now time.Time) (int, []string, bool) {
	if current.ProjectID == "" || candidate.ProjectID != current.ProjectID {
		return 0, nil, false
	}
	if current.RepoID != "" && candidate.RepoID != "" && current.RepoID != candidate.RepoID {
		return 0, nil, false
	}
	if candidate.Status == "cancelled" || !hasUsefulTaskMemorySignal(candidate) {
		return 0, nil, false
	}
	reasons := []string{"same-project prior context"}
	if strings.TrimSpace(candidate.SearchText) != "" {
		reasons = append(reasons, "has recorded task context")
	}
	if candidate.LatestHandoffStatus != "" {
		reasons = append(reasons, "has handoff state")
	}
	if !candidate.UpdatedAt.IsZero() && now.Sub(candidate.UpdatedAt) <= 14*24*time.Hour {
		reasons = append(reasons, "recently updated")
	}
	return baseScore + sameProjectFallbackWeight, uniqueStrings(reasons), true
}

func hasUsefulTaskMemorySignal(task Task) bool {
	return strings.TrimSpace(task.SearchText) != "" ||
		task.LatestHandoffStatus != "" ||
		task.Status == "completed" ||
		task.Status == "handoff_ready" ||
		task.Status == "blocked" ||
		len(task.Risks) > 0 ||
		len(task.Blockers) > 0
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
	for _, rel := range detail.Relationships {
		if rel.TargetTaskID != "" {
			out[rel.TargetTaskID] = append(out[rel.TargetTaskID], rel.Type)
		}
	}
	for _, rel := range detail.IncomingRelationships {
		if rel.SourceTaskID != "" {
			out[rel.SourceTaskID] = append(out[rel.SourceTaskID], "incoming_"+rel.Type)
		}
	}
	return out
}

func relatedTaskScore(current, candidate Task) (int, []string) {
	return relatedTaskScoreAt(current, candidate, time.Now())
}

func relatedTaskScoreAt(current, candidate Task, now time.Time) (int, []string) {
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
	if !candidate.UpdatedAt.IsZero() && now.Sub(candidate.UpdatedAt) <= 14*24*time.Hour {
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
	handoffSummary := relatedHandoffSummary(detail)
	handoffMarkdown, handoffSource := relatedHandoffMarkdown(detail)
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
		HandoffMarkdown:  handoffMarkdown,
		HandoffSource:    handoffSource,
	}
}

func relatedHandoffSummary(detail TaskDetail) string {
	if shouldShowHandoffPacket(detail.HandoffPacket) && detail.HandoffPacket.Packet.HandoffMessage != "" {
		return detail.HandoffPacket.Packet.HandoffMessage
	}
	if len(detail.Handoffs) > 0 {
		return detail.Handoffs[len(detail.Handoffs)-1].ResumeSummary
	}
	return ""
}

func relatedHandoffMarkdown(detail TaskDetail) (string, string) {
	if len(detail.HandoffCheckpoints) > 0 {
		checkpoint := detail.HandoffCheckpoints[len(detail.HandoffCheckpoints)-1]
		return limitRelatedHandoffMarkdown(checkpoint.Markdown), "handoff_checkpoint:" + checkpoint.ID
	}
	if shouldShowHandoffPacket(detail.HandoffPacket) && strings.TrimSpace(detail.HandoffPacket.Markdown) != "" {
		return limitRelatedHandoffMarkdown(detail.HandoffPacket.Markdown), "handoff_packet:" + detail.HandoffPacket.ID
	}
	return "", ""
}

func shouldShowHandoffPacket(packet *HandoffPacket) bool {
	if packet == nil || strings.TrimSpace(packet.Markdown) == "" {
		return false
	}
	if packet.Source != "generated_fallback" {
		return true
	}
	useful := 0
	for _, value := range []string{
		packet.Packet.CurrentState,
		packet.Packet.HandoffMessage,
		strings.Join(packet.Packet.CompletedWork, "\n"),
		strings.Join(packet.Packet.ImportantDecisions, "\n"),
		strings.Join(packet.Packet.ImplementationNotes, "\n"),
		strings.Join(packet.Packet.RemainingWork, "\n"),
		strings.Join(packet.Packet.SuggestedNextSteps, "\n"),
	} {
		value = strings.TrimSpace(value)
		if value != "" && !isNoisyRunContext(value) && !isGeneratedFallbackPlaceholder(value) {
			useful++
		}
	}
	return useful > 0
}

func isGeneratedFallbackPlaceholder(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	placeholders := []string{
		"no material decision made",
		"none recorded",
		"verify the recorded work",
		"continue from the recorded work",
		"continue from the latest task context",
		"replace this inferred title or goal",
	}
	for _, phrase := range placeholders {
		if strings.Contains(value, phrase) {
			return true
		}
	}
	return false
}

func limitRelatedHandoffMarkdown(markdown string) string {
	markdown = strings.TrimSpace(markdown)
	if len(markdown) <= maxRelatedHandoffMarkdownChars {
		return markdown
	}
	return markdown[:maxRelatedHandoffMarkdownChars] + "\n\n<!-- Related handoff markdown truncated; inspect the source task for full checkpoint history. -->"
}

func compactContextEntries(entries []ContextEntry, max int) []ContextEntry {
	out := []ContextEntry{}
	seen := map[string]bool{}
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry.Stage == "superseded" {
			continue
		}
		if isNoisyRunContext(entry.Content) {
			continue
		}
		key := entry.Kind + "\x00" + strings.TrimSpace(entry.Content)
		if entry.MemoryKey != "" {
			key = "memory\x00" + entry.MemoryKey
		}
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

func usefulGitRefs(refs []GitRef) []GitRef {
	out := []GitRef{}
	seen := map[string]bool{}
	for _, ref := range refs {
		if isNoisyRunContext(ref.Note) {
			continue
		}
		files := []string{}
		for _, file := range ref.ChangedFiles {
			if !isTaskPilotManagedRepoFile(file) {
				files = append(files, file)
			}
		}
		if len(files) == 0 && strings.TrimSpace(ref.PRURL) == "" && strings.TrimSpace(ref.CommitSHA) == "" {
			continue
		}
		ref.ChangedFiles = files
		key := ref.Branch + "\x00" + ref.CommitSHA + "\x00" + strings.Join(ref.ChangedFiles, "\x00") + "\x00" + ref.PRURL
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ref)
	}
	return out
}

func isNoisyRunContext(content string) bool {
	normalized := strings.ToLower(strings.TrimSpace(content))
	noisy := []string{
		"taskpilot run is still active; heartbeat renewed",
		"taskpilot daemon captured live uncommitted repo activity",
		"taskpilot inferred this task from repo activity",
		"live uncommitted work observed by taskpilot daemon",
		"inferred by taskpilot from live uncommitted work",
	}
	for _, phrase := range noisy {
		if strings.Contains(normalized, phrase) {
			return true
		}
	}
	return false
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

func limitTaskIntelligenceDecisions(values []TaskIntelligenceDecision, max int) []TaskIntelligenceDecision {
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

func importRunContext(taskID, path string, seen map[string]bool) int {
	entries := readNewRunContextEntries(path, seen)
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

func readNewRunContextEntries(path string, seen map[string]bool) []runContextEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := []runContextEntry{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if entry, ok := parseRunContextLine(scanner.Text()); ok {
			key := runContextEntryKey(entry)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, entry)
		}
	}
	return out
}

func runContextEntryKey(entry runContextEntry) string {
	return entry.Kind + "\x00" + strings.TrimSpace(entry.Content)
}

func parseRunContextLine(line string) (runContextEntry, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return runContextEntry{}, false
	}
	var entry runContextEntry
	if strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &entry) == nil {
		entry.Content = strings.TrimSpace(entry.Content)
		entry.Kind, entry.Content = normalizeRunContextEntry(entry.Kind, entry.Content)
		return entry, entry.Content != ""
	}
	parts := strings.SplitN(line, ":", 2)
	if len(parts) == 2 {
		entry.Content = strings.TrimSpace(parts[1])
		entry.Kind, entry.Content = normalizeRunContextEntry(parts[0], entry.Content)
		return entry, entry.Content != ""
	}
	return runContextEntry{Kind: "note", Content: line}, true
}

func normalizeRunContextEntry(kind, content string) (string, string) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	content = strings.TrimSpace(content)
	label := func(prefix string) string {
		if content == "" {
			return ""
		}
		return prefix + ": " + content
	}
	switch kind {
	case "summary", "completed", "completion":
		return "summary", content
	case "finding", "findings", "root_cause", "root-cause", "cause":
		return "summary", label("Finding")
	case "decision":
		return "decision", content
	case "rationale", "reasoning", "reason", "why":
		return "note", label("Rationale")
	case "rejected", "rejected_approach", "rejected-approach", "alternative", "alternatives":
		return "decision", label("Rejected approach")
	case "constraint", "assumption", "assumptions", "caveat":
		return "risk", label("Assumption")
	case "risk":
		return "risk", content
	case "blocker", "blocked":
		return "blocker", content
	case "output_ref", "output", "artifact", "file", "files", "changed", "changed_files", "changed-files", "touched", "touched_files", "touched-files":
		return "output_ref", content
	case "verification", "verified", "test", "tests", "check", "checks", "validation":
		return "note", label("Verification")
	case "next", "next_step", "next-step", "todo", "followup", "follow_up", "follow-up":
		return "next", content
	case "progress", "note", "":
		return "note", content
	default:
		return "note", label(kind)
	}
}

func normalizeContextKind(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case "summary", "decision", "note", "risk", "blocker", "output_ref", "next":
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
	for path := range gitChangedFileSnapshot() {
		out[path] = true
	}
	return out
}

type gitFileState struct {
	Status  string
	ModTime int64
	Size    int64
	Hash    string
}

func gitChangedFileSnapshot() map[string]gitFileState {
	out := map[string]gitFileState{}
	cmd := exec.Command("git", "status", "--porcelain")
	data, err := cmd.Output()
	if err != nil {
		return workspaceFileSnapshot()
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 4 {
			continue
		}
		status := strings.TrimSpace(line[:2])
		path := strings.TrimSpace(line[3:])
		if strings.Contains(path, " -> ") {
			parts := strings.Split(path, " -> ")
			path = parts[len(parts)-1]
		}
		if path != "" {
			out[path] = gitFileState{Status: status}
			if info, err := os.Stat(path); err == nil {
				state := out[path]
				state.ModTime = info.ModTime().UnixNano()
				state.Size = info.Size()
				out[path] = state
			}
		}
	}
	return out
}

func workspaceFileSnapshot() map[string]gitFileState {
	out := map[string]gitFileState{}
	root, err := os.Getwd()
	if err != nil {
		return out
	}
	ignoredDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, ".taskpilot": true,
		"dist": true, "build": true, "coverage": true, ".next": true,
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if ignoredDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		out[rel] = gitFileState{Status: "workspace", ModTime: info.ModTime().UnixNano(), Size: info.Size()}
		return nil
	})
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

func touchedFilesSummary(before, after map[string]gitFileState) (string, string, []string) {
	if len(after) == 0 {
		return "", "", nil
	}
	newOrChanged := []string{}
	existing := []string{}
	for path, afterState := range after {
		if beforeState, ok := before[path]; ok && beforeState == afterState {
			existing = append(existing, path)
		} else {
			newOrChanged = append(newOrChanged, path)
		}
	}
	sort.Strings(newOrChanged)
	sort.Strings(existing)
	affected := ""
	if len(newOrChanged) > 0 {
		lines := []string{"Files changed during this run:"}
		for _, path := range newOrChanged {
			lines = append(lines, "- "+path)
		}
		affected = strings.Join(lines, "\n")
	}
	warning := ""
	if len(existing) > 0 {
		lines := []string{"Pre-existing dirty worktree files were present before this run and are not treated as this task's affected files:"}
		for _, path := range existing {
			lines = append(lines, "- "+path)
		}
		warning = strings.Join(lines, "\n")
	}
	return affected, warning, newOrChanged
}

func runAgent(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: taskpilot agent init|configure|doctor")
	}
	switch args[0] {
	case "init":
		path := "AGENTS.md"
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists", path)
		}
		return os.WriteFile(path, []byte(agentRulesFile()), 0644)
	case "configure":
		fs := flag.NewFlagSet("agent configure", flag.ExitOnError)
		repoPath := fs.String("repo", ".", "repo path")
		dryRun := fs.Bool("dry-run", false, "show changes without writing")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		names := fs.Args()
		if len(names) == 0 {
			names = []string{"all"}
		}
		root, err := gitRoot(*repoPath)
		if err != nil {
			return err
		}
		repo, err := loadRepoConfig(root)
		if err != nil {
			return err
		}
		results := configureAgentAdapters(repo, *dryRun, names)
		if *jsonOut {
			return print(results, true)
		}
		for _, result := range results {
			fmt.Printf("%s: %s\n", result.Name, result.Message)
			if result.ManualFallback != "" {
				fmt.Println(result.ManualFallback)
			}
		}
		return nil
	case "doctor":
		fs := flag.NewFlagSet("agent doctor", flag.ExitOnError)
		repoPath := fs.String("repo", ".", "repo path")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		root, err := gitRoot(*repoPath)
		if err != nil {
			return err
		}
		repo, err := loadRepoConfig(root)
		if err != nil {
			return err
		}
		health := doctorAgentAdapters(repo)
		if *jsonOut {
			return print(health, true)
		}
		for _, item := range health {
			fmt.Printf("%s: %s", item.Name, item.Status)
			if item.Message != "" {
				fmt.Printf(" - %s", item.Message)
			}
			fmt.Println()
		}
		return nil
	default:
		return fmt.Errorf("unknown agent command %q", args[0])
	}
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
		mcpTool("create_project", "Create a TaskPilot project.", map[string]any{"name": mcpString("Project name"), "description": mcpString("Optional project description")}, []string{"name"}),
		mcpTool("list_projects", "List TaskPilot projects.", map[string]any{}, []string{}),
		mcpTool("create_repository", "Register a repository in TaskPilot.", map[string]any{"project_id": mcpString("Project ID"), "name": mcpString("Repository name"), "path": mcpString("Local path or remote URL"), "default_branch": mcpString("Default branch, defaults to main")}, []string{"project_id", "name"}),
		mcpTool("list_repositories", "List TaskPilot repositories.", map[string]any{"project_id": mcpString("Optional project ID")}, []string{}),
		mcpTool("create_workspace", "Create a TaskPilot workspace.", map[string]any{"project_id": mcpString("Project ID"), "name": mcpString("Workspace name"), "actor_id": mcpString("Optional actor ID"), "machine_name": mcpString("Optional machine name"), "kind": mcpString("Workspace kind, defaults to local")}, []string{"project_id", "name"}),
		mcpTool("list_workspaces", "List TaskPilot workspaces.", map[string]any{"project_id": mcpString("Optional project ID")}, []string{}),
		mcpTool("list_actors", "List TaskPilot actors.", map[string]any{}, []string{}),
		mcpTool("search_tasks", "Search TaskPilot tasks by query, project, repo, workspace, status, owner, priority, and completion state.", map[string]any{"query": mcpString("Text to search in title, goal, scope, blockers, risks, and task memory"), "project_id": mcpString("Optional project ID"), "repo_id": mcpString("Optional repository ID"), "workspace_id": mcpString("Optional workspace ID"), "status": mcpString("Optional task status"), "owner_id": mcpString("Optional owner actor ID"), "priority": mcpString("Optional priority"), "include_completed": map[string]any{"type": "boolean"}, "limit": map[string]any{"type": "integer", "description": "Maximum records to return"}}, []string{}),
		mcpTool("list_my_tasks", "List tasks owned by the configured TaskPilot actor.", map[string]any{"project_id": mcpString("Optional project ID"), "repo_id": mcpString("Optional repository ID"), "workspace_id": mcpString("Optional workspace ID"), "include_completed": map[string]any{"type": "boolean"}, "limit": map[string]any{"type": "integer", "description": "Maximum records to return"}}, []string{}),
		mcpTool("list_blocked_tasks", "List blocked tasks or tasks with blockers/dependencies.", map[string]any{"project_id": mcpString("Optional project ID"), "repo_id": mcpString("Optional repository ID"), "workspace_id": mcpString("Optional workspace ID"), "include_completed": map[string]any{"type": "boolean"}, "limit": map[string]any{"type": "integer", "description": "Maximum records to return"}}, []string{}),
		mcpTool("list_active_locks", "List active or stale TaskPilot locks, optionally filtered by scope.", map[string]any{"project_id": mcpString("Optional project ID"), "scope": mcpString("Optional file, glob, artifact, or semantic scope"), "limit": map[string]any{"type": "integer", "description": "Maximum records to return"}}, []string{}),
		mcpTool("list_conflicts", "List current TaskPilot conflicts.", map[string]any{"status": mcpString("Optional conflict status, defaults to open")}, []string{}),
		mcpTool("read_task", "Read full TaskPilot task detail.", map[string]any{"task_id": mcpString("Task ID")}, []string{"task_id"}),
		mcpTool("read_task_memory", "Read compact task memory: context, decisions, comments, artifacts, git refs, locks, handoffs, and checkpoints.", map[string]any{"task_id": mcpString("Task ID")}, []string{"task_id"}),
		mcpTool("task_context_bundle", "Read compact current-task context plus selected related TaskPilot context.", map[string]any{"task_id": mcpString("Task ID")}, []string{"task_id"}),
		mcpTool("ask_taskpilot", "Answer a natural-language TaskPilot question by retrieving matching records and returning concise evidence.", map[string]any{"query": mcpString("Question to ask TaskPilot"), "task_id": mcpString("Optional task ID to focus the question"), "project_id": mcpString("Optional project ID"), "include_completed": map[string]any{"type": "boolean"}, "limit": map[string]any{"type": "integer", "description": "Maximum evidence records to return"}}, []string{"query"}),
		mcpTool("get_current_repo_context", "Render current TaskPilot context for an enabled Git repo.", map[string]any{"repo": mcpString("Repo path, defaults to current directory"), "format": mcpString("markdown or json, defaults to markdown")}, []string{}),
		mcpTool("get_active_overlaps", "Return active or overlapping TaskPilot work for an enabled Git repo.", map[string]any{"repo": mcpString("Repo path, defaults to current directory")}, []string{}),
		mcpTool("ensure_task_for_repo_session", "Find or create the TaskPilot task for current live repo activity.", map[string]any{"repo": mcpString("Repo path, defaults to current directory")}, []string{}),
		mcpTool("record_repo_session_context", "Append sanitized context to the resolved current repo task.", map[string]any{"repo": mcpString("Repo path, defaults to current directory"), "kind": mcpString("summary, decision, note, risk, blocker, output_ref, next"), "content": mcpString("Sanitized context content"), "files": mcpStringArray("Related product files")}, []string{"content"}),
		mcpTool("record_repo_semantic_memory", "Append structured semantic memory to the resolved current repo task, or to an explicit task_id when supplied.", map[string]any{"repo": mcpString("Repo path, defaults to current directory"), "task_id": mcpString("Optional explicit TaskPilot task ID to receive memory"), "completed_work": mcpString("What changed or was completed"), "why": mcpString("Why it changed or important reasoning"), "verification": mcpString("Verification performed"), "remaining_work": mcpString("Remaining work or next step"), "files": mcpStringArray("Related product files"), "stage": mcpString("working or final, defaults to working")}, []string{"completed_work"}),
		mcpTool("create_task", "Create a new TaskPilot task.", map[string]any{"title": mcpString("Task title"), "goal": mcpString("Task goal"), "type": mcpString("Task type, defaults to implementation"), "priority": mcpString("Priority, defaults to normal"), "project_id": mcpString("Optional project ID"), "repo_id": mcpString("Optional repository ID"), "workspace_id": mcpString("Optional workspace ID"), "parent_task_id": mcpString("Optional parent task ID"), "scope": mcpStringArray("Task scopes such as files, globs, artifacts, or semantic areas"), "requirements": mcpStringArray("Task requirements"), "completion_criteria": mcpStringArray("Completion criteria"), "risks": mcpStringArray("Known risks"), "blockers": mcpStringArray("Known blockers"), "privacy_level": mcpString("Privacy level, defaults to sanitized_context")}, []string{"title", "goal"}),
		mcpTool("create_subtask", "Create a subtask under an existing TaskPilot task.", map[string]any{"parent_task_id": mcpString("Parent task ID"), "title": mcpString("Subtask title"), "goal": mcpString("Subtask goal"), "type": mcpString("Task type, defaults to implementation"), "priority": mcpString("Priority, defaults to normal"), "scope": mcpStringArray("Subtask scopes"), "requirements": mcpStringArray("Subtask requirements"), "completion_criteria": mcpStringArray("Subtask completion criteria"), "risks": mcpStringArray("Known risks"), "blockers": mcpStringArray("Known blockers")}, []string{"parent_task_id", "title", "goal"}),
		mcpTool("add_dependency", "Add a dependency so one task is blocked by another task.", map[string]any{"task_id": mcpString("Task ID that is blocked"), "depends_on_id": mcpString("Task ID this task depends on")}, []string{"task_id", "depends_on_id"}),
		mcpTool("add_task_relationship", "Add an explicit relationship between two tasks: parent_of, subtask_of, related_to, depends_on, blocks, continues, duplicates, or supersedes.", map[string]any{"source_task_id": mcpString("Source task ID"), "target_task_id": mcpString("Target task ID"), "type": mcpString("Relationship type"), "reason": mcpString("Reason for the relationship"), "confidence": map[string]any{"type": "number", "description": "Confidence from 0 to 1"}, "source": mcpString("Creation source such as agent, inference, or developer")}, []string{"source_task_id", "target_task_id", "type"}),
		mcpTool("remove_dependency", "Remove a task dependency by dependency ID.", map[string]any{"dependency_id": mcpString("Dependency ID")}, []string{"dependency_id"}),
		mcpTool("update_task", "Update editable TaskPilot task fields.", map[string]any{"task_id": mcpString("Task ID"), "title": mcpString("Optional title"), "goal": mcpString("Optional goal"), "type": mcpString("Optional type"), "priority": mcpString("Optional priority"), "project_id": mcpString("Optional project ID"), "repo_id": mcpString("Optional repository ID"), "workspace_id": mcpString("Optional workspace ID"), "parent_task_id": mcpString("Optional parent task ID"), "privacy_level": mcpString("Optional privacy level"), "scope": mcpStringArray("Replacement scopes"), "requirements": mcpStringArray("Replacement requirements"), "completion_criteria": mcpStringArray("Replacement completion criteria"), "risks": mcpStringArray("Replacement risks"), "blockers": mcpStringArray("Replacement blockers"), "reason": mcpString("Reason for update")}, []string{"task_id"}),
		mcpTool("append_task_fields", "Append requirements, completion criteria, risks, blockers, or scopes to a task.", map[string]any{"task_id": mcpString("Task ID"), "scope": mcpStringArray("Scopes to append"), "requirements": mcpStringArray("Requirements to append"), "completion_criteria": mcpStringArray("Completion criteria to append"), "risks": mcpStringArray("Risks to append"), "blockers": mcpStringArray("Blockers to append"), "reason": mcpString("Reason for update")}, []string{"task_id"}),
		mcpTool("update_task_status", "Update a TaskPilot task status.", map[string]any{"task_id": mcpString("Task ID"), "status": mcpString("New task status such as ready, claimed, in_progress, blocked, handoff_ready, in_review, completed, or cancelled")}, []string{"task_id", "status"}),
		mcpTool("delete_task", "Delete a TaskPilot task by ID.", map[string]any{"task_id": mcpString("Task ID")}, []string{"task_id"}),
		mcpTool("claim_task", "Claim a TaskPilot task.", map[string]any{"task_id": mcpString("Task ID"), "force": map[string]any{"type": "boolean"}, "reason": mcpString("Reason for force claim")}, []string{"task_id"}),
		mcpTool("heartbeat_task", "Renew active task ownership heartbeat.", map[string]any{"task_id": mcpString("Task ID")}, []string{"task_id"}),
		mcpTool("release_task", "Release ownership of a TaskPilot task.", map[string]any{"task_id": mcpString("Task ID")}, []string{"task_id"}),
		mcpTool("start_task_session", "Start a TaskPilot task session.", map[string]any{"task_id": mcpString("Task ID")}, []string{"task_id"}),
		mcpTool("finish_task_session", "Finish a TaskPilot task session.", map[string]any{"task_id": mcpString("Task ID"), "session_id": mcpString("Session ID"), "exit_status": mcpString("Exit status such as success or failed"), "finish_reason": mcpString("Finish reason")}, []string{"task_id", "session_id"}),
		mcpTool("append_context", "Append sanitized task context.", map[string]any{"task_id": mcpString("Task ID"), "kind": mcpString("summary, decision, note, risk, blocker, output_ref, next"), "content": mcpString("Sanitized context content"), "files": mcpStringArray("Related product files")}, []string{"task_id", "content"}),
		mcpTool("add_decision", "Add a first-class decision record to a task.", map[string]any{"task_id": mcpString("Task ID"), "decision": mcpString("Decision made"), "reason": mcpString("Reason for the decision"), "impact": mcpString("Impact of the decision"), "alternatives": mcpStringArray("Alternatives considered")}, []string{"task_id", "decision"}),
		mcpTool("add_comment", "Add a comment to a task.", map[string]any{"task_id": mcpString("Task ID"), "body": mcpString("Comment body")}, []string{"task_id", "body"}),
		mcpTool("add_artifact", "Attach an external artifact reference to a task.", map[string]any{"task_id": mcpString("Task ID"), "kind": mcpString("Artifact kind such as pr, doc, build, report, link"), "title": mcpString("Artifact title"), "uri": mcpString("Artifact URI"), "description": mcpString("Optional description"), "metadata": map[string]any{"type": "object", "description": "Optional metadata object"}}, []string{"task_id", "kind", "title", "uri"}),
		mcpTool("add_git_ref", "Attach git branch, commit, PR, changed files, or note to a task.", map[string]any{"task_id": mcpString("Task ID"), "branch": mcpString("Branch name"), "commit_sha": mcpString("Commit SHA"), "pr_url": mcpString("Pull request URL"), "changed_files": mcpStringArray("Changed files"), "note": mcpString("Optional note")}, []string{"task_id"}),
		mcpTool("create_context_snapshot", "Create a compact context snapshot for a task.", map[string]any{"task_id": mcpString("Task ID"), "snapshot_type": mcpString("Snapshot type, defaults to manual")}, []string{"task_id"}),
		mcpTool("acquire_lock", "Acquire a TaskPilot lock for a task scope.", map[string]any{"task_id": mcpString("Task ID"), "scope": mcpString("File, glob, artifact, task, or semantic scope"), "scope_type": mcpString("file, file_glob, artifact, task, or semantic")}, []string{"task_id", "scope"}),
		mcpTool("check_scope_conflicts", "Check whether a scope conflicts with active TaskPilot locks.", map[string]any{"scope": mcpString("File, glob, artifact, task, or semantic scope"), "scope_type": mcpString("Scope type, defaults to file_glob"), "project_id": mcpString("Optional project ID"), "limit": map[string]any{"type": "integer", "description": "Maximum records to return"}}, []string{"scope"}),
		mcpTool("release_lock", "Release a TaskPilot lock.", map[string]any{"lock_id": mcpString("Lock ID"), "reason": mcpString("Optional release reason")}, []string{"lock_id"}),
		mcpTool("renew_lock", "Renew a TaskPilot lock.", map[string]any{"lock_id": mcpString("Lock ID")}, []string{"lock_id"}),
		mcpTool("override_lock", "Override a TaskPilot lock.", map[string]any{"lock_id": mcpString("Lock ID"), "reason": mcpString("Override reason")}, []string{"lock_id", "reason"}),
		mcpTool("prepare_handoff", "Prepare a TaskPilot handoff with summary and next steps.", map[string]any{"task_id": mcpString("Task ID"), "to_actor_id": mcpString("Optional target actor ID"), "summary": mcpString("Handoff summary"), "next_steps": map[string]any{"type": "array", "items": mcpString("Next step")}}, []string{"task_id", "summary"}),
		mcpTool("generate_handoff_packet", "Generate a structured handoff packet for a task.", map[string]any{"task_id": mcpString("Task ID"), "handoff_id": mcpString("Optional handoff ID"), "status": mcpString("Packet status, defaults to draft")}, []string{"task_id"}),
		mcpTool("read_latest_handoff", "Read the latest handoff packet for a task.", map[string]any{"task_id": mcpString("Task ID")}, []string{"task_id"}),
		mcpTool("publish_handoff", "Publish a handoff packet by packet ID.", map[string]any{"packet_id": mcpString("Handoff packet ID")}, []string{"packet_id"}),
		mcpTool("accept_handoff", "Accept a handoff by handoff ID.", map[string]any{"handoff_id": mcpString("Handoff ID")}, []string{"handoff_id"}),
		mcpTool("reject_handoff", "Reject a handoff by handoff ID.", map[string]any{"handoff_id": mcpString("Handoff ID")}, []string{"handoff_id"}),
		mcpTool("list_handoffs", "List TaskPilot handoffs.", map[string]any{"task_id": mcpString("Optional task ID")}, []string{}),
		mcpTool("checkpoint_handoff", "Save a handoff markdown checkpoint.", map[string]any{"task_id": mcpString("Task ID"), "packet_id": mcpString("Optional handoff packet ID"), "session_id": mcpString("Optional task session ID"), "markdown": mcpString("Sanitized handoff markdown")}, []string{"task_id", "markdown"}),
		mcpTool("list_task_events", "List audit events for one task.", map[string]any{"task_id": mcpString("Task ID"), "since": map[string]any{"type": "integer", "description": "Only events after this event ID"}}, []string{"task_id"}),
		mcpTool("list_recent_events", "List recent TaskPilot audit events.", map[string]any{"since": map[string]any{"type": "integer", "description": "Only events after this event ID"}}, []string{}),
		mcpTool("find_related_tasks", "Find tasks related to a task by links, scope, repo, project, and memory signals.", map[string]any{"task_id": mcpString("Task ID")}, []string{"task_id"}),
		mcpTool("summarize_task", "Summarize one TaskPilot task from its structured memory.", map[string]any{"task_id": mcpString("Task ID")}, []string{"task_id"}),
		mcpTool("summarize_project", "Summarize TaskPilot project state from tasks and memory signals.", map[string]any{"project_id": mcpString("Optional project ID"), "include_completed": map[string]any{"type": "boolean"}, "limit": map[string]any{"type": "integer", "description": "Maximum tasks to inspect"}}, []string{}),
		mcpTool("find_decisions", "Find TaskPilot decisions matching a query.", map[string]any{"query": mcpString("Optional decision search text"), "project_id": mcpString("Optional project ID"), "task_id": mcpString("Optional task ID"), "limit": map[string]any{"type": "integer", "description": "Maximum records to return"}}, []string{}),
		mcpTool("find_blockers", "Find blockers from task fields and blocker context.", map[string]any{"query": mcpString("Optional blocker search text"), "project_id": mcpString("Optional project ID"), "task_id": mcpString("Optional task ID"), "include_completed": map[string]any{"type": "boolean"}, "limit": map[string]any{"type": "integer", "description": "Maximum records to return"}}, []string{}),
		mcpTool("find_outputs", "Find outputs from context, artifacts, and git refs.", map[string]any{"query": mcpString("Optional output search text"), "project_id": mcpString("Optional project ID"), "task_id": mcpString("Optional task ID"), "include_completed": map[string]any{"type": "boolean"}, "limit": map[string]any{"type": "integer", "description": "Maximum records to return"}}, []string{}),
		mcpTool("complete_task", "Complete a task with a summary.", map[string]any{"task_id": mcpString("Task ID"), "summary": mcpString("Completion summary")}, []string{"task_id", "summary"}),
	}
}

func mcpString(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func mcpStringArray(description string) map[string]any {
	return map[string]any{"type": "array", "items": mcpString("Value"), "description": description}
}

func mcpTool(name, description string, properties map[string]any, required []string) map[string]any {
	return map[string]any{"name": name, "description": description, "inputSchema": map[string]any{"type": "object", "properties": properties, "required": required}}
}

func callMCPTool(name string, args map[string]any) (any, error) {
	switch name {
	case "create_project":
		name, err := mcpRequireArg(args, "name")
		if err != nil {
			return nil, err
		}
		var out Project
		if err := request("POST", "/api/projects", map[string]any{"name": name, "description": mcpArg(args, "description")}, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "list_projects":
		var out []Project
		if err := request("GET", "/api/projects", nil, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(map[string]any{"summary": fmt.Sprintf("Found %d TaskPilot projects.", len(out)), "records": out}), nil
	case "create_repository":
		projectID, err := mcpRequireArg(args, "project_id")
		if err != nil {
			return nil, err
		}
		name, err := mcpRequireArg(args, "name")
		if err != nil {
			return nil, err
		}
		branch := mcpArg(args, "default_branch")
		if branch == "" {
			branch = "main"
		}
		var out Repository
		body := map[string]any{"project_id": projectID, "name": name, "path": mcpArg(args, "path"), "default_branch": branch}
		if err := request("POST", "/api/repositories", body, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "list_repositories":
		path := "/api/repositories"
		if projectID := mcpArg(args, "project_id"); projectID != "" {
			path += "?project_id=" + url.QueryEscape(projectID)
		}
		var out []Repository
		if err := request("GET", path, nil, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(map[string]any{"summary": fmt.Sprintf("Found %d TaskPilot repositories.", len(out)), "records": out}), nil
	case "create_workspace":
		projectID, err := mcpRequireArg(args, "project_id")
		if err != nil {
			return nil, err
		}
		name, err := mcpRequireArg(args, "name")
		if err != nil {
			return nil, err
		}
		kind := mcpArg(args, "kind")
		if kind == "" {
			kind = "local"
		}
		var out Workspace
		body := map[string]any{"project_id": projectID, "name": name, "actor_id": mcpArg(args, "actor_id"), "machine_name": mcpArg(args, "machine_name"), "kind": kind}
		if err := request("POST", "/api/workspaces", body, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "list_workspaces":
		path := "/api/workspaces"
		if projectID := mcpArg(args, "project_id"); projectID != "" {
			path += "?project_id=" + url.QueryEscape(projectID)
		}
		var out []Workspace
		if err := request("GET", path, nil, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(map[string]any{"summary": fmt.Sprintf("Found %d TaskPilot workspaces.", len(out)), "records": out}), nil
	case "list_actors":
		var out []Actor
		if err := request("GET", "/api/actors", nil, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(map[string]any{"summary": fmt.Sprintf("Found %d TaskPilot actors.", len(out)), "records": out}), nil
	case "search_tasks":
		tasks, err := mcpFilteredTasks(args)
		if err != nil {
			return nil, err
		}
		return mcpToolResult(mcpTaskQueryResponse("Matched TaskPilot tasks.", tasks, nil)), nil
	case "list_my_tasks":
		cfg, err := loadConfig()
		if err != nil {
			return nil, err
		}
		if cfg.ActorID == "" {
			return nil, fmt.Errorf("no TaskPilot actor session configured; run `taskpilot actor activate --secret <actor-secret>`")
		}
		if args == nil {
			args = map[string]any{}
		}
		args["owner_id"] = cfg.ActorID
		tasks, err := mcpFilteredTasks(args)
		if err != nil {
			return nil, err
		}
		return mcpToolResult(mcpTaskQueryResponse("Tasks owned by "+cfg.ActorID+".", tasks, nil)), nil
	case "list_blocked_tasks":
		filterArgs := map[string]any{"project_id": mcpArg(args, "project_id"), "include_completed": mcpBoolArg(args, "include_completed"), "limit": 1000}
		tasks, err := mcpFilteredTasks(filterArgs)
		if err != nil {
			return nil, err
		}
		blocked := []Task{}
		limit := mcpIntArg(args, "limit", 20)
		for _, task := range tasks {
			if task.Status == "blocked" || len(task.Blockers) > 0 || task.OpenDependencyCount > 0 {
				blocked = append(blocked, task)
				if limit > 0 && len(blocked) >= limit {
					break
				}
			}
		}
		return mcpToolResult(mcpTaskQueryResponse("Blocked TaskPilot tasks.", blocked, nil)), nil
	case "list_active_locks":
		locks, warnings, err := mcpActiveLocks(args)
		if err != nil {
			return nil, err
		}
		return mcpToolResult(map[string]any{"summary": fmt.Sprintf("Found %d active or stale TaskPilot locks.", len(locks)), "matched_count": len(locks), "records": locks, "warnings": warnings}), nil
	case "list_conflicts":
		status := mcpArg(args, "status")
		if status == "" {
			status = "open"
		}
		var out []Conflict
		path := "/api/conflicts"
		if status != "" {
			path += "?status=" + url.QueryEscape(status)
		}
		if err := request("GET", path, nil, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(map[string]any{"summary": fmt.Sprintf("Found %d TaskPilot conflicts.", len(out)), "matched_count": len(out), "records": out}), nil
	case "read_task":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		var out TaskDetail
		if err := request("GET", "/api/tasks/"+url.PathEscape(taskID), nil, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "read_task_memory":
		detail, err := mcpReadTaskDetail(args)
		if err != nil {
			return nil, err
		}
		return mcpToolResult(mcpTaskMemory(detail)), nil
	case "task_context_bundle":
		detail, err := mcpReadTaskDetail(args)
		if err != nil {
			return nil, err
		}
		return mcpToolResult(map[string]any{"summary": "Compact TaskPilot context bundle for " + detail.Task.ID + ".", "current_task": compactAgentTaskDetail(detail), "related_tasks": collectRelatedAgentContexts(detail)}), nil
	case "ask_taskpilot":
		out, err := mcpAskTaskPilot(args)
		if err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "get_current_repo_context":
		repo := mcpArg(args, "repo")
		if repo == "" {
			repo = "."
		}
		format := mcpArg(args, "format")
		if format == "" {
			format = "markdown"
		}
		root, err := gitRoot(repo)
		if err != nil {
			return nil, err
		}
		rendered, err := renderRepoContext(root, format)
		if err != nil {
			return nil, err
		}
		return mcpToolResult(map[string]any{"summary": "Rendered TaskPilot repo context.", "repo": root, "context": rendered}), nil
	case "get_active_overlaps":
		repo := mcpArg(args, "repo")
		if repo == "" {
			repo = "."
		}
		activity, err := currentRepoActivity(repo)
		if err != nil {
			return nil, err
		}
		tasks, err := tasksForRepo(activity.Config.RepoID, activity.Config.ProjectID)
		if err != nil {
			return nil, err
		}
		overlaps := activeRepoOverlaps(tasks, activity)
		locks, warnings, _ := mcpActiveLocks(map[string]any{"project_id": activity.Config.ProjectID, "limit": 50})
		return mcpToolResult(map[string]any{"summary": fmt.Sprintf("Found %d active or overlapping TaskPilot task(s).", len(overlaps)), "records": overlaps, "locks": relevantLocks(locks, activity), "warnings": warnings}), nil
	case "ensure_task_for_repo_session":
		repo := mcpArg(args, "repo")
		if repo == "" {
			repo = "."
		}
		activity, err := currentRepoActivity(repo)
		if err != nil {
			return nil, err
		}
		task, match, err := ensureTaskForRepoActivityWithIntentWithProxy(activity, repoWorkIntent{Kind: "mcp_session", Source: "mcp"}, true)
		if err != nil {
			return nil, err
		}
		return mcpToolResult(map[string]any{"summary": "Resolved TaskPilot task for current repo activity.", "task": task, "decision": repoTaskIntelligenceDecision(match.Action, match)}), nil
	case "record_repo_session_context":
		repo := mcpArg(args, "repo")
		if repo == "" {
			repo = "."
		}
		content, err := mcpRequireArg(args, "content")
		if err != nil {
			return nil, err
		}
		activity, err := currentRepoActivity(repo)
		if err != nil {
			return nil, err
		}
		intent := repoWorkIntent{Kind: "repo_session_context", Objective: content, Completed: content, Files: mcpStringSliceArg(args, "files"), Source: "mcp"}
		task, match, err := ensureTaskForRepoActivityWithIntentWithProxy(activity, intent, true)
		if err != nil {
			return nil, err
		}
		kind := mcpArg(args, "kind")
		if kind == "" {
			kind = "note"
		}
		var out ContextEntry
		files := mcpStringSliceArg(args, "files")
		if len(files) == 0 {
			files = activity.ChangedFiles
		}
		if err := request("POST", "/api/tasks/"+url.PathEscape(task.ID)+"/context", map[string]any{"kind": kind, "content": content, "source": "mcp", "reason": "repo_session", "confidence": "agent_authored", "files": filterProductRepoFiles(files), "intelligence_decision": repoTaskIntelligenceDecision("route_repo_context", match)}, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(map[string]any{"summary": "Recorded sanitized repo session context.", "task": task, "context": out}), nil
	case "record_repo_semantic_memory":
		repo := mcpArg(args, "repo")
		if repo == "" {
			repo = "."
		}
		completed, err := mcpRequireArg(args, "completed_work")
		if err != nil {
			return nil, err
		}
		out, err := recordRepoSemanticMemoryForTask(repo, mcpArg(args, "task_id"), completed, mcpArg(args, "why"), mcpArg(args, "verification"), mcpArg(args, "remaining_work"), mcpStringSliceArg(args, "files"), mcpArg(args, "stage"), "mcp", true)
		if err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "create_task":
		body, err := mcpTaskInput(args, false)
		if err != nil {
			return nil, err
		}
		var out Task
		if err := request("POST", "/api/tasks", body, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "create_subtask":
		parentID, err := mcpRequireArg(args, "parent_task_id")
		if err != nil {
			return nil, err
		}
		body, err := mcpTaskInput(args, true)
		if err != nil {
			return nil, err
		}
		var out Task
		if err := request("POST", "/api/tasks/"+url.PathEscape(parentID)+"/subtasks", body, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "add_dependency":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		dependsOnID, err := mcpRequireArg(args, "depends_on_id")
		if err != nil {
			return nil, err
		}
		var out TaskDependency
		if err := request("POST", "/api/tasks/"+url.PathEscape(taskID)+"/dependencies", map[string]any{"depends_on_id": dependsOnID}, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "add_task_relationship":
		sourceTaskID, err := mcpRequireArg(args, "source_task_id")
		if err != nil {
			return nil, err
		}
		targetTaskID, err := mcpRequireArg(args, "target_task_id")
		if err != nil {
			return nil, err
		}
		relationshipType, err := mcpRequireArg(args, "type")
		if err != nil {
			return nil, err
		}
		var out TaskRelationship
		body := map[string]any{"target_task_id": targetTaskID, "type": relationshipType, "reason": mcpArg(args, "reason"), "confidence": mcpFloatArg(args, "confidence", 1), "source": mcpArg(args, "source")}
		if err := request("POST", "/api/tasks/"+url.PathEscape(sourceTaskID)+"/relationships", body, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "remove_dependency":
		dependencyID, err := mcpRequireArg(args, "dependency_id")
		if err != nil {
			return nil, err
		}
		if err := request("DELETE", "/api/dependencies/"+url.PathEscape(dependencyID), nil, nil); err != nil {
			return nil, err
		}
		return mcpToolResult(map[string]any{"status": "ok", "dependency_id": dependencyID}), nil
	case "update_task":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		body := mcpTaskUpdateInput(args)
		var out Task
		if err := request("PATCH", "/api/tasks/"+url.PathEscape(taskID), body, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "append_task_fields":
		out, err := mcpAppendTaskFields(args)
		if err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "update_task_status":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		status, err := mcpRequireArg(args, "status")
		if err != nil {
			return nil, err
		}
		var out Task
		if err := request("PATCH", "/api/tasks/"+url.PathEscape(taskID), map[string]any{"status": status}, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "delete_task":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		if err := request("DELETE", "/api/tasks/"+url.PathEscape(taskID), nil, nil); err != nil {
			return nil, err
		}
		return mcpToolResult(map[string]any{"status": "ok", "task_id": taskID}), nil
	case "claim_task":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		var out Task
		body := map[string]any{"force": mcpBoolArg(args, "force"), "reason": mcpArg(args, "reason")}
		if err := request("POST", "/api/tasks/"+url.PathEscape(taskID)+"/claim", body, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "heartbeat_task":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		var out Task
		if err := request("POST", "/api/tasks/"+url.PathEscape(taskID)+"/heartbeat", map[string]any{}, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "release_task":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		var out Task
		if err := request("POST", "/api/tasks/"+url.PathEscape(taskID)+"/release", map[string]any{}, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "start_task_session":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		var out TaskSession
		if err := request("POST", "/api/tasks/"+url.PathEscape(taskID)+"/sessions/start", map[string]any{}, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "finish_task_session":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		sessionID, err := mcpRequireArg(args, "session_id")
		if err != nil {
			return nil, err
		}
		exitStatus := mcpArg(args, "exit_status")
		if exitStatus == "" {
			exitStatus = "success"
		}
		var out Task
		body := map[string]any{"session_id": sessionID, "exit_status": exitStatus, "finish_reason": mcpArg(args, "finish_reason")}
		if err := request("POST", "/api/tasks/"+url.PathEscape(taskID)+"/sessions/finish", body, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "append_context":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		content, err := mcpRequireArg(args, "content")
		if err != nil {
			return nil, err
		}
		var out ContextEntry
		kind := mcpArg(args, "kind")
		if kind == "" {
			kind = "note"
		}
		body := map[string]any{"kind": kind, "content": content, "source": "mcp", "confidence": "agent_authored", "files": filterProductRepoFiles(mcpStringSliceArg(args, "files"))}
		if err := request("POST", "/api/tasks/"+url.PathEscape(taskID)+"/context", body, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "add_decision":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		decision, err := mcpRequireArg(args, "decision")
		if err != nil {
			return nil, err
		}
		var out DecisionRecord
		body := map[string]any{"decision": decision, "reason": mcpArg(args, "reason"), "impact": mcpArg(args, "impact"), "alternatives": mcpStringSliceArg(args, "alternatives")}
		if err := request("POST", "/api/tasks/"+url.PathEscape(taskID)+"/decisions", body, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "add_comment":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		bodyText, err := mcpRequireArg(args, "body")
		if err != nil {
			return nil, err
		}
		var out Comment
		if err := request("POST", "/api/tasks/"+url.PathEscape(taskID)+"/comments", map[string]any{"body": bodyText}, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "add_artifact":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		kind, err := mcpRequireArg(args, "kind")
		if err != nil {
			return nil, err
		}
		title, err := mcpRequireArg(args, "title")
		if err != nil {
			return nil, err
		}
		uri, err := mcpRequireArg(args, "uri")
		if err != nil {
			return nil, err
		}
		var out Artifact
		body := map[string]any{"kind": kind, "title": title, "uri": uri, "description": mcpArg(args, "description"), "metadata": mcpMapArg(args, "metadata")}
		if err := request("POST", "/api/tasks/"+url.PathEscape(taskID)+"/artifacts", body, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "add_git_ref":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		var out GitRef
		body := map[string]any{"branch": mcpArg(args, "branch"), "commit_sha": mcpArg(args, "commit_sha"), "pr_url": mcpArg(args, "pr_url"), "changed_files": mcpStringSliceArg(args, "changed_files"), "note": mcpArg(args, "note")}
		if err := request("POST", "/api/tasks/"+url.PathEscape(taskID)+"/git", body, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "create_context_snapshot":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		snapshotType := mcpArg(args, "snapshot_type")
		if snapshotType == "" {
			snapshotType = "manual"
		}
		var out ContextSnapshot
		if err := request("POST", "/api/tasks/"+url.PathEscape(taskID)+"/snapshots", map[string]any{"snapshot_type": snapshotType}, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "acquire_lock":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		scope, err := mcpRequireArg(args, "scope")
		if err != nil {
			return nil, err
		}
		scopeType := mcpArg(args, "scope_type")
		if scopeType == "" {
			scopeType = "file_glob"
		}
		var out Lock
		if err := request("POST", "/api/tasks/"+url.PathEscape(taskID)+"/locks", map[string]any{"scope": scope, "scope_type": scopeType}, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "check_scope_conflicts":
		scope, err := mcpRequireArg(args, "scope")
		if err != nil {
			return nil, err
		}
		checkArgs := map[string]any{"project_id": mcpArg(args, "project_id"), "scope": scope, "limit": mcpIntArg(args, "limit", 50)}
		locks, warnings, err := mcpActiveLocks(checkArgs)
		if err != nil {
			return nil, err
		}
		return mcpToolResult(map[string]any{"summary": fmt.Sprintf("Found %d active lock conflict candidate(s) for %s.", len(locks), scope), "matched_count": len(locks), "records": locks, "warnings": warnings}), nil
	case "release_lock", "renew_lock", "override_lock":
		lockID, err := mcpRequireArg(args, "lock_id")
		if err != nil {
			return nil, err
		}
		action := strings.TrimSuffix(strings.TrimPrefix(name, "release_"), "_lock")
		if name == "release_lock" {
			action = "release"
		}
		if name == "renew_lock" {
			action = "renew"
		}
		if name == "override_lock" {
			action = "override"
		}
		body := map[string]any{}
		if action == "release" || action == "override" {
			body["reason"] = mcpArg(args, "reason")
		}
		var out Lock
		if err := request("POST", "/api/locks/"+url.PathEscape(lockID)+"/"+action, body, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "prepare_handoff":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		summary, err := mcpRequireArg(args, "summary")
		if err != nil {
			return nil, err
		}
		var out Handoff
		if err := request("POST", "/api/tasks/"+url.PathEscape(taskID)+"/handoff", map[string]any{"to_actor_id": mcpArg(args, "to_actor_id"), "summary": summary, "next_steps": mcpStringSliceArg(args, "next_steps")}, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "generate_handoff_packet":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		status := mcpArg(args, "status")
		if status == "" {
			status = "draft"
		}
		var out HandoffPacket
		body := map[string]any{"handoff_id": mcpArg(args, "handoff_id"), "status": status}
		if err := request("POST", "/api/tasks/"+url.PathEscape(taskID)+"/handoff-packet/generate", body, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "read_latest_handoff":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		var out HandoffPacket
		if err := request("GET", "/api/tasks/"+url.PathEscape(taskID)+"/handoff-packet", nil, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "publish_handoff":
		packetID, err := mcpRequireArg(args, "packet_id")
		if err != nil {
			return nil, err
		}
		var out HandoffPacket
		if err := request("POST", "/api/handoff-packets/"+url.PathEscape(packetID)+"/publish", map[string]any{}, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "accept_handoff", "reject_handoff":
		handoffID, err := mcpRequireArg(args, "handoff_id")
		if err != nil {
			return nil, err
		}
		action := "accept"
		if name == "reject_handoff" {
			action = "reject"
		}
		var out Handoff
		if err := request("POST", "/api/handoffs/"+url.PathEscape(handoffID)+"/"+action, map[string]any{}, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "list_handoffs":
		if taskID := mcpArg(args, "task_id"); taskID != "" {
			var detail TaskDetail
			if err := request("GET", "/api/tasks/"+url.PathEscape(taskID), nil, &detail); err != nil {
				return nil, err
			}
			return mcpToolResult(map[string]any{"summary": fmt.Sprintf("Found %d handoffs for %s.", len(detail.Handoffs), taskID), "records": detail.Handoffs}), nil
		}
		var out []Handoff
		if err := request("GET", "/api/handoffs", nil, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(map[string]any{"summary": fmt.Sprintf("Found %d TaskPilot handoffs.", len(out)), "records": out}), nil
	case "checkpoint_handoff":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		markdown, err := mcpRequireArg(args, "markdown")
		if err != nil {
			return nil, err
		}
		var out HandoffCheckpoint
		body := map[string]any{"packet_id": mcpArg(args, "packet_id"), "session_id": mcpArg(args, "session_id"), "markdown": markdown}
		if err := request("POST", "/api/tasks/"+url.PathEscape(taskID)+"/handoff-checkpoints", body, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "list_task_events":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		var detail TaskDetail
		if err := request("GET", "/api/tasks/"+url.PathEscape(taskID), nil, &detail); err != nil {
			return nil, err
		}
		out := detail.Events
		if since := mcpIntArg(args, "since", 0); since > 0 {
			filtered := []Event{}
			for _, event := range out {
				if event.ID > int64(since) {
					filtered = append(filtered, event)
				}
			}
			out = filtered
		}
		return mcpToolResult(map[string]any{"summary": fmt.Sprintf("Found %d events for %s.", len(out), taskID), "records": out}), nil
	case "list_recent_events":
		path := "/api/events"
		if since := mcpIntArg(args, "since", 0); since > 0 {
			path += "?since=" + url.QueryEscape(fmt.Sprint(since))
		}
		var out []Event
		if err := request("GET", path, nil, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(map[string]any{"summary": fmt.Sprintf("Found %d recent TaskPilot events.", len(out)), "records": out}), nil
	case "find_related_tasks":
		detail, err := mcpReadTaskDetail(args)
		if err != nil {
			return nil, err
		}
		related := collectRelatedAgentContexts(detail)
		return mcpToolResult(map[string]any{"summary": fmt.Sprintf("Found %d related tasks for %s.", len(related), detail.Task.ID), "records": related}), nil
	case "summarize_task":
		detail, err := mcpReadTaskDetail(args)
		if err != nil {
			return nil, err
		}
		return mcpToolResult(mcpSummarizeTask(detail)), nil
	case "summarize_project":
		out, err := mcpSummarizeProject(args)
		if err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "find_decisions":
		out, err := mcpFindDecisions(args)
		if err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "find_blockers":
		out, err := mcpFindBlockers(args)
		if err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "find_outputs":
		out, err := mcpFindOutputs(args)
		if err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	case "complete_task":
		taskID, err := mcpRequireArg(args, "task_id")
		if err != nil {
			return nil, err
		}
		summary, err := mcpRequireArg(args, "summary")
		if err != nil {
			return nil, err
		}
		var out Task
		if err := request("POST", "/api/tasks/"+url.PathEscape(taskID)+"/complete", map[string]any{"summary": summary}, &out); err != nil {
			return nil, err
		}
		return mcpToolResult(out), nil
	default:
		return nil, fmt.Errorf("unknown MCP tool %s", name)
	}
}

func mcpRequireArg(args map[string]any, key string) (string, error) {
	value := strings.TrimSpace(mcpArg(args, key))
	if value == "" {
		return "", fmt.Errorf("missing required argument %q", key)
	}
	return value, nil
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

func mcpIntArg(args map[string]any, key string, fallback int) int {
	if args == nil {
		return fallback
	}
	switch v := args[key].(type) {
	case float64:
		if v > 0 {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	}
	return fallback
}

func mcpFloatArg(args map[string]any, key string, fallback float64) float64 {
	if args == nil {
		return fallback
	}
	switch v := args[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return parsed
		}
	}
	return fallback
}

func mcpStringSliceArg(args map[string]any, key string) []string {
	if args == nil {
		return nil
	}
	raw, ok := args[key].([]any)
	if !ok {
		if strings.TrimSpace(mcpArg(args, key)) != "" {
			return []string{mcpArg(args, key)}
		}
		return nil
	}
	out := []string{}
	for _, item := range raw {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func mcpMapArg(args map[string]any, key string) map[string]any {
	if args == nil {
		return nil
	}
	if v, ok := args[key].(map[string]any); ok {
		return v
	}
	return nil
}

func mcpHasArg(args map[string]any, key string) bool {
	if args == nil {
		return false
	}
	_, ok := args[key]
	return ok
}

func mcpToolResult(v any) map[string]any {
	b, _ := json.MarshalIndent(v, "", "  ")
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}}
}

func mcpTaskInput(args map[string]any, subtask bool) (TaskInput, error) {
	title, err := mcpRequireArg(args, "title")
	if err != nil {
		return TaskInput{}, err
	}
	goal, err := mcpRequireArg(args, "goal")
	if err != nil {
		return TaskInput{}, err
	}
	in := TaskInput{
		ProjectID:          mcpArg(args, "project_id"),
		RepoID:             mcpArg(args, "repo_id"),
		WorkspaceID:        mcpArg(args, "workspace_id"),
		ParentTaskID:       mcpArg(args, "parent_task_id"),
		Title:              title,
		Goal:               goal,
		Type:               mcpArg(args, "type"),
		Status:             mcpArg(args, "status"),
		Priority:           mcpArg(args, "priority"),
		Scope:              mcpStringSliceArg(args, "scope"),
		Requirements:       mcpStringSliceArg(args, "requirements"),
		CompletionCriteria: mcpStringSliceArg(args, "completion_criteria"),
		Risks:              mcpStringSliceArg(args, "risks"),
		Blockers:           mcpStringSliceArg(args, "blockers"),
		PrivacyLevel:       mcpArg(args, "privacy_level"),
	}
	if subtask {
		in.ProjectID = ""
		in.RepoID = ""
		in.WorkspaceID = ""
		in.ParentTaskID = ""
		in.Status = ""
		in.PrivacyLevel = ""
	}
	return in, nil
}

func mcpTaskUpdateInput(args map[string]any) map[string]any {
	body := map[string]any{"reason": mcpArg(args, "reason")}
	for _, key := range []string{"title", "goal", "type", "priority", "project_id", "repo_id", "workspace_id", "parent_task_id", "privacy_level"} {
		if value := mcpArg(args, key); value != "" {
			body[key] = value
		}
	}
	for _, key := range []string{"scope", "requirements", "completion_criteria", "risks", "blockers"} {
		if mcpHasArg(args, key) {
			body[key] = mcpStringSliceArg(args, key)
		}
	}
	return body
}

func mcpAppendTaskFields(args map[string]any) (Task, error) {
	taskID, err := mcpRequireArg(args, "task_id")
	if err != nil {
		return Task{}, err
	}
	var detail TaskDetail
	if err := request("GET", "/api/tasks/"+url.PathEscape(taskID), nil, &detail); err != nil {
		return Task{}, err
	}
	body := map[string]any{"reason": mcpArg(args, "reason")}
	if mcpHasArg(args, "scope") {
		body["scope"] = appendUniqueStrings(detail.Task.Scope, mcpStringSliceArg(args, "scope")...)
	}
	if mcpHasArg(args, "requirements") {
		body["requirements"] = appendUniqueStrings(detail.Task.Requirements, mcpStringSliceArg(args, "requirements")...)
	}
	if mcpHasArg(args, "completion_criteria") {
		body["completion_criteria"] = appendUniqueStrings(detail.Task.CompletionCriteria, mcpStringSliceArg(args, "completion_criteria")...)
	}
	if mcpHasArg(args, "risks") {
		body["risks"] = appendUniqueStrings(detail.Task.Risks, mcpStringSliceArg(args, "risks")...)
	}
	if mcpHasArg(args, "blockers") {
		body["blockers"] = appendUniqueStrings(detail.Task.Blockers, mcpStringSliceArg(args, "blockers")...)
	}
	var out Task
	if err := request("PATCH", "/api/tasks/"+url.PathEscape(taskID), body, &out); err != nil {
		return Task{}, err
	}
	return out, nil
}

func appendUniqueStrings(base []string, additions ...string) []string {
	out := append([]string{}, base...)
	seen := map[string]bool{}
	for _, item := range out {
		seen[strings.ToLower(strings.TrimSpace(item))] = true
	}
	for _, item := range additions {
		item = strings.TrimSpace(item)
		key := strings.ToLower(item)
		if item == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, item)
	}
	return out
}

func mcpReadTaskDetail(args map[string]any) (TaskDetail, error) {
	taskID, err := mcpRequireArg(args, "task_id")
	if err != nil {
		return TaskDetail{}, err
	}
	var detail TaskDetail
	if err := request("GET", "/api/tasks/"+url.PathEscape(taskID), nil, &detail); err != nil {
		return TaskDetail{}, err
	}
	return detail, nil
}

func mcpFilteredTasks(args map[string]any) ([]Task, error) {
	path := "/api/tasks"
	query := url.Values{}
	if projectID := mcpArg(args, "project_id"); projectID != "" {
		query.Set("project_id", projectID)
	}
	if repoID := mcpArg(args, "repo_id"); repoID != "" {
		query.Set("repo_id", repoID)
	}
	if workspaceID := mcpArg(args, "workspace_id"); workspaceID != "" {
		query.Set("workspace_id", workspaceID)
	}
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var tasks []Task
	if err := request("GET", path, nil, &tasks); err != nil {
		return nil, err
	}
	filtered := filterMCPTasks(tasks, mcpTaskFilter{
		Query:            mcpArg(args, "query"),
		Status:           mcpArg(args, "status"),
		OwnerID:          mcpArg(args, "owner_id"),
		Priority:         mcpArg(args, "priority"),
		IncludeCompleted: mcpBoolArg(args, "include_completed"),
		Limit:            mcpIntArg(args, "limit", 20),
	})
	return filtered, nil
}

type mcpTaskFilter struct {
	Query            string
	Status           string
	OwnerID          string
	Priority         string
	IncludeCompleted bool
	Limit            int
}

func filterMCPTasks(tasks []Task, filter mcpTaskFilter) []Task {
	queryTerms := strings.Fields(strings.ToLower(strings.TrimSpace(filter.Query)))
	out := []Task{}
	for _, task := range tasks {
		if !filter.IncludeCompleted && (task.Status == "completed" || task.Status == "cancelled") {
			continue
		}
		if filter.Status != "" && task.Status != filter.Status {
			continue
		}
		if filter.OwnerID != "" && task.OwnerID != filter.OwnerID {
			continue
		}
		if filter.Priority != "" && task.Priority != filter.Priority {
			continue
		}
		if len(queryTerms) > 0 && !mcpTaskMatchesQuery(task, queryTerms) {
			continue
		}
		out = append(out, task)
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func mcpTaskMatchesQuery(task Task, terms []string) bool {
	haystack := strings.ToLower(strings.Join([]string{
		task.ID,
		task.Title,
		task.Goal,
		task.Type,
		task.Status,
		task.Priority,
		task.OwnerID,
		strings.Join(task.Scope, " "),
		strings.Join(task.Requirements, " "),
		strings.Join(task.CompletionCriteria, " "),
		strings.Join(task.Risks, " "),
		strings.Join(task.Blockers, " "),
		task.SearchText,
	}, " "))
	for _, term := range terms {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	return true
}

func mcpTaskQueryResponse(summary string, tasks []Task, warnings []string) map[string]any {
	if len(tasks) == 0 {
		summary = "No matching TaskPilot tasks found."
	}
	return map[string]any{"summary": summary, "matched_count": len(tasks), "records": tasks, "warnings": warnings}
}

func mcpActiveLocks(args map[string]any) ([]Lock, []string, error) {
	tasks, err := mcpFilteredTasks(map[string]any{"project_id": mcpArg(args, "project_id"), "include_completed": true, "limit": 1000})
	if err != nil {
		return nil, nil, err
	}
	scope := mcpArg(args, "scope")
	limit := mcpIntArg(args, "limit", 50)
	locks := []Lock{}
	warnings := []string{}
	for _, task := range tasks {
		if task.ActiveLockCount == 0 {
			continue
		}
		var taskLocks []Lock
		if err := request("GET", "/api/tasks/"+url.PathEscape(task.ID)+"/locks?active=true", nil, &taskLocks); err != nil {
			warnings = append(warnings, "Could not read locks for "+task.ID+": "+err.Error())
			continue
		}
		for _, lock := range taskLocks {
			if scope != "" && !scopeOverlaps(scope, lock.Scope) {
				continue
			}
			locks = append(locks, lock)
			if limit > 0 && len(locks) >= limit {
				return locks, warnings, nil
			}
		}
	}
	return locks, warnings, nil
}

func mcpTaskMemory(detail TaskDetail) map[string]any {
	return map[string]any{
		"summary":             "Compact TaskPilot memory for " + detail.Task.ID + ".",
		"task":                detail.Task,
		"context":             compactContextEntries(detail.Context, 40),
		"decisions":           limitDecisions(detail.Decisions, 20),
		"comments":            limitComments(detail.Comments, 20),
		"artifacts":           limitArtifacts(detail.Artifacts, 20),
		"git_refs":            limitGitRefs(detail.GitRefs, 20),
		"locks":               detail.Locks,
		"handoffs":            detail.Handoffs,
		"handoff_packet":      detail.HandoffPacket,
		"handoff_checkpoints": detail.HandoffCheckpoints,
	}
}

func mcpAskTaskPilot(args map[string]any) (map[string]any, error) {
	query, err := mcpRequireArg(args, "query")
	if err != nil {
		return nil, err
	}
	limit := mcpIntArg(args, "limit", 8)
	if taskID := mcpArg(args, "task_id"); taskID != "" {
		detail, err := mcpReadTaskDetail(map[string]any{"task_id": taskID})
		if err != nil {
			return nil, err
		}
		evidence := mcpEvidenceFromDetail(detail, query, limit)
		return mcpAskResponse(query, evidence), nil
	}
	tasks, err := mcpFilteredTasks(map[string]any{"query": query, "project_id": mcpArg(args, "project_id"), "include_completed": mcpBoolArg(args, "include_completed"), "limit": limit})
	if err != nil {
		return nil, err
	}
	evidence := []map[string]any{}
	for _, task := range tasks {
		evidence = append(evidence, map[string]any{"type": "task", "task_id": task.ID, "title": task.Title, "status": task.Status, "priority": task.Priority, "owner_id": task.OwnerID, "goal": task.Goal, "blockers": task.Blockers, "risks": task.Risks, "scope": task.Scope})
	}
	return mcpAskResponse(query, evidence), nil
}

func mcpEvidenceFromDetail(detail TaskDetail, query string, limit int) []map[string]any {
	terms := strings.Fields(strings.ToLower(query))
	evidence := []map[string]any{}
	add := func(record map[string]any) {
		if limit > 0 && len(evidence) >= limit {
			return
		}
		evidence = append(evidence, record)
	}
	if len(terms) == 0 || mcpTaskMatchesQuery(detail.Task, terms) {
		add(map[string]any{"type": "task", "task_id": detail.Task.ID, "title": detail.Task.Title, "status": detail.Task.Status, "priority": detail.Task.Priority, "goal": detail.Task.Goal, "blockers": detail.Task.Blockers, "risks": detail.Task.Risks, "scope": detail.Task.Scope})
	}
	for _, decision := range detail.Decisions {
		text := strings.Join([]string{decision.Decision, decision.Reason, decision.Impact, strings.Join(decision.Alternatives, " ")}, " ")
		if mcpTextMatchesTerms(text, terms) {
			add(map[string]any{"type": "decision", "task_id": decision.TaskID, "decision": decision.Decision, "reason": decision.Reason, "impact": decision.Impact, "created_at": decision.CreatedAt})
		}
	}
	for _, entry := range compactContextEntries(detail.Context, 80) {
		if mcpTextMatchesTerms(entry.Kind+" "+entry.Content, terms) {
			add(map[string]any{"type": "context", "task_id": entry.TaskID, "kind": entry.Kind, "content": entry.Content, "created_at": entry.CreatedAt})
		}
	}
	for _, artifact := range detail.Artifacts {
		if mcpTextMatchesTerms(artifact.Kind+" "+artifact.Title+" "+artifact.URI+" "+artifact.Description, terms) {
			add(map[string]any{"type": "artifact", "task_id": artifact.TaskID, "kind": artifact.Kind, "title": artifact.Title, "uri": artifact.URI, "description": artifact.Description})
		}
	}
	for _, lock := range detail.Locks {
		if mcpTextMatchesTerms(lock.Scope+" "+lock.ScopeType+" "+lock.Status+" "+lock.OwnerID, terms) {
			add(map[string]any{"type": "lock", "task_id": lock.TaskID, "scope": lock.Scope, "scope_type": lock.ScopeType, "status": lock.Status, "owner_id": lock.OwnerID, "expires_at": lock.ExpiresAt})
		}
	}
	if len(evidence) == 0 {
		add(map[string]any{"type": "task", "task_id": detail.Task.ID, "title": detail.Task.Title, "status": detail.Task.Status, "goal": detail.Task.Goal})
	}
	return evidence
}

func mcpTextMatchesTerms(text string, terms []string) bool {
	if len(terms) == 0 {
		return true
	}
	haystack := strings.ToLower(text)
	for _, term := range terms {
		if strings.Contains(haystack, term) {
			return true
		}
	}
	return false
}

func mcpAskResponse(query string, evidence []map[string]any) map[string]any {
	summary := "No matching TaskPilot records found."
	if len(evidence) > 0 {
		summary = fmt.Sprintf("Found %d TaskPilot record(s) relevant to %q.", len(evidence), query)
	}
	return map[string]any{
		"answer_summary":      summary,
		"evidence":            evidence,
		"matched_count":       len(evidence),
		"suggested_followups": []string{"Use read_task or read_task_memory for a specific task ID when you need complete context."},
	}
}

func mcpSummarizeTask(detail TaskDetail) map[string]any {
	summaries := []string{}
	next := []string{}
	outputs := []string{}
	contextBlockers := []string{}
	risks := append([]string{}, detail.Task.Risks...)
	for _, entry := range compactContextEntries(detail.Context, 80) {
		switch entry.Kind {
		case "summary":
			summaries = append(summaries, entry.Content)
		case "next":
			next = append(next, entry.Content)
		case "output_ref":
			outputs = append(outputs, entry.Content)
		case "risk":
			risks = append(risks, entry.Content)
		case "blocker":
			contextBlockers = append(contextBlockers, entry.Content)
		}
	}
	return map[string]any{
		"summary":              "Task " + detail.Task.ID + ": " + detail.Task.Title,
		"task":                 detail.Task,
		"owner":                detail.Owner,
		"recent_summaries":     limitStrings(uniqueStrings(summaries), 8),
		"decisions":            limitDecisions(detail.Decisions, 8),
		"blockers":             limitStrings(uniqueStrings(append(append([]string{}, detail.Task.Blockers...), contextBlockers...)), 8),
		"risks":                limitStrings(uniqueStrings(risks), 8),
		"next_steps":           limitStrings(uniqueStrings(next), 8),
		"outputs":              limitStrings(uniqueStrings(outputs), 8),
		"artifacts":            limitArtifacts(detail.Artifacts, 8),
		"git_refs":             limitGitRefs(detail.GitRefs, 8),
		"active_locks":         activeLocksOnly(detail.Locks),
		"handoff_summary":      relatedHandoffSummary(detail),
		"event_count":          len(detail.Events),
		"subtask_count":        len(detail.Subtasks),
		"dependency_count":     len(detail.Dependencies),
		"dependent_count":      len(detail.Dependents),
		"checkpoint_count":     len(detail.HandoffCheckpoints),
		"has_handoff_packet":   detail.HandoffPacket != nil,
		"latest_snapshot_id":   latestSnapshotID(detail),
		"latest_handoff_id":    latestHandoffID(detail.Handoffs),
		"latest_checkpoint_id": latestCheckpointID(detail.HandoffCheckpoints),
	}
}

func activeLocksOnly(locks []Lock) []Lock {
	out := []Lock{}
	for _, lock := range locks {
		if lock.ReleasedAt == nil && (lock.Status == "active" || lock.Status == "stale") {
			out = append(out, lock)
		}
	}
	return out
}

func latestSnapshotID(detail TaskDetail) string {
	if detail.LatestSnapshot != nil {
		return detail.LatestSnapshot.ID
	}
	return ""
}

func latestHandoffID(handoffs []Handoff) string {
	if len(handoffs) == 0 {
		return ""
	}
	return handoffs[0].ID
}

func latestCheckpointID(checkpoints []HandoffCheckpoint) string {
	if len(checkpoints) == 0 {
		return ""
	}
	return checkpoints[len(checkpoints)-1].ID
}

func mcpSummarizeProject(args map[string]any) (map[string]any, error) {
	tasks, err := mcpFilteredTasks(map[string]any{"project_id": mcpArg(args, "project_id"), "include_completed": mcpBoolArg(args, "include_completed"), "limit": mcpIntArg(args, "limit", 100)})
	if err != nil {
		return nil, err
	}
	statusCounts := map[string]int{}
	priorityCounts := map[string]int{}
	blocked := []Task{}
	inProgress := []Task{}
	ready := []Task{}
	for _, task := range tasks {
		statusCounts[task.Status]++
		priorityCounts[task.Priority]++
		if task.Status == "blocked" || len(task.Blockers) > 0 || task.OpenDependencyCount > 0 {
			blocked = append(blocked, task)
		}
		if task.Status == "in_progress" || task.Status == "claimed" {
			inProgress = append(inProgress, task)
		}
		if task.Status == "ready" {
			ready = append(ready, task)
		}
	}
	return map[string]any{
		"summary":         fmt.Sprintf("Project summary from %d TaskPilot task(s).", len(tasks)),
		"task_count":      len(tasks),
		"status_counts":   statusCounts,
		"priority_counts": priorityCounts,
		"blocked_tasks":   limitTasks(blocked, 10),
		"active_tasks":    limitTasks(inProgress, 10),
		"ready_tasks":     limitTasks(ready, 10),
	}, nil
}

func limitTasks(tasks []Task, limit int) []Task {
	if len(tasks) <= limit {
		return tasks
	}
	return tasks[:limit]
}

func mcpFindDecisions(args map[string]any) (map[string]any, error) {
	details, err := mcpDetailsForSearch(args)
	if err != nil {
		return nil, err
	}
	query := mcpArg(args, "query")
	limit := mcpIntArg(args, "limit", 20)
	records := []map[string]any{}
	for _, detail := range details {
		for _, decision := range detail.Decisions {
			text := strings.Join([]string{decision.Decision, decision.Reason, decision.Impact, strings.Join(decision.Alternatives, " ")}, " ")
			if query == "" || mcpTextMatchesTerms(text, strings.Fields(strings.ToLower(query))) {
				records = append(records, map[string]any{"task_id": detail.Task.ID, "task_title": detail.Task.Title, "decision": decision})
				if limit > 0 && len(records) >= limit {
					return map[string]any{"summary": fmt.Sprintf("Found %d matching decisions.", len(records)), "records": records}, nil
				}
			}
		}
	}
	return map[string]any{"summary": fmt.Sprintf("Found %d matching decisions.", len(records)), "records": records}, nil
}

func mcpFindBlockers(args map[string]any) (map[string]any, error) {
	details, err := mcpDetailsForSearch(args)
	if err != nil {
		return nil, err
	}
	query := mcpArg(args, "query")
	terms := strings.Fields(strings.ToLower(query))
	limit := mcpIntArg(args, "limit", 20)
	records := []map[string]any{}
	add := func(task Task, source, text string) bool {
		if query != "" && !mcpTextMatchesTerms(text, terms) {
			return false
		}
		records = append(records, map[string]any{"task_id": task.ID, "task_title": task.Title, "source": source, "text": text})
		return limit > 0 && len(records) >= limit
	}
	for _, detail := range details {
		for _, blocker := range detail.Task.Blockers {
			if add(detail.Task, "task.blockers", blocker) {
				return map[string]any{"summary": fmt.Sprintf("Found %d blockers.", len(records)), "records": records}, nil
			}
		}
		for _, entry := range detail.Context {
			if entry.Stage == "superseded" {
				continue
			}
			if entry.Kind == "blocker" && add(detail.Task, "context", entry.Content) {
				return map[string]any{"summary": fmt.Sprintf("Found %d blockers.", len(records)), "records": records}, nil
			}
		}
		for _, dep := range detail.Dependencies {
			label := dep.DependsOnID
			if dep.DependsOnTask != nil {
				label = dep.DependsOnTask.Title + " (" + dep.DependsOnTask.Status + ")"
			}
			if add(detail.Task, "dependency", label) {
				return map[string]any{"summary": fmt.Sprintf("Found %d blockers.", len(records)), "records": records}, nil
			}
		}
	}
	return map[string]any{"summary": fmt.Sprintf("Found %d blockers.", len(records)), "records": records}, nil
}

func mcpFindOutputs(args map[string]any) (map[string]any, error) {
	details, err := mcpDetailsForSearch(args)
	if err != nil {
		return nil, err
	}
	query := mcpArg(args, "query")
	terms := strings.Fields(strings.ToLower(query))
	limit := mcpIntArg(args, "limit", 20)
	records := []map[string]any{}
	add := func(record map[string]any, text string) bool {
		if query != "" && !mcpTextMatchesTerms(text, terms) {
			return false
		}
		records = append(records, record)
		return limit > 0 && len(records) >= limit
	}
	for _, detail := range details {
		for _, entry := range detail.Context {
			if entry.Stage == "superseded" {
				continue
			}
			if entry.Kind == "output_ref" {
				if add(map[string]any{"type": "context_output", "task_id": detail.Task.ID, "task_title": detail.Task.Title, "content": entry.Content}, entry.Content) {
					return map[string]any{"summary": fmt.Sprintf("Found %d outputs.", len(records)), "records": records}, nil
				}
			}
		}
		for _, artifact := range detail.Artifacts {
			text := artifact.Kind + " " + artifact.Title + " " + artifact.URI + " " + artifact.Description
			if add(map[string]any{"type": "artifact", "task_id": detail.Task.ID, "task_title": detail.Task.Title, "artifact": artifact}, text) {
				return map[string]any{"summary": fmt.Sprintf("Found %d outputs.", len(records)), "records": records}, nil
			}
		}
		for _, gitRef := range detail.GitRefs {
			text := gitRef.Branch + " " + gitRef.CommitSHA + " " + gitRef.PRURL + " " + strings.Join(gitRef.ChangedFiles, " ") + " " + gitRef.Note
			if add(map[string]any{"type": "git_ref", "task_id": detail.Task.ID, "task_title": detail.Task.Title, "git_ref": gitRef}, text) {
				return map[string]any{"summary": fmt.Sprintf("Found %d outputs.", len(records)), "records": records}, nil
			}
		}
	}
	return map[string]any{"summary": fmt.Sprintf("Found %d outputs.", len(records)), "records": records}, nil
}

func mcpDetailsForSearch(args map[string]any) ([]TaskDetail, error) {
	if taskID := mcpArg(args, "task_id"); taskID != "" {
		detail, err := mcpReadTaskDetail(map[string]any{"task_id": taskID})
		if err != nil {
			return nil, err
		}
		return []TaskDetail{detail}, nil
	}
	tasks, err := mcpFilteredTasks(map[string]any{"project_id": mcpArg(args, "project_id"), "include_completed": mcpBoolArg(args, "include_completed"), "limit": mcpIntArg(args, "limit", 50)})
	if err != nil {
		return nil, err
	}
	details := []TaskDetail{}
	for _, task := range tasks {
		var detail TaskDetail
		if err := request("GET", "/api/tasks/"+url.PathEscape(task.ID), nil, &detail); err != nil {
			continue
		}
		details = append(details, detail)
	}
	return details, nil
}

func agentInstructions(taskID string) string {
	return `You are working inside TaskPilot coordination.

TaskPilot is the shared task memory for humans and agents across machines. Treat it as the source of truth for task status, ownership, decisions, handoffs, and coordination.

Current task:
- TASKPILOT_TASK_ID=` + taskID + `
- Use TASKPILOT_SERVER when calling TaskPilot.
- Use TASKPILOT_ACTOR_ID and TASKPILOT_ACTOR_SESSION_ID as your agent identity.
- Read TASKPILOT_TASK_CONTEXT_FILE for the current task snapshot.
- Read TASKPILOT_RELATED_CONTEXT_FILE for selected prior/linked work context.
- Write task progress to TASKPILOT_RUN_CONTEXT_FILE.
- Keep TASKPILOT_HANDOFF_FILE updated as the transfer-ready memory for the next agent.

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
Update TASKPILOT_HANDOFF_FILE after every meaningful prompt response or work unit, then run ` + "`taskpilot handoff checkpoint $TASKPILOT_TASK_ID --file \"$TASKPILOT_HANDOFF_FILE\"`" + `. This file is the authoritative handoff draft. Do not erase previous completed work or decisions; append the new truth and update current state / remaining work / next steps. It must include completed work, important decisions, current state, remaining work, suggested next steps, and a handoff message. If no material decision was made, write exactly: No material decision made; work followed existing requirements.

` + handoffWritingRules() + `

Write context that would let a different agent continue the work without reading this chat. Prefer short, specific entries over vague status updates.

Accepted context formats:
- summary: Traced invite signup failure to expiry comparison after token lookup.
- finding: Token format is reused by existing invite links, so changing it would break old emails.
- decision: Patch expiry comparison only; keep token format unchanged to preserve compatibility.
- rationale: The failure is after validation, so DB schema changes are unnecessary.
- rejected: Rejected adding a new invite_tokens table because the existing token record has enough state.
- risk: Timezone handling may still be fragile around midnight UTC.
- blocker: Need a real expired invite sample before changing cleanup behavior.
- files: src/auth/invite.go, src/auth/invite_test.go
- verification: go test ./src/auth passed after adding invited-user regression coverage.
- next: Add one regression test for already-used invite tokens.
- {"kind":"decision","content":"Patch expiry comparison only because expiry validation is the failing boundary."}

Recommended update timing:
- After reading important task context, write a summary only if it changes the plan.
- After finding a root cause, write a finding or summary.
- After making a decision, write a decision and include the reason/tradeoff.
- After rejecting an approach, write a rejected entry so the next agent does not repeat it.
- After discovering a risk, assumption, or blocker, write it immediately.
- After changing files or creating outputs, write files or output_ref.
- After running tests/checks, write verification.
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

TaskPilot is the system of record for real repository work. Do not treat task
creation as optional just because the user did not provide a task ID.

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

When the user starts work without a TaskPilot task ID:

1. Inspect TaskPilot context before editing: active session task, likely repo
   task, existing open tasks, recent semantic memory, changed files, locks, and
   relationships.
2. If there is a matching active or open task for the same objective, reuse it,
   claim it, acquire needed locks, and record all later memory against it.
3. If there is no matching task and the request is real repository work, create
   the owning task before or immediately after the first safe inspection step.
4. "Real repository work" includes implementation, bug fixing, refactoring,
   tests, documentation that decides or changes project direction, planning
   artifacts, technical decisions, architecture notes, dependency choices,
   schema/API changes, release/deployment work, and any multi-step investigation
   whose result should be remembered.
5. Tiny mechanical requests may skip task creation only when they are clearly
   disposable and do not create a decision, product direction, or future
   coordination need. Even then, record semantic memory only if it can be tied
   to a suitable existing task or clearly queued as lightweight context.
6. If TaskPilot is unavailable, unauthorized, or not enabled, say so, continue
   only as far as the user's request safely allows, and record the failed
   TaskPilot action in your final response or handoff.

# Task Intelligence Rules

Before creating a TaskPilot task or recording repo memory:

1. Inspect the active task context, likely current repo task, related tasks,
   recent memory, changed files, and existing relationships.
2. Prefer an existing task when the objective, active session, semantic memory,
   or changed files match the current work.
3. Create a subtask for a smaller piece of an existing objective.
4. Create a new task only for a distinct outcome, and connect it to related
   work with ` + "`parent_of`" + `, ` + "`subtask_of`" + `, ` + "`related_to`" + `, ` + "`depends_on`" + `, ` + "`blocks`" + `,
   ` + "`continues`" + `, ` + "`duplicates`" + `, or ` + "`supersedes`" + ` when relevant.
5. Treat the first meaningful work in a newly enabled repo as a distinct
   outcome that needs an initial owning task unless a matching task already
   exists.
6. Use one primary task for one logical outcome. Keep follow-up prompts in the
   same session on that task when they deepen the same outcome, but create a new
   related task when the user switches to a different deliverable, decision,
   bug, feature, or investigation.
7. A prompt that creates or updates a technical decision, planning document,
   architecture direction, API contract, schema, dependency choice, or release
   choice must have an owning task. Do not record only semantic memory for that
   work.
8. Record semantic memory against the task that owns the work, not a fresh
   file-based task and not an unassigned repo-level note when a task should
   exist.
9. Use outcome-based task names such as ` + "`Define Snake game technical decisions`" + `
   or ` + "`Fix semantic memory routing for active repo tasks`" + `; avoid names like
   ` + "`Update controls.md`" + `, ` + "`Update technicaldecisions.md`" + `, or
   ` + "`Inferred work on 6 changed files`" + `.
10. Include the intended outcome, reasoning, verification, files, task
    selection or creation rationale, and remaining work when recording semantic
    memory.
11. Improve inferred task titles, goals, scope, and relationships when better
    context becomes available.
12. If a task decision is ambiguous, choose the safest auditable action: reuse
    high-confidence matches, create a related task for a distinct moderate-
    confidence outcome, and document why.

Session boundary examples:

- Same task: "create planning.md for Snake" followed by "make an initial brief
  of technicaldecisions.md from planning.md"; both belong to the same game
  planning outcome unless the repo already has a separate technical-decisions
  task.
- New related task: after planning the Snake game, the user asks to implement
  gameplay, fix deployment, change authentication, or choose a production
  persistence architecture.
- Subtask: the user asks for a smaller bounded part of a known parent outcome,
  such as "add keyboard input handling" under an implementation task.
- No new task: the user asks a one-line question, requests a read-only status
  check, or asks for a disposable local scratch file with no future coordination
  value.
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
		return fmt.Errorf("usage: taskpilot repo create|list|repair")
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
	case "repair":
		fs := flag.NewFlagSet("repo repair", flag.ExitOnError)
		repoPath := fs.String("repo", ".", "repo path")
		dryRun := fs.Bool("dry-run", false, "show planned repairs without writing")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		out, err := repairRepoState(*repoPath, *dryRun)
		if err != nil {
			return err
		}
		return print(out, *jsonOut)
	default:
		return fmt.Errorf("unknown repo command %q", args[0])
	}
}

func repairRepoState(repoPath string, dryRun bool) (map[string]any, error) {
	activity, err := currentRepoActivity(repoPath)
	if err != nil {
		return nil, err
	}
	tasks, err := tasksForRepo(activity.Config.RepoID, activity.Config.ProjectID)
	if err != nil {
		return nil, err
	}
	actions := []string{}
	for _, task := range tasks {
		cleanScope := filterProductRepoFiles(task.Scope)
		patch := map[string]any{}
		if !sameStringSet(cleanScope, task.Scope) {
			patch["scope"] = cleanScope
			actions = append(actions, "clean task scope "+task.ID)
		}
		if strings.HasPrefix(strings.ToLower(task.Title), "inferred work on") && len(cleanScope) > 0 {
			patch["title"] = "Update " + strings.Join(limitStrings(cleanScope, 3), ", ")
			actions = append(actions, "rename inferred task "+task.ID)
		}
		if len(patch) > 0 && !dryRun {
			patch["reason"] = "repo repair removed TaskPilot-managed noise"
			var updated Task
			_ = request("PATCH", "/api/tasks/"+url.PathEscape(task.ID), patch, &updated)
		}
	}
	locks, _, _ := mcpActiveLocks(map[string]any{"project_id": activity.Config.ProjectID, "limit": 500})
	releasedLocks := 0
	for _, lock := range locks {
		if lock.TaskID == "" || !isTaskPilotManagedRepoFile(lock.Scope) {
			continue
		}
		actions = append(actions, "release TaskPilot-managed lock "+lock.ID+" on "+lock.Scope)
		releasedLocks++
		if !dryRun {
			var out Lock
			_ = request("POST", "/api/locks/"+url.PathEscape(lock.ID)+"/release", map[string]any{"reason": "repo repair released TaskPilot-managed coordination file lock"}, &out)
		}
	}
	return map[string]any{"status": "ok", "dry_run": dryRun, "repo": activity.Config.RepoID, "actions": actions, "released_locks": releasedLocks}, nil
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
	email := fs.String("email", "", "user email for local CLI identity notes")
	_ = fs.Parse(args)
	cfg, _ := loadConfigFile()
	cfg.Server = strings.TrimRight(*server, "/")
	cfg.Email = strings.TrimSpace(*email)
	return saveConfig(cfg)
}

func runConfig(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: taskpilot config show|set-server|set-email OR taskpilot config set-actor <actor-id> <actor-secret>")
	}
	switch args[0] {
	case "show":
		fs := flag.NewFlagSet("config show", flag.ExitOnError)
		effective := fs.Bool("effective", false, "show effective config including environment overrides")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		diagnostics, err := loadConfigDiagnostics(*effective)
		if err != nil {
			return err
		}
		if !*jsonOut {
			printConfigDiagnostics(diagnostics)
			return nil
		}
		b, _ := json.MarshalIndent(diagnostics, "", "  ")
		fmt.Println(string(b))
		return nil
	case "set-server":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot config set-server <url>")
		}
		cfg, _ := loadConfigFile()
		cfg.Server = strings.TrimRight(args[1], "/")
		return saveConfig(cfg)
	case "set-email":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot config set-email <email>")
		}
		cfg, _ := loadConfigFile()
		cfg.Email = strings.TrimSpace(args[1])
		return saveConfig(cfg)
	case "set-actor":
		if len(args) < 3 {
			return fmt.Errorf("usage: taskpilot config set-actor <actor-id> <actor-secret>")
		}
		cfg, _ := loadConfigFile()
		cfg.ActorID = args[1]
		cfg.ActorSecret = args[2]
		if err := saveConfig(cfg); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Warning: config set-actor stores deprecated global actor credentials. Prefer `taskpilot actor activate --secret <actor-secret>` for terminal-scoped sessions.")
		return nil
	default:
		return fmt.Errorf("unknown config command %q", args[0])
	}
}

func runActor(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: taskpilot actor activate|current|deactivate|sessions|run|list")
	}
	switch args[0] {
	case "activate":
		fs := flag.NewFlagSet("actor activate", flag.ExitOnError)
		secret := fs.String("secret", os.Getenv("TASKPILOT_ACTOR_SECRET"), "actor secret")
		actorID := fs.String("actor-id", "", "optional actor id")
		provider := fs.String("provider", detectAgentProvider(nil), "agent provider")
		taskID := fs.String("task-id", os.Getenv("TASKPILOT_TASK_ID"), "current task id")
		repoPath := fs.String("repo", bestEffortGitRoot("."), "repository path")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		if strings.TrimSpace(*secret) == "" {
			return fmt.Errorf("--secret is required")
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if cfg.Server == "" {
			cfg.Server = "http://127.0.0.1:8080"
		}
		activation, err := activateActorSessionWithConfig(cfg, ActorSessionStartInput{
			ActorID:        *actorID,
			ActorSecret:    *secret,
			MachineID:      machineID(),
			TerminalID:     terminalID(),
			AgentProvider:  *provider,
			ProcessID:      os.Getpid(),
			CurrentTaskID:  *taskID,
			ClientVersion:  "taskpilot-cli",
			RepositoryPath: *repoPath,
		})
		if err != nil {
			return err
		}
		session := terminalActorSession{
			Server:            cfg.Server,
			ActorID:           activation.Actor.ID,
			ActorName:         activation.Actor.Name,
			ActorSessionID:    activation.Session.ID,
			ActorSessionToken: activation.SessionToken,
			CurrentTaskID:     activation.Session.CurrentTaskID,
			AgentProvider:     activation.Session.AgentProvider,
			RepositoryPath:    activation.Session.RepositoryPath,
			MachineID:         activation.Session.MachineID,
			TerminalID:        activation.Session.TerminalID,
			ProcessID:         activation.Session.ProcessID,
			Status:            activation.Session.Status,
			StartedAt:         activation.Session.StartedAt,
		}
		if err := saveTerminalActorSession(session); err != nil {
			return err
		}
		if *jsonOut {
			return print(activation, true)
		}
		fmt.Println("Actor activated for this terminal.")
		fmt.Printf("Actor: %s (%s)\n", activation.Actor.Name, activation.Actor.ID)
		fmt.Printf("Session: %s\n", activation.Session.ID)
		fmt.Printf("Server: %s\n", cfg.Server)
		fmt.Printf("Status: %s\n", activation.Session.Status)
		return nil
	case "current":
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		session, ok := loadTerminalActorSession(cfg)
		if !ok && cfg.ActorSessionID != "" {
			session = terminalActorSession{Server: cfg.Server, ActorID: cfg.ActorID, ActorSessionID: cfg.ActorSessionID, ActorSessionToken: cfg.ActorSessionToken, CurrentTaskID: cfg.CurrentTaskID, AgentProvider: cfg.AgentProvider, Status: "active"}
			ok = true
		}
		if !ok {
			fmt.Println("No actor is active in this terminal.")
			if cfg.ActorID != "" && cfg.ActorSecret != "" {
				fmt.Println("A deprecated global actor is configured; the next authenticated command will migrate it into a terminal session.")
			}
			fmt.Println("Activate one with:")
			fmt.Println("taskpilot actor activate --secret <actor-secret>")
			return nil
		}
		var live ActorSession
		_ = request("GET", "/api/actor-sessions/current", nil, &live)
		if live.ID != "" {
			session.Status = live.Status
			session.CurrentTaskID = live.CurrentTaskID
			session.LastHeartbeatAt = live.LastHeartbeatAt
			_ = saveTerminalActorSession(session)
		}
		fmt.Printf("Actor: %s\n", firstNonEmpty(session.ActorName, session.ActorID))
		fmt.Printf("Session: %s\n", session.ActorSessionID)
		fmt.Println("Scope: current terminal")
		if session.CurrentTaskID != "" {
			fmt.Printf("Current task: %s\n", session.CurrentTaskID)
		}
		fmt.Printf("Status: %s\n", firstNonEmpty(session.Status, "active"))
		return nil
	case "deactivate":
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if cfg.ActorSessionID == "" {
			fmt.Println("No actor is active in this terminal.")
			return nil
		}
		var out ActorSession
		_ = request("POST", "/api/actor-sessions/current/end", map[string]any{}, &out)
		if err := clearTerminalActorSession(cfg); err != nil {
			return err
		}
		fmt.Println("Actor session deactivated for this terminal.")
		return nil
	case "sessions":
		fs := flag.NewFlagSet("actor sessions", flag.ExitOnError)
		actorID := fs.String("actor-id", "", "filter by actor id")
		active := fs.Bool("active", false, "only active sessions")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		path := "/api/actor-sessions"
		q := url.Values{}
		if *actorID != "" {
			q.Set("actor_id", *actorID)
		}
		if *active {
			q.Set("active", "true")
		}
		if encoded := q.Encode(); encoded != "" {
			path += "?" + encoded
		}
		var out []ActorSession
		if err := request("GET", path, nil, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "run":
		return runActorSessionCommand(args[1:])
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

func runActorSessionCommand(args []string) error {
	sep := -1
	for i, arg := range args {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || sep == len(args)-1 {
		return fmt.Errorf("usage: taskpilot actor run --secret <actor-secret> [--provider codex] -- <agent-command> [args...]")
	}
	fs := flag.NewFlagSet("actor run", flag.ExitOnError)
	secret := fs.String("secret", os.Getenv("TASKPILOT_ACTOR_SECRET"), "actor secret")
	actorID := fs.String("actor-id", "", "optional actor id")
	provider := fs.String("provider", "", "agent provider")
	taskID := fs.String("task-id", os.Getenv("TASKPILOT_TASK_ID"), "current task id")
	repoPath := fs.String("repo", bestEffortGitRoot("."), "repository path")
	_ = fs.Parse(args[:sep])
	commandArgs := args[sep+1:]
	if strings.TrimSpace(*secret) == "" {
		return fmt.Errorf("--secret is required")
	}
	if *provider == "" {
		*provider = detectAgentProvider(commandArgs)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if cfg.Server == "" {
		cfg.Server = "http://127.0.0.1:8080"
	}
	activation, err := activateActorSessionWithConfig(cfg, ActorSessionStartInput{
		ActorID:        *actorID,
		ActorSecret:    *secret,
		MachineID:      machineID(),
		TerminalID:     terminalID(),
		AgentProvider:  *provider,
		ProcessID:      os.Getpid(),
		CurrentTaskID:  *taskID,
		ClientVersion:  "taskpilot-cli",
		RepositoryPath: *repoPath,
	})
	if err != nil {
		return err
	}
	childCfg := cfg
	childCfg.ActorID = activation.Actor.ID
	childCfg.ActorSessionID = activation.Session.ID
	childCfg.ActorSessionToken = activation.SessionToken
	childCfg.CurrentTaskID = activation.Session.CurrentTaskID
	childCfg.AgentProvider = activation.Session.AgentProvider
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	done := make(chan struct{})
	go actorSessionHeartbeatLoop(ctx, childCfg, done)
	cmd := exec.CommandContext(ctx, commandArgs[0], commandArgs[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(),
		"TASKPILOT_SERVER="+cfg.Server,
		"TASKPILOT_ACTOR_ID="+activation.Actor.ID,
		"TASKPILOT_ACTOR_SESSION_ID="+activation.Session.ID,
		"TASKPILOT_ACTOR_SESSION_TOKEN="+activation.SessionToken,
		"TASKPILOT_TERMINAL_ID="+terminalID(),
		"TASKPILOT_AGENT_PROVIDER="+activation.Session.AgentProvider,
	)
	if activation.Session.CurrentTaskID != "" {
		cmd.Env = append(cmd.Env, "TASKPILOT_TASK_ID="+activation.Session.CurrentTaskID)
	}
	err = cmd.Run()
	close(done)
	_ = doRequestWithConfig(childCfg, "POST", "/api/actor-sessions/current/end", map[string]any{}, nil, true)
	return err
}

func actorSessionHeartbeatLoop(ctx context.Context, cfg Config, done <-chan struct{}) {
	ticker := time.NewTicker(heartbeatInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			_ = doRequestWithConfig(cfg, "POST", "/api/actor-sessions/current/heartbeat", ActorSessionHeartbeat{
				CurrentTaskID: cfg.CurrentTaskID,
				MachineID:     machineID(),
				TerminalID:    terminalID(),
				AgentProvider: cfg.AgentProvider,
				ProcessID:     os.Getpid(),
				Status:        "active",
			}, nil, true)
		}
	}
}

func doRequestWithConfig(cfg Config, method, path string, body any, out any, includeActor bool) error {
	var bodyBytes []byte
	if body != nil {
		bodyBytes, _ = json.Marshal(body)
	}
	data, err := doRequestBytesWithConfig(cfg, method, path, bodyBytes, includeActor)
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
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
		repo := fs.String("repo", "", "repository id")
		workspace := fs.String("workspace", "", "workspace id")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		path := "/api/tasks"
		query := url.Values{}
		if *project != "" {
			query.Set("project_id", *project)
		}
		if *repo != "" {
			query.Set("repo_id", *repo)
		}
		if *workspace != "" {
			query.Set("workspace_id", *workspace)
		}
		if encoded := query.Encode(); encoded != "" {
			path += "?" + encoded
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
	if len(args) < 1 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Println(contextUsage())
		return nil
	}
	switch args[0] {
	case "render":
		fs := flag.NewFlagSet("context render", flag.ExitOnError)
		repoPath := fs.String("repo", ".", "repo path")
		format := fs.String("format", "markdown", "markdown, codex, claude, gemini, or json")
		_ = fs.Parse(args[1:])
		root, err := gitRoot(*repoPath)
		if err != nil {
			return err
		}
		rendered, err := renderRepoContext(root, *format)
		if err != nil {
			return err
		}
		fmt.Println(rendered)
		return nil
	case "checkpoint":
		fs := flag.NewFlagSet("context checkpoint", flag.ExitOnError)
		repoPath := fs.String("repo", ".", "repo path")
		source := fs.String("source", "manual", "memory source: manual, agent-hook, daemon, mcp, ui")
		reason := fs.String("reason", "manual", "checkpoint reason")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		out, err := checkpointRepoContext(*repoPath, *source, *reason)
		if err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "draft-summary":
		fs := flag.NewFlagSet("context draft-summary", flag.ExitOnError)
		repoPath := fs.String("repo", ".", "repo path")
		format := fs.String("format", "markdown", "markdown or json")
		_ = fs.Parse(args[1:])
		draft, err := draftRepoSemanticSummary(*repoPath)
		if err != nil {
			return err
		}
		if strings.EqualFold(*format, "json") {
			return print(draft, true)
		}
		if draft.Status == "noop" {
			fmt.Println("No product file changes detected.")
			return nil
		}
		fmt.Println(draft.Summary)
		return nil
	case "record-semantic":
		fs := flag.NewFlagSet("context record-semantic", flag.ExitOnError)
		repoPath := fs.String("repo", ".", "repo path")
		taskID := fs.String("task-id", "", "explicit TaskPilot task ID to receive memory")
		completed := fs.String("completed-work", "", "what changed or was completed")
		why := fs.String("why", "", "why it changed or important reasoning")
		verification := fs.String("verification", "", "verification performed")
		remaining := fs.String("remaining-work", "", "remaining work or next step")
		files := fs.String("files", "", "comma-separated related product files")
		stage := fs.String("stage", "working", "working or final")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		out, err := recordRepoSemanticMemoryForTask(*repoPath, *taskID, *completed, *why, *verification, *remaining, splitCSV(*files), *stage, "agent-hook", true)
		if err != nil {
			return err
		}
		return print(out, *jsonOut)
	}
	if args[0] != "append" || len(args) < 2 {
		return fmt.Errorf("usage: taskpilot context append <task-id> --kind decision --content text")
	}
	fs := flag.NewFlagSet("context append", flag.ExitOnError)
	kind := fs.String("kind", "note", "context kind")
	content := fs.String("content", "", "content")
	source := fs.String("source", "manual", "context source")
	reason := fs.String("reason", "", "context reason")
	confidence := fs.String("confidence", "", "agent_authored, metadata_inferred, or file_checkpoint")
	files := fs.String("files", "", "comma-separated related files")
	memoryKey := fs.String("memory-key", "", "stable memory key for compacted repo memory")
	stage := fs.String("stage", "", "active, working, or final")
	jsonOut := fs.Bool("json", false, "print JSON")
	id := args[1]
	_ = fs.Parse(args[2:])
	var out ContextEntry
	if err := request("POST", "/api/tasks/"+url.PathEscape(id)+"/context", map[string]any{"kind": *kind, "content": *content, "source": *source, "reason": *reason, "confidence": *confidence, "files": splitCSV(*files), "memory_key": *memoryKey, "stage": *stage}, &out); err != nil {
		return err
	}
	return print(out, *jsonOut)
}

func contextUsage() string {
	return strings.TrimSpace(`usage: taskpilot context <command>

Commands:
  render           Render repo startup context for agents.
  record-semantic  Record or queue agent-authored semantic memory for the active repo task.
  checkpoint       Record or queue a metadata-inferred repo checkpoint.
  draft-summary    Preview the local privacy-safe semantic draft.
  append           Append context to an explicit task ID.

Agent semantic memory fallback:
  taskpilot context record-semantic --repo . --completed-work "..." --why "..." --verification "..." --remaining-work "..." --files path1,path2

Success is either status=recorded or status=queued. Queued memory is flushed by the TaskPilot daemon.`)
}

func checkpointRepoContext(repoPath, source, reason string) (map[string]any, error) {
	return checkpointRepoContextWithQueue(repoPath, source, reason, true)
}

func doRequestViaDaemonProxy(cfg Config, method, path string, body []byte, includeActor bool) ([]byte, error) {
	if strings.TrimSpace(os.Getenv("TASKPILOT_DISABLE_REQUEST_PROXY")) != "" {
		return nil, fmt.Errorf("TaskPilot request proxy disabled")
	}
	queued, responsePath, err := queueAPIRequest(cfg, method, path, body, includeActor)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(apiRequestProxyTimeout())
	for time.Now().Before(deadline) {
		response, err := readQueuedAPIResponse(responsePath)
		if err == nil {
			_ = os.Remove(responsePath)
			if response.Status == "ok" {
				return response.Body, nil
			}
			if strings.TrimSpace(response.Error) != "" {
				return nil, errors.New(response.Error)
			}
			return nil, fmt.Errorf("TaskPilot daemon request %s failed without an error message", queued.ID)
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil, fmt.Errorf("TaskPilot daemon request %s timed out waiting for local daemon response", queued.ID)
}

func apiRequestProxyTimeout() time.Duration {
	if value := strings.TrimSpace(os.Getenv("TASKPILOT_REQUEST_PROXY_TIMEOUT")); value != "" {
		if duration, err := time.ParseDuration(value); err == nil && duration > 0 {
			return duration
		}
	}
	return 20 * time.Second
}

func queueAPIRequest(cfg Config, method, path string, body []byte, includeActor bool) (queuedAPIRequest, string, error) {
	root, err := taskPilotRepoRoot(".")
	if err != nil {
		return queuedAPIRequest{}, "", fmt.Errorf("TaskPilot request proxy requires a TaskPilot-enabled repo: %w", err)
	}
	now := time.Now().UTC()
	seed := fmt.Sprintf("%s\n%s\n%s\n%s\n%d", cfg.Server, cfg.ActorID, method, path, now.UnixNano())
	sum := sha256.Sum256(append([]byte(seed), body...))
	id := fmt.Sprintf("api_request_%x", sum[:8])
	queued := queuedAPIRequest{
		ID:            id,
		Server:        cfg.Server,
		ActorID:       cfg.ActorID,
		Method:        method,
		Path:          path,
		Body:          append([]byte(nil), body...),
		IncludeActor:  includeActor,
		CreatedAt:     now,
		LastAttemptAt: now,
		Attempts:      1,
	}
	paths := queuedAPIRequestCandidatePaths(root, id)
	queued, err = writeQueuedAPIRequestFirstWritable(paths, queued)
	if err != nil {
		return queuedAPIRequest{}, "", err
	}
	return queued, queuedAPIResponsePathForRequestPath(queued.QueuePath), nil
}

func flushQueuedAPIRequests(repoPaths ...string) (int, int, error) {
	cfg, err := loadConfig()
	if err != nil {
		return 0, 0, err
	}
	if cfg.Server == "" {
		cfg.Server = "http://127.0.0.1:8080"
	}
	paths, err := queuedAPIRequestPaths(repoPaths...)
	if err != nil {
		return 0, 0, err
	}
	flushed := 0
	failed := 0
	for _, path := range paths {
		queued, err := readQueuedAPIRequest(path)
		if err != nil {
			failed++
			continue
		}
		if queued.Server != cfg.Server || queued.ActorID != cfg.ActorID {
			continue
		}
		body, err := doRequestBytes(queued.Method, queued.Path, queued.Body, queued.IncludeActor, false)
		response := queuedAPIResponse{ID: queued.ID, Status: "ok", CompletedAt: time.Now().UTC()}
		if err != nil {
			response.Status = "error"
			response.Error = err.Error()
			failed++
			queued.Attempts++
			queued.LastAttemptAt = time.Now().UTC()
			queued.LastError = err.Error()
			_ = writeQueuedAPIRequest(path, queued)
		} else {
			response.Body = body
			flushed++
			_ = os.Remove(path)
		}
		if writeErr := writeQueuedAPIResponse(queuedAPIResponsePathForRequestPath(path), response); writeErr != nil && err == nil {
			failed++
			flushed--
		}
	}
	return flushed, failed, nil
}

func queuedAPIRequestPaths(repoPaths ...string) ([]string, error) {
	return queuedJSONPaths(apiRequestOutboxDirs(repoPaths...)...)
}

func queuedAPIRequestCandidatePaths(repoPath, id string) []string {
	paths := []string{}
	for _, dir := range apiRequestOutboxDirs(repoPath) {
		paths = append(paths, filepath.Join(dir, id+".json"))
	}
	return uniqueStrings(paths)
}

func readQueuedAPIRequest(path string) (queuedAPIRequest, error) {
	var queued queuedAPIRequest
	data, err := os.ReadFile(path)
	if err != nil {
		return queued, err
	}
	return queued, json.Unmarshal(data, &queued)
}

func writeQueuedAPIRequest(path string, queued queuedAPIRequest) error {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(queued, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeQueuedAPIRequestFirstWritable(paths []string, queued queuedAPIRequest) (queuedAPIRequest, error) {
	var errs []error
	for _, path := range uniqueStrings(paths) {
		next := queued
		next.QueuePath = path
		if err := writeQueuedAPIRequest(path, next); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			continue
		}
		return next, nil
	}
	return queuedAPIRequest{}, errors.Join(errs...)
}

func readQueuedAPIResponse(path string) (queuedAPIResponse, error) {
	var response queuedAPIResponse
	data, err := os.ReadFile(path)
	if err != nil {
		return response, err
	}
	return response, json.Unmarshal(data, &response)
}

func writeQueuedAPIResponse(path string, response queuedAPIResponse) error {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(response, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func queuedAPIResponsePathForRequestPath(path string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(path)), "api-responses", filepath.Base(path))
}

func apiRequestOutboxDirs(repoPaths ...string) []string {
	dirs := []string{}
	for _, repoPath := range repoPaths {
		root := repoOutboxRoot(repoPath)
		if root == "" {
			continue
		}
		dirs = append(dirs, filepath.Join(root, ".taskpilot", "outbox", "api-requests"))
	}
	return uniqueStrings(dirs)
}

func recordRepoSemanticMemory(repoPath, completed, why, verification, remaining string, files []string, stage, source string, queueOnRetriable bool) (map[string]any, error) {
	return recordRepoSemanticMemoryForTask(repoPath, "", completed, why, verification, remaining, files, stage, source, queueOnRetriable)
}

func currentActorSessionTaskID() string {
	if taskID := strings.TrimSpace(os.Getenv("TASKPILOT_TASK_ID")); taskID != "" {
		return taskID
	}
	cfg, err := loadConfig()
	if err != nil {
		return ""
	}
	if cfg.CurrentTaskID != "" {
		return cfg.CurrentTaskID
	}
	if cfg.ActorSessionID == "" || cfg.ActorSessionToken == "" {
		return ""
	}
	var session ActorSession
	if err := doRequestWithConfig(cfg, "GET", "/api/actor-sessions/current", nil, &session, true); err != nil {
		return ""
	}
	return strings.TrimSpace(session.CurrentTaskID)
}

func recordRepoSemanticMemoryForTask(repoPath, explicitTaskID, completed, why, verification, remaining string, files []string, stage, source string, queueOnRetriable bool) (map[string]any, error) {
	completed = strings.TrimSpace(completed)
	if completed == "" {
		return nil, fmt.Errorf("completed work is required")
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = "mcp"
	}
	activity, err := currentRepoActivity(repoPath)
	if err != nil {
		return nil, err
	}
	intent := repoWorkIntent{Kind: "semantic_memory", Completed: completed, Why: why, Verification: verification, Remaining: remaining, Files: files, Stage: stage, Source: source}
	explicitTaskID = strings.TrimSpace(explicitTaskID)
	sessionTaskID := ""
	if explicitTaskID == "" {
		sessionTaskID = currentActorSessionTaskID()
		explicitTaskID = sessionTaskID
	}
	var task Task
	var match repoTaskMatch
	if explicitTaskID != "" {
		var detail TaskDetail
		err = requestNoProxy("GET", "/api/tasks/"+url.PathEscape(explicitTaskID), nil, &detail)
		if err == nil {
			task = detail.Task
			reason := "explicit task id provided by agent"
			evidence := "explicit_task_id: " + explicitTaskID
			if sessionTaskID != "" {
				reason = "current actor session task"
				evidence = "actor_session_current_task_id: " + sessionTaskID
			}
			match = repoTaskMatch{Task: task, Score: 1000, Confidence: 0.99, Action: "reuse", Reasons: []string{reason}, Evidence: []string{evidence}}
		}
	} else {
		task, match, err = ensureTaskForRepoActivityWithIntentWithProxy(activity, intent, false)
	}
	if err != nil {
		if queued, ok, queueErr := queueRepoSemanticMemoryOnRetriable(repoPath, completed, why, verification, remaining, files, stage, source, explicitTaskID, err, queueOnRetriable); ok {
			return map[string]any{"status": "queued", "reason": "TaskPilot server unavailable; semantic memory queued locally for daemon retry", "queued_memory": queued}, queueErr
		}
		return nil, err
	}
	files = filterProductRepoFiles(files)
	if len(files) == 0 {
		files = filterProductRepoFiles(activity.ChangedFiles)
	}
	stage = strings.TrimSpace(stage)
	if stage == "" {
		stage = "working"
	}
	content := semanticMemoryContent(completed, why, verification, remaining)
	var entry ContextEntry
	decision := repoTaskIntelligenceDecision("route_semantic_memory", match)
	body := map[string]any{"kind": "summary", "content": content, "source": source, "reason": "semantic_memory", "confidence": "agent_authored", "files": files, "memory_key": repoMemoryKey(activity, task.ID, files), "stage": stage, "intelligence_decision": decision}
	if err := requestNoProxy("POST", "/api/tasks/"+url.PathEscape(task.ID)+"/context", body, &entry); err != nil {
		if queued, ok, queueErr := queueRepoSemanticMemoryOnRetriable(repoPath, completed, why, verification, remaining, files, stage, source, explicitTaskID, err, queueOnRetriable); ok {
			return map[string]any{"status": "queued", "reason": "TaskPilot server unavailable; semantic memory queued locally for daemon retry", "queued_memory": queued, "task": task}, queueErr
		}
		return nil, err
	}
	return map[string]any{"status": "recorded", "summary": "Recorded structured semantic repo memory.", "task": task, "context": entry}, nil
}

func checkpointRepoContextWithQueue(repoPath, source, reason string, queueOnRetriable bool) (map[string]any, error) {
	activity, err := currentRepoActivity(repoPath)
	if err != nil {
		return nil, err
	}
	files := filterProductRepoFiles(activity.ChangedFiles)
	if len(files) == 0 {
		return map[string]any{"status": "noop", "reason": "no product file changes detected", "changed_files": files}, nil
	}
	task, err := ensureTaskForRepoActivity(activity)
	if err != nil {
		if queued, ok, queueErr := queueRepoCheckpointOnRetriable(repoPath, source, reason, err, queueOnRetriable); ok {
			return map[string]any{"status": "queued", "reason": "TaskPilot server unavailable; repo checkpoint queued locally for daemon retry", "queued_checkpoint": queued, "changed_files": files}, queueErr
		}
		return nil, err
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = "manual"
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "manual"
	}
	draft, _ := draftRepoSemanticSummary(activity.Config.GitRoot)
	summary := repoCheckpointSummary(activity, files)
	confidence := "file_checkpoint"
	if strings.TrimSpace(draft.Summary) != "" && draft.Confidence == "metadata_inferred" {
		summary = draft.Summary
		confidence = draft.Confidence
	}
	stage := "working"
	if reason == "session_end" {
		stage = "final"
	}
	memoryKey := repoMemoryKey(activity, task.ID, files)
	var detail TaskDetail
	if err := request("GET", "/api/tasks/"+url.PathEscape(task.ID), nil, &detail); err == nil {
		if repoCheckpointAlreadyRecorded(detail.Context, files, summary, reason) {
			return map[string]any{"status": "noop", "reason": "checkpoint already recorded", "task": task, "changed_files": files}, nil
		}
	}
	var entry ContextEntry
	if err := request("POST", "/api/tasks/"+url.PathEscape(task.ID)+"/context", map[string]any{"kind": "summary", "content": summary, "source": source, "reason": reason, "confidence": confidence, "files": files, "memory_key": memoryKey, "stage": stage}, &entry); err != nil {
		if queued, ok, queueErr := queueRepoCheckpointOnRetriable(repoPath, source, reason, err, queueOnRetriable); ok {
			return map[string]any{"status": "queued", "reason": "TaskPilot server unavailable; repo checkpoint queued locally for daemon retry", "queued_checkpoint": queued, "changed_files": files, "confidence": confidence, "memory_key": memoryKey, "stage": stage}, queueErr
		}
		return nil, err
	}
	return map[string]any{"status": "recorded", "task": task, "context": entry, "changed_files": files, "confidence": confidence, "memory_key": memoryKey, "stage": stage}, nil
}

func queueRepoCheckpointOnRetriable(repoPath, source, reason string, cause error, enabled bool) (queuedRepoCheckpoint, bool, error) {
	if !enabled || !isRetriableRequestError(cause) {
		return queuedRepoCheckpoint{}, false, nil
	}
	queued, err := queueRepoCheckpoint(repoPath, source, reason, cause)
	if err != nil {
		return queuedRepoCheckpoint{}, true, fmt.Errorf("%w; additionally failed to queue repo checkpoint locally: %v", cause, err)
	}
	_, _ = fmt.Fprintf(os.Stderr, "TaskPilot: repo checkpoint queued locally as %s and will retry from the daemon.\n", queued.ID)
	return queued, true, nil
}

func queueRepoSemanticMemoryOnRetriable(repoPath, completed, why, verification, remaining string, files []string, stage, source, taskID string, cause error, enabled bool) (queuedRepoSemanticMemory, bool, error) {
	if !enabled || !isRetriableRequestError(cause) {
		return queuedRepoSemanticMemory{}, false, nil
	}
	queued, err := queueRepoSemanticMemoryForTask(repoPath, taskID, completed, why, verification, remaining, files, stage, source, cause)
	if err != nil {
		return queuedRepoSemanticMemory{}, true, fmt.Errorf("%w; additionally failed to queue semantic memory locally: %v", cause, err)
	}
	_, _ = fmt.Fprintf(os.Stderr, "TaskPilot: semantic memory queued locally as %s and will retry from the daemon.\n", queued.ID)
	return queued, true, nil
}

func queueRepoSemanticMemory(repoPath, completed, why, verification, remaining string, files []string, stage, source string, cause error) (queuedRepoSemanticMemory, error) {
	return queueRepoSemanticMemoryForTask(repoPath, "", completed, why, verification, remaining, files, stage, source, cause)
}

func queueRepoSemanticMemoryForTask(repoPath, taskID, completed, why, verification, remaining string, files []string, stage, source string, cause error) (queuedRepoSemanticMemory, error) {
	cfg, err := loadConfig()
	if err != nil {
		return queuedRepoSemanticMemory{}, err
	}
	if cfg.Server == "" {
		cfg.Server = "http://127.0.0.1:8080"
	}
	root, err := gitRoot(repoPath)
	if err == nil {
		repoPath = root
	}
	stage = strings.TrimSpace(stage)
	if stage == "" {
		stage = "working"
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = "mcp"
	}
	files = filterProductRepoFiles(files)
	taskID = strings.TrimSpace(taskID)
	now := time.Now().UTC()
	sum := sha256.Sum256([]byte(strings.Join(append([]string{cfg.Server, cfg.ActorID, filepath.Clean(repoPath), taskID, completed, why, verification, remaining, stage, source}, files...), "\n")))
	id := fmt.Sprintf("repo_semantic_%x", sum[:8])
	queued := queuedRepoSemanticMemory{
		ID:            id,
		Server:        cfg.Server,
		ActorID:       cfg.ActorID,
		RepoPath:      filepath.Clean(repoPath),
		TaskID:        taskID,
		CompletedWork: completed,
		Why:           why,
		Verification:  verification,
		RemainingWork: remaining,
		Files:         files,
		Source:        source,
		Stage:         stage,
		CreatedAt:     now,
		LastAttemptAt: now,
		Attempts:      1,
		LastError:     cause.Error(),
	}
	paths := queuedRepoSemanticMemoryCandidatePaths(repoPath, id)
	queued, err = writeQueuedRepoSemanticMemoryFirstWritable(paths, queued)
	if err != nil {
		return queuedRepoSemanticMemory{}, err
	}
	return queued, nil
}

func flushQueuedRepoSemanticMemories(repoPaths ...string) (int, int, error) {
	cfg, err := loadConfig()
	if err != nil {
		return 0, 0, err
	}
	if cfg.Server == "" {
		cfg.Server = "http://127.0.0.1:8080"
	}
	paths, err := queuedRepoSemanticMemoryPaths(repoPaths...)
	if err != nil {
		return 0, 0, err
	}
	flushed := 0
	failed := 0
	for _, path := range paths {
		queued, err := readQueuedRepoSemanticMemory(path)
		if err != nil {
			failed++
			continue
		}
		if queued.Server != cfg.Server || queued.ActorID != cfg.ActorID {
			continue
		}
		source := strings.TrimSpace(queued.Source)
		if source == "" {
			source = "mcp"
		}
		_, err = recordRepoSemanticMemoryForTask(queued.RepoPath, queued.TaskID, queued.CompletedWork, queued.Why, queued.Verification, queued.RemainingWork, queued.Files, queued.Stage, source, false)
		if err == nil {
			_ = os.Remove(path)
			flushed++
			continue
		}
		failed++
		queued.Attempts++
		queued.LastAttemptAt = time.Now().UTC()
		queued.LastError = err.Error()
		_ = writeQueuedRepoSemanticMemory(path, queued)
	}
	return flushed, failed, nil
}

func queuedRepoSemanticMemoryPaths(repoPaths ...string) ([]string, error) {
	return queuedJSONPaths(repoSemanticMemoryOutboxDirs(repoPaths...)...)
}

func queuedRepoSemanticMemoryPath(id string) string {
	return filepath.Join(repoSemanticMemoryOutboxDir(), id+".json")
}

func queuedRepoSemanticMemoryCandidatePaths(repoPath, id string) []string {
	paths := []string{}
	for _, dir := range repoSemanticMemoryOutboxDirs(repoPath) {
		paths = append(paths, filepath.Join(dir, id+".json"))
	}
	return uniqueStrings(paths)
}

func readQueuedRepoSemanticMemory(path string) (queuedRepoSemanticMemory, error) {
	var queued queuedRepoSemanticMemory
	data, err := os.ReadFile(path)
	if err != nil {
		return queued, err
	}
	return queued, json.Unmarshal(data, &queued)
}

func writeQueuedRepoSemanticMemory(path string, queued queuedRepoSemanticMemory) error {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(queued, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeQueuedRepoSemanticMemoryFirstWritable(paths []string, queued queuedRepoSemanticMemory) (queuedRepoSemanticMemory, error) {
	var errs []error
	for _, path := range uniqueStrings(paths) {
		next := queued
		if existing, err := readQueuedRepoSemanticMemory(path); err == nil {
			next.CreatedAt = existing.CreatedAt
			next.Attempts = existing.Attempts + 1
		}
		next.QueuePath = path
		if err := writeQueuedRepoSemanticMemory(path, next); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			continue
		}
		return next, nil
	}
	return queuedRepoSemanticMemory{}, errors.Join(errs...)
}

func repoSemanticMemoryOutboxDir() string {
	return filepath.Join(taskpilotHomeDir(), "outbox", "repo-semantic-memory")
}

func repoSemanticMemoryOutboxDirs(repoPaths ...string) []string {
	return repoOutboxDirs("repo-semantic-memory", repoPaths...)
}

func queueRepoCheckpoint(repoPath, source, reason string, cause error) (queuedRepoCheckpoint, error) {
	cfg, err := loadConfig()
	if err != nil {
		return queuedRepoCheckpoint{}, err
	}
	if cfg.Server == "" {
		cfg.Server = "http://127.0.0.1:8080"
	}
	root, err := gitRoot(repoPath)
	if err == nil {
		repoPath = root
	}
	now := time.Now().UTC()
	sum := sha256.Sum256([]byte(strings.Join([]string{cfg.Server, cfg.ActorID, filepath.Clean(repoPath), source, reason}, "\n")))
	id := fmt.Sprintf("repo_checkpoint_%x", sum[:8])
	queued := queuedRepoCheckpoint{
		ID:            id,
		Server:        cfg.Server,
		ActorID:       cfg.ActorID,
		RepoPath:      filepath.Clean(repoPath),
		Source:        source,
		Reason:        reason,
		CreatedAt:     now,
		LastAttemptAt: now,
		Attempts:      1,
		LastError:     cause.Error(),
	}
	paths := queuedRepoCheckpointCandidatePaths(repoPath, id)
	queued, err = writeQueuedRepoCheckpointFirstWritable(paths, queued)
	if err != nil {
		return queuedRepoCheckpoint{}, err
	}
	return queued, nil
}

func flushQueuedRepoCheckpoints(repoPaths ...string) (int, int, error) {
	cfg, err := loadConfig()
	if err != nil {
		return 0, 0, err
	}
	if cfg.Server == "" {
		cfg.Server = "http://127.0.0.1:8080"
	}
	paths, err := queuedRepoCheckpointPaths(repoPaths...)
	if err != nil {
		return 0, 0, err
	}
	flushed := 0
	failed := 0
	for _, path := range paths {
		queued, err := readQueuedRepoCheckpoint(path)
		if err != nil {
			failed++
			continue
		}
		if queued.Server != cfg.Server || queued.ActorID != cfg.ActorID {
			continue
		}
		_, err = checkpointRepoContextWithQueue(queued.RepoPath, queued.Source, queued.Reason, false)
		if err == nil {
			_ = os.Remove(path)
			flushed++
			continue
		}
		failed++
		queued.Attempts++
		queued.LastAttemptAt = time.Now().UTC()
		queued.LastError = err.Error()
		_ = writeQueuedRepoCheckpoint(path, queued)
	}
	return flushed, failed, nil
}

func queuedRepoCheckpointPaths(repoPaths ...string) ([]string, error) {
	return queuedJSONPaths(repoCheckpointOutboxDirs(repoPaths...)...)
}

func queuedRepoCheckpointPath(id string) string {
	return filepath.Join(repoCheckpointOutboxDir(), id+".json")
}

func queuedRepoCheckpointCandidatePaths(repoPath, id string) []string {
	paths := []string{}
	for _, dir := range repoCheckpointOutboxDirs(repoPath) {
		paths = append(paths, filepath.Join(dir, id+".json"))
	}
	return uniqueStrings(paths)
}

func readQueuedRepoCheckpoint(path string) (queuedRepoCheckpoint, error) {
	var queued queuedRepoCheckpoint
	data, err := os.ReadFile(path)
	if err != nil {
		return queued, err
	}
	return queued, json.Unmarshal(data, &queued)
}

func writeQueuedRepoCheckpoint(path string, queued queuedRepoCheckpoint) error {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(queued, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeQueuedRepoCheckpointFirstWritable(paths []string, queued queuedRepoCheckpoint) (queuedRepoCheckpoint, error) {
	var errs []error
	for _, path := range uniqueStrings(paths) {
		next := queued
		if existing, err := readQueuedRepoCheckpoint(path); err == nil {
			next.CreatedAt = existing.CreatedAt
			next.Attempts = existing.Attempts + 1
		}
		next.QueuePath = path
		if err := writeQueuedRepoCheckpoint(path, next); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			continue
		}
		return next, nil
	}
	return queuedRepoCheckpoint{}, errors.Join(errs...)
}

func repoCheckpointOutboxDir() string {
	return filepath.Join(taskpilotHomeDir(), "outbox", "repo-checkpoints")
}

func repoCheckpointOutboxDirs(repoPaths ...string) []string {
	return repoOutboxDirs("repo-checkpoints", repoPaths...)
}

func repoOutboxDirs(kind string, repoPaths ...string) []string {
	dirs := []string{filepath.Join(taskpilotHomeDir(), "outbox", kind)}
	for _, repoPath := range repoPaths {
		root := repoOutboxRoot(repoPath)
		if root == "" {
			continue
		}
		dirs = append(dirs, filepath.Join(root, ".taskpilot", "outbox", kind))
	}
	return uniqueStrings(dirs)
}

func repoOutboxRoot(repoPath string) string {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		return ""
	}
	if root, err := gitRoot(repoPath); err == nil {
		return filepath.Clean(root)
	}
	return filepath.Clean(repoPath)
}

func queuedJSONPaths(dirs ...string) ([]string, error) {
	paths := []string{}
	var errs []error
	for _, dir := range uniqueStrings(dirs) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) || os.IsPermission(err) || errors.Is(err, syscall.ENOTDIR) {
				continue
			}
			errs = append(errs, err)
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return paths, nil
}

type repoSemanticDraft struct {
	Status     string                  `json:"status"`
	Summary    string                  `json:"summary,omitempty"`
	Confidence string                  `json:"confidence,omitempty"`
	Files      []string                `json:"files,omitempty"`
	Stats      map[string]repoDiffStat `json:"stats,omitempty"`
	Headings   map[string][]string     `json:"headings,omitempty"`
}

type repoDiffStat struct {
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Status  string `json:"status,omitempty"`
}

func draftRepoSemanticSummary(repoPath string) (repoSemanticDraft, error) {
	activity, err := currentRepoActivity(repoPath)
	if err != nil {
		return repoSemanticDraft{}, err
	}
	files := filterProductRepoFiles(activity.ChangedFiles)
	if len(files) == 0 {
		return repoSemanticDraft{Status: "noop", Confidence: "file_checkpoint", Files: files}, nil
	}
	stats := repoDiffStats(activity.Config.GitRoot, files)
	headings := markdownHeadings(activity.Config.GitRoot, files)
	summary := metadataSemanticSummary(activity, files, stats, headings)
	confidence := "file_checkpoint"
	if len(headings) > 0 || hasMeaningfulDiffStats(stats) {
		confidence = "metadata_inferred"
	}
	return repoSemanticDraft{Status: "ok", Summary: summary, Confidence: confidence, Files: files, Stats: stats, Headings: headings}, nil
}

func metadataSemanticSummary(activity repoActivity, files []string, stats map[string]repoDiffStat, headings map[string][]string) string {
	action := "Updated"
	allAdded := len(files) > 0
	for _, file := range files {
		status := strings.TrimSpace(stats[file].Status)
		if status != "??" && !strings.Contains(status, "A") {
			allAdded = false
			break
		}
	}
	if allAdded {
		action = "Added"
	}
	if len(files) == 1 {
		file := redactSensitiveText(files[0])
		if hs := headings[files[0]]; len(hs) > 0 {
			return fmt.Sprintf("%s %s with sections covering %s.", action, file, strings.Join(limitStrings(hs, 6), ", "))
		}
		if st := stats[files[0]]; st.Added > 0 || st.Deleted > 0 {
			return fmt.Sprintf("%s %s with %d line(s) added and %d removed.", action, file, st.Added, st.Deleted)
		}
		return fmt.Sprintf("%s %s and recorded the repo work checkpoint.", action, file)
	}
	parts := []string{}
	for _, file := range limitStrings(files, 5) {
		name := redactSensitiveText(file)
		if hs := headings[file]; len(hs) > 0 {
			parts = append(parts, name+" sections: "+strings.Join(limitStrings(hs, 3), ", "))
		} else {
			parts = append(parts, name)
		}
	}
	return fmt.Sprintf("%s %d product files: %s.", action, len(files), strings.Join(parts, "; "))
}

func repoDiffStats(root string, files []string) map[string]repoDiffStat {
	out := map[string]repoDiffStat{}
	states := gitChangedFileSnapshotIn(root)
	for _, file := range files {
		out[file] = repoDiffStat{Status: strings.TrimSpace(states[file].Status)}
	}
	args := append([]string{"-C", root, "diff", "--numstat", "HEAD", "--"}, files...)
	data, err := exec.Command("git", args...).Output()
	if err != nil {
		return out
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		file := filepath.ToSlash(fields[len(fields)-1])
		stat := out[file]
		stat.Added = parseNumstatField(fields[0])
		stat.Deleted = parseNumstatField(fields[1])
		out[file] = stat
	}
	return out
}

func parseNumstatField(value string) int {
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return n
}

func hasMeaningfulDiffStats(stats map[string]repoDiffStat) bool {
	for _, stat := range stats {
		if stat.Added > 0 || stat.Deleted > 0 {
			return true
		}
	}
	return false
}

func markdownHeadings(root string, files []string) map[string][]string {
	out := map[string][]string{}
	for _, file := range files {
		if !strings.EqualFold(filepath.Ext(file), ".md") {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(file))
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		headings := []string{}
		scanner := bufio.NewScanner(bytes.NewReader(data))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "#") {
				continue
			}
			heading := strings.TrimSpace(strings.TrimLeft(line, "#"))
			if heading == "" || strings.Contains(heading, "TASKPILOT:") {
				continue
			}
			headings = append(headings, redactSensitiveText(heading))
			if len(headings) >= 8 {
				break
			}
		}
		if len(headings) > 0 {
			out[file] = uniqueStrings(headings)
		}
	}
	return out
}

func redactSensitiveText(value string) string {
	value = regexp.MustCompile(`(?i)(password|secret|token|api[_-]?key|credential)[^\s,;:]*`).ReplaceAllString(value, "[redacted]")
	value = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`).ReplaceAllString(value, "${1}[redacted]")
	return strings.TrimSpace(value)
}

func repoCheckpointSummary(activity repoActivity, files []string) string {
	action := "Updated"
	states := gitChangedFileSnapshotIn(activity.Config.GitRoot)
	allAdded := len(files) > 0
	for _, file := range files {
		status := strings.TrimSpace(states[file].Status)
		if status != "??" && !strings.Contains(status, "A") {
			allAdded = false
			break
		}
	}
	if allAdded {
		action = "Added"
	}
	if len(files) == 1 {
		return fmt.Sprintf("%s %s and recorded the repo work checkpoint.", action, files[0])
	}
	return fmt.Sprintf("%s %s and recorded the repo work checkpoint.", action, strings.Join(limitStrings(files, 5), ", "))
}

func repoMemoryKey(activity repoActivity, taskID string, files []string) string {
	normalized := normalizedMemoryFiles(files)
	if len(normalized) == 0 {
		normalized = []string{"repo"}
	}
	scope := activity.Config.RepoID
	if scope == "" {
		scope = activity.Config.RemoteURL
	}
	if scope == "" {
		scope = activity.Config.GitRoot
	}
	raw := strings.Join([]string{"repo", scope, taskID, strings.Join(normalized, "\n")}, "\n")
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("repo:%x", sum)
}

func normalizedMemoryFiles(files []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, file := range files {
		file = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(strings.TrimSpace(file))), "./")
		if file == "." || file == "" || isTaskPilotManagedRepoFile(file) {
			continue
		}
		if seen[file] {
			continue
		}
		seen[file] = true
		out = append(out, file)
	}
	sort.Strings(out)
	return out
}

func repoCheckpointAlreadyRecorded(entries []ContextEntry, files []string, summary, reason string) bool {
	for i := len(entries) - 1; i >= 0 && i >= len(entries)-20; i-- {
		entry := entries[i]
		if entry.Stage == "superseded" {
			continue
		}
		if isNoisyRunContext(entry.Content) {
			continue
		}
		if strings.TrimSpace(entry.Content) == summary {
			return true
		}
	}
	return false
}

func semanticMemoryContent(completed, why, verification, remaining string) string {
	parts := []string{}
	if completed = strings.TrimSpace(completed); completed != "" {
		parts = append(parts, "Completed: "+completed)
	}
	if why = strings.TrimSpace(why); why != "" {
		parts = append(parts, "Why: "+why)
	}
	if verification = strings.TrimSpace(verification); verification != "" {
		parts = append(parts, "Verification: "+verification)
	}
	if remaining = strings.TrimSpace(remaining); remaining != "" {
		parts = append(parts, "Remaining: "+remaining)
	}
	return strings.Join(parts, " ")
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
		return fmt.Errorf("usage: taskpilot handoff prepare|checkpoint|sync|accept|reject")
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
	case "checkpoint":
		if len(args) < 2 {
			return fmt.Errorf("usage: taskpilot handoff checkpoint <task-id> --file path [--packet-id id] [--session-id id] [--json]")
		}
		fs := flag.NewFlagSet("handoff checkpoint", flag.ExitOnError)
		filePath := fs.String("file", "", "handoff markdown file")
		packetID := fs.String("packet-id", os.Getenv("TASKPILOT_HANDOFF_PACKET_ID"), "handoff packet id")
		sessionID := fs.String("session-id", os.Getenv("TASKPILOT_SESSION_ID"), "taskpilot run session id")
		jsonOut := fs.Bool("json", false, "print JSON")
		id := args[1]
		_ = fs.Parse(args[2:])
		if *filePath == "" {
			return fmt.Errorf("usage: taskpilot handoff checkpoint <task-id> --file path")
		}
		data, err := os.ReadFile(*filePath)
		if err != nil {
			return err
		}
		checkpoint, queued, err := sendOrQueueHandoffCheckpoint(id, *packetID, *sessionID, string(data))
		if err != nil {
			return err
		}
		if *jsonOut {
			if queued != nil {
				return print(map[string]any{"status": "queued", "queued_checkpoint": queued}, true)
			}
			return print(checkpoint, true)
		}
		if queued != nil {
			fmt.Printf("handoff checkpoint queued for sync: %s\n", queued.ID)
			return nil
		}
		return print(checkpoint, false)
	case "sync":
		fs := flag.NewFlagSet("handoff sync", flag.ExitOnError)
		watch := fs.Bool("watch", false, "keep retrying queued handoff checkpoints until synced or max duration elapses")
		interval := fs.Duration("interval", handoffSyncInterval(), "retry interval for --watch")
		maxDuration := fs.Duration("max-duration", handoffSyncMaxDuration(), "maximum runtime for --watch")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		result, err := runHandoffOutboxSync(context.Background(), handoffSyncOptions{Watch: *watch, Interval: *interval, MaxDuration: *maxDuration})
		if err != nil {
			return err
		}
		if *jsonOut {
			return print(result, true)
		}
		if result.Skipped {
			fmt.Println("handoff checkpoint sync: already running")
			return nil
		}
		fmt.Printf("handoff checkpoint sync: flushed=%d failed=%d\n", result.Flushed, result.Failed)
		return nil
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
	return doRequestWithProxy(method, path, body, out, true, true)
}

func requestNoActor(method, path string, body any, out any) error {
	return doRequestWithProxy(method, path, body, out, false, true)
}

func requestNoProxy(method, path string, body any, out any) error {
	return doRequestWithProxy(method, path, body, out, true, false)
}

func taskRunOwnershipError(taskID string, cfg Config, detail TaskDetail, cause error) error {
	if detail.Task.OwnerID == "" || detail.Task.OwnerID == cfg.ActorID || !strings.Contains(cause.Error(), "actively owned") {
		return cause
	}
	owner := detail.Task.OwnerID
	if detail.Owner != nil {
		owner = fmt.Sprintf("%s (%s, %s)", detail.Owner.Name, detail.Owner.Kind, detail.Owner.ID)
	}
	current := cfg.ActorID
	if current == "" {
		current = "no actor configured"
	}
	return fmt.Errorf(
		"%w\n\nTaskPilot run is configured as actor %s, but task %s is currently owned by %s.\n"+
			"Accept the handoff with the same CLI actor, or intentionally transfer the task first:\n"+
			"  taskpilot task claim %s --force --reason \"continue handoff from CLI agent\"\n"+
			"To inspect local CLI identity, run:\n"+
			"  taskpilot config show",
		cause, current, taskID, owner, taskID,
	)
}

func doRequest(method, path string, body any, out any, includeActor bool) error {
	return doRequestWithProxy(method, path, body, out, includeActor, true)
}

func doRequestWithProxy(method, path string, body any, out any, includeActor bool, allowProxy bool) error {
	var bodyBytes []byte
	if body != nil {
		bodyBytes, _ = json.Marshal(body)
	}
	data, err := doRequestBytes(method, path, bodyBytes, includeActor, allowProxy)
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func doRequestBytes(method, path string, bodyBytes []byte, includeActor bool, allowProxy bool) ([]byte, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	if cfg.Server == "" {
		cfg.Server = "http://127.0.0.1:8080"
	}
	if includeActor {
		if err := ensureActorSessionForConfig(&cfg, false); err != nil {
			return nil, err
		}
	}
	data, err := doRequestBytesWithConfig(cfg, method, path, bodyBytes, includeActor)
	if err != nil {
		if allowProxy && isRetriableRequestError(err) {
			if proxied, proxyErr := doRequestViaDaemonProxy(cfg, method, path, bodyBytes, includeActor); proxyErr == nil {
				return proxied, nil
			}
		}
		return nil, err
	}
	return data, nil
}

func doRequestBytesWithConfig(cfg Config, method, path string, bodyBytes []byte, includeActor bool) ([]byte, error) {
	var reader io.Reader
	if bodyBytes != nil {
		reader = bytes.NewReader(bodyBytes)
	}
	req, err := http.NewRequest(method, strings.TrimRight(cfg.Server, "/")+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if includeActor {
		if cfg.ActorSessionID != "" && cfg.ActorSessionToken != "" {
			if cfg.ActorID != "" {
				req.Header.Set("X-Actor-ID", cfg.ActorID)
			}
			req.Header.Set("X-Actor-Session-ID", cfg.ActorSessionID)
			req.Header.Set("X-Actor-Session-Token", cfg.ActorSessionToken)
		} else {
			if cfg.ActorID != "" {
				req.Header.Set("X-Actor-ID", cfg.ActorID)
			}
			if cfg.ActorSecret != "" {
				req.Header.Set("X-Actor-Secret", cfg.ActorSecret)
			}
		}
	}
	client := &http.Client{Timeout: taskPilotHTTPTimeout()}
	resp, err := client.Do(req)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) || os.IsTimeout(err) || errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "connect: connection refused") || strings.Contains(err.Error(), "operation not permitted") {
			return nil, retriableRequestError{err: fmt.Errorf("cannot reach TaskPilot server at %s; start it with `taskpilot serve --addr 127.0.0.1:8080` and check `taskpilot config set-server`", cfg.Server)}
		}
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		retriable := resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		var ae APIError
		if json.Unmarshal(data, &ae) == nil && ae.Message != "" {
			err := fmt.Errorf("%s: %s", ae.Error, apiErrorMessage(ae))
			if retriable {
				return nil, retriableRequestError{err: err}
			}
			return nil, err
		}
		err := fmt.Errorf("request failed: %s", resp.Status)
		if retriable {
			return nil, retriableRequestError{err: err}
		}
		return nil, err
	}
	return data, nil
}

func ensureActorSessionForConfig(cfg *Config, migrateLegacy bool) error {
	if cfg == nil || cfg.ActorSessionID != "" || cfg.ActorSessionToken != "" {
		return nil
	}
	if strings.TrimSpace(cfg.ActorSecret) == "" {
		return nil
	}
	if !migrateLegacy && strings.TrimSpace(cfg.ActorID) != "" {
		return nil
	}
	activation, err := activateActorSessionWithConfig(*cfg, ActorSessionStartInput{
		ActorID:        cfg.ActorID,
		ActorSecret:    cfg.ActorSecret,
		MachineID:      machineID(),
		TerminalID:     terminalID(),
		AgentProvider:  firstNonEmpty(cfg.AgentProvider, detectAgentProvider(nil)),
		ProcessID:      os.Getpid(),
		CurrentTaskID:  firstNonEmpty(cfg.CurrentTaskID, os.Getenv("TASKPILOT_TASK_ID")),
		ClientVersion:  "taskpilot-cli",
		RepositoryPath: bestEffortGitRoot("."),
	})
	if err != nil {
		return err
	}
	cfg.ActorID = activation.Actor.ID
	cfg.ActorSecret = ""
	cfg.ActorSessionID = activation.Session.ID
	cfg.ActorSessionToken = activation.SessionToken
	cfg.CurrentTaskID = activation.Session.CurrentTaskID
	cfg.AgentProvider = activation.Session.AgentProvider
	return saveTerminalActorSession(terminalActorSession{
		Server:            cfg.Server,
		ActorID:           activation.Actor.ID,
		ActorName:         activation.Actor.Name,
		ActorSessionID:    activation.Session.ID,
		ActorSessionToken: activation.SessionToken,
		CurrentTaskID:     activation.Session.CurrentTaskID,
		AgentProvider:     activation.Session.AgentProvider,
		RepositoryPath:    activation.Session.RepositoryPath,
		MachineID:         activation.Session.MachineID,
		TerminalID:        activation.Session.TerminalID,
		ProcessID:         activation.Session.ProcessID,
		Status:            activation.Session.Status,
		StartedAt:         activation.Session.StartedAt,
	})
}

func activateActorSessionWithConfig(cfg Config, in ActorSessionStartInput) (ActorSessionActivation, error) {
	if cfg.Server == "" {
		cfg.Server = "http://127.0.0.1:8080"
	}
	var out ActorSessionActivation
	bodyBytes, _ := json.Marshal(in)
	data, err := doRequestBytesWithConfig(cfg, "POST", "/api/actor-sessions/activate", bodyBytes, false)
	if err != nil {
		return ActorSessionActivation{}, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return ActorSessionActivation{}, err
	}
	return out, nil
}

func bestEffortGitRoot(path string) string {
	root, err := gitRoot(path)
	if err != nil {
		return ""
	}
	return root
}

func taskPilotHTTPTimeout() time.Duration {
	if value := strings.TrimSpace(os.Getenv("TASKPILOT_HTTP_TIMEOUT")); value != "" {
		if duration, err := time.ParseDuration(value); err == nil && duration > 0 {
			return duration
		}
	}
	return 8 * time.Second
}

func apiErrorMessage(ae APIError) string {
	message := strings.TrimSpace(ae.Message)
	if len(ae.Errors) == 0 {
		return message
	}
	lines := []string{message}
	for _, validationErr := range ae.Errors {
		parts := []string{}
		if validationErr.Section != "" {
			parts = append(parts, validationErr.Section)
		}
		if validationErr.Line > 0 {
			parts = append(parts, fmt.Sprintf("line %d", validationErr.Line))
		}
		prefix := ""
		if len(parts) > 0 {
			prefix = strings.Join(parts, " ") + ": "
		}
		lines = append(lines, "- "+prefix+validationErr.Message)
	}
	return strings.Join(lines, "\n")
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
		fmt.Printf("%s\t%s\t%s\tactive=%t\n", x.ID, x.Email, x.Name, x.Active)
	case []User:
		for _, u := range x {
			fmt.Printf("%s\t%s\t%s\tactive=%t\n", u.ID, u.Email, u.Name, u.Active)
		}
	case TaskDetail:
		fmt.Printf("%s\t%s\t%s\nProject: %s\nRepo: %s\nWorkspace: %s\nParent: %s\nGoal: %s\nOwner: %s\nScope: %s\n", x.Task.ID, x.Task.Status, x.Task.Title, x.Task.ProjectID, x.Task.RepoID, x.Task.WorkspaceID, x.Task.ParentTaskID, x.Task.Goal, x.Task.OwnerID, strings.Join(filterProductRepoFiles(x.Task.Scope), ", "))
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
		gitRefs := usefulGitRefs(x.GitRefs)
		if len(gitRefs) > 0 {
			fmt.Println("\nGit:")
			for _, g := range gitRefs {
				fmt.Printf("- %s branch=%s commit=%s pr=%s files=%s\n", g.ID, g.Branch, g.CommitSHA, g.PRURL, strings.Join(g.ChangedFiles, ","))
			}
		}
		contextEntries := compactContextEntries(x.Context, 80)
		if len(contextEntries) > 0 {
			fmt.Println("\nContext:")
			for _, c := range contextEntries {
				fmt.Printf("- %s: %s\n", c.Kind, c.Content)
			}
		}
		if shouldShowHandoffPacket(x.HandoffPacket) {
			fmt.Printf("\nLatest Handoff Memory (%s v%d, %s):\n%s\n", x.HandoffPacket.Status, x.HandoffPacket.Version, x.HandoffPacket.Source, strings.TrimSpace(x.HandoffPacket.Markdown))
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
	cfg, err := loadConfigFile()
	if err != nil {
		return cfg, err
	}
	return applyConfigEnvOverrides(cfg), nil
}

func loadConfigFile() (Config, error) {
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

func applyConfigEnvOverrides(cfg Config) Config {
	if v := strings.TrimSpace(os.Getenv("TASKPILOT_SERVER")); v != "" {
		cfg.Server = strings.TrimRight(v, "/")
	}
	cfg = applyTerminalActorSession(cfg)
	legacyActorEnv := false
	if v := strings.TrimSpace(os.Getenv("TASKPILOT_ACTOR_ID")); v != "" {
		cfg.ActorID = v
		legacyActorEnv = true
	}
	if v := strings.TrimSpace(os.Getenv("TASKPILOT_ACTOR_SECRET")); v != "" {
		cfg.ActorSecret = v
		legacyActorEnv = true
	}
	if legacyActorEnv {
		cfg.ActorSessionID = ""
		cfg.ActorSessionToken = ""
	}
	if v := strings.TrimSpace(os.Getenv("TASKPILOT_ACTOR_SESSION_ID")); v != "" {
		cfg.ActorSessionID = v
	}
	if v := strings.TrimSpace(os.Getenv("TASKPILOT_ACTOR_SESSION_TOKEN")); v != "" {
		cfg.ActorSessionToken = v
	}
	if v := strings.TrimSpace(os.Getenv("TASKPILOT_TASK_ID")); v != "" && cfg.CurrentTaskID == "" {
		cfg.CurrentTaskID = v
	}
	if v := strings.TrimSpace(os.Getenv("TASKPILOT_AGENT_PROVIDER")); v != "" {
		cfg.AgentProvider = v
	}
	return cfg
}

func applyTerminalActorSession(cfg Config) Config {
	session, ok := loadTerminalActorSession(cfg)
	if !ok {
		return cfg
	}
	if cfg.Server == "" && session.Server != "" {
		cfg.Server = strings.TrimRight(session.Server, "/")
	}
	cfg.ActorID = session.ActorID
	cfg.ActorSecret = ""
	cfg.ActorSessionID = session.ActorSessionID
	cfg.ActorSessionToken = session.ActorSessionToken
	cfg.CurrentTaskID = session.CurrentTaskID
	cfg.AgentProvider = session.AgentProvider
	return cfg
}

func loadTerminalActorSession(cfg Config) (terminalActorSession, bool) {
	path := actorSessionFilePath(cfg)
	b, err := os.ReadFile(path)
	if err != nil {
		return terminalActorSession{}, false
	}
	var session terminalActorSession
	if json.Unmarshal(b, &session) != nil || session.ActorSessionID == "" || session.ActorSessionToken == "" || session.ActorID == "" {
		return terminalActorSession{}, false
	}
	session.SessionFile = path
	return session, true
}

func saveTerminalActorSession(session terminalActorSession) error {
	path := actorSessionFilePath(Config{Server: session.Server})
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	session.SessionFile = path
	if session.TerminalID == "" {
		session.TerminalID = terminalID()
	}
	if session.MachineID == "" {
		session.MachineID = machineID()
	}
	if session.StartedAt.IsZero() {
		session.StartedAt = time.Now().UTC()
	}
	session.LastHeartbeatAt = time.Now().UTC()
	b, _ := json.MarshalIndent(session, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func clearTerminalActorSession(cfg Config) error {
	path := actorSessionFilePath(cfg)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func updateTerminalActorSessionTask(cfg Config, taskID string) error {
	session, ok := loadTerminalActorSession(cfg)
	if !ok {
		return nil
	}
	session.CurrentTaskID = strings.TrimSpace(taskID)
	session.Status = "active"
	return saveTerminalActorSession(session)
}

func actorSessionFilePath(cfg Config) string {
	if v := strings.TrimSpace(os.Getenv("TASKPILOT_ACTOR_SESSION_FILE")); v != "" {
		return v
	}
	server := strings.TrimRight(firstNonEmpty(cfg.Server, os.Getenv("TASKPILOT_SERVER"), "http://127.0.0.1:8080"), "/")
	key := configPath() + "|" + server + "|" + terminalID()
	sum := sha256.Sum256([]byte(key))
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".taskpilot", "actor-sessions", fmt.Sprintf("%x.json", sum[:8]))
}

func terminalID() string {
	for _, key := range []string{"TASKPILOT_TERMINAL_ID", "TERM_SESSION_ID", "WT_SESSION", "TMUX_PANE", "STY", "SSH_TTY"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return key + ":" + v
		}
	}
	for _, path := range []string{"/proc/self/fd/0", "/dev/fd/0"} {
		if target, err := os.Readlink(path); err == nil && strings.TrimSpace(target) != "" {
			return "tty:" + target
		}
	}
	return fmt.Sprintf("ppid:%d", os.Getppid())
}

func machineID() string {
	if host, err := os.Hostname(); err == nil && strings.TrimSpace(host) != "" {
		return host
	}
	return "local-machine"
}

func detectAgentProvider(command []string) string {
	if v := strings.TrimSpace(os.Getenv("TASKPILOT_AGENT_PROVIDER")); v != "" {
		return v
	}
	if len(command) > 0 {
		name := strings.ToLower(filepath.Base(command[0]))
		switch {
		case strings.Contains(name, "codex"):
			return "codex"
		case strings.Contains(name, "claude"):
			return "claude"
		case strings.Contains(name, "gemini"):
			return "gemini"
		}
	}
	return "manual"
}

func loadConfigDiagnostics(effective bool) (configDiagnostics, error) {
	fileCfg, err := loadConfigFile()
	if err != nil {
		return configDiagnostics{}, err
	}
	cfg := applyTerminalActorSession(fileCfg)
	sources := map[string]string{
		"server":       "file",
		"email":        "file",
		"actor_id":     "file",
		"actor_secret": "file",
	}
	sessionSource := ""
	sessionFile := actorSessionFilePath(fileCfg)
	envOverrideActive := false
	if effective {
		if strings.TrimSpace(os.Getenv("TASKPILOT_SERVER")) != "" {
			sources["server"] = "env:TASKPILOT_SERVER"
			envOverrideActive = true
		}
		if strings.TrimSpace(os.Getenv("TASKPILOT_ACTOR_ID")) != "" {
			sources["actor_id"] = "env:TASKPILOT_ACTOR_ID"
			envOverrideActive = true
		}
		if strings.TrimSpace(os.Getenv("TASKPILOT_ACTOR_SECRET")) != "" {
			sources["actor_secret"] = "env:TASKPILOT_ACTOR_SECRET"
			envOverrideActive = true
		}
		if strings.TrimSpace(os.Getenv("TASKPILOT_ACTOR_SESSION_ID")) != "" {
			sources["actor_session_id"] = "env:TASKPILOT_ACTOR_SESSION_ID"
			sessionSource = "environment"
			envOverrideActive = true
		}
		if strings.TrimSpace(os.Getenv("TASKPILOT_ACTOR_SESSION_TOKEN")) != "" {
			sources["actor_session_token"] = "env:TASKPILOT_ACTOR_SESSION_TOKEN"
			sessionSource = "environment"
			envOverrideActive = true
		}
		cfg = applyConfigEnvOverrides(fileCfg)
	}
	auth := "actor_secret"
	if cfg.ActorSessionID != "" && cfg.ActorSessionToken != "" {
		auth = "actor_session"
		if sessionSource == "" {
			sessionSource = "terminal-session-file"
		}
	}
	if cfg.ActorID == "" || cfg.ActorSecret == "" {
		auth = "not_configured"
	}
	if cfg.ActorSessionID != "" && cfg.ActorSessionToken != "" {
		auth = "actor_session"
	}
	if sessionSource == "" {
		if _, ok := loadTerminalActorSession(fileCfg); ok {
			sessionSource = "terminal-session-file"
		}
	}
	return configDiagnostics{
		ConfigPath:            configPath(),
		Server:                cfg.Server,
		Email:                 cfg.Email,
		ActorID:               cfg.ActorID,
		HasSecret:             cfg.ActorSecret != "",
		Auth:                  auth,
		Sources:               sources,
		Effective:             effective,
		EnvOverrideActive:     envOverrideActive,
		DeprecatedGlobalActor: fileCfg.ActorID != "" || fileCfg.ActorSecret != "",
		Global: globalConfigDiagnostics{
			Server:          fileCfg.Server,
			Email:           fileCfg.Email,
			LegacyActorID:   fileCfg.ActorID,
			HasLegacySecret: fileCfg.ActorSecret != "",
		},
		CurrentTerminalSession: sessionConfigDiagnostics{
			Active:        cfg.ActorSessionID != "" && cfg.ActorSessionToken != "",
			Scope:         "current terminal",
			ActorID:       cfg.ActorID,
			SessionID:     cfg.ActorSessionID,
			Provider:      cfg.AgentProvider,
			Status:        firstNonEmpty(sessionStatusFromConfig(cfg), "active"),
			CurrentTaskID: cfg.CurrentTaskID,
			SessionFile:   sessionFile,
			Source:        sessionSource,
			HasToken:      cfg.ActorSessionToken != "",
		},
	}, nil
}

func sessionStatusFromConfig(cfg Config) string {
	session, ok := loadTerminalActorSession(cfg)
	if !ok {
		return ""
	}
	return session.Status
}

func printConfigDiagnostics(d configDiagnostics) {
	fmt.Println("Global configuration")
	fmt.Println("--------------------")
	fmt.Printf("Server: %s\n", firstNonEmpty(d.Global.Server, d.Server, "http://127.0.0.1:8080"))
	if d.Global.Email != "" {
		fmt.Printf("Email: %s\n", d.Global.Email)
	}
	if d.Global.LegacyActorID != "" {
		fmt.Printf("Deprecated global actor: %s\n", d.Global.LegacyActorID)
	}
	fmt.Println()
	fmt.Println("Current terminal session")
	fmt.Println("------------------------")
	s := d.CurrentTerminalSession
	if !s.Active {
		fmt.Println("No actor is active in this terminal.")
		if d.DeprecatedGlobalActor {
			fmt.Println("Deprecated global actor credentials are present and will be migrated on the next authenticated command.")
		}
		fmt.Println()
		fmt.Println("Activate one with:")
		fmt.Println("taskpilot actor activate --secret <actor-secret>")
		return
	}
	fmt.Printf("Actor ID: %s\n", s.ActorID)
	fmt.Printf("Session ID: %s\n", s.SessionID)
	if s.Provider != "" {
		fmt.Printf("Provider: %s\n", s.Provider)
	}
	fmt.Printf("Status: %s\n", firstNonEmpty(s.Status, "active"))
	if s.CurrentTaskID != "" {
		fmt.Printf("Current task: %s\n", s.CurrentTaskID)
	}
	fmt.Println("Scope: current terminal")
}

func saveConfig(cfg Config) error {
	if err := ensureDir(filepath.Dir(configPath())); err != nil {
		return err
	}
	cfg.ActorSessionID = ""
	cfg.ActorSessionToken = ""
	cfg.CurrentTaskID = ""
	cfg.AgentProvider = ""
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
