package symphony

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

type Orchestrator struct {
	mu       sync.Mutex
	cfg      RuntimeConfig
	workflow WorkflowDefinition
	tracker  Tracker
	runner   AgentRunner
	logger   *slog.Logger

	running       map[string]*RunningEntry
	claimed       map[string]bool
	retryAttempts map[string]*RetryEntry
	completed     map[string]bool
	totals        OpenCodeTotals
	rateLimits    map[string]any
	recentEvents  map[string][]AgentEvent
	lastErrors    map[string]string
}

type RunningEntry struct {
	Issue                    Issue
	Identifier               string
	IssueURL                 string
	Attempt                  *int
	RetryAttempt             int
	Cancel                   context.CancelFunc
	StartedAt                time.Time
	SessionID                string
	MessageID                string
	OpenCodeServerPID        string
	LastOpenCodeEvent        string
	LastOpenCodeTimestamp    *time.Time
	LastOpenCodeMessage      string
	OpenCodeInputTokens      int64
	OpenCodeOutputTokens     int64
	OpenCodeTotalTokens      int64
	LastReportedInputTokens  int64
	LastReportedOutputTokens int64
	LastReportedTotalTokens  int64
	TurnCount                int
}

type RetryEntry struct {
	IssueID    string    `json:"issue_id"`
	Identifier string    `json:"issue_identifier"`
	IssueURL   string    `json:"issue_url,omitempty"`
	Attempt    int       `json:"attempt"`
	DueAt      time.Time `json:"due_at"`
	Error      string    `json:"error,omitempty"`
}

type OpenCodeTotals struct {
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	TotalTokens    int64   `json:"total_tokens"`
	SecondsRunning float64 `json:"seconds_running"`
}

type RuntimeSnapshot struct {
	GeneratedAt    time.Time               `json:"generated_at"`
	Counts         map[string]int          `json:"counts"`
	Running        []RunningSnapshot       `json:"running"`
	Retrying       []RetryEntry            `json:"retrying"`
	OpenCodeTotals OpenCodeTotals          `json:"opencode_totals"`
	RateLimits     map[string]any          `json:"rate_limits"`
	LastErrors     map[string]string       `json:"last_errors,omitempty"`
	RecentEvents   map[string][]AgentEvent `json:"recent_events,omitempty"`
}

type RunningSnapshot struct {
	IssueID         string        `json:"issue_id"`
	IssueIdentifier string        `json:"issue_identifier"`
	IssueURL        string        `json:"issue_url,omitempty"`
	State           string        `json:"state"`
	SessionID       string        `json:"session_id,omitempty"`
	TurnCount       int           `json:"turn_count"`
	LastEvent       string        `json:"last_event,omitempty"`
	LastMessage     string        `json:"last_message,omitempty"`
	StartedAt       time.Time     `json:"started_at"`
	LastEventAt     *time.Time    `json:"last_event_at,omitempty"`
	Tokens          TokenSnapshot `json:"tokens"`
}

type TokenSnapshot struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

func NewOrchestrator(cfg RuntimeConfig, workflow WorkflowDefinition, tracker Tracker, runner AgentRunner, logger *slog.Logger) *Orchestrator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Orchestrator{
		cfg:           cfg,
		workflow:      workflow,
		tracker:       tracker,
		runner:        runner,
		logger:        logger,
		running:       map[string]*RunningEntry{},
		claimed:       map[string]bool{},
		retryAttempts: map[string]*RetryEntry{},
		completed:     map[string]bool{},
		recentEvents:  map[string][]AgentEvent{},
		lastErrors:    map[string]string{},
	}
}

func (o *Orchestrator) UpdateConfig(cfg RuntimeConfig, workflow WorkflowDefinition) {
	o.UpdateRuntime(cfg, workflow, nil)
}

func (o *Orchestrator) UpdateRuntime(cfg RuntimeConfig, workflow WorkflowDefinition, tracker Tracker) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cfg = cfg
	o.workflow = workflow
	if tracker != nil {
		o.tracker = tracker
	}
}

func (o *Orchestrator) Run(ctx context.Context) error {
	if err := o.StartupCleanup(ctx); err != nil {
		o.logger.Warn("startup cleanup failed", "error", err)
	}
	if err := o.Tick(ctx); err != nil {
		o.logger.Warn("initial tick failed", "error", err)
	}
	for {
		interval := o.config().Polling.Interval
		if interval <= 0 {
			interval = 30 * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			o.cancelAll()
			return nil
		case <-timer.C:
			if err := o.Tick(ctx); err != nil {
				o.logger.Warn("tick failed", "error", err)
			}
		}
	}
}

func (o *Orchestrator) Tick(ctx context.Context) error {
	o.reconcileRunning(ctx)
	o.processDueRetries(ctx)
	cfg := o.config()
	if err := ValidateDispatchConfig(cfg); err != nil {
		o.logger.Error("dispatch validation failed", "error", err)
		return err
	}
	if o.tracker == nil {
		return nil
	}
	issues, err := o.tracker.FetchCandidateIssues(ctx)
	if err != nil {
		o.logger.Warn("candidate fetch failed", "error", err)
		return err
	}
	sortIssuesForDispatch(issues)
	for _, issue := range issues {
		if o.availableSlots() <= 0 {
			break
		}
		if o.shouldDispatch(issue) {
			o.dispatch(ctx, issue, nil)
		}
	}
	return nil
}

func (o *Orchestrator) StartupCleanup(ctx context.Context) error {
	cfg := o.config()
	if o.tracker == nil {
		return nil
	}
	terminal, err := o.tracker.FetchIssuesByStates(ctx, cfg.Tracker.TerminalStates)
	if err != nil {
		return err
	}
	wm := WorkspaceManager{Config: cfg, Logger: o.logger}
	for _, issue := range terminal {
		if err := wm.RemoveForIssue(ctx, issue.Identifier); err != nil {
			o.logger.Warn("terminal workspace cleanup failed", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "error", err)
		}
	}
	return nil
}

func (o *Orchestrator) dispatch(ctx context.Context, issue Issue, attempt *int) {
	o.mu.Lock()
	if _, ok := o.running[issue.ID]; ok {
		o.mu.Unlock()
		return
	}
	if attempt == nil && o.claimed[issue.ID] {
		o.mu.Unlock()
		return
	}
	cfg := o.cfg
	workflow := o.workflow
	runner := o.runner
	if runner == nil {
		runner = NewDefaultAgentRunner(cfg, workflow, o.tracker, o.logger)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	retryAttempt := 0
	if attempt != nil {
		retryAttempt = *attempt
	}
	entry := &RunningEntry{
		Issue:        cloneIssue(issue),
		Identifier:   issue.Identifier,
		Attempt:      cloneIntPtr(attempt),
		RetryAttempt: retryAttempt,
		Cancel:       cancel,
		StartedAt:    time.Now().UTC(),
	}
	if issue.URL != nil {
		entry.IssueURL = *issue.URL
	}
	o.running[issue.ID] = entry
	o.claimed[issue.ID] = true
	delete(o.retryAttempts, issue.ID)
	o.mu.Unlock()

	o.logger.Info("dispatch started", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "attempt", retryAttempt)
	go func() {
		err := runner.Run(workerCtx, issue, attempt, func(event AgentEvent) {
			o.ApplyAgentEvent(issue.ID, event)
		})
		o.handleWorkerExit(issue.ID, err)
	}()
}

func (o *Orchestrator) ApplyAgentEvent(issueID string, event AgentEvent) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	entry, ok := o.running[issueID]
	if !ok {
		return
	}
	if event.SessionID != "" {
		entry.SessionID = event.SessionID
	}
	if event.MessageID != "" {
		entry.MessageID = event.MessageID
	}
	if event.OpenCodeServerPID != "" {
		entry.OpenCodeServerPID = event.OpenCodeServerPID
	}
	entry.LastOpenCodeEvent = event.Event
	entry.LastOpenCodeTimestamp = &event.Timestamp
	entry.LastOpenCodeMessage = event.Message
	if event.Event == "turn_completed" || event.Event == "turn_failed" || event.Event == "turn_cancelled" || event.Event == "turn_ended_with_error" {
		entry.TurnCount++
	}
	o.applyTokenDeltas(entry, event)
	if event.RateLimits != nil {
		o.rateLimits = cloneMap(event.RateLimits)
	}
	o.recentEvents[issueID] = appendCappedEvents(o.recentEvents[issueID], event, 25)
}

func (o *Orchestrator) handleWorkerExit(issueID string, err error) {
	o.mu.Lock()
	entry, ok := o.running[issueID]
	if !ok {
		o.mu.Unlock()
		return
	}
	delete(o.running, issueID)
	o.addRuntimeLocked(entry)
	if err == nil {
		o.completed[issueID] = true
		o.scheduleRetryLocked(issueID, entry.Identifier, entry.IssueURL, 1, 1*time.Second, "")
		o.mu.Unlock()
		o.logger.Info("worker completed", "issue_id", issueID, "issue_identifier", entry.Identifier)
		return
	}
	attempt := entry.RetryAttempt + 1
	if attempt <= 0 {
		attempt = 1
	}
	delay := o.retryDelayLocked(attempt)
	message := fmt.Sprintf("worker exited: %v", err)
	o.lastErrors[issueID] = message
	o.scheduleRetryLocked(issueID, entry.Identifier, entry.IssueURL, attempt, delay, message)
	o.mu.Unlock()
	o.logger.Warn("worker failed", "issue_id", issueID, "issue_identifier", entry.Identifier, "attempt", attempt, "error", err)
}

func (o *Orchestrator) reconcileRunning(ctx context.Context) {
	o.reconcileStalled(ctx)
	ids := o.runningIDs()
	if len(ids) == 0 || o.tracker == nil {
		return
	}
	issues, err := o.tracker.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		o.logger.Warn("running state refresh failed", "error", err)
		return
	}
	byID := map[string]Issue{}
	for _, issue := range issues {
		byID[issue.ID] = issue
	}
	cfg := o.config()
	for _, id := range ids {
		issue, ok := byID[id]
		if !ok {
			continue
		}
		if containsState(cfg.Tracker.TerminalStates, issue.State) {
			o.terminateRunning(ctx, id, true, "terminal_state")
			continue
		}
		if containsState(cfg.Tracker.ActiveStates, issue.State) {
			o.mu.Lock()
			if entry, ok := o.running[id]; ok {
				entry.Issue = cloneIssue(issue)
				if issue.URL != nil {
					entry.IssueURL = *issue.URL
				}
			}
			o.mu.Unlock()
			continue
		}
		o.terminateRunning(ctx, id, false, "non_active_state")
	}
}

func (o *Orchestrator) reconcileStalled(ctx context.Context) {
	cfg := o.config()
	if cfg.OpenCode.StallTimeout <= 0 {
		return
	}
	now := time.Now().UTC()
	var stalled []string
	o.mu.Lock()
	for id, entry := range o.running {
		since := entry.StartedAt
		if entry.LastOpenCodeTimestamp != nil {
			since = *entry.LastOpenCodeTimestamp
		}
		if now.Sub(since) > cfg.OpenCode.StallTimeout {
			stalled = append(stalled, id)
		}
	}
	o.mu.Unlock()
	for _, id := range stalled {
		o.terminateStalled(ctx, id)
	}
}

func (o *Orchestrator) terminateStalled(ctx context.Context, issueID string) {
	o.mu.Lock()
	entry, ok := o.running[issueID]
	if !ok {
		o.mu.Unlock()
		return
	}
	delete(o.running, issueID)
	o.addRuntimeLocked(entry)
	attempt := entry.RetryAttempt + 1
	if attempt <= 0 {
		attempt = 1
	}
	delay := o.retryDelayLocked(attempt)
	o.scheduleRetryLocked(issueID, entry.Identifier, entry.IssueURL, attempt, delay, "stalled")
	cancel := entry.Cancel
	o.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	o.logger.Warn("worker stalled", "issue_id", issueID, "issue_identifier", entry.Identifier)
	_ = ctx
}

func (o *Orchestrator) terminateRunning(ctx context.Context, issueID string, cleanupWorkspace bool, reason string) {
	o.mu.Lock()
	entry, ok := o.running[issueID]
	if !ok {
		o.mu.Unlock()
		return
	}
	delete(o.running, issueID)
	delete(o.claimed, issueID)
	delete(o.retryAttempts, issueID)
	o.addRuntimeLocked(entry)
	cancel := entry.Cancel
	cfg := o.cfg
	o.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if cleanupWorkspace {
		wm := WorkspaceManager{Config: cfg, Logger: o.logger}
		if err := wm.RemoveForIssue(ctx, entry.Identifier); err != nil {
			o.logger.Warn("workspace cleanup failed", "issue_id", issueID, "issue_identifier", entry.Identifier, "error", err)
		}
	}
	o.logger.Info("worker terminated", "issue_id", issueID, "issue_identifier", entry.Identifier, "reason", reason, "cleanup_workspace", cleanupWorkspace)
}

func (o *Orchestrator) processDueRetries(ctx context.Context) {
	now := time.Now().UTC()
	var due []*RetryEntry
	o.mu.Lock()
	for id, entry := range o.retryAttempts {
		if !entry.DueAt.After(now) {
			due = append(due, cloneRetryEntry(entry))
			delete(o.retryAttempts, id)
		}
	}
	o.mu.Unlock()
	if len(due) == 0 || o.tracker == nil {
		return
	}
	candidates, err := o.tracker.FetchCandidateIssues(ctx)
	if err != nil {
		for _, retry := range due {
			o.scheduleRetry(retry.IssueID, retry.Identifier, retry.IssueURL, retry.Attempt+1, "retry poll failed")
		}
		return
	}
	byID := map[string]Issue{}
	for _, issue := range candidates {
		byID[issue.ID] = issue
	}
	for _, retry := range due {
		issue, ok := byID[retry.IssueID]
		if !ok {
			o.mu.Lock()
			delete(o.claimed, retry.IssueID)
			o.mu.Unlock()
			continue
		}
		if o.availableSlots() <= 0 {
			o.scheduleRetry(retry.IssueID, issue.Identifier, stringValueFromPtr(issue.URL), retry.Attempt+1, "no available orchestrator slots")
			continue
		}
		o.mu.Lock()
		delete(o.claimed, retry.IssueID)
		o.mu.Unlock()
		attempt := retry.Attempt
		if o.shouldDispatch(issue) {
			o.dispatch(ctx, issue, &attempt)
		} else {
			o.mu.Lock()
			delete(o.claimed, retry.IssueID)
			o.mu.Unlock()
		}
	}
}

func (o *Orchestrator) shouldDispatch(issue Issue) bool {
	cfg := o.config()
	if issue.ID == "" || issue.Identifier == "" || issue.Title == "" || issue.State == "" {
		return false
	}
	if !containsState(cfg.Tracker.ActiveStates, issue.State) || containsState(cfg.Tracker.TerminalStates, issue.State) {
		return false
	}
	if normalizeState(issue.State) == "todo" {
		for _, blocker := range issue.BlockedBy {
			if blocker.State == nil || !containsState(cfg.Tracker.TerminalStates, *blocker.State) {
				return false
			}
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.claimed[issue.ID] {
		return false
	}
	if _, ok := o.running[issue.ID]; ok {
		return false
	}
	if o.availableSlotsLocked() <= 0 {
		return false
	}
	stateLimit := cfg.Agent.MaxConcurrentAgentsByState[normalizeState(issue.State)]
	if stateLimit > 0 {
		count := 0
		for _, entry := range o.running {
			if normalizeState(entry.Issue.State) == normalizeState(issue.State) {
				count++
			}
		}
		if count >= stateLimit {
			return false
		}
	}
	return true
}

func (o *Orchestrator) Snapshot() RuntimeSnapshot {
	now := time.Now().UTC()
	o.mu.Lock()
	defer o.mu.Unlock()
	running := make([]RunningSnapshot, 0, len(o.running))
	liveSeconds := 0.0
	for id, entry := range o.running {
		liveSeconds += now.Sub(entry.StartedAt).Seconds()
		running = append(running, RunningSnapshot{
			IssueID:         id,
			IssueIdentifier: entry.Identifier,
			IssueURL:        entry.IssueURL,
			State:           entry.Issue.State,
			SessionID:       entry.SessionID,
			TurnCount:       entry.TurnCount,
			LastEvent:       entry.LastOpenCodeEvent,
			LastMessage:     entry.LastOpenCodeMessage,
			StartedAt:       entry.StartedAt,
			LastEventAt:     entry.LastOpenCodeTimestamp,
			Tokens: TokenSnapshot{
				InputTokens:  entry.OpenCodeInputTokens,
				OutputTokens: entry.OpenCodeOutputTokens,
				TotalTokens:  entry.OpenCodeTotalTokens,
			},
		})
	}
	sort.Slice(running, func(i, j int) bool {
		return running[i].IssueIdentifier < running[j].IssueIdentifier
	})
	retrying := make([]RetryEntry, 0, len(o.retryAttempts))
	for _, retry := range o.retryAttempts {
		retrying = append(retrying, *cloneRetryEntry(retry))
	}
	sort.Slice(retrying, func(i, j int) bool {
		return retrying[i].DueAt.Before(retrying[j].DueAt)
	})
	totals := o.totals
	totals.SecondsRunning += liveSeconds
	return RuntimeSnapshot{
		GeneratedAt: now,
		Counts: map[string]int{
			"running":  len(running),
			"retrying": len(retrying),
		},
		Running:        running,
		Retrying:       retrying,
		OpenCodeTotals: totals,
		RateLimits:     cloneMap(o.rateLimits),
		LastErrors:     cloneStringMap(o.lastErrors),
		RecentEvents:   cloneEventsMap(o.recentEvents),
	}
}

func (o *Orchestrator) scheduleRetry(issueID, identifier, issueURL string, attempt int, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.scheduleRetryLocked(issueID, identifier, issueURL, attempt, o.retryDelayLocked(attempt), reason)
}

func (o *Orchestrator) scheduleRetryLocked(issueID, identifier, issueURL string, attempt int, delay time.Duration, reason string) {
	if attempt <= 0 {
		attempt = 1
	}
	o.claimed[issueID] = true
	o.retryAttempts[issueID] = &RetryEntry{
		IssueID:    issueID,
		Identifier: identifier,
		IssueURL:   issueURL,
		Attempt:    attempt,
		DueAt:      time.Now().UTC().Add(delay),
		Error:      reason,
	}
}

func (o *Orchestrator) retryDelayLocked(attempt int) time.Duration {
	if attempt <= 0 {
		attempt = 1
	}
	delay := 10 * time.Second
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= o.cfg.Agent.MaxRetryBackoff {
			return o.cfg.Agent.MaxRetryBackoff
		}
	}
	if delay > o.cfg.Agent.MaxRetryBackoff {
		return o.cfg.Agent.MaxRetryBackoff
	}
	return delay
}

func (o *Orchestrator) addRuntimeLocked(entry *RunningEntry) {
	o.totals.SecondsRunning += time.Since(entry.StartedAt).Seconds()
}

func (o *Orchestrator) applyTokenDeltas(entry *RunningEntry, event AgentEvent) {
	if event.AbsoluteInputTokens != nil {
		delta := *event.AbsoluteInputTokens - entry.LastReportedInputTokens
		if delta > 0 {
			o.totals.InputTokens += delta
		}
		entry.LastReportedInputTokens = *event.AbsoluteInputTokens
		entry.OpenCodeInputTokens = *event.AbsoluteInputTokens
	}
	if event.AbsoluteOutputTokens != nil {
		delta := *event.AbsoluteOutputTokens - entry.LastReportedOutputTokens
		if delta > 0 {
			o.totals.OutputTokens += delta
		}
		entry.LastReportedOutputTokens = *event.AbsoluteOutputTokens
		entry.OpenCodeOutputTokens = *event.AbsoluteOutputTokens
	}
	if event.AbsoluteTotalTokens != nil {
		delta := *event.AbsoluteTotalTokens - entry.LastReportedTotalTokens
		if delta > 0 {
			o.totals.TotalTokens += delta
		}
		entry.LastReportedTotalTokens = *event.AbsoluteTotalTokens
		entry.OpenCodeTotalTokens = *event.AbsoluteTotalTokens
	}
}

func (o *Orchestrator) availableSlots() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.availableSlotsLocked()
}

func (o *Orchestrator) availableSlotsLocked() int {
	available := o.cfg.Agent.MaxConcurrentAgents - len(o.running)
	if available < 0 {
		return 0
	}
	return available
}

func (o *Orchestrator) runningIDs() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	ids := make([]string, 0, len(o.running))
	for id := range o.running {
		ids = append(ids, id)
	}
	return ids
}

func (o *Orchestrator) config() RuntimeConfig {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cfg
}

func (o *Orchestrator) cancelAll() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, entry := range o.running {
		if entry.Cancel != nil {
			entry.Cancel()
		}
	}
}

func sortIssuesForDispatch(issues []Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		if priorityRank(a.Priority) != priorityRank(b.Priority) {
			return priorityRank(a.Priority) < priorityRank(b.Priority)
		}
		if !timeEqualPtr(a.CreatedAt, b.CreatedAt) {
			if a.CreatedAt == nil {
				return false
			}
			if b.CreatedAt == nil {
				return true
			}
			return a.CreatedAt.Before(*b.CreatedAt)
		}
		return a.Identifier < b.Identifier
	})
}

func priorityRank(p *int) int {
	if p == nil {
		return 1 << 30
	}
	return *p
}

func timeEqualPtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func cloneIntPtr(v *int) *int {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func cloneRetryEntry(entry *RetryEntry) *RetryEntry {
	if entry == nil {
		return nil
	}
	c := *entry
	return &c
}

func appendCappedEvents(events []AgentEvent, event AgentEvent, max int) []AgentEvent {
	events = append(events, event)
	if len(events) <= max {
		return events
	}
	return append([]AgentEvent(nil), events[len(events)-max:]...)
}

func cloneStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneEventsMap(m map[string][]AgentEvent) map[string][]AgentEvent {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string][]AgentEvent, len(m))
	for k, v := range m {
		out[k] = append([]AgentEvent(nil), v...)
	}
	return out
}

func stringValueFromPtr(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
