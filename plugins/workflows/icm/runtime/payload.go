// Package runtime assembles per-turn XML payloads for ICM sub-agent dispatch.
//
// A payload bundles the grounding (skills + grounding files the agent must
// internalize) and the layer data (declared input artifacts and an optional
// fan-out item) the sub-agent transforms. Optional feedback blocks
// (previous_attempt, previous_iteration) carry retry / loop signal. The
// builder is pure — it reads from the filesystem only, never the event bus
// — so it can be exercised in isolation. The operator system prompt is
// rendered separately at posture-registration time and is NOT produced
// here.
package runtime

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"
	"github.com/frankbardon/nexus/plugins/workflows/icm/session"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// defaultInlineArtifactLimitBytes is the size threshold above which input
// artifacts are emitted as <artifact_ref/> rather than inlined.
const defaultInlineArtifactLimitBytes = 32 * 1024

// PayloadBuilder assembles per-turn XML payloads for sub-agent dispatch.
//
// A single builder is reused across the turns of a workflow run. It is
// stateless beyond its configuration and safe for sequential use.
type PayloadBuilder struct {
	// Workflow is the loaded workspace contract.
	Workflow *workspace.Workflow
	// Session is the active run that owns artifact paths.
	Session *session.Session
	// InlineArtifactLimitBytes is the inline-vs-ref size threshold.
	// Zero falls back to defaultInlineArtifactLimitBytes.
	InlineArtifactLimitBytes int
	// Logger receives WARN entries when artifacts / grounding files are
	// missing or oversized. nil falls back to slog.Default().
	Logger *slog.Logger
}

// PayloadInputs is the per-turn context supplied by the orchestrator.
type PayloadInputs struct {
	// Stage is the stage being dispatched.
	Stage *workspace.Stage
	// Iteration is 1-based for loop iterations; 0 for non-loop stages.
	Iteration int
	// Turn is the 1-based turn within the current invocation.
	Turn int
	// ItemID identifies a fan-out item; "" for non-fan-out stages.
	ItemID string
	// ItemValue is the JSON-marshallable fan-out item value, populated
	// only for fan-out invocations.
	ItemValue any
	// RunID is the active session's run ID, surfaced as an attribute on
	// the root element when non-empty.
	RunID string
	// PreviousAttempt carries the prior turn's output + validator failures
	// for retry feedback within the same invocation.
	PreviousAttempt *PreviousAttempt
	// PreviousIteration carries the prior loop iteration's artifact + exit
	// failures.
	PreviousIteration *PreviousIteration
}

// PreviousAttempt represents one prior turn's output + failures within the
// same invocation (turn-loop retry feedback).
type PreviousAttempt struct {
	// Turn is the 1-based turn number of the prior attempt.
	Turn int
	// Output is the raw output produced by the prior turn.
	Output []byte
	// Failures lists the validator results that triggered the retry.
	// Entries with Verdict == "pass" are filtered out.
	Failures []icmtypes.ConditionResult
	// HumanFeedback is the free-text response from a HITL `continue`
	// action under turn_policy=until_human_approves. Optional.
	HumanFeedback string
}

// PreviousIteration represents the immediately prior loop iteration.
type PreviousIteration struct {
	// Index is the 1-based prior iteration number.
	Index int
	// Artifact is the raw bytes of the prior iteration's artifact.
	Artifact []byte
	// Path is the logical ref ("<stage_id>/<filename>") for the artifact.
	Path string
	// Failures lists the loop exit-condition results that triggered the
	// next iteration. Entries with Verdict == "pass" are filtered out.
	Failures []icmtypes.ConditionResult
}

// Build assembles the user-message XML payload for a single turn.
//
// Pure I/O: reads grounding files and input artifacts from disk; never
// touches the event bus or HITL surfaces. Missing artifacts and missing
// grounding files become <artifact_ref missing="true"/> / <file_ref/>
// elements plus a WARN log entry — no error is returned. Genuine I/O
// failures (e.g. permission denied) propagate.
func (b *PayloadBuilder) Build(in PayloadInputs) (string, error) {
	if in.Stage == nil {
		return "", fmt.Errorf("payload: stage is required")
	}

	var sb strings.Builder

	attrs := []string{"stage", in.Stage.ID, "turn", strconv.Itoa(in.Turn)}
	if in.Iteration > 0 {
		attrs = append(attrs, "iteration", strconv.Itoa(in.Iteration))
	}
	if in.ItemID != "" {
		attrs = append(attrs, "item", in.ItemID)
	}
	if in.RunID != "" {
		attrs = append(attrs, "run_id", in.RunID)
	}
	engine.XMLTag(&sb, "icm_turn", attrs...)

	if err := b.writeGrounding(&sb, in.Stage); err != nil {
		return "", err
	}
	if err := b.writeLayerData(&sb, in); err != nil {
		return "", err
	}
	if in.PreviousAttempt != nil {
		b.writePreviousAttempt(&sb, in.PreviousAttempt)
	}
	if in.PreviousIteration != nil {
		b.writePreviousIteration(&sb, in.PreviousIteration)
	}
	b.writeInstructions(&sb, in.Stage)

	engine.XMLClose(&sb, "icm_turn")
	return sb.String(), nil
}

// ---------------------------------------------------------------------------
// Block writers
// ---------------------------------------------------------------------------

// writeGrounding emits the <grounding> block: skills, stage-local grounding
// files, shared grounding files (in that order).
func (b *PayloadBuilder) writeGrounding(sb *strings.Builder, s *workspace.Stage) error {
	engine.XMLTag(sb, "grounding")

	for name, sk := range s.Skills {
		b.writeSkill(sb, name, sk)
	}

	for _, rel := range s.Inputs.Grounding {
		abs := filepath.Join(s.Folder, "grounding", rel)
		if err := b.writeGroundingFile(sb, "file", rel, abs); err != nil {
			return err
		}
	}

	for _, rel := range s.Inputs.SharedGrounding {
		abs := filepath.Join(b.Workflow.Root, "shared", "grounding", rel)
		if err := b.writeGroundingFile(sb, "shared_file", rel, abs); err != nil {
			return err
		}
	}

	engine.XMLClose(sb, "grounding")
	return nil
}

// writeSkill emits a single <skill> element with optional references.
// The body is always inlined; references appear as <ref> entries.
func (b *PayloadBuilder) writeSkill(sb *strings.Builder, name string, sk *workspace.Skill) {
	engine.XMLTag(sb, "skill", "name", name, "source", string(sk.Source))
	if sk.Description != "" {
		engine.XMLTag(sb, "description")
		sb.WriteString(engine.XMLCDATA(sk.Description))
		sb.WriteByte('\n')
		engine.XMLClose(sb, "description")
	}
	engine.XMLTag(sb, "body")
	sb.WriteString(engine.XMLCDATA(sk.Body))
	sb.WriteByte('\n')
	engine.XMLClose(sb, "body")
	if len(sk.References) > 0 {
		engine.XMLTag(sb, "references_available")
		for _, r := range sk.References {
			if r.Description != "" {
				sb.WriteString(fmt.Sprintf("<ref path=%q description=%q/>\n",
					engine.XMLEscape(r.Path), engine.XMLEscape(r.Description)))
			} else {
				sb.WriteString(fmt.Sprintf("<ref path=%q/>\n", engine.XMLEscape(r.Path)))
			}
		}
		engine.XMLClose(sb, "references_available")
	}
	engine.XMLClose(sb, "skill")
}

// writeGroundingFile emits a grounding file inline, or a *_ref element when
// reading fails / the file exceeds the inline threshold. Grounding is
// expected to be small + stable; oversized files generate a WARN.
func (b *PayloadBuilder) writeGroundingFile(sb *strings.Builder, elemName, relPath, absPath string) error {
	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			b.warn("grounding file missing", "elem", elemName, "path", relPath)
			b.writeRef(sb, elemName+"_ref", relPath, 0, true)
			return nil
		}
		return fmt.Errorf("payload: stat grounding %s: %w", relPath, err)
	}

	limit := b.inlineLimit()
	if info.Size() > int64(limit) {
		b.warn("grounding file exceeds inline threshold; emitting ref",
			"elem", elemName, "path", relPath, "size_bytes", info.Size(), "limit_bytes", limit)
		b.writeRef(sb, elemName+"_ref", relPath, info.Size(), false)
		return nil
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			b.warn("grounding file vanished between stat and read", "elem", elemName, "path", relPath)
			b.writeRef(sb, elemName+"_ref", relPath, 0, true)
			return nil
		}
		return fmt.Errorf("payload: read grounding %s: %w", relPath, err)
	}
	engine.XMLTag(sb, elemName, "path", relPath)
	sb.WriteString(engine.XMLCDATA(string(data)))
	sb.WriteByte('\n')
	engine.XMLClose(sb, elemName)
	return nil
}

// writeLayerData emits the <layer_data> block: declared input artifacts
// (inline or ref) plus the fan-out item when present.
func (b *PayloadBuilder) writeLayerData(sb *strings.Builder, in PayloadInputs) error {
	engine.XMLTag(sb, "layer_data")

	for _, ref := range in.Stage.Inputs.Artifacts {
		if err := b.writeArtifact(sb, ref); err != nil {
			return err
		}
	}

	if in.ItemID != "" && in.Stage.FanOut != nil {
		if err := b.writeFanOutItem(sb, in.Stage.FanOut.ItemVar, in.ItemValue); err != nil {
			return err
		}
	}

	engine.XMLClose(sb, "layer_data")
	return nil
}

// writeArtifact emits a single declared input artifact. Inline-vs-ref is
// decided by: (1) resolvability — unresolved refs become missing=true;
// (2) UTF-8 validity — binary content always uses artifact_ref; (3) size —
// large content uses artifact_ref with size_bytes.
func (b *PayloadBuilder) writeArtifact(sb *strings.Builder, ref string) error {
	abs, err := b.Session.ResolveLogicalRef(ref)
	if err != nil {
		b.warn("artifact ref unresolved", "ref", ref, "error", err)
		b.writeRef(sb, "artifact_ref", ref, 0, true)
		return nil
	}

	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			b.warn("artifact missing at dispatch", "ref", ref, "path", abs)
			b.writeRef(sb, "artifact_ref", ref, 0, true)
			return nil
		}
		return fmt.Errorf("payload: stat artifact %s: %w", ref, err)
	}

	limit := b.inlineLimit()
	if info.Size() > int64(limit) {
		b.writeRef(sb, "artifact_ref", ref, info.Size(), false)
		return nil
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			b.warn("artifact vanished between stat and read", "ref", ref)
			b.writeRef(sb, "artifact_ref", ref, 0, true)
			return nil
		}
		return fmt.Errorf("payload: read artifact %s: %w", ref, err)
	}

	// Binary content (non-UTF-8) is always emitted as a ref so the agent
	// can pick it up via a tool rather than receiving corrupted text.
	if !utf8.Valid(data) {
		b.writeRef(sb, "artifact_ref", ref, info.Size(), false)
		return nil
	}

	engine.XMLTag(sb, "artifact", "path", ref)
	sb.WriteString(engine.XMLCDATA(string(data)))
	sb.WriteByte('\n')
	engine.XMLClose(sb, "artifact")
	return nil
}

// writeRef emits an *_ref element with optional size_bytes / missing flag.
func (b *PayloadBuilder) writeRef(sb *strings.Builder, elem, path string, size int64, missing bool) {
	sb.WriteByte('<')
	sb.WriteString(elem)
	fmt.Fprintf(sb, " path=%q", engine.XMLEscape(path))
	if size > 0 {
		fmt.Fprintf(sb, " size_bytes=%q", strconv.FormatInt(size, 10))
	}
	if missing {
		sb.WriteString(` missing="true"`)
	}
	sb.WriteString("/>\n")
}

// writeFanOutItem emits the <fan_out_item key="..."> wrapper around a
// compact JSON encoding of the item value.
func (b *PayloadBuilder) writeFanOutItem(sb *strings.Builder, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("payload: marshal fan_out_item: %w", err)
	}
	engine.XMLTag(sb, "fan_out_item", "key", key)
	sb.WriteString(engine.XMLCDATA(string(data)))
	sb.WriteByte('\n')
	engine.XMLClose(sb, "fan_out_item")
	return nil
}

// writePreviousAttempt emits the validator-retry feedback block.
func (b *PayloadBuilder) writePreviousAttempt(sb *strings.Builder, pa *PreviousAttempt) {
	engine.XMLTag(sb, "previous_attempt", "turn", strconv.Itoa(pa.Turn))
	engine.XMLTag(sb, "output")
	sb.WriteString(engine.XMLCDATA(string(pa.Output)))
	sb.WriteByte('\n')
	engine.XMLClose(sb, "output")
	b.writeFailures(sb, "validator_feedback", pa.Failures)
	if pa.HumanFeedback != "" {
		engine.XMLTag(sb, "human_feedback")
		sb.WriteString(engine.XMLCDATA(pa.HumanFeedback))
		sb.WriteByte('\n')
		engine.XMLClose(sb, "human_feedback")
	}
	engine.XMLClose(sb, "previous_attempt")
}

// writePreviousIteration emits the loop-iteration feedback block.
func (b *PayloadBuilder) writePreviousIteration(sb *strings.Builder, pi *PreviousIteration) {
	engine.XMLTag(sb, "previous_iteration", "index", strconv.Itoa(pi.Index))
	if pi.Path != "" {
		engine.XMLTag(sb, "artifact", "path", pi.Path)
	} else {
		engine.XMLTag(sb, "artifact")
	}
	sb.WriteString(engine.XMLCDATA(string(pi.Artifact)))
	sb.WriteByte('\n')
	engine.XMLClose(sb, "artifact")
	b.writeFailures(sb, "exit_failures", pi.Failures)
	engine.XMLClose(sb, "previous_iteration")
}

// writeFailures emits a container of <failure> elements. Entries whose
// Verdict is "pass" are filtered out so the agent only sees actionable
// signal. Empty containers are suppressed entirely.
func (b *PayloadBuilder) writeFailures(sb *strings.Builder, container string, fails []icmtypes.ConditionResult) {
	hasFailure := false
	for _, f := range fails {
		if f.Verdict != "pass" {
			hasFailure = true
			break
		}
	}
	if !hasFailure {
		return
	}
	engine.XMLTag(sb, container)
	for _, f := range fails {
		if f.Verdict == "pass" {
			continue
		}
		attrs := []string{}
		if f.Name != "" {
			attrs = append(attrs, "name", f.Name)
		}
		if f.Type != "" {
			attrs = append(attrs, "type", f.Type)
		}
		engine.XMLTag(sb, "failure", attrs...)
		sb.WriteString(engine.XMLCDATA(f.Feedback))
		sb.WriteByte('\n')
		engine.XMLClose(sb, "failure")
	}
	engine.XMLClose(sb, container)
}

// writeInstructions emits the stage's role/process text. Always present
// even when Role is empty so the surrounding XML schema stays uniform.
func (b *PayloadBuilder) writeInstructions(sb *strings.Builder, s *workspace.Stage) {
	engine.XMLTag(sb, "instructions")
	sb.WriteString(engine.XMLCDATA(s.Role))
	sb.WriteByte('\n')
	engine.XMLClose(sb, "instructions")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// inlineLimit returns the configured inline threshold, defaulting to
// defaultInlineArtifactLimitBytes when unset or non-positive.
func (b *PayloadBuilder) inlineLimit() int {
	if b.InlineArtifactLimitBytes > 0 {
		return b.InlineArtifactLimitBytes
	}
	return defaultInlineArtifactLimitBytes
}

// warn routes a WARN-level message through the configured logger, falling
// back to slog.Default() when none was supplied.
func (b *PayloadBuilder) warn(msg string, args ...any) {
	logger := b.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("icm.payload: "+msg, args...)
}
