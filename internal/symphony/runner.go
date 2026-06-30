package symphony

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

type DefaultAgentRunner struct {
	Config    RuntimeConfig
	Workflow  WorkflowDefinition
	Tracker   Tracker
	Workspace WorkspaceManager
	OpenCode  *OpenCodeClient
	Logger    *slog.Logger
}

func NewDefaultAgentRunner(cfg RuntimeConfig, workflow WorkflowDefinition, tracker Tracker, logger *slog.Logger) *DefaultAgentRunner {
	wm := WorkspaceManager{Config: cfg, Logger: logger}
	return &DefaultAgentRunner{
		Config:    cfg,
		Workflow:  workflow,
		Tracker:   tracker,
		Workspace: wm,
		OpenCode:  &OpenCodeClient{Config: cfg, Logger: logger},
		Logger:    logger,
	}
}

func (r *DefaultAgentRunner) Run(ctx context.Context, issue Issue, attempt *int, emit func(AgentEvent)) error {
	logger := r.logger().With("issue_id", issue.ID, "issue_identifier", issue.Identifier)
	ws, err := r.Workspace.CreateForIssue(ctx, issue.Identifier)
	if err != nil {
		logger.Error("workspace failed", "error", err)
		return fmt.Errorf("workspace error: %w", err)
	}
	afterRunNeeded := true
	defer func() {
		if afterRunNeeded && strings.TrimSpace(r.Config.Hooks.AfterRun) != "" {
			_ = r.Workspace.RunHook(context.Background(), "after_run", r.Config.Hooks.AfterRun, ws.Path, false)
		}
	}()

	if err := r.Workspace.RunHook(ctx, "before_run", r.Config.Hooks.BeforeRun, ws.Path, true); err != nil {
		return fmt.Errorf("before_run hook error: %w", err)
	}

	session, err := r.OpenCode.StartSession(ctx, ws.Path, issue, emit)
	if err != nil {
		return fmt.Errorf("agent session startup error: %w", err)
	}
	defer func() {
		if err := session.Stop(context.Background()); err != nil {
			logger.Warn("opencode stop failed", "session_id", session.ID, "error", err)
		}
	}()

	maxTurns := r.Config.Agent.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 20
	}
	current := cloneIssue(issue)
	for turn := 1; turn <= maxTurns; turn++ {
		prompt, err := r.buildTurnPrompt(current, attempt, turn, maxTurns)
		if err != nil {
			return fmt.Errorf("prompt error: %w", err)
		}
		if err := session.RunTurn(ctx, prompt, current, emit); err != nil {
			return fmt.Errorf("agent turn error: %w", err)
		}
		if r.Tracker == nil {
			break
		}
		refreshed, err := r.Tracker.FetchIssueStatesByIDs(ctx, []string{current.ID})
		if err != nil {
			return fmt.Errorf("issue state refresh error: %w", err)
		}
		if len(refreshed) > 0 {
			current = refreshed[0]
		}
		if !containsState(r.Config.Tracker.ActiveStates, current.State) {
			break
		}
	}
	afterRunNeeded = false
	if strings.TrimSpace(r.Config.Hooks.AfterRun) != "" {
		_ = r.Workspace.RunHook(context.Background(), "after_run", r.Config.Hooks.AfterRun, ws.Path, false)
	}
	return nil
}

func (r *DefaultAgentRunner) buildTurnPrompt(issue Issue, attempt *int, turn, maxTurns int) (string, error) {
	if turn == 1 {
		return RenderPrompt(r.Workflow.PromptTemplate, issue, attempt)
	}
	return fmt.Sprintf(
		"Continue working on %s (%s). This is continuation turn %d of %d in the current Symphony worker session. Do not repeat the original issue prompt; use the existing session history and move the issue toward the workflow-defined handoff state.",
		issue.Identifier,
		issue.Title,
		turn,
		maxTurns,
	), nil
}

func (r *DefaultAgentRunner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}
