package symphony

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var workspaceSafeChar = regexp.MustCompile(`[^A-Za-z0-9._-]`)

type Workspace struct {
	Path         string
	WorkspaceKey string
	CreatedNow   bool
}

type WorkspaceManager struct {
	Config RuntimeConfig
	Logger *slog.Logger
}

func SanitizeWorkspaceKey(identifier string) string {
	key := workspaceSafeChar.ReplaceAllString(identifier, "_")
	if key == "" {
		return "_"
	}
	return key
}

func (m WorkspaceManager) CreateForIssue(ctx context.Context, identifier string) (Workspace, error) {
	root, err := filepath.Abs(m.Config.Workspace.Root)
	if err != nil {
		return Workspace{}, err
	}
	key := SanitizeWorkspaceKey(identifier)
	path := filepath.Join(root, key)
	if err := EnsureInsideRoot(root, path); err != nil {
		return Workspace{}, err
	}

	created := false
	info, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return Workspace{}, err
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return Workspace{}, err
		}
		created = true
	} else if !info.IsDir() {
		return Workspace{}, fmt.Errorf("workspace path exists and is not a directory: %s", path)
	}

	ws := Workspace{Path: path, WorkspaceKey: key, CreatedNow: created}
	if created && strings.TrimSpace(m.Config.Hooks.AfterCreate) != "" {
		if err := m.RunHook(ctx, "after_create", m.Config.Hooks.AfterCreate, path, true); err != nil {
			return Workspace{}, err
		}
	}
	return ws, nil
}

func (m WorkspaceManager) RemoveForIssue(ctx context.Context, identifier string) error {
	root, err := filepath.Abs(m.Config.Workspace.Root)
	if err != nil {
		return err
	}
	path := filepath.Join(root, SanitizeWorkspaceKey(identifier))
	if err := EnsureInsideRoot(root, path); err != nil {
		return err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	if strings.TrimSpace(m.Config.Hooks.BeforeRemove) != "" {
		_ = m.RunHook(ctx, "before_remove", m.Config.Hooks.BeforeRemove, path, false)
	}
	return os.RemoveAll(path)
}

func (m WorkspaceManager) RunHook(ctx context.Context, name, script, cwd string, fatal bool) error {
	if strings.TrimSpace(script) == "" {
		return nil
	}
	timeout := m.Config.Hooks.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	hookCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	logger := m.logger()
	logger.Info("hook started", "hook", name, "cwd", cwd)
	cmd := exec.CommandContext(hookCtx, "sh", "-lc", script)
	cmd.Dir = cwd
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{Buffer: &out, Limit: 64 * 1024}
	cmd.Stderr = &limitedWriter{Buffer: &out, Limit: 64 * 1024}
	err := cmd.Run()
	if hookCtx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("hook %s timed out after %s", name, timeout)
	}
	if err != nil {
		logger.Warn("hook failed", "hook", name, "cwd", cwd, "error", err, "output", truncateString(out.String(), 4096))
		if fatal {
			return err
		}
		return nil
	}
	logger.Info("hook completed", "hook", name, "cwd", cwd)
	return nil
}

func EnsureInsideRoot(root, path string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return fmt.Errorf("workspace path %s is outside root %s", absPath, absRoot)
	}
	return nil
}

func ValidateAgentCWD(workspacePath, cwd string) error {
	absWorkspace, err := filepath.Abs(workspacePath)
	if err != nil {
		return err
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return err
	}
	if absWorkspace != absCWD {
		return fmt.Errorf("%w: cwd=%s workspace=%s", ErrInvalidWorkspaceCWD, absCWD, absWorkspace)
	}
	return nil
}

func (m WorkspaceManager) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

type limitedWriter struct {
	*bytes.Buffer
	Limit int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.Buffer.Len() < w.Limit {
		remaining := w.Limit - w.Buffer.Len()
		if len(p) <= remaining {
			_, _ = w.Buffer.Write(p)
		} else {
			_, _ = w.Buffer.Write(p[:remaining])
		}
	}
	return len(p), nil
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
