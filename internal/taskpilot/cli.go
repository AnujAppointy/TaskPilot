package taskpilot

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Server      string `json:"server"`
	Token       string `json:"token"`
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
		_ = fs.Parse(args[1:])
		return ListenAndServe(*addr, *db, *token)
	case "login":
		return runLogin(args[1:])
	case "config":
		return runConfig(args[1:])
	case "actor":
		return runActor(args[1:])
	case "task":
		return runTask(args[1:])
	case "context":
		return runContext(args[1:])
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
  taskpilot config set-server http://127.0.0.1:8080
  taskpilot config set-token dev-token
  taskpilot config set-actor actor_... <actor-secret>

Agent CLI:
  taskpilot actor register --name "Codex on Anuj Mac" --kind agent --machine anuj-mac
  taskpilot task create --title "Fix signup bug" --goal "Resolve invited-user signup failure" --scope "src/auth/*"
  taskpilot task list
  taskpilot task show <task-id>
  taskpilot task claim <task-id>
  taskpilot lock acquire <task-id> --scope "src/auth/*"
  taskpilot context append <task-id> --kind decision --content "Keep token format unchanged"
  taskpilot handoff prepare <task-id> --summary "Ready for next agent" --next "Write test" --next "Patch logic"
`)
}

func runLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	server := fs.String("server", "http://127.0.0.1:8080", "server URL")
	token := fs.String("token", "dev-token", "team token")
	_ = fs.Parse(args)
	cfg, _ := loadConfig()
	cfg.Server = strings.TrimRight(*server, "/")
	cfg.Token = *token
	return saveConfig(cfg)
}

func runConfig(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: taskpilot config set-server|set-token <value> OR taskpilot config set-actor <actor-id> <actor-secret>")
	}
	cfg, _ := loadConfig()
	switch args[0] {
	case "set-server":
		cfg.Server = strings.TrimRight(args[1], "/")
	case "set-token":
		cfg.Token = args[1]
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
		return fmt.Errorf("usage: taskpilot task create|list|show|claim|release|heartbeat|status|complete")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("task create", flag.ExitOnError)
		title := fs.String("title", "", "title")
		goal := fs.String("goal", "", "goal")
		typ := fs.String("type", "implementation", "task type")
		priority := fs.String("priority", "normal", "priority")
		scope := fs.String("scope", "", "comma-separated scope")
		jsonOut := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args[1:])
		var out Task
		body := TaskInput{Title: *title, Goal: *goal, Type: *typ, Priority: *priority, Scope: splitCSV(*scope)}
		if err := request("POST", "/api/tasks", body, &out); err != nil {
			return err
		}
		return print(out, *jsonOut)
	case "list":
		var out []Task
		if err := request("GET", "/api/tasks", nil, &out); err != nil {
			return err
		}
		return print(out, has(args, "--json"))
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
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
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
		fmt.Printf("%s\t%s\t%s\towner=%s\n", x.ID, x.Status, x.Title, x.OwnerID)
	case []Task:
		for _, t := range x {
			fmt.Printf("%s\t%s\t%s\towner=%s\tlocks=%d\n", t.ID, t.Status, t.Title, t.OwnerID, t.ActiveLockCount)
		}
	case Actor:
		fmt.Printf("%s\t%s\t%s\n", x.ID, x.Kind, x.Name)
	case []Actor:
		for _, a := range x {
			fmt.Printf("%s\t%s\t%s\t%s\n", a.ID, a.Kind, a.Name, a.MachineName)
		}
	case TaskDetail:
		fmt.Printf("%s\t%s\t%s\nGoal: %s\nOwner: %s\nScope: %s\n", x.Task.ID, x.Task.Status, x.Task.Title, x.Task.Goal, x.Task.OwnerID, strings.Join(x.Task.Scope, ", "))
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
