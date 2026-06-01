package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

const (
	reservedInputStage = "00_input"
)

// Shared regexes. These compile once at package init.
var (
	stageFolderRE = regexp.MustCompile(`^\d+_[a-z0-9_]+$`)
	skillNameRE   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	artifactRefRE = regexp.MustCompile(`^([0-9]+_[a-z0-9_]+|00_input)/([^/]+)$`)
	postureNameRE = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)
)

// Loader is the entry point for reading a workspace from disk. A Loader
// instance lets callers inject options (e.g. embedded default operator
// bytes) without per-call wiring.
type Loader struct {
	defaultOperatorBytes []byte
}

// LoaderOption configures a Loader.
type LoaderOption func(*Loader)

// WithDefaultOperatorBytes supplies the embedded fallback operator.md
// content used when the workspace omits its own operator file. The
// plugin package embeds the file (workspace cannot reach across to a
// sibling package's go:embed) and forwards the bytes here.
func WithDefaultOperatorBytes(b []byte) LoaderOption {
	return func(l *Loader) { l.defaultOperatorBytes = b }
}

// NewLoader builds a Loader with the given options.
func NewLoader(opts ...LoaderOption) *Loader {
	l := &Loader{}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// LoadWorkspace reads a workspace folder and produces a validated
// *Workflow. Returns *LoadErrors (a multi-error) when validation fails.
// The workspace is not modified.
func LoadWorkspace(root string, opts ...LoaderOption) (*Workflow, error) {
	return NewLoader(opts...).Load(root)
}

// Validate runs LoadWorkspace but discards the result. Useful in CI and
// the `icm_validate` LLM tool.
func Validate(root string, opts ...LoaderOption) error {
	_, err := LoadWorkspace(root, opts...)
	return err
}

// Load reads the workspace at root and returns a fully validated
// *Workflow. All file references resolve at load time; regex patterns
// compile; gojq expressions compile; JSON schemas parse + compile. Errors
// are aggregated rather than short-circuited.
func (l *Loader) Load(root string) (*Workflow, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, &LoadError{Path: root, Msg: "cannot resolve absolute path", Cause: err}
	}
	if info, err := os.Stat(absRoot); err != nil || !info.IsDir() {
		return nil, &LoadError{Path: absRoot, Msg: "workspace root is not a directory"}
	}

	ctx := &loadCtx{root: absRoot, loader: l}
	wf := &Workflow{Root: absRoot}

	wf.LayerNames, wf.Defaults = ctx.loadConfig()
	wf.Operator = ctx.loadOperator(wf.LayerNames)
	wf.WorkspaceDoc = ctx.loadWorkspaceDoc(wf.LayerNames)
	wf.Stages = ctx.loadStages(wf.LayerNames, wf.Defaults)
	wf.Verifiers = ctx.loadVerifiers(wf.LayerNames, wf.Defaults)

	// Cross-stage validation must happen after all stages are parsed.
	ctx.validateArtifactRefs(wf.Stages)
	ctx.validateVerifierRefs(wf.Stages, wf.Verifiers)
	ctx.resolveSkills(wf.Stages)

	if len(ctx.errs) > 0 {
		return nil, &LoadErrors{Errors: ctx.errs}
	}
	return wf, nil
}

// loadCtx accumulates errors as the loader walks the workspace.
type loadCtx struct {
	root   string
	loader *Loader
	errs   []*LoadError
}

func (c *loadCtx) addError(path, msg string) {
	c.errs = append(c.errs, &LoadError{Path: path, Msg: msg})
}

func (c *loadCtx) addErrorf(path, format string, args ...any) {
	c.errs = append(c.errs, &LoadError{Path: path, Msg: fmt.Sprintf(format, args...)})
}

func (c *loadCtx) addStageError(stageID, path, msg string) {
	c.errs = append(c.errs, &LoadError{Stage: stageID, Path: path, Msg: msg})
}

func (c *loadCtx) addStageErrorf(stageID, path, format string, args ...any) {
	c.errs = append(c.errs, &LoadError{Stage: stageID, Path: path, Msg: fmt.Sprintf(format, args...)})
}

// resolveWorkspacePath turns a workspace-relative path into an absolute
// path under the workspace root. It does not check existence.
func (c *loadCtx) resolveWorkspacePath(rel string) string {
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(c.root, rel)
}
