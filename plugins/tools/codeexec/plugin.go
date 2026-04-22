package codeexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/traefik/yaegi/interp"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.tool.code_exec"
	pluginName = "Code Execution Tool"
	version    = "0.1.0"

	toolName = "run_code"
)

// Default caps — overridable via config.
const (
	defaultTimeout        = 30 * time.Second
	defaultMaxOutputBytes = 64 * 1024
)

// Plugin implements programmatic tool calling via an embedded Yaegi interpreter.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace
	prompts *engine.PromptRegistry

	// Config.
	timeout          time.Duration
	maxOutputBytes   int
	maxWorkers       int      // concurrency ceiling for parallel.* primitives
	allowedPackages  []string // stdlib whitelist
	allowedImports   map[string]bool
	persistScripts   bool
	rejectGoroutines bool

	// Registry of other tools — snapshot from tool.register events. Keyed by
	// tool name. Used to generate typed bindings at run_code time.
	regMu sync.RWMutex
	tools map[string]events.ToolDef

	// Active skill helpers keyed by skill name.
	skillMu sync.RWMutex
	skills  map[string]*skillHelpers

	// In-flight invocation. Phase-1 has no goroutines so there is at most one.
	invMu     sync.Mutex
	activeInv *invocation

	unsubs []func()
}

// New creates a new codeexec plugin.
func New() engine.Plugin {
	return &Plugin{
		timeout:          defaultTimeout,
		maxOutputBytes:   defaultMaxOutputBytes,
		maxWorkers:       runtime.NumCPU(),
		allowedPackages:  defaultAllowedStdlib,
		persistScripts:   true,
		rejectGoroutines: true,
		tools:            map[string]events.ToolDef{},
		skills:           map[string]*skillHelpers{},
	}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session
	p.prompts = ctx.Prompts

	if v, ok := ctx.Config["timeout_seconds"].(int); ok && v > 0 {
		p.timeout = time.Duration(v) * time.Second
	}
	if v, ok := ctx.Config["timeout_seconds"].(float64); ok && v > 0 {
		p.timeout = time.Duration(v) * time.Second
	}
	if v, ok := ctx.Config["max_output_bytes"].(int); ok && v > 0 {
		p.maxOutputBytes = v
	}
	if v, ok := ctx.Config["max_output_bytes"].(float64); ok && v > 0 {
		p.maxOutputBytes = int(v)
	}
	if v, ok := ctx.Config["max_workers"].(int); ok && v > 0 {
		p.maxWorkers = v
	}
	if v, ok := ctx.Config["max_workers"].(float64); ok && v > 0 {
		p.maxWorkers = int(v)
	}
	if v, ok := ctx.Config["persist_scripts"].(bool); ok {
		p.persistScripts = v
	}
	if v, ok := ctx.Config["reject_goroutines"].(bool); ok {
		p.rejectGoroutines = v
	}
	if raw, ok := ctx.Config["allowed_packages"].([]any); ok {
		out := make([]string, 0, len(raw))
		for _, e := range raw {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			p.allowedPackages = out
		}
	}

	p.rebuildAllowedImports()

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleToolInvoke,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.result", p.handleToolResult,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.register", p.handleToolRegister,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("skill.loaded", p.handleSkillLoaded,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("skill.deactivate", p.handleSkillDeactivate,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	return nil
}

func (p *Plugin) Ready() error {
	if p.prompts != nil {
		p.prompts.Register("nexus.tool.code_exec.contract", 30, p.buildContractSection)
		p.prompts.Register("nexus.tool.code_exec.bindings", 40, p.buildBindingsSection)
	}
	_ = p.bus.Emit("tool.register", events.ToolDef{
		Name:        toolName,
		Description: runCodeShortDescription,
		Class:       "code",
		Subclass:    "execute",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"script": map[string]any{
					"type":        "string",
					"description": "Complete Go source for a `package main` program that declares `func Run(ctx context.Context) (any, error)`. See the `nexus.tool.code_exec.contract` and `nexus.tool.code_exec.bindings` prompt sections for the full contract and the live list of available imports.",
				},
			},
			"required": []string{"script"},
		},
	})
	return nil
}

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, u := range p.unsubs {
		u()
	}
	if p.prompts != nil {
		p.prompts.Unregister("nexus.tool.code_exec.contract")
		p.prompts.Unregister("nexus.tool.code_exec.bindings")
	}
	return nil
}

// buildContractSection renders the static run_code contract (invocation rules,
// allowed imports, forbidden imports, error semantics). Kept out of the tool
// description so the LLM sees it once as a dedicated prompt section instead of
// bloating the tool schema.
func (p *Plugin) buildContractSection() string { return runCodeContract }

// buildBindingsSection renders an authoritative map of every `tools.*`,
// `skills/<name>`, and `parallel.*` symbol currently visible to scripts.
// Evaluated at every LLM request by PromptRegistry, so tool additions,
// skill activations, and deactivations show up without re-registering
// run_code. Exists because the static contract section can't name bindings
// it hasn't seen — and past LLM hallucinations of `tools.FileRead` (when the
// tool was `read_file` → `tools.ReadFile`) wasted turns.
func (p *Plugin) buildBindingsSection() string {
	p.regMu.RLock()
	names := make([]string, 0, len(p.tools))
	for name := range p.tools {
		if name == toolName {
			continue
		}
		names = append(names, name)
	}
	p.regMu.RUnlock()
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("Available `tools.*` bindings (generated from the live registry — trust this list over the general naming rule):\n")
	if len(names) == 0 {
		b.WriteString("  (no tools registered yet)\n")
	}
	p.regMu.RLock()
	for _, name := range names {
		td := p.tools[name]
		fn := toolFuncName(name)
		args := toolArgsTypeName(name)
		resultDesc := "tools.Result{Output, Error, OutputFile}"
		if td.OutputSchema != nil {
			resultDesc = "tools." + toolResultTypeName(name) + " (typed from OutputSchema)"
		}
		fmt.Fprintf(&b, "  - tools.%s(tools.%s) → %s\n", fn, args, resultDesc)
	}
	p.regMu.RUnlock()

	p.skillMu.RLock()
	skillNames := make([]string, 0, len(p.skills))
	for name := range p.skills {
		skillNames = append(skillNames, name)
	}
	p.skillMu.RUnlock()
	sort.Strings(skillNames)
	if len(skillNames) > 0 {
		b.WriteString("\nActive skill helper imports:\n")
		for _, name := range skillNames {
			fmt.Fprintf(&b, "  - import \"skills/%s\"\n", name)
		}
	}

	b.WriteString("\nAlways available: parallel.Map, parallel.ForEach, parallel.All.")
	return b.String()
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "tool.invoke", Priority: 50},
		{EventType: "tool.result", Priority: 50},
		{EventType: "tool.register", Priority: 50},
		{EventType: "skill.loaded", Priority: 50},
		{EventType: "skill.deactivate", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"tool.register",
		"before:tool.invoke",
		"tool.invoke",
		"before:tool.result",
		"tool.result",
		"code.exec.request",
		"code.exec.stdout",
		"code.exec.result",
	}
}

func (p *Plugin) rebuildAllowedImports() {
	out := make(map[string]bool, len(p.allowedPackages)+2)
	for _, s := range p.allowedPackages {
		out[s] = true
	}
	out["tools"] = true
	out["parallel"] = true
	p.allowedImports = out
}

// -- event handlers ---------------------------------------------------------

func (p *Plugin) handleToolInvoke(e engine.Event[any]) {
	tc, ok := e.Payload.(events.ToolCall)
	if !ok || tc.Name != toolName {
		return
	}
	p.runScript(tc)
}

func (p *Plugin) handleToolResult(e engine.Event[any]) {
	res, ok := e.Payload.(events.ToolResult)
	if !ok {
		return
	}
	p.invMu.Lock()
	inv := p.activeInv
	p.invMu.Unlock()
	if inv == nil {
		return
	}
	inv.routeResult(res)
}

func (p *Plugin) handleToolRegister(e engine.Event[any]) {
	td, ok := e.Payload.(events.ToolDef)
	if !ok {
		return
	}
	// Our own run_code tool is registered through the same event path —
	// skip to avoid recursion into the script.
	if td.Name == toolName {
		return
	}
	p.regMu.Lock()
	p.tools[td.Name] = td
	p.regMu.Unlock()
}

func (p *Plugin) handleSkillLoaded(e engine.Event[any]) {
	sc, ok := e.Payload.(events.SkillContent)
	if !ok {
		return
	}
	helpers, err := loadSkillHelpers(sc.Name, sc.BaseDir)
	if err != nil {
		p.logger.Warn("skill helper load failed", "skill", sc.Name, "error", err)
		return
	}
	if helpers == nil {
		return
	}
	p.skillMu.Lock()
	p.skills[sc.Name] = helpers
	p.skillMu.Unlock()
}

func (p *Plugin) handleSkillDeactivate(e engine.Event[any]) {
	ref, ok := e.Payload.(events.SkillRef)
	if !ok {
		return
	}
	p.skillMu.Lock()
	delete(p.skills, ref.Name)
	p.skillMu.Unlock()
}

// -- script execution -------------------------------------------------------

// runScript handles a run_code tool invocation end-to-end: parse & validate
// the script, build a fresh Yaegi interpreter, inject stdlib + tools + skill
// bindings, execute with a timeout + output cap, persist artifacts, and emit
// a tool.result back to the agent.
func (p *Plugin) runScript(tc events.ToolCall) {
	script, _ := tc.Arguments["script"].(string)
	if script == "" {
		p.emitResult(tc, "", "", "script argument is required", 0, false)
		return
	}

	// Build the snapshot of currently-active skills — needed both for import
	// validation and for code.exec.request emission.
	p.skillMu.RLock()
	activeSkills := make([]string, 0, len(p.skills))
	for name := range p.skills {
		activeSkills = append(activeSkills, name)
	}
	p.skillMu.RUnlock()
	sort.Strings(activeSkills)

	allowedImports := p.importsForRun(activeSkills)

	analysis, err := staticAnalyze(script, allowedImports)
	if err != nil {
		p.emitResult(tc, "", "", fmt.Sprintf("script rejected: %v", err), 0, false)
		return
	}

	_ = p.bus.Emit("code.exec.request", events.CodeExecRequest{
		CallID:  tc.ID,
		TurnID:  tc.TurnID,
		Script:  script,
		Imports: analysis.Imports,
		Skills:  activeSkills,
	})

	stdout := newStreamingWriter(p.bus, tc.ID, tc.TurnID, p.maxOutputBytes)
	// finish closes the stdout stream (flushing a Final chunk) and then emits
	// code.exec.result / tool.result. Ordering matters: consumers expect
	// code.exec.stdout{Final:true} to land before the result so they can
	// mark the live-output section closed before showing the summary.
	finish := func(result, errMsg string, durationMs int64) {
		stdout.Close()
		p.emitResult(tc, stdout.String(), result, errMsg, durationMs, stdout.Truncated())
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	inv := &invocation{
		ctx:          ctx,
		bus:          p.bus,
		turnID:       tc.TurnID,
		parentCallID: tc.ID,
		pending:      map[string]chan events.ToolResult{},
	}

	p.invMu.Lock()
	p.activeInv = inv
	p.invMu.Unlock()
	defer func() {
		p.invMu.Lock()
		p.activeInv = nil
		p.invMu.Unlock()
	}()

	// Stage active skill helpers into a per-invocation GOPATH so the script
	// can `import "skills/<name>"` through Yaegi's normal resolver.
	p.skillMu.RLock()
	helpers := make([]*skillHelpers, 0, len(p.skills))
	for _, h := range p.skills {
		helpers = append(helpers, h)
	}
	p.skillMu.RUnlock()

	goPath, cleanup, err := stageSkillHelpers(helpers)
	if err != nil {
		p.emitResult(tc, "", "", fmt.Sprintf("stage skills: %v", err), 0, false)
		return
	}
	defer cleanup()

	i := interp.New(interp.Options{
		Stdout: stdout,
		Stderr: stdout,
		GoPath: goPath,
	})
	if err := i.Use(filteredStdlibSymbols(p.allowedPackages)); err != nil {
		finish("", fmt.Sprintf("install stdlib: %v", err), 0)
		return
	}

	p.regMu.RLock()
	toolDefs := make([]events.ToolDef, 0, len(p.tools))
	for _, td := range p.tools {
		toolDefs = append(toolDefs, td)
	}
	p.regMu.RUnlock()
	sort.Slice(toolDefs, func(i, j int) bool { return toolDefs[i].Name < toolDefs[j].Name })

	toolExports, _, err := inv.buildToolsExports(toolDefs, toolName)
	if err != nil {
		finish("", fmt.Sprintf("build tools package: %v", err), 0)
		return
	}
	if err := i.Use(toolExports); err != nil {
		finish("", fmt.Sprintf("install tools package: %v", err), 0)
		return
	}
	if err := i.Use(buildParallelExports(p.maxWorkers)); err != nil {
		finish("", fmt.Sprintf("install parallel package: %v", err), 0)
		return
	}

	started := time.Now()

	// Evaluate the user's script. Any parse/type error surfaces here.
	if _, err := i.Eval(script); err != nil {
		finish("", fmt.Sprintf("compile error: %v", err), elapsedMs(started))
		return
	}

	// Resolve main.Run and invoke it with the timeout ctx.
	runVal, err := i.Eval("main.Run")
	if err != nil {
		finish("", fmt.Sprintf("resolve main.Run: %v", err), elapsedMs(started))
		return
	}

	returned, runErr := invokeRun(runVal, ctx)

	// Surface script panics and errors.
	if runErr != nil {
		if errors.Is(runErr, context.DeadlineExceeded) {
			finish("", fmt.Sprintf("script timed out after %s", p.timeout), elapsedMs(started))
			return
		}
		finish("", fmt.Sprintf("runtime error: %v", runErr), elapsedMs(started))
		return
	}

	resultJSON, err := marshalReturn(returned)
	if err != nil {
		finish("", fmt.Sprintf("marshal return: %v", err), elapsedMs(started))
		return
	}

	// Persist uses the aggregated stdout — close the writer first so the tail
	// lands in the buffer before we snapshot it.
	stdout.Close()
	p.persist(tc, script, stdout.String(), resultJSON, "")
	finish(resultJSON, "", elapsedMs(started))
}

// importsForRun returns the set of allowed import paths for a specific
// script invocation. Base = stdlib whitelist + "tools"; plus "skills/<name>"
// for every currently-active skill whose helpers successfully loaded.
func (p *Plugin) importsForRun(activeSkills []string) map[string]bool {
	out := make(map[string]bool, len(p.allowedImports)+len(activeSkills))
	for k, v := range p.allowedImports {
		out[k] = v
	}
	for _, name := range activeSkills {
		out["skills/"+name] = true
	}
	return out
}

// stageSkillHelpers materializes the active skills' rewritten helper source
// into a temp directory laid out as a Go workspace
// (<gopath>/src/skills/<name>/<file>.go) so Yaegi's import resolver can pick
// them up when the script does `import "skills/<name>"`.
//
// Returns the GOPATH to pass to interp.Options.GoPath and a cleanup func.
// Both are always safe to call/use — an empty helper list yields an
// absent path and a no-op cleanup.
func stageSkillHelpers(helpers []*skillHelpers) (string, func(), error) {
	if len(helpers) == 0 {
		return "", func() {}, nil
	}
	root, err := os.MkdirTemp("", "nexus-codeexec-gopath-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(root) }

	for _, h := range helpers {
		pkgDir := filepath.Join(root, "src", "skills", h.Name)
		if err := os.MkdirAll(pkgDir, 0700); err != nil {
			cleanup()
			return "", func() {}, err
		}
		// Stable file order so diagnostics are reproducible.
		files := make([]string, 0, len(h.Sources))
		for fn := range h.Sources {
			files = append(files, fn)
		}
		sort.Strings(files)
		for _, fn := range files {
			if err := os.WriteFile(filepath.Join(pkgDir, fn), []byte(h.Sources[fn]), 0600); err != nil {
				cleanup()
				return "", func() {}, err
			}
		}
	}
	return root, cleanup, nil
}

// invokeRun calls main.Run(ctx). Translates panics into errors and, if ctx
// is already cancelled, returns the ctx error directly rather than whatever
// the interpreter surfaced.
func invokeRun(runVal reflect.Value, ctx context.Context) (rtn any, rerr error) {
	defer func() {
		if r := recover(); r != nil {
			rerr = fmt.Errorf("panic: %v", r)
		}
	}()

	out := runVal.Call([]reflect.Value{reflect.ValueOf(ctx)})
	if len(out) != 2 {
		return nil, fmt.Errorf("run returned %d values, want 2", len(out))
	}
	if !out[1].IsNil() {
		if err, ok := out[1].Interface().(error); ok {
			return nil, err
		}
		return nil, fmt.Errorf("run returned non-error second value: %v", out[1].Interface())
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !out[0].IsValid() {
		return nil, nil
	}
	return out[0].Interface(), nil
}

// marshalReturn JSON-encodes Run's return value. Empty values (nil, "")
// produce the empty string so we don't send useless "null" literals back to
// the model.
func marshalReturn(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func elapsedMs(started time.Time) int64 { return time.Since(started).Milliseconds() }

// persist writes script + artifacts to the session workspace. No-op when
// session is absent or persist_scripts is disabled.
func (p *Plugin) persist(tc events.ToolCall, script, stdout, result, errMsg string) {
	if p.session == nil || !p.persistScripts {
		return
	}
	base := fmt.Sprintf("plugins/%s/%s", pluginID, tc.ID)
	_ = p.session.WriteFile(base+"/script.go", []byte(script))
	if stdout != "" {
		_ = p.session.WriteFile(base+"/stdout.txt", []byte(stdout))
	}
	if result != "" {
		_ = p.session.WriteFile(base+"/result.json", []byte(result))
	}
	if errMsg != "" {
		_ = p.session.WriteFile(base+"/error.txt", []byte(errMsg))
	}
}

// emitResult sends code.exec.result + tool.result (with vetoable guard) back
// to the agent. The Output of tool.result is a JSON document containing
// stdout, the return value, and any error — giving the model a structured
// surface to reason over.
func (p *Plugin) emitResult(tc events.ToolCall, stdout, result, errMsg string, durationMs int64, truncated bool) {
	if errMsg != "" && p.persistScripts && p.session != nil {
		base := fmt.Sprintf("plugins/%s/%s", pluginID, tc.ID)
		_ = p.session.WriteFile(base+"/error.txt", []byte(errMsg))
	}

	_ = p.bus.Emit("code.exec.result", events.CodeExecResult{
		CallID:    tc.ID,
		TurnID:    tc.TurnID,
		Output:    stdout,
		Result:    result,
		Error:     errMsg,
		Duration:  durationMs,
		Truncated: truncated,
	})

	envelope := map[string]any{}
	if stdout != "" {
		envelope["stdout"] = stdout
	}
	if result != "" {
		envelope["result"] = json.RawMessage(result)
	}
	if truncated {
		envelope["stdout_truncated"] = true
	}
	out, _ := json.Marshal(envelope)

	r := events.ToolResult{
		ID:     tc.ID,
		Name:   tc.Name,
		Output: string(out),
		Error:  errMsg,
		TurnID: tc.TurnID,
	}
	if veto, verr := p.bus.EmitVetoable("before:tool.result", &r); verr == nil && veto.Vetoed {
		p.logger.Info("code_exec tool.result vetoed", "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("tool.result", r)
}

// runCodeShortDescription is the tool-schema description. Kept tight — the
// full contract lives in the nexus.tool.code_exec.contract prompt section so
// it isn't re-tokenized as part of every tool-listing block.
const runCodeShortDescription = `Run a short Go program to handle a computational or mechanical task in one turn. Prefer this when the answer is produced by computation (chaining tool calls, looping, parsing/transforming data, math, fan-out) rather than judgment. Avoid for prose, summarization, translation, or open-ended reasoning. See the nexus.tool.code_exec.contract and nexus.tool.code_exec.bindings prompt sections for the full contract and the live binding list.`

// runCodeContract is the detailed contract rendered as a system-prompt
// section. The LLM needs enough structure to produce a valid script without
// multiple round-trips, but this does not belong in the tool schema itself.
const runCodeContract = `The run_code tool runs a short Go program to collapse computational / mechanical work into one turn.

When to use:
  - Chaining multiple tool calls, looping over items, parsing or transforming
    structured data, math, hashing, date arithmetic, grouping / aggregation,
    fan-out across independent work. Collapses many sequential tool_use turns
    into one and gives deterministic results.

When NOT to use:
  - Prose, summarization, translation, explanation, or other open-ended
    reasoning — a script adds latency and a failure surface with no
    correctness benefit. If the task is "think and write text", answer
    directly.

Contract — your script must be valid Go, package main, and declare

    func Run(ctx context.Context) (any, error)

Its return value is JSON-marshaled back to you. Anything printed to stdout is
also returned (up to a per-call byte cap).

Available imports:
  - Go stdlib (pure compute, no filesystem/network): fmt, strings, strconv,
    bytes, regexp, unicode, unicode/utf8, encoding/json, encoding/base64,
    encoding/hex, encoding/csv, encoding/xml, crypto/sha256, crypto/md5,
    crypto/hmac, crypto/rand, math, math/big, math/rand/v2, math/bits, sort,
    container/heap, container/list, errors, time, context, sync, sync/atomic,
    io, bufio, hash, hash/crc32, hash/fnv.
  - tools: type-safe bindings for every tool available to you on this turn.
    Names are PascalCased from the tool's snake_case name: "shell" →
    tools.Shell. The authoritative list for this session is in the
    nexus.tool.code_exec.bindings prompt section — consult it instead of
    guessing. Args struct mirrors the tool's parameter schema:
    tools.Shell(tools.ShellArgs{Command: "ls"}). Return type depends on
    whether the tool declares an output schema:
      * Schema-ful tools return a typed struct tools.<Tool>Result with fields
        from the schema. Example: r, err := tools.Shell(tools.ShellArgs{...});
        use r.Stdout, r.Stderr, r.ExitCode.
      * Schema-less tools return tools.Result{Output, Error, OutputFile}
        where Output is a plain string you parse yourself.
  - parallel: Map(ctx, items, fn) / ForEach(ctx, items, fn) / All(ctx, fns...).
    Use these to fan out independent work (tool calls or compute) instead of
    serializing — first error cancels the rest. Map returns any; cast to the
    typed slice: results.([]T).
  - skills/<skill_name>: helper functions shipped with currently-active skills.

Forbidden: import "os", "net/*", "syscall", "unsafe", "reflect", "runtime";
any goroutines (go statement); any filesystem or network access that bypasses
tools.*.

Errors: a non-nil second return value from Run is surfaced verbatim. Tool
veto, rate-limit, or timeout also surface as errors returned from the tools.*
call that triggered them.`
