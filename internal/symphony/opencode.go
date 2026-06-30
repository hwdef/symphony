package symphony

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type OpenCodeClient struct {
	Config RuntimeConfig
	HTTP   *http.Client
	Logger *slog.Logger
}

type OpenCodeSession struct {
	ID         string
	BaseURL    string
	Workspace  string
	Process    *os.Process
	cmd        *exec.Cmd
	done       chan error
	cancelSSE  context.CancelFunc
	httpClient *http.Client
	cfg        RuntimeConfig
	logger     *slog.Logger
	mu         sync.Mutex
	stopped    bool
}

func (c *OpenCodeClient) StartSession(ctx context.Context, workspace string, issue Issue, onEvent func(AgentEvent)) (*OpenCodeSession, error) {
	if err := ValidateAgentCWD(workspace, workspace); err != nil {
		return nil, err
	}
	cfg := c.Config
	if err := EnsureInsideRoot(cfg.Workspace.Root, workspace); err != nil {
		return nil, err
	}
	logger := c.logger()
	client := c.httpClient()
	baseURL := strings.TrimRight(cfg.OpenCode.BaseURL, "/")
	var cmd *exec.Cmd
	var proc *os.Process
	var done chan error
	if baseURL == "" {
		host := cfg.OpenCode.Host
		port := cfg.OpenCode.Port
		if port == 0 {
			allocated, err := allocateLoopbackPort(host)
			if err != nil {
				return nil, err
			}
			port = allocated
		}
		baseURL = "http://" + net.JoinHostPort(host, strconv.Itoa(port))
		if err := c.writeOpenCodeConfig(workspace); err != nil {
			return nil, err
		}
		command := cfg.OpenCode.Command
		if strings.TrimSpace(command) == "" {
			return nil, ErrMissingOpenCodeCommand
		}
		cmd = exec.CommandContext(ctx, "bash", "-lc", command)
		cmd.Dir = workspace
		cmd.Env = append(os.Environ(),
			"OPENCODE_HOST="+host,
			"OPENCODE_PORT="+strconv.Itoa(port),
		)
		if cfg.OpenCode.AuthPassword != "" {
			cmd.Env = append(cmd.Env, "OPENCODE_SERVER_PASSWORD="+cfg.OpenCode.AuthPassword)
		}
		stdout, _ := cmd.StdoutPipe()
		stderr, _ := cmd.StderrPipe()
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("opencode_not_found: %w", err)
		}
		proc = cmd.Process
		done = make(chan error, 1)
		go drainProcessOutput(logger, "stdout", stdout)
		go drainProcessOutput(logger, "stderr", stderr)
		go func() {
			err := cmd.Wait()
			if err != nil {
				logger.Warn("opencode server exited", "pid", proc.Pid, "error", err)
			} else {
				logger.Info("opencode server exited", "pid", proc.Pid)
			}
			done <- err
			close(done)
		}()
	}

	session := &OpenCodeSession{
		BaseURL:    baseURL,
		Workspace:  workspace,
		Process:    proc,
		cmd:        cmd,
		done:       done,
		httpClient: client,
		cfg:        cfg,
		logger:     logger,
	}
	if err := session.waitReady(ctx); err != nil {
		_ = session.Stop(context.Background())
		return nil, err
	}
	if err := session.create(ctx, issue); err != nil {
		_ = session.Stop(context.Background())
		return nil, err
	}
	if onEvent != nil {
		pid := ""
		if proc != nil {
			pid = strconv.Itoa(proc.Pid)
		}
		onEvent(AgentEvent{
			Event:             "session_started",
			Timestamp:         time.Now().UTC(),
			SessionID:         session.ID,
			OpenCodeServerPID: pid,
		})
		sseCtx, cancel := context.WithCancel(ctx)
		session.cancelSSE = cancel
		go session.streamEvents(sseCtx, onEvent)
	}
	return session, nil
}

func (s *OpenCodeSession) RunTurn(ctx context.Context, prompt string, issue Issue, onEvent func(AgentEvent)) error {
	turnCtx, cancel := context.WithTimeout(ctx, s.cfg.OpenCode.TurnTimeout)
	defer cancel()
	payload := map[string]any{
		"parts": []map[string]any{
			{"type": "text", "text": prompt},
		},
	}
	if s.cfg.OpenCode.Agent != "" {
		payload["agent"] = s.cfg.OpenCode.Agent
	}
	if s.cfg.OpenCode.Model != nil {
		payload["model"] = s.cfg.OpenCode.Model
	}
	if s.cfg.OpenCode.Permission != nil {
		payload["permission"] = s.cfg.OpenCode.Permission
	}
	resp, body, err := s.doJSON(turnCtx, http.MethodPost, "/session/"+url.PathEscape(s.ID)+"/message", payload)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http_error: status=%d body=%s", resp.StatusCode, truncateString(string(body), 2048))
	}
	event := agentEventFromPayload("turn_completed", body)
	event.Timestamp = time.Now().UTC()
	event.SessionID = s.ID
	if event.Message == "" {
		event.Message = "message response received"
	}
	if onEvent != nil {
		onEvent(event)
	}
	if err := eventTerminalError(event.Event); err != nil {
		return err
	}
	return nil
}

func (s *OpenCodeSession) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	cancel := s.cancelSSE
	proc := s.Process
	done := s.done
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if s.ID != "" {
		stopCtx, cancelReq := context.WithTimeout(ctx, s.cfg.OpenCode.ReadTimeout)
		req, err := s.newRequest(stopCtx, http.MethodDelete, "/session/"+url.PathEscape(s.ID), nil)
		if err == nil {
			_, _ = s.httpClient.Do(req)
		}
		cancelReq()
	}
	if proc == nil {
		return nil
	}
	if done != nil {
		select {
		case <-done:
			return nil
		default:
		}
	}
	if err := proc.Signal(os.Interrupt); err != nil {
		_ = proc.Kill()
		return err
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = proc.Kill()
		}
	}
	return nil
}

func (s *OpenCodeSession) waitReady(ctx context.Context) error {
	deadlineCtx, cancel := context.WithTimeout(ctx, s.cfg.OpenCode.ReadTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		req, err := s.newRequest(deadlineCtx, http.MethodGet, "/global/health", nil)
		if err != nil {
			return err
		}
		resp, err := s.httpClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			lastErr = fmt.Errorf("health status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case <-deadlineCtx.Done():
			return fmt.Errorf("health_timeout: %w: %v", deadlineCtx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func (s *OpenCodeSession) create(ctx context.Context, issue Issue) error {
	title := strings.TrimSpace(issue.Identifier + ": " + issue.Title)
	payload := map[string]any{"title": title}
	resp, body, err := s.doJSON(ctx, http.MethodPost, "/session", payload)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("response_error: status=%d body=%s", resp.StatusCode, truncateString(string(body), 2048))
	}
	id, err := extractID(body)
	if err != nil {
		return err
	}
	s.ID = id
	return nil
}

func (s *OpenCodeSession) streamEvents(ctx context.Context, onEvent func(AgentEvent)) {
	for _, path := range []string{"/event", "/global/event"} {
		if s.tryStreamEvents(ctx, path, onEvent) {
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func (s *OpenCodeSession) tryStreamEvents(ctx context.Context, path string, onEvent func(AgentEvent)) bool {
	req, err := s.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10<<20)
	var eventName string
	var dataLines []string
	flush := func() {
		if len(dataLines) == 0 {
			eventName = ""
			return
		}
		data := strings.Join(dataLines, "\n")
		event := agentEventFromPayload(eventName, []byte(data))
		if event.Event == "" {
			event.Event = "other_message"
		}
		if event.Timestamp.IsZero() {
			event.Timestamp = time.Now().UTC()
		}
		if event.SessionID == "" {
			event.SessionID = s.ID
		}
		onEvent(event)
		eventName = ""
		dataLines = dataLines[:0]
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flush()
	return scanner.Err() == nil
}

func (s *OpenCodeSession) doJSON(ctx context.Context, method, path string, payload any) (*http.Response, []byte, error) {
	body := bytes.Buffer{}
	if payload != nil {
		if err := json.NewEncoder(&body).Encode(payload); err != nil {
			return nil, nil, err
		}
	}
	reqCtx, cancel := context.WithTimeout(ctx, s.cfg.OpenCode.ReadTimeout)
	if method == http.MethodPost && strings.Contains(path, "/message") {
		cancel()
		reqCtx, cancel = context.WithTimeout(ctx, s.cfg.OpenCode.TurnTimeout)
	}
	defer cancel()
	req, err := s.newRequest(reqCtx, method, path, &body)
	if err != nil {
		return nil, nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		if reqCtx.Err() == context.DeadlineExceeded {
			if method == http.MethodPost && strings.Contains(path, "/message") {
				return nil, nil, fmt.Errorf("turn_timeout: %w", reqCtx.Err())
			}
			return nil, nil, fmt.Errorf("response_timeout: %w", reqCtx.Err())
		}
		return nil, nil, fmt.Errorf("http_error: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return resp, nil, fmt.Errorf("response_error: %w", err)
	}
	return resp, respBody, nil
}

func (s *OpenCodeSession) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.BaseURL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	if s.cfg.OpenCode.AuthUsername != "" || s.cfg.OpenCode.AuthPassword != "" {
		req.SetBasicAuth(s.cfg.OpenCode.AuthUsername, s.cfg.OpenCode.AuthPassword)
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (c *OpenCodeClient) writeOpenCodeConfig(workspace string) error {
	cfg := c.Config.OpenCode
	if len(cfg.Config) == 0 && cfg.Permission == nil {
		return nil
	}
	passThrough := cloneMap(cfg.Config)
	if passThrough == nil {
		passThrough = map[string]any{}
	}
	if cfg.Permission != nil {
		if _, ok := passThrough["permission"]; !ok {
			passThrough["permission"] = cfg.Permission
		}
	}
	data, err := json.MarshalIndent(passThrough, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(workspace, ".opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "symphony-config.json"), data, 0o600); err != nil {
		return err
	}
	nativePath := filepath.Join(workspace, "opencode.json")
	if _, err := os.Stat(nativePath); err == nil {
		c.logger().Warn("opencode.json already exists; leaving repo-owned config unchanged", "path", nativePath, "sidecar", filepath.Join(dir, "symphony-config.json"))
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(nativePath, data, 0o600)
}

func (c *OpenCodeClient) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	timeout := c.Config.OpenCode.TurnTimeout
	if timeout <= 0 {
		timeout = time.Hour
	}
	return &http.Client{Timeout: timeout + c.Config.OpenCode.ReadTimeout}
}

func (c *OpenCodeClient) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func allocateLoopbackPort(host string) (int, error) {
	if host == "" {
		host = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func drainProcessOutput(logger *slog.Logger, stream string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		logger.Debug("opencode diagnostic", "stream", stream, "message", truncateString(line, 2048))
	}
}

func extractID(body []byte) (string, error) {
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", fmt.Errorf("response_error: %w", err)
	}
	if id := findStringByKey(raw, "id"); id != "" {
		return id, nil
	}
	if id := findStringByKey(raw, "session_id"); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("response_error: missing session id")
}

func eventTerminalError(event string) error {
	switch event {
	case "turn_failed":
		return fmt.Errorf("turn_failed")
	case "turn_cancelled":
		return fmt.Errorf("turn_cancelled")
	case "turn_ended_with_error":
		return fmt.Errorf("turn_failed")
	case "turn_input_required":
		return fmt.Errorf("turn_input_required")
	case "permission_denied":
		return fmt.Errorf("permission_denied")
	default:
		return nil
	}
}

func agentEventFromPayload(defaultEvent string, body []byte) AgentEvent {
	event := AgentEvent{Event: defaultEvent, Timestamp: time.Now().UTC()}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		event.Message = truncateString(string(body), 2048)
		return event
	}
	event.Raw = raw
	if name := firstString(raw, "event", "type", "name"); name != "" {
		event.Event = name
	}
	event.MessageID = firstString(raw, "message_id", "messageID", "id")
	event.SessionID = firstString(raw, "session_id", "sessionID")
	event.Message = firstString(raw, "message", "text", "summary", "content")
	if ts := firstString(raw, "timestamp", "time", "created_at"); ts != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			event.Timestamp = parsed
		}
	}
	if usage, ok := findMapByKey(raw, "usage"); ok {
		event.Usage = usage
		if in, ok := findInt64Any(usage, "input_tokens", "input", "prompt_tokens", "prompt"); ok {
			event.AbsoluteInputTokens = &in
		}
		if out, ok := findInt64Any(usage, "output_tokens", "output", "completion_tokens", "completion"); ok {
			event.AbsoluteOutputTokens = &out
		}
		if total, ok := findInt64Any(usage, "total_tokens", "total"); ok {
			event.AbsoluteTotalTokens = &total
		}
	}
	if totals, ok := findMapByKey(raw, "total_token_usage"); ok {
		event.Usage = totals
		if in, ok := findInt64Any(totals, "input_tokens", "input", "prompt_tokens", "prompt"); ok {
			event.AbsoluteInputTokens = &in
		}
		if out, ok := findInt64Any(totals, "output_tokens", "output", "completion_tokens", "completion"); ok {
			event.AbsoluteOutputTokens = &out
		}
		if total, ok := findInt64Any(totals, "total_tokens", "total"); ok {
			event.AbsoluteTotalTokens = &total
		}
	}
	if rate, ok := findMapByKey(raw, "rate_limits"); ok {
		event.RateLimits = rate
	}
	return event
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := m[key]; ok {
			if s := scalarString(value); s != "" {
				return s
			}
		}
	}
	return ""
}

func findStringByKey(v any, key string) string {
	switch x := v.(type) {
	case map[string]any:
		if value, ok := x[key]; ok {
			if s := scalarString(value); s != "" {
				return s
			}
		}
		for _, value := range x {
			if s := findStringByKey(value, key); s != "" {
				return s
			}
		}
	case []any:
		for _, value := range x {
			if s := findStringByKey(value, key); s != "" {
				return s
			}
		}
	}
	return ""
}

func findMapByKey(v any, key string) (map[string]any, bool) {
	switch x := v.(type) {
	case map[string]any:
		if value, ok := x[key]; ok {
			if m, ok := value.(map[string]any); ok {
				return m, true
			}
		}
		for _, value := range x {
			if m, ok := findMapByKey(value, key); ok {
				return m, true
			}
		}
	case []any:
		for _, value := range x {
			if m, ok := findMapByKey(value, key); ok {
				return m, true
			}
		}
	}
	return nil, false
}

func findInt64Any(m map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		value, ok := m[key]
		if !ok {
			continue
		}
		switch x := value.(type) {
		case int64:
			return x, true
		case int:
			return int64(x), true
		case float64:
			return int64(x), true
		case json.Number:
			n, err := x.Int64()
			return n, err == nil
		case string:
			n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
			if err == nil {
				return n, true
			}
		}
	}
	return 0, false
}
