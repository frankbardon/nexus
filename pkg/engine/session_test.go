package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestNewSessionWorkspace_CreatesDirStructure(t *testing.T) {
	root := t.TempDir()

	ws, err := NewSessionWorkspace(root, nil)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}
	if ws.ID == "" {
		t.Fatal("expected non-empty session ID")
	}
	if ws.RootDir != filepath.Join(root, ws.ID) {
		t.Fatalf("RootDir = %q, want %q", ws.RootDir, filepath.Join(root, ws.ID))
	}

	for _, sub := range []string{"context", "files", "plugins", "metadata"} {
		full := filepath.Join(ws.RootDir, sub)
		info, err := os.Stat(full)
		if err != nil {
			t.Errorf("missing subdir %q: %v", sub, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%q exists but is not a directory", sub)
		}
	}
}

func TestNewSessionWorkspace_WritesInitialMetadata(t *testing.T) {
	root := t.TempDir()

	ws, err := NewSessionWorkspace(root, nil)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}

	metaPath := filepath.Join(ws.RootDir, "metadata", "session.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("metadata file missing: %v", err)
	}
	var meta SessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("metadata not valid JSON: %v", err)
	}
	if meta.ID != ws.ID {
		t.Errorf("meta.ID = %q, want %q", meta.ID, ws.ID)
	}
	if meta.Status != "active" {
		t.Errorf("meta.Status = %q, want %q", meta.Status, "active")
	}
	if meta.Labels == nil {
		t.Error("meta.Labels should be non-nil empty map, got nil")
	}
	if meta.StartedAt.IsZero() {
		t.Error("meta.StartedAt is zero")
	}
}

func TestLoadSessionWorkspace_ReopensExisting(t *testing.T) {
	root := t.TempDir()

	original, err := NewSessionWorkspace(root, nil)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}

	// Mutate metadata to simulate a closed session.
	meta, err := original.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	now := meta.StartedAt
	meta.EndedAt = &now
	meta.Status = "ended"
	if err := original.SaveMeta(meta); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}

	reloaded, err := LoadSessionWorkspace(root, original.ID, nil)
	if err != nil {
		t.Fatalf("LoadSessionWorkspace: %v", err)
	}
	if reloaded.ID != original.ID {
		t.Errorf("reloaded.ID = %q, want %q", reloaded.ID, original.ID)
	}
	if !reloaded.StartedAt.Equal(original.StartedAt) {
		t.Errorf("StartedAt mismatch: got %v, want %v", reloaded.StartedAt, original.StartedAt)
	}

	// Status must be reset to active and EndedAt cleared.
	persisted, err := reloaded.SessionMetadata()
	if err != nil {
		t.Fatalf("re-read metadata: %v", err)
	}
	if persisted.Status != "active" {
		t.Errorf("Status after reload = %q, want %q", persisted.Status, "active")
	}
	if persisted.EndedAt != nil {
		t.Errorf("EndedAt should be cleared, got %v", *persisted.EndedAt)
	}
}

func TestLoadSessionWorkspace_MissingDir(t *testing.T) {
	root := t.TempDir()
	if _, err := LoadSessionWorkspace(root, "does-not-exist", nil); err == nil {
		t.Fatal("expected error for missing session dir, got nil")
	}
}

func TestLoadSessionWorkspace_NotADir(t *testing.T) {
	root := t.TempDir()
	notDir := filepath.Join(root, "regular-file")
	if err := os.WriteFile(notDir, []byte("data"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if _, err := LoadSessionWorkspace(root, "regular-file", nil); err == nil {
		t.Fatal("expected error when session path is a file, got nil")
	}
}

func TestSessionWorkspace_WriteFile_EmitsCreatedAndUpdated(t *testing.T) {
	bus := NewEventBus()
	var (
		mu     sync.Mutex
		events []string
	)
	bus.Subscribe("session.file.created", func(e Event[any]) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e.Type)
	})
	bus.Subscribe("session.file.updated", func(e Event[any]) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e.Type)
	})

	ws, err := NewSessionWorkspace(t.TempDir(), bus)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}

	if err := ws.WriteFile("files/note.txt", []byte("hello")); err != nil {
		t.Fatalf("first WriteFile: %v", err)
	}
	if err := ws.WriteFile("files/note.txt", []byte("world")); err != nil {
		t.Fatalf("second WriteFile: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("expected 2 emissions, got %d (%v)", len(events), events)
	}
	if events[0] != "session.file.created" {
		t.Errorf("first emission = %q, want session.file.created", events[0])
	}
	if events[1] != "session.file.updated" {
		t.Errorf("second emission = %q, want session.file.updated", events[1])
	}
}

func TestSessionWorkspace_WriteFile_NilBusOK(t *testing.T) {
	ws, err := NewSessionWorkspace(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}
	// Must not panic with nil bus.
	if err := ws.WriteFile("files/x", []byte("y")); err != nil {
		t.Fatalf("WriteFile with nil bus: %v", err)
	}
}

func TestSessionWorkspace_ReadFile_RoundTrip(t *testing.T) {
	ws, err := NewSessionWorkspace(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}

	want := []byte("payload")
	if err := ws.WriteFile("files/data.bin", want); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := ws.ReadFile("files/data.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("ReadFile = %q, want %q", got, want)
	}

	if _, err := ws.ReadFile("files/missing.bin"); err == nil {
		t.Fatal("expected error reading missing file")
	}
}

func TestSessionWorkspace_AppendFile(t *testing.T) {
	ws, err := NewSessionWorkspace(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}

	if err := ws.AppendFile("files/log.jsonl", []byte("a\n")); err != nil {
		t.Fatalf("AppendFile (new): %v", err)
	}
	if err := ws.AppendFile("files/log.jsonl", []byte("b\n")); err != nil {
		t.Fatalf("AppendFile (existing): %v", err)
	}

	got, err := ws.ReadFile("files/log.jsonl")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "a\nb\n" {
		t.Fatalf("AppendFile contents = %q, want %q", got, "a\nb\n")
	}
}

func TestSessionWorkspace_ListFiles(t *testing.T) {
	ws, err := NewSessionWorkspace(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}
	if err := ws.WriteFile("files/a.txt", []byte("1")); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if err := ws.WriteFile("files/b.txt", []byte("2")); err != nil {
		t.Fatalf("seed b: %v", err)
	}

	names, err := ws.ListFiles("files")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for _, want := range []string{"a.txt", "b.txt"} {
		if !got[want] {
			t.Errorf("ListFiles missing %q (got %v)", want, names)
		}
	}

	if _, err := ws.ListFiles("does-not-exist"); err == nil {
		t.Fatal("expected error listing missing dir")
	}
}

func TestSessionWorkspace_FileExists(t *testing.T) {
	ws, err := NewSessionWorkspace(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}
	if ws.FileExists("nope") {
		t.Error("FileExists returned true for missing path")
	}
	if err := ws.WriteFile("files/here.txt", []byte("x")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !ws.FileExists("files/here.txt") {
		t.Error("FileExists returned false for existing path")
	}
}

func TestSessionWorkspace_PluginDir_LazyCreate(t *testing.T) {
	ws, err := NewSessionWorkspace(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}

	dir := ws.PluginDir("nexus.test.lazy")
	want := filepath.Join(ws.RootDir, "plugins", "nexus.test.lazy")
	if dir != want {
		t.Errorf("PluginDir = %q, want %q", dir, want)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("PluginDir not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("PluginDir exists but is not a directory")
	}
}

func TestSessionWorkspace_SaveMeta_RoundTrip(t *testing.T) {
	ws, err := NewSessionWorkspace(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}

	meta, err := ws.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}

	meta.TurnCount = 7
	meta.TokensUsed = 1234
	meta.Profile = "test-profile"
	meta.Labels["env"] = "dev"
	if err := ws.SaveMeta(meta); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}

	got, err := ws.SessionMetadata()
	if err != nil {
		t.Fatalf("re-read SessionMetadata: %v", err)
	}
	if got.TurnCount != 7 {
		t.Errorf("TurnCount = %d, want 7", got.TurnCount)
	}
	if got.TokensUsed != 1234 {
		t.Errorf("TokensUsed = %d, want 1234", got.TokensUsed)
	}
	if got.Profile != "test-profile" {
		t.Errorf("Profile = %q, want test-profile", got.Profile)
	}
	if got.Labels["env"] != "dev" {
		t.Errorf("Labels[env] = %q, want dev", got.Labels["env"])
	}
}

func TestSessionWorkspace_SubdirAccessors(t *testing.T) {
	ws, err := NewSessionWorkspace(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}
	cases := map[string]string{
		"context":  ws.ContextDir(),
		"files":    ws.FilesDir(),
		"blobs":    ws.BlobsDir(),
		"metadata": ws.MetadataDir(),
	}
	for sub, got := range cases {
		want := filepath.Join(ws.RootDir, sub)
		if got != want {
			t.Errorf("%sDir() = %q, want %q", sub, got, want)
		}
	}
}

func TestSessionConfigSnapshotPath_MissingSnapshot(t *testing.T) {
	// Empty configPath uses DefaultConfig().Core.Sessions.Root which expands
	// under the user's home. We assert only the error path with a session ID
	// that cannot exist there.
	_, err := SessionConfigSnapshotPath("", "definitely-not-a-real-session-id-12345")
	if err == nil {
		t.Fatal("expected error for missing snapshot, got nil")
	}
}
