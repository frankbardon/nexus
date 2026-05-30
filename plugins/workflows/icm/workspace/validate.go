package workspace

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/itchyny/gojq"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// icm.yaml: layer names and workspace defaults
// ---------------------------------------------------------------------------

type rawConfig struct {
	LayerNames LayerNames `yaml:"layer_names"`
	Defaults   struct {
		TurnPolicy   TurnPolicy  `yaml:"turn_policy"`
		HumanGate    HumanGate   `yaml:"human_gate"`
		OnError      ErrorPolicy `yaml:"on_error"`
		JudgePosture string      `yaml:"judge_posture"`
		Agent        AgentSpec   `yaml:"agent"`
	} `yaml:"defaults"`
	Operator struct {
		Overlay string `yaml:"overlay"`
	} `yaml:"operator"`
}

func (c *loadCtx) loadConfig() (LayerNames, WorkspaceDefaults) {
	names := LayerNames{
		Operator:  "operator.md",
		Workspace: "workspace.md",
		Contract:  "contract.md",
		Grounding: "grounding",
	}
	defs := WorkspaceDefaults{
		TurnPolicy: TurnsFixed,
		HumanGate:  HumanGateNone,
		OnError:    ErrorHalt,
	}

	path := filepath.Join(c.root, "icm.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			c.addErrorf(path, "cannot read icm.yaml: %v", err)
		}
		return names, defs
	}

	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		c.addErrorf(path, "icm.yaml: invalid YAML: %v", err)
		return names, defs
	}

	if raw.LayerNames.Operator != "" {
		names.Operator = raw.LayerNames.Operator
	}
	if raw.LayerNames.Workspace != "" {
		names.Workspace = raw.LayerNames.Workspace
	}
	if raw.LayerNames.Contract != "" {
		names.Contract = raw.LayerNames.Contract
	}
	if raw.LayerNames.Grounding != "" {
		names.Grounding = raw.LayerNames.Grounding
	}

	if raw.Defaults.TurnPolicy != "" {
		if !validTurnPolicy(raw.Defaults.TurnPolicy) {
			c.addErrorf(path, "defaults.turn_policy %q invalid", raw.Defaults.TurnPolicy)
		} else {
			defs.TurnPolicy = raw.Defaults.TurnPolicy
		}
	}
	if raw.Defaults.HumanGate != "" {
		if !validHumanGate(raw.Defaults.HumanGate) {
			c.addErrorf(path, "defaults.human_gate %q invalid", raw.Defaults.HumanGate)
		} else {
			defs.HumanGate = raw.Defaults.HumanGate
		}
	}
	if raw.Defaults.OnError != "" {
		if !validErrorPolicy(raw.Defaults.OnError) {
			c.addErrorf(path, "defaults.on_error %q invalid", raw.Defaults.OnError)
		} else {
			defs.OnError = raw.Defaults.OnError
		}
	}
	defs.JudgePosture = raw.Defaults.JudgePosture
	defs.Agent = raw.Defaults.Agent
	if defs.Agent.Posture != "" && !postureNameRE.MatchString(defs.Agent.Posture) {
		c.addErrorf(path, "defaults.agent.posture %q invalid", defs.Agent.Posture)
	}

	return names, defs
}

// ---------------------------------------------------------------------------
// operator.md: resolve workspace override or embedded default; apply overlay
// ---------------------------------------------------------------------------

func (c *loadCtx) loadOperator(names LayerNames) OperatorConfig {
	cfg := OperatorConfig{}
	workspacePath := filepath.Join(c.root, names.Operator)

	if body, err := os.ReadFile(workspacePath); err == nil {
		cfg.Body = string(body)
		cfg.Source = "workspace"
	} else if os.IsNotExist(err) {
		if len(c.loader.defaultOperatorBytes) == 0 {
			c.addError("", "default operator template not supplied and workspace operator file missing")
			return cfg
		}
		cfg.Body = string(c.loader.defaultOperatorBytes)
		cfg.Source = "default"
	} else {
		c.addErrorf(workspacePath, "cannot read operator file: %v", err)
		return cfg
	}

	// Overlay path derives from the configured operator filename:
	// "operator.md" -> "operator.overlay.md"; "ops.md" -> "ops.overlay.md".
	overlayName := overlayFilename(names.Operator)
	overlayPath := filepath.Join(c.root, overlayName)
	if data, err := os.ReadFile(overlayPath); err == nil {
		cfg.Overlay = string(data)
		cfg.Body = cfg.Body + "\n\n" + cfg.Overlay
		cfg.Source = cfg.Source + "+overlay"
	} else if !os.IsNotExist(err) {
		c.addErrorf(overlayPath, "cannot read operator overlay: %v", err)
	}

	return cfg
}

// overlayFilename returns "<base>.overlay.<ext>" — e.g.
// "operator.md" -> "operator.overlay.md". Files without extensions get
// the suffix appended directly.
func overlayFilename(operatorName string) string {
	ext := filepath.Ext(operatorName)
	stem := strings.TrimSuffix(operatorName, ext)
	if ext == "" {
		return operatorName + ".overlay"
	}
	return stem + ".overlay" + ext
}

// ---------------------------------------------------------------------------
// workspace.md: required, non-empty
// ---------------------------------------------------------------------------

func (c *loadCtx) loadWorkspaceDoc(names LayerNames) string {
	path := filepath.Join(c.root, names.Workspace)
	data, err := os.ReadFile(path)
	if err != nil {
		c.addErrorf(path, "workspace.md missing or unreadable: %v", err)
		return ""
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		c.addError(path, "workspace doc is empty")
	}
	return string(data)
}

// ---------------------------------------------------------------------------
// stages/: enumerate, parse contracts, validate
// ---------------------------------------------------------------------------

func (c *loadCtx) loadStages(names LayerNames, defs WorkspaceDefaults) []Stage {
	stagesDir := filepath.Join(c.root, "stages")
	entries, err := os.ReadDir(stagesDir)
	if err != nil {
		c.addErrorf(stagesDir, "stages directory missing or unreadable: %v", err)
		return nil
	}

	var folders []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == reservedInputStage {
			c.addErrorf(filepath.Join(stagesDir, name),
				"stage folder name %q is reserved", reservedInputStage)
			continue
		}
		if !stageFolderRE.MatchString(name) {
			c.addErrorf(filepath.Join(stagesDir, name),
				"stage folder name must match ^\\d+_[a-z0-9_]+$, got %q", name)
			continue
		}
		folders = append(folders, name)
	}
	// Sort by numeric prefix for execution order.
	sort.Slice(folders, func(i, j int) bool {
		return prefixSortKey(folders[i]) < prefixSortKey(folders[j])
	})

	// Reject ambiguous orderings (duplicate numeric prefixes).
	seenPrefix := map[string]string{}
	for _, f := range folders {
		prefix := strings.SplitN(f, "_", 2)[0]
		if prev, ok := seenPrefix[prefix]; ok {
			c.addErrorf(filepath.Join(stagesDir, f),
				"duplicate stage prefix %q (also %q); execution order is ambiguous", prefix, prev)
		}
		seenPrefix[prefix] = f
	}

	stages := make([]Stage, 0, len(folders))
	for _, name := range folders {
		stageDir := filepath.Join(stagesDir, name)
		stage := c.loadStage(stageDir, name, names, defs)
		// Reject any artifacts/ folder inside stage dirs.
		artPath := filepath.Join(stageDir, "artifacts")
		if info, err := os.Stat(artPath); err == nil && info.IsDir() {
			c.addStageError(name, artPath,
				"stage folder contains artifacts/ — artifacts live in the session dir, not the workspace")
		}
		if stage.Folder != "" || stage.ID != "" {
			stages = append(stages, stage)
		}
	}

	return stages
}

// prefixSortKey returns a string suitable for numeric prefix sorting:
// it zero-pads the numeric prefix to 9 chars so "9_x" sorts before
// "10_x".
func prefixSortKey(folder string) string {
	parts := strings.SplitN(folder, "_", 2)
	if len(parts) == 0 {
		return folder
	}
	if len(parts[0]) >= 9 {
		return folder
	}
	return strings.Repeat("0", 9-len(parts[0])) + folder
}

// ---------------------------------------------------------------------------
// per-stage contract.md
// ---------------------------------------------------------------------------

func (c *loadCtx) loadStage(stageDir, folderName string, names LayerNames, defs WorkspaceDefaults) Stage {
	stage := Stage{
		ID:     folderName,
		Folder: stageDir,
		Skills: map[string]*Skill{},
	}

	contractPath := filepath.Join(stageDir, names.Contract)
	data, err := os.ReadFile(contractPath)
	if err != nil {
		c.addStageErrorf(folderName, contractPath, "contract missing or unreadable: %v", err)
		return stage
	}

	frontmatter, body, err := splitContract(data)
	if err != nil {
		c.addStageErrorf(folderName, contractPath, "%v", err)
		return stage
	}
	if strings.TrimSpace(body) == "" {
		c.addStageError(folderName, contractPath, "contract body (after second '---') is empty")
	}
	stage.Role = body

	var raw rawContract
	if err := yaml.Unmarshal(frontmatter, &raw); err != nil {
		c.addStageErrorf(folderName, contractPath, "invalid YAML front-matter: %v", err)
		return stage
	}

	// id consistency
	if raw.ID != "" && raw.ID != folderName {
		c.addStageErrorf(folderName, contractPath,
			"front-matter id %q does not match folder name %q", raw.ID, folderName)
	}

	// Display: explicit > first body line > stage ID
	switch {
	case strings.TrimSpace(raw.Display) != "":
		stage.Display = truncateDisplay(strings.TrimSpace(raw.Display))
	default:
		if line := firstNonEmptyLine(body); line != "" {
			stage.Display = truncateDisplay(line)
		} else {
			stage.Display = stage.ID
		}
	}

	stage.Turns = applyTurnDefaults(raw.Turns, defs)
	if !validTurnPolicy(stage.Turns.Policy) {
		c.addStageErrorf(folderName, contractPath, "turns.policy %q invalid", stage.Turns.Policy)
	}
	if stage.Turns.Max < 1 {
		c.addStageErrorf(folderName, contractPath, "turns.max must be a positive integer, got %d", stage.Turns.Max)
	}

	stage.HumanGate = raw.HumanGate
	if stage.HumanGate == "" {
		stage.HumanGate = defs.HumanGate
	}
	if !validHumanGate(stage.HumanGate) {
		c.addStageErrorf(folderName, contractPath, "human_gate %q invalid", stage.HumanGate)
	}

	stage.OnError = raw.OnError
	if stage.OnError == "" {
		if defs.OnError != "" {
			stage.OnError = defs.OnError
		} else {
			stage.OnError = ErrorHalt
		}
	}
	if !validErrorPolicy(stage.OnError) {
		c.addStageErrorf(folderName, contractPath, "on_error %q invalid", stage.OnError)
	}

	stage.Output = c.validateOutput(folderName, contractPath, raw.Output)

	if raw.Loop != nil {
		c.validateLoop(folderName, contractPath, raw.Loop)
		stage.Loop = raw.Loop
	}
	if stage.Turns.Policy == TurnsUntilValid && len(stage.Output.Validators) == 0 {
		c.addStageError(folderName, contractPath,
			"turns.policy=until_valid requires at least one output.validator")
	}

	if raw.FanOut != nil {
		c.validateFanOut(folderName, contractPath, raw.FanOut)
		stage.FanOut = raw.FanOut
	}

	stage.Inputs = c.validateInputs(folderName, stageDir, raw.Inputs)
	stage.Agent = c.mergeAgent(folderName, contractPath, raw.Agent, defs.Agent)
	stage.Verifiers = raw.Verifiers

	return stage
}

// ---------------------------------------------------------------------------
// output, predicates, loop, fan-out validation
// ---------------------------------------------------------------------------

func (c *loadCtx) validateOutput(stageID, contractPath string, out OutputSpec) OutputSpec {
	if out.Format == "" {
		out.Format = OutputText
	}
	if out.Format != OutputText && out.Format != OutputJSON {
		c.addStageErrorf(stageID, contractPath, "output.format %q invalid", out.Format)
	}
	if out.Persist == "" {
		out.Persist = PersistFileRef
	}
	if !validPersistMode(out.Persist) {
		c.addStageErrorf(stageID, contractPath, "output.persist %q invalid", out.Persist)
	}
	if out.Filename == "" {
		c.addStageError(stageID, contractPath, "output.filename is required")
	} else if strings.ContainsAny(out.Filename, "/\\") {
		c.addStageErrorf(stageID, contractPath, "output.filename %q must not contain path separators", out.Filename)
	}

	if out.Format == OutputJSON {
		if out.Schema == "" {
			c.addStageError(stageID, contractPath, "output.schema is required when output.format=json")
		} else {
			c.validateJSONSchemaPath(stageID, contractPath, out.Schema)
		}
	}

	for i := range out.Validators {
		c.validatePredicate(stageID, contractPath, &out.Validators[i], i, "output.validators")
	}
	return out
}

func (c *loadCtx) validateLoop(stageID, contractPath string, loop *LoopConfig) {
	if loop.MaxIterations <= 0 {
		c.addStageErrorf(stageID, contractPath, "loop.max_iterations must be a positive integer, got %d", loop.MaxIterations)
	}
	if len(loop.Until) == 0 {
		c.addStageError(stageID, contractPath, "loop.until must have at least one condition")
	}
	if loop.OnExhausted == "" {
		loop.OnExhausted = ExhaustedHumanGate
	}
	if loop.OnExhausted != ExhaustedHumanGate && loop.OnExhausted != ExhaustedError {
		c.addStageErrorf(stageID, contractPath, "loop.on_exhausted %q invalid", loop.OnExhausted)
	}
	for i := range loop.Until {
		c.validatePredicate(stageID, contractPath, &loop.Until[i], i, "loop.until")
	}
}

func (c *loadCtx) validateFanOut(stageID, contractPath string, fo *FanOutConfig) {
	if fo.Source == "" {
		c.addStageError(stageID, contractPath, "fan_out.source is required")
	} else if !artifactRefRE.MatchString(fo.Source) {
		c.addStageErrorf(stageID, contractPath,
			"fan_out.source %q must be of the form '<stage_id>/<filename>'", fo.Source)
	}
	if fo.ItemVar == "" {
		c.addStageError(stageID, contractPath, "fan_out.item_var is required")
	}
	if fo.MaxParallel <= 0 {
		fo.MaxParallel = 1
	}
	if fo.OnItemFailure == "" {
		fo.OnItemFailure = ItemFailureContinue
	}
	if fo.OnItemFailure != ItemFailureContinue && fo.OnItemFailure != ItemFailureHalt {
		c.addStageErrorf(stageID, contractPath, "fan_out.on_item_failure %q invalid", fo.OnItemFailure)
	}

	// Compile gojq expressions at load so the orchestrator can dispatch
	// straight against fo.compiledJSONPath / fo.compiledItemID.
	if fo.JSONPath != "" {
		if code, err := compileJQ(fo.JSONPath); err != nil {
			c.addStageErrorf(stageID, contractPath, "fan_out.jsonpath %q does not compile: %v", fo.JSONPath, err)
		} else {
			fo.compiledJSONPath = code
		}
	}
	if fo.ItemID != "" {
		if code, err := compileJQ(fo.ItemID); err != nil {
			c.addStageErrorf(stageID, contractPath, "fan_out.item_id %q does not compile: %v", fo.ItemID, err)
		} else {
			fo.compiledItemID = code
		}
	}
}

// compileJQ parses + compiles a gojq expression. Exported via the
// FanOutConfig accessors; not used elsewhere in the loader.
func compileJQ(expr string) (*gojq.Code, error) {
	q, err := gojq.Parse(expr)
	if err != nil {
		return nil, err
	}
	return gojq.Compile(q)
}

func (c *loadCtx) validatePredicate(stageID, contractPath string, p *Predicate, idx int, container string) {
	loc := fmt.Sprintf("%s[%d]", container, idx)
	if p.Name == "" {
		p.Name = fmt.Sprintf("%s_%d", p.Type, idx)
	}
	switch p.Type {
	case PredSchema:
		if p.SchemaPath == "" {
			c.addStageErrorf(stageID, contractPath, "%s type=schema requires 'schema' path", loc)
		} else {
			c.validateJSONSchemaPath(stageID, contractPath, p.SchemaPath)
		}
	case PredRegex:
		if p.Pattern == "" {
			c.addStageErrorf(stageID, contractPath, "%s type=regex requires 'pattern'", loc)
		} else {
			re, err := regexp.Compile(p.Pattern)
			if err != nil {
				c.addStageErrorf(stageID, contractPath, "%s regex pattern: %v", loc, err)
			} else {
				p.compiledRegex = re
			}
		}
		if p.Anchor == "" {
			p.Anchor = AnchorWhole
		}
		switch p.Anchor {
		case AnchorFirstLine, AnchorLastLine, AnchorWhole:
		default:
			c.addStageErrorf(stageID, contractPath, "%s anchor %q invalid", loc, p.Anchor)
		}
	case PredNative:
		if p.Handler == "" {
			c.addStageErrorf(stageID, contractPath, "%s type=native requires 'handler'", loc)
		}
		// Handler existence checked at dispatch (Nexus registry state).
	case PredCommand:
		if p.Run == "" {
			c.addStageErrorf(stageID, contractPath, "%s type=command requires 'run'", loc)
		} else {
			abs := c.resolveWorkspacePath(p.Run)
			info, err := os.Stat(abs)
			if err != nil {
				c.addStageErrorf(stageID, contractPath, "%s command script %q: %v", loc, p.Run, err)
			} else if info.Mode()&0o111 == 0 {
				c.addStageErrorf(stageID, contractPath, "%s command script %q is not executable", loc, p.Run)
			}
		}
		if p.TimeoutSeconds < 0 {
			c.addStageErrorf(stageID, contractPath, "%s timeout_seconds must be >= 0, got %d", loc, p.TimeoutSeconds)
		}
	case PredLLM:
		if p.Rubric == "" {
			c.addStageErrorf(stageID, contractPath, "%s type=llm requires 'rubric'", loc)
		} else {
			abs := c.resolveWorkspacePath(p.Rubric)
			if _, err := os.Stat(abs); err != nil {
				c.addStageErrorf(stageID, contractPath, "%s rubric %q: %v", loc, p.Rubric, err)
			}
		}
		if p.Model != "" && !postureNameRE.MatchString(p.Model) {
			c.addStageErrorf(stageID, contractPath, "%s model (judge posture) %q invalid", loc, p.Model)
		}
	case PredHuman:
		if p.Prompt == "" {
			c.addStageErrorf(stageID, contractPath, "%s type=human requires 'prompt'", loc)
		}
	default:
		c.addStageErrorf(stageID, contractPath, "%s type %q invalid (expected schema|regex|native|command|llm|human)", loc, p.Type)
	}
}

func (c *loadCtx) validateJSONSchemaPath(stageID, contractPath, schemaPath string) {
	abs := c.resolveWorkspacePath(schemaPath)
	data, err := os.ReadFile(abs)
	if err != nil {
		c.addStageErrorf(stageID, contractPath, "schema %q: %v", schemaPath, err)
		return
	}
	// Sanity-check it parses as JSON. The plugin re-compiles + registers
	// at runtime via ctx.Schemas; here we just want early failure.
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		c.addStageErrorf(stageID, contractPath, "schema %q: invalid JSON: %v", schemaPath, err)
		return
	}
	// Attempt a draft-2020 compile too, to catch malformed schemas early.
	parsed, err := jsonschema.UnmarshalJSON(strings.NewReader(string(data)))
	if err != nil {
		c.addStageErrorf(stageID, contractPath, "schema %q: %v", schemaPath, err)
		return
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(schemaPath, parsed); err != nil {
		c.addStageErrorf(stageID, contractPath, "schema %q: %v", schemaPath, err)
		return
	}
	if _, err := compiler.Compile(schemaPath); err != nil {
		c.addStageErrorf(stageID, contractPath, "schema %q does not compile: %v", schemaPath, err)
	}
}

// ---------------------------------------------------------------------------
// inputs: grounding, shared_grounding, artifacts, skills
// ---------------------------------------------------------------------------

func (c *loadCtx) validateInputs(stageID, stageDir string, in InputScope) InputScope {
	groundingDir := filepath.Join(stageDir, "grounding")
	for _, rel := range in.Grounding {
		abs := filepath.Join(groundingDir, rel)
		if _, err := os.Stat(abs); err != nil {
			c.addStageErrorf(stageID, abs, "inputs.grounding %q: %v", rel, err)
		}
	}

	sharedDir := filepath.Join(c.root, "shared", "grounding")
	for _, rel := range in.SharedGrounding {
		abs := filepath.Join(sharedDir, rel)
		if _, err := os.Stat(abs); err != nil {
			c.addStageErrorf(stageID, abs, "inputs.shared_grounding %q: %v", rel, err)
		}
	}

	for _, ref := range in.Artifacts {
		if strings.Contains(ref, "/artifacts/") {
			c.addStageErrorf(stageID, stageDir,
				"inputs.artifacts %q must not include artifacts/ segment (use '<stage_id>/<filename>')", ref)
			continue
		}
		if !artifactRefRE.MatchString(ref) {
			c.addStageErrorf(stageID, stageDir,
				"inputs.artifacts %q must be of the form '<stage_id>/<filename>'", ref)
		}
	}

	// Skill names: shape check only here. Resolution is cross-stage,
	// handled in resolveSkills() after all stages parse.
	for _, name := range in.Skills {
		if !skillNameRE.MatchString(name) {
			c.addStageErrorf(stageID, stageDir,
				"inputs.skills %q must match ^[a-z][a-z0-9-]*$", name)
		}
	}

	return in
}

// ---------------------------------------------------------------------------
// cross-stage validation
// ---------------------------------------------------------------------------

// validateArtifactRefs ensures every inputs.artifacts entry points at an
// earlier stage's declared output filename, or at the reserved 00_input.
func (c *loadCtx) validateArtifactRefs(stages []Stage) {
	outputs := map[string]string{}
	order := map[string]int{}
	for i, s := range stages {
		outputs[s.ID] = s.Output.Filename
		order[s.ID] = i
	}

	for i, s := range stages {
		for _, ref := range s.Inputs.Artifacts {
			m := artifactRefRE.FindStringSubmatch(ref)
			if m == nil {
				continue // shape error already reported
			}
			srcStage, filename := m[1], m[2]
			if srcStage == reservedInputStage {
				continue // initial input is provided at dispatch
			}
			pos, known := order[srcStage]
			if !known {
				c.addStageErrorf(s.ID, s.Folder,
					"inputs.artifacts %q references unknown stage %q", ref, srcStage)
				continue
			}
			if pos >= i {
				c.addStageErrorf(s.ID, s.Folder,
					"inputs.artifacts %q references stage %q which does not run before %q", ref, srcStage, s.ID)
				continue
			}
			if outputs[srcStage] != filename {
				c.addStageErrorf(s.ID, s.Folder,
					"inputs.artifacts %q: stage %q declares output filename %q, not %q",
					ref, srcStage, outputs[srcStage], filename)
			}
		}

		// fan_out.source has the same shape and ordering constraints.
		if s.FanOut != nil && s.FanOut.Source != "" {
			m := artifactRefRE.FindStringSubmatch(s.FanOut.Source)
			if m != nil {
				srcStage, filename := m[1], m[2]
				if srcStage != reservedInputStage {
					pos, known := order[srcStage]
					if !known {
						c.addStageErrorf(s.ID, s.Folder,
							"fan_out.source %q references unknown stage %q", s.FanOut.Source, srcStage)
					} else if pos >= i {
						c.addStageErrorf(s.ID, s.Folder,
							"fan_out.source %q references stage %q which does not run before %q",
							s.FanOut.Source, srcStage, s.ID)
					} else if outputs[srcStage] != filename {
						c.addStageErrorf(s.ID, s.Folder,
							"fan_out.source %q: stage %q declares output filename %q, not %q",
							s.FanOut.Source, srcStage, outputs[srcStage], filename)
					}
				}
			}
		}
	}
}

// validateVerifierRefs ensures every declared verifier ID resolves.
func (c *loadCtx) validateVerifierRefs(stages []Stage, verifiers map[string]*Stage) {
	for _, s := range stages {
		for _, vID := range s.Verifiers {
			if _, ok := verifiers[vID]; !ok {
				c.addStageErrorf(s.ID, s.Folder, "verifier %q not found under verifiers/", vID)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// verifiers/: parsed using the same contract logic
// ---------------------------------------------------------------------------

func (c *loadCtx) loadVerifiers(names LayerNames, defs WorkspaceDefaults) map[string]*Stage {
	verDir := filepath.Join(c.root, "verifiers")
	out := map[string]*Stage{}

	entries, err := os.ReadDir(verDir)
	if err != nil {
		if !os.IsNotExist(err) {
			c.addErrorf(verDir, "verifiers directory unreadable: %v", err)
		}
		return out
	}

	for _, e := range entries {
		if e.IsDir() {
			s := c.loadStage(filepath.Join(verDir, e.Name()), e.Name(), names, defs)
			if s.ID != "" {
				out[s.ID] = &s
			}
			continue
		}
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}

		// Verifier as a single .md file with frontmatter + body.
		id := strings.TrimSuffix(e.Name(), ".md")
		path := filepath.Join(verDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			c.addErrorf(path, "verifier unreadable: %v", err)
			continue
		}
		frontmatter, body, err := splitContract(data)
		if err != nil {
			c.addErrorf(path, "verifier: %v", err)
			continue
		}
		var raw rawContract
		if err := yaml.Unmarshal(frontmatter, &raw); err != nil {
			c.addErrorf(path, "verifier: invalid YAML: %v", err)
			continue
		}
		turns := applyTurnDefaults(raw.Turns, defs)
		human := raw.HumanGate
		if human == "" {
			human = defs.HumanGate
		}
		onErr := raw.OnError
		if onErr == "" {
			if defs.OnError != "" {
				onErr = defs.OnError
			} else {
				onErr = ErrorHalt
			}
		}
		display := strings.TrimSpace(raw.Display)
		if display == "" {
			if line := firstNonEmptyLine(body); line != "" {
				display = line
			} else {
				display = id
			}
		}
		s := Stage{
			ID:        id,
			Display:   truncateDisplay(display),
			Folder:    verDir,
			Role:      body,
			Turns:     turns,
			HumanGate: human,
			OnError:   onErr,
			Output:    c.validateOutput(id, path, raw.Output),
			Inputs:    raw.Inputs,
			Agent:     c.mergeAgent(id, path, raw.Agent, defs.Agent),
			Skills:    map[string]*Skill{},
		}
		out[id] = &s
	}
	return out
}

// ---------------------------------------------------------------------------
// skills: resolve through precedence chain
// ---------------------------------------------------------------------------

func (c *loadCtx) resolveSkills(stages []Stage) {
	// Cache by absolute folder path so two stages requesting the same
	// skill share the parsed struct.
	cache := map[string]*Skill{}

	for i := range stages {
		s := &stages[i]
		for _, name := range s.Inputs.Skills {
			if _, already := s.Skills[name]; already {
				continue
			}
			path, source, ok := c.findSkill(name, s.Folder)
			if !ok {
				c.addStageErrorf(s.ID, s.Folder,
					"skill %q not found (looked in stage-local skills/, shared/skills/)", name)
				continue
			}
			sk, ok := cache[path]
			if !ok {
				sk = c.parseSkill(path, source)
				if sk == nil {
					continue
				}
				cache[path] = sk
			}
			s.Skills[name] = sk
		}
	}
}

func (c *loadCtx) findSkill(name, stageDir string) (path string, source SkillSource, ok bool) {
	local := filepath.Join(stageDir, "skills", name)
	if isSkillDir(local) {
		return local, SkillStageLocal, true
	}
	shared := filepath.Join(c.root, "shared", "skills", name)
	if isSkillDir(shared) {
		return shared, SkillWorkspace, true
	}
	return "", "", false
}

func isSkillDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	_, err = os.Stat(filepath.Join(path, "SKILL.md"))
	return err == nil
}

func (c *loadCtx) parseSkill(path string, source SkillSource) *Skill {
	skillMD := filepath.Join(path, "SKILL.md")
	data, err := os.ReadFile(skillMD)
	if err != nil {
		c.addErrorf(skillMD, "skill: %v", err)
		return nil
	}
	frontmatter, body, err := splitContract(data)
	if err != nil {
		c.addErrorf(skillMD, "skill: %v", err)
		return nil
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal(frontmatter, &fm); err != nil {
		c.addErrorf(skillMD, "skill: invalid YAML frontmatter: %v", err)
		return nil
	}
	folderName := filepath.Base(path)
	if fm.Name == "" {
		c.addError(skillMD, "skill: frontmatter 'name' is required")
	} else if fm.Name != folderName {
		c.addErrorf(skillMD, "skill: frontmatter name %q does not match folder name %q", fm.Name, folderName)
	}
	if fm.Description == "" {
		c.addError(skillMD, "skill: frontmatter 'description' is required")
	}

	sk := &Skill{
		Name:        fm.Name,
		Description: fm.Description,
		Source:      source,
		Path:        path,
		Body:        body,
	}

	refsDir := filepath.Join(path, "references")
	if info, err := os.Stat(refsDir); err == nil && info.IsDir() {
		_ = filepath.WalkDir(refsDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if !d.Type().IsRegular() {
				c.addErrorf(p, "skill reference is not a regular file")
				return nil
			}
			rel, err := filepath.Rel(refsDir, p)
			if err != nil {
				return nil
			}
			sk.References = append(sk.References, SkillRef{Path: rel})
			return nil
		})
		sort.Slice(sk.References, func(i, j int) bool {
			return sk.References[i].Path < sk.References[j].Path
		})
	}
	return sk
}

// ---------------------------------------------------------------------------
// helpers: defaults, validation predicates, agent merge
// ---------------------------------------------------------------------------

func applyTurnDefaults(t TurnConfig, defs WorkspaceDefaults) TurnConfig {
	if t.Policy == "" {
		if defs.TurnPolicy != "" {
			t.Policy = defs.TurnPolicy
		} else {
			t.Policy = TurnsFixed
		}
	}
	if t.Max == 0 {
		if t.Policy == TurnsUntilValid {
			t.Max = 3
		} else {
			t.Max = 1
		}
	}
	return t
}

// mergeAgent overlays stage-level AgentSpec fields atop workspace
// defaults. Posture name (when set) is validated for shape only —
// existence is deferred to runtime since postures register dynamically.
func (c *loadCtx) mergeAgent(stageID, contractPath string, stage, defaults AgentSpec) AgentSpec {
	if stage.Posture == "" {
		stage.Posture = defaults.Posture
	}
	if stage.Posture != "" && !postureNameRE.MatchString(stage.Posture) {
		c.addStageErrorf(stageID, contractPath, "agent.posture %q invalid (must match ^[a-z][a-z0-9_.-]*$)", stage.Posture)
	}
	if stage.ModelRole == "" {
		stage.ModelRole = defaults.ModelRole
	}
	if stage.Tools == nil {
		stage.Tools = defaults.Tools
	}
	if stage.PromptOverlay == "" {
		stage.PromptOverlay = defaults.PromptOverlay
	}
	// Budget: stage values win when non-zero, otherwise inherit defaults.
	if stage.Budget.TimeoutSeconds == 0 {
		stage.Budget.TimeoutSeconds = defaults.Budget.TimeoutSeconds
	}
	if stage.Budget.MaxTokens == 0 {
		stage.Budget.MaxTokens = defaults.Budget.MaxTokens
	}
	if stage.Budget.MaxToolCalls == 0 {
		stage.Budget.MaxToolCalls = defaults.Budget.MaxToolCalls
	}
	if stage.MaxRecursionDepth == 0 {
		stage.MaxRecursionDepth = defaults.MaxRecursionDepth
	}
	if stage.Budget.TimeoutSeconds < 0 {
		c.addStageErrorf(stageID, contractPath, "agent.budget.timeout_seconds must be >= 0, got %d", stage.Budget.TimeoutSeconds)
	}
	if stage.Budget.MaxTokens < 0 {
		c.addStageErrorf(stageID, contractPath, "agent.budget.max_tokens must be >= 0, got %d", stage.Budget.MaxTokens)
	}
	if stage.Budget.MaxToolCalls < 0 {
		c.addStageErrorf(stageID, contractPath, "agent.budget.max_tool_calls must be >= 0, got %d", stage.Budget.MaxToolCalls)
	}
	if stage.MaxRecursionDepth < 0 {
		c.addStageErrorf(stageID, contractPath, "agent.max_recursion_depth must be >= 0, got %d", stage.MaxRecursionDepth)
	}
	return stage
}

func validTurnPolicy(p TurnPolicy) bool {
	switch p {
	case TurnsFixed, TurnsUntilValid, TurnsUntilHumanApproves:
		return true
	}
	return false
}

func validHumanGate(g HumanGate) bool {
	switch g {
	case HumanGateNone, HumanGateStart, HumanGateEnd, HumanGateBoth:
		return true
	}
	return false
}

func validErrorPolicy(e ErrorPolicy) bool {
	switch e {
	case ErrorHalt, ErrorRetry, ErrorHumanGate:
		return true
	}
	return false
}

func validPersistMode(p PersistMode) bool {
	switch p {
	case PersistContext, PersistFileRef, PersistBoth:
		return true
	}
	return false
}
