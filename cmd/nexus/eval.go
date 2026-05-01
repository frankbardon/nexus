package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/eval/baseline"
	evalcase "github.com/frankbardon/nexus/pkg/eval/case"
	"github.com/frankbardon/nexus/pkg/eval/report"
	"github.com/frankbardon/nexus/pkg/eval/runner"
	"gopkg.in/yaml.v3"
)

// jsonEncoder builds a *json.Encoder for w. Wrapping io.Writer keeps
// writeBaselineJSON polymorphic across *os.File and bytes.Buffer (tests).
func jsonEncoder(w io.Writer) *json.Encoder { return json.NewEncoder(w) }

// runEval is the "nexus eval" subcommand. It dispatches to subcommands
// (run, baseline, record, promote) based on the first positional arg.
// Mirrors the shape of runIngest — same flag style, same logger handling,
// same exit-code idioms.
//
// Top-level layout:
//
//	nexus eval [--inspect-mode] <subcommand> [flags]
//
// `--inspect-mode` is a Phase 5 stub. Subcommands `record` and `promote`
// are Phase 3 stubs.
func runEval(args []string) int {
	if len(args) == 0 {
		printEvalHelp()
		return 0
	}
	// Early sniff for --inspect-mode at the top level (Phase 5).
	for _, a := range args {
		if a == "--inspect-mode" {
			fmt.Fprintln(os.Stderr, "nexus eval --inspect-mode: not implemented in this phase (Phase 5)")
			return 2
		}
	}

	switch args[0] {
	case "-h", "--help", "help":
		printEvalHelp()
		return 0
	case "run":
		return runEvalRun(args[1:])
	case "baseline":
		return runEvalBaseline(args[1:])
	case "record", "promote":
		fmt.Fprintf(os.Stderr, "nexus eval %s: not implemented in this phase (Phase 3)\n", args[0])
		return 2
	default:
		fmt.Fprintf(os.Stderr, "nexus eval: unknown subcommand %q\n", args[0])
		printEvalHelp()
		return 2
	}
}

func printEvalHelp() {
	fmt.Fprintln(os.Stderr, `usage: nexus eval <subcommand> [flags]

Subcommands:
  run        Run one or more eval cases and emit a JSON report.
  baseline   Diff a fresh report against a stored baseline; gate on thresholds.
  record     Promote a session into an eval case (Phase 3 — stub).
  promote    Alias for record (Phase 3 — stub).

Top-level flags:
  --inspect-mode   JSON-on-stdin/stdout protocol mode (Phase 5 — stub).

Use 'nexus eval <subcommand> -h' for subcommand-specific flags.`)
}

// -- run subcommand ----------------------------------------------------------

func runEvalRun(args []string) int {
	fs := flag.NewFlagSet("eval run", flag.ExitOnError)
	caseID := fs.String("case", "", "single case ID to run; takes precedence over --cases-dir filtering")
	casesDir := fs.String("cases-dir", "", "directory containing case bundles (default: from eval.cases_dir, else tests/eval/cases)")
	tagsCSV := fs.String("tags", "", "comma-separated tag filter; cases missing any tag are skipped")
	model := fs.String("model", "", "override core.models.default for every case")
	deterministic := fs.Bool("deterministic", true, "deterministic mode (no LLM judge calls — Phase 5 wires --full)")
	full := fs.Bool("full", false, "run LLM-judge semantic assertions (Phase 5 — currently no-op)")
	parallel := fs.Int("parallel", 0, "max concurrent cases (default 4)")
	reportDir := fs.String("report-dir", "", "output directory (default: from eval.reports_dir, else tests/eval/reports)")
	configPath := fs.String("config", "", "optional config path that contains an eval.* block; defaults are used otherwise")

	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: nexus eval run [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *full && *deterministic {
		// --full implies !deterministic; the user explicitly asked for full.
		*deterministic = false
	}
	if *full {
		fmt.Fprintln(os.Stderr, "warning: --full is a Phase 5 placeholder; running in deterministic mode")
		*deterministic = true
	}

	cfg := loadEvalConfig(*configPath)
	resolvedCasesDir := firstNonEmpty(engine.ExpandPath(*casesDir), engine.ExpandPath(cfg.CasesDir), engine.ExpandPath("tests/eval/cases"))
	resolvedReportDir := firstNonEmpty(engine.ExpandPath(*reportDir), engine.ExpandPath(cfg.ReportsDir), engine.ExpandPath("tests/eval/reports"))

	// Discover cases.
	var cases []*evalcase.Case
	if *caseID != "" {
		c, err := evalcase.Load(filepath.Join(resolvedCasesDir, *caseID))
		if err != nil {
			fmt.Fprintf(os.Stderr, "load case %q: %v\n", *caseID, err)
			return 1
		}
		cases = []*evalcase.Case{c}
	} else {
		discovered, err := discoverCases(resolvedCasesDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "discover cases under %q: %v\n", resolvedCasesDir, err)
			return 1
		}
		if len(discovered) == 0 {
			fmt.Fprintf(os.Stderr, "no cases found under %q\n", resolvedCasesDir)
			return 1
		}
		cases = discovered
	}

	mode := "deterministic"
	if !*deterministic {
		mode = "full"
	}

	runID := time.Now().UTC().Format("20060102T150405Z")
	tags := splitCSV(*tagsCSV)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Per-case sessions root: a temp tree under report-dir/run-id keeps
	// session artifacts close to the report and out of ~/.nexus/sessions.
	runDir := filepath.Join(resolvedReportDir, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create run dir: %v\n", err)
		return 1
	}
	sessionsRoot := filepath.Join(runDir, "_sessions")
	if err := os.MkdirAll(sessionsRoot, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create sessions dir: %v\n", err)
		return 1
	}

	results := runner.RunMany(ctx, cases, runner.MultiOptions{
		Parallelism:   *parallel,
		Tags:          tags,
		ModelOverride: *model,
		PerCase: runner.Options{
			SessionsRoot: sessionsRoot,
		},
	})
	r := report.Aggregate(mode, results)
	r.RunID = runID

	// Write report.json + summary.txt to the run directory.
	reportPath := filepath.Join(runDir, "report.json")
	f, err := os.Create(reportPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open report: %v\n", err)
		return 1
	}
	if err := report.WriteJSON(f, r); err != nil {
		f.Close()
		fmt.Fprintf(os.Stderr, "write report: %v\n", err)
		return 1
	}
	f.Close()

	summaryPath := filepath.Join(runDir, "summary.txt")
	if sf, err := os.Create(summaryPath); err == nil {
		_ = report.WriteSummary(sf, r)
		sf.Close()
	}

	// Human summary to stderr (TTY-aware).
	_ = report.WriteTerminalSummary(os.Stdout, r)
	fmt.Fprintf(os.Stderr, "\nreport: %s\n", reportPath)

	if r.Summary.Failed > 0 {
		return 1
	}
	return 0
}

// -- baseline subcommand -----------------------------------------------------

func runEvalBaseline(args []string) int {
	fs := flag.NewFlagSet("eval baseline", flag.ExitOnError)
	against := fs.String("against", "", "baseline report file or directory (required)")
	freshPath := fs.String("report", "", "fresh report file or directory (defaults to the most recent under eval.reports_dir)")
	failScore := fs.Float64("fail-on-score-drop", 0, "absolute pass-rate drop threshold (0–1); 0 disables (default: from eval.baseline.fail_on_score_drop)")
	failLatency := fs.Float64("fail-on-latency-p95-drop", 0, "relative p95-latency increase threshold; 0 disables (default: from eval.baseline.fail_on_latency_p95_drop)")
	configPath := fs.String("config", "", "optional config path that contains an eval.baseline.* block")
	outPath := fs.String("out", "", "write the diff JSON to this path (default: stdout)")

	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: nexus eval baseline --against <path> [--report <path>] [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *against == "" {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "\nerror: --against is required")
		return 2
	}

	cfg := loadEvalConfig(*configPath)

	// Resolve fresh report: if not given, scan reports_dir for the most
	// recently-created run with a report.json.
	freshResolved := engine.ExpandPath(*freshPath)
	if freshResolved == "" {
		latest, err := mostRecentReport(engine.ExpandPath(firstNonEmpty(cfg.ReportsDir, "tests/eval/reports")))
		if err != nil {
			fmt.Fprintf(os.Stderr, "auto-resolve fresh report: %v\n", err)
			return 1
		}
		freshResolved = latest
	}

	againstReport, err := baseline.LoadReport(engine.ExpandPath(*against))
	if err != nil {
		fmt.Fprintf(os.Stderr, "load against: %v\n", err)
		return 1
	}
	freshReport, err := baseline.LoadReport(freshResolved)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load fresh: %v\n", err)
		return 1
	}

	d, err := baseline.Compute(againstReport, freshReport)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compute diff: %v\n", err)
		return 1
	}

	// Apply thresholds. Flag overrides config; config defaults to 0.
	thresholds := baseline.Thresholds{
		FailOnScoreDrop:      pickFloat(*failScore, cfg.Baseline.FailOnScoreDrop),
		FailOnLatencyP95Drop: pickFloat(*failLatency, cfg.Baseline.FailOnLatencyP95Drop),
	}
	failed := d.Decide(thresholds)

	out := os.Stdout
	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open out: %v\n", err)
			return 1
		}
		defer f.Close()
		out = f
	}
	if err := writeBaselineJSON(out, d); err != nil {
		fmt.Fprintf(os.Stderr, "write diff: %v\n", err)
		return 1
	}

	if failed {
		fmt.Fprintln(os.Stderr, "baseline: thresholds breached — failing")
		return 1
	}
	return 0
}

// -- helpers -----------------------------------------------------------------

// evalConfig is the top-level eval block.
type evalConfig struct {
	CasesDir   string `yaml:"cases_dir"`
	ReportsDir string `yaml:"reports_dir"`
	Judge      struct {
		Model       string  `yaml:"model"`
		Temperature float64 `yaml:"temperature"`
		NSamples    int     `yaml:"n_samples"`
		Cache       bool    `yaml:"cache"`
	} `yaml:"judge"`
	Baseline struct {
		FailOnScoreDrop      float64 `yaml:"fail_on_score_drop"`
		FailOnLatencyP95Drop float64 `yaml:"fail_on_latency_p95_drop"`
	} `yaml:"baseline"`
}

// loadEvalConfig parses just the `eval:` block from a YAML config file. The
// rest of the file (the engine config) is ignored. An empty path or missing
// block returns zero defaults, which is the right behavior — flags fill in
// what the user wants.
func loadEvalConfig(path string) evalConfig {
	if path == "" {
		return evalConfig{}
	}
	data, err := os.ReadFile(engine.ExpandPath(path))
	if err != nil {
		return evalConfig{}
	}
	var wrapper struct {
		Eval evalConfig `yaml:"eval"`
	}
	if err := yaml.Unmarshal(data, &wrapper); err != nil {
		return evalConfig{}
	}
	return wrapper.Eval
}

// discoverCases walks dir, treating each immediate child directory as a case.
// Cases that fail to load are reported but do not abort the discovery — the
// CLI surfaces the load error and skips the case.
func discoverCases(dir string) ([]*evalcase.Case, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*evalcase.Case
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Skip hidden / underscore-prefixed dirs (e.g. _record sentinels).
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		path := filepath.Join(dir, name)
		c, err := evalcase.Load(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", name, err)
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func pickFloat(flagValue, configValue float64) float64 {
	if flagValue > 0 {
		return flagValue
	}
	return configValue
}

// writeBaselineJSON encodes the diff as indented JSON.
func writeBaselineJSON(w io.Writer, d *baseline.Diff) error {
	enc := jsonEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}

// mostRecentReport scans reportsDir for run subdirectories and returns the
// path to the lexicographically largest one's report.json. We rely on the
// timestamped run-id format ("20060102T150405Z") sorting in chronological
// order — the same trick the engine uses for session IDs.
func mostRecentReport(reportsDir string) (string, error) {
	entries, err := os.ReadDir(reportsDir)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no run directories under %q", reportsDir)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return filepath.Join(reportsDir, names[0], "report.json"), nil
}
