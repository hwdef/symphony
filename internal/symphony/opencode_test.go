package symphony

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenCodeExternalServerSessionAndTurn(t *testing.T) {
	var sawSession bool
	var sawMessage bool
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/global/health":
			return testJSONResponse(t, http.StatusOK, nil, map[string]any{"ok": true}), nil
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			sawSession = true
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(payload["title"].(string), "#1: Fix") {
				t.Fatalf("session title = %#v", payload["title"])
			}
			return testJSONResponse(t, http.StatusOK, nil, map[string]any{"id": "ses_1"}), nil
		case r.Method == http.MethodPost && r.URL.Path == "/session/ses_1/message":
			sawMessage = true
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["agent"] != "build" {
				t.Fatalf("agent not passed: %#v", payload)
			}
			parts := payload["parts"].([]any)
			first := parts[0].(map[string]any)
			if first["text"] != "do work" {
				t.Fatalf("prompt not passed: %#v", payload)
			}
			return testJSONResponse(t, http.StatusOK, nil, map[string]any{
				"id":    "msg_1",
				"event": "turn_completed",
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
			}), nil
		case r.Method == http.MethodDelete && r.URL.Path == "/session/ses_1":
			return testJSONResponse(t, http.StatusOK, nil, map[string]any{"ok": true}), nil
		case r.URL.Path == "/event" || r.URL.Path == "/global/event":
			return testJSONResponse(t, http.StatusNotFound, nil, map[string]any{"error": "not found"}), nil
		default:
			t.Fatalf("unexpected opencode request %s %s", r.Method, r.URL.Path)
			return testJSONResponse(t, http.StatusNotFound, nil, nil), nil
		}
	})

	cfg := RuntimeConfig{
		Workspace: WorkspaceConfig{Root: t.TempDir()},
		OpenCode: OpenCodeConfig{
			BaseURL:     "http://opencode.test",
			Agent:       "build",
			ReadTimeout: 500 * time.Millisecond,
			TurnTimeout: time.Second,
		},
	}
	client := OpenCodeClient{Config: cfg, HTTP: &http.Client{Transport: transport}}
	var events []AgentEvent
	session, err := client.StartSession(t.Context(), cfg.Workspace.Root, Issue{ID: "1", Identifier: "#1", Title: "Fix", State: "Todo"}, func(event AgentEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Stop(t.Context())
	if session.ID != "ses_1" || !sawSession {
		t.Fatalf("session not created: %#v", session)
	}
	if err := session.RunTurn(t.Context(), "do work", Issue{ID: "1", Identifier: "#1", Title: "Fix", State: "Todo"}, func(event AgentEvent) {
		events = append(events, event)
	}); err != nil {
		t.Fatal(err)
	}
	if !sawMessage {
		t.Fatalf("message endpoint was not called")
	}
	if len(events) < 2 {
		t.Fatalf("expected session and turn events: %#v", events)
	}
	last := events[len(events)-1]
	if last.Event != "turn_completed" || last.AbsoluteTotalTokens == nil || *last.AbsoluteTotalTokens != 15 {
		t.Fatalf("turn event did not extract usage: %#v", last)
	}
}

func TestOpenCodeTurnInputRequiredFailsTurn(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session/ses_1/message":
			return testJSONResponse(t, http.StatusOK, nil, map[string]any{"event": "turn_input_required"}), nil
		default:
			return testJSONResponse(t, http.StatusOK, nil, map[string]any{"ok": true}), nil
		}
	})
	cfg := RuntimeConfig{
		OpenCode: OpenCodeConfig{
			BaseURL:     "http://opencode.test",
			ReadTimeout: 500 * time.Millisecond,
			TurnTimeout: time.Second,
		},
	}
	session := &OpenCodeSession{
		ID:         "ses_1",
		BaseURL:    cfg.OpenCode.BaseURL,
		httpClient: &http.Client{Transport: transport},
		cfg:        cfg,
	}
	err := session.RunTurn(t.Context(), "prompt", Issue{ID: "1", Identifier: "#1", Title: "Fix", State: "Todo"}, nil)
	if err == nil || err.Error() != "turn_input_required" {
		t.Fatalf("expected turn_input_required error, got %v", err)
	}
}

func TestOpenCodeMaterializesConfigWithoutOverwritingExistingNativeConfig(t *testing.T) {
	workspace := t.TempDir()
	cfg := RuntimeConfig{
		Workspace: WorkspaceConfig{Root: workspace},
		OpenCode: OpenCodeConfig{
			Config:     map[string]any{"model": "anthropic/claude-sonnet-4"},
			Permission: "allow",
		},
	}
	client := OpenCodeClient{Config: cfg}
	if err := client.writeOpenCodeConfig(workspace); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{filepath.Join(".opencode", "symphony-config.json"), "opencode.json"} {
		data, err := os.ReadFile(filepath.Join(workspace, rel))
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["model"] != "anthropic/claude-sonnet-4" || payload["permission"] != "allow" {
			t.Fatalf("%s payload = %#v", rel, payload)
		}
	}

	if err := os.WriteFile(filepath.Join(workspace, "opencode.json"), []byte(`{"model":"repo/model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.OpenCode.Config = map[string]any{"model": "workflow/model"}
	client = OpenCodeClient{Config: cfg}
	if err := client.writeOpenCodeConfig(workspace); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"model":"repo/model"}` {
		t.Fatalf("existing opencode.json was overwritten: %s", data)
	}
}
