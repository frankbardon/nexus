package scene

import (
	"testing"
)

func TestMemoryStore_CreateGetPatchDelete(t *testing.T) {
	s := NewMemoryStore()
	h, err := s.Create("sess1", "chart.vega", map[string]any{"k": 1}, "agent-1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if h.Version != 1 {
		t.Errorf("initial version = %d", h.Version)
	}

	sc, err := s.Get("sess1", h.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := sc.Content.(map[string]any)["k"]; got != 1 {
		t.Errorf("k = %v", got)
	}
	if len(sc.History) != 1 || !sc.History[0].Initial {
		t.Errorf("history not seeded: %+v", sc.History)
	}

	h2, err := s.Patch("sess1", h.ID, map[string]any{"k": 2, "extra": "added"}, "agent-2")
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if h2.Version != 2 {
		t.Errorf("patched version = %d", h2.Version)
	}

	sc2, _ := s.Get("sess1", h.ID)
	if got := sc2.Content.(map[string]any)["k"]; got != 2 {
		t.Errorf("k after patch = %v", got)
	}
	if got := sc2.Content.(map[string]any)["extra"]; got != "added" {
		t.Errorf("extra = %v", got)
	}
	if len(sc2.History) != 2 {
		t.Errorf("history len after patch = %d", len(sc2.History))
	}

	if err := s.Delete("sess1", h.ID); err != nil {
		t.Errorf("delete: %v", err)
	}
	if _, err := s.Get("sess1", h.ID); err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestMemoryStore_ListPerSession(t *testing.T) {
	s := NewMemoryStore()
	a, _ := s.Create("session-a", "x", nil, "")
	b, _ := s.Create("session-a", "x", nil, "")
	_, _ = s.Create("session-b", "x", nil, "")

	got := s.List("session-a")
	if len(got) != 2 {
		t.Errorf("session-a list len = %d, want 2", len(got))
	}
	ids := map[string]bool{a.ID: false, b.ID: false}
	for _, h := range got {
		ids[h.ID] = true
	}
	for id, seen := range ids {
		if !seen {
			t.Errorf("missing %q from list", id)
		}
	}
}

func TestShallowMerge_NonMapReplaces(t *testing.T) {
	p := ShallowMerge{}
	if got := p.Apply("old", "new"); got != "new" {
		t.Errorf("string replace = %v", got)
	}
	if got := p.Apply(map[string]any{"a": 1}, "scalar"); got != "scalar" {
		t.Errorf("map vs scalar replaces with scalar: %v", got)
	}
}
