package symphony

import (
	"context"
	"log/slog"
)

func NewTrackerFromConfig(cfg RuntimeConfig) (Tracker, error) {
	if cfg.Tracker.Kind != "gitlab" {
		return nil, ErrUnsupportedTrackerKind
	}
	return NewGitLabClient(cfg.Tracker), nil
}

func BuildService(ctx context.Context, store *WorkflowStore, logger *slog.Logger) (*Orchestrator, error) {
	if err := store.LoadInitial(); err != nil {
		return nil, err
	}
	cfg := store.Config()
	tracker, err := NewTrackerFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	orch := NewOrchestrator(cfg, store.Definition(), tracker, nil, logger)
	if err := store.Watch(ctx, func(cfg RuntimeConfig, def WorkflowDefinition) {
		tracker, err := NewTrackerFromConfig(cfg)
		if err != nil {
			if logger != nil {
				logger.Error("workflow reload tracker setup failed", "error", err)
			}
			return
		}
		orch.UpdateRuntime(cfg, def, tracker)
		if logger != nil {
			logger.Info("workflow reloaded", "path", cfg.WorkflowPath)
		}
	}, func(err error) {
		if logger != nil {
			logger.Error("workflow reload failed", "error", err)
		}
	}); err != nil {
		return nil, err
	}
	return orch, nil
}
