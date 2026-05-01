//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEvalCLI_RunAllCases builds the binary and invokes
// `nexus eval run --cases-dir tests/eval/cases --deterministic`. Asserts
// exit code 0, that all 5 seed cases land in the report, and that every
// case passes.
//
// This is the headline acceptance test for Phase 2: it runs the entire
// eval surface end-to-end through the binary, exactly as a CI pipeline
// would. No API key required — all cases are mock-mode.
func TestEvalCLI_RunAllCases(t *testing.T) {
	repoRoot := projectFile(t, "")
	binPath := filepath.Join(t.TempDir(), "nexus-eval")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/nexus")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	reportDir := t.TempDir()
	cmd := exec.Command(binPath,
		"eval", "run",
		"--cases-dir", filepath.Join(repoRoot, "tests/eval/cases"),
		"--report-dir", reportDir,
		"--deterministic",
	)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("eval run failed: %v\n%s", err, out)
	}

	// Locate the run-id-named subdirectory under reportDir.
	entries, err := os.ReadDir(reportDir)
	if err != nil {
		t.Fatalf("read reportDir: %v", err)
	}
	var runDir string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), "_") {
			runDir = filepath.Join(reportDir, e.Name())
			break
		}
	}
	if runDir == "" {
		t.Fatalf("no run dir found under %s; output=\n%s", reportDir, out)
	}

	// Parse and assert on report.json.
	reportPath := filepath.Join(runDir, "report.json")
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}

	var r struct {
		SchemaVersion string `json:"schema_version"`
		Summary       struct {
			Total  int `json:"total"`
			Passed int `json:"passed"`
			Failed int `json:"failed"`
		} `json:"summary"`
		Cases []struct {
			CaseID string `json:"case_id"`
			Pass   bool   `json:"pass"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("decode report: %v\n%s", err, data)
	}

	if r.SchemaVersion != "1" {
		t.Errorf("schema_version=%q want 1", r.SchemaVersion)
	}
	if r.Summary.Total != 5 {
		t.Errorf("total=%d want 5", r.Summary.Total)
	}
	if r.Summary.Failed != 0 {
		failures := []string{}
		for _, c := range r.Cases {
			if !c.Pass {
				failures = append(failures, c.CaseID)
			}
		}
		t.Errorf("failed=%d (%v); cli output:\n%s", r.Summary.Failed, failures, out)
	}
}

// TestEvalCLI_BaselineEqualReports runs the same cases twice and uses the
// baseline subcommand to confirm equal reports diff to a clean exit code.
func TestEvalCLI_BaselineEqualReports(t *testing.T) {
	repoRoot := projectFile(t, "")
	binPath := filepath.Join(t.TempDir(), "nexus-eval")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/nexus")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	run := func() string {
		dir := t.TempDir()
		cmd := exec.Command(binPath,
			"eval", "run",
			"--cases-dir", filepath.Join(repoRoot, "tests/eval/cases"),
			"--report-dir", dir,
		)
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), "NO_COLOR=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("eval run: %v\n%s", err, out)
		}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), "_") {
				return filepath.Join(dir, e.Name())
			}
		}
		t.Fatal("no run dir found")
		return ""
	}
	a := run()
	b := run()

	cmd := exec.Command(binPath, "eval", "baseline", "--against", a, "--report", b)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("baseline equal-reports failed: %v\n%s", err, out)
	}
}
