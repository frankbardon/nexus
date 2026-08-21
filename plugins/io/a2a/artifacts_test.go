package a2a

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// ---- helpers ----

// toolCall is one scripted tool round-trip: what the agent asked for and what
// came back.
type toolCall struct {
	name       string
	id         string
	output     string
	failure    string
	structured map[string]any
	outputFile string
}

// scriptedToolTurn plays a turn that calls tools before answering, in the order
// the ReAct agent emits: the turn opens, the model asks for tools, each tool
// runs, the model answers, the output gates publish it, the turn ends.
func scriptedToolTurn(answer string, calls ...toolCall) func(engine.EventBus, events.UserInput) {
	return func(bus engine.EventBus, in events.UserInput) {
		turnID := "turn-" + in.Content
		_ = bus.Emit("agent.turn.start", events.TurnInfo{
			SchemaVersion: events.TurnInfoVersion, TurnID: turnID, SessionID: in.SessionID,
		})
		for _, c := range calls {
			_ = bus.Emit("tool.invoke", events.ToolCall{
				SchemaVersion: events.ToolCallVersion,
				ID:            c.id, Name: c.name, TurnID: turnID,
				Arguments: map[string]any{"probe": c.id},
			})
			_ = bus.Emit("tool.result", events.ToolResult{
				SchemaVersion: events.ToolResultVersion,
				ID:            c.id, Name: c.name, TurnID: turnID,
				Output: c.output, Error: c.failure,
				OutputStructured: c.structured,
				OutputFile:       c.outputFile,
			})
		}
		_ = bus.Emit("llm.response", events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion, Content: answer, FinishReason: "end_turn",
			Usage: events.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18},
			Model: "test-model",
		})
		_ = bus.Emit("io.output", events.AgentOutput{
			SchemaVersion: events.AgentOutputVersion, Content: answer, Role: "assistant", TurnID: turnID,
		})
		_ = bus.Emit("agent.turn.end", events.TurnInfo{
			SchemaVersion: events.TurnInfoVersion, TurnID: turnID,
		})
	}
}

// blockingSend runs one blocking SendMessage and returns the Task it answered
// with. Blocking rather than streaming on purpose: blockOnTask drains the run's
// frame channel as the turn produces it, so a test that emits hundreds of
// artifacts is exercising the budget rather than the channel buffer.
func blockingSend(t *testing.T, p *Plugin, prompt, contextID string, mutate ...func(*http.Request)) a2a.Task {
	t.Helper()
	args := append([]func(*http.Request){
		withVersion("1.0"),
		jsonrpcBody(t, a2a.MethodSendMessage, sendMessageParams(prompt, contextID)),
	}, mutate...)
	rec := do(t, p.server, http.MethodPost, "/a2a", args...)
	if rec.Code != http.StatusOK {
		t.Fatalf("SendMessage status = %d: %s", rec.Code, rec.Body)
	}
	resp := rpcResponse(t, rec.Body.Bytes())
	if resp.Error != nil {
		t.Fatalf("SendMessage failed: %+v", resp.Error)
	}
	var out a2a.SendMessageResponse
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("decoding the SendMessage result: %v (%s)", err, resp.Result)
	}
	if out.Task == nil {
		t.Fatal("SendMessage answered with no task")
	}
	return *out.Task
}

// storedArtifacts reads a task's artifacts back out of the durable store, which
// is where the volume bound actually has to hold.
func storedArtifacts(t *testing.T, p *Plugin, taskID string) []a2a.Artifact {
	t.Helper()
	rec, found, err := p.tasks.For(nexusauth.Principal{}).Get(taskID)
	if err != nil {
		t.Fatalf("reading task %q back: %v", taskID, err)
	}
	if !found {
		t.Fatalf("task %q is not in the store", taskID)
	}
	return rec.Artifacts
}

// artifactByID finds one artifact in a task's set.
func artifactByID(artifacts []a2a.Artifact, suffix string) (a2a.Artifact, bool) {
	for _, a := range artifacts {
		if strings.HasSuffix(a.ArtifactID, suffix) {
			return a, true
		}
	}
	return a2a.Artifact{}, false
}

// totalArtifactBytes is the measurement the volume bound is stated in: the JSON
// encoding, which is what travels the wire and what lands in the store.
func totalArtifactBytes(artifacts []a2a.Artifact) int {
	total := 0
	for _, a := range artifacts {
		total += artifactSize(a)
	}
	return total
}

// ---- tool results ----

// TestToolResultsBecomeArtifacts is the story's blunt assertion: every tool
// result is published, without an operator having enabled anything.
func TestToolResultsBecomeArtifacts(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	playAgent(t, bus, scriptedToolTurn("done",
		toolCall{name: "web_search", id: "call-1", output: "three results"},
		toolCall{name: "read_file", id: "call-2", failure: "no such file"},
	))

	task := blockingSend(t, p, "use some tools", "ctx-tools")
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("task state = %s, want completed", task.Status.State)
	}

	first, ok := artifactByID(task.Artifacts, "-tool-call-1")
	if !ok {
		t.Fatalf("no artifact for the first tool call; got %d artifacts", len(task.Artifacts))
	}
	if first.Name != "web_search" {
		t.Errorf("artifact name = %q, want the tool name", first.Name)
	}
	if text, _ := first.Parts[0].TextValue(); text != "three results" {
		t.Errorf("artifact text = %q, want the tool output", text)
	}
	if first.Metadata[metadataToolCallID] != "call-1" {
		t.Errorf("artifact does not carry the call id: %v", first.Metadata)
	}

	// A FAILED tool is still an artifact, flagged rather than omitted: "the tool
	// ran and failed" is a fact the client needs as much as a success.
	failed, ok := artifactByID(task.Artifacts, "-tool-call-2")
	if !ok {
		t.Fatal("no artifact for the failed tool call")
	}
	if failed.Metadata[metadataToolFailed] != true {
		t.Errorf("failed tool artifact is not flagged: %v", failed.Metadata)
	}

	// And every one of them reached the store, not only the wire.
	if got := len(storedArtifacts(t, p, task.ID)); got != len(task.Artifacts) {
		t.Errorf("stored artifacts = %d, wire artifacts = %d", got, len(task.Artifacts))
	}
}

// TestToolArtifactsPrecedeTheTerminalStatus pins E1-S4's ordering rule against
// the new artifact volume: A2A closes a stream on the frame reporting a terminal
// state, so an artifact queued after it would never be delivered.
func TestToolArtifactsPrecedeTheTerminalStatus(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	playAgent(t, bus, scriptedToolTurn("done",
		toolCall{name: "shell", id: "call-1", output: "ok"},
		toolCall{name: "shell", id: "call-2", output: "ok"},
	))

	rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
		jsonrpcBody(t, a2a.MethodSendStreamingMessage, sendMessageParams("stream tools", "ctx-order")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	fs := frames(t, rec.Body.Bytes())
	terminalAt := -1
	for i, f := range fs {
		if f.Kind() == a2a.StreamPayloadStatusUpdate && f.StatusUpdate.Status.State.IsTerminal() {
			terminalAt = i
		}
	}
	if terminalAt != len(fs)-1 {
		t.Fatalf("terminal frame at %d of %d; nothing may follow it", terminalAt, len(fs)-1)
	}
	artifacts := 0
	for _, f := range fs {
		if f.Kind() == a2a.StreamPayloadArtifactUpdate {
			artifacts++
		}
	}
	// Two tool results plus the response.
	if artifacts != 3 {
		t.Errorf("artifact frames = %d, want 3 (two tool results and the response)", artifacts)
	}
}

// TestStructuredToolOutputRidesAJSONPart pins that a tool's typed result is a
// JSON document on the artifact rather than a string a client has to re-parse.
func TestStructuredToolOutputRidesAJSONPart(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	playAgent(t, bus, scriptedToolTurn("done", toolCall{
		name: "check_file_size", id: "call-1", output: "1024 bytes",
		structured: map[string]any{"bytes": 1024, "path": "notes.md"},
	}))

	task := blockingSend(t, p, "measure something", "ctx-structured")
	art, ok := artifactByID(task.Artifacts, "-tool-call-1")
	if !ok {
		t.Fatal("no tool artifact")
	}
	if len(art.Parts) != 2 {
		t.Fatalf("artifact parts = %d, want a text part and a json part", len(art.Parts))
	}
	if art.Parts[1].MediaType != mediaTypeJSON {
		t.Errorf("structured part mediaType = %q, want %q", art.Parts[1].MediaType, mediaTypeJSON)
	}
	var decoded map[string]any
	if err := json.Unmarshal(art.Parts[1].Data, &decoded); err != nil {
		t.Fatalf("the structured part is not decodable JSON: %v", err)
	}
	if decoded["path"] != "notes.md" {
		t.Errorf("structured part = %v, want the tool's typed result", decoded)
	}
}

// ---- structured output ----

// TestStructuredOutputBecomesAJSONPart is the first acceptance criterion: a
// turn whose answer is a JSON document publishes it as application/json, not
// only as prose.
func TestStructuredOutputBecomesAJSONPart(t *testing.T) {
	const answer = `{"verdict":"approved","score":0.91}`
	p, bus := newTestPlugin(t, nil)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		in, ok := e.Payload.(events.UserInput)
		if !ok {
			return
		}
		// The schema is declared on the request the way a provider with native
		// structured output sees it.
		_ = bus.Emit("llm.request", events.LLMRequest{
			SchemaVersion: events.LLMRequestVersion,
			ResponseFormat: &events.ResponseFormat{
				Type: "json_schema", Name: "verdict", Schema: map[string]any{"type": "object"},
			},
		})
		scriptedTurn(answer)(bus, in)
	}, engine.WithSource("test.agent"))

	task := blockingSend(t, p, "decide", "ctx-json")
	art, ok := artifactByID(task.Artifacts, artifactSuffix)
	if !ok {
		t.Fatal("no response artifact")
	}
	if len(art.Parts) != 2 {
		t.Fatalf("response parts = %d, want the text part and a json part", len(art.Parts))
	}
	if text, _ := art.Parts[0].TextValue(); text != answer {
		t.Errorf("first part is not the text answer: %q", text)
	}
	if art.Parts[1].MediaType != mediaTypeJSON {
		t.Errorf("json part mediaType = %q, want %q", art.Parts[1].MediaType, mediaTypeJSON)
	}
	if art.Metadata[metadataJSONSchema] != "verdict" {
		t.Errorf("response artifact does not name the schema: %v", art.Metadata)
	}
}

// TestFencedJSONIsUnwrapped covers the shape models actually emit.
func TestFencedJSONIsUnwrapped(t *testing.T) {
	raw, ok := structuredJSON("```json\n{\"a\":1}\n```")
	if !ok {
		t.Fatal("a fenced JSON object was not recognized")
	}
	if strings.TrimSpace(string(raw)) != `{"a":1}` {
		t.Errorf("unwrapped json = %q", raw)
	}
}

// TestProseIsNotStructuredOutput pins the other side: an ordinary answer must
// not acquire a JSON part, and a bare scalar is prose as far as this is
// concerned.
func TestProseIsNotStructuredOutput(t *testing.T) {
	for _, text := range []string{"the answer is 42", "42", `"yes"`, "true", "{not json"} {
		if _, ok := structuredJSON(text); ok {
			t.Errorf("%q was treated as structured output", text)
		}
	}
}

// ---- files ----

// TestWrittenFilesBecomeInlineArtifacts is the file half of the story: a path a
// tool reported writing becomes an artifact carrying the bytes.
func TestWrittenFilesBecomeInlineArtifacts(t *testing.T) {
	base := t.TempDir()
	const body = "# notes\n\nthe agent wrote this.\n"
	if err := os.WriteFile(filepath.Join(base, "notes.md"), []byte(body), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	p, bus := newTestPlugin(t, map[string]any{
		"artifacts": map[string]any{"file_base_dir": base},
	})
	playAgent(t, bus, scriptedToolTurn("written", toolCall{
		name: "write_file", id: "call-1", output: "wrote 30 bytes",
		structured: map[string]any{"path": "notes.md", "bytes_written": len(body)},
	}))

	task := blockingSend(t, p, "write a file", "ctx-file")
	art, ok := artifactByID(task.Artifacts, "-file-notes.md")
	if !ok {
		t.Fatalf("no artifact for the written file; got %+v", artifactIDs(task.Artifacts))
	}
	if len(art.Parts) != 1 || art.Parts[0].Kind() != a2a.PartKindRaw {
		t.Fatalf("file artifact parts = %+v, want one raw part", art.Parts)
	}
	if string(art.Parts[0].Raw) != body {
		t.Errorf("inline content = %q, want the file's bytes", art.Parts[0].Raw)
	}
	if art.Parts[0].Filename != "notes.md" {
		t.Errorf("raw part filename = %q", art.Parts[0].Filename)
	}
	if !strings.HasPrefix(art.Parts[0].MediaType, "text/markdown") {
		t.Errorf("raw part mediaType = %q, want a markdown type", art.Parts[0].MediaType)
	}
}

// TestOversizeFileDegradesToANote is the cap's stated failure mode. It must not
// be a silent drop and it must not be an unbounded inline.
func TestOversizeFileDegradesToANote(t *testing.T) {
	base := t.TempDir()
	big := strings.Repeat("x", 4096)
	if err := os.WriteFile(filepath.Join(base, "big.bin"), []byte(big), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	p, bus := newTestPlugin(t, map[string]any{
		"artifacts": map[string]any{"file_base_dir": base, "max_file_bytes": 1024},
	})
	playAgent(t, bus, scriptedToolTurn("written", toolCall{
		name: "write_file", id: "call-1", output: "wrote a lot",
		structured: map[string]any{"path": "big.bin"},
	}))

	task := blockingSend(t, p, "write a big file", "ctx-big")
	art, ok := artifactByID(task.Artifacts, "-file-big.bin")
	if !ok {
		t.Fatal("an oversized file was dropped instead of degrading to a note")
	}
	if art.Metadata[metadataOmitted] != true {
		t.Errorf("the note is not flagged as omitted content: %v", art.Metadata)
	}
	if art.Parts[0].Kind() != a2a.PartKindText {
		t.Fatalf("the note carries a %s part; an over-cap file must not be inlined", art.Parts[0].Kind())
	}
	text, _ := art.Parts[0].TextValue()
	if !strings.Contains(text, "big.bin") || !strings.Contains(text, "1024") {
		t.Errorf("the note does not say what was withheld or why: %q", text)
	}
	if artifactSize(art) > 1024 {
		t.Errorf("the degraded note is %d bytes; degradation must cost less than inlining", artifactSize(art))
	}
}

// TestFilesOutsideTheBaseDirAreDropped is the exfiltration guard. A tool that
// reports a path outside the configured directory is either broken or hostile,
// and inlining what it named into a response that leaves the process cannot be
// walked back.
func TestFilesOutsideTheBaseDirAreDropped(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "workspace")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	secret := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secret, []byte("do not publish"), 0o600); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	pol := artifactPolicy{fileBaseDir: base, fileSources: map[string][]string{"write_file": {"path"}}}
	for _, reported := range []string{"../secret.txt", secret, "./../secret.txt"} {
		files := detectWrittenFiles(events.ToolResult{
			Name: "write_file", OutputStructured: map[string]any{"path": reported},
		}, pol)
		if len(files) != 0 {
			t.Errorf("path %q escaped the base directory: %+v", reported, files)
		}
	}
}

// TestSymlinkedFilesOutsideTheBaseDirAreDropped covers the case a lexical check
// alone would admit: a link INSIDE the workspace pointing out of it.
func TestSymlinkedFilesOutsideTheBaseDirAreDropped(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "workspace")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	secret := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secret, []byte("do not publish"), 0o600); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(base, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	pol := artifactPolicy{fileBaseDir: base, fileSources: map[string][]string{"write_file": {"path"}}}
	files := detectWrittenFiles(events.ToolResult{
		Name: "write_file", OutputStructured: map[string]any{"path": "link.txt"},
	}, pol)
	if len(files) != 0 {
		t.Errorf("a symlink out of the workspace was followed and published: %+v", files)
	}
}

// TestOutputFileIsHonouredForEveryTool pins the generic engine field: any tool
// may report a written path there, which is the seam a shell wrapper uses.
func TestOutputFileIsHonouredForEveryTool(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "report.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	pol := artifactPolicy{fileBaseDir: base, fileSources: nil}
	files := detectWrittenFiles(events.ToolResult{Name: "shell", OutputFile: "report.txt"}, pol)
	if len(files) != 1 || filepath.Base(files[0].resolved) != "report.txt" {
		t.Fatalf("OutputFile was not detected: %+v", files)
	}
}

// TestUninstrumentedWritesAreMissedByDesign pins the documented limitation, so
// that the day someone adds workspace snapshot-diffing they have to come here
// and change a test that says why it was not done.
func TestUninstrumentedWritesAreMissedByDesign(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "sneaky.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	pol := artifactPolicy{fileBaseDir: base, fileSources: map[string][]string{"write_file": {"path"}}}
	// A shell command that redirected into the workspace reports stdout and an
	// exit code, and nothing about the file. Detection is tool.result-based, so
	// there is nothing to detect.
	files := detectWrittenFiles(events.ToolResult{
		Name:             "run_shell",
		Output:           "",
		OutputStructured: map[string]any{"stdout": "", "exit_code": 0},
	}, pol)
	if len(files) != 0 {
		t.Fatalf("an uninstrumented write was detected; the documented limitation no longer holds: %+v", files)
	}
}

// ---- the volume bound ----

// TestArtifactVolumeStaysBoundedOnALongTurn is the load-bearing test of this
// story, and it fails rather than warns.
//
// Unconditional tool-result artifacts, times inline file parts, times a
// disk-persisted store is an unbounded product. The claim under test is that
// three caps make it a bounded one: one task's artifacts stay under
// artifacts.max_task_bytes however chatty the turn was, and the store's total
// stays under that times tasks.max_per_context.
func TestArtifactVolumeStaysBoundedOnALongTurn(t *testing.T) {
	const (
		maxTaskBytes  = 64 * 1024
		maxToolOutput = 2 * 1024
		toolCalls     = 400
		perContext    = 3
	)

	p, bus := newTestPlugin(t, map[string]any{
		"tasks": map[string]any{"max_per_context": perContext},
		"artifacts": map[string]any{
			"max_task_bytes":        maxTaskBytes,
			"max_tool_output_bytes": maxToolOutput,
		},
	})

	// Every tool result is far larger than the per-result cap, so each one is
	// truncated AND the aggregate would blow the budget many times over.
	calls := make([]toolCall, 0, toolCalls)
	for i := range toolCalls {
		calls = append(calls, toolCall{
			name:   "shell",
			id:     fmt.Sprintf("call-%d", i),
			output: strings.Repeat("x", 32*1024),
		})
	}
	playAgent(t, bus, scriptedToolTurn("done", calls...))

	// More tasks than retention keeps, so the eviction half of the bound is
	// exercised too.
	var taskIDs []string
	for i := range perContext + 2 {
		task := blockingSend(t, p, fmt.Sprintf("long turn %d", i), "ctx-volume")
		if task.Status.State != a2a.TaskStateCompleted {
			t.Fatalf("turn %d state = %s, want completed", i, task.Status.State)
		}
		taskIDs = append(taskIDs, task.ID)

		// The per-artifact cap holds for every artifact the turn published.
		for _, art := range task.Artifacts {
			if strings.HasSuffix(art.ArtifactID, artifactSuffix) {
				continue
			}
			if size := artifactSize(art); size > maxToolOutput*2 {
				t.Fatalf("artifact %q is %d bytes; the %d byte tool-output cap did not hold",
					art.ArtifactID, size, maxToolOutput)
			}
		}

		// The per-task budget holds. Measured over everything except the final
		// response, which is the turn's answer and is never suppressed.
		var auxiliary []a2a.Artifact
		for _, art := range task.Artifacts {
			if !strings.HasSuffix(art.ArtifactID, artifactSuffix) {
				auxiliary = append(auxiliary, art)
			}
		}
		if got := totalArtifactBytes(auxiliary); got > maxTaskBytes {
			t.Fatalf("turn %d published %d artifact bytes; artifacts.max_task_bytes is %d",
				i, got, maxTaskBytes)
		}

		// And the client was TOLD it was not being shown everything, rather than
		// the artifacts simply stopping.
		notice, ok := artifactByID(task.Artifacts, "-artifacts-truncated")
		if !ok {
			t.Fatalf("turn %d spent its budget silently; no suppression notice was published", i)
		}
		suppressed, _ := notice.Metadata[metadataSuppressed].(float64)
		if suppressed <= 0 {
			// The wire value round-trips through JSON, so it decodes as a float.
			if n, isInt := notice.Metadata[metadataSuppressed].(int); isInt {
				suppressed = float64(n)
			}
		}
		if suppressed <= 0 {
			t.Errorf("turn %d suppression notice reports %v suppressed artifacts",
				i, notice.Metadata[metadataSuppressed])
		}
	}

	// The store as a whole. Retention keeps at most perContext tasks, and each
	// is bounded above, so the product is the stated worst case. Exceeding it is
	// a failure, not a warning: this is the number an operator sizes a disk on.
	kept := 0
	storeBytes := 0
	for _, id := range taskIDs {
		rec, found, err := p.tasks.For(nexusauth.Principal{}).Get(id)
		if err != nil {
			t.Fatalf("reading task %q: %v", id, err)
		}
		if !found {
			continue
		}
		kept++
		storeBytes += totalArtifactBytes(rec.Artifacts)
	}
	if kept > perContext {
		t.Fatalf("the store kept %d tasks; tasks.max_per_context is %d", kept, perContext)
	}
	// The response artifact is the only thing outside the budget, and it is the
	// turn's answer, so the ceiling is the budget plus a small allowance for it.
	const responseAllowance = 4 * 1024
	if ceiling := perContext * (maxTaskBytes + responseAllowance); storeBytes > ceiling {
		t.Fatalf("the store holds %d artifact bytes; the cap x retention ceiling is %d",
			storeBytes, ceiling)
	}
}

// TestBudgetRefusesRatherThanOverruns pins the accounting directly, without a
// turn in the way.
func TestBudgetRefusesRatherThanOverruns(t *testing.T) {
	b := newArtifactBudget(4096)
	spendable := 4096 - budgetNoticeReserve
	if !b.charge(spendable) {
		t.Fatal("an artifact that exactly fits was refused")
	}
	if b.charge(1) {
		t.Fatal("an artifact past the budget was admitted")
	}
	if b.remaining < 0 {
		t.Fatalf("the budget went negative: %d", b.remaining)
	}
	if !b.needsNotice() {
		t.Fatal("the first refusal did not mint a notice")
	}
	if b.needsNotice() {
		t.Fatal("the notice was minted twice")
	}
	if b.suppressed != 1 {
		t.Errorf("suppressed = %d, want 1", b.suppressed)
	}
}

// TestZeroBudgetIsUnbounded pins the documented escape hatch, so the foot-gun is
// at least a deliberate one.
func TestZeroBudgetIsUnbounded(t *testing.T) {
	b := newArtifactBudget(0)
	for range 1000 {
		if !b.charge(1 << 20) {
			t.Fatal("max_task_bytes: 0 refused an artifact; it must disable the budget")
		}
	}
	if b.suppressed != 0 {
		t.Errorf("suppressed = %d with the budget disabled", b.suppressed)
	}
}

// ---- acceptedOutputModes ----

// TestTextOnlyClientGetsNoBinaryParts pins that acceptedOutputModes is honoured
// rather than merely validated: a client that said "text/plain" is not sent
// base64.
func TestTextOnlyClientGetsNoBinaryParts(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "notes.md"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	p, bus := newTestPlugin(t, map[string]any{
		"artifacts": map[string]any{"file_base_dir": base},
	})
	playAgent(t, bus, scriptedToolTurn(`{"ok":true}`, toolCall{
		name: "write_file", id: "call-1", output: "wrote it",
		structured: map[string]any{"path": "notes.md"},
	}))

	params := sendMessageParams("write a file", "ctx-textonly")
	params["configuration"] = map[string]any{"acceptedOutputModes": []any{"text/plain"}}
	rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
		jsonrpcBody(t, a2a.MethodSendMessage, params))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	resp := rpcResponse(t, rec.Body.Bytes())
	if resp.Error != nil {
		t.Fatalf("SendMessage failed: %+v", resp.Error)
	}
	var out a2a.SendMessageResponse
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, art := range out.Task.Artifacts {
		for i, part := range art.Parts {
			if kind := part.Kind(); kind != a2a.PartKindText {
				t.Errorf("artifact %q part %d is %s; the client accepts text only",
					art.ArtifactID, i, kind)
			}
		}
	}
	// The file is still REPORTED, so the client learns it exists.
	if _, ok := artifactByID(out.Task.Artifacts, "-file-notes.md"); !ok {
		t.Error("a text-only client was not told the file existed at all")
	}
}

// TestAcceptsTextOnly covers the mode list readings that decide the above.
func TestAcceptsTextOnly(t *testing.T) {
	cases := map[string]struct {
		modes []string
		want  bool
	}{
		"unset":          {nil, false},
		"text only":      {[]string{"text/plain"}, true},
		"text wildcard":  {[]string{"text/*"}, true},
		"any":            {[]string{"*/*"}, false},
		"text plus json": {[]string{"text/plain", "application/json"}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := acceptsTextOnly(c.modes); got != c.want {
				t.Errorf("acceptsTextOnly(%v) = %v, want %v", c.modes, got, c.want)
			}
		})
	}
}

// ---- config ----

// TestArtifactDefaults pins the shipped numbers, which the configuration
// reference quotes.
func TestArtifactDefaults(t *testing.T) {
	cfg, err := parseConfig(map[string]any{"card": minimalCard()})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.artifacts.maxFileBytes != defaultMaxFileBytes {
		t.Errorf("max_file_bytes default = %d, want %d", cfg.artifacts.maxFileBytes, defaultMaxFileBytes)
	}
	if cfg.artifacts.maxToolOutputBytes != defaultMaxToolOutputBytes {
		t.Errorf("max_tool_output_bytes default = %d, want %d",
			cfg.artifacts.maxToolOutputBytes, defaultMaxToolOutputBytes)
	}
	if cfg.artifacts.maxTaskBytes != defaultMaxTaskBytes {
		t.Errorf("max_task_bytes default = %d, want %d", cfg.artifacts.maxTaskBytes, defaultMaxTaskBytes)
	}
	if got := cfg.artifacts.fileSources["write_file"]; len(got) != 1 || got[0] != "path" {
		t.Errorf("default file_sources = %v, want write_file -> [path]", cfg.artifacts.fileSources)
	}
	if cfg.artifacts.fileBaseDir != "" {
		t.Errorf("file_base_dir has a config-independent default (%q); it must come from the session",
			cfg.artifacts.fileBaseDir)
	}
}

// TestFileSourcesReplaceTheDefault pins the wholesale-replacement rule.
func TestFileSourcesReplaceTheDefault(t *testing.T) {
	cfg, err := parseConfig(testConfig(t, map[string]any{
		"artifacts": map[string]any{
			"file_sources": map[string]any{"save_report": []any{"output_path", "sidecar"}},
		},
	}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if _, present := cfg.artifacts.fileSources["write_file"]; present {
		t.Error("the default rule survived an explicit file_sources block")
	}
	if got := cfg.artifacts.fileSources["save_report"]; len(got) != 2 {
		t.Errorf("file_sources = %v", cfg.artifacts.fileSources)
	}
}

// TestArtifactConfigRefusals pins the boot errors an operator should get rather
// than a silently wrong cap.
func TestArtifactConfigRefusals(t *testing.T) {
	cases := map[string]any{
		"negative cap":       map[string]any{"max_file_bytes": -1},
		"fractional cap":     map[string]any{"max_task_bytes": 1.5},
		"non-mapping block":  "yes please",
		"empty source list":  map[string]any{"file_sources": map[string]any{"write_file": []any{}}},
		"non-mapping source": map[string]any{"file_sources": []any{"write_file"}},
	}
	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig(testConfig(t, map[string]any{"artifacts": block})); err == nil {
				t.Fatal("an invalid artifacts block was accepted")
			}
		})
	}
}

// artifactIDs renders an artifact set for a failure message.
func artifactIDs(artifacts []a2a.Artifact) []string {
	out := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		out = append(out, a.ArtifactID)
	}
	return out
}
