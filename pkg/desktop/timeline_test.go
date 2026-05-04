package desktop

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/journal"
)

// makeTestShell builds a shell whose agent's config tells the engine to
// store sessions under tmp. We don't actually boot an engine here — the
// timeline binding methods only need to know where the journal lives.
func makeTestShell(t *testing.T, tmp string) *Shell {
	t.Helper()
	cfgYAML := []byte("core:\n  sessions:\n    root: " + tmp + "\n  log_level: error\nplugins:\n  active: []\n")
	s := &Shell{
		Agents: []Agent{{
			ID:         "test-agent",
			Name:       "Test Agent",
			ConfigYAML: cfgYAML,
		}},
		DataDir: tmp, // shell scratch dir; not used for journal here
	}
	s.agents = map[string]*agentState{
		"test-agent": {status: AgentStatusIdle},
	}
	return s
}

// seedJournal writes nEvents envelopes to tmp/<sessionID>/journal/.
func seedJournal(t *testing.T, tmp, sessionID string, nEvents int) {
	t.Helper()
	dir := filepath.Join(tmp, sessionID, "journal")
	w, err := journal.NewWriter(dir, journal.WriterOptions{
		FsyncMode:  journal.FsyncEveryEvent,
		BufferSize: 4,
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i := 1; i <= nEvents; i++ {
		w.Append(&journal.Envelope{
			Seq:  uint64(i),
			Ts:   time.Unix(int64(1700000000+i), 0).UTC(),
			Type: "test.event",
		})
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestInspectSession_ReturnsTimeline(t *testing.T) {
	tmp := t.TempDir()
	s := makeTestShell(t, tmp)
	seedJournal(t, tmp, "sess-1", 5)

	events, err := s.InspectSession("test-agent", "sess-1")
	if err != nil {
		t.Fatalf("InspectSession: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("len(events) = %d; want 5", len(events))
	}
	for i, e := range events {
		if e.Seq != uint64(i+1) {
			t.Fatalf("events[%d].Seq = %d; want %d", i, e.Seq, i+1)
		}
		if e.Type != "test.event" {
			t.Fatalf("events[%d].Type = %q; want test.event", i, e.Type)
		}
	}
}

func TestInspectSession_EmptyOnMissingDir(t *testing.T) {
	tmp := t.TempDir()
	s := makeTestShell(t, tmp)

	events, err := s.InspectSession("test-agent", "nonexistent")
	if err != nil {
		t.Fatalf("InspectSession (missing): %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("len(events) = %d; want 0 for missing session", len(events))
	}
}

func TestGetSessionEvent_ReturnsPayload(t *testing.T) {
	tmp := t.TempDir()
	s := makeTestShell(t, tmp)
	seedJournal(t, tmp, "sess-1", 3)

	detail, err := s.GetSessionEvent("test-agent", "sess-1", 2)
	if err != nil {
		t.Fatalf("GetSessionEvent: %v", err)
	}
	if detail.Seq != 2 {
		t.Fatalf("detail.Seq = %d; want 2", detail.Seq)
	}
	if detail.Type != "test.event" {
		t.Fatalf("detail.Type = %q; want test.event", detail.Type)
	}
}

func TestGetSessionEvent_NotFound(t *testing.T) {
	tmp := t.TempDir()
	s := makeTestShell(t, tmp)
	seedJournal(t, tmp, "sess-1", 3)

	if _, err := s.GetSessionEvent("test-agent", "sess-1", 99); err == nil {
		t.Fatal("expected error for missing seq")
	}
}

func TestRewindSession_TruncatesAndArchives(t *testing.T) {
	tmp := t.TempDir()
	s := makeTestShell(t, tmp)
	seedJournal(t, tmp, "sess-1", 6)

	res, err := s.RewindSession("test-agent", "sess-1", 3)
	if err != nil {
		t.Fatalf("RewindSession: %v", err)
	}
	if res.TruncatedSeq != 3 {
		t.Fatalf("TruncatedSeq = %d; want 3", res.TruncatedSeq)
	}
	if res.EventsKept != 3 {
		t.Fatalf("EventsKept = %d; want 3", res.EventsKept)
	}
	if res.EventsArchived != 6 {
		t.Fatalf("EventsArchived = %d; want 6", res.EventsArchived)
	}
	if res.ArchiveName == "" {
		t.Fatal("expected non-empty ArchiveName")
	}

	// Live journal should now hold only 3 events.
	events, err := s.InspectSession("test-agent", "sess-1")
	if err != nil {
		t.Fatalf("InspectSession after rewind: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("post-rewind events = %d; want 3", len(events))
	}

	// Archive should be discoverable.
	archives, err := s.ListSessionArchives("test-agent", "sess-1")
	if err != nil {
		t.Fatalf("ListSessionArchives: %v", err)
	}
	if len(archives) != 1 || archives[0] != res.ArchiveName {
		t.Fatalf("archives = %v; want [%s]", archives, res.ArchiveName)
	}
}

func TestRestoreSession_ReversesRewind(t *testing.T) {
	tmp := t.TempDir()
	s := makeTestShell(t, tmp)
	seedJournal(t, tmp, "sess-1", 6)

	res, err := s.RewindSession("test-agent", "sess-1", 3)
	if err != nil {
		t.Fatalf("RewindSession: %v", err)
	}

	if err := s.RestoreSession("test-agent", "sess-1", res.ArchiveName); err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}

	events, err := s.InspectSession("test-agent", "sess-1")
	if err != nil {
		t.Fatalf("InspectSession after restore: %v", err)
	}
	if len(events) != 6 {
		t.Fatalf("post-restore events = %d; want 6", len(events))
	}

	// Restoring should produce a second archive (the pre-restore rotate).
	archives, err := s.ListSessionArchives("test-agent", "sess-1")
	if err != nil {
		t.Fatalf("ListSessionArchives after restore: %v", err)
	}
	if len(archives) < 2 {
		t.Fatalf("post-restore archives = %d; want >= 2", len(archives))
	}
}

func TestRewindSession_RefusesActiveSession(t *testing.T) {
	tmp := t.TempDir()
	s := makeTestShell(t, tmp)
	seedJournal(t, tmp, "sess-1", 4)

	// Mark sess-1 as the agent's active session by setting a non-nil
	// engine pointer and matching session id. The guard checks both.
	s.agents["test-agent"] = &agentState{
		status:    AgentStatusRunning,
		sessionID: "sess-1",
		eng:       &engine.Engine{}, // sentinel non-nil pointer
	}

	if _, err := s.RewindSession("test-agent", "sess-1", 2); err == nil {
		t.Fatal("expected error rewinding a live session")
	}
}

func TestListSessionArchives_EmptyForFreshSession(t *testing.T) {
	tmp := t.TempDir()
	s := makeTestShell(t, tmp)
	seedJournal(t, tmp, "sess-1", 2)

	archives, err := s.ListSessionArchives("test-agent", "sess-1")
	if err != nil {
		t.Fatalf("ListSessionArchives: %v", err)
	}
	if len(archives) != 0 {
		t.Fatalf("archives = %v; want empty", archives)
	}
}
