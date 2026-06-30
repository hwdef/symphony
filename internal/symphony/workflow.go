package symphony

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

type WorkflowStore struct {
	path string
	env  func(string) string

	mu       sync.RWMutex
	def      WorkflowDefinition
	cfg      RuntimeConfig
	lastErr  error
	lastStat time.Time
}

func NewWorkflowStore(path string, env func(string) string) *WorkflowStore {
	if path == "" {
		path = filepath.Join(".", "WORKFLOW.md")
	}
	if env == nil {
		env = os.Getenv
	}
	return &WorkflowStore{path: path, env: env}
}

func SelectWorkflowPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return filepath.Join(".", "WORKFLOW.md")
}

func LoadWorkflow(path string) (WorkflowDefinition, error) {
	if path == "" {
		path = SelectWorkflowPath("")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return WorkflowDefinition{}, fmt.Errorf("%w: %s", ErrMissingWorkflowFile, path)
		}
		return WorkflowDefinition{}, fmt.Errorf("%w: %v", ErrMissingWorkflowFile, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return WorkflowDefinition{}, fmt.Errorf("%w: %v", ErrMissingWorkflowFile, err)
	}

	cfg, body, err := parseWorkflowBytes(data)
	if err != nil {
		return WorkflowDefinition{}, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return WorkflowDefinition{
		Config:         cfg,
		PromptTemplate: strings.TrimSpace(body),
		Path:           abs,
		ModTime:        info.ModTime(),
	}, nil
}

func parseWorkflowBytes(data []byte) (map[string]any, string, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") && strings.TrimSpace(text) != "---" {
		return map[string]any{}, strings.TrimSpace(text), nil
	}

	lines := strings.Split(text, "\n")
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return nil, "", fmt.Errorf("%w: missing closing front matter delimiter", ErrWorkflowParse)
	}

	front := strings.Join(lines[1:end], "\n")
	body := strings.Join(lines[end+1:], "\n")
	if strings.TrimSpace(front) == "" {
		return map[string]any{}, strings.TrimSpace(body), nil
	}

	var raw any
	if err := yaml.Unmarshal([]byte(front), &raw); err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrWorkflowParse, err)
	}
	converted := normalizeYAML(raw)
	if converted == nil {
		return map[string]any{}, strings.TrimSpace(body), nil
	}
	cfg, ok := converted.(map[string]any)
	if !ok {
		return nil, "", fmt.Errorf("%w", ErrWorkflowFrontMatterNotMap)
	}
	return cfg, strings.TrimSpace(body), nil
}

func normalizeYAML(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = normalizeYAML(v)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			ks, ok := k.(string)
			if !ok {
				ks = fmt.Sprint(k)
			}
			out[ks] = normalizeYAML(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = normalizeYAML(v)
		}
		return out
	default:
		return x
	}
}

func (s *WorkflowStore) LoadInitial() error {
	def, err := LoadWorkflow(s.path)
	if err != nil {
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		return err
	}
	cfg, err := ResolveConfig(def, s.env)
	if err != nil {
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		return err
	}
	if err := ValidateDispatchConfig(cfg); err != nil {
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	s.def = def
	s.cfg = cfg
	s.lastErr = nil
	s.lastStat = def.ModTime
	s.mu.Unlock()
	return nil
}

func (s *WorkflowStore) Definition() WorkflowDefinition {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.def
}

func (s *WorkflowStore) Config() RuntimeConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *WorkflowStore) LastError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastErr
}

func (s *WorkflowStore) ReloadIfChanged() (bool, error) {
	info, err := os.Stat(s.path)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrMissingWorkflowFile, err)
	}
	s.mu.RLock()
	last := s.lastStat
	s.mu.RUnlock()
	if !info.ModTime().After(last) {
		return false, nil
	}
	return s.Reload()
}

func (s *WorkflowStore) Reload() (bool, error) {
	def, err := LoadWorkflow(s.path)
	if err != nil {
		s.setReloadError(err)
		return false, err
	}
	cfg, err := ResolveConfig(def, s.env)
	if err != nil {
		s.setReloadError(err)
		return false, err
	}
	if err := ValidateDispatchConfig(cfg); err != nil {
		s.setReloadError(err)
		return false, err
	}
	s.mu.Lock()
	s.def = def
	s.cfg = cfg
	s.lastErr = nil
	s.lastStat = def.ModTime
	s.mu.Unlock()
	return true, nil
}

func (s *WorkflowStore) setReloadError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = err
}

func (s *WorkflowStore) Watch(ctx context.Context, onReload func(RuntimeConfig, WorkflowDefinition), onError func(error)) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := watcher.Add(dir); err != nil {
		_ = watcher.Close()
		return err
	}
	go func() {
		defer watcher.Close()
		timer := time.NewTimer(time.Hour)
		if !timer.Stop() {
			<-timer.C
		}
		pending := false
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if filepath.Clean(event.Name) != filepath.Clean(s.path) {
					continue
				}
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Chmod) == 0 {
					continue
				}
				if pending {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
				}
				pending = true
				timer.Reset(100 * time.Millisecond)
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				if onError != nil {
					onError(err)
				}
			case <-timer.C:
				pending = false
				changed, err := s.Reload()
				if err != nil {
					if onError != nil {
						onError(err)
					}
					continue
				}
				if changed && onReload != nil {
					onReload(s.Config(), s.Definition())
				}
			}
		}
	}()
	return nil
}
