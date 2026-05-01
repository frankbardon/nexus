package promote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/journal"
	"gopkg.in/yaml.v3"
)

// PromoteOptions controls a single Promote call.
type PromoteOptions struct {
	// SessionDir is the absolute path to ~/.nexus/sessions/<id>. The CLI is
	// responsible for resolving an ID to a path via engine.ExpandPath; this
	// package treats the value as already-resolved.
	SessionDir string
	// CaseID is the new case's identifier; becomes the basename of the
	// resulting case directory under CasesDir.
	CaseID string
	// CasesDir is the absolute path to tests/eval/cases (or the user's
	// chosen alternative).
	CasesDir string
	// Owner is recorded into case.yaml. If empty, falls back to $USER, then
	// "unknown".
	Owner string
	// Tags is recorded into case.yaml as the case's tag list.
	Tags []string
	// Description is recorded into case.yaml. If empty, a synthesized
	// default referencing the source session ID is used.
	Description string
	// OpenEditor, when true, exec's $EDITOR (fallback chain: $VISUAL, nano,
	// vi) on the assertions.yaml after Promote returns. The CLI surface
	// owns this — library callers typically leave it false.
	OpenEditor bool
	// Force, when true, overwrites an existing case directory at the
	// destination path. Without Force, an existing directory is an error.
	Force bool
	// Now, when non-zero, overrides time.Now() for recorded_at. Tests use
	// it to land deterministic timestamps.
	Now time.Time
}

// PromoteResult is what Promote returns to its caller. CLI prints fields
// from it; tests assert on them.
type PromoteResult struct {
	// CaseDir is the absolute path of the new case directory.
	CaseDir string
	// InputCount is the number of io.input events the journal carried.
	InputCount int
	// EventCount is the total number of envelopes copied from the journal.
	EventCount int
	// Warnings are non-fatal advisories surfaced to the user — failed
	// session statuses, non-replayable event types, missing config
	// snapshot, etc.
	Warnings []string
}

// Promote turns a real session directory into a deterministic eval case.
//
// Steps:
//
//  1. Validate the session dir, journal, and metadata exist.
//  2. Validate / handle the destination case dir per opts.Force.
//  3. Copy the journal byte-for-byte (header + active segment + rotated).
//  4. Copy metadata/config-snapshot.yaml verbatim to input/config.yaml.
//  5. Project journaled io.input events to input/inputs.yaml.
//  6. Synthesize a starter assertions.yaml.
//  7. Write case.yaml metadata.
//  8. Optionally launch $EDITOR on assertions.yaml.
//
// On failure after the case dir was created, Promote attempts to remove
// the partial directory so a retry isn't blocked by a stale shell.
func Promote(ctx context.Context, opts PromoteOptions) (*PromoteResult, error) {
	if opts.SessionDir == "" {
		return nil, fmt.Errorf("promote: SessionDir is required")
	}
	if opts.CaseID == "" {
		return nil, fmt.Errorf("promote: CaseID is required")
	}
	if opts.CasesDir == "" {
		return nil, fmt.Errorf("promote: CasesDir is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := validateSessionDir(opts.SessionDir); err != nil {
		return nil, err
	}

	caseDir := filepath.Join(opts.CasesDir, opts.CaseID)
	if err := prepareCaseDir(caseDir, opts.Force); err != nil {
		return nil, err
	}

	// Cleanup-on-error: if any step below fails, drop the partial case dir.
	// On success, success=true so the cleanup is a no-op.
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(caseDir)
		}
	}()

	res := &PromoteResult{CaseDir: caseDir}

	// 1) Copy journal.
	srcJournal := filepath.Join(opts.SessionDir, "journal")
	dstJournal := filepath.Join(caseDir, "journal")
	if err := copyDir(srcJournal, dstJournal); err != nil {
		return nil, fmt.Errorf("copy journal: %w", err)
	}

	// 2) Copy config snapshot verbatim — the engine should run on the same
	//    config the recorded session used.
	srcCfg := filepath.Join(opts.SessionDir, "metadata", "config-snapshot.yaml")
	dstCfgDir := filepath.Join(caseDir, "input")
	if err := os.MkdirAll(dstCfgDir, 0o755); err != nil {
		return nil, fmt.Errorf("create input dir: %w", err)
	}
	dstCfg := filepath.Join(dstCfgDir, "config.yaml")
	if err := copyFile(srcCfg, dstCfg); err != nil {
		// Snapshot may be absent on very early boot failures; surface as a
		// warning but keep going so the user can paste in a config by hand.
		if errors.Is(err, os.ErrNotExist) {
			res.Warnings = append(res.Warnings, fmt.Sprintf("config-snapshot.yaml not found in %s; input/config.yaml left empty", opts.SessionDir))
			if werr := os.WriteFile(dstCfg, []byte("# config-snapshot.yaml was missing in the source session.\n# Paste in the engine config you want this case to run against.\n"), 0o644); werr != nil {
				return nil, fmt.Errorf("placeholder config: %w", werr)
			}
		} else {
			return nil, fmt.Errorf("copy config snapshot: %w", err)
		}
	}

	// 3) Open the (now-copied) journal and project envelopes once.
	envs, err := readJournal(dstJournal)
	if err != nil {
		return nil, fmt.Errorf("read journal: %w", err)
	}
	res.EventCount = len(envs)

	// 4) Reconstruct inputs.yaml.
	inputs := ExtractInputs(envs)
	res.InputCount = len(inputs)
	inputsBytes, err := inputsYAMLBytes(inputs)
	if err != nil {
		return nil, fmt.Errorf("inputs.yaml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dstCfgDir, "inputs.yaml"), inputsBytes, 0o644); err != nil {
		return nil, fmt.Errorf("write inputs.yaml: %w", err)
	}

	// 5) Synthesize assertions.yaml.
	assertBytes, err := SynthesizeAssertions(envs)
	if err != nil {
		return nil, fmt.Errorf("synthesize assertions: %w", err)
	}
	assertPath := filepath.Join(caseDir, "assertions.yaml")
	if err := os.WriteFile(assertPath, assertBytes, 0o644); err != nil {
		return nil, fmt.Errorf("write assertions.yaml: %w", err)
	}

	// 6) Write case.yaml. Read metadata for status + model baseline.
	meta := readSessionMeta(filepath.Join(opts.SessionDir, "metadata", "session.json"))
	model := extractModelBaseline(filepath.Join(dstCfgDir, "config.yaml"))
	if model == "" && meta != nil {
		// SessionMeta has no model field today; the config snapshot is the
		// authoritative source. Leaving this branch documented for future use.
	}
	caseYAMLBytes, err := buildCaseYAML(caseEntry{
		Name:          opts.CaseID,
		Description:   opts.Description,
		Tags:          opts.Tags,
		Owner:         resolveOwner(opts.Owner),
		FreshnessDays: 30,
		ModelBaseline: model,
		RecordedAt:    pickNow(opts.Now),
		SessionID:     sessionIDFromDir(opts.SessionDir),
	})
	if err != nil {
		return nil, fmt.Errorf("build case.yaml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(caseDir, "case.yaml"), caseYAMLBytes, 0o644); err != nil {
		return nil, fmt.Errorf("write case.yaml: %w", err)
	}

	// 7) Surface warnings.
	if meta != nil && meta.Status != "" && meta.Status != "completed" && meta.Status != "active" {
		res.Warnings = append(res.Warnings, fmt.Sprintf("session ended with status=%q; promoted as failure-reproduction case", meta.Status))
	}
	o := observe(envs)
	if len(o.NonReplayableHits) > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("journal contains non-replayable event types %v; replay short-circuits side-effect failures, so these branches will not re-fire — see docs/src/eval/promotion.md", o.NonReplayableHits))
	}

	success = true

	// 8) Optional editor launch — happens after success so a $EDITOR launch
	//    failure does not nuke a perfectly good case dir.
	if opts.OpenEditor {
		if err := launchEditor(ctx, assertPath); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("could not launch editor: %v", err))
		}
	}

	return res, nil
}

// validateSessionDir checks the session directory has the expected shape.
func validateSessionDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("session dir %q: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("session path %q is not a directory", dir)
	}
	journalDir := filepath.Join(dir, "journal")
	if jinfo, err := os.Stat(journalDir); err != nil {
		return fmt.Errorf("session journal dir %q: %w", journalDir, err)
	} else if !jinfo.IsDir() {
		return fmt.Errorf("session journal path %q is not a directory", journalDir)
	}
	if _, err := os.Stat(filepath.Join(journalDir, "header.json")); err != nil {
		return fmt.Errorf("journal header missing under %q: %w", journalDir, err)
	}
	metaDir := filepath.Join(dir, "metadata")
	if _, err := os.Stat(metaDir); err != nil {
		return fmt.Errorf("session metadata dir %q: %w", metaDir, err)
	}
	return nil
}

// prepareCaseDir creates the case dir (or replaces it when Force is true).
// Returns an error if the dir already exists and Force is false.
func prepareCaseDir(caseDir string, force bool) error {
	if _, err := os.Stat(caseDir); err == nil {
		if !force {
			return fmt.Errorf("case dir %q already exists (use --force to overwrite)", caseDir)
		}
		if err := os.RemoveAll(caseDir); err != nil {
			return fmt.Errorf("remove existing case dir: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat case dir %q: %w", caseDir, err)
	}
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		return fmt.Errorf("create case dir: %w", err)
	}
	return nil
}

// copyDir recursively copies src into dst (which must not exist; the caller
// is responsible for prep). Preserves regular files byte-for-byte; skips
// special files (sockets, devices) — journals never include them.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(path, target)
	})
}

// copyFile copies the contents of src to dst, preserving file mode.
func copyFile(src, dst string) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	info, err := sf.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	df, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer df.Close()
	if _, err := io.Copy(df, sf); err != nil {
		return err
	}
	return nil
}

// readJournal opens a journal directory and materializes every envelope.
// Promote uses the slice in two passes (inputs + scaffold), so reading once
// is more efficient than two journal.Iter calls.
func readJournal(dir string) ([]journal.Envelope, error) {
	r, err := journal.Open(dir)
	if err != nil {
		return nil, err
	}
	var out []journal.Envelope
	if err := r.Iter(func(e journal.Envelope) bool {
		out = append(out, e)
		return true
	}); err != nil {
		return nil, err
	}
	// Defensive sort: the reader iterates segments in seq order already, but
	// being explicit guards against future segment-ordering changes.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// caseEntry is the minimal mirror of evalcase.Meta with an extra session_id
// hook used in the synthesized description.
type caseEntry struct {
	Name          string
	Description   string
	Tags          []string
	Owner         string
	FreshnessDays int
	ModelBaseline string
	RecordedAt    time.Time
	SessionID     string
}

// buildCaseYAML serializes a case-meta block. We render a top-level mapping
// with a header comment so the resulting case.yaml is self-explanatory.
func buildCaseYAML(c caseEntry) ([]byte, error) {
	desc := c.Description
	if desc == "" {
		desc = fmt.Sprintf("Promoted from session %s on %s. Edit this description to capture the\ndesired behaviour the case is locking in.", c.SessionID, c.RecordedAt.Format(time.RFC3339))
	}

	// Build via yaml.Node so we can attach a head comment without polluting
	// the data model.
	doc := &yaml.Node{Kind: yaml.DocumentNode}
	root := &yaml.Node{
		Kind:        yaml.MappingNode,
		HeadComment: "Auto-generated by `nexus eval promote`. Edit freely; reference\ndocs/src/eval/case-format.md for every field's semantics.",
	}
	doc.Content = []*yaml.Node{root}

	addStr := func(k, v string) {
		valNode := &yaml.Node{Kind: yaml.ScalarNode, Value: v, Tag: "!!str"}
		if hasNewline(v) {
			valNode.Style = yaml.LiteralStyle
		}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: k, Tag: "!!str"},
			valNode,
		)
	}
	addInt := func(k string, v int) {
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: k, Tag: "!!str"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("%d", v), Tag: "!!int"},
		)
	}
	addStrSeq := func(k string, vs []string) {
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: k, Tag: "!!str"},
			stringSeqNode(vs),
		)
	}

	addStr("name", c.Name)
	addStr("description", desc)
	addStrSeq("tags", c.Tags)
	addStr("owner", c.Owner)
	addInt("freshness_days", c.FreshnessDays)
	addStr("model_baseline", c.ModelBaseline)
	addStr("recorded_at", c.RecordedAt.Format(time.RFC3339))

	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// hasNewline reports whether s contains a literal newline. Used to pick
// literal-block style for multi-line case.yaml description fields so the
// resulting YAML stays readable.
func hasNewline(s string) bool {
	for _, r := range s {
		if r == '\n' {
			return true
		}
	}
	return false
}

// stringSeqNode produces a flow-style sequence node from a string slice. A
// nil/empty slice renders as `[]`.
func stringSeqNode(vs []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	for _, v := range vs {
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: v, Tag: "!!str"})
	}
	return n
}

// resolveOwner picks the owner string per the documented fallback chain.
func resolveOwner(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}

// pickNow returns t when non-zero, else time.Now().UTC(). Tests pin the
// timestamp; production lets it float.
func pickNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t.UTC()
}

// sessionIDFromDir returns the basename of the session directory, used in
// the synthesized description and warnings.
func sessionIDFromDir(dir string) string {
	return filepath.Base(filepath.Clean(dir))
}

// minimalSessionMeta is the shape of metadata/session.json we read for
// status surfacing. We don't import engine.SessionMeta to keep the promote
// package free of an engine dependency (cycle hazard with allplugins).
type minimalSessionMeta struct {
	Status string `json:"status"`
}

// readSessionMeta reads the session.json file. Returns nil on any failure;
// missing metadata is non-fatal.
func readSessionMeta(path string) *minimalSessionMeta {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m minimalSessionMeta
	if err := jsonUnmarshal(data, &m); err != nil {
		return nil
	}
	return &m
}

// extractModelBaseline reads core.models.default from the case's
// input/config.yaml. Returns "" if the key is absent or the file is
// unreadable; the case author can fill the field in by hand.
func extractModelBaseline(configPath string) string {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	var doc struct {
		Core struct {
			Models map[string]any `yaml:"models"`
		} `yaml:"core"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return ""
	}
	if v, ok := doc.Core.Models["default"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// launchEditor exec's $EDITOR (with the documented fallback chain) on path.
// The child inherits stdin/stdout/stderr so terminal editors run normally.
func launchEditor(ctx context.Context, path string) error {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	for _, candidate := range []string{editor, "nano", "vi"} {
		if candidate == "" {
			continue
		}
		bin, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		cmd := exec.CommandContext(ctx, bin, path)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	return fmt.Errorf("no editor available (set $EDITOR or install nano/vi)")
}
