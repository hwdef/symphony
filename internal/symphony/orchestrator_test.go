package symphony

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestOrchestratorNormalExitSchedulesContinuationRetry(t *testing.T) {
	cfg := testRuntimeConfig(t.TempDir())
	tracker := newFakeTracker([]Issue{testIssue("1", "#1", "Todo")})
	runner := &fakeRunner{returnErr: nil, emitUsage: true, started: make(chan Issue, 1)}
	orch := NewOrchestrator(cfg, WorkflowDefinition{PromptTemplate: "work"}, tracker, runner, nil)

	if err := orch.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	eventually(t, func() bool {
		return orch.Snapshot().Counts["retrying"] == 1
	})
	snapshot := orch.Snapshot()
	if snapshot.Retrying[0].Attempt != 1 {
		t.Fatalf("continuation retry attempt = %d", snapshot.Retrying[0].Attempt)
	}
	if snapshot.OpenCodeTotals.TotalTokens != 15 {
		t.Fatalf("token totals not aggregated: %#v", snapshot.OpenCodeTotals)
	}
}

func TestOrchestratorTerminalReconciliationCancelsAndCleansWorkspace(t *testing.T) {
	root := t.TempDir()
	cfg := testRuntimeConfig(root)
	tracker := newFakeTracker([]Issue{testIssue("1", "#1", "Todo")})
	runner := &fakeRunner{block: true, started: make(chan Issue, 1)}
	orch := NewOrchestrator(cfg, WorkflowDefinition{PromptTemplate: "work"}, tracker, runner, nil)
	wsPath := filepath.Join(root, SanitizeWorkspaceKey("#1"))
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := orch.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	eventually(t, func() bool {
		return orch.Snapshot().Counts["running"] == 1
	})

	tracker.setCandidates(nil)
	tracker.setState(testIssue("1", "#1", "Done"))
	if err := orch.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		return orch.Snapshot().Counts["running"] == 0
	})
	if _, err := os.Stat(wsPath); !os.IsNotExist(err) {
		t.Fatalf("terminal workspace was not removed, stat err=%v", err)
	}
}

func TestOrchestratorTodoBlockedByNonTerminalIsNotDispatched(t *testing.T) {
	cfg := testRuntimeConfig(t.TempDir())
	blockerState := "In Progress"
	issue := testIssue("1", "#1", "Todo")
	issue.BlockedBy = []BlockerRef{{State: &blockerState}}
	tracker := newFakeTracker([]Issue{issue})
	runner := &fakeRunner{returnErr: nil, started: make(chan Issue, 1)}
	orch := NewOrchestrator(cfg, WorkflowDefinition{PromptTemplate: "work"}, tracker, runner, nil)

	if err := orch.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if snapshot := orch.Snapshot(); snapshot.Counts["running"] != 0 || snapshot.Counts["retrying"] != 0 {
		t.Fatalf("blocked issue should not dispatch: %#v", snapshot.Counts)
	}
	select {
	case issue := <-runner.started:
		t.Fatalf("runner unexpectedly started for %#v", issue)
	default:
	}
}

func testRuntimeConfig(root string) RuntimeConfig {
	return RuntimeConfig{
		Tracker: TrackerConfig{
			Kind:           "gitlab",
			APIKey:         "token",
			ProjectID:      "123",
			ActiveStates:   []string{"Todo", "In Progress"},
			TerminalStates: []string{"Done", "Closed"},
		},
		Polling:   PollingConfig{Interval: time.Hour},
		Workspace: WorkspaceConfig{Root: root},
		Hooks:     HooksConfig{Timeout: time.Second},
		Agent: AgentConfig{
			MaxConcurrentAgents:        2,
			MaxTurns:                   1,
			MaxRetryBackoff:            time.Minute,
			MaxConcurrentAgentsByState: map[string]int{},
		},
		OpenCode: OpenCodeConfig{
			BaseURL:      "http://127.0.0.1:9",
			ReadTimeout:  time.Second,
			TurnTimeout:  time.Second,
			StallTimeout: time.Hour,
		},
	}
}

func testIssue(id, identifier, state string) Issue {
	return Issue{ID: id, Identifier: identifier, Title: "Issue " + identifier, State: state}
}

type fakeTracker struct {
	mu         sync.Mutex
	candidates []Issue
	states     map[string]Issue
	terminal   []Issue
}

func newFakeTracker(candidates []Issue) *fakeTracker {
	states := map[string]Issue{}
	for _, issue := range candidates {
		states[issue.ID] = issue
	}
	return &fakeTracker{candidates: candidates, states: states}
}

func (f *fakeTracker) FetchCandidateIssues(context.Context) ([]Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneIssues(f.candidates), nil
}

func (f *fakeTracker) FetchIssuesByStates(context.Context, []string) ([]Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneIssues(f.terminal), nil
}

func (f *fakeTracker) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Issue
	for _, id := range ids {
		if issue, ok := f.states[id]; ok {
			out = append(out, cloneIssue(issue))
		}
	}
	return out, nil
}

func (f *fakeTracker) setState(issue Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[issue.ID] = issue
}

func (f *fakeTracker) setCandidates(issues []Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.candidates = issues
}

func cloneIssues(issues []Issue) []Issue {
	out := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		out = append(out, cloneIssue(issue))
	}
	return out
}

type fakeRunner struct {
	returnErr error
	block     bool
	emitUsage bool
	started   chan Issue
}

func (r *fakeRunner) Run(ctx context.Context, issue Issue, _ *int, emit func(AgentEvent)) error {
	if r.started != nil {
		r.started <- issue
	}
	if emit != nil {
		emit(AgentEvent{Event: "session_started", Timestamp: time.Now().UTC(), SessionID: "ses_" + issue.ID})
		if r.emitUsage {
			in, out, total := int64(10), int64(5), int64(15)
			emit(AgentEvent{Event: "turn_completed", Timestamp: time.Now().UTC(), SessionID: "ses_" + issue.ID, AbsoluteInputTokens: &in, AbsoluteOutputTokens: &out, AbsoluteTotalTokens: &total})
		}
	}
	if r.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if r.returnErr != nil {
		return r.returnErr
	}
	return nil
}

func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ok() {
		t.Fatal(errors.New("condition was not met"))
	}
}
