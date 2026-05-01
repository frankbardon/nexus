// Package runner — multi-case driver. RunMany executes a slice of cases in
// bounded parallel, applies the model override (which mutates each case's
// raw config bytes — not a parsed Config), and returns one combined Report.
//
// The single-case Run remains the workhorse. RunMany is a thin coordinator:
// fan out, collect, fold into the Phase 1 report shape (no schema bump).
package runner

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"time"

	evalcase "github.com/frankbardon/nexus/pkg/eval/case"
	"gopkg.in/yaml.v3"
)

// MultiOptions tunes RunMany.
type MultiOptions struct {
	// Parallelism caps concurrent case executions. <=0 picks a sensible
	// default (4, or NumCPU when smaller).
	Parallelism int
	// Tags, when non-empty, restricts the run to cases whose Meta.Tags
	// contains every entry. Empty means "all cases".
	Tags []string
	// ModelOverride, when non-empty, rewrites core.models.default in each
	// case's config bytes before constructing the engine. Surgical YAML
	// rewrite — does not touch other keys.
	ModelOverride string
	// PerCase is forwarded to every Run() call. Notably, SessionsRoot here
	// is shared — RunMany subdivides it into per-case sub-tempdirs to keep
	// concurrent runs isolated. Pass an empty SessionsRoot to inherit the
	// case-config default (production), or t.TempDir() in tests.
	PerCase Options
}

// RunMany executes cases concurrently and returns one Result per case, in
// stable case-ID order. Failure of one case does not abort the others —
// every case runs, and a Result whose Run() returned a hard error (engine
// boot failure, journal load error) lands here with a synthesized failed
// "runner" assertion so callers can fold it into a report uniformly.
//
// The aggregator (pkg/eval/report.Aggregate) is invoked by callers, not by
// this package — keeping report out of runner's import set avoids an
// import cycle (report depends on runner.Result).
func RunMany(ctx context.Context, cases []*evalcase.Case, opts MultiOptions) []*Result {
	parallel := opts.Parallelism
	if parallel <= 0 {
		parallel = 4
		if cpu := runtime.NumCPU(); cpu > 0 && cpu < parallel {
			parallel = cpu
		}
	}

	filtered := filterByTags(cases, opts.Tags)
	if len(filtered) == 0 {
		return nil
	}

	// Stable order for deterministic dispatch + reporting.
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].ID < filtered[j].ID })

	results := make([]*Result, len(filtered))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup

	for i, c := range filtered {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, cs *evalcase.Case) {
			defer wg.Done()
			defer func() { <-sem }()
			res := runOne(ctx, cs, opts)
			results[idx] = res
		}(i, c)
	}
	wg.Wait()
	return results
}

// runOne runs a single case, applies the model override, and synthesizes a
// failed-result envelope on hard error so the report still surfaces it.
func runOne(ctx context.Context, c *evalcase.Case, opts MultiOptions) *Result {
	caseCopy := *c // shallow copy so we can rewrite ConfigYAML without racing siblings
	if opts.ModelOverride != "" {
		rewritten, err := overrideDefaultModelRole(caseCopy.ConfigYAML, opts.ModelOverride)
		if err == nil {
			caseCopy.ConfigYAML = rewritten
		} else {
			return failedResult(c.ID, fmt.Errorf("model override: %w", err))
		}
	}
	res, err := Run(ctx, &caseCopy, opts.PerCase)
	if err != nil {
		return failedResult(c.ID, err)
	}
	return res
}

// failedResult synthesizes a Result for a case whose Run() errored out before
// assertions could be evaluated. Surfacing it as a regular result keeps the
// report shape uniform.
func failedResult(caseID string, err error) *Result {
	now := time.Now()
	return &Result{
		CaseID:    caseID,
		StartedAt: now,
		EndedAt:   now,
		Pass:      false,
		Assertions: []evalcase.AssertionResult{
			{
				Kind:    "runner",
				Pass:    false,
				Message: err.Error(),
			},
		},
		Counts: map[string]int{},
	}
}

// filterByTags returns cases whose Meta.Tags is a superset of want. An empty
// want passes everything through.
func filterByTags(cases []*evalcase.Case, want []string) []*evalcase.Case {
	if len(want) == 0 {
		return cases
	}
	wantSet := make(map[string]struct{}, len(want))
	for _, t := range want {
		wantSet[t] = struct{}{}
	}
	out := make([]*evalcase.Case, 0, len(cases))
	for _, c := range cases {
		have := make(map[string]struct{}, len(c.Meta.Tags))
		for _, t := range c.Meta.Tags {
			have[t] = struct{}{}
		}
		ok := true
		for w := range wantSet {
			if _, present := have[w]; !present {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, c)
		}
	}
	return out
}

// overrideDefaultModelRole rewrites core.models.default to role. Mirrors the
// surgical-YAML approach used by overrideSessionsRoot — yaml.v3 node API,
// preserves unrelated keys.
func overrideDefaultModelRole(in []byte, role string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(in, &doc); err != nil {
		return nil, fmt.Errorf("unmarshal yaml: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		out := map[string]any{
			"core": map[string]any{
				"models": map[string]any{"default": role},
			},
		}
		return yaml.Marshal(out)
	}
	rootNode := doc.Content[0]
	if rootNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("yaml root is not a mapping")
	}
	core := getOrCreateMapping(rootNode, "core")
	models := getOrCreateMapping(core, "models")
	setStringKey(models, "default", role)
	return yaml.Marshal(&doc)
}
