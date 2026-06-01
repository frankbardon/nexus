package predicates

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/sandbox"
	"github.com/frankbardon/nexus/pkg/engine/sandbox/host"
	"github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// silentLogger returns a slog logger that discards everything. Keeps
// test output clean.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newHostSandbox builds the host sandbox backend for command-predicate
// tests. The host backend executes `sh -c <script>` against the live
// kernel, which is what the evaluator expects.
func newHostSandbox(t *testing.T) sandbox.Sandbox {
	t.Helper()
	sb, err := host.New(map[string]any{})
	if err != nil {
		t.Fatalf("build host sandbox: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb
}

// newEvaluator builds an Evaluator wired with a fresh SchemaRegistry,
// the host sandbox, and a silent logger. Bus is nil; callers set it
// when they want to assert emissions.
func newEvaluator(t *testing.T) *Evaluator {
	t.Helper()
	logger := silentLogger()
	return NewEvaluator(engine.NewSchemaRegistry(logger), newHostSandbox(t), nil, logger)
}

// regexPred returns a regex predicate with its compiled regex
// pre-populated, mirroring what the workspace loader does.
func regexPred(name, pattern string, anchor workspace.RegexAnchor, message string) *workspace.Predicate {
	p := &workspace.Predicate{
		Type:    workspace.PredRegex,
		Name:    name,
		Pattern: pattern,
		Anchor:  anchor,
		Message: message,
	}
	p.SetCompiledRegex(regexp.MustCompile(pattern))
	return p
}

// ---------------------------------------------------------------------
// schema predicate
// ---------------------------------------------------------------------

func TestEvalSchema_Pass(t *testing.T) {
	e := newEvaluator(t)
	name := PredicateSchemaName("nexus.workflows.icm", "01_intake", "v")
	e.Schemas.Register(name, map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"type":                 "object",
		"required":             []any{"name"},
		"additionalProperties": false,
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
		},
	}, "test")

	p := &workspace.Predicate{Type: workspace.PredSchema, Name: "v", SchemaPath: name}
	sc := StageEvalContext{InstanceID: "nexus.workflows.icm", StageID: "01_intake"}

	r := e.Evaluate(context.Background(), p, []byte(`{"name":"frank"}`), sc)
	if !r.Verdict {
		t.Fatalf("expected pass; got fail: %s", r.Feedback)
	}
	if r.Name != "v" || r.Type != workspace.PredSchema {
		t.Errorf("name/type not preserved: %+v", r)
	}
	if r.Elapsed <= 0 {
		t.Errorf("expected non-zero elapsed time")
	}
}

func TestEvalSchema_ArtifactNotJSON(t *testing.T) {
	e := newEvaluator(t)
	name := "icm.test.s1.v"
	e.Schemas.Register(name, map[string]any{"type": "object"}, "test")
	p := &workspace.Predicate{Type: workspace.PredSchema, Name: "v", SchemaPath: name}

	r := e.Evaluate(context.Background(), p, []byte("not json"), StageEvalContext{})
	if r.Verdict {
		t.Errorf("expected fail (not json)")
	}
	if r.Feedback == "" {
		t.Errorf("expected non-empty feedback")
	}
}

func TestEvalSchema_ViolatesSchema(t *testing.T) {
	e := newEvaluator(t)
	name := "icm.test.s2.v"
	e.Schemas.Register(name, map[string]any{
		"type":     "object",
		"required": []any{"name"},
	}, "test")
	p := &workspace.Predicate{Type: workspace.PredSchema, Name: "v", SchemaPath: name}

	r := e.Evaluate(context.Background(), p, []byte(`{"other":"x"}`), StageEvalContext{})
	if r.Verdict {
		t.Errorf("expected fail (missing required)")
	}
	if r.Feedback == "" {
		t.Errorf("expected non-empty feedback")
	}
}

func TestEvalSchema_UnknownName(t *testing.T) {
	e := newEvaluator(t)
	p := &workspace.Predicate{Type: workspace.PredSchema, Name: "v", SchemaPath: "icm.never.registered"}

	r := e.Evaluate(context.Background(), p, []byte(`{}`), StageEvalContext{})
	if r.Verdict {
		t.Errorf("expected fail (unknown schema)")
	}
	if r.Feedback == "" {
		t.Errorf("expected non-empty feedback")
	}
}

func TestEvalSchema_CompileCache(t *testing.T) {
	e := newEvaluator(t)
	sc := StageEvalContext{InstanceID: "nexus.workflows.icm", StageID: "s3"}
	name := PredicateSchemaName(sc.InstanceID, sc.StageID, "v")
	e.Schemas.Register(name, map[string]any{"type": "object"}, "test")
	p := &workspace.Predicate{Type: workspace.PredSchema, Name: "v", SchemaPath: name}

	_ = e.Evaluate(context.Background(), p, []byte(`{}`), sc)
	if _, ok := e.schemaCompileCache[name]; !ok {
		t.Fatalf("schema not cached after first evaluation")
	}
	firstPtr := e.schemaCompileCache[name]

	_ = e.Evaluate(context.Background(), p, []byte(`{}`), sc)
	secondPtr := e.schemaCompileCache[name]
	if firstPtr != secondPtr {
		t.Errorf("expected cached schema to be reused; got distinct compile result")
	}
}

func TestEvalSchema_RegistryNotConfigured(t *testing.T) {
	// Build an evaluator with Schemas left nil to exercise the
	// not-configured branch.
	e := NewEvaluator(nil, nil, nil, silentLogger())
	p := &workspace.Predicate{Type: workspace.PredSchema, Name: "v", SchemaPath: "x"}
	r := e.Evaluate(context.Background(), p, []byte(`{}`), StageEvalContext{})
	if r.Verdict {
		t.Errorf("expected fail when registry is nil")
	}
	if r.Feedback == "" {
		t.Errorf("expected non-empty feedback when registry is nil")
	}
}

// ---------------------------------------------------------------------
// regex predicate
// ---------------------------------------------------------------------

func TestEvalRegex_WholeAnchor(t *testing.T) {
	e := newEvaluator(t)
	p := regexPred("body", `(?s)foo.*bar`, workspace.AnchorWhole, "")
	r := e.Evaluate(context.Background(), p, []byte("foo\nmiddle\nbar"), StageEvalContext{})
	if !r.Verdict {
		t.Errorf("expected whole-anchor match to pass: %s", r.Feedback)
	}
}

func TestEvalRegex_FirstLine(t *testing.T) {
	e := newEvaluator(t)
	p := regexPred("h1", `^# .+`, workspace.AnchorFirstLine, "")
	r := e.Evaluate(context.Background(), p, []byte("# Title\n\nBody.\n"), StageEvalContext{})
	if !r.Verdict {
		t.Errorf("expected first_line match to pass: %s", r.Feedback)
	}
}

func TestEvalRegex_LastLine(t *testing.T) {
	e := newEvaluator(t)
	p := regexPred("done", `done\.$`, workspace.AnchorLastLine, "")
	r := e.Evaluate(context.Background(), p, []byte("Line 1\nLine 2\nWe are done.\n"), StageEvalContext{})
	if !r.Verdict {
		t.Errorf("expected last_line match to pass: %s", r.Feedback)
	}
}

func TestEvalRegex_NoMatch_CustomMessage(t *testing.T) {
	e := newEvaluator(t)
	p := regexPred("h1", `^# .+`, workspace.AnchorFirstLine, "first line must be H1")
	r := e.Evaluate(context.Background(), p, []byte("not a heading\n# late\n"), StageEvalContext{})
	if r.Verdict {
		t.Errorf("expected fail")
	}
	if r.Feedback != "first line must be H1" {
		t.Errorf("expected configured message, got %q", r.Feedback)
	}
}

func TestEvalRegex_NoMatch_DefaultMessage(t *testing.T) {
	e := newEvaluator(t)
	p := regexPred("h1", `^# .+`, workspace.AnchorFirstLine, "")
	r := e.Evaluate(context.Background(), p, []byte("not a heading\n"), StageEvalContext{})
	if r.Verdict {
		t.Errorf("expected fail")
	}
	if r.Feedback == "" {
		t.Errorf("expected default feedback when Message empty")
	}
}

func TestEvalRegex_NotCompiled(t *testing.T) {
	e := newEvaluator(t)
	p := &workspace.Predicate{Type: workspace.PredRegex, Pattern: `x`, Anchor: workspace.AnchorWhole}
	r := e.Evaluate(context.Background(), p, []byte("data"), StageEvalContext{})
	if r.Verdict {
		t.Errorf("expected fail when regex not compiled at load time")
	}
}

// ---------------------------------------------------------------------
// command predicate
// ---------------------------------------------------------------------

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("command predicate tests are POSIX-only")
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestEvalCommand_PassesOnExitZero(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "pass.sh", "#!/bin/sh\nexit 0\n")
	e := newEvaluator(t)
	p := &workspace.Predicate{Type: workspace.PredCommand, Name: "ok", Run: "pass.sh"}
	sc := StageEvalContext{WorkspaceRoot: dir}
	r := e.Evaluate(context.Background(), p, []byte("anything"), sc)
	if !r.Verdict {
		t.Fatalf("expected pass; got fail: %s", r.Feedback)
	}
}

func TestEvalCommand_StdoutBecomesFeedback(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "fail.sh", "#!/bin/sh\necho 'specific failure'\nexit 2\n")
	e := newEvaluator(t)
	p := &workspace.Predicate{Type: workspace.PredCommand, Name: "out", Run: "fail.sh"}
	sc := StageEvalContext{WorkspaceRoot: dir}
	r := e.Evaluate(context.Background(), p, []byte("artifact"), sc)
	if r.Verdict {
		t.Errorf("expected fail")
	}
	if r.Feedback != "specific failure" {
		t.Errorf("expected stdout as feedback, got %q", r.Feedback)
	}
}

func TestEvalCommand_StderrFallback(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "fail.sh", "#!/bin/sh\necho 'on stderr' 1>&2\nexit 3\n")
	e := newEvaluator(t)
	p := &workspace.Predicate{Type: workspace.PredCommand, Name: "err", Run: "fail.sh"}
	sc := StageEvalContext{WorkspaceRoot: dir}
	r := e.Evaluate(context.Background(), p, []byte("x"), sc)
	if r.Verdict {
		t.Errorf("expected fail")
	}
	if r.Feedback != "on stderr" {
		t.Errorf("expected stderr fallback, got %q", r.Feedback)
	}
}

func TestEvalCommand_Timeout(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "slow.sh", "#!/bin/sh\nwhile true; do sleep 1; done\n")
	e := newEvaluator(t)
	p := &workspace.Predicate{
		Type:           workspace.PredCommand,
		Name:           "slow",
		Run:            "slow.sh",
		TimeoutSeconds: 1,
	}
	sc := StageEvalContext{WorkspaceRoot: dir}
	start := time.Now()
	r := e.Evaluate(context.Background(), p, []byte("x"), sc)
	elapsed := time.Since(start)
	if r.Verdict {
		t.Errorf("expected fail on timeout")
	}
	// Allow up to ~5s: 1s context timeout + 2s sandbox WaitDelay + slack
	// for slow CI. The point of the assertion is that the timeout fired,
	// not that the kill is instantaneous.
	if elapsed > 5*time.Second {
		t.Errorf("timeout took too long: %s (expected ~1s + WaitDelay)", elapsed)
	}
	if r.Feedback == "" {
		t.Errorf("expected non-empty feedback on timeout")
	}
}

func TestEvalCommand_ScriptMissing(t *testing.T) {
	dir := t.TempDir()
	e := newEvaluator(t)
	p := &workspace.Predicate{Type: workspace.PredCommand, Name: "ghost", Run: "ghost.sh"}
	sc := StageEvalContext{WorkspaceRoot: dir}
	r := e.Evaluate(context.Background(), p, []byte("x"), sc)
	if r.Verdict {
		t.Errorf("expected fail when script does not exist")
	}
	if r.Feedback == "" {
		t.Errorf("expected non-empty feedback when script missing")
	}
}

func TestEvalCommand_StdinPipedToScript(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "magic.sh", `#!/bin/sh
read line
if [ "$line" = "magic" ]; then
  exit 0
else
  echo "wanted magic, got $line"
  exit 1
fi
`)
	e := newEvaluator(t)
	p := &workspace.Predicate{Type: workspace.PredCommand, Name: "stdin", Run: "magic.sh"}
	sc := StageEvalContext{WorkspaceRoot: dir}

	r := e.Evaluate(context.Background(), p, []byte("magic\n"), sc)
	if !r.Verdict {
		t.Errorf("expected pass on matching stdin: %s", r.Feedback)
	}
	r = e.Evaluate(context.Background(), p, []byte("nope\n"), sc)
	if r.Verdict {
		t.Errorf("expected fail on mismatched stdin")
	}
}

func TestEvalCommand_TimeoutPrecedence(t *testing.T) {
	e := &Evaluator{CommandTimeoutSecs: 99}
	sc := StageEvalContext{StageBudgetTimeoutSec: 42}
	p := &workspace.Predicate{TimeoutSeconds: 7}

	if got := e.resolveCommandTimeout(p, sc); got != 7*time.Second {
		t.Errorf("predicate timeout should win; got %s", got)
	}
	p.TimeoutSeconds = 0
	if got := e.resolveCommandTimeout(p, sc); got != 42*time.Second {
		t.Errorf("stage budget should win when predicate timeout zero; got %s", got)
	}
	sc.StageBudgetTimeoutSec = 0
	if got := e.resolveCommandTimeout(p, sc); got != 99*time.Second {
		t.Errorf("plugin default should win when others zero; got %s", got)
	}
	e.CommandTimeoutSecs = 0
	if got := e.resolveCommandTimeout(p, sc); got != defaultCommandTimeout {
		t.Errorf("hard default should win when everything zero; got %s", got)
	}
}

// ---------------------------------------------------------------------
// native predicate
// ---------------------------------------------------------------------

type fakeNative struct {
	verdict  bool
	feedback string
}

func (f *fakeNative) Evaluate(_ context.Context, _ map[string]any, _ []byte) NativeResult {
	return NativeResult{Verdict: f.verdict, Feedback: f.feedback}
}

func TestEvalNative_Registered(t *testing.T) {
	e := newEvaluator(t)
	e.RegisterNative("wc", &fakeNative{verdict: true, feedback: "ok"})
	p := &workspace.Predicate{Type: workspace.PredNative, Name: "wc", Handler: "wc"}
	r := e.Evaluate(context.Background(), p, []byte("x"), StageEvalContext{})
	if !r.Verdict {
		t.Errorf("expected pass: %s", r.Feedback)
	}
	if r.Name != "wc" {
		t.Errorf("name not preserved: %s", r.Name)
	}
	if r.Type != workspace.PredNative {
		t.Errorf("type not preserved: %s", r.Type)
	}
}

func TestEvalNative_Missing(t *testing.T) {
	e := newEvaluator(t)
	p := &workspace.Predicate{Type: workspace.PredNative, Name: "absent", Handler: "absent"}
	r := e.Evaluate(context.Background(), p, []byte("x"), StageEvalContext{})
	if r.Verdict {
		t.Errorf("expected fail (handler unregistered)")
	}
	if r.Feedback == "" {
		t.Errorf("expected non-empty feedback")
	}
}

func TestEvalNative_MissingHandlerName(t *testing.T) {
	e := newEvaluator(t)
	p := &workspace.Predicate{Type: workspace.PredNative, Name: "anon"}
	r := e.Evaluate(context.Background(), p, []byte("x"), StageEvalContext{})
	if r.Verdict {
		t.Errorf("expected fail when handler name missing")
	}
}

func TestEvalNative_ConcurrentRegisterLookup(t *testing.T) {
	e := newEvaluator(t)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			e.RegisterNative("h", &fakeNative{verdict: true})
		}(i)
		go func() {
			defer wg.Done()
			_, _ = e.LookupNative("h")
		}()
	}
	wg.Wait()
	if h, ok := e.LookupNative("h"); !ok || h == nil {
		t.Errorf("expected handler to be present after racing register/lookup")
	}
}

// ---------------------------------------------------------------------
// llm + human stubs
// ---------------------------------------------------------------------

func TestEvalLLM_NotConfigured(t *testing.T) {
	e := newEvaluator(t)
	p := &workspace.Predicate{Type: workspace.PredLLM, Name: "judge", Rubric: "x"}
	r := e.Evaluate(context.Background(), p, []byte("x"), StageEvalContext{})
	if r.Verdict {
		t.Errorf("expected fail (judge not configured)")
	}
	if r.Feedback == "" {
		t.Errorf("expected non-empty feedback")
	}
}

func TestEvalLLM_DispatcherInvoked(t *testing.T) {
	e := newEvaluator(t)
	var called bool
	score := 0.8
	e.Judge = func(_ context.Context, _ *workspace.Predicate, _ []byte, _ StageEvalContext) (bool, string, *float64, error) {
		called = true
		return true, "good", &score, nil
	}
	p := &workspace.Predicate{Type: workspace.PredLLM, Name: "judge", Rubric: "x"}
	r := e.Evaluate(context.Background(), p, []byte("x"), StageEvalContext{})
	if !called {
		t.Errorf("dispatcher should have been invoked")
	}
	if !r.Verdict || r.Feedback != "good" || r.Score == nil || *r.Score != 0.8 {
		t.Errorf("unexpected result: %+v", r)
	}
}

func TestEvalHuman_NotConfigured(t *testing.T) {
	e := newEvaluator(t)
	p := &workspace.Predicate{Type: workspace.PredHuman, Name: "review", Prompt: "ok?"}
	r := e.Evaluate(context.Background(), p, []byte("x"), StageEvalContext{})
	if r.Verdict {
		t.Errorf("expected fail (human not configured)")
	}
	if r.Feedback == "" {
		t.Errorf("expected non-empty feedback")
	}
}

func TestEvalHuman_DispatcherInvoked(t *testing.T) {
	e := newEvaluator(t)
	var called bool
	e.Human = func(_ context.Context, _ *workspace.Predicate, _ []byte, _ StageEvalContext) (bool, string, error) {
		called = true
		return true, "approved", nil
	}
	p := &workspace.Predicate{Type: workspace.PredHuman, Name: "review", Prompt: "ok?"}
	r := e.Evaluate(context.Background(), p, []byte("x"), StageEvalContext{})
	if !called {
		t.Errorf("dispatcher should have been invoked")
	}
	if !r.Verdict || r.Feedback != "approved" {
		t.Errorf("unexpected result: %+v", r)
	}
}

// ---------------------------------------------------------------------
// EvaluateAll
// ---------------------------------------------------------------------

func TestEvaluateAll_AllPass(t *testing.T) {
	e := newEvaluator(t)
	e.RegisterNative("a", &fakeNative{verdict: true})
	e.RegisterNative("b", &fakeNative{verdict: true})
	ps := []workspace.Predicate{
		{Type: workspace.PredNative, Name: "p1", Handler: "a"},
		{Type: workspace.PredNative, Name: "p2", Handler: "b"},
	}
	passed, results := e.EvaluateAll(context.Background(), ps, []byte("x"), StageEvalContext{})
	if !passed {
		t.Errorf("expected all to pass")
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results, got %d", len(results))
	}
	if results[0].Name != "p1" || results[1].Name != "p2" {
		t.Errorf("results out of order: %+v", results)
	}
}

func TestEvaluateAll_ShortCircuit(t *testing.T) {
	e := newEvaluator(t)
	e.RegisterNative("ok", &fakeNative{verdict: true})
	e.RegisterNative("bad", &fakeNative{verdict: false, feedback: "no"})
	e.RegisterNative("never", &fakeNative{verdict: false, feedback: "should not run"})
	ps := []workspace.Predicate{
		{Type: workspace.PredNative, Name: "p1", Handler: "ok"},
		{Type: workspace.PredNative, Name: "p2", Handler: "bad"},
		{Type: workspace.PredNative, Name: "p3", Handler: "never"},
	}
	passed, results := e.EvaluateAll(context.Background(), ps, []byte("x"), StageEvalContext{})
	if passed {
		t.Errorf("expected failure")
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results (short-circuit), got %d", len(results))
	}
	if results[1].Verdict {
		t.Errorf("expected second result to be the failure")
	}
}

func TestEvaluateAll_EmitsFailureEvent(t *testing.T) {
	bus := engine.NewEventBus()
	gotCh := make(chan icmtypes.ICMPredicateFailed, 1)
	unsub := bus.Subscribe("icm.predicate.failed", func(ev engine.Event[any]) {
		if p, ok := ev.Payload.(icmtypes.ICMPredicateFailed); ok {
			select {
			case gotCh <- p:
			default:
			}
		}
	})
	defer unsub()

	logger := silentLogger()
	e := NewEvaluator(engine.NewSchemaRegistry(logger), newHostSandbox(t), bus, logger)
	e.RegisterNative("bad", &fakeNative{verdict: false, feedback: "nope"})

	ps := []workspace.Predicate{{Type: workspace.PredNative, Name: "v1", Handler: "bad"}}
	sc := StageEvalContext{
		RunID:     "run-1",
		StageID:   "01_intake",
		ItemID:    "",
		Container: "output.validators",
	}
	passed, _ := e.EvaluateAll(context.Background(), ps, []byte("x"), sc)
	if passed {
		t.Fatalf("expected failure")
	}

	select {
	case got := <-gotCh:
		if got.RunID != "run-1" {
			t.Errorf("RunID mismatch: %s", got.RunID)
		}
		if got.StageID != "01_intake" {
			t.Errorf("StageID mismatch: %s", got.StageID)
		}
		if got.Container != "output.validators" {
			t.Errorf("Container mismatch: %s", got.Container)
		}
		if got.PredicateName != "v1" {
			t.Errorf("PredicateName mismatch: %s", got.PredicateName)
		}
		if got.PredicateType != string(workspace.PredNative) {
			t.Errorf("PredicateType mismatch: %s", got.PredicateType)
		}
		if got.Feedback != "nope" {
			t.Errorf("Feedback mismatch: %s", got.Feedback)
		}
		if got.SchemaVersion != icmtypes.ICMPredicateFailedVersion {
			t.Errorf("SchemaVersion mismatch: %d", got.SchemaVersion)
		}
	case <-time.After(time.Second):
		t.Fatalf("did not receive icm.predicate.failed event")
	}
}

func TestEvaluateAll_NilBusSkipsEmission(t *testing.T) {
	// Sanity check that EvaluateAll without a bus doesn't panic.
	e := newEvaluator(t) // bus nil
	e.RegisterNative("bad", &fakeNative{verdict: false})
	ps := []workspace.Predicate{{Type: workspace.PredNative, Name: "v1", Handler: "bad"}}
	passed, results := e.EvaluateAll(context.Background(), ps, []byte("x"), StageEvalContext{})
	if passed || len(results) != 1 {
		t.Errorf("unexpected outcome: passed=%v results=%d", passed, len(results))
	}
}

// ---------------------------------------------------------------------
// PredicateSchemaName helper
// ---------------------------------------------------------------------

func TestPredicateSchemaName(t *testing.T) {
	// Suffixed instance: only the segment after the last "/" enters the
	// name. This keeps registration (PostureBuilder) and dispatch
	// (evaluator) in lockstep with the canonical icmtypes helper.
	got := PredicateSchemaName("nexus.workflows.icm/research", "02_script", "schema_0")
	want := "icm.research.02_script.schema_0"
	if got != want {
		t.Errorf("suffixed: got %q want %q", got, want)
	}
	// Default instance (no slash): no suffix segment.
	got = PredicateSchemaName("nexus.workflows.icm", "01_intake", "v")
	want = "icm.01_intake.v"
	if got != want {
		t.Errorf("default: got %q want %q", got, want)
	}
}
