package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/openai/symphony/internal/symphony"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("symphony", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	port := fs.Int("port", -1, "enable HTTP observability server on this port")
	logsRoot := fs.String("logs-root", "", "optional directory for structured service logs")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	workflowPath := symphony.SelectWorkflowPath("")
	if fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "usage: symphony [--port PORT] [--logs-root DIR] [path-to-WORKFLOW.md]")
		return 2
	}
	if fs.NArg() == 1 {
		workflowPath = fs.Arg(0)
	}

	logger, closeLogs, err := buildLogger(*logsRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to configure logs: %v\n", err)
		return 1
	}
	defer closeLogs()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store := symphony.NewWorkflowStore(workflowPath, os.Getenv)
	orch, err := symphony.BuildService(ctx, store, logger)
	if err != nil {
		logger.Error("startup failed", "error", err)
		return 1
	}
	cfg := store.Config()
	effectivePort := -1
	if cfg.Server.Port != nil {
		effectivePort = *cfg.Server.Port
	}
	if *port >= 0 {
		effectivePort = *port
	}
	if effectivePort >= 0 {
		_, url, err := symphony.StartHTTPServer(ctx, cfg.Server.Host, effectivePort, orch, logger)
		if err != nil {
			logger.Error("http server startup failed", "error", err)
			return 1
		}
		logger.Info("http server started", "url", url)
		fmt.Fprintf(os.Stderr, "Symphony dashboard: %s\n", url)
	}

	if err := orch.Run(ctx); err != nil {
		logger.Error("service failed", "error", err)
		return 1
	}
	logger.Info("service stopped")
	return 0
}

func buildLogger(logsRoot string) (*slog.Logger, func(), error) {
	var writers []io.Writer
	writers = append(writers, os.Stderr)
	var file *os.File
	if logsRoot != "" {
		if err := os.MkdirAll(logsRoot, 0o755); err != nil {
			return nil, func() {}, err
		}
		f, err := os.OpenFile(filepath.Join(logsRoot, "symphony.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, func() {}, err
		}
		file = f
		writers = append(writers, f)
	}
	handler := slog.NewJSONHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: slog.LevelInfo})
	closeFn := func() {
		if file != nil {
			_ = file.Close()
		}
	}
	return slog.New(handler), closeFn, nil
}
