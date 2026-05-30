package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// defaultOperatorTemplate is a stand-in for the plugin's embedded
// operator.md, supplied to the loader via WithDefaultOperatorBytes.
var defaultOperatorTemplate = []byte("# ICM Operator (default)\n\nYou are running stage {{ .Stage.ID }}.\n")

// ---------------------------------------------------------------------------
// fixture helpers
// ---------------------------------------------------------------------------

func mustWriteFile(t *testing.T, root, relPath, content string) {
	t.Helper()
	abs := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(abs), err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", abs, err)
	}
}

func mustMkdir(t *testing.T, root string, parts ...string) {
	t.Helper()
	abs := filepath.Join(append([]string{root}, parts...)...)
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", abs, err)
	}
}

func newMinimalWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWriteFile(t, dir, "workspace.md", "# Test workflow\n\nA minimal test workspace.\n")
	mustWriteFile(t, dir, "stages/01_test/contract.md", minimalContract("01_test", "out.md"))
	return dir
}

func minimalContract(id, filename string) string {
	return `---
id: ` + id + `
output:
  filename: ` + filename + `
---

# Process

Do the work.
`
}

func load(t *testing.T, dir string) (*Workflow, error) {
	t.Helper()
	return LoadWorkspace(dir, WithDefaultOperatorBytes(defaultOperatorTemplate))
}

func requireLoadError(t *testing.T, err error, mustContain string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", mustContain)
	}
	if !strings.Contains(err.Error(), mustContain) {
		t.Fatalf("expected error containing %q, got: %v", mustContain, err)
	}
	var multi *LoadErrors
	var single *LoadError
	if !errors.As(err, &multi) && !errors.As(err, &single) {
		t.Errorf("expected *LoadErrors or *LoadError, got %T", err)
	}
}

func findStage(wf *Workflow, id string) *Stage {
	for i := range wf.Stages {
		if wf.Stages[i].ID == id {
			return &wf.Stages[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// happy path
// ---------------------------------------------------------------------------

func TestLoad_MinimalValid(t *testing.T) {
	dir := newMinimalWorkspace(t)
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if wf == nil {
		t.Fatal("workflow is nil")
	}
	if len(wf.Stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(wf.Stages))
	}
	s := wf.Stages[0]
	if s.ID != "01_test" {
		t.Errorf("expected stage ID %q, got %q", "01_test", s.ID)
	}
	if s.Output.Filename != "out.md" {
		t.Errorf("expected output.filename %q, got %q", "out.md", s.Output.Filename)
	}
	if s.Output.Format != OutputText {
		t.Errorf("expected default format text, got %q", s.Output.Format)
	}
	if s.HumanGate != HumanGateNone {
		t.Errorf("expected default human_gate none, got %q", s.HumanGate)
	}
	if s.OnError != ErrorHalt {
		t.Errorf("expected default on_error halt, got %q", s.OnError)
	}
	if s.Turns.Policy != TurnsFixed {
		t.Errorf("expected default turns.policy fixed, got %q", s.Turns.Policy)
	}
	if s.Turns.Max != 1 {
		t.Errorf("expected default turns.max 1, got %d", s.Turns.Max)
	}
	if s.Display != "Process" {
		t.Errorf("expected display derived from first body line, got %q", s.Display)
	}
	if wf.Operator.Source != "default" {
		t.Errorf("expected operator source 'default', got %q", wf.Operator.Source)
	}
}

func TestLoad_MultipleStagesInOrder(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/02_second/contract.md", minimalContract("02_second", "second.md"))
	mustWriteFile(t, dir, "stages/03_third/contract.md", minimalContract("03_third", "third.md"))

	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(wf.Stages) != 3 {
		t.Fatalf("expected 3 stages, got %d", len(wf.Stages))
	}
	expected := []string{"01_test", "02_second", "03_third"}
	for i, e := range expected {
		if wf.Stages[i].ID != e {
			t.Errorf("stage %d: expected %q, got %q", i, e, wf.Stages[i].ID)
		}
	}
}

func TestLoad_NumericSortOrder(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, dir, "workspace.md", "# Test\n")
	// String sort would put 10 before 2 — verify numeric ordering wins.
	mustWriteFile(t, dir, "stages/2_two/contract.md", minimalContract("2_two", "two.md"))
	mustWriteFile(t, dir, "stages/10_ten/contract.md", minimalContract("10_ten", "ten.md"))

	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(wf.Stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(wf.Stages))
	}
	if wf.Stages[0].ID != "2_two" || wf.Stages[1].ID != "10_ten" {
		t.Errorf("expected numeric sort 2_two before 10_ten, got %v",
			[]string{wf.Stages[0].ID, wf.Stages[1].ID})
	}
}

func TestLoad_DisplayExplicit(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
display: Custom Display Label
output:
  filename: out.md
---

# Process

body
`)
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if wf.Stages[0].Display != "Custom Display Label" {
		t.Errorf("expected explicit display, got %q", wf.Stages[0].Display)
	}
}

func TestLoad_DisplayTruncated(t *testing.T) {
	long := strings.Repeat("x", 200)
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
display: `+long+`
output:
  filename: out.md
---

# Process

body
`)
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	d := wf.Stages[0].Display
	if !strings.HasSuffix(d, "…") {
		t.Errorf("expected display truncated with ellipsis, got %q", d)
	}
}

// ---------------------------------------------------------------------------
// operator + workspace.md presence
// ---------------------------------------------------------------------------

func TestLoad_OperatorWorkspaceOverride(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "operator.md", "workspace operator body")
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if wf.Operator.Source != "workspace" {
		t.Errorf("expected workspace source, got %q", wf.Operator.Source)
	}
	if !strings.Contains(wf.Operator.Body, "workspace operator body") {
		t.Errorf("expected workspace body, got %q", wf.Operator.Body)
	}
}

func TestLoad_OperatorOverlayApplied(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "operator.overlay.md", "OVERLAY BODY")
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !strings.Contains(wf.Operator.Source, "+overlay") {
		t.Errorf("expected source to mark overlay, got %q", wf.Operator.Source)
	}
	if !strings.Contains(wf.Operator.Body, "OVERLAY BODY") {
		t.Errorf("expected overlay appended to body")
	}
	if wf.Operator.Overlay != "OVERLAY BODY" {
		t.Errorf("expected Overlay field populated, got %q", wf.Operator.Overlay)
	}
}

func TestLoad_OperatorOverlayDerivedFromLayerName(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "icm.yaml", `layer_names:
  operator: system.md
`)
	mustWriteFile(t, dir, "system.md", "custom operator")
	mustWriteFile(t, dir, "system.overlay.md", "OVL")
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !strings.Contains(wf.Operator.Body, "custom operator") {
		t.Errorf("expected custom operator body")
	}
	if !strings.Contains(wf.Operator.Body, "OVL") {
		t.Errorf("expected overlay to be derived from system.md → system.overlay.md")
	}
}

func TestLoad_MissingWorkspaceMD(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, dir, "stages/01_test/contract.md", minimalContract("01_test", "out.md"))
	_, err := load(t, dir)
	requireLoadError(t, err, "workspace.md")
}

func TestLoad_FallbackToDefaultOperator(t *testing.T) {
	dir := newMinimalWorkspace(t)
	// no operator.md → loader uses defaultOperatorBytes
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if wf.Operator.Source != "default" {
		t.Errorf("expected source default, got %q", wf.Operator.Source)
	}
}

func TestLoad_NoDefaultOperatorAndNoWorkspaceOperator(t *testing.T) {
	dir := newMinimalWorkspace(t)
	_, err := LoadWorkspace(dir) // no WithDefaultOperatorBytes
	requireLoadError(t, err, "default operator")
}

func TestLoad_MissingContract(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, dir, "workspace.md", "# Test\n")
	mustMkdir(t, dir, "stages", "01_test")
	_, err := load(t, dir)
	requireLoadError(t, err, "contract")
}

func TestLoad_NoStagesDirectory(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, dir, "workspace.md", "# Test\n")
	_, err := load(t, dir)
	requireLoadError(t, err, "stages")
}

// ---------------------------------------------------------------------------
// stage folder name validation
// ---------------------------------------------------------------------------

func TestLoad_InvalidStageFolderName_NoPrefix(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/badname/contract.md", minimalContract("badname", "out.md"))
	_, err := load(t, dir)
	requireLoadError(t, err, "badname")
}

func TestLoad_InvalidStageFolderName_DashSeparator(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/99-bad/contract.md", minimalContract("99-bad", "out.md"))
	_, err := load(t, dir)
	requireLoadError(t, err, "99-bad")
}

func TestLoad_InvalidStageFolderName_WithSpace(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/02 has space/contract.md", minimalContract("02_space", "out.md"))
	_, err := load(t, dir)
	requireLoadError(t, err, "02 has space")
}

func TestLoad_ReservedStageFolder(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/00_input/contract.md", minimalContract("00_input", "x.md"))
	_, err := load(t, dir)
	requireLoadError(t, err, "reserved")
}

func TestLoad_DuplicateStagePrefix(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_other/contract.md", minimalContract("01_other", "other.md"))
	_, err := load(t, dir)
	requireLoadError(t, err, "duplicate stage prefix")
}

func TestLoad_StageWithArtifactsFolder(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustMkdir(t, dir, "stages", "01_test", "artifacts")
	_, err := load(t, dir)
	requireLoadError(t, err, "artifacts/")
}

// ---------------------------------------------------------------------------
// contract YAML / body
// ---------------------------------------------------------------------------

func TestLoad_MissingFrontmatterDelimiter(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", "no frontmatter here\n")
	_, err := load(t, dir)
	requireLoadError(t, err, "front-matter")
}

func TestLoad_MissingFrontmatterClose(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", "---\nid: 01_test\n\n# no closer\n")
	_, err := load(t, dir)
	requireLoadError(t, err, "front-matter")
}

func TestLoad_EmptyContractBody(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
---
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "body")
}

func TestLoad_IDMismatch(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: different_name
output:
  filename: out.md
---

# Process

Do the work.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "id")
}

func TestLoad_InvalidYAMLFrontmatter(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
turns:
  policy: ["not a string"]
---

# Process

Do the work.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "YAML")
}

// ---------------------------------------------------------------------------
// output
// ---------------------------------------------------------------------------

func TestLoad_MissingFilename(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output: {}
---

# Process

Do the work.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "output.filename")
}

func TestLoad_FilenameWithSeparator(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: sub/out.md
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "path separators")
}

func TestLoad_JSONWithoutSchema(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  format: json
  filename: out.json
---

# Process

Do the work.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "output.schema is required")
}

func TestLoad_JSONWithValidSchema(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "schemas/out.json", `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["name"],
  "properties": {"name": {"type": "string"}}
}`)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  format: json
  schema: schemas/out.json
  filename: out.json
---

# Process

Do the work.
`)
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if wf.Stages[0].Output.Schema != "schemas/out.json" {
		t.Errorf("expected schema path, got %q", wf.Stages[0].Output.Schema)
	}
}

func TestLoad_InvalidSchemaJSON(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "schemas/out.json", `{not json`)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  format: json
  schema: schemas/out.json
  filename: out.json
---

# Process

Do the work.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "schema")
}

func TestLoad_InvalidFormat(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  format: xml
  filename: out.xml
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "format")
}

// ---------------------------------------------------------------------------
// predicates
// ---------------------------------------------------------------------------

func TestLoad_RegexValidatorCompiles(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
  validators:
    - type: regex
      pattern: '^# .+'
      anchor: first_line
---

# Process

Do the work.
`)
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(wf.Stages[0].Output.Validators) != 1 {
		t.Fatalf("expected 1 validator, got %d", len(wf.Stages[0].Output.Validators))
	}
	v := wf.Stages[0].Output.Validators[0]
	if v.CompiledRegex() == nil {
		t.Errorf("expected compiled regex to be populated")
	}
	if v.Name != "regex_0" {
		t.Errorf("expected default name regex_0, got %q", v.Name)
	}
}

func TestLoad_RegexValidatorInvalid(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
  validators:
    - type: regex
      pattern: '['
      anchor: whole
---

# Process

Do the work.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "regex")
}

func TestLoad_RegexInvalidAnchor(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
  validators:
    - type: regex
      pattern: 'foo'
      anchor: somewhere
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "anchor")
}

func TestLoad_UnknownPredicateType(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
  validators:
    - type: nonsense
---

# Process

Do the work.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "invalid")
}

func TestLoad_NativePredicateMissingHandler(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
  validators:
    - type: native
      args: {key: value}
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "handler")
}

func TestLoad_HumanPredicateMissingPrompt(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
  validators:
    - type: human
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "prompt")
}

func TestLoad_CommandPredicateMissingRun(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
  validators:
    - type: command
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "run")
}

func TestLoad_CommandPredicateScriptMissing(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
  validators:
    - type: command
      run: scripts/missing.sh
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "missing.sh")
}

func TestLoad_CommandPredicateNotExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable-bit checks don't apply on Windows")
	}
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "scripts/check.sh", "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(dir, "scripts/check.sh"), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
  validators:
    - type: command
      run: scripts/check.sh
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "not executable")
}

func TestLoad_CommandPredicateExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable-bit checks don't apply on Windows")
	}
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "scripts/check.sh", "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(dir, "scripts/check.sh"), 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
  validators:
    - type: command
      run: scripts/check.sh
---

# Process

body
`)
	if _, err := load(t, dir); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestLoad_UntilValidWithNoValidators(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
turns:
  policy: until_valid
output:
  filename: out.md
---

# Process

Do the work.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "until_valid")
}

func TestLoad_UntilValidDefaultsToMax3(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
turns:
  policy: until_valid
output:
  filename: out.md
  validators:
    - type: regex
      pattern: '^.+$'
---

# Process

body
`)
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if wf.Stages[0].Turns.Max != 3 {
		t.Errorf("expected until_valid default Max=3, got %d", wf.Stages[0].Turns.Max)
	}
}

func TestLoad_LLMValidatorMissingRubric(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
  validators:
    - type: llm
      rubric: validators/missing.md
---

# Process

Do the work.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "rubric")
}

func TestLoad_LLMValidatorWithRubric(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "validators/quality.md", "# Quality rubric\n\nFail if X.\n")
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
  validators:
    - type: llm
      rubric: validators/quality.md
---

# Process

Do the work.
`)
	if _, err := load(t, dir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_LLMValidatorInvalidJudgePostureName(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "validators/q.md", "rubric body\n")
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
  validators:
    - type: llm
      rubric: validators/q.md
      model: "Has Caps"
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "posture")
}

func TestLoad_SchemaPredicateMissingPath(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
  validators:
    - type: schema
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "schema")
}

// ---------------------------------------------------------------------------
// loop and fan-out
// ---------------------------------------------------------------------------

func TestLoad_LoopValid(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "validators/approved.md", "# Approved rubric\n")
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
loop:
  max_iterations: 3
  until:
    - type: llm
      rubric: validators/approved.md
---

# Process

Do the work.
`)
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wf.Stages[0].Loop == nil {
		t.Fatal("expected loop config")
	}
	if wf.Stages[0].Loop.OnExhausted != ExhaustedHumanGate {
		t.Errorf("expected default on_exhausted=human_gate, got %q", wf.Stages[0].Loop.OnExhausted)
	}
}

func TestLoad_LoopMissingMaxIterations(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "validators/approved.md", "# Approved rubric\n")
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
loop:
  until:
    - type: llm
      rubric: validators/approved.md
---

# Process

Do the work.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "max_iterations")
}

func TestLoad_LoopEmptyUntil(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
loop:
  max_iterations: 5
  until: []
---

# Process

Do the work.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "until")
}

func TestLoad_LoopInvalidOnExhausted(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "validators/r.md", "rubric\n")
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
loop:
  max_iterations: 1
  on_exhausted: ignore
  until:
    - type: llm
      rubric: validators/r.md
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "on_exhausted")
}

func TestLoad_FanOutValid(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/02_dispatch/contract.md", `---
id: 02_dispatch
output:
  filename: drafts.json
  format: json
  schema: schemas/drafts.json
fan_out:
  source: 01_test/out.md
  item_var: topic
  jsonpath: .topics
  item_id: .id
---

# Process

Draft per topic.
`)
	mustWriteFile(t, dir, "schemas/drafts.json", `{"type":"array"}`)
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := findStage(wf, "02_dispatch")
	if s == nil {
		t.Fatal("stage 02_dispatch not loaded")
	}
	if s.FanOut == nil {
		t.Fatal("expected fan_out config")
	}
	if s.FanOut.MaxParallel != 1 {
		t.Errorf("expected default max_parallel=1, got %d", s.FanOut.MaxParallel)
	}
	if s.FanOut.OnItemFailure != ItemFailureContinue {
		t.Errorf("expected default on_item_failure=continue, got %q", s.FanOut.OnItemFailure)
	}
	if s.FanOut.CompiledJSONPath() == nil {
		t.Errorf("expected jsonpath to compile")
	}
	if s.FanOut.CompiledItemID() == nil {
		t.Errorf("expected item_id to compile")
	}
}

func TestLoad_FanOutBadShape(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/02_dispatch/contract.md", `---
id: 02_dispatch
output:
  filename: drafts.md
fan_out:
  source: not-a-valid-ref
  item_var: x
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "fan_out.source")
}

func TestLoad_FanOutMissingSource(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/02_dispatch/contract.md", `---
id: 02_dispatch
output:
  filename: drafts.md
fan_out:
  item_var: x
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "fan_out.source")
}

func TestLoad_FanOutMissingItemVar(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/02_dispatch/contract.md", `---
id: 02_dispatch
output:
  filename: drafts.md
fan_out:
  source: 01_test/out.md
---

# Process

Draft per topic.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "item_var")
}

func TestLoad_FanOutBadJSONPathCompile(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/02_dispatch/contract.md", `---
id: 02_dispatch
output:
  filename: drafts.md
fan_out:
  source: 01_test/out.md
  item_var: x
  jsonpath: ".invalid (\""
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "jsonpath")
}

func TestLoad_FanOutMaxParallelDefaults(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/02_dispatch/contract.md", `---
id: 02_dispatch
output:
  filename: drafts.md
fan_out:
  source: 01_test/out.md
  item_var: x
  max_parallel: 0
---

# Process

body
`)
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	s := findStage(wf, "02_dispatch")
	if s.FanOut.MaxParallel != 1 {
		t.Errorf("expected MaxParallel default=1, got %d", s.FanOut.MaxParallel)
	}
}

// ---------------------------------------------------------------------------
// cross-stage artifact ref resolution
// ---------------------------------------------------------------------------

func TestLoad_ArtifactRefValid(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/02_consume/contract.md", `---
id: 02_consume
output:
  filename: final.md
inputs:
  artifacts: [01_test/out.md]
---

# Process

Use the input.
`)
	if _, err := load(t, dir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_ArtifactRefUnknownStage(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/02_consume/contract.md", `---
id: 02_consume
output:
  filename: final.md
inputs:
  artifacts: [99_phantom/out.md]
---

# Process

Use the input.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "unknown stage")
}

func TestLoad_ArtifactRefFilenameMismatch(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/02_consume/contract.md", `---
id: 02_consume
output:
  filename: final.md
inputs:
  artifacts: [01_test/different.md]
---

# Process

Use the input.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "different.md")
}

func TestLoad_ArtifactRefBackwardOrder(t *testing.T) {
	dir := newMinimalWorkspace(t)
	// 01_test references 02_later — forbidden.
	mustWriteFile(t, dir, "stages/02_later/contract.md", minimalContract("02_later", "later.md"))
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
inputs:
  artifacts: [02_later/later.md]
---

# Process

Use a future stage's output.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "does not run before")
}

func TestLoad_InitialInputRefAllowed(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
inputs:
  artifacts: [00_input/brief.md]
---

# Process

Read the brief.
`)
	if _, err := load(t, dir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_ArtifactRefWithArtifactsSegmentRejected(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/02_consume/contract.md", `---
id: 02_consume
output:
  filename: final.md
inputs:
  artifacts: [01_test/artifacts/out.md]
---

# Process

Use the input.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "artifacts/")
}

// ---------------------------------------------------------------------------
// inputs.grounding / shared_grounding existence
// ---------------------------------------------------------------------------

func TestLoad_GroundingMissingFile(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
inputs:
  grounding: [absent.md]
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "absent.md")
}

func TestLoad_SharedGroundingMissingFile(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
inputs:
  shared_grounding: [absent.md]
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "absent.md")
}

func TestLoad_GroundingPresent(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/grounding/style.md", "style guide\n")
	mustWriteFile(t, dir, "shared/grounding/glossary.md", "glossary\n")
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
inputs:
  grounding: [style.md]
  shared_grounding: [glossary.md]
---

# Process

body
`)
	if _, err := load(t, dir); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

// ---------------------------------------------------------------------------
// skills
// ---------------------------------------------------------------------------

func TestLoad_SkillResolution_Shared(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "shared/skills/voice/SKILL.md", `---
name: voice
description: Voice and tone for narrative content.
---

# Voice

Use first person plural.
`)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
inputs:
  skills: [voice]
---

# Process

Do the work.
`)
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sk := wf.Stages[0].Skills["voice"]
	if sk == nil {
		t.Fatal("skill 'voice' not resolved")
	}
	if sk.Source != SkillWorkspace {
		t.Errorf("expected source=workspace, got %q", sk.Source)
	}
	if !strings.Contains(sk.Body, "first person plural") {
		t.Errorf("skill body not loaded, got: %q", sk.Body)
	}
}

func TestLoad_SkillResolution_StageLocalShadowsShared(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "shared/skills/voice/SKILL.md", `---
name: voice
description: shared voice
---
shared body
`)
	mustWriteFile(t, dir, "stages/01_test/skills/voice/SKILL.md", `---
name: voice
description: stage-local voice
---
stage body
`)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
inputs:
  skills: [voice]
---

# Process

Do the work.
`)
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sk := wf.Stages[0].Skills["voice"]
	if sk == nil {
		t.Fatal("skill 'voice' not resolved")
	}
	if sk.Source != SkillStageLocal {
		t.Errorf("expected stage-local source, got %q", sk.Source)
	}
	if !strings.Contains(sk.Body, "stage body") {
		t.Errorf("expected stage-local body, got: %q", sk.Body)
	}
}

func TestLoad_SkillNotFound(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
inputs:
  skills: [phantom]
---

# Process

Do the work.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "phantom")
}

func TestLoad_SkillBadName(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
inputs:
  skills: [Bad_Name]
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "Bad_Name")
}

func TestLoad_SkillMissingDescription(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "shared/skills/voice/SKILL.md", `---
name: voice
---

body
`)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
inputs:
  skills: [voice]
---

# Process

Do the work.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "description")
}

func TestLoad_SkillMissingName(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "shared/skills/voice/SKILL.md", `---
description: voice
---

body
`)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
inputs:
  skills: [voice]
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "name")
}

func TestLoad_SkillNameMismatch(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "shared/skills/voice/SKILL.md", `---
name: different
description: testing
---
body
`)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
inputs:
  skills: [voice]
---

# Process

Do the work.
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "name")
}

func TestLoad_SkillEnumeratesReferences(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "shared/skills/voice/SKILL.md", `---
name: voice
description: testing
---
body
`)
	mustWriteFile(t, dir, "shared/skills/voice/references/tone.md", "tone reference\n")
	mustWriteFile(t, dir, "shared/skills/voice/references/structure.md", "structure reference\n")
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
inputs:
  skills: [voice]
---

# Process

Do the work.
`)
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sk := wf.Stages[0].Skills["voice"]
	if len(sk.References) != 2 {
		t.Fatalf("expected 2 references, got %d", len(sk.References))
	}
	// References are sorted alphabetically.
	if sk.References[0].Path != "structure.md" {
		t.Errorf("expected structure.md first, got %q", sk.References[0].Path)
	}
}

// ---------------------------------------------------------------------------
// icm.yaml overrides + agent merge
// ---------------------------------------------------------------------------

func TestLoad_ICMYamlDefaults(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "icm.yaml", `defaults:
  human_gate: end
  judge_posture: judge_default
  agent:
    model_role: writer
`)
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wf.Stages[0].HumanGate != HumanGateEnd {
		t.Errorf("expected human_gate=end from defaults, got %q", wf.Stages[0].HumanGate)
	}
	if wf.Stages[0].Agent.ModelRole != "writer" {
		t.Errorf("expected model_role=writer from defaults, got %q", wf.Stages[0].Agent.ModelRole)
	}
	if wf.Defaults.JudgePosture != "judge_default" {
		t.Errorf("expected JudgePosture=judge_default, got %q", wf.Defaults.JudgePosture)
	}
}

func TestLoad_ICMYamlInvalidValues(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "icm.yaml", `defaults:
  human_gate: bogus
  turn_policy: weird
  on_error: huh
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "human_gate")
}

func TestLoad_StageOverridesWorkspaceDefaults(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "icm.yaml", `defaults:
  human_gate: end
`)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
human_gate: none
output:
  filename: out.md
---

# Process

Do the work.
`)
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wf.Stages[0].HumanGate != HumanGateNone {
		t.Errorf("expected stage override human_gate=none, got %q", wf.Stages[0].HumanGate)
	}
}

func TestLoad_AgentBudgetInheritance(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "icm.yaml", `defaults:
  agent:
    model_role: writer
    posture: workspace_default
    budget:
      timeout_seconds: 30
      max_tokens: 1000
      max_tool_calls: 5
    max_recursion_depth: 2
`)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
agent:
  budget:
    max_tokens: 2000
output:
  filename: out.md
---

# Process

body
`)
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	a := wf.Stages[0].Agent
	if a.Posture != "workspace_default" {
		t.Errorf("expected inherited posture, got %q", a.Posture)
	}
	if a.Budget.TimeoutSeconds != 30 {
		t.Errorf("expected inherited timeout_seconds=30, got %d", a.Budget.TimeoutSeconds)
	}
	if a.Budget.MaxTokens != 2000 {
		t.Errorf("expected overridden max_tokens=2000, got %d", a.Budget.MaxTokens)
	}
	if a.Budget.MaxToolCalls != 5 {
		t.Errorf("expected inherited max_tool_calls=5, got %d", a.Budget.MaxToolCalls)
	}
	if a.MaxRecursionDepth != 2 {
		t.Errorf("expected inherited max_recursion_depth=2, got %d", a.MaxRecursionDepth)
	}
}

func TestLoad_AgentPostureInvalidName(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
agent:
  posture: "Has Caps"
output:
  filename: out.md
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "posture")
}

func TestLoad_AgentBudgetNegative(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
agent:
  budget:
    timeout_seconds: -1
output:
  filename: out.md
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "timeout_seconds")
}

// ---------------------------------------------------------------------------
// error aggregation
// ---------------------------------------------------------------------------

func TestLoad_AggregatesMultipleErrors(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
  validators:
    - type: regex
      pattern: '['
      anchor: whole
    - type: llm
      rubric: validators/missing.md
---

# Process

Do the work.
`)
	_, err := load(t, dir)
	if err == nil {
		t.Fatal("expected aggregated errors, got nil")
	}
	var multi *LoadErrors
	if !errors.As(err, &multi) {
		t.Fatalf("expected *LoadErrors, got %T: %v", err, err)
	}
	if len(multi.Errors) < 2 {
		t.Errorf("expected multiple aggregated errors, got %d: %v", len(multi.Errors), err)
	}
}

// ---------------------------------------------------------------------------
// verifiers
// ---------------------------------------------------------------------------

func TestLoad_VerifierDeclaredButMissing(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
verifiers: [qc]
---

# Process

body
`)
	_, err := load(t, dir)
	requireLoadError(t, err, "qc")
}

func TestLoad_VerifierAsFolderResolves(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "verifiers/qc/contract.md", `---
id: qc
output:
  filename: report.md
---

# QC

Check it.
`)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
verifiers: [qc]
---

# Process

body
`)
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if _, ok := wf.Verifiers["qc"]; !ok {
		t.Errorf("expected verifier qc resolved")
	}
}

func TestLoad_VerifierAsFileResolves(t *testing.T) {
	dir := newMinimalWorkspace(t)
	mustWriteFile(t, dir, "verifiers/qc.md", `---
id: qc
output:
  filename: report.md
---

# QC

Check it.
`)
	mustWriteFile(t, dir, "stages/01_test/contract.md", `---
id: 01_test
output:
  filename: out.md
verifiers: [qc]
---

# Process

body
`)
	wf, err := load(t, dir)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if _, ok := wf.Verifiers["qc"]; !ok {
		t.Errorf("expected verifier qc resolved from .md file")
	}
}

// ---------------------------------------------------------------------------
// Validate convenience
// ---------------------------------------------------------------------------

func TestValidate_PassesOnValidWorkspace(t *testing.T) {
	dir := newMinimalWorkspace(t)
	if err := Validate(dir, WithDefaultOperatorBytes(defaultOperatorTemplate)); err != nil {
		t.Fatalf("Validate returned error on valid workspace: %v", err)
	}
}

func TestValidate_FailsOnInvalidWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := Validate(dir, WithDefaultOperatorBytes(defaultOperatorTemplate)); err == nil {
		t.Fatal("Validate returned nil on invalid workspace")
	}
}

// ---------------------------------------------------------------------------
// LoadError formatting
// ---------------------------------------------------------------------------

func TestLoadError_Error(t *testing.T) {
	cases := []struct {
		name string
		e    *LoadError
		want string
	}{
		{
			name: "with line",
			e:    &LoadError{Path: "/x/y", Line: 12, Msg: "oops"},
			want: "/x/y:12: oops",
		},
		{
			name: "with path no line",
			e:    &LoadError{Path: "/x/y", Msg: "oops"},
			want: "/x/y: oops",
		},
		{
			name: "no path",
			e:    &LoadError{Msg: "oops"},
			want: "oops",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.e.Error(); got != c.want {
				t.Errorf("want %q, got %q", c.want, got)
			}
		})
	}
}

func TestLoadErrors_Error_Single(t *testing.T) {
	es := &LoadErrors{Errors: []*LoadError{{Path: "/a", Msg: "first"}}}
	if got := es.Error(); got != "/a: first" {
		t.Errorf("single-error aggregate should pass through, got %q", got)
	}
}

func TestLoadErrors_Error_Multiple(t *testing.T) {
	es := &LoadErrors{Errors: []*LoadError{
		{Path: "/a", Msg: "first"},
		{Path: "/b", Msg: "second"},
	}}
	s := es.Error()
	if !strings.Contains(s, "2 errors") {
		t.Errorf("expected count in aggregate, got %q", s)
	}
	if !strings.Contains(s, "first") || !strings.Contains(s, "second") {
		t.Errorf("expected both errors in output, got %q", s)
	}
}
