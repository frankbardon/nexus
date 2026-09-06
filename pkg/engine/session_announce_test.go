package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// collectAnnouncedFiles records the event type alongside the full payload.
// collectSessionFileDeltas (session_test.go) drops the type because the helper
// under test there decides it from a single documented rule; the announcement
// helpers decide it two different ways — sampled for a write, derived from the
// offset for an append — so the type is part of what is being asserted here.
func collectAnnouncedFiles(bus EventBus) func() []string {
	var (
		mu   sync.Mutex
		seen []string
	)
	record := func(e Event[any]) {
		data, _ := e.Payload.(map[string]any)
		path, _ := data["path"].(string)
		size, _ := data["size"].(int)
		offset, _ := data["offset"].(int)
		added, _ := data["bytes_added"].(int)
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, fmt.Sprintf("%s %s size=%d offset=%d added=%d",
			e.Type, path, size, offset, added))
	}
	bus.Subscribe("session.file.created", record)
	bus.Subscribe("session.file.updated", record)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func newAnnounceWorkspace(t *testing.T) (*SessionWorkspace, func() []string) {
	t.Helper()
	bus := NewEventBus()
	ws, err := NewSessionWorkspace(t.TempDir(), bus)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}
	return ws, collectAnnouncedFiles(bus)
}

// A raw writer's whole point is that it holds its own os.* call, so the helper
// has to derive the session-relative path itself — the caller only has an
// absolute one.
func TestAnnounceWrite_EmitsSessionRelativePath(t *testing.T) {
	ws, snapshot := newAnnounceWorkspace(t)

	full := filepath.Join(ws.PluginDir("nexus.scene"), "scenes.json")
	if err := os.WriteFile(full, []byte("[]"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ws.AnnounceWrite(full, false)

	// Overwrite: existed=true selects .updated, and the delta still reports the
	// whole object as new because a rewrite really does replace every byte.
	if err := os.WriteFile(full, []byte("[1,2]"), 0o644); err != nil {
		t.Fatalf("WriteFile overwrite: %v", err)
	}
	ws.AnnounceWrite(full, true)

	want := []string{
		"session.file.created plugins/nexus.scene/scenes.json size=2 offset=0 added=2",
		"session.file.updated plugins/nexus.scene/scenes.json size=5 offset=0 added=5",
	}
	got := snapshot()
	if len(got) != len(want) {
		t.Fatalf("emissions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("emission %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The append case derives created-vs-updated from the offset rather than
// sampling the filesystem, so a descriptor opened with O_CREATE at plugin Init
// still produces a "created" for the first real bytes written through it.
func TestAnnounceAppend_FirstAppendToEmptyFileReportsCreated(t *testing.T) {
	ws, snapshot := newAnnounceWorkspace(t)

	full := filepath.Join(ws.PluginDir("nexus.scene"), "scenes.jsonl")
	f, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	for _, line := range []string{"aaaa\n", "bb\n"} {
		n, wErr := f.Write([]byte(line))
		if wErr != nil {
			t.Fatalf("Write: %v", wErr)
		}
		ws.AnnounceAppend(full, n)
	}

	want := []string{
		"session.file.created plugins/nexus.scene/scenes.jsonl size=5 offset=0 added=5",
		"session.file.updated plugins/nexus.scene/scenes.jsonl size=8 offset=5 added=3",
	}
	got := snapshot()
	if len(got) != len(want) {
		t.Fatalf("emissions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("emission %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// Plugins whose output directory is configurable (the HITL registry, the
// sampler, long-term memory) point outside every session tree by default. The
// helper swallowing those is what lets such a plugin announce unconditionally
// instead of repeating an escape check at each call site.
func TestAnnounce_PathOutsideSessionRootIsSilent(t *testing.T) {
	ws, snapshot := newAnnounceWorkspace(t)

	outside := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(outside, []byte("{}"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ws.AnnounceWrite(outside, false)
	ws.AnnounceAppend(outside, 2)

	// A sibling of the session root whose name merely shares its prefix must
	// not be mistaken for a child — a plain string-prefix check would.
	sibling := ws.RootDir + "-backup"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	decoy := filepath.Join(sibling, "session.json")
	if err := os.WriteFile(decoy, []byte("{}"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ws.AnnounceWrite(decoy, false)

	if got := snapshot(); len(got) != 0 {
		t.Fatalf("emissions = %v, want none", got)
	}
}

// The two excluded-by-design writers must be unannounceable however a future
// caller wires the seam up, not merely un-called today.
func TestAnnounce_ExcludedPathsAreUnannounceable(t *testing.T) {
	ws, snapshot := newAnnounceWorkspace(t)

	lock := filepath.Join(ws.RootDir, sessionLockFilename)
	if err := os.WriteFile(lock, []byte("{}"), 0o644); err != nil {
		t.Fatalf("WriteFile lock: %v", err)
	}
	ws.AnnounceWrite(lock, false)

	for _, name := range []string{"store.db-wal", "store.db-shm", "store.db-journal"} {
		p := filepath.Join(ws.PluginDir("nexus.gate.token_budget"), name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
		ws.AnnounceWrite(p, false)
		ws.AnnounceAppend(p, 1)
	}

	if got := snapshot(); len(got) != 0 {
		t.Fatalf("emissions = %v, want none", got)
	}
}

// A workspace with no bus is the shape plugin unit tests build by hand
// (&SessionWorkspace{ID: "..."}). Announcing through one must be inert rather
// than a nil dereference, or adding an announcement breaks every such test.
func TestAnnounce_NoBusOrNoRootIsInert(t *testing.T) {
	for name, ws := range map[string]*SessionWorkspace{
		"nil workspace": nil,
		"no bus":        {ID: "sess-1", RootDir: t.TempDir()},
		"no root":       {ID: "sess-1", bus: NewEventBus()},
	} {
		t.Run(name, func(t *testing.T) {
			ws.AnnounceWrite(filepath.Join(t.TempDir(), "f.txt"), false)
			ws.AnnounceAppend(filepath.Join(t.TempDir(), "f.txt"), 1)
		})
	}
}

// A missing file reports size 0 rather than dropping the event, for the same
// reason AppendFile tolerates a failed Stat: a subscriber that re-reads the
// path is still correct with a wrong size and unrecoverably wrong with no
// event at all.
func TestAnnounceWrite_MissingFileStillAnnounces(t *testing.T) {
	ws, snapshot := newAnnounceWorkspace(t)

	ws.AnnounceWrite(filepath.Join(ws.FilesDir(), "vanished.txt"), false)

	want := "session.file.created files/vanished.txt size=0 offset=0 added=0"
	got := snapshot()
	if len(got) != 1 || got[0] != want {
		t.Fatalf("emissions = %v, want [%q]", got, want)
	}
}
