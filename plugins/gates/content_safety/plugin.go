package contentsafety

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.gate.content_safety"

// Check represents a named content safety check with a compiled regex.
type check struct {
	name    string
	pattern *regexp.Regexp
}

// builtinChecks defines all available content safety checks.
var builtinChecks = []struct {
	name      string
	configKey string
	pattern   string
}{
	{"email", "check_pii_email", `[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`},
	{"phone", "check_pii_phone", `\b(\+?1[-.\s]?)?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}\b`},
	{"ssn", "check_pii_ssn", `\b\d{3}-\d{2}-\d{4}\b`},
	{"api_key", "check_secrets_api_key", `(AKIA[0-9A-Z]{16})|(sk-[a-zA-Z0-9]{20,})|(ghp_[a-zA-Z0-9]{36})|(xox[bporas]-[a-zA-Z0-9\-]+)`},
	{"private_key", "check_secrets_private_key", `-----BEGIN\s+(RSA\s+|EC\s+|DSA\s+|OPENSSH\s+)?PRIVATE\s+KEY-----`},
	{"password", "check_secrets_password", `(?i)(password|passwd|pwd|secret|token)\s*[:=]\s*\S+`},
	{"credit_card", "check_credit_card", `\b\d{4}[- ]?\d{4}[- ]?\d{4}[- ]?\d{4}\b`},
	{"internal_ip", "check_ip_internal", `\b(10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3})\b`},
}

// New creates a new content safety gate plugin instance.
func New() engine.Plugin {
	return &Plugin{
		action:  "block",
		message: "Content blocked: contains sensitive information ({checks}).",
	}
}

// Plugin gates before:io.output events by running multiple programmatic checks
// for PII, secrets, and other sensitive content. Each check individually toggleable.
//
// Surface coverage:
//   - before:io.output is always scanned. This is the egress edge for top-level
//     agents (ReAct, planexec, orchestrator synthesis).
//   - before:tool.result is scanned only when ScanToolResults is true. The audit
//     surfaced this as the egress edge for sub-agent / delegate output, which
//     reaches the parent agent through tool results rather than io.output —
//     so without this opt-in, PII produced by a worker is invisible to the
//     gate. Default-off preserves prior behavior: tool results from
//     legitimately external sources (web_fetch, knowledge_search) can carry
//     phone numbers / addresses that aren't PII leaks, and enabling scanning
//     blanket-wide would generate false positives. Operators that ship
//     orchestrator topologies (worker outputs feeding parent) should turn
//     this on.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	checks           []check
	action           string // "block" or "redact"
	message          string
	scanToolResults  bool
	scanLLMResponses bool
	scanStream       bool
	streamHoldBack   int

	unsubs []func()
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Content Safety Gate" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["action"].(string); ok && (v == "block" || v == "redact") {
		p.action = v
	}
	if v, ok := ctx.Config["message"].(string); ok && v != "" {
		p.message = v
	}
	if v, ok := ctx.Config["scan_tool_results"].(bool); ok {
		p.scanToolResults = v
	}
	if v, ok := ctx.Config["scan_llm_responses"].(bool); ok {
		p.scanLLMResponses = v
	}
	// Streaming defaults to following scan_llm_responses. An operator who
	// asked for responses to be gated wants that guarantee to survive
	// streaming; without this the engine would quietly turn streaming off for
	// them instead (see engine.StreamUnsafePlugins), which is safe but slow.
	p.scanStream = p.scanLLMResponses
	if v, ok := ctx.Config["scan_stream"].(bool); ok {
		p.scanStream = v
	}
	switch v := ctx.Config["stream_hold_back"].(type) {
	case int:
		p.streamHoldBack = v
	case float64:
		p.streamHoldBack = int(v)
	}
	if p.streamHoldBack < 0 {
		p.streamHoldBack = 0
	}

	// Enable builtin checks (all on by default).
	for _, bc := range builtinChecks {
		enabled := true
		if v, ok := ctx.Config[bc.configKey].(bool); ok {
			enabled = v
		}
		if enabled {
			re, err := regexp.Compile(bc.pattern)
			if err != nil {
				p.logger.Error("failed to compile builtin pattern",
					"check", bc.name, "error", err)
				continue
			}
			p.checks = append(p.checks, check{name: bc.name, pattern: re})
		}
	}

	// Custom patterns.
	if v, ok := ctx.Config["custom_patterns"]; ok {
		p.loadCustomPatterns(v)
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:io.output", p.handleBeforeOutput,
			engine.WithPriority(10), engine.WithSource(pluginID)),
	)
	if p.scanToolResults {
		p.unsubs = append(p.unsubs,
			p.bus.Subscribe("before:tool.result", p.handleBeforeToolResult,
				engine.WithPriority(10), engine.WithSource(pluginID)),
		)
	}
	if p.scanLLMResponses {
		p.unsubs = append(p.unsubs,
			p.bus.Subscribe("before:llm.response", p.handleBeforeLLMResponse,
				engine.WithPriority(8), engine.WithSource(pluginID)),
		)
	}
	if p.scanStream {
		p.unsubs = append(p.unsubs,
			p.bus.Subscribe(engine.EventBeforeStreamChunk, p.handleBeforeStreamChunk,
				engine.WithPriority(8), engine.WithSource(pluginID)),
		)
	}

	checkNames := make([]string, len(p.checks))
	for i, c := range p.checks {
		checkNames[i] = c.name
	}
	p.logger.Info("content safety gate initialized",
		"checks", strings.Join(checkNames, ","),
		"action", p.action,
		"scan_tool_results", p.scanToolResults,
		"scan_llm_responses", p.scanLLMResponses,
		"scan_stream", p.scanStream,
		"stream_hold_back", p.streamHoldBack)
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	// Priority 8 puts content_safety first in the before:io.output pipeline:
	// redact-mode mutation must run before veto-only gates (stop_words at 10)
	// or redaction is dead code. json_schema sits between at 9 so format
	// retries operate on post-redaction content. See docs/configuration
	// reference and issue #121 for the rationale.
	subs := []engine.EventSubscription{
		{EventType: "before:io.output", Priority: 8},
	}
	if p.scanToolResults {
		subs = append(subs, engine.EventSubscription{EventType: "before:tool.result", Priority: 8})
	}
	if p.scanLLMResponses {
		subs = append(subs, engine.EventSubscription{EventType: "before:llm.response", Priority: 8})
	}
	if p.scanStream {
		subs = append(subs, engine.EventSubscription{EventType: engine.EventBeforeStreamChunk, Priority: 8})
	}
	return subs
}

func (p *Plugin) Emissions() []string {
	return []string{"io.output"}
}

func (p *Plugin) handleBeforeOutput(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	output, ok := vp.Original.(*events.AgentOutput)
	if !ok {
		return
	}

	// A gate-authored refusal from a before:llm.response veto has already been
	// adjudicated; re-judging it here would at best duplicate work and at worst
	// veto it into a blank, which is the outcome the substitution contract
	// exists to prevent.
	if engine.IsVetoSubstituted(output.Metadata) {
		return
	}

	if output.Role != "assistant" {
		return
	}

	// Run all checks, collect matches.
	var matched []string
	for _, c := range p.checks {
		if c.pattern.MatchString(output.Content) {
			matched = append(matched, c.name)
		}
	}

	if len(matched) == 0 {
		return
	}

	checkList := strings.Join(matched, ", ")

	switch p.action {
	case "block":
		// Actually blocks the output — ops-relevant, no gate-arranged
		// auto-recovery.
		p.logger.Info("content safety checks triggered",
			"checks", checkList, "action", p.action)
		msg := strings.ReplaceAll(p.message, "{checks}", checkList)
		vp.Veto = engine.VetoResult{
			Vetoed: true,
			Reason: fmt.Sprintf("Content safety: %s", checkList),
		}
		_ = p.bus.Emit("io.output", events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: msg,
			Role: "system",
		})

	case "redact":
		// Degraded-but-recovered: output proceeds with matches redacted.
		p.logger.Warn("content safety checks triggered",
			"checks", checkList, "action", p.action)
		content := output.Content
		for _, c := range p.checks {
			for _, name := range matched {
				if c.name == name {
					content = c.pattern.ReplaceAllString(content, "[REDACTED]")
				}
			}
		}
		output.Content = content
		// Don't veto — modified output proceeds.
	}
}

// handleBeforeToolResult mirrors handleBeforeOutput against a tool.result
// payload. Only active when scan_tool_results is enabled; rationale on the
// Plugin type doc-comment. Scans the textual Output field — binary OutputData
// and structured OutputStructured are not scanned (regex on bytes/maps is
// unreliable and adds false-positive risk).
func (p *Plugin) handleBeforeToolResult(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	result, ok := vp.Original.(*events.ToolResult)
	if !ok {
		return
	}
	if result.Output == "" {
		return
	}

	var matched []string
	for _, c := range p.checks {
		if c.pattern.MatchString(result.Output) {
			matched = append(matched, c.name)
		}
	}
	if len(matched) == 0 {
		return
	}

	checkList := strings.Join(matched, ", ")

	switch p.action {
	case "block":
		// Actually blocks the tool result — ops-relevant, no gate-arranged
		// auto-recovery.
		p.logger.Info("content safety checks triggered on tool.result",
			"tool", result.Name, "id", result.ID,
			"checks", checkList, "action", p.action)
		vp.Veto = engine.VetoResult{
			Vetoed: true,
			Reason: fmt.Sprintf("Content safety (tool.result): %s", checkList),
		}
		// Surface a synthetic error result so the calling agent sees something
		// concrete instead of a silent veto.
		_ = p.bus.Emit("tool.result", events.ToolResult{
			SchemaVersion: events.ToolResultVersion,
			ID:            result.ID,
			Name:          result.Name,
			Error:         "blocked by content_safety: " + checkList,
			TurnID:        result.TurnID,
		})
	case "redact":
		// Degraded-but-recovered: tool result proceeds with matches redacted.
		p.logger.Warn("content safety checks triggered on tool.result",
			"tool", result.Name, "id", result.ID,
			"checks", checkList, "action", p.action)
		content := result.Output
		for _, c := range p.checks {
			for _, name := range matched {
				if c.name == name {
					content = c.pattern.ReplaceAllString(content, "[REDACTED]")
				}
			}
		}
		result.Output = content
	}
}

// handleBeforeLLMResponse mirrors handleBeforeOutput against the model's raw
// response, before the agent loop consumes it. Only active when
// scan_llm_responses is enabled.
//
// This catches what before:io.output cannot: io.output fires after the loop
// has already executed the response's tool calls, so it can scrub the prose
// the user finally reads but cannot stop sensitive content from reaching a
// tool. Gating here does both — in block mode the substituted response
// carries no tool calls at all (see engine.PublishLLMResponse).
//
// Only the textual Content is scanned. Tool-call arguments are not: they are
// provider-shaped JSON, and regex over them is unreliable. A deployment that
// needs argument scanning should gate before:tool.invoke, which sees parsed
// arguments.
//
// Streaming caveat: a provider with stream: true has already emitted this
// text as llm.stream.chunk events. Redaction and blocking here keep the
// content out of conversation history and out of tool arguments, but cannot
// retract what a UI already rendered. Use stream: false where non-disclosure
// is the requirement.
func (p *Plugin) handleBeforeLLMResponse(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	resp, ok := vp.Original.(*events.LLMResponse)
	if !ok {
		return
	}
	if resp.Content == "" {
		return
	}

	var matched []string
	for _, c := range p.checks {
		if c.pattern.MatchString(resp.Content) {
			matched = append(matched, c.name)
		}
	}
	if len(matched) == 0 {
		return
	}

	checkList := strings.Join(matched, ", ")

	switch p.action {
	case "block":
		p.logger.Info("content safety checks triggered on llm.response",
			"model", resp.Model, "request_id", resp.RequestID,
			"checks", checkList, "action", p.action,
			"tool_calls_dropped", len(resp.ToolCalls))
		// Dictate the replacement text, then veto. PublishLLMResponse keeps
		// the Content set here and clears ToolCalls, so the agent loop
		// receives the operator's message rather than the flagged output and
		// never executes what the flagged response asked for.
		resp.Content = strings.ReplaceAll(p.message, "{checks}", checkList)
		vp.Veto = engine.VetoResult{
			Vetoed: true,
			Reason: fmt.Sprintf("Content safety (llm.response): %s", checkList),
		}

	case "redact":
		// Degraded-but-recovered: the response proceeds with matches redacted
		// and its tool calls intact. Mutation without veto is the replace
		// path — no substitution, no dropped tool calls.
		p.logger.Warn("content safety checks triggered on llm.response",
			"model", resp.Model, "request_id", resp.RequestID,
			"checks", checkList, "action", p.action)
		content := resp.Content
		for _, c := range p.checks {
			for _, name := range matched {
				if c.name == name {
					content = c.pattern.ReplaceAllString(content, "[REDACTED]")
				}
			}
		}
		resp.Content = content
	}
}

// streamScanLookbehind bounds how far back of the already-released text each
// segment is rescanned. A match that can still be acted on must end at or
// after the release boundary, so it cannot begin more than one match-length
// before it; 4 KiB is far longer than any credential, identifier or phrase
// these patterns describe. The bound is what keeps per-segment work constant
// instead of growing with the length of the response.
const streamScanLookbehind = 4096

// handleBeforeStreamChunk is the streaming counterpart of
// handleBeforeLLMResponse, and the reason this gate keeps its guarantee with
// streaming on.
//
// before:llm.response fires once the stream has finished, so a veto there
// governs history and tool execution but arrives after every token has been
// rendered. This hook runs before any text is released, so a block here is
// prevention rather than regret — and it holds for every transport, including
// ones whose protocol has no way to retract what they already sent.
//
// # Why detection is complete, and what the hold is actually for
//
// The scan runs over the turn's cumulative text, not over the delta, and acts
// on any match ending at or after the release boundary. That combination is
// what makes detection independent of how a provider happens to chunk: a
// match's final character is, by definition, new on the segment where the
// match first completes, so every match is seen on the segment that completes
// it however many deltas its earlier characters were spread across.
//
// The hold does something different and worth stating plainly, because it is
// easy to assume it is what does the detecting. It bounds the RESIDUE — how
// much of a match's leading text was already released when the block lands.
// Withholding the trailing non-whitespace run means a token-shaped pattern
// (an SSN, a card number, an API key, an email: every builtin here) is still
// entirely unreleased at the moment it completes, so blocking it leaks
// nothing at all rather than leaking all but its last character.
//
// # The one case that is not clean
//
// A pattern that spans whitespace can begin in text already released, because
// a space is precisely where the trailing-run rule decides text is settled.
// Detection still fires, but redaction cannot honor its contract — half the
// value has shipped and no rewrite of the remaining half unsays it. Redact
// mode therefore escalates such a match to a block rather than emitting a
// half-scrubbed value and calling it redacted. Operators with a
// whitespace-spanning custom pattern set stream_hold_back to a byte floor
// wide enough to cover it, which moves those matches back into the clean case
// at the cost of that many bytes of lag.
func (p *Plugin) handleBeforeStreamChunk(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	seg, ok := vp.Original.(*events.StreamSegment)
	if !ok || seg.Content == "" {
		return
	}
	// The hold is set on the way out, from whatever Content ends up being:
	// redaction rewrites it, and a hold counted against the pre-redaction
	// text would slice the rewritten text in the wrong place.
	defer func() { seg.HoldFrom(streamHoldFrom(seg.Content, p.streamHoldBack)) }()

	matched, spans, straddled := p.scanSegment(seg)
	if len(matched) == 0 {
		return
	}

	checkList := strings.Join(matched, ", ")
	action := p.action
	if action == "redact" && straddled {
		// Redaction cannot reach text that has already left the engine, so
		// the honest move is to stop the stream rather than to ship a value
		// with only its tail scrubbed.
		action = "block"
	}

	switch action {
	case "block":
		p.logger.Info("content safety checks triggered on llm.stream.chunk",
			"turn_id", seg.TurnID, "request_id", seg.RequestID,
			"checks", checkList, "action", action,
			"configured_action", p.action,
			"straddled_release_boundary", straddled,
			"released_bytes", seg.PendingOffset())
		// No replacement text here: a stream has no substitute, only an end.
		// The user-facing message arrives from handleBeforeLLMResponse when
		// the provider publishes the completed response.
		vp.Veto = engine.VetoResult{
			Vetoed: true,
			Reason: fmt.Sprintf("Content safety (llm.stream.chunk): %s", checkList),
		}

	case "redact":
		p.logger.Warn("content safety checks triggered on llm.stream.chunk",
			"turn_id", seg.TurnID, "request_id", seg.RequestID,
			"checks", checkList, "action", action)
		seg.Content = redactSpans(seg.Content, spans)
	}
}

// scanSegment finds the matches in a segment that can still be acted on.
//
// It returns the names of the checks that tripped, the byte ranges within
// seg.Content covering matches that lie wholly in unreleased text, and
// whether any match began in text already released. A match ending at or
// before the release boundary is ignored: it was adjudicated on an earlier
// segment, and re-reporting it would cut a stream over bytes this handler
// itself let through.
func (p *Plugin) scanSegment(seg *events.StreamSegment) (matched []string, spans [][2]int, straddled bool) {
	full := seg.Full()
	pending := seg.PendingOffset()

	from := pending - streamScanLookbehind
	if from < 0 {
		from = 0
	}
	for from > 0 && !utf8.RuneStart(full[from]) {
		from--
	}
	window := full[from:]
	boundary := pending - from

	for _, c := range p.checks {
		// Every match, not just the leftmost: an earlier one sitting wholly
		// in released text would otherwise mask a live one behind it.
		hit := false
		for _, loc := range c.pattern.FindAllStringIndex(window, -1) {
			if loc[1] <= boundary {
				continue
			}
			hit = true
			if loc[0] < boundary {
				straddled = true
				continue
			}
			spans = append(spans, [2]int{loc[0] - boundary, loc[1] - boundary})
		}
		if hit {
			matched = append(matched, c.name)
		}
	}
	return matched, spans, straddled
}

// redactSpans replaces the given byte ranges of content with a marker.
// Ranges are replaced back-to-front so earlier offsets stay valid, and
// overlapping ranges (two checks matching the same text) collapse into one
// marker rather than nesting.
func redactSpans(content string, spans [][2]int) string {
	if len(spans) == 0 {
		return content
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })

	merged := make([][2]int, 0, len(spans))
	for _, sp := range spans {
		if n := len(merged); n > 0 && sp[0] <= merged[n-1][1] {
			if sp[1] > merged[n-1][1] {
				merged[n-1][1] = sp[1]
			}
			continue
		}
		merged = append(merged, sp)
	}

	out := content
	for i := len(merged) - 1; i >= 0; i-- {
		lo, hi := merged[i][0], merged[i][1]
		if lo < 0 || hi > len(out) || lo > hi {
			continue
		}
		out = out[:lo] + "[REDACTED]" + out[hi:]
	}
	return out
}

// streamHoldFrom returns the offset in content from which a gate should defer
// release: the start of the trailing non-whitespace run, pulled back further
// if a configured floor demands it.
//
// The trailing run is the load-bearing half. Token-shaped patterns cannot
// contain whitespace, so once a whitespace character has arrived after a run,
// that run is complete and safe to judge; everything after it might still be
// growing. floor exists for patterns that do span whitespace, where no
// boundary in the text is a safe place to stop.
func streamHoldFrom(content string, floor int) int {
	from := len(content)
	for from > 0 {
		r := content[from-1]
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			break
		}
		from--
	}
	if floor > 0 && len(content)-floor < from {
		from = len(content) - floor
		if from < 0 {
			from = 0
		}
	}
	return from
}

func (p *Plugin) loadCustomPatterns(v any) {
	var raw []string
	switch list := v.(type) {
	case []any:
		for _, item := range list {
			if s, ok := item.(string); ok {
				raw = append(raw, s)
			}
		}
	case []string:
		raw = list
	}

	for _, pat := range raw {
		re, err := regexp.Compile(pat)
		if err != nil {
			p.logger.Error("failed to compile custom pattern",
				"pattern", pat, "error", err)
			continue
		}
		p.checks = append(p.checks, check{name: "custom:" + pat, pattern: re})
	}
}

// luhnValid performs Luhn checksum validation on a digit string.
// Not currently wired in (credit card check uses regex only),
// available for future enhancement.
func luhnValid(number string) bool {
	// Strip spaces and dashes.
	clean := strings.NewReplacer(" ", "", "-", "").Replace(number)
	if len(clean) < 13 || len(clean) > 19 {
		return false
	}

	sum := 0
	alt := false
	for i := len(clean) - 1; i >= 0; i-- {
		d := int(clean[i] - '0')
		if d < 0 || d > 9 {
			return false
		}
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}
