//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	evalcase "github.com/frankbardon/nexus/pkg/eval/case"
	"github.com/frankbardon/nexus/pkg/eval/report"
	"github.com/frankbardon/nexus/pkg/eval/runner"
)

// TestEvalRunner_BuildErrorFix runs the seed case end-to-end via the new
// runner and confirms every assertion in its assertions.yaml passes.
//
// Mock mode: no API key. Replay short-circuits LLM and tool calls. Pure
// determinism — tagged integration so it lives alongside the rest of the
// suite, not because it makes external calls.
func TestEvalRunner_BuildErrorFix(t *testing.T) {
	caseDir := projectFile(t, "tests/eval/cases/build-error-fix")
	c, err := evalcase.Load(caseDir)
	if err != nil {
		t.Fatalf("Load case: %v", err)
	}

	sessionsRoot := t.TempDir()
	t.Cleanup(func() { os.RemoveAll(sessionsRoot) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := runner.Run(ctx, c, runner.Options{SessionsRoot: sessionsRoot})
	if err != nil {
		t.Fatalf("runner.Run: %v", err)
	}

	if !res.Pass {
		// Report failures and counts for fast diagnosis.
		var fails []string
		for _, a := range res.Assertions {
			if !a.Pass {
				fails = append(fails, fmt.Sprintf("[%s] %s diag=%v", a.Kind, a.Message, a.Diagnostics))
			}
		}
		t.Fatalf("seed case failed assertions:\n  %v\n  counts=%v", fails, res.Counts)
	}

	if got := len(res.Assertions); got == 0 {
		t.Errorf("expected assertions in result, got 0")
	}

	// Sanity: the report aggregator round-trips the result without panic
	// and surfaces matching pass/fail flags.
	r := report.Aggregate("deterministic", []*runner.Result{res})
	if r.Summary.Total != 1 || r.Summary.Passed != 1 {
		t.Errorf("report summary=%+v", r.Summary)
	}
}

// projectFile resolves a path relative to the repo root regardless of where
// the test binary is run from. Mirrors testharness.findProjectRoot.
func projectFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("cannot find project root")
		}
		dir = parent
	}
}
