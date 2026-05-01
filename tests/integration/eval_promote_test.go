//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	evalcase "github.com/frankbardon/nexus/pkg/eval/case"
	"github.com/frankbardon/nexus/pkg/eval/promote"
	"github.com/frankbardon/nexus/pkg/eval/runner"
)

// TestEvalPromote_RoundTrip is the headline acceptance test for Phase 3:
//
//  1. Synthesize a "session directory" from the build-error-fix seed case's
//     journal and config — exactly the on-disk shape ~/.nexus/sessions/<id>
//     produces.
//  2. Call promote.Promote — same code path the CLI uses.
//  3. Load the promoted case via evalcase.Load and replay it via runner.Run.
//
// Pass = the promoted case is green deterministic, no API key required.
//
// This binds the contract: a real session, promoted, must produce a case
// the runner will replay green without any human edits.
func TestEvalPromote_RoundTrip(t *testing.T) {
	sessionDir := buildFakeSessionFromSeed(t, "build-error-fix", "completed")

	casesDir := filepath.Join(t.TempDir(), "cases")
	res, err := promote.Promote(context.Background(), promote.PromoteOptions{
		SessionDir:  sessionDir,
		CaseID:      "promoted-build-error-fix",
		CasesDir:    casesDir,
		Owner:       "test@nexus.local",
		Tags:        []string{"promoted", "smoke"},
		Description: "round-trip of build-error-fix via promote.Promote",
		OpenEditor:  false,
	})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if res.InputCount == 0 {
		t.Fatalf("expected input events in the promoted case (build-error-fix has 2)")
	}

	// Loader round-trip — every required field must parse.
	c, err := evalcase.Load(res.CaseDir)
	if err != nil {
		t.Fatalf("evalcase.Load(%q): %v", res.CaseDir, err)
	}
	if got := len(c.Inputs); got != res.InputCount {
		t.Errorf("inputs.yaml mismatch: %d vs Promote.InputCount=%d", got, res.InputCount)
	}

	// Replay the promoted case. Use a tempdir for sessions so we don't
	// trample on ~/.nexus/sessions.
	sessionsRoot := t.TempDir()
	t.Cleanup(func() { os.RemoveAll(sessionsRoot) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rres, err := runner.Run(ctx, c, runner.Options{SessionsRoot: sessionsRoot})
	if err != nil {
		t.Fatalf("runner.Run on promoted case: %v", err)
	}
	if !rres.Pass {
		var fails []string
		for _, a := range rres.Assertions {
			if !a.Pass {
				fails = append(fails, a.Kind+": "+a.Message)
			}
		}
		t.Fatalf("promoted case failed assertions:\n  %v\n  counts=%v", fails, rres.Counts)
	}
}

// TestEvalPromote_CLI_NoEditExit0 builds the binary and invokes
// `nexus eval promote --no-edit --force` end-to-end. Asserts exit 0 and
// that the case dir is loadable via evalcase. Mirrors the acceptance
// criterion verbatim.
func TestEvalPromote_CLI_NoEditExit0(t *testing.T) {
	sessionDir := buildFakeSessionFromSeed(t, "build-error-fix", "completed")
	repoRoot := projectFile(t, "")
	binPath := filepath.Join(t.TempDir(), "nexus-promote")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/nexus")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	casesDir := filepath.Join(t.TempDir(), "cases")
	cmd := exec.Command(binPath,
		"eval", "promote",
		"--session", sessionDir,
		"--case", "promoted-cli-roundtrip",
		"--cases-dir", casesDir,
		"--no-edit",
		"--force",
	)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("eval promote: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "promoted session") {
		t.Errorf("expected 'promoted session' in CLI output; got:\n%s", out)
	}

	c, err := evalcase.Load(filepath.Join(casesDir, "promoted-cli-roundtrip"))
	if err != nil {
		t.Fatalf("evalcase.Load on CLI-promoted case: %v", err)
	}
	if c.Meta.Name != "promoted-cli-roundtrip" {
		t.Errorf("Meta.Name = %q want promoted-cli-roundtrip", c.Meta.Name)
	}
}

// buildFakeSessionFromSeed reads a seed case under tests/eval/cases/<id>/
// and reshapes it into the on-disk session-dir layout (journal + metadata
// + config-snapshot). The seed cases were hand-crafted with this exact
// fidelity in mind so promotion can round-trip without external services.
func buildFakeSessionFromSeed(t *testing.T, seedID, status string) string {
	t.Helper()
	repoRoot := projectFile(t, "")
	seedDir := filepath.Join(repoRoot, "tests/eval/cases", seedID)

	sessionsRoot := t.TempDir()
	sessionID := "fake-" + seedID
	sessionDir := filepath.Join(sessionsRoot, sessionID)
	if err := os.MkdirAll(filepath.Join(sessionDir, "metadata"), 0o755); err != nil {
		t.Fatalf("mkdir metadata: %v", err)
	}

	// Copy journal/ verbatim from the seed.
	if err := copyTreeForTest(filepath.Join(seedDir, "journal"), filepath.Join(sessionDir, "journal")); err != nil {
		t.Fatalf("copy seed journal: %v", err)
	}

	// Synthesize a session.json — promote only reads .Status, so a minimal
	// shape is enough.
	meta := map[string]any{
		"id":         sessionID,
		"started_at": time.Now().UTC().Format(time.RFC3339Nano),
		"status":     status,
	}
	mb, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(sessionDir, "metadata", "session.json"), mb, 0o644); err != nil {
		t.Fatalf("write session.json: %v", err)
	}

	// Use the seed's input/config.yaml as the session's config-snapshot.
	cfg, err := os.ReadFile(filepath.Join(seedDir, "input", "config.yaml"))
	if err != nil {
		t.Fatalf("read seed config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "metadata", "config-snapshot.yaml"), cfg, 0o644); err != nil {
		t.Fatalf("write config-snapshot.yaml: %v", err)
	}

	return sessionDir
}

func copyTreeForTest(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		sf, err := os.Open(path)
		if err != nil {
			return err
		}
		defer sf.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		df, err := os.Create(target)
		if err != nil {
			return err
		}
		defer df.Close()
		_, err = io.Copy(df, sf)
		return err
	})
}
