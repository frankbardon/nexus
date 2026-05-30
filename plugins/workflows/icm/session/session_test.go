package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"
)

// ---------------------------------------------------------------------------
// constructor
// ---------------------------------------------------------------------------

func TestNewSession_CreatesRoot(t *testing.T) {
	dataDir := t.TempDir()
	s, err := NewSession(dataDir, "run_abc", nil)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if s.RunID != "run_abc" {
		t.Errorf("expected RunID=run_abc, got %q", s.RunID)
	}
	expectedRoot := filepath.Join(dataDir, "run_abc")
	if s.RootDir != expectedRoot {
		t.Errorf("expected RootDir=%q, got %q", expectedRoot, s.RootDir)
	}
	if _, err := os.Stat(filepath.Join(s.RootDir, ".icm")); err != nil {
		t.Errorf(".icm directory not created: %v", err)
	}
	if s.StartedAt.IsZero() {
		t.Errorf("StartedAt not set")
	}
}

func TestNewSession_RequiresRunID(t *testing.T) {
	if _, err := NewSession(t.TempDir(), "", nil); err == nil {
		t.Error("expected error for empty run ID")
	}
}

func TestNewSession_RequiresDataDir(t *testing.T) {
	if _, err := NewSession("", "run_x", nil); err == nil {
		t.Error("expected error for empty data dir")
	}
}

func TestOpenSession_RoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := NewSession(dataDir, "run_open", nil); err != nil {
		t.Fatal(err)
	}
	got, err := OpenSession(dataDir, "run_open", nil)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if got.RunID != "run_open" {
		t.Errorf("expected RunID=run_open, got %q", got.RunID)
	}
}

func TestOpenSession_MissingErrors(t *testing.T) {
	if _, err := OpenSession(t.TempDir(), "no_such_run", nil); err == nil {
		t.Error("expected error opening non-existent session")
	}
}

// ---------------------------------------------------------------------------
// path resolution
// ---------------------------------------------------------------------------

func TestSession_PathLayouts(t *testing.T) {
	s, _ := NewSession(t.TempDir(), "run_x", nil)

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"stage dir", s.StageDir("01_research"), "01_research"},
		{"plain artifact", s.ArtifactPath("01_research", "out.md"), "01_research/out.md"},
		{"iteration dir", s.IterationDir("02_draft", 3), "02_draft/iter_03"},
		{"iteration artifact", s.IterationArtifactPath("02_draft", "draft.md", 2), "02_draft/iter_02/draft.md"},
		{"item dir", s.ItemDir("03_per", "topic_a"), "03_per/items/topic_a"},
		{"item artifact", s.ItemArtifactPath("03_per", "topic_a", "out.md"), "03_per/items/topic_a/out.md"},
		{"item iter artifact", s.ItemIterationArtifactPath("03_per", "topic_a", "out.md", 2), "03_per/items/topic_a/iter_02/out.md"},
		{"aggregate path", s.AggregatePath("03_per", "all.json"), "03_per/all.json"},
	}
	for _, c := range cases {
		rel := strings.TrimPrefix(c.got, s.RootDir+string(filepath.Separator))
		if rel != c.want {
			t.Errorf("%s: expected %q, got %q", c.name, c.want, rel)
		}
	}
}

func TestSession_SidecarPath(t *testing.T) {
	got := SidecarPath(filepath.Join("/some", "path", "artifact.md"))
	want := filepath.Join("/some", "path", "artifact.md") + ".icm.json"
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

// ---------------------------------------------------------------------------
// artifact write / sidecar
// ---------------------------------------------------------------------------

func TestSession_WriteArtifactCreatesParents(t *testing.T) {
	s, _ := NewSession(t.TempDir(), "run_y", nil)
	path := s.IterationArtifactPath("02_draft", "draft.md", 1)
	if err := s.WriteArtifact(path, []byte("draft v1\n")); err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != "draft v1\n" {
		t.Errorf("content mismatch: %q", string(data))
	}
	// Temp file should not linger.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected .tmp removed after rename, stat err=%v", err)
	}
}

func TestSession_WriteSidecar(t *testing.T) {
	s, _ := NewSession(t.TempDir(), "run_z", nil)
	artPath := s.ArtifactPath("01_research", "research.md")
	if err := s.WriteArtifact(artPath, []byte("findings\n")); err != nil {
		t.Fatal(err)
	}
	meta := ArtifactMeta{
		StageID:           "01_research",
		IterationsRun:     5,
		ConvergenceFailed: true,
		UnmetConditions: []icmtypes.ConditionResult{
			{Type: "llm", Name: "approved", Verdict: "fail", Feedback: "needs more depth"},
		},
		Delegate: DelegateMeta{
			PostureName: "research_role",
			TokensUsed:  1234,
			ElapsedMS:   789,
		},
	}
	if err := s.WriteSidecar(artPath, meta); err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	data, err := os.ReadFile(SidecarPath(artPath))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var back ArtifactMeta
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal sidecar: %v", err)
	}
	if !back.ConvergenceFailed {
		t.Error("convergence_failed not roundtripped")
	}
	if len(back.UnmetConditions) != 1 {
		t.Fatalf("expected 1 unmet condition, got %d", len(back.UnmetConditions))
	}
	if back.UnmetConditions[0].Feedback != "needs more depth" {
		t.Errorf("feedback mismatch: %q", back.UnmetConditions[0].Feedback)
	}
	if back.Delegate.PostureName != "research_role" {
		t.Errorf("delegate posture not roundtripped: %q", back.Delegate.PostureName)
	}
	if back.WrittenAt.IsZero() {
		t.Errorf("WrittenAt should auto-stamp on save")
	}
}

func TestSession_WriteRunMeta(t *testing.T) {
	s, _ := NewSession(t.TempDir(), "run_meta", nil)
	meta := RunMeta{
		RunID:          "run_meta",
		InstanceID:     "nexus.workflows.icm/script",
		WorkspaceRoot:  "/tmp/workspace",
		WorkspaceName:  "script",
		StartedAt:      s.StartedAt,
		ConfigSnapshot: map[string]any{"loop_max_restarts": 3},
	}
	if err := s.WriteRunMeta(meta); err != nil {
		t.Fatalf("WriteRunMeta: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(s.RootDir, ".icm", "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var back RunMeta
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal run.json: %v", err)
	}
	if back.InstanceID != "nexus.workflows.icm/script" {
		t.Errorf("instance id mismatch: %q", back.InstanceID)
	}
	if back.WorkspaceName != "script" {
		t.Errorf("workspace name mismatch: %q", back.WorkspaceName)
	}
}

// ---------------------------------------------------------------------------
// logical ref resolution
// ---------------------------------------------------------------------------

func TestSession_ResolveLogicalRef_Plain(t *testing.T) {
	s, _ := NewSession(t.TempDir(), "run_q", nil)
	target := s.ArtifactPath("01_research", "out.md")
	if err := s.WriteArtifact(target, []byte("data")); err != nil {
		t.Fatal(err)
	}
	got, err := s.ResolveLogicalRef("01_research/out.md")
	if err != nil {
		t.Fatalf("ResolveLogicalRef: %v", err)
	}
	if got != target {
		t.Errorf("expected %q, got %q", target, got)
	}
}

func TestSession_ResolveLogicalRef_LatestIteration(t *testing.T) {
	s, _ := NewSession(t.TempDir(), "run_q", nil)
	if err := s.WriteArtifact(s.IterationArtifactPath("02_draft", "draft.md", 1), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteArtifact(s.IterationArtifactPath("02_draft", "draft.md", 3), []byte("v3")); err != nil {
		t.Fatal(err)
	}
	got, err := s.ResolveLogicalRef("02_draft/draft.md")
	if err != nil {
		t.Fatalf("ResolveLogicalRef: %v", err)
	}
	if !strings.Contains(got, "iter_03") {
		t.Errorf("expected latest iter_03, got %q", got)
	}
}

func TestSession_ResolveLogicalRef_PlainBeatsIter(t *testing.T) {
	s, _ := NewSession(t.TempDir(), "run_q", nil)
	if err := s.WriteArtifact(s.IterationArtifactPath("02_draft", "draft.md", 1), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	plain := s.ArtifactPath("02_draft", "draft.md")
	if err := s.WriteArtifact(plain, []byte("promoted")); err != nil {
		t.Fatal(err)
	}
	got, err := s.ResolveLogicalRef("02_draft/draft.md")
	if err != nil {
		t.Fatalf("ResolveLogicalRef: %v", err)
	}
	if got != plain {
		t.Errorf("expected plain path %q, got %q", plain, got)
	}
}

func TestSession_ResolveLogicalRef_NotFound(t *testing.T) {
	s, _ := NewSession(t.TempDir(), "run_q", nil)
	if _, err := s.ResolveLogicalRef("99_phantom/missing.md"); err == nil {
		t.Error("expected error for nonexistent ref")
	}
}

func TestSession_ResolveLogicalRef_BadShape(t *testing.T) {
	s, _ := NewSession(t.TempDir(), "run_q", nil)
	cases := []string{"", "no_slash", "/leading", "trailing/", "//empty"}
	for _, c := range cases {
		if _, err := s.ResolveLogicalRef(c); err == nil {
			t.Errorf("expected error for malformed ref %q", c)
		}
	}
}

// TestSession_ResolveLogicalRef_NaturalNumberSort proves the handoff bug
// (lexicographic iter sort which would rank iter_9 above iter_10) is
// fixed. With iter_9, iter_10, iter_99, iter_100 present,
// ResolveLogicalRef must pick iter_100.
func TestSession_ResolveLogicalRef_NaturalNumberSort(t *testing.T) {
	s, _ := NewSession(t.TempDir(), "run_q", nil)
	stage := "02_draft"
	// Write in non-natural order so any accidental insertion-order win
	// doesn't mask the sort bug.
	for _, i := range []int{9, 100, 10, 99} {
		// Bypass IterationDir's zero-pad to produce mixed widths
		// iter_9 (1 digit) ... iter_100 (3 digits).
		name := fmt.Sprintf("iter_%d", i)
		dir := filepath.Join(s.StageDir(stage), name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := s.WriteArtifact(filepath.Join(dir, "draft.md"), []byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ResolveLogicalRef(stage + "/draft.md")
	if err != nil {
		t.Fatalf("ResolveLogicalRef: %v", err)
	}
	if !strings.Contains(got, "iter_100") {
		t.Errorf("expected iter_100 to win natural sort, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// initial input copying
// ---------------------------------------------------------------------------

func TestSession_CopyInitialInputsFromDir(t *testing.T) {
	root := t.TempDir()
	inputsDir := filepath.Join(root, "workspace_inputs")
	if err := os.MkdirAll(inputsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inputsDir, "brief.md"), []byte("the brief"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, _ := NewSession(t.TempDir(), "run_w", nil)
	if err := s.CopyInitialInputsFromDir(inputsDir); err != nil {
		t.Fatalf("CopyInitialInputsFromDir: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(s.StageDir(reservedInputStage), "brief.md"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "the brief" {
		t.Errorf("content mismatch: %q", string(got))
	}
}

func TestSession_CopyInitialInputs_MissingDirIsOK(t *testing.T) {
	s, _ := NewSession(t.TempDir(), "run_w", nil)
	if err := s.CopyInitialInputsFromDir(filepath.Join(t.TempDir(), "doesnotexist")); err != nil {
		t.Errorf("expected nil for missing inputs dir, got: %v", err)
	}
}

func TestSession_CopyInitialInputs_Explicit(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "brief.md")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := NewSession(t.TempDir(), "run_ex", nil)
	if err := s.CopyInitialInputs([]string{src}); err != nil {
		t.Fatalf("CopyInitialInputs: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(s.StageDir(reservedInputStage), "brief.md"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("content mismatch: %q", string(got))
	}
}

// ---------------------------------------------------------------------------
// state.json
// ---------------------------------------------------------------------------

func TestSession_StateRoundtrip(t *testing.T) {
	s, _ := NewSession(t.TempDir(), "run_s", nil)
	st, err := s.LoadState()
	if err != nil {
		t.Fatalf("LoadState (initial): %v", err)
	}
	if st.RunID != "run_s" {
		t.Errorf("expected RunID=run_s, got %q", st.RunID)
	}
	if st.StartedAt.IsZero() {
		t.Errorf("zero-value state should carry StartedAt from session")
	}

	st.Stages = []StageState{
		{ID: "01_a", Status: StageStatusDone},
		{ID: "02_b", Status: StageStatusRunning, Iterations: []IterationState{
			{Index: 1, Status: StageStatusRunning, ExitResults: []icmtypes.ConditionResult{
				{Type: "llm", Verdict: "fail", Feedback: "not yet"},
			}},
		}},
	}
	st.CurrentStage = 1
	st.Outcome = OutcomeRunning
	st.Verifiers = map[string]VerifierState{
		"v1": {Status: StageStatusDone, Verdict: "pass"},
	}
	if err := s.SaveState(st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	back, err := s.LoadState()
	if err != nil {
		t.Fatalf("LoadState (back): %v", err)
	}
	if len(back.Stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(back.Stages))
	}
	if back.Stages[0].Status != StageStatusDone {
		t.Errorf("expected stage 0 done, got %q", back.Stages[0].Status)
	}
	if back.Stages[1].Iterations[0].ExitResults[0].Feedback != "not yet" {
		t.Errorf("iteration exit result not roundtripped: %+v", back.Stages[1].Iterations[0].ExitResults)
	}
	if back.CurrentStage != 1 {
		t.Errorf("expected CurrentStage=1, got %d", back.CurrentStage)
	}
	if back.Outcome != OutcomeRunning {
		t.Errorf("expected outcome=running, got %q", back.Outcome)
	}
	if v, ok := back.Verifiers["v1"]; !ok || v.Verdict != "pass" {
		t.Errorf("verifier not roundtripped: %+v", back.Verifiers)
	}
	if back.UpdatedAt.IsZero() {
		t.Errorf("UpdatedAt not set on save")
	}
}

func TestSession_SaveStateNilRejected(t *testing.T) {
	s, _ := NewSession(t.TempDir(), "run_nil", nil)
	if err := s.SaveState(nil); err == nil {
		t.Error("expected error saving nil state")
	}
}

// TestSession_SaveStateConcurrent makes sure stateMu actually serializes
// writers — race detector + many goroutines covers the contract that
// the orchestrator's fan-out workers can call SaveState concurrently.
func TestSession_SaveStateConcurrent(t *testing.T) {
	s, _ := NewSession(t.TempDir(), "run_c", nil)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st := &RunState{RunID: s.RunID, CurrentStage: i}
			if err := s.SaveState(st); err != nil {
				t.Errorf("SaveState: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if _, err := s.LoadState(); err != nil {
		t.Errorf("LoadState after concurrent saves: %v", err)
	}
}

// ---------------------------------------------------------------------------
// stage clearing
// ---------------------------------------------------------------------------

func TestSession_ClearStage(t *testing.T) {
	s, _ := NewSession(t.TempDir(), "run_clr", nil)
	path := s.IterationArtifactPath("02_draft", "draft.md", 1)
	if err := s.WriteArtifact(path, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearStage("02_draft"); err != nil {
		t.Fatalf("ClearStage: %v", err)
	}
	if _, err := os.Stat(s.StageDir("02_draft")); !os.IsNotExist(err) {
		t.Errorf("expected stage dir removed, stat err=%v", err)
	}
	// Clearing a non-existent stage is fine.
	if err := s.ClearStage("never_existed"); err != nil {
		t.Errorf("expected nil for missing stage, got: %v", err)
	}
}
