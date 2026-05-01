package report

import (
	"fmt"
	"io"
	"os"
)

// WriteTerminalSummary writes a short, optionally-coloured summary of the
// report to w. Colour is enabled only when w is *os.File pointing at a tty
// AND the NO_COLOR env var is unset (per https://no-color.org/).
//
// This sits alongside WriteSummary (the plain version used by tests and
// JSON-companion writers) — both write the same information; this one adds
// ANSI colour for human-facing CLI output.
func WriteTerminalSummary(w io.Writer, r *Report) error {
	col := newColorizer(w)
	fmt.Fprintf(w, "%s (%s mode)\n", col.bold("eval report"), r.Mode)
	if r.RunID != "" {
		fmt.Fprintf(w, "  run_id=%s\n", r.RunID)
	}
	fmt.Fprintf(w, "  total=%d %s=%d %s=%d\n",
		r.Summary.Total,
		col.green("passed"), r.Summary.Passed,
		col.red("failed"), r.Summary.Failed,
	)
	for _, c := range r.Cases {
		mark := col.green("PASS")
		if !c.Pass {
			mark = col.red("FAIL")
		}
		fmt.Fprintf(w, "  [%s] %s\n", mark, c.CaseID)
		for _, a := range c.Assertions {
			amark := col.green("ok")
			if !a.Pass {
				amark = col.red("fail")
			}
			fmt.Fprintf(w, "    - %-26s %s", a.Kind, amark)
			if a.Message != "" {
				fmt.Fprintf(w, ": %s", col.dim(a.Message))
			}
			fmt.Fprintln(w)
		}
	}
	return nil
}

// colorizer wraps an io.Writer and conditionally emits ANSI escape codes.
type colorizer struct {
	color bool
}

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
)

func newColorizer(w io.Writer) colorizer {
	if os.Getenv("NO_COLOR") != "" {
		return colorizer{color: false}
	}
	if f, ok := w.(*os.File); ok && f != nil {
		if isTerminal(f) {
			return colorizer{color: true}
		}
	}
	return colorizer{color: false}
}

// isTerminal returns true when f appears to be a terminal — checked by
// stat'ing the fd and looking for ModeCharDevice. Avoids the
// golang.org/x/term dependency promotion that direct import would force.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func (c colorizer) wrap(prefix, s string) string {
	if !c.color {
		return s
	}
	return prefix + s + ansiReset
}

func (c colorizer) bold(s string) string   { return c.wrap(ansiBold, s) }
func (c colorizer) dim(s string) string    { return c.wrap(ansiDim, s) }
func (c colorizer) red(s string) string    { return c.wrap(ansiRed, s) }
func (c colorizer) green(s string) string  { return c.wrap(ansiGreen, s) }
func (c colorizer) yellow(s string) string { return c.wrap(ansiYellow, s) }
