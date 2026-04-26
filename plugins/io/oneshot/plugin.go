// Package oneshot provides a non-interactive IO plugin that runs the agent
// for a single turn, auto-approves every approval request, and emits a JSON
// transcript of the session to stdout (and optionally a file).
//
// It is intended for scripting, batch processing, and CI use cases where no
// human is available to drive the TUI or browser UI. The name reflects its
// semantics: one prompt in, one transcript out, then the process exits.
package oneshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.io.oneshot"

// Plugin is the oneshot IO plugin. It has no UI; it feeds a single prompt
// into the agent, collects all events during the turn, auto-approves any
// approval requests, and finally writes a JSON transcript when the turn ends.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace
	unsubs  []func()

	// Config
	input      string // explicit input from config
	inputFile  string // path to a file whose contents are the input
	outputFile string // optional path to write JSON transcript
	pretty     bool   // pretty-print JSON
	readStdin  bool   // whether to read stdin when no other input source supplied

	// State captured during the run
	mu        sync.Mutex
	startedAt time.Time
	endedAt   time.Time
	turnDepth int  // number of in-flight turns (start - end)
	turnsSeen int  // total agent.turn.start events observed after input was sent
	inputSent bool // whether we have emitted the initial io.input
	finalized bool // whether we have already flushed output + ended the session

	finalOutput string
	plans       []planRecord
	planUpdates []planUpdateRecord
	thinking    []thinkingRecord
	approvals   []approvalRecord
	errorsLog   []errorRecord
}

// New creates a new oneshot IO plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Oneshot IO" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "io.output", Priority: 50},
		{EventType: "io.approval.request", Priority: 10}, // run early so we auto-approve first
		{EventType: "plan.approval.request", Priority: 10},
		{EventType: "io.ask", Priority: 10},
		{EventType: "plan.created", Priority: 50},
		{EventType: "agent.plan", Priority: 50},
		{EventType: "thinking.step", Priority: 50},
		{EventType: "agent.turn.start", Priority: 50},
		{EventType: "agent.turn.end", Priority: 50},
		{EventType: "core.error", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"io.input",
		"before:io.input",
		"io.approval.response",
		"plan.approval.response",
		"io.ask.response",
		"io.session.start",
		"io.session.end",
	}
}

// Init reads config and wires the event handlers.
func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session

	// Config — all fields optional.
	if v, ok := ctx.Config["input"].(string); ok {
		p.input = v
	}
	if v, ok := ctx.Config["input_file"].(string); ok {
		p.inputFile = engine.ExpandPath(v)
	}
	if v, ok := ctx.Config["output_file"].(string); ok {
		p.outputFile = engine.ExpandPath(v)
	}
	p.pretty = true
	if v, ok := ctx.Config["pretty"].(bool); ok {
		p.pretty = v
	}
	p.readStdin = true
	if v, ok := ctx.Config["read_stdin"].(bool); ok {
		p.readStdin = v
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("io.output", p.handleOutput, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.approval.request", p.handleApprovalRequest, engine.WithSource(pluginID)),
		p.bus.Subscribe("plan.approval.request", p.handlePlanApprovalRequest, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.ask", p.handleAsk, engine.WithSource(pluginID)),
		p.bus.Subscribe("plan.created", p.handlePlanCreated, engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.plan", p.handleAgentPlan, engine.WithSource(pluginID)),
		p.bus.Subscribe("thinking.step", p.handleThinking, engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.turn.start", p.handleTurnStart, engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.turn.end", p.handleTurnEnd, engine.WithSource(pluginID)),
		p.bus.Subscribe("core.error", p.handleError, engine.WithSource(pluginID)),
	)

	p.logger.Info("oneshot IO plugin initialized")
	return nil
}

// Ready resolves the prompt (env > config input > config input_file > stdin),
// emits io.session.start, then dispatches the prompt as an io.input event.
// All subsequent work happens inside event handlers until agent.turn.end
// triggers finalization.
func (p *Plugin) Ready() error {
	p.mu.Lock()
	p.startedAt = time.Now()
	p.mu.Unlock()

	_ = p.bus.Emit("io.session.start", events.SessionInfo{
		Transport: "oneshot",
	})

	prompt, err := p.resolvePrompt()
	if err != nil {
		p.logger.Error("oneshot: failed to resolve prompt", "error", err)
		// Finalize with an error so the run still produces a JSON document.
		p.mu.Lock()
		p.errorsLog = append(p.errorsLog, errorRecord{Source: pluginID, Message: err.Error()})
		p.mu.Unlock()
		p.finalize()
		return nil
	}

	// Kick off the single turn asynchronously so Ready() returns and the
	// engine can enter its main wait loop. The bus dispatches synchronously,
	// so running Emit in-line here would block Ready for the duration of the
	// entire agent run — which prevents the engine from installing its signal
	// handlers and session-end listener.
	go func() {
		p.mu.Lock()
		p.inputSent = true
		p.mu.Unlock()

		input := events.UserInput{Content: prompt}
		if veto, err := p.bus.EmitVetoable("before:io.input", &input); err == nil && veto.Vetoed {
			return
		}
		_ = p.bus.Emit("io.input", input)
	}()

	return nil
}

// Shutdown flushes any remaining state and unsubscribes.
func (p *Plugin) Shutdown(ctx context.Context) error {
	// If the agent never finished (e.g. shutdown via signal), still attempt
	// to write whatever we captured so the caller gets something actionable.
	p.finalize()

	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

// resolvePrompt picks the input source using this precedence:
//  1. NEXUS_ONESHOT_PROMPT environment variable
//  2. plugin config "input"
//  3. plugin config "input_file"
//  4. stdin, if it is piped (i.e. not a terminal)
func (p *Plugin) resolvePrompt() (string, error) {
	if v := strings.TrimSpace(os.Getenv("NEXUS_ONESHOT_PROMPT")); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(p.input); v != "" {
		return v, nil
	}
	if p.inputFile != "" {
		data, err := os.ReadFile(p.inputFile)
		if err != nil {
			return "", fmt.Errorf("reading input_file %q: %w", p.inputFile, err)
		}
		s := strings.TrimSpace(string(data))
		if s == "" {
			return "", fmt.Errorf("input_file %q is empty", p.inputFile)
		}
		return s, nil
	}
	if p.readStdin && !isStdinTTY() {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading stdin: %w", err)
		}
		s := strings.TrimSpace(string(data))
		if s == "" {
			return "", errors.New("no prompt provided: stdin was empty and no input/input_file/env var set")
		}
		return s, nil
	}
	return "", errors.New("no prompt provided: set NEXUS_ONESHOT_PROMPT, plugin config input/input_file, or pipe stdin")
}

func isStdinTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return true
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// -- event handlers --------------------------------------------------------

func (p *Plugin) handleOutput(e engine.Event[any]) {
	out, ok := e.Payload.(events.AgentOutput)
	if !ok {
		return
	}
	// Only keep the final assistant message(s). Streaming chunks and
	// system/tool role messages are ignored for the final output capture,
	// but errors are surfaced into the errors list.
	if out.Role == "error" {
		p.mu.Lock()
		p.errorsLog = append(p.errorsLog, errorRecord{
			Source:  "agent",
			Message: out.Content,
			TurnID:  out.TurnID,
		})
		p.mu.Unlock()
		return
	}
	if out.Role != "assistant" {
		return
	}
	// Unlike the TUI/browser plugins, we do NOT skip messages that were
	// streamed. The react agent emits a final io.output with the complete
	// content after streaming ends; for oneshot callers the full buffer is
	// exactly what we want to capture.

	p.mu.Lock()
	if p.finalOutput == "" {
		p.finalOutput = out.Content
	} else {
		// Multiple assistant messages in the same turn are concatenated.
		p.finalOutput += "\n" + out.Content
	}
	p.mu.Unlock()
}

func (p *Plugin) handleApprovalRequest(e engine.Event[any]) {
	req, ok := e.Payload.(events.ApprovalRequest)
	if !ok {
		return
	}
	p.mu.Lock()
	p.approvals = append(p.approvals, approvalRecord{
		Kind:         "tool",
		PromptID:     req.PromptID,
		Description:  req.Description,
		ToolCall:     req.ToolCall,
		Risk:         req.Risk,
		AutoApproved: true,
	})
	p.mu.Unlock()

	_ = p.bus.Emit("io.approval.response", events.ApprovalResponse{
		PromptID: req.PromptID,
		Approved: true,
		Always:   false,
	})
}

func (p *Plugin) handlePlanApprovalRequest(e engine.Event[any]) {
	req, ok := e.Payload.(events.ApprovalRequest)
	if !ok {
		return
	}
	p.mu.Lock()
	p.approvals = append(p.approvals, approvalRecord{
		Kind:         "plan",
		PromptID:     req.PromptID,
		Description:  req.Description,
		Risk:         req.Risk,
		AutoApproved: true,
	})
	p.mu.Unlock()

	_ = p.bus.Emit("plan.approval.response", events.ApprovalResponse{
		PromptID: req.PromptID,
		Approved: true,
		Always:   false,
	})
}

// handleAsk responds to io.ask events. In oneshot mode there is no human to
// answer questions, so we emit an empty response. The asking plugin is
// expected to handle empty answers gracefully; if it cannot, the agent run
// will surface an error that we capture in the JSON transcript.
func (p *Plugin) handleAsk(e engine.Event[any]) {
	ask, ok := e.Payload.(events.AskUser)
	if !ok {
		return
	}
	p.mu.Lock()
	p.approvals = append(p.approvals, approvalRecord{
		Kind:         "ask",
		PromptID:     ask.PromptID,
		Description:  ask.Question,
		AutoApproved: true,
	})
	p.mu.Unlock()

	_ = p.bus.Emit("io.ask.response", events.AskUserResponse{
		PromptID: ask.PromptID,
		Answer:   "",
	})
}

func (p *Plugin) handlePlanCreated(e engine.Event[any]) {
	result, ok := e.Payload.(events.PlanResult)
	if !ok {
		return
	}
	steps := make([]planStepRecord, len(result.Steps))
	for i, s := range result.Steps {
		steps[i] = planStepRecord{
			ID:          s.ID,
			Description: s.Description,
			Status:      s.Status,
			Order:       s.Order,
		}
	}
	p.mu.Lock()
	p.plans = append(p.plans, planRecord{
		PlanID:  result.PlanID,
		Summary: result.Summary,
		Source:  result.Source,
		TurnID:  result.TurnID,
		Steps:   steps,
	})
	p.mu.Unlock()
}

func (p *Plugin) handleAgentPlan(e engine.Event[any]) {
	plan, ok := e.Payload.(events.Plan)
	if !ok {
		return
	}
	steps := make([]planStepRecord, len(plan.Steps))
	for i, s := range plan.Steps {
		steps[i] = planStepRecord{
			Description: s.Description,
			Status:      s.Status,
			Order:       i + 1,
		}
	}
	p.mu.Lock()
	p.planUpdates = append(p.planUpdates, planUpdateRecord{
		TurnID: plan.TurnID,
		Steps:  steps,
	})
	p.mu.Unlock()
}

func (p *Plugin) handleThinking(e engine.Event[any]) {
	step, ok := e.Payload.(events.ThinkingStep)
	if !ok {
		return
	}
	p.mu.Lock()
	p.thinking = append(p.thinking, thinkingRecord{
		TurnID:    step.TurnID,
		Source:    step.Source,
		Phase:     step.Phase,
		Content:   step.Content,
		Timestamp: step.Timestamp,
	})
	p.mu.Unlock()
}

func (p *Plugin) handleError(e engine.Event[any]) {
	info, ok := e.Payload.(events.ErrorInfo)
	if !ok {
		return
	}
	msg := ""
	if info.Err != nil {
		msg = info.Err.Error()
	}
	p.mu.Lock()
	p.errorsLog = append(p.errorsLog, errorRecord{
		Source:  info.Source,
		Message: msg,
	})
	p.mu.Unlock()
}

func (p *Plugin) handleTurnStart(e engine.Event[any]) {
	p.mu.Lock()
	if p.inputSent {
		p.turnDepth++
		p.turnsSeen++
	}
	p.mu.Unlock()
}

func (p *Plugin) handleTurnEnd(e engine.Event[any]) {
	p.mu.Lock()
	if !p.inputSent {
		p.mu.Unlock()
		return
	}
	if p.turnDepth > 0 {
		p.turnDepth--
	}
	done := p.turnDepth == 0 && p.turnsSeen > 0 && !p.finalized
	p.mu.Unlock()

	if !done {
		return
	}

	// Finalize in a goroutine so we don't hold up the event dispatcher
	// while writing to stdout / disk and emitting io.session.end.
	go p.finalize()
}

// finalize is idempotent. It builds the JSON transcript, writes it to stdout
// and optionally to disk, then emits io.session.end to terminate the engine.
func (p *Plugin) finalize() {
	p.mu.Lock()
	if p.finalized {
		p.mu.Unlock()
		return
	}
	p.finalized = true
	p.endedAt = time.Now()

	transcript := oneshotTranscript{
		Schema:      "nexus.oneshot.transcript/v1",
		StartedAt:   p.startedAt.Format(time.RFC3339Nano),
		EndedAt:     p.endedAt.Format(time.RFC3339Nano),
		DurationMS:  p.endedAt.Sub(p.startedAt).Milliseconds(),
		FinalOutput: p.finalOutput,
		Plans:       cloneSlice(p.plans),
		PlanUpdates: cloneSlice(p.planUpdates),
		Thinking:    cloneSlice(p.thinking),
		Approvals:   cloneSlice(p.approvals),
		Errors:      cloneSlice(p.errorsLog),
	}
	if p.session != nil {
		transcript.SessionID = p.session.ID
	}
	outputFile := p.outputFile
	pretty := p.pretty
	p.mu.Unlock()

	var (
		data []byte
		err  error
	)
	if pretty {
		data, err = json.MarshalIndent(transcript, "", "  ")
	} else {
		data, err = json.Marshal(transcript)
	}
	if err != nil {
		p.logger.Error("oneshot: failed to marshal transcript", "error", err)
		_ = p.bus.Emit("io.session.end", events.SessionInfo{Transport: "oneshot"})
		return
	}

	// Always write to stdout. A trailing newline makes the output friendly
	// for shell pipelines (e.g. `nexus -config oneshot.yaml | jq .`).
	_, _ = os.Stdout.Write(data)
	_, _ = os.Stdout.Write([]byte("\n"))

	if outputFile != "" {
		if err := os.WriteFile(outputFile, data, 0o644); err != nil {
			p.logger.Error("oneshot: failed to write output file",
				"path", outputFile, "error", err)
		}
	}

	_ = p.bus.Emit("io.session.end", events.SessionInfo{Transport: "oneshot"})
}

func cloneSlice[T any](in []T) []T {
	if len(in) == 0 {
		return nil
	}
	out := make([]T, len(in))
	copy(out, in)
	return out
}
