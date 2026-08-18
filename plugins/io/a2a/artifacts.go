package a2a

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file owns everything a turn publishes as an A2A Artifact, and the bound
// that keeps it from growing without limit.
//
// # What becomes an artifact
//
// A2A puts task OUTPUT in artifacts and conversation in messages (specification
// section 3.7). Four things a Nexus turn produces are output by that reading:
//
//   - The final assistant text. One artifact per task, as of E1-S4.
//   - Structured output, as a Part with mediaType application/json rather than a
//     JSON document flattened into a string a client has to re-parse.
//   - Every tool result, UNCONDITIONALLY. A tool result is work the agent did on
//     the client's behalf, and it is the single most useful thing an A2A client
//     can be shown during a long turn. This is deliberately not behind a flag:
//     an interop transport whose observability depends on the operator having
//     turned it on is one whose partners cannot rely on it.
//   - Every file the turn wrote, with its contents inline as a base64 raw Part.
//
// # What does NOT become an artifact
//
// A human-in-the-loop question. E2-S3 records it on the INPUT_REQUIRED status
// message and in the task's message history, which is where a request for input
// belongs: it is an inbound solicitation, not output. Making it an artifact as
// well would put a question in the output channel and count one event twice.
//
// # File detection is tool-result-based, and misses uninstrumented writes
//
// A file becomes an artifact only when a tool.result names it: either through
// events.ToolResult.OutputFile, or through a structured-output key named by
// artifacts.file_sources (nexus.tool.fileio's write_file reports "path", which
// is the default rule). Snapshot-diffing the session workspace is explicitly out
// of scope, so a file written by a path that reports nothing — a shell command
// redirecting to a file, a tool that writes silently — is NOT published. That is
// a limitation, stated plainly rather than papered over: the transport reports
// what the bus tells it and does not go looking.
//
// # The bound, and why it is load-bearing
//
// Unconditional tool-result artifacts, times inline base64 file parts, times a
// task store that persists every artifact to disk, is an unbounded product. Three
// caps make it a bounded one:
//
//	per artifact:  max_file_bytes (inline file contents)
//	               max_tool_output_bytes (tool result text)
//	per task:      max_task_bytes (every artifact except the final response)
//	per store:     max_task_bytes x tasks.max_per_context
//
// Exceeding a cap DEGRADES rather than drops or explodes: an oversized file
// becomes a metadata note naming the file and its size, an oversized tool output
// is truncated with a note saying so, and a task that exhausts its budget emits
// one notice artifact recording how many artifacts were suppressed. A client is
// always told what it is not being shown.

// Artifact-policy defaults.
//
// The numbers are chosen against the product above rather than in isolation.
// With tasks.max_per_context at its own default of 200, defaultMaxTaskBytes
// bounds the artifact side of one session's store at roughly 200 MiB in the
// worst case where every retained task saturates its budget — which no ordinary
// session approaches, since a turn's artifacts are the tool outputs it actually
// produced. An operator who cannot afford that worst case lowers either knob;
// the product is documented so the choice is arithmetic rather than a guess.
//
// defaultMaxFileBytes is 256 KiB. Inline content is base64 in JSON, so it costs
// about a third more on the wire than on disk; a quarter-megabyte source file,
// config or report rides comfortably, and anything larger is a payload a client
// should be fetching by other means rather than receiving as a side effect of a
// turn it asked for.
//
// defaultMaxToolOutputBytes is 16 KiB, which is far more than a tool result a
// model can usefully consume — the same output is going into the model's own
// context, where it is bounded by the context window long before this.
const (
	defaultMaxFileBytes       = 256 * 1024
	defaultMaxToolOutputBytes = 16 * 1024
	defaultMaxTaskBytes       = 1024 * 1024
)

// budgetNoticeReserve is the slice of a task's budget held back so the
// "artifacts were suppressed" notice can always be emitted.
//
// Without it, the notice would be the first thing an exhausted budget refused,
// and a client would see artifacts simply stop with no explanation — which is
// the silent-drop outcome this whole file exists to avoid.
const budgetNoticeReserve = 1024

// defaultFileSourceTool and defaultFileSourceKey are the one file-reporting rule
// shipped out of the box: nexus.tool.fileio's write_file reports the path it
// wrote under the "path" key of its structured output.
//
// Shell is deliberately absent. nexus.tool.shell reports stdout, stderr and an
// exit code — it does not report what the command touched — so there is no
// honest default rule for it. An operator whose shell wrapper DOES report a
// written path adds it to artifacts.file_sources; that is what "and shell where
// configured" means here, and pretending otherwise would advertise detection
// that cannot happen.
const (
	defaultFileSourceTool = "write_file"
	defaultFileSourceKey  = "path"
)

// artifactPolicy is the resolved `artifacts:` configuration block.
type artifactPolicy struct {
	// maxFileBytes caps the inline content of one file artifact. A file over it
	// degrades to a metadata note. Zero means no file is ever inlined: every
	// detected file becomes a note.
	maxFileBytes int
	// maxToolOutputBytes caps the text part of one tool-result artifact. Longer
	// output is truncated with a note. Zero disables the cap.
	maxToolOutputBytes int
	// maxTaskBytes is one task's total artifact budget, excluding the final
	// response artifact. Zero disables the budget, which is a foot-gun the
	// reference documents rather than hides.
	maxTaskBytes int
	// fileBaseDir is the directory reported file paths are resolved against and
	// confined to. Empty disables file artifacts entirely — there is nowhere
	// safe to resolve a relative path to, and reading an absolute path a tool
	// reported without a containing directory to check it against would let a
	// tool exfiltrate any file the process can read.
	fileBaseDir string
	// fileSources maps a tool name to the structured-output keys that carry the
	// paths it wrote. events.ToolResult.OutputFile is always honoured on top of
	// this, for every tool, since it is the engine's own field for exactly this.
	fileSources map[string][]string
}

// defaultArtifactPolicy is the policy before any config is applied. fileBaseDir
// is filled in from the session workspace at Init; it has no config-independent
// default.
func defaultArtifactPolicy() artifactPolicy {
	return artifactPolicy{
		maxFileBytes:       defaultMaxFileBytes,
		maxToolOutputBytes: defaultMaxToolOutputBytes,
		maxTaskBytes:       defaultMaxTaskBytes,
		fileSources:        map[string][]string{defaultFileSourceTool: {defaultFileSourceKey}},
	}
}

// parseArtifacts resolves the `artifacts:` block, starting from the supplied
// defaults so a block that sets one knob leaves the others alone.
//
// file_sources REPLACES the default rule wholesale rather than merging with it,
// which is the same rule the engine applies to configuration for a required
// plugin: an operator who writes the key is stating the whole set, and a
// field-level merge would make "no rules at all" inexpressible.
func parseArtifacts(raw any, defaults artifactPolicy) (artifactPolicy, error) {
	out := defaults
	if raw == nil {
		return out, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return out, fmt.Errorf("%s: %s: want a mapping, got %T", pluginID, cfgKeyArtifacts, raw)
	}

	for key, target := range map[string]*int{
		cfgKeyArtifactsMaxFile:    &out.maxFileBytes,
		cfgKeyArtifactsMaxToolOut: &out.maxToolOutputBytes,
		cfgKeyArtifactsMaxTask:    &out.maxTaskBytes,
	} {
		v, present := m[key]
		if !present || v == nil {
			continue
		}
		n, err := configInt(v)
		if err != nil {
			return out, fmt.Errorf("%s: %s.%s: %w", pluginID, cfgKeyArtifacts, key, err)
		}
		if n < 0 {
			return out, fmt.Errorf("%s: %s.%s: must not be negative; use 0 to disable the cap",
				pluginID, cfgKeyArtifacts, key)
		}
		*target = n
	}

	if v, ok := m[cfgKeyArtifactsFileBaseDir].(string); ok && strings.TrimSpace(v) != "" {
		out.fileBaseDir = engine.ExpandPath(strings.TrimSpace(v))
	}

	if v, present := m[cfgKeyArtifactsFileSources]; present && v != nil {
		sources, err := parseFileSources(v)
		if err != nil {
			return out, err
		}
		out.fileSources = sources
	}
	return out, nil
}

// parseFileSources reads the tool-name -> structured-key mapping.
func parseFileSources(raw any) (map[string][]string, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: %s.%s: want a mapping of tool name to structured-output keys, got %T",
			pluginID, cfgKeyArtifacts, cfgKeyArtifactsFileSources, raw)
	}
	out := make(map[string][]string, len(m))
	for tool, v := range m {
		keys := optStringList(v)
		if len(keys) == 0 {
			return nil, fmt.Errorf("%s: %s.%s.%s: name at least one structured-output key carrying a written path",
				pluginID, cfgKeyArtifacts, cfgKeyArtifactsFileSources, tool)
		}
		out[tool] = keys
	}
	return out, nil
}

// ---- the per-task budget ----

// artifactBudget is one task's allowance for everything except its final
// response artifact.
//
// It is not a rate limit and it does not block: an artifact that does not fit is
// counted and dropped, and the first drop mints the notice that tells the client
// so. The reserve guarantees the notice itself always fits.
//
// It is NOT safe for concurrent use; the run holds its lock across every call.
type artifactBudget struct {
	// remaining is the unspent allowance. Negative capacity is impossible: a
	// charge that would overrun is refused rather than taken.
	remaining int
	// unlimited disables accounting entirely (max_task_bytes: 0).
	unlimited bool
	// suppressed counts artifacts the budget refused.
	suppressed int
	// noticed records that the suppression notice has been minted once.
	noticed bool
}

func newArtifactBudget(maxTaskBytes int) *artifactBudget {
	if maxTaskBytes <= 0 {
		return &artifactBudget{unlimited: true}
	}
	remaining := maxTaskBytes - budgetNoticeReserve
	if remaining < 0 {
		remaining = 0
	}
	return &artifactBudget{remaining: remaining}
}

// charge reports whether an artifact of size bytes fits, and spends the
// allowance when it does. A refusal increments the suppressed count.
func (b *artifactBudget) charge(size int) bool {
	if b.unlimited {
		return true
	}
	if size > b.remaining {
		b.suppressed++
		return false
	}
	b.remaining -= size
	return true
}

// needsNotice reports whether a suppression notice is owed, and marks it minted.
func (b *artifactBudget) needsNotice() bool {
	if b.suppressed == 0 || b.noticed {
		return false
	}
	b.noticed = true
	return true
}

// artifactSize is the cost an artifact is charged, measured as the bytes it
// occupies on the wire and in the store. Both are the JSON encoding, so one
// measurement covers both.
func artifactSize(a a2a.Artifact) int {
	encoded, err := json.Marshal(a)
	if err != nil {
		// Unencodable content cannot be published at all; charging a nominal
		// cost keeps the budget monotonic and the emit path will fail its own
		// encode later.
		return budgetNoticeReserve
	}
	return len(encoded)
}

// ---- artifact construction ----

// Artifact metadata keys. They are plain, namespaced keys rather than Nexus
// extension payloads: an artifact is canonical A2A output, and a client that
// does not speak the extension should still be able to tell which tool produced
// what without decoding anything.
const (
	metadataToolName   = "nexus.tool.name"
	metadataToolCallID = "nexus.tool.callId"
	metadataToolFailed = "nexus.tool.failed"
	metadataTruncated  = "nexus.artifact.truncated"
	metadataOmitted    = "nexus.artifact.omitted"
	metadataFilePath   = "nexus.file.path"
	metadataFileSize   = "nexus.file.bytes"
	metadataSuppressed = "nexus.artifact.suppressedCount"
	metadataJSONSchema = "nexus.output.schema"
)

// mediaTypeJSON is the media type structured output and structured tool results
// are published under.
//
// It is the IANA type, deliberately NOT a2a.ContentTypeJSON — that constant is
// application/a2a+json, the media type of the PROTOCOL's own request and
// response documents. An artifact part carrying a tool's structured result is
// not an A2A document, and labelling it as one would tell a client to decode it
// as a protocol envelope.
const mediaTypeJSON = "application/json"

// mediaTypeBinary is the fallback for a file whose extension names nothing.
const mediaTypeBinary = "application/octet-stream"

// toolResultArtifact renders one tool result.
//
// The text part is always present, because a2a.Artifact requires at least one
// part and a result with neither output nor error is still a fact worth
// reporting — the tool ran and said nothing. Structured output rides alongside
// as a real JSON part, so a client gets the typed value the tool produced rather
// than a string it has to parse back.
func toolResultArtifact(taskID string, seq int, res events.ToolResult, pol artifactPolicy, allowNonText bool) a2a.Artifact {
	id := res.ID
	if id == "" {
		id = fmt.Sprintf("seq-%d", seq)
	}
	art := a2a.Artifact{
		ArtifactID:  taskID + "-tool-" + id,
		Name:        res.Name,
		Description: fmt.Sprintf("Result of the %s tool call.", res.Name),
		Metadata: map[string]any{
			metadataToolName:   res.Name,
			metadataToolCallID: res.ID,
		},
	}

	body := res.Output
	if res.Error != "" {
		art.Metadata[metadataToolFailed] = true
		if body == "" {
			body = res.Error
		} else {
			body += "\n\nERROR: " + res.Error
		}
	}
	if body == "" {
		body = "(the tool produced no output)"
	}
	body, truncated := truncate(body, pol.maxToolOutputBytes)
	if truncated {
		art.Metadata[metadataTruncated] = true
	}
	art.Parts = append(art.Parts, a2a.TextPart(body))

	if allowNonText && len(res.OutputStructured) > 0 {
		if part, err := jsonPart(res.OutputStructured); err == nil {
			art.Parts = append(art.Parts, part)
		}
	}
	return art
}

// truncate cuts s to at most limit bytes on a rune boundary, appending a note
// that says so. A zero or negative limit disables the cut.
func truncate(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n\n[truncated: %d of %d bytes shown]", cut, len(s)), true
}

// isRuneStart reports whether b begins a UTF-8 rune, so a cut never splits one.
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// jsonPart builds a Part carrying a value as real JSON under the IANA media
// type, so a client reads a document rather than a quoted string.
func jsonPart(value any) (a2a.Part, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return a2a.Part{}, fmt.Errorf("encoding structured content as a json part: %w", err)
	}
	return a2a.Part{Data: json.RawMessage(raw), MediaType: mediaTypeJSON}, nil
}

// ---- file artifacts ----

// writtenFile is one file a tool result reported having written.
type writtenFile struct {
	// reported is the path exactly as the tool named it, used in messages so an
	// operator sees what the tool said rather than a rewritten form.
	reported string
	// resolved is the absolute path, already confined to the policy's base dir.
	resolved string
}

// detectWrittenFiles reads the paths a tool result reports having written.
//
// Two sources, both explicit: events.ToolResult.OutputFile, which the engine
// defines as "file written to session workspace" and which therefore applies to
// every tool; and the structured-output keys the policy names for this tool.
// Nothing is inferred from the tool's text output — a heuristic that scraped
// paths out of prose would publish files the agent merely mentioned.
//
// A path that escapes the base directory is DROPPED, not clamped. A tool that
// reports "../../.ssh/id_rsa" is either broken or hostile, and inlining the file
// it named into a response that leaves the process is the one outcome that
// cannot be walked back.
func detectWrittenFiles(res events.ToolResult, pol artifactPolicy) []writtenFile {
	if pol.fileBaseDir == "" {
		return nil
	}
	var reported []string
	if res.OutputFile != "" {
		reported = append(reported, res.OutputFile)
	}
	for _, key := range pol.fileSources[res.Name] {
		if v, ok := res.OutputStructured[key].(string); ok && strings.TrimSpace(v) != "" {
			reported = append(reported, strings.TrimSpace(v))
		}
	}

	seen := make(map[string]bool, len(reported))
	var out []writtenFile
	for _, path := range reported {
		resolved, ok := confine(path, pol.fileBaseDir)
		if !ok || seen[resolved] {
			continue
		}
		seen[resolved] = true
		out = append(out, writtenFile{reported: path, resolved: resolved})
	}
	// Deterministic order, so the artifact sequence a turn produces does not
	// depend on Go's map iteration.
	sort.Slice(out, func(i, j int) bool { return out[i].resolved < out[j].resolved })
	return out
}

// confine resolves path against base and reports whether the result stays
// inside it, symlinks included.
func confine(path, base string) (string, bool) {
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", false
	}
	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(absBase, candidate)
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return "", false
	}
	// Symlinks are followed on both sides before the comparison: a link INSIDE
	// the base directory pointing outside it is the interesting case, and a
	// lexical check alone would admit it. EvalSymlinks fails on a path that does
	// not exist, which is not an error here — the containment check then runs
	// lexically and the read that follows reports the missing file.
	if real, err := filepath.EvalSymlinks(candidate); err == nil {
		candidate = real
	}
	if real, err := filepath.EvalSymlinks(absBase); err == nil {
		absBase = real
	}
	rel, err := filepath.Rel(absBase, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return candidate, true
}

// fileArtifact renders one written file.
//
// Contents ride as an inline base64 raw Part when they fit under
// max_file_bytes. A file OVER the cap degrades to a metadata note — an artifact
// naming the file, its size and the cap that excluded it — rather than being
// dropped silently or inlined anyway. The note is the whole point of the
// degradation: a client is told the file exists and why it is not attached, so
// it can go and fetch it by whatever means the deployment provides.
//
// It reports false for a path that is not a regular readable file, which covers
// a directory, a device node and a tool that reported a path it did not
// actually write.
func fileArtifact(taskID string, f writtenFile, pol artifactPolicy, allowNonText bool) (a2a.Artifact, bool) {
	info, err := os.Stat(f.resolved)
	if err != nil || !info.Mode().IsRegular() {
		return a2a.Artifact{}, false
	}
	name := filepath.Base(f.resolved)
	art := a2a.Artifact{
		// The reported path is what makes the id unique within the task: two
		// writes to the SAME file replace one artifact rather than accumulating
		// one per write, which is what a client following a turn wants to see.
		ArtifactID:  taskID + "-file-" + artifactIDToken(f.reported),
		Name:        name,
		Description: fmt.Sprintf("File written during this turn: %s", f.reported),
		Metadata: map[string]any{
			metadataFilePath: f.reported,
			metadataFileSize: info.Size(),
		},
	}

	oversize := pol.maxFileBytes <= 0 || info.Size() > int64(pol.maxFileBytes)
	if oversize || !allowNonText {
		var why string
		switch {
		case !allowNonText:
			why = "the request accepts text output only"
		case pol.maxFileBytes <= 0:
			why = "this agent is configured not to inline file contents (artifacts.max_file_bytes is 0)"
		default:
			why = fmt.Sprintf("the file exceeds the %d byte inline limit", pol.maxFileBytes)
		}
		reason := fmt.Sprintf("%s (%d bytes) was written during this turn. "+
			"Its contents are not attached: %s.", f.reported, info.Size(), why)
		art.Metadata[metadataOmitted] = true
		art.Parts = []a2a.Part{a2a.TextPart(reason)}
		return art, true
	}

	data, err := os.ReadFile(f.resolved)
	if err != nil {
		return a2a.Artifact{}, false
	}
	art.Parts = []a2a.Part{a2a.RawPart(data, fileMediaType(name), name)}
	return art, true
}

// maxArtifactIDToken bounds the path-derived half of a file artifact's id. The
// id is a primary-key column in the task store, so an unbounded one would let a
// deeply nested path cost more to index than the artifact costs to hold.
const maxArtifactIDToken = 96

// artifactIDToken reduces a reported path to something safe to embed in an
// artifact id: stable, unique per path, and free of separators that would read
// as structure to a client parsing the id (which nothing should, but ids travel
// through logs and URLs).
//
// A path too long for the bound is truncated and disambiguated with a hash of
// the whole path, so two long paths sharing a tail stay distinct rather than one
// artifact silently replacing the other.
func artifactIDToken(path string) string {
	var b strings.Builder
	for _, r := range path {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	token := b.String()
	if len(token) <= maxArtifactIDToken {
		return token
	}
	sum := sha256.Sum256([]byte(path))
	// The TAIL is kept: a path's filename is what distinguishes it to a human
	// reading a task's artifact list, and the directories above it rarely do.
	return token[len(token)-maxArtifactIDToken:] + "-" + hex.EncodeToString(sum[:6])
}

// knownFileMediaTypes is the deterministic half of the media-type mapping.
//
// mime.TypeByExtension consults the HOST's mime.types files, so the same agent
// writing the same .md file would be reported as text/markdown on one machine
// and application/octet-stream on another. A partner's client should not have to
// cope with that, so the extensions an agent actually writes are pinned here and
// the host table is consulted only for everything else.
var knownFileMediaTypes = map[string]string{
	".csv":  "text/csv; charset=utf-8",
	".go":   "text/x-go; charset=utf-8",
	".html": "text/html; charset=utf-8",
	".json": mediaTypeJSON,
	".log":  "text/plain; charset=utf-8",
	".md":   "text/markdown; charset=utf-8",
	".py":   "text/x-python; charset=utf-8",
	".sh":   "text/x-shellscript; charset=utf-8",
	".sql":  "application/sql",
	".toml": "application/toml",
	".ts":   "text/x-typescript; charset=utf-8",
	".txt":  "text/plain; charset=utf-8",
	".yaml": "application/yaml",
	".yml":  "application/yaml",
}

// fileMediaType maps a filename onto a media type, falling back to the binary
// catch-all. The charset parameter is kept where it applies: it is information
// the client would otherwise have to guess.
func fileMediaType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if t, ok := knownFileMediaTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return mediaTypeBinary
}

// ---- the suppression notice ----

// suppressionNotice is the one artifact a task emits when its budget is spent.
//
// It carries the count so far rather than a final total, because it is minted at
// the moment of the first refusal and the turn is still running. The count is
// restated on the same artifact id when the task completes, which the store
// upserts rather than duplicates, so the record ends with the true total and the
// wire carries exactly two frames for it.
func suppressionNotice(taskID string, suppressed int) a2a.Artifact {
	return a2a.Artifact{
		ArtifactID: taskID + "-artifacts-truncated",
		Name:       "artifacts-truncated",
		Description: "Some artifacts this turn produced were not published, " +
			"because the task reached its artifact budget.",
		Parts: []a2a.Part{a2a.TextPart(fmt.Sprintf(
			"%d artifact(s) produced by this turn were not published: the task reached its "+
				"artifact budget (artifacts.max_task_bytes). The task's own answer is unaffected.",
			suppressed))},
		Metadata: map[string]any{metadataSuppressed: suppressed},
	}
}
