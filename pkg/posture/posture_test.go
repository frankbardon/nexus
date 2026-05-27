package posture

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRegistry_RegisterGetList(t *testing.T) {
	r := NewRegistry()
	p := AgentPosture{
		Name:         "analyst",
		SystemPrompt: "Be analytical.",
		AllowedTools: []string{"web_search"},
	}
	if err := r.Register(p); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := r.Get("analyst")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Version == "" {
		t.Errorf("Version not assigned")
	}
	if got.SystemPrompt != p.SystemPrompt {
		t.Errorf("SystemPrompt mismatch")
	}
	if l := r.List(); len(l) != 1 {
		t.Errorf("List len = %d, want 1", len(l))
	}
}

func TestRegistry_RegisterRejectsEmptyName(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(AgentPosture{}); err == nil {
		t.Errorf("expected error for empty name")
	}
}

func TestRegistry_VersionChangesOnContentChange(t *testing.T) {
	a := AgentPosture{Name: "x", SystemPrompt: "v1"}
	b := AgentPosture{Name: "x", SystemPrompt: "v2"}
	if HashPosture(a) == HashPosture(b) {
		t.Errorf("hash unchanged across content change")
	}
}

func TestRegistry_WatchEmitsAddedAndModified(t *testing.T) {
	r := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := r.Watch(ctx)

	_ = r.Register(AgentPosture{Name: "p", SystemPrompt: "v1"})
	select {
	case c := <-ch:
		if c.Kind != ChangeAdded || c.Name != "p" {
			t.Errorf("first event = %+v, want Added/p", c)
		}
	case <-time.After(time.Second):
		t.Fatal("no add event")
	}

	_ = r.Register(AgentPosture{Name: "p", SystemPrompt: "v2"})
	select {
	case c := <-ch:
		if c.Kind != ChangeModified {
			t.Errorf("expected Modified, got %v", c.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("no modify event")
	}

	_ = r.Remove("p")
	select {
	case c := <-ch:
		if c.Kind != ChangeRemoved {
			t.Errorf("expected Removed, got %v", c.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("no remove event")
	}
}

func TestLoadDir_ParsesYAML(t *testing.T) {
	dir := t.TempDir()
	yamlBody := []byte(`name: analyst
description: deep reader
system_prompt: think carefully
allowed_tools:
  - web_search
  - read_pdf
model:
  model_role: reasoning
default_budget:
  timeout: 30s
  max_tokens: 4000
  max_tool_calls: 5
`)
	if err := os.WriteFile(filepath.Join(dir, "analyst.yaml"), yamlBody, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	bad := []byte("not: [valid yaml")
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), bad, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	postures, errs := LoadDir(dir)
	if len(postures) != 1 {
		t.Fatalf("postures len = %d, want 1", len(postures))
	}
	if len(errs) != 1 {
		t.Errorf("errs len = %d, want 1 (for bad.yaml)", len(errs))
	}
	p := postures[0]
	if p.Name != "analyst" {
		t.Errorf("Name = %q", p.Name)
	}
	if p.DefaultBudget.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v", p.DefaultBudget.Timeout)
	}
	if len(p.AllowedTools) != 2 {
		t.Errorf("AllowedTools len = %d", len(p.AllowedTools))
	}
	if p.Version == "" {
		t.Errorf("Version not assigned by loader")
	}
}
