package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"

	evalcase "github.com/frankbardon/nexus/pkg/eval/case"
	evalrunner "github.com/frankbardon/nexus/pkg/eval/runner"
)

// runEvalRecipe runs one or more golden eval cases via the pkg/eval
// runner. Each case is a directory under tests/eval/cases/<id>/ holding:
//
//   - case.yaml         metadata (name, tags, owner, freshness budget)
//   - input/config.yaml engine config the case runs against
//   - input/inputs.yaml scripted user inputs (drive nexus.io.test)
//   - journal/          golden journal from the original recording
//   - assertions.yaml   deterministic + semantic assertions
//
// At replay time the engine swaps every side-effecting plugin (LLM
// providers, tools) into "stash" mode — it pops responses from the
// journal instead of calling out, so the recipe needs zero API keys.
//
// What this recipe demonstrates:
//
//   - The eval harness as a contract test: the same agent + plugin
//     stack that ran during recording must reproduce the same event
//     stream during replay. Drift surfaces as assertion failures.
//   - Hermetic CI-side validation. Re-runs are deterministic and free.
//
// Usage:
//
//	cmd/demo recipe eval                 # run the default case
//	cmd/demo recipe eval --case ID       # run a specific case
//	cmd/demo recipe eval --root DIR      # override cases root
//
// The recipe prints a pass/fail line plus per-assertion detail.
// Exit code is 0 on pass, non-zero on fail or load error.
func runEvalRecipe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	caseID := fs.String("case", "skills-discovery",
		"Case directory under --root to run. Default: skills-discovery.")
	root := fs.String("root", "tests/eval/cases",
		"Cases root directory. Defaults to the in-repo tests/eval/cases.")
	if err := fs.Parse(args); err != nil {
		return err
	}

	caseDir, err := filepath.Abs(filepath.Join(*root, *caseID))
	if err != nil {
		return fmt.Errorf("resolve case dir: %w", err)
	}

	c, err := evalcase.Load(caseDir)
	if err != nil {
		return fmt.Errorf("load case %s: %w", *caseID, err)
	}

	fmt.Println("Recipe: eval")
	fmt.Printf("  Case:        %s\n", c.ID)
	fmt.Printf("  Description: %s\n", c.Meta.Description)
	if len(c.Meta.Tags) > 0 {
		fmt.Printf("  Tags:        %v\n", c.Meta.Tags)
	}
	fmt.Println()

	// Run with default options (logger discarded, default boot/replay
	// timeouts). Production CI would tighten timeouts and capture the
	// logger.
	res, err := evalrunner.Run(ctx, c, evalrunner.Options{})
	if err != nil {
		return fmt.Errorf("run case %s: %w", c.ID, err)
	}

	verdict := "FAIL"
	if res.Pass {
		verdict = "PASS"
	}
	fmt.Printf("  Verdict:     %s\n", verdict)
	fmt.Printf("  Duration:    %s\n", res.EndedAt.Sub(res.StartedAt))
	fmt.Printf("  Assertions:  %d\n", len(res.Assertions))
	for _, a := range res.Assertions {
		marker := "PASS"
		if !a.Pass {
			marker = "FAIL"
		}
		fmt.Printf("    [%s] %-22s %s\n", marker, a.Kind, a.Message)
	}

	if !res.Pass {
		return fmt.Errorf("eval case %s failed", c.ID)
	}
	return nil
}
