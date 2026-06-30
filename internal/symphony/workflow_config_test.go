package symphony

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadWorkflowAndResolveConfig(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	body := `---
tracker:
  kind: gitlab
  endpoint: https://gitlab.example.com/api/v4/
  api_key: $GITLAB_TOKEN
  project_id: group%2Fproject
  required_labels: [Ready, " needs-review "]
polling:
  interval_ms: 1234
workspace:
  root: ./workspaces
hooks:
  timeout_ms: 250
agent:
  max_concurrent_agents: 3
  max_turns: 2
  max_retry_backoff_ms: 42000
  max_concurrent_agents_by_state:
    Todo: 1
    bad: 0
opencode:
  base_url: http://127.0.0.1:4099
  auth_password: $OPENCODE_PASSWORD
---
Hello {{ issue.identifier }}`
	if err := os.WriteFile(workflowPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	def, err := LoadWorkflow(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ResolveConfig(def, func(key string) string {
		switch key {
		case "GITLAB_TOKEN":
			return "secret-token"
		case "OPENCODE_PASSWORD":
			return "server-password"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Tracker.APIKey != "secret-token" {
		t.Fatalf("api key was not resolved from env")
	}
	if cfg.Tracker.Endpoint != "https://gitlab.example.com/api/v4" {
		t.Fatalf("endpoint trailing slash not normalized: %q", cfg.Tracker.Endpoint)
	}
	if cfg.Workspace.Root != filepath.Join(dir, "workspaces") {
		t.Fatalf("workspace root = %q", cfg.Workspace.Root)
	}
	if cfg.Polling.Interval != 1234*time.Millisecond {
		t.Fatalf("poll interval = %s", cfg.Polling.Interval)
	}
	if cfg.Agent.MaxConcurrentAgentsByState["todo"] != 1 {
		t.Fatalf("per-state concurrency not normalized: %#v", cfg.Agent.MaxConcurrentAgentsByState)
	}
	if _, ok := cfg.Agent.MaxConcurrentAgentsByState["bad"]; ok {
		t.Fatalf("invalid per-state concurrency entry was retained")
	}
	if cfg.OpenCode.AuthPassword != "server-password" {
		t.Fatalf("opencode password was not resolved")
	}
	if err := ValidateDispatchConfig(cfg); err != nil {
		t.Fatalf("valid dispatch config rejected: %v", err)
	}
}

func TestLoadWorkflowFrontMatterNonMap(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("---\n- nope\n---\nbody"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWorkflow(workflowPath)
	if !errors.Is(err, ErrWorkflowFrontMatterNotMap) {
		t.Fatalf("expected front matter map error, got %v", err)
	}
}

func TestWorkflowStoreInvalidReloadKeepsLastGoodConfig(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	valid := `---
tracker:
  kind: gitlab
  api_key: $GITLAB_TOKEN
  project_id: "123"
opencode:
  base_url: http://127.0.0.1:4099
---
ok`
	if err := os.WriteFile(workflowPath, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewWorkflowStore(workflowPath, func(key string) string {
		if key == "GITLAB_TOKEN" {
			return "token"
		}
		return ""
	})
	if err := store.LoadInitial(); err != nil {
		t.Fatal(err)
	}
	before := store.Config()
	if err := os.WriteFile(workflowPath, []byte("---\ntracker: [bad\n---\nbroken"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := store.Reload()
	if err == nil || changed {
		t.Fatalf("expected failed reload, changed=%v err=%v", changed, err)
	}
	after := store.Config()
	if after.Tracker.ProjectID != before.Tracker.ProjectID || after.OpenCode.BaseURL != before.OpenCode.BaseURL {
		t.Fatalf("last known good config was not preserved: before=%#v after=%#v", before, after)
	}
	if store.LastError() == nil {
		t.Fatalf("reload error was not stored")
	}
}
