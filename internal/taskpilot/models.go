package taskpilot

import "time"

const (
	DefaultClaimTTL = 15 * time.Minute
	DefaultLockTTL  = 30 * time.Minute
)

type Actor struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Kind        string     `json:"kind"`
	MachineName string     `json:"machine_name,omitempty"`
	Secret      string     `json:"actor_secret,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	LastSeenAt  *time.Time `json:"last_seen_at,omitempty"`
}

type User struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	Name       string     `json:"name"`
	Role       string     `json:"role"`
	Active     bool       `json:"active"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
}

type APIKey struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	ActorID   string     `json:"actor_id"`
	Role      string     `json:"role"`
	Scopes    []string   `json:"scopes"`
	Prefix    string     `json:"prefix"`
	Secret    string     `json:"api_key,omitempty"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

type Principal struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Role    string   `json:"role"`
	ActorID string   `json:"actor_id,omitempty"`
	UserID  string   `json:"user_id,omitempty"`
	Scopes  []string `json:"scopes,omitempty"`
}

type Project struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

type Repository struct {
	ID            string    `json:"id"`
	ProjectID     string    `json:"project_id"`
	Name          string    `json:"name"`
	Path          string    `json:"path,omitempty"`
	DefaultBranch string    `json:"default_branch,omitempty"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
}

type Workspace struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"project_id"`
	ActorID     string     `json:"actor_id,omitempty"`
	Name        string     `json:"name"`
	MachineName string     `json:"machine_name,omitempty"`
	Kind        string     `json:"kind"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	LastSeenAt  *time.Time `json:"last_seen_at,omitempty"`
}

type Task struct {
	ID                     string     `json:"id"`
	ProjectID              string     `json:"project_id"`
	RepoID                 string     `json:"repo_id,omitempty"`
	WorkspaceID            string     `json:"workspace_id,omitempty"`
	ParentTaskID           string     `json:"parent_task_id,omitempty"`
	Title                  string     `json:"title"`
	Goal                   string     `json:"goal"`
	Type                   string     `json:"type"`
	Status                 string     `json:"status"`
	Priority               string     `json:"priority"`
	OwnerID                string     `json:"owner_id,omitempty"`
	CreatedBy              string     `json:"created_by"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
	ClaimExpiresAt         *time.Time `json:"claim_expires_at,omitempty"`
	LastHeartbeatAt        *time.Time `json:"last_heartbeat_at,omitempty"`
	PrivacyLevel           string     `json:"privacy_level"`
	Scope                  []string   `json:"scope"`
	Requirements           []string   `json:"requirements"`
	CompletionCriteria     []string   `json:"completion_criteria"`
	Risks                  []string   `json:"risks"`
	Blockers               []string   `json:"blockers"`
	ActiveLockCount        int        `json:"active_lock_count,omitempty"`
	LatestHandoffStatus    string     `json:"latest_handoff_status,omitempty"`
	PotentialConflictCount int        `json:"potential_conflict_count,omitempty"`
	SubtaskCount           int        `json:"subtask_count,omitempty"`
	OpenDependencyCount    int        `json:"open_dependency_count,omitempty"`
	BlockedByCount         int        `json:"blocked_by_count,omitempty"`
	SearchText             string     `json:"search_text,omitempty"`
}

type TaskDependency struct {
	ID            string    `json:"id"`
	TaskID        string    `json:"task_id"`
	DependsOnID   string    `json:"depends_on_id"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
	Task          *Task     `json:"task,omitempty"`
	DependsOnTask *Task     `json:"depends_on_task,omitempty"`
}

type ContextEntry struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	AuthorID  string    `json:"author_id"`
	Kind      string    `json:"kind"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type DecisionRecord struct {
	ID           string    `json:"id"`
	TaskID       string    `json:"task_id"`
	AuthorID     string    `json:"author_id"`
	Decision     string    `json:"decision"`
	Alternatives []string  `json:"alternatives"`
	Reason       string    `json:"reason"`
	Impact       string    `json:"impact"`
	CreatedAt    time.Time `json:"created_at"`
}

type Comment struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	AuthorID  string    `json:"author_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

type Artifact struct {
	ID          string         `json:"id"`
	TaskID      string         `json:"task_id"`
	AuthorID    string         `json:"author_id"`
	Kind        string         `json:"kind"`
	Title       string         `json:"title"`
	URI         string         `json:"uri"`
	Description string         `json:"description,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}

type GitRef struct {
	ID           string    `json:"id"`
	TaskID       string    `json:"task_id"`
	AuthorID     string    `json:"author_id"`
	Branch       string    `json:"branch,omitempty"`
	CommitSHA    string    `json:"commit_sha,omitempty"`
	PRURL        string    `json:"pr_url,omitempty"`
	ChangedFiles []string  `json:"changed_files,omitempty"`
	Note         string    `json:"note,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

type Lock struct {
	ID         string     `json:"id"`
	TaskID     string     `json:"task_id"`
	OwnerID    string     `json:"owner_id"`
	Scope      string     `json:"scope"`
	ScopeType  string     `json:"scope_type"`
	ExpiresAt  time.Time  `json:"expires_at"`
	CreatedAt  time.Time  `json:"created_at"`
	ReleasedAt *time.Time `json:"released_at,omitempty"`
}

type Conflict struct {
	ID                string     `json:"id"`
	TaskID            string     `json:"task_id,omitempty"`
	ActorID           string     `json:"actor_id,omitempty"`
	ConflictType      string     `json:"conflict_type"`
	Status            string     `json:"status"`
	Scope             string     `json:"scope,omitempty"`
	ScopeType         string     `json:"scope_type,omitempty"`
	CurrentOwnerID    string     `json:"current_owner_id,omitempty"`
	OtherActorID      string     `json:"other_actor_id,omitempty"`
	OtherTaskID       string     `json:"other_task_id,omitempty"`
	LockID            string     `json:"lock_id,omitempty"`
	ConflictingLockID string     `json:"conflicting_lock_id,omitempty"`
	Resolution        string     `json:"resolution,omitempty"`
	ResolutionNote    string     `json:"resolution_note,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	ResolvedAt        *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy        string     `json:"resolved_by,omitempty"`
	Task              *Task      `json:"task,omitempty"`
	OtherTask         *Task      `json:"other_task,omitempty"`
}

type Handoff struct {
	ID            string     `json:"id"`
	TaskID        string     `json:"task_id"`
	FromActorID   string     `json:"from_actor_id"`
	ToActorID     string     `json:"to_actor_id,omitempty"`
	Status        string     `json:"status"`
	ResumeSummary string     `json:"resume_summary"`
	NextSteps     []string   `json:"next_steps"`
	CreatedAt     time.Time  `json:"created_at"`
	AcceptedAt    *time.Time `json:"accepted_at,omitempty"`
}

type Event struct {
	ID        int64     `json:"id"`
	TaskID    string    `json:"task_id,omitempty"`
	ActorID   string    `json:"actor_id"`
	EventType string    `json:"event_type"`
	Payload   any       `json:"payload"`
	CreatedAt time.Time `json:"created_at"`
}

type TaskDetail struct {
	Task         Task             `json:"task"`
	Owner        *Actor           `json:"owner,omitempty"`
	Parent       *Task            `json:"parent,omitempty"`
	Subtasks     []Task           `json:"subtasks"`
	Dependencies []TaskDependency `json:"dependencies"`
	Dependents   []TaskDependency `json:"dependents"`
	Context      []ContextEntry   `json:"context"`
	Decisions    []DecisionRecord `json:"decisions"`
	Comments     []Comment        `json:"comments"`
	Artifacts    []Artifact       `json:"artifacts"`
	GitRefs      []GitRef         `json:"git_refs"`
	Locks        []Lock           `json:"locks"`
	Handoffs     []Handoff        `json:"handoffs"`
	Events       []Event          `json:"events"`
}

type APIError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}
