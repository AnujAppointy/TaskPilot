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

type Task struct {
	ID                     string     `json:"id"`
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
}

type ContextEntry struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	AuthorID  string    `json:"author_id"`
	Kind      string    `json:"kind"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
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
	Task     Task           `json:"task"`
	Owner    *Actor         `json:"owner,omitempty"`
	Context  []ContextEntry `json:"context"`
	Locks    []Lock         `json:"locks"`
	Handoffs []Handoff      `json:"handoffs"`
	Events   []Event        `json:"events"`
}

type APIError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}
