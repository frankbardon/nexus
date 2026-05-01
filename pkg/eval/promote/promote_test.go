package promote

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/journal"
)

// fakeSession constructs a synthetic ~/.nexus/sessions/<id>/ tree under
// rootDir for promote-side tests. The journal is hand-crafted via the real
// journal.Writer so the on-disk byte shape is exactly what Promote() reads
// from a real session.
//
// status governs the metadata.status field — flipping it to "failed" lets
// us drive the warning surfacing test without needing a full engine boot.
func fakeSession(t *testing.T, rootDir, sessionID, status string, extraEnvs []journal.Envelope) string {
	t.Helper()
	sessionDir := filepath.Join(rootDir, sessionID)
	if err := os.MkdirAll(filepath.Join(sessionDir, "metadata"), 0o755); err != nil {
		t.Fatalf("mkdir metadata: %v", err)
	}

	// 1) Journal: real writer so the header + active segment are byte-correct.
	journalDir := filepath.Join(sessionDir, "journal")
	w, err := journal.NewWriter(journalDir, journal.WriterOptions{
		FsyncMode: journal.FsyncNone,
		SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("journal.NewWriter: %v", err)
	}
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	envs := []journal.Envelope{
		{Seq: 1, Ts: t0, Type: "io.session.start", Payload: map[string]any{"session_id": sessionID}},
		{Seq: 2, Ts: t0.Add(10 * time.Millisecond), Type: "io.input", Payload: map[string]any{"Content": "first prompt"}},
		{Seq: 3, Ts: t0.Add(20 * time.Millisecond), Type: "agent.turn.start"},
		{Seq: 4, Ts: t0.Add(30 * time.Millisecond), Type: "llm.response", Payload: map[string]any{
			"Usage": map[string]any{"PromptTokens": 80, "CompletionTokens": 40},
		}},
		{Seq: 5, Ts: t0.Add(40 * time.Millisecond), Type: "tool.invoke", Payload: map[string]any{
			"Name": "read_file", "Arguments": map[string]any{"path": "main.go"},
		}},
		{Seq: 6, Ts: t0.Add(50 * time.Millisecond), Type: "tool.result", Payload: map[string]any{}},
		{Seq: 7, Ts: t0.Add(60 * time.Millisecond), Type: "llm.response", Payload: map[string]any{
			"Usage": map[string]any{"PromptTokens": 140, "CompletionTokens": 70},
		}},
		{Seq: 8, Ts: t0.Add(70 * time.Millisecond), Type: "agent.turn.end"},
	}
	envs = append(envs, extraEnvs...)
	for i := range envs {
		w.Append(&envs[i])
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("journal close: %v", err)
	}

	// 2) Metadata: minimal session.json. Status drives the warning path.
	meta := map[string]any{
		"id":         sessionID,
		"started_at": t0.Format(time.RFC3339Nano),
		"status":     status,
	}
	metaBytes, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(sessionDir, "metadata", "session.json"), metaBytes, 0o644); err != nil {
		t.Fatalf("write session.json: %v", err)
	}

	// 3) Config snapshot — a minimal mock-mode config the runner can boot.
	cfg := `core:
  log_level: warn
  models:
    default: mock
    mock:
      provider: nexus.llm.anthropic
      model: mock
      max_tokens: 1024
  sessions:
    root: ~/.nexus/test-sessions
journal:
  fsync: none
plugins:
  active:
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.memory.capped
    - nexus.tool.file
  nexus.llm.anthropic:
    api_key: "sk-mock-not-used"
  nexus.agent.react:
    system_prompt: "Test."
  nexus.memory.capped:
    persist: false
`
	if err := os.WriteFile(filepath.Join(sessionDir, "metadata", "config-snapshot.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config-snapshot.yaml: %v", err)
	}
	return sessionDir
}

// TestPromote_HappyPath: synthesize a fake session, promote it, and load
// the resulting case via evalcase. Loader-round-trip is the binding
// contract: if Promote produces a directory the loader chokes on, the
// runner can't replay the case.
func TestPromote_HappyPath(t *testing.T) {
	root := t.TempDir()
	sessionDir := fakeSession(t, root, "session-001", "completed", nil)

	casesDir := filepath.Join(t.TempDir(), "cases")
	res, err := Promote(context.Background(), PromoteOptions{
		SessionDir:  sessionDir,
		CaseID:      "promoted-happy",
		CasesDir:    casesDir,
		Owner:       "test@example.com",
		Tags:        []string{"smoke", "promoted"},
		Description: "round-trip smoke",
		OpenEditor:  false,
		Now:         time.Date(2026, 5, 1, 13, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}

	caseDir := filepath.Join(casesDir, "promoted-happy")
	if res.CaseDir != caseDir {
		t.Errorf("CaseDir = %q want %q", res.CaseDir, caseDir)
	}
	if res.InputCount != 1 {
		t.Errorf("InputCount = %d want 1", res.InputCount)
	}
	if res.EventCount != 8 {
		t.Errorf("EventCount = %d want 8", res.EventCount)
	}

	// Required artifacts exist.
	for _, sub := range []string{
		"case.yaml",
		"input/config.yaml",
		"input/inputs.yaml",
		"journal/header.json",
		"journal/events.jsonl",
		"assertions.yaml",
	} {
		path := filepath.Join(caseDir, sub)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing %s: %v", sub, err)
		}
	}

	// Cross-check on the synthesized case.yaml: name, owner, tags survive,
	// recorded_at lands in ISO 8601, and the description is the user's.
	caseYAML, err := os.ReadFile(filepath.Join(caseDir, "case.yaml"))
	if err != nil {
		t.Fatalf("read case.yaml: %v", err)
	}
	if !strings.Contains(string(caseYAML), "name: promoted-happy") {
		t.Errorf("case.yaml missing name:\n%s", caseYAML)
	}
	if !strings.Contains(string(caseYAML), "owner: test@example.com") {
		t.Errorf("case.yaml missing owner:\n%s", caseYAML)
	}
	if !strings.Contains(string(caseYAML), "model_baseline: mock") {
		t.Errorf("case.yaml missing model_baseline=mock:\n%s", caseYAML)
	}
	if !strings.Contains(string(caseYAML), "2026-05-01T13:00:00Z") {
		t.Errorf("case.yaml missing pinned recorded_at:\n%s", caseYAML)
	}

	// inputs.yaml carries the journaled input string.
	inputsYAMLBytes, _ := os.ReadFile(filepath.Join(caseDir, "input/inputs.yaml"))
	if !strings.Contains(string(inputsYAMLBytes), "first prompt") {
		t.Errorf("inputs.yaml missing scripted input:\n%s", inputsYAMLBytes)
	}

	// Config copied verbatim — the head of the file is the same.
	got, _ := os.ReadFile(filepath.Join(caseDir, "input/config.yaml"))
	if !strings.HasPrefix(string(got), "core:\n  log_level: warn") {
		t.Errorf("config.yaml not copied verbatim:\n%s", got)
	}

	// Loader round-trip.
	if err := loadCaseExternal(t, caseDir); err != nil {
		t.Fatalf("evalcase.Load round-trip: %v", err)
	}

	// No warnings on the happy path.
	if len(res.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", res.Warnings)
	}
}

// TestPromote_FailedSessionWarns confirms the warning path triggers when
// the source session ended with status=failed.
func TestPromote_FailedSessionWarns(t *testing.T) {
	root := t.TempDir()
	sessionDir := fakeSession(t, root, "session-fail", "failed", nil)

	casesDir := filepath.Join(t.TempDir(), "cases")
	res, err := Promote(context.Background(), PromoteOptions{
		SessionDir: sessionDir,
		CaseID:     "promoted-fail",
		CasesDir:   casesDir,
		OpenEditor: false,
		Now:        time.Now(),
	})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}

	if !anyWarning(res.Warnings, "status=\"failed\"") {
		t.Errorf("expected status=failed warning, got %v", res.Warnings)
	}
}

// TestPromote_NonReplayableWarns lights up when the journal carries a
// fallback-style failure event the replay path will short-circuit silently.
func TestPromote_NonReplayableWarns(t *testing.T) {
	root := t.TempDir()
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	extra := []journal.Envelope{
		{Seq: 100, Ts: t0.Add(80 * time.Millisecond), Type: "provider.fallback.error", Payload: map[string]any{"provider": "openai"}},
		{Seq: 101, Ts: t0.Add(90 * time.Millisecond), Type: "provider.fallback.advance", Payload: map[string]any{"to": "anthropic"}},
	}
	sessionDir := fakeSession(t, root, "session-fb", "completed", extra)

	casesDir := filepath.Join(t.TempDir(), "cases")
	res, err := Promote(context.Background(), PromoteOptions{
		SessionDir: sessionDir,
		CaseID:     "promoted-fb",
		CasesDir:   casesDir,
		OpenEditor: false,
		Now:        time.Now(),
	})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if !anyWarning(res.Warnings, "non-replayable") {
		t.Errorf("expected non-replayable warning, got %v", res.Warnings)
	}
}

// TestPromote_ForceOverwrite confirms Force re-uses an existing case dir
// without leaving stale artifacts behind.
func TestPromote_ForceOverwrite(t *testing.T) {
	root := t.TempDir()
	sessionDir := fakeSession(t, root, "session-002", "completed", nil)

	casesDir := filepath.Join(t.TempDir(), "cases")
	caseDir := filepath.Join(casesDir, "promoted-force")
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(caseDir, "stale.txt"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("setup stale: %v", err)
	}

	if _, err := Promote(context.Background(), PromoteOptions{
		SessionDir: sessionDir,
		CaseID:     "promoted-force",
		CasesDir:   casesDir,
		OpenEditor: false,
	}); err == nil {
		t.Fatalf("expected error on existing dir without --force")
	}
	if _, err := os.Stat(filepath.Join(caseDir, "stale.txt")); err != nil {
		t.Errorf("non-force path unexpectedly removed stale.txt: %v", err)
	}

	res, err := Promote(context.Background(), PromoteOptions{
		SessionDir: sessionDir,
		CaseID:     "promoted-force",
		CasesDir:   casesDir,
		OpenEditor: false,
		Force:      true,
		Now:        time.Now(),
	})
	if err != nil {
		t.Fatalf("Promote (force): %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.CaseDir, "stale.txt")); err == nil {
		t.Errorf("force did not remove stale.txt under %s", res.CaseDir)
	}
}

// TestPromote_MissingSession returns an error and does not create a case dir.
func TestPromote_MissingSession(t *testing.T) {
	casesDir := filepath.Join(t.TempDir(), "cases")
	if _, err := Promote(context.Background(), PromoteOptions{
		SessionDir: "/no/such/path",
		CaseID:     "promoted-nope",
		CasesDir:   casesDir,
		OpenEditor: false,
	}); err == nil {
		t.Fatalf("expected error for missing session dir")
	}
	if _, err := os.Stat(filepath.Join(casesDir, "promoted-nope")); !os.IsNotExist(err) {
		t.Errorf("partial case dir survived a missing-session error: %v", err)
	}
}

// TestPromote_RecordedAtPopulated checks the timestamp lands in case.yaml
// even when no Now override is provided. Keeps the docs honest about what
// Promote writes.
func TestPromote_RecordedAtPopulated(t *testing.T) {
	root := t.TempDir()
	sessionDir := fakeSession(t, root, "session-ts", "completed", nil)

	casesDir := filepath.Join(t.TempDir(), "cases")
	res, err := Promote(context.Background(), PromoteOptions{
		SessionDir: sessionDir,
		CaseID:     "promoted-ts",
		CasesDir:   casesDir,
		OpenEditor: false,
	})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	caseYAML, _ := os.ReadFile(filepath.Join(res.CaseDir, "case.yaml"))
	if !strings.Contains(string(caseYAML), "recorded_at:") {
		t.Errorf("recorded_at missing:\n%s", caseYAML)
	}
}

func anyWarning(warnings []string, needle string) bool {
	for _, w := range warnings {
		if strings.Contains(w, needle) {
			return true
		}
	}
	return false
}
