package symphony

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
)

const (
	DefaultPrompt = "You are working on an issue from GitLab."
)

var (
	ErrMissingWorkflowFile       = errors.New("missing_workflow_file")
	ErrWorkflowParse             = errors.New("workflow_parse_error")
	ErrWorkflowFrontMatterNotMap = errors.New("workflow_front_matter_not_a_map")
	ErrTemplateParse             = errors.New("template_parse_error")
	ErrTemplateRender            = errors.New("template_render_error")
	ErrUnsupportedTrackerKind    = errors.New("unsupported_tracker_kind")
	ErrMissingTrackerAPIKey      = errors.New("missing_tracker_api_key")
	ErrMissingTrackerProjectID   = errors.New("missing_tracker_project_id")
	ErrMissingOpenCodeCommand    = errors.New("missing_opencode_command")
	ErrInvalidWorkspaceCWD       = errors.New("invalid_workspace_cwd")
)

type Issue struct {
	ID          string       `json:"id"`
	Identifier  string       `json:"identifier"`
	Title       string       `json:"title"`
	Description *string      `json:"description,omitempty"`
	Priority    *int         `json:"priority,omitempty"`
	State       string       `json:"state"`
	BranchName  *string      `json:"branch_name,omitempty"`
	URL         *string      `json:"url,omitempty"`
	Labels      []string     `json:"labels"`
	BlockedBy   []BlockerRef `json:"blocked_by"`
	CreatedAt   *time.Time   `json:"created_at,omitempty"`
	UpdatedAt   *time.Time   `json:"updated_at,omitempty"`
}

type BlockerRef struct {
	ID         *string `json:"id,omitempty"`
	Identifier *string `json:"identifier,omitempty"`
	State      *string `json:"state,omitempty"`
}

type WorkflowDefinition struct {
	Config         map[string]any
	PromptTemplate string
	Path           string
	ModTime        time.Time
}

type RuntimeConfig struct {
	WorkflowPath string
	WorkflowDir  string
	Tracker      TrackerConfig
	Polling      PollingConfig
	Workspace    WorkspaceConfig
	Hooks        HooksConfig
	Agent        AgentConfig
	OpenCode     OpenCodeConfig
	Server       ServerConfig
}

type TrackerConfig struct {
	Kind             string
	Endpoint         string
	APIKey           string
	ProjectID        string
	Assignee         string
	StateLabelPrefix string
	RequiredLabels   []string
	ActiveStates     []string
	TerminalStates   []string
}

type PollingConfig struct {
	Interval time.Duration
}

type WorkspaceConfig struct {
	Root string
}

type HooksConfig struct {
	AfterCreate  string
	BeforeRun    string
	AfterRun     string
	BeforeRemove string
	Timeout      time.Duration
}

type AgentConfig struct {
	MaxConcurrentAgents        int
	MaxTurns                   int
	MaxRetryBackoff            time.Duration
	MaxConcurrentAgentsByState map[string]int
}

type OpenCodeConfig struct {
	Command      string
	Host         string
	Port         int
	BaseURL      string
	Config       map[string]any
	Agent        string
	Model        any
	Permission   any
	AuthUsername string
	AuthPassword string
	TurnTimeout  time.Duration
	ReadTimeout  time.Duration
	StallTimeout time.Duration
}

type ServerConfig struct {
	Port *int
	Host string
}

type Tracker interface {
	FetchCandidateIssues(context.Context) ([]Issue, error)
	FetchIssuesByStates(context.Context, []string) ([]Issue, error)
	FetchIssueStatesByIDs(context.Context, []string) ([]Issue, error)
}

type AgentRunner interface {
	Run(context.Context, Issue, *int, func(AgentEvent)) error
}

type AgentEvent struct {
	Event                string         `json:"event"`
	Timestamp            time.Time      `json:"timestamp"`
	SessionID            string         `json:"session_id,omitempty"`
	MessageID            string         `json:"message_id,omitempty"`
	OpenCodeServerPID    string         `json:"opencode_server_pid,omitempty"`
	Message              string         `json:"message,omitempty"`
	Usage                map[string]any `json:"usage,omitempty"`
	RateLimits           map[string]any `json:"rate_limits,omitempty"`
	Raw                  map[string]any `json:"raw,omitempty"`
	LastInputTokens      int64          `json:"last_input_tokens,omitempty"`
	LastOutputTokens     int64          `json:"last_output_tokens,omitempty"`
	LastTotalTokens      int64          `json:"last_total_tokens,omitempty"`
	AbsoluteInputTokens  *int64         `json:"absolute_input_tokens,omitempty"`
	AbsoluteOutputTokens *int64         `json:"absolute_output_tokens,omitempty"`
	AbsoluteTotalTokens  *int64         `json:"absolute_total_tokens,omitempty"`
}

func (e AgentEvent) MarshalLogValue() slog.Value {
	fields := []slog.Attr{
		slog.String("event", e.Event),
		slog.Time("timestamp", e.Timestamp),
	}
	if e.SessionID != "" {
		fields = append(fields, slog.String("session_id", e.SessionID))
	}
	if e.MessageID != "" {
		fields = append(fields, slog.String("message_id", e.MessageID))
	}
	if e.Message != "" {
		fields = append(fields, slog.String("message", e.Message))
	}
	return slog.GroupValue(fields...)
}

func cloneIssue(issue Issue) Issue {
	c := issue
	c.Labels = append([]string(nil), issue.Labels...)
	c.BlockedBy = append([]BlockerRef(nil), issue.BlockedBy...)
	return c
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		return cp
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		return cp
	}
	return out
}
