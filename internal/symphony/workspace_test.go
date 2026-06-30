package symphony

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkspaceCreateReuseAndHooks(t *testing.T) {
	root := t.TempDir()
	cfg := RuntimeConfig{
		Workspace: WorkspaceConfig{Root: root},
		Hooks: HooksConfig{
			AfterCreate: `printf created > marker`,
			Timeout:     time.Second,
		},
	}
	manager := WorkspaceManager{Config: cfg}
	ws, err := manager.CreateForIssue(context.Background(), "#12/A")
	if err != nil {
		t.Fatal(err)
	}
	if ws.WorkspaceKey != "_12_A" {
		t.Fatalf("workspace key = %q", ws.WorkspaceKey)
	}
	if !ws.CreatedNow {
		t.Fatalf("expected new workspace")
	}
	data, err := os.ReadFile(filepath.Join(ws.Path, "marker"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "created" {
		t.Fatalf("after_create hook did not write marker")
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "marker"), []byte("reused"), 0o600); err != nil {
		t.Fatal(err)
	}
	reused, err := manager.CreateForIssue(context.Background(), "#12/A")
	if err != nil {
		t.Fatal(err)
	}
	if reused.CreatedNow {
		t.Fatalf("existing workspace should be reused")
	}
	data, err = os.ReadFile(filepath.Join(ws.Path, "marker"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "reused" {
		t.Fatalf("after_create ran on reused workspace")
	}
}

func TestWorkspaceContainment(t *testing.T) {
	root := t.TempDir()
	if err := EnsureInsideRoot(root, filepath.Join(root, "child")); err != nil {
		t.Fatalf("valid child rejected: %v", err)
	}
	if err := EnsureInsideRoot(root, filepath.Dir(root)); err == nil {
		t.Fatalf("outside path accepted")
	}
	if err := ValidateAgentCWD(filepath.Join(root, "child"), filepath.Join(root, "other")); err == nil {
		t.Fatalf("mismatched cwd accepted")
	}
}
