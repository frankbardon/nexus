package runtime

import (
	"bytes"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"
	"github.com/frankbardon/nexus/plugins/workflows/icm/session"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// --- helpers ---------------------------------------------------------------

// setup creates a minimal hermetic environment: a workspace root with a
// stage folder + shared grounding scaffolding, plus a session under a
// distinct data dir. It does NOT write any artifacts or grounding files;
// individual tests do that.
type fixture struct {
	t            *testing.T
	workspaceDir string
	stageDir     string
	groundingDir string
	sharedDir    string
	session      *session.Session
	workflow     *workspace.Workflow
}

func setup(t *testing.T) *fixture {
	t.Helper()
	tmp := t.TempDir()
	workspaceDir := filepath.Join(tmp, "ws")
	stageDir := filepath.Join(workspaceDir, "stages", "01_draft")
	groundingDir := filepath.Join(stageDir, "grounding")
	sharedDir := filepath.Join(workspaceDir, "shared", "grounding")
	for _, d := range []string{groundingDir, sharedDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	dataDir := filepath.Join(tmp, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	sess, err := session.NewSession(dataDir, "run_test", nil)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	return &fixture{
		t:            t,
		workspaceDir: workspaceDir,
		stageDir:     stageDir,
		groundingDir: groundingDir,
		sharedDir:    sharedDir,
		session:      sess,
		workflow: &workspace.Workflow{
			Root: workspaceDir,
		},
	}
}

func (f *fixture) writeStageArtifact(stage, filename string, data []byte) string {
	f.t.Helper()
	path := f.session.ArtifactPath(stage, filename)
	if err := f.session.WriteArtifact(path, data); err != nil {
		f.t.Fatalf("WriteArtifact: %v", err)
	}
	return path
}

func (f *fixture) writeGroundingFile(rel string, data []byte) {
	f.t.Helper()
	abs := filepath.Join(f.groundingDir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		f.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, data, 0o644); err != nil {
		f.t.Fatalf("write grounding: %v", err)
	}
}

func (f *fixture) builder() *PayloadBuilder {
	return &PayloadBuilder{
		Workflow: f.workflow,
		Session:  f.session,
	}
}

// parseXML decodes the payload into a generic tree for structural assertions.
// Attribute ordering and whitespace are not load-bearing.
type xmlNode struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Children []xmlNode  `xml:",any"`
	Text     string     `xml:",chardata"`
}

func parse(t *testing.T, payload string) xmlNode {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(payload))
	var root xmlNode
	if err := dec.Decode(&root); err != nil && err != io.EOF {
		t.Fatalf("decode payload: %v\npayload:\n%s", err, payload)
	}
	return root
}

func (n xmlNode) child(name string) *xmlNode {
	for i := range n.Children {
		if n.Children[i].XMLName.Local == name {
			return &n.Children[i]
		}
	}
	return nil
}

func (n xmlNode) childrenNamed(name string) []xmlNode {
	out := []xmlNode{}
	for _, c := range n.Children {
		if c.XMLName.Local == name {
			out = append(out, c)
		}
	}
	return out
}

func (n xmlNode) attr(name string) string {
	for _, a := range n.Attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected substring %q in:\n%s", needle, haystack)
	}
}

func mustNotContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("did not expect substring %q in:\n%s", needle, haystack)
	}
}

// --- tests -----------------------------------------------------------------

// 1. Happy path: stage + 1 grounding file + 1 small inline artifact.
func TestBuild_HappyPath(t *testing.T) {
	f := setup(t)
	f.writeGroundingFile("constraint.md", []byte("style: terse"))
	f.writeStageArtifact("00_input", "brief.md", []byte("write a draft"))

	stage := &workspace.Stage{
		ID:     "01_draft",
		Folder: f.stageDir,
		Role:   "you are a drafter",
		Inputs: workspace.InputScope{
			Grounding: []string{"constraint.md"},
			Artifacts: []string{"00_input/brief.md"},
		},
	}

	out, err := f.builder().Build(PayloadInputs{
		Stage:  stage,
		Turn:   1,
		RunID:  "run_test",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	root := parse(t, out)
	if root.XMLName.Local != "icm_turn" {
		t.Fatalf("root = %q, want icm_turn", root.XMLName.Local)
	}
	if root.attr("stage") != "01_draft" {
		t.Errorf("stage attr = %q", root.attr("stage"))
	}
	if root.attr("turn") != "1" {
		t.Errorf("turn attr = %q", root.attr("turn"))
	}
	if root.attr("run_id") != "run_test" {
		t.Errorf("run_id attr = %q", root.attr("run_id"))
	}
	if root.attr("iteration") != "" {
		t.Errorf("iteration attr should be absent, got %q", root.attr("iteration"))
	}
	if root.attr("item") != "" {
		t.Errorf("item attr should be absent, got %q", root.attr("item"))
	}

	g := root.child("grounding")
	if g == nil {
		t.Fatalf("no <grounding>")
	}
	files := g.childrenNamed("file")
	if len(files) != 1 || files[0].attr("path") != "constraint.md" {
		t.Errorf("grounding files = %+v", files)
	}

	ld := root.child("layer_data")
	if ld == nil {
		t.Fatalf("no <layer_data>")
	}
	arts := ld.childrenNamed("artifact")
	if len(arts) != 1 || arts[0].attr("path") != "00_input/brief.md" {
		t.Errorf("artifacts = %+v", arts)
	}
	mustContain(t, arts[0].Text, "write a draft")

	instr := root.child("instructions")
	if instr == nil {
		t.Fatalf("no <instructions>")
	}
	mustContain(t, instr.Text, "you are a drafter")
}

// 2. Large artifact emits a ref, not inline content.
func TestBuild_LargeArtifactEmitsRef(t *testing.T) {
	f := setup(t)
	// 64KB of "A" — comfortably above the 32KB default.
	big := bytes.Repeat([]byte("A"), 64*1024)
	f.writeStageArtifact("00_input", "huge.txt", big)

	stage := &workspace.Stage{
		ID:     "01_draft",
		Folder: f.stageDir,
		Inputs: workspace.InputScope{Artifacts: []string{"00_input/huge.txt"}},
	}

	out, err := f.builder().Build(PayloadInputs{Stage: stage, Turn: 1})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	root := parse(t, out)
	ld := root.child("layer_data")
	if ld == nil {
		t.Fatalf("no <layer_data>")
	}
	if len(ld.childrenNamed("artifact")) != 0 {
		t.Errorf("expected no inline artifact, got one")
	}
	refs := ld.childrenNamed("artifact_ref")
	if len(refs) != 1 {
		t.Fatalf("expected one artifact_ref, got %d", len(refs))
	}
	if refs[0].attr("path") != "00_input/huge.txt" {
		t.Errorf("ref path = %q", refs[0].attr("path"))
	}
	if refs[0].attr("size_bytes") == "" {
		t.Errorf("ref missing size_bytes")
	}
	if refs[0].attr("missing") == "true" {
		t.Errorf("ref incorrectly flagged missing")
	}
	mustNotContain(t, out, "AAAA") // body not inlined
}

// 3. Binary artifact (invalid UTF-8) always uses a ref even when small.
func TestBuild_BinaryArtifactAlwaysRef(t *testing.T) {
	f := setup(t)
	// Bytes 0xFF 0xFE are not valid UTF-8 in isolation.
	f.writeStageArtifact("00_input", "image.bin", []byte{0xFF, 0xFE, 0x00, 0xC0})

	stage := &workspace.Stage{
		ID:     "01_draft",
		Folder: f.stageDir,
		Inputs: workspace.InputScope{Artifacts: []string{"00_input/image.bin"}},
	}

	out, err := f.builder().Build(PayloadInputs{Stage: stage, Turn: 1})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	root := parse(t, out)
	ld := root.child("layer_data")
	if len(ld.childrenNamed("artifact")) != 0 {
		t.Errorf("expected no inline artifact for binary content")
	}
	refs := ld.childrenNamed("artifact_ref")
	if len(refs) != 1 {
		t.Fatalf("expected one artifact_ref, got %d", len(refs))
	}
	if refs[0].attr("missing") == "true" {
		t.Errorf("binary ref should not be marked missing")
	}
}

// 4. Missing artifact -> ref with missing="true" and no error.
func TestBuild_MissingArtifactEmitsMissingRef(t *testing.T) {
	f := setup(t)
	stage := &workspace.Stage{
		ID:     "01_draft",
		Folder: f.stageDir,
		Inputs: workspace.InputScope{Artifacts: []string{"00_input/ghost.md"}},
	}

	out, err := f.builder().Build(PayloadInputs{Stage: stage, Turn: 1})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	root := parse(t, out)
	refs := root.child("layer_data").childrenNamed("artifact_ref")
	if len(refs) != 1 {
		t.Fatalf("expected one artifact_ref, got %d", len(refs))
	}
	if refs[0].attr("missing") != "true" {
		t.Errorf("expected missing=\"true\", got %q", refs[0].attr("missing"))
	}
	if refs[0].attr("path") != "00_input/ghost.md" {
		t.Errorf("path = %q", refs[0].attr("path"))
	}
}

// 5. Skill with references emits <skill> + <references_available>.
func TestBuild_SkillWithReferences(t *testing.T) {
	f := setup(t)
	stage := &workspace.Stage{
		ID:     "01_draft",
		Folder: f.stageDir,
		Skills: map[string]*workspace.Skill{
			"writing-style": {
				Name:        "writing-style",
				Description: "house style guide",
				Source:      workspace.SkillWorkspace,
				Body:        "use active voice",
				References: []workspace.SkillRef{
					{Path: "voice.md", Description: "voice notes"},
					{Path: "examples/before-after.md"},
				},
			},
		},
	}

	out, err := f.builder().Build(PayloadInputs{Stage: stage, Turn: 1})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	root := parse(t, out)
	g := root.child("grounding")
	if g == nil {
		t.Fatalf("no <grounding>")
	}
	skills := g.childrenNamed("skill")
	if len(skills) != 1 {
		t.Fatalf("expected one skill, got %d", len(skills))
	}
	sk := skills[0]
	if sk.attr("name") != "writing-style" || sk.attr("source") != "workspace" {
		t.Errorf("skill attrs = %+v", sk.Attrs)
	}
	body := sk.child("body")
	if body == nil || !strings.Contains(body.Text, "use active voice") {
		t.Errorf("body missing or wrong: %+v", body)
	}
	refs := sk.child("references_available")
	if refs == nil {
		t.Fatalf("no <references_available>")
	}
	refEntries := refs.childrenNamed("ref")
	if len(refEntries) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refEntries))
	}
	if refEntries[0].attr("path") != "voice.md" {
		t.Errorf("first ref path = %q", refEntries[0].attr("path"))
	}
	if refEntries[0].attr("description") != "voice notes" {
		t.Errorf("first ref description = %q", refEntries[0].attr("description"))
	}
	if refEntries[1].attr("path") != "examples/before-after.md" {
		t.Errorf("second ref path = %q", refEntries[1].attr("path"))
	}
}

// 6. Fan-out item populated -> <fan_out_item> present with compact JSON.
func TestBuild_FanOutItem(t *testing.T) {
	f := setup(t)
	stage := &workspace.Stage{
		ID:     "01_draft",
		Folder: f.stageDir,
		FanOut: &workspace.FanOutConfig{ItemVar: "topic"},
	}

	itemValue := map[string]any{"title": "intro", "weight": 3}
	out, err := f.builder().Build(PayloadInputs{
		Stage:     stage,
		Turn:      1,
		ItemID:    "item_01",
		ItemValue: itemValue,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	root := parse(t, out)
	if root.attr("item") != "item_01" {
		t.Errorf("item attr = %q", root.attr("item"))
	}
	ld := root.child("layer_data")
	items := ld.childrenNamed("fan_out_item")
	if len(items) != 1 {
		t.Fatalf("expected one fan_out_item, got %d", len(items))
	}
	if items[0].attr("key") != "topic" {
		t.Errorf("key = %q", items[0].attr("key"))
	}
	// compact JSON — no indentation
	if !strings.Contains(items[0].Text, `"title":"intro"`) {
		t.Errorf("expected compact JSON, got: %q", items[0].Text)
	}
	if strings.Contains(items[0].Text, "\n  ") {
		t.Errorf("fan_out_item JSON should be compact, got indented: %q", items[0].Text)
	}
}

// 7. Previous attempt block with validator failures + human feedback.
func TestBuild_PreviousAttempt(t *testing.T) {
	f := setup(t)
	stage := &workspace.Stage{ID: "01_draft", Folder: f.stageDir}
	pa := &PreviousAttempt{
		Turn:   2,
		Output: []byte("earlier draft text"),
		Failures: []icmtypes.ConditionResult{
			{Type: "regex", Name: "has_heading", Verdict: "fail", Feedback: "missing # title"},
			{Type: "schema", Name: "shape", Verdict: "pass", Feedback: "ok"}, // filtered
		},
		HumanFeedback: "make it punchier",
	}
	out, err := f.builder().Build(PayloadInputs{
		Stage:           stage,
		Turn:            3,
		PreviousAttempt: pa,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	root := parse(t, out)
	prev := root.child("previous_attempt")
	if prev == nil {
		t.Fatalf("no <previous_attempt>")
	}
	if prev.attr("turn") != "2" {
		t.Errorf("turn attr = %q", prev.attr("turn"))
	}
	output := prev.child("output")
	if output == nil || !strings.Contains(output.Text, "earlier draft text") {
		t.Errorf("output missing/wrong: %+v", output)
	}
	vf := prev.child("validator_feedback")
	if vf == nil {
		t.Fatalf("no <validator_feedback>")
	}
	failures := vf.childrenNamed("failure")
	if len(failures) != 1 {
		t.Fatalf("expected 1 failure (pass filtered), got %d", len(failures))
	}
	if failures[0].attr("name") != "has_heading" || failures[0].attr("type") != "regex" {
		t.Errorf("failure attrs = %+v", failures[0].Attrs)
	}
	if !strings.Contains(failures[0].Text, "missing # title") {
		t.Errorf("failure feedback wrong: %q", failures[0].Text)
	}
	hf := prev.child("human_feedback")
	if hf == nil || !strings.Contains(hf.Text, "make it punchier") {
		t.Errorf("human_feedback missing/wrong: %+v", hf)
	}
}

// 8. Previous iteration block with exit failures.
func TestBuild_PreviousIteration(t *testing.T) {
	f := setup(t)
	stage := &workspace.Stage{ID: "03_refine", Folder: f.stageDir}
	pi := &PreviousIteration{
		Index:    2,
		Artifact: []byte("rev-2 content"),
		Path:     "03_refine/draft.md",
		Failures: []icmtypes.ConditionResult{
			{Type: "llm", Name: "quality", Verdict: "fail", Feedback: "needs depth"},
		},
	}
	out, err := f.builder().Build(PayloadInputs{
		Stage:             stage,
		Turn:              1,
		Iteration:         3,
		PreviousIteration: pi,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	root := parse(t, out)
	if root.attr("iteration") != "3" {
		t.Errorf("iteration attr = %q", root.attr("iteration"))
	}
	prev := root.child("previous_iteration")
	if prev == nil {
		t.Fatalf("no <previous_iteration>")
	}
	if prev.attr("index") != "2" {
		t.Errorf("index attr = %q", prev.attr("index"))
	}
	art := prev.child("artifact")
	if art == nil {
		t.Fatalf("no <artifact> in previous_iteration")
	}
	if art.attr("path") != "03_refine/draft.md" {
		t.Errorf("artifact path = %q", art.attr("path"))
	}
	if !strings.Contains(art.Text, "rev-2 content") {
		t.Errorf("artifact text = %q", art.Text)
	}
	ef := prev.child("exit_failures")
	if ef == nil {
		t.Fatalf("no <exit_failures>")
	}
	fails := ef.childrenNamed("failure")
	if len(fails) != 1 || fails[0].attr("name") != "quality" {
		t.Errorf("exit failures = %+v", fails)
	}
}

// 9. CDATA escaping for content containing the literal "]]>" sequence.
func TestBuild_CDATAEscaping(t *testing.T) {
	f := setup(t)
	// Artifact body intentionally contains a CDATA terminator.
	content := []byte("before ]]> after")
	f.writeStageArtifact("00_input", "tricky.md", content)
	stage := &workspace.Stage{
		ID:     "01_draft",
		Folder: f.stageDir,
		Inputs: workspace.InputScope{Artifacts: []string{"00_input/tricky.md"}},
	}

	out, err := f.builder().Build(PayloadInputs{Stage: stage, Turn: 1})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Must NOT contain a raw "]]>" inside a CDATA section — XMLCDATA
	// splits it into "]]]]><![CDATA[>". The decoder still surfaces the
	// original characters as text.
	mustContain(t, out, "]]]]><![CDATA[>")

	// Parser round-trip must reconstitute the original characters.
	root := parse(t, out)
	art := root.child("layer_data").childrenNamed("artifact")
	if len(art) != 1 {
		t.Fatalf("expected one artifact, got %d", len(art))
	}
	if !strings.Contains(art[0].Text, "before ]]> after") {
		t.Errorf("CDATA round-trip failed: %q", art[0].Text)
	}
}

// 10. Optional blocks: turn 1 / iter 1 with no prior context.
func TestBuild_OptionalBlocksAbsent(t *testing.T) {
	f := setup(t)
	stage := &workspace.Stage{ID: "01_draft", Folder: f.stageDir, Role: "drafter"}

	out, err := f.builder().Build(PayloadInputs{Stage: stage, Turn: 1, Iteration: 1})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	root := parse(t, out)
	if root.child("previous_attempt") != nil {
		t.Errorf("previous_attempt should be absent on turn 1")
	}
	if root.child("previous_iteration") != nil {
		t.Errorf("previous_iteration should be absent on iter 1")
	}
	// iteration attr is emitted because >0; orchestrator decides not to
	// pass Iteration on non-loop stages. Just sanity-check we still get
	// grounding/layer_data/instructions.
	if root.child("grounding") == nil {
		t.Errorf("missing <grounding>")
	}
	if root.child("layer_data") == nil {
		t.Errorf("missing <layer_data>")
	}
	if root.child("instructions") == nil {
		t.Errorf("missing <instructions>")
	}
}

// Bonus: shared grounding files resolve from the workspace root.
func TestBuild_SharedGrounding(t *testing.T) {
	f := setup(t)
	// shared/grounding/policy.md
	sharedPath := filepath.Join(f.sharedDir, "policy.md")
	if err := os.WriteFile(sharedPath, []byte("no PII"), 0o644); err != nil {
		t.Fatalf("write shared: %v", err)
	}
	stage := &workspace.Stage{
		ID:     "01_draft",
		Folder: f.stageDir,
		Inputs: workspace.InputScope{SharedGrounding: []string{"policy.md"}},
	}

	out, err := f.builder().Build(PayloadInputs{Stage: stage, Turn: 1})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	root := parse(t, out)
	shared := root.child("grounding").childrenNamed("shared_file")
	if len(shared) != 1 {
		t.Fatalf("expected 1 shared_file, got %d", len(shared))
	}
	if shared[0].attr("path") != "policy.md" {
		t.Errorf("path = %q", shared[0].attr("path"))
	}
	if !strings.Contains(shared[0].Text, "no PII") {
		t.Errorf("body = %q", shared[0].Text)
	}
}
