package symphony

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func ResolveConfig(def WorkflowDefinition, env func(string) string) (RuntimeConfig, error) {
	if env == nil {
		env = os.Getenv
	}
	workflowPath := def.Path
	if workflowPath == "" {
		workflowPath = SelectWorkflowPath("")
	}
	absWorkflow, err := filepath.Abs(workflowPath)
	if err == nil {
		workflowPath = absWorkflow
	}
	workflowDir := filepath.Dir(workflowPath)
	raw := def.Config
	if raw == nil {
		raw = map[string]any{}
	}

	trackerRaw := mapValue(raw, "tracker")
	pollingRaw := mapValue(raw, "polling")
	workspaceRaw := mapValue(raw, "workspace")
	hooksRaw := mapValue(raw, "hooks")
	agentRaw := mapValue(raw, "agent")
	opencodeRaw := mapValue(raw, "opencode")
	serverRaw := mapValue(raw, "server")

	tracker := TrackerConfig{
		Kind:             stringValue(trackerRaw, "kind", ""),
		Endpoint:         stringValue(trackerRaw, "endpoint", ""),
		APIKey:           envBackedString(stringValue(trackerRaw, "api_key", ""), env),
		ProjectID:        scalarString(trackerRaw["project_id"]),
		Assignee:         stringValue(trackerRaw, "assignee", ""),
		StateLabelPrefix: stringValue(trackerRaw, "state_label_prefix", "Status::"),
		RequiredLabels:   stringSlice(trackerRaw["required_labels"], nil),
		ActiveStates:     stringSlice(trackerRaw["active_states"], []string{"Todo", "In Progress"}),
		TerminalStates:   stringSlice(trackerRaw["terminal_states"], []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"}),
	}
	if tracker.Kind == "gitlab" && tracker.Endpoint == "" {
		tracker.Endpoint = "https://gitlab.com/api/v4"
	}
	tracker.Endpoint = strings.TrimRight(tracker.Endpoint, "/")
	if tracker.Kind == "gitlab" && tracker.APIKey == "" {
		tracker.APIKey = env("GITLAB_TOKEN")
	}
	tracker.RequiredLabels = normalizeConfiguredLabels(tracker.RequiredLabels)

	root := stringValue(workspaceRaw, "root", filepath.Join(os.TempDir(), "symphony_workspaces"))
	root = envBackedString(root, env)
	root, err = expandPath(root, workflowDir)
	if err != nil {
		return RuntimeConfig{}, err
	}

	maxByState := map[string]int{}
	for k, v := range mapValue(agentRaw, "max_concurrent_agents_by_state") {
		n, ok := intValueAny(v)
		if !ok || n <= 0 {
			continue
		}
		maxByState[normalizeState(k)] = n
	}

	opencodeConfig := mapValue(opencodeRaw, "config")
	if opencodeConfig == nil {
		opencodeConfig = nil
	}
	permission := opencodeRaw["permission"]
	if len(opencodeConfig) > 0 {
		if _, ok := opencodeConfig["permission"]; ok {
			permission = nil
		}
	}

	serverPort := optionalInt(serverRaw["port"])

	cfg := RuntimeConfig{
		WorkflowPath: workflowPath,
		WorkflowDir:  workflowDir,
		Tracker:      tracker,
		Polling: PollingConfig{
			Interval: durationMS(intValue(pollingRaw, "interval_ms", 30000)),
		},
		Workspace: WorkspaceConfig{Root: root},
		Hooks: HooksConfig{
			AfterCreate:  stringValue(hooksRaw, "after_create", ""),
			BeforeRun:    stringValue(hooksRaw, "before_run", ""),
			AfterRun:     stringValue(hooksRaw, "after_run", ""),
			BeforeRemove: stringValue(hooksRaw, "before_remove", ""),
			Timeout:      durationMS(intValue(hooksRaw, "timeout_ms", 60000)),
		},
		Agent: AgentConfig{
			MaxConcurrentAgents:        intValue(agentRaw, "max_concurrent_agents", 10),
			MaxTurns:                   intValue(agentRaw, "max_turns", 20),
			MaxRetryBackoff:            durationMS(intValue(agentRaw, "max_retry_backoff_ms", 300000)),
			MaxConcurrentAgentsByState: maxByState,
		},
		OpenCode: OpenCodeConfig{
			Command:      stringValue(opencodeRaw, "command", `opencode serve --hostname "$OPENCODE_HOST" --port "$OPENCODE_PORT"`),
			Host:         stringValue(opencodeRaw, "host", "127.0.0.1"),
			Port:         intValue(opencodeRaw, "port", 0),
			BaseURL:      strings.TrimRight(stringValue(opencodeRaw, "base_url", ""), "/"),
			Config:       opencodeConfig,
			Agent:        stringValue(opencodeRaw, "agent", ""),
			Model:        opencodeRaw["model"],
			Permission:   permission,
			AuthUsername: envBackedString(stringValue(opencodeRaw, "auth_username", ""), env),
			AuthPassword: envBackedString(stringValue(opencodeRaw, "auth_password", ""), env),
			TurnTimeout:  durationMS(intValue(opencodeRaw, "turn_timeout_ms", 3600000)),
			ReadTimeout:  durationMS(intValue(opencodeRaw, "read_timeout_ms", 5000)),
			StallTimeout: durationMS(intValue(opencodeRaw, "stall_timeout_ms", 300000)),
		},
		Server: ServerConfig{
			Port: serverPort,
			Host: stringValue(serverRaw, "host", "127.0.0.1"),
		},
	}

	return cfg, validateStaticConfig(cfg)
}

func ValidateDispatchConfig(cfg RuntimeConfig) error {
	if err := validateStaticConfig(cfg); err != nil {
		return err
	}
	if cfg.Tracker.Kind == "" || cfg.Tracker.Kind != "gitlab" {
		return fmt.Errorf("%w: %s", ErrUnsupportedTrackerKind, cfg.Tracker.Kind)
	}
	if cfg.Tracker.APIKey == "" {
		return ErrMissingTrackerAPIKey
	}
	if cfg.Tracker.ProjectID == "" {
		return ErrMissingTrackerProjectID
	}
	if cfg.OpenCode.BaseURL == "" && strings.TrimSpace(cfg.OpenCode.Command) == "" {
		return ErrMissingOpenCodeCommand
	}
	return nil
}

func validateStaticConfig(cfg RuntimeConfig) error {
	if cfg.Polling.Interval <= 0 {
		return fmt.Errorf("polling.interval_ms must be positive")
	}
	if cfg.Hooks.Timeout <= 0 {
		return fmt.Errorf("hooks.timeout_ms must be positive")
	}
	if cfg.Agent.MaxConcurrentAgents <= 0 {
		return fmt.Errorf("agent.max_concurrent_agents must be positive")
	}
	if cfg.Agent.MaxTurns <= 0 {
		return fmt.Errorf("agent.max_turns must be positive")
	}
	if cfg.Agent.MaxRetryBackoff <= 0 {
		return fmt.Errorf("agent.max_retry_backoff_ms must be positive")
	}
	if cfg.OpenCode.ReadTimeout <= 0 {
		return fmt.Errorf("opencode.read_timeout_ms must be positive")
	}
	if cfg.OpenCode.TurnTimeout <= 0 {
		return fmt.Errorf("opencode.turn_timeout_ms must be positive")
	}
	if cfg.OpenCode.Host == "" && cfg.OpenCode.BaseURL == "" {
		return fmt.Errorf("opencode.host must be present when opencode.base_url is unset")
	}
	return nil
}

func mapValue(m map[string]any, key string) map[string]any {
	v, ok := m[key]
	if !ok || v == nil {
		return map[string]any{}
	}
	switch x := v.(type) {
	case map[string]any:
		return x
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[fmt.Sprint(k)] = v
		}
		return out
	default:
		return map[string]any{}
	}
}

func stringValue(m map[string]any, key, fallback string) string {
	if v, ok := m[key]; ok {
		s := scalarString(v)
		if s != "" {
			return s
		}
	}
	return fallback
}

func scalarString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

func intValue(m map[string]any, key string, fallback int) int {
	if v, ok := m[key]; ok {
		if n, ok := intValueAny(v); ok {
			return n
		}
	}
	return fallback
}

func intValueAny(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case int32:
		return int(x), true
	case float64:
		if x == float64(int(x)) {
			return int(x), true
		}
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		if err == nil {
			return n, true
		}
	}
	return 0, false
}

func optionalInt(v any) *int {
	if n, ok := intValueAny(v); ok {
		return &n
	}
	return nil
}

func durationMS(ms int) time.Duration {
	return time.Duration(ms) * time.Millisecond
}

func envBackedString(value string, env func(string) string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "$") && len(value) > 1 && isEnvName(value[1:]) {
		return strings.TrimSpace(env(value[1:]))
	}
	return value
}

func isEnvName(s string) bool {
	for i, r := range s {
		if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return s != ""
}

func expandPath(value, baseDir string) (string, error) {
	if value == "" {
		value = filepath.Join(os.TempDir(), "symphony_workspaces")
	}
	if strings.HasPrefix(value, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if value == "~" {
			value = home
		} else if strings.HasPrefix(value, "~/") {
			value = filepath.Join(home, value[2:])
		}
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(baseDir, value)
	}
	return filepath.Abs(value)
}

func stringSlice(v any, fallback []string) []string {
	if v == nil {
		return append([]string(nil), fallback...)
	}
	switch x := v.(type) {
	case []string:
		return append([]string(nil), x...)
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			out = append(out, scalarString(item))
		}
		return out
	default:
		s := scalarString(x)
		if s == "" {
			return append([]string(nil), fallback...)
		}
		return []string{s}
	}
}

func normalizeConfiguredLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.ToLower(strings.TrimSpace(label))
		if label == "" {
			out = append(out, "")
			continue
		}
		out = append(out, label)
	}
	return out
}

func normalizeState(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func containsState(states []string, state string) bool {
	state = normalizeState(state)
	for _, candidate := range states {
		if normalizeState(candidate) == state {
			return true
		}
	}
	return false
}
