// Cost report CLI for the per-step router + multi-dim budget gate
// (idea 09). Reads the journal of a single session — or every session
// rooted under sessions.root — and aggregates llm.response usage by tag
// dimension.
//
//	nexus cost report --session <id>
//	nexus cost report --tenant acme --since 24h
//	nexus cost report --since 7d                # all sessions, all tenants
//
// The cost figures come from llm.response.cost_usd which providers emit
// using pkg/engine/pricing — keeping the CLI provider-agnostic and the
// price table in one place.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/journal"
)

func runCost(args []string) int {
	if len(args) == 0 {
		costUsage()
		return 2
	}
	switch args[0] {
	case "report":
		return runCostReport(args[1:])
	default:
		costUsage()
		fmt.Fprintf(os.Stderr, "\nerror: unknown action %q\n", args[0])
		return 2
	}
}

func costUsage() {
	fmt.Fprintln(os.Stderr, `usage: nexus cost <action> [flags]

actions:
  report     Aggregate llm.response usage by tag dimension.

each action accepts:
  --config <path>   path to nexus config (default: nexus.yaml)`)
}

func runCostReport(args []string) int {
	fs := flag.NewFlagSet("cost report", flag.ExitOnError)
	configPath := fs.String("config", "nexus.yaml", "path to config file")
	sessionID := fs.String("session", "", "limit to a single session id (default: all sessions)")
	tenant := fs.String("tenant", "", "limit to one tenant tag value")
	groupBy := fs.String("group-by", "session_id", "tag dimension to group by (session_id|tenant|project|user|source_plugin|model|task_kind)")
	since := fs.Duration("since", 0, "consider only events newer than this duration (e.g. 24h, 7d). 0 = no limit")
	jsonOut := fs.Bool("json", false, "emit JSON instead of a table")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: nexus cost report [flags]

flags:`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	root := engine.ExpandPath(cfg.Core.Sessions.Root)
	if root == "" {
		fmt.Fprintln(os.Stderr, "sessions.root not configured")
		return 1
	}

	sessions, err := selectSessions(root, *sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	cutoff := time.Time{}
	if *since > 0 {
		cutoff = time.Now().Add(-*since)
	}

	report := newCostReport(*groupBy)
	for _, sid := range sessions {
		dir := filepath.Join(root, sid, "journal")
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		r, err := journal.Open(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open %s: %v\n", sid, err)
			continue
		}
		_ = r.Iter(func(env journal.Envelope) bool {
			if env.Type != "llm.response" {
				return true
			}
			if !cutoff.IsZero() && env.Ts.Before(cutoff) {
				return true
			}
			rec, ok := decodeUsage(env.Payload)
			if !ok {
				return true
			}
			if *tenant != "" && rec.tenant() != *tenant {
				return true
			}
			rec.SessionID = sid
			report.add(rec)
			return true
		})
	}

	if *jsonOut {
		return report.emitJSON()
	}
	return report.emitTable()
}

// selectSessions returns the list of session IDs to scan. When a single
// session is requested, just that one; otherwise every direct child of
// the sessions root that contains a `journal/` directory.
func selectSessions(root, only string) ([]string, error) {
	if only != "" {
		return []string{only}, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", root, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "journal")); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// usageRecord is the slice of an llm.response we care about for cost
// reporting. We decode only the fields that contribute to aggregation —
// the journal also stores Citations, Alternatives, etc, which the cost
// report ignores.
type usageRecord struct {
	SessionID string
	Model     string
	CostUSD   float64
	Usage     struct {
		PromptTokens     int `json:"PromptTokens"`
		CompletionTokens int `json:"CompletionTokens"`
		TotalTokens      int `json:"TotalTokens"`
	}
	Tags     map[string]string `json:"Tags"`
	Metadata map[string]any    `json:"Metadata"`
}

func (r usageRecord) tenant() string  { return r.tag("tenant") }
func (r usageRecord) project() string { return r.tag("project") }
func (r usageRecord) user() string    { return r.tag("user") }
func (r usageRecord) plugin() string  { return r.tag("source_plugin") }

func (r usageRecord) tag(k string) string {
	if r.Tags != nil {
		if v, ok := r.Tags[k]; ok {
			return v
		}
	}
	return ""
}

func (r usageRecord) taskKind() string {
	if r.Metadata == nil {
		return ""
	}
	if v, ok := r.Metadata["task_kind"].(string); ok {
		return v
	}
	return ""
}

// decodeUsage round-trips the journal payload (a map[string]any after
// JSON decode) through json.Marshal/Unmarshal into usageRecord. The
// double-encode keeps the CLI decoupled from the events package's
// internal field naming since the journal stores the marshaled shape.
func decodeUsage(payload any) (usageRecord, bool) {
	if payload == nil {
		return usageRecord{}, false
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return usageRecord{}, false
	}
	var r usageRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return usageRecord{}, false
	}
	// Skip placeholder/empty responses — they carry no cost signal.
	if r.CostUSD == 0 && r.Usage.TotalTokens == 0 {
		return usageRecord{}, false
	}
	return r, true
}

// costReport accumulates usage by group key.
type costReport struct {
	groupBy string
	rows    map[string]*costRow
}

type costRow struct {
	Key     string  `json:"key"`
	Calls   int     `json:"calls"`
	Input   int     `json:"input_tokens"`
	Output  int     `json:"output_tokens"`
	Total   int     `json:"total_tokens"`
	CostUSD float64 `json:"cost_usd"`
}

func newCostReport(groupBy string) *costReport {
	return &costReport{groupBy: groupBy, rows: make(map[string]*costRow)}
}

func (r *costReport) add(rec usageRecord) {
	key := r.keyFor(rec)
	if key == "" {
		key = "(unset)"
	}
	row, ok := r.rows[key]
	if !ok {
		row = &costRow{Key: key}
		r.rows[key] = row
	}
	row.Calls++
	row.Input += rec.Usage.PromptTokens
	row.Output += rec.Usage.CompletionTokens
	row.Total += rec.Usage.TotalTokens
	row.CostUSD += rec.CostUSD
}

func (r *costReport) keyFor(rec usageRecord) string {
	switch r.groupBy {
	case "session_id":
		return rec.SessionID
	case "tenant":
		return rec.tenant()
	case "project":
		return rec.project()
	case "user":
		return rec.user()
	case "source_plugin":
		return rec.plugin()
	case "model":
		return rec.Model
	case "task_kind":
		return rec.taskKind()
	default:
		return rec.SessionID
	}
}

func (r *costReport) sortedRows() []*costRow {
	out := make([]*costRow, 0, len(r.rows))
	for _, row := range r.rows {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CostUSD != out[j].CostUSD {
			return out[i].CostUSD > out[j].CostUSD
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func (r *costReport) emitJSON() int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	type out struct {
		GroupBy string     `json:"group_by"`
		Rows    []*costRow `json:"rows"`
		Totals  costRow    `json:"totals"`
	}
	rows := r.sortedRows()
	totals := costRow{Key: "TOTAL"}
	for _, row := range rows {
		totals.Calls += row.Calls
		totals.Input += row.Input
		totals.Output += row.Output
		totals.Total += row.Total
		totals.CostUSD += row.CostUSD
	}
	if err := enc.Encode(out{GroupBy: r.groupBy, Rows: rows, Totals: totals}); err != nil {
		fmt.Fprintf(os.Stderr, "json: %v\n", err)
		return 1
	}
	return 0
}

func (r *costReport) emitTable() int {
	rows := r.sortedRows()
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "no llm.response records matched.")
		return 0
	}

	keyHeader := r.groupBy
	keyWidth := len(keyHeader)
	for _, row := range rows {
		if l := len(row.Key); l > keyWidth {
			keyWidth = l
		}
	}
	if keyWidth > 64 {
		keyWidth = 64
	}

	fmt.Printf("%-*s  %6s  %12s  %12s  %12s  %10s\n",
		keyWidth, keyHeader, "calls", "input", "output", "total", "cost_usd")
	fmt.Println(strings.Repeat("-", keyWidth+2+6+2+12+2+12+2+12+2+10))

	totals := costRow{}
	for _, row := range rows {
		totals.Calls += row.Calls
		totals.Input += row.Input
		totals.Output += row.Output
		totals.Total += row.Total
		totals.CostUSD += row.CostUSD
		key := row.Key
		if len(key) > keyWidth {
			key = key[:keyWidth-1] + "…"
		}
		fmt.Printf("%-*s  %6d  %12d  %12d  %12d  %10.4f\n",
			keyWidth, key, row.Calls, row.Input, row.Output, row.Total, row.CostUSD)
	}
	fmt.Println(strings.Repeat("-", keyWidth+2+6+2+12+2+12+2+12+2+10))
	fmt.Printf("%-*s  %6d  %12d  %12d  %12d  %10.4f\n",
		keyWidth, "TOTAL", totals.Calls, totals.Input, totals.Output, totals.Total, totals.CostUSD)
	return 0
}
