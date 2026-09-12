package dynvars

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/nexusheaders"
)

// runtimeContextTag names the XML block. It is deliberately not the old
// "system-context": the content no longer rides the system prompt, and a tag
// that says otherwise would misdescribe where the model is reading it from.
// The underscore form matches every other structural tag in the codebase
// (<execution_plan>, <session_context>, <skill_context>).
const runtimeContextTag = "runtime_context"

const (
	pluginID   = "nexus.system.dynvars"
	pluginName = "Dynamic Variables"
	version    = "0.1.0"
)

// Plugin injects dynamic runtime variables into the turn's own user message,
// as an XML block prepended to it on the way to the provider.
//
// # Why the user message and not the system prompt
//
// These variables CHANGE EVERY TURN — the clock most obviously. A system
// prompt is the most cache-stable region of a request and sits at the very
// front of it, so putting a per-turn value there invalidates the prompt cache
// at position zero and re-bills the entire conversation on every turn. Moving
// them to the live user message puts the only volatile bytes at the END of the
// prompt, which is exactly where a cache wants its variance.
//
// # Why it never reaches stored history
//
// The block is applied to the OUTBOUND llm.request, not to the message the
// memory plugin persisted. That is what stops the obvious failure of per-turn
// injection: ten turns in, a history carrying ten increasingly stale copies of
// "Current time", contradicting each other. Nothing has to detect or strip an
// old block, because no old block is ever written — see decorate.
type Plugin struct {
	logger  *slog.Logger
	bus     engine.EventBus
	session *engine.SessionWorkspace
	system  *engine.SystemInfo
	replay  *engine.ReplayState
	config  varConfig
	unsubs  []func()
}

type varConfig struct {
	Date       bool
	Time       bool
	Timezone   bool
	CWD        bool
	SessionDir bool
	OS         bool

	// RequestHeaders names the X-Nexus-* request headers of the current turn
	// to surface in the prompt, by normalized header name (`X-Nexus-Timezone`
	// -> `timezone`). Empty by default.
	//
	// # Why an allowlist and not a boolean
	//
	// These values are client-controlled and unauthenticated — a caller writes
	// whatever it likes into them (see pkg/nexusheaders). The engine therefore
	// binds them into the RESERVED session-label namespace, which every
	// <session_context> builder filters out, so nothing reaches a prompt just
	// because somebody sent it. This setting is the deliberate exception, and
	// naming each header is what keeps it deliberate: an operator who wants the
	// caller's timezone in the prompt gets exactly that, and does not silently
	// also get whatever new header a client starts sending next month.
	RequestHeaders []string
}

// New creates a new dynamic variables plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

// Subscriptions declares the one hook this plugin needs: the outbound request,
// where the block is applied.
//
// Priority 20 puts it after the handlers that rewrite a request's shape —
// fanout (2), fallback (3), the schema registry (5), progressive discovery (8)
// and skills (15) — so none of them sees a message this plugin has already
// decorated, and a veto from any of them costs nothing to undo.
func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "before:llm.request", Priority: 20},
	}
}

func (p *Plugin) Emissions() []string { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.logger = ctx.Logger
	p.bus = ctx.Bus
	p.session = ctx.Session
	p.system = ctx.System
	p.replay = ctx.Replay

	if v, ok := ctx.Config["date"].(bool); ok {
		p.config.Date = v
	}
	if v, ok := ctx.Config["time"].(bool); ok {
		p.config.Time = v
	}
	if v, ok := ctx.Config["timezone"].(bool); ok {
		p.config.Timezone = v
	}
	if v, ok := ctx.Config["cwd"].(bool); ok {
		p.config.CWD = v
	}
	if v, ok := ctx.Config["session_dir"].(bool); ok {
		p.config.SessionDir = v
	}
	if v, ok := ctx.Config["os"].(bool); ok {
		p.config.OS = v
	}
	if raw, ok := ctx.Config["request_headers"].([]any); ok {
		for _, item := range raw {
			name, ok := item.(string)
			if !ok {
				return fmt.Errorf("nexus.system.dynvars: request_headers entries must be strings, got %T", item)
			}
			name = strings.ToLower(strings.TrimSpace(name))
			name = strings.TrimPrefix(name, strings.ToLower(nexusheaders.Prefix))
			if name == "" {
				continue
			}
			p.config.RequestHeaders = append(p.config.RequestHeaders, name)
		}
		sort.Strings(p.config.RequestHeaders)
	}

	if p.bus != nil {
		p.unsubs = append(p.unsubs, p.bus.Subscribe("before:llm.request",
			p.handleBeforeLLMRequest, engine.WithPriority(20), engine.WithSource(pluginID)))
	}

	p.logger.Info("dynamic variables plugin initialized")
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	p.unsubs = nil
	return nil
}

func (p *Plugin) buildSection() string {
	var lines []string

	// Replay caveat: time.Now() and os.Getwd() are non-deterministic. The
	// llm.response is served from the journaled stash so the output is
	// stable, but the request diverges from the original run. Warn once so
	// it shows up in replay diagnostics.
	if p.replay != nil && p.replay.Active() {
		engine.NondeterministicWarn(p.logger, "dynvars: time.Now / os.Getwd during replay")
	}

	now := time.Now()

	if p.config.Date {
		lines = append(lines, fmt.Sprintf("Current date: %s", now.Format("2006-01-02")))
	}
	if p.config.Time {
		lines = append(lines, fmt.Sprintf("Current time: %s", now.Format("15:04:05")))
	}
	if p.config.Timezone {
		zone, _ := now.Zone()
		lines = append(lines, fmt.Sprintf("Timezone: %s", zone))
	}
	if p.config.CWD {
		if cwd, err := os.Getwd(); err == nil {
			lines = append(lines, fmt.Sprintf("Working directory: %s", cwd))
		}
	}
	if p.config.SessionDir && p.session != nil {
		lines = append(lines, fmt.Sprintf("Session directory: %s", p.session.RootDir))
	}
	if p.config.OS && p.system != nil {
		lines = append(lines, fmt.Sprintf("OS: %s/%s", p.system.OS, p.system.Arch))
	}
	// Read per render, not cached at Init: the headers belong to the turn in
	// flight, and a session serving several callers rebinds them on every one.
	// A header the caller did not send contributes no line at all, rather than
	// an empty one the model would have to interpret.
	for _, name := range p.config.RequestHeaders {
		if p.session == nil {
			break
		}
		if value, ok := p.session.RequestHeader(name); ok && value != "" {
			lines = append(lines, fmt.Sprintf("Request header %s%s: %s", nexusheaders.Prefix, name, value))
		}
	}

	if len(lines) == 0 {
		return ""
	}

	return engine.XMLWrap(runtimeContextTag, strings.Join(lines, "\n"))
}

// handleBeforeLLMRequest applies the block to the request on its way out.
func (p *Plugin) handleBeforeLLMRequest(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	req, ok := vp.Original.(*events.LLMRequest)
	if !ok {
		return
	}
	block := p.buildSection()
	if block == "" {
		return
	}
	decorate(req, block)
}

// decorate prepends block to the request's last user message.
//
// # It copies before it writes, and that is the whole safety property
//
// req.Messages may share its backing array with whatever a memory.history
// provider handed the agent. nexus.memory.capped copies on query today, but
// this plugin cannot know which provider is active, and writing through an
// aliased slice would edit the CONVERSATION rather than the request — which is
// precisely how a history fills up with stale copies of a per-turn block.
// Cloning the slice (and the Parts of the one message touched) makes that
// impossible by construction rather than by the good behaviour of every
// provider that might ever be plugged in.
//
// The LAST user message is the target because that is the turn in flight; on a
// later ReAct iteration the tool results that follow it are the model's own
// work, and the block belongs with what the human said, not after it.
//
// A request with no user message at all — a planner or a gate repair prompt —
// is left alone. Inventing a user message to hold runtime variables would put
// words in the conversation that nobody said.
func decorate(req *events.LLMRequest, block string) {
	idx := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	// Idempotent: a request re-emitted through this hook must not stack a
	// second block on the same message.
	if strings.HasPrefix(req.Messages[idx].Content, "<"+runtimeContextTag+">") {
		return
	}

	req.Messages = slices.Clone(req.Messages)
	msg := req.Messages[idx]
	msg.Content = block + "\n" + msg.Content

	// A multimodal message may be serialized from Parts INSTEAD of Content
	// (see events.MessagePart), so a block written only to Content would be
	// silently dropped for exactly the requests most likely to need context.
	// A leading text part keeps it visible on both paths.
	if len(msg.Parts) > 0 {
		parts := make([]events.MessagePart, 0, len(msg.Parts)+1)
		parts = append(parts, events.MessagePart{Type: "text", Text: block})
		parts = append(parts, msg.Parts...)
		msg.Parts = parts
	}
	req.Messages[idx] = msg
}
