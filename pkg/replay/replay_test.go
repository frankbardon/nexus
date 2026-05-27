package replay

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/journal"
)

func writeHeader(t *testing.T, dir string) {
	t.Helper()
	h := journal.Header{
		SchemaVersion: journal.SchemaVersion,
		CreatedAt:     time.Now(),
		FsyncMode:     "none",
	}
	data, _ := json.MarshalIndent(h, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "header.json"), data, 0o644); err != nil {
		t.Fatalf("write header: %v", err)
	}
}

func appendEnv(t *testing.T, dir string, envs ...journal.Envelope) {
	t.Helper()
	path := filepath.Join(dir, "events.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	for _, e := range envs {
		line, _ := json.Marshal(e)
		f.Write(append(line, '\n'))
	}
}

func TestReplay_Session_BuildsEventList(t *testing.T) {
	root := t.TempDir()
	sessionID := "sess-1"
	journalDir := filepath.Join(root, sessionID, "journal")
	if err := os.MkdirAll(journalDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeHeader(t, journalDir)

	appendEnv(t, journalDir,
		journal.Envelope{Seq: 1, Type: "io.session.start", AgentID: ""},
		journal.Envelope{Seq: 2, ParentSeq: 1, ParentID: "p1", Type: "llm.request", AgentID: "root"},
		journal.Envelope{Seq: 3, ParentSeq: 2, ParentID: "p2", Type: "llm.response", AgentID: "root"},
		journal.Envelope{Seq: 4, ParentSeq: 3, Type: "delegate.start", AgentID: "delegate/analyst/abc"},
		journal.Envelope{Seq: 5, ParentSeq: 4, Type: "llm.request", AgentID: "delegate/analyst/abc", Depth: 1},
	)

	rep, err := Session(context.Background(), sessionID, Options{SessionsRoot: root})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if len(rep.Events) != 5 {
		t.Errorf("Events len = %d, want 5", len(rep.Events))
	}
	if rep.LastSeq != 5 {
		t.Errorf("LastSeq = %d", rep.LastSeq)
	}

	roots := rep.Roots()
	if len(roots) != 1 || roots[0].Type != "io.session.start" {
		t.Errorf("Roots = %+v", roots)
	}
	children := rep.Children(3)
	if len(children) != 1 || children[0].Type != "delegate.start" {
		t.Errorf("Children(3) = %+v", children)
	}
	byAgent := rep.ByAgent("delegate/analyst/abc")
	if len(byAgent) != 2 {
		t.Errorf("ByAgent len = %d", len(byAgent))
	}
}

func TestReplay_SessionAt_StopsAtSeq(t *testing.T) {
	root := t.TempDir()
	sessionID := "sess-2"
	journalDir := filepath.Join(root, sessionID, "journal")
	_ = os.MkdirAll(journalDir, 0o755)
	writeHeader(t, journalDir)
	appendEnv(t, journalDir,
		journal.Envelope{Seq: 1, Type: "a"},
		journal.Envelope{Seq: 2, Type: "b"},
		journal.Envelope{Seq: 3, Type: "c"},
	)
	rep, err := SessionAt(context.Background(), sessionID, 2, Options{SessionsRoot: root})
	if err != nil {
		t.Fatalf("at: %v", err)
	}
	if len(rep.Events) != 2 {
		t.Errorf("events = %d", len(rep.Events))
	}
}

func TestReplay_ScenesReconstructed(t *testing.T) {
	root := t.TempDir()
	sessionID := "sess-3"
	journalDir := filepath.Join(root, sessionID, "journal")
	_ = os.MkdirAll(journalDir, 0o755)
	writeHeader(t, journalDir)

	scenesDir := filepath.Join(root, sessionID, "plugins", ScenePluginID)
	_ = os.MkdirAll(scenesDir, 0o755)
	scenesPath := filepath.Join(scenesDir, "scenes.jsonl")
	lines := []map[string]any{
		{"kind": "created", "scene_id": "scene_a", "schema": "doc.markdown", "version": 1, "patch": map[string]any{"text": "hi"}},
		{"kind": "patched", "scene_id": "scene_a", "schema": "doc.markdown", "version": 2, "patch": map[string]any{"text": "hello"}},
	}
	f, _ := os.Create(scenesPath)
	for _, l := range lines {
		data, _ := json.Marshal(l)
		f.Write(append(data, '\n'))
	}
	f.Close()

	rep, err := Session(context.Background(), sessionID, Options{SessionsRoot: root})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if len(rep.Scenes) != 1 {
		t.Fatalf("scenes len = %d", len(rep.Scenes))
	}
	got := rep.Scenes[0].Content.(map[string]any)
	if got["text"] != "hello" {
		t.Errorf("text = %v", got["text"])
	}
	if rep.Scenes[0].Handle.Version != 2 {
		t.Errorf("version = %d", rep.Scenes[0].Handle.Version)
	}
}
